package main

import (
	"cmp"
	"fmt"
	"maps"
	"math/rand"
	"net"
	"slices"
	"strings"
	"sync"
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
}

func NewProxyManager(xrayDir, testURL string, portBase, instanceCount int, subURLs []string, checkInterval time.Duration) *ProxyManager {
	m := &ProxyManager{
		subURLs:       subURLs,
		xrayDir:       xrayDir,
		testURL:       testURL,
		portBase:      portBase,
		checkInterval: checkInterval,
		statuses:      make([]InstanceStatus, instanceCount),
	}

	// Port layout: all SOCKS5 ports first, then all HTTP ports.
	// Instance i -> SOCKS=base+i, HTTP=base+instanceCount+i.
	block, err := findAvailableBlock(portBase, instanceCount*2)
	if err != nil {
		errLog("no available port block: %v", err)
		return m
	}

	for i := range instanceCount {
		socksPort := block + i
		httpPort := block + instanceCount + i

		selector := NewProxySelector(xrayDir, testURL, socksPort, httpPort, checkInterval)
		m.instances = append(m.instances, selector)

		m.statuses[i] = InstanceStatus{
			Index:  i,
			SOCKS:  fmt.Sprintf("0.0.0.0:%d", socksPort),
			HTTP:   fmt.Sprintf("0.0.0.0:%d", httpPort),
			Status: "starting",
		}

		debugLog("Instance %d: SOCKS5=:%d HTTP=:%d", i, socksPort, httpPort)
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
			ln.Close()
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
	var lastErr error = fmt.Errorf("all %d subscription URLs failed", len(clean))
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
		if !seen[c.Key()] {
			seen[c.Key()] = true
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

func (m *ProxyManager) markDown(i int, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= 0 && i < len(m.statuses) {
		m.statuses[i].Status = "down"
		m.statuses[i].Error = errMsg
	}
}

// pickBestBackend returns the loopback addr of the best alive instance (SOCKS
// or HTTP-bridge side). Best = fastest among stability-qualified (streak >=
// AGG_MIN_STREAK): a slow node that holds 50s polls beats a fast one that
// RSTs them. Nothing qualifying falls back to fastest (never refuse service).
// False when nothing is servable.
func (m *ProxyManager) pickBestBackend(httpSide bool) (string, bool) {
	alive := m.GetAliveStatuses()
	if len(alive) == 0 {
		return "", false
	}
	best := alive[0]
	if minStreak := aggMinStreak(); minStreak > 0 {
		for _, s := range alive {
			// Alive is latency-sorted: first qualifier is fastest qualifier.
			if s.OkStreak >= minStreak {
				best = s
				break
			}
		}
	}
	insts, _ := m.snapshot()
	if best.Index < 0 || best.Index >= len(insts) {
		return "", false
	}
	port := insts[best.Index].SOCKSPort()
	if httpSide {
		port = insts[best.Index].HTTPPort()
	}
	return fmt.Sprintf("127.0.0.1:%d", port), true
}

// usedPorts snapshots every instance SOCKS+HTTP port for collision-aware
// planning (aggregate endpoint placement).
func (m *ProxyManager) usedPorts() map[int]bool {
	insts, _ := m.snapshot()
	out := make(map[int]bool, len(insts)*2)
	for _, inst := range insts {
		out[inst.SOCKSPort()] = true
		out[inst.HTTPPort()] = true
	}
	return out
}

// setAggregate records the stable endpoint addrs exposed via /health.
// Empty strings mean that protocol is disabled.
func (m *ProxyManager) setAggregate(socks, http string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.aggSocks = socks
	m.aggHTTP = http
}

func (m *ProxyManager) aggregateAddrs() (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.aggSocks, m.aggHTTP
}

func (m *ProxyManager) markOK(i int, name string, lat time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= 0 && i < len(m.statuses) {
		m.statuses[i].Status = "ok"
		m.statuses[i].Error = ""
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
			}
		}
	}
}

func (m *ProxyManager) RefreshSubscriptions() {
	insts, subURLs := m.snapshot()

	pool := FetchMergedSubscriptions(subURLs)
	if len(pool) == 0 {
		warnLog("refresh got 0 configs from all sources, keeping old")
		return
	}
	pool = dedupConfigs(pool)
	shuffleBase := time.Now().UnixNano()
	var wg sync.WaitGroup
	sem := make(chan struct{}, probeWorkers)
	for i, inst := range insts {
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
		m.reconcileActive(i, inst)
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
		return
	}
}

// reconcileActive re-anchors an ok instance after a refresh swapped its pool
// (UpdateConfigs already re-anchored the index by key; this handles the rest).
// Vanished active -> switch now. Slow active (>rotateSlowLatency) -> probe a
// few fresh candidates on a THROWAWAY port (serving is never interrupted) and
// rotate only to one proven >=30% faster. Fast present active -> untouched.
func (m *ProxyManager) reconcileActive(i int, inst *ProxySelector) {
	active := inst.ActiveConfig()
	present := active != nil &&
		slices.ContainsFunc(inst.snapshotConfigs(), func(c ProxyConfig) bool {
			return c.Key() == active.Key()
		})
	if !present {
		warnLog("Instance %d: active config vanished from pool, rotating...", i)
		m.markDown(i, "active config vanished from pool")
		used := m.buildExcluding(i)
		if err := inst.SwitchToNextExcluding(used); err != nil {
			m.markDown(i, err.Error())
			return
		}
		if cfg := inst.ActiveConfig(); cfg != nil {
			m.markOK(i, cfg.Name, inst.LastLatency())
		}
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
