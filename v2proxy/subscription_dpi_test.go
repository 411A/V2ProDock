package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestDPIRankPrefersRealityOverRawShadowsocks is the reported failure directly.
//
// An operator with a VLESS Reality subscription watched the daemon adopt a
// legacy-Shadowsocks node over raw TCP:443 - exactly the shape active probing
// fingerprints - while the Reality config sat unused in the same pool. The
// daemon ranked on latency alone, and raw Shadowsocks is the fastest thing in
// any pool because there is no handshake to finish. These are the two extremes
// and the ordering has to put the TLS one first.
func TestDPIRankPrefersRealityOverRawShadowsocks(t *testing.T) {
	reality := "vless://uuid@pinned.reality.example:443?encryption=none&security=reality&sni=www.microsoft.com&fp=chrome&type=tcp&flow=xtls-rprx-vision#Reality"
	legacySS := "ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ@20.234.97.18:443#raw-ss"
	ss2022 := "ss://MjAyMi1ibGFrZTMtYWVzLTEyOC1nY206c2VydmVya2V5@20.234.97.18:443#SS2022"

	if got := dpiRank(reality); got != rankReality {
		t.Fatalf("vless reality rank = %d, want %d", got, rankReality)
	}
	if got := dpiRank(legacySS); got != rankRaw {
		t.Fatalf("legacy shadowsocks rank = %d, want %d (it must rank LAST)", got, rankRaw)
	}
	if got := dpiRank(ss2022); got != rankSS2022 {
		t.Fatalf("ss-2022 rank = %d, want %d", got, rankSS2022)
	}
	// Legacy SS must never outrank a 2022 SS either: the handshake shape is what
	// the DPI rules actually match on.
	if dpiRank(legacySS) >= dpiRank(ss2022) {
		t.Fatalf("legacy SS (%d) outranked ss-2022 (%d)", dpiRank(legacySS), dpiRank(ss2022))
	}
}

// TestOrderByDPIRankPlacesTLSFirstAndKeepsSubscriptionOrder checks the two
// properties that matter: the resistant protocol is TRIED first, so it is the one
// that gets adopted; and inside a tier the subscription's own order survives, so
// an operator can still rank two Reality hosts against each other.
func TestOrderByDPIRankPlacesTLSFirstAndKeepsSubscriptionOrder(t *testing.T) {
	pool := []ProxyConfig{
		{Name: "raw-ss", Raw: "ss://YWVzLTI1Ni1nY206cA@1.1.1.1:443"},
		{Name: "vmess-raw", Raw: vmessLine("tcp", "")},
		{Name: "reality-b", Raw: "vless://u@b.example:443?security=reality&sni=x.example#b"},
		{Name: "vmess-tls", Raw: vmessLine("ws", "tls")},
		{Name: "trojan", Raw: "trojan://pass@t.example:443?security=tls#t"},
		{Name: "reality-a", Raw: "vless://u@a.example:443?security=reality&sni=x.example#a"},
	}
	orderByDPIRank(pool)

	want := []string{"reality-b", "reality-a", "trojan", "vmess-tls", "vmess-raw", "raw-ss"}
	for i, name := range want {
		if pool[i].Name != name {
			t.Fatalf("position %d = %q, want %q`nfull order: %v", i, pool[i].Name, name, names(pool))
		}
	}
	// The two Reality hosts must keep their ORIGINAL relative order, so the
	// subscription's ordering is not silently discarded.
	if pool[0].Name != "reality-b" || pool[1].Name != "reality-a" {
		t.Fatalf("tier order not stable: %v", names(pool))
	}
}

// TestOrderByDPIRankFallsBackWhenNothingIsReality matters as much as the top
// tier: a pool with no Reality host must still be ordered, and a pool of only raw
// Shadowsocks must keep every one of them rather than drop any.
func TestOrderByDPIRankFallsBackWhenNothingIsReality(t *testing.T) {
	pool := []ProxyConfig{
		{Name: "a", Raw: "ss://YWVzLTI1Ni1nY206cA@2.2.2.2:443"},
		{Name: "b", Raw: "ss://YWVzLTI1Ni1nY206cA@3.3.3.3:443"},
		{Name: "t", Raw: "trojan://pass@9.9.9.9:443#t"},
	}
	orderByDPIRank(pool)
	if len(pool) != 3 {
		t.Fatalf("pool lost entries: got %d, want 3 (%v)", len(pool), names(pool))
	}
	if pool[0].Name != "t" {
		t.Fatalf("trojan should lead a Reality-free pool, got %v", names(pool))
	}
}

// TestDPIRankUnknownSchemeRanksLast guards against promotion by accident: an
// unrecognised scheme must never outrank a known TLS-carrying tier, because the
// whole ranking exists to prefer the boring shapes we understand.
func TestDPIRankUnknownSchemeRanksLast(t *testing.T) {
	trojan := "trojan://pass@8.8.8.8:443#t"
	for _, junk := range []string{"", "notauri", "hysteria2://x", "vless://no-at-sign"} {
		if got := dpiRank(junk); got > dpiRank(trojan) {
			t.Fatalf("unrecognised %q ranked %d, above trojan at %d", junk, got, dpiRank(trojan))
		}
	}
}

func names(pool []ProxyConfig) []string {
	out := make([]string, 0, len(pool))
	for _, c := range pool {
		out = append(out, c.Name)
	}
	return out
}

// vmessLine builds a real vmess:// payload the way a subscription would.
func vmessLine(net, tls string) string {
	body := `{"v":"2","ps":"vmess","add":"vmess.example","port":"443","id":"b831381d-6324-4d53-ad4f-8cda48b30811","net":"` + net + `","tls":"` + tls + `"}`
	return "vmess://" + base64.StdEncoding.EncodeToString([]byte(body))
}

var _ = strings.TrimSpace
