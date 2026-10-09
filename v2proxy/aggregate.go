package main

// Stable aggregate endpoints: one SOCKS5 port + one HTTP port whose listeners
// NEVER restart, surviving every xray switch underneath. Each accepted
// connection is dialled against the alive instances in preference order
// (fastest currently-stable first) and served by the first one that accepts,
// so long-polling clients (Telegram bots via httpx) keep ONE fixed config and
// ride out upstream churn with a reconnect instead of a rewrite+restart.
//
// Cost per connection: two goroutines + 64KiB pooled buffers (the same relay
// primitive as the HTTP bridges). Fail-fast when nothing is alive: the client
// sees connection refused/dial-timeout immediately and retries, instead of
// hanging on a dead port.

import (
	"fmt"
	"net"
	"strconv"
	"time"
)

// aggFailoverBudget bounds the TOTAL time one accepted connection may spend
// walking the candidate ladder. It is deliberately equal to bridgeDialTimeout:
// the single-backend code spent exactly that on its one dial, so failover can
// never make a long-poll client wait longer than it used to, and a healthy pool
// whose head accepts still pays only one dial. Attempts SHARE this one
// deadline - remaining budget, never a fresh full budget - so N candidates
// cannot cost N x bridgeDialTimeout, and a black-holed head that burns the
// whole budget ends the walk instead of extending it. Nothing-alive still
// closes instantly and is never dialled at all.
const aggFailoverBudget = bridgeDialTimeout

func startAggregator(m *ProxyManager, socksPort, httpPort int) {
	if socksPort > 0 {
		go serveAggregate(m, socksPort, false)
	}
	if httpPort > 0 {
		go serveAggregate(m, httpPort, true)
	}
}

func serveAggregate(m *ProxyManager, port int, httpBackend bool) {
	kind := "SOCKS5"
	if httpBackend {
		kind = "HTTP"
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		errLog("aggregate %s bind :%d failed: %v", kind, port, err)
		return
	}
	infoLog("Aggregate %s on :%d (fastest alive, per-connection failover)", kind, port)
	for {
		c, err := ln.Accept()
		if err != nil {
			debugLog("aggregate :%d accept: %v", port, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go handleAggregateConn(m, c, httpBackend)
	}
}

// handleAggregateConn serves one client from the first candidate that accepts.
//
// Per-connection failover: candidates are walked in aggregateCandidates'
// preference order until a dial succeeds. A refused or black-holed port is a
// normal outcome, not a client error - the next candidate gets its turn. Only
// when the ladder is exhausted (or nothing was alive to begin with) is the
// client cut loose, which is the fail-fast contract: a long-poll client retries
// now instead of hanging on a dead port.
//
// Once a dial succeeds this function is DONE: the relay owns the connection and
// no later candidate is tried, so an established tunnel is never re-pointed.
func handleAggregateConn(m *ProxyManager, client net.Conn, httpBackend bool) {
	cands := m.aggregateCandidates(httpBackend)
	if len(cands) == 0 {
		_ = client.Close()
		return
	}
	deadline := time.Now().Add(aggFailoverBudget)
	for _, backend := range cands {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		up, err := net.DialTimeout("tcp", backend, left)
		if err != nil {
			debugLog("aggregate %s unreachable, trying next: %v", backend, err)
			continue
		}
		// Credit goes to the backend that actually got the flow, and only
		// when bytes really move: relay fires the hook on every read that
		// carried bytes (never on a zero-byte read or a skipped candidate), so
		// a backend that served nothing, and one that took the connection but
		// never proved it, never bank a note.
		egress := func() {}
		if _, p, perr := net.SplitHostPort(backend); perr == nil {
			if port, cerr := strconv.Atoi(p); cerr == nil {
				egress = func() { noteEgress(port) }
			}
		}
		go relay(up, client, egress)
		go relay(client, up, egress)
		return
	}
	_ = client.Close()
}

// aggregateCandidates returns the loopback backends of every usable alive
// instance, best first. It is the ordered generalization of pickBestBackend:
// the head is exactly that function's answer in every AGG_MIN_STREAK regime,
// so adding failover cannot change WHICH node a healthy pool lands on, only
// what happens once the head's port turns out to be dead.
//
// The order mirrors pickBestBackend's intent: stability-qualified instances
// first, fastest-first within the tier (a slow node holding 50s polls beats a
// fast one that RSTs them), then the unqualified pool as a last-resort tail so
// a qualified-tier outage still finds service. GetAliveStatuses is already
// latency-sorted, so this partition preserves "fastest first" inside each
// tier. When nothing qualifies, pickBestBackend falls back to plain
// fastest-wins; the fallback here is the whole alive list unchanged, for the
// same reason - never refuse service because no node has warmed up yet.
//
// Entries with no instance behind their Index, or with an unset port, are
// dropped rather than dialled: pickBestBackend returns "not ok" for those,
// which would cost a connection that the next candidate can still serve.
func (m *ProxyManager) aggregateCandidates(httpSide bool) []string {
	alive := m.GetAliveStatuses()
	if len(alive) == 0 {
		return nil
	}
	order := alive
	if minStreak := aggMinStreak(); minStreak > 0 {
		qualified := make([]InstanceStatus, 0, len(alive))
		rest := make([]InstanceStatus, 0, len(alive))
		for _, s := range alive {
			if s.OkStreak >= minStreak {
				qualified = append(qualified, s)
			} else {
				rest = append(rest, s)
			}
		}
		if len(qualified) > 0 {
			order = make([]InstanceStatus, 0, len(alive))
			order = append(order, qualified...)
			order = append(order, rest...)
		}
	}
	insts, _ := m.snapshot()
	out := make([]string, 0, len(order))
	for _, s := range order {
		if s.Index < 0 || s.Index >= len(insts) {
			continue
		}
		port := insts[s.Index].SOCKSPort()
		if httpSide {
			port = insts[s.Index].HTTPPort()
		}
		if port <= 0 {
			continue
		}
		out = append(out, fmt.Sprintf("127.0.0.1:%d", port))
	}
	return out
}
