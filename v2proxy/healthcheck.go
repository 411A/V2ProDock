package main

import (
	"context"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/proxy"
)

type HealthResult struct {
	Latency time.Duration
	Working bool
	Error   error
}

func TestProxyHealth(proxyAddr string, testURL string, timeout time.Duration) HealthResult {
	result := HealthResult{}

	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
	if err != nil {
		result.Error = err
		return result
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		},
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
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
	defer resp.Body.Close()

	result.Working = true
	result.Latency = latency
	return result
}
