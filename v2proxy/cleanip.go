package main

// Clean-IP escape hatch for L2 censorship (CDN edge-IP blocks).
//
// When the censor null-routes or RSTs specific Cloudflare/GCore/Fastly edge
// IPs, the SNI and credentials in subscription links are still fine — only
// the TCP destination is dead. CLEAN_IP_MAP="blocked-host=working-ip,..."
// swaps the dial address at config-ingest time while leaving SNI, Host
// header, path, UUID and every other parameter untouched (the standard
// "clean IP" technique; transport-agnostic for WS-TLS, gRPC-TLS, Reality).
//
// The operator finds the working IP once from the VM with
// scripts/cleanip-scan.sh and sets the env; the daemon itself gains no
// background sweeper, no new deps, no timers — deliberately lean. The
// automated harvester, multi-protocol generator and relay tiers live in
// AGENT.md future plans, not here.

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"strings"
)

// cleanIPMap parses CLEAN_IP_MAP="blocked=working,..." into blocked-host
// (lowercased) -> working address. Malformed entries are ignored; empty or
// unset yields nil (identity behavior). Read fresh on every call: ingest is
// refresh-scale, never hot-path, and tests override per-case via t.Setenv.
func cleanIPMap() map[string]string {
	raw := strings.TrimSpace(os.Getenv("CLEAN_IP_MAP"))
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(entry, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// rewriteCleanIP swaps a blocked dial address for its mapped working address.
// URL links (vless/trojan/ss/hy2): only the host part is replaced, port and
// all query/fragment (SNI, Host, path, flow) preserved. vmess base64-JSON:
// only the "add" field is replaced, "sni"/"host" preserved. Anything
// unparseable or unmapped returns byte-identical input (no behavior change).
func rewriteCleanIP(raw string, m map[string]string) string {
	if len(m) == 0 {
		return raw
	}
	if strings.HasPrefix(raw, "vmess://") {
		return rewriteVmessAdd(raw, m)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := strings.ToLower(u.Hostname())
	gear, ok := m[host]
	if !ok {
		return raw
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(gear, port)
	} else {
		u.Host = gear
	}
	out := u.String()
	debugLog("clean-IP: dial %s -> %s (SNI/params untouched)", host, gear)
	return out
}

// rewriteVmessAdd replaces only the "add" dial field of a vmess base64-JSON
// link. Decode failures pass through untouched (parseVmess errors as before).
func rewriteVmessAdd(raw string, m map[string]string) string {
	b64 := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, strings.TrimPrefix(raw, "vmess://"))
	if mod := len(b64) % 4; mod != 0 {
		b64 += strings.Repeat("=", 4-mod)
	}
	var data []byte
	var err error
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if data, err = enc.DecodeString(b64); err == nil {
			break
		}
	}
	if err != nil {
		return raw
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return raw
	}
	add, _ := fields["add"].(string)
	gear, ok := m[strings.ToLower(strings.TrimSpace(add))]
	if !ok {
		return raw
	}
	fields["add"] = gear
	rebuilt, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	debugLog("clean-IP: dial %s -> %s (sni/host untouched)", add, gear)
	return "vmess://" + base64.StdEncoding.EncodeToString(rebuilt)
}
