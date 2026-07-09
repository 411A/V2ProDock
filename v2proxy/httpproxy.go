package main

import (
	"bufio"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

func startHTTPProxy(addr, socksAddr string) {
	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		log.Printf("HTTP proxy dialer failed: %v", err)
		return
	}

	server := &http.Server{
		Addr: addr,
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
		log.Printf("HTTP proxy bind failed on %s: %v (SOCKS5 on :27019)", addr, err)
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

	// Remove proxy headers
	r.Header.Del("Proxy-Connection")
	r.Header.Del("Proxy-Authorization")

	// Write request
	if err := r.Write(conn); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	// Read response
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	// Forward response
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)

	// If there's buffered data, send it
	if reader.Buffered() > 0 {
		io.Copy(w, reader)
	}
}

func handleConnect(w http.ResponseWriter, r *http.Request, dialer proxy.Dialer) {
	target := r.Host
	if !strings.Contains(target, ":") {
		target += ":443"
	}

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

	// Send proper CONNECT response
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	go relay(destConn, clientConn)
	go relay(clientConn, destConn)
}

func relay(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()

	// Set deadlines to prevent hanging connections
	deadline := time.Now().Add(5 * time.Minute)
	dst.SetDeadline(deadline)
	src.SetDeadline(deadline)

	io.Copy(dst, src)
}
