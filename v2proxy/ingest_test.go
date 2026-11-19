package main

// Ingest-filter tests: burned configs must never enter the pool. Parsing stays
// pure; these exercise the fetch-time filter directly.

import (
	"encoding/json"
	"testing"
)

func ingestCfg(protocol string, stream map[string]any) ProxyConfig {
	out := map[string]any{
		"protocol": protocol,
		"settings": map[string]any{
			"vnext": []any{map[string]any{"address": "node.example.com"}},
		},
		"streamSettings": stream,
	}
	b, _ := json.Marshal(out)
	return ProxyConfig{Name: protocol, Endpoint: "node.example.com:443", XrayCfg: b}
}

func TestFilterIngestRejectsInsecureAndImplausibleSNI(t *testing.T) {
	t.Setenv("REQUIRE_TRANSPORT_SECURITY", "1")
	t.Setenv("SNI_DENYLIST", "")
	t.Setenv("SNI_DNS_CHECK", "0")

	plainVMess := ingestCfg("vmess", map[string]any{"network": "ws", "security": ""})
	plainVless := ingestCfg("vless", map[string]any{"network": "tcp"})
	ss := ingestCfg("shadowsocks", map[string]any{"network": "tcp"})
	tlsVMess := ingestCfg("vmess", map[string]any{"network": "ws", "security": "tls"})
	realityGood := ingestCfg("vless", map[string]any{
		"network": "tcp", "security": "reality",
		"realitySettings": map[string]any{"serverName": "real.example.com"},
	})
	realityOwn := ingestCfg("vless", map[string]any{
		"network": "tcp", "security": "reality",
		"realitySettings": map[string]any{"serverName": "node.example.com"},
	})
	realityBadTLD := ingestCfg("vless", map[string]any{
		"network": "tcp", "security": "reality",
		"realitySettings": map[string]any{"serverName": "bogus.invalidtld"},
	})
	realityBadSubdomain := ingestCfg("vless", map[string]any{
		"network": "tcp", "security": "reality",
		"realitySettings": map[string]any{"serverName": "www.bogus.invalidtld"},
	})
	realityIP := ingestCfg("vless", map[string]any{
		"network": "tcp", "security": "reality",
		"realitySettings": map[string]any{"serverName": "127.0.0.1"},
	})
	realityEmpty := ingestCfg("vless", map[string]any{
		"network": "tcp", "security": "reality",
	})

	kept, stats := filterIngestConfigs([]ProxyConfig{
		plainVMess, plainVless, ss, tlsVMess, realityGood, realityOwn,
		realityBadTLD, realityBadSubdomain, realityIP, realityEmpty,
	})
	if len(kept) != 3 {
		t.Fatalf("kept %d configs, want 3 (tls vmess, good reality, own reality): %+v", len(kept), kept)
	}
	if stats.Insecure != 3 || stats.BadSNI != 4 {
		t.Fatalf("stats = %+v, want Insecure=3 BadSNI=4", stats)
	}
	t.Setenv("REQUIRE_TRANSPORT_SECURITY", "0")
	kept, _ = filterIngestConfigs([]ProxyConfig{ss, plainVMess, plainVless})
	if len(kept) != 3 {
		t.Fatalf("opt-out must restore unsafe transports, kept %d", len(kept))
	}
	t.Setenv("REQUIRE_TRANSPORT_SECURITY", "")
	kept, _ = filterIngestConfigs([]ProxyConfig{ss, plainVMess, plainVless})
	if len(kept) != 3 {
		t.Fatalf("default (unset) must be general-purpose, kept %d", len(kept))
	}
	t.Setenv("REQUIRE_TRANSPORT_SECURITY", "1")
	kept, _ = filterIngestConfigs([]ProxyConfig{realityBadTLD})
	if len(kept) != 0 {
		t.Fatal("bad SNI must stay rejected even with transport security off/on")
	}

	// Operator allowlist is the escape hatch for a suffix newer than the
	// bundled public-suffix list.
	t.Setenv("SNI_ALLOWLIST", "*.newtld")
	allowed := ingestCfg("vless", map[string]any{
		"network": "tcp", "security": "reality",
		"realitySettings": map[string]any{"serverName": "real.newtld"},
	})
	kept, _ = filterIngestConfigs([]ProxyConfig{allowed})
	if len(kept) != 1 {
		t.Fatal("SNI_ALLOWLIST must allow an otherwise unknown suffix")
	}
	t.Setenv("SNI_ALLOWLIST", "")
}

func TestSNIDenylistCanBeCleared(t *testing.T) {
	t.Setenv("SNI_DENYLIST", "-")
	if list := sniDenylist(); len(list) != 0 {
		t.Fatalf("SNI_DENYLIST=- must clear, got %v", list)
	}
	t.Setenv("SNI_DENYLIST", "bad.example,*.burned")
	list := sniDenylist()
	if !sniBlocked("bad.example", list) || !sniBlocked("x.burned", list) {
		t.Fatalf("denylist patterns not honored: %v", list)
	}
	if sniBlocked("real.example.com", list) {
		t.Fatal("real.example must not be blocked")
	}
}

func TestValidSNIHost(t *testing.T) {
	for _, good := range []string{"example.com", "www.example.co.uk", "xn--bcher-kva.example"} {
		if !validSNIHost(good) {
			t.Errorf("validSNIHost(%q) = false", good)
		}
	}
	for _, bad := range []string{"", "localhost", "127.0.0.1", "single", "bad_domain.com", "-bad.com", "bad-.com", "x.local", "x.internal"} {
		if validSNIHost(bad) {
			t.Errorf("validSNIHost(%q) = true, want false", bad)
		}
	}
	// Public-suffix plausibility is generic: a made-up TLD is rejected, while
	// an ordinary domain and a multi-label private suffix (CDN tenant) are not.
	if !plausibleSNI("example.com") || !plausibleSNI("x.workers.dev") {
		t.Error("ordinary/private-suffix hosts must be plausible")
	}
	if plausibleSNI("bogus.invalidtld") || plausibleSNI("host.invalidtld") {
		t.Error("a single unknown TLD must not be plausible")
	}
}
