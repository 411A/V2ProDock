package main

import (
	"cmp"
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

func testSingleURL(proxyAddr, testURL string, timeout time.Duration) HealthResult {
	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
	if err != nil {
		return HealthResult{Error: err}
	}

	transport := &http.Transport{
		DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		},
		TLSHandshakeTimeout:   healthTLSHandshakeTimeout,
		ResponseHeaderTimeout: healthResponseHeaderTimeout,
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

// TestProxyQuick races the primary URL against one independent fallback
// (different infrastructure: Google vs Cloudflare), 3s each. First success
// wins, so the cost is ALWAYS <= 3s — but a single filtered/blocked endpoint
// can no longer condemn a healthy tunnel. Both URLs travel through the xray
// SOCKS port under test; nothing here is a local TCP check.
func TestProxyQuick(proxyAddr, testURL string) HealthResult {
	primary := cmp.Or(testURL, probeURL)
	if quickFallbackURL == "" || quickFallbackURL == primary {
		return testSingleURL(proxyAddr, primary, quickProbeTimeout)
	}
	type out struct {
		res HealthResult
		fb  bool
	}
	ch := make(chan out, 2)
	go func() { ch <- out{testSingleURL(proxyAddr, primary, quickProbeTimeout), false} }()
	go func() { ch <- out{testSingleURL(proxyAddr, quickFallbackURL, quickProbeTimeout), true} }()
	first := <-ch
	if first.res.Working {
		return first.res
	}
	second := <-ch
	if second.res.Working {
		return second.res
	}
	return first.res
}
