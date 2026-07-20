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
	apiPort       int
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

		log.Printf("Instance %d: SOCKS5=:%d HTTP=:%d", i, socksPort, httpPort)
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

func (m *ProxyManager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.instances) == 0 {
		return fmt.Errorf("no instances configured")
	}

	for i, inst := range m.instances {
		configs, err := FetchSubscription(m.subURLs[i%len(m.subURLs)])
		if err != nil {
			log.Printf("Instance %d: subscription fetch failed: %v", i, err)
			m.statuses[i].Status = "down"
			m.statuses[i].Error = err.Error()
			continue
		}
		log.Printf("Instance %d: parsed %d configs", i, len(configs))
		inst.UpdateConfigs(configs)

		if err := inst.StartWithBest(); err != nil {
			log.Printf("Instance %d: no working config: %v", i, err)
			m.statuses[i].Status = "down"
			m.statuses[i].Error = err.Error()
			continue
		}

		cfg := inst.ActiveConfig()
		if cfg != nil {
			m.statuses[i].Name = cfg.Name
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
			if err := inst.SwitchToNext(); err != nil {
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
		log.Printf("Instance %d: refreshed %d configs", i, len(configs))
		inst.UpdateConfigs(configs)
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
