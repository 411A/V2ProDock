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
func dialSocksTimeout(dialer proxy.Dialer, network, addr string, timeout time.Duration) (net.Conn, error) {
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := dialer.Dial(network, addr)
		ch <- res{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-time.After(timeout):
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
	New: func() interface{} {
		b := make([]byte, relayBufSize)
		return &b
	},
}

var (
	connSem  chan struct{}
	onceInit sync.Once
)

func getMaxConns() int {
	if v := os.Getenv("MAX_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxConns
}

func initConnSem() {
	onceInit.Do(func() {
		max := getMaxConns()
		connSem = make(chan struct{}, max)
		for i := 0; i < max; i++ {
			connSem <- struct{}{}
		}
	})
}

func startHTTPProxy(addr, socksAddr string) {
	initConnSem()

	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		errLog("HTTP proxy dialer failed: %v", err)
		return
	}

	server := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: bridgeReadHeaderTimeout,
		IdleTimeout:       bridgeIdleTimeout,
		MaxHeaderBytes:    bridgeMaxHeaderBytes,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				handleConnect(w, r, dialer)
			} else {
				handlePlainHTTP(w, r, dialer)
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

func handlePlainHTTP(w http.ResponseWriter, r *http.Request, dialer proxy.Dialer) {
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
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		debugLog("copy response body failed: %v", err)
	}

	if reader.Buffered() > 0 {
		if _, err := io.Copy(w, reader); err != nil {
			debugLog("copy buffered failed: %v", err)
		}
	}
}

func handleConnect(w http.ResponseWriter, r *http.Request, dialer proxy.Dialer) {
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
	defer func() { connSem <- struct{}{} }()

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

	go relay(destConn, clientConn)
	go relay(clientConn, destConn)
}

func relay(dst, src net.Conn) {
	defer func() { _ = dst.Close() }()
	defer func() { _ = src.Close() }()

	deadline := time.Now().Add(relayIdleDeadline)
	_ = dst.SetDeadline(deadline)
	_ = src.SetDeadline(deadline)

	bufp := relayBufPool.Get().(*[]byte)
	defer relayBufPool.Put(bufp)

	buf := *bufp
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if readErr != nil {
			return
		}
	}
}
