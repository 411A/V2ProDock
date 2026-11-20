package main

// Single-upstream mode tests: parse without leaking credentials, render a
// native xray socks outbound, force one serving instance, fail closed on a
// bad value, and keep subscription refresh out of the data path.

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseUpstreamProxy(t *testing.T) {
	valid := []struct {
		raw  string
		host string
		port int
		user string
		pass string
	}{
		{"socks5://gateway.example:1080", "gateway.example", 1080, "", ""},
		{"socks5h://gateway.example:1080", "gateway.example", 1080, "", ""},
		{"socks://gateway.example:1080", "gateway.example", 1080, "", ""},
		{"socks5://user:pa%40ss@gateway.example:1080", "gateway.example", 1080, "user", "pa@ss"},
		{"socks5://[2001:db8::1]:1080", "2001:db8::1", 1080, "", ""},
	}
	for _, tc := range valid {
		got, err := parseUpstreamProxy(tc.raw)
		if err != nil {
			t.Fatalf("parseUpstreamProxy(%q): %v", tc.raw, err)
		}
		if got.Host != tc.host || got.Port != tc.port || got.User != tc.user || got.Pass != tc.pass {
			t.Fatalf("parseUpstreamProxy(%q) = %+v", tc.raw, got)
		}
	}

	invalid := []string{
		"",
		"http://gateway.example:1080",
		"socks5://gateway.example",
		"socks5://:1080",
		"socks5://gateway.example:0",
		"socks5://gateway.example:70000",
		"socks5://gateway.example:notaport",
		"socks5://gateway.example:1080/path",
		"socks5://gateway.example:1080?x=1",
		"socks5://gateway.example:1080#frag",
	}
	for _, raw := range invalid {
		if _, err := parseUpstreamProxy(raw); err == nil {
			t.Fatalf("parseUpstreamProxy(%q) must fail", raw)
		}
	}
}

func TestUpstreamProxyErrorDoesNotLeakCredentials(t *testing.T) {
	const secret = "topsecret"
	_, err := parseUpstreamProxy("socks5://user:" + secret + "@gateway.example:notaport")
	if err == nil {
		t.Fatal("invalid port must fail")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the credential: %v", err)
	}
}

func TestUpstreamProxyConfigShape(t *testing.T) {
	t.Setenv("UPSTREAM_PROXY", "socks5://user:p%40ss@gateway.example:1080")
	cfg, err := upstreamProxyConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("configured upstream must produce a config")
	}
	if cfg.Endpoint != "gateway.example:1080" {
		t.Fatalf("endpoint = %q", cfg.Endpoint)
	}
	if cfg.Raw != "socks5://gateway.example:1080" {
		t.Fatalf("raw identity must not carry credentials: %q", cfg.Raw)
	}
	if strings.Contains(cfg.Name, "user") || strings.Contains(cfg.Name, "p@ss") {
		t.Fatalf("name leaked credentials: %q", cfg.Name)
	}

	var outbound map[string]any
	if err := json.Unmarshal(cfg.XrayCfg, &outbound); err != nil {
		t.Fatal(err)
	}
	if outbound["protocol"] != "socks" || outbound["tag"] != upstreamOutTag {
		t.Fatalf("outbound = %v", outbound)
	}
	settings, _ := outbound["settings"].(map[string]any)
	servers, _ := settings["servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("servers = %v", settings["servers"])
	}
	server, _ := servers[0].(map[string]any)
	if server["address"] != "gateway.example" || server["port"] != float64(1080) {
		t.Fatalf("server = %v", server)
	}
	users, _ := server["users"].([]any)
	if len(users) != 1 {
		t.Fatalf("users = %v", server["users"])
	}
	user, _ := users[0].(map[string]any)
	if user["user"] != "user" || user["pass"] != "p@ss" {
		t.Fatalf("user = %v", user)
	}
}

func TestUpstreamProxyConfigEmpty(t *testing.T) {
	t.Setenv("UPSTREAM_PROXY", "")
	cfg, err := upstreamProxyConfig()
	if err != nil || cfg != nil {
		t.Fatalf("empty UPSTREAM_PROXY = %+v, %v; want nil, nil", cfg, err)
	}
}

func TestRenderUpstreamProxyOutbound(t *testing.T) {
	t.Setenv("UPSTREAM_PROXY", "socks5://user:pass@gateway.example:1080")
	t.Setenv("CF_RELAY_URL", "")
	t.Setenv("XRAY_MUX", "0")
	t.Setenv("XRAY_FRAGMENT", "0")
	t.Setenv("XRAY_TCP_CONGESTION", "")
	cfg, err := upstreamProxyConfig()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sel := NewProxySelector(dir, "https://probe.example/", 28001, 28002, time.Minute)
	path, err := sel.renderXrayConfig(*cfg, sel.SOCKSPort(), 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("rendered config mode = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var full map[string]any
	if err := json.Unmarshal(data, &full); err != nil {
		t.Fatal(err)
	}
	outbounds, _ := full["outbounds"].([]any)
	if len(outbounds) < 3 {
		t.Fatalf("outbounds = %v", outbounds)
	}
	first, _ := outbounds[0].(map[string]any)
	if first["protocol"] != "socks" || first["tag"] != upstreamOutTag {
		t.Fatalf("outbounds[0] = %v, want the socks upstream", first)
	}
	settings, _ := first["settings"].(map[string]any)
	servers, _ := settings["servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("servers = %v", settings["servers"])
	}
	server, _ := servers[0].(map[string]any)
	if server["address"] != "gateway.example" || server["port"] != float64(1080) {
		t.Fatalf("server = %v", server)
	}
}

func TestNewProxyManagerUpstreamForcesSingleInstance(t *testing.T) {
	t.Setenv("UPSTREAM_PROXY", "socks5://gateway.example:1080")
	t.Setenv("CF_RELAY_URL", "")
	m := NewProxyManager(t.TempDir(), "https://probe.example/", freePortBlock(t, 1), 5, nil, time.Minute)
	defer m.Stop()
	if m.upstreamErr != nil {
		t.Fatalf("upstream error: %v", m.upstreamErr)
	}
	if m.upstream == nil {
		t.Fatal("upstream config missing")
	}
	if len(m.instances) != 1 || len(m.statuses) != 1 {
		t.Fatalf("instances=%d statuses=%d, want 1/1", len(m.instances), len(m.statuses))
	}
	pool, err := m.initialPool(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != 1 || pool[0].Endpoint != "gateway.example:1080" {
		t.Fatalf("initial pool = %+v", pool)
	}
}

func TestNewProxyManagerInvalidUpstreamFailsClosed(t *testing.T) {
	const secret = "topsecret"
	t.Setenv("UPSTREAM_PROXY", "socks5://user:"+secret+"@gateway.example:notaport")
	m := NewProxyManager(t.TempDir(), "https://probe.example/", freePortBlock(t, 1), 3, nil, time.Minute)
	defer m.Stop()
	if m.upstreamErr == nil {
		t.Fatal("invalid upstream must record an error")
	}
	if strings.Contains(m.upstreamErr.Error(), secret) {
		t.Fatalf("startup error leaked credentials: %v", m.upstreamErr)
	}
	if _, err := m.initialPool(nil); err == nil {
		t.Fatal("initialPool must fail closed, not fetch a subscription pool")
	}
	if len(m.instances) != 1 {
		t.Fatalf("instances = %d, want 1", len(m.instances))
	}
}

func TestRefreshSubscriptionsNoOpInUpstreamMode(t *testing.T) {
	m := &ProxyManager{
		upstream: &ProxyConfig{Name: "upstream", Endpoint: "gateway.example:1080"},
		subURLs:  []string{"http://127.0.0.1:1/subscription"},
	}
	start := time.Now()
	m.RefreshSubscriptions()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("upstream mode must not fetch subscriptions (took %s)", elapsed)
	}
}
