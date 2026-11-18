package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
)

var errDialTimeout = errors.New("upstream dial timeout")

// dialSocksTimeout bounds the SOCKS dial: a dead upstream previously hung the
// handler (and its connection slot) indefinitely. Timeout -> caller gets a
// fast 504 instead of a hung client.
//
// Production always takes the context path: proxy.SOCKS5 returns a
// *socks.Dialer that implements proxy.ContextDialer, so cancellation reaches
// the SOCKS handshake reads and x/net closes the in-flight socket. The
// goroutine+timer fallback exists only for custom Dialers that do not
// implement ContextDialer; there a late connection is closed by a cleanup
// goroutine, so a timeout still cannot leak an fd.
func dialSocksTimeout(dialer proxy.Dialer, network, addr string, timeout time.Duration) (net.Conn, error) {
	// The production dialer is a *socks.Dialer, which implements
	// proxy.ContextDialer. Using it is strictly cheaper and safer than the
	// goroutine+timer fallback: cancellation reaches the SOCKS handshake reads,
	// x/net closes the in-flight socket, and no goroutine survives a timeout.
	// The old path returned 504 while the dial and its cleanup goroutine stayed
	// blocked until the peer answered (they never did for a blackholed port).
	if cd, ok := dialer.(proxy.ContextDialer); ok {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		c, err := cd.DialContext(ctx, network, addr)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("%w after %s", errDialTimeout, timeout)
			}
			return nil, err
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// A conn that raced the deadline must not be handed to a caller
			// that is about to receive a timeout verdict.
			_ = c.Close()
			return nil, fmt.Errorf("%w after %s", errDialTimeout, timeout)
		}
		return c, nil
	}

	type res struct {
		c   net.Conn
		err error
	}
	// Buffered so the abandoned goroutine never blocks on send.
	ch := make(chan res, 1)
	go func() {
		c, err := dialer.Dial(network, addr)
		ch <- res{c, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-timer.C:
		go func() {
			if r := <-ch; r.c != nil {
				_ = r.c.Close()
			}
		}()
		return nil, fmt.Errorf("%w after %s", errDialTimeout, timeout)
	}
}

func dialErrorCode(err error) int {
	if errors.Is(err, errDialTimeout) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

var relayBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, relayBufSize)
		return &b
	},
}

var connSem chan struct{}

// initConnSem fills the connection-slot semaphore exactly once; later
// MAX_CONNS changes intentionally do not resize the live channel.
var initConnSem = sync.OnceFunc(func() {
	max := getMaxConns()
	connSem = make(chan struct{}, max)
	for range max {
		connSem <- struct{}{}
	}
})

func getMaxConns() int {
	if v := os.Getenv("MAX_CONNS"); v != "" {
		// Clamped like switchWorkerCount/portOrDefault/aggregatePort. This was the
		// only env knob with no ceiling, and initConnSem fills the channel once
		// per slot - so MAX_CONNS=1e9 stalled boot for ~25s instead of failing
		// safe.
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > maxConnsCap {
				warnLog("MAX_CONNS=%d exceeds cap %d, using %d", n, maxConnsCap, maxConnsCap)
				return maxConnsCap
			}
			return n
		}
		warnLog("MAX_CONNS=%q invalid, using default %d", v, defaultMaxConns)
	}
	return defaultMaxConns
}

func startHTTPProxy(addr, socksAddr string) {
	initConnSem()

	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		errLog("HTTP proxy dialer failed: %v", err)
		return
	}

	// Bridge identity for passive health: any proven byte flow through this
	// listener timestamps the instance as responsive (ground truth that
	// outranks synthetic probes). Unparseable addr disables notes silently.
	httpPort := 0
	if _, p, err := net.SplitHostPort(addr); err == nil {
		_, _ = fmt.Sscanf(p, "%d", &httpPort)
	}

	server := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: bridgeReadHeaderTimeout,
		IdleTimeout:       bridgeIdleTimeout,
		MaxHeaderBytes:    bridgeMaxHeaderBytes,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				handleConnect(w, r, dialer, httpPort)
			} else {
				handlePlainHTTP(w, r, dialer, httpPort)
			}
		}),
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		errLog("HTTP proxy bind failed on %s: %v", addr, err)
		return
	}

	go func() {
		debugLog("HTTP proxy on %s (via %s)", addr, socksAddr)
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			errLog("HTTP proxy error: %v", err)
		}
	}()
}

// egressNotes timestamps proven byte flows per instance port: the passive
// health registry. A proxy that just served real traffic is responsive by
// definition — HealthCheck trusts freshness here over synthetic probes.
//
// A sync.Map of *atomic.Int64, not a mutex-guarded map: this is written from
// every relay goroutine on every noted hop, so a single global mutex made all
// concurrent tunnels contend on one cache line for a timestamp nobody reads
// more than once a tick. Loads for the fixed per-instance port set are
// lock-free in the read-mostly path.
var egressNotes sync.Map // map[int]*atomic.Int64

// egressEpoch anchors the monotonic timestamps stored in egressNotes. Using
// time.Since(egressEpoch) keeps the ledger immune to wall-clock jumps, which a
// raw UnixNano stamp would not be: an NTP step back would age every live note
// at once and let a probe strike a serving instance.
var egressEpoch = time.Now()

func egressElapsed() int64 { return int64(time.Since(egressEpoch)) }

// httpPortEndpoints maps a bridge listener port to the upstream active behind
// it. The manager updates it on every markOK/switch, so a short-body count is
// attributed to the NODE, not to the local port that survives the switch.
var httpPortEndpoints sync.Map // map[int]string

// shortTransfers counts plain-HTTP bodies that ended before Content-Length.
// TLS/CONNECT bodies are opaque here; the throughput gate covers those.
var shortTransfers sync.Map // map[string]*atomic.Int64

func setHTTPPortEndpoint(port int, endpoint string) {
	if port <= 0 {
		return
	}
	endpoint = normalizeEndpoint(endpoint)
	if endpoint == "" {
		httpPortEndpoints.Delete(port)
		return
	}
	httpPortEndpoints.Store(port, endpoint)
}

func noteShortTransfer(port int) {
	v, ok := httpPortEndpoints.Load(port)
	if !ok {
		return
	}
	endpoint, _ := v.(string)
	if endpoint == "" {
		return
	}
	c, _ := shortTransfers.LoadOrStore(endpoint, new(atomic.Int64))
	c.(*atomic.Int64).Add(1)
}

func shortTransferCount(endpoint string) int64 {
	v, ok := shortTransfers.Load(normalizeEndpoint(endpoint))
	if !ok {
		return 0
	}
	return v.(*atomic.Int64).Load()
}

func clearShortTransfers(endpoint string) {
	shortTransfers.Delete(normalizeEndpoint(endpoint))
}

// noteEgress records a proven flow for one instance port (bridge HTTP port or
// serving SOCKS port). Port 0 (unparseable listener addr) is dropped: a note
// that matches nothing must never fake health.
func noteEgress(port int) {
	if port <= 0 {
		return
	}
	v, ok := egressNotes.Load(port)
	if !ok {
		v, _ = egressNotes.LoadOrStore(port, new(atomic.Int64))
	}
	v.(*atomic.Int64).Store(egressElapsed())
}

// egressActive reports whether ANY of the given instance ports proved a flow
// within grace. Pure read for HealthCheck; keys are the fixed per-instance
// port set, so the map stays tiny forever.
func egressActive(ports []int, grace time.Duration) bool {
	cutoff := egressElapsed() - int64(grace)
	for _, p := range ports {
		if v, ok := egressNotes.Load(p); ok && v.(*atomic.Int64).Load() > cutoff {
			return true
		}
	}
	return false
}

// acquireConnSlot takes one MAX_CONNS slot, waiting at most proxySlotWait.
// The fast path is non-blocking: the old code created a time.After timer for
// EVERY request even when a slot was free, leaving one timer to fire (and one
// wakeup) per connection five seconds later. At hundreds of requests/second
// that is a timer-heap storm for no reason.
func acquireConnSlot() bool {
	select {
	case <-connSem:
		return true
	default:
	}
	timer := time.NewTimer(proxySlotWait)
	defer timer.Stop()
	select {
	case <-connSem:
		return true
	case <-timer.C:
		return false
	}
}

func handlePlainHTTP(w http.ResponseWriter, r *http.Request, dialer proxy.Dialer, httpPort int) {
	// Acquire connection slot to prevent unbounded HTTP request goroutines
	if !acquireConnSlot() {
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	defer func() { connSem <- struct{}{} }()

	host := r.URL.Host
	if host == "" {
		host = r.Host
	}
	if !strings.Contains(host, ":") {
		host += ":80"
	}

	conn, err := dialSocksTimeout(dialer, "tcp", host, bridgeDialTimeout)
	if err != nil {
		http.Error(w, err.Error(), dialErrorCode(err))
		return
	}
	defer func() { _ = conn.Close() }()

	r.Header.Del("Proxy-Connection")
	r.Header.Del("Proxy-Authorization")

	// The write side is an IDLE budget, not an absolute one: a large upload
	// (Telegram file, video publish) legitimately takes longer than the
	// header-phase budget, and the old single SetDeadline severed it mid-body
	// at exactly bridgeUpstreamDeadline. Each Write re-arms, so only a server
	// that stops accepting bytes trips it.
	if err := r.Write(idleWriteConn{Conn: conn, idle: bridgeUpstreamDeadline}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// The response head keeps the absolute anti-stall budget: a server that
	// never answers must still recycle the slot.
	_ = conn.SetDeadline(time.Now().Add(bridgeUpstreamDeadline))

	reader := bufio.NewReaderSize(conn, relayBufSize)
	resp, err := http.ReadResponse(reader, r)
	// Informational responses (100 Continue, 103 Early Hints) are not the
	// final response. The old code forwarded the 100 as if it were the body's
	// response, so an Expect: 100-continue upload saw an empty 100 and the real
	// 200 was never read. Skip them; 101 stays final (upgrade semantics).
	for err == nil && resp.StatusCode >= 100 && resp.StatusCode < 200 && resp.StatusCode != http.StatusSwitchingProtocols {
		_ = resp.Body.Close()
		resp, err = http.ReadResponse(reader, r)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	// Response head arrived through the tunnel: full path proven (SOCKS +
	// upstream + egress). Timestamp it — this outranks any synthetic probe.
	noteEgress(httpPort)

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// Header phase gets the absolute anti-stall budget (a stalled server never
	// sends headers, and this is what recycles the slot). The body must NOT
	// inherit it: a large JSON/download that outlives 60s was cut mid-transfer
	// with only a debugLog, and since the status line is already committed the
	// client cannot even be told — it just sees a short body and resumes.
	// The body budget must be an IDLE window re-armed per chunk, exactly as
	// relayWithIdle does. Arming it once here made it an absolute wall again —
	// the total-transfer limit this variable was renamed to stop being — so any
	// body outliving 5 minutes was severed no matter how many bytes had moved,
	// behind an already-committed 200.
	_ = conn.SetDeadline(time.Now().Add(relayIdleDeadline))
	total, copyErr := copyBodyIdle(w, resp.Body, conn)
	if copyErr != nil {
		debugLog("copy response body failed: %v", copyErr)
	}
	// A body shorter than its own Content-Length is a truncation/quality
	// signal, not a client error. Count it per active endpoint so three
	// occurrences can auto-memo the node (the throughput gate covers TLS,
	// where Content-Length is invisible).
	if resp.ContentLength > 0 && total < resp.ContentLength &&
		(copyErr == nil || errors.Is(copyErr, io.ErrUnexpectedEOF)) {
		noteShortTransfer(httpPort)
		debugLog("short upstream body: %d/%d bytes", total, resp.ContentLength)
	}

	if reader.Buffered() > 0 {
		if _, err := io.Copy(w, reader); err != nil {
			debugLog("copy buffered failed: %v", err)
		}
	}
}

func handleConnect(w http.ResponseWriter, r *http.Request, dialer proxy.Dialer, httpPort int) {
	target := r.Host
	if !strings.Contains(target, ":") {
		target += ":443"
	}

	// Acquire connection slot
	if !acquireConnSlot() {
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	// The slot guards the DIAL/HIJACK phase and is returned when this handler
	// returns; the relay goroutine also calls releaseSlot, but sync.Once makes
	// the second call a no-op. sync.Once, NOT a plain `if slotHeld` flag: the
	// closure can run from BOTH goroutines, so an unsynchronised bool is a
	// check-then-act race that can double-release the slot or drop one (a
	// permanent leak). Found by the soak harness under -race.
	//
	// MAX_CONNS therefore bounds concurrent bridge setup work, not how many
	// live tunnels a client may hold. That is deliberate: browsers and
	// Telegram keep many long-lived media sockets, and enforcing the cap on
	// them would return 503 for new segments while the existing tunnels are
	// idle. The idle deadline reclaims genuinely dead tunnels.
	var releaseOnce sync.Once
	releaseSlot := func() { releaseOnce.Do(func() { connSem <- struct{}{} }) }
	defer releaseSlot()

	destConn, err := dialSocksTimeout(dialer, "tcp", target, bridgeDialTimeout)
	if err != nil {
		http.Error(w, err.Error(), dialErrorCode(err))
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = destConn.Close()
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	clientConn, brw, err := hijacker.Hijack()
	if err != nil {
		_ = destConn.Close()
		return
	}

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = destConn.Close()
		_ = clientConn.Close()
		debugLog("hijack write failed: %v", err)
		return
	}

	// A fast client may pipeline the first tunnel bytes (typically the TLS
	// ClientHello) into the same packet as the CONNECT request. The HTTP
	// server has already read those bytes into the hijacked bufio.Reader;
	// discarding brw (the old `_` in Hijack) silently dropped them, and the
	// upstream then waited for a handshake that would never be re-sent.
	if brw != nil && brw.Reader.Buffered() > 0 {
		early, _ := brw.Reader.Peek(brw.Reader.Buffered())
		// The target may stop reading; this write is still part of the setup
		// phase and must not pin the handler/slot forever.
		_ = destConn.SetWriteDeadline(time.Now().Add(bridgeUpstreamDeadline))
		_, werr := destConn.Write(early)
		_ = destConn.SetWriteDeadline(time.Time{})
		if werr != nil {
			debugLog("early tunnel bytes failed: %v", werr)
			_ = destConn.Close()
			_ = clientConn.Close()
			return
		}
	}

	// Any byte in either direction proves the tunnel live (the 200 above only
	// proved the dial), and every later batch re-proves it — see relay. A
	// timestamp is idempotent, so the repeat fires cost nothing but the ledger
	// entry that keeps a long-lived connection from ageing out of the guard.
	egress := func() { noteEgress(httpPort) }
	// The relay owns both connections from here. The connection slot was
	// acquired for the dial/hijack phase and is returned when this handler
	// exits (see above); keeping MAX_CONNS off live tunnels is deliberate.
	go func() {
		defer releaseSlot()
		relayPair(destConn, clientConn, egress)
	}()
}

// plainBodyIsIdleBudget documents (and TestPlainHTTPBodyDeadlineIsRearmed
// asserts) that the plain-HTTP body phase uses a re-armed idle budget rather
// than one absolute wall. A const so the invariant is checkable by the suite.
const plainBodyIsIdleBudget = true

// idleWriteConn turns a write deadline into an IDLE budget: every successful
// Write pushes the deadline forward. Request.Write() uses it to stream an
// arbitrarily large request body without inheriting one absolute wall.
type idleWriteConn struct {
	net.Conn
	idle time.Duration
}

func (c idleWriteConn) Write(p []byte) (int, error) {
	if c.idle > 0 {
		_ = c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	}
	return c.Conn.Write(p)
}

// copyBodyIdle streams a response body under an IDLE budget: conn's deadline is
// re-armed after every chunk that actually moves bytes, so a large transfer is
// never severed merely for taking a long time, while a genuinely silent peer
// still trips the budget. This mirrors relayWithIdle's arm() discipline, which
// the plain-HTTP path was missing.
func copyBodyIdle(w io.Writer, body io.Reader, conn net.Conn) (int64, error) {
	bufp := relayBufPool.Get().(*[]byte)
	defer relayBufPool.Put(bufp)
	buf := *bufp
	// Re-arm at most once per second (or a quarter of a test-shrunk budget):
	// the deadline only has to notice a fully silent peer, not every chunk.
	rearmEvery := relayIdleDeadline / 4
	if rearmEvery > time.Second {
		rearmEvery = time.Second
	}
	if rearmEvery < 0 {
		rearmEvery = 0
	}
	var lastArm time.Time
	arm := func() {
		now := time.Now()
		if rearmEvery > 0 && now.Sub(lastArm) < rearmEvery {
			return
		}
		_ = conn.SetDeadline(now.Add(relayIdleDeadline))
		lastArm = now
	}
	// Go's ResponseWriter buffers ~4KiB. A live stream (SSE, chunked event
	// feed, long-poll body) whose chunks are smaller than that would sit in
	// the buffer until the next chunk or EOF, which reads as a stall to the
	// client. Flushing after every delivered chunk makes each one visible
	// immediately; for bulk 64KiB copies Flush is a no-op on an empty buffer.
	var flush func()
	if f, ok := w.(http.Flusher); ok {
		flush = f.Flush
	}
	var total int64
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			m, writeErr := w.Write(buf[:n])
			total += int64(m)
			if writeErr != nil {
				return total, writeErr
			}
			if flush != nil {
				flush()
			}
			arm()
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

// setKeepAlive arms dead-peer detection on a relayed stream. Full tuning
// (idle/interval/count) where the platform allows, classic keepalive period
// otherwise. Non-TCP conns are left alone. Never fails the caller: worst case
// is today's behavior (absolute deadline only).
func setKeepAlive(c net.Conn) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	if err := tc.SetKeepAliveConfig(net.KeepAliveConfig{
		Enable:   true,
		Idle:     keepAliveIdle,
		Interval: keepAliveInterval,
		Count:    keepAliveCount,
	}); err != nil {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(keepAliveIdle)
	}
}

// relay splices two connections until either side finishes, with an IDLE
// deadline (relayIdleDeadline) re-armed while bytes move.
//
// onFlow is invoked on the FIRST byte, then at most once per
// egressNoteInterval (one second). noteEgress is a freshness stamp, so the
// replay keeps a live tunnel noted for as long as it carries traffic while a
// genuinely silent tunnel still reads as idle. The old callback fired on every
// 32KiB hop; the ledger only answers "was there traffic within egressGrace",
// so the extra map writes bought no extra truth.
func relay(dst, src net.Conn, onFlow func()) {
	relayWithIdle(dst, src, onFlow, relayIdleDeadline)
}

// relayPair splices two connections in both directions and closes both only
// after BOTH directions finish. Each direction half-closes the peer on a clean
// EOF, so a protocol that signals "request complete" with a TCP FIN still gets
// its response: the old code closed both sockets as soon as the first
// direction ended, truncating every such exchange. A hard error still tears
// both down immediately so a dead peer cannot pin the other direction.
func relayPair(a, b net.Conn, onFlow func()) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); relay(b, a, onFlow) }() // a -> b
	go func() { defer wg.Done(); relay(a, b, onFlow) }() // b -> a
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

// closeWrite sends FIN without tearing down the read side. *net.TCPConn is the
// production case; anything else (net.Pipe in tests, a custom Dialer's conn)
// falls back to a full close, preserving the old end-of-stream behavior.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// relayWithIdle is relay with the idle budget injectable so the behaviour is
// testable in milliseconds instead of minutes.
//
// The deadline must be re-armed while data moves. A single SetDeadline at entry
// is an ABSOLUTE wall: every connection older than the budget was severed
// mid-stream regardless of how well it was transferring, so large JSON/binary
// bodies that simply took longer than the wall arrived truncated and the client
// had to resume. That is not a blackhole detector, it is a transfer-size limit
// wearing an idle constant's name. Re-arming makes it mean what it says: only a
// peer that stops moving entirely trips it.
func relayWithIdle(dst, src net.Conn, onFlow func(), idle time.Duration) {
	// Configure liveness on src only: relay is used as a PAIR (one goroutine
	// per direction), so every socket is a src exactly once. Setting both ends
	// in both goroutines issued every keepalive syscall twice for no added
	// coverage.
	setKeepAlive(src)

	// Re-arming both deadlines on EVERY hop is two deadline updates per 32KiB
	// chunk. The timeout only matters when a peer goes fully silent, so one
	// re-arm per second is indistinguishable in effect (a 5-minute idle budget
	// becomes at most 5m+1s) while removing that overhead from the throughput
	// path. Small injected budgets (tests) keep a proportional re-arm cadence.
	rearmEvery := idle / 4
	if rearmEvery > time.Second {
		rearmEvery = time.Second
	}
	if rearmEvery < 0 {
		rearmEvery = 0
	}
	var lastRearm time.Time
	arm := func(force bool) {
		now := time.Now()
		if !force && rearmEvery > 0 && now.Sub(lastRearm) < rearmEvery {
			return
		}
		d := now.Add(idle)
		_ = dst.SetDeadline(d)
		_ = src.SetDeadline(d)
		lastRearm = now
	}
	arm(true)

	bufp := relayBufPool.Get().(*[]byte)
	defer relayBufPool.Put(bufp)

	buf := *bufp
	// The passive ledger answers "was there traffic within egressGrace (90s)",
	// so a once-per-second stamp is just as conclusive as one per 32KiB chunk.
	// The FIRST byte always fires immediately, which is what makes a newly
	// active tunnel visible on the very next health read.
	var lastFlow time.Time
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if onFlow != nil && (lastFlow.IsZero() || time.Since(lastFlow) >= egressNoteInterval) {
				onFlow()
				lastFlow = time.Now()
			}
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				debugLog("relay write ended stream: %v", writeErr)
				_ = dst.Close()
				_ = src.Close()
				return
			}
		}
		if readErr != nil {
			// EOF and reset are the ordinary end of a proxied stream and stay
			// quiet. A timeout is the one that matters, and it used to be
			// swallowed completely — which is why a truncated download looked
			// like a server problem with no trace on our side.
			if errors.Is(readErr, io.EOF) {
				// Clean end of THIS direction: let the reverse direction keep
				// draining (a request body FIN must not kill the response).
				closeWrite(dst)
				return
			}
			debugLog("relay read ended stream after %s idle: %v", idle, readErr)
			_ = dst.Close()
			_ = src.Close()
			return
		}
		arm(false)
	}
}
