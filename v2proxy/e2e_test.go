package main

// End-to-end lifecycle tests with a FAKE xray binary (python stub).
// The stub reads its real rendered config file, binds the real SOCKS port,
// and behaves per upstream tag: "dead-crash" exits at once, "dead-hang"
// accepts but never answers (dead upstream), anything else serves SOCKS5->204.
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
import json, socket, sys, threading, time

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
            l = c.recv(1)
            if not l:
                return
            need = l[0]
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
        c.sendall(b"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
    except Exception:
        pass
    finally:
        try:
            c.close()
        except Exception:
            pass

def main():
    cfg = json.load(open(sys.argv[sys.argv.index("-c") + 1]))
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
	defer ln.Close()
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
	// Refresh drops A entirely: reconcile must switch to B and serve it.
	s.UpdateConfigs([]ProxyConfig{e2eCand("B", "e2e-b:1", "good")})
	m := &ProxyManager{
		instances: []*ProxySelector{s},
		statuses:  []InstanceStatus{{Index: 0, Status: "ok"}},
		xrayDir:   dir,
	}
	m.reconcileActive(0, s)
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
	m.reconcileActive(0, s) // same pool: must cost zero disruption
	if s.currentPID() != before {
		t.Fatal("fast present active must not be restarted by refresh")
	}
	if n := stubCount(dir); n != 1 {
		t.Fatalf("expected exactly 1 stub xray, found %d", n)
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
	defer resp.Body.Close()
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
	defer resp.Body.Close()
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
	defer ln.Close()
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
	defer resp.Body.Close()
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
