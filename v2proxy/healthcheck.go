package main

import (
	"cmp"
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

type HealthResult struct {
	Latency time.Duration
	Working bool
	Error   error
	// Status is the HTTP status when an exchange completed (0 = transport
	// failure, no response). Lets callers separate "tunnel dead" from
	// "probe target throttled us" (429/403 condemn nothing).
	Status int
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
		return HealthResult{Working: true, Latency: latency, Status: resp.StatusCode}
	}

	return HealthResult{Working: false, Latency: latency, Status: resp.StatusCode}
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

// telegramWorking is the acceptance predicate: any COMPLETED exchange proves
// Telegram reachability (even a 404 envelope); only transport errors fail.
// Pure so it stays unit-testable — the SOCKS/TLS transport underneath is the
// same testSingleURL path the 204 legs already prove hermetically.
func telegramWorking(res HealthResult) bool {
	return res.Error == nil
}

// testTelegram probes Telegram reachability directly: generic-204-ok-but-
// Telegram-filtered would otherwise be a false healthy. Kill-switch:
// TELEGRAM_PROBE=0.
func testTelegram(proxyAddr string) HealthResult {
	res := testSingleURL(proxyAddr, telegramProbeURL, quickProbeTimeout)
	res.Working = telegramWorking(res)
	return res
}

func telegramProbeEnabled() bool {
	return strings.TrimSpace(os.Getenv("TELEGRAM_PROBE")) != "0"
}

// TestProxyQuick races the primary URL against independent fallbacks
// (Google vs Cloudflare infra, plus Telegram itself), 3s each. First success
// wins, so the cost is ALWAYS <= 3s — but a single filtered/blocked endpoint
// can no longer condemn a healthy tunnel. Every URL travels through the xray
// SOCKS port under test; nothing here is a local TCP check.
func TestProxyQuick(proxyAddr, testURL string) HealthResult {
	primary := cmp.Or(testURL, probeURL)
	targets := []func() HealthResult{
		func() HealthResult { return testSingleURL(proxyAddr, primary, quickProbeTimeout) },
	}
	if quickFallbackURL != "" && quickFallbackURL != primary {
		targets = append(targets, func() HealthResult {
			return testSingleURL(proxyAddr, quickFallbackURL, quickProbeTimeout)
		})
	}
	if telegramProbeEnabled() && telegramProbeURL != primary && telegramProbeURL != quickFallbackURL {
		targets = append(targets, func() HealthResult { return testTelegram(proxyAddr) })
	}
	if len(targets) == 1 {
		return targets[0]()
	}
	ch := make(chan HealthResult, len(targets))
	for _, fn := range targets {
		go func() { ch <- fn() }()
	}
	first := <-ch
	if first.Working {
		return first
	}
	for range len(targets) - 1 {
		if next := <-ch; next.Working {
			return next
		}
	}
	return first
}
