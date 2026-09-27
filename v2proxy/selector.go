package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
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
	// Stability signals for long-polling clients: a 2s proxy that stays up an
	// hour beats a 200ms one that dies every 5 minutes, but latency alone
	// cannot see that. okStreak counts CONSECUTIVE successful health checks
	// on the current upstream (any failure zeroes it); activeSince marks when
	// the current upstream was adopted. Exposed via /proxies for ranking.
	okStreak    int
	activeSince time.Time
	// tempMu serializes throwaway-port allocation (bind :0, render, launch):
	// two workers must never pick the same ephemeral port/file.
	tempMu sync.Mutex
	// tempSeq makes throwaway config filenames unique even if the kernel
	// ever hands two workers the same ephemeral port number.
	tempSeq atomic.Uint64
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
	s.activeIndex = slices.IndexFunc(s.configs, func(c ProxyConfig) bool {
		return activeKey != "" && c.Key() == activeKey
	})
}

// snapshotConfigs returns a copy of the pool for key lookups outside s.mu.
func (s *ProxySelector) snapshotConfigs() []ProxyConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.configs)
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
		// Indexing a nil exclude map is safe in Go (yields zero value),
		// so no nil check is needed here.
		if _, ok := exclude[key]; ok {
			continue
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
			s.okStreak = 0
			s.activeSince = time.Now()
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

	// Passive proof first: real client bytes flowed through this instance
	// within grace — it is responsive BY DEFINITION, so no synthetic probe
	// may strike it. Skipping also spares probe traffic on busy instances.
	if egressActive([]int{s.httpPort, s.socksPort}, egressGrace) {
		s.lastCheck = time.Now()
		s.failCount = 0
		s.okStreak++
		debugLog("instance serving client traffic, probe skipped (streak %d)", s.okStreak)
		return true
	}

	// Single fast probe: the old multi-URL pass cost up to ~32s per check and
	// held s.mu the whole time, stalling switches and status reads.
	result := TestProxyQuick(fmt.Sprintf("127.0.0.1:%d", s.socksPort), s.testURL)
	s.lastCheck = time.Now()
	s.lastLatency = result.Latency

	if result.Working {
		s.failCount = 0
		s.okStreak++
		if s.activeSince.IsZero() {
			s.activeSince = time.Now()
		}
		return true
	}

	// Inconclusive, not dead: the PROBE TARGET throttled/blocked our egress
	// IP (429/403) while the tunnel itself may be fine. Neither strike nor
	// absolve — hold serving, retry next tick. Anything else strikes.
	if result.Status == http.StatusTooManyRequests || result.Status == http.StatusForbidden {
		debugLog("probe inconclusive (%d from target, tunnel may be fine): %s", result.Status, shortName(s.configs[s.activeIndex].Name))
		return true
	}

	s.failCount++
	s.okStreak = 0
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
	for k := range n {
		i := (start + k) % n
		if i == oldIndex {
			continue
		}
		order = append(order, i)
	}
	return order
}

func (s *ProxySelector) SwitchToNextExcluding(exclude map[string]int) error {
	// Snapshot under lock; the search itself runs lock-free on throwaway
	// ports so the OLD xray keeps serving its port the whole time. Only the
	// final swap (stop old, start proven winner) briefly rebinds the port.
	s.mu.Lock()
	if len(s.configs) == 0 {
		s.mu.Unlock()
		return fmt.Errorf("no configs available")
	}
	configs := slices.Clone(s.configs)
	startIdx := s.activeIndex + 1
	if startIdx >= len(configs) {
		startIdx = 0
	}
	oldIndex := s.activeIndex
	var oldKey string
	if oldIndex >= 0 && oldIndex < len(configs) {
		oldKey = configs[oldIndex].Key()
	}
	cands := make([]int, 0, len(configs))
	for _, i := range switchOrder(len(configs), startIdx, oldIndex) {
		if _, ok := exclude[configs[i].Key()]; ok {
			continue
		}
		cands = append(cands, i)
	}
	s.mu.Unlock()

	if len(cands) == 0 {
		return fmt.Errorf("no working config found")
	}

	// Parallel search: workers probe candidates on throwaway ports, first
	// proven winner stops the search. Serving is untouched until the swap.
	searchStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), switchBudget)
	defer cancel()
	winIdx, winLat := s.searchCandidates(ctx, configs, cands)
	debugLog("switch scanned %d candidate(s) in %dms", len(cands), time.Since(searchStart).Milliseconds())
	if winIdx < 0 {
		debugLog("switch found no working candidate (old keeps serving)")
		return fmt.Errorf("no working config found")
	}

	// Swap under lock. The pool may have been refreshed mid-search, so the
	// winner is re-anchored by KEY, never trusted by snapshot index.
	s.mu.Lock()
	defer s.mu.Unlock()
	winKey := configs[winIdx].Key()
	cur := -1
	for i := range s.configs {
		if s.configs[i].Key() == winKey {
			cur = i
			break
		}
	}
	if cur < 0 {
		return fmt.Errorf("winner vanished after refresh")
	}
	s.stopXray()
	if err := s.startXray(cur); err != nil {
		s.tryRestoreLocked(oldKey)
		return fmt.Errorf("winner failed on serving port: %w", err)
	}
	if !waitForPort(s.socksPort, switchPortWait) {
		s.stopXray()
		s.tryRestoreLocked(oldKey)
		return fmt.Errorf("winner never bound serving port")
	}
	if res := TestProxyQuick(fmt.Sprintf("127.0.0.1:%d", s.socksPort), s.testURL); !res.Working {
		debugLog("winner failed verification on serving port: %v", res.Error)
		s.stopXray()
		s.tryRestoreLocked(oldKey)
		return fmt.Errorf("winner failed verification: %w", res.Error)
	}
	s.activeIndex = cur
	s.lastLatency = winLat
	s.failCount = 0
	s.okStreak = 0
	s.activeSince = time.Now()
	switchedLog(s.configs[cur].Name, winLat.Milliseconds())
	return nil
}

// Stability returns the consecutive-success streak and adoption time of the
// current upstream (zero values = never healthy yet).
func (s *ProxySelector) Stability() (int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.okStreak, s.activeSince
}

// tryRestoreLocked best-effort restarts the pre-switch config (by key) after
// a failed swap. Caller holds s.mu. A dead upstream is still better than a
// silent port: the next tick retries the switch.
func (s *ProxySelector) tryRestoreLocked(oldKey string) {
	if oldKey == "" {
		return
	}
	for i := range s.configs {
		if s.configs[i].Key() == oldKey {
			debugLog("restoring pre-switch config %s", shortName(s.configs[i].Name))
			if err := s.startXray(i); err == nil {
				s.activeIndex = i
				s.failCount = 0
				s.okStreak = 0
				s.activeSince = time.Now()
			}
			return
		}
	}
}

// searchCandidates probes snapshot candidates in parallel on throwaway ports.
// Returns the snapshot index + latency of the first proven winner, or -1 when
// the budget expires with nothing working. Never touches serving state.
func (s *ProxySelector) searchCandidates(ctx context.Context, configs []ProxyConfig, cands []int) (int, time.Duration) {
	jobs := make(chan int)
	var wg sync.WaitGroup
	var winMu sync.Mutex
	winIdx := -1
	var winLat time.Duration

	worker := func() {
		defer wg.Done()
		for idx := range jobs {
			if ctx.Err() != nil {
				return
			}
			res := s.probeSnapshotOnTempPort(configs[idx])
			if res.Working {
				winMu.Lock()
				if winIdx < 0 {
					winIdx, winLat = idx, res.Latency
				}
				winMu.Unlock()
				return
			}
			debugLog("candidate %s: unhealthy: %v", shortName(configs[idx].Name), res.Error)
		}
	}
	n := min(switchWorkerCount(), len(cands))
	for range n {
		wg.Add(1)
		go worker()
	}
	go func() {
		defer close(jobs)
		for _, idx := range cands {
			select {
			case jobs <- idx:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	return winIdx, winLat
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

	cfgPath, err := s.renderXrayConfig(s.configs[index], s.socksPort, 0)
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
	return s.probeSnapshotOnTempPort(cfg)
}

// probeSnapshotOnTempPort is the lock-free core used by parallel switches and
// rotation alike: it only reads immutable selector fields (xrayDir, testURL),
// so any number of workers may run it concurrently. Port allocation through
// launch is serialized on tempMu so two workers can never share an ephemeral
// port or config file.
func (s *ProxySelector) probeSnapshotOnTempPort(cfg ProxyConfig) HealthResult {
	s.tempMu.Lock()
	// Ephemeral port: bind :0, read back the port, release.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.tempMu.Unlock()
		return HealthResult{Error: err}
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	seq := s.tempSeq.Add(1)
	cfgPath, err := s.renderXrayConfig(cfg, port, seq)
	if err != nil {
		s.tempMu.Unlock()
		return HealthResult{Error: err}
	}

	cmd, err := launchXray(s.xrayDir, cfgPath)
	if err != nil {
		s.tempMu.Unlock()
		_ = os.Remove(cfgPath)
		return HealthResult{Error: err}
	}
	open := waitForPort(port, switchPortWait)
	s.tempMu.Unlock()
	defer func() {
		stopXrayCmdPort(cmd, port)
		_ = os.Remove(cfgPath)
	}()

	if !open {
		return HealthResult{Error: fmt.Errorf("temp xray port never opened")}
	}
	return TestProxyQuick(fmt.Sprintf("127.0.0.1:%d", port), s.testURL)
}

// renderXrayConfig writes the full xray config for one upstream. seq > 0
// marks a throwaway probe config: the filename carries the sequence so
// parallel workers never clobber each other (or the serving config).
// When XRAY_FRAGMENT=1 and the upstream negotiates TLS, the proxy outbound
// chains via sockopt.dialerProxy at a freedom outbound tagged "frag-out"
// that carries settings.fragment (schema proven against Xray 26.3.27:
// loads + proxies end-to-end; a "fragment"-protocol outbound does NOT exist
// in this build). Plaintext upstreams and pre-chained outbounds are left
// untouched — fragment buys them nothing and costs handshake overhead.
func (s *ProxySelector) renderXrayConfig(cfg ProxyConfig, socksPort int, seq uint64) (string, error) {
	logLevel := "none"
	if isDebug() {
		logLevel = "warning"
	}
	fullConfig := map[string]any{
		"log": map[string]any{
			"loglevel": logLevel,
		},
		"dns": map[string]any{
			"servers": []string{
				"https://1.1.1.1/dns-query",
				"localhost",
			},
		},
		"inbounds": []map[string]any{
			{
				"tag":      "socks-in",
				"port":     socksPort,
				"listen":   "0.0.0.0",
				"protocol": "socks",
				"settings": map[string]any{
					"auth": "noauth",
					"udp":  true,
				},
			},
		},
		"outbounds": []any{},
	}

	var outbound map[string]any
	if err := json.Unmarshal(cfg.XrayCfg, &outbound); err != nil {
		return "", fmt.Errorf("bad config: %w", err)
	}

	outbounds := []any{outbound}
	if fragmentEnabled() && upstreamUsesTLS(outbound) && !hasCustomDialChain(outbound) {
		chainFragment(outbound)
		outbounds = append(outbounds, map[string]any{
			"protocol": "freedom",
			"tag":      fragmentOutTag,
			"settings": map[string]any{
				"fragment": map[string]any{
					"packets":  fragmentPackets,
					"length":   fragmentLength,
					"interval": fragmentInterval,
				},
			},
		})
	}
	outbounds = append(outbounds,
		map[string]any{
			"protocol": "freedom",
			"tag":      "direct",
		},
		map[string]any{
			"protocol": "blackhole",
			"tag":      "blocked",
		},
	)
	fullConfig["outbounds"] = outbounds

	fullConfig["routing"] = map[string]any{
		"domainStrategy": "AsIs",
		"rules": []map[string]any{
			{
				"type":        "field",
				"outboundTag": "blocked",
				"protocol":    []string{"bittorrent"},
			},
		},
	}

	name := fmt.Sprintf("config-%d.json", socksPort)
	if seq > 0 {
		name = fmt.Sprintf("config-rot-%d-%d.json", socksPort, seq)
	}
	cfgPath := filepath.Join(s.xrayDir, name)
	cfgData, _ := json.Marshal(fullConfig)
	if err := os.WriteFile(cfgPath, cfgData, 0644); err != nil {
		return "", err
	}
	return cfgPath, nil
}

// chainFragment points an upstream outbound's dial path at the fragment
// carrier, preserving any existing sockopt keys the subscription set.
func chainFragment(outbound map[string]any) {
	ss, ok := outbound["streamSettings"].(map[string]any)
	if !ok {
		ss = map[string]any{}
		outbound["streamSettings"] = ss
	}
	so, ok := ss["sockopt"].(map[string]any)
	if !ok {
		so = map[string]any{}
		ss["sockopt"] = so
	}
	so["dialerProxy"] = fragmentOutTag
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
