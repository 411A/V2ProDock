package main

// Upload-capability tests: a download probe cannot see a node that accepts a
// CONNECT and streams a body but drops or stalls a request body. The upload
// gate is a separate, opt-in signal and must never remove a node from service.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUploadEnvParsing(t *testing.T) {
	t.Setenv("UPLOAD_CHECK", "0")
	if uploadCheckEnabled() {
		t.Fatal("UPLOAD_CHECK=0 must disable the upload gate")
	}
	t.Setenv("UPLOAD_CHECK", "1")
	t.Setenv("UPLOAD_CHECK_URL", "")
	if uploadGateActive() {
		t.Fatal("an empty UPLOAD_CHECK_URL must keep the gate idle")
	}
	t.Setenv("UPLOAD_CHECK_URL", "https://upload.example/echo")
	if !uploadCheckEnabled() || !uploadGateActive() {
		t.Fatal("UPLOAD_CHECK_URL must activate the upload gate")
	}

	t.Setenv("UPLOAD_CHECK_BYTES", "4096")
	if got := uploadProbeSize(); got != 4096 {
		t.Fatalf("upload size = %d, want 4096", got)
	}
	t.Setenv("UPLOAD_CHECK_BYTES", "1") // below minimum -> default
	if got := uploadProbeSize(); got != uploadProbeBytesDefault {
		t.Fatalf("invalid upload size = %d, want default %d", got, uploadProbeBytesDefault)
	}
	t.Setenv("UPLOAD_CHECK_TIMEOUT_SECS", "30")
	if got := uploadProbeTimeout(); got != 30*time.Second {
		t.Fatalf("upload timeout = %s, want 30s", got)
	}
	t.Setenv("UPLOAD_CHECK_TIMEOUT_SECS", "1") // below minimum -> default
	if got := uploadProbeTimeout(); got != uploadProbeTimeoutDefault {
		t.Fatalf("invalid upload timeout = %s, want default %s", got, uploadProbeTimeoutDefault)
	}
}

func TestMeasureUploadDialSuccess(t *testing.T) {
	const size = 16 << 10
	gotBody := make(chan int, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		gotBody <- len(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	dial := (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	res := measureUploadDial(dial, srv.URL, size, 5*time.Second)
	if res.Err != nil {
		t.Fatalf("upload measurement: %v", res.Err)
	}
	if res.Inconclusive {
		t.Fatal("2xx upload must be conclusive")
	}
	if res.Status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.Status)
	}
	if res.Bytes != size || res.Expected != size {
		t.Fatalf("bytes=%d expected=%d, want %d/%d", res.Bytes, res.Expected, size, size)
	}
	if n := <-gotBody; n != size {
		t.Fatalf("target received %d bytes, want %d", n, size)
	}
	if res.Elapsed <= 0 {
		t.Fatal("elapsed must be positive")
	}
}

func TestMeasureUploadDialRejectedTargetIsInconclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	dial := (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	res := measureUploadDial(dial, srv.URL, 4<<10, 5*time.Second)
	if !res.Inconclusive {
		t.Fatalf("403 must be inconclusive: %+v", res)
	}
	if res.Bytes != 0 {
		t.Fatalf("rejected target must not count sent bytes: %+v", res)
	}
	if res.Err == nil {
		t.Fatal("rejected target must carry a diagnostic error")
	}
}

func TestMeasureUploadDialRedirectIsInconclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	dial := (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	res := measureUploadDial(dial, srv.URL, 4<<10, 5*time.Second)
	if !res.Inconclusive || res.Status != http.StatusFound {
		t.Fatalf("redirect must be an inconclusive target verdict: %+v", res)
	}
}

func TestMeasureUploadDialInvalidTargetIsInconclusive(t *testing.T) {
	dial := (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	for _, raw := range []string{"ftp://upload.example/post", "://bad"} {
		res := measureUploadDial(dial, raw, 4<<10, time.Second)
		if !res.Inconclusive {
			t.Fatalf("%q must be a configuration error, not node failure: %+v", raw, res)
		}
	}
}

func TestMeasureUploadDialBrokenTunnel(t *testing.T) {
	// Accept the request far enough to read its headers, then close without a
	// response. That is a request-body path that dies in transit, not a target
	// rejection, so it must be conclusive.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 1024)
		_, _ = c.Read(buf)
		_ = c.Close()
	}()

	dial := (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	res := measureUploadDial(dial, "http://"+ln.Addr().String()+"/drop", 4<<10, 2*time.Second)
	<-done
	if res.Err == nil {
		t.Fatalf("dropped upload must fail: %+v", res)
	}
	if res.Inconclusive {
		t.Fatalf("a dropped request is a node failure, not a target verdict: %+v", res)
	}
}

func TestApplyUploadHysteresis(t *testing.T) {
	const endpoint = "upload.example:443"
	m := backendManager([]InstanceStatus{{
		Index: 0, Status: "ok", Endpoint: endpoint,
	}}, 27951, 27961)

	fail := UploadResult{Expected: 100, Status: 0, Err: io.ErrUnexpectedEOF, URL: "https://upload.example/echo"}
	ok := UploadResult{Expected: 100, Bytes: 100, Status: 204, URL: "https://upload.example/echo"}

	m.applyUpload(0, endpoint, fail)
	s := m.statusOf(0)
	if s.UploadBroken || s.UploadOK {
		t.Fatalf("first failure must not confirm or promote: %+v", s)
	}
	if usable, measured, inconclusive := m.UploadStats(); usable != 0 || measured != 1 || inconclusive != 0 {
		t.Fatalf("stats after one failure = %d/%d/%d, want 0/1/0", usable, measured, inconclusive)
	}

	m.applyUpload(0, endpoint, fail)
	s = m.statusOf(0)
	if !s.UploadBroken || s.UploadOK {
		t.Fatalf("second consecutive failure must confirm broken: %+v", s)
	}

	m.applyUpload(0, endpoint, ok)
	s = m.statusOf(0)
	if !s.UploadOK || s.UploadBroken || s.uploadFails != 0 {
		t.Fatalf("success must restore upload proof: %+v", s)
	}
	if usable, measured, inconclusive := m.UploadStats(); usable != 1 || measured != 1 || inconclusive != 0 {
		t.Fatalf("stats after success = %d/%d/%d, want 1/1/0", usable, measured, inconclusive)
	}

	// One failure after proof leaves the proven state intact (hysteresis).
	m.applyUpload(0, endpoint, fail)
	s = m.statusOf(0)
	if !s.UploadOK || s.UploadBroken {
		t.Fatalf("one failure must not drop proven upload: %+v", s)
	}
	m.applyUpload(0, endpoint, fail)
	if s = m.statusOf(0); !s.UploadBroken || s.UploadOK {
		t.Fatalf("two failures must confirm broken: %+v", s)
	}
}

func TestApplyUploadInconclusiveChangesNothing(t *testing.T) {
	const endpoint = "upload.example:443"
	m := backendManager([]InstanceStatus{{
		Index: 0, Status: "ok", Endpoint: endpoint,
	}}, 27951, 27961)
	m.applyUpload(0, endpoint, UploadResult{Expected: 100, Bytes: 100, Status: 204})
	if s := m.statusOf(0); !s.UploadOK {
		t.Fatalf("setup: %+v", s)
	}
	m.applyUpload(0, endpoint, UploadResult{Status: 429, Inconclusive: true})
	s := m.statusOf(0)
	if !s.UploadOK || s.UploadBroken || s.uploadFails != 0 {
		t.Fatalf("429 must not change upload state: %+v", s)
	}
	if s.uploadAt.IsZero() {
		t.Fatal("inconclusive probe must still advance the retry clock")
	}
}

func TestUploadStatsInconclusive(t *testing.T) {
	const endpoint = "upload-inconclusive.example:443"
	m := backendManager([]InstanceStatus{{
		Index: 0, Status: "ok", Endpoint: endpoint,
	}}, 27951, 27961)
	m.applyUpload(0, endpoint, UploadResult{Status: 405, Inconclusive: true})
	if usable, measured, inconclusive := m.UploadStats(); usable != 0 || measured != 1 || inconclusive != 1 {
		t.Fatalf("stats after inconclusive probe = %d/%d/%d, want 0/1/1", usable, measured, inconclusive)
	}
	if s := m.statusOf(0); s.UploadOK || s.UploadBroken || s.uploadFails != 0 {
		t.Fatalf("inconclusive probe must not change node state: %+v", s)
	}
}

func TestUploadOrderingPrefersProvenButNeverRejects(t *testing.T) {
	t.Setenv("UPLOAD_CHECK_URL", "https://upload.example/echo")
	in := []InstanceStatus{
		{Index: 0, Status: "ok", LatMs: 50, OkStreak: 9, ThroughputKbps: 2500, UploadOK: true},
		{Index: 1, Status: "ok", LatMs: 1, OkStreak: 9, ThroughputKbps: 5000},
		{Index: 2, Status: "ok", LatMs: 1, OkStreak: 9, ThroughputKbps: 12000, UploadBroken: true},
	}
	got := orderAlive(in, 0)
	want := []int{0, 1, 2}
	for i, idx := range want {
		if got[i].Index != idx {
			t.Fatalf("order[%d] = %d, want %d (proven upload leads): %+v", i, got[i].Index, idx, got)
		}
	}

	// No node can upload: upload state must not penalize the best downloader.
	in[0].UploadOK = false
	got = orderAlive(in, 0)
	want = []int{2, 1, 0}
	for i, idx := range want {
		if got[i].Index != idx {
			t.Fatalf("no-upload order[%d] = %d, want %d (best download first): %+v", i, got[i].Index, idx, got)
		}
	}
}

func TestUploadPreferenceOutranksStreak(t *testing.T) {
	t.Setenv("UPLOAD_CHECK_URL", "https://upload.example/echo")
	in := []InstanceStatus{
		{Index: 0, Status: "ok", LatMs: 50, OkStreak: 1, ThroughputKbps: 2500, UploadOK: true},
		{Index: 1, Status: "ok", LatMs: 1, OkStreak: 9, ThroughputKbps: 9000},
	}
	got := orderAlive(in, 2)
	if got[0].Index != 0 {
		t.Fatalf("a proven uploader with a short streak must still lead: %+v", got)
	}
}

func TestUploadPinningRequiresProof(t *testing.T) {
	const endpoint = "upload-pin.example:443"
	const provenEndpoint = "upload-proven.example:443"
	t.Setenv("UPLOAD_CHECK_URL", "https://upload.example/echo")

	m := backendManager([]InstanceStatus{
		{Index: 0, Status: "ok"},
		{Index: 1, Status: "ok"},
	}, 27951, 27961, 27952, 27962)
	m.instances[0].configs = []ProxyConfig{{Name: "proven", Endpoint: endpoint}}
	m.instances[0].activeIndex = 0
	m.instances[1].configs = []ProxyConfig{{Name: "proven-peer", Endpoint: provenEndpoint}}
	m.instances[1].activeIndex = 0
	m.markOK(0, "proven", time.Millisecond)
	m.markOK(1, "proven-peer", time.Millisecond)

	// Another alive node has upload proof and bulk capacity, so this
	// download-only node must not be pinned.
	m.applyThroughput(1, provenEndpoint, ThroughputResult{Kbps: 5000, Bytes: 100, Expected: 100, Status: 200})
	m.applyUpload(1, provenEndpoint, UploadResult{Expected: 100, Bytes: 100, Status: 204})
	if len(m.pinned) != 1 {
		t.Fatalf("setup: proven peer must be pinned: %+v", m.pinned)
	}
	m.applyThroughput(0, endpoint, ThroughputResult{Kbps: 5000, Bytes: 100, Expected: 100, Status: 200})
	if _, ok := m.pinned[endpoint]; ok {
		t.Fatalf("download-only proof must not pin while a full-capability peer exists: %+v", m.pinned)
	}

	m.applyUpload(0, endpoint, UploadResult{Expected: 100, Bytes: 100, Status: 204})
	if _, ok := m.pinned[endpoint]; !ok {
		t.Fatalf("upload proof must pin a bulk-usable node: %+v", m.pinned)
	}

	m.applyUpload(0, endpoint, UploadResult{Err: io.ErrUnexpectedEOF, Expected: 100})
	m.applyUpload(0, endpoint, UploadResult{Err: io.ErrUnexpectedEOF, Expected: 100})
	if _, ok := m.pinned[endpoint]; ok {
		t.Fatalf("confirmed broken upload must drop the pin while a proven peer exists: %+v", m.pinned)
	}

	m.applyUpload(0, endpoint, UploadResult{Expected: 100, Bytes: 100, Status: 204})
	if _, ok := m.pinned[endpoint]; !ok {
		t.Fatalf("recovered upload must pin again: %+v", m.pinned)
	}
}

func TestUploadPinningFallsBackWhenNoProvenNode(t *testing.T) {
	const endpoint = "upload-fallback.example:443"
	t.Setenv("UPLOAD_CHECK_URL", "https://upload.example/echo")

	m := backendManager([]InstanceStatus{{Index: 0, Status: "ok"}}, 27951, 27961)
	m.instances[0].configs = []ProxyConfig{{Name: "fallback", Endpoint: endpoint}}
	m.instances[0].activeIndex = 0
	m.markOK(0, "fallback", time.Millisecond)
	m.applyThroughput(0, endpoint, ThroughputResult{Kbps: 5000, Bytes: 100, Expected: 100, Status: 200})
	if _, ok := m.pinned[endpoint]; !ok {
		t.Fatalf("with no upload-capable node, the best downloader must still be pinned: %+v", m.pinned)
	}

	// The same node becoming proven must remain pinned.
	m.applyUpload(0, endpoint, UploadResult{Expected: 100, Bytes: 100, Status: 204})
	if _, ok := m.pinned[endpoint]; !ok {
		t.Fatalf("proven uploader must stay pinned: %+v", m.pinned)
	}
}

func TestUploadPinningUnchangedWhenGateOff(t *testing.T) {
	const endpoint = "no-upload-check.example:443"
	t.Setenv("UPLOAD_CHECK_URL", "")

	m := backendManager([]InstanceStatus{{Index: 0, Status: "ok"}}, 27951, 27961)
	m.instances[0].configs = []ProxyConfig{{Name: "proven", Endpoint: endpoint}}
	m.instances[0].activeIndex = 0
	m.markOK(0, "proven", time.Millisecond)
	m.applyThroughput(0, endpoint, ThroughputResult{Kbps: 5000, Bytes: 100, Expected: 100, Status: 200})
	if len(m.pinned) != 1 {
		t.Fatalf("gate off must preserve download-only pinning: %+v", m.pinned)
	}
}

func TestApplyUploadIgnoresNodeThatWentDown(t *testing.T) {
	const endpoint = "down-upload.example:443"
	m := backendManager([]InstanceStatus{{
		Index: 0, Status: "down", Endpoint: endpoint,
	}}, 27951, 27961)
	m.applyUpload(0, endpoint, UploadResult{Expected: 100, Bytes: 100, Status: 204})
	if s := m.statusOf(0); s.UploadOK || s.UploadBroken {
		t.Fatalf("a probe that finished after markDown must not change state: %+v", s)
	}
}

func TestUploadStateResetsOnSwitch(t *testing.T) {
	m := backendManager([]InstanceStatus{{Index: 0, Status: "ok"}}, 27971, 27981)
	m.instances[0].configs = []ProxyConfig{{Name: "a", Endpoint: "a.example:443"}}
	m.instances[0].activeIndex = 0
	m.markOK(0, "a", time.Millisecond)
	m.applyUpload(0, "a.example:443", UploadResult{Expected: 100, Bytes: 100, Status: 204})
	if s := m.statusOf(0); !s.UploadOK {
		t.Fatalf("setup: %+v", s)
	}

	m.instances[0].configs = []ProxyConfig{{Name: "b", Endpoint: "b.example:443"}}
	m.instances[0].activeIndex = 0
	m.markOK(0, "b", time.Millisecond)
	s := m.statusOf(0)
	if s.Endpoint != "b.example:443" || s.UploadOK || s.UploadBroken || s.uploadFails != 0 {
		t.Fatalf("switch inherited upload state: %+v", s)
	}
}

func TestAPIHealthUploadGate(t *testing.T) {
	t.Setenv("THROUGHPUT_CHECK", "0") // no background download probes
	t.Setenv("UPLOAD_CHECK_URL", "https://upload.example/echo")
	m, api := startTestStack(t, 27850, 1)
	m.markOK(0, "node", 5*time.Millisecond)
	m.mu.Lock()
	m.statuses[0].Endpoint = "node.example:443"
	m.mu.Unlock()

	fail := UploadResult{Expected: 100, Err: io.ErrUnexpectedEOF}
	m.applyUpload(0, "node.example:443", fail)
	_, health := getJSON(t, api+"/health")
	if health["status"] != "ok" || health["upload_warning"] != nil {
		t.Fatalf("one failure must not degrade health: %v", health)
	}
	if health["usable_upload"] != float64(0) || health["upload_measured"] != float64(1) {
		t.Fatalf("/health after one failure = %v, want usable=0 measured=1", health)
	}

	m.applyUpload(0, "node.example:443", fail)
	_, health = getJSON(t, api+"/health")
	if health["upload_check"] != true || health["usable_upload"] != float64(0) ||
		health["upload_measured"] != float64(1) {
		t.Fatalf("/health upload fields = %v, want check=true usable=0 measured=1", health)
	}
	if health["status"] != "degraded" || health["upload_warning"] == nil {
		t.Fatalf("/health must report a pool with no upload-capable node: %v", health)
	}

	m.applyUpload(0, "node.example:443", UploadResult{Expected: 100, Bytes: 100, Status: 204})
	_, health = getJSON(t, api+"/health")
	if health["usable_upload"] != float64(1) || health["status"] != "ok" {
		t.Fatalf("/health after upload recovery = %v, want usable_upload=1 status=ok", health)
	}
	if health["upload_measured"] != float64(1) {
		t.Fatalf("/health upload measured = %v, want 1", health["upload_measured"])
	}
}

func TestAPIHealthUploadProbeInconclusive(t *testing.T) {
	t.Setenv("THROUGHPUT_CHECK", "0")
	t.Setenv("UPLOAD_CHECK_URL", "https://upload.example/echo")
	m, api := startTestStack(t, 27854, 1)
	m.markOK(0, "node", 5*time.Millisecond)
	m.mu.Lock()
	m.statuses[0].Endpoint = "node.example:443"
	m.mu.Unlock()

	m.applyUpload(0, "node.example:443", UploadResult{Status: 405, Inconclusive: true})
	_, health := getJSON(t, api+"/health")
	if health["upload_check"] != true || health["usable_upload"] != float64(0) ||
		health["upload_measured"] != float64(1) || health["upload_inconclusive"] != float64(1) {
		t.Fatalf("/health inconclusive upload fields = %v", health)
	}
	if health["status"] != "degraded" || health["upload_probe_warning"] == nil {
		t.Fatalf("/health must expose an all-inconclusive upload gate: %v", health)
	}
	if health["upload_warning"] != nil {
		t.Fatalf("inconclusive probes must not be reported as broken nodes: %v", health)
	}
}

// A whole-body POST must be sent exactly once and not silently converted into
// a GET by redirect handling or an empty body.
func TestMeasureUploadThroughSOCKS(t *testing.T) {
	const size = 8 << 10
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	gotBody := make(chan int, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		if !socksHandshake(c) {
			return
		}
		req, err := http.ReadRequest(bufio.NewReader(c))
		if err != nil {
			return
		}
		body, _ := io.ReadAll(req.Body)
		gotBody <- len(body)
		_, _ = io.WriteString(c, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	res := measureUpload(fmt.Sprintf("127.0.0.1:%d", port), "http://upload.example/post", size, 5*time.Second)
	if res.Err != nil || res.Status != http.StatusNoContent {
		t.Fatalf("SOCKS upload: %+v", res)
	}
	if n := <-gotBody; n != size {
		t.Fatalf("SOCKS target received %d bytes, want %d", n, size)
	}
}

func TestMaybeMeasureUploadSchedulesProbe(t *testing.T) {
	const size = 8 << 10
	t.Setenv("UPLOAD_CHECK_URL", "http://upload.example/post")
	t.Setenv("UPLOAD_CHECK_BYTES", "8192")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	gotBody := make(chan int, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		if !socksHandshake(c) {
			return
		}
		req, err := http.ReadRequest(bufio.NewReader(c))
		if err != nil {
			return
		}
		body, _ := io.ReadAll(req.Body)
		gotBody <- len(body)
		_, _ = io.WriteString(c, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	inst := NewProxySelector("/nonexistent", "http://probe.invalid/", port, 0, time.Minute)
	m := &ProxyManager{
		instances: []*ProxySelector{inst},
		statuses:  []InstanceStatus{{Index: 0, Status: "ok", Endpoint: "node.example:443"}},
	}
	m.maybeMeasureUpload(0, inst)

	deadline := time.Now().Add(5 * time.Second)
	for {
		s := m.statusOf(0)
		if s.UploadOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scheduled upload probe did not promote: %+v", s)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := <-gotBody; n != size {
		t.Fatalf("scheduled target received %d bytes, want %d", n, size)
	}
}

func TestMeasureUploadDialSendsDeclaredLength(t *testing.T) {
	const size = 8 << 10
	seen := make(chan int64, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- r.ContentLength
		if int64(len(body)) != size {
			t.Errorf("body = %d, want %d", len(body), size)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	dial := (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	res := measureUploadDial(dial, srv.URL, size, 5*time.Second)
	if res.Err != nil || res.Status != http.StatusOK {
		t.Fatalf("upload: %+v", res)
	}
	if got := <-seen; got != size {
		t.Fatalf("Content-Length = %d, want %d", got, size)
	}
	if res.Bytes != size || res.Expected != size {
		t.Fatalf("bytes=%d expected=%d, want %d/%d", res.Bytes, res.Expected, size, size)
	}
}

// The body must be the declared payload, not an empty reader or a GET-like
// request; a target that answers 2xx after reading it is the success case.
func TestMeasureUploadDialSendsFullBody(t *testing.T) {
	const size = 4 << 10
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dial := (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	if res := measureUploadDial(dial, srv.URL, size, 5*time.Second); res.Err != nil {
		t.Fatalf("upload: %+v", res)
	}
	body := <-got
	if !bytes.Equal(body, bytes.Repeat([]byte{'v'}, size)) {
		t.Fatalf("body corruption or truncation: len=%d", len(body))
	}
}
