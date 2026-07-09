package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/proxy"
)

func startHTTPProxy(addr, socksAddr string) {
	transport := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			return url.Parse(fmt.Sprintf("socks5://%s", socksAddr))
		},
	}

	server := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				handleConnect(w, r, socksAddr)
			} else {
				handlePlainHTTP(w, r, transport)
			}
		}),
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("HTTP proxy bind failed on %s: %v (SOCKS5 on :1080)", addr, err)
		return
	}

	go func() {
		log.Printf("HTTP proxy on %s (via %s)", addr, socksAddr)
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP proxy error: %v", err)
		}
	}()
}

func handlePlainHTTP(w http.ResponseWriter, r *http.Request, transport *http.Transport) {
	r.Header.Del("Proxy-Connection")
	r.Header.Del("Proxy-Authorization")

	resp, err := transport.RoundTrip(r)
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
}

func handleConnect(w http.ResponseWriter, r *http.Request, socksAddr string) {
	target := r.Host
	if !strings.Contains(target, ":") {
		target += ":443"
	}

	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	conn, err := dialer.Dial("tcp", target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		conn.Close()
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		conn.Close()
		return
	}

	go relay(conn, clientConn)
	go relay(clientConn, conn)
}

func relay(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	io.Copy(dst, src)
}
