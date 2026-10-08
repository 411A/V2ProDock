package main

// Policy tests: repository conventions enforced as code. These lock in the
// use-modern-go skill rules (any, slices.SortFunc, range-over-int) plus the
// Dockerfile/go.mod toolchain contract, so regressions fail tests, not review.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func repoSources(t *testing.T, includeTests bool) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	src := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// This file necessarily names the banned patterns to enforce them.
		if name == "policy_test.go" {
			continue
		}
		if !includeTests && strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		src[name] = string(raw)
	}
	if len(src) == 0 {
		t.Fatal("no sources found")
	}
	return src
}

// TestPolicyModernIdioms bans pre-modernize patterns skill-wide.
func TestPolicyModernIdioms(t *testing.T) {
	bans := []struct {
		name string
		re   *regexp.Regexp
		hint string
	}{
		{"interface{}", regexp.MustCompile(`interface\{\}`), "use any"},
		{"sort.Slice", regexp.MustCompile(`sort\.Slice`), "use slices.SortFunc with cmp.Compare"},
		// Only the pure convertible shape: loops with extra conditions
		// (e.g. deadline bounds) cannot use range-over-int and are exempt.
		{"c-style for", regexp.MustCompile(`for \w+ := 0; \w+ < \w+; \w+\+\+`), "use for i := range n"},
	}
	for _, tc := range bans {
		for file, body := range repoSources(t, true) {
			if loc := tc.re.FindStringIndex(body); loc != nil {
				line := 1 + strings.Count(body[:loc[0]], "\n")
				t.Errorf("policy: %s banned in %s:%d (%s)", tc.name, file, line, tc.hint)
			}
		}
	}
}

// TestPolicyNoPanicInProduction keeps crash behavior explicit (log + exit
// only in main paths), never a hidden panic in library code.
func TestPolicyNoPanicInProduction(t *testing.T) {
	re := regexp.MustCompile(`panic\(`)
	for file, body := range repoSources(t, false) {
		if loc := re.FindStringIndex(body); loc != nil {
			line := 1 + strings.Count(body[:loc[0]], "\n")
			t.Errorf("policy: panic( banned in production code %s:%d", file, line)
		}
	}
}

// TestPolicyToolchainContract pins Dockerfile to go.mod: the builder image
// minor must equal the module language version, and GOTOOLCHAIN=local keeps
// builds reproducible (no silent toolchain upgrades inside Docker).
func TestPolicyToolchainContract(t *testing.T) {
	gomod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	var modVer string
	for line := range strings.SplitSeq(string(gomod), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "go "); ok {
			modVer = strings.TrimSpace(v)
			break
		}
	}
	maj, min := parseDotted(modVer)

	docker, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	imageLine := ""
	for line := range strings.SplitSeq(string(docker), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FROM golang:") {
			imageLine = strings.TrimSpace(line)
		}
	}
	if imageLine == "" {
		t.Fatal("Dockerfile has no golang builder image")
	}
	// Compare minor versions: golang:1.25-alpine vs go 1.25.0.
	ver := strings.TrimPrefix(strings.Fields(imageLine)[1], "golang:")
	dMaj, dMin := parseDotted(strings.SplitN(ver, "-", 2)[0])
	if dMaj != maj || dMin != min {
		t.Fatalf("Dockerfile %q mismatches go.mod %q", imageLine, modVer)
	}
	if !strings.Contains(string(docker), "GOTOOLCHAIN=local") {
		t.Error("Dockerfile must set GOTOOLCHAIN=local for reproducible builds")
	}
}

// TestPolicyLintGate keeps the strict lint gate from eroding. golangci-lint v2
// dropped v1's implicit default exclusions, which is how this repo drifted to
// 33 findings (31 unchecked Close/Remove) while AGENT.md still claimed EXIT 0.
// The config is therefore part of the contract, not a local preference.
func TestPolicyLintGate(t *testing.T) {
	raw, err := os.ReadFile(".golangci.yml")
	if err != nil {
		t.Fatalf("the strict lint gate requires a committed .golangci.yml: %v", err)
	}
	cfg := string(raw)

	// errcheck was where the drift accumulated; it must stay on, and tests must
	// stay in scope (every unchecked Close lived in a _test.go file).
	for _, want := range []string{"errcheck", "tests: true"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("policy: .golangci.yml must contain %q", want)
		}
	}
	if strings.Contains(cfg, "default: none") {
		t.Error("policy: .golangci.yml must not disable the standard linter set")
	}
	// The production-only correctness tier beyond the standard set.
	for _, want := range []string{"bodyclose", "errorlint", "gocritic", "revive", "misspell", "unconvert"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("policy: .golangci.yml should enable %q", want)
		}
	}
	// stylecheck (ST*) is where the naming rule lives; it must run beyond the
	// default subset, or an acronym like defaultAggHttpPort slips through.
	if !strings.Contains(cfg, "- -ST1000") || !strings.Contains(cfg, "all") {
		t.Error("policy: .golangci.yml must run staticcheck's full check set minus the doc-comment rules")
	}
	// Vendored code must stay out of the gate.
	if !strings.Contains(cfg, "vendor/") {
		t.Error("policy: .golangci.yml must exclude vendor/")
	}
}
func TestPolicyAPIRoutes(t *testing.T) {
	raw, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/proxies", "/all", "/health", "/vpn", "/refresh"} {
		if !strings.Contains(string(raw), route) {
			t.Errorf("policy: API route %s not registered in api.go", route)
		}
	}
	// /refresh must stay POST-only via a method-aware pattern.
	if !strings.Contains(string(raw), `"POST /refresh"`) {
		t.Error("policy: /refresh must use the method-aware \"POST /refresh\" pattern")
	}
}
