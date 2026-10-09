package main

// Populate-path bind safety: tryConfigs must not grade a candidate on a SOCKS
// port that xray has not finished binding.
//
// launchXray only proves the PROCESS outlived xrayCrashDetect (100ms). Probing
// into the gap between "process alive" and "listener bound" reads connection
// refused, classifyProbe calls that verdictReject, and markBad poisons the
// SHARED probe ledger — so a healthy upstream is skipped by every peer instance
// for the rest of the round. The rotation path already learned this
// (probeTempPort waits for the port); populate is the path that skipped it.
//
// These tests need a real child process (tryConfigs launches one), but not a
// real xray: the fake xray is this test binary re-executed, which keeps the
// whole tier hermetic — loopback only, no python, no /proc, no xray download.
// The production mode is read from the rendered config's outbound tag, exactly
// like the python stub in e2e_test.go, so one pool can mix modes.
//
// Modes (delimiter ":" so mode names stay free of hyphens):
//   - "good"       — bind immediately, serve SOCKS5 -> 204
//   - "slowbind"   — stay alive, bind only when the test opens the gate below
//   - "neverbind"  — stay alive forever without ever binding (dead config that
//     does not crash, or a host too starved to schedule the bind)
//   - "crash"      — exit at once, like a config xray rejects outright
//
// "slowbind" waits for a GATE the test opens, not for a wall-clock delay: a
// fixed sleep is a coin flip, because loading this ~20MB binary on a busy host
// can outlast the delay entirely and the bind would then happen before the
// parent's probe — making the regression test vacuous. The gate is opened only
// after the child has logged its own start (proving the child is up) plus
// several crash-detect windows, so the bind provably lands after the parent has
// already probed.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeXrayEnv marks a re-exec of this test binary as a fake xray child.
// init() sees it before TestMain and never returns.
const fakeXrayEnv = "V2PROXY_TEST_FAKE_XRAY"

// fakeXrayLogEnv, when set, names a file every fake xray appends its mode to
// at startup: the only way to count how many children a scan actually launched.
const fakeXrayLogEnv = "V2PROXY_TEST_FAKE_XRAY_LOG"

// fakeXrayGateEnv, when set, names a file a "slowbind" child waits for before
// it binds its listener.
const fakeXrayGateEnv = "V2PROXY_TEST_FAKE_XRAY_GATE"

func init() {
	if os.Getenv(fakeXrayEnv) == "" {
		return
	}
	fakeXrayMain()
}

// fakeXrayMain is the child half: bind (or not), serve SOCKS5, never return.
func fakeXrayMain() {
	port, tag, ok := fakeXrayConfig()
	if !ok {
		os.Exit(2)
	}
	fakeXrayNote(tag)
	switch tag {
	case "crash":
		os.Exit(3)
	case "neverbind":
		for {
			time.Sleep(time.Hour)
		}
	case "slowbind":
		if !fakeXrayWaitGate() {
			os.Exit(4)
		}
	}
	fakeXrayServe(port)
}

// fakeXrayWaitGate blocks until the test opens the gate file. Called by a
// "slowbind" child AFTER it has logged its own start, so the parent can prove
// the bind happens strictly later.
func fakeXrayWaitGate() bool {
	path := os.Getenv(fakeXrayGateEnv)
	if path == "" {
		return true
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// fakeXrayConfig reads the rendered config exactly as the python stub does:
// the port from inbounds[0], the behaviour from outbounds[0].tag.
func fakeXrayConfig() (port int, tag string, ok bool) {
	path := ""
	for i, a := range os.Args {
		if a == "-c" && i+1 < len(os.Args) {
			path = os.Args[i+1]
		}
	}
	if path == "" {
		return 0, "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, "", false
	}
	var doc struct {
		Inbounds  []struct{ Port int }
		Outbounds []struct{ Tag string }
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Inbounds) == 0 {
		return 0, "", false
	}
	tag = "good"
	if len(doc.Outbounds) > 0 && doc.Outbounds[0].Tag != "" {
		tag = doc.Outbounds[0].Tag
	}
	return doc.Inbounds[0].Port, tag, true
}

func fakeXrayNote(tag string) {
	path := os.Getenv(fakeXrayLogEnv)
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s\t%d\n", tag, os.Getpid())
}

// bindGateDelay is how long past the parent's crash-detect window the gate
// opens. Comfortably more than xrayCrashDetect so "bound late" is a certainty,
// not a race.
const bindGateDelay = 6 * xrayCrashDetect

// holdBindGate makes the next "slowbind" child bind only after it has started
// AND after bindGateDelay has passed — i.e. strictly after the parent's
// launchXray has already returned and probed. Returns the startup log path so
// the caller can count children.
func holdBindGate(t *testing.T, dir string) string {
	t.Helper()
	logPath := filepath.Join(dir, "starts.log")
	gate := filepath.Join(dir, "bind-go")
	t.Setenv(fakeXrayLogEnv, logPath)
	t.Setenv(fakeXrayGateEnv, gate)
	t.Cleanup(func() { _ = os.Remove(gate) })
	// Goroutine, not a test helper: it must not touch *testing.T.
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if raw, err := os.ReadFile(logPath); err == nil && len(raw) > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(bindGateDelay)
		_ = os.WriteFile(gate, []byte("go"), 0o600)
	}()
	return logPath
}

func fakeXrayServe(port int) {
	ln, err := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(port))
	if err != nil {
		os.Exit(3)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go fakeXrayConn(conn)
	}
}

// fakeXrayConn is the minimum SOCKS5 wire sequence a probe performs, answered
// with 204 whatever was asked for — no upstream, no internet, loopback only.
func fakeXrayConn(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 512)
	// greeting: VER NMETHODS METHODS...
	if _, err := io.ReadFull(c, buf[:2]); err != nil || buf[0] != 5 {
		return
	}
	if _, err := io.ReadFull(c, buf[:int(buf[1])]); err != nil {
		return
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return
	}
	// request: VER CMD RSV ATYP DST.ADDR DST.PORT
	if _, err := io.ReadFull(c, buf[:4]); err != nil {
		return
	}
	var err error
	switch buf[3] {
	case 1:
		_, err = io.ReadFull(c, buf[:4])
	case 3:
		if _, err = io.ReadFull(c, buf[:1]); err == nil {
			_, err = io.ReadFull(c, buf[:int(buf[0])])
		}
	case 4:
		_, err = io.ReadFull(c, buf[:16])
	default:
		return
	}
	if err != nil {
		return
	}
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return
	}
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	head := make([]byte, 0, 512)
	for !bytes.Contains(head, []byte("\r\n\r\n")) && len(head) < 4096 {
		n, rerr := c.Read(buf)
		if n > 0 {
			head = append(head, buf[:n]...)
		}
		if rerr != nil {
			return
		}
	}
	_, _ = c.Write([]byte("HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
}

// installFakeXray drops a copy of this test binary into dir as `xray` and
// marks the environment so the copy runs as a fake xray when launched. The
// selector's own launchXray does the rest — no seam in production code.
func installFakeXray(t *testing.T, dir string) {
	t.Helper()
	name := "xray"
	if runtime.GOOS == "windows" {
		name = "xray.exe"
	}
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeXrayEnv, "1")
	// The Telegram leg would dial api.telegram.org through the stub; drop it so
	// the race is exactly the plain-HTTP primary plus the Cloudflare fallback.
	t.Setenv("TELEGRAM_PROBE", "0")
}

// shrinkPopulatePortWait pins the bind budget for a test. Restored on cleanup.
func shrinkPopulatePortWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := populatePortWait
	populatePortWait = d
	t.Cleanup(func() { populatePortWait = prev })
}

func fakeXrayLogLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.FieldsFunc(string(raw), func(r rune) bool { return r == '\n' })
}

// newFakeXraySelector wires a selector onto the fake xray with loopback-only
// probe targets.
func newFakeXraySelector(t *testing.T, dir string, socks, httpPort int) *ProxySelector {
	t.Helper()
	return newTestSelector(t, dir, "http://probe.invalid/", socks, httpPort)
}

// THE REGRESSION TEST. A candidate whose xray is alive but slow to bind is
// ADOPTED, and — the part that actually bit in production — never enters the
// shared bad ledger.
//
// On the pre-fix code this fails twice over: the probe ran bindGateDelay after
// the child logged its start, the port was still closed, connection refused
// graded as verdictReject, and the candidate was markBad'd, so populate
// reported "no working config found" for a perfectly healthy upstream.
func TestPopulateWaitsForBindBeforeProbing(t *testing.T) {
	dir := t.TempDir()
	installFakeXray(t, dir)
	logPath := holdBindGate(t, dir)
	shrinkPopulatePortWait(t, 3*time.Second)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := newFakeXraySelector(t, dir, socks, httpP)
	s.UpdateConfigs([]ProxyConfig{e2eCand("slow", "slowbind-endpoint:1", "slowbind")})

	shared := newProbeShared()
	start := time.Now()
	if err := s.startShared(nil, shared, time.Now().Add(30*time.Second)); err != nil {
		t.Fatalf("a slow-binding but healthy candidate must be adopted, got: %v", err)
	}
	elapsed := time.Since(start)
	if got := s.ActiveConfig(); got == nil || got.Key() != "slowbind-endpoint:1" {
		t.Fatalf("expected the slow-binding candidate to be active, got %+v", got)
	}
	// The whole point of the defect: a bind that merely ran late is not an
	// upstream failure, so nothing about this candidate may reach the ledger.
	if len(shared.bad) != 0 {
		t.Fatalf("a slow bind poisoned the shared ledger (%d entries): the upstream is healthy, only the host was busy", len(shared.bad))
	}
	// The probe must not have run before the listener existed.
	if elapsed < bindGateDelay {
		t.Fatalf("populate returned in %s: it probed before xray bound the port", elapsed)
	}
	if starts := fakeXrayLogLines(t, logPath); len(starts) != 1 {
		t.Fatalf("expected exactly 1 child launch, got %v", starts)
	}
	if !s.HealthCheck() {
		t.Fatal("the adopted instance must be healthy")
	}
}

// A port that never opens is NOT proof the upstream is dead: the config may be
// fine and the host merely too starved to schedule the bind. Poisoning the
// shared ledger here is the defect, so the candidate must stay clean AND
// claimable, and the scan must move on to the next one.
func TestPopulateNeverBoundCandidateIsNotMarkedBad(t *testing.T) {
	dir := t.TempDir()
	installFakeXray(t, dir)
	shrinkPopulatePortWait(t, 400*time.Millisecond)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := newFakeXraySelector(t, dir, socks, httpP)
	s.UpdateConfigs([]ProxyConfig{
		e2eCand("never", "neverbind-endpoint:1", "neverbind"),
		e2eCand("good", "good-endpoint:1", "good"),
	})

	shared := newProbeShared()
	skipped, err := s.tryConfigs(nil, shared, time.Now().Add(30*time.Second), true)
	if err != nil {
		t.Fatalf("populate must move past a never-binding candidate: %v", err)
	}
	if got := s.ActiveConfig(); got == nil || got.Key() != "good-endpoint:1" {
		t.Fatalf("expected the healthy candidate to be adopted, got %+v", got)
	}
	if shared.isBad("neverbind-endpoint:1") {
		t.Fatal("a port that never opened must NOT poison the shared ledger — that is indistinguishable from host starvation")
	}
	if !shared.tryClaim("neverbind-endpoint:1") {
		t.Fatal("a never-bound candidate must stay claimable for a later pass")
	}
	// It is still worth a retry: an unproven candidate counts toward the retry
	// pass, which is where a starved host gets its second chance.
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (the unproven candidate must be retried once)", skipped)
	}
}

// The scan must still honour probeTimeout. bindBudget clamps the per-candidate
// wait to what is left of the deadline, so a 5s bind budget cannot push a
// 400ms-deadline scan past its budget: exactly one child is ever launched.
func TestPopulateBindWaitStopsAtDeadline(t *testing.T) {
	dir := t.TempDir()
	installFakeXray(t, dir)
	logPath := filepath.Join(dir, "starts.log")
	t.Setenv(fakeXrayLogEnv, logPath)
	// Production-sized budget: only the deadline may shorten it. If the clamp
	// regressed, this test would launch all three children at 5s each.
	shrinkPopulatePortWait(t, 5*time.Second)

	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := newFakeXraySelector(t, dir, socks, httpP)
	var cfgs []ProxyConfig
	for _, name := range []string{"n1", "n2", "n3"} {
		cfgs = append(cfgs, e2eCand(name, name+"-endpoint:1", "neverbind"))
	}
	s.UpdateConfigs(cfgs)

	_, err := s.tryConfigs(nil, newProbeShared(), time.Now().Add(400*time.Millisecond), true)
	if err == nil {
		t.Fatal("a pool that never binds must not report success")
	}
	if !strings.Contains(err.Error(), "probe timeout") {
		t.Fatalf("want the scan to end on the probe deadline, got %v", err)
	}
	starts := fakeXrayLogLines(t, logPath)
	if len(starts) != 1 {
		t.Fatalf("launched %d children before the deadline: %v — the bind wait overran probeTimeout", len(starts), starts)
	}
}

// Requirement 4: the healthy path must not regress. A candidate that binds
// promptly is adopted in a fraction of populatePortWait — the added wait costs
// nothing when the port is already there.
func TestPopulateAdoptsImmediatelyBoundCandidate(t *testing.T) {
	dir := t.TempDir()
	installFakeXray(t, dir)
	// Production budget, deliberately NOT shrunk: the point is that adoption
	// never comes close to spending it.
	if populatePortWait <= time.Second {
		t.Fatalf("populatePortWait = %s, expected the production value", populatePortWait)
	}
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := newFakeXraySelector(t, dir, socks, httpP)
	s.UpdateConfigs([]ProxyConfig{e2eCand("good", "good-endpoint:1", "good")})

	start := time.Now()
	if err := s.StartWithBest(); err != nil {
		t.Fatalf("StartWithBest failed: %v", err)
	}
	elapsed := time.Since(start)
	if got := s.ActiveConfig(); got == nil || got.Key() != "good-endpoint:1" {
		t.Fatalf("expected the healthy candidate to be active, got %+v", got)
	}
	if elapsed >= populatePortWait {
		t.Fatalf("healthy populate took %s — it waited out the full %s bind budget", elapsed, populatePortWait)
	}
}

// A config that CRASHES is still a genuinely dead upstream and must never be
// adopted — the fix must not have turned the reject path into "try everything".
func TestPopulateCrashingCandidateIsNeverAdopted(t *testing.T) {
	dir := t.TempDir()
	installFakeXray(t, dir)
	shrinkPopulatePortWait(t, 400*time.Millisecond)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := newFakeXraySelector(t, dir, socks, httpP)
	s.UpdateConfigs([]ProxyConfig{
		e2eCand("crash", "crash-endpoint:1", "crash"),
		e2eCand("good", "good-endpoint:1", "good"),
	})

	shared := newProbeShared()
	if err := s.startShared(nil, shared, time.Now().Add(30*time.Second)); err != nil {
		t.Fatalf("populate must skip the crashing candidate and serve the good one: %v", err)
	}
	if got := s.ActiveConfig(); got == nil || got.Key() != "good-endpoint:1" {
		t.Fatalf("expected the healthy candidate to be active, got %+v", got)
	}
	// Where launchXray's crash detect can actually see the death (unix; on
	// windows processAlive is best-effort and always true) the crash is proof
	// of death and MUST poison the ledger, exactly as before this fix.
	if runtime.GOOS != "windows" && !shared.isBad("crash-endpoint:1") {
		t.Fatal("a candidate whose xray crashed on start must poison the shared ledger")
	}
}

// TestPopulateDeadCandidateDoesNotCostTheBindWait is the COST half of
// TestPopulateCrashingCandidateIsNeverAdopted, which only checks the verdict.
//
// Production found the other half: a pool of public configs is mostly dead, and
// the bind wait ran to its full timeout for every one of them before anyone
// noticed the child had already exited. The verdict was right, the scan was
// minutes long, and the symptom was indistinguishable from a hang - no progress
// line for minutes because 0/N never changed.
//
// So the wait has to end when the CHILD ends, not only when the port opens. The
// budget here is deliberately generous relative to the fake xray's death so a
// regression that waits the full budget fails loudly instead of by a hair.
func TestPopulateDeadCandidateDoesNotCostTheBindWait(t *testing.T) {
	dir := t.TempDir()
	installFakeXray(t, dir)
	shrinkPopulatePortWait(t, 3*time.Second)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := newFakeXraySelector(t, dir, socks, httpP)
	dead := make([]ProxyConfig, 0, 4)
	for i := range 4 {
		dead = append(dead, e2eCand("crash", fmt.Sprintf("crash-%d:1", i), "crash"))
	}
	s.UpdateConfigs(dead)

	start := time.Now()
	shared := newProbeShared()
	// The pool is entirely dead, so an error is the correct outcome; what is
	// being measured is how long getting there takes.
	_ = s.startShared(nil, shared, time.Now().Add(30*time.Second))
	elapsed := time.Since(start)

	// 4 dead candidates at the full 3s budget is 12s. Real death is ~100ms each.
	// The threshold allows for a slow CI box while still catching a full-budget
	// wait, which is the regression.
	if max := 4 * time.Second; elapsed > max {
		t.Fatalf("4 dead candidates took %v; the bind wait must end when the child does, not run its full %v budget (bound would allow ~%v)", elapsed, 3*time.Second, max)
	}
}

// bindBudget is pure and decides whether the per-candidate bind wait can push
// the scan past probeTimeout. Pinned directly so the clamp cannot rot.
func TestBindBudgetClampsToDeadline(t *testing.T) {
	shrinkPopulatePortWait(t, 5*time.Second)

	if got := bindBudget(time.Time{}); got != 5*time.Second {
		t.Errorf("no deadline (StartWithBest): bindBudget = %s, want the full %s", got, 5*time.Second)
	}
	if got := bindBudget(time.Now().Add(time.Hour)); got != 5*time.Second {
		t.Errorf("distant deadline: bindBudget = %s, want the full 5s", got)
	}
	if got := bindBudget(time.Now().Add(250 * time.Millisecond)); got <= 0 || got > 250*time.Millisecond {
		t.Errorf("near deadline: bindBudget = %s, want (0, 250ms]", got)
	}
	if got := bindBudget(time.Now().Add(-time.Second)); got > 0 {
		t.Errorf("expired deadline: bindBudget = %s, want <= 0 so the scan ends", got)
	}
}
