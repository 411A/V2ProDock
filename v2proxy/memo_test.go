package main

// Quality-memo tests: a node that is alive but known to truncate large
// transfers must stay servable as a last resort, carry the warning, and never
// outrank a clean node.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseProxyMemos(t *testing.T) {
	got := parseProxyMemos(" Fast.Example:443 = truncates >12KB; downloads fail ,slow.example:443=slow but stable, ,bad-entry,empty.example:443=,")
	if len(got) != 2 {
		t.Fatalf("parsed %d entries, want 2: %v", len(got), got)
	}
	if got["fast.example:443"] != "truncates >12KB; downloads fail" {
		t.Fatalf("memo = %q", got["fast.example:443"])
	}
	if got["slow.example:443"] != "slow but stable" {
		t.Fatalf("memo = %q", got["slow.example:443"])
	}
	if got := parseProxyMemos(""); got == nil || len(got) != 0 {
		t.Fatalf("empty env must yield an empty non-nil map, got %v", got)
	}
	// Later duplicate wins (last operator statement is the current truth).
	if got := parseProxyMemos("a:1=first,a:1=second"); got["a:1"] != "second" {
		t.Fatalf("duplicate = %q, want second", got["a:1"])
	}
}

func TestOrderAliveCleanBeforeDegraded(t *testing.T) {
	in := []InstanceStatus{
		{Index: 2, LatMs: 50, OkStreak: 3},
		{Index: 1, LatMs: 900, OkStreak: 3},
		{Index: 0, LatMs: 10, OkStreak: 9, Degraded: true, Memo: "truncates >12KB"},
	}
	got := orderAlive(in, 3)
	if len(got) != 3 {
		t.Fatalf("order len = %d", len(got))
	}
	if got[0].Index != 2 || got[1].Index != 1 {
		t.Fatalf("clean order = [%d %d], want [2 1] (latency, no degraded first)", got[0].Index, got[1].Index)
	}
	if got[2].Index != 0 {
		t.Fatalf("degraded must be last, got %d", got[2].Index)
	}

	// Gate off must not change the clean/degraded invariant.
	got = orderAlive(in, 0)
	if got[0].Degraded || got[1].Degraded || !got[2].Degraded {
		t.Fatalf("minStreak=0 order = %+v, want clean,clean,degraded", got)
	}

	// Only degraded nodes: they remain servable (the deliberate last resort).
	only := []InstanceStatus{
		{Index: 0, LatMs: 10, Degraded: true},
		{Index: 1, LatMs: 20, Degraded: true},
	}
	got = orderAlive(only, 3)
	if len(got) != 2 || !got[0].Degraded || !got[1].Degraded {
		t.Fatalf("only-degraded ladder = %+v, want both preserved", got)
	}
}

func TestSetMemoRanksDegradedAndExposesIt(t *testing.T) {
	m := backendManager([]InstanceStatus{
		{Index: 0, Status: "ok", LatMs: 10, OkStreak: 5, Endpoint: "fast.example:443"},
		{Index: 1, Status: "ok", LatMs: 900, OkStreak: 5, Endpoint: "clean.example:443"},
	}, 27911, 27921, 27912, 27922)
	_ = m.aggregateCandidates(false) // publish the initial ladder

	if got := m.aggregateCandidates(false); got[0] != "127.0.0.1:27911" {
		t.Fatalf("initial head = %s, want the fast node", got[0])
	}
	// Normalization is part of the public contract: /memo accepts the
	// endpoint exactly as /proxies prints it, case/space included.
	m.SetMemo(" FAST.EXAMPLE:443 ", "truncates >12KB; downloads and uploads fail")

	if got := m.aggregateCandidates(false); got[0] != "127.0.0.1:27912" {
		t.Fatalf("after memo head = %s, want the clean node (:27912)", got[0])
	}
	if n := m.DegradedCount(); n != 1 {
		t.Fatalf("DegradedCount = %d, want 1", n)
	}
	alive := m.GetAliveStatuses()
	if len(alive) != 2 || !alive[1].Degraded || alive[1].Memo == "" {
		t.Fatalf("/proxies ordering/fields wrong: %+v", alive)
	}
	if alive[0].Degraded {
		t.Fatalf("clean node reported degraded: %+v", alive[0])
	}

	// Clearing returns the fast node to the head immediately.
	m.SetMemo("fast.example:443", "")
	if got := m.aggregateCandidates(false); got[0] != "127.0.0.1:27911" {
		t.Fatalf("after clear head = %s, want the fast node", got[0])
	}
	if n := m.DegradedCount(); n != 0 {
		t.Fatalf("DegradedCount after clear = %d, want 0", n)
	}
}

func TestMarkOKAttachesMemoByEndpoint(t *testing.T) {
	m := backendManager([]InstanceStatus{{Index: 0}}, 27931, 27941)
	m.instances[0].configs = []ProxyConfig{{Name: "fast", Endpoint: "fast.example:443"}}
	m.instances[0].activeIndex = 0
	m.memos = map[string]string{"fast.example:443": "truncating: >12KB only"}

	m.markOK(0, "fast", 2*time.Millisecond)
	s := m.statusOf(0)
	if s.Endpoint != "fast.example:443" {
		t.Fatalf("endpoint = %q", s.Endpoint)
	}
	if !s.Degraded || s.Memo != "truncating: >12KB only" {
		t.Fatalf("memo not attached: %+v", s)
	}
}

func TestAPIMemoMarksFallbackOnly(t *testing.T) {
	t.Setenv("PROXY_MEMOS", "")
	m, api := startTestStack(t, 27820, 1)
	m.markOK(0, "fast", 5*time.Millisecond)
	m.mu.Lock()
	m.statuses[0].Endpoint = "fast.example:443"
	m.mu.Unlock()

	resp, err := testClient().Post(api+"/memo", "application/json",
		strings.NewReader(`{"endpoint":"fast.example:443","memo":"truncates >12KB; downloads and uploads fail"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /memo status %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp, err = testClient().Get(api + "/proxies")
	if err != nil {
		t.Fatal(err)
	}
	var alive []InstanceStatus
	if err := json.NewDecoder(resp.Body).Decode(&alive); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(alive) != 1 || alive[0].Memo == "" || !alive[0].Degraded {
		t.Fatalf("/proxies memo fields = %+v", alive)
	}

	_, health := getJSON(t, api+"/health")
	if health["degraded"] != float64(1) || health["fallback_only"] != true {
		t.Fatalf("/health = %v, want degraded=1 fallback_only=true", health)
	}

	// GET is not a state-changing verb: same mux rule as /refresh.
	resp, err = testClient().Get(api + "/memo")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /memo status %d, want 405", resp.StatusCode)
	}

	// Empty memo clears the note and the fallback warning.
	resp, err = testClient().Post(api+"/memo", "application/json",
		strings.NewReader(`{"endpoint":"fast.example:443","memo":""}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	_, health = getJSON(t, api+"/health")
	if health["degraded"] != float64(0) {
		t.Fatalf("/health after clear = %v, want degraded=0", health)
	}
	if _, ok := health["fallback_only"]; ok {
		t.Fatalf("/health still warns fallback_only after clear: %v", health)
	}
}
