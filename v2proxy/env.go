package main

// Environment-driven tuning knobs. All helpers are pure and cheap (getenv per
// call is fine: renders/switches are infrequent, never per-packet).

import (
	"os"
	"strconv"
	"strings"
	"time"
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
