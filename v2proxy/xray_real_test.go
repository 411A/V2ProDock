package main

// Real-xray validation: exercises the PRODUCTION code paths
// (renderXrayConfig -> launchXray -> TestProxyQuick -> selector start/switch,
// watchdog orphan accounting) against a GENUINE xray binary instead of a stub.
//
// Topology (all loopback, no internet required):
//
//	[local HTTP target] <-freedom- [server xray: vmess in] <-vmess- [client xray: socks in]
//
// Run: V2PRODOCK_REAL_XRAY=/path/to/xray go test -run TestRealXray -v .
// Skipped otherwise (CI without the binary stays hermetic).

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const realTestUUID = "123e4567-e89b-12d3-a456-426614174000"

func needRealXray(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode: real-xray validation skipped")
	}
	p := os.Getenv("V2PRODOCK_REAL_XRAY")
	if p == "" {
		t.Skip("V2PRODOCK_REAL_XRAY not set: real-xray validation skipped")
	}
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		t.Skipf("real xray binary not usable at %q", p)
	}
	return p
}

func startLocalHTTPTarget(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return "http://" + addr + "/"
}

func startRealServer(t *testing.T, bin string, port int) *os.Process {
	t.Helper()
	cfg := fmt.Sprintf(`{"log":{"loglevel":"warning"},"inbounds":[{"port":%d,"listen":"127.0.0.1","protocol":"vmess","settings":{"clients":[{"id":%q}]}}],"outbounds":[{"protocol":"freedom"}]}`,
		port, realTestUUID)
	path := filepath.Join(t.TempDir(), "srv.json")
	if err := os.WriteFile(path, []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "run", "-c", path)
	cmd.Stdout = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("server xray start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	deadline := time.Now().Add(10 * time.Second)
	for !tcpOpen(fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("server xray never bound")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cmd.Process
}

func realOutbound(serverPort int) []byte {
	return []byte(fmt.Sprintf(
		`{"protocol":"vmess","settings":{"vnext":[{"address":"127.0.0.1","port":%d,"users":[{"id":%q,"alterId":0}]}]},"streamSettings":{"network":"tcp"}}`,
		serverPort, realTestUUID))
}

func TestRealXrayProbeVerdicts(t *testing.T) {
	bin := needRealXray(t)
	target := startLocalHTTPTarget(t)
	srvPort := freeLoopbackPort(t)
	srv := startRealServer(t, bin, srvPort)

	dir := t.TempDir()
	cp, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "xray"), cp, 0755); err != nil {
		t.Fatal(err)
	}

	sel := NewProxySelector(dir, target, freeLoopbackPort(t), freeLoopbackPort(t), time.Minute)

	// 1. Renderer output must be accepted by the genuine binary, and the
	//    quick probe through it must report WORKING.
	good := ProxyConfig{Name: "good", Raw: "real-good", Endpoint: "real-good:1", XrayCfg: realOutbound(srvPort)}
	cfgPath, err := sel.renderXrayConfig(good, sel.SOCKSPort(), 0)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	cmd, err := launchXray(dir, cfgPath)
	if err != nil {
		t.Fatalf("genuine xray rejected rendered config: %v", err)
	}
	defer stopXrayCmdPort(cmd, sel.SOCKSPort())
	if !waitForPort(sel.SOCKSPort(), 5*time.Second) {
		t.Fatal("genuine xray never bound SOCKS port")
	}
	if res := TestProxyQuick(fmt.Sprintf("127.0.0.1:%d", sel.SOCKSPort()), target); !res.Working {
		t.Fatalf("quick probe through REAL xray must work, got %v", res.Error)
	}

	// 2. Dead upstream behind an OPEN SOCKS port must report NOT working
	//    (TCP-open is never a health verdict), and stay bounded.
	deadSock := freeLoopbackPort(t)
	closedUpstream := freeLoopbackPort(t) // nothing listens: upstream refused
	dead := ProxyConfig{Name: "dead", Raw: "real-dead", Endpoint: "real-dead:1", XrayCfg: realOutbound(closedUpstream)}
	deadCfg, err := sel.renderXrayConfig(dead, deadSock, 99)
	if err != nil {
		t.Fatalf("render dead: %v", err)
	}
	defer os.Remove(deadCfg)
	deadCmd, err := launchXray(dir, deadCfg)
	if err != nil {
		t.Fatalf("launch dead-upstream client: %v", err)
	}
	defer stopXrayCmdPort(deadCmd, deadSock)
	if !waitForPort(deadSock, 5*time.Second) {
		t.Fatal("dead-upstream xray never bound (expected: bound, tunnel dead)")
	}
	if !tcpOpen(fmt.Sprintf("127.0.0.1:%d", deadSock), 300*time.Millisecond) {
		t.Fatal("precondition broken: SOCKS port must be TCP-open here")
	}
	start := time.Now()
	res := TestProxyQuick(fmt.Sprintf("127.0.0.1:%d", deadSock), target)
	if res.Working {
		t.Fatal("probe through dead upstream must NOT report working")
	}
	if el := time.Since(start); el > quickProbeTimeout+5*time.Second {
		t.Fatalf("dead-tunnel probe took %s, budget is %s", el, quickProbeTimeout)
	}

	// 3. Killing the server must flip the good chain to NOT working:
	//    the verdict follows the tunnel, not the local process/port.
	if err := srv.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = srv.Wait()
	if res := TestProxyQuick(fmt.Sprintf("127.0.0.1:%d", sel.SOCKSPort()), target); res.Working {
		t.Fatal("probe must fail after server death")
	}
}

// realTLSOutbound wraps realOutbound with a TLS streamSettings marker so the
// renderer attaches the fragment chain. The loopback test server speaks
// plaintext, so this config is for LOAD + CHAIN-WIRING validation only (the
// binary must accept it); end-to-end TLS runs in production, not loopback.
// NOTE: no tlsSettings needed — and "allowInsecure" must NEVER appear: Xray
// 26.x removed it at load time ("migrated to pinnedPeerCertSha256").
func realTLSOutbound(serverPort int) []byte {
	return []byte(fmt.Sprintf(
		`{"protocol":"vmess","settings":{"vnext":[{"address":"127.0.0.1","port":%d,"users":[{"id":%q,"alterId":0}]}]},"streamSettings":{"network":"tcp","security":"tls"}}`,
		serverPort, realTestUUID))
}

func copyRealXray(t *testing.T, bin, dir string) {
	t.Helper()
	cp, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "xray"), cp, 0755); err != nil {
		t.Fatal(err)
	}
}

func TestRealXrayFragmentChain(t *testing.T) {
	bin := needRealXray(t)
	target := startLocalHTTPTarget(t)
	srvPort := freeLoopbackPort(t)
	startRealServer(t, bin, srvPort)

	dir := t.TempDir()
	copyRealXray(t, bin, dir)
	t.Setenv("XRAY_FRAGMENT", "1")

	sel := NewProxySelector(dir, target, freeLoopbackPort(t), freeLoopbackPort(t), time.Minute)

	// 1. TLS-marked upstream: the genuine binary must ACCEPT the rendered
	//    fragment chain (sockopt.dialerProxy + freedom carrier). A wrong
	//    schema dies here at load ("unknown config id" / bad settings).
	tlsCfg := ProxyConfig{Name: "tls", Raw: "real-tls", Endpoint: "real-tls:1", XrayCfg: realTLSOutbound(srvPort)}
	cfgPath, err := sel.renderXrayConfig(tlsCfg, sel.SOCKSPort(), 0)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	defer os.Remove(cfgPath)
	cmd, err := launchXray(dir, cfgPath)
	if err != nil {
		t.Fatalf("genuine xray rejected the fragment chain: %v", err)
	}
	defer stopXrayCmdPort(cmd, sel.SOCKSPort())
	if !waitForPort(sel.SOCKSPort(), 5*time.Second) {
		t.Fatal("fragment-chained xray never bound")
	}

	// 2. Plaintext upstream through the SAME renderer with fragment on: no
	//    chain is attached (nothing to gain), and the tunnel proxies fully.
	plain := ProxyConfig{Name: "plain", Raw: "real-plain", Endpoint: "real-plain:1", XrayCfg: realOutbound(srvPort)}
	plainSock := freeLoopbackPort(t)
	plainCfg, err := sel.renderXrayConfig(plain, plainSock, 7)
	if err != nil {
		t.Fatalf("render plain: %v", err)
	}
	defer os.Remove(plainCfg)
	plainCmd, err := launchXray(dir, plainCfg)
	if err != nil {
		t.Fatalf("launch plain: %v", err)
	}
	defer stopXrayCmdPort(plainCmd, plainSock)
	if !waitForPort(plainSock, 5*time.Second) {
		t.Fatal("plain xray never bound")
	}
	if res := TestProxyQuick(fmt.Sprintf("127.0.0.1:%d", plainSock), target); !res.Working {
		t.Fatalf("fragment-enabled renderer must still proxy plaintext, got %v", res.Error)
	}
}

func TestRealXrayParallelSwitch(t *testing.T) {
	bin := needRealXray(t)
	target := startLocalHTTPTarget(t)
	srvPort := freeLoopbackPort(t)
	startRealServer(t, bin, srvPort)

	dir := t.TempDir()
	copyRealXray(t, bin, dir)

	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := NewProxySelector(dir, target, socks, httpP, time.Minute)
	closed1, closed2 := freeLoopbackPort(t), freeLoopbackPort(t)
	s.UpdateConfigs([]ProxyConfig{
		{Name: "dead1", Raw: "rp-dead1", Endpoint: "rp-dead1:1", XrayCfg: realOutbound(closed1)},
		{Name: "dead2", Raw: "rp-dead2", Endpoint: "rp-dead2:1", XrayCfg: realOutbound(closed2)},
		{Name: "good", Raw: "rp-good", Endpoint: "rp-good:1", XrayCfg: realOutbound(srvPort)},
	})
	// Seed serving state on a dead config, then switch: the parallel search
	// must land the good one, bounded, with exactly one child left.
	if err := s.startXray(0); err != nil {
		t.Fatalf("seed start: %v", err)
	}
	s.mu.Lock()
	s.activeIndex = 0
	s.mu.Unlock()

	start := time.Now()
	if err := s.SwitchToNextExcluding(nil); err != nil {
		t.Fatalf("parallel switch with real xray failed: %v", err)
	}
	if el := time.Since(start); el > switchBudget {
		t.Fatalf("parallel switch took %s, budget is %s", el, switchBudget)
	}
	if got := s.ActiveConfig(); got == nil || got.Key() != "rp-good:1" {
		t.Fatalf("expected active rp-good:1, got %+v", got)
	}
	if n := len(listXrayPIDs(dir)); n != 1 {
		t.Fatalf("expected exactly 1 real xray child after switch, found %d", n)
	}
	if !s.HealthCheck() {
		t.Fatal("switched instance must be healthy")
	}
}

func TestRealXrayAggregate(t *testing.T) {
	bin := needRealXray(t)
	target := startLocalHTTPTarget(t)
	srvPort := freeLoopbackPort(t)
	startRealServer(t, bin, srvPort)

	dir := t.TempDir()
	copyRealXray(t, bin, dir)

	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := NewProxySelector(dir, target, socks, httpP, time.Minute)
	s.UpdateConfigs([]ProxyConfig{
		{Name: "good", Raw: "ra-good", Endpoint: "ra-good:1", XrayCfg: realOutbound(srvPort)},
	})
	if err := s.StartWithBest(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := &ProxyManager{
		instances: []*ProxySelector{s},
		statuses:  []InstanceStatus{{Index: 0, Status: "ok", LatMs: s.LastLatency().Milliseconds()}},
	}
	ap := freeAggPort(t)
	startAggregator(m, ap, 0)
	addr := fmt.Sprintf("127.0.0.1:%d", ap)
	waitAggTCP(t, addr)
	if res := TestProxyQuick(addr, target); !res.Working {
		t.Fatalf("aggregate over real xray must serve, got %v", res.Error)
	}
}

func TestRealXraySelectorLifecycle(t *testing.T) {
	bin := needRealXray(t)
	target := startLocalHTTPTarget(t)
	srvPort := freeLoopbackPort(t)
	startRealServer(t, bin, srvPort)

	dir := t.TempDir()
	cp, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "xray"), cp, 0755); err != nil {
		t.Fatal(err)
	}

	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := NewProxySelector(dir, target, socks, httpP, time.Minute)
	closedUpstream := freeLoopbackPort(t)
	s.UpdateConfigs([]ProxyConfig{
		{Name: "broken", Raw: "r-broken", Endpoint: "r-broken:1", XrayCfg: []byte(`{invalid`)},
		{Name: "dead", Raw: "r-dead", Endpoint: "r-dead:1", XrayCfg: realOutbound(closedUpstream)},
		{Name: "good", Raw: "r-good", Endpoint: "r-good:1", XrayCfg: realOutbound(srvPort)},
	})

	start := time.Now()
	if err := s.StartWithBest(); err != nil {
		t.Fatalf("StartWithBest with real xray failed: %v", err)
	}
	if el := time.Since(start); el > 60*time.Second {
		t.Fatalf("real-xray populate took %s, must stay bounded", el)
	}
	if got := s.ActiveConfig(); got == nil || got.Key() != "r-good:1" {
		t.Fatalf("expected active r-good:1, got %+v", got)
	}
	if n := len(listXrayPIDs(dir)); n != 1 {
		t.Fatalf("expected exactly 1 real xray child, found %d", n)
	}
	if !s.HealthCheck() {
		t.Fatal("HealthCheck through real xray must pass")
	}

	// Refresh that drops the active key must re-anchor (not serve stale index).
	s.UpdateConfigs([]ProxyConfig{
		{Name: "dead", Raw: "r-dead", Endpoint: "r-dead:1", XrayCfg: realOutbound(closedUpstream)},
	})
	if got := s.ActiveConfig(); got != nil {
		t.Fatalf("vanished active must read nil after re-anchor, got %+v", got)
	}
}
