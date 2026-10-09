package main

// Per-connection failover for the stable aggregate endpoints.
//
// The aggregate used to promise "fastest alive, per-connection failover" in
// its own log line and header comment while dialling exactly ONE instance
// (pickBestBackend) and cutting the client loose on the first refusal. These
// tests are hermetic - loopback listeners only, no xray child, no internet -
// and they pin three things the single-candidate version could not do:
//
//   - a dead fastest instance no longer kills a connection that a slower alive
//     instance could have served (the failover itself),
//   - the ordered ladder still puts exactly the same node first that
//     pickBestBackend would have chosen, so failover cannot silently reroute a
//     healthy pool onto a flappy one,
//   - the fail-fast and egress contracts are unchanged: nothing alive still
//     closes instantly, and only the backend that actually took bytes is
//     credited with a proven flow.

import (
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

// aggResult carries one proxied exchange out of the bounded goroutine below.
type aggResult struct {
	status string
	err    error
}

// aggGet performs a raw HTTP/1.1 GET through the aggregate SOCKS5 listener and
// returns the response status line with the elapsed wall clock.
//
// The exchange runs under its own budget instead of a bare client deadline: a
// regression that wedges the dial ladder must fail with a number attached
// rather than hang the test binary until the package timeout.
func aggGet(t *testing.T, aggAddr, target, path string, budget time.Duration) (string, time.Duration) {
	t.Helper()
	start := time.Now()
	done := make(chan aggResult, 1)
	go func() {
		st, err := rawAggGet(aggAddr, target, path)
		done <- aggResult{status: st, err: err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("aggregate %s GET %s: %v", aggAddr, path, r.err)
		}
		return r.status, time.Since(start)
	case <-time.After(budget):
		t.Fatalf("aggregate %s GET %s produced nothing in %s (must fail fast, not hang)", aggAddr, path, budget)
		return "", time.Since(start)
	}
}

// rawAggGet is the transport half of aggGet: SOCKS5 CONNECT through the
// aggregate, one request, one status line.
func rawAggGet(aggAddr, target, path string) (string, error) {
	dialer, err := proxy.SOCKS5("tcp", aggAddr, nil, proxy.Direct)
	if err != nil {
		return "", err
	}
	c, err := dialer.Dial("tcp", target)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: probe.invalid\r\nConnection: close\r\n\r\n", path); err != nil {
		return "", err
	}
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

// aggExpectClosed asserts the aggregate accepts the connection and then hangs
// up without serving a single byte, and returns how long that took. This is
// the deliberate fail-fast shape: a long-poll client must be able to retry
// NOW rather than sit on a dead port.
func aggExpectClosed(t *testing.T, aggAddr string, budget time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	c, err := net.DialTimeout("tcp", aggAddr, 2*time.Second)
	if err != nil {
		// Refused at the listener itself is even faster: still fail-fast.
		return time.Since(start)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(budget))
	if n, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatalf("aggregate %s served %d byte(s); it must serve nothing", aggAddr, n)
	}
	return time.Since(start)
}

// deadPort returns a loopback port that is free RIGHT NOW, i.e. one whose
// connect is refused rather than answered. A dead instance is exactly this:
// the xray child is gone but the status still reads ok.
func deadPort(t *testing.T) int {
	t.Helper()
	p := freeLoopbackPort(t)
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p), 500*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatalf("port %d unexpectedly accepted a connection; it is not a dead port", p)
	}
	return p
}

// TestAggregateFailsOverToNextAliveInstance is the defect itself.
//
// The fastest, fully stability-qualified instance points at a port nothing is
// listening on. Before the fix handleAggregateConn dialled exactly that
// instance, got ECONNREFUSED, and closed the client - one dead node killed
// every connection even though a live instance sat right behind it in
// preference order.
func TestAggregateFailsOverToNextAliveInstance(t *testing.T) {
	liveAddr, done := serveSocks204(t, "ok")
	defer done()
	live := splitPort(t, liveAddr)
	dead := deadPort(t)

	m := backendManager([]InstanceStatus{
		{Index: 0, Status: "ok", LatMs: 5, OkStreak: 9},   // fastest + qualified, but DEAD
		{Index: 1, Status: "ok", LatMs: 400, OkStreak: 9}, // slower + qualified, and alive
	}, dead, dead+1000, live, live+1000)

	// Premise, stated explicitly: the ladder's FIRST candidate is the dead
	// port, and that is precisely what the old code dialled.
	if got, _ := m.pickBestBackend(false); got != fmt.Sprintf("127.0.0.1:%d", dead) {
		t.Fatalf("fastest alive must be the dead one for this test to mean anything, got %s", got)
	}
	cands := m.aggregateCandidates(false)
	if len(cands) != 2 || cands[0] != fmt.Sprintf("127.0.0.1:%d", dead) || cands[1] != fmt.Sprintf("127.0.0.1:%d", live) {
		t.Fatalf("candidate ladder = %v, want [dead:%d live:%d]", cands, dead, live)
	}

	ap := freeAggPort(t)
	startAggregator(m, ap, 0)
	addr := fmt.Sprintf("127.0.0.1:%d", ap)
	waitAggTCP(t, addr)

	status, elapsed := aggGet(t, addr, "probe.invalid:80", "/generate_204", 5*time.Second)
	if !strings.HasPrefix(status, "HTTP/1.1 204") {
		t.Fatalf("aggregate served %q, want the live backend's 204 (failover failed)", status)
	}
	// A refused loopback dial returns in microseconds, so walking one dead
	// candidate must cost nothing measurable - not a slice of the 5s budget.
	if elapsed > bridgeDialTimeout {
		t.Fatalf("failover took %s; a refused dial must not consume the %s budget", elapsed, bridgeDialTimeout)
	}
}

// TestAggregateSingleCandidateChoiceWasUnreachable is the counterfactual for
// the test above: the one address the old code was given cannot be dialled.
// That is what makes the previous behaviour a guaranteed connection loss
// rather than a rare race.
func TestAggregateSingleCandidateChoiceWasUnreachable(t *testing.T) {
	liveAddr, done := serveSocks204(t, "ok")
	defer done()
	m := backendManager([]InstanceStatus{
		{Index: 0, Status: "ok", LatMs: 5, OkStreak: 9},
		{Index: 1, Status: "ok", LatMs: 400, OkStreak: 9},
	}, deadPort(t), 0, splitPort(t, liveAddr), 0)

	only, ok := m.pickBestBackend(false)
	if !ok {
		t.Fatal("expected a single best backend")
	}
	if c, err := net.DialTimeout("tcp", only, 500*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatalf("the single candidate %s accepted; the failover test above is vacuous", only)
	}
	// ...while the second candidate is fine, so service was available.
	next := m.aggregateCandidates(false)[1]
	if c, err := net.DialTimeout("tcp", next, 500*time.Millisecond); err != nil {
		t.Fatalf("second candidate %s must be reachable: %v", next, err)
	} else {
		_ = c.Close()
	}
}

// TestAggregateCandidatesMatchPickBest pins the ORDERING contract: the head of
// the ladder is byte-identical to pickBestBackend's answer in every
// AGG_MIN_STREAK regime, so failover only ever kicks in AFTER the node the old
// code would have used has been given its chance.
func TestAggregateCandidatesMatchPickBest(t *testing.T) {
	// 0: flappy-fast (streak 1), 1: stable-slow (streak 10), 2: cold (streak 0).
	newMgr := func() *ProxyManager {
		return backendManager([]InstanceStatus{
			{Index: 0, Status: "ok", LatMs: 50, OkStreak: 1},
			{Index: 1, Status: "ok", LatMs: 800, OkStreak: 10},
			{Index: 2, Status: "ok", LatMs: 100, OkStreak: 0},
			{Index: 3, Status: "down", LatMs: 5, OkStreak: 99},
		}, 27801, 27811, 27802, 27812, 27803, 27813, 27804, 27814)
	}
	for _, tc := range []struct{ name, streak, socksHead, httpHead string }{
		{"default gate", "", "127.0.0.1:27802", "127.0.0.1:27812"},   // stable-slow qualified
		{"gate disabled", "0", "127.0.0.1:27801", "127.0.0.1:27811"}, // pure fastest wins
		{"nothing qualified", "50", "127.0.0.1:27801", "127.0.0.1:27811"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGG_MIN_STREAK", tc.streak)
			m := newMgr()
			for _, side := range []struct {
				httpSide bool
				want     string
			}{{false, tc.socksHead}, {true, tc.httpHead}} {
				best, ok := m.pickBestBackend(side.httpSide)
				if !ok || best != side.want {
					t.Fatalf("httpSide=%v: pickBestBackend = %q,%v want %q", side.httpSide, best, ok, side.want)
				}
				cands := m.aggregateCandidates(side.httpSide)
				if len(cands) == 0 || cands[0] != best {
					t.Fatalf("httpSide=%v: ladder head = %v, want %s (must equal pickBestBackend)", side.httpSide, cands, best)
				}
			}
		})
	}
}

// TestAggregateCandidatesTierOrder pins the tail order that pickBestBackend
// cannot express: qualified first (fastest-first inside the tier), then the
// cold/unqualified as last resorts, and down instances never appear at all.
func TestAggregateCandidatesTierOrder(t *testing.T) {
	t.Setenv("AGG_MIN_STREAK", "3")
	m := backendManager([]InstanceStatus{
		{Index: 0, Status: "ok", LatMs: 50, OkStreak: 1},   // unqualified fast
		{Index: 1, Status: "ok", LatMs: 800, OkStreak: 10}, // qualified slow
		{Index: 2, Status: "ok", LatMs: 100, OkStreak: 0},  // unqualified
		{Index: 3, Status: "down", LatMs: 1, OkStreak: 99}, // dead: excluded
		{Index: 4, Status: "ok", LatMs: 900, OkStreak: 10}, // qualified slow
	}, 27801, 27811, 27802, 27812, 27803, 27813, 27804, 27814, 27805, 27815)

	want := []string{"127.0.0.1:27802", "127.0.0.1:27805", "127.0.0.1:27801", "127.0.0.1:27803"}
	if got := m.aggregateCandidates(false); !slices.Equal(got, want) {
		t.Fatalf("ladder = %v, want %v (qualified-fastest, then last resorts)", got, want)
	}
}

// TestAggregateCandidatesDropUnusable guards the two entries that would burn
// the shared dial budget on a guaranteed refusal. Status index 4 has no
// instance behind it; the instance at index 1 has no SOCKS port configured.
func TestAggregateCandidatesDropUnusable(t *testing.T) {
	t.Setenv("AGG_MIN_STREAK", "0")
	m := backendManager([]InstanceStatus{
		{Index: 0, Status: "ok", LatMs: 10},
		{Index: 1, Status: "ok", LatMs: 20}, // socks port unset
		{Index: 2, Status: "ok", LatMs: 30},
		{Index: 4, Status: "ok", LatMs: 40}, // no instance behind this index
	}, 27801, 27811, 0, 27813, 27804, 27814, 27805, 27815)

	want := []string{"127.0.0.1:27801", "127.0.0.1:27804"}
	if got := m.aggregateCandidates(false); !slices.Equal(got, want) {
		t.Fatalf("ladder = %v, want the two usable ports only", got)
	}
	// The HTTP side of the same pool is still complete.
	wantHTTP := []string{"127.0.0.1:27811", "127.0.0.1:27813", "127.0.0.1:27814"}
	if got := m.aggregateCandidates(true); !slices.Equal(got, wantHTTP) {
		t.Fatalf("http ladder = %v, want %v", got, wantHTTP)
	}
}

// TestAggregateFailFastWhenNothingAlive keeps requirement 4 intact: no alive
// instance must close the client immediately, with no ladder and no dial.
func TestAggregateFailFastWhenNothingAlive(t *testing.T) {
	m := backendManager([]InstanceStatus{
		{Index: 0, Status: "down"},
		{Index: 1, Status: "starting"},
	}, 27801, 27811, 27802, 27812)
	if cands := m.aggregateCandidates(false); len(cands) != 0 {
		t.Fatalf("nothing alive must yield no candidates, got %v", cands)
	}

	ap := freeAggPort(t)
	startAggregator(m, ap, 0)
	addr := fmt.Sprintf("127.0.0.1:%d", ap)
	waitAggTCP(t, addr)

	elapsed := aggExpectClosed(t, addr, 2*time.Second)
	if elapsed > bridgeDialTimeout {
		t.Fatalf("nothing-alive took %s; the client must be released immediately", elapsed)
	}
}

// TestAggregateClosesAfterExhaustingLadder covers the other half of the
// fail-fast contract: alive-but-dead-instance pool. The client is held until
// every candidate has been tried, then released - and because loopback
// refusals are instant, "exhausted" still means "immediately".
func TestAggregateClosesAfterExhaustingLadder(t *testing.T) {
	m := backendManager([]InstanceStatus{
		{Index: 0, Status: "ok", LatMs: 5, OkStreak: 9},
		{Index: 1, Status: "ok", LatMs: 50, OkStreak: 9},
		{Index: 2, Status: "ok", LatMs: 90, OkStreak: 9},
	}, deadPort(t), 27811, deadPort(t), 27813, deadPort(t), 27815)

	ap := freeAggPort(t)
	startAggregator(m, ap, 0)
	addr := fmt.Sprintf("127.0.0.1:%d", ap)
	waitAggTCP(t, addr)

	elapsed := aggExpectClosed(t, addr, 2*time.Second)
	if elapsed > bridgeDialTimeout {
		t.Fatalf("exhausting 3 refused candidates took %s, must stay inside the %s budget", elapsed, bridgeDialTimeout)
	}
}

// TestAggregateEgressCreditsOnlyUsedBackend keeps noteEgress honest: it fires
// only on a genuinely proven byte flow, and only for the backend that took
// the connection. The dead first candidate must never bank a note - that is
// how a crashed instance used to look healthy to HealthCheck.
func TestAggregateEgressCreditsOnlyUsedBackend(t *testing.T) {
	liveAddr, done := serveSocks204(t, "ok")
	defer done()
	live := splitPort(t, liveAddr)
	dead := deadPort(t)

	m := backendManager([]InstanceStatus{
		{Index: 0, Status: "ok", LatMs: 5, OkStreak: 9},
		{Index: 1, Status: "ok", LatMs: 400, OkStreak: 9},
	}, dead, dead+1000, live, live+1000)

	ap := freeAggPort(t)
	startAggregator(m, ap, 0)
	addr := fmt.Sprintf("127.0.0.1:%d", ap)
	waitAggTCP(t, addr)

	if status, _ := aggGet(t, addr, "probe.invalid:80", "/generate_204", 5*time.Second); !strings.HasPrefix(status, "HTTP/1.1 204") {
		t.Fatalf("aggregate served %q, want 204", status)
	}
	// noteEgress fires from the relay goroutine on the first byte, so allow it
	// a moment to land before reading the registry.
	deadline := time.Now().Add(2 * time.Second)
	for !egressActive([]int{live}, egressGrace) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !egressActive([]int{live}, egressGrace) {
		t.Fatal("the backend that actually served the flow must record egress")
	}
	if egressActive([]int{dead}, egressGrace) {
		t.Fatal("a skipped, never-dialled instance must not record egress")
	}
}
