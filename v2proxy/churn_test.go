package main

// Regression tests for the 2026-09-28 production incident. That log showed
// three compounding failures at once:
//  1. every refresh evicted healthy actives ("active config vanished from
//     pool, rotating...") because the remote list rotates — forced switches
//     kept streaks at 0 and flapped the API's fastest entry;
//  2. one dead LAN subscription host burned full retry budgets on EVERY
//     refresh (~fetchAttempts x fetchTimeout + backoffs) while contributing
//     nothing, plus a WARN each cycle;
//  3. the orphan watchdog reaped in-flight throwaway switch probes
//     (config-rot-*.json matches the orphan pattern, but only serving PIDs
//     were tracked), manufacturing "no working config found" storms.
//
// All hermetic: httptest servers + closed loopback ports. No internet, no
// xray binary (missing binary fails launches instantly and deterministically).

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	churnVlessA = "vless://11111111-1111-4111-8111-111111111111@10.9.9.1:443?security=tls&sni=example.com&type=tcp#ChurnA"
	churnVlessB = "vless://22222222-2222-4222-8222-222222222222@10.9.9.2:443?security=tls&sni=example.com&type=tcp#ChurnB"
	churnVlessC = "vless://33333333-3333-4333-8333-333333333333@10.9.9.3:443?security=tls&sni=example.com&type=tcp#ChurnC"
)

func mustParseChurn(t *testing.T, raw string) ProxyConfig {
	t.Helper()
	c, err := parseToXrayConfig(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return *c
}

func churnPoolServer(t *testing.T, lines ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, strings.Join(lines, "\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// junkPoolServer returns 200 with an unparsable body: 0 proxies, fast, and
// — critically — no loopback-fallback roulette (a fetched body never tries
// fallbacks, unlike a refused connection, whose fallback candidates can
// blackhole for full fetchTimeouts on some networks). It models every
// "source contributed nothing" case for the failed-marking path.
func junkPoolServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, "this is not a proxy line")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---- orphan victim selection: temp probes are managed, not orphans ----

func TestOrphanVictimsExemptTempProbes(t *testing.T) {
	found := map[int]bool{101: true, 102: true, 103: true}
	keep := map[int]bool{101: true, 102: true} // serving + in-flight probe
	victims := orphanVictims(found, keep)
	if len(victims) != 1 || !slices.Contains(victims, 103) {
		t.Fatalf("only the true stray must be a victim, got %v", victims)
	}
}

func TestTempPIDTracking(t *testing.T) {
	s := &ProxySelector{}
	s.trackTemp(11)
	s.trackTemp(22)
	s.trackTemp(0) // never a real pid: ignored
	snap := s.tempPIDSnapshot()
	if len(snap) != 2 || !snap[11] || !snap[22] {
		t.Fatalf("tracked pids must snapshot, got %v", snap)
	}
	snap[11] = false // snapshots are copies: mutating must not leak back
	if !s.tempPIDSnapshot()[11] {
		t.Fatal("snapshot must be a copy")
	}
	s.untrackTemp(11)
	snap = s.tempPIDSnapshot()
	if len(snap) != 1 || !snap[22] {
		t.Fatalf("untrack must remove, got %v", snap)
	}
}

func TestManagedPIDsIncludesTemp(t *testing.T) {
	s := &ProxySelector{}
	m := &ProxyManager{
		instances: []*ProxySelector{s},
		statuses:  []InstanceStatus{{Index: 0, Status: "ok"}},
	}
	s.trackTemp(4242)
	if !m.managedPIDs()[4242] {
		t.Fatal("in-flight probe pid must be in the watchdog keep set")
	}
	s.untrackTemp(4242)
	if m.managedPIDs()[4242] {
		t.Fatal("finished probe pid must leave the keep set")
	}
}

// ---- dead-source circuit breaker ----

func TestSrcBreakerTransitions(t *testing.T) {
	b := &srcBreaker{}
	dead := "http://192.0.2.9/sub.txt"
	if !b.allow(dead) {
		t.Fatal("fresh source must be allowed")
	}
	b.note(dead, false)
	b.note(dead, false)
	if !b.allow(dead) {
		t.Fatal("below threshold must still be allowed")
	}
	b.note(dead, false) // third strike: rests
	if b.restLeft(dead) != srcSkipCycles {
		t.Fatalf("must rest %d cycles, got %d", srcSkipCycles, b.restLeft(dead))
	}
	for i := range srcSkipCycles {
		if b.allow(dead) {
			t.Fatalf("must skip while resting (cycle %d)", i)
		}
	}
	if !b.allow(dead) {
		t.Fatal("rest exhausted: must probe recovery")
	}
	b.note(dead, true)
	if b.restLeft(dead) != 0 {
		t.Fatal("success must clear the rest")
	}
	if !b.allow(dead) {
		t.Fatal("recovered source must be allowed")
	}
	// Re-arms on fresh consecutive failures.
	b.note(dead, false)
	b.note(dead, false)
	b.note(dead, false)
	if b.restLeft(dead) != srcSkipCycles {
		t.Fatal("breaker must re-arm after recovery + fresh failures")
	}
}

func TestFetchMergedReportMarksFailed(t *testing.T) {
	good := churnPoolServer(t, churnVlessB, churnVlessC)
	dead := junkPoolServer(t)
	merged, failed := fetchMergedReport([]string{dead.URL, good.URL})
	if len(merged) != 2 {
		t.Fatalf("good source must contribute 2 configs, got %d", len(merged))
	}
	if !failed[dead.URL] {
		t.Fatal("0-proxy source must be reported failed")
	}
	if failed[good.URL] {
		t.Fatal("good source must not be reported failed")
	}
}

func TestRefreshSkipsRestingSource(t *testing.T) {
	good := churnPoolServer(t, churnVlessB)
	// Never fetched while resting, so any syntactically-valid URL stands in
	// for the dead production host here (proves the skip, not the failure).
	dead := "http://192.0.2.9/sub.txt"
	dir := t.TempDir()
	sel := NewProxySelector(dir, "http://probe.invalid/", 0, 0, time.Minute)
	a := mustParseChurn(t, churnVlessA)
	sel.UpdateConfigs([]ProxyConfig{a})
	sel.activeIndex = 0 // serving A, never failed it
	m := &ProxyManager{
		instances: []*ProxySelector{sel},
		statuses:  []InstanceStatus{{Index: 0, Status: "ok"}},
		subURLs:   []string{dead, good.URL},
		xrayDir:   dir,
	}
	// Pre-armed breaker: dead must not be fetched at all this refresh.
	m.srcBrk.fails = map[string]int{dead: srcFailThreshold}
	m.srcBrk.skip = map[string]int{dead: srcSkipCycles}

	m.RefreshSubscriptions()

	if got := m.srcBrk.restLeft(dead); got != srcSkipCycles-1 {
		t.Fatalf("rest countdown must tick (proves skip, no fetch), got %d", got)
	}
	if got := sel.ActiveConfig(); got == nil || got.Key() != a.Key() {
		t.Fatalf("healthy active must survive the refresh, got %+v", got)
	}
	foundB := false
	for _, c := range sel.snapshotConfigs() {
		if strings.Contains(c.Name, "ChurnB") {
			foundB = true
		}
	}
	if !foundB {
		t.Fatal("good source pool must still be applied")
	}
}

// ---- refresh retains healthy-but-vanished actives, rotates failing ones ----

func TestRefreshRetainsHealthyVanishedActive(t *testing.T) {
	good := churnPoolServer(t, churnVlessB)
	dir := t.TempDir()
	sel := NewProxySelector(dir, "http://probe.invalid/", 0, 0, time.Minute)
	a := mustParseChurn(t, churnVlessA)
	sel.UpdateConfigs([]ProxyConfig{a})
	sel.activeIndex = 0 // serving A, failCount 0 = never failed
	m := &ProxyManager{
		instances: []*ProxySelector{sel},
		statuses:  []InstanceStatus{{Index: 0, Status: "ok"}},
		subURLs:   []string{good.URL},
		xrayDir:   dir,
	}

	m.RefreshSubscriptions()

	got := sel.ActiveConfig()
	if got == nil || got.Key() != a.Key() {
		t.Fatalf("healthy vanished active must be retained, got %+v", got)
	}
	if st := m.statusOf(0); st.Status != "ok" {
		t.Fatalf("retained instance must stay ok, got %q (%s)", st.Status, st.Error)
	}
}

func TestRefreshRotatesFailedVanishedActive(t *testing.T) {
	good := churnPoolServer(t, churnVlessB)
	dir := t.TempDir()
	sel := NewProxySelector(dir, "http://probe.invalid/", 0, 0, time.Minute)
	a := mustParseChurn(t, churnVlessA)
	sel.UpdateConfigs([]ProxyConfig{a})
	sel.activeIndex = 0
	sel.failCount = 2 // actually failing: rotation path, as before
	m := &ProxyManager{
		instances: []*ProxySelector{sel},
		statuses:  []InstanceStatus{{Index: 0, Status: "ok"}},
		subURLs:   []string{good.URL},
		xrayDir:   dir, // no xray binary: switch fails fast and deterministically
	}

	m.RefreshSubscriptions()

	if st := m.statusOf(0); st.Status == "ok" {
		t.Fatal("failing vanished active must rotate, not stay ok")
	} else if !strings.Contains(st.Error, "no working config") {
		t.Fatalf("rotation must be attempted (switch error), got %q", st.Error)
	}
}

func TestRetainActiveUnit(t *testing.T) {
	s := &ProxySelector{}
	a := mustParseChurn(t, churnVlessA)
	b := mustParseChurn(t, churnVlessB)
	s.UpdateConfigs([]ProxyConfig{b})
	s.activeIndex = -1
	s.RetainActive(a)
	if got := s.ActiveConfig(); got == nil || got.Key() != a.Key() {
		t.Fatalf("retained config must become active, got %+v", got)
	}
	if len(s.snapshotConfigs()) != 2 {
		t.Fatal("pool must grow by the retained config")
	}
	// Re-anchor case: key already present, no duplicate.
	s.UpdateConfigs([]ProxyConfig{b, a})
	s.activeIndex = -1
	s.RetainActive(a)
	if got := s.ActiveConfig(); got == nil || got.Key() != a.Key() {
		t.Fatalf("must re-anchor to present key, got %+v", got)
	}
	if len(s.snapshotConfigs()) != 2 {
		t.Fatal("present key must not duplicate")
	}
}
