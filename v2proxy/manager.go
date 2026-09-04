package main

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultPortBase = 27019
	maxPort         = 27999
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
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
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

func rotateConfigs(in []ProxyConfig, offset int) {
	if len(in) <= 1 || offset == 0 {
		return
	}
	offset %= len(in)
	if offset < 0 {
		offset += len(in)
	}
	tmp := make([]ProxyConfig, len(in))
	copy(tmp, in)
	copy(in, tmp[offset:])
	copy(in[len(in)-offset:], tmp[:offset])
}

func (m *ProxyManager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.instances) == 0 {
		return fmt.Errorf("no instances configured")
	}

	pool, err := fetchPoolWithRetry(m.subURLs)
	if err != nil {
		for i := range m.instances {
			errLog("Instance %d: subscription fetch failed: %v", i, err)
			m.statuses[i].Status = "down"
			m.statuses[i].Error = err.Error()
		}
		return nil
	}
	pool = dedupConfigs(pool)
	debugLog("subscription pool: %d unique configs from %d source(s)", len(pool), len(m.subURLs))

	rawLists := make([][]ProxyConfig, len(m.instances))
	for i := range m.instances {
		configs := make([]ProxyConfig, len(pool))
		copy(configs, pool)
		rotateConfigs(configs, i*3)
		rawLists[i] = configs
		debugLog("Instance %d: parsed %d unique configs", i, len(configs))
		m.instances[i].UpdateConfigs(configs)
	}

	bannerLog(fmt.Sprintf("Populating %d instances — testing proxies sequentially, please wait 30-60s...", len(m.instances)))
	used := make(map[string]int)
	for i, inst := range m.instances {
		if rawLists[i] == nil {
			continue
		}
		debugLog("Instance %d: testing %d configs for first working unique proxy...", i, len(rawLists[i]))
		if err := inst.StartWithBestExcluding(used); err != nil {
			errLog("Instance %d: no working unique config: %v", i, err)
			m.statuses[i].Status = "down"
			m.statuses[i].Error = err.Error()
			continue
		}
		if cfg := inst.ActiveConfig(); cfg != nil {
			m.statuses[i].Name = cfg.Name
			used[cfg.Raw] = i
		}
		m.statuses[i].Status = "ok"
		m.statuses[i].Latency = inst.LastLatency()
		m.statuses[i].LatMs = m.statuses[i].Latency.Milliseconds()
	}

	return nil
}

func (m *ProxyManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range m.instances {
		inst.Stop()
	}
}

func (m *ProxyManager) HealthCheckAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, inst := range m.instances {
		if !inst.ShouldCheck() {
			continue
		}

		if inst.HealthCheck() {
			cfg := inst.ActiveConfig()
			m.statuses[i].Status = "ok"
			m.statuses[i].Error = ""
			if cfg != nil {
				m.statuses[i].Name = cfg.Name
			}
			m.statuses[i].Latency = inst.LastLatency()
			m.statuses[i].LatMs = m.statuses[i].Latency.Milliseconds()
		} else {
			m.statuses[i].Status = "down"
			m.statuses[i].Error = "health check failed"
			warnLog("Instance %d: proxy failed, switching...", i)
			used := make(map[string]int)
			for j, o := range m.instances {
				if j == i {
					continue
				}
				if c := o.ActiveConfig(); c != nil {
					used[c.Raw] = j
				}
			}
			if err := inst.SwitchToNextExcluding(used); err != nil {
				errLog("Instance %d: switch failed: %v", i, err)
				m.statuses[i].Error = err.Error()
			} else {
				cfg := inst.ActiveConfig()
				m.statuses[i].Status = "ok"
				m.statuses[i].Error = ""
				if cfg != nil {
					m.statuses[i].Name = cfg.Name
				}
				m.statuses[i].Latency = inst.LastLatency()
				m.statuses[i].LatMs = m.statuses[i].Latency.Milliseconds()
			}
		}
	}
}

func (m *ProxyManager) RefreshSubscriptions() {
	m.mu.Lock()
	defer m.mu.Unlock()

	pool := FetchMergedSubscriptions(m.subURLs)
	if len(pool) == 0 {
		warnLog("refresh got 0 configs from all sources, keeping old")
		return
	}
	pool = dedupConfigs(pool)
	for i, inst := range m.instances {
		configs := make([]ProxyConfig, len(pool))
		copy(configs, pool)
		rotateConfigs(configs, i*3)
		debugLog("Instance %d: refreshed %d unique configs", i, len(configs))
		inst.UpdateConfigs(configs)

		if m.statuses[i].Status != "ok" {
			used := make(map[string]int)
			for j, o := range m.instances {
				if j == i {
					continue
				}
				if c := o.ActiveConfig(); c != nil {
					used[c.Raw] = j
				}
			}
			if err := inst.StartWithBestExcluding(used); err == nil {
				cfg := inst.ActiveConfig()
				m.statuses[i].Status = "ok"
				m.statuses[i].Error = ""
				if cfg != nil {
					m.statuses[i].Name = cfg.Name
				}
				m.statuses[i].Latency = inst.LastLatency()
				m.statuses[i].LatMs = m.statuses[i].Latency.Milliseconds()
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
