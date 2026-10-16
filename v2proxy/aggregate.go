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
		go serveAggregate(m, socksPort, "SOCKS5", func(c net.Conn) { handleAggregateConn(m, c, false) })
	}
	if httpPort > 0 {
		go serveAggregate(m, httpPort, "HTTP", func(c net.Conn) { handleAggregateConn(m, c, true) })
	}
}

// startRelayAggregator serves the relay-egress fan-out: one stable SOCKS
// port whose connections ride the tunnel first and exit via the clean-IP
// relay. Port 0 disables (feature off). Direct aggregates are untouched.
func startRelayAggregator(m *ProxyManager, port int) {
	if port > 0 {
		go serveAggregate(m, port, "RELAY", func(c net.Conn) { handleRelayConn(m, c) })
	}
}

func serveAggregate(m *ProxyManager, port int, kind string, handle func(net.Conn)) {
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
		go handle(c)
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
	serveCandidates(client, m.aggregateCandidates(httpBackend), true)
}

// handleRelayConn serves one client from the relay ladder, but ONLY while
// the relay path itself proved alive this tick. The relay is a single shared
// hop behind every instance: walking N backends cannot route around a down
// worker, so a dark relay fails fast instead of burning the whole failover
// budget on guaranteed failures. Direct paths never consult this gate.
func handleRelayConn(m *ProxyManager, client net.Conn) {
	if !m.relayAlive.Load() {
		_ = client.Close()
		return
	}
	serveCandidates(client, m.aggregateRelayCandidates(), false)
}

func serveCandidates(client net.Conn, cands []string, trackEgress bool) {
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
		// Per-candidate, NOT the whole remaining budget. A blackholed candidate -
		// one that accepts nothing and never sends the SOCKS greeting, which is
		// what a SYN-dropping or middleboxed host looks like - would otherwise
		// consume every millisecond left and starve every later candidate, turning
		// one bad node into a failed client handshake. The soak caught exactly
		// that: "aggregate socks handshake: no method selection".
		//
		// The cap has to be generous enough that a merely SLOW working backend
		// still gets served, because the whole point of the ordering is that a
		// slow node beating a fast one that RSTs is better than neither.
		budget := left
		if budget > aggCandidateDialBudget {
			budget = aggCandidateDialBudget
		}
		up, err := net.DialTimeout("tcp", backend, budget)
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
		if trackEgress {
			if _, p, perr := net.SplitHostPort(backend); perr == nil {
				if port, cerr := strconv.Atoi(p); cerr == nil {
					egress = func() { noteEgress(port) }
				}
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
	order := m.orderedAlive()
	if len(order) == 0 {
		return nil
	}
	return portsForOrder(m, order, httpSide)
}

// aggregateRelayCandidates is the relay-path twin of aggregateCandidates:
// identical streak ordering, but the per-instance RELAY inbounds. Instances
// without a relay port (feature off) contribute nothing, so relay-off
// yields nil and the relay aggregate never serves.
func (m *ProxyManager) aggregateRelayCandidates() []string {
	order := m.orderedAlive()
	if len(order) == 0 {
		return nil
	}
	insts, _ := m.snapshot()
	out := make([]string, 0, len(order))
	for _, s := range order {
		if s.Index < 0 || s.Index >= len(insts) {
			continue
		}
		if port := insts[s.Index].RelayPort(); port > 0 {
			out = append(out, fmt.Sprintf("127.0.0.1:%d", port))
		}
	}
	return out
}

// orderedAlive is the streak partition both candidate ladders share: the
// relay ladder must rank nodes exactly like the direct ladder, or one path
// would prefer a node the other path distrusts.
func (m *ProxyManager) orderedAlive() []InstanceStatus {
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
	return order
}

// portsForOrder maps streak-ordered statuses onto loopback backends, dropping
// entries with no instance or unset port rather than dialling them (a dead
// dial costs a connection the next candidate could still serve).
func portsForOrder(m *ProxyManager, order []InstanceStatus, httpSide bool) []string {
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
