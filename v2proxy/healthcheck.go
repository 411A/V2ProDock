package main

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

type HealthResult struct {
	Latency time.Duration
	Working bool
	Error   error
}

var (
	transportCacheMu sync.Mutex
	transportCache   = make(map[string]*http.Transport)
)

func getCachedTransport(proxyAddr string) (*http.Transport, error) {
	transportCacheMu.Lock()
	defer transportCacheMu.Unlock()

	if t, ok := transportCache[proxyAddr]; ok {
		return t, nil
	}

	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
	if err != nil {
		return nil, err
	}

	t := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		},
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		MaxIdleConns:          2,
		IdleConnTimeout:       90 * time.Second,
		DisableKeepAlives:     true,
	}

	transportCache[proxyAddr] = t
	return t, nil
}

func TestProxyHealth(proxyAddr string, testURL string, timeout time.Duration) HealthResult {
	result := HealthResult{}

	transport, err := getCachedTransport(proxyAddr)
	if err != nil {
		result.Error = err
		return result
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}

	start := time.Now()
	resp, err := client.Get(testURL)
	latency := time.Since(start)

	if err != nil {
		result.Error = err
		result.Latency = latency
		return result
	}
	resp.Body.Close()

	result.Working = true
	result.Latency = latency
	return result
}
