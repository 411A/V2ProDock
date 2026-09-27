package main

// Stability-layer tests: fast parallel failover, stable aggregate endpoints,
// and opt-in DPI fragmentation. Hermetic except the stub-xray search tests
// (linux + python3, same gate as e2e_test.go) and the real-xray fragment
// tests in xray_real_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

func TestSwitchWorkerCountClamp(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"", switchWorkersDefault},
		{"abc", switchWorkersDefault},
		{"0", 1},
		{"-4", 1},
		{"1", 1},
		{"5", 5},
		{"99", switchWorkersMax},
	} {
		t.Setenv("SWITCH_WORKERS", c.in)
		if got := switchWorkerCount(); got != c.want {
			t.Fatalf("SWITCH_WORKERS=%q -> %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFragmentEnabledParsing(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "on", "yes", " YES "} {
		t.Setenv("XRAY_FRAGMENT", v)
		if !fragmentEnabled() {
			t.Fatalf("XRAY_FRAGMENT=%q must enable", v)
		}
	}
	for _, v := range []string{"", "0", "off", "no", "2"} {
		t.Setenv("XRAY_FRAGMENT", v)
		if fragmentEnabled() {
			t.Fatalf("XRAY_FRAGMENT=%q must not enable", v)
		}
	}
}

func TestUpstreamUsesTLS(t *testing.T) {
	mk := func(sec string) map[string]any {
		return map[string]any{"streamSettings": map[string]any{"security": sec}}
	}
	for _, sec := range []string{"tls", "TLS", "reality", " Reality "} {
		if !upstreamUsesTLS(mk(sec)) {
			t.Fatalf("security=%q must count as TLS", sec)
		}
	}
	for _, sec := range []string{"", "none", "plaintext"} {
		if upstreamUsesTLS(mk(sec)) {
			t.Fatalf("security=%q must not count as TLS", sec)
		}
	}
	if upstreamUsesTLS(map[string]any{}) {
		t.Fatal("missing streamSettings must not count as TLS")
	}
	if upstreamUsesTLS(map[string]any{"streamSettings": "tls"}) {
		t.Fatal("malformed streamSettings must not count as TLS")
	}
}

func TestHasCustomDialChain(t *testing.T) {
	chained := map[string]any{"streamSettings": map[string]any{"sockopt": map[string]any{"dialerProxy": "x"}}}
	if !hasCustomDialChain(chained) {
		t.Fatal("existing dialerProxy must be detected")
	}
	proxied := map[string]any{"proxySettings": map[string]any{"tag": "y"}}
	if !hasCustomDialChain(proxied) {
		t.Fatal("existing proxySettings must be detected")
	}
	if hasCustomDialChain(map[string]any{"streamSettings": map[string]any{}}) {
		t.Fatal("clean outbound must not count as chained")
	}
}

func TestIsPlainHTTP(t *testing.T) {
	if !isPlainHTTP("http://api.ipify.org") {
		t.Fatal("http URL must be flagged")
	}
	if !isPlainHTTP(" HTTP://x ") {
		t.Fatal("case/space must be tolerated")
	}
	for _, u := range []string{"https://a", "", "socks5://a", "httpbin"} {
		if isPlainHTTP(u) {
			t.Fatalf("%q must not be flagged", u)
		}
	}
}

func TestAggregatePortResolve(t *testing.T) {
	t.Setenv("AGGREGATE_SOCKS_PORT", "")
	if got := aggregatePort("AGGREGATE_SOCKS_PORT", 27017); got != 27017 {
		t.Fatalf("unset must yield default, got %d", got)
	}
	t.Setenv("AGGREGATE_SOCKS_PORT", "0")
	if got := aggregatePort("AGGREGATE_SOCKS_PORT", 27017); got != 0 {
		t.Fatalf("0 must disable, got %d", got)
	}
	t.Setenv("AGGREGATE_SOCKS_PORT", "27099")
	if got := aggregatePort("AGGREGATE_SOCKS_PORT", 27017); got != 27099 {
		t.Fatalf("override must win, got %d", got)
	}
	for _, bad := range []string{"abc", "-5", "99999", "12x"} {
		t.Setenv("AGGREGATE_SOCKS_PORT", bad)
		if got := aggregatePort("AGGREGATE_SOCKS_PORT", 27017); got != 27017 {
			t.Fatalf("bad %q must fall back to default, got %d", bad, got)
		}
	}
}

func TestPickAggregatePorts(t *testing.T) {
	// Default constants honor the deployment contract: distinct from each
	// other and from the API port, inside the published 27000-27100 range.
	seen := map[int]bool{defaultAggSocksPort: true}
	if seen[defaultAggHttpPort] {
		t.Fatal("aggregate SOCKS/HTTP defaults must differ")
	}
	for _, p := range []int{defaultAggSocksPort, defaultAggHttpPort} {
		if p == defaultAPIPort || p < 27000 || p > aggPublishedMax {
			t.Fatalf("aggregate default %d violates deployment contract", p)
		}
	}
	// Busy candidate (held listener inside the managed range): scans upward
	// to a free port. (Ephemeral :0 ports live above maxPort and are
	// correctly out of scan range — a past revision of this test tripped
	// on exactly that.)
	ln, busy := holdPortInRange(t, 20000, 26000)
	defer ln.Close()
	got, _ := pickAggregatePorts(busy, 0, 27018, map[int]bool{})
	if got <= busy || got > maxPort {
		t.Fatalf("busy port %d must scan upward within range, got %d", busy, got)
	}
	if tcpOpen(fmt.Sprintf("127.0.0.1:%d", got), 200*time.Millisecond) {
		t.Fatalf("picked port %d must be free", got)
	}
	// used-map + API collisions are skipped the same way.
	got, _ = pickAggregatePorts(busy, 0, busy, map[int]bool{busy: true, busy + 1: true})
	if got == busy || got == busy+1 {
		t.Fatalf("used/API ports must be skipped, got %d", got)
	}
	// Disabled stays disabled; sub-1024 candidates scan from 1024.
	s, h := pickAggregatePorts(0, 0, 27018, map[int]bool{})
	if s != 0 || h != 0 {
		t.Fatalf("0 must disable, got %d/%d", s, h)
	}
	s, _ = pickAggregatePorts(80, 0, 27018, map[int]bool{})
	if s != 0 && (s < 1024 || tcpOpen(fmt.Sprintf("127.0.0.1:%d", s), 200*time.Millisecond)) {
		t.Fatalf("privileged candidate must scan from 1024 to a free port, got %d", s)
	}
}

// renderOutbound parses a freshly rendered serving config back for assertions.
func renderOutbound(t *testing.T, dir string, xrayCfg string) map[string]any {
	t.Helper()
	sel := NewProxySelector(dir, "http://probe.invalid/", 27991, 27992, time.Minute)
	path, err := sel.renderXrayConfig(ProxyConfig{Name: "n", Raw: "r", XrayCfg: []byte(xrayCfg)}, 27991, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var full map[string]any
	if err := json.Unmarshal(raw, &full); err != nil {
		t.Fatal(err)
	}
	outs, ok := full["outbounds"].([]any)
	if !ok || len(outs) == 0 {
		t.Fatal("rendered config has no outbounds")
	}
	up, ok := outs[0].(map[string]any)
	if !ok {
		t.Fatal("first outbound malformed")
	}
	return map[string]any{"up": up, "all": outs}
}

func TestRenderFragmentChainTLS(t *testing.T) {
	t.Setenv("XRAY_FRAGMENT", "1")
	got := renderOutbound(t, t.TempDir(),
		`{"protocol":"vless","settings":{},"streamSettings":{"network":"tcp","security":"tls"}}`)
	up := got["up"].(map[string]any)
	so, ok := up["streamSettings"].(map[string]any)["sockopt"].(map[string]any)
	if !ok || so["dialerProxy"] != fragmentOutTag {
		t.Fatalf("TLS upstream must chain at %q, got %v", fragmentOutTag, so)
	}
	found := false
	for _, o := range got["all"].([]any) {
		m := o.(map[string]any)
		if m["tag"] == fragmentOutTag {
			found = true
			if m["protocol"] != "freedom" {
				t.Fatalf("carrier must be freedom, got %v", m["protocol"])
			}
			frag, ok := m["settings"].(map[string]any)["fragment"].(map[string]any)
			if !ok || frag["packets"] != fragmentPackets || frag["length"] != fragmentLength || frag["interval"] != fragmentInterval {
				t.Fatalf("carrier fragment settings wrong: %v", m["settings"])
			}
		}
	}
	if !found {
		t.Fatal("fragment carrier outbound missing")
	}
}

func TestRenderFragmentSkipped(t *testing.T) {
	t.Setenv("XRAY_FRAGMENT", "1")
	// Plaintext: no chain, no carrier.
	got := renderOutbound(t, t.TempDir(),
		`{"protocol":"vmess","settings":{},"streamSettings":{"network":"tcp"}}`)
	up := got["up"].(map[string]any)
	if ss, ok := up["streamSettings"].(map[string]any)["sockopt"]; ok {
		t.Fatalf("plaintext must not gain sockopt, got %v", ss)
	}
	for _, o := range got["all"].([]any) {
		if o.(map[string]any)["tag"] == fragmentOutTag {
			t.Fatal("plaintext must not gain a carrier")
		}
	}
	// Pre-chained TLS: user intent wins, no double chain.
	got = renderOutbound(t, t.TempDir(),
		`{"protocol":"vless","settings":{},"streamSettings":{"security":"tls","sockopt":{"dialerProxy":"mine"}}}`)
	up = got["up"].(map[string]any)
	so := up["streamSettings"].(map[string]any)["sockopt"].(map[string]any)
	if so["dialerProxy"] != "mine" {
		t.Fatalf("existing chain must be preserved, got %v", so)
	}
}

func TestRenderNoFragmentByDefault(t *testing.T) {
	t.Setenv("XRAY_FRAGMENT", "")
	got := renderOutbound(t, t.TempDir(),
		`{"protocol":"vless","settings":{},"streamSettings":{"security":"tls"}}`)
	up := got["up"].(map[string]any)
	if _, ok := up["streamSettings"].(map[string]any)["sockopt"]; ok {
		t.Fatal("fragment off by default: no sockopt may appear")
	}
}

// ---- pickBestBackend ----

func backendManager(statuses []InstanceStatus, ports ...int) *ProxyManager {
	insts := make([]*ProxySelector, 0, len(statuses))
	for i := range statuses {
		insts = append(insts, NewProxySelector("/nonexistent", "http://probe.invalid/", ports[2*i], ports[2*i+1], time.Minute))
	}
	return &ProxyManager{instances: insts, statuses: statuses}
}

func TestPickBestBackend(t *testing.T) {
	m := backendManager([]InstanceStatus{
		{Index: 0, Status: "ok", LatMs: 300},
		{Index: 1, Status: "ok", LatMs: 50},
		{Index: 2, Status: "down"},
	}, 27801, 27811, 27802, 27812, 27803, 27813)
	if got, ok := m.pickBestBackend(false); !ok || got != "127.0.0.1:27802" {
		t.Fatalf("socks best = %v,%v want 127.0.0.1:27802", got, ok)
	}
	if got, ok := m.pickBestBackend(true); !ok || got != "127.0.0.1:27812" {
		t.Fatalf("http best = %v,%v want 127.0.0.1:27812", got, ok)
	}
	m2 := backendManager([]InstanceStatus{{Index: 0, Status: "down"}}, 27851, 27861)
	if _, ok := m2.pickBestBackend(false); ok {
		t.Fatal("no alive must report false")
	}
	m3 := backendManager([]InstanceStatus{{Index: 7, Status: "ok", LatMs: 1}}, 27852, 27862)
	if _, ok := m3.pickBestBackend(false); ok {
		t.Fatal("out-of-range index must report false")
	}
}

// ---- aggregate relay, hermetic (echo backends, no xray) ----

func serveEcho(t *testing.T, marker string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = c.Write([]byte(marker))
				buf := make([]byte, 256)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					if _, err := c.Write(buf[:n]); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func freeAggPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

func waitAggTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !tcpOpen(addr, 200*time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("aggregate %s never bound", addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// holdPortInRange binds and holds one free port in [lo, hi] for collision tests.
func holdPortInRange(t *testing.T, lo, hi int) (net.Listener, int) {
	t.Helper()
	for p := lo; p <= hi; p++ {
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p)); err == nil {
			return ln, p
		}
	}
	t.Fatalf("no free port in [%d,%d]", lo, hi)
	return nil, 0
}

func splitPort(t *testing.T, addr string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	fmt.Sscanf(p, "%d", &n)
	return n
}

func TestAggregateRelayRoundTrip(t *testing.T) {
	echoPort := splitPort(t, serveEcho(t, "E1"))
	m := backendManager([]InstanceStatus{{Index: 0, Status: "ok", LatMs: 5}}, echoPort, echoPort+1000)
	ap := freeAggPort(t)
	startAggregator(m, ap, 0)
	addr := fmt.Sprintf("127.0.0.1:%d", ap)
	waitAggTCP(t, addr)

	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	head, ok := readN(c, 2)
	if !ok || string(head) != "E1" {
		t.Fatalf("marker = %q, want E1", head)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	echo, ok := readN(c, 4)
	if !ok || string(echo) != "ping" {
		t.Fatalf("echo = %q", echo)
	}
}

func TestAggregateFailsFastWhenDown(t *testing.T) {
	m := backendManager([]InstanceStatus{{Index: 0, Status: "down"}}, 27871, 27881)
	ap := freeAggPort(t)
	startAggregator(m, ap, 0)
	addr := fmt.Sprintf("127.0.0.1:%d", ap)
	waitAggTCP(t, addr)
	start := time.Now()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return // refused even faster: acceptable fail-fast
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("down aggregate must not serve bytes")
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("fail-fast took %s", el)
	}
}

func TestAggregateFollowsFastest(t *testing.T) {
	fastPort := splitPort(t, serveEcho(t, "FA"))
	slowPort := splitPort(t, serveEcho(t, "SL"))
	m := backendManager([]InstanceStatus{
		{Index: 0, Status: "ok", LatMs: 500},
		{Index: 1, Status: "ok", LatMs: 20},
	}, slowPort, slowPort+1000, fastPort, fastPort+1000)
	if got, _ := m.pickBestBackend(false); got != fmt.Sprintf("127.0.0.1:%d", fastPort) {
		t.Fatalf("best must be fast backend, got %s", got)
	}
	// Fast degrades: next connection must move without any client change.
	m.markDown(1, "slow test")
	if got, _ := m.pickBestBackend(false); got != fmt.Sprintf("127.0.0.1:%d", slowPort) {
		t.Fatalf("after degrade best must be slow backend, got %s", got)
	}
}

// ---- parallel search never disturbs serving state ----

func searchCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), switchBudget)
}

func TestSearchLeavesServingAlone(t *testing.T) {
	needStub(t)
	dir := t.TempDir()
	writeStubXray(t, dir)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := NewProxySelector(dir, "http://probe.invalid/", socks, httpP, time.Minute)
	s.UpdateConfigs([]ProxyConfig{
		e2eCand("old", "e2e-old:9", "good"),
		e2eCand("bad1", "e2e-bad1:9", "dead-hang"),
		e2eCand("bad2", "e2e-bad2:9", "dead-crash"),
		e2eCand("new", "e2e-new:9", "good"),
	})
	if err := s.StartWithBest(); err != nil {
		t.Fatal(err)
	}
	beforePID := s.currentPID()
	snap := s.snapshotConfigs()
	order := switchOrder(len(snap), 1, 0)
	ctx, cancel := searchCtx()
	defer cancel()
	idx, _ := s.searchCandidates(ctx, snap, order)
	if idx < 0 {
		t.Fatal("search must find the good candidate")
	}
	if snap[idx].Key() != "e2e-new:9" && snap[idx].Key() != "e2e-old:9" {
		t.Fatalf("winner must be a good candidate, got %s", snap[idx].Key())
	}
	if got := s.currentPID(); got != beforePID {
		t.Fatalf("serving child changed mid-search (%d -> %d)", beforePID, got)
	}
	if n := stubCount(dir); n != 1 {
		t.Fatalf("search must leave exactly the serving child, found %d", n)
	}
}

func TestSearchAllDeadBounded(t *testing.T) {
	needStub(t)
	dir := t.TempDir()
	writeStubXray(t, dir)
	s := NewProxySelector(dir, "http://probe.invalid/", freeLoopbackPort(t), freeLoopbackPort(t), time.Minute)
	snap := []ProxyConfig{
		e2eCand("d1", "e2e-d1:8", "dead-hang"),
		e2eCand("d2", "e2e-d2:8", "dead-hang"),
		e2eCand("d3", "e2e-d3:8", "dead-crash"),
	}
	s.UpdateConfigs(snap)
	ctx, cancel := searchCtx()
	defer cancel()
	start := time.Now()
	idx, _ := s.searchCandidates(ctx, snap, []int{0, 1, 2})
	if el := time.Since(start); el > 60*time.Second {
		t.Fatalf("all-dead search took %s, must respect worker parallelism", el)
	}
	if idx >= 0 {
		t.Fatal("all-dead search must report -1")
	}
	if n := stubCount(dir); n != 0 {
		t.Fatalf("temp children must be reaped, found %d", n)
	}
}
