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

const quickFallbackURL = "https://cp.cloudflare.com/generate_204"

const quickProbeTimeout = 3 * time.Second

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
	defaultAggHttpPort  = 27016 // AGGREGATE_HTTP_PORT overrides
	aggPublishedMax     = 27100 // compose publishes 27000-27100; above is container-only
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

// Fallback health URLs for full checks (tried in order after the primary).
// All HTTPS: plain-HTTP probes die under DPI even when the tunnel is fine.
// NOTE: cp.cloudflare.com needs the /generate_204 path — the bare host 404s.
var fallbackHealthURLs = []string{
	"https://www.gstatic.com/generate_204",
	"https://cp.cloudflare.com/generate_204",
	"https://api.ipify.org",
}

// ---- Refresh loop ----
const subscriptionRefreshInterval = 120 * time.Second

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
	defaultMaxConns         = 128 // concurrent proxied connections (MAX_CONNS env overrides)
	relayBufSize            = 32 * 1024
	proxySlotWait           = 5 * time.Second  // wait for a connection slot before 503
	bridgeDialTimeout       = 5 * time.Second  // upstream SOCKS dial budget: fail fast with 503, never hang
	bridgeUpstreamDeadline  = 60 * time.Second // total budget per plain-HTTP relay so slots recycle
	relayIdleDeadline       = 5 * time.Minute  // idle deadline on relayed CONNECT streams
	bridgeReadHeaderTimeout = 10 * time.Second
	bridgeIdleTimeout       = 120 * time.Second
	bridgeMaxHeaderBytes    = 4096
)

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
