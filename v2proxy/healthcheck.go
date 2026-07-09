package main

import (
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type HealthResult struct {
	Latency time.Duration
	Working bool
	Error   error
}

func TestProxyHealth(proxyAddr string, testURL string, timeout time.Duration) HealthResult {
	result := HealthResult{}

	transport := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			return url.Parse(fmt.Sprintf("socks5://%s", proxyAddr))
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

	// Accept any response - the proxy is working if we get a response at all
	result.Working = true
	result.Latency = latency
	return result
}
