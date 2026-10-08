package main

// Dynamic per-URL reachability: "which of my proxies can actually reach THIS
// url?".
//
// HEALTH_CHECK_URL answers one question for the whole pool - "is the tunnel
// alive?" - and its answer is a single verdict per instance. That is the wrong
// question for a caller who cares about one destination: a pool can be entirely
// healthy against gstatic while every node is filtered for a given site, and
// the only way to find out is to ask each upstream about that site.
//
// Identity is a DOMAIN, not a string. http://x.com, https://www.x.com and
// x.com are the same endpoint, so they resolve to one key. The key is the
// registrable domain (eTLD+1) via the public suffix list, not a naive
// last-two-labels split: www.example.co.uk must key to example.co.uk, never to
// co.uk.
//
// The measurement is still per exact URL: /bot123/getUpdates and / are
// different resources behind one key, so results are cached per probe URL and
// the domain key is only the reported identity. Claiming a path is reachable
// because a sibling path answered would be exactly the kind of unearned
// verdict this series has been removing.
//
// SAFETY: the probe dials FROM the upstream server, so a request for a
// private address probes the UPSTREAM's LAN or cloud metadata, not the
// operator's. That is a third-party SSRF, so literal private/loopback/
// link-local targets are refused. Limitation stated honestly: this validates
// the LITERAL only. A public hostname that resolves to 169.254.169.254 at the
// upstream is not detectable from here - the upstream resolves independently
// and we never see that answer.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

// ---- bounds ----
// Every limit here exists because the endpoint takes untrusted input and
// each candidate probe costs a throwaway xray process (~15MB).
const (
	checkMaxDepth     = 32      // hard cap on pool candidates per request
	checkMaxURLLen    = 2048    // absurd URLs are rejected, not parsed
	checkMaxBody      = 8 << 10 // request body cap
	checkCacheTTL     = 60 * time.Second
	checkCacheEntries = 128 // bounded: key eviction is oldest-write, never unbounded
	checkBudget       = 45 * time.Second
)

// CheckedProxy is one upstream's verdict for the requested URL.
type CheckedProxy struct {
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	Instance  int    `json:"instance"`
	LatencyMS int64  `json:"latency_ms"`
	Source    string `json:"source"` // "alive" (already serving) | "pool" (throwaway probe)
}

// URLCheckResult is the /check payload.
type URLCheckResult struct {
	Key        string         `json:"key"`        // registrable domain: the caller's "same URL"
	ProbedURL  string         `json:"probed_url"` // what was actually fetched
	Alive      []CheckedProxy `json:"alive"`      // instances already serving
	Pool       []CheckedProxy `json:"pool"`       // pool candidates probed on demand
	Checked    int            `json:"checked"`    // probes performed
	Failed     int            `json:"failed"`     // probes that did not reach it
	DurationMS int64          `json:"duration_ms"`
	Cached     bool           `json:"cached"`
	Truncated  bool           `json:"truncated"` // depth cap hit before the pool was exhausted
	// Err is set when the check completed but degraded (e.g. the pool stage was
	// cut short). A 200 with Err is still a usable answer about the ALIVE set.
	Err string `json:"error,omitempty"`
}

// checkTarget is a normalized, safety-vetted probe destination.
type checkTarget struct {
	Key      string // eTLD+1 identity
	ProbeURL string // exact URL fetched through the tunnel
	Host     string // normalized host[:port]
}

// normalizeCheckTarget turns loose user input into a vetted probe target.
// Accepted: "example.com", "http://example.com", "https://www.example.com/a?b=c".
// All three of the first three forms key to the same domain.
func normalizeCheckTarget(raw string) (checkTarget, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return checkTarget{}, errors.New("empty url")
	}
	if len(s) > checkMaxURLLen {
		return checkTarget{}, fmt.Errorf("url too long (max %d bytes)", checkMaxURLLen)
	}
	// A bare host gets the scheme the daemon already insists on: plain HTTP is
	// RST-injected by DPI even through a working tunnel (see isPlainHTTP).
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return checkTarget{}, fmt.Errorf("not a valid url: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return checkTarget{}, fmt.Errorf("unsupported scheme %q (only http and https)", u.Scheme)
	}
	// Credentials in a probe target are never legitimate and would end up in
	// logs and cache keys.
	if u.User != nil {
		return checkTarget{}, errors.New("credentials in url are not allowed")
	}
	if u.Host == "" {
		return checkTarget{}, errors.New("url has no host")
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimSuffix(host, ".") // FQDN root form
	if host == "" {
		return checkTarget{}, errors.New("url has no host")
	}
	if err := validateCheckHost(host); err != nil {
		return checkTarget{}, err
	}
	probe := url.URL{Scheme: strings.ToLower(u.Scheme), Host: u.Host, Path: u.Path, RawQuery: u.RawQuery}
	if probe.Path == "" {
		probe.Path = "/"
	}
	return checkTarget{
		Key:      registrableDomain(host),
		ProbeURL: probe.String(),
		Host:     u.Host,
	}, nil
}

// registrableDomain returns the eTLD+1 of host. The public suffix list is the
// only correct way to do this: last-two-labels turns www.example.co.uk into
// "co.uk", which would merge unrelated sites.
func registrableDomain(host string) string {
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil && d != "" {
		return d
	}
	// Single-label names and IP literals are outside the list's model. Use the
	// host itself, minus a www. prefix so the two still unify.
	h := host
	if trimmed, ok := strings.CutPrefix(h, "www."); ok {
		h = trimmed
	}
	return h
}

// validateCheckHost refuses targets that would turn the upstream into a probe
// for its own private network.
func validateCheckHost(host string) error {
	if ip := net.ParseIP(host); ip != nil {
		// IsPrivate covers RFC1918 + fc00::/7; the rest are loopback, link-local
		// (which is how 169.254.169.254 metadata is reached), unspecified and
		// multicast.
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Errorf("refusing non-public target %q: the probe dials from the upstream, so this would scan the upstream's own network", host)
		}
		return nil
	}
	switch {
	case host == "localhost" || strings.HasSuffix(host, ".localhost"):
		return fmt.Errorf("refusing loopback target %q", host)
	case strings.HasSuffix(host, ".local"), strings.HasSuffix(host, ".internal"):
		return fmt.Errorf("refusing private-suffix target %q", host)
	case strings.HasSuffix(host, ".home.arpa"):
		return fmt.Errorf("refusing reserved target %q", host)
	case !strings.Contains(host, "."):
		return fmt.Errorf("refusing single-label target %q (intranet name)", host)
	}
	return nil
}

// ---- bounded, de-duplicating result cache ----

// checkFlight lets concurrent requests for the SAME probe URL share one round
// of probing instead of each launching their own xray children.
type checkFlight struct {
	done chan struct{}
	res  URLCheckResult
	err  error
}

type checkCacheEntry struct {
	at  time.Time
	res URLCheckResult
}

type urlCheckCache struct {
	mu       sync.Mutex
	entries  map[string]checkCacheEntry
	inflight map[string]*checkFlight
	clock    func() time.Time
}

func newURLCheckCache() *urlCheckCache {
	return &urlCheckCache{
		entries:  make(map[string]checkCacheEntry),
		inflight: make(map[string]*checkFlight),
		clock:    time.Now,
	}
}

func (c *urlCheckCache) get(key string) (URLCheckResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || c.clock().Sub(e.at) > checkCacheTTL {
		return URLCheckResult{}, false
	}
	res := e.res
	res.Cached = true
	return res, true
}

// begin claims the probe for key, or joins an in-flight one. leader reports
// whether the caller must actually do the work.
func (c *urlCheckCache) begin(key string) (*checkFlight, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.inflight[key]; ok {
		return f, false
	}
	f := &checkFlight{done: make(chan struct{})}
	c.inflight[key] = f
	return f, true
}

func (c *urlCheckCache) finish(key string, f *checkFlight, res URLCheckResult, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f.res, f.err = res, err
	if err == nil {
		// Bounded map: evict the oldest entry rather than grow without limit.
		for len(c.entries) >= checkCacheEntries {
			var oldestKey string
			var oldest time.Time
			first := true
			for k, e := range c.entries {
				if first || e.at.Before(oldest) {
					oldestKey, oldest, first = k, e.at, false
				}
			}
			if first {
				break // map already empty
			}
			delete(c.entries, oldestKey)
		}
		c.entries[key] = checkCacheEntry{at: c.clock(), res: res}
	}
	delete(c.inflight, key)
	close(f.done)
}

// CheckURL returns which upstreams can reach rawURL. depth is how many pool
// CANDIDATES to probe on top of the instances already serving (0 = alive only,
// which launches no processes at all).
//
// Read-only with respect to pool state: no activeIndex change, no shared
// ledger entry, no serving-port rebind. A URL nothing can reach therefore
// cannot damage the pool - the same discipline that keeps a throttled probe
// target from culling it.
func (m *ProxyManager) CheckURL(ctx context.Context, rawURL string, depth int) (URLCheckResult, error) {
	target, err := normalizeCheckTarget(rawURL)
	if err != nil {
		return URLCheckResult{}, err
	}
	if depth < 0 {
		depth = 0
	}
	if depth > checkMaxDepth {
		depth = checkMaxDepth
	}
	// Cache on the exact probe URL: two paths under one domain key are two
	// different measurements.
	cacheKey := target.ProbeURL
	if m.urlChecks == nil {
		m.urlChecks = newURLCheckCache()
	}
	if res, ok := m.urlChecks.get(cacheKey); ok && res.Checked >= m.checkedFor(res) {
		// A deeper earlier result still satisfies a shallower request.
		return res, nil
	}
	f, leader := m.urlChecks.begin(cacheKey)
	if !leader {
		select {
		case <-ctx.Done():
			return URLCheckResult{}, ctx.Err()
		case <-f.done:
		}
		res := f.res
		res.Cached = true
		return res, f.err
	}
	res, err := m.runURLCheck(ctx, target, depth)
	m.urlChecks.finish(cacheKey, f, res, err)
	return res, err
}

// checkedFor is the probe count a cached result represents, so a deeper cached
// answer can answer a shallower request.
func (m *ProxyManager) checkedFor(res URLCheckResult) int {
	return len(res.Alive) + len(res.Pool)
}

func (m *ProxyManager) runURLCheck(ctx context.Context, target checkTarget, depth int) (URLCheckResult, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, checkBudget)
	defer cancel()

	res := URLCheckResult{Key: target.Key, ProbedURL: target.ProbeURL}

	// 1. Instances already serving: probed on their live SOCKS port, so this
	//    costs zero processes and answers most callers outright.
	insts, _ := m.snapshot()
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, inst := range insts {
		active := inst.ActiveConfig()
		if active == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := testSingleURL(fmt.Sprintf("127.0.0.1:%d", inst.SOCKSPort()), target.ProbeURL, quickProbeTimeout)
			mu.Lock()
			defer mu.Unlock()
			res.Checked++
			if r.Working {
				res.Alive = append(res.Alive, CheckedProxy{
					Name: active.Name, Endpoint: active.Key(), Instance: i,
					LatencyMS: r.Latency.Milliseconds(), Source: "alive",
				})
				return
			}
			res.Failed++
		}()
	}
	wg.Wait()

	// 2. Optionally widen into the pool. Candidates come from the first
	//    instance's snapshot minus whatever it already serves: a SAMPLE, not
	//    an exhaustive search - a 600-config pool cannot be swept per request.
	if depth > 0 && ctx.Err() == nil {
		res.Pool, res.Truncated = m.probePoolForURL(ctx, target, depth)
		for range res.Pool {
			res.Checked++
		}
		res.Failed += depth - len(res.Pool)
	}

	res.DurationMS = time.Since(start).Milliseconds()
	if res.Checked == 0 {
		return res, errors.New("no instance is serving a proxy right now; nothing could be checked")
	}
	return res, nil
}

// probePoolForURL probes up to depth pool candidates against target, in
// parallel, using the same worker budget as a failover switch.
func (m *ProxyManager) probePoolForURL(ctx context.Context, target checkTarget, depth int) (working []CheckedProxy, truncated bool) {
	insts, _ := m.snapshot()
	if len(insts) == 0 {
		return nil, false
	}
	src := insts[0]
	cands := src.snapshotConfigs()
	activeKey := ""
	if a := src.ActiveConfig(); a != nil {
		activeKey = a.Key()
	}
	if len(cands) > depth {
		cands = cands[:depth]
		truncated = true
	}
	workers := min(switchWorkerCount(), len(cands))
	if workers < 1 {
		return nil, truncated
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	queue := make(chan ProxyConfig)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cfg := range queue {
				if ctx.Err() != nil {
					return
				}
				r := src.probeTempPortURL(cfg, target.ProbeURL)
				if !r.Working {
					continue
				}
				mu.Lock()
				working = append(working, CheckedProxy{
					Name: cfg.Name, Endpoint: cfg.Key(), Instance: -1,
					LatencyMS: r.Latency.Milliseconds(), Source: "pool",
				})
				mu.Unlock()
			}
		}()
	}
send:
	for _, cfg := range cands {
		if cfg.Key() == activeKey {
			continue
		}
		select {
		case queue <- cfg:
		case <-ctx.Done():
			break send
		}
	}
	close(queue)
	wg.Wait()
	return working, truncated
}

// ---- HTTP surface ----

type checkRequest struct {
	URL   string `json:"url"`
	Depth int    `json:"depth"`
}

// handleCheck is the /check handler body, shared by POST /check and
// GET /check?url=... so both spellings behave identically.
func (m *ProxyManager) handleCheck(w http.ResponseWriter, r *http.Request) {
	var req checkRequest
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		req.URL = q.Get("url")
		if d := q.Get("depth"); d != "" {
			n, err := strconv.Atoi(d)
			if err != nil {
				httpError(w, http.StatusBadRequest, fmt.Sprintf("depth %q is not a number", d))
				return
			}
			req.Depth = n
		}
	default:
		r.Body = http.MaxBytesReader(w, r.Body, checkMaxBody)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			httpError(w, http.StatusBadRequest, "body must be JSON {\"url\":\"...\",\"depth\":N}")
			return
		}
	}
	res, err := m.CheckURL(r.Context(), req.URL, req.Depth)
	if err != nil {
		// A rejected or unusable target is the caller's problem (400); a
		// degraded-but-answered check is not.
		if res.Key == "" {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		res.Err = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		debugLog("encode /check failed: %v", err)
	}
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": msg}); err != nil {
		debugLog("encode error body failed: %v", err)
	}
}
