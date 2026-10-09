# Serving-traffic analysis: what `ServingTraffic()` can and cannot see

HEAD `2a466bf`, tree clean. Everything below is labelled **VERIFIED** (read in
source, or measured against the real xray binary / a live WSL kernel) or
**INFERRED** (reasoned from verified facts, not executed).

No existing file was modified. This document is the only new file.

---

## 1. Verification of the reported gap

### 1.1 The guard and its evidence — VERIFIED

```go
// selector.go:109-111
func (s *ProxySelector) ServingTraffic() bool {
	return egressActive([]int{s.httpPort, s.socksPort}, egressGrace)
}
```

`egressGrace` is 90s (`constants.go:203`). `egressActive`
(`httpproxy.go:170-180`) is a pure read of `egressNotes.lastOK`, keyed by int
port.

`noteEgress(port)` has exactly **three** production call sites — VERIFIED by
exhaustive grep over `*.go`:

| Site | Port credited |
|---|---|
| `httpproxy.go:227` (`handlePlainHTTP`, after response headers) | the instance **HTTP** port |
| `httpproxy.go:314` (`handleConnect`, wired into `relay` as `onFirstByte`) | the instance **HTTP** port |
| `aggregate.go:99` (`handleAggregateConn`, wired into `relay` as `onFirstByte`) | the **chosen backend's** port |

`noteEgress` drops `port <= 0` (`httpproxy.go:158-161`) — VERIFIED.

The SOCKS inbound is rendered as an xray inbound, not a Go listener —
VERIFIED at `selector.go:818-829` (`"tag": "socks-in"`, `"protocol": "socks"`,
`"listen": "0.0.0.0"`), launched as a child process at `selector.go:674`. No Go
code ever accepts on `socksPort`. So a client pinned directly to
`127.0.0.1:<socksPort>` moves bytes the daemon does not observe.

### 1.2 The claim that no production path credits `socksPort` is **WRONG** — REFUTED

The third call site does credit `socksPort`, in production, for every
client that arrives through the **aggregate SOCKS listener**.

```go
// aggregate.go:96-102
egress := func() {}
if _, p, perr := net.SplitHostPort(backend); perr == nil {
    if port, cerr := strconv.Atoi(p); cerr == nil {
        egress = func() { noteEgress(port) }
    }
}
```

`backend` comes from `aggregateCandidates(httpBackend)`, and for the SOCKS-side
aggregate (`httpBackend == false`, wired at `aggregate.go:34-40`) the port is:

```go
// aggregate.go:155-158
port := insts[s.Index].SOCKSPort()
if httpSide {
    port = insts[s.Index].HTTPPort()
}
```

So `127.0.0.1:27017` (the documented stable SOCKS endpoint) → `noteEgress(27019)`
for instance 0, and so on. This path is already covered by a test:
`aggregate_failover_test.go:322` (`TestAggregateEgressCreditsOnlyUsedBackend`)
asserts `egressActive([]int{live})` where `live` is a **SOCKS** port
(`backendManager(..., dead, dead+1000, live, live+1000)`, `stability_test.go:692-701`
pairs `ports[2i]` as the SOCKS port).

**The real gap is narrower than reported.** It is not "the `socksPort` half of
the slice can only be true in tests". It is:

> Clients that connect **straight to a per-instance SOCKS port**
> (`27019..27020`, published by `docker-compose.yml:19`) are invisible to
> `ServingTraffic()`. Clients on the aggregate (`27017`/`27016`) and clients on
> any per-instance HTTP bridge port are visible.

### 1.3 How much does that matter in practice — VERIFIED

`README.md:194-202` tells operators to pin long-lived clients at the aggregate
and explicitly warns that per-instance ports die on failover. So the
*documented, recommended* configuration is already protected.

But `docker-compose.yml:18-19` publishes `27000-27017` and `27019-27100`, the
API advertises per-instance `socks5` endpoints (`manager.go:140`), and
`README.md:92` documents them as the addressing scheme. Direct per-instance
SOCKS pinning is a supported, reachable configuration, and for it the guard in
`manager.go:690` reads `false` while live connections exist. **The defect is
real** — narrower in scope than stated, but a genuine cut-connection path that
`bc27675` was written to close and does not.

### 1.4 A second, related gap the report did not mention — VERIFIED

`noteEgress` is a **one-shot per connection** timestamp, not a byte rate
(`relay` nils `onFirstByte` after the first successful read,
`httpproxy.go:419-422`). A client that connects once and then sits idle on an
open tunnel has no note after 90s and reads as idle. For a Telegram bot doing
50s `getUpdates` the note is refreshed every cycle, so this is latent rather
than active — but it means "no recent bytes" is not the same as "nothing to
lose".

### 1.5 The rotation path that is actually affected — VERIFIED

`reconcileActive` (`manager.go:648-748`) has two rotating branches:

1. **vanished-from-pool** (`manager.go:670-680`) — forces a switch.
2. **slow-but-present** (`manager.go:690-747`) — guarded by `ServingTraffic()`
   at line 690, then by `LatencyFresh` at 700, then `LastLatency() <
   rotateSlowLatency` at 705.

Only branch 2 consults the guard. Branch 1 is reached when the key is a
duplicate across instances, or `failCount > 0`, or `prev == nil` — see §4.4.

---

## 2. Option (a): poll xray's stats API

### 2.1 What the binary actually supports — VERIFIED by inspection

Inspected `/tmp/xraydl/x/xray` (Xray 26.3.27, `d2758a0`, go1.26.1), not guessed:

```
$ xray help
  run / version / api / convert / tls / uuid / x25519 / wg / mldsa65 / mlkem768 / vlessenc / buildMphCache

$ xray help api
  stats, statsquery, statssys, bi, bo, adi, ado, rmi, rmo, lsi, lso,
  adu, rmu, inbounduser, inboundusercount, adrules, rmrules, lsrules,
  sib, statsonline, statsonlineiplist, statsgetallonlineusers
```

`StatsService` gRPC methods are present in the binary
(`*command.StatsServiceServer`, `GetStats`, `QueryStats` — VERIFIED via
`strings`). So the capability exists.

### 2.2 `docket` is gone in this build — VERIFIED

Every documented/legacy recipe fails on this binary:

```
protocol "docket" -> Failed to start: ... unknown config id: docket
protocol "https" -> unknown config id: https
protocol "grpc"  -> unknown config id: grpc
```

`grep -c docket` over the binary: **0 occurrences**. Any config carrying a
`docket` inbound (the classic Xray API recipe) will not boot here.

### 2.3 Two working shapes — VERIFIED

Measured end to end against the real binary, with a real SOCKS request through
the port (`curl --socks5` → HTTP 204), then `xray api statsquery`:

| # | api shape | `policy` block | result |
|---|---|---|---|
| A | simplified `api.listen` | none | `{}` — **no counters** |
| B | simplified `api.listen` | levels + system | `{"stat":[{"name":"inbound>>>socks-in>>>traffic>>>uplink","value":869},{"name":"inbound>>>socks-in>>>traffic>>>downlink","value":12}]}` |
| C | `tunnel` inbound tagged `api` + routing rule `inboundTag:["api"] → outboundTag:"api"` | none | `{}` — **no counters** |
| D | same as C | levels + system | full counter set incl. `inbound>>>api>>>traffic>>>` |

`statsquery -pattern ''` returns `{}` **without** a `policy` block even after
proven traffic. `inboundusercount` and `statsonline` are useless here — VERIFIED
errors: `handler not found` and `user>>>>>>online not found`, because a
`noauth` SOCKS inbound has no user accounts to count.

### 2.4 Would it break the fragment / `sockopt.dialerProxy` chain? — VERIFIED: no

`renderXrayConfig` (`selector.go:803-888`) composes `log`, `dns`, `inbounds`,
`outbounds`, `routing`. `api`, `stats` and `policy` are disjoint top-level
keys, and the fragment chain lives entirely in
`outbound.streamSettings.sockopt.dialerProxy` (`chainFragment`,
`selector.go:892-904`). A config combining a `dialerProxy` upstream, the
`frag-out` fragment carrier, `api`/`stats`/`policy`, and the bittorrent
blackhole rule boots clean on 26.3.27:

```
2026/10/09 20:49:16 [Info]  infra/conf/serial: Reading config: …
2026/10/09 20:49:16 [Warning] core: Xray 26.3.27 started
```

So (a) is **schema-compatible**. The problems are elsewhere.

### 2.5 Why (a) is still the wrong-sized fix

1. **An extra port per xray process.** Either `api.listen` or a `tunnel`
   inbound needs a port. `renderXrayConfig` is called for the serving config
   *and* for every throwaway probe config (`selector.go:728`, `seq > 0`), with
   up to `probeWorkers = 10` concurrent probes (`constants.go:18`). Every one
   of them would need its own collision-free API port on top of the ephemeral
   SOCKS port it already races for — strictly more of the exact
   port-contention class `selector.go:263-289` was written to fight.
2. **A gRPC client dependency.** The daemon has no gRPC; `go.mod` carries only
   `charmbracelet/lipgloss`, `charmbracelet/log`, `golang.org/x/net`. Polling
   means adding `google.golang.org/grpc` + protobuf codegen to a
   deliberately-lean daemon (`AGENT.md:62-67`: "the daemon stays lean"). The
   alternative — `exec` the xray binary's `api statsquery` and parse JSON —
   forks a ~21MB-binary process per instance per tick, and couples the daemon
   to an xray CLI surface that just demonstrably lost `docket`.
3. **It answers a different question.** Counters are cumulative bytes. To get
   "recent" you must sample, diff, and carry per-instance baseline state that
   must be re-baselined on every rotation (a fresh process starts at zero).
   §2.3 shows a `-pattern ''` query can return `{}` on a healthy, busy process,
   so a nil/empty result is indistinguishable from "idle" unless you add
   policy — another failure mode to guard.
4. **Client-side sockets are free.** The thing that actually needs protecting
   is "is a socket open", and the kernel already publishes that.

---

## 3. Option (b): count ESTABLISHED sockets on the serving port

### 3.1 Feasibility — VERIFIED against a live kernel

The daemon and its xray children share one network namespace (single container,
no `netns` exec), so `/proc/net/tcp{,6}` is the right table.

Measured with a real xray SOCKS inbound on `0.0.0.0:39401`:

```
baseline (no client):      tcp=0 tcp6=0
1 client tunnel held:      tcp=0 tcp6=1
3 client tunnels held:     tcp=0 tcp6=3
during the daemon's own
SOCKS probe:               tcp=0 tcp6=4
after all clients closed:  tcp=0 tcp6=0
```

Two implementation details this proved, both of which a naive
implementation gets wrong:

* **Both tables must be scanned.** A dual-stack listener on `0.0.0.0` appears
  in `/proc/net/tcp6` with v4-mapped addresses
  (`0000000000000000FFFF00000100007F:9A4D`), **not** in `/proc/net/tcp`. Raw
  row, VERIFIED:

  ```
  103: 0000000000000000FFFF00000100007F:9A4D 0000000000000000FFFF00000100007F:DB8A 01 00000000:00000000 00:00000000 00000000  1000  0 563375 …
  ```

  The IPv6 hex has no colons, so the local address contains exactly one `:` and
  `strings.Cut(fields[1], ":")` is correct for both tables.
* **Field 3 is the state**, `"01"` = `ESTABLISHED`. Only `ESTABLISHED` counts:
  a socket in `CLOSE_WAIT`/`TIME_WAIT` has already lost its peer, so the
  tunnel it belonged to is broken either way and must not block a rotation.

The parse was prototyped and run as a standalone binary against that live
listener; counts tracked held client tunnels 1:1 and returned 0 on an unrelated
port.

### 3.2 Blast radius

* `reconcileActive` gains one extra term in a guard that already `return`s
  before any candidate is probed. `rotateMaxCandidates` (3),
  `rotateBudget` (20s) and the `>=30% faster` margin
  (`manager.go:712-729`) are **untouched** — fewer rotation attempts, never
  more expensive ones.
* **Deliberately NOT wired into `HealthCheck`.** `HealthCheck`'s passive branch
  (`selector.go:399-405`) skips the *probe* and reports healthy. An open socket
  proves nothing about the upstream — a blackholed-but-open tunnel is the
  classic DPI case, and `processFinished`/`relayWithIdle` exist precisely
  because those sockets lie. Folding socket-presence into `ServingTraffic()`
  would let one stuck client make a dead upstream look healthy forever, and
  would break `TestHealthCheckStrikesDeadUpstreamWithoutTraffic`
  (`stability_test.go:435-445`). Keeping the two signals separate is a
  correctness requirement, not tidiness.
* **Cost.** Two small reads of a kernel-generated table, per instance, per
  120s refresh tick (`constants.go:125`). Negligible; the whole watchdog
  already does a `/proc` scan per tick (`watchdog.go:58-82`).
* **Known imprecision — stated plainly.** The daemon dials the serving port
  itself, during `HealthCheck`'s probe (`selector.go:409`), during
  `waitForPort`/`waitForPortFree` (`selector.go:967-992`), and during the
  watchdog's `tcpOpen` (`watchdog.go:165`). A probe in flight therefore reads
  as "busy". The error is one-sided and self-correcting: a legitimate rotation
  is deferred to the next 120s tick instead of severing a long-poll. It is the
  right direction to be wrong in, but it is a real flake surface for the two
  existing tests that assert rotation *happens* —
  `TestReconcileStaleLatencyNotRotated` (`e2e_test.go:427`) and
  `TestReconcileSlowIdleActiveStillRotates` (`e2e_test.go:466`). §5.2 hardens
  both.
* Non-Linux returns 0, which the guard reads as "no evidence" — exactly the
  pre-patch behaviour. It never freezes rotations on a platform we cannot
  measure.

### 3.3 It does NOT need build tags

`proc_unix.go` / `proc_windows.go` are build-tagged, but they only exist for
`syscall.Signal` / process-group work. `/proc` *reading* already has an
untagged precedent in this repo: `watchdog.go:58-62` guards
`listXrayPIDs` with `runtime.GOOS != "linux"` and returns empty. Following that
precedent keeps the diff to two existing files plus two test edits, and — more
importantly — lets the parser stay **untagged and therefore testable on
Windows**, where the e2e tier currently skips ~25 tests (`AGENT.md:33-36`).

---

## 4. Option (c): make the guard conservative

### 4.1 What "never rotate unless idleness is proven" means without a new evidence source

It means the rotation branch is dead. `reconcileActive` can only ever rotate
when it can prove idleness, and with the current evidence set that proof is
unreachable for exactly the case the branch exists to handle.

### 4.2 Measured blast radius

* `TestReconcileSlowIdleActiveStillRotates` (`e2e_test.go:466-499`) fails
  outright — it asserts the rotation happens.
* The pool freezes on one bad upstream permanently. The only remaining recovery
  is the health-check path, which needs `healthFailThreshold = 3` consecutive
  probe failures (`constants.go:50`). A *slow but functional* upstream
  (`lastLatency` just over `rotateSlowLatency = 2500ms`,
  `constants.go:91`) never trips that path. The daemon would serve a
  deliberately degraded node forever.
* That node's `OkStreak` keeps incrementing on every health tick
  (`selector.go:402`/`416`), and `aggMinStreak = 3` (`constants.go:211`) is a
  *minimum* qualification gate in `pickBestBackend` (`manager.go:265-273`).
  So the frozen slow node also monopolises the aggregate's preferred head —
  every new connection on `27017` is routed to it. A conservative guard with
  no evidence source converts a bounded latency problem into an unbounded
  concentration problem.
* `rotateMaxCandidates` / `rotateBudget` / the 30% margin become dead
  constants. `AGENT.md:62-67` treats shipped, exercised behaviour as
  "do not rebuild"; deleting the rotation feature is not a small change.

### 4.3 (c) is only correct *paired with* an evidence source

Once (b) exists, "idleness proven" becomes reachable: zero `ESTABLISHED`
sockets on the serving port **and** no byte note within `egressGrace`. That is
the same conservative posture, with a proof. Which is why the recommendation is
(b) expressed in conservative terms.

### 4.4 The vanished-from-pool branch is a separate product decision

`manager.go:670-680` forces a switch with **no** traffic guard. It is reached
when the active config left the refreshed pool and is *either* duplicated
across instances *or* already struck (`failCount > 0`) *or* there was no
previous active. Adding `ServingClients()` there is not obviously right:

* duplicate-key branch — switching is how duplicate serving is prevented;
  refusing would leave two instances on one upstream;
* `failCount > 0` branch — the health path already decided the upstream is
  bad, and skipping the switch strands a dead port.

This is a genuine product trade (never duplicate an upstream vs. never cut a
socket), it predates `bc27675`, and it is **out of scope for the minimal
patch**. Flagging it rather than silently widening the diff.

---

## 5. Recommendation

**Option (b), scoped to `reconcileActive`, wired as an additional
already-satisfied condition — and only once (c)'s conservatism is attached to
it.** Rationale:

* **Correctness.** It measures the thing rotation destroys. ESTABLISHED
  sockets on the serving port *are* the in-flight long-polls, including the
  ones the byte ledger can never see.
* **Blast radius.** One extra term in an early-return guard. Zero new
  dependencies, zero new ports, zero new xray config keys, no change to
  `HealthCheck`, no change to `renderXrayConfig` and therefore none to the
  fragment chain. `rotateMaxCandidates`, `rotateBudget`, `OkStreak` and
  `aggMinStreak` are all untouched.
* **Portability.** Untagged, `runtime.GOOS`-guarded like `listXrayPIDs`, and the
  parser is pure so it is unit-testable on every platform including Windows.
* **Failure direction.** Every imprecision errs toward *not* rotating.
* **Honest answer to "is this a product decision?"** Partly. The *guard
  semantics* are not — they follow directly from `bc27675`'s stated intent.
  What **is** a product decision, and is deliberately excluded: the
  vanished-from-pool branch (§4.4), and whether a client that keeps a socket
  open forever should be able to starve rotation indefinitely. The patch below
  resolves the latter in favour of the client's connection, consistent with the
  existing code, and says so in a comment.

### 5.1 The patch

**(a) `watchdog.go` — new section after `tcpOpen` (line 137), before
`managedPIDs`.**

Before — nothing exists:

```go
func tcpOpen(addr string, timeout time.Duration) bool {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func (m *ProxyManager) managedPIDs() map[int]bool {
```

After:

```go
func tcpOpen(addr string, timeout time.Duration) bool {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// ---- serving-connection accounting (rotation safety) ----
//
// egressNotes answers "did BYTES flow, recently?". This answers "does a client
// socket still stand on the serving port?". The two are not interchangeable:
// a client that dialled the SOCKS port and is now idle - or mid-long-poll,
// waiting on a response nobody is going to send - has proven nothing recently
// and still owns a connection that stopXray() would sever. That blind spot is
// exactly why a client pinned straight to a per-instance SOCKS port
// (27019..27100) could be rotated out from under a live tunnel: the bytes
// crossed the xray process, which the daemon never observes.
//
// ESTABLISHED (st 01) only. A socket in CLOSE_WAIT/TIME_WAIT has already lost
// its peer, so the tunnel it belonged to is broken either way and must never
// hold a rotation hostage. Counting any other state would let one abandoned
// connection freeze the pool.
//
// The daemon dials the serving port itself - the HealthCheck probe, the bind
// waits, the watchdog's tcpOpen - so a count above zero can be produced by our
// own traffic. That is the safe direction to err: a rotation is deferred to the
// next tick rather than severing a long-poll.

const tcpStateEstablished = "01"

// parseEstablishedOnPort counts ESTABLISHED sockets whose LOCAL port is port in
// one /proc/net/tcp{,6} table. Pure - table in, count out - so the format
// contract is unit-testable on every platform, including the ones with no
// /proc at all. The caller supplies the table.
//
// The local address is HEXADDR:HEXPORT with exactly one colon (the IPv6 hex
// form is colonless), and field 3 is the two-digit state. Header and short
// lines fall out on the length and state checks.
func parseEstablishedOnPort(table string, port int) int {
	if port <= 0 {
		return 0
	}
	want := fmt.Sprintf("%04X", port)
	n := 0
	for line := range strings.SplitSeq(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != tcpStateEstablished {
			continue
		}
		if localPort, ok := strings.Cut(fields[1], ":"); ok && localPort == want {
			n++
		}
	}
	return n
}

// establishedOnPort reports how many ESTABLISHED TCP sockets this host holds on
// port. Both tables are scanned because a dual-stack listener on 0.0.0.0 shows
// up in tcp6 with v4-mapped addresses and never in tcp. Non-Linux, or an
// unreadable /proc, yields 0 - read by every caller as "no evidence", the
// pre-existing behaviour, never as proof of idleness.
func establishedOnPort(port int) int {
	if runtime.GOOS != "linux" || port <= 0 {
		return 0
	}
	n := 0
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		n += parseEstablishedOnPort(string(raw), port)
	}
	return n
}

func (m *ProxyManager) managedPIDs() map[int]bool {
```

No new imports: `watchdog.go:16-24` already imports `fmt`, `net`, `os`,
`path/filepath`, `runtime`, `strconv`, `strings`, `time`.

**(b) `selector.go` — new method after `ServingTraffic` (line 111).**

Before:

```go
func (s *ProxySelector) ServingTraffic() bool {
	return egressActive([]int{s.httpPort, s.socksPort}, egressGrace)
}

// FailCount reports consecutive health failures on the current upstream (0 =
// never failed it). Used by refresh to tell "remote list rotated" apart from
// "proxy actually dying".
```

After:

```go
func (s *ProxySelector) ServingTraffic() bool {
	return egressActive([]int{s.httpPort, s.socksPort}, egressGrace)
}

// ServingClients reports whether client sockets are currently open on this
// instance's SOCKS port - the port xray itself owns, and therefore the one
// stopXray() severs.
//
// Deliberately NOT folded into ServingTraffic, and the two must not drift into
// one another: ServingTraffic means "proven responsive by recent bytes", which
// is why HealthCheck may skip its probe on it, whereas an open socket means
// only "something is connected" - a blackholed-but-open tunnel is the defining
// DPI failure and looks identical from here. Wiring this into HealthCheck would
// let one wedged client report a dead upstream as healthy indefinitely.
//
// Only socksPort is inspected: every severed connection terminates there. The
// HTTP bridge listener lives in this process and survives a rotation, so its
// sockets are not what a rotation destroys.
func (s *ProxySelector) ServingClients() bool {
	return establishedOnPort(s.socksPort) > 0
}

// FailCount reports consecutive health failures on the current upstream (0 =
// never failed it). Used by refresh to tell "remote list rotated" apart from
// "proxy actually dying".
```

**(c) `manager.go:690-694` — the guard itself.**

Before:

```go
	if inst.ServingTraffic() {
		debugLog("Instance %d: serving client traffic, slow rotation skipped (active %s at %dms)",
			i, shortName(active.Name), inst.LastLatency().Milliseconds())
		return
	}
```

After:

```go
	// ServingTraffic only sees ports THIS process serves. A client pinned
	// straight to a per-instance SOCKS port moves its bytes across the xray
	// child, which we never observe, so the byte ledger alone would call that
	// instance idle while its long-polls are live. ServingClients reads the
	// sockets directly. Together they make idleness a PROVEN condition: no
	// recent bytes AND nothing connected.
	if inst.ServingTraffic() || inst.ServingClients() {
		debugLog("Instance %d: serving client traffic, slow rotation skipped (active %s at %dms)",
			i, shortName(active.Name), inst.LastLatency().Milliseconds())
		return
	}
```

### 5.2 The tests

**(a) `resilience_test.go` — pure parser, runs on every platform.** Insert
before `TestCmdlineIsManaged` (`resilience_test.go:338`), next to the existing
`/proc` format test. Rows are verbatim captures from the live tables measured
in §3.1.

```go
// TestParseEstablishedOnPort pins the /proc/net/tcp contract the rotation
// guard leans on: ESTABLISHED only, LOCAL port only, header and short lines
// skipped. A regression here is silent in both directions - counting too few
// severs live long-polls again, counting too many freezes the pool - so the
// fixtures are real captured rows, including the v4-mapped tcp6 form.
func TestParseEstablishedOnPort(t *testing.T) {
	const table = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0000000000000000FFFF00000100007F:9A4D 0000000000000000FFFF00000100007F:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 565275 1 0000000000000000 100 0 0 10 0
 103: 0000000000000000FFFF00000100007F:9A4D 0000000000000000FFFF00000100007F:DB8A 01 00000000:00000000 00:00000000 00000000  1000        0 563375 1 0000000000000000  20 4 31 10 -1
 104: 0000000000000000FFFF00000100007F:9A4D 0000000000000000FFFF00000100007F:DB8B 06 00000000:00000000 00:00000000 00000000  1000        0 563376 1 0000000000000000  20 4 31 10 -1
 105: 0000000000000000FFFF00000100007F:DB8A 0000000000000000FFFF00000100007F:9A4D 01 00000000:00000000 00:00000000 00000000  1000        0 563377 1 0000000000000000  20 4 31 10 -1
`
	const port = 0x9A4D // 39549
	if got := parseEstablishedOnPort(table, port); got != 1 {
		t.Fatalf("established on the serving port = %d, want 1 (LISTEN and TIME_WAIT excluded)", got)
	}
	// The peer-side row has the serving port as its REMOTE address: that is the
	// client's socket, not ours, and must never count.
	if got := parseEstablishedOnPort(table, 0xDB8A); got != 1 {
		t.Fatalf("established on the client port = %d, want 1", got)
	}
	if got := parseEstablishedOnPort(table, 0xFFFF); got != 0 {
		t.Fatalf("unrelated port = %d, want 0", got)
	}
	if got := parseEstablishedOnPort("", port); got != 0 {
		t.Fatalf("unreadable table = %d, want 0 (no evidence is not evidence of idleness)", got)
	}
	if got := parseEstablishedOnPort(table, 0); got != 0 {
		t.Fatalf("port 0 = %d, want 0", got)
	}
}
```

**(b) `e2e_test.go` — the regression itself: a live socket, zero byte evidence.**
Insert after `TestReconcileSlowServingActiveRetained` (ends `e2e_test.go:393`).

```go
// The blind spot bc27675 left open: egressNotes sees only ports THIS process
// serves, and the per-instance SOCKS inbound is served by the xray child. A
// client pinned straight to that port therefore moved bytes the daemon never
// observed - ServingTraffic() read false, reconcileActive rotated, and the live
// tunnel died mid-response. ServingClients() looks at the socket instead.
func TestReconcileSlowActiveWithOpenClientNotRotated(t *testing.T) {
	needStub(t)
	dir := t.TempDir()
	writeStubXray(t, dir)
	socks, httpP := freeLoopbackPort(t), freeLoopbackPort(t)
	s := newTestSelector(t, dir, "http://probe.invalid/", socks, httpP)
	s.UpdateConfigs([]ProxyConfig{e2eCand("A", "e2e-a:1", "good"), e2eCand("B", "e2e-b:1", "good")})
	if err := s.StartWithBest(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.lastLatency = rotateSlowLatency + time.Second
	s.lastProbe = time.Now()
	s.mu.Unlock()
	// No byte evidence whatsoever - that is the entire point of this test.
	expireEgress(socks, httpP)
	if s.ServingTraffic() {
		t.Fatal("precondition: the byte ledger must see nothing")
	}
	if n := establishedOnPort(socks); n != 0 {
		t.Skipf("a daemon probe still holds %d socket(s) on the serving port", n)
	}
	// A bare TCP connect is enough: the kernel completes the handshake and the
	// socket is ESTABLISHED whether or not the stub has accepted it yet.
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", socks), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if !s.ServingClients() {
		t.Fatal("precondition: an open client socket must be visible")
	}
	before := s.currentPID()
	m := &ProxyManager{
		instances: []*ProxySelector{s},
		statuses:  []InstanceStatus{{Index: 0, Status: "ok"}},
		xrayDir:   dir,
	}
	m.reconcileActive(0, s, s.ActiveConfig())
	if got := s.ActiveConfig(); got == nil || got.Key() != "e2e-a:1" {
		t.Fatalf("instance with a live client socket must not be rotated, got %+v", got)
	}
	if s.currentPID() != before {
		t.Fatal("rotation severed the client's live connection")
	}
}
```

**(c) `e2e_test.go` — keep the two "must still rotate" tests honest.** Both
already `expireEgress` their ports; add the socket precondition so a lingering
probe socket skips rather than flakes:

In `TestReconcileStaleLatencyNotRotated` (`e2e_test.go:447`) and
`TestReconcileSlowIdleActiveStillRotates` (`e2e_test.go:481`), after the
existing `ServingTraffic` precondition:

```go
	if n := establishedOnPort(socks); n != 0 {
		t.Skipf("a daemon probe still holds %d socket(s) on the serving port", n)
	}
```

`fmt` and `net` are already imported in `e2e_test.go:12-13`.

### 5.3 Verification to run

```bash
cd v2proxy
gofmt -l . | grep -v '^vendor'                  # expect no output
go vet ./... && go vet -mod=mod ./...
staticcheck -fail all -checks all ./...
golangci-lint run --timeout 180s ./...
go test -count=1 ./...
go test -race -count=1 ./...
```

Then in WSL2, per `AGENT.md:32-54`, including
`V2PRODOCK_REAL_XRAY=/tmp/xraydl/x/xray` — the real-xray tier is the one that
would catch a regression in `renderXrayConfig` (which this patch does not
touch, but the fragment config is exactly what that tier proves).

---

## 6. Summary table

| Option | Closes the gap | Correctness | Blast radius | Verdict |
|---|---|---|---|---|
| (a) xray stats API | yes | answers "bytes since start", needs per-process diff state; `docket` is gone in 26.3.27; counters need a `policy` block or `QueryStats` returns `{}` | +1 port per xray incl. all 10 concurrent probes; +gRPC dep or per-tick fork; `renderXrayConfig` gains keys | schema-compatible (verified) but far too large |
| (b) `/proc/net/tcp{,6}` ESTABLISHED count | yes, on the exact signal | direct measure of what rotation destroys | one term in one early-return guard; no deps, no ports, no config change; untagged, testable on Windows | **recommended** |
| (c) conservative guard alone | no | idleness becomes unprovable ⇒ rotation dead; frozen slow node also wins `aggMinStreak` and monopolises the aggregate head | deletes `rotateMaxCandidates`/`rotateBudget`/the 30% margin; breaks `TestReconcileSlowIdleActiveStillRotates` | only correct *with* (b) |
| (d) serve the SOCKS inbound from Go | yes | n/a | a relay in front of every tunnel: new latency, new failure mode, new hot path | rejected |

### Honest limits of this analysis

* The §2 / §3 measurements were taken in WSL2 against `/tmp/xraydl/x/xray`
  26.3.27 and that kernel. The project ships the same xray version
  (`AGENT.md:47`, `constants.go` fragment comment), and the container shares
  one netns, so both results should hold in production — but they were not
  measured inside the container.
* `parseEstablishedOnPort` was validated as a standalone program against a live
  listener, not as a compiled-in package function; the Go patch itself has not
  been compiled or tested, by ownership constraint.
* Whether a client that holds a socket open indefinitely should be able to
  starve rotation forever is a product question the patch answers in favour of
  the client's connection, matching existing behaviour. If the answer should be
  the opposite, the guard needs a bounded-wait escape hatch — a larger change,
  deliberately not smuggled in here.
* `manager.go:670-680` (vanished-from-pool forced switch) remains unguarded.
  See §4.4.