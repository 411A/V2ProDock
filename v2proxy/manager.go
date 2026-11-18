package main

import (
	"cmp"
	"fmt"
	"maps"
	"math/rand"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type InstanceStatus struct {
	Index   int           `json:"index"`
	SOCKS   string        `json:"socks5"`
	HTTP    string        `json:"http"`
	Status  string        `json:"status"`
	Latency time.Duration `json:"-"`
	LatMs   int64         `json:"latency_ms"`
	Name    string        `json:"name"`
	Error   string        `json:"error,omitempty"`
	// Endpoint is the active upstream identity (host:port). It is the key a
	// caller uses to attach a memo: truncation is a property of the remote
	// node, not of this instance's local port, and the node changes on switch.
	Endpoint string `json:"endpoint,omitempty"`
	// Memo is a free-text quality note (e.g. "truncates >12KB; downloads and
	// uploads fail"). A memo'd instance is still alive and still eligible when
	// nothing clean remains, but it is ranked behind every clean instance.
	// Degraded is the machine-readable form of Memo != "".
	Memo     string `json:"memo,omitempty"`
	Degraded bool   `json:"degraded,omitempty"`
	// ThroughputKbps is the measured bulk rate (kilobits/s; 0 = not measured).
	// BulkUsable is true only after a completed fetch at or above the floor.
	// A low measurement demotes the node via Degraded but never marks it down.
	ThroughputKbps int64 `json:"throughput_kbps,omitempty"`
	BulkUsable     bool  `json:"bulk_usable,omitempty"`
	// throughputAt/lows are scheduler state, not API surface.
	throughputAt  time.Time
	lowThroughput int
	// Stability signals: consecutive successful health checks on the current
	// upstream, and when it was adopted. Long-polling clients should prefer
	// high streaks over merely low latency (stable-slow beats flappy-fast).
	OkStreak    int    `json:"ok_streak"`
	ActiveSince string `json:"active_since,omitempty"`
}

type ProxyManager struct {
	mu            sync.Mutex
	instances     []*ProxySelector
	statuses      []InstanceStatus
	subURLs       []string
	xrayDir       string
	testURL       string
	portBase      int
	checkInterval time.Duration
	aggSocks      string
	aggHTTP       string
	aggRelay      string
	// memos are quality notes keyed by upstream endpoint (lowercased
	// host:port). They never mark an instance down: a memo'd node is the
	// deliberate last resort, not a failure. Seeded by PROXY_MEMOS and updated
	// by POST /memo; guarded by mu.
	memos map[string]string
	// Throughput probes are background work: a single worker (so a 10-node
	// pool never bursts 10 x 512KiB at once) and a per-instance in-flight set.
	throughputSem  chan struct{}
	throughputBusy map[int]bool
	// cache is the routing ladder derived from statuses. It is written only by
	// status writes (health tick / refresh, under mu) and read lock-free by
	// every aggregate connection. A pointer swap means reader and writer never
	// share the same snapshot, and a stale snapshot is impossible after a
	// markOK/markDown because each one publishes a rebuilt cache.
	cache atomic.Pointer[statusCache]
	// relayAlive gates the relay aggregate: probed once per health tick
	// through the relay path itself (the worker is shared, per-node probes
	// would burn N x budget to learn one fact). False = fail fast.
	relayAlive atomic.Bool
	// urlChecks memoizes /check results per exact probe URL and collapses
	// concurrent identical requests onto one round of probing. Lazily built
	// because NewProxyManager is also built directly by tests.
	urlChecks *urlCheckCache
	// srcBrk rests dead subscription sources across refreshes. It NEEDS a
	// mutex: the comment claiming "subscription loop only" was false, because
	// POST /refresh runs RefreshSubscriptions in its own goroutine (api.go), so
	// the ticker and an API-triggered refresh routinely overlapped and wrote
	// these maps concurrently - a `fatal error: concurrent map writes` that
	// takes the daemon down. A VALUE field with an internal mutex (not a
	// pointer) so the zero value is usable: tests build &ProxyManager{...}
	// literals, and a nil pointer here was a segfault.
	srcBrk srcBreaker
}

// statusCache is the immutable routing snapshot derived from statuses. Slices
// are never mutated after build; replacing the pointer leaves old readers with
// a valid array, so callers may keep the slice header after the load.
type statusCache struct {
	socks []string
	http  []string
	relay []string
}

// srcBreaker circuit-breaks dead subscription sources across refreshes: a
// host that is down burns fetchAttempts x fetchTimeout on EVERY refresh
// while contributing nothing (production: one dead LAN host cost ~20s per
// 120s refresh plus a WARN each cycle). After srcFailThreshold consecutive
// failures the source rests for srcSkipCycles refreshes; the next allowed
// fetch then probes recovery (success resets, failure re-arms).
type srcBreaker struct {
	mu    sync.Mutex
	fails map[string]int
	skip  map[string]int
}

// allow reports whether url may be fetched this refresh, counting down rests.
func (b *srcBreaker) allow(url string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	left, resting := b.skip[url]
	if !resting || left <= 0 {
		return true
	}
	b.skip[url] = left - 1
	return false
}

// restLeft reports remaining rest cycles (0 = not resting).
func (b *srcBreaker) restLeft(url string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.skip[url]
}

// note records one refresh outcome for url.
func (b *srcBreaker) note(url string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		delete(b.fails, url)
		delete(b.skip, url)
		return
	}
	if b.fails == nil {
		b.fails = make(map[string]int)
	}
	b.fails[url]++
	if b.fails[url] >= srcFailThreshold {
		if b.skip == nil {
			b.skip = make(map[string]int)
		}
		if _, resting := b.skip[url]; !resting {
			b.skip[url] = srcSkipCycles
			warnLog("breaker: subscription %s failed %d in a row, resting it for %d refreshes", url, b.fails[url], srcSkipCycles)
		}
	}
}

func NewProxyManager(xrayDir, testURL string, portBase, instanceCount int, subURLs []string, checkInterval time.Duration) *ProxyManager {
	m := &ProxyManager{
		subURLs:       subURLs,
		xrayDir:       xrayDir,
		testURL:       testURL,
		portBase:      portBase,
		checkInterval: checkInterval,
		statuses:      make([]InstanceStatus, instanceCount),
		urlChecks:     newURLCheckCache(),
		memos:         parseProxyMemos(os.Getenv("PROXY_MEMOS")),
		throughputSem: make(chan struct{}, throughputProbeWorkers),
	}

	// Port layout: all SOCKS5 ports first, then all HTTP ports, then (relay
	// egress on) all relay SOCKS ports. Instance i -> SOCKS=base+i,
	// HTTP=base+instanceCount+i, RELAY=base+2*instanceCount+i. Relay-off
	// keeps the old 2N block exactly, so existing deployments see no port
	// shift from this feature existing.
	portsPerInst := 2
	_, relayOn := relayConfig()
	if relayOn {
		portsPerInst = 3
	}
	// Instance i -> SOCKS=base+i, HTTP=base+instanceCount+i.
	block, err := findAvailableBlock(portBase, instanceCount*portsPerInst)
	if err != nil {
		errLog("no available port block: %v", err)
		return m
	}

	for i := range instanceCount {
		socksPort := block + i
		httpPort := block + instanceCount + i

		selector := NewProxySelector(xrayDir, testURL, socksPort, httpPort, checkInterval)
		if relayOn {
			selector.SetRelayPort(block + 2*instanceCount + i)
		}
		m.instances = append(m.instances, selector)

		m.statuses[i] = InstanceStatus{
			Index:  i,
			SOCKS:  fmt.Sprintf("0.0.0.0:%d", socksPort),
			HTTP:   fmt.Sprintf("0.0.0.0:%d", httpPort),
			Status: "starting",
		}

		debugLog("Instance %d: SOCKS5=:%d HTTP=:%d RELAY=:%d", i, socksPort, httpPort, selector.RelayPort())
	}

	return m
}

func findAvailableBlock(start, count int) (int, error) {
	for base := start; base+count-1 <= maxPort; base++ {
		free := true
		for port := base; port < base+count; port++ {
			ln, e := net.Listen("tcp", fmt.Sprintf(":%d", port))
			if e != nil {
				free = false
				break
			}
			// Probe-only listener: the close result cannot change the answer,
			// but a failed close would leak the descriptor for every candidate
			// port scanned, so it must not be silently ignored.
			_ = ln.Close()
		}
		if free {
			return base, nil
		}
	}
	return 0, fmt.Errorf("no available block of %d ports starting from %d", count, start)
}

func fetchPoolWithRetry(urls []string) ([]ProxyConfig, error) {
	clean := make([]string, 0, len(urls))
	for _, u := range urls {
		if strings.TrimSpace(u) != "" {
			clean = append(clean, strings.TrimSpace(u))
		}
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("no subscription URLs configured")
	}
	// fmt.Errorf already returns error: the explicit conversion was redundant.
	lastErr := fmt.Errorf("all %d subscription URLs failed", len(clean))
	for attempt := range fetchPoolAttempts {
		if attempt > 0 {
			time.Sleep(fetchPoolRetrySleep)
		}
		if merged := FetchMergedSubscriptions(clean); len(merged) > 0 {
			return merged, nil
		}
		lastErr = fmt.Errorf("attempt %d/2: all %d subscription URLs failed", attempt+1, len(clean))
		debugLog("fetch pool retry %d/2: %v", attempt+1, lastErr)
	}
	return nil, lastErr
}

func dedupConfigs(in []ProxyConfig) []ProxyConfig {
	seen := make(map[string]bool, len(in))
	out := make([]ProxyConfig, 0, len(in))
	for _, c := range in {
		key := c.Key()
		if !seen[key] {
			seen[key] = true
			out = append(out, c)
		}
	}
	if dropped := len(in) - len(out); dropped > 0 {
		debugLog("dedup: dropped %d duplicate line(s)/same-server entries", dropped)
	}
	return out
}

// shuffleConfigs randomly permutes configs with the given seed. Each probing
// worker gets its own order so all workers sample the whole pool uniformly
// instead of grinding the same contiguous block in lockstep (working proxies
// often cluster in one region of the pool). Uniqueness across instances is
// still guaranteed by the shared claim set, not by ordering.
func shuffleConfigs(in []ProxyConfig, seed int64) {
	r := rand.New(rand.NewSource(seed))
	r.Shuffle(len(in), func(a, b int) { in[a], in[b] = in[b], in[a] })
}

func (m *ProxyManager) snapshot() (insts []*ProxySelector, subURLs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	insts = slices.Clone(m.instances)
	subURLs = slices.Clone(m.subURLs)
	return insts, subURLs
}

func (m *ProxyManager) buildExcluding(i int) map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	used := make(map[string]int)
	for j, o := range m.instances {
		if j == i {
			continue
		}
		if c := o.ActiveConfig(); c != nil {
			used[c.Key()] = j
		}
	}
	return used
}

func normalizeEndpoint(endpoint string) string {
	return strings.ToLower(strings.TrimSpace(endpoint))
}

// memoForLocked returns the quality note attached to an endpoint. Caller holds
// m.mu (or owns the manager exclusively). Nil maps are valid: tests construct
// &ProxyManager{} directly.
func (m *ProxyManager) memoForLocked(endpoint string) string {
	if m.memos == nil {
		return ""
	}
	return m.memos[normalizeEndpoint(endpoint)]
}

// applyQualityLocked copies the active upstream identity and its memo into one
// status row. The memo travels with the ENDPOINT, not the local port: when an
// instance switches configs, the old note must not follow the port to a
// different node. Degraded is memo OR a confirmed low throughput measurement;
// the latter uses the same last-resort lane without inventing a memo.
func (m *ProxyManager) applyQualityLocked(i int) {
	if i < 0 || i >= len(m.statuses) || i >= len(m.instances) {
		return
	}
	cfg := m.instances[i].ActiveConfig()
	port := portOfAddr(m.statuses[i].HTTP)
	if cfg == nil {
		m.statuses[i].Endpoint = ""
		m.statuses[i].Memo = ""
		m.statuses[i].Degraded = false
		m.statuses[i].ThroughputKbps = 0
		m.statuses[i].BulkUsable = false
		m.statuses[i].lowThroughput = 0
		m.statuses[i].throughputAt = time.Time{}
		if port > 0 {
			setHTTPPortEndpoint(port, "")
		}
		return
	}
	if prev := m.statuses[i].Endpoint; prev != cfg.Endpoint {
		// A different node: never inherit the previous node's throughput class
		// or its measurement clock. The next health tick schedules a fresh one.
		m.statuses[i].ThroughputKbps = 0
		m.statuses[i].BulkUsable = false
		m.statuses[i].lowThroughput = 0
		m.statuses[i].throughputAt = time.Time{}
	}
	m.statuses[i].Endpoint = cfg.Endpoint
	memo := m.memoForLocked(cfg.Endpoint)
	m.statuses[i].Memo = memo
	m.statuses[i].Degraded = memo != "" || throughputLow(&m.statuses[i])
	if port > 0 {
		setHTTPPortEndpoint(port, cfg.Endpoint)
	}
}

// throughputLow is true only after TWO consecutive measured-low bodies. A
// single low sample is recorded (ThroughputKbps) but does not demote; error
// samples with zero bytes are inconclusive and never reach here.
func throughputLow(s *InstanceStatus) bool {
	return s.ThroughputKbps > 0 && s.lowThroughput >= 2
}

func portOfAddr(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return 0
	}
	return n
}

// SetMemo attaches (or clears, when memo is empty) a quality note to an
// upstream endpoint and republishes the routing ladder. A memo'd endpoint is
// still alive and still servable: it ranks after every clean instance, so it
// is the deliberate last resort instead of being discarded.
func (m *ProxyManager) SetMemo(endpoint, memo string) {
	endpoint = normalizeEndpoint(endpoint)
	if endpoint == "" {
		return
	}
	memo = strings.TrimSpace(memo)
	if len(memo) > 500 {
		memo = memo[:500]
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if memo == "" {
		delete(m.memos, endpoint)
	} else {
		if m.memos == nil {
			m.memos = make(map[string]string)
		}
		m.memos[endpoint] = memo
	}
	for i := range m.statuses {
		if normalizeEndpoint(m.statuses[i].Endpoint) == endpoint {
			m.statuses[i].Memo = memo
			m.statuses[i].Degraded = memo != "" || throughputLow(&m.statuses[i])
		}
	}
	m.buildCacheLocked()
}

// DegradedCount reports alive instances in the last-resort lane (a quality
// memo or two confirmed low throughput measurements).
func (m *ProxyManager) DegradedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.statuses {
		if s.Status == "ok" && s.Degraded {
			n++
		}
	}
	return n
}

// BulkStats reports alive nodes with a completed throughput measurement and
// those at/above the floor. measured=0 means the gate has not run yet (or is
// disabled), so callers must not treat 0/0 as a broken pool.
func (m *ProxyManager) BulkStats() (usable, measured int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.statuses {
		if s.Status != "ok" {
			continue
		}
		if s.ThroughputKbps > 0 {
			measured++
		}
		if s.BulkUsable {
			usable++
		}
	}
	return usable, measured
}

// applyThroughput folds one measurement into the status. A completed body at
// or above the floor promotes immediately; two consecutive low/partial bodies
// demote (hysteresis prevents a single bad sample from flapping the pool).
// Errors with zero bytes are inconclusive and never demote.
func (m *ProxyManager) applyThroughput(i int, endpoint string, res ThroughputResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.statuses) || m.statuses[i].Endpoint != endpoint {
		return // instance switched while the probe was in flight
	}
	s := &m.statuses[i]
	s.throughputAt = time.Now()
	if res.Bytes > 0 && res.Kbps > 0 {
		s.ThroughputKbps = res.Kbps
	}
	complete := res.Err == nil && res.Status >= 200 && res.Status < 300 &&
		res.Expected > 0 && res.Bytes >= res.Expected &&
		res.Kbps >= throughputFloorKbps()
	switch {
	case complete:
		s.BulkUsable = true
		s.lowThroughput = 0
	case res.Bytes > 0:
		s.lowThroughput++
		if s.lowThroughput >= 2 {
			s.BulkUsable = false
		}
	}
	s.Degraded = s.Memo != "" || throughputLow(s)
	m.buildCacheLocked()
}

// maybeMeasureThroughput schedules a background bulk fetch when the instance
// is due. It never blocks the health loop and never competes with a client that
// carried bytes in the last 5s. Two workers bound total probe bandwidth.
func (m *ProxyManager) maybeMeasureThroughput(i int, inst *ProxySelector) {
	if !throughputCheckEnabled() {
		return
	}
	m.mu.Lock()
	if i < 0 || i >= len(m.statuses) {
		m.mu.Unlock()
		return
	}
	st := m.statuses[i]
	if st.Status != "ok" || st.Endpoint == "" {
		m.mu.Unlock()
		return
	}
	due := throughputInterval()
	if st.lowThroughput == 1 {
		// First low sample: confirm on the next health tick, not after the
		// full cadence. Two strikes still protect against one transient.
		due = throughputConfirmInterval
	}
	if !st.throughputAt.IsZero() && time.Since(st.throughputAt) < due {
		m.mu.Unlock()
		return
	}
	if m.throughputBusy == nil {
		m.throughputBusy = make(map[int]bool)
	}
	if m.throughputBusy[i] {
		m.mu.Unlock()
		return
	}
	if m.throughputSem == nil {
		m.throughputSem = make(chan struct{}, throughputProbeWorkers)
	}
	sem := m.throughputSem
	m.mu.Unlock()

	if egressActive([]int{inst.HTTPPort(), inst.SOCKSPort()}, 5*time.Second) {
		return // a live tunnel is already proving capacity; don't compete
	}
	select {
	case sem <- struct{}{}:
	default:
		return // another instance's measurement is in flight
	}

	m.mu.Lock()
	if m.throughputBusy[i] {
		m.mu.Unlock()
		<-sem
		return
	}
	m.throughputBusy[i] = true
	m.mu.Unlock()

	endpoint := st.Endpoint
	socksAddr := inst.SOCKSAddr()
	url := throughputProbeURL()
	want := throughputProbeSize()
	go func() {
		defer func() {
			<-sem
			m.mu.Lock()
			delete(m.throughputBusy, i)
			m.mu.Unlock()
		}()
		res := measureThroughput(socksAddr, url, want, throughputProbeTimeout)
		m.applyThroughput(i, endpoint, res)
	}()
}

// autoMemoShortTransfers turns repeated short plain-HTTP bodies into a memo.
// The bridge counts by endpoint (TLS bodies are opaque); three truncations is
// enough to stop trusting the node for bulk without waiting for an operator.
func (m *ProxyManager) autoMemoShortTransfers(i int) {
	st := m.statusOf(i)
	if st.Status != "ok" || st.Endpoint == "" || st.Memo != "" {
		return
	}
	n := shortTransferCount(st.Endpoint)
	if n < shortTransferMemoAfter {
		return
	}
	memo := fmt.Sprintf("auto: %d short plain-HTTP transfers; truncating or unsuitable for large downloads", n)
	m.SetMemo(st.Endpoint, memo)
	clearShortTransfers(st.Endpoint)
	warnLog("auto-memo %s: %s", st.Endpoint, memo)
}

func (m *ProxyManager) markDown(i int, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= 0 && i < len(m.statuses) {
		m.applyQualityLocked(i)
		m.statuses[i].Status = "down"
		m.statuses[i].Error = errMsg
		m.buildCacheLocked()
	}
}

// pickBestBackend returns the loopback addr of the best alive instance (SOCKS
// or HTTP-bridge side). Best = fastest among stability-qualified (streak >=
// AGG_MIN_STREAK): a slow node that holds 50s polls beats a fast one that
// RSTs them. Nothing qualifying falls back to fastest (never refuse service).
// False when nothing is servable.
func (m *ProxyManager) pickBestBackend(httpSide bool) (string, bool) {
	cands := m.candidateLadder(httpSide, false)
	if len(cands) == 0 {
		return "", false
	}
	return cands[0], true
}

// buildCacheLocked publishes a fresh routing snapshot. Caller holds m.mu (or
// owns the manager exclusively during construction/tests). The snapshot is
// immutable once stored, so readers need no lock at all.
func (m *ProxyManager) buildCacheLocked() *statusCache {
	alive := make([]InstanceStatus, 0, len(m.statuses))
	for _, s := range m.statuses {
		if s.Status == "ok" {
			alive = append(alive, s)
		}
	}
	slices.SortFunc(alive, func(a, b InstanceStatus) int {
		return cmp.Compare(a.LatMs, b.LatMs)
	})
	order := orderAlive(alive, aggMinStreak())
	c := &statusCache{
		socks: portsForOrder(m.instances, order, false),
		http:  portsForOrder(m.instances, order, true),
		relay: relayPortsForOrder(m.instances, order),
	}
	m.cache.Store(c)
	return c
}

// candidateLadder returns the cached backend list for one protocol side. The
// load is lock-free: the aggregate path is the hottest code in the daemon
// (every accepted client connection) and must not serialize on the manager
// mutex shared with health checks and refreshes. The fallback build exists for
// tests and direct &ProxyManager{} literals that never went through a status
// write; production publishes a cache on every markOK/markDown.
func (m *ProxyManager) candidateLadder(httpSide, relayPath bool) []string {
	c := m.cache.Load()
	if c == nil {
		m.mu.Lock()
		c = m.cache.Load()
		if c == nil {
			c = m.buildCacheLocked()
		}
		m.mu.Unlock()
	}
	switch {
	case relayPath:
		return c.relay
	case httpSide:
		return c.http
	default:
		return c.socks
	}
}

// usedPorts snapshots every instance SOCKS+HTTP port for collision-aware
// planning (aggregate endpoint placement).
func (m *ProxyManager) usedPorts() map[int]bool {
	insts, _ := m.snapshot()
	out := make(map[int]bool, len(insts)*3)
	for _, inst := range insts {
		out[inst.SOCKSPort()] = true
		out[inst.HTTPPort()] = true
		if p := inst.RelayPort(); p > 0 {
			out[p] = true
		}
	}
	return out
}

// setAggregate records the stable endpoint addrs exposed via /health.
// Empty strings mean that protocol is disabled.
func (m *ProxyManager) setAggregate(socks, http, relay string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.aggSocks = socks
	m.aggHTTP = http
	m.aggRelay = relay
}

func (m *ProxyManager) aggregateAddrs() (string, string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.aggSocks, m.aggHTTP, m.aggRelay
}

func (m *ProxyManager) relayAddr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.aggRelay
}

func (m *ProxyManager) markOK(i int, name string, lat time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= 0 && i < len(m.statuses) {
		m.statuses[i].Status = "ok"
		m.statuses[i].Error = ""
		m.applyQualityLocked(i)
		if name != "" {
			m.statuses[i].Name = name
		}
		m.statuses[i].Latency = lat
		m.statuses[i].LatMs = lat.Milliseconds()
		if i < len(m.instances) {
			streak, since := m.instances[i].Stability()
			m.statuses[i].OkStreak = streak
			if !since.IsZero() {
				m.statuses[i].ActiveSince = since.UTC().Format(time.RFC3339)
			}
		}
		m.buildCacheLocked()
	}
}

// serveReady reports whether populate may return early: all instances, or at
// least serveMinReady of them (thin pools would otherwise grind for minutes
// before anything serves). Stragglers keep healing via the health + refresh
// loops, which both drive bounded switches for non-ok instances.
func serveReady(alive, total int) bool {
	if total <= 0 || alive <= 0 {
		return false
	}
	if alive >= total {
		return true
	}
	return alive >= min(serveMinReady, total)
}

// Start fetches subscriptions and probes proxies, retrying in rounds until
// serveReady holds (all instances, or enough to serve on thin pools).
// Stragglers are marked down with a healing note and keep recovering in the
// background. The manager lock is only held for short state updates so the
// API stays responsive during populate.
func (m *ProxyManager) Start() error {
	insts, subURLs := m.snapshot()
	if len(insts) == 0 {
		return fmt.Errorf("no instances configured")
	}

	// Populate until EVERY instance holds a unique working proxy. Rounds refetch
	// the pool, so a short pool heals itself when sources update. Statuses only
	// ever go starting -> ok; nothing is marked down here.
	bannerLog(fmt.Sprintf("Populating %d instances — testing proxies (up to %d at a time), please wait 30-60s...", len(insts), probeWorkers))
	var usedMu sync.Mutex
	used := make(map[string]int)
	snapUsed := func() map[string]int {
		usedMu.Lock()
		defer usedMu.Unlock()
		return maps.Clone(used)
	}

	// Progress logs only when the ready count CHANGES: a thin pool grinding
	// 0/10 for 7 minutes used to log the identical line every 20s.
	lastPopLog := -1
	var popMu sync.Mutex
	notePopProgress := func() {
		ready := m.AliveCount()
		popMu.Lock()
		defer popMu.Unlock()
		if ready == lastPopLog {
			return
		}
		lastPopLog = ready
		infoLog("Populating... %d/%d ready", ready, len(insts))
	}

	stopTick := make(chan struct{})
	var tickWg sync.WaitGroup
	tickWg.Go(func() {
		t := time.NewTicker(populateTick)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				notePopProgress()
			case <-stopTick:
				return
			}
		}
	})

	round := 0
	for {
		round++
		if round > maxPopulateRounds {
			// Bounded startup: serve what works, mark the rest down, return.
			// The health loop keeps healing stragglers with bounded switches,
			// so a thin pool can no longer wedge startup forever.
			for i := range insts {
				if m.statusOf(i).Status != "ok" {
					m.markDown(i, "populate budget exhausted, retrying in background")
				}
			}
			warnLog("populate gave up after %d rounds: %d/%d ready, rest heal in background", maxPopulateRounds, m.AliveCount(), len(insts))
			break
		}
		if round > 1 {
			notePopProgress()
			time.Sleep(populateRetryDelay)
		}
		pool, err := fetchPoolWithRetry(subURLs)
		if err != nil {
			warnLog("populate round %d: fetch failed: %v — retrying...", round, err)
			time.Sleep(populateRetryDelay)
			continue
		}
		pool = dedupConfigs(pool)
		debugLog("subscription pool: %d unique configs from %d source(s)", len(pool), len(subURLs))

		// Each missing worker shuffles independently so all sample the whole
		// pool uniformly instead of grinding one contiguous block each.
		// Already-ok instances keep serving untouched.
		shuffleBase := time.Now().UnixNano()
		rawLists := make([][]ProxyConfig, len(insts))
		for i := range insts {
			if m.statusOf(i).Status == "ok" {
				continue
			}
			configs := make([]ProxyConfig, len(pool))
			copy(configs, pool)
			shuffleConfigs(configs, shuffleBase+int64(i)*1099511628211)
			rawLists[i] = configs
			debugLog("Instance %d: parsed %d unique configs", i, len(configs))
			insts[i].UpdateConfigs(configs)
		}
		shared := newProbeShared()

		sem := make(chan struct{}, probeWorkers)
		var wg sync.WaitGroup
		for i, inst := range insts {
			if m.statusOf(i).Status == "ok" || rawLists[i] == nil {
				continue
			}
			wg.Go(func() {
				sem <- struct{}{}
				defer func() { <-sem }()
				debugLog("Instance %d: testing %d configs for first working unique proxy...", i, len(rawLists[i]))
				deadline := time.Now().Add(probeTimeout)
				for attempt := 0; attempt < probeMaxAttempts && time.Now().Before(deadline); attempt++ {
					if err := inst.startShared(snapUsed(), shared, deadline); err != nil {
						debugLog("Instance %d: %v", i, err)
						return
					}
					cfg := inst.ActiveConfig()
					if cfg == nil {
						debugLog("Instance %d: no active config yet", i)
						return
					}
					usedMu.Lock()
					_, dup := used[cfg.Key()]
					if !dup {
						used[cfg.Key()] = i
					}
					usedMu.Unlock()
					if dup {
						debugLog("Instance %d: %s taken by another instance, retrying...", i, shortName(cfg.Name))
						continue
					}
					m.markOK(i, cfg.Name, inst.LastLatency())
					m.afterMarkOK(i, inst)
					return
				}
				debugLog("Instance %d: round over without a claim, will retry", i)
			})
		}
		wg.Wait()
		if serveReady(m.AliveCount(), len(insts)) {
			if m.AliveCount() < len(insts) {
				// Thin pool: serve NOW, heal the rest in background instead
				// of grinding up to maxPopulateRounds before anything works.
				for i := range insts {
					if m.statusOf(i).Status != "ok" {
						m.markDown(i, "background healing after early serve")
					}
				}
				infoLog("serving early: %d/%d ready, rest heal in background", m.AliveCount(), len(insts))
			}
			break
		}
	}
	close(stopTick)
	tickWg.Wait()

	return nil
}

func (m *ProxyManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range m.instances {
		inst.Stop()
	}
}

func (m *ProxyManager) statusOf(i int) InstanceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= 0 && i < len(m.statuses) {
		return m.statuses[i]
	}
	return InstanceStatus{}
}

// afterMarkOK is the non-blocking quality pass run after an instance is
// promoted: consume short-transfer evidence, then schedule a bulk measurement
// if one is due. Neither step may hold the health loop.
func (m *ProxyManager) afterMarkOK(i int, inst *ProxySelector) {
	m.autoMemoShortTransfers(i)
	m.maybeMeasureThroughput(i, inst)
}

func (m *ProxyManager) HealthCheckAll() {
	insts, _ := m.snapshot()

	for i, inst := range insts {
		if !inst.ShouldCheck() {
			continue
		}

		if inst.HealthCheck() {
			name := ""
			if cfg := inst.ActiveConfig(); cfg != nil {
				name = cfg.Name
			}
			m.markOK(i, name, inst.LastLatency())
			m.afterMarkOK(i, inst)
		} else {
			m.markDown(i, "health check failed")
			warnLog("Instance %d: proxy failed, switching...", i)
			used := m.buildExcluding(i)
			if err := inst.SwitchToNextExcluding(used); err != nil {
				errLog("Instance %d: switch failed: %v", i, err)
				m.markDown(i, err.Error())
			} else {
				name := ""
				if cfg := inst.ActiveConfig(); cfg != nil {
					name = cfg.Name
				}
				m.markOK(i, name, inst.LastLatency())
				m.afterMarkOK(i, inst)
			}
		}
	}
	m.probeRelay()
}

// probeRelay grades the relay path ONCE per health tick through the relay
// aggregate itself. The worker is shared by every instance, so per-node
// probes would burn N x budget to learn one fact; one end-to-end probe
// through the fan-out answers it. While false the relay aggregate fails fast
// and direct paths are untouched. Feature off (no addr) = never probed,
// never alive, never served: zero behavior change.
func (m *ProxyManager) probeRelay() {
	if _, ok := relayConfig(); !ok {
		return
	}
	addr := m.relayAddr()
	if addr == "" {
		return
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	if res := TestProxyQuick(net.JoinHostPort("127.0.0.1", port), m.testURL); res.Working {
		if !m.relayAlive.Load() {
			infoLog("relay egress alive via %s (%s)", addr, res.Latency)
		}
		m.relayAlive.Store(true)
		return
	}
	if m.relayAlive.Load() {
		warnLog("relay egress down via %s, relay aggregate failing fast (direct unaffected)", addr)
	}
	m.relayAlive.Store(false)
}

func (m *ProxyManager) RefreshSubscriptions() {
	insts, subURLs := m.snapshot()

	// Dead sources rest instead of burning full retry budgets every cycle.
	allowed := make([]string, 0, len(subURLs))
	for _, u := range subURLs {
		if strings.TrimSpace(u) == "" {
			continue
		}
		if !m.srcBrk.allow(u) {
			debugLog("breaker: skipping %s (%d rest refreshes left)", u, m.srcBrk.restLeft(u))
			continue
		}
		allowed = append(allowed, u)
	}
	if len(allowed) == 0 {
		debugLog("breaker: all sources resting, keeping old configs")
		return
	}
	pool, failed := fetchMergedReport(allowed)
	for _, u := range allowed {
		m.srcBrk.note(u, !failed[u])
	}
	if len(pool) == 0 {
		warnLog("refresh got 0 configs from all sources, keeping old")
		return
	}
	pool = dedupConfigs(pool)
	shuffleBase := time.Now().UnixNano()
	var wg sync.WaitGroup
	sem := make(chan struct{}, probeWorkers)
	for i, inst := range insts {
		// Captured BEFORE the swap: reconcile needs the pre-refresh active
		// to tell "remote list rotated" apart from "proxy died".
		prevActive := inst.ActiveConfig()
		configs := make([]ProxyConfig, len(pool))
		copy(configs, pool)
		shuffleConfigs(configs, shuffleBase+int64(i)*1099511628211)
		debugLog("Instance %d: refreshed %d unique configs", i, len(configs))
		inst.UpdateConfigs(configs)

		if m.statusOf(i).Status != "ok" {
			// Down instances recover in PARALLEL (each switch is already
			// budget-bounded): sequential recovery multiplied worst-case
			// stalls by the instance count and wedged the refresh ticker.
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				m.recoverInstance(i, inst)
			}()
			continue
		}
		// Status is ok, but the slice was just swapped underneath the
		// instance: re-anchor the active config by KEY (index alone may now
		// point at a different proxy), rotate it away if it vanished, and
		// opportunistically replace it if it got slow — all strictly bounded
		// so a healthy fast instance costs nothing here.
		m.reconcileActive(i, inst, prevActive)
	}
	wg.Wait()
}

// recoverInstance drives one down instance back to ok via the fast parallel
// switch (unique-claim guarded: a parallel peer may have just taken the same
// upstream, in which case one bounded retry with a refreshed exclude runs).
func (m *ProxyManager) recoverInstance(i int, inst *ProxySelector) {
	for range 2 {
		used := m.buildExcluding(i)
		if err := inst.SwitchToNextExcluding(used); err != nil {
			return
		}
		cfg := inst.ActiveConfig()
		if cfg == nil {
			return
		}
		if owner, dup := used[cfg.Key()]; dup {
			debugLog("Instance %d: %s taken by instance %d, retrying...", i, shortName(cfg.Name), owner)
			continue
		}
		m.markOK(i, cfg.Name, inst.LastLatency())
		m.afterMarkOK(i, inst)
		return
	}
}

// reconcileActive re-anchors an ok instance after a refresh swapped its pool
// (UpdateConfigs already re-anchored the index by key; this handles the rest).
// Vanished-but-healthy active -> retained (see below). Vanished-and-failing
// active -> switch now. Slow active (>rotateSlowLatency, measured FRESHLY) ->
// probe a few fresh candidates on a THROWAWAY port (serving is never
// interrupted) and rotate only to one proven >=30% faster. Serving live traffic
// (recent bytes or an open client socket), or holding a stale measurement ->
// untouched: rotation rebinds the serving port and would cut real connections,
// which is a far worse failure than slow. Fast present active -> untouched.
func (m *ProxyManager) reconcileActive(i int, inst *ProxySelector, prev *ProxyConfig) {
	active := inst.ActiveConfig()
	present := active != nil && inst.hasConfig(active.Key())
	if !present {
		// Vanished but never failed: the remote list ROTATED, the proxy did
		// not die — keep serving it (re-inserted at the pool head) instead
		// of forcing a full switch. A truly dead proxy is evicted by the
		// next health tick (bounded staleness <= 1 interval); a forced
		// switch on every rotation is certain churn (production: endless
		// vanish -> rotate -> switch-fail with streaks pinned at 0, which
		// also flapped the API's fastest and churned the VPN pin). Skipped
		// when a peer already owns the key (no duplicate serving).
		if prev != nil && inst.FailCount() == 0 {
			if _, dup := m.buildExcluding(i)[prev.Key()]; !dup {
				inst.RetainActive(*prev)
				debugLog("Instance %d: active %s missing from refresh but healthy, retained", i, shortName(prev.Name))
				return
			}
		}
		warnLog("Instance %d: active config vanished from pool, rotating...", i)
		m.markDown(i, "active config vanished from pool")
		used := m.buildExcluding(i)
		if err := inst.SwitchToNextExcluding(used); err != nil {
			m.markDown(i, err.Error())
			return
		}
		if cfg := inst.ActiveConfig(); cfg != nil {
			m.markOK(i, cfg.Name, inst.LastLatency())
			m.afterMarkOK(i, inst)
		}
		return
	}
	// Real bytes outrank synthetic optimization. A rotation is stopXray() +
	// startXray() on the SAME serving ports (selector.go), which severs every
	// in-flight connection on this instance — a Telegram long-poll dies
	// mid-response and the client raises RemoteProtocolError, while the proxy
	// honestly keeps reporting "ok". HealthCheck already refuses to strike a
	// serving instance (selector.go); this path had no such guard, so a busy
	// instance was severable on every refresh tick. A slow-but-serving tunnel
	// beats a faster one that cuts live connections.
	//
	// The byte ledger alone is not enough, though: egressNotes only ever sees
	// ports THIS process serves, and the per-instance SOCKS inbound belongs to
	// the xray child, so a client pinned straight to 127.0.0.1:<socksPort> moves
	// bytes we never observe. ServingClients reads the sockets themselves — the
	// connection rotation actually destroys, including one parked mid-long-poll.
	// Together they make idleness a PROVEN condition: no recent bytes AND nothing
	// connected (or no evidence at all, for at most servingSocketUnprovenGrace).
	if inst.ServingTraffic() || inst.ServingClients() {
		debugLog("Instance %d: serving client traffic, slow rotation skipped (active %s at %dms)",
			i, shortName(active.Name), inst.LastLatency().Milliseconds())
		return
	}
	// A stale measurement cannot prove "slow": HealthCheck skips probes for a
	// serving instance, so lastLatency stays frozen at its last real reading.
	// Judging it against rotateSlowLatency now would rotate on the strength of
	// a forgotten number. Once traffic stops the probes resume and the figure
	// goes fresh again.
	if !inst.LatencyFresh(rotateLatencyMaxAge) {
		debugLog("Instance %d: latency for %s is stale, rotation deferred to a fresh measurement",
			i, shortName(active.Name))
		return
	}
	if inst.LastLatency() < rotateSlowLatency {
		return
	}
	used := m.buildExcluding(i)
	deadline := time.Now().Add(rotateBudget)
	tried := 0
	for _, c := range inst.snapshotConfigs() {
		if tried >= rotateMaxCandidates || time.Now().After(deadline) {
			break
		}
		if c.Key() == active.Key() {
			continue
		}
		if _, ok := used[c.Key()]; ok {
			continue // owned by another instance — don't steal
		}
		tried++
		res := inst.probeCandidateOnTempPort(c)
		if !res.Working {
			continue
		}
		// Rotate only for a decisive win: candidate >=30% faster.
		if res.Latency*10 >= inst.LastLatency()*7 {
			continue
		}
		// Surgical switch: exclude everything but the proven candidate. The
		// switch skips the current active by index and restores it on failure,
		// so the instance can never strand on a random third proxy.
		excl := make(map[string]int, len(inst.snapshotConfigs()))
		for _, o := range inst.snapshotConfigs() {
			if o.Key() != c.Key() {
				excl[o.Key()] = -1
			}
		}
		if err := inst.SwitchToNextExcluding(excl); err != nil {
			break
		}
		if cfg := inst.ActiveConfig(); cfg != nil && cfg.Key() == c.Key() {
			m.markOK(i, cfg.Name, inst.LastLatency())
			m.afterMarkOK(i, inst)
			infoLog("Instance %d: rotated slow %s -> %s (%dms)", i, shortName(active.Name), shortName(cfg.Name), inst.LastLatency().Milliseconds())
		}
		return
	}
}

func (m *ProxyManager) GetStatuses() []InstanceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := slices.Clone(m.statuses)

	// Human-friendly: always in instance order.
	slices.SortFunc(result, func(a, b InstanceStatus) int {
		return cmp.Compare(a.Index, b.Index)
	})

	return result
}

func (m *ProxyManager) GetAliveStatuses() []InstanceStatus {
	m.mu.Lock()
	alive := make([]InstanceStatus, 0, len(m.statuses))
	for _, s := range m.statuses {
		if s.Status == "ok" {
			alive = append(alive, s)
		}
	}
	m.mu.Unlock()

	slices.SortFunc(alive, func(a, b InstanceStatus) int {
		// Aware clients see the warning on the entry itself, and the list keeps
		// clean nodes first so a naive client that takes proxies[0] does not
		// start with a known-truncating or confirmed-slow node. Within each
		// tier, measured throughput class beats handshake latency.
		if a.Degraded != b.Degraded {
			if a.Degraded {
				return 1
			}
			return -1
		}
		if ta, tb := throughputTier(a.ThroughputKbps), throughputTier(b.ThroughputKbps); ta != tb {
			return cmp.Compare(tb, ta)
		}
		return cmp.Compare(a.LatMs, b.LatMs)
	})

	return alive
}

func (m *ProxyManager) InstanceCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.instances)
}

func (m *ProxyManager) AliveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.statuses {
		if s.Status == "ok" {
			n++
		}
	}
	return n
}

func (m *ProxyManager) StartingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.statuses {
		if s.Status == "starting" {
			n++
		}
	}
	return n
}
