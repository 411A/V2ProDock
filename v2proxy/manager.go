package main

import (
	"fmt"
	"log"
	"net"
	"sort"
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

	for i := 0; i < instanceCount; i++ {
		var socksPort, httpPort int

		if i == 0 {
			// Instance 0 always uses the fixed default port
			socksPort = defaultPortBase
			httpPort = defaultPortBase + 1
		} else {
			// Additional instances get dynamically assigned ports
			var err error
			socksPort, httpPort, err = findAvailablePorts(portBase + i*2)
			if err != nil {
				log.Printf("Instance %d: no available ports starting from %d: %v", i, portBase+i*2, err)
				continue
			}
		}

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

func findAvailablePorts(start int) (socksPort, httpPort int, err error) {
	for port := start; port <= maxPort-1; port++ {
		ln1, e1 := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if e1 != nil {
			continue
		}
		ln1.Close()

		ln2, e2 := net.Listen("tcp", fmt.Sprintf(":%d", port+1))
		if e2 != nil {
			continue
		}
		ln2.Close()

		return port, port + 1, nil
	}
	return 0, 0, fmt.Errorf("no available port pair found starting from %d", start)
}

func fetchWithRetry(subURL string) ([]ProxyConfig, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		cfgs, err := FetchSubscription(subURL)
		if err == nil && len(cfgs) > 0 {
			return cfgs, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("empty subscription (0 proxies)")
		}
		debugLog("Fetch retry %d/4 for %s: %v", attempt+1, subURL, lastErr)
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

	rawLists := make([][]ProxyConfig, len(m.instances))
	for i := range m.instances {
		configs, err := fetchWithRetry(m.subURLs[i%len(m.subURLs)])
		if err != nil {
			log.Printf("Instance %d: subscription fetch failed: %v", i, err)
			m.statuses[i].Status = "down"
			m.statuses[i].Error = err.Error()
			continue
		}
		configs = dedupConfigs(configs)
		rotateConfigs(configs, i*3)
		rawLists[i] = configs
		debugLog("Instance %d: parsed %d unique configs", i, len(configs))
		m.instances[i].UpdateConfigs(configs)
	}

	log.Printf("Populating %d instances — testing proxies sequentially, please wait 30-60s...", len(m.instances))
	used := make(map[string]int)
	for i, inst := range m.instances {
		if rawLists[i] == nil {
			continue
		}
		debugLog("Instance %d: testing %d configs for first working unique proxy...", i, len(rawLists[i]))
		if err := inst.StartWithBestExcluding(used); err != nil {
			log.Printf("Instance %d: no working unique config: %v", i, err)
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
			log.Printf("Instance %d: proxy failed, switching...", i)
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
				log.Printf("Instance %d: switch failed: %v", i, err)
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

	for i, inst := range m.instances {
		configs, err := FetchSubscription(m.subURLs[i%len(m.subURLs)])
		if err != nil {
			log.Printf("Instance %d: refresh failed: %v", i, err)
			continue
		}
		if len(configs) == 0 {
			debugLog("Instance %d: refresh got 0 configs, keeping old", i)
			continue
		}
		configs = dedupConfigs(configs)
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
