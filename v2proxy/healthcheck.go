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

var fallbackHealthURLs = []string{
	"https://www.gstatic.com/generate_204",
	"https://cp.cloudflare.com",
	"http://api.ipify.org",
}

// probeURL is the fastest reliable URL for initial probing — HTTP, no TLS.
const probeURL = "http://www.gstatic.com/generate_204"

func testSingleURL(proxyAddr, testURL string, timeout time.Duration) HealthResult {
	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
	if err != nil {
		return HealthResult{Error: err}
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		MaxIdleConns:          1,
		IdleConnTimeout:       5 * time.Second,
		DisableKeepAlives:     true,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}

	start := time.Now()
	resp, err := client.Get(testURL)
	latency := time.Since(start)

	if err != nil {
		return HealthResult{Error: err, Latency: latency}
	}
	resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return HealthResult{Working: true, Latency: latency}
	}

	return HealthResult{Working: false, Latency: latency}
}

func TestProxyHealth(proxyAddr string, primaryURL string, timeout time.Duration) HealthResult {
	// 1. Try primary URL
	urlsToTry := []string{}
	if primaryURL != "" {
		urlsToTry = append(urlsToTry, primaryURL)
	}

	// Add fallbacks if not already present
	for _, fb := range fallbackHealthURLs {
		if fb != primaryURL {
			urlsToTry = append(urlsToTry, fb)
		}
	}

	var lastRes HealthResult
	for _, url := range urlsToTry {
		res := testSingleURL(proxyAddr, url, timeout)
		if res.Working {
			return res
		}
		lastRes = res
	}

	return lastRes
}

// TestProxyQuick does a single fast probe: one URL, 3s timeout, no fallbacks.
// Used during initial population to test many candidates quickly.
func TestProxyQuick(proxyAddr, testURL string) HealthResult {
	url := probeURL
	if testURL != "" {
		url = testURL
	}
	return testSingleURL(proxyAddr, url, 3*time.Second)
}
