package main

// Resource soak harness: drives the REAL relay data paths in a loop for a
// configurable duration and asserts that goroutines, file descriptors, heap,
// connection slots and xray child processes do not GROW without bound.
//
// KNOWN LIMITATION — this harness currently FAILS under `go test -race`.
// handleConnect's slot-release closure in httpproxy.go reads and writes an
// unsynchronised `slotHeld bool` from BOTH the handler goroutine (its defer)
// and the relay goroutine (the inner defer), so two concurrent CONNECT tunnels
// trip the race detector. That is a REAL production data race, not a harness
// artefact: a bare loop of CONNECT tunnels with no soak rig reproduces it.
// httpproxy.go is outside this task's file ownership, so it is reported rather
// than fixed. Everything else here is race-clean; the growth assertions, the
// child-process accounting and the fixture wiring all pass under -race apart
// from this one production defect. See README_SOAK.md.
//
// The daemon holds a TCP relay per proxied connection, a goroutine PAIR per
// relay direction, a 32KiB pooled buffer per direction, one connection-slot
// semaphore entry per live tunnel, and spawns/kills an xray CHILD PROCESS per
// upstream switch. None of that shows up in a unit test: a leak is invisible
// until a long-lived tunnel-serving process runs for hours. This file is the
// missing measurement.
//
// GATED — `go test ./...` stays fast and hermetic because the soak only runs
// when the operator opts in and never under -short:
//
//	V2PRODOCK_SOAK=1 go test -run TestSoak -count=1 -v .
//	V2PRODOCK_SOAK=1 V2PRODOCK_SOAK_SECONDS=25 go test -run TestSoak -count=1 -v .
//	V2PRODOCK_SOAK=1 V2PRODOCK_SOAK_SECONDS=7200 go test -run TestSoak -count=1 -v -timeout 7500s .
//	V2PRODOCK_SOAK=1 go test -race -run TestSoak -count=1 -v .
//
// See README_SOAK.md for the metric definitions and the threshold rationale.
//
// What each iteration exercises (only the FAR SIDE of the wire is stubbed — the
// daemon's own code is the real thing):
//
//   - HTTP bridge plain-GET path through a serveSocks204("ok") upstream;
//   - HTTP bridge CONNECT path: dial, exchange bytes, hold open, close;
//   - the aggregate relay on BOTH listeners, routed by the real
//     ProxyManager.pickBestBackend over real *ProxySelector instances;
//   - the dead-upstream path (closed SOCKS port -> 502);
//   - the hung-upstream dial-timeout path (SOCKS port that never answers the
//     greeting -> 504) — the path whose abandoned goroutine used to leak an fd
//     per timeout;
//   - the hung-upstream CONNECT path (SOCKS handshake completes, then silence):
//     a client abandoning a LIVE tunnel, which is the fd+slot+goroutine hazard;
//   - the connection-slot semaphore, which must return to cap after the settle;
//   - per-iteration aggregate failover (mark the live instance down, confirm the
//     relay fails fast on the dead backend, bring it back);
//   - when a stub xray is usable (linux + python3): real child-process churn —
//     StartWithBest / SwitchToNextExcluding / HealthCheck / WatchdogCheck — with
//     the live-child count asserted after every tick and zero after Stop.
//
// Absolute counts are meaningless across machines (GOMAXPROCS, pre-existing
// package fixtures, GC timing), so NOTHING here asserts an absolute number.
// Every assertion is a DELTA against a baseline sampled from a fully quiesced
// process after a warmup window.

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- gating and pacing knobs ----

const (
	soakEnvGate     = "V2PRODOCK_SOAK"
	soakEnvDuration = "V2PRODOCK_SOAK_SECONDS"

	soakDefaultDuration = 60 * time.Second
	soakMinDuration     = 10 * time.Second
	soakMaxDuration     = 24 * time.Hour

	soakWorkers             = 4
	soakGetsPerIteration    = 4
	soakTunnelsPerIteration = 2
	soakTunnelHold          = 100 * time.Millisecond
	soakSampleEvery         = 5 * time.Second
	soakMaxSamples          = 4096

	// Warmup window: one fifth of the budget, capped. Everything measured
	// afterwards is steady state, so first-touch costs (sync.Pool filling, the
	// relay buffer cache, http.Transport pools, the first GC cycle, the stub's
	// accept backlog) are excluded from the delta by construction.
	soakWarmupMax = 15 * time.Second

	// Settle: how long the process is left alone (no in-flight work) before a
	// baseline or final sample is taken. Retired relay goroutines, the
	// dial-timeout orphan closers and TIME_WAIT sockets all need a moment to
	// disappear; sampling earlier measures lag, not retention.
	soakSettle       = 5 * time.Second
	soakQuietSamples = 5
	soakQuietGap     = 200 * time.Millisecond

	// Quiesce budget: the longest a worker may legitimately stay inside one
	// iteration. The bound must exceed the slowest single operation the rig
	// drives — the hung-dial probe blocks for bridgeDialTimeout (5s) and the
	// soak HTTP client caps every read at soakHTTPReadTimeout (15s) — or a
	// quiesce would give up while a worker was still legitimately busy and the
	// "settled" sample would measure in-flight work rather than retention.
	soakQuiesceBudget = 20 * time.Second
	soakQuiesceTick   = 20 * time.Millisecond

	// Slow-path pacing. The hung-dial probe costs bridgeDialTimeout (5s) and
	// the hung-CONNECT probe parks a stub goroutine for 30s (the "hang" stub
	// sleeps after completing the handshake), so both are rate limited to keep
	// the FIXTURE's footprint from swamping the thing being measured.
	soakHoleGap     = 6 * time.Second
	soakHangGap     = 8 * time.Second
	soakFailoverGap = 1500 * time.Millisecond
	soakSwitchGap   = 2 * time.Second

	// ---- thresholds ----
	//
	// Every one is a GROWTH budget over a warm, quiesced baseline, never an
	// absolute count, and each is justified the same way: derived from the size
	// of the resource ONE leak unit retains, so that
	//
	//	budget / bytes-per-leak  <<  iterations performed in the window
	//
	// A correct daemon has nothing to accumulate (every connection is closed
	// before the next one opens), so the only legitimate contribution to the
	// delta is the unwinding tail of the previous burst plus whatever the
	// FIXTURES still hold. The numbers below are therefore "many leak units",
	// not "a bit of noise": each one fails within seconds of a per-connection
	// leak appearing rather than after an hour of slowly drifting upward.
	//
	// Goroutines: +40. A relay pair is 2 goroutines plus 1 net/http conn
	// goroutine per live connection, so one leaked connection is ~3
	// goroutines. At the observed rate (see README_SOAK.md for the measured
	// figure) 40 is reached by a per-connection leak within a couple of
	// seconds of the measure window opening, while leaving >10x headroom over
	// the single-digit tail a loaded box shows.
	soakMaxGoroutineGrowth = 40
	//
	// FDs: +32. Closing a loopback socket releases its descriptor immediately
	// (the kernel retains a TIME_WAIT entry, not an fd), so a correct daemon's
	// count is flat and the listener set is fixed for the whole run. A leaked
	// descriptor is 1 per unclosed conn per side, so 32 tolerates a couple of
	// dozen descriptors still held by unwinding relays and by the SOCKS/HTTP
	// fixtures, and still catches a per-tunnel fd leak inside the first second
	// of the window.
	soakMaxFDGrowth = 32
	//
	// Heap: +8MiB after a forced GC. The pooled relay buffer is relayBufSize
	// (32KiB) and a connection holds two of them, so a buffer-per-connection
	// leak reaches 8MiB after ~128 connections — well inside the minimum 10s
	// budget — whereas GC pacing on a loaded box moves HeapAlloc by a few MiB
	// between samples. Forcing GC before BOTH the baseline and the final
	// sample (soakQuietSample) is what makes this a retention measurement
	// rather than a garbage-rate measurement.
	soakMaxHeapGrowth = 8 << 20
	//
	// Passive-health registry keys: +8. egressNotes.lastOK is keyed by instance
	// port and every port in play is fixed for the run, so the key COUNT should
	// not move at all; +8 absorbs keys synthesised from ports that merely
	// happen to be recycled by the OS across tests, while still failing if a
	// key is being minted per connection (e.g. from an unparsed port).
	soakMaxEgressKeyGrowth = 8
	//
	// Connection slots: 0. connSem is pre-filled to cap, so any slot still held
	// after the settle is an unreleased slot. This is the sharpest assertion in
	// the file: MAX_CONNS is 128 by default, so an unreleased slot is caught
	// after 128 tunnels rather than after the goroutine/fd counts have drifted
	// enough to notice. Zero is not a guess — cap-len is exactly 0 when the
	// semaphore is at rest.
	soakMaxSlotGrowth = 0

	// Coverage floors. These exist because bounded growth proved over no work
	// proves nothing. The fast-path floors are scaled by the measured window
	// (soakFloorFast) so a 10s smoke run is not judged against a 2h run's
	// expectations; the slow-path floors are scaled by each path's own rate
	// limit (soakFloorSlow), one tick of slack allowed, because a hung-dial
	// probe costs 5s of a worker and cannot possibly run every iteration.
	soakMinBridgeGets   = 60
	soakMinTunnels      = 30
	soakMinAggregate    = 30
	soakMinFailovers    = 3
	soakErrSampleLimit  = 3
	soakHTTPReadTimeout = 15 * time.Second
)

// soakMaxXrayChildren is the concurrent live-child ceiling for the child-process
// phase: the serving child plus at most one full parallel throwaway-probe
// fan-out. SwitchToNextExcluding runs min(switchWorkerCount(), candidates)
// probes concurrently, each holding a live child, WHILE the old child keeps
// serving — so 1+switchWorkerCount() is the honest bound, not an invented 2.
//
// A var, not a const: it derives from switchWorkerCount(), which reads
// SWITCH_WORKERS at call time.
//
// It is checked from a concurrent sampler (soakChildPeak), not only between
// ticks: a child spawned and orphaned inside a single tick would be invisible
// to tick-boundary sampling. Exactly 0 after Stop is asserted separately and is
// the actual orphan guarantee.
var soakMaxXrayChildren = 1 + switchWorkerCount()

// soakFloorFast scales a fast-path coverage floor with the measured window: the
// per-second rate a loopback soak reaches on the reference machine, halved, so
// the floor is met by a healthy run with 2x margin on any slower box.
func soakFloorFast(base int, main time.Duration) int {
	if n := int(main.Seconds()) * base / 2; n > base {
		return n
	}
	return base
}

// soakFloorSlow scales a rate-limited path's coverage floor by its own gap,
// allowing one tick of slack: the probe is admitted at most once per gap, so a
// floor higher than that is a flake, not a signal.
func soakFloorSlow(gap time.Duration, main time.Duration) int {
	n := int(main/gap) - 1
	return max(n, 1)
}

// soakEnabled reports whether the operator opted in to the soak.
func soakEnabled() bool {
	return strings.TrimSpace(os.Getenv(soakEnvGate)) == "1"
}

// soakDuration resolves the soak budget. An unparsable value is a fatal
// typo, not something to silently replace with a default: a soak that quietly
// ran for 60s instead of the 4h the operator asked for is worse than no soak.
func soakDuration(t *testing.T) time.Duration {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(soakEnvDuration))
	if raw == "" {
		return soakDefaultDuration
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s=%q is not an integer number of seconds", soakEnvDuration, raw)
	}
	d := time.Duration(secs) * time.Second
	switch {
	case d < soakMinDuration:
		t.Logf("soak: %s=%ds raised to the %s floor", soakEnvDuration, secs, soakMinDuration)
		d = soakMinDuration
	case d > soakMaxDuration:
		t.Logf("soak: %s=%ds capped at %s", soakEnvDuration, secs, soakMaxDuration)
		d = soakMaxDuration
	}
	return d
}

// ---- metrics ----

// soakSample is one snapshot of everything this harness tracks. Every field is
// machine-specific in absolute terms; only the difference between two quiesced
// samples is meaningful.
type soakSample struct {
	goroutines  int
	fds         int // -1 where the platform exposes no fd table
	heapAlloc   uint64
	heapObjects uint64
	children    int // -1 where no child accounting is possible
	egressKeys  int // len(egressNotes.lastOK)
	slotsHeld   int // connSem slots in use
	iterations  int64
}

func (s soakSample) line(name string, other soakSample, growth string) string {
	return fmt.Sprintf("  %-14s baseline=%-12d final=%-12d delta=%+d (%s)",
		name, s.value(name), other.value(name), other.value(name)-s.value(name), growth)
}

// value maps a metric name to its value so the report can be written by name.
func (s soakSample) value(name string) int {
	switch name {
	case "goroutines":
		return s.goroutines
	case "fds":
		return s.fds
	case "heap_bytes":
		return int(s.heapAlloc)
	case "egress_keys":
		return s.egressKeys
	case "slots_held":
		return s.slotsHeld
	case "xray_children":
		return s.children
	default:
		return 0
	}
}

const (
	metricGoroutines = "goroutines"
	metricFDs        = "fds"
	metricHeap       = "heap_bytes"
	metricEgress     = "egress_keys"
	metricSlots      = "slots_held"
	metricChildren   = "xray_children"
)

// soakFDCount counts /proc/self/fd. Linux only: the module targets linux,
// windows and darwin, and only linux exposes a per-process descriptor table
// without pulling in a dependency. -1 means "this metric is unavailable here"
// and is reported as such — it never fails the soak, because a harness that
// cannot measure fds on darwin must still run on darwin.
func soakFDCount() int {
	if runtime.GOOS != "linux" {
		return -1
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

// soakSampleNow takes one snapshot. childDir may be empty when no stub xray is
// usable, in which case the child metric is reported as unavailable.
func soakSampleNow(childDir string, iterations int64) soakSample {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s := soakSample{
		goroutines:  runtime.NumGoroutine(),
		fds:         soakFDCount(),
		heapAlloc:   ms.HeapAlloc,
		heapObjects: ms.HeapObjects,
		children:    -1,
		iterations:  iterations,
	}
	if childDir != "" && runtime.GOOS == "linux" {
		s.children = len(listXrayPIDs(childDir))
	}
	egressNotes.mu.Lock()
	s.egressKeys = len(egressNotes.lastOK)
	egressNotes.mu.Unlock()
	if connSem != nil {
		s.slotsHeld = cap(connSem) - len(connSem)
	}
	return s
}

// ---- rate limiting ----

// soakLimiter admits at most one caller per gap. The slow paths (hung dial,
// hung CONNECT, failover flip, child switch) each cost seconds of wall clock
// and leave a fixture behind; a limiter keeps their contribution bounded and
// predictable instead of proportional to loop speed.
type soakLimiter struct {
	gap  time.Duration
	next atomic.Int64 // unix nanos of the next admission
}

func (l *soakLimiter) allow() bool {
	if l.gap <= 0 {
		return true
	}
	now := time.Now().UnixNano()
	for {
		prev := l.next.Load()
		if now < prev {
			return false
		}
		if l.next.CompareAndSwap(prev, now+int64(l.gap)) {
			return true
		}
	}
}

// ---- the rig: every listener the soak drives ----

// soakRig owns the listeners. Note what is NOT here: none of them can be shut
// down. startHTTPProxy and startAggregator have no stop path by design (a
// bridge listener is never meant to restart), so their listeners and accept
// goroutines legitimately outlive the test and are a FIXED cost, present in
// the baseline and in the final sample alike. That is precisely why the
// assertions are deltas.
type soakRig struct {
	goodSocksAddr string
	clientFor     map[string]*http.Client
	bridgeAddrs   []string

	bridgeAddr     string
	hangBridgeAddr string
	holeBridgeAddr string
	deadBridgeAddr string

	aggSocksAddr string
	aggHTTPAddr  string

	m *ProxyManager

	holeLimiter     soakLimiter
	hangLimiter     soakLimiter
	failoverLimiter soakLimiter
}

// soakProxyClient is a bridge-bound client with keep-alives off: every request
// is a fresh connection and a fresh relay, which is the churn being measured.
func soakProxyClient(addr string) *http.Client {
	pu := &url.URL{Scheme: "http", Host: addr}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(pu),
			DisableKeepAlives: true,
		},
		Timeout: soakHTTPReadTimeout,
	}
}

// soakBlackholeSocks is a SOCKS port that accepts TCP and then never answers
// the greeting. serveSocks204("hang") deliberately COMPLETES the handshake
// before going silent, so it cannot reach dialSocksTimeout's timeout branch —
// only a never-answering listener can, and that branch is the one that used to
// strand an un-closed connection per timeout. Each accepted connection is
// closed after hold so the fixture's own goroutines drain.
func soakBlackholeSocks(t *testing.T, hold time.Duration) (addr string, closeFn func()) {
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
			go func() {
				time.Sleep(hold)
				_ = c.Close()
			}()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// newSoakRig wires the whole topology. Every listener is created here, once, so
// the per-iteration cost is connections rather than setup.
func newSoakRig(t *testing.T) *soakRig {
	t.Helper()

	// A directory with no xray binary in it: the aggregate-path selectors can
	// never spawn a child, so phase A cannot manufacture orphans.
	rig := &soakRig{clientFor: map[string]*http.Client{}}
	selDir := t.TempDir()

	goodAddr, closeGood := serveSocks204(t, "ok")
	t.Cleanup(closeGood)
	rig.goodSocksAddr = goodAddr

	// "hang" completes the SOCKS handshake and then goes silent: a LIVE tunnel
	// to a mute upstream, which is the abandon-a-tunnel case.
	hangSocks, closeHang := serveSocks204(t, "hang")
	t.Cleanup(closeHang)

	// A closed port stands in for a dead upstream (dial refused -> 502).
	deadSocks, closeDead := soakBlackholeSocks(t, 0)
	closeDead()

	// A port that accepts and never answers: the dial-timeout path (-> 504).
	// The accepted conns are closed after the bridge's own dial budget has
	// expired, so the fixture does not keep fds past the measurement.
	holeSocks, closeHole := soakBlackholeSocks(t, bridgeDialTimeout+5*time.Second)
	t.Cleanup(closeHole)

	// The bridge serving live traffic, plus the three degraded variants.
	rig.bridgeAddr = soakStartBridge(t, goodAddr)
	rig.hangBridgeAddr = soakStartBridge(t, hangSocks)
	rig.holeBridgeAddr = soakStartBridge(t, holeSocks)
	rig.deadBridgeAddr = soakStartBridge(t, deadSocks)
	rig.bridgeAddrs = []string{rig.bridgeAddr, rig.hangBridgeAddr, rig.holeBridgeAddr, rig.deadBridgeAddr}
	for _, addr := range rig.bridgeAddrs {
		rig.clientFor[addr] = soakProxyClient(addr)
	}

	// The aggregate: real ProxyManager, real *ProxySelector instances. The
	// "fast" instance's SOCKS port IS the live mini-SOCKS and its HTTP port IS
	// the live bridge, so a client that reaches the aggregate exercises
	// aggregate -> bridge -> SOCKS in one request. The second instance owns a
	// closed port: it is the failover target, and it must fail fast.
	fastSocks := splitPort(t, goodAddr)
	deadSocksPort := freeLoopbackPort(t)
	deadHTTPPort := freeLoopbackPort(t)
	fastSel := newTestSelector(t, selDir, "http://probe.invalid/", fastSocks, splitPort(t, rig.bridgeAddr))
	deadSel := newTestSelector(t, selDir, "http://probe.invalid/", deadSocksPort, deadHTTPPort)
	rig.m = &ProxyManager{
		instances: []*ProxySelector{fastSel, deadSel},
		statuses: []InstanceStatus{
			{Index: 0, Status: "ok", LatMs: 5, OkStreak: aggMinStreakDefault + 5, Name: "soak-fast"},
			{Index: 1, Status: "ok", LatMs: 900, OkStreak: aggMinStreakDefault + 5, Name: "soak-dead"},
		},
		subURLs: []string{},
		xrayDir: selDir,
	}

	aggSocksPort, aggHTTPPort := freeAggPort(t), freeAggPort(t)
	startAggregator(rig.m, aggSocksPort, aggHTTPPort)
	rig.aggSocksAddr = fmt.Sprintf("127.0.0.1:%d", aggSocksPort)
	rig.aggHTTPAddr = fmt.Sprintf("127.0.0.1:%d", aggHTTPPort)
	waitAggTCP(t, rig.aggSocksAddr)
	waitAggTCP(t, rig.aggHTTPAddr)

	// Ports are handed out by the OS and CAN be recycled between tests, so a
	// note left behind by an earlier test in this binary could otherwise
	// satisfy a "live traffic" expectation by accident and turn an assertion
	// into a no-op. Clear the passive-health registry for every port in play.
	ports := []int{
		fastSocks, deadSocksPort, deadHTTPPort, splitPort(t, rig.bridgeAddr),
		splitPort(t, rig.hangBridgeAddr), splitPort(t, rig.holeBridgeAddr),
		splitPort(t, rig.deadBridgeAddr), aggSocksPort, aggHTTPPort,
	}
	expireEgress(ports...)
	t.Cleanup(func() { expireEgress(ports...) })

	rig.holeLimiter.gap = soakHoleGap
	rig.hangLimiter.gap = soakHangGap
	rig.failoverLimiter.gap = soakFailoverGap

	return rig
}

// soakStartBridge binds a bridge on a fresh loopback port and waits for it.
func soakStartBridge(t *testing.T, socksAddr string) string {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", freeLoopbackPort(t))
	startHTTPProxy(addr, socksAddr)
	waitAggTCP(t, addr)
	return addr
}

// ---- per-worker counters ----

// soakErrors keeps a count plus a few example messages: the count is the
// assertion, the samples are the diagnosis.
type soakErrors struct {
	n       int
	samples []string
}

func (e *soakErrors) add(err error) {
	if err == nil {
		return
	}
	e.n++
	if len(e.samples) < soakErrSampleLimit {
		e.samples = append(e.samples, err.Error())
	}
}

func (e *soakErrors) mergeInto(dst *soakErrors) {
	dst.n += e.n
	for _, s := range e.samples {
		if len(dst.samples) < soakErrSampleLimit {
			dst.samples = append(dst.samples, s)
		}
	}
}

// soakStats is one worker's tally. Each worker owns its own element of the
// stats slice, so no locking is needed for the counters themselves.
type soakStats struct {
	bridgeGets    int
	bridgeOK      int
	bridgeBad     int
	tunnels       int
	tunnelOK      int
	aggSocks      int
	aggHTTP       int
	aggBytesMoved int
	deadProbes    int
	deadOK        int
	holeProbes    int
	holeOK        int
	hangProbes    int
	hangOK        int
	failovers     int
	failoverFast  int
	// childTicks counts xray child-process churn cycles (start/switch/health/
	// watchdog). Only the dedicated child phase bumps it.
	childTicks int
	// childSwitches counts ticks whose SwitchToNextExcluding actually swapped
	// the serving child. Distinguished from ticks because a tick where every
	// candidate was dead is churn without a swap, and a phase that only ever
	// produced the former would not exercise the orphan-prone path at all.
	childSwitches int
	// iterations counts completed passes over the whole path set.
	iterations int
	errs       soakErrors

	// Per-path wall-clock totals in nanoseconds plus per-path call counts.
	// Not asserted: a slow path is a latency finding, not a leak. But without
	// them the iteration rate is unexplainable — a reader seeing 20 iterations
	// in 20s cannot tell a healthy slow path from a wedged one, and cannot tell
	// WHICH path is slow.
	nGet, nTun, nAggS, nAggH, nDead, nHole, nHang, nFailover atomic.Int64
	cGet, cTun, cAggS, cAggH, cDead, cHole, cHang, cFailover atomic.Int64
}

func (w *soakStats) merge(dst *soakStats) {
	dst.bridgeGets += w.bridgeGets
	dst.bridgeOK += w.bridgeOK
	dst.bridgeBad += w.bridgeBad
	dst.tunnels += w.tunnels
	dst.tunnelOK += w.tunnelOK
	dst.aggSocks += w.aggSocks
	dst.aggHTTP += w.aggHTTP
	dst.aggBytesMoved += w.aggBytesMoved
	dst.deadProbes += w.deadProbes
	dst.deadOK += w.deadOK
	dst.holeProbes += w.holeProbes
	dst.holeOK += w.holeOK
	dst.hangProbes += w.hangProbes
	dst.hangOK += w.hangOK
	dst.failovers += w.failovers
	dst.failoverFast += w.failoverFast
	dst.childTicks += w.childTicks
	dst.childSwitches += w.childSwitches
	dst.iterations += w.iterations
	dst.nGet.Add(w.nGet.Load())
	dst.nTun.Add(w.nTun.Load())
	dst.nAggS.Add(w.nAggS.Load())
	dst.nAggH.Add(w.nAggH.Load())
	dst.nDead.Add(w.nDead.Load())
	dst.nHole.Add(w.nHole.Load())
	dst.nHang.Add(w.nHang.Load())
	dst.nFailover.Add(w.nFailover.Load())
	dst.cGet.Add(w.cGet.Load())
	dst.cTun.Add(w.cTun.Load())
	dst.cAggS.Add(w.cAggS.Load())
	dst.cAggH.Add(w.cAggH.Load())
	dst.cDead.Add(w.cDead.Load())
	dst.cHole.Add(w.cHole.Load())
	dst.cHang.Add(w.cHang.Load())
	dst.cFailover.Add(w.cFailover.Load())
	w.errs.mergeInto(&dst.errs)
}

// ---- one iteration over the real paths ----

// soakIterate performs a single pass. Every counter it bumps is a claim that
// the path actually ran, so the final report shows coverage rather than
// asserting growth over work that never happened.
func (r *soakRig) iterate(w *soakStats, it int) {
	// The helpers below return an error AND record it on w.errs, so discarding
	// the return here loses nothing: the tally is the assertion, and these three
	// paths are the HAPPY ones whose failure must not stop the iteration.
	for range soakGetsPerIteration {
		w.cGet.Add(1)
		started := time.Now()
		_ = r.bridgeGet(w, r.bridgeAddr, 0)
		w.nGet.Add(int64(time.Since(started)))
	}
	for range soakTunnelsPerIteration {
		w.cTun.Add(1)
		started := time.Now()
		_ = r.bridgeTunnel(w, r.bridgeAddr, soakTunnelHold, true)
		w.nTun.Add(int64(time.Since(started)))
	}
	w.cAggS.Add(1)
	started := time.Now()
	_ = r.aggregateSocks(w)
	w.nAggS.Add(int64(time.Since(started)))
	w.cAggH.Add(1)
	started = time.Now()
	_ = r.aggregateHTTP(w)
	w.nAggH.Add(int64(time.Since(started)))
	w.cDead.Add(1)
	started = time.Now()
	// Dead upstream: refused dial must surface as 502, immediately.
	w.deadProbes++
	if r.bridgeGet(w, r.deadBridgeAddr, http.StatusBadGateway) == nil {
		w.deadOK++
	}
	w.nDead.Add(int64(time.Since(started)))

	// Hung upstream: dial budget must surface as 504.
	if r.holeLimiter.allow() {
		w.cHole.Add(1)
		started := time.Now()
		if r.bridgeGet(w, r.holeBridgeAddr, http.StatusGatewayTimeout) == nil {
			w.holeOK++
		}
		w.nHole.Add(int64(time.Since(started)))
		w.holeProbes++
	}

	// Hung upstream, CONNECT flavour: the handshake succeeds so a real tunnel
	// exists, then the peer goes mute. Hold it and walk away — this is the
	// abandoned-live-tunnel case (fd + slot + goroutine pair).
	if r.hangLimiter.allow() {
		w.cHang.Add(1)
		started := time.Now()
		if r.bridgeTunnel(w, r.hangBridgeAddr, soakTunnelHold, false) == nil {
			w.hangOK++
		}
		w.nHang.Add(int64(time.Since(started)))
		w.hangProbes++
	}

	// Per-iteration failover: force the aggregate onto the dead instance and
	// require it to fail fast rather than hand back a black hole.
	if r.failoverLimiter.allow() {
		w.cFailover.Add(1)
		started := time.Now()
		r.aggregateFailover(w)
		w.nFailover.Add(int64(time.Since(started)))
	}
	_ = it
}

// bridgeGet drives one plain GET through a bridge and checks the status when
// want is non-zero (0 = "any completed response"; a live upstream must answer,
// but the exact code is not the point).
func (r *soakRig) bridgeGet(w *soakStats, addr string, want int) error {
	w.bridgeGets++
	resp, err := r.clientFor[addr].Get("http://upstream.invalid/soak")
	if err != nil {
		w.errs.add(fmt.Errorf("bridge %s GET: %w", addr, err))
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := soakDrainBody(resp); err != nil {
		w.errs.add(fmt.Errorf("bridge %s GET body: %w", addr, err))
		return err
	}
	if want != 0 && resp.StatusCode != want {
		w.errs.add(fmt.Errorf("bridge %s GET status %d, want %d", addr, resp.StatusCode, want))
		return fmt.Errorf("bridge %s status %d want %d", addr, resp.StatusCode, want)
	}
	// Per-path classification is deliberately NOT done here. It belongs to
	// iterate(), which knows which probe it started; doing it in both places
	// double-counts (the hung-CONNECT tally came out as 6 for 3 probes).
	w.bridgeOK++
	return nil
}

// bridgeTunnel opens a CONNECT tunnel, optionally exchanges a request/response
// through it, holds it open, and closes it.
func (r *soakRig) bridgeTunnel(w *soakStats, addr string, hold time.Duration, exchange bool) error {
	w.tunnels++
	c, err := soakDial(addr)
	if err != nil {
		w.errs.add(fmt.Errorf("tunnel dial %s: %w", addr, err))
		return err
	}
	defer func() { _ = c.Close() }()
	if err := c.SetDeadline(time.Now().Add(soakHTTPReadTimeout)); err != nil {
		w.errs.add(fmt.Errorf("tunnel deadline %s: %w", addr, err))
		return err
	}
	const host = "tunnel.invalid:443"
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", host, host); err != nil {
		w.errs.add(fmt.Errorf("tunnel write %s: %w", addr, err))
		return err
	}
	br := bufio.NewReader(c)
	code, err := soakReadStatus(br)
	if err != nil {
		w.errs.add(fmt.Errorf("tunnel status %s: %w", addr, err))
		return err
	}
	if code != http.StatusOK {
		w.errs.add(fmt.Errorf("tunnel %s status %d, want 200", addr, code))
		return fmt.Errorf("tunnel %s status %d", addr, code)
	}
	if exchange {
		// Prove bytes move BOTH ways before parking the tunnel: the 204 comes
		// back through the relay pair and the pooled buffer.
		if _, err := fmt.Fprint(c, "GET /soak HTTP/1.1\r\nHost: tunnel.invalid\r\n\r\n"); err != nil {
			w.errs.add(fmt.Errorf("tunnel request %s: %w", addr, err))
			return err
		}
		code, err := soakReadStatus(br)
		if err != nil {
			w.errs.add(fmt.Errorf("tunnel response %s: %w", addr, err))
			return err
		}
		if code != http.StatusNoContent {
			w.errs.add(fmt.Errorf("tunnel %s upstream status %d, want 204", addr, code))
			return fmt.Errorf("tunnel %s upstream status %d", addr, code)
		}
	}
	// Hold it open so the relay pair is genuinely mid-flight, then drop it.
	time.Sleep(hold)
	// No per-address tally here; see bridgeGet.
	w.tunnelOK++
	return nil
}

// soakSocksGreet performs the CLIENT side of a no-auth SOCKS5 CONNECT.
//
// The package's socksHandshake is the SERVER side (it is used by the
// handleURLSocks fixture): it reads the greeting instead of writing it. Using
// it as a client deadlocks on the first read until the connection deadline, so
// every aggregate SOCKS probe was silently burning 5s of a worker per
// iteration — which is what held the whole rig to under one iteration/second.
// The aggregate relay is a byte pipe, so this must be a real client greeting.
func soakSocksGreet(c net.Conn, host string, port int) error {
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil { // VER, 1 method, no-auth
		return fmt.Errorf("write greeting: %w", err)
	}
	resp, ok := readN(c, 2)
	if !ok {
		return fmt.Errorf("no method selection")
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		return fmt.Errorf("method refused: % x", resp)
	}
	// VER, CMD=CONNECT, RSV, ATYP=domain, len, host, port (big endian).
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return fmt.Errorf("write connect: %w", err)
	}
	rep, ok := readN(c, 4)
	if !ok {
		return fmt.Errorf("no connect reply")
	}
	if rep[1] != 0x00 {
		return fmt.Errorf("connect refused, reply code %d", rep[1])
	}
	// Drain whatever BND.ADDR/BND.PORT the server appended, so the stream is
	// positioned at the tunnelled payload.
	switch rep[3] {
	case 0x01:
		if _, ok := readN(c, 4+2); !ok {
			return fmt.Errorf("truncated IPv4 bind address")
		}
	case 0x04:
		if _, ok := readN(c, 16+2); !ok {
			return fmt.Errorf("truncated IPv6 bind address")
		}
	case 0x03:
		l, ok := readN(c, 1)
		if !ok {
			return fmt.Errorf("truncated domain bind length")
		}
		if _, ok := readN(c, int(l[0])+2); !ok {
			return fmt.Errorf("truncated domain bind address")
		}
	default:
		return fmt.Errorf("unknown bind atyp %d", rep[3])
	}
	return nil
}

// aggregateSocks drives one SOCKS5 request through the aggregate's SOCKS
// listener. The aggregate is a raw relay, so the client speaks SOCKS5 straight
// through it to the backend mini-SOCKS.
func (r *soakRig) aggregateSocks(w *soakStats) error {
	w.aggSocks++
	c, err := soakDial(r.aggSocksAddr)
	if err != nil {
		w.errs.add(fmt.Errorf("aggregate socks dial: %w", err))
		return err
	}
	defer func() { _ = c.Close() }()
	if err := c.SetDeadline(time.Now().Add(soakHTTPReadTimeout)); err != nil {
		w.errs.add(fmt.Errorf("aggregate socks deadline: %w", err))
		return err
	}
	if err := soakSocksGreet(c, "agg.invalid", 80); err != nil {
		w.errs.add(fmt.Errorf("aggregate socks handshake: %w", err))
		return err
	}
	if _, err := fmt.Fprint(c, "GET /soak HTTP/1.1\r\nHost: agg.invalid\r\n\r\n"); err != nil {
		w.errs.add(fmt.Errorf("aggregate socks write: %w", err))
		return err
	}
	code, err := soakReadStatus(bufio.NewReader(c))
	if err != nil || code == 0 {
		// Fail-fast is a legitimate outcome: another worker may be mid-failover,
		// which points the aggregate at the closed instance on purpose.
		if isFastClose(err) {
			return nil
		}
		// err is nil only when code == 0 with no read error, which
		// soakReadStatus cannot produce; the nil guard keeps %w honest if that
		// ever changes, because %w on a nil error renders "%!w(<nil>)".
		if err == nil {
			err = errNoStatusLine
		}
		w.errs.add(fmt.Errorf("aggregate socks response (status %d): %w", code, err))
		return err
	}
	if code != http.StatusNoContent {
		w.errs.add(fmt.Errorf("aggregate socks status %d, want 204", code))
		return fmt.Errorf("aggregate socks status %d", code)
	}
	w.aggBytesMoved++
	return nil
}

// aggregateHTTP drives one plain HTTP request through the aggregate's HTTP
// listener, which relays into the live bridge and on to the SOCKS stub.
func (r *soakRig) aggregateHTTP(w *soakStats) error {
	w.aggHTTP++
	c, err := soakDial(r.aggHTTPAddr)
	if err != nil {
		w.errs.add(fmt.Errorf("aggregate http dial: %w", err))
		return err
	}
	defer func() { _ = c.Close() }()
	if err := c.SetDeadline(time.Now().Add(soakHTTPReadTimeout)); err != nil {
		w.errs.add(fmt.Errorf("aggregate http deadline: %w", err))
		return err
	}
	req := "GET http://agg.invalid/soak HTTP/1.1\r\nHost: agg.invalid\r\nConnection: close\r\n\r\n"
	if _, err := fmt.Fprint(c, req); err != nil {
		w.errs.add(fmt.Errorf("aggregate http write: %w", err))
		return err
	}
	code, err := soakReadStatus(bufio.NewReader(c))
	if err != nil || code == 0 {
		if isFastClose(err) {
			return nil
		}
		if err == nil {
			err = errNoStatusLine
		}
		w.errs.add(fmt.Errorf("aggregate http response (status %d): %w", code, err))
		return err
	}
	if code != http.StatusNoContent {
		w.errs.add(fmt.Errorf("aggregate http status %d, want 204", code))
		return fmt.Errorf("aggregate http status %d", code)
	}
	w.aggBytesMoved++
	return nil
}

// aggregateFailover marks the live instance down so pickBestBackend must route
// to the dead one, proves the relay fails fast, then restores it.
func (r *soakRig) aggregateFailover(w *soakStats) {
	w.failovers++
	r.m.markDown(0, "soak: forced failover probe")
	start := time.Now()
	c, err := soakDial(r.aggSocksAddr)
	if err == nil {
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		// The aggregate dials the closed instance port, gets refused, and
		// closes the client conn, so the greeting fails. Anything else means
		// the relay handed back a black hole instead of failing fast.
		if gerr := soakSocksGreet(c, "agg.invalid", 80); gerr != nil {
			err = gerr
		}
		_ = c.Close()
	}
	elapsed := time.Since(start)
	r.m.markOK(0, "soak-fast", 5*time.Millisecond)
	if err == nil {
		w.errs.add(fmt.Errorf("failover: dead backend answered in %s", elapsed))
		return
	}
	if elapsed > 3*time.Second {
		w.errs.add(fmt.Errorf("failover took %s, must fail fast", elapsed))
		return
	}
	w.failoverFast++
}

// soakDial opens one loopback connection with a bounded dial.
func soakDial(addr string) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, 3*time.Second)
}

// soakReadStatus reads a response head and returns the status code. A clean
// close before any status line arrives yields (0, err), which callers treat as
// "peer hung up", not as a protocol error.
//
// Leading blank lines are SKIPPED, not treated as the end of the headers. This
// is not leniency, it is correctness: the reader is reused across the CONNECT
// response and the tunnelled response on the same connection, so the CRLF that
// terminates the 200 Connection Established head is still buffered when the
// second read starts. Treating it as "headers ended" failed every tunnel that
// exchanged bytes — the exact probe that proves the relay carries data.
func soakReadStatus(br *bufio.Reader) (int, error) {
	for range 64 {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, err
		}
		if fields := strings.Fields(line); len(fields) >= 2 && strings.HasPrefix(fields[0], "HTTP/") {
			code, cerr := strconv.Atoi(fields[1])
			if cerr != nil {
				return 0, fmt.Errorf("unparsable status line %q", strings.TrimSpace(line))
			}
			return code, nil
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
	}
	return 0, fmt.Errorf("no status line in 64 header lines")
}

// errNoStatusLine stands in for "the exchange ended without a parseable status
// line" so the %w wraps above always have a non-nil error to carry.
var errNoStatusLine = errors.New("no status line before the stream ended")

// isFastClose reports whether err is the peer hanging up without answering,
// which is the documented fail-fast outcome for the aggregate relay.
func isFastClose(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	// Substring matching as well: the relay's own half-close surfaces as a
	// syscall-level error that wraps neither io.EOF nor net.ErrClosed, and the
	// failover window must not count those as soak errors.
	msg := err.Error()
	return strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "use of closed")
}

// soakDrainBody reads a response body to EOF so the transport can complete the
// exchange and close the connection deterministically.
//
// The read error is REPORTED rather than swallowed: io.EOF is the normal end and
// returns nil, but a truncated or reset body means the relay severed a stream
// mid-flight, which is exactly the class of defect this harness exists to catch.
// Swallowing it (the previous `return total, nil` on any error) would have let a
// body-cutting bug pass as a clean read.
func soakDrainBody(resp *http.Response) (int64, error) {
	var total int64
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		total += int64(n)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, fmt.Errorf("read %d bytes then %w", total, err)
		}
	}
}

// ---- workers ----

// soakWorker runs iterations until stop is closed. paused/inFlight form a
// barrier: a quiesce() is only honoured once no worker is inside an iteration,
// which is what makes a "settled" sample an actual measurement of retention
// rather than of in-flight work.
type soakWorker struct {
	id       int
	rig      *soakRig
	stats    soakStats
	paused   *atomic.Bool
	inFlight *atomic.Int64
	ticks    *atomic.Int64
	stop     <-chan struct{}
}

// run is the one place where the pause protocol must be exactly right.
//
// The ordering here is load-bearing and was a deadlock in the first draft: a
// worker that incremented inFlight and THEN waited for paused to clear held its
// in-flight slot for as long as the pause lasted, so quiesce()'s "wait for
// inFlight == 0" could never be satisfied — the first quiesce spun until the
// test timeout. The rule is therefore:
//
//	check paused -> if set, idle WITHOUT holding a slot
//	take the slot
//	re-check paused -> if set, RELEASE the slot and idle again
//	run the iteration
//
// The re-check closes the window where quiesce sets paused between a worker's
// check and its increment; because a worker always releases before idling, the
// slot count is guaranteed to drain to zero.
func (w *soakWorker) run() {
	it := 0
	for {
		select {
		case <-w.stop:
			return
		default:
		}
		if w.paused.Load() {
			time.Sleep(soakQuiesceTick)
			continue
		}
		w.inFlight.Add(1)
		if w.paused.Load() {
			w.inFlight.Add(-1)
			continue
		}
		w.rig.iterate(&w.stats, it)
		w.inFlight.Add(-1)
		it++
		w.stats.iterations = it
		w.ticks.Add(1)
	}
}

// soakBarrier publishes pause/resume/quiesce across all workers.
type soakBarrier struct {
	paused   atomic.Bool
	inFlight atomic.Int64
}

// quiesce parks every worker and waits until none is inside an iteration. It
// reports whether the barrier actually drained: a false return means a worker
// was still mid-iteration past soakQuiesceBudget, so the sample that follows
// measures in-flight work rather than retention and must not be trusted.
func (b *soakBarrier) quiesce() bool {
	b.paused.Store(true)
	deadline := time.Now().Add(soakQuiesceBudget)
	for b.inFlight.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(soakQuiesceTick)
	}
	return b.inFlight.Load() == 0
}

func (b *soakBarrier) resume() { b.paused.Store(false) }

// ---- the xray child-process phase ----

// soakStubUsable reports whether the package's python fake-xray fixture can run
// here. It needs linux for /proc child accounting and python3 for the stub
// interpreter, so the child-process dimension of the soak is simply unavailable
// elsewhere — reported, not silently skipped.
func soakStubUsable() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	_, err := exec.LookPath("python3")
	return err == nil
}

// soakChildRig is everything the xray child-process phase needs. Built on the
// TEST goroutine (soakNewChildRig) and only driven from the soak's churn
// goroutine, because t.TempDir/t.Cleanup/t.Fatal are not safe off the test
// goroutine: t.Fatal there calls runtime.Goexit on the wrong goroutine and the
// test hangs instead of failing.
type soakChildRig struct {
	dir string
	sel *ProxySelector
	m   *ProxyManager

	// peak is the highest concurrent live-child count soakChildPeak observed.
	// Concurrent because the phase's own tick-boundary sampling would miss a
	// spawn+orphan+exit that happened entirely between two ticks — which is
	// exactly the shape of an orphan leak.
	peak atomic.Int64
	// peakViolation records the first over-ceiling observation, since a
	// goroutine cannot call t.Errorf once the test has returned.
	peakViolation atomic.Int64
}

// soakNewChildRig builds the child-process rig, or returns nil when the python
// stub is not usable on this platform. The selector goes through newTestSelector,
// so t.Cleanup(s.Stop) kills the serving child even if the phase dies early;
// TestMain's reaper is the backstop for anything that outlives the binary.
func soakNewChildRig(t *testing.T) *soakChildRig {
	t.Helper()
	if !soakStubUsable() {
		return nil
	}
	dir := t.TempDir()
	writeStubXray(t, dir)

	// httpPort points at a live acceptor rather than a free port: the watchdog
	// rebinds an unresponsive HTTP port by starting a bridge, and no bridge in
	// this process has a stop path, so pointing it at a dead port would leave
	// one permanent extra listener behind for a single log line.
	socksPort := freeLoopbackPort(t)
	httpAcceptor, closeAcceptor := soakBlackholeSocks(t, time.Hour)
	t.Cleanup(closeAcceptor)
	sel := newTestSelector(t, dir, "http://probe.invalid/", socksPort, splitPort(t, httpAcceptor))
	// TWO working candidates, not one. With a single good upstream every
	// SwitchToNextExcluding fails ("no working config found") and the serving
	// child is never actually swapped, so the orphan-prone path (stop old, start
	// winner, on the SAME serving port) would never run. With two, switches
	// really do rotate the child, and the two dead candidates keep the
	// all-candidates-fail branch exercised too.
	sel.UpdateConfigs([]ProxyConfig{
		e2eCand("soak-good-a", "soak-good-a:1", "good"),
		e2eCand("soak-good-b", "soak-good-b:1", "good"),
		e2eCand("soak-crash", "soak-crash:1", "dead-crash"),
		e2eCand("soak-hang", "soak-hang:1", "dead-hang"),
	})
	return &soakChildRig{
		dir: dir,
		sel: sel,
		m: &ProxyManager{
			instances: []*ProxySelector{sel},
			statuses:  []InstanceStatus{{Index: 0, Status: "starting", Name: "soak-child"}},
			xrayDir:   dir,
		},
	}
}

// live counts the children this rig is responsible for right now.
func (c *soakChildRig) live() int { return len(listXrayPIDs(c.dir)) }

// note records the current count, updating the peak and remembering the first
// ceiling breach instead of failing the test inline.
func (c *soakChildRig) note() {
	n := int64(c.live())
	if n > c.peak.Load() {
		c.peak.Store(n)
	}
	if n > int64(soakMaxXrayChildren) {
		c.peakViolation.CompareAndSwap(0, n)
	}
}

// soakChildPeak samples the live-child count concurrently for the life of the
// phase. It is what makes the ceiling meaningful: tick-boundary sampling alone
// cannot see a child that was spawned, orphaned and reaped between two ticks.
func soakChildPeak(c *soakChildRig, stop <-chan struct{}) {
	ticker := time.NewTicker(soakSampleEvery / 2)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			c.note()
		}
	}
}

// soakChildPhase churns real child processes: start, switch, health-check,
// watchdog orphan prune. It runs off the test goroutine, so it may not touch
// t for anything but Logf/Errorf. It returns the xray directory (for the
// /proc metric) and the peak concurrent child count observed.
func soakChildPhase(t *testing.T, c *soakChildRig, stop <-chan struct{}, gap *soakLimiter, stats *soakStats) (string, int) {
	t.Helper()
	if c == nil {
		t.Logf("soak: xray child-process phase UNAVAILABLE (needs linux + python3); "+
			"the %d-process ceiling is reported as not-measured", soakMaxXrayChildren)
		return "", -1
	}
	dir, sel, m := c.dir, c.sel, c.m

	peakDone := make(chan struct{})
	go func() {
		defer close(peakDone)
		soakChildPeak(c, stop)
	}()

	if err := sel.StartWithBest(); err != nil {
		t.Errorf("soak: stub StartWithBest failed: %v", err)
	} else {
		if n := c.live(); n != 1 {
			t.Errorf("soak: %d children after start, want exactly 1 serving child", n)
		}
		m.markOK(0, "soak-child", 5*time.Millisecond)
	}
	c.note()
	stats.childSwitches = 0

	for {
		select {
		case <-stop:
			// The whole point of the phase: after Stop, zero children must be
			// left holding ports. Stop is called here AND registered via
			// newTestSelector's t.Cleanup, so the child cannot escape even if
			// this phase returns early or the test aborts.
			sel.Stop()
			<-peakDone
			if n := c.live(); n != 0 {
				t.Errorf("soak: %d xray children survived Stop", n)
			}
			if v := c.peakViolation.Load(); v != 0 {
				t.Errorf("soak: %d concurrent xray children observed, ceiling is %d",
					v, soakMaxXrayChildren)
			}
			return dir, int(c.peak.Load())
		default:
		}
		if !gap.allow() {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		// A switch may legitimately fail (every candidate dead) or succeed
		// (the swap kills the old child and starts the new one). Both are the
		// churn we want; what must hold is that neither orphans a process.
		if err := sel.SwitchToNextExcluding(nil); err != nil {
			t.Logf("soak: switch found no working candidate (%v) - orphan accounting still applies", err)
		} else {
			stats.childSwitches++
		}
		for range healthFailThreshold {
			if !sel.HealthCheck() {
				break
			}
		}
		// The watchdog's port-sanity + orphan-prune path, including its
		// re-verify-before-kill grace window. Its prune is scoped to this rig's
		// own t.TempDir(), so it can never reap another test's children.
		m.WatchdogCheck()
		if c.live() == 0 {
			m.markDown(0, "soak: no child serving")
			if err := sel.StartWithBest(); err != nil {
				t.Logf("soak: restart after child loss failed: %v", err)
			} else {
				m.markOK(0, "soak-child", 5*time.Millisecond)
			}
		}
		stats.childTicks++
		c.note()
	}
}

// ---- the test ----

func TestSoakResourceBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: resource soak skipped")
	}
	if !soakEnabled() {
		t.Skipf("%s!=1: resource soak skipped (set %s=1 to run; see README_SOAK.md)", soakEnvGate, soakEnvGate)
	}
	if runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("no loopback TCP listeners on this platform")
	}

	budget := soakDuration(t)
	warmup := budget / 5
	if warmup > soakWarmupMax {
		warmup = soakWarmupMax
	}
	if warmup < time.Second {
		warmup = time.Second
	}
	main := budget - warmup
	if main < time.Second {
		main = time.Second
		warmup = budget / 2
	}
	t.Logf("soak: budget=%s (warmup %s + measure %s), %d workers, GOOS=%s",
		budget, warmup, main, soakWorkers, runtime.GOOS)

	rig := newSoakRig(t)

	var barrier soakBarrier
	var ticks atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	workers := make([]*soakWorker, 0, soakWorkers)
	for id := range soakWorkers {
		w := &soakWorker{
			id:       id,
			rig:      rig,
			paused:   &barrier.paused,
			inFlight: &barrier.inFlight,
			ticks:    &ticks,
			stop:     stop,
		}
		workers = append(workers, w)
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.run()
		}()
	}

	// The child-process phase runs alongside the connection churn: the two
	// dimensions of the leak (per-connection resources vs per-switch processes)
	// are independent, and overlapping them is closer to production anyway.
	// The rig is built HERE, on the test goroutine, because it needs
	// t.TempDir/t.Cleanup; only the churn loop runs off-goroutine.
	childRig := soakNewChildRig(t)
	if childRig == nil {
		t.Logf("soak: xray child-process phase UNAVAILABLE (needs linux + python3); "+
			"the %d-process ceiling is reported as not-measured", soakMaxXrayChildren)
	}
	// childDir is read on this goroutine, so it must NOT be assigned by the
	// churn goroutine (that was an unsynchronised write the race detector
	// rightly flags). The rig already knows its own directory, so it is read
	// here instead.
	childDir := ""
	if childRig != nil {
		childDir = childRig.dir
	}
	childStats := &soakStats{}
	childPeak := -1
	childDone := make(chan struct{})
	go func() {
		defer close(childDone)
		gap := soakLimiter{gap: soakSwitchGap}
		_, childPeak = soakChildPhase(t, childRig, stop, &gap, childStats)
	}()

	// ---- warmup, then the baseline ----
	warmDeadline := time.Now().Add(warmup)
	for time.Now().Before(warmDeadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if !barrier.quiesce() {
		t.Errorf("soak: workers did not quiesce within %s after warmup; the baseline sample "+
			"below would measure in-flight work, so every delta from it is meaningless", soakQuiesceBudget)
	}
	t.Logf("soak: warmup done, settling %s before the baseline sample", soakSettle)
	time.Sleep(soakSettle)
	// childDir is passed to BOTH samples on purpose: the baseline must be able
	// to see the serving child for the delta to mean anything, and the final
	// sample must be able to prove there is none left.
	baseline := soakQuietSample(childDir, 0)
	t.Logf("soak: baseline after warmup: goroutines=%d fds=%d heap=%dB egressKeys=%d slotsHeld=%d children=%d",
		baseline.goroutines, baseline.fds, baseline.heapAlloc, baseline.egressKeys,
		baseline.slotsHeld, baseline.children)
	barrier.resume()

	// ---- measured window ----
	measureDeadline := time.Now().Add(main)
	measureStart := time.Now()
	var samples []soakSample
	nextSample := time.Now().Add(soakSampleEvery)
	for time.Now().Before(measureDeadline) {
		time.Sleep(100 * time.Millisecond)
		if !time.Now().Before(nextSample) {
			nextSample = time.Now().Add(soakSampleEvery)
			s := soakSampleNow(childDir, ticks.Load())
			samples = append(samples, s)
			if len(samples) <= soakMaxSamples {
				t.Logf("soak: +%s goroutines=%d fds=%d heap=%dB slotsHeld=%d children=%d iterations=%d",
					time.Since(measureStart).Truncate(time.Second),
					s.goroutines, s.fds, s.heapAlloc, s.slotsHeld, s.children, s.iterations)
			}
		}
	}

	// ---- quiesce and take the final sample ----
	close(stop)
	if !barrier.quiesce() {
		t.Errorf("soak: workers did not quiesce within %s after the measure window; "+
			"the final sample would measure in-flight work", soakQuiesceBudget)
	}
	wg.Wait()
	// Waiting here (not just closing stop) is what guarantees the child phase
	// has run sel.Stop() before the final sample is taken: the xray-child delta
	// is only meaningful once its Stop has completed.
	<-childDone
	t.Logf("soak: measure window done, settling %s before the final sample", soakSettle)
	time.Sleep(soakSettle)
	final := soakQuietSample(childDir, 0)

	// ---- report ----
	// merge(src, dst) accumulates src INTO dst. The receiver being the SOURCE
	// is deliberate and load-bearing: written the other way round (total.merge
	// as dst) the workers' tallies are copied into each worker's own struct and
	// `total` stays zero, which silently reports "0 GETs, 0 errors" and turns
	// every coverage floor into an unconditional failure while hiding the real
	// numbers entirely.
	var total soakStats
	for _, w := range workers {
		w.stats.merge(&total)
	}
	total.childTicks = childStats.childTicks
	total.childSwitches = childStats.childSwitches

	iterations := sumIterations(workers)
	t.Logf("soak: %d iterations across %d workers in %s (%.1f iterations/s aggregate)",
		iterations, soakWorkers, budget, float64(iterations)/budget.Seconds())
	t.Logf("soak: bridge GETs=%d (ok %d) tunnels=%d (ok %d) aggregateSocks=%d aggregateHTTP=%d bytesThroughRelay=%d",
		total.bridgeGets, total.bridgeOK, total.tunnels, total.tunnelOK,
		total.aggSocks, total.aggHTTP, total.aggBytesMoved)
	t.Logf("soak: dead=%d/%d hungDial=%d/%d hungConnect=%d/%d failovers=%d (fast %d)",
		total.deadOK, total.deadProbes, total.holeOK, total.holeProbes,
		total.hangOK, total.hangProbes, total.failoverFast, total.failovers)
	t.Logf("soak: per-path mean latency: get=%s tunnel=%s aggSocks=%s aggHTTP=%s dead=%s "+
		"hungDial=%s hungConnect=%s failover=%s",
		soakMean(total.nGet.Load(), total.cGet.Load()),
		soakMean(total.nTun.Load(), total.cTun.Load()),
		soakMean(total.nAggS.Load(), total.cAggS.Load()),
		soakMean(total.nAggH.Load(), total.cAggH.Load()),
		soakMean(total.nDead.Load(), total.cDead.Load()),
		soakMean(total.nHole.Load(), total.cHole.Load()),
		soakMean(total.nHang.Load(), total.cHang.Load()),
		soakMean(total.nFailover.Load(), total.cFailover.Load()))
	if childDir != "" {
		t.Logf("soak: xray child ticks=%d (successful swaps %d) peak concurrent children=%d "+
			"(ceiling %d), 0 after Stop",
			total.childTicks, childStats.childSwitches, childPeak, soakMaxXrayChildren)
	}

	t.Logf("soak: growth vs the quiesced baseline (absolutes are machine-specific; only deltas are asserted):")
	t.Logf("%s", baseline.line(metricGoroutines, final, fmt.Sprintf("budget +%d", soakMaxGoroutineGrowth)))
	if baseline.fds >= 0 && final.fds >= 0 {
		t.Logf("%s", baseline.line(metricFDs, final, fmt.Sprintf("budget +%d", soakMaxFDGrowth)))
	} else {
		t.Logf("  %-14s unavailable on %s (no /proc/self/fd); metric skipped, not failed", metricFDs, runtime.GOOS)
	}
	t.Logf("  %-14s baseline=%-12d final=%-12d delta=%+d (budget +%dB)",
		metricHeap, baseline.heapAlloc, final.heapAlloc,
		int64(final.heapAlloc)-int64(baseline.heapAlloc), int64(soakMaxHeapGrowth))
	t.Logf("%s", baseline.line(metricEgress, final, fmt.Sprintf("budget +%d", soakMaxEgressKeyGrowth)))
	t.Logf("%s", baseline.line(metricSlots, final, fmt.Sprintf("budget +%d", soakMaxSlotGrowth)))
	if final.children >= 0 {
		t.Logf("%s", baseline.line(metricChildren, final, fmt.Sprintf("budget +%d", soakMaxXrayChildren)))
	} else {
		t.Logf("  %-14s not measured (no stub xray available on this platform)", metricChildren)
	}
	// Mid-window peak: a monotonic ramp is the leak signature, and the
	// baseline-to-final delta alone hides it when the leak is later released.
	soakReportPeaks(t, samples)

	// ---- assertions ----
	var failures []string
	if d := final.goroutines - baseline.goroutines; d > soakMaxGoroutineGrowth {
		failures = append(failures, fmt.Sprintf("goroutines grew %+d (budget +%d): %d -> %d",
			d, soakMaxGoroutineGrowth, baseline.goroutines, final.goroutines))
	}
	if baseline.fds >= 0 && final.fds >= 0 {
		if d := final.fds - baseline.fds; d > soakMaxFDGrowth {
			failures = append(failures, fmt.Sprintf("open fds grew %+d (budget +%d): %d -> %d",
				d, soakMaxFDGrowth, baseline.fds, final.fds))
		}
	}
	if d := int64(final.heapAlloc) - int64(baseline.heapAlloc); d > soakMaxHeapGrowth {
		failures = append(failures, fmt.Sprintf("retained heap grew %+dB (budget +%dB): %dB -> %dB",
			d, int64(soakMaxHeapGrowth), baseline.heapAlloc, final.heapAlloc))
	}
	if d := final.egressKeys - baseline.egressKeys; d > soakMaxEgressKeyGrowth {
		failures = append(failures, fmt.Sprintf("passive-health registry keys grew %+d (budget +%d): %d -> %d",
			d, soakMaxEgressKeyGrowth, baseline.egressKeys, final.egressKeys))
	}
	if d := final.slotsHeld - baseline.slotsHeld; d > soakMaxSlotGrowth {
		failures = append(failures, fmt.Sprintf("connection slots held grew %+d (budget +%d): %d -> %d "+
			"(a tunnel that never released its slot; MAX_CONNS is %d by default)",
			d, soakMaxSlotGrowth, baseline.slotsHeld, final.slotsHeld, getMaxConns()))
	}
	if final.children >= 0 && final.children != 0 {
		// Not a growth budget: the guarantee is absolute. Every selector goes
		// through newTestSelector (t.Cleanup -> Stop) and the phase Stop()s
		// explicitly before returning, so a surviving child is an orphan
		// holding an ephemeral port — the exact failure that has broken this
		// suite before. Asserted here as well as inside the phase so it is
		// reported even if the phase returned early.
		failures = append(failures, fmt.Sprintf("%d xray children still alive after the phase's Stop: "+
			"an orphan is holding a listening port", final.children))
	}
	if total.errs.n > 0 {
		failures = append(failures, fmt.Sprintf("%d request/relay errors, e.g. %s",
			total.errs.n, strings.Join(total.errs.samples, " | ")))
	}

	// Coverage floors: growth bounds proved over no work prove nothing. Each
	// floor is scaled by the window actually measured (soakFloorFast) or by the
	// path's own rate limit (soakFloorSlow), so a 10s smoke run is not judged
	// against a 2h run's expectations.
	for _, c := range []struct {
		name     string
		got, min int
	}{
		{"bridge GETs", total.bridgeOK, soakFloorFast(soakMinBridgeGets, main)},
		{"CONNECT tunnels", total.tunnelOK, soakFloorFast(soakMinTunnels, main)},
		{"aggregate relays", total.aggSocks + total.aggHTTP, soakFloorFast(soakMinAggregate, main)},
		{"dead-upstream probes", total.deadOK, soakFloorFast(2, main)},
		{"hung-dial probes", total.holeOK, soakFloorSlow(soakHoleGap, main)},
		{"hung-CONNECT probes", total.hangOK, soakFloorSlow(soakHangGap, main)},
		{"aggregate failovers", total.failoverFast, soakFloorSlow(soakFailoverGap, main)},
	} {
		if c.got < c.min {
			failures = append(failures, fmt.Sprintf("%s completed %d times, floor is %d: the soak did not "+
				"exercise enough of this path for its growth bound to mean anything", c.name, c.got, c.min))
		}
	}

	for _, f := range failures {
		t.Errorf("soak: %s", f)
	}
	if len(failures) == 0 {
		t.Logf("soak: PASS — every bounded-growth assertion held over %s of churn", budget)
	}
}

// soakReportPeaks logs the highest mid-window value of each metric. Not an
// assertion: these samples are taken WHILE work is in flight, so their
// magnitude is meaningless in absolute terms. They are here because a leak that
// ramps and is later released shows up here and not in the quiesced
// baseline-to-final delta, and a reader diagnosing a failure needs the shape of
// the curve, not just its endpoints.
func soakReportPeaks(t *testing.T, samples []soakSample) {
	t.Helper()
	if len(samples) == 0 {
		t.Logf("soak: no mid-window samples taken")
		return
	}
	peak := func(pick func(soakSample) int) int {
		best := 0
		for _, s := range samples {
			if v := pick(s); v > best {
				best = v
			}
		}
		return best
	}
	fdPeak := -1
	for _, s := range samples {
		if s.fds > fdPeak {
			fdPeak = s.fds
		}
	}
	t.Logf("soak: mid-window peaks over %d in-flight samples: goroutines=%d fds=%d heap=%dB slotsHeld=%d children=%d",
		len(samples),
		peak(func(s soakSample) int { return s.goroutines }),
		fdPeak,
		peak(func(s soakSample) int { return int(s.heapAlloc) }),
		peak(func(s soakSample) int { return s.slotsHeld }),
		peak(func(s soakSample) int { return s.children }),
	)
}

// soakQuietSample takes soakQuietSamples samples from an idle process and
// returns their MEDIAN per metric. A median rather than a minimum because a
// single minimum can be an outlier low, and rather than a maximum because the
// runtime legitimately parks GC workers and its own bookkeeping goroutines.
//
// childDir is the xray directory to count live children in, or "" for none. It
// is passed explicitly rather than left as "" because the xray-child delta is
// only meaningful if BOTH ends of it can see children: the baseline should show
// the serving child, the final sample zero once the phase has run Stop.
func soakQuietSample(childDir string, iterations int64) soakSample {
	g := make([]int, 0, soakQuietSamples)
	f := make([]int, 0, soakQuietSamples)
	h := make([]uint64, 0, soakQuietSamples)
	e := make([]int, 0, soakQuietSamples)
	s := make([]int, 0, soakQuietSamples)
	c := make([]int, 0, soakQuietSamples)
	for range soakQuietSamples {
		runtime.GC()
		time.Sleep(soakQuietGap)
		sample := soakSampleNow(childDir, iterations)
		g = append(g, sample.goroutines)
		h = append(h, sample.heapAlloc)
		e = append(e, sample.egressKeys)
		s = append(s, sample.slotsHeld)
		if sample.fds >= 0 {
			f = append(f, sample.fds)
		}
		if sample.children >= 0 {
			c = append(c, sample.children)
		}
	}
	out := soakSample{
		goroutines: soakMedian(g),
		fds:        -1,
		heapAlloc:  soakMedianU64(h),
		egressKeys: soakMedian(e),
		children:   -1,
		iterations: iterations,
	}
	if len(f) > 0 {
		out.fds = soakMedian(f)
	}
	if len(c) > 0 {
		out.children = soakMedian(c)
	}
	if connSem != nil {
		out.slotsHeld = soakMedian(s)
	}
	return out
}

// soakMedian is the middle sample of v. Copies before sorting so the caller's
// slice keeps its sampling order (which is what makes the median debuggable
// against the raw per-sample log).
func soakMedian(v []int) int {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.SortFunc(s, cmp.Compare)
	return s[len(s)/2]
}

func soakMedianU64(v []uint64) uint64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.SortFunc(s, cmp.Compare)
	return s[len(s)/2]
}

// soakMean formats the mean of n nanoseconds over calls samples as a duration,
// reporting "-" when the path never ran.
func soakMean(n, calls int64) string {
	if calls == 0 {
		return "-"
	}
	return (time.Duration(n / calls)).String()
}

// sumIterations reports how many iterations the workers actually completed.
func sumIterations(workers []*soakWorker) int {
	total := 0
	for _, w := range workers {
		total += w.stats.iterations
	}
	return total
}
