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
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.configs) == 0 {
		return fmt.Errorf("no configs available")
	}

	for i := range s.configs {
		if exclude != nil {
			if _, ok := exclude[s.configs[i].Raw]; ok {
				continue
			}
		}
		if err := s.startXray(i); err != nil {
			continue
		}
		if waitForPort(s.socksPort, 2*time.Second) {
			result := TestProxyHealth(fmt.Sprintf("127.0.0.1:%d", s.socksPort), s.testURL, 8*time.Second)
			if result.Working {
				s.activeIndex = i
				s.lastLatency = result.Latency
				s.failCount = 0
				readyLog(s.configs[i].Name, result.Latency.Milliseconds())
				return nil
			}
		}
		s.stopXray()
	}
	return fmt.Errorf("no working config found")
}

func (s *ProxySelector) HealthCheck() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeIndex < 0 || s.activeIndex >= len(s.configs) {
		return false
	}

	result := TestProxyHealth(fmt.Sprintf("127.0.0.1:%d", s.socksPort), s.testURL, 8*time.Second)
	s.lastCheck = time.Now()
	s.lastLatency = result.Latency

	if result.Working {
		s.failCount = 0
		return true
	}

	s.failCount++
	warnLog("Health FAIL (%d/3): %s - %v", s.failCount, shortName(s.configs[s.activeIndex].Name), result.Error)

	if s.failCount < 3 {
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
		if waitForPort(s.socksPort, 2*time.Second) {
			result := TestProxyHealth(fmt.Sprintf("127.0.0.1:%d", s.socksPort), s.testURL, 8*time.Second)
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
	time.Sleep(500 * time.Millisecond)
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
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		waitForPortFree(s.socksPort, 3*time.Second)
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
