package main

import "time"

// Central place for every user-tunable static setting.
// Env vars (PORT_BASE, PROXY_INSTANCES, API_PORT, ...) still override the
// defaults below where supported — see main.go.

// ---- Ports ----
const (
	defaultPortBase = 27019 // first SOCKS5 port; instances take base..base+N-1, HTTP bridge base+N..base+2N-1
	defaultAPIPort  = 27018
	maxPort         = 27999 // upper bound when scanning for a free port block
)

// ---- Initial probing (populate) ----
const (
	probeWorkers       = 10              // parallel probers; raises speed, raises concurrent traffic
	probeTimeout       = 3 * time.Minute // per-instance deadline before it is marked down
	probeMaxAttempts   = 3               // full-pool passes per instance before giving up
	populateTick       = 20 * time.Second
	populateRetryDelay = 15 * time.Second // wait between populate rounds for missing instances
	maxPopulateRounds  = 20               // then serve degraded; the health loop keeps healing stragglers
)

// ---- Quick probe: primary + ONE independent fallback, raced, used everywhere hot ----
// Plain-HTTP probes die under DPI even when the tunnel itself is fine, and a
// single hard-blocked endpoint must not condemn a healthy tunnel — so the
// quick probe races two URLs on independent infrastructure (Google vs
// Cloudflare). First success wins; worst case is still one 3s budget.
const probeURL = "https://www.gstatic.com/generate_204"

// Cloudflare's 204 endpoint lives at the /generate_204 PATH — the bare host
// answers 404, which a status-grading probe would read as "tunnel dead".
const quickFallbackURL = "https://cp.cloudflare.com/generate_204"

const quickProbeTimeout = 3 * time.Second

// Cap on how much of a probe response body is drained before the connection is
// released. A 204 has none; a throttled or 404 target may send a body, and an
// undrained response holds the tunnel (and the single idle-conn slot) open.
const probeDrainCap = 64 << 10

// ---- Steady-state health checking ----
const (
	healthCheckInterval         = 60 * time.Second // periodic check + refresh cadence
	healthCheckTimeout          = 8 * time.Second  // per-URL timeout for full health checks
	healthTLSHandshakeTimeout   = 5 * time.Second
	healthResponseHeaderTimeout = 5 * time.Second
	healthFailThreshold         = 3 // consecutive fails before an instance is switched
)

// ---- Failover switching (bounded: never freeze the ticker loop) ----
const switchBudget = 60 * time.Second // aggregate cap for one switch pass

// ---- Parallel probing (zero-dead-window switches) ----
// A switch probes candidates on throwaway ports with this many workers while
// the OLD xray keeps serving its port. Only after a winner is proven does the
// swap happen (brief rebind), so searching never equals outage. Sequential
// HealthCheckAll bounds total churn to one instance's workers at a time.
const (
	switchWorkersDefault = 3 // SWITCH_WORKERS overrides, clamped to [1, switchWorkersMax]
	switchWorkersMax     = 8
)

// ---- Stable aggregate endpoints (single ports that survive switches) ----
// Raw TCP relays in front of the instances: each new connection is routed to
// the fastest alive instance at dial time, so clients (long-polling bots)
// never rewrite config. Inside the published 27000-27100 range; 0 disables.
const (
	defaultAggSocksPort = 27017 // AGGREGATE_SOCKS_PORT overrides
	defaultAggHTTPPort  = 27016 // AGGREGATE_HTTP_PORT overrides
	aggPublishedMax     = 27100 // compose publishes 27000-27017 + 27019-27100 (API holds 27018)
)

// ---- DPI fragmentation (opt-in; binary-verified schema, off by default) ----
// Mechanism (proven against Xray 26.3.27: loads + proxies end-to-end):
// the proxy outbound chains via streamSettings.sockopt.dialerProxy at a
// freedom outbound tagged "frag-out" that carries settings.fragment. Only
// TLS/reality upstreams get the chain (plaintext gains nothing, pays
// overhead); "tlshello" fragments the handshake only (cheapest effective).
const (
	fragmentOutTag   = "frag-out"
	fragmentPackets  = "tlshello"
	fragmentLength   = "100-200"
	fragmentInterval = "10-20"
)

// ---- Refresh rotation (only slow/missing actives are touched; fast ones cost zero) ----
const (
	rotateSlowLatency    = 2500 * time.Millisecond // active slower than this becomes rotation-eligible
	rotateMaxCandidates  = 3                       // fresh candidates tried per rotation, then stop
	rotateBudget         = 20 * time.Second        // aggregate cap for one rotation attempt
	watchdogPortDialWait = 300 * time.Millisecond  // TCP sanity-dial timeout per instance port
	// How old a latency measurement may be and still justify a rotation. Idle
	// instances are measured every healthCheckInterval (60s), so 3x leaves room
	// for two skipped ticks; a busier one goes unmeasured for as long as it
	// keeps serving (HealthCheck deliberately skips those probes) and must NOT
	// be rotated on a forgotten number.
	rotateLatencyMaxAge = 3 * healthCheckInterval
)

// ---- Subscription fetching ----
const (
	fetchTimeout        = 10 * time.Second
	fetchAttempts       = 2 // per-URL attempts (exponential backoff between them)
	fetchBackoffBase    = 500 * time.Millisecond
	fetchMaxBody        = 10 << 20 // 10 MiB cap per subscription response
	fetchPoolAttempts   = 2        // whole-pool retries
	fetchPoolRetrySleep = 2 * time.Second
)

const userAgent = "V2ProDock/1.0"

// Loopback subscription URLs are unreachable from inside Docker, so these
// host-side replacements are tried (host.docker.internal first).
var loopbackFallbackHosts = []string{
	"host.docker.internal",
	"172.17.0.1",
	"172.18.0.1",
	"10.0.2.2",
}

// ---- Refresh loop ----
const subscriptionRefreshInterval = 120 * time.Second

// ---- Dead-source circuit breaker (refresh loop) ----
const (
	srcFailThreshold = 3 // consecutive refresh failures before a source rests
	srcSkipCycles    = 5 // refreshes a dead source is skipped (then probed again)
)

// ---- xray process lifecycle ----
const (
	xrayCrashDetect  = 100 * time.Millisecond // wait after start to catch instant crashes (bind fails surface in ms)
	xrayStopWait     = 2 * time.Second        // graceful stop before SIGKILL
	xrayPortFreeWait = 3 * time.Second        // wait for old SOCKS port to free after stop
	switchPortWait   = 2 * time.Second        // port wait when switching to a new config
)

// ---- Paths and startup defaults ----
const (
	defaultXrayDir   = "/root/xray"
	configDir        = "/root/config"
	subscriptionFile = "subscription.txt"
)

const defaultHealthCheckURL = "https://www.gstatic.com/generate_204"

const defaultInstanceCount = 1

// ---- SOCKS5 -> HTTP bridge ----
const (
	defaultMaxConns         = 128  // concurrent proxied connections (MAX_CONNS env overrides)
	maxConnsCap             = 4096 // hard ceiling on MAX_CONNS: the semaphore is filled once per slot, so an unclamped value stalled boot for ~25s at 1e9
	relayBufSize            = 32 * 1024
	proxySlotWait           = 5 * time.Second // wait for a connection slot before 503
	bridgeDialTimeout       = 5 * time.Second // upstream SOCKS dial budget: fail fast with 503, never hang
	bridgeReadHeaderTimeout = 10 * time.Second
	bridgeIdleTimeout       = 120 * time.Second
	bridgeMaxHeaderBytes    = 4096
)

// Time budgets, vars rather than consts ONLY so tests can shrink them to
// milliseconds (same precedent as xrayDownloadTimeout).
var (
	// Absolute budget for the HEADER phase of a plain-HTTP exchange: a stalled
	// server never sends headers, and this is what recycles the connection
	// slot. It must not outlive the header phase or it truncates bodies (see
	// handlePlainHTTP).
	bridgeUpstreamDeadline = 60 * time.Second
	// IDLE deadline for streamed bytes, re-armed after every hop. Applies to
	// relayed CONNECT streams and to the plain-HTTP body phase. Never a total
	// transfer budget: that severs healthy long downloads mid-payload.
	relayIdleDeadline = 5 * time.Minute
)

// ---- Half-open connection detection (long-poll survival) ----
// A DPI blackhole (no FIN/RST) leaves relay Reads blocked until the absolute
// deadline. Aggressive keepalives on BOTH relay ends detect the dead peer in
// ~idle+interval*count instead of minutes, so the client reconnects onto a
// fresh upstream. Idle stays above long-poll silence: healthy polls never
// trip, only truly dead sockets do.
const (
	keepAliveIdle     = 30 * time.Second
	keepAliveInterval = 10 * time.Second
	keepAliveCount    = 3
)

// ---- Telegram-targeted probing ----
// api.telegram.org root answers non-2xx (documented 404 envelope), so it can
// never be a plain health URL — but ANY completed HTTPS exchange with it
// proves Telegram reachability, which is exactly what the gateway needs.
// Raced alongside the 204s; first success wins, cost stays one 3s budget.
const telegramProbeURL = "https://api.telegram.org/"

// ---- Passive health: real client traffic outranks synthetic probes ----
// A proxy that just served real bytes is responsive BY DEFINITION — no probe
// blip may kill it. Bridges (per-instance HTTP) and the aggregate (raw relay)
// timestamp every proven byte flow per instance port; HealthCheck trusts
// activity fresher than this instead of probing. 90s covers a quiet tick
// with margin while bounding true-down detection to grace + 3 strikes.
const egressGrace = 90 * time.Second

// ---- Aggregate stability gating ----
// Lowest-latency-first routes long-polls onto nodes that die mid-poll
// (production-proven: flappy-fast RSTs 50s getUpdates). Candidates need this
// many consecutive successes (~minutes of health) to be preferred; fastest
// wins WITHIN the qualified tier. Nothing qualifying falls back to fastest
// (never refuse service); 0 disables the gate (legacy pure-latency).
const aggMinStreakDefault = 3 // AGG_MIN_STREAK overrides

// ---- Startup serving threshold ----
// Thin pools grind for minutes before EVERY instance lands one. Serve as soon
// as this many are ready (capped by instance count); stragglers keep healing
// via the health + refresh loops. N=1 keeps the old all-must-be-ok behavior.
const serveMinReady = 2

// ---- Status API server ----
const (
	apiReadTimeout       = 5 * time.Second
	apiWriteTimeout      = 10 * time.Second
	apiReadHeaderTimeout = 3 * time.Second
	apiIdleTimeout       = 120 * time.Second
	apiMaxHeaderBytes    = 2048
)

// ---- Logging ----
const (
	logTimeFormat = "2006/01/02 15:04:05.000"
	latWarnMs     = 1500 // latency coloring thresholds
	latCritMs     = 3000
	shortNameMax  = 48 // proxy-name truncation length
	shortErrMax   = 30 // error truncation length in the summary table
)

// ---- xray download ----
const xrayDownloadURLTmpl = "https://github.com/XTLS/Xray-core/releases/latest/download/Xray-%s-%s.zip"

const xrayDownloadAttempts = 2 // whole fetch+extract passes before boot fails

// Bounds one download attempt (plain http.Get has NO timeout: a blackholed
// route wedged boot forever). Var, not const, so tests can shrink it against
// a hanging stub server.
var xrayDownloadTimeout = 3 * time.Minute
