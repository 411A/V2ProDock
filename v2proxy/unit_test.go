package main

// Unit-layer tests: pure-function coverage for parsers, helpers, and small
// utilities. All hermetic — no sockets, no processes, no network.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestToInt(t *testing.T) {
	cases := []struct {
		in   string
		def  int
		want int
	}{
		{"443", 80, 443},
		{"  8080 ", 80, 8080},
		{"", 80, 80},
		{"<nil>", 80, 80},
		{"abc", 80, 80},
		{"-1", 80, -1},
	}
	for _, c := range cases {
		if got := toInt(c.in, c.def); got != c.want {
			t.Errorf("toInt(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"128MiB", 128 << 20},
		{"128mib", 128 << 20},
		{"256MB", 256 * 1000 * 1000},
		{"2GiB", 2 << 30},
		{"1GB", 1000 * 1000 * 1000},
		{"100", 100},
	}
	for _, c := range cases {
		got, err := parseBytes(c.in)
		if err != nil {
			t.Errorf("parseBytes(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseBytes(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "abc", "MiB"} {
		if _, err := parseBytes(bad); err == nil {
			t.Errorf("parseBytes(%q) must error", bad)
		}
	}
}

func TestBuildTLS(t *testing.T) {
	q := url.Values{"sni": {"example.com"}, "fp": {"chrome"}, "alpn": {"h2,http/1.1"}}
	tcp := buildTLS(q, "tcp")
	if tcp["serverName"] != "example.com" || tcp["fingerprint"] != "chrome" {
		t.Errorf("tcp tlsSettings wrong: %v", tcp)
	}
	if alpn, ok := tcp["alpn"].([]string); !ok || !slices.Equal(alpn, []string{"h2", "http/1.1"}) {
		t.Errorf("tcp must carry split alpn: %v", tcp)
	}
	// WebSocket transport drops ALPN (deprecated in xray v26 for WS).
	ws := buildTLS(q, "ws")
	if _, ok := ws["alpn"]; ok {
		t.Errorf("ws must not carry alpn: %v", ws)
	}
}

func TestParseTrojan(t *testing.T) {
	raw := "trojan://secret@example.com:443?type=ws&path=%2Fws&host=example.com#Trojan_Test"
	cfg, err := parseToXrayConfig(raw)
	if err != nil {
		t.Fatalf("parse trojan: %v", err)
	}
	if cfg.Name != "Trojan_Test" || cfg.Endpoint != "example.com:443" {
		t.Errorf("trojan identity wrong: %+v", cfg)
	}
	var out map[string]any
	if err := json.Unmarshal(cfg.XrayCfg, &out); err != nil {
		t.Fatal(err)
	}
	if out["protocol"] != "trojan" {
		t.Errorf("protocol = %v", out["protocol"])
	}
	servers := out["settings"].(map[string]any)["servers"].([]any)
	if servers[0].(map[string]any)["password"] != "secret" {
		t.Errorf("trojan password misplaced: %v", servers[0])
	}
}

func TestParseSS(t *testing.T) {
	user := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:password123"))
	cfg, err := parseToXrayConfig("ss://" + user + "@1.2.3.4:8388#SS_Test")
	if err != nil {
		t.Fatalf("parse ss: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(cfg.XrayCfg, &out); err != nil {
		t.Fatal(err)
	}
	srv := out["settings"].(map[string]any)["servers"].([]any)[0].(map[string]any)
	if srv["method"] != "aes-256-gcm" || srv["password"] != "password123" {
		t.Errorf("ss server wrong: %v", srv)
	}
	// Stream ciphers were removed in Xray 26+ and must be rejected.
	badUser := base64.StdEncoding.EncodeToString([]byte("rc4-md5:pw"))
	if _, err := parseToXrayConfig("ss://" + badUser + "@1.2.3.4:8388#x"); err == nil {
		t.Error("unsupported SS cipher must error")
	}
}

func TestParseHy2Rejected(t *testing.T) {
	if _, err := parseToXrayConfig("hy2://x@example.com:443#H"); err == nil {
		t.Error("hysteria2 must be rejected (xray-core cannot serve it)")
	}
	if _, err := parseToXrayConfig("unknown://x#U"); err == nil {
		t.Error("unknown scheme must error")
	}
}

func TestParseVlessTLS(t *testing.T) {
	raw := "vless://uuid@example.com:443?security=tls&sni=example.com&type=tcp#TLS_Test"
	cfg, err := parseToXrayConfig(raw)
	if err != nil {
		t.Fatalf("parse vless tls: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(cfg.XrayCfg, &out); err != nil {
		t.Fatal(err)
	}
	tls, ok := out["streamSettings"].(map[string]any)["tlsSettings"].(map[string]any)
	if !ok || tls["serverName"] != "example.com" {
		t.Errorf("tlsSettings wrong: %v", out["streamSettings"])
	}
}

func TestShortName(t *testing.T) {
	if got := shortName(""); got != "unknown" {
		t.Errorf("empty -> %q", got)
	}
	long := strings.Repeat("x", shortNameMax+20)
	if got := []rune(shortName(long)); len(got) != shortNameMax+1 || string(got[len(got)-1]) != "…" {
		t.Errorf("truncation wrong: %q", shortName(long))
	}
	if got := shortName("plain"); got != "plain" {
		t.Errorf("plain passthrough wrong: %q", got)
	}
}

func TestPortOnly(t *testing.T) {
	if got := portOnly("0.0.0.0:27019"); got != "27019" {
		t.Errorf("portOnly = %q", got)
	}
	if got := portOnly("noport"); got != "noport" {
		t.Errorf("portOnly bare = %q", got)
	}
}

func TestShortErr(t *testing.T) {
	if got := shortErr("a\nb"); got != "a b" {
		t.Errorf("newline fold wrong: %q", got)
	}
}

func TestGetMaxConns(t *testing.T) {
	t.Setenv("MAX_CONNS", "64")
	if got := getMaxConns(); got != 64 {
		t.Errorf("env 64 -> %d", got)
	}
	t.Setenv("MAX_CONNS", "0")
	if got := getMaxConns(); got != defaultMaxConns {
		t.Errorf("env 0 must fall back, got %d", got)
	}
	t.Setenv("MAX_CONNS", "nope")
	if got := getMaxConns(); got != defaultMaxConns {
		t.Errorf("env garbage must fall back, got %d", got)
	}
	_ = os.Unsetenv("MAX_CONNS")
	if got := getMaxConns(); got != defaultMaxConns {
		t.Errorf("unset must fall back, got %d", got)
	}
}

func TestSubscriptionCandidates(t *testing.T) {
	loop := subscriptionCandidates("http://127.0.0.1:27141/subscription")
	if len(loop) < 2 {
		t.Fatalf("loopback must expand to fallbacks, got %v", loop)
	}
	found := false
	for _, c := range loop {
		if strings.Contains(c, "host.docker.internal") {
			found = true
		}
	}
	if !found {
		t.Errorf("host.docker.internal missing: %v", loop)
	}
	direct := subscriptionCandidates("https://example.com/sub")
	if len(direct) != 1 || direct[0] != "https://example.com/sub" {
		t.Errorf("public URL must pass through untouched: %v", direct)
	}
}

func TestShuffleDeterministic(t *testing.T) {
	mk := func() []ProxyConfig {
		var out []ProxyConfig
		for i := range 20 {
			out = append(out, ProxyConfig{Name: fmt.Sprintf("n%d", i), Endpoint: fmt.Sprintf("e:%d", i)})
		}
		return out
	}
	a, b := mk(), mk()
	shuffleConfigs(a, 42)
	shuffleConfigs(b, 42)
	for i := range a {
		if a[i].Endpoint != b[i].Endpoint {
			t.Fatal("same seed must give same order")
		}
	}
	// Still a permutation of the input (compared in the same sorted order).
	keys := make([]string, 0, len(a))
	for _, c := range a {
		keys = append(keys, c.Endpoint)
	}
	slices.Sort(keys)
	want := make([]string, 0, len(a))
	for i := range 20 {
		want = append(want, fmt.Sprintf("e:%d", i))
	}
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Fatalf("shuffle lost an element: %v", keys)
	}
}

func TestDialErrorCode(t *testing.T) {
	if got := dialErrorCode(fmt.Errorf("wrap: %w", errDialTimeout)); got != 504 {
		t.Errorf("timeout must map to 504, got %d", got)
	}
	if got := dialErrorCode(errors.New("boom")); got != 502 {
		t.Errorf("generic must map to 502, got %d", got)
	}
}

func TestPruneOrphanImpossibleDir(t *testing.T) {
	keepAll := func() map[int]bool { return map[int]bool{} }
	dir := t.TempDir() // no xray children can match here
	if got := pruneOrphanXray(dir, keepAll); got != 0 {
		t.Fatalf("empty scan must kill nothing, got %d", got)
	}
}
