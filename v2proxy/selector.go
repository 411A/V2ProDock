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

// currentPID returns the managed xray child PID (0 = none) for watchdog accounting.
func (s *ProxySelector) currentPID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.xrayCmd != nil && s.xrayCmd.Process != nil {
		return s.xrayCmd.Process.Pid
	}
	return 0
}
func (s *ProxySelector) LastLatency() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastLatency
}

func (s *ProxySelector) UpdateConfigs(configs []ProxyConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-anchor the active index by KEY: the new pool is a different slice, so
	// the old index may now point at a different proxy (or out of range).
	var activeKey string
	if s.activeIndex >= 0 && s.activeIndex < len(s.configs) {
		activeKey = s.configs[s.activeIndex].Key()
	}
	s.configs = configs
	s.activeIndex = -1
	if activeKey != "" {
		for i := range s.configs {
			if s.configs[i].Key() == activeKey {
				s.activeIndex = i
				break
			}
		}
	}
}

// snapshotConfigs returns a copy of the pool for key lookups outside s.mu.
func (s *ProxySelector) snapshotConfigs() []ProxyConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProxyConfig, len(s.configs))
	copy(out, s.configs)
	return out
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
		key := s.configs[i].Key()
		if exclude != nil {
			if _, ok := exclude[s.configs[i].Key()]; ok {
				continue
			}
		}
		if respectBad && shared.isBad(key) {
			skipped++
			continue
		}
		// Reserve before probing so no two workers test the same candidate.
		if !shared.tryClaim(key) {
			continue
		}
		if err := s.startXray(i); err != nil {
			debugLog("candidate %d/%d %s: xray start failed: %v", i+1, len(s.configs), shortName(s.configs[i].Name), err)
			shared.markBad(key)
			shared.unclaim(key)
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
		shared.markBad(key)
		shared.unclaim(key)
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

	// Single fast probe: the old multi-URL pass cost up to ~32s per check and
	// held s.mu the whole time, stalling switches and status reads.
	result := TestProxyQuick(fmt.Sprintf("127.0.0.1:%d", s.socksPort), s.testURL)
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

// switchOrder returns every candidate index exactly once, starting just after
// the active one and wrapping around (the old index itself is excluded: there
// is no point re-probing the config that just failed).
// The previous loop `for i := start; i != old` NEVER TERMINATED when old == -1
// (instance never started), churning xray processes forever.
func switchOrder(n, startIdx, oldIndex int) []int {
	if n <= 0 {
		return nil
	}
	start := startIdx % n
	if start < 0 {
		start = 0
	}
	order := make([]int, 0, n)
	for k := 0; k < n; k++ {
		i := (start + k) % n
		if i == oldIndex {
			continue
		}
		order = append(order, i)
	}
	return order
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
	deadline := time.Now().Add(switchBudget)

	for _, i := range switchOrder(len(s.configs), startIdx, oldIndex) {
		if time.Now().After(deadline) {
			debugLog("switch budget exhausted, giving up for now")
			break
		}
		if exclude != nil {
			if _, ok := exclude[s.configs[i].Key()]; ok {
				continue
			}
		}
		if oldCmd != nil {
			s.stopXrayCmd(oldCmd)
			oldCmd = nil
		}

		if err := s.startXray(i); err != nil {
			continue
		}
		if !waitForPort(s.socksPort, switchPortWait) {
			s.stopXray()
			continue
		}
		// Fast single-URL probe: a full multi-URL health pass here cost ~32s
		// per dead candidate and froze the ticker loop for tens of minutes.
		result := TestProxyQuick(fmt.Sprintf("127.0.0.1:%d", s.socksPort), s.testURL)
		if result.Working {
			s.activeIndex = i
			s.lastLatency = result.Latency
			s.failCount = 0
			switchedLog(s.configs[i].Name, result.Latency.Milliseconds())
			return nil
		}
		debugLog("candidate %s: unhealthy: %v", shortName(s.configs[i].Name), result.Error)
		s.stopXray()
	}

	if oldIndex >= 0 && oldIndex < len(s.configs) {
		debugLog("All alternative configs failed. Attempting to restore original config %s", shortName(s.configs[oldIndex].Name))
		s.stopXray()
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

	cfgPath, err := s.renderXrayConfig(s.configs[index], s.socksPort, false)
	if err != nil {
		return err
	}

	// Never orphan a previous child: a failed start leaves s.xrayCmd pointing
	// at a possibly-live process, and overwriting the handle would leak it
	// (leaked xrays keep holding the SOCKS port and break every later bind).
	if s.xrayCmd != nil && s.xrayCmd.Process != nil {
		s.stopXrayCmd(s.xrayCmd)
		s.xrayCmd = nil
	}

	cmd, err := launchXray(s.xrayDir, cfgPath)
	if err != nil {
		return err
	}
	s.xrayCmd = cmd
	return nil
}

// probeCandidateOnTempPort tests one candidate through a throwaway xray on an
// ephemeral loopback port. Serving on the real SOCKS port is never touched,
// so rotation probing costs zero disruption. Callers must NOT hold s.mu (it
// takes s.mu briefly for config access only).
func (s *ProxySelector) probeCandidateOnTempPort(cfg ProxyConfig) HealthResult {
	// Ephemeral port: bind :0, read back the port, release.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return HealthResult{Error: err}
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	cfgPath, err := s.renderXrayConfig(cfg, port, true)
	if err != nil {
		return HealthResult{Error: err}
	}
	defer os.Remove(cfgPath)

	cmd, err := launchXray(s.xrayDir, cfgPath)
	if err != nil {
		return HealthResult{Error: err}
	}
	defer stopXrayCmdPort(cmd, port)

	if !waitForPort(port, switchPortWait) {
		return HealthResult{Error: fmt.Errorf("temp xray port never opened")}
	}
	return TestProxyQuick(fmt.Sprintf("127.0.0.1:%d", port), s.testURL)
}

// renderXrayConfig writes the full xray config for one upstream. temp files
// get a distinct name so rotation probes never clobber the serving config.
func (s *ProxySelector) renderXrayConfig(cfg ProxyConfig, socksPort int, temp bool) (string, error) {
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
				"port":     socksPort,
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
		return "", fmt.Errorf("bad config: %w", err)
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

	name := fmt.Sprintf("config-%d.json", socksPort)
	if temp {
		name = fmt.Sprintf("config-rot-%d.json", socksPort)
	}
	cfgPath := filepath.Join(s.xrayDir, name)
	cfgData, _ := json.Marshal(fullConfig)
	if err := os.WriteFile(cfgPath, cfgData, 0644); err != nil {
		return "", err
	}
	return cfgPath, nil
}

// launchXray starts one xray child in its own process group and rejects
// instant crashes (bad config, missing binary, port already held).
func launchXray(xrayDir, cfgPath string) (*exec.Cmd, error) {
	xrayBin := filepath.Join(xrayDir, "xray")
	cmd := exec.Command(xrayBin, "run", "-c", cfgPath)
	isolateChild(cmd)
	if isDebug() {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start failed: %w", err)
	}

	// Detect immediate crashes (bad config, missing binary, etc.)
	time.Sleep(xrayCrashDetect)
	if !processAlive(cmd.Process) {
		_ = cmd.Wait() // reap; caller drops the handle, so reap here
		return nil, fmt.Errorf("xray crashed on start")
	}

	return cmd, nil
}

func (s *ProxySelector) stopXray() {
	s.stopXrayCmd(s.xrayCmd)
	s.xrayCmd = nil
}

func (s *ProxySelector) stopXrayCmd(cmd *exec.Cmd) {
	stopXrayCmdPort(cmd, s.socksPort)
}

// stopXrayCmdPort terminates one xray child (whole process group on unix),
// reaps it, and waits until its port is actually free — so the next bind on
// that port cannot fail. Package-level so temp-port rotation probes reuse it.
func stopXrayCmdPort(cmd *exec.Cmd, port int) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	// Whole process group on unix (negative PID); ESRCH (already dead) is fine.
	_ = signalGroup(pid, os.Interrupt)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait() // reap the zombie either way
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(xrayStopWait):
		_ = killGroup(pid)
		<-done
	}
	waitForPortFree(port, xrayPortFreeWait)
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
