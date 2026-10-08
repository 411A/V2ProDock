package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
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
	// URL is the probe target that produced this verdict. Every verdict must
	// name the URL it was measured against: a bare "failed" cannot be acted
	// on (which target throttled us? which one answered?), and it is what
	// makes "is THIS url reachable through the proxy" answerable at all.
	URL string
}

// Describe renders a verdict for logs and status output: which target, which
// outcome. A completed exchange with a failing status has Error == nil, so
// naive %v formatting logged a bare "<nil>" for the exact case an operator
// needs to read (target answered, we did not like the answer).
func (r HealthResult) Describe() string {
	target := r.URL
	if target == "" {
		target = "<no target>"
	}
	switch {
	case r.Error != nil:
		return target + ": " + r.Error.Error()
	case r.Working && r.Status != 0:
		return fmt.Sprintf("%s: %d ok", target, r.Status)
	case r.Working:
		return target + ": ok"
	default:
		return fmt.Sprintf("%s: status %d", target, r.Status)
	}
}

func testSingleURL(proxyAddr, testURL string, timeout time.Duration) HealthResult {
	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
	if err != nil {
		return HealthResult{Error: err, URL: testURL}
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
		return HealthResult{Error: err, Latency: latency, URL: testURL}
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain before Close so the transport is free to complete the exchange and
	// reuse the connection; an undrained body is also what turns a truncated
	// response into a confusing "unexpected EOF" on the NEXT probe.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeDrainCap))

	return HealthResult{
		Working: resp.StatusCode >= 200 && resp.StatusCode < 400,
		Latency: latency,
		Status:  resp.StatusCode,
		URL:     testURL,
	}
}

// telegramWorking is the acceptance predicate: any COMPLETED exchange proves
// Telegram reachability (even a 404 envelope); only transport errors fail.
// Pure so it stays unit-testable — the SOCKS/TLS transport underneath is the
// same testSingleURL path the 204 legs already prove hermetically.
func telegramWorking(res HealthResult) bool {
	return res.Error == nil
}

func telegramProbeEnabled() bool {
	return strings.TrimSpace(os.Getenv("TELEGRAM_PROBE")) != "0"
}

// probeInconclusive reports a verdict produced by the PROBE TARGET rather
// than by the tunnel: 429/403 mean the target throttled or forbade our egress
// IP while the upstream may be perfectly alive. Such a result condemns
// NOTHING — treating it as death rotates a healthy pool off a node that a
// moment later works fine, and (worse) poisons the shared probe ledger so
// every peer instance skips that candidate too.
func probeInconclusive(res HealthResult) bool {
	return res.Status == http.StatusTooManyRequests || res.Status == http.StatusForbidden
}

// probeVerdict is what a pool must DO with one candidate after probing it.
type probeVerdict int

const (
	// verdictAdopt: proven reachable, serve it.
	verdictAdopt probeVerdict = iota
	// verdictUnproven: the probe TARGET answered us, not the upstream
	// (throttled/forbidden). The upstream is untested, not dead — keep it
	// claimable and never poison the shared ledger with it.
	verdictUnproven
	// verdictReject: proven dead or unreachable. Poison the shared ledger so
	// peer instances do not waste probes on it.
	verdictReject
)

func (v probeVerdict) String() string {
	switch v {
	case verdictAdopt:
		return "adopt"
	case verdictUnproven:
		return "unproven"
	case verdictReject:
		return "reject"
	default:
		return "unknown"
	}
}

// classifyProbe maps one probe result to the action the pool must take.
// Extracted from tryConfigs so the "unproven is not dead" rule is directly
// testable: it is the single decision that keeps a throttled probe target
// from culling an entire healthy pool.
func classifyProbe(res HealthResult) probeVerdict {
	switch {
	case res.Working:
		return verdictAdopt
	case probeInconclusive(res):
		return verdictUnproven
	default:
		return verdictReject
	}
}

// probeLeg is one raced probe target plus the rule that grades its outcome.
type probeLeg struct {
	url   string
	grade func(HealthResult) bool
}

// statusOK is the default grading: only a 2xx/3xx body proves the tunnel.
func statusOK(res HealthResult) bool { return res.Working }

// probeLegs returns the exact, DEDUPED set of targets TestProxyQuick races
// for testURL: the configured primary (or probeURL), the Cloudflare
// fallback on independent infrastructure, and — unless switched off or
// already covered — Telegram's API root. Membership is a pure function so it
// is unit-testable without a network; TestProxyQuick owns only the race.
//
// Dedup matters: with HEALTH_CHECK_URL set to one of the constants the
// duplicate leg used to double the concurrent probe traffic for nothing.
func probeLegs(testURL string) []probeLeg {
	primary := cmp.Or(testURL, probeURL)
	legs := []probeLeg{{url: primary, grade: statusOK}}
	if quickFallbackURL != "" && quickFallbackURL != primary {
		legs = append(legs, probeLeg{url: quickFallbackURL, grade: statusOK})
	}
	has := func(u string) bool {
		return slices.ContainsFunc(legs, func(l probeLeg) bool { return l.url == u })
	}
	if telegramProbeEnabled() && !has(telegramProbeURL) {
		legs = append(legs, probeLeg{url: telegramProbeURL, grade: telegramWorking})
	}
	return legs
}

// TestProxyQuick races every probe target simultaneously (3s each) and returns
// the first leg whose own grading rule accepts it. Every leg travels through
// the xray SOCKS port under test; nothing here is a local TCP check.
//
// Cost is ALWAYS <= one 3s budget, and no single filtered or throttled
// endpoint can condemn a healthy tunnel — that is the entire reason the
// fallback exists (plain-HTTP probes die under DPI even through a working
// tunnel; Telegram's ASN can be filtered while every generic site answers).
func TestProxyQuick(proxyAddr, testURL string) HealthResult {
	return raceProbe(proxyAddr, probeLegs(testURL))
}

// raceProbe is the race itself, parameterized over the leg set so the
// behaviour that matters (fallback rescues a filtered primary, Telegram's
// accept-any-404 rule, throttled targets staying inconclusive) is provable
// hermetically against loopback targets. The production leg set is composed by
// probeLegs; keeping the two apart means testing the race never requires
// mutating a deployment constant — and no test ever has to fake a TLS
// handshake to reach an https:// probe URL.
func raceProbe(proxyAddr string, legs []probeLeg) HealthResult {
	if len(legs) == 0 {
		return HealthResult{Error: errors.New("no probe targets configured")}
	}
	if len(legs) == 1 {
		return testSingleURL(proxyAddr, legs[0].url, quickProbeTimeout)
	}
	type outcome struct {
		res HealthResult
		leg probeLeg
	}
	// Buffered for every leg: a losing leg must never block on send, or the
	// race would leak one goroutine per probe forever.
	ch := make(chan outcome, len(legs))
	for _, leg := range legs {
		go func() {
			ch <- outcome{res: testSingleURL(proxyAddr, leg.url, quickProbeTimeout), leg: leg}
		}()
	}
	accept := func(o outcome) (HealthResult, bool) {
		if !o.leg.grade(o.res) {
			return o.res, false
		}
		// Normalize: Telegram's rule accepts a 404, but callers switch on
		// Working. Leave the verdict self-consistent with the rule that won.
		o.res.Working = true
		return o.res, true
	}
	first := <-ch
	if res, ok := accept(first); ok {
		return res
	}
	for range len(legs) - 1 {
		if res, ok := accept(<-ch); ok {
			return res
		}
	}
	return first.res
}
