package main

// URL-reachability probe suite: the contract that answers "is THIS url
// reachable through the proxy?".
//
// The old shape of this coverage had a hole. TestProxyQuick races three
// INDEPENDENT targets (primary + Cloudflare + Telegram) and returns whichever
// leg wins, so a green result says "some leg answered" — it can NEVER prove a
// specific URL is reachable unless that URL happens to be one of the raced
// constants. Worse, the real-xray tests asserted a loopback target THROUGH
// that race, so each one silently fired real probe traffic at gstatic,
// Cloudflare and api.telegram.org while claiming to be hermetic.
//
// Two things fix that, both here:
//   - HealthResult.URL records which target produced a verdict, and
//     testSingleURL probes exactly one URL with no fallback race. Specific-URL
//     assertions now use it.
//   - serveSocksByURL is a SOCKS5 origin that grades each request BY ITS URL,
//     so a test can make exactly one target reachable and prove that target —
//     and only that target — decided the verdict.
//
// Everything in this file is hermetic: loopback only, no internet, no xray.

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// serveSocksByURL speaks just enough SOCKS5 (no-auth CONNECT) and then answers
// each tunnelled HTTP request with the status mapped for "host/path"
// (scheme-agnostic — the probe clients ask for https:// but the stub is
// plaintext). An unmapped target answers 404, which is exactly what a
// filtered/blocked endpoint looks like from the proxy's side.
//
// This is what makes a single URL's verdict observable: without URL-keyed
// grading every leg gets the same answer and the race proves nothing about
// which one carried the verdict.
func serveSocksByURL(t *testing.T, statuses map[string]int) (addr string, closeFn func()) {
	t.Helper()
	return serveSocksDelayed(t, statuses, nil)
}

// serveSocksDelayed is serveSocksByURL with per-URL artificial latency, used to
// drive a leg that provably finishes AFTER the race has already returned.
func serveSocksDelayed(t *testing.T, statuses map[string]int, delays map[string]time.Duration) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleURLSocks(c, statuses, delays)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func handleURLSocks(c net.Conn, statuses map[string]int, delays map[string]time.Duration) {
	defer func() { _ = c.Close() }()
	if !socksHandshake(c) {
		return
	}
	key, ok := readRequestTarget(c)
	if !ok {
		return
	}
	if d := delays[key]; d > 0 {
		time.Sleep(d)
	}
	status, mapped := statuses[key]
	if !mapped {
		status = http.StatusNotFound
	}
	writeStatus(c, status)
}

// socksHandshake performs the no-auth SOCKS5 greeting + CONNECT and returns
// false when the client hangs up or asks for something unsupported.
func socksHandshake(c net.Conn) bool {
	greet, ok := readN(c, 2)
	if !ok || greet[0] != 0x05 {
		return false
	}
	if _, ok := readN(c, int(greet[1])); !ok {
		return false
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return false
	}
	hdr, ok := readN(c, 4)
	if !ok || hdr[1] != 0x01 { // CONNECT only
		return false
	}
	switch hdr[3] { // ATYP
	case 0x01:
		_, ok = readN(c, 4)
	case 0x03:
		l, ok1 := readN(c, 1)
		if !ok1 {
			return false
		}
		_, ok = readN(c, int(l[0]))
	case 0x04:
		_, ok = readN(c, 16)
	default:
		return false
	}
	if !ok {
		return false
	}
	if _, ok := readN(c, 2); !ok { // port
		return false
	}
	_, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	return err == nil
}

// readRequestTarget slurps the request head and reduces it to "host/path".
func readRequestTarget(c net.Conn) (string, bool) {
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	var head strings.Builder
	for {
		line, err := br.ReadString('\n')
		head.WriteString(line)
		if err != nil {
			return "", false
		}
		if line == "\r\n" || line == "\n" {
			break
		}
		if head.Len() > 4096 {
			return "", false
		}
	}
	lines := strings.Split(head.String(), "\n")
	var path, host string
	// Request line: "GET /generate_204 HTTP/1.1" — the path is field 2. It
	// carries no colon, so it must be parsed BEFORE the header loop rather
	// than inside it.
	if f := strings.Fields(lines[0]); len(f) >= 2 {
		path = f[1]
	}
	for _, line := range lines[1:] {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(name), "host") {
			host = strings.TrimSpace(value)
		}
	}
	if path == "" || host == "" {
		return "", false
	}
	// Strip a default port so callers key on host/path alone.
	host, _, _ = strings.Cut(host, ":")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return host + path, true
}

func writeStatus(c net.Conn, status int) {
	body := ""
	switch status {
	case http.StatusNoContent, http.StatusNotFound, http.StatusForbidden:
	default:
		body = `{"probe":"stub"}`
	}
	head := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
	_, _ = c.Write([]byte(head))
}

// urlKey reduces a probe URL to the "host/path" key serveSocksByURL grades on.
// It must agree exactly with what readRequestTarget reconstructs, including the
// root-path case: an empty path normalizes to "/", NOT to a doubled slash.
func urlKey(t *testing.T, rawURL string) string {
	t.Helper()
	rest, found := strings.CutPrefix(rawURL, "https://")
	if !found {
		rest, found = strings.CutPrefix(rawURL, "http://")
		if !found {
			t.Fatalf("probe URL must carry a scheme: %s", rawURL)
		}
	}
	host, path, found := strings.Cut(rest, "/")
	if !found || path == "" {
		return host + "/"
	}
	return host + "/" + path
}

// ---- leg composition (pure: no network, no timing) ----

func legURLs(legs []probeLeg) []string {
	out := make([]string, 0, len(legs))
	for _, l := range legs {
		out = append(out, l.url)
	}
	return out
}

func TestProbeLegComposition(t *testing.T) {
	t.Run("defaults to gstatic when unset", func(t *testing.T) {
		got := legURLs(probeLegs(""))
		want := []string{probeURL, quickFallbackURL, telegramProbeURL}
		if !slices.Equal(got, want) {
			t.Fatalf("legs = %v, want %v", got, want)
		}
	})
	t.Run("configured primary leads", func(t *testing.T) {
		const custom = "https://example.test/generate_204"
		got := legURLs(probeLegs(custom))
		if got[0] != custom {
			t.Fatalf("primary leg = %q, want %q", got[0], custom)
		}
		if !slices.Equal(got, []string{custom, quickFallbackURL, telegramProbeURL}) {
			t.Fatalf("legs = %v", got)
		}
	})
	t.Run("telegram kill switch drops the third leg", func(t *testing.T) {
		t.Setenv("TELEGRAM_PROBE", "0")
		if got := legURLs(probeLegs("")); len(got) != 2 {
			t.Fatalf("legs = %v, want 2 with TELEGRAM_PROBE=0", got)
		}
	})
	t.Run("primary equal to a constant is deduped", func(t *testing.T) {
		// Pointing HEALTH_CHECK_URL at Cloudflare must not race the same URL
		// twice: duplicate legs doubled concurrent probe traffic for nothing.
		for _, dup := range []string{quickFallbackURL, telegramProbeURL} {
			got := legURLs(probeLegs(dup))
			if slices.Contains(got, probeURL) {
				t.Fatalf("primary %q should suppress gstatic, got %v", dup, got)
			}
			if n := slices.Index(got, dup); n != 0 {
				t.Fatalf("primary %q must lead, got %v", dup, got)
			}
		}
		got := legURLs(probeLegs(quickFallbackURL))
		if want := []string{quickFallbackURL, telegramProbeURL}; !slices.Equal(got, want) {
			t.Fatalf("legs = %v, want %v", got, want)
		}
	})
	t.Run("grading rules differ per leg", func(t *testing.T) {
		legs := probeLegs("")
		// Telegram accepts ANY completed exchange (its root 404s by design);
		// the 204 legs accept only a 2xx/3xx body.
		tg := legs[len(legs)-1]
		if tg.url != telegramProbeURL {
			t.Fatalf("last leg = %q, want the telegram target", tg.url)
		}
		if !tg.grade(HealthResult{Status: http.StatusNotFound}) {
			t.Fatal("telegram leg must accept a completed 404 exchange")
		}
		if tg.grade(HealthResult{Error: fmt.Errorf("dial refused")}) {
			t.Fatal("telegram leg must reject a transport error")
		}
		if statusOK(HealthResult{Status: http.StatusNotFound}) {
			t.Fatal("204 legs must reject a 404")
		}
		if !statusOK(HealthResult{Working: true, Status: http.StatusNoContent}) {
			t.Fatal("204 legs must accept a 204")
		}
	})
}

// ---- the race, proven over real exchanges ----
//
// These drive raceProbe directly with PLAINTEXT loopback targets. That is not
// a shortcut: a loopback stub cannot terminate TLS, and testSingleURL keeps
// the default (verifying) TLS config in production, so an https:// probe URL
// is unreachable from a hermetic test by construction. The production leg set
// is covered purely by TestProbeLegComposition, the race machinery by the
// tests below — the two compose into TestProxyQuick with nothing untested.

// Loopback probe targets used by the race tests. Plaintext on purpose: see above.
const (
	blockedTarget   = "http://blocked.example/generate_204"
	telegramTarget  = "http://telegram.example/"
	onlyTarget      = "http://only-this.example/generate_204"
	unreachTarget   = "http://nowhere.example/generate_204"
	secondUnreach   = "http://nowhere-2.example/generate_204"
	workingFallback = "http://fallback-ok.example/generate_204"
)

func statusLegs(urls ...string) []probeLeg {
	legs := make([]probeLeg, 0, len(urls))
	for _, u := range urls {
		legs = append(legs, probeLeg{url: u, grade: statusOK})
	}
	return legs
}

// A filtered or blocked primary must NOT condemn a tunnel whose fallback
// answers — the entire reason the fallback legs exist.
func TestRaceRescuesFilteredPrimary(t *testing.T) {
	addr, done := serveSocksByURL(t, map[string]int{
		urlKey(t, workingFallback): http.StatusNoContent,
	})
	defer done()
	res := raceProbe(addr, statusLegs(blockedTarget, workingFallback))
	if !res.Working {
		t.Fatalf("a working fallback must carry the verdict, got %s", res.Describe())
	}
	if res.URL != workingFallback {
		t.Fatalf("winning leg = %q, want the reachable fallback %q (a verdict must name its target)", res.URL, workingFallback)
	}
}

// The core specific-URL contract: exactly one target is reachable, and the
// verdict must be attributable to THAT target and nothing else.
func TestRaceVerdictNamesTheReachableURL(t *testing.T) {
	addr, done := serveSocksByURL(t, map[string]int{
		urlKey(t, onlyTarget): http.StatusNoContent,
	})
	defer done()
	res := raceProbe(addr, statusLegs(blockedTarget, onlyTarget))
	if !res.Working {
		t.Fatalf("the one reachable target must prove the tunnel, got %s", res.Describe())
	}
	if res.URL != onlyTarget {
		t.Fatalf("verdict names %q, want the only reachable target %q", res.URL, onlyTarget)
	}
	if res.Status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.Status)
	}
}

// The inverse: when the specific target is NOT reachable and nothing else is,
// the verdict must fail rather than borrow some other leg's success.
func TestRaceUnreachableURLFails(t *testing.T) {
	addr, done := serveSocksByURL(t, nil)
	defer done()
	res := raceProbe(addr, statusLegs(unreachTarget, secondUnreach))
	if res.Working {
		t.Fatalf("no target reachable, must not report working (%s)", res.Describe())
	}
	if res.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want the 404 the target served", res.Status)
	}
	if probeInconclusive(res) {
		t.Fatal("404 is a real verdict, not an inconclusive throttle")
	}
	if got := res.Describe(); !strings.Contains(got, "404") {
		t.Fatalf("Describe must surface the status, got %q", got)
	}
}

// Telegram's root 404s by design, so its leg grades a completed exchange as
// success: proven here over a REAL exchange, not just the pure predicate.
func TestRaceTelegramLegAccepts404Exchange(t *testing.T) {
	addr, done := serveSocksByURL(t, map[string]int{
		urlKey(t, telegramTarget): http.StatusNotFound,
	})
	defer done()
	res := raceProbe(addr, []probeLeg{
		{url: blockedTarget, grade: statusOK},
		{url: telegramTarget, grade: telegramWorking},
	})
	if !res.Working {
		t.Fatalf("a completed telegram exchange must prove reachability, got %s", res.Describe())
	}
	if res.URL != telegramTarget {
		t.Fatalf("winning leg = %q, want %q", res.URL, telegramTarget)
	}
	if res.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want the 404 envelope", res.Status)
	}
	// The verdict must stay self-consistent: callers switch on Working, so an
	// accepted-by-rule result can never report Working=false.
	if !res.Working {
		t.Fatal("accepted-by-rule must be normalized to Working=true")
	}
}

// A leg that arrives FIRST but is rejected by its own grading rule must never
// become the reported verdict. Order-independent by construction: the fast leg
// can only ever be rejected, the slow leg can only ever be accepted, so the
// winner is decided by the rules, not by a scheduler race.
func TestRaceReportsAcceptedLegNotFirst(t *testing.T) {
	addr, done := serveSocksDelayed(t,
		map[string]int{
			urlKey(t, blockedTarget):   http.StatusNotFound,
			urlKey(t, workingFallback): http.StatusNoContent,
		},
		map[string]time.Duration{urlKey(t, workingFallback): 750 * time.Millisecond},
	)
	defer done()
	res := raceProbe(addr, statusLegs(blockedTarget, workingFallback))
	if !res.Working {
		t.Fatalf("the accepted leg must decide the verdict, got %s", res.Describe())
	}
	if res.URL != workingFallback {
		t.Fatalf("winning leg = %q, want the accepted leg %q", res.URL, workingFallback)
	}
	if raceProbe(addr, nil).Error == nil {
		t.Fatal("an empty leg set must report an error, not a phantom success")
	}
}

// A throttled probe target is INCONCLUSIVE, never a death sentence.
func TestProbeThrottleIsInconclusive(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusForbidden} {
		addr, done := serveSocksByURL(t, map[string]int{
			urlKey(t, unreachTarget): status,
		})
		res := raceProbe(addr, statusLegs(unreachTarget))
		done()
		if res.Working {
			t.Fatalf("status %d must not read as working", status)
		}
		if !probeInconclusive(res) {
			t.Fatalf("status %d must be inconclusive, got %s", status, res.Describe())
		}
	}
	// And the boundary: nothing else excuses a failure.
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusMovedPermanently} {
		addr, done := serveSocksByURL(t, map[string]int{
			urlKey(t, unreachTarget): status,
		})
		res := raceProbe(addr, statusLegs(unreachTarget))
		done()
		if probeInconclusive(res) {
			t.Fatalf("status %d must condemn, not excuse", status)
		}
	}
	// A transport failure carries no status and must condemn.
	if probeInconclusive(HealthResult{Error: fmt.Errorf("dial tcp: connection refused")}) {
		t.Fatal("transport failure is a real failure")
	}
	// An accepted result is never inconclusive.
	if probeInconclusive(HealthResult{Working: true, Status: http.StatusNoContent}) {
		t.Fatal("a working verdict must not be inconclusive")
	}
}

// Losing legs must never block on send: the race channel is buffered for every
// leg, so a loser that finishes AFTER the winner returns cannot leak a
// goroutine that waits forever.
func TestRaceDoesNotLeakLosingLegs(t *testing.T) {
	addr, done := serveSocksDelayed(t,
		map[string]int{urlKey(t, workingFallback): http.StatusNoContent},
		map[string]time.Duration{urlKey(t, blockedTarget): 2 * time.Second},
	)
	defer done()

	before := runtime.NumGoroutine()
	for range 5 {
		res := raceProbe(addr, statusLegs(blockedTarget, workingFallback))
		if !res.Working {
			t.Fatalf("fast leg must win regardless of the slow one: %s", res.Describe())
		}
	}
	// The slow losers outlive the race by design; they must still all finish.
	deadline := time.Now().Add(15 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines leaked: %d before, %d after", before, after)
	}
}

// classifyProbe is the decision that keeps a throttled probe target from
// culling a healthy pool. Table-driven over every verdict shape the transport
// can produce, so the "unproven is not dead" rule cannot silently regress.
func TestClassifyProbe(t *testing.T) {
	cases := []struct {
		name string
		res  HealthResult
		want probeVerdict
	}{
		{"204 reachable", HealthResult{Working: true, Status: http.StatusNoContent}, verdictAdopt},
		{"301 redirect is a working exchange", HealthResult{Working: true, Status: http.StatusMovedPermanently}, verdictAdopt},
		{"telegram 404 accepted by rule", HealthResult{Working: true, Status: http.StatusNotFound}, verdictAdopt},
		{"429 throttled is unproven", HealthResult{Status: http.StatusTooManyRequests}, verdictUnproven},
		{"403 forbidden is unproven", HealthResult{Status: http.StatusForbidden}, verdictUnproven},
		{"404 is a real reject", HealthResult{Status: http.StatusNotFound}, verdictReject},
		{"500 is a real reject", HealthResult{Status: http.StatusInternalServerError}, verdictReject},
		{"503 is a real reject", HealthResult{Status: http.StatusServiceUnavailable}, verdictReject},
		{"transport failure rejects", HealthResult{Error: fmt.Errorf("dial: refused")}, verdictReject},
		{"empty verdict rejects", HealthResult{}, verdictReject},
	}
	for _, c := range cases {
		if got := classifyProbe(c.res); got != c.want {
			t.Errorf("%s: classifyProbe = %s, want %s (%s)", c.name, got, c.want, c.res.Describe())
		}
	}
	for _, v := range []probeVerdict{verdictAdopt, verdictUnproven, verdictReject} {
		if v.String() == "unknown" {
			t.Errorf("verdict %d has no name", v)
		}
	}
}

// The shared ledger must never learn about an unproven candidate: poisoning it
// makes every peer instance skip a node that is probably fine.
func TestUnprovenProbeDoesNotPoisonSharedLedger(t *testing.T) {
	shared := newProbeShared()
	const key = "node.example:443"
	if !shared.tryClaim(key) {
		t.Fatal("first claim must succeed")
	}
	// Exactly the sequence tryConfigs runs for an unproven candidate.
	shared.unclaim(key)
	if shared.isBad(key) {
		t.Fatal("an unproven candidate must stay claimable, not be poisoned")
	}
	if !shared.tryClaim(key) {
		t.Fatal("an unproven candidate must be re-claimable by a peer instance")
	}
	// A genuine reject, by contrast, must poison so peers stop wasting probes.
	shared.markBad(key)
	if !shared.isBad(key) {
		t.Fatal("a rejected candidate must be recorded bad")
	}
	shared.unclaim(key)
}

// The linux-only fake-xray e2e test proves the tryConfigs wiring end to end,
// but it cannot run everywhere — so the branch is also locked structurally
// here: verdictUnproven must release the claim and must NEVER poison the
// shared ledger. This is the single edit that reintroduced the "throttled probe
// target culls the pool" bug, and it is one line away from coming back.
func TestUnprovenBranchNeverPoisonsLedger(t *testing.T) {
	src, err := os.ReadFile("selector.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "case verdictUnproven:")
	if start < 0 {
		t.Fatal("tryConfigs must branch on verdictUnproven (found no such case)")
	}
	rest := body[start:]
	end := strings.Index(rest, "default:")
	if end < 0 {
		t.Fatal("verdictUnproven must be followed by an explicit default branch")
	}
	branch := rest[:end]
	if strings.Contains(branch, "markBad") {
		t.Error("the verdictUnproven branch must not call markBad: a throttled probe TARGET proves nothing about the upstream")
	}
	if !strings.Contains(branch, "unclaim") {
		t.Error("the verdictUnproven branch must unclaim so the candidate stays available")
	}
	if !strings.Contains(branch, "continue") {
		t.Error("the verdictUnproven branch must continue to the next candidate, not adopt it")
	}
}

// Describe must never render a completed-but-rejected exchange as "<nil>":
// that is exactly the verdict an operator has to read.
func TestHealthResultDescribe(t *testing.T) {
	cases := []struct {
		name string
		res  HealthResult
		want string
	}{
		{"ok with status", HealthResult{URL: "https://a.test/", Working: true, Status: 204}, "https://a.test/: 204 ok"},
		{"ok no status", HealthResult{URL: "https://a.test/", Working: true}, "https://a.test/: ok"},
		{"bad status", HealthResult{URL: "https://a.test/", Status: 404}, "https://a.test/: status 404"},
		{"transport", HealthResult{URL: "https://a.test/", Error: fmt.Errorf("refused")}, "https://a.test/: refused"},
		{"no target", HealthResult{Status: 500}, "<no target>: status 500"},
	}
	for _, c := range cases {
		if got := c.res.Describe(); got != c.want {
			t.Errorf("%s: Describe() = %q, want %q", c.name, got, c.want)
		}
	}
}
