package main

import (
	"bytes"
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
	return testSingleURLContext(context.Background(), proxyAddr, testURL, timeout)
}

// testSingleURLContext is testSingleURL with a cancelable context. The race
// uses it to abort losing legs the moment a winner is accepted, so a healthy
// primary answer no longer leaves two full TLS handshakes running in the
// background until their own 3s budgets expire. A canceled leg returns an
// error that the race discards; no verdict ever depends on it.
func testSingleURLContext(ctx context.Context, proxyAddr, testURL string, timeout time.Duration) HealthResult {
	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
	if err != nil {
		return HealthResult{Error: err, URL: testURL}
	}
	// The SOCKS5 dialer implements ContextDialer, so cancellation reaches the
	// dial and its in-flight handshake reads instead of leaking a goroutine
	// that outlives the probe (the old Dial path had no ctx at all).
	cd, _ := dialer.(proxy.ContextDialer)

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if cd != nil {
				return cd.DialContext(ctx, network, addr)
			}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		return HealthResult{Error: err, URL: testURL}
	}

	start := time.Now()
	resp, err := client.Do(req)
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

// ThroughputResult is one bulk-transfer measurement through an instance's
// SOCKS port. Expected is the body length the target actually promised (after
// Content-Length clamping); Bytes == Expected with Err == nil is a complete
// transfer. Kbps is kilobits/s, so divide by 8 for KB/s.
type ThroughputResult struct {
	Kbps         int64 // tail-window rate (kilobits/s)
	AvgKbps      int64 // whole-body average, for observability
	Bytes        int64
	Expected     int64
	Status       int
	Err          error
	Inconclusive bool // target throttled/forbade us (429/403/5xx): do not demote
	URL          string
	Elapsed      time.Duration
}

// throughputMeter samples byte progress so the caller can score the tail, not
// the average. A node that bursts for 1MiB and then crawls has a good average
// but a zero tail; that is precisely the shaper this gate must catch.
type throughputMeter struct {
	start   time.Time
	total   int64
	samples []throughputSample
}

type throughputSample struct {
	at    time.Time
	total int64
}

func (m *throughputMeter) Write(p []byte) (int, error) {
	m.total += int64(len(p))
	m.samples = append(m.samples, throughputSample{at: time.Now(), total: m.total})
	return len(p), nil
}

func (m *throughputMeter) kbps(end time.Time) (tail, avg int64) {
	elapsed := end.Sub(m.start)
	if elapsed <= 0 {
		elapsed = time.Millisecond
	}
	avg = int64(float64(m.total) * 8 / elapsed.Seconds() / 1000)
	if len(m.samples) == 0 {
		return 0, avg
	}
	cut := end.Add(-throughputTailWindow)
	baseAt, baseTotal := m.start, int64(0)
	found := false
	for _, s := range m.samples {
		if s.at.Before(cut) {
			baseAt, baseTotal = s.at, s.total
			found = true
			continue
		}
		break
	}
	if !found {
		// The whole transfer fits inside the tail window.
		return avg, avg
	}
	tailElapsed := end.Sub(baseAt)
	tailBytes := m.samples[len(m.samples)-1].total - baseTotal
	if tailElapsed < throughputMinTail {
		return avg, avg
	}
	tail = int64(float64(tailBytes) * 8 / tailElapsed.Seconds() / 1000)
	return tail, avg
}

// measureThroughput dials the target through proxyAddr (an instance's xray
// SOCKS port) and streams a body without buffering it. It returns partial
// bytes + elapsed even when the read times out or the peer truncates, so the
// caller can distinguish "slow" from "no measurement".
func measureThroughput(proxyAddr, rawURL string, expected int64, timeout time.Duration) ThroughputResult {
	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
	if err != nil {
		return ThroughputResult{Err: err, URL: rawURL, Expected: expected}
	}
	cd, _ := dialer.(proxy.ContextDialer)
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if cd != nil {
			return cd.DialContext(ctx, network, addr)
		}
		return dialer.Dial(network, addr)
	}
	return measureThroughputDial(dial, rawURL, expected, timeout)
}

func measureThroughputDial(dial func(ctx context.Context, network, addr string) (net.Conn, error),
	rawURL string, expected int64, timeout time.Duration) ThroughputResult {
	res := ThroughputResult{URL: rawURL, Expected: expected}
	if expected <= 0 {
		expected = throughputProbeBytesDefault
		res.Expected = expected
	}
	transport := &http.Transport{
		DialContext:           dial,
		TLSHandshakeTimeout:   healthTLSHandshakeTimeout,
		ResponseHeaderTimeout: healthResponseHeaderTimeout,
		MaxIdleConns:          1,
		IdleConnTimeout:       5 * time.Second,
		DisableKeepAlives:     true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: timeout}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		res.Err = err
		return res
	}
	resp, err := client.Do(req)
	if err != nil {
		res.Err = err
		return res
	}
	defer func() { _ = resp.Body.Close() }()
	res.Status = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		res.Inconclusive = true
		res.Err = fmt.Errorf("throughput target returned %d", resp.StatusCode)
		return res
	}
	// Read at most Content-Length, but keep Expected as the REQUESTED size:
	// a proxy that rewrites the header to its truncation length (e.g. 12KB)
	// must read as short, not as a valid small body. A fixed custom URL should
	// set THROUGHPUT_CHECK_BYTES to its real size.
	want := expected
	if resp.ContentLength > 0 && resp.ContentLength < want {
		want = resp.ContentLength
	}
	meter := &throughputMeter{start: time.Now()}
	_, readErr := io.CopyN(meter, resp.Body, want)
	end := time.Now()
	res.Elapsed = end.Sub(meter.start)
	res.Bytes = meter.total
	res.Err = readErr
	if res.Elapsed <= 0 {
		res.Elapsed = time.Millisecond
	}
	res.Kbps, res.AvgKbps = meter.kbps(end)
	return res
}

// UploadResult is one POST through an instance's SOCKS port. The probe proves
// that a request BODY traverses the tunnel, which a download-only measurement
// cannot see. A completed 2xx with Bytes == Expected is success; a target that
// rejects the request (4xx/5xx/redirect) is Inconclusive and condemns nothing.
type UploadResult struct {
	Bytes        int64
	Expected     int64
	Status       int
	Err          error
	Inconclusive bool
	URL          string
	Elapsed      time.Duration
}

// measureUpload sends a body through proxyAddr (the instance SOCKS port).
func measureUpload(proxyAddr, rawURL string, size int64, timeout time.Duration) UploadResult {
	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
	if err != nil {
		// A bad local SOCKS address is probe configuration, not node evidence.
		return UploadResult{Err: err, URL: rawURL, Expected: size, Inconclusive: true}
	}
	cd, _ := dialer.(proxy.ContextDialer)
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if cd != nil {
			return cd.DialContext(ctx, network, addr)
		}
		return dialer.Dial(network, addr)
	}
	return measureUploadDial(dial, rawURL, size, timeout)
}

func measureUploadDial(dial func(ctx context.Context, network, addr string) (net.Conn, error),
	rawURL string, size int64, timeout time.Duration) UploadResult {
	res := UploadResult{URL: rawURL, Expected: size}
	if size <= 0 {
		size = uploadProbeBytesDefault
		res.Expected = size
	}
	if size > uploadProbeBytesMax {
		size = uploadProbeBytesMax
		res.Expected = size
	}
	payload := bytes.Repeat([]byte{'v'}, int(size))

	transport := &http.Transport{
		DialContext:           dial,
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
		// An upload endpoint must accept the POST directly. Following a
		// redirect would turn some redirects into a GET, hiding a broken
		// request-body path behind a successful page fetch.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(payload))
	if err != nil {
		// A malformed configured URL is never evidence about this node.
		res.Inconclusive = true
		res.Err = err
		return res
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		res.Inconclusive = true
		res.Err = fmt.Errorf("upload target scheme %q must be http or https", req.URL.Scheme)
		return res
	}
	req.ContentLength = int64(len(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Cache-Control", "no-store")

	start := time.Now()
	resp, err := client.Do(req)
	res.Elapsed = time.Since(start)
	if err != nil {
		res.Err = err
		return res
	}
	defer func() { _ = resp.Body.Close() }()
	res.Status = resp.StatusCode
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeDrainCap))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The target answered (or redirected): the node carried the request
		// far enough to get a verdict about the TARGET, not about capacity.
		res.Inconclusive = true
		res.Err = fmt.Errorf("upload target returned %d", resp.StatusCode)
		return res
	}
	res.Bytes = int64(len(payload))
	return res
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
	// Cancel every still-running leg once a winner is accepted. The race's
	// answer is "the first leg its own grading rule accepts"; a losing leg can
	// never change that answer, so letting it burn a full TLS handshake (and up
	// to quickProbeTimeout of network/CPU) after the verdict is already known
	// was pure waste on every healthy health tick.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		res HealthResult
		leg probeLeg
	}
	// Buffered for every leg: a losing leg must never block on send, or the
	// race would leak one goroutine per probe forever.
	ch := make(chan outcome, len(legs))
	for _, leg := range legs {
		go func() {
			ch <- outcome{res: testSingleURLContext(ctx, proxyAddr, leg.url, quickProbeTimeout), leg: leg}
		}()
	}
	accept := func(o outcome) (HealthResult, bool) {
		if !o.leg.grade(o.res) {
			return o.res, false
		}
		// Normalize: Telegram's rule accepts a 404, but callers switch on
		// Working. Leave the verdict self-consistent with the rule that won.
		o.res.Working = true
		cancel()
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
