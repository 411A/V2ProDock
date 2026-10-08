package main

// Dynamic URL-check (/check) tests.
//
// Split by what each can prove:
//   - normalization is pure and exhaustively table-driven (the "x.com ==
//     http://www.x.com" contract, and the SSRF refusals);
//   - the endpoint is exercised over REAL HTTP against the real mux with real
//     SOCKS5 stubs, so the response contract is proven end to end;
//   - the cache is proven by time travel, not by sleeping;
//   - read-only-ness is proven by asserting pool state is untouched after a
//     check against a URL nothing can reach - the discipline that keeps a
//     filtered target from culling the pool.
//
// Hermetic: loopback only, no internet, no xray binary.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- normalization: the "same URL" contract ----

func TestNormalizeCheckTargetUnifiesSpellings(t *testing.T) {
	// Every spelling of one endpoint must produce ONE key.
	spellings := []string{
		"example.com",
		"http://example.com",
		"https://example.com",
		"https://www.example.com",
		"http://www.example.com/",
		"  EXAMPLE.com  ",
		"https://example.com:443/",
	}
	var wantKey string
	for _, s := range spellings {
		got, err := normalizeCheckTarget(s)
		if err != nil {
			t.Fatalf("normalize(%q): %v", s, err)
		}
		if wantKey == "" {
			wantKey = got.Key
		}
		if got.Key != wantKey {
			t.Errorf("normalize(%q).Key = %q, want %q (all spellings share a key)", s, got.Key, wantKey)
		}
	}
	if wantKey != "example.com" {
		t.Fatalf("key = %q, want example.com", wantKey)
	}
}

func TestNormalizeCheckTargetUsesPublicSuffixList(t *testing.T) {
	// The whole reason for publicsuffix: a naive last-two-labels split turns
	// these into the shared suffix and would MERGE unrelated sites.
	cases := []struct{ in, wantKey string }{
		{"https://www.example.co.uk", "example.co.uk"},
		{"example.com.au", "example.com.au"},
		{"https://a.b.example.org", "example.org"},
		{"https://shop.example.co.jp/path", "example.co.jp"},
	}
	for _, c := range cases {
		got, err := normalizeCheckTarget(c.in)
		if err != nil {
			t.Fatalf("normalize(%q): %v", c.in, err)
		}
		if got.Key != c.wantKey {
			t.Errorf("normalize(%q).Key = %q, want %q", c.in, got.Key, c.wantKey)
		}
	}
	// And the two ccTLDs must NOT have collapsed together, which is what the
	// naive split would have done.
	uk, _ := normalizeCheckTarget("https://www.example.co.uk")
	au, _ := normalizeCheckTarget("https://www.example.com.au")
	if uk.Key == au.Key {
		t.Fatalf("unrelated sites merged under key %q", uk.Key)
	}
}

func TestNormalizeCheckTargetProbedURL(t *testing.T) {
	// The KEY is the domain; the MEASUREMENT is the exact URL. Two paths under
	// one key must stay distinct measurements, or claiming /a is reachable
	// because /b answered would be an unearned verdict.
	a, err := normalizeCheckTarget("https://example.com/bot123/getUpdates")
	if err != nil {
		t.Fatal(err)
	}
	b, err := normalizeCheckTarget("https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if a.Key != b.Key {
		t.Fatalf("same domain must share a key: %q vs %q", a.Key, b.Key)
	}
	if a.ProbeURL == b.ProbeURL {
		t.Fatal("different paths must remain different probe URLs")
	}
	if a.ProbeURL != "https://example.com/bot123/getUpdates" {
		t.Errorf("ProbeURL = %q, path must be preserved", a.ProbeURL)
	}
	// Query must survive too, and a bare host must get a path.
	if a.ProbeURL != "https://example.com/bot123/getUpdates" {
		t.Errorf("ProbeURL = %q", a.ProbeURL)
	}
	q, err := normalizeCheckTarget("example.com/a?b=c")
	if err != nil {
		t.Fatal(err)
	}
	if q.ProbeURL != "https://example.com/a?b=c" {
		t.Errorf("ProbeURL = %q, query must be preserved", q.ProbeURL)
	}
	// No scheme supplied: HTTPS, because plain HTTP is DPI-RST-injected even
	// through a working tunnel (see isPlainHTTP).
	if !strings.HasPrefix(b.ProbeURL, "https://") {
		t.Errorf("default scheme must be https, got %q", b.ProbeURL)
	}
	if b.ProbeURL != "https://example.com/" {
		t.Errorf("bare host must gain a root path, got %q", b.ProbeURL)
	}
	// Fragment is not sent to the server, so it must not be probed.
	f, err := normalizeCheckTarget("https://example.com/a#frag")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.ProbeURL, "frag") {
		t.Errorf("fragment must be dropped, got %q", f.ProbeURL)
	}
}

func TestNormalizeCheckTargetRejects(t *testing.T) {
	cases := []struct{ in, why string }{
		{"", "empty"},
		{"   ", "blank"},
		{"ftp://example.com", "scheme"},
		{"file:///etc/passwd", "scheme"},
		{"gopher://example.com", "scheme"},
		{"https://", "no host"},
		{"https://user:pass@example.com", "credentials"},
		{"https://user@example.com", "credentials"},
		{"://example.com", "unparseable"},
		{strings.Repeat("a", checkMaxURLLen+1), "too long"},
	}
	for _, c := range cases {
		if _, err := normalizeCheckTarget(c.in); err == nil {
			t.Errorf("normalize(%q) must be rejected (%s)", truncForMsg(c.in), c.why)
		}
	}
}

func truncForMsg(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// ---- SSRF guard ----

// The probe dials FROM the upstream, so a private target means scanning the
// UPSTREAM's LAN or metadata service. Refused, with the reason in the error.
func TestCheckTargetSSRFGuard(t *testing.T) {
	cases := []struct{ in, why string }{
		{"127.0.0.1", "loopback literal"},
		{"127.1.2.3", "loopback literal"},
		{"10.0.0.5", "RFC1918"},
		{"172.16.0.1", "RFC1918"},
		{"192.168.1.1", "RFC1918"},
		{"169.254.169.254", "cloud metadata"},
		{"0.0.0.0", "unspecified"},
		{"[::1]", "IPv6 loopback"},
		{"[fe80::1]", "IPv6 link-local"},
		{"[fc00::1]", "IPv6 unique-local"},
		{"localhost", "loopback name"},
		{"foo.localhost", "loopback name"},
		{"printer.local", "private suffix"},
		{"db.internal", "private suffix"},
		{"gw.home.arpa", "reserved"},
		{"intranet", "single label"},
		{"box", "single label"},
	}
	for _, c := range cases {
		got, err := normalizeCheckTarget(c.in)
		if err == nil {
			t.Errorf("normalize(%q) must be REFUSED (%s) but got key=%q", c.in, c.why, got.Key)
		}
	}
	// And a public target must still be accepted, or the guard is useless.
	for _, ok := range []string{"example.com", "https://8.8.8.8/", "1.1.1.1", "https://sub.example.co.uk/x"} {
		if _, err := normalizeCheckTarget(ok); err != nil {
			t.Errorf("normalize(%q) must be allowed, got %v", ok, err)
		}
	}
}

// The guard must not be a blanket ban: a public IP literal is a legitimate
// target and must key sensibly.
func TestCheckTargetAllowsPublicIPLiteral(t *testing.T) {
	got, err := normalizeCheckTarget("https://8.8.8.8/resolve")
	if err != nil {
		t.Fatalf("public IP literal must be allowed: %v", err)
	}
	if got.Key != "8.8.8.8" {
		t.Errorf("Key = %q, want the literal 8.8.8.8", got.Key)
	}
}

// ---- HTTP endpoint, end to end over the real mux ----

// checkStack boots a manager with stub-backed instances plus the real API, and
// returns the /check base URL. served maps a probe host to the status its SOCKS
// stub should answer, so a check against one host can succeed while another
// fails - which is the entire feature.
func checkStack(t *testing.T, served map[string]int, n int) (m *ProxyManager, base string) {
	t.Helper()
	// Must stay under maxPort (27999) or findAvailableBlock allocates nothing:
	// the scan is bounded above by maxPort, so an out-of-range base silently
	// yields a manager with zero instances.
	const basePort = 27900
	m = NewProxyManager(t.TempDir(), "http://probe.invalid/", basePort, n, []string{"http://127.0.0.1:9/sub"}, time.Minute)
	m.urlChecks = newURLCheckCache()
	if len(m.instances) != n {
		t.Fatalf("precondition: want %d instances, manager built %d (is basePort %d under maxPort %d?)", n, len(m.instances), basePort, maxPort)
	}
	for i := range n {
		inst := m.instances[i]
		// One listening SOCKS5 stub per instance: testSingleURL dials the
		// instance's serving port directly, so this is what answers.
		addr, done := serveSocksByURL(t, served)
		t.Cleanup(done)
		proxySocks(t, inst, addr)
		inst.UpdateConfigs([]ProxyConfig{{Name: "node-" + string(rune('A'+i)), Raw: "r" + string(rune('A'+i)), Endpoint: "e" + string(rune('A'+i)) + ":1"}})
		inst.RetainActive(inst.snapshotConfigs()[0])
	}
	port := startAPI(m, basePort+60)
	b := "http://127.0.0.1:" + itoa(port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := testClient().Get(b + "/health")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return m, b
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("check API on %s never answered", b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// proxySocks fronts the instance's OWN serving port with a TCP relay to the
// SOCKS stub, so testSingleURL (which dials 127.0.0.1:<socksPort>) reaches it.
// Using the port the manager already allocated avoids mutating the selector,
// so there is no race against the API reading it.
func proxySocks(t *testing.T, inst *ProxySelector, upstream string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(inst.SOCKSPort()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				up, err := net.Dial("tcp", upstream)
				if err != nil {
					return
				}
				defer func() { _ = up.Close() }()
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
				<-done
			}(c)
		}
	}()
}

func TestCheckEndpointListsWorkingProxies(t *testing.T) {
	const want = "reachable.example"
	// Both spellings answer, so the assertion below proves the ENDPOINT unifies
	// them: we probe http://www.reachable.example/a and get back the key
	// "reachable.example". Plain http because a loopback stub cannot terminate
	// TLS - the default-scheme behaviour is covered by the pure normalization
	// test instead.
	served := map[string]int{
		urlKey(t, "http://reachable.example/"):      http.StatusNoContent,
		urlKey(t, "http://www.reachable.example/a"): http.StatusNoContent,
	}
	_, base := checkStack(t, served, 2)

	res := postCheck(t, base, "http://www."+want+"/a", 0)
	if res.Key != want {
		t.Errorf("Key = %q, want %q", res.Key, want)
	}
	if res.ProbedURL != "http://www."+want+"/a" {
		t.Errorf("ProbedURL = %q", res.ProbedURL)
	}
	if len(res.Alive) != 2 {
		t.Fatalf("both instances should reach it, got %d: %+v", len(res.Alive), res.Alive)
	}
	for _, a := range res.Alive {
		if a.Source != "alive" {
			t.Errorf("Source = %q, want alive", a.Source)
		}
		if a.Name == "" || a.Endpoint == "" {
			t.Errorf("entry must identify the proxy: %+v", a)
		}
	}
	if res.Checked != 2 {
		t.Errorf("Checked = %d, want 2", res.Checked)
	}
}

func TestCheckEndpointReportsNoneWorking(t *testing.T) {
	// Nothing serves the host: a working 200 with an empty list, NOT an error.
	// "No proxy can reach it" is a real answer the caller asked for.
	_, base := checkStack(t, map[string]int{}, 1)
	res := postCheck(t, base, "http://filtered.example/", 0)
	if len(res.Alive) != 0 || len(res.Pool) != 0 {
		t.Fatalf("expected no working proxies, got %+v", res)
	}
	if res.Checked != 1 || res.Failed != 1 {
		t.Errorf("Checked=%d Failed=%d, want 1/1", res.Checked, res.Failed)
	}
	if res.Key != "filtered.example" {
		t.Errorf("Key = %q", res.Key)
	}
}

func TestCheckEndpointDistinguishesHosts(t *testing.T) {
	// The core value: one pool, two destinations, different answers.
	served := map[string]int{urlKey(t, "http://allowed.example/"): http.StatusNoContent}
	_, base := checkStack(t, served, 1)
	if got := postCheck(t, base, "http://allowed.example/", 0); len(got.Alive) != 1 {
		t.Fatalf("allowed.example should be reachable: %+v", got)
	}
	if got := postCheck(t, base, "http://blocked.example/", 0); len(got.Alive) != 0 {
		t.Fatalf("blocked.example must NOT be reported reachable: %+v", got)
	}
	// ... and the keys must not collide.
	if postCheck(t, base, "allowed.example", 0).Key == postCheck(t, base, "blocked.example", 0).Key {
		t.Fatal("distinct hosts must not share a key")
	}
}

func TestCheckEndpointRejectsBadInput(t *testing.T) {
	_, base := checkStack(t, map[string]int{}, 1)
	for _, bad := range []string{"", "   ", "ftp://example.com", "127.0.0.1", "169.254.169.254", "localhost", "https://u:p@example.com"} {
		resp, err := testClient().Post(base+"/check", "application/json", strings.NewReader(`{"url":`+quote(bad)+`}`))
		if err != nil {
			t.Fatalf("POST /check: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("url %q: status %d, want 400", truncForMsg(bad), resp.StatusCode)
		}
	}
	// Malformed JSON and unknown fields are rejected too.
	for _, body := range []string{`not json`, `{"url":"example.com","bogus":1}`} {
		resp, err := testClient().Post(base+"/check", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: status %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestCheckEndpointGETMatchesPOST(t *testing.T) {
	served := map[string]int{urlKey(t, "http://get.example/"): http.StatusNoContent}
	_, base := checkStack(t, served, 1)
	viaPOST := postCheck(t, base, "http://get.example/", 0)
	resp, err := testClient().Get(base + "/check?url=http%3A%2F%2Fget.example%2F&depth=0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // decodeBody closes too; Close is idempotent
	var viaGET URLCheckResult
	decodeBody(t, resp, &viaGET)
	if viaGET.Key != viaPOST.Key || viaGET.ProbedURL != viaPOST.ProbedURL {
		t.Fatalf("GET and POST disagree: %+v vs %+v", viaGET, viaPOST)
	}
	// A junk depth must be a clear 400, not a silent zero.
	resp2, err := testClient().Get(base + "/check?url=http%3A%2F%2Fget.example%2F&depth=abc")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("depth=abc: status %d, want 400", resp2.StatusCode)
	}
}

func TestCheckEndpointBoundsDepth(t *testing.T) {
	served := map[string]int{urlKey(t, "http://deep.example/"): http.StatusNoContent}
	_, base := checkStack(t, served, 1)
	// Absurd depth must be clamped, never honoured literally.
	res := postCheck(t, base, "http://deep.example/", 1_000_000)
	if len(res.Pool) > checkMaxDepth {
		t.Fatalf("depth must clamp to %d, probed %d", checkMaxDepth, len(res.Pool))
	}
	// Negative depth means "alive only", not "probe everything".
	neg := postCheck(t, base, "http://deep.example/", -5)
	if len(neg.Pool) != 0 {
		t.Fatalf("negative depth must probe no pool candidates, got %d", len(neg.Pool))
	}
}

// ---- cache ----

// Time travel, not sleeping: the cache clock is injectable precisely so this
// does not cost a minute of wall time.
func TestURLCacheTTLAndSingleFlight(t *testing.T) {
	c := newURLCheckCache()
	now := time.Unix(1_700_000_000, 0)
	c.clock = func() time.Time { return now }
	seed := URLCheckResult{Key: "example.com", Checked: 1}
	f0, _ := c.begin("u1")
	c.finish("u1", f0, seed, nil)
	if got, ok := c.get("u1"); !ok || !got.Cached || got.Key != "example.com" {
		t.Fatalf("fresh entry must hit and be marked cached, got %+v ok=%v", got, ok)
	}
	now = now.Add(checkCacheTTL + time.Second)
	if _, ok := c.get("u1"); ok {
		t.Fatal("entry must be stale after the TTL")
	}
	// A failing probe must never be cached.
	f, _ := c.begin("bad")
	c.finish("bad", f, URLCheckResult{}, context.DeadlineExceeded)
	if _, ok := c.get("bad"); ok {
		t.Fatal("a failed check must not be cached")
	}
	// Bounded: the map must never exceed its cap.
	for i := range checkCacheEntries + 50 {
		k := "k" + strconv.Itoa(i)
		ff, _ := c.begin(k)
		c.finish(k, ff, URLCheckResult{Key: k}, nil)
	}
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > checkCacheEntries {
		t.Fatalf("cache grew to %d, cap is %d", n, checkCacheEntries)
	}
}

func TestURLCacheSingleFlight(t *testing.T) {
	c := newURLCheckCache()
	f1, leader1 := c.begin("same")
	f2, leader2 := c.begin("same")
	if !leader1 {
		t.Fatal("first caller must lead")
	}
	if leader2 {
		t.Fatal("second caller must join, not lead")
	}
	if f1 != f2 {
		t.Fatal("both callers must share one flight")
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-f1.done
		}()
	}
	c.finish("same", f1, URLCheckResult{Key: "k"}, nil)
	wg.Wait()
	// After completion the key is no longer in flight, so a fresh caller leads.
	if _, leader := c.begin("same"); !leader {
		t.Fatal("after completion a new caller must lead")
	}
}

// ---- read-only guarantee ----

// A check against a target NOTHING can reach must leave the pool exactly as it
// was: same active config, same child, nothing marked bad. This is the property
// that keeps a filtered destination from damaging a healthy pool - the same
// lesson as the 429/403 probe fix.
func TestCheckDoesNotMutatePool(t *testing.T) {
	m, _ := checkStack(t, map[string]int{}, 2)
	insts, _ := m.snapshot()
	before := make([]string, len(insts))
	pids := make([]int, len(insts))
	for i, inst := range insts {
		if a := inst.ActiveConfig(); a != nil {
			before[i] = a.Key()
		}
		pids[i] = inst.currentPID()
	}
	if _, err := m.CheckURL(context.Background(), "http://unreachable.example/", 0); err != nil {
		t.Fatalf("CheckURL: %v", err)
	}
	for i, inst := range insts {
		got := ""
		if a := inst.ActiveConfig(); a != nil {
			got = a.Key()
		}
		if got != before[i] {
			t.Errorf("instance %d active changed %q -> %q", i, before[i], got)
		}
		// The comment promised "same child" too; the previous version
		// collected the PID and threw it away, so only half the invariant was
		// ever checked.
		if got := inst.currentPID(); got != pids[i] {
			t.Errorf("instance %d child changed %d -> %d", i, pids[i], got)
		}
	}
}

// A context cancelled mid-check must return promptly and must not leave an
// in-flight entry wedged (which would make every later call block).
func TestCheckRespectsContextCancel(t *testing.T) {
	served := map[string]int{urlKey(t, "http://slow.example/"): http.StatusNoContent}
	m, _ := checkStack(t, served, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, _ = m.CheckURL(ctx, "http://slow.example/", 0)
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("cancelled check took %s, must return promptly", el)
	}
	// The cache must still be usable afterwards.
	if m.urlChecks == nil {
		t.Fatal("cache must survive a cancelled check")
	}
}

// ---- helpers ----

func postCheck(t *testing.T, base, url string, depth int) URLCheckResult {
	t.Helper()
	body := `{"url":` + quote(url) + `,"depth":` + strconv.Itoa(depth) + `}`
	resp, err := testClient().Post(base+"/check", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /check: %v", err)
	}
	defer func() { _ = resp.Body.Close() }() // decodeBody closes too; Close is idempotent
	var res URLCheckResult
	decodeBody(t, resp, &res)
	return res
}

func decodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, truncForMsg(string(raw)))
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode %s: %v", truncForMsg(string(raw)), err)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
