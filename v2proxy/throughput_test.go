package main

// Bulk-throughput tests: measure a real body, demote only after hysteresis,
// order by capacity class, and turn repeated short plain-HTTP bodies into a
// memo automatically.

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestThroughputEnvParsing(t *testing.T) {
	t.Setenv("THROUGHPUT_CHECK", "0")
	if throughputCheckEnabled() {
		t.Fatal("THROUGHPUT_CHECK=0 must disable the gate")
	}
	t.Setenv("THROUGHPUT_CHECK", "1")
	if !throughputCheckEnabled() {
		t.Fatal("THROUGHPUT_CHECK=1 must enable the gate")
	}
	t.Setenv("THROUGHPUT_FLOOR_KBPS", "1500")
	if got := throughputFloorKbps(); got != 1500 {
		t.Fatalf("floor = %d, want 1500", got)
	}
	t.Setenv("THROUGHPUT_CHECK_BYTES", "131072")
	t.Setenv("THROUGHPUT_CHECK_URL", "https://example.test/down?bytes=%d&keep=%20")
	if got := throughputProbeURL(); got != "https://example.test/down?bytes=131072&keep=%20" {
		t.Fatalf("probe URL = %q (literal %%20 must survive)", got)
	}
	t.Setenv("THROUGHPUT_CHECK_BYTES", "1") // invalid -> default
	if got := throughputProbeSize(); got != throughputProbeBytesDefault {
		t.Fatalf("invalid size = %d, want default %d", got, throughputProbeBytesDefault)
	}
}

func TestMeasureThroughputDial(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 256<<10)
	full := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer full.Close()

	dial := (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	res := measureThroughputDial(dial, full.URL, int64(len(payload)), 5*time.Second)
	if res.Err != nil {
		t.Fatalf("full measurement: %v", res.Err)
	}
	if res.Bytes != int64(len(payload)) || res.Expected != int64(len(payload)) {
		t.Fatalf("bytes=%d expected=%d, want %d/%d", res.Bytes, res.Expected, len(payload), len(payload))
	}
	if res.Kbps <= 0 {
		t.Fatalf("Kbps = %d, want > 0", res.Kbps)
	}

	// A peer that promises 256KiB but sends 8KiB must read as SHORT, not as a
	// tiny-but-successful body.
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload[:8<<10])
	}))
	defer short.Close()
	res = measureThroughputDial(dial, short.URL, int64(len(payload)), 5*time.Second)
	if res.Bytes >= res.Expected {
		t.Fatalf("short body read %d/%d, want fewer", res.Bytes, res.Expected)
	}
	if res.Err == nil {
		t.Fatal("short body must carry a read error, not look complete")
	}
}

func TestOrderAliveThroughputTier(t *testing.T) {
	in := []InstanceStatus{
		{Index: 0, LatMs: 1, OkStreak: 9},                         // unknown, fastest handshake
		{Index: 1, LatMs: 400, OkStreak: 9, ThroughputKbps: 9000}, // >= 1 MB/s
		{Index: 2, LatMs: 5, OkStreak: 9, ThroughputKbps: 2500},   // >= 256 KB/s
		{Index: 3, LatMs: 10, OkStreak: 9, ThroughputKbps: 12000, Degraded: true, Memo: "truncates uploads"},
	}
	got := orderAlive(in, 3)
	want := []int{1, 2, 0, 3}
	if len(got) != len(want) {
		t.Fatalf("order len = %d", len(got))
	}
	for i, idx := range want {
		if got[i].Index != idx {
			t.Fatalf("order[%d] = %d, want %d (tier, then latency, degraded last): %+v", i, got[i].Index, idx, got)
		}
	}
}

func TestApplyThroughputHysteresis(t *testing.T) {
	m := backendManager([]InstanceStatus{{
		Index: 0, Status: "ok", Endpoint: "node.example:443",
	}}, 27951, 27961)
	low := ThroughputResult{Kbps: 100, Bytes: 100, Expected: 100, Status: 200}
	high := ThroughputResult{Kbps: 5000, Bytes: 100, Expected: 100, Status: 200}

	m.applyThroughput(0, "node.example:443", low)
	if s := m.statusOf(0); s.Degraded {
		t.Fatalf("first low sample demoted immediately: %+v", s)
	}
	m.applyThroughput(0, "node.example:443", low)
	if s := m.statusOf(0); !s.Degraded || s.BulkUsable {
		t.Fatalf("second low sample must demote: %+v", s)
	}
	m.applyThroughput(0, "node.example:443", high)
	if s := m.statusOf(0); s.Degraded || !s.BulkUsable || s.ThroughputKbps != high.Kbps {
		t.Fatalf("high sample must promote: %+v", s)
	}

	// A manual memo outranks a recovered throughput number; the node is still
	// last-resort for a reason the gate cannot see (e.g. upload breakage).
	m.SetMemo("node.example:443", "uploads are broken")
	m.applyThroughput(0, "node.example:443", high)
	if s := m.statusOf(0); !s.Degraded || !s.BulkUsable {
		t.Fatalf("memo must keep degraded while BulkUsable stays true: %+v", s)
	}
}

func TestEndpointSwitchResetsThroughput(t *testing.T) {
	m := backendManager([]InstanceStatus{{Index: 0, Status: "ok"}}, 27971, 27981)
	m.instances[0].configs = []ProxyConfig{{Name: "a", Endpoint: "a.example:443"}}
	m.instances[0].activeIndex = 0
	m.markOK(0, "a", time.Millisecond)
	m.applyThroughput(0, "a.example:443", ThroughputResult{Kbps: 5000, Bytes: 100, Expected: 100, Status: 200})
	if s := m.statusOf(0); !s.BulkUsable {
		t.Fatalf("setup: %+v", s)
	}

	// Switch the active node. The old measurement must not follow the port.
	m.instances[0].configs = []ProxyConfig{{Name: "b", Endpoint: "b.example:443"}}
	m.instances[0].activeIndex = 0
	m.markOK(0, "b", time.Millisecond)
	s := m.statusOf(0)
	if s.Endpoint != "b.example:443" || s.ThroughputKbps != 0 || s.BulkUsable {
		t.Fatalf("switch inherited old quality: %+v", s)
	}
}

func TestAutoMemoShortTransfers(t *testing.T) {
	const endpoint = "short-auto.example:443"
	const httpPort = 27971
	m := backendManager([]InstanceStatus{{
		Index: 0, Status: "ok", Endpoint: endpoint, HTTP: "0.0.0.0:27971",
	}}, 27970, httpPort)
	setHTTPPortEndpoint(httpPort, endpoint)
	defer setHTTPPortEndpoint(httpPort, "")

	for range shortTransferMemoAfter {
		noteShortTransfer(httpPort)
	}
	m.autoMemoShortTransfers(0)
	s := m.statusOf(0)
	if !s.Degraded || !strings.Contains(s.Memo, "auto:") {
		t.Fatalf("short transfers did not auto-memo: %+v", s)
	}
	if n := shortTransferCount(endpoint); n != 0 {
		t.Fatalf("counter = %d after memo, want reset", n)
	}
}

func TestPlainHTTPCountsShortBody(t *testing.T) {
	const endpoint = "short-bridge.example:443"
	const bridgePort = 23456
	upstream, proxySide := net.Pipe()
	defer func() { _ = upstream.Close() }()
	go func() {
		defer func() { _ = upstream.Close() }()
		br := bufio.NewReader(upstream)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		_ = req.Body.Close()
		_, _ = io.WriteString(upstream,
			"HTTP/1.1 200 OK\r\nContent-Length: 100\r\nConnection: close\r\n\r\n0123456789")
	}()
	setHTTPPortEndpoint(bridgePort, endpoint)
	defer setHTTPPortEndpoint(bridgePort, "")
	defer clearShortTransfers(endpoint)

	initConnSem()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlePlainHTTP(w, r, staticDialer{proxySide}, bridgePort)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	proxyURL, _ := url.Parse("http://" + ln.Addr().String())
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	resp, err := client.Get("http://example.invalid/short")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body) // expected to error mid-body
	_ = resp.Body.Close()
	if n := shortTransferCount(endpoint); n != 1 {
		t.Fatalf("short transfer count = %d, want 1", n)
	}
}

func TestAPIHealthBulkGate(t *testing.T) {
	t.Setenv("THROUGHPUT_CHECK", "0") // no background probes in this test
	m, api := startTestStack(t, 27840, 1)
	m.markOK(0, "node", 5*time.Millisecond)
	m.mu.Lock()
	m.statuses[0].Endpoint = "node.example:443"
	m.mu.Unlock()

	low := ThroughputResult{Kbps: 100, Bytes: 100, Expected: 100, Status: 200}
	m.applyThroughput(0, "node.example:443", low)
	m.applyThroughput(0, "node.example:443", low)
	_, health := getJSON(t, api+"/health")
	if health["usable_bulk"] != float64(0) || health["bulk_measured"] != float64(1) {
		t.Fatalf("/health bulk fields = %v, want usable_bulk=0 measured=1", health)
	}
	if health["status"] != "degraded" || health["bulk_warning"] == nil {
		t.Fatalf("/health must report a bulk-degraded pool: %v", health)
	}

	high := ThroughputResult{Kbps: 5000, Bytes: 100, Expected: 100, Status: 200}
	m.applyThroughput(0, "node.example:443", high)
	_, health = getJSON(t, api+"/health")
	if health["usable_bulk"] != float64(1) || health["status"] != "ok" {
		t.Fatalf("/health after recovery = %v, want usable_bulk=1 status=ok", health)
	}
}
