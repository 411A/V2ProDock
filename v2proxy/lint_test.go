package main

// Static-analysis layer tests: gofmt cleanliness and go vet must hold on
// every run, not just in CI. Failures here are style/correctness regressions.

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func goBinary(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	t.Skip("go toolchain not on PATH")
	return ""
}

// TestGofmtClean fails if any non-vendored source file needs formatting.
func TestGofmtClean(t *testing.T) {
	gofmt, err := exec.LookPath("gofmt")
	if err != nil {
		// Not on PATH (common on Windows): ask the toolchain for GOROOT.
		out, gerr := exec.Command(goBinary(t), "env", "GOROOT").Output()
		if gerr != nil {
			t.Skipf("cannot locate gofmt: %v", gerr)
		}
		gofmt = filepath.Join(strings.TrimSpace(string(out)), "bin", "gofmt")
		if runtime.GOOS == "windows" {
			gofmt += ".exe"
		}
	}
	out, err := exec.Command(gofmt, "-l", ".").Output()
	if err != nil {
		t.Fatalf("gofmt -l: %v", err)
	}
	var dirty []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "vendor") {
			continue
		}
		dirty = append(dirty, line)
	}
	if len(dirty) > 0 {
		t.Fatalf("unformatted files (run gofmt -w):\n%s", strings.Join(dirty, "\n"))
	}
}

// TestGoVetClean runs the full vet suite over the module.
func TestGoVetClean(t *testing.T) {
	goBin := goBinary(t)
	cmd := exec.Command(goBin, "vet", "./...")
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go vet ./... failed: %v\n%s", err, raw)
	}
}
