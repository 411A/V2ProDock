package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

var errDialTimeout = errors.New("upstream dial timeout")

// dialSocksTimeout bounds the SOCKS dial: a dead upstream previously hung the
// handler (and its connection slot) indefinitely. Timeout -> caller gets a
// fast 504 instead of a hung client.
//
// The abandoned goroutine is NOT left parked and a late connection is NOT
// dropped: x/net's socks.Dialer resolves the proxy with context.Background(), so
// its handshake reads are unbounded. The previous version returned 504 while
// that goroutine stayed blocked in io.ReadFull forever, and if the handshake
// later succeeded the fully-open net.Conn was discarded with no Close - an fd
// leak on every timeout that raced. Closing the orphan is the only way to keep
// the fd table bounded.
func dialSocksTimeout(dialer proxy.Dialer, network, addr string, timeout time.Duration) (net.Conn, error) {
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
var egressNotes = struct {
	mu     sync.Mutex
	lastOK map[int]time.Time
}{lastOK: map[int]time.Time{}}

// noteEgress records a proven flow for one instance port (bridge HTTP port or
// serving SOCKS port). Port 0 (unparseable listener addr) is dropped: a note
// that matches nothing must never fake health.
func noteEgress(port int) {
	if port <= 0 {
		return
	}
	egressNotes.mu.Lock()
	defer egressNotes.mu.Unlock()
	egressNotes.lastOK[port] = time.Now()
}

// egressActive reports whether ANY of the given instance ports proved a flow
// within grace. Pure read for HealthCheck; keys are the fixed per-instance
// port set, so the map stays tiny forever.
func egressActive(ports []int, grace time.Duration) bool {
	cutoff := time.Now().Add(-grace)
	egressNotes.mu.Lock()
	defer egressNotes.mu.Unlock()
	for _, p := range ports {
		if t, ok := egressNotes.lastOK[p]; ok && t.After(cutoff) {
			return true
		}
	}
	return false
}

func handlePlainHTTP(w http.ResponseWriter, r *http.Request, dialer proxy.Dialer, httpPort int) {
	// Acquire connection slot to prevent unbounded HTTP request goroutines
	select {
	case <-connSem:
	case <-time.After(proxySlotWait):
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
	// Absolute budget for the whole upstream exchange so a stalled server
	// cannot pin a connection slot forever (CONNECT streams keep relayIdleDeadline).
	_ = conn.SetDeadline(time.Now().Add(bridgeUpstreamDeadline))

	r.Header.Del("Proxy-Connection")
	r.Header.Del("Proxy-Authorization")

	if err := r.Write(conn); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	reader := bufio.NewReaderSize(conn, relayBufSize)
	resp, err := http.ReadResponse(reader, r)
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
	if _, err := copyBodyIdle(w, resp.Body, conn); err != nil {
		debugLog("copy response body failed: %v", err)
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
	select {
	case <-connSem:
	case <-time.After(proxySlotWait):
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	// The slot is released by the RELAY PAIR, not by this handler's defer. The
	// handler returns as soon as the tunnel is spliced while the tunnel itself
	// lives as long as the client holds it open, so a defer here held the slot
	// for the dial only and MAX_CONNS bounded nothing on the CONNECT path — the
	// dominant one for an HTTPS bridge. Every abandoned tunnel pinned an fd, two
	// goroutines and a pooled buffer with nothing to stop it.
	// sync.Once, NOT a plain `if slotHeld` flag: this closure runs from BOTH the
	// handler goroutine's defer and the relay goroutine's defer, so an
	// unsynchronised bool is a check-then-act race that can double-release the
	// slot (MAX_CONNS silently stops bounding anything) or drop one (a permanent
	// leak). Found by the soak harness under -race, reproduced with a bare loop
	// of CONNECT tunnels and no rig.
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

	clientConn, _, err := hijacker.Hijack()
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

	// Any byte in either direction proves the tunnel live (the 200 above only
	// proved the dial), and every later batch re-proves it — see relay. A
	// timestamp is idempotent, so the repeat fires cost nothing but the ledger
	// entry that keeps a long-lived connection from ageing out of the guard.
	egress := func() { noteEgress(httpPort) }
	// Hand the slot to the relays: released once BOTH directions finish, so one
	// tunnel occupies exactly one connection slot for its whole life.
	go func() {
		defer releaseSlot()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); relay(destConn, clientConn, egress) }()
		go func() { defer wg.Done(); relay(clientConn, destConn, egress) }()
		wg.Wait()
	}()
}

// plainBodyIsIdleBudget documents (and TestPlainHTTPBodyDeadlineIsRearmed
// asserts) that the plain-HTTP body phase uses a re-armed idle budget rather
// than one absolute wall. A const so the invariant is checkable by the suite.
const plainBodyIsIdleBudget = true

// copyBodyIdle streams a response body under an IDLE budget: conn's deadline is
// re-armed after every chunk that actually moves bytes, so a large transfer is
// never severed merely for taking a long time, while a genuinely silent peer
// still trips the budget. This mirrors relayWithIdle's arm() discipline, which
// the plain-HTTP path was missing.
func copyBodyIdle(w io.Writer, body io.Reader, conn net.Conn) (int64, error) {
	bufp := relayBufPool.Get().(*[]byte)
	defer relayBufPool.Put(bufp)
	buf := *bufp
	var total int64
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			m, writeErr := w.Write(buf[:n])
			total += int64(m)
			if writeErr != nil {
				return total, writeErr
			}
			_ = conn.SetDeadline(time.Now().Add(relayIdleDeadline))
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
// deadline (relayIdleDeadline) re-armed after every successful hop.
//
// onFlow is invoked after every read that ACTUALLY MOVED BYTES — not once, on
// the first one. noteEgress is a one-shot-per-connection timestamp, so a client
// that connected once and then sat on the tunnel (mid-long-poll, waiting on a
// reply nobody is going to send) carried no note after egressGrace and read as
// idle while its connection was very much alive — on the aggregate path, which
// is already protected against rotation by this very ledger. Re-firing per hop
// keeps a live tunnel noted for as long as it carries traffic and costs no
// timer and no extra goroutine: the relay loop already owns the wakeup, and it
// fires only when n > 0, so a genuinely silent tunnel still reads as idle.
func relay(dst, src net.Conn, onFlow func()) {
	relayWithIdle(dst, src, onFlow, relayIdleDeadline)
}

// relayWithIdle is relay with the idle budget injectable so the behaviour is
// testable in milliseconds instead of minutes.
//
// The deadline must be re-armed per hop. A single SetDeadline at entry is an
// ABSOLUTE wall: every connection older than the budget was severed mid-stream
// regardless of how well it was transferring, so large JSON/binary bodies that
// simply took longer than the wall arrived truncated and the client had to
// resume. That is not a blackhole detector, it is a transfer-size limit wearing
// an idle constant's name. Re-arming per hop makes it mean what it says: only a
// peer that stops moving entirely trips it.
func relayWithIdle(dst, src net.Conn, onFlow func(), idle time.Duration) {
	// Both ends: a blackholed upstream must surface here (relay unblocks,
	// both sides close, client reconnects) instead of hanging to the deadline.
	setKeepAlive(dst)
	setKeepAlive(src)
	defer func() { _ = dst.Close() }()
	defer func() { _ = src.Close() }()

	arm := func() {
		d := time.Now().Add(idle)
		_ = dst.SetDeadline(d)
		_ = src.SetDeadline(d)
	}
	arm()

	bufp := relayBufPool.Get().(*[]byte)
	defer relayBufPool.Put(bufp)

	buf := *bufp
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if onFlow != nil {
				onFlow()
			}
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				debugLog("relay write ended stream: %v", writeErr)
				return
			}
		}
		if readErr != nil {
			// EOF and reset are the ordinary end of a proxied stream and stay
			// quiet. A timeout is the one that matters, and it used to be
			// swallowed completely — which is why a truncated download looked
			// like a server problem with no trace on our side.
			if !errors.Is(readErr, io.EOF) {
				debugLog("relay read ended stream after %s idle: %v", idle, readErr)
			}
			return
		}
		arm()
	}
}
