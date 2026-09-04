package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type ProxySelector struct {
	mu            sync.Mutex
	configs       []ProxyConfig
	activeIndex   int
	failCount     int
	xrayCmd       *exec.Cmd
	xrayDir       string
	testURL       string
	socksPort     int
	httpPort      int
	checkInterval time.Duration
	lastCheck     time.Time
	lastLatency   time.Duration
}

func NewProxySelector(xrayDir, testURL string, socksPort, httpPort int, checkInterval time.Duration) *ProxySelector {
	if err := os.MkdirAll(xrayDir, 0755); err != nil {
		debugLog("mkdir %s failed: %v", xrayDir, err)
	}
	return &ProxySelector{
		xrayDir:       xrayDir,
		testURL:       testURL,
		socksPort:     socksPort,
		httpPort:      httpPort,
		checkInterval: checkInterval,
		activeIndex:   -1,
	}
}

func (s *ProxySelector) SOCKSPort() int { return s.socksPort }
func (s *ProxySelector) HTTPPort() int  { return s.httpPort }
func (s *ProxySelector) LastLatency() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastLatency
}

func (s *ProxySelector) UpdateConfigs(configs []ProxyConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configs = configs
}

func (s *ProxySelector) StartWithBest() error {
	return s.StartWithBestExcluding(nil)
}

func (s *ProxySelector) StartWithBestExcluding(exclude map[string]int) error {
	return s.startShared(exclude, nil, time.Time{})
}

// probeShared lets concurrently probing instances share what they learned:
// configs that already failed elsewhere are skipped instead of re-probed,
// and configs currently being probed (or already owned) are claimed so no
// two workers waste time testing the same candidate.
type probeShared struct {
	mu      sync.Mutex
	bad     map[string]struct{}
	claimed map[string]struct{}
}

func newProbeShared() *probeShared {
	return &probeShared{bad: make(map[string]struct{}), claimed: make(map[string]struct{})}
}

func (p *probeShared) isBad(raw string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.bad[raw]
	return ok
}

func (p *probeShared) markBad(raw string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bad[raw] = struct{}{}
}

// tryClaim atomically reserves a config for probing. Returns false if another
// worker already claimed it, so the same candidate is never tested twice.
func (p *probeShared) tryClaim(raw string) bool {
	if p == nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.claimed[raw]; ok {
		return false
	}
	p.claimed[raw] = struct{}{}
	return true
}

// unclaim releases a reservation after a failed probe. Successful probes keep
// their claim — the config is now owned by that instance.
func (p *probeShared) unclaim(raw string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.claimed, raw)
}

func (s *ProxySelector) startShared(exclude map[string]int, shared *probeShared, deadline time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.configs) == 0 {
		return fmt.Errorf("no configs available")
	}
	skipped, err := s.tryConfigs(exclude, shared, deadline, true)
	if err == nil {
		return nil
	}
	if skipped == 0 {
		return err
	}
	// Bad marks can come from transient failures — retry them once before giving up.
	debugLog("retrying %d bad-marked configs (transient failures possible)...", skipped)
	_, err2 := s.tryConfigs(exclude, shared, deadline, false)
	return err2
}

func (s *ProxySelector) tryConfigs(exclude map[string]int, shared *probeShared, deadline time.Time, respectBad bool) (int, error) {
	skipped := 0
	for i := range s.configs {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return skipped, fmt.Errorf("probe timeout")
		}
		if exclude != nil {
			if _, ok := exclude[s.configs[i].Raw]; ok {
				continue
			}
		}
		if respectBad && shared.isBad(s.configs[i].Raw) {
			skipped++
			continue
		}
		// Reserve before probing so no two workers test the same candidate.
		if !shared.tryClaim(s.configs[i].Raw) {
			continue
		}
		if err := s.startXray(i); err != nil {
			debugLog("candidate %d/%d %s: xray start failed: %v", i+1, len(s.configs), shortName(s.configs[i].Name), err)
			shared.markBad(s.configs[i].Raw)
			shared.unclaim(s.configs[i].Raw)
			continue
		}
		// Fast probe: skip waitForPort, use single-URL 3s timeout.
		// If xray is up, we get a response; if not, connection refused is fast.
		result := TestProxyQuick(fmt.Sprintf("127.0.0.1:%d", s.socksPort), s.testURL)
		if result.Working {
			s.activeIndex = i
			s.lastLatency = result.Latency
			s.failCount = 0
			readyLog(s.configs[i].Name, result.Latency.Milliseconds())
			return skipped, nil
		}
		debugLog("candidate %d/%d %s: unhealthy: %v", i+1, len(s.configs), shortName(s.configs[i].Name), result.Error)
		shared.markBad(s.configs[i].Raw)
		shared.unclaim(s.configs[i].Raw)
		s.stopXray()
	}
	return skipped, fmt.Errorf("no working config found")
}

func (s *ProxySelector) HealthCheck() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeIndex < 0 || s.activeIndex >= len(s.configs) {
		return false
	}

	result := TestProxyHealth(fmt.Sprintf("127.0.0.1:%d", s.socksPort), s.testURL, healthCheckTimeout)
	s.lastCheck = time.Now()
	s.lastLatency = result.Latency

	if result.Working {
		s.failCount = 0
		return true
	}

	s.failCount++
	warnLog("Health FAIL (%d/3): %s - %v", s.failCount, shortName(s.configs[s.activeIndex].Name), result.Error)

	if s.failCount < healthFailThreshold {
		// Consider still healthy until threshold is met
		return true
	}

	return false
}

func (s *ProxySelector) SwitchToNext() error {
	return s.SwitchToNextExcluding(nil)
}

func (s *ProxySelector) SwitchToNextExcluding(exclude map[string]int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.configs) == 0 {
		return fmt.Errorf("no configs available")
	}

	startIdx := s.activeIndex + 1
	if startIdx >= len(s.configs) {
		startIdx = 0
	}

	oldCmd := s.xrayCmd
	oldIndex := s.activeIndex

	for i := startIdx; i != oldIndex; i = (i + 1) % len(s.configs) {
		if i < 0 || i >= len(s.configs) {
			continue
		}
		if exclude != nil {
			if _, ok := exclude[s.configs[i].Raw]; ok {
				continue
			}
		}
		s.stopXrayCmd(oldCmd)
		oldCmd = nil

		if err := s.startXray(i); err != nil {
			continue
		}
		if waitForPort(s.socksPort, switchPortWait) {
			result := TestProxyHealth(fmt.Sprintf("127.0.0.1:%d", s.socksPort), s.testURL, healthCheckTimeout)
			if result.Working {
				s.activeIndex = i
				s.lastLatency = result.Latency
				s.failCount = 0
				switchedLog(s.configs[i].Name, result.Latency.Milliseconds())
				return nil
			}
		}
		s.stopXray()
	}

	if oldIndex >= 0 && oldIndex < len(s.configs) {
		debugLog("All alternative configs failed. Attempting to restore original config %s", shortName(s.configs[oldIndex].Name))
		if err := s.startXray(oldIndex); err == nil {
			s.activeIndex = oldIndex
			s.failCount = 0
		}
	}

	return fmt.Errorf("no working config found")
}

func (s *ProxySelector) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopXray()
}

func (s *ProxySelector) ActiveConfig() *ProxyConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeIndex < 0 || s.activeIndex >= len(s.configs) {
		return nil
	}
	c := s.configs[s.activeIndex]
	return &c
}

func (s *ProxySelector) ShouldCheck() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastCheck) >= s.checkInterval
}

func (s *ProxySelector) startXray(index int) error {
	if index < 0 || index >= len(s.configs) {
		return fmt.Errorf("invalid index: %d", index)
	}

	cfg := s.configs[index]

	logLevel := "none"
	if isDebug() {
		logLevel = "warning"
	}
	fullConfig := map[string]interface{}{
		"log": map[string]interface{}{
			"loglevel": logLevel,
		},
		"dns": map[string]interface{}{
			"servers": []string{
				"https://1.1.1.1/dns-query",
				"localhost",
			},
		},
		"inbounds": []map[string]interface{}{
			{
				"tag":      "socks-in",
				"port":     s.socksPort,
				"listen":   "0.0.0.0",
				"protocol": "socks",
				"settings": map[string]interface{}{
					"auth": "noauth",
					"udp":  true,
				},
			},
		},
		"outbounds": []interface{}{},
	}

	var outbound map[string]interface{}
	if err := json.Unmarshal(cfg.XrayCfg, &outbound); err != nil {
		return fmt.Errorf("bad config: %w", err)
	}

	fullConfig["outbounds"] = []interface{}{
		outbound,
		map[string]interface{}{
			"protocol": "freedom",
			"tag":      "direct",
		},
		map[string]interface{}{
			"protocol": "blackhole",
			"tag":      "blocked",
		},
	}

	fullConfig["routing"] = map[string]interface{}{
		"domainStrategy": "AsIs",
		"rules": []map[string]interface{}{
			{
				"type":        "field",
				"outboundTag": "blocked",
				"protocol":    []string{"bittorrent"},
			},
		},
	}

	cfgPath := filepath.Join(s.xrayDir, fmt.Sprintf("config-%d.json", s.socksPort))
	cfgData, _ := json.Marshal(fullConfig)
	if err := os.WriteFile(cfgPath, cfgData, 0644); err != nil {
		return err
	}

	xrayBin := filepath.Join(s.xrayDir, "xray")
	s.xrayCmd = exec.Command(xrayBin, "run", "-c", cfgPath)
	if isDebug() {
		s.xrayCmd.Stdout = os.Stdout
		s.xrayCmd.Stderr = os.Stderr
	} else {
		s.xrayCmd.Stdout = io.Discard
		s.xrayCmd.Stderr = io.Discard
	}

	if err := s.xrayCmd.Start(); err != nil {
		return fmt.Errorf("start failed: %w", err)
	}

	// Detect immediate crashes (bad config, missing binary, etc.)
	time.Sleep(xrayCrashDetect)
	if s.xrayCmd.Process != nil && s.xrayCmd.Process.Signal(syscall.Signal(0)) != nil {
		return fmt.Errorf("xray crashed on start")
	}

	return nil
}

func (s *ProxySelector) stopXray() {
	s.stopXrayCmd(s.xrayCmd)
	s.xrayCmd = nil
}

func (s *ProxySelector) stopXrayCmd(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(xrayStopWait):
			_ = cmd.Process.Kill()
			<-done
		}
		waitForPortFree(s.socksPort, xrayPortFreeWait)
	}
}

func waitForPort(port int, timeout time.Duration) bool {
	addr := fmt.Sprintf(":%d", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func waitForPortFree(port int, timeout time.Duration) {
	addr := fmt.Sprintf(":%d", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return
		}
		conn.Close()
		time.Sleep(50 * time.Millisecond)
	}
}
