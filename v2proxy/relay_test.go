package main

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRelayConfigParses pins the CF_RELAY_URL contract: standard proxy URLs
// in, everything else degrades to OFF (never to a half-chained pool).
func TestRelayConfigParses(t *testing.T) {
	cases := []struct {
		name   string
		env    string
		wantOK bool
		host   string
		port   int
		tls    bool
		user   string
		pass   string
	}{
		{name: "empty disables", env: "", wantOK: false},
		{name: "plain host:port", env: "http://127.0.0.1:8888", wantOK: true, host: "127.0.0.1", port: 8888},
		{name: "https is a TLS leg", env: "https://relay.example:443", wantOK: true, host: "relay.example", port: 443, tls: true},
		{name: "https default port", env: "https://relay.example", wantOK: true, host: "relay.example", port: 443, tls: true},
		{name: "http default port", env: "http://relay.example", wantOK: true, host: "relay.example", port: 80},
		{name: "basic auth", env: "http://u:p@h.example:3128", wantOK: true, host: "h.example", port: 3128, user: "u", pass: "p"},
		{name: "ftp rejected", env: "ftp://h.example:21", wantOK: false},
		{name: "socks scheme rejected", env: "socks5://h.example:1080", wantOK: false},
		{name: "no host rejected", env: "http://:8888", wantOK: false},
		{name: "bare word rejected", env: "notaurl", wantOK: false},
		{name: "bad port rejected", env: "http://h.example:notaport", wantOK: false},
		{name: "zero port rejected", env: "http://h.example:0", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CF_RELAY_URL", tc.env)
			rc, ok := relayConfig()
			if ok != tc.wantOK {
				t.Fatalf("relayConfig(%q) ok=%v, want %v", tc.env, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if rc.host != tc.host || rc.port != tc.port || rc.tls != tc.tls || rc.user != tc.user || rc.pass != tc.pass {
				t.Fatalf("relayConfig(%q) = %+v, want host=%q port=%d tls=%v user=%q", tc.env, rc, tc.host, tc.port, tc.tls, tc.user)
			}
		})
	}
}

// relayUpstream is the stand-in tunnel for render tests: a freedom outbound
// already carrying the tag every subscription parser emits.
func relayUpstream() ProxyConfig {
	return ProxyConfig{Name: "tun", Raw: "tun", Endpoint: "tun:1", XrayCfg: []byte(`{"protocol":"freedom","tag":"proxy"}`)}
}

func decodeRendered(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var full map[string]any
	if err := json.Unmarshal(raw, &full); err != nil {
		t.Fatal(err)
	}
	return full
}

// TestRenderRelayChain is the shape contract: relay inbound present, upstream
// still first (default route untouched), relay-out chained THROUGH the
// upstream tag, routing rule keyed on the relay inbound tag.
func TestRenderRelayChain(t *testing.T) {
	t.Setenv("CF_RELAY_URL", "http://127.0.0.1:18888")
	dir := t.TempDir()
	sel := newTestSelector(t, dir, "http://probe.invalid/", freeLoopbackPort(t), freeLoopbackPort(t))
	relayPort := freeLoopbackPort(t)
	sel.SetRelayPort(relayPort)
	if got := sel.RelayPort(); got != relayPort {
		t.Fatalf("RelayPort() = %d, want %d", got, relayPort)
	}

	cfgPath, err := sel.renderXrayConfig(relayUpstream(), sel.SOCKSPort(), 0)
	if err != nil {
		t.Fatal(err)
	}
	full := decodeRendered(t, cfgPath)

	inbounds, _ := full["inbounds"].([]any)
	if len(inbounds) != 2 {
		t.Fatalf("inbounds = %d, want 2 (direct + relay)", len(inbounds))
	}
	ri, _ := inbounds[1].(map[string]any)
	if ri["tag"] != relayInboundTag || ri["protocol"] != "socks" {
		t.Fatalf("relay inbound = %v, want tag %q socks", ri, relayInboundTag)
	}
	if port, _ := ri["port"].(float64); int(port) != relayPort {
		t.Fatalf("relay inbound port = %v, want %d", ri["port"], relayPort)
	}

	outbounds, _ := full["outbounds"].([]any)
	if len(outbounds) < 2 {
		t.Fatalf("outbounds = %d, want >= 2", len(outbounds))
	}
	first, _ := outbounds[0].(map[string]any)
	if first["tag"] != upstreamOutTag {
		t.Fatalf("outbounds[0].tag = %v, want %q (default route must stay on the upstream)", first["tag"], upstreamOutTag)
	}
	ro, _ := outbounds[1].(map[string]any)
	if ro["protocol"] != "http" || ro["tag"] != relayOutTag {
		t.Fatalf("outbounds[1] = %v, want http/%s", ro, relayOutTag)
	}
	servers, _ := ro["settings"].(map[string]any)["servers"].([]any)
	srv, _ := servers[0].(map[string]any)
	if srv["address"] != "127.0.0.1" {
		t.Fatalf("relay server address = %v", srv["address"])
	}
	if port, _ := srv["port"].(float64); int(port) != 18888 {
		t.Fatalf("relay server port = %v, want 18888", srv["port"])
	}
	ps, _ := ro["proxySettings"].(map[string]any)
	if ps["tag"] != upstreamOutTag || ps["transportLayer"] != true {
		t.Fatalf("proxySettings = %v, want tag %q transportLayer true (else the relay is dialled direct, past the tunnel)", ps, upstreamOutTag)
	}
	if _, ok := ro["streamSettings"]; ok {
		t.Fatalf("plain-http relay must not carry streamSettings: %v", ro["streamSettings"])
	}

	rules, _ := full["routing"].(map[string]any)["rules"].([]any)
	var relayRule map[string]any
	for _, r := range rules {
		if rm, _ := r.(map[string]any); rm["outboundTag"] == relayOutTag {
			relayRule = rm
		}
	}
	if relayRule == nil {
		t.Fatalf("no routing rule targets %q (rules: %v)", relayOutTag, rules)
	}
	inTags, _ := relayRule["inboundTag"].([]any)
	if len(inTags) != 1 || inTags[0] != relayInboundTag {
		t.Fatalf("relay rule inboundTag = %v, want [%q]", inTags, relayInboundTag)
	}
	// The pre-existing bittorrent rule must still be FIRST: relay routing is
	// additive, never a reorder of the default path.
	if first, _ := rules[0].(map[string]any); first["outboundTag"] != "blocked" {
		t.Fatalf("rules[0] = %v, want the bittorrent->blocked rule first", rules[0])
	}
}

// TestRenderRelayOffByteIdentical guards the default: no env, no relay port
// set, and the render carries no relay key at all.
func TestRenderRelayOffByteIdentical(t *testing.T) {
	t.Setenv("CF_RELAY_URL", "")
	dir := t.TempDir()
	sel := newTestSelector(t, dir, "http://probe.invalid/", freeLoopbackPort(t), freeLoopbackPort(t))
	cfgPath, err := sel.renderXrayConfig(relayUpstream(), sel.SOCKSPort(), 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(cfgPath)
	for _, key := range []string{relayOutTag, relayInboundTag, "proxySettings"} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("relay-off render contains %q", key)
		}
	}
}

// TestRenderRelayTLS asserts the https leg shape (runtime-proven by the TLS
// subtest of TestRelayChainEndToEnd): TLS to the relay with its hostname.
func TestRenderRelayTLS(t *testing.T) {
	t.Setenv("CF_RELAY_URL", "https://relay.example:443")
	dir := t.TempDir()
	sel := newTestSelector(t, dir, "http://probe.invalid/", freeLoopbackPort(t), freeLoopbackPort(t))
	sel.SetRelayPort(freeLoopbackPort(t))
	cfgPath, err := sel.renderXrayConfig(relayUpstream(), sel.SOCKSPort(), 0)
	if err != nil {
		t.Fatal(err)
	}
	full := decodeRendered(t, cfgPath)
	outbounds, _ := full["outbounds"].([]any)
	ro, _ := outbounds[1].(map[string]any)
	ss, _ := ro["streamSettings"].(map[string]any)
	if ss["security"] != "tls" {
		t.Fatalf("relay streamSettings = %v, want tls", ss)
	}
	if ts, _ := ss["tlsSettings"].(map[string]any); ts["serverName"] != "relay.example" {
		t.Fatalf("tlsSettings = %v, want serverName relay.example (verified against public roots; no skipping knob exists)", ss["tlsSettings"])
	}
	ts, _ := ss["tlsSettings"].(map[string]any)
	if _, bad := ts["allowInsecure"]; bad {
		t.Fatalf("allowInsecure was removed from xray: must never be rendered: %v", ts)
	}
}

// TestRelayCandidatesMirrorDirect proves both ladders rank nodes identically:
// same streak ordering, relay ports substituted 1:1, relay-off yielding nil.
func TestRelayCandidatesMirrorDirect(t *testing.T) {
	t.Setenv("AGG_MIN_STREAK", "3")
	mk := func(relay0, relay1 int) *ProxyManager {
		m := &ProxyManager{}
		i0 := NewProxySelector(t.TempDir(), "http://probe.invalid/", 27111, 27121, time.Minute)
		i1 := NewProxySelector(t.TempDir(), "http://probe.invalid/", 27112, 27122, time.Minute)
		i0.SetRelayPort(relay0)
		i1.SetRelayPort(relay1)
		m.instances = []*ProxySelector{i0, i1}
		m.statuses = []InstanceStatus{
			{Index: 1, Status: "ok", LatMs: 50, OkStreak: 5},
			{Index: 0, Status: "ok", LatMs: 100, OkStreak: 5},
		}
		return m
	}
	m := mk(27131, 27132)
	if got, want := m.aggregateCandidates(false), []string{"127.0.0.1:27112", "127.0.0.1:27111"}; !equalStrings(got, want) {
		t.Fatalf("direct = %v, want %v", got, want)
	}
	if got, want := m.aggregateRelayCandidates(), []string{"127.0.0.1:27132", "127.0.0.1:27131"}; !equalStrings(got, want) {
		t.Fatalf("relay = %v, want %v (same order, relay ports)", got, want)
	}
	if got := mk(0, 0).aggregateRelayCandidates(); len(got) != 0 {
		t.Fatalf("relay-off candidates = %v, want nil (never serve)", got)
	}
	// One relay-less instance contributes nothing but costs the ladder nothing.
	if got, want := mk(0, 27132).aggregateRelayCandidates(), []string{"127.0.0.1:27132"}; !equalStrings(got, want) {
		t.Fatalf("partial relay = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRelayGateFailsFast: a dark relay refuses at once instead of burning the
// failover budget on a hop that cannot route around the outage.
func TestRelayGateFailsFast(t *testing.T) {
	m := &ProxyManager{} // relayAlive zero = false
	c1, c2 := net.Pipe()
	done := make(chan error, 1)
	go func() {
		handleRelayConn(m, c1)
		done <- nil
	}()
	_ = c2.SetDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8)
	if _, err := c2.Read(buf); err == nil {
		t.Fatal("dark relay accepted bytes; must fail fast")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dark relay handler did not return")
	}
	_ = c2.Close()
}

// TestRelayServesBytes: an alive relay is a transparent byte relay onto the
// instance relay inbound (the SOCKS handshake runs end-to-end with xray, so a
// raw echo backend proves the fan-out without speaking any proxy protocol).
func TestRelayServesBytes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	relayPort := ln.Addr().(*net.TCPAddr).Port

	m := &ProxyManager{}
	inst := NewProxySelector(t.TempDir(), "http://probe.invalid/", freeLoopbackPort(t), freeLoopbackPort(t), time.Minute)
	inst.SetRelayPort(relayPort)
	m.instances = []*ProxySelector{inst}
	m.statuses = []InstanceStatus{{Index: 0, Status: "ok", LatMs: 1, OkStreak: 9}}
	m.relayAlive.Store(true)

	c1, c2 := net.Pipe()
	go handleRelayConn(m, c1)
	_ = c2.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c2.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c2, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q,%v, want ping", buf, err)
	}
	_ = c2.Close()
}

// freePortBlock finds a fully-free contiguous block of n loopback ports.
// NewProxyManager scans upward on collision and the layout assertions below
// are relative, so even a lost race stays green — this only picks a base
// that is free RIGHT NOW to avoid the scan entirely.
func freePortBlock(t *testing.T, n int) int {
	t.Helper()
	for _, base := range []int{27600, 27700, 27800, 27900, 27400} {
		if base+n-1 > maxPort {
			continue
		}
		var held []net.Listener
		ok := true
		for p := base; p < base+n; p++ {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
			if err != nil {
				ok = false
				break
			}
			held = append(held, ln)
		}
		for _, ln := range held {
			_ = ln.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatal("no free port block for layout test")
	return 0
}

// TestRelayPortLayout: relay-on appends a third port block and relay-off
// keeps the old 2N layout exactly. Assertions are RELATIVE to the allocated
// base (never absolute ports): the full suite runs hundreds of tests in one
// binary and a fixed base is one held port away from a scan-upward shift.
func TestRelayPortLayout(t *testing.T) {
	t.Setenv("CF_RELAY_URL", "http://127.0.0.1:9999")
	m := NewProxyManager(t.TempDir(), "http://probe.invalid/", freePortBlock(t, 6), 2, nil, time.Minute)
	if len(m.instances) != 2 {
		t.Fatalf("instances = %d, want 2", len(m.instances))
	}
	base := m.instances[0].SOCKSPort()
	for i, inst := range m.instances {
		if got, want := inst.SOCKSPort(), base+i; got != want {
			t.Fatalf("instance %d socks = %d, want %d", i, got, want)
		}
		if got, want := inst.HTTPPort(), base+2+i; got != want {
			t.Fatalf("instance %d http = %d, want %d", i, got, want)
		}
		if got, want := inst.RelayPort(), base+4+i; got != want {
			t.Fatalf("instance %d relay = %d, want %d (third block, direct untouched)", i, got, want)
		}
	}

	t.Setenv("CF_RELAY_URL", "")
	m2 := NewProxyManager(t.TempDir(), "http://probe.invalid/", freePortBlock(t, 4), 2, nil, time.Minute)
	base2 := m2.instances[0].SOCKSPort()
	for i, inst := range m2.instances {
		if got := inst.RelayPort(); got != 0 {
			t.Fatalf("relay-off instance %d relay = %d, want 0", i, got)
		}
		if got, want := inst.HTTPPort(), base2+2+i; got != want {
			t.Fatalf("relay-off instance %d http = %d, want %d (old 2N layout exactly)", i, got, want)
		}
	}
}

// BenchmarkDpiRank prices one classification: it runs once per config per
// ingest (never hot-path), so even microseconds would be lost in the fetch.
func BenchmarkDpiRank(b *testing.B) {
	lines := []string{
		"vless://uuid@pinned.reality.example:443?encryption=none&security=reality&sni=www.microsoft.com&fp=chrome&type=tcp&flow=xtls-rprx-vision#Reality",
		"ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ@20.234.97.18:443#raw-ss",
		"trojan://pass@t.example:443?security=tls#t",
		vmessLine("ws", "tls"),
		vmessLine("tcp", ""),
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dpiRank(lines[i%len(lines)])
	}
}

// BenchmarkOrderByDPIRank prices a full 100-config pool ranking: the ingest
// budget is dominated by fetching (seconds) and probing (minutes), so this
// must read as noise next to them.
func BenchmarkOrderByDPIRank(b *testing.B) {
	tmpl := []ProxyConfig{
		{Name: "reality", Raw: "vless://u@a.example:443?security=reality&sni=x.example#a"},
		{Name: "trojan", Raw: "trojan://pass@t.example:443#t"},
		{Name: "vmess-tls", Raw: vmessLine("ws", "tls")},
		{Name: "vmess-raw", Raw: vmessLine("tcp", "")},
		{Name: "ss", Raw: "ss://YWVzLTI1Ni1nY206cA@1.1.1.1:443#s"},
	}
	pool := make([]ProxyConfig, 0, 100)
	for len(pool) < 100 {
		pool = append(pool, tmpl...)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		work := append([]ProxyConfig(nil), pool...)
		orderByDPIRank(work)
	}
}

// ---- end-to-end chain against the genuine binary ----

// connectStub is the smallest standard HTTP proxy: CONNECT host:port,
// optional TLS on the relay leg, then raw bytes. It records every CONNECT
// target so the test can prove egress went THROUGH the relay, not direct.
type connectStub struct {
	ln      net.Listener
	mu      sync.Mutex
	targets []string
}

func startCONNECTStub(t *testing.T, cert *tls.Certificate) *connectStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := ln
	if cert != nil {
		l = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{*cert}})
	}
	st := &connectStub{ln: l}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go st.serve(c)
		}
	}()
	return st
}

func (st *connectStub) addr() string { return st.ln.Addr().String() }

func (st *connectStub) saw() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]string(nil), st.targets...)
}

func (st *connectStub) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	parts := strings.Fields(line)
	if len(parts) < 2 || !strings.EqualFold(parts[0], "CONNECT") {
		_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		return
	}
	target := parts[1]
	if _, _, err := net.SplitHostPort(target); err != nil {
		_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		return
	}
	// Drain headers (a Proxy-Authorization one is fine to ignore here).
	for {
		h, err := br.ReadString('\n')
		if err != nil || h == "\r\n" || h == "\n" {
			break
		}
	}
	up, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer func() { _ = up.Close() }()
	st.mu.Lock()
	st.targets = append(st.targets, target)
	st.mu.Unlock()
	if _, err := c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	go func() { _, _ = io.Copy(up, br) }()
	_, _ = io.Copy(c, up)
}

// issueRelayCert mints a private CA plus a 127.0.0.1 server certificate and
// returns the server cert with a CA-bundle path. The xray child verifies the
// relay leg against public roots in production (workers.dev); the test hands
// its private CA to the child via SSL_CERT_FILE (inherited env), so the TLS
// leg is verified exactly like production with no skipping knob anywhere.
func issueRelayCert(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	now := time.Now()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "relay-test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, &caTmpl, &caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	srvKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srvTmpl := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "relay-test"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, &srvTmpl, ca, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(srvKey)}),
	)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0644); err != nil {
		t.Fatal(err)
	}
	return cert, caPath
}

// socksGet performs a client-side SOCKS5 CONNECT through proxyAddr and a
// plain HTTP GET over the tunnel, returning the origin status code.
func socksGet(t *testing.T, proxyAddr, targetURL string) int {
	t.Helper()
	u, err := url.Parse(targetURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(u.Hostname()).To4()
	if ip == nil {
		t.Fatalf("test targets must be IPv4 literals, got %q", u.Hostname())
	}
	c, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	greet := make([]byte, 2)
	if _, err := io.ReadFull(c, greet); err != nil || greet[0] != 0x05 || greet[1] != 0x00 {
		t.Fatalf("socks greeting = %v,%v", greet, err)
	}
	req := []byte{0x05, 0x01, 0x00, 0x01, ip[0], ip[1], ip[2], ip[3], byte(port >> 8), byte(port)}
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	rep := make([]byte, 10)
	if _, err := io.ReadFull(c, rep); err != nil || rep[1] != 0x00 {
		t.Fatalf("socks connect reply = %v,%v", rep, err)
	}
	if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", u.RequestURI(), u.Host); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var proto string
	var code int
	if _, err := fmt.Sscanf(line, "%s %d", &proto, &code); err != nil {
		t.Fatalf("bad status line %q: %v", line, err)
	}
	return code
}

// TestRelayChainEndToEnd proves the rendered chain against the genuine xray:
// relay inbound -> relay-out -> (proxySettings) upstream -> CONNECT stub ->
// origin, with the direct inbound unaffected. The freedom upstream stands in
// for the tunnel; the only unproven hop in production is the worker speaking
// standard CONNECT (graded at runtime by the relay probe).
func TestRelayChainEndToEnd(t *testing.T) {
	bin := needRealXray(t)
	target := startLocalHTTPTarget(t) // 204
	tu, _ := url.Parse(target)
	targetHostPort := tu.Host

	for _, tlsMode := range []bool{false, true} {
		name := "plain"
		if tlsMode {
			name = "tls-to-relay"
		}
		t.Run(name, func(t *testing.T) {
			var cert *tls.Certificate
			scheme := "http"
			if tlsMode {
				scheme = "https"
				// Production verifies the relay cert against public roots
				// (workers.dev); the test mints a private CA and hands it to the
				// xray child via SSL_CERT_FILE (inherited env), so the relay leg
				// is verified exactly like production. No knob skips this.
				c, caPath := issueRelayCert(t)
				cert = &c
				t.Setenv("SSL_CERT_FILE", caPath)
			}
			stub := startCONNECTStub(t, cert)
			t.Setenv("CF_RELAY_URL", scheme+"://"+stub.addr())

			dir := t.TempDir()
			cp, err := os.ReadFile(bin)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dir+"/xray", cp, 0755); err != nil {
				t.Fatal(err)
			}
			sel := newTestSelector(t, dir, target, freeLoopbackPort(t), freeLoopbackPort(t))
			sel.SetRelayPort(freeLoopbackPort(t))
			cfgPath, err := sel.renderXrayConfig(relayUpstream(), sel.SOCKSPort(), 0)
			if err != nil {
				t.Fatal(err)
			}
			cmd, err := launchXray(dir, cfgPath)
			if err != nil {
				t.Fatalf("genuine xray rejected relay-chained config: %v", err)
			}
			t.Cleanup(func() {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
					_, _ = cmd.Process.Wait()
				}
			})
			if !waitForPort(sel.SOCKSPort(), 5*time.Second) {
				t.Fatal("xray never bound direct SOCKS port")
			}
			if !waitForPort(sel.RelayPort(), 5*time.Second) {
				t.Fatal("xray never bound relay SOCKS port")
			}
			relayAddr := fmt.Sprintf("127.0.0.1:%d", sel.RelayPort())
			if got := socksGet(t, relayAddr, target); got != http.StatusNoContent {
				t.Fatalf("relay path status = %d, want 204", got)
			}
			directAddr := fmt.Sprintf("127.0.0.1:%d", sel.SOCKSPort())
			if got := socksGet(t, directAddr, target); got != http.StatusNoContent {
				t.Fatalf("direct path status = %d, want 204 (relay must not disturb it)", got)
			}
			found := false
			for _, tg := range stub.saw() {
				if tg == targetHostPort {
					found = true
				}
			}
			if !found {
				t.Fatalf("relay never saw CONNECT %s (saw %v): bytes did not exit via the relay", targetHostPort, stub.saw())
			}
		})
	}
}
