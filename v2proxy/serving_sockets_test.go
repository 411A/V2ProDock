package main

// Serving-socket guard: the rotation evidence egressNotes structurally cannot
// provide. A client pinned straight to a per-instance SOCKS port moves its bytes
// across the xray child, so the byte ledger is blind to it, and bc27675's guard
// therefore read "idle" on an instance with a live tunnel standing on it.
//
// The pure half runs on every platform, Windows included: the parser is a
// function over []byte precisely so the /proc format contract stays testable
// where /proc does not exist. The socket half needs a real kernel AND foreign
// processes — a client opened by this process sits in this process's own
// descriptor table, which is exactly the set the guard subtracts, so an
// in-process client would test nothing at all.

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

// procTCPFixture is a verbatim capture of a dual-stack listener's tables: the
// tcp6 rows carry v4-mapped addresses (the only place a 0.0.0.0 listener's IPv4
// clients ever appear), and the inodes are the ones the kernel printed. Row 0 is
// the listener; 103/104 are one accepted connection seen from BOTH ends, the
// daemon's own probe (104 is its descriptor); 105/106 are a second one, a real
// client; 107 is a TIME_WAIT remnant.
const procTCPFixture = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0000000000000000FFFF00000100007F:9A4D 0000000000000000FFFF000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 563271 1 0000000000000000 100 0 0 10 0
 103: 0000000000000000FFFF00000100007F:9A4D 0000000000000000FFFF00000100007F:DB8A 01 00000000:00000000 00:00000000 00000000  1000        0 0 1 0000000000000000 20 4 31 10 -1
 104: 0000000000000000FFFF00000100007F:DB8A 0000000000000000FFFF00000100007F:9A4D 01 00000000:00000000 00:00000000 00000000  1000        0 563377 1 0000000000000000 20 4 31 10 -1
 105: 0000000000000000FFFF00000100007F:9A4D 0000000000000000FFFF00000100007F:DB8C 01 00000000:00000000 00:00000000 00000000  1000        0 0 1 0000000000000000 20 4 31 10 -1
 106: 0000000000000000FFFF00000100007F:DB8C 0000000000000000FFFF00000100007F:9A4D 01 00000000:00000000 00:00000000 00000000  1000        0 918273 1 0000000000000000 20 4 31 10 -1
 107: 0000000000000000FFFF00000100007F:9A4D 0000000000000000FFFF00000100007F:DB8D 06 00000000:00000000 00:00000000 00000000  1000        0 0 1 0000000000000000 20 4 31 10 -1
`

// TestParseProcNetTCP pins the /proc/net/tcp contract the rotation guard leans
// on. A regression is silent in both directions — counting too few severs live
// long-polls again, counting too many freezes the pool — and the only place that
// can be caught cheaply is a fixture, since the failure needs a live socket.
func TestParseProcNetTCP(t *testing.T) {
	rows, ok := parseProcNetTCP([]byte(procTCPFixture))
	if !ok {
		t.Fatal("a well-formed table must parse")
	}
	if len(rows) != 6 {
		t.Fatalf("parsed %d rows, want 6 (header excluded, every data row kept)", len(rows))
	}
	if rows[0].state != "0A" || rows[1].state != tcpStateEstablished || rows[1].local.port != 0x9A4D {
		t.Fatalf("row order or state column misread: %+v / %+v", rows[0], rows[1])
	}
	if rows[0].inode != "563271" || rows[2].inode != "563377" || rows[4].inode != "918273" {
		t.Fatalf("inode column misread: %q / %q / %q", rows[0].inode, rows[2].inode, rows[4].inode)
	}
	// The dual-stack trap, at the level of the decode: a v4-mapped tcp6 address
	// must fold onto its IPv4 spelling, or the two ends of one connection can
	// never be paired and every IPv4 client looks like the daemon's own traffic.
	if got := rows[1].local.addr; got != "0100007F" {
		t.Fatalf("v4-mapped address left unfolded: %q", got)
	}
	if rows[0].local.String() != "0100007F:39501" {
		t.Fatalf("endpoint key = %q, want the folded IPv4 form", rows[0].local)
	}
	// Every imprecision must fail SAFE, which for a parser means "not understood"
	// rather than "nothing there": an unparsable row that reads as an empty table
	// is how the guard would sever live long-polls.
	for name, table := range map[string]string{
		"garbage row":      procTCPFixture + " not a socket row at all\n",
		"truncated row":    procTCPFixture + " 108: 0100007F:9A4D 0100007F:DB8A\n",
		"non-hex state":    procTCPFixture + " 109: 0100007F:9A4D 0100007F:DB8A ZZ 00000000:00000000 00:00000000 00000000 1000 0 1\n",
		"address-less row": procTCPFixture + " 110: 9A4D DB8A 01 00000000:00000000 00:00000000 00000000 1000 0 1\n",
		"no header":        "  0: 0100007F:9A4D 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 1\n",
		"empty table":      "",
	} {
		if _, ok := parseProcNetTCP([]byte(table)); ok {
			t.Errorf("%s: must report the table as ununderstood, not as idle", name)
		}
	}
	// A table read can end mid-line; a trailing partial row is the one shape that
	// must not condemn the whole pass, so blank tails are simply ignored.
	if rows, ok := parseProcNetTCP([]byte(procTCPFixture + "\n\n")); !ok || len(rows) != 6 {
		t.Fatalf("blank tail changed the verdict: ok=%v rows=%d", ok, len(rows))
	}
}

// TestForeignEstablishedSeparatesDaemonSockets is the two measured traps in one
// table: our own liveness pokes (row 103's peer is row 104, whose inode is a
// descriptor of ours) and a genuine client (row 105's peer is row 106, whose
// inode is not).
func TestForeignEstablishedSeparatesDaemonSockets(t *testing.T) {
	rows, ok := parseProcNetTCP([]byte(procTCPFixture))
	if !ok {
		t.Fatal("fixture must parse")
	}
	// Row 104 is the daemon's own end of the connection xray accepted on 0x9A4D.
	own := map[string]bool{"563377": true}

	if got := foreignEstablished(rows, own, 0x9A4D); got != 1 {
		t.Fatalf("client sockets on the serving port = %d, want 1 (our own poke subtracted, TIME_WAIT and LISTEN ignored)", got)
	}
	// Prove the subtraction is what removed it: with no ownership filter the same
	// table holds two established sockets, ours and the user's.
	if got := foreignEstablished(rows, nil, 0x9A4D); got != 2 {
		t.Fatalf("unfiltered established sockets = %d, want 2 (test would be vacuous otherwise)", got)
	}
	// The peer ends live on their own ephemeral ports. Ownership is decided per
	// CONNECTION, so the daemon's own probe port now answers 0 - that connection
	// has an owned end wherever you count it from - while the client's ephemeral
	// port answers 1. Querying the daemon's port is the regression test for
	// pairing: an implementation that filters rows independently gets 1 here and
	// would be counting the daemon's own dial as a client.
	if got := foreignEstablished(rows, own, 0xDB8A); got != 0 {
		t.Fatalf("established on our own ephemeral port = %d, want 0 (that connection is our own probe)", got)
	}
	if got := foreignEstablished(rows, own, 0xDB8C); got != 1 {
		t.Fatalf("established on the client's ephemeral port = %d, want 1", got)
	}
	if got := foreignEstablished(rows, own, 0xFFFF); got != 0 {
		t.Fatalf("unrelated port = %d, want 0", got)
	}
	// Ownership is matched on the full four-tuple, never on the port alone: an
	// inode that pairs with nothing must not excuse a real client.
	if got := foreignEstablished(rows, map[string]bool{"563271": true}, 0x9A4D); got != 2 {
		t.Fatalf("unpaired ownership entry changed the count to %d, want 2", got)
	}
}

// TestServingClientsUnknownEvidenceExpires is the bounded-conservatism contract:
// an unreadable /proc reads as BUSY (reliability first) but never permanently,
// and a socket we can actually see is never time-boxed at all.
func TestServingClientsUnknownEvidenceExpires(t *testing.T) {
	// Sanity on the bound itself: it must outlast a couple of refresh ticks (a
	// transient table read must never cost a client its connection) and still be
	// short enough that a hard-broke /proc heals the pool in minutes.
	if servingSocketUnprovenGrace < 2*subscriptionRefreshInterval ||
		servingSocketUnprovenGrace > 30*subscriptionRefreshInterval {
		t.Fatalf("servingSocketUnprovenGrace out of sane range: %s (refresh every %s)",
			servingSocketUnprovenGrace, subscriptionRefreshInterval)
	}
	s := newTestSelector(t, t.TempDir(), "http://probe.invalid/", freeLoopbackPort(t), freeLoopbackPort(t))
	unknown := func(int) (int, bool) { return 0, false }
	provenEmpty := func(int) (int, bool) { return 0, true }
	provenBusy := func(int) (int, bool) { return 1, true }
	backdate := func(d time.Duration) {
		s.servingMu.Lock()
		s.servingUnproven = time.Now().Add(-d)
		s.servingMu.Unlock()
	}

	if !s.servingClients(unknown) {
		t.Fatal("an unreadable /proc must read as busy: reliability first")
	}
	if !s.servingClients(unknown) {
		t.Fatal("still inside the window: one blind pass must not hand out a rotation")
	}
	backdate(servingSocketUnprovenGrace + time.Second)
	if s.servingClients(unknown) {
		t.Fatalf("a permanently broken /proc wedged rotation for good (bound %s)", servingSocketUnprovenGrace)
	}
	// Evidence returning re-arms the window in both directions: the instance is
	// immediately rotatable again, and a later blind pass starts a full window.
	if s.servingClients(provenEmpty) {
		t.Fatal("proven-idle must be rotatable")
	}
	s.servingMu.Lock()
	reArmed := s.servingUnproven.IsZero()
	s.servingMu.Unlock()
	if !reArmed {
		t.Fatal("conclusive evidence must clear the staleness clock")
	}
	if !s.servingClients(unknown) {
		t.Fatal("a fresh unknown window must start busy again")
	}
	// A socket we can SEE is the connection rotation destroys. Expiring that is
	// the opposite decision, so it is not time-boxed.
	backdate(10 * servingSocketUnprovenGrace)
	if !s.servingClients(provenBusy) {
		t.Fatal("a proven client socket must never expire: that is the cut bc27675 exists to prevent")
	}
	// ... and a proven-empty pass still clears the clock afterwards.
	if s.servingClients(provenEmpty) {
		t.Fatal("proven-idle must stay rotatable")
	}
}

// ---- Real sockets on a real kernel ----

// needProcClients gates the tests that need /proc AND a client in another
// process. Same convention as needStub: this suite is developed on Windows,
// where there is no /proc at all.
func needProcClients(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("serving-socket evidence needs linux (/proc/net/tcp)")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required to hold a socket open in another process")
	}
}

// foreignListener starts a listener on 0.0.0.0 in a SEPARATE process and
// returns its port, so the accepted end is foreign exactly as xray's is.
//
// Foreign by necessity, and this is the correction that made the whole guard
// testable: an in-process net.Listen puts the serving end in THIS process's own
// descriptor table, which is precisely the set the guard subtracts. Every
// client against such a listener then looks like the daemon's own probe, and the
// test proves nothing - it asserts "no client is ever seen" and calls it a pass.
func foreignListener(t *testing.T) (int, func()) {
	t.Helper()
	port := freeLoopbackPort(t)
	script := "import socket,sys\n" +
		"# AF_INET6 with V6ONLY off is a DUAL-STACK listener, which is what Go's\n" +
		"# net.Listen('tcp','0.0.0.0') - and therefore xray - actually binds on\n" +
		"# linux. Its IPv4 clients then appear in tcp6 ONLY, as v4-mapped rows.\n" +
		"s=socket.socket(socket.AF_INET6)\n" +
		"s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)\n" +
		"s.setsockopt(socket.IPPROTO_IPV6,socket.IPV6_V6ONLY,0)\n" +
		"s.bind(('::',int(sys.argv[1])))\n" +
		"s.listen(16)\n" +
		"held=[]\n" +
		"while True:\n" +
		"    c,_=s.accept()\n" +
		"    held.append(c)\n"
	cmd := exec.Command("python3", "-u", "-c", script, strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start foreign listener: %v", err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		})
	}
	t.Cleanup(stop)
	// Wait for the bind, or the first assertions race the listener's startup.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
			_ = c.Close()
			return port, stop
		}
		if time.Now().After(deadline) {
			t.Fatalf("foreign listener never bound port %d", port)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// parkForeignClient connects to 127.0.0.1:port from a SEPARATE process, sends
// nothing, and holds the connection open. Foreign by necessity: a socket opened
// by this process is in this process's own descriptor table, which is precisely
// the set the guard subtracts.
func parkForeignClient(t *testing.T, port int) (stop func()) {
	t.Helper()
	script := "import socket,sys,time\n" +
		"c=socket.create_connection(('127.0.0.1',int(sys.argv[1])))\n" +
		"time.sleep(120)\n"
	cmd := exec.Command("python3", "-c", script, strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start parked client: %v", err)
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		})
	}
	t.Cleanup(stop)
	return stop
}

// waitServingClients polls until the guard agrees with want. Parking a client
// races the kernel publishing its row, so the assertion waits instead of
// sampling once and calling a torn-down socket "not seen".
func waitServingClients(t *testing.T, s *ProxySelector, want bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if s.ServingClients() == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ServingClients() never became %v on port %d", want, s.SOCKSPort())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// anyEstablishedOnPort is the RAW kernel truth on port: the parse with no
// ownership subtraction at all, so it counts both ends of every connection and
// the daemon's own sockets included. It is deliberately not what ServingClients
// answers - it exists so the assertions can prove the guard filters rather than
// merely sees nothing, which is what an empty result on a broken scan would
// look like.
func anyEstablishedOnPort(t *testing.T, port int) int {
	t.Helper()
	// BOTH files must go in one call: a v4-mapped dual-stack connection is
	// published in tcp AND tcp6, and folding connections across a per-file split
	// would either double-count it or hide it.
	return foreignEstablished(readProcRows(t), nil, port)
}

// readProcRows returns every parsed row from both tables, or fails loudly - a
// live table we cannot understand must never read as "nothing connected".
func readProcRows(t *testing.T) []procConn {
	t.Helper()
	rows := make([]procConn, 0, 256)
	for _, path := range procNetTCPFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		parsed, ok := parseProcNetTCP(raw)
		if !ok {
			t.Fatalf("parse %s: live table not understood", path)
		}
		rows = append(rows, parsed...)
	}
	return rows
}

// foreignOnPort is the population the rotation guard actually reasons about:
// sockets on port with the daemon's own ends removed.
//
// Ownership MUST be subtracted for this shape of assertion. newTestSelector
// starts a stub xray and HealthCheck immediately probes it, so the test process
// holds live sockets on the serving port, and a raw count reads those as
// "stray sockets" on an instance that is, in fact, idle.
func foreignOnPort(t *testing.T, port int) int {
	t.Helper()
	own, ok := procOwnInodes()
	if !ok {
		t.Fatalf("cannot read own socket inodes: %s", procSelfFD)
	}
	return foreignEstablished(readProcRows(t), own, port)
}

// TestServingClientsSeesForeignSocketNotOwnProbe is the guard against BOTH
// measured traps, against a real kernel and real sockets: the daemon's own dial
// (what the health probe, the bind waits and the watchdog's tcpOpen all do)
// must never read as a client, and a client from another process must always be
// seen — even though xray's dual-stack 0.0.0.0 listener publishes it in tcp6
// only, as a v4-mapped row.
func TestServingClientsSeesForeignSocketNotOwnProbe(t *testing.T) {
	needProcClients(t)
	// 0.0.0.0 exactly as renderXrayConfig binds the SOCKS inbound, and in another
	// process exactly as xray holds it - see foreignListener for why the second
	// half is load-bearing.
	port, _ := foreignListener(t)

	s := newTestSelector(t, t.TempDir(), "http://probe.invalid/", port, freeLoopbackPort(t))
	waitServingClients(t, s, false)
	if n := anyEstablishedOnPort(t, port); n != 0 {
		t.Fatalf("precondition: nothing may be connected yet, found %d", n)
	}

	// The daemon's own liveness poke: a real, ESTABLISHED socket on the serving
	// port that must still read as idle.
	self, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = self.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for anyEstablishedOnPort(t, port) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := anyEstablishedOnPort(t, port); n == 0 {
		t.Fatal("precondition: our own dial must show up in /proc, or the rest proves nothing")
	}
	if s.ServingClients() {
		t.Fatalf("the daemon's own probe read as client traffic (%d raw sockets): every rotation would be deferred", anyEstablishedOnPort(t, port))
	}

	// A client in another process: the case bc27675's byte ledger cannot see.
	stop := parkForeignClient(t, port)
	waitServingClients(t, s, true)
	raw := anyEstablishedOnPort(t, port)
	if raw < 2 {
		t.Fatalf("precondition: expected our poke plus the client's socket, saw %d", raw)
	}
	// The dual-stack trap, stated correctly. It is tempting to assert the CLIENT's
	// socket is tcp6-only, and that assertion is simply false: an IPv4 client
	// connected to a dual-stack listener holds a real IPv4 socket, so its own row
	// is published in tcp like any other. Only the SERVING end is v4-mapped into
	// tcp6 - and that end is xray's, the half that carries the port as its local
	// address. A tcp-only scan therefore never sees the listener's own rows at all,
	// which is precisely why procNetTCPFiles carries tcp6 and why matching both
	// columns matters.
	servingEndsIn := func(path string) int {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rows, ok := parseProcNetTCP(body)
		if !ok {
			t.Fatalf("parse %s: live table not understood", path)
		}
		n := 0
		for _, r := range rows {
			if r.state == tcpStateEstablished && r.local.port == port {
				n++
			}
		}
		return n
	}
	if n := servingEndsIn(procNetTCP6); n == 0 {
		t.Fatalf("no serving end in tcp6: a dual-stack listener's accepted sockets must be v4-mapped there")
	}
	if n := servingEndsIn(procNetTCP); n != 0 {
		t.Fatalf("serving end published %d rows in tcp; the dual-stack mapping is not being exercised", n)
	}

	stop()
	waitServingClients(t, s, false)
}

// TestServingClientsDoesNotFakeHealth is the health/traffic decoupling. An open
// socket proves that SOMETHING is connected, nothing more: a blackholed-but-open
// tunnel is the defining DPI failure and is exactly what the probe below meets.
// Folding this signal into HealthCheck would let one wedged client report a dead
// upstream as healthy forever.
func TestServingClientsDoesNotFakeHealth(t *testing.T) {
	needProcClients(t)
	// Foreign listener so the parked client's connection is neither end owned by
	// this process - see foreignListener. It HOLDS connections rather than
	// hanging up, which is what the client half of this test needs; the health
	// probe therefore fails on its own timeout rather than on an instant reset.
	socks, _ := foreignListener(t)
	s := newTestSelector(t, t.TempDir(), "http://probe.invalid/", socks, freeLoopbackPort(t))
	s.UpdateConfigs([]ProxyConfig{{Name: "n", Raw: "r", Endpoint: "e:1"}})
	s.mu.Lock()
	s.activeIndex = 0
	s.mu.Unlock()

	parkForeignClient(t, socks)
	waitServingClients(t, s, true)
	expireEgress(socks, s.HTTPPort())
	if s.ServingTraffic() {
		t.Fatal("precondition: the byte ledger must see nothing")
	}

	down := false
	for range healthFailThreshold {
		if !s.HealthCheck() {
			down = true
		}
	}
	if !down {
		t.Fatal("an open client socket kept a dead upstream healthy: the two signals must stay separate")
	}
	if !s.ServingClients() {
		t.Fatal("the socket signal itself must still be there — it just does not feed health")
	}
}

// TestReconcileSlowActiveWithOpenClientNotRotated is the gap itself: an instance
// with a live client socket on its SOCKS port and NO byte evidence at all — the
// exact state in which the pre-patch guard rotated the tunnel out from under a
// long-poll. Rotation is stopXray()+startXray() on the serving port.
func TestReconcileSlowActiveWithOpenClientNotRotated(t *testing.T) {
	needStub(t)
	dir := t.TempDir()
	writeStubXray(t, dir)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := newTestSelector(t, dir, "http://probe.invalid/", socks, httpP)
	s.UpdateConfigs([]ProxyConfig{e2eCand("A", "e2e-a:1", "good"), e2eCand("B", "e2e-b:1", "good")})
	if err := s.StartWithBest(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.lastLatency = rotateSlowLatency + time.Second
	s.lastProbe = time.Now()
	s.mu.Unlock()
	// No byte evidence whatsoever — that is the entire point of this test.
	expireEgress(socks, httpP)
	if s.ServingTraffic() {
		t.Fatal("precondition: the byte ledger must see nothing")
	}
	waitServingClients(t, s, false)

	parkForeignClient(t, socks)
	waitServingClients(t, s, true)
	before := s.currentPID()
	m := &ProxyManager{
		instances: []*ProxySelector{s},
		statuses:  []InstanceStatus{{Index: 0, Status: "ok"}},
		xrayDir:   dir,
	}
	m.reconcileActive(0, s, s.ActiveConfig())
	if got := s.ActiveConfig(); got == nil || got.Key() != "e2e-a:1" {
		t.Fatalf("instance with a live client socket must not be rotated, got %+v", got)
	}
	if s.currentPID() != before {
		t.Fatal("rotation severed the client's live connection")
	}
	// The socket must have been there for the whole decision: if the client had
	// gone away the assertions above would pass for the wrong reason.
	if !s.ServingClients() {
		t.Fatal("precondition lost: the client socket vanished before the guard was consulted")
	}
}

// TestReconcileSlowIdleActiveStillRotatesWithoutSockets is the other half: the
// guard protects connections, it must not freeze the pool. A healthy, unbound,
// freshly-measured-slow instance still rotates — otherwise a slow-but-working
// upstream is served forever, and it keeps winning aggMinStreak, so it would
// monopolise the aggregate head rather than merely stay put.
func TestReconcileSlowIdleActiveStillRotatesWithoutSockets(t *testing.T) {
	needStub(t)
	dir := t.TempDir()
	writeStubXray(t, dir)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := newTestSelector(t, dir, "http://probe.invalid/", socks, httpP)
	s.UpdateConfigs([]ProxyConfig{e2eCand("A", "e2e-a:1", "good"), e2eCand("B", "e2e-b:1", "good")})
	if err := s.StartWithBest(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.lastLatency = rotateSlowLatency + time.Second
	s.lastProbe = time.Now()
	s.mu.Unlock()
	expireEgress(socks, httpP)
	if s.ServingTraffic() {
		t.Fatal("precondition: instance must look idle to the byte ledger")
	}
	// Waiting rather than sampling: the daemon's own probe has just closed, and
	// its socket must leave the table before "idle" means anything.
	waitServingClients(t, s, false)
	if n := foreignOnPort(t, socks); n != 0 {
		t.Fatalf("precondition: %d foreign socket(s) on the serving port", n)
	}
	if !s.LatencyFresh(rotateLatencyMaxAge) {
		t.Fatal("precondition: measurement must be fresh")
	}
	m := &ProxyManager{
		instances: []*ProxySelector{s},
		statuses:  []InstanceStatus{{Index: 0, Status: "ok"}},
		xrayDir:   dir,
	}
	m.reconcileActive(0, s, s.ActiveConfig())
	if got := s.ActiveConfig(); got == nil || got.Key() != "e2e-b:1" {
		t.Fatalf("fresh+slow idle instance must still rotate to e2e-b:1, got %+v", got)
	}
}
