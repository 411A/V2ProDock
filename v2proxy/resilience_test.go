package main

// Regression tests for the failover/process-lifecycle hardening.
// All tests are hermetic: loopback TCP only, no subscriptions, no xray binary.

import (
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---- switchOrder: every candidate at most once, always terminates ----

func TestSwitchOrderTerminates(t *testing.T) {
	cases := []struct {
		n, start, old int
		want          []int
	}{
		{5, 0, -1, []int{0, 1, 2, 3, 4}}, // never-started: visit all, no infinite loop
		{3, 2, 1, []int{2, 0}},           // wrap, skip old
		{1, 0, 0, []int{}},               // single config, only old: nothing to try
		{4, 0, 3, []int{0, 1, 2}},
		{4, 3, 3, []int{0, 1, 2}}, // start wraps to 0
		{0, 0, -1, nil},
	}
	for _, c := range cases {
		got := switchOrder(c.n, c.start, c.old)
		if len(got) != len(c.want) {
			t.Fatalf("switchOrder(%d,%d,%d) = %v, want %v", c.n, c.start, c.old, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("switchOrder(%d,%d,%d) = %v, want %v", c.n, c.start, c.old, got, c.want)
			}
		}
	}
}

// The old loop `for i := start; i != old` spun forever for old == -1.
// Simulate bounded iteration: switchOrder output must be finite and unique.
func TestSwitchOrderFiniteForUnstarted(t *testing.T) {
	for n := 1; n < 64; n++ {
		order := switchOrder(n, 0, -1)
		if len(order) != n {
			t.Fatalf("n=%d: expected %d visits, got %d", n, n, len(order))
		}
		seen := map[int]bool{}
		for _, i := range order {
			if seen[i] {
				t.Fatalf("n=%d: index %d visited twice", n, i)
			}
			seen[i] = true
		}
	}
}

// ---- UpdateConfigs re-anchors the active index by key ----

func TestUpdateConfigsReanchorsByKey(t *testing.T) {
	mk := func(name string) ProxyConfig { return ProxyConfig{Name: name, Raw: name} }
	s := &ProxySelector{configs: []ProxyConfig{mk("A"), mk("B"), mk("C")}, activeIndex: 2} // C
	s.UpdateConfigs([]ProxyConfig{mk("C"), mk("X"), mk("A")})
	if s.activeIndex != 0 {
		t.Fatalf("expected re-anchor to C at 0, got %d", s.activeIndex)
	}
	// C vanished from the new pool -> -1 so callers switch instead of
	// serving the wrong config at a stale index.
	s.UpdateConfigs([]ProxyConfig{mk("X"), mk("Y")})
	if s.activeIndex != -1 {
		t.Fatalf("vanished active must reset to -1, got %d", s.activeIndex)
	}
}

// ---- stopXrayCmdPort: child reaped, port freed, returns promptly ----

func TestStopXrayReapsChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses unix sleep")
	}
	// Free loopback port: waitForPortFree must return instantly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	cmd := exec.Command("sleep", "30")
	// Mirror production: every managed child owns its process group, which is
	// what the group kill targets (a bare child shares the test's group and
	// negative-PID signalling would ESRCH).
	isolateChild(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("no sleep binary: %v", err)
	}
	pid := cmd.Process.Pid
	start := time.Now()
	stopXrayCmdPort(cmd, port)
	if el := time.Since(start); el > xrayStopWait+5*time.Second {
		t.Fatalf("stop took %s, expected prompt group kill", el)
	}
	if processAlive(cmd.Process) {
		t.Fatalf("pid %d still alive after stop", pid)
	}
}

// ---- Miniature SOCKS5 origin for probe tests ----

// serveSocks204 speaks just enough SOCKS5 (no-auth CONNECT) then answers any
// HTTP request with 204. mode="ok" answers, mode="hang" completes SOCKS then
// goes silent (tests the client-side timeout budget).
func serveSocks204(t *testing.T, mode string) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleMiniSocks(c, mode)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func readN(c net.Conn, n int) ([]byte, bool) {
	buf := make([]byte, n)
	got := 0
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for got < n {
		m, err := c.Read(buf[got:])
		if err != nil {
			return nil, false
		}
		got += m
	}
	return buf, true
}

func handleMiniSocks(c net.Conn, mode string) {
	defer c.Close()
	greet, ok := readN(c, 2)
	if !ok || greet[0] != 0x05 {
		return
	}
	if _, ok := readN(c, int(greet[1])); !ok {
		return
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	hdr, ok := readN(c, 4)
	if !ok || hdr[1] != 0x01 {
		return
	}
	switch hdr[3] {
	case 0x01:
		if _, ok := readN(c, 4); !ok {
			return
		}
	case 0x03:
		l, ok := readN(c, 1)
		if !ok {
			return
		}
		if _, ok := readN(c, int(l[0])); !ok {
			return
		}
	case 0x04:
		if _, ok := readN(c, 16); !ok {
			return
		}
	default:
		return
	}
	if _, ok := readN(c, 2); !ok {
		return
	}
	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	if mode == "hang" {
		time.Sleep(30 * time.Second)
		return
	}
	// Slurp the HTTP request head, then 204.
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 256)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for !strings.Contains(string(buf), "\r\n\r\n") && len(buf) < 4096 {
		m, err := c.Read(tmp)
		if err != nil {
			return
		}
		buf = append(buf, tmp[:m]...)
	}
	_, _ = c.Write([]byte("HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
}

func TestQuickProbeAgainstMiniSocks(t *testing.T) {
	addr, done := serveSocks204(t, "ok")
	defer done()
	res := TestProxyQuick(addr, "http://probe.invalid/generate_204")
	if !res.Working {
		t.Fatalf("expected working probe, got %v", res.Error)
	}
	if res.Latency > 5*time.Second {
		t.Fatalf("loopback probe took %s, expected ms", res.Latency)
	}
}

func TestQuickProbeDeadPortFailsFast(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // now refused
	start := time.Now()
	res := TestProxyQuick(addr, "http://probe.invalid/")
	if res.Working {
		t.Fatal("expected failure on closed port")
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("dead-port probe took %s, must fail fast", el)
	}
}

func TestQuickProbeHungUpstreamBounded(t *testing.T) {
	addr, done := serveSocks204(t, "hang")
	defer done()
	start := time.Now()
	res := TestProxyQuick(addr, "http://probe.invalid/")
	if res.Working {
		t.Fatal("expected failure on hung upstream")
	}
	if el := time.Since(start); el > quickProbeTimeout+5*time.Second {
		t.Fatalf("hung probe took %s, budget is %s", el, quickProbeTimeout)
	}
}

// ---- Probe URLs must stay HTTPS (DPI kills plain HTTP) ----

func TestProbeURLsAreHTTPS(t *testing.T) {
	for _, u := range append([]string{probeURL, quickFallbackURL, defaultHealthCheckURL}, fallbackHealthURLs...) {
		if !strings.HasPrefix(u, "https://") {
			t.Fatalf("probe URL must be HTTPS, got %s", u)
		}
	}
	if !strings.Contains(fallbackHealthURLs[1], "/generate_204") {
		t.Fatalf("cloudflare fallback needs /generate_204 path, got %s", fallbackHealthURLs[1])
	}
}

// ---- Watchdog cmdline matcher: no false positives/negatives ----

func TestCmdlineIsManaged(t *testing.T) {
	dir := "/root/xray"
	good := "xray\x00run\x00-c\x00" + dir + "/config-27019.json\x00"
	if !cmdlineIsManaged(good, dir) {
		t.Fatal("should match our serving xray")
	}
	rot := "/usr/local/bin/xray\x00run\x00-c\x00" + dir + "/config-rot-4567.json\x00"
	if !cmdlineIsManaged(rot, dir) {
		t.Fatal("should match temp rotation probe xray")
	}
	// Shebang/wrapper launch: interpreter at argv[0], xray script at argv[1].
	env := "python3\x00/tmp/w/xray\x00run\x00-c\x00" + dir + "/config-27019.json\x00"
	if !cmdlineIsManaged(env, dir) {
		t.Fatal("should match interpreter-fronted xray launch")
	}
	bad := []string{
		"xray\x00run\x00-c\x00/etc/other/config-1.json\x00", // foreign dir
		"hevsocks\x00-tunnel\x00",                           // other binary
		"xray\x00run\x00",                                   // too short
		"",
	}
	for _, b := range bad {
		if cmdlineIsManaged(b, dir) {
			t.Fatalf("must not match %q", b)
		}
	}
}

// ---- Budgets sane (failover bounded but not trigger-happy) ----

func TestBudgetsSane(t *testing.T) {
	if switchBudget < 10*time.Second || switchBudget > 5*time.Minute {
		t.Fatalf("switchBudget out of sane range: %s", switchBudget)
	}
	if rotateMaxCandidates < 1 || rotateMaxCandidates > 10 {
		t.Fatalf("rotateMaxCandidates out of sane range: %d", rotateMaxCandidates)
	}
	if quickProbeTimeout > healthCheckTimeout {
		t.Fatalf("quick probe (%s) must be cheaper than full check (%s)", quickProbeTimeout, healthCheckTimeout)
	}
	fmt.Println("budgets ok")
}
