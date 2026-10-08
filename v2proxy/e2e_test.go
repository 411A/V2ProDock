package main

// End-to-end lifecycle tests with a FAKE xray binary (python stub).
// The stub reads its real rendered config file, binds the real SOCKS port,
// and behaves per upstream tag: "dead-crash" exits at once, "dead-hang"
// accepts but never answers (dead upstream), "throttled" answers 429 like a
// rate-limiting probe target, anything else serves SOCKS5->204.
// This exercises production start/stop/switch/health/bridge paths for real,
// including orphan accounting via the watchdog's own cmdline matcher.

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const stubXray = `#!/usr/bin/env python3
import contextlib
import json
import socket
import sys
import threading
import time

# Deliberately flat and branchy: this is a SOCKS5 protocol fixture, and
# splitting handle() into helpers would only obscure the wire sequence it
# exists to reproduce. See AGENT.md for the ruff exemption (C901 only).
def handle(c, mode):
    try:
        c.settimeout(5)
        data = c.recv(2)
        if len(data) < 2 or data[0] != 5:
            return
        n = data[1]
        got = b""
        while len(got) < n:
            chunk = c.recv(n - len(got))
            if not chunk:
                return
            got += chunk
        c.sendall(b"\x05\x00")
        hdr = b""
        while len(hdr) < 4:
            chunk = c.recv(4 - len(hdr))
            if not chunk:
                return
            hdr += chunk
        if hdr[1] != 1:
            return
        atyp = hdr[3]
        if atyp == 1:
            need = 4
        elif atyp == 3:
            nbytes = c.recv(1)
            if not nbytes:
                return
            need = nbytes[0]
        elif atyp == 4:
            need = 16
        else:
            return
        got = b""
        while len(got) < need:
            chunk = c.recv(need - len(got))
            if not chunk:
                return
            got += chunk
        prt = b""
        while len(prt) < 2:
            chunk = c.recv(2 - len(prt))
            if not chunk:
                return
            prt += chunk
        c.sendall(b"\x05\x00\x00\x01\x00\x00\x00\x00\x00\x00")
        if mode == "dead-hang":
            time.sleep(60)
            return
        buf = b""
        c.settimeout(5)
        while b"\r\n\r\n" not in buf and len(buf) < 4096:
            chunk = c.recv(256)
            if not chunk:
                return
            buf += chunk
        if mode == "throttled":
            c.sendall(b"HTTP/1.1 429 Too Many Requests\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
            return
        c.sendall(b"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
    except Exception:
        pass
    finally:
        with contextlib.suppress(Exception):
            c.close()

def main():
    with open(sys.argv[sys.argv.index("-c") + 1]) as fh:
        cfg = json.load(fh)
    port = cfg["inbounds"][0]["port"]
    mode = cfg["outbounds"][0].get("tag", "good")
    if mode == "dead-crash":
        sys.exit(3)
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("0.0.0.0", port))
    srv.listen(64)
    while True:
        c, _ = srv.accept()
        threading.Thread(target=handle, args=(c, mode), daemon=True).start()

main()
`

func needStub(t *testing.T) {
	t.Helper()
	// Stub-count assertions rely on /proc scanning, so these run on linux only.
	if runtime.GOOS != "linux" {
		t.Skip("fake-xray e2e requires linux (/proc accounting)")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for fake-xray e2e")
	}
}

func writeStubXray(t *testing.T, dir string) {
	t.Helper()
	p := filepath.Join(dir, "xray")
	if err := os.WriteFile(p, []byte(stubXray), 0755); err != nil {
		t.Fatal(err)
	}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

func e2eCand(name, endpoint, mode string) ProxyConfig {
	return ProxyConfig{
		Name:     name,
		Raw:      name,
		Endpoint: endpoint,
		XrayCfg:  []byte(fmt.Sprintf(`{"protocol":"freedom","tag":%q}`, mode)),
	}
}

func stubCount(dir string) int {
	return len(listXrayPIDs(dir))
}

func TestLifecycleStartSwitchNoOrphans(t *testing.T) {
	needStub(t)
	dir := t.TempDir()
	writeStubXray(t, dir)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := NewProxySelector(dir, "http://probe.invalid/", socks, httpP, time.Minute)
	s.UpdateConfigs([]ProxyConfig{
		e2eCand("crash", "e2e-crash:1", "dead-crash"),
		e2eCand("hang", "e2e-hang:1", "dead-hang"),
		e2eCand("good", "e2e-good:1", "good"),
	})

	start := time.Now()
	if err := s.StartWithBest(); err != nil {
		t.Fatalf("StartWithBest failed: %v", err)
	}
	if el := time.Since(start); el > 30*time.Second {
		t.Fatalf("populate with 2 dead + 1 good took %s, must be fast", el)
	}
	if got := s.ActiveConfig(); got == nil || got.Key() != "e2e-good:1" {
		t.Fatalf("expected active e2e-good:1, got %+v", got)
	}
	if n := stubCount(dir); n != 1 {
		t.Fatalf("expected exactly 1 stub xray after start, found %d (orphans!)", n)
	}
	if !s.HealthCheck() {
		t.Fatal("HealthCheck on good stub must pass")
	}

	// Murder the good child externally: next checks must fail, switch must
	// terminate bounded (only dead candidates left) and restore-or-fail cleanly.
	for pid := range listXrayPIDs(dir) {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for stubCount(dir) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	for range healthFailThreshold {
		s.HealthCheck()
	}
	swStart := time.Now()
	err := s.SwitchToNextExcluding(nil)
	if el := time.Since(swStart); el > switchBudget+30*time.Second {
		t.Fatalf("all-dead switch took %s, budget is %s", el, switchBudget)
	}
	if err == nil {
		t.Fatal("expected switch error when every candidate is dead")
	}
	if n := stubCount(dir); n > 1 {
		t.Fatalf("expected <=1 stub xray after failed switch, found %d (orphans!)", n)
	}
}

func TestReconcileVanishedActiveRotates(t *testing.T) {
	needStub(t)
	dir := t.TempDir()
	writeStubXray(t, dir)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := NewProxySelector(dir, "http://probe.invalid/", socks, httpP, time.Minute)
	s.UpdateConfigs([]ProxyConfig{e2eCand("A", "e2e-a:1", "good")})
	if err := s.StartWithBest(); err != nil {
		t.Fatal(err)
	}
	before := s.currentPID()
	if before <= 0 {
		t.Fatal("no managed child after start")
	}
	// Refresh drops A entirely while A is actually failing: reconcile must
	// switch to B and serve it.
	prev := s.ActiveConfig()
	s.failCount = healthFailThreshold // poison: A is dying, not just rotated away
	s.UpdateConfigs([]ProxyConfig{e2eCand("B", "e2e-b:1", "good")})
	m := &ProxyManager{
		instances: []*ProxySelector{s},
		statuses:  []InstanceStatus{{Index: 0, Status: "ok"}},
		xrayDir:   dir,
	}
	m.reconcileActive(0, s, prev)
	got := s.ActiveConfig()
	if got == nil || got.Key() != "e2e-b:1" {
		t.Fatalf("expected rotation to e2e-b:1, got %+v", got)
	}
	if !s.HealthCheck() {
		t.Fatal("rotated instance must be healthy")
	}
	if n := stubCount(dir); n != 1 {
		t.Fatalf("expected exactly 1 stub xray after rotation, found %d", n)
	}
}

func TestReconcileVanishedHealthyActiveRetained(t *testing.T) {
	needStub(t)
	dir := t.TempDir()
	writeStubXray(t, dir)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := NewProxySelector(dir, "http://probe.invalid/", socks, httpP, time.Minute)
	s.UpdateConfigs([]ProxyConfig{e2eCand("A", "e2e-a:1", "good")})
	if err := s.StartWithBest(); err != nil {
		t.Fatal(err)
	}
	before := s.currentPID()
	if before <= 0 {
		t.Fatal("no managed child after start")
	}
	// Refresh drops A while A is healthy: keep serving A, same child, no
	// switch (the production churn was rotating these every refresh).
	prev := s.ActiveConfig()
	s.UpdateConfigs([]ProxyConfig{e2eCand("B", "e2e-b:1", "good")})
	m := &ProxyManager{
		instances: []*ProxySelector{s},
		statuses:  []InstanceStatus{{Index: 0, Status: "ok"}},
		xrayDir:   dir,
	}
	m.reconcileActive(0, s, prev)
	got := s.ActiveConfig()
	if got == nil || got.Key() != "e2e-a:1" {
		t.Fatalf("expected healthy active e2e-a:1 retained, got %+v", got)
	}
	if s.currentPID() != before {
		t.Fatal("retained active must not restart xray")
	}
	if n := stubCount(dir); n != 1 {
		t.Fatalf("expected exactly 1 stub xray after retain, found %d", n)
	}
}

func TestReconcileFastActiveUntouched(t *testing.T) {
	needStub(t)
	dir := t.TempDir()
	writeStubXray(t, dir)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := NewProxySelector(dir, "http://probe.invalid/", socks, httpP, time.Minute)
	s.UpdateConfigs([]ProxyConfig{e2eCand("A", "e2e-a:1", "good"), e2eCand("B", "e2e-b:1", "good")})
	if err := s.StartWithBest(); err != nil {
		t.Fatal(err)
	}
	s.HealthCheck() // record loopback-fast latency
	before := s.currentPID()
	m := &ProxyManager{
		instances: []*ProxySelector{s},
		statuses:  []InstanceStatus{{Index: 0, Status: "ok"}},
		xrayDir:   dir,
	}
	m.reconcileActive(0, s, s.ActiveConfig()) // same pool: must cost zero disruption
	if s.currentPID() != before {
		t.Fatal("fast present active must not be restarted by refresh")
	}
	if n := stubCount(dir); n != 1 {
		t.Fatalf("expected exactly 1 stub xray, found %d", n)
	}
}

// expireEgress backdates any recorded flow for these ports past the grace
// window. Ports are handed out by the OS (:0) and CAN be recycled between
// tests, so a note left behind by an earlier test could otherwise satisfy a
// later test's "no live traffic" precondition by accident — silently turning
// its assertions into no-ops.
func expireEgress(ports ...int) {
	egressNotes.mu.Lock()
	defer egressNotes.mu.Unlock()
	for _, p := range ports {
		if _, ok := egressNotes.lastOK[p]; ok {
			egressNotes.lastOK[p] = time.Now().Add(-2 * egressGrace)
		}
	}
}

// A busy instance must never be rotated for being slow: rotation is
// stopXray()+startXray() on the SAME serving ports, so it severs live
// connections (a Telegram long-poll dies mid-response -> RemoteProtocolError)
// while the proxy still reports "ok". HealthCheck's passive branch is the
// precedent; the refresh rotation path must honour the same evidence.
func TestReconcileSlowServingActiveRetained(t *testing.T) {
	needStub(t)
	dir := t.TempDir()
	writeStubXray(t, dir)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := NewProxySelector(dir, "http://probe.invalid/", socks, httpP, time.Minute)
	s.UpdateConfigs([]ProxyConfig{e2eCand("A", "e2e-a:1", "good"), e2eCand("B", "e2e-b:1", "good")})
	if err := s.StartWithBest(); err != nil {
		t.Fatal(err)
	}
	// Force the rotation-eligible branch: slow + freshly measured + serving.
	s.mu.Lock()
	s.lastLatency = rotateSlowLatency + time.Second
	s.lastProbe = time.Now()
	s.mu.Unlock()
	noteEgress(socks) // real client bytes: the instance is serving
	if !s.ServingTraffic() {
		t.Fatal("precondition: ServingTraffic must see the noted egress")
	}
	before := s.currentPID()
	m := &ProxyManager{
		instances: []*ProxySelector{s},
		statuses:  []InstanceStatus{{Index: 0, Status: "ok"}},
		xrayDir:   dir,
	}
	m.reconcileActive(0, s, s.ActiveConfig())
	if got := s.ActiveConfig(); got == nil || got.Key() != "e2e-a:1" {
		t.Fatalf("serving instance must not be rotated for slowness, got %+v", got)
	}
	if s.currentPID() != before {
		t.Fatal("serving instance must not restart xray: live connections were cut")
	}
}

// The passive branch must NOT fabricate a measurement: it keeps skipping the
// probe (that is the whole point), so the latency figure stays frozen and
// LatencyFresh must report it as stale rather than let a forgotten number
// qualify a rotation.
func TestPassiveHealthLeavesLatencyStale(t *testing.T) {
	needStub(t)
	dir := t.TempDir()
	writeStubXray(t, dir)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := NewProxySelector(dir, "http://probe.invalid/", socks, httpP, time.Minute)
	s.UpdateConfigs([]ProxyConfig{e2eCand("A", "e2e-a:1", "good")})
	if err := s.StartWithBest(); err != nil {
		t.Fatal(err)
	}
	if !s.LatencyFresh(rotateLatencyMaxAge) {
		t.Fatal("precondition: start measurement must be fresh")
	}
	noteEgress(socks)
	// Age the measurement past the window, then let the passive branch run.
	s.mu.Lock()
	s.lastProbe = time.Now().Add(-2 * rotateLatencyMaxAge)
	s.mu.Unlock()
	if !s.HealthCheck() {
		t.Fatal("serving instance must stay healthy")
	}
	if s.LatencyFresh(rotateLatencyMaxAge) {
		t.Fatal("skipped probe must not make an old measurement look fresh")
	}
}

// Stale measurement alone (instance idle again) must also block rotation:
// "slow" may not be inferred from a number nobody measured recently.
func TestReconcileStaleLatencyNotRotated(t *testing.T) {
	needStub(t)
	dir := t.TempDir()
	writeStubXray(t, dir)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := NewProxySelector(dir, "http://probe.invalid/", socks, httpP, time.Minute)
	s.UpdateConfigs([]ProxyConfig{e2eCand("A", "e2e-a:1", "good"), e2eCand("B", "e2e-b:1", "good")})
	if err := s.StartWithBest(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.lastLatency = rotateSlowLatency + time.Second
	s.lastProbe = time.Now().Add(-2 * rotateLatencyMaxAge)
	s.mu.Unlock()
	before := s.currentPID()
	m := &ProxyManager{
		instances: []*ProxySelector{s},
		statuses:  []InstanceStatus{{Index: 0, Status: "ok"}},
		xrayDir:   dir,
	}
	m.reconcileActive(0, s, s.ActiveConfig())
	if got := s.ActiveConfig(); got == nil || got.Key() != "e2e-a:1" {
		t.Fatalf("stale measurement must not trigger rotation, got %+v", got)
	}
	if s.currentPID() != before {
		t.Fatal("stale-measurement instance must not restart xray")
	}
}

// A freshly measured, idle, genuinely slow instance MUST still rotate —
// the guards exist to protect live traffic, not to freeze the pool.
func TestReconcileSlowIdleActiveStillRotates(t *testing.T) {
	needStub(t)
	dir := t.TempDir()
	writeStubXray(t, dir)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := NewProxySelector(dir, "http://probe.invalid/", socks, httpP, time.Minute)
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
		t.Fatal("precondition: instance must look idle, got ServingTraffic=true")
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
	// Must rotate: A is fresh+slow, B is a working fast candidate and clears
	// the 30% margin. If the guards ever degenerate into "never rotate", the
	// pool silently freezes on one bad upstream and this is what notices.
	if got := s.ActiveConfig(); got == nil || got.Key() != "e2e-b:1" {
		t.Fatalf("fresh+slow idle instance must still rotate to e2e-b:1, got %+v", got)
	}
	if n := stubCount(dir); n != 1 {
		t.Fatalf("rotation must leave exactly 1 stub xray, found %d", n)
	}
}

// A probe target that rate-limits us (429) must leave the upstream UNPROVEN,
// not poisoned. Previously tryConfigs markBad'd it, which propagated through
// the shared probe ledger: every peer instance then skipped the same
// candidate and the entire pool churned on one throttled 429.
//
// This is the wiring proof for classifyProbe's verdictUnproven arm — the
// hermetic table test covers the decision, this covers the call site.
//
// startShared is called DIRECTLY with a real ledger, not via StartWithBest:
// StartWithBest passes a nil shared, so the ledger this test inspects would
// never be written to and every assertion here would be vacuously true.
func TestPopulateThrottledTargetDoesNotPoisonPool(t *testing.T) {
	needStub(t)
	dir := t.TempDir()
	writeStubXray(t, dir)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := NewProxySelector(dir, "http://probe.invalid/", socks, httpP, time.Minute)
	// Stop the serving child before the test ends: a stub left listening
	// outlives the test binary's stdout pipe, which makes `go test` hang for
	// the full stub lifetime ("Test I/O incomplete ...") under LOG_LEVEL=debug.
	t.Cleanup(s.Stop)
	s.UpdateConfigs([]ProxyConfig{
		e2eCand("throttled", "e2e-throttle:1", "throttled"),
		e2eCand("good", "e2e-good:1", "good"),
	})
	shared := newProbeShared()
	if err := s.startShared(nil, shared, time.Now().Add(30*time.Second)); err != nil {
		t.Fatalf("populate must skip the throttled candidate and serve the good one: %v", err)
	}
	if got := s.ActiveConfig(); got == nil || got.Key() != "e2e-good:1" {
		t.Fatalf("expected active e2e-good:1, got %+v", got)
	}
	// Precondition: the ledger really was used, so the assertions below mean
	// something. Without this the negative check passes on an empty ledger.
	if len(shared.claimed) == 0 && len(shared.bad) == 0 {
		t.Fatal("precondition: populate must have populated the shared ledger")
	}
	if shared.isBad("e2e-throttle:1") {
		t.Fatal("a throttled probe TARGET must not poison the shared ledger: the upstream is untested, not dead")
	}
	if !s.HealthCheck() {
		t.Fatal("serving instance must be healthy")
	}
	if n := stubCount(dir); n != 1 {
		t.Fatalf("expected exactly 1 stub xray, found %d (orphans!)", n)
	}
}

// The mirror image: a genuinely dead upstream (transport failure) MUST be
// poisoned, or every instance re-probes known-dead nodes on every refresh.
func TestPopulateDeadTargetDoesPoisonPool(t *testing.T) {
	needStub(t)
	dir := t.TempDir()
	writeStubXray(t, dir)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := NewProxySelector(dir, "http://probe.invalid/", socks, httpP, time.Minute)
	t.Cleanup(s.Stop)
	s.UpdateConfigs([]ProxyConfig{
		e2eCand("hang", "e2e-hang:1", "dead-hang"),
		e2eCand("good", "e2e-good:1", "good"),
	})
	shared := newProbeShared()
	if err := s.startShared(nil, shared, time.Now().Add(30*time.Second)); err != nil {
		t.Fatalf("populate failed: %v", err)
	}
	if got := s.ActiveConfig(); got == nil || got.Key() != "e2e-good:1" {
		t.Fatalf("expected active e2e-good:1, got %+v", got)
	}
	if len(shared.bad) == 0 {
		t.Fatal("precondition: the dead candidate must have been recorded bad")
	}
	if !shared.isBad("e2e-hang:1") {
		t.Fatal("a dead upstream must be recorded bad so peer instances skip it")
	}
	if shared.isBad("e2e-good:1") {
		t.Fatal("the serving upstream must never be marked bad")
	}
}

// ---- HTTP bridge behavior ----

func proxyClient(t *testing.T, bridgeAddr string) *http.Client {
	t.Helper()
	pu, _ := url.Parse("http://" + bridgeAddr)
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(pu)},
		Timeout:   30 * time.Second,
	}
}

func TestBridgeServesThroughWorkingUpstream(t *testing.T) {
	socksAddr, done := serveSocks204(t, "ok")
	defer done()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bridgeAddr := ln.Addr().String()
	_ = ln.Close()
	startHTTPProxy("127.0.0.1:"+portOf(bridgeAddr), socksAddr)
	deadline := time.Now().Add(5 * time.Second)
	for !tcpOpen(bridgeAddr, 200*time.Millisecond) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	resp, err := proxyClient(t, bridgeAddr).Get("http://example.com/")
	if err != nil {
		t.Fatalf("bridge GET failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 204 {
		t.Fatalf("expected 204 via bridge, got %d", resp.StatusCode)
	}
}

func TestBridgeDeadUpstreamFast502(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadSocks := ln.Addr().String()
	_ = ln.Close() // refused
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bridgeAddr := ln2.Addr().String()
	_ = ln2.Close()
	startHTTPProxy("127.0.0.1:"+portOf(bridgeAddr), deadSocks)
	deadline := time.Now().Add(5 * time.Second)
	for !tcpOpen(bridgeAddr, 200*time.Millisecond) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	start := time.Now()
	resp, err := proxyClient(t, bridgeAddr).Get("http://example.com/")
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("dead-upstream bridge took %s, must fail fast", el)
	}
	if err != nil {
		t.Fatalf("expected HTTP error status, got transport error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 for dead upstream, got %d", resp.StatusCode)
	}
}

func TestBridgeHungUpstreamBounded504(t *testing.T) {
	// Blackhole SOCKS: accepts TCP, never handshakes -> dial must time out.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hole := ln.Addr().String()
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { time.Sleep(30 * time.Second); _ = c.Close() }()
		}
	}()
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bridgeAddr := ln2.Addr().String()
	_ = ln2.Close()
	startHTTPProxy("127.0.0.1:"+portOf(bridgeAddr), hole)
	deadline := time.Now().Add(5 * time.Second)
	for !tcpOpen(bridgeAddr, 200*time.Millisecond) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	start := time.Now()
	resp, err := proxyClient(t, bridgeAddr).Get("http://example.com/")
	el := time.Since(start)
	if err != nil {
		t.Fatalf("expected HTTP error status, got transport error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 for hung upstream, got %d", resp.StatusCode)
	}
	if el < bridgeDialTimeout || el > bridgeDialTimeout+10*time.Second {
		t.Fatalf("hung-upstream took %s, must respect the %s dial budget", el, bridgeDialTimeout)
	}
}

func portOf(addr string) string {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return p
}
