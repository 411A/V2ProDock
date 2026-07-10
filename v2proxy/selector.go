package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	xraySocksPortStart = 27019
	xraySocksPortEnd   = 27999
)

type ProxySelector struct {
	mu            sync.Mutex
	configs       []ProxyConfig
	activeIndex   int
	xrayCmd       *exec.Cmd
	xrayDir       string
	testURL       string
	socksPort     int
	checkInterval time.Duration
	lastCheck     time.Time
}

func NewProxySelector(xrayDir, testURL string, checkInterval time.Duration) *ProxySelector {
	os.MkdirAll(xrayDir, 0755)
	port := resolvePort()
	return &ProxySelector{
		xrayDir:       xrayDir,
		testURL:       testURL,
		socksPort:     port,
		checkInterval: checkInterval,
		activeIndex:   -1,
	}
}

func resolvePort() int {
	// Check if 27019 is free
	conn, err := net.DialTimeout("tcp", "127.0.0.1:27019", 200*time.Millisecond)
	if err != nil {
		// Port is free, use it
		return 27019
	}
	conn.Close()

	// Port is taken - check if it's our own xray process
	if isOursOnPort(27019) {
		// Kill our old process and reuse 27019
		killProcessOnPort(27019)
		time.Sleep(500 * time.Millisecond)
		return 27019
	}

	// Port is taken by something else - find next available
	log.Printf("Port 27019 occupied by another process, finding alternative...")
	for port := 27020; port <= xraySocksPortEnd; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			ln.Close()
			log.Printf("Using port %d instead", port)
			return port
		}
	}

	// Fallback
	return 27019
}

func isOursOnPort(port int) bool {
	// Check if an xray or v2proxy process owns this port
	cmd := exec.Command("lsof", "-i", fmt.Sprintf(":%d", port), "-t")
	out, err := cmd.Output()
	if err != nil {
		return false
	}

	pids := strings.Fields(strings.TrimSpace(string(out)))
	for _, pid := range pids {
		// Check process name
		cmd2 := exec.Command("ps", "-p", pid, "-o", "comm=")
		name, err := cmd2.Output()
		if err != nil {
			continue
		}
		procName := strings.TrimSpace(string(name))
		if strings.Contains(procName, "xray") || strings.Contains(procName, "v2proxy") {
			return true
		}
	}
	return false
}

func killProcessOnPort(port int) {
	cmd := exec.Command("lsof", "-i", fmt.Sprintf(":%d", port), "-t")
	out, err := cmd.Output()
	if err != nil {
		return
	}

	pids := strings.Fields(strings.TrimSpace(string(out)))
	for _, pid := range pids {
		exec.Command("kill", "-9", pid).Run()
		log.Printf("Killed old process %s on port %d", pid, port)
	}
}

func (s *ProxySelector) UpdateConfigs(configs []ProxyConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configs = configs
}

func (s *ProxySelector) StartWithBest() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.configs) == 0 {
		return fmt.Errorf("no configs available")
	}

	for i := range s.configs {
		if err := s.startXray(i); err != nil {
			continue
		}
		if waitForPort(fmt.Sprintf(":%d", s.socksPort), 2*time.Second) {
			result := TestProxyHealth(fmt.Sprintf("127.0.0.1:%d", s.socksPort), s.testURL, 8*time.Second)
			if result.Working {
				s.activeIndex = i
				log.Printf("Using: %s (latency: %v)", s.configs[i].Name, result.Latency)
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

	if result.Working {
		log.Printf("Health OK: %s (%v)", s.configs[s.activeIndex].Name, result.Latency)
		return true
	}

	log.Printf("Health FAIL: %s - %v", s.configs[s.activeIndex].Name, result.Error)
	return false
}

func (s *ProxySelector) SwitchToNext() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stopXray()

	startIdx := s.activeIndex + 1
	if startIdx >= len(s.configs) {
		startIdx = 0
	}

	for i := startIdx; i != s.activeIndex; i = (i + 1) % len(s.configs) {
		if err := s.startXray(i); err != nil {
			continue
		}
		if waitForPort(fmt.Sprintf(":%d", s.socksPort), 2*time.Second) {
			result := TestProxyHealth(fmt.Sprintf("127.0.0.1:%d", s.socksPort), s.testURL, 8*time.Second)
			if result.Working {
				s.activeIndex = i
				log.Printf("Switched: %s (%v)", s.configs[i].Name, result.Latency)
				return nil
			}
		}
		s.stopXray()
	}
	return fmt.Errorf("no working config found")
}

func (s *ProxySelector) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopXray()
}

func (s *ProxySelector) GetStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeIndex < 0 || s.activeIndex >= len(s.configs) {
		return "No active proxy"
	}
	httpPort := s.socksPort + 1
	return fmt.Sprintf("Active: %s | SOCKS5: :%d | HTTP: :%d | Total: %d",
		s.configs[s.activeIndex].Name, s.socksPort, httpPort, len(s.configs))
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

	fullConfig := map[string]interface{}{
		"log": map[string]interface{}{
			"loglevel": "info",
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

	cfgPath := filepath.Join(s.xrayDir, "config.json")
	cfgData, _ := json.MarshalIndent(fullConfig, "", "  ")
	if err := os.WriteFile(cfgPath, cfgData, 0644); err != nil {
		return err
	}

	xrayBin := filepath.Join(s.xrayDir, "xray")
	s.xrayCmd = exec.Command(xrayBin, "run", "-c", cfgPath)
	s.xrayCmd.Stdout = os.Stdout
	s.xrayCmd.Stderr = os.Stderr

	if err := s.xrayCmd.Start(); err != nil {
		return fmt.Errorf("start failed: %w", err)
	}

	return nil
}

func (s *ProxySelector) stopXray() {
	if s.xrayCmd != nil && s.xrayCmd.Process != nil {
		// Try graceful shutdown first (SIGTERM), then force kill
		s.xrayCmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() {
			s.xrayCmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			s.xrayCmd.Process.Kill()
			<-done
		}
		s.xrayCmd = nil
		waitForPortFree(fmt.Sprintf(":%d", s.socksPort), 3*time.Second)
	}
}

func waitForPort(addr string, timeout time.Duration) bool {
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

func waitForPortFree(addr string, timeout time.Duration) {
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
