package main

// Ingest hygiene: reject configs that cannot survive a shaping middlebox
// before they enter the pool. A perfect selector over unusable configs still
// gives 1-5KB/s; the pool is the product.
//
// Generic rules:
//   - no transport security: Shadowsocks, or VMess/VLESS with no TLS/Reality
//   - Reality with an implausible SNI: not a valid public hostname, or listed
//     by the operator's optional SNI_DENYLIST
//
// The rules are deliberately applied at FETCH time, not in parseToXrayConfig:
// parsing stays pure and unit-testable, and the filter can be disabled with
// REQUIRE_TRANSPORT_SECURITY=0 for an operator who knowingly has a secure
// non-TLS transport.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

type ingestStats struct {
	Insecure  int
	BadSNI    int
	Malformed int
}

func (s ingestStats) total() int { return s.Insecure + s.BadSNI + s.Malformed }

func requireTransportSecurity() bool {
	// Opt-in: a general-purpose pool may legitimately carry Shadowsocks or a
	// bare VMess transport. DPI-hardened deployments set this to 1.
	switch strings.ToLower(strings.TrimSpace(os.Getenv("REQUIRE_TRANSPORT_SECURITY"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

func sniDNSCheckEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SNI_DNS_CHECK"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// sniDenylist is operator-supplied only: no region- or campaign-specific
// names are compiled in. Entries are exact hosts or *.suffix patterns.
func sniDenylist() []string { return parseSNIPatterns(os.Getenv("SNI_DENYLIST")) }

// sniAllowlist lets an operator keep a Reality host whose suffix is newer than
// the bundled public-suffix list (or otherwise intentionally private). It only
// bypasses the suffix plausibility check, never the denylist.
func sniAllowlist() []string { return parseSNIPatterns(os.Getenv("SNI_ALLOWLIST")) }

func parseSNIPatterns(raw string) []string {
	var out []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry != "" && entry != "-" {
			out = append(out, entry)
		}
	}
	return out
}

func sniMatches(sni string, list []string) bool {
	sni = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(sni)), ".")
	for _, pat := range list {
		pat = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(pat)), ".")
		if pat == "" {
			continue
		}
		if strings.HasPrefix(pat, "*.") {
			if strings.HasSuffix(sni, pat[1:]) {
				return true
			}
			continue
		}
		if sni == pat {
			return true
		}
	}
	return false
}

func sniBlocked(sni string, list []string) bool { return sniMatches(sni, list) }
func sniAllowed(sni string, list []string) bool { return sniMatches(sni, list) }

// validSNIHost rejects IPs, single labels, local names and malformed labels.
// It is syntax-only; optional DNS confirmation lives in sniResolvable.
func validSNIHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return false
	}
	if host == "localhost" ||
		strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") ||
		strings.HasSuffix(host, ".home.arpa") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

// plausibleSNI requires a syntactically valid host under a recognized public
// suffix. This is generic: a made-up TLD is rejected, while ordinary domains
// and multi-label private suffixes (CDN tenants, for example) remain valid.
// No specific site or campaign is compiled in.
func plausibleSNI(host string) bool {
	if !validSNIHost(host) {
		return false
	}
	suffix, icann := publicsuffix.PublicSuffix(host)
	if suffix == "" {
		return false
	}
	return icann || strings.Contains(suffix, ".")
}

var sniDNSCache sync.Map // map[string]bool

// sniResolvable accepts on timeout/SERVFAIL (inconclusive) and rejects only a
// definite NXDOMAIN. A DNS-filtered VPS must not lose a good node because its
// resolver cannot see the SNI.
func sniResolvable(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if v, ok := sniDNSCache.Load(host); ok {
		return v.(bool)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	ok := true
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			ok = false
		}
	} else if len(addrs) == 0 {
		ok = false
	}
	sniDNSCache.Store(host, ok)
	return ok
}

// streamSecurity extracts streamSettings.security ("" when absent).
func streamSecurity(out map[string]any) string {
	ss, _ := out["streamSettings"].(map[string]any)
	sec, _ := ss["security"].(string)
	return strings.ToLower(strings.TrimSpace(sec))
}

// outboundServer returns the remote address from the protocol settings.
func outboundServer(out map[string]any) string {
	settings, _ := out["settings"].(map[string]any)
	if vnext, ok := settings["vnext"].([]any); ok && len(vnext) > 0 {
		if m, ok := vnext[0].(map[string]any); ok {
			s, _ := m["address"].(string)
			return strings.TrimSpace(s)
		}
	}
	if servers, ok := settings["servers"].([]any); ok && len(servers) > 0 {
		if m, ok := servers[0].(map[string]any); ok {
			s, _ := m["address"].(string)
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// realitySNI returns the configured serverName for a Reality outbound.
func realitySNI(out map[string]any) string {
	ss, _ := out["streamSettings"].(map[string]any)
	rs, _ := ss["realitySettings"].(map[string]any)
	sni, _ := rs["serverName"].(string)
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(sni)), ".")
}

// filterIngestConfigs applies the pool rules and returns kept configs plus
// rejection counts. Order is preserved; callers re-rank afterwards.
func filterIngestConfigs(in []ProxyConfig) ([]ProxyConfig, ingestStats) {
	var stats ingestStats
	requireTLS := requireTransportSecurity()
	deny := sniDenylist()
	allow := sniAllowlist()
	dnsCheck := sniDNSCheckEnabled()
	out := make([]ProxyConfig, 0, len(in))

	for _, cfg := range in {
		var outbound map[string]any
		if err := json.Unmarshal(cfg.XrayCfg, &outbound); err != nil {
			stats.Malformed++
			continue
		}
		proto := strings.ToLower(strings.TrimSpace(fmt.Sprint(outbound["protocol"])))
		sec := streamSecurity(outbound)

		if requireTLS {
			switch proto {
			case "vmess", "vless", "trojan":
				if sec != "tls" && sec != "reality" {
					stats.Insecure++
					continue
				}
			case "shadowsocks":
				// Shadowsocks has no TLS transport; it is a distinct DPI
				// fingerprint and the measured class that gets shaped.
				stats.Insecure++
				continue
			default:
				stats.Insecure++
				continue
			}
		}

		if sec == "reality" {
			sni := realitySNI(outbound)
			if sniBlocked(sni, deny) || (!plausibleSNI(sni) && !sniAllowed(sni, allow)) {
				stats.BadSNI++
				continue
			}
			if dnsCheck {
				own := strings.EqualFold(sni, strings.TrimSpace(outboundServer(outbound)))
				if !own && !sniResolvable(sni) {
					stats.BadSNI++
					continue
				}
			}
		}
		out = append(out, cfg)
	}
	return out, stats
}
