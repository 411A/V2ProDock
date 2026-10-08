package main

// Process hygiene for the test binary.
//
// The 24 orphans that accumulate on a developer's machine are real, LIVE
// processes (state S, not zombies), each holding a LISTENING ephemeral port and
// roughly 15-40MB. They are not produced by any current test - every launch is
// paired with a t.Cleanup(s.Stop) via newTestSelector - but nothing in the suite
// can ever remove them:
//
//   - pruneOrphanXray needs a live *ProxyManager keep-set, and no test calls
//     WatchdogCheck, so the reaper is unreachable from a test run;
//   - they are re-parented to init once the test binary exits, so they outlive
//     the run that made them;
//   - t.Cleanup does not run at all when `go test` hits its -timeout (the
//     handler calls os.Exit), which is the remaining way a child escapes.
//
// They matter because ephemeral ports are exactly what freeLoopbackPort hands
// out: an orphan holding a port turns waitForPort's check green against the
// WRONG process, and a recycled port makes a stub fail to bind inside
// launchXray's 100ms crash window.
//
// So this reaps by shape rather than by xrayDir (a per-test t.TempDir() is gone
// by the time TestMain runs): the same argv shape the production watchdog
// matches - a binary whose base is xray, the literal arg "run", and a -c config
// path under a temp test directory.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// testOrphanCmdline matches this PACKAGE's test children only: it requires the
// config path to sit under an OS temp dir whose name starts with "Test", which a
// production /root/xray never does.
func testOrphanCmdline(raw string) bool {
	parts := strings.Split(raw, "\x00")
	if len(parts) < 4 {
		return false
	}
	base := filepath.Base(parts[0])
	if base != "xray" && base != "xray.exe" && !strings.HasSuffix(parts[0], "/xray") {
		// The python stub is launched via shebang: interpreter at argv[0],
		// the script at argv[1].
		if !strings.HasPrefix(parts[0], "python") || !strings.HasSuffix(parts[1], "/xray") {
			return false
		}
		parts = parts[1:]
	}
	hasRun := false
	for _, p := range parts {
		if p == "run" {
			hasRun = true
		}
	}
	if !hasRun {
		return false
	}
	for i, p := range parts {
		if p != "-c" || i+1 >= len(parts) {
			continue
		}
		cfg := parts[i+1]
		name := filepath.Base(cfg)
		if !strings.HasPrefix(name, "config-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(cfg))
		// /tmp/TestXxx…/001/config-1.json  or  /mnt/c/.../TestXxx…/001/…
		if i := strings.LastIndex(dir, "/Test"); i >= 0 {
			return true
		}
	}
	return false
}

// reapTestOrphans kills leftover stub/real xray children from earlier runs.
// Best effort: a PID that exits between listing and killing is not an error.
func reapTestOrphans() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0 // non-linux: nothing to scan
	}
	self := os.Getpid()
	killed := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if !testOrphanCmdline(string(raw)) {
			continue
		}
		// killGroup already handles the per-OS signalling (negative PID on unix
		// for the whole group, plain kill on windows). A PID that exits between
		// listing and killing is not an error.
		_ = killGroup(pid)
		killed++
	}
	return killed
}

func TestMain(m *testing.M) {
	code := m.Run()
	if n := reapTestOrphans(); n > 0 {
		println("v2proxy: reaped", n, "orphaned test xray process(es) from an earlier run")
	}
	os.Exit(code)
}
