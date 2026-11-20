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
	"cmp"
	"fmt"
	"net"
	"slices"
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
		go relayPair(up, client, egress)
		return
	}
	_ = client.Close()
}

// candidateLadder serves both aggregate endpoints (and pickBestBackend) from
// the manager's cached routing snapshot. Before the cache, every accepted
// connection locked the manager, cloned every status, sorted them and built a
// fresh []string of backends. That is pure derived state: it only changes when
// a status write lands (health tick / refresh), so it is built there instead.
func (m *ProxyManager) aggregateCandidates(httpSide bool) []string {
	return m.candidateLadder(httpSide, false)
}

// aggregateRelayCandidates is the relay-path twin of aggregateCandidates:
// identical streak ordering, but the per-instance RELAY inbounds. Instances
// without a relay port (feature off) contribute nothing, so relay-off
// yields nil and the relay aggregate never serves.
func (m *ProxyManager) aggregateRelayCandidates() []string {
	return m.candidateLadder(false, true)
}

// throughputTier buckets measured bulk rates (kbps) so ordering reacts to a
// real capacity class, not to noise in every sample. Unknown (0) is tier 0:
// measured-good nodes lead, then unmeasured nodes, while confirmed-low nodes
// are already in the degraded partition.
func throughputTier(kbps int64) int {
	switch {
	case kbps >= 8192: // >= 1 MB/s
		return 4
	case kbps >= 4096: // >= 512 KB/s
		return 3
	case kbps >= 2048: // >= 256 KB/s
		return 2
	case kbps >= 1024: // >= 128 KB/s
		return 1
	default:
		return 0
	}
}

// Upload classes order a pool that has at least one proven upload path:
// proven first, then unmeasured, then confirmed-broken. When no alive node is
// proven, the class is ignored entirely so the pool still picks its best
// downloader instead of refusing service.
const (
	uploadBrokenTier = iota
	uploadUnknownTier
	uploadProvenTier
)

func uploadTier(s InstanceStatus) int {
	switch {
	case s.UploadBroken:
		return uploadBrokenTier
	case s.UploadOK:
		return uploadProvenTier
	default:
		return uploadUnknownTier
	}
}

// uploadPreferred is true only when the gate is active AND the group carries
// positive proof. That is the "if nobody can upload, choose best download"
// rule: without proof, upload state never penalizes a fast download-only node.
func uploadPreferred(group []InstanceStatus) bool {
	if !uploadGateActive() {
		return false
	}
	for _, s := range group {
		if s.UploadOK {
			return true
		}
	}
	return false
}

// partitionQuality splits clean from last-resort nodes without reordering.
func partitionQuality(alive []InstanceStatus) (clean, degraded []InstanceStatus) {
	clean = make([]InstanceStatus, 0, len(alive))
	degraded = make([]InstanceStatus, 0, len(alive))
	for _, s := range alive {
		if s.Degraded {
			degraded = append(degraded, s)
		} else {
			clean = append(clean, s)
		}
	}
	return clean, degraded
}

// sortByCapacity orders one partition: throughput tier first, latency second.
func sortByCapacity(group []InstanceStatus) {
	slices.SortStableFunc(group, func(a, b InstanceStatus) int {
		if ta, tb := throughputTier(a.ThroughputKbps), throughputTier(b.ThroughputKbps); ta != tb {
			return cmp.Compare(tb, ta)
		}
		return cmp.Compare(a.LatMs, b.LatMs)
	})
}

// orderGroup applies upload class (when proven), capacity class, then the
// streak stability partition. Upload class is bucketed BEFORE stability so a
// newly proven uploader cannot be pushed behind an upload-unknown node merely
// because it has a shorter streak.
func orderGroup(group []InstanceStatus, minStreak int) []InstanceStatus {
	if len(group) == 0 {
		return group
	}
	if !uploadPreferred(group) {
		sortByCapacity(group)
		return orderStable(group, minStreak)
	}
	buckets := make([][]InstanceStatus, uploadProvenTier+1)
	for _, s := range group {
		tier := uploadTier(s)
		buckets[tier] = append(buckets[tier], s)
	}
	out := make([]InstanceStatus, 0, len(group))
	for tier := uploadProvenTier; tier >= uploadBrokenTier; tier-- {
		bucket := buckets[tier]
		sortByCapacity(bucket)
		out = append(out, orderStable(bucket, minStreak)...)
	}
	return out
}

// orderAlive is the routing order both candidate ladders share. Clean
// instances always come before memo'd/low-throughput (degraded) ones: a node
// that truncates or crawls must not be chosen while any clean node is alive,
// but it stays in the ladder as the last resort instead of being discarded.
// Within each partition, a proven upload path leads (only if one exists),
// then measured throughput class beats handshake latency, and the streak
// partition remains the tiebreaker inside that class.
func orderAlive(alive []InstanceStatus, minStreak int) []InstanceStatus {
	if len(alive) == 0 {
		return alive
	}
	clean, degraded := partitionQuality(alive)
	out := make([]InstanceStatus, 0, len(alive))
	out = append(out, orderGroup(clean, minStreak)...)
	out = append(out, orderGroup(degraded, minStreak)...)
	return out
}

// sortAliveByQuality is orderAlive without the streak partition, used by
// /proxies: clean before degraded, upload class, capacity class, latency.
func sortAliveByQuality(alive []InstanceStatus) []InstanceStatus {
	clean, degraded := partitionQuality(alive)
	out := make([]InstanceStatus, 0, len(alive))
	out = append(out, orderGroup(clean, 0)...)
	out = append(out, orderGroup(degraded, 0)...)
	return out
}

// orderStable is the streak partition: qualified (streak >= minStreak) before
// the rest. The caller's order is preserved inside each partition, and the
// input is already latency-sorted by buildCacheLocked.
func orderStable(alive []InstanceStatus, minStreak int) []InstanceStatus {
	if len(alive) == 0 || minStreak <= 0 {
		return alive
	}
	qualified := make([]InstanceStatus, 0, len(alive))
	rest := make([]InstanceStatus, 0, len(alive))
	for _, s := range alive {
		if s.OkStreak >= minStreak {
			qualified = append(qualified, s)
		} else {
			rest = append(rest, s)
		}
	}
	if len(qualified) == 0 {
		return alive
	}
	order := make([]InstanceStatus, 0, len(alive))
	order = append(order, qualified...)
	order = append(order, rest...)
	return order
}

// portsForOrder maps streak-ordered statuses onto loopback backends, dropping
// entries with no instance or unset port rather than dialling them (a dead
// dial costs a connection the next candidate could still serve).
func portsForOrder(insts []*ProxySelector, order []InstanceStatus, httpSide bool) []string {
	out := make([]string, 0, len(order))
	for _, s := range order {
		if s.Index < 0 || s.Index >= len(insts) {
			continue
		}
		if httpSide {
			if insts[s.Index].HTTPPort() > 0 {
				out = append(out, insts[s.Index].HTTPAddr())
			}
			continue
		}
		if insts[s.Index].SOCKSPort() > 0 {
			out = append(out, insts[s.Index].SOCKSAddr())
		}
	}
	return out
}

// relayPortsForOrder is portsForOrder for the relay inbounds: same order, but
// only instances that actually carry a relay port contribute a backend.
func relayPortsForOrder(insts []*ProxySelector, order []InstanceStatus) []string {
	out := make([]string, 0, len(order))
	for _, s := range order {
		if s.Index < 0 || s.Index >= len(insts) {
			continue
		}
		if port, addr := insts[s.Index].RelayEndpoint(); port > 0 {
			out = append(out, addr)
		}
	}
	return out
}
