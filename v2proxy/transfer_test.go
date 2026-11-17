package main

// Data-plane transfer tests: the bridge must survive the shapes real media
// clients use (half-closed request streams, pipelined CONNECT payloads,
// multi-second uploads, informational responses, and live chunked bodies).
// Everything here is hermetic: loopback TCP or net.Pipe, no xray.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A protocol that signals "request complete" with a FIN must still receive
// the response. relayPair's half-close is what makes that possible; the old
// relay closed both sockets on the first EOF and truncated the exchange.
func TestRelayPairHalfCloseKeepsResponse(t *testing.T) {
	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientLn.Close() }()
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upLn.Close() }()

	client, err := net.Dial("tcp", clientLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	relayA, err := clientLn.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relayA.Close() }()

	up, err := net.Dial("tcp", upLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = up.Close() }()
	relayB, err := upLn.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relayB.Close() }()

	done := make(chan struct{})
	go func() { defer close(done); relayPair(relayA, relayB, nil) }()

	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	_ = up.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	// End the request direction exactly as protocols that use FIN framing do.
	tc, ok := client.(*net.TCPConn)
	if !ok {
		t.Fatalf("client is %T, want *net.TCPConn", client)
	}
	if err := tc.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(up)
	if err != nil {
		t.Fatalf("upstream read: %v", err)
	}
	if string(got) != "request" {
		t.Fatalf("upstream got %q, want request (FIN must propagate as half-close)", got)
	}
	if _, err := up.Write([]byte("response")); err != nil {
		t.Fatalf("upstream write: %v", err)
	}
	_ = up.Close()

	back, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("client read after half-close: %v", err)
	}
	if string(back) != "response" {
		t.Fatalf("client got %q, want response (request FIN must not kill the response)", back)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relayPair did not finish after both directions ended")
	}
}

// A client may pipeline its TLS ClientHello right behind CONNECT. Those bytes
// are already in the hijacked bufio.Reader; dropping them (the old `_`) makes
// the upstream wait for a handshake that will never be repeated.
func TestConnectForwardsBufferedClientHello(t *testing.T) {
	initConnSem()
	upProxy, upSide := net.Pipe()
	defer func() { _ = upSide.Close() }()

	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 5)
		_ = upSide.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(upSide, buf); err != nil {
			got <- nil
			return
		}
		got <- buf
		_, _ = upSide.Write([]byte("world"))
		_ = upSide.Close()
	}()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleConnect(w, r, staticDialer{upProxy}, 0)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	c, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	// One Write: CONNECT head + early tunnel bytes. The server's bufio reader
	// normally consumes both in its first read, which is the case under test.
	if _, err := io.WriteString(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\nEARLY"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	var early []byte
	select {
	case early = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received the pipelined bytes")
	}
	if string(early) != "EARLY" {
		t.Fatalf("upstream got %q, want EARLY (hijack buffer must be forwarded)", early)
	}
	resp := make([]byte, 5)
	if _, err := io.ReadFull(br, resp); err != nil {
		t.Fatalf("reverse direction after early bytes: %v", err)
	}
	if string(resp) != "world" {
		t.Fatalf("client got %q, want world", resp)
	}
}

// trickleReader emits chunks with a pause between them, so a whole request
// body takes longer than bridgeUpstreamDeadline while every individual write
// stays well inside it.
type trickleReader struct {
	chunks []string
	pause  time.Duration
	i      int
}

func (r *trickleReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	if r.i > 0 {
		time.Sleep(r.pause)
	}
	n := copy(p, r.chunks[r.i])
	r.i++
	return n, nil
}

// A large upload is bounded by progress, not by total time. The old absolute
// pre-response deadline severed every request body that took longer than the
// header budget - a real failure mode for file uploads.
func TestPlainHTTPUploadOutlivesHeaderBudget(t *testing.T) {
	oldHeader := bridgeUpstreamDeadline
	bridgeUpstreamDeadline = 300 * time.Millisecond
	defer func() { bridgeUpstreamDeadline = oldHeader }()

	const body = "0123456789abcdef"
	upstream, proxySide := net.Pipe()
	defer func() { _ = upstream.Close() }()

	received := make(chan string, 1)
	go func() {
		defer func() { _ = upstream.Close() }()
		br := bufio.NewReader(upstream)
		req, err := http.ReadRequest(br)
		if err != nil {
			received <- "read error: " + err.Error()
			return
		}
		got, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			received <- "body error: " + err.Error()
			return
		}
		received <- string(got)
		_, _ = io.WriteString(upstream, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	}()

	initConnSem()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlePlainHTTP(w, r, staticDialer{proxySide}, 0)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	proxyURL, _ := url.Parse("http://" + ln.Addr().String())
	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	// Four chunks, 120ms apart: ~360ms total, beyond the 300ms wall, while
	// every gap is inside the per-write idle budget.
	req, err := http.NewRequest("POST", "http://example.invalid/upload",
		&trickleReader{chunks: []string{"0123", "4567", "89ab", "cdef"}, pause: 120 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("upload failed mid-body: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || string(rb) != "ok" {
		t.Fatalf("response = %d %q, want 200 ok", resp.StatusCode, rb)
	}
	select {
	case got := <-received:
		if got != body {
			t.Fatalf("upstream body = %q, want %q", got, body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received the full body")
	}
}

// 100 Continue (and any other 1xx) is not the final response. The old code
// forwarded it as one and never read the real 200, so Expect: 100-continue
// exchanges looked empty.
func TestPlainHTTPSkipsInformationalResponse(t *testing.T) {
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
			"HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	}()

	initConnSem()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlePlainHTTP(w, r, staticDialer{proxySide}, 0)
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
	resp, err := client.Get("http://example.invalid/one")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("response = %d %q, want the final 200 ok (1xx must be skipped)", resp.StatusCode, body)
	}
}

// A live stream whose chunks are smaller than the ResponseWriter buffer must
// become visible immediately, not only when the next chunk arrives.
func TestPlainHTTPFlushesStreamingChunks(t *testing.T) {
	upstream, proxySide := net.Pipe()
	defer func() { _ = upstream.Close() }()
	release := make(chan struct{})
	go func() {
		defer func() { _ = upstream.Close() }()
		br := bufio.NewReader(upstream)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		_ = req.Body.Close()
		_, _ = io.WriteString(upstream,
			"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n1\r\na\r\n")
		<-release
		_, _ = io.WriteString(upstream, "1\r\nb\r\n0\r\n\r\n")
	}()

	initConnSem()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlePlainHTTP(w, r, staticDialer{proxySide}, 0)
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
	resp, err := client.Get("http://example.invalid/live")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("first live chunk did not arrive before the stream continued: %v", err)
	}
	if buf[0] != 'a' {
		t.Fatalf("first byte = %q, want a", buf[0])
	}
	close(release)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rest, []byte("b")) {
		t.Fatalf("rest = %q, want b", rest)
	}
}

// directTestDialer resolves the CONNECT target directly, so the test can put
// its chunk-generator on a loopback port and exercise the real relay code.
type directTestDialer struct{}

func (directTestDialer) Dial(network, addr string) (net.Conn, error) {
	return net.DialTimeout(network, addr, 5*time.Second)
}

// A multi-megabyte tunnel is the shape a video segment or a Telegram file
// takes. The relay must move every byte in both directions; a fixed 12KB
// ceiling (a common broken-proxy symptom) would fail here immediately.
func TestConnectMovesMultiMegabytePayload(t *testing.T) {
	initConnSem()
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upLn.Close() }()

	payload := bytes.Repeat([]byte("0123456789abcdef"), 256*1024) // 4 MiB
	upDone := make(chan error, 1)
	go func() {
		c, err := upLn.Accept()
		if err != nil {
			upDone <- err
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(20 * time.Second))
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(c, got); err != nil {
			upDone <- fmt.Errorf("upstream read: %w", err)
			return
		}
		if !bytes.Equal(got, payload) {
			upDone <- fmt.Errorf("upstream payload mismatch")
			return
		}
		if _, err := c.Write(payload); err != nil {
			upDone <- fmt.Errorf("upstream write: %w", err)
			return
		}
		upDone <- nil
	}()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleConnect(w, r, directTestDialer{}, 0)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	c, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))

	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", upLn.Addr(), upLn.Addr()); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("tunnel write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("tunnel read after %d/%d bytes: %v", len(got), len(payload), err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("tunnel payload mismatch")
	}
	if err := <-upDone; err != nil {
		t.Fatal(err)
	}
}

// The plain-HTTP response path (a large binary/JSON body) must also stream all
// bytes, not just the first read that fits an internal buffer.
func TestPlainHTTPMovesMultiMegabytePayload(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefghijklmnop"), 256*1024) // 4 MiB
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
		_, _ = fmt.Fprintf(upstream, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(payload))
		_, _ = upstream.Write(payload)
	}()

	initConnSem()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlePlainHTTP(w, r, staticDialer{proxySide}, 0)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	proxyURL, _ := url.Parse("http://" + ln.Addr().String())
	client := &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	resp, err := client.Get("http://example.invalid/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("body = %d bytes, want %d (a truncating bridge would stop far earlier)", len(body), len(payload))
	}
}

func TestApplyUpstreamSockopt(t *testing.T) {
	// Nothing requested: no streamSettings/sockopt may appear at all.
	empty := map[string]any{}
	applyUpstreamSockopt(empty, "")
	if _, ok := empty["streamSettings"]; ok {
		t.Fatalf("empty congestion added streamSettings: %v", empty)
	}

	// Subscription intent wins: never overwrite an explicit algorithm.
	existing := map[string]any{
		"streamSettings": map[string]any{
			"sockopt": map[string]any{"tcpCongestion": "cubic"},
		},
	}
	applyUpstreamSockopt(existing, "bbr")
	so := existing["streamSettings"].(map[string]any)["sockopt"].(map[string]any)
	if so["tcpCongestion"] != "cubic" {
		t.Fatalf("tcpCongestion = %v, want subscription's cubic preserved", so["tcpCongestion"])
	}

	// No stream settings yet: create both levels without disturbing others.
	fresh := map[string]any{"streamSettings": map[string]any{"network": "tcp"}}
	applyUpstreamSockopt(fresh, "bbr")
	ss := fresh["streamSettings"].(map[string]any)
	if ss["network"] != "tcp" {
		t.Fatalf("network = %v, want tcp preserved", ss["network"])
	}
	if got := ss["sockopt"].(map[string]any)["tcpCongestion"]; got != "bbr" {
		t.Fatalf("tcpCongestion = %v, want bbr", got)
	}
}

func TestTcpCongestionParsing(t *testing.T) {
	good := []struct{ in, want string }{
		{"", ""},
		{"bbr", "bbr"},
		{" BBR ", "bbr"},
		{"cubic", "cubic"},
		{"westwood", "westwood"},
	}
	for _, tc := range good {
		t.Setenv("XRAY_TCP_CONGESTION", tc.in)
		if got := tcpCongestion(); got != tc.want {
			t.Fatalf("tcpCongestion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"bbr;rm -rf /", "bbr\tbbr", "bbr cubic", "bbr/1"} {
		t.Setenv("XRAY_TCP_CONGESTION", bad)
		if got := tcpCongestion(); got != "" {
			t.Fatalf("tcpCongestion(%q) = %q, want empty (must never render garbage)", bad, got)
		}
	}
}
