package main

import (
	"bufio"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

const (
	maxConcurrentConns = 256
	relayBufSize       = 32 * 1024
)

var relayBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, relayBufSize)
		return &b
	},
}

var (
	connSem  = make(chan struct{}, maxConcurrentConns)
	onceInit sync.Once
)

func initConnSem() {
	onceInit.Do(func() {
		for i := 0; i < maxConcurrentConns; i++ {
			connSem <- struct{}{}
		}
	})
}

func startHTTPProxy(addr, socksAddr string) {
	initConnSem()

	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		log.Printf("HTTP proxy dialer failed: %v", err)
		return
	}

	server := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    4096,
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
		log.Printf("HTTP proxy bind failed on %s: %v", addr, err)
		return
	}

	go func() {
		log.Printf("HTTP proxy on %s (via %s)", addr, socksAddr)
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP proxy error: %v", err)
		}
	}()
}

func handlePlainHTTP(w http.ResponseWriter, r *http.Request, dialer proxy.Dialer) {
	host := r.URL.Host
	if host == "" {
		host = r.Host
	}
	if !strings.Contains(host, ":") {
		host += ":80"
	}

	conn, err := dialer.Dial("tcp", host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer conn.Close()

	r.Header.Del("Proxy-Connection")
	r.Header.Del("Proxy-Authorization")

	if err := r.Write(conn); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	reader := bufio.NewReaderSize(conn, relayBufSize)
	resp, err := http.ReadResponse(reader, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)

	if reader.Buffered() > 0 {
		io.Copy(w, reader)
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
	case <-time.After(5 * time.Second):
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	defer func() { connSem <- struct{}{} }()

	destConn, err := dialer.Dial("tcp", target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		destConn.Close()
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		destConn.Close()
		return
	}

	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	go relay(destConn, clientConn)
	go relay(clientConn, destConn)
}

func relay(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()

	deadline := time.Now().Add(5 * time.Minute)
	dst.SetDeadline(deadline)
	src.SetDeadline(deadline)

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
