package main

// Self-healing watchdog: backstop behind the normal lifecycle.
//
//   - Port sanity: an instance marked ok whose local SOCKS port no longer
//     accepts TCP is switched immediately (bounded switch, no waiting for the
//     next health tick). A dead HTTP bridge listener is rebound the same way.
//   - Orphan prune: any xray child matching our config pattern that is NOT one
//     of the currently managed PIDs is group-killed. Victims are re-verified
//     against a fresh PID set right before the kill, so a child spawned by a
//     racing switch/refresh between scan and kill is never harmed.
//
// Cost is one /proc scan + a few loopback dials per health tick — negligible.

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// cmdlineIsManaged reports whether a /proc cmdline belongs to one of our xray
// children: some argv part launches the xray binary and it runs
// `run -c <xrayDir>/config-*.json`. The binary may sit at argv[1] (shebang or
// wrapper re-exec puts the interpreter at argv[0]), so any part may name it.
func cmdlineIsManaged(cmdline string, xrayDir string) bool {
	parts := strings.Split(cmdline, "\x00")
	if len(parts) < 4 {
		return false
	}
	hasBin := false
	hasRun := false
	hasCfg := false
	for _, p := range parts {
		base := filepath.Base(p)
		if base == "xray" || base == "xray.exe" ||
			strings.HasSuffix(p, string(os.PathSeparator)+"xray") ||
			strings.HasSuffix(p, string(os.PathSeparator)+"xray.exe") {
			hasBin = true
		}
		if p == "run" {
			hasRun = true
		}
		if strings.HasSuffix(p, ".json") && strings.Contains(p, xrayDir) &&
			strings.Contains(filepath.Base(p), "config-") {
			hasCfg = true
		}
	}
	return hasBin && hasRun && hasCfg
}

// listXrayPIDs returns PIDs of processes matching our xray pattern.
// Non-Linux (no /proc) yields an empty set — pruning is a no-op there.
func listXrayPIDs(xrayDir string) map[int]bool {
	out := make(map[int]bool)
	if runtime.GOOS != "linux" {
		return out
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	self := os.Getpid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		if cmdlineIsManaged(string(raw), xrayDir) {
			out[pid] = true
		}
	}
	return out
}

// orphanVictims is the pure victim-selection core of the prune: every found
// PID absent from the keep set. Pure so the exemption logic stays
// unit-testable without /proc.
func orphanVictims(found, keep map[int]bool) []int {
	victims := make([]int, 0)
	for pid := range found {
		if !keep[pid] {
			victims = append(victims, pid)
		}
	}
	return victims
}

// pruneOrphanXray group-kills matching xray processes absent from the live set.
// Each victim is re-verified against a fresh live() snapshot just before the
// kill so a concurrently spawned child is never harmed. Returns kill count.
func pruneOrphanXray(xrayDir string, live func() map[int]bool) int {
	found := listXrayPIDs(xrayDir)
	if len(found) == 0 {
		return 0
	}
	victims := orphanVictims(found, live())
	if len(victims) == 0 {
		return 0
	}
	// Grace window: a child spawned after the scan assigns its handle under
	// s.mu within milliseconds; re-check so it lands in the keep set.
	time.Sleep(500 * time.Millisecond)
	keep := live()
	killed := 0
	for _, pid := range victims {
		if keep[pid] {
			continue
		}
		// Confirm it is still ours before signalling (pid reuse guard).
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil || !cmdlineIsManaged(string(raw), xrayDir) {
			continue
		}
		_ = killGroup(pid)
		debugLog("watchdog: reaped orphan xray pid %d", pid)
		killed++
	}
	return killed
}

func tcpOpen(addr string, timeout time.Duration) bool {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func (m *ProxyManager) managedPIDs() map[int]bool {
	insts, _ := m.snapshot()
	out := make(map[int]bool, len(insts))
	for _, inst := range insts {
		if pid := inst.currentPID(); pid > 0 {
			out[pid] = true
		}
		// In-flight throwaway probes own no serving handle but are live
		// managed children all the same — exempt them from the prune.
		for pid := range inst.tempPIDSnapshot() {
			out[pid] = true
		}
	}
	return out
}

// WatchdogCheck runs after every health pass: port sanity per ok instance +
// orphan prune. All branches are fast (loopback dials, one /proc scan); the
// only slow path is an immediate bounded switch for a provably-dead port.
func (m *ProxyManager) WatchdogCheck() {
	insts, _ := m.snapshot()
	for i, inst := range insts {
		if m.statusOf(i).Status != "ok" {
			continue
		}
		socksAddr := fmt.Sprintf("127.0.0.1:%d", inst.SOCKSPort())
		if !tcpOpen(socksAddr, watchdogPortDialWait) {
			warnLog("Instance %d: SOCKS %s unresponsive despite ok status, switching now...", i, socksAddr)
			m.markDown(i, "watchdog: local SOCKS port unresponsive")
			used := m.buildExcluding(i)
			if err := inst.SwitchToNextExcluding(used); err != nil {
				m.markDown(i, err.Error())
			} else if cfg := inst.ActiveConfig(); cfg != nil {
				m.markOK(i, cfg.Name, inst.LastLatency())
			}
			continue
		}
		httpAddr := fmt.Sprintf("127.0.0.1:%d", inst.HTTPPort())
		if !tcpOpen(httpAddr, watchdogPortDialWait) {
			// Rebind is safe: if the old listener is somehow alive, Listen
			// fails with "address in use" and logs; if dead, serving resumes.
			errLog("Instance %d: HTTP bridge %s down, rebinding...", i, httpAddr)
			startHTTPProxy(
				fmt.Sprintf("0.0.0.0:%d", inst.HTTPPort()),
				fmt.Sprintf("127.0.0.1:%d", inst.SOCKSPort()),
			)
		}
	}
	if killed := pruneOrphanXray(m.xrayDir, m.managedPIDs); killed > 0 {
		warnLog("watchdog: reaped %d orphan xray process(es)", killed)
	}
}
