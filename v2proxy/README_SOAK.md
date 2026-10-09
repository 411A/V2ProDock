# v2proxy resource soak

`TestSoakResourceBounded` (in `soak_test.go`) drives the daemon's real relay
data paths in a loop for a configurable duration and asserts that goroutines,
file descriptors, retained heap, connection slots and xray child processes do
not **grow** without bound.

It is not a benchmark and it is not a correctness test for behaviour. It exists
because the daemon holds a TCP relay per proxied connection, a goroutine pair
per relay direction, a 32KiB pooled buffer per direction, one connection-slot
semaphore entry per live tunnel, and it spawns/kills an xray **child process**
per upstream switch. None of that shows up in a unit test: a leak is invisible
until a long-lived tunnel-serving process runs for hours.

## Running it

The soak is gated. `go test ./...` stays fast and hermetic because the test only
runs when the operator opts in, and never under `-short`:

```
V2PRODOCK_SOAK=1 go test -run TestSoak -count=1 -v .
```

| Variable | Meaning | Default |
| --- | --- | --- |
| `V2PRODOCK_SOAK` | `1` enables the soak. Any other value skips it. | unset (skip) |
| `V2PRODOCK_SOAK_SECONDS` | Total budget in seconds. Floored at 10s, capped at 24h. A non-integer is a fatal typo, not a silent default. | 60 |

### Short run (a smoke check, ~45s wall)

```
wsl.exe -d Ubuntu -- bash -c "cd /mnt/x/Projects/V2ProDock/v2proxy && \
  V2PRODOCK_SOAK=1 V2PRODOCK_SOAK_SECONDS=25 \
  go test -count=1 -run TestSoak -v -timeout 120s ./..."
```

Use WSL (or any Linux) because the fd metric reads `/proc/self/fd`; the soak
still runs on Windows and macOS, it just reports the fd column as unavailable.

### Long run (the real signal)

A budget of a few hours is where a per-connection leak becomes unmissable. Raise
the go test timeout past the budget or the harness handler will `os.Exit` and
skip every `t.Cleanup`, which is exactly how a child process escapes and
becomes an orphan holding an ephemeral port.

```
V2PRODOCK_SOAK=1 V2PRODOCK_SOAK_SECONDS=7200 \
  go test -run TestSoak -count=1 -v -timeout 7500s .
```

### Under the race detector

```
V2PRODOCK_SOAK=1 V2PRODOCK_SOAK_SECONDS=25 go test -race -run TestSoak -count=1 -v .
```

**This currently fails, on a production defect, not on the harness.** See
[Known limitation](#known-limitation-one-real-data-race) below.

## What each iteration exercises

Only the **far side** of the wire is stubbed; the daemon's own code is the real
thing. Four workers run the path set concurrently against one rig:

- **HTTP bridge plain GET** through a `serveSocks204("ok")` upstream.
- **HTTP bridge CONNECT**: dial, exchange a request/response, hold open, close.
  This is the probe that proves bytes move both ways through the relay pair.
- **Aggregate relay on both listeners**, routed by the real
  `ProxyManager.pickBestBackend` over real `*ProxySelector` instances. The
  live instance's SOCKS port *is* the mini-SOCKS stub and its HTTP port *is* the
  live bridge, so one aggregate request traverses aggregate → bridge → SOCKS.
- **Dead upstream** (closed SOCKS port → 502). Must fail fast.
- **Hung-upstream dial timeout** (port accepts, never answers the greeting → 504).
  This is the path whose abandoned goroutine used to strand an fd per timeout.
- **Hung-upstream CONNECT** (SOCKS handshake completes, then silence): a client
  abandoning a *live* tunnel. The fd + slot + goroutine-pair hazard.
- **Connection-slot semaphore**: asserted back at capacity after the settle.
- **Per-iteration aggregate failover**: mark the live instance down, require the
  relay to fail fast on the dead backend, restore it.
- **xray child churn** (linux + python3 only): `StartWithBest`,
  `SwitchToNextExcluding`, `HealthCheck`, `WatchdogCheck` — real
  start/switch/health/prune cycles against the package's python fake-xray, with
  the live-child count asserted by a concurrent sampler and zero asserted after
  `Stop`.

Slow paths are rate limited (`soakHoleGap` 6s, `soakHangGap` 8s,
`soakFailoverGap` 1.5s, `soakSwitchGap` 2s) so the *fixtures'* footprint does
not swamp the thing being measured. That is also why the coverage floors for
those paths are derived from their own gap rather than from the iteration count.

## The metrics, and what a failure means

Every number is a **delta against a baseline** sampled from a fully quiesced
process after a warmup window. Absolute counts are meaningless across machines
(GOMAXPROCS, pre-existing package fixtures, GC timing), so nothing here asserts
an absolute number.

The baseline is the **median of 5 samples**, each preceded by a forced
`runtime.GC()` and a 200ms gap. A median rather than a minimum (a minimum can be
an outlier low) and rather than a maximum (the runtime legitimately parks GC
workers and its own bookkeeping goroutines).

| Metric | Source | What growth means |
| --- | --- | --- |
| `goroutines` | `runtime.NumGoroutine()` | A relay pair, a `net/http` conn goroutine, or a dial-timeout orphan closer is not being retired. This is the broadest net: almost any per-connection leak shows here first. |
| `fds` | `len(os.ReadDir("/proc/self/fd"))` | A socket is not being closed. Closing a loopback socket releases the descriptor immediately — the kernel keeps a `TIME_WAIT` entry, not an fd — so a correct daemon's count is flat and the listener set is fixed for the whole run. |
| `heap_bytes` | `runtime.ReadMemStats().HeapAlloc`, post-GC | Memory is being *retained*, not merely allocated. Because GC is forced before both ends of the comparison, this is a retention measurement rather than a garbage-rate measurement. |
| `egress_keys` | `len(egressNotes.lastOK)` | The passive-health registry is growing a key per connection instead of one per fixed instance port — e.g. a key minted from an unparsed port. |
| `slots_held` | `cap(connSem) - len(connSem)` | A tunnel never released its connection slot. The sharpest assertion in the file: `connSem` is pre-filled to cap, so the budget is exactly 0 and any slot still held after the settle is unreleased. With `MAX_CONNS` at its 128 default an unreleased slot is caught after 128 tunnels, long before the goroutine/fd counts drift enough to notice. |
| `xray_children` | `listXrayPIDs(childDir)` (linux `/proc`) | Processes are being spawned without being reaped. Asserted as an absolute 0 after `Stop`, plus a concurrent-ceiling check while running. |
| coverage floors | per-path counters | The soak did not exercise enough of a path for its growth bound to mean anything. A soak that silently tested nothing would "prove" bounded growth by never growing. |

A failure also fires if **any** request/relay error was recorded during the run:
the error count is an assertion in its own right, with the first three messages
attached as the diagnosis.

Mid-window peaks are logged but not asserted. Those samples are taken *while
work is in flight*, so their magnitude is meaningless in absolute terms — they
are there because a leak that ramps and is later released shows up there and not
in the quiesced baseline-to-final delta, and because a reader diagnosing a
failure needs the shape of the curve, not just its endpoints.

## The thresholds and why they are what they are

Every threshold is a **growth budget over a warm, quiesced baseline**, never an
absolute count. The shared reasoning: derive the budget from the size of the
resource that **one leak unit retains**, so that

```
budget / bytes-per-leak  <<  iterations performed in the window
```

A correct daemon has nothing to accumulate — every connection is closed before
the next one opens — so the only legitimate contribution to the delta is the
unwinding tail of the previous burst plus whatever the fixtures still hold. The
numbers are therefore "many leak units", not "a bit of noise".

| Threshold | Value | Justification |
| --- | --- | --- |
| goroutines | `+40` | A relay pair is 2 goroutines plus 1 `net/http` conn goroutine per live connection, so one leaked connection is ~3 goroutines. At the observed rate (below) 40 is reached by a per-connection leak within a couple of seconds of the window opening, while leaving >10x headroom over the single-digit tail a loaded box shows. |
| fds | `+32` | One leaked descriptor per unclosed connection per side. 32 tolerates a couple of dozen descriptors still held by unwinding relays and by the SOCKS/HTTP fixtures, and still catches a per-tunnel fd leak inside the first second of the window. |
| heap | `+8MiB` | The pooled relay buffer is `relayBufSize` (32KiB) and a connection holds two of them, so a buffer-per-connection leak reaches 8MiB after ~128 connections — well inside the minimum 10s budget — whereas GC pacing on a loaded box moves `HeapAlloc` by a few MiB between samples. |
| egress keys | `+8` | Every port in play is fixed for the run, so the key count should not move at all. +8 absorbs keys synthesised from ports the OS merely recycled across tests, while still failing if a key is minted per connection. |
| slots | `+0` | Not a guess: `cap(connSem) - len(connSem)` is exactly 0 when the semaphore is at rest. Any deviation is an unreleased slot. |
| xray children | `<= 1 + switchWorkerCount()` (= 4 by default) | Derived, not invented. `SwitchToNextExcluding` runs `min(switchWorkerCount(), candidates)` throwaway probes concurrently — each holding a live child — *while the old child keeps serving*, so `1 + switchWorkerCount()` is the honest concurrent bound. Checked from a concurrent sampler every 2.5s, not only between ticks, because a child spawned, orphaned and reaped inside one tick would be invisible to tick-boundary sampling. |

Coverage floors are **scaled by the window actually measured** (`soakFloorFast`)
or by each path's own rate limit (`soakFloorSlow`, one tick of slack), so a 10s
smoke run is not judged against a 2h run's expectations and a rate-limited path
is not held to an impossible iteration count.

### Observed numbers (25s budget, linux/WSL, Go 1.26)

25-second budget, linux/WSL, Go 1.26.4:

```
budget=25s (warmup 5s + measure 20s), 4 workers
383 iterations across 4 workers (15.3 iterations/s aggregate)
bridge GETs=1920 (ok 1920) tunnels=770 (ok 770)
aggregateSocks=383 aggregateHTTP=383 bytesThroughRelay=766
dead=383/383 hungDial=5/5 hungConnect=4/4 failovers=18 (fast 18)
per-path mean latency: get=473µs tunnel=101ms aggSocks=610µs aggHTTP=839µs
                       dead=355µs hungDial=5.001s hungConnect=101ms failover=177µs
xray child ticks=8 (successful swaps 8) peak concurrent children=2 (ceiling 4), 0 after Stop

  goroutines     baseline=58  final=23  delta=-35  (budget +40)
  fds            baseline=37  final=29  delta=-8   (budget +32)
  heap_bytes     baseline=597696  final=412736  delta=-184960 (budget +8388608)
  egress_keys    baseline=2   final=2   delta=+0   (budget +8)
  slots_held     baseline=0   final=0   delta=+0   (budget +0)
  xray_children  baseline=2   final=0   delta=-2   (ceiling 4)
mid-window peaks: goroutines=63 fds=46 heap=1380336B slotsHeld=1 children=2
PASS
```

The 60-second default budget, same machine:

```
921 iterations across 4 workers (15.3 iterations/s aggregate)
bridge GETs=4615 (ok 4615) tunnels=1850 (ok 1850) bytesThroughRelay=1842
dead=921/921 hungDial=10/10 hungConnect=8/8 failovers=39 (fast 39)
xray child ticks=16 (successful swaps 16) peak concurrent children=2 (ceiling 4), 0 after Stop

  goroutines     baseline=52  final=30  delta=-22  (budget +40)
  fds            baseline=36  final=36  delta=+0   (budget +32)
  heap_bytes     baseline=672248  final=417368  delta=-254880 (budget +8388608)
  egress_keys    baseline=2   final=2   delta=+0   (budget +8)
  slots_held     baseline=0   final=0   delta=+0   (budget +0)
  xray_children  baseline=2   final=0   delta=-2   (ceiling 4)
mid-window peaks: goroutines=82 fds=61 heap=3129712B slotsHeld=3 children=2
PASS
```

Every growth delta is **negative**: after the churn stops, the process holds
*less* than it did at the quiesced baseline, because the baseline still carries
the child phase's serving children and the warm tail. The point is the sign of
the delta, not its magnitude — a leak shows up as a positive delta climbing with
iteration count, not as a large number.

Roughly **15 iterations/s** aggregate across 4 workers, i.e. ~1900 bridge GETs,
~770 CONNECT tunnels and ~760 relayed requests in 20 seconds. At that rate a
single leaked fd per tunnel would add ~770 descriptors inside one 20s window
against a budget of 32.

The `hungDial` mean of 5.001s is not a defect: it is `bridgeDialTimeout` doing
its job, and it is the assertion that it happens at all (`holeOK` counts only
probes that actually returned 504).

## Cleanup: no orphaned xray processes

The repo has a history of live orphaned xray children holding LISTENING
ephemeral ports, which then break later tests (a recycled port satisfies a
liveness check against the wrong process; a stub fails to bind inside
`launchXray`'s 100ms crash window). This harness starts children deliberately,
so it cleans up three ways:

1. Every selector is built via `newTestSelector`, which registers
   `t.Cleanup(s.Stop)`. The child phase's `Stop()` is also called explicitly on
   the shutdown path, so the child is killed even if the phase returns early.
2. The watchdog prune inside the phase is scoped to this rig's own
   `t.TempDir()`, so it can never reap another test's children.
3. `TestMain`'s orphan reaper in `main_test.go` is the backstop for anything
   that outlives the binary.

The rig is built on the **test goroutine** (`soakNewChildRig`) rather than inside
the churn goroutine: `t.TempDir`, `t.Cleanup` and `t.Fatal` are not safe off the
test goroutine — `t.Fatal` there calls `runtime.Goexit` on the wrong goroutine
and the test hangs instead of failing. Only the churn loop and the concurrent
peak sampler run off-goroutine, and both are restricted to `t.Logf`/`t.Errorf`.

The final assertion is absolute: **zero** live children after `Stop`, checked
both inside the phase and again on the final quiesced sample. An orphan holding
a port fails the soak rather than being left for the next run to trip over.

## Known limitation: one real data race

`go test -race` **fails** on this harness. The report is in
`handleConnect` (`httpproxy.go`):

```go
slotHeld := true
releaseSlot := func() {
    if slotHeld {      // <- unsynchronised read
        slotHeld = false // <- unsynchronised write
        connSem <- struct{}{}
    }
}
defer releaseSlot()          // handler goroutine
...
go func() {
    defer releaseSlot()      // relay goroutine
    ...
}()
```

`slotHeld` is a plain `bool` captured by the closure and touched from both the
handler goroutine (whose `defer` fires as soon as the tunnel is spliced) and the
relay goroutine (whose `defer` fires when both relay directions finish). Two
concurrent CONNECT tunnels are enough to trip the detector.

This is a **real production data race**, not a harness artefact: a bare loop of
200 CONNECT tunnels against a bridge, with no soak rig involved, reproduces it in
under two seconds. It is also not merely a detector complaint — an unsynchronised
check-then-act on a release-once flag can double-release the semaphore slot
(`connSem` over-filled, so `MAX_CONNS` silently stops bounding anything) or drop
a release entirely (a permanent slot leak). `httpproxy.go` was outside this
task's file ownership, so it is reported rather than fixed. The fix is a
`sync.Once` (or an `atomic.Bool` compare-and-swap) around the release.

Everything else in the harness is race-clean: the growth assertions, the
child-process accounting, the barrier protocol and the fixture wiring all hold
under `-race`.

## What this harness CANNOT detect

Being explicit about this matters more than the pass message.

- **Anything time-dependent beyond the budget.** A leak of one resource per
  *hour* is invisible to a 25s run. Long soaks are the only defence, and even a
  2h run cannot see a leak that needs a full upstream-refresh cycle or a
  production-shaped config reload, neither of which this rig drives.
- **Leaks that need real upstream behaviour.** The far side is a python stub
  answering 204. Paths that depend on a real proxy's TLS negotiation,
  fragmentation, `sockopt.dialerProxy` chaining, multi-host SNI, or genuinely
  large/slow transfers are not exercised. In particular the body-copy and
  `relayIdleDeadline` re-arm paths only ever see a zero-length body.
- **The real configuration-management paths.** `subscribe.go`, `urlcheck.go`,
  `refresh` and `reconcile` are not driven. Config churn is a plausible source of
  instance/index bookkeeping leaks and this harness would not see one.
- **Multi-instance behaviour at scale.** The aggregate rig has two instances and
  four workers. Pool sizes in the tens or hundreds, `probeWorkers` contention on
  a small box, and `AGG_MIN_STREAK` behaviour over a long-lived pool are out of
  scope.
- **Anything below the threshold within a short run.** A leak that retains less
  than its budget over the measured window passes. The floors and budgets are
  calibrated so a *per-connection* leak fails fast; a rarer, smaller leak needs
  a longer budget, and the delta being negative on a short run is not evidence
  that no leak exists.
- **Per-platform resources.** fds are linux-only (`/proc/self/fd`); on Windows
  and macOS the metric reports "unavailable, skipped, not failed". The
  xray-child dimension needs linux (`/proc`) **and** `python3` on PATH, and is
  reported as not-measured elsewhere rather than silently skipped. Goroutines,
  heap, egress keys and slots are portable.
- **Absolute values.** By construction every assertion is a delta against a
  quiesced baseline. A process that starts already holding 10,000 fds and stays
  there passes; only *growth* is a finding.
- **Correctness of behaviour.** The soak asserts bounded resource growth and
  that the degraded paths return the right status codes. It does not assert
  response contents, ordering, latency SLAs, or that a healthy upstream is never
  marked down. Those belong in the functional tests.
- **A quiesce that failed.** If the workers cannot be parked within
  `soakQuiesceBudget` (20s, chosen to exceed the slowest single operation the rig
  drives), the harness reports that explicitly and every delta from that sample
  is meaningless — a loud, deliberate failure rather than a silently meaningless
  measurement.