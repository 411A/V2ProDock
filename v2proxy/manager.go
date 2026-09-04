package main

import (
	"fmt"
	"math/rand"
	"net"
	"sort"
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

	for i := 0; i < instanceCount; i++ {
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
	for attempt := 0; attempt < fetchPoolAttempts; attempt++ {
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
		if !seen[c.Raw] {
			seen[c.Raw] = true
			out = append(out, c)
		}
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
	insts = make([]*ProxySelector, len(m.instances))
	copy(insts, m.instances)
	subURLs = make([]string, len(m.subURLs))
	copy(subURLs, m.subURLs)
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
			used[c.Raw] = j
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
	}
}

// Start fetches subscriptions and probes proxies. The manager lock is only
// held for short state updates so the API stays responsive during populate.
func (m *ProxyManager) Start() error {
	insts, subURLs := m.snapshot()
	if len(insts) == 0 {
		return fmt.Errorf("no instances configured")
	}

	pool, err := fetchPoolWithRetry(subURLs)
	if err != nil {
		for i := range insts {
			errLog("Instance %d: subscription fetch failed: %v", i, err)
			m.markDown(i, err.Error())
		}
		return nil
	}
	pool = dedupConfigs(pool)
	debugLog("subscription pool: %d unique configs from %d source(s)", len(pool), len(subURLs))

	// Each worker shuffles independently so all instances sample the whole pool
	// uniformly from t=0 instead of grinding one contiguous block each.
	shuffleBase := time.Now().UnixNano()
	rawLists := make([][]ProxyConfig, len(insts))
	for i := range insts {
		configs := make([]ProxyConfig, len(pool))
		copy(configs, pool)
		shuffleConfigs(configs, shuffleBase+int64(i)*1099511628211)
		rawLists[i] = configs
		debugLog("Instance %d: parsed %d unique configs", i, len(configs))
		insts[i].UpdateConfigs(configs)
	}
	shared := newProbeShared()

	bannerLog(fmt.Sprintf("Populating %d instances — testing proxies (up to %d at a time), please wait 30-60s...", len(insts), probeWorkers))
	var usedMu sync.Mutex
	used := make(map[string]int)
	snapUsed := func() map[string]int {
		usedMu.Lock()
		defer usedMu.Unlock()
		cp := make(map[string]int, len(used))
		for k, v := range used {
			cp[k] = v
		}
		return cp
	}

	stopTick := make(chan struct{})
	var tickWg sync.WaitGroup
	tickWg.Add(1)
	go func() {
		defer tickWg.Done()
		t := time.NewTicker(populateTick)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				infoLog("Populating... %d/%d ready", m.AliveCount(), len(insts))
			case <-stopTick:
				return
			}
		}
	}()

	sem := make(chan struct{}, probeWorkers)
	var wg sync.WaitGroup
	for i, inst := range insts {
		if rawLists[i] == nil {
			m.markDown(i, "no configs available")
			continue
		}
		wg.Add(1)
		go func(idx int, sel *ProxySelector) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			debugLog("Instance %d: testing %d configs for first working unique proxy...", idx, len(rawLists[idx]))
			deadline := time.Now().Add(probeTimeout)
			for attempt := 0; attempt < probeMaxAttempts && time.Now().Before(deadline); attempt++ {
				if err := sel.startShared(snapUsed(), shared, deadline); err != nil {
					errLog("Instance %d: no working unique config: %v", idx, err)
					m.markDown(idx, err.Error())
					return
				}
				cfg := sel.ActiveConfig()
				if cfg == nil {
					m.markDown(idx, "no active config")
					return
				}
				usedMu.Lock()
				_, dup := used[cfg.Raw]
				if !dup {
					used[cfg.Raw] = idx
				}
				usedMu.Unlock()
				if dup {
					debugLog("Instance %d: %s taken by another instance, retrying...", idx, shortName(cfg.Name))
					continue
				}
				m.markOK(idx, cfg.Name, sel.LastLatency())
				infoLog("Instance %d ready (%d/%d)", idx, m.AliveCount(), len(insts))
				return
			}
			m.markDown(idx, "could not claim a unique working config")
		}(i, inst)
	}
	wg.Wait()
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
	shared := newProbeShared()
	for i, inst := range insts {
		configs := make([]ProxyConfig, len(pool))
		copy(configs, pool)
		shuffleConfigs(configs, shuffleBase+int64(i)*1099511628211)
		debugLog("Instance %d: refreshed %d unique configs", i, len(configs))
		inst.UpdateConfigs(configs)

		if m.statusOf(i).Status != "ok" {
			used := m.buildExcluding(i)
			if err := inst.startShared(used, shared, time.Now().Add(probeTimeout)); err == nil {
				name := ""
				if cfg := inst.ActiveConfig(); cfg != nil {
					name = cfg.Name
				}
				m.markOK(i, name, inst.LastLatency())
			}
		}
	}
}

func (m *ProxyManager) GetStatuses() []InstanceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]InstanceStatus, len(m.statuses))
	copy(result, m.statuses)

	sort.Slice(result, func(i, j int) bool {
		if result[i].Status == "ok" && result[j].Status != "ok" {
			return true
		}
		if result[i].Status != "ok" && result[j].Status == "ok" {
			return false
		}
		return result[i].LatMs < result[j].LatMs
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

	sort.Slice(alive, func(i, j int) bool {
		return alive[i].LatMs < alive[j].LatMs
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
