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
	probeWorkers     = 10              // parallel probers; raises speed, raises concurrent traffic
	probeTimeout     = 3 * time.Minute // per-instance deadline before it is marked down
	probeMaxAttempts = 3               // full-pool passes per instance before giving up
	populateTick     = 20 * time.Second
)

// ---- Quick probe: single URL, no fallbacks, used while populating ----
const probeURL = "http://www.gstatic.com/generate_204"

const quickProbeTimeout = 3 * time.Second

// ---- Steady-state health checking ----
const (
	healthCheckInterval         = 60 * time.Second // periodic check + refresh cadence
	healthCheckTimeout          = 8 * time.Second  // per-URL timeout for full health checks
	healthTLSHandshakeTimeout   = 5 * time.Second
	healthResponseHeaderTimeout = 5 * time.Second
	healthFailThreshold         = 3 // consecutive fails before an instance is switched
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

// Fallback health URLs for steady-state checks (tried in order after the primary).
var fallbackHealthURLs = []string{
	"https://www.gstatic.com/generate_204",
	"https://cp.cloudflare.com",
	"http://api.ipify.org",
}

// ---- Refresh loop ----
const subscriptionRefreshInterval = 120 * time.Second

// ---- xray process lifecycle ----
const (
	xrayCrashDetect  = 200 * time.Millisecond // wait after start to catch instant crashes
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

const defaultHealthCheckURL = "http://httpbin.org/ip"

const defaultInstanceCount = 1

// ---- SOCKS5 -> HTTP bridge ----
const (
	defaultMaxConns         = 128 // concurrent proxied connections (MAX_CONNS env overrides)
	relayBufSize            = 32 * 1024
	proxySlotWait           = 5 * time.Second // wait for a connection slot before 503
	relayIdleDeadline       = 5 * time.Minute // idle deadline on relayed CONNECT streams
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
