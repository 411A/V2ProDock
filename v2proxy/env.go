package main

// Environment-driven tuning knobs. All helpers are pure and cheap (getenv per
// call is fine: renders/switches are infrequent, never per-packet).

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	throughputFloorWarn    sync.Once
	throughputIntervalWarn sync.Once
	throughputBytesWarn    sync.Once
	throughputTimeoutWarn  sync.Once
	throughputURLWarn      sync.Once
)

// switchWorkerCount bounds parallel temp-port probing during a switch.
// 1 = old sequential behavior. Clamped to [1, switchWorkersMax] so a typo
// cannot fork-bomb a 256MB box with xray children.
func switchWorkerCount() int {
	v := strings.TrimSpace(os.Getenv("SWITCH_WORKERS"))
	if v == "" {
		return switchWorkersDefault
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return switchWorkersDefault
	}
	return min(max(n, 1), switchWorkersMax)
}

// fragmentEnabled opts the DPI-evasion fragment chain in (default off:
// fragment costs extra packets + latency on every TLS handshake).
// Accepted truthy values: 1, true, on, yes (case-insensitive).
func fragmentEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("XRAY_FRAGMENT"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// upstreamUsesTLS reports whether a parsed xray outbound negotiates TLS on
// its transport (streamSettings.security tls|reality). Fragment is only
// attached to those: plaintext upstreams gain nothing and would just pay
// the handshake overhead.
func upstreamUsesTLS(outbound map[string]any) bool {
	ss, ok := outbound["streamSettings"].(map[string]any)
	if !ok {
		return false
	}
	sec, _ := ss["security"].(string)
	switch strings.ToLower(strings.TrimSpace(sec)) {
	case "tls", "reality":
		return true
	}
	return false
}

// hasCustomDialChain reports whether the outbound already controls its own
// dial path (user-supplied dialerProxy or proxySettings). Fragment must not
// override those: silently changing a curated chain breaks intent.
func hasCustomDialChain(outbound map[string]any) bool {
	if ss, ok := outbound["streamSettings"].(map[string]any); ok {
		if so, ok := ss["sockopt"].(map[string]any); ok {
			if dp, _ := so["dialerProxy"].(string); strings.TrimSpace(dp) != "" {
				return true
			}
		}
	}
	if ps, ok := outbound["proxySettings"].(map[string]any); ok {
		if tag, _ := ps["tag"].(string); strings.TrimSpace(tag) != "" {
			return true
		}
	}
	return false
}

// aggMinStreak floors aggregate candidacy at this many consecutive successes.
// Absurd values degrade safely to always-fallback (legacy behavior).
func aggMinStreak() int {
	v := strings.TrimSpace(os.Getenv("AGG_MIN_STREAK"))
	if v == "" {
		return aggMinStreakDefault
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return aggMinStreakDefault
	}
	return n
}

// parseProxyMemos parses PROXY_MEMOS: comma-separated endpoint=memo entries.
// The endpoint is the normalized host:port shown in /proxies. A memo may not
// contain a comma (it would start the next entry), but spaces, ';', '>' and
// '=' are fine. Empty keys/values are ignored; a later duplicate wins.
func parseProxyMemos(raw string) map[string]string {
	out := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		key, memo, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		key = normalizeEndpoint(key)
		memo = strings.TrimSpace(memo)
		if key == "" || memo == "" {
			continue
		}
		out[key] = memo
	}
	return out
}

// throughputCheckEnabled lets an operator opt out of the bulk fetch entirely
// (metered links, or a network where the throughput target is blocked).
func throughputCheckEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("THROUGHPUT_CHECK"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// throughputFloorKbps is the demotion floor in kilobits/s. 800 kbps ≈ 100 KB/s:
// enough for a 720p stream, far below a healthy node on the measured links.
func throughputFloorKbps() int64 {
	v := strings.TrimSpace(os.Getenv("THROUGHPUT_FLOOR_KBPS"))
	if v == "" {
		return throughputFloorKbpsDefault
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 || n > 1_000_000 {
		throughputFloorWarn.Do(func() { warnLog("THROUGHPUT_FLOOR_KBPS=%q invalid, using %d", v, throughputFloorKbpsDefault) })
		return throughputFloorKbpsDefault
	}
	return n
}

// throughputInterval is the per-instance re-measure cadence. Clamped to a
// sensible range so a typo cannot probe every second or once a week.
func throughputInterval() time.Duration {
	v := strings.TrimSpace(os.Getenv("THROUGHPUT_INTERVAL_SECS"))
	if v == "" {
		return throughputIntervalDefault
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 30 || n > 86400 {
		throughputIntervalWarn.Do(func() { warnLog("THROUGHPUT_INTERVAL_SECS=%q invalid, using %s", v, throughputIntervalDefault) })
		return throughputIntervalDefault
	}
	return time.Duration(n) * time.Second
}

// throughputProbeSize bounds the measured body. 4MiB outruns the common
// 0.5-1MiB burst allowance; 16MiB is the largest background fetch allowed.
func throughputProbeSize() int64 {
	v := strings.TrimSpace(os.Getenv("THROUGHPUT_CHECK_BYTES"))
	if v == "" {
		return throughputProbeBytesDefault
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 64<<10 || n > 16<<20 {
		throughputBytesWarn.Do(func() { warnLog("THROUGHPUT_CHECK_BYTES=%q invalid, using %d", v, throughputProbeBytesDefault) })
		return throughputProbeBytesDefault
	}
	return n
}

// throughputProbeTimeout budgets the whole 4MiB body. 100 KB/s needs ~41s, so
// the default leaves headroom for a node that is at (not below) the floor.
func throughputProbeTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("THROUGHPUT_TIMEOUT_SECS"))
	if v == "" {
		return throughputProbeTimeoutDefault
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 5 || n > 180 {
		throughputTimeoutWarn.Do(func() { warnLog("THROUGHPUT_TIMEOUT_SECS=%q invalid, using %s", v, throughputProbeTimeoutDefault) })
		return throughputProbeTimeoutDefault
	}
	return time.Duration(n) * time.Second
}

// throughputProbeURL requires an operator-selected object. Public speed-test
// endpoints are per-IP rate-limited (429 through the proxy), and a 429 must not
// be mistaken for a slow node. Empty = gate idle with one warning.
func throughputProbeURL() string {
	raw := strings.TrimSpace(os.Getenv("THROUGHPUT_CHECK_URL"))
	if raw == "" {
		throughputURLWarn.Do(func() {
			warnLog("THROUGHPUT_CHECK_URL is unset: bulk gate idle (set a 4MiB+ operator-controlled object URL)")
		})
		return ""
	}
	if strings.Contains(raw, "%d") {
		// Replace only the byte placeholder: Sprintf would also interpret a
		// literal %20/%2F in a user-supplied URL and corrupt it.
		return strings.Replace(raw, "%d", strconv.FormatInt(throughputProbeSize(), 10), 1)
	}
	return raw
}

// muxEnabled opts into xray mux.cool on supported outbounds (default off).
// Mux can help many parallel flows on a per-flow-shaped ISP; it cannot make a
// single bulk TCP stream faster and adds head-of-line risk, so it is a
// measured experiment, not a default.
func muxEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("XRAY_MUX"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

func muxConcurrency() int {
	v := strings.TrimSpace(os.Getenv("XRAY_MUX_CONCURRENCY"))
	if v == "" {
		return throughputMuxConcurrencyDef
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > throughputMuxConcurrencyMax {
		warnLog("XRAY_MUX_CONCURRENCY=%q invalid, using %d", v, throughputMuxConcurrencyDef)
		return throughputMuxConcurrencyDef
	}
	return n
}

// applyMux adds mux only where it is supported and the subscription has not
// already chosen a policy. XTLS Vision flows are skipped: mux.cool and Vision
// are not a valid combination.
func applyMux(outbound map[string]any) {
	if !muxEnabled() {
		return
	}
	if _, exists := outbound["mux"]; exists {
		return // subscription intent wins
	}
	switch strings.ToLower(strings.TrimSpace(fmt.Sprint(outbound["protocol"]))) {
	case "vmess", "vless", "trojan":
	default:
		return
	}
	if containsVisionFlow(outbound) {
		return
	}
	n := muxConcurrency()
	outbound["mux"] = map[string]any{
		"enabled":         true,
		"concurrency":     n,
		"xudpConcurrency": n,
	}
}

// containsVisionFlow protects raw subscription configs that put the flow in
// settings.vnext[].users[] rather than streamSettings.flow: mux.cool and XTLS
// Vision are not a valid combination anywhere in the object.
func containsVisionFlow(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "flow" && strings.Contains(strings.ToLower(fmt.Sprint(val)), "vision") {
				return true
			}
			if containsVisionFlow(val) {
				return true
			}
		}
	case []any:
		for _, val := range t {
			if containsVisionFlow(val) {
				return true
			}
		}
	}
	return false
}

// tcpCongestion parses XRAY_TCP_CONGESTION: a kernel congestion-control
// algorithm name applied per outbound socket (e.g. bbr). Empty/off by default
// (zero behavior change); garbage is ignored with a warning rather than
// rendered into xray, where an unknown name would fail every dial.
func tcpCongestion() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("XRAY_TCP_CONGESTION")))
	if v == "" {
		return ""
	}
	for _, r := range v {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			warnLog("XRAY_TCP_CONGESTION=%q invalid, using the kernel default", v)
			return ""
		}
	}
	return v
}

// applyUpstreamSockopt adds performance-only socket options to an outbound
// without overriding anything the subscription already chose. tcpCongestion
// is the one knob that changes sustained bulk throughput on a lossy path:
// CUBIC collapses on packet loss, BBR paces instead.
func applyUpstreamSockopt(outbound map[string]any, congestion string) {
	if congestion == "" {
		return
	}
	ss, ok := outbound["streamSettings"].(map[string]any)
	if !ok {
		ss = map[string]any{}
		outbound["streamSettings"] = ss
	}
	so, ok := ss["sockopt"].(map[string]any)
	if !ok {
		so = map[string]any{}
		ss["sockopt"] = so
	}
	if _, exists := so["tcpCongestion"]; exists {
		return
	}
	so["tcpCongestion"] = congestion
}

// isPlainHTTP reports a downgrade-prone probe URL. Plain-HTTP inner traffic
// is RST-injected by DPI on bare transports even when the tunnel is fine,
// so a primary like this condemns healthy proxies (warn at startup).
func isPlainHTTP(u string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(u)), "http://")
}

// aggregatePort resolves one stable-endpoint port: env override wins,
// "0" disables that protocol, otherwise the default. Out-of-range values
// fall back to the default (fail safe, never fail closed on a typo).
func aggregatePort(envKey string, def int) int {
	v := strings.TrimSpace(os.Getenv(envKey))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 65535 {
		return def
	}
	return n
}

// portOrDefault parses a TCP port env with fail-safe fallback: garbage,
// negatives and out-of-range values degrade to the default with a loud
// warning instead of binding surprises (PORT_BASE=0 scans upward from 0 and
// can squat a privileged port as root; API_PORT junk breaks /health checks).
func portOrDefault(envKey string, def int) int {
	v := strings.TrimSpace(os.Getenv(envKey))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		warnLog("%s=%q invalid, using default %d", envKey, v, def)
		return def
	}
	return n
}

// pickAggregatePorts chooses collision-free stable ports. A candidate that is
// taken (API port, instance SOCKS/HTTP, or bind-busy) scans upward until free.
// 0 in / 0 out disables that protocol. Candidates below 1024 are skipped
// (unprivileged container cannot bind them). Results above the published
// 27000-27100 range are unusable from the host: loud warning, not silence.
func pickAggregatePorts(socksCand, httpCand, apiPort int, used map[int]bool) (int, int) {
	s, h := scanAggPort(socksCand, apiPort, used), scanAggPort(httpCand, apiPort, used)
	for _, p := range []int{s, h} {
		if p > aggPublishedMax {
			warnLog("aggregate port %d is outside published 27000-27100: host clients cannot reach it (set AGGREGATE_*_PORT explicitly)", p)
		}
	}
	return s, h
}

func scanAggPort(cand, apiPort int, used map[int]bool) int {
	if cand <= 0 {
		return 0
	}
	for p := max(cand, 1024); p <= maxPort; p++ {
		if p == apiPort || used[p] {
			continue
		}
		if tcpOpen("127.0.0.1:"+strconv.Itoa(p), 100*time.Millisecond) {
			continue
		}
		return p
	}
	return 0
}

// relayCfg is one parsed CF_RELAY_URL: a STANDARD HTTP proxy upstream
// (CONNECT + optional Basic auth). Custom header-relay protocols are NOT
// accepted here: xray's http outbound cannot speak them, so they would fail
// loudly at every render instead of cleanly at startup.
type relayCfg struct {
	host string
	port int
	user string
	pass string
	tls  bool // https:// scheme: TLS to the relay (workers.dev is https-only)
}

// relayConfig parses CF_RELAY_URL ("http(s)://[user:pass@]host[:port]").
// Empty = feature off (zero behavior change). Anything malformed degrades to
// off with a loud warning, never to a half-chained pool: a relay that cannot
// be dialled would otherwise take every relay-path client down with it.
func relayConfig() (relayCfg, bool) {
	raw := strings.TrimSpace(os.Getenv("CF_RELAY_URL"))
	if raw == "" {
		return relayCfg{}, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		warnLog("CF_RELAY_URL=%q invalid, relay egress disabled", raw)
		return relayCfg{}, false
	}
	var tls bool
	switch strings.ToLower(u.Scheme) {
	case "http":
	case "https":
		tls = true
	default:
		warnLog("CF_RELAY_URL=%q scheme must be http or https, relay egress disabled", raw)
		return relayCfg{}, false
	}
	port := 80
	if tls {
		port = 443
	}
	if p := strings.TrimSpace(u.Port()); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			warnLog("CF_RELAY_URL=%q port invalid, relay egress disabled", raw)
			return relayCfg{}, false
		}
		port = n
	}
	user := ""
	pass := ""
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	return relayCfg{host: u.Hostname(), port: port, user: user, pass: pass, tls: tls}, true
}
