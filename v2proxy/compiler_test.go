package main

// Compiler-layer tests: prove the toolchain can actually compile this module
// for every supported target and that the modern stdlib APIs we rely on
// (per the use-modern-go skill) exist. A failure here means "upgrade Go",
// not "fix the code".

import (
	"cmp"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestModernAPIsLink exercises the newest stdlib APIs used by production code
// so an outdated toolchain fails at build time with a clear error.
func TestModernAPIsLink(t *testing.T) {
	var wg sync.WaitGroup
	done := make(chan string, 1)
	wg.Go(func() { done <- cmp.Or("", "modern") })
	wg.Wait()
	if got := <-done; got != "modern" {
		t.Fatalf("wg.Go/cmp.Or misbehaving: %q", got)
	}
	var lines []string
	for line := range strings.SplitSeq("a\nb", "\n") {
		lines = append(lines, line)
	}
	if !slices.Equal(lines, []string{"a", "b"}) {
		t.Fatalf("strings.SplitSeq misbehaving: %q", lines)
	}
}

func TestGoModLanguageVersion(t *testing.T) {
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	var modVer string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "go "); ok {
			modVer = strings.TrimSpace(v)
			break
		}
	}
	if modVer == "" {
		t.Fatal("go.mod has no go directive")
	}
	maj, min := parseDotted(modVer)
	if maj != 1 || min < 25 {
		t.Fatalf("go.mod requires go >= 1.25 for wg.Go/SplitSeq (got %s)", modVer)
	}
	tver := strings.TrimPrefix(runtime.Version(), "go")
	tmaj, tmin := parseDotted(tver)
	if tmaj != maj || tmin < min {
		t.Fatalf("toolchain %s older than go.mod %s", runtime.Version(), modVer)
	}
}

func parseDotted(v string) (int, int) {
	parts := strings.SplitN(v, ".", 3)
	maj, _ := strconv.Atoi(leadingDigits(parts[0]))
	mn := 0
	if len(parts) > 1 {
		mn, _ = strconv.Atoi(leadingDigits(parts[1]))
	}
	return maj, mn
}

func leadingDigits(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

// TestCrossCompileTargets compiles the production package for every shipping
// target (Windows + Linux amd64/arm64, matching the README multi-platform
// claim and the Dockerfile). Catches build-tag mistakes in proc_unix/windows.
func TestCrossCompileTargets(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	targets := []struct{ goos, goarch string }{
		{"windows", "amd64"},
		{"linux", "amd64"},
		{"linux", "arm64"},
	}
	for _, tc := range targets {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "v2proxy")
			if tc.goos == "windows" {
				out += ".exe"
			}
			cmd := exec.Command(goBin, "build", "-o", out, ".")
			cmd.Env = append(os.Environ(),
				"GOOS="+tc.goos,
				"GOARCH="+tc.goarch,
				"CGO_ENABLED=0",
			)
			if raw, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("compile %s/%s: %v\n%s", tc.goos, tc.goarch, err, raw)
			}
			if st, err := os.Stat(out); err != nil || st.Size() == 0 {
				t.Fatalf("compile %s/%s produced no binary", tc.goos, tc.goarch)
			}
		})
	}
}
