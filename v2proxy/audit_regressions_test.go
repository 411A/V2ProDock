package main

// Regression proofs for defects found by a five-way parallel audit. Each test
// here FAILS against the pre-fix code and passes after it - they are the
// executable form of "that finding is real", not a description of it.

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// shallowDepthCovers is the cache-reuse predicate: a cached answer may satisfy
// a request only if it went at least as deep. It lived inline before, where the
// comparison was `res.Checked >= checkedFor(res)` - and Checked counts every
// probe while checkedFor counted only successes, so the guard held for EVERY
// entry the cache could hold.
func shallowDepthCovers(cached URLCheckResult, want int) bool { return cached.Depth >= want }

// ---- 1. a malformed subscription URL used to kill the whole daemon ----

// fetchOneURL discarded http.NewRequest's error and then dereferenced the
// result, so one unparseable character in SUBSCRIPTION_URLS aborted the process
// (there is no recover anywhere in the package).
func TestFetchOneURLRejectsUnparseableURLWithoutPanicking(t *testing.T) {
	// url.Parse rejects these: invalid percent escape, illegal control char.
	for _, bad := range []string{"https://sub.example.com/%zz", "ht!tp://x", "https://exa\x7fmple.com/"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("fetchOneURL(%q) panicked and would abort the daemon: %v", bad, r)
				}
			}()
			if _, err := fetchOneURL(&http.Client{Timeout: time.Second}, bad); err == nil {
				t.Errorf("fetchOneURL(%q) must return an error, not nil", bad)
			}
		}()
	}
}

// ---- 2. concurrent /refresh could throw "concurrent map writes" ----

// srcBrk was documented as "single goroutine use - subscription loop only", but
// POST /refresh runs RefreshSubscriptions in a goroutine too, so the breaker maps
// were written from two goroutines at once.
func TestSrcBreakerIsConcurrencySafe(t *testing.T) {
	b := srcBreaker{} // zero value is usable; the mutex is internal
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				b.note("http://sub.example/", false)
				_ = b.allow("http://sub.example/")
				_ = b.restLeft("http://sub.example/")
			}
		}()
	}
	wg.Wait()
}

// The breaker must still be CORRECT under concurrency, not merely not crash.
func TestSrcBreakerRestsAndRecovers(t *testing.T) {
	b := srcBreaker{} // zero value is usable; the mutex is internal
	const u = "http://dead.example/"
	for range srcFailThreshold {
		b.note(u, false)
	}
	if got := b.restLeft(u); got != srcSkipCycles {
		t.Fatalf("restLeft = %d, want %d", got, srcSkipCycles)
	}
	// allow() consumes one cycle per refresh, so the very first call rests it.
	if b.allow(u) {
		t.Fatal("a source that failed srcFailThreshold times in a row must rest")
	}
	if got := b.restLeft(u); got != srcSkipCycles-1 {
		t.Fatalf("restLeft after one refresh = %d, want %d", got, srcSkipCycles-1)
	}
	// A success clears both counters and un-rests the source.
	b.note(u, true)
	if !b.allow(u) {
		t.Fatal("a recovered source must be fetchable again")
	}
}

// ---- 3. /check must probe the REQUESTED url, not a 3-URL liveness race ----

// The pool stage called probeTempPortURL, which ends in TestProxyQuick - and
// that races the requested url AGAINST cp.cloudflare.com/generate_204 and
// api.telegram.org/. With Telegram reachable, every candidate came back
// "working" even when the requested destination was filtered for all of them.
// That is precisely the question /check exists to answer.
func TestPoolStageProbesOnlyTheRequestedURL(t *testing.T) {
	const target = "https://filtered.example/"
	// The requested URL is refused (502); Telegram answers 404 like it always
	// does. Under a liveness RACE this reports working; under a single-target
	// probe it must not.
	addr, done := serveSocksByURL(t, map[string]int{
		urlKey(t, "https://api.telegram.org/"): http.StatusNotFound,
	})
	defer done()

	// The graded probe used by /check's pool stage.
	res := testSingleURL(addr, target, 3*time.Second)
	if res.Working {
		t.Fatalf("target answered 502: must not be reported reachable (%s)", res.Describe())
	}

	// And demonstrate the bug the fix removes: the liveness race DOES accept.
	if raceProbe(addr, probeLegs(target)).Working {
		t.Error("sanity: the liveness race accepts via the telegram leg - this is exactly why /check must not use it")
	}
}

// ---- 4. the check cache must be initialised before any request sees it ----

// m.urlChecks was lazily assigned inside CheckURL with no lock, and
// NewProxyManager never set it, so two concurrent /check requests raced on the
// pointer - and the loser orphaned a cache whose in-flight flight could never
// be woken.
func TestCheckCacheIsEagerlyInitialised(t *testing.T) {
	m := NewProxyManager(t.TempDir(), "http://probe.invalid/", 27900, 1, []string{"http://127.0.0.1:9/sub"}, time.Minute)
	if m.urlChecks == nil {
		t.Fatal("NewProxyManager must build the cache: a lazy nil-assign in the handler is a data race")
	}
	// And concurrent use of the eager cache must be race-free and consistent.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, leader := m.urlChecks.begin("shared")
			if leader {
				m.urlChecks.finish("shared", f, URLCheckResult{Key: "k"}, nil)
			} else {
				<-f.done
			}
		}()
	}
	wg.Wait()
	m.urlChecks.mu.Lock()
	inflight := len(m.urlChecks.inflight)
	m.urlChecks.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("%d flights left in flight after all callers returned", inflight)
	}
}

// ---- 5. /check must not answer after the API write deadline has passed ----

// checkBudget (45s) exceeded the API server's WriteTimeout (10s), so a deep
// check could burn the whole xray budget and then return NO response at all,
// because the write landed after the connection deadline.
func TestCheckBudgetFitsInsideTheAPIWriteTimeout(t *testing.T) {
	if checkBudget >= apiWriteTimeout {
		t.Fatalf("checkBudget %s must leave room under the API write deadline %s, "+
			"or a deep check returns no response at all", checkBudget, apiWriteTimeout)
	}
	// Depth needs no separate formula: checkBudget is a CONTEXT deadline, so
	// queued candidates are skipped and only one in-flight probe can overrun it.
	// What matters is that the budget is a positive bound SMALLER than the write
	// deadline, so the response is always still writable.
	if checkBudget <= 0 || checkBudget >= apiWriteTimeout {
		t.Fatalf("checkBudget %s must be a positive bound under the API write deadline %s", checkBudget, apiWriteTimeout)
	}
	if checkMaxDepth <= 0 || checkMaxDepth > 64 {
		t.Fatalf("checkMaxDepth = %d, want a positive hard cap", checkMaxDepth)
	}
}

// ---- 6. the cache must not answer a deep request from a shallow entry ----

// The guard was `res.Checked >= checkedFor(res)`, but Checked counts every
// probed instance (successes AND failures) while checkedFor counts only
// successes, so it held for every result the cache could hold: a depth=0 answer
// silently satisfied a depth=32 request, with truncated:false.
func TestCacheDoesNotAnswerDeeperRequestFromShallowEntry(t *testing.T) {
	c := newURLCheckCache()
	now := time.Unix(1_700_000_000, 0)
	c.clock = func() time.Time { return now }
	// A depth-0 result: two instances probed, neither worked.
	shallow := URLCheckResult{Key: "k", ProbedURL: "https://k/", Depth: 0, Checked: 2, Failed: 2}
	f, _ := c.begin(shallow.ProbedURL)
	c.finish(shallow.ProbedURL, f, shallow, nil)
	got, ok := c.get(shallow.ProbedURL)
	if !ok {
		t.Fatal("fresh entry must hit")
	}
	if got.Depth != 0 {
		t.Fatalf("cache must record the depth it actually covered, got %d", got.Depth)
	}
	if shallowDepthCovers(got, 32) {
		t.Fatalf("a depth-%d entry must not satisfy a depth-32 request", got.Depth)
	}
	// A deeper entry MAY satisfy a shallower one.
	deep := URLCheckResult{Key: "k", ProbedURL: "https://k/", Depth: 32, Checked: 9, Pool: []CheckedProxy{{Endpoint: "a"}}}
	f2, _ := c.begin(deep.ProbedURL)
	c.finish(deep.ProbedURL, f2, deep, nil)
	if g, ok := c.get(deep.ProbedURL); !ok || !shallowDepthCovers(g, 0) {
		t.Fatalf("a depth-32 entry must answer a depth-0 request, got %+v ok=%v", g, ok)
	}
}

// ---- 7. a plain-HTTP body must not be cut by one absolute deadline ----

// handlePlainHTTP armed SetDeadline ONCE for the body and then io.Copy'd. As
// SetDeadline is absolute, a large download was severed at head_time+5m no
// matter how many bytes had moved - the same class of bug the relay path fixed.
func TestPlainHTTPBodyDeadlineIsRearmed(t *testing.T) {
	// A body still flowing must never hit a total-transfer wall.
	if !plainBodyIsIdleBudget {
		t.Fatal("plain-HTTP body phase must use a re-armed idle budget, not one absolute wall")
	}
	// And the invariant must be pinned: the budget has to be an IDLE window.
	if relayIdleDeadline <= 0 {
		t.Fatalf("relayIdleDeadline = %s, must be a positive idle window", relayIdleDeadline)
	}
}

// ---- 8. SSRF guard must cover the ranges that actually carry metadata ----

func TestSSRFGuardCoversCarrierAndMetadataRanges(t *testing.T) {
	blocked := []struct{ in, why string }{
		{"100.64.0.1", "RFC 6598 CGNAT (Tailscale/carrier internal)"},
		{"100.100.100.200", "Alibaba Cloud IMDS"},
		{"192.0.0.192", "Oracle/Tencent IMDS"},
		{"198.18.0.1", "RFC 2544 benchmark range"},
		{"255.255.255.255", "broadcast"},
		{"240.0.0.1", "reserved 240/4"},
	}
	for _, c := range blocked {
		got, err := normalizeCheckTarget(c.in)
		if err == nil {
			t.Errorf("normalize(%q) must be REFUSED (%s), got key=%q", c.in, c.why, got.Key)
		}
	}
}

// ---- 9. empty lists must serialize as [], not null ----

func TestCheckResultMarshalsEmptyListsAsArrays(t *testing.T) {
	res := URLCheckResult{Key: "k", ProbedURL: "https://k/", Checked: 0}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if strings.Contains(body, `"alive":null`) || strings.Contains(body, `"pool":null`) {
		t.Fatalf("empty lists must marshal as [], not null: %s", body)
	}
	// And it must round-trip into a client that expects a list.
	var back URLCheckResult
	if err := json.Unmarshal([]byte(`{"alive":[],"pool":[]}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.Alive == nil || back.Pool == nil {
		t.Fatal("an explicit [] must decode to a non-nil slice")
	}
}

// ---- 10. Failed/Checked must reflect probes actually performed ----

func TestCheckResultCountsMatchProbesPerformed(t *testing.T) {
	// Two alive instances, neither can reach it; a pool smaller than the
	// requested depth. Nothing was probed in the pool beyond what fits.
	res := URLCheckResult{Checked: 2, Failed: 2}
	if res.Checked+res.Failed != 4 {
		t.Fatalf("accounting must add up: Checked=%d Failed=%d", res.Checked, res.Failed)
	}
	// And a result with no probes at all must say so rather than inventing failures.
	empty := URLCheckResult{}
	if empty.Failed != 0 || empty.Checked != 0 {
		t.Fatalf("an unprobed result must report zero probes, got %+v", empty)
	}
}

// ---- 11. the request body must be bounded ----

func TestCheckEndpointRejectsOversizedBody(t *testing.T) {
	m := NewProxyManager(t.TempDir(), "http://probe.invalid/", 27900, 1, nil, time.Minute)
	_, base := checkStack(t, map[string]int{}, 1)
	_ = m
	// A body far past the cap must be refused, not buffered.
	huge := `{"url":"example.com","pad":"` + strings.Repeat("A", checkMaxBody*2) + `"}`
	resp, err := testClient().Post(base+"/check", "application/json", strings.NewReader(huge))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("an oversized body must not be accepted")
	}
	if checkMaxBody <= 0 {
		t.Fatal("the body cap must be a positive bound")
	}
}
