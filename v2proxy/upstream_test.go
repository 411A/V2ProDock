package main

// Unified-source tests: static upstreams parse into ordinary pool configs,
// keep credentials out of status/log identity, merge with subscriptions, and
// do not force a separate one-instance mode.

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseStaticUpstream(t *testing.T) {
	valid := []struct {
		raw    string
		scheme string
		host   string
		port   int
		user   string
		pass   string
		tls    bool
	}{
		{"socks5://gateway.example:1080", "socks5", "gateway.example", 1080, "", "", false},
		{"socks5h://gateway.example:1080", "socks5h", "gateway.example", 1080, "", "", false},
		{"socks://gateway.example:1080", "socks", "gateway.example", 1080, "", "", false},
		{"http://gateway.example:8080", "http", "gateway.example", 8080, "", "", false},
		{"https://gateway.example:8443", "https", "gateway.example", 8443, "", "", true},
		{"socks5://user:pa%40ss@gateway.example:1080", "socks5", "gateway.example", 1080, "user", "pa@ss", false},
		{"socks5://[2001:db8::1]:1080", "socks5", "2001:db8::1", 1080, "", "", false},
	}
	for _, tc := range valid {
		got, err := parseStaticUpstream("test", tc.raw)
		if err != nil {
			t.Fatalf("parseStaticUpstream(%q): %v", tc.raw, err)
		}
		if got.Scheme != tc.scheme || got.Host != tc.host || got.Port != tc.port ||
			got.User != tc.user || got.Pass != tc.pass || got.TLS != tc.tls {
			t.Fatalf("parseStaticUpstream(%q) = %+v", tc.raw, got)
		}
	}

	invalid := []string{
		"",
		"ftp://gateway.example:1080",
		"http://gateway.example",
		"http://:8080",
		"http://gateway.example:0",
		"http://gateway.example:70000",
		"http://gateway.example:notaport",
		"http://gateway.example:8080/path",
		"http://gateway.example:8080?x=1",
		"http://gateway.example:8080#frag",
	}
	for _, raw := range invalid {
		if _, err := parseStaticUpstream("test", raw); err == nil {
			t.Fatalf("parseStaticUpstream(%q) must fail", raw)
		}
	}
}

func TestStaticUpstreamErrorDoesNotLeakCredentials(t *testing.T) {
	const secret = "topsecret"
	_, err := parseStaticUpstream("test", "socks5://user:"+secret+"@gateway.example:notaport")
	if err == nil {
		t.Fatal("invalid port must fail")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the credential: %v", err)
	}
}

func TestStaticUpstreamConfigShape(t *testing.T) {
	socks, err := staticUpstreamConfig("alpha", "socks5://user:p%40ss@gateway.example:1080")
	if err != nil {
		t.Fatal(err)
	}
	if socks.Endpoint != "gateway.example:1080" {
		t.Fatalf("endpoint = %q", socks.Endpoint)
	}
	if socks.Raw != "socks5://gateway.example:1080" {
		t.Fatalf("raw identity must not carry credentials: %q", socks.Raw)
	}
	if !socks.Static || socks.Source != "static:alpha" {
		t.Fatalf("static identity = %+v", socks)
	}
	var outbound map[string]any
	if err := json.Unmarshal(socks.XrayCfg, &outbound); err != nil {
		t.Fatal(err)
	}
	if outbound["protocol"] != "socks" || outbound["tag"] != upstreamOutTag {
		t.Fatalf("outbound = %v", outbound)
	}
	settings, _ := outbound["settings"].(map[string]any)
	servers, _ := settings["servers"].([]any)
	server, _ := servers[0].(map[string]any)
	if server["address"] != "gateway.example" || server["port"] != float64(1080) {
		t.Fatalf("server = %v", server)
	}
	users, _ := server["users"].([]any)
	user, _ := users[0].(map[string]any)
	if user["user"] != "user" || user["pass"] != "p@ss" {
		t.Fatalf("user = %v", user)
	}

	httpP, err := staticUpstreamConfig("plain-http", "http://gateway.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	var httpOut map[string]any
	if err := json.Unmarshal(httpP.XrayCfg, &httpOut); err != nil {
		t.Fatal(err)
	}
	if httpOut["protocol"] != "http" {
		t.Fatalf("http outbound = %v", httpOut)
	}
	if _, ok := httpOut["streamSettings"]; ok {
		t.Fatalf("plain http must not get TLS settings: %v", httpOut)
	}

	httpsP, err := staticUpstreamConfig("secure-http", "https://gateway.example:8443")
	if err != nil {
		t.Fatal(err)
	}
	var httpsOut map[string]any
	if err := json.Unmarshal(httpsP.XrayCfg, &httpsOut); err != nil {
		t.Fatal(err)
	}
	ss, _ := httpsOut["streamSettings"].(map[string]any)
	if ss["security"] != "tls" {
		t.Fatalf("https outbound must carry TLS: %v", httpsOut)
	}
}

func TestStaticUpstreamConfigsCombined(t *testing.T) {
	t.Setenv("STATIC_UPSTREAMS", "alpha=socks5://a.example:1080\nbeta=https://b.example:8443")
	t.Setenv("UPSTREAM_PROXY", "socks5://c.example:1080")
	cfgs, err := staticUpstreamConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfgs) != 3 {
		t.Fatalf("configs = %d, want 3: %+v", len(cfgs), cfgs)
	}
	want := []struct {
		name     string
		endpoint string
		source   string
	}{
		{"alpha", "a.example:1080", "static:alpha"},
		{"beta", "b.example:8443", "static:beta"},
		{"upstream", "c.example:1080", "static:upstream"},
	}
	for i, w := range want {
		if cfgs[i].Name != w.name || cfgs[i].Endpoint != w.endpoint || cfgs[i].Source != w.source {
			t.Fatalf("config[%d] = %+v, want %+v", i, cfgs[i], w)
		}
	}
}

func TestStaticUpstreamsConfigured(t *testing.T) {
	t.Setenv("STATIC_UPSTREAMS", "")
	t.Setenv("UPSTREAM_PROXY", "")
	if staticUpstreamsConfigured() {
		t.Fatal("empty vars must not enable static sources")
	}
	t.Setenv("UPSTREAM_PROXY", "socks5://gateway.example:1080")
	if !staticUpstreamsConfigured() {
		t.Fatal("UPSTREAM_PROXY must count as a static source")
	}
}

func TestNewProxyManagerMergesStaticSources(t *testing.T) {
	t.Setenv("STATIC_UPSTREAMS", "alpha=socks5://a.example:1080")
	t.Setenv("UPSTREAM_PROXY", "socks5://c.example:1080")
	t.Setenv("CF_RELAY_URL", "")
	m := NewProxyManager(t.TempDir(), "https://probe.example/", freePortBlock(t, 3), 3, nil, time.Minute)
	defer m.Stop()
	if m.staticErr != nil {
		t.Fatalf("static error: %v", m.staticErr)
	}
	if len(m.staticConfigs) != 2 {
		t.Fatalf("static configs = %d, want 2", len(m.staticConfigs))
	}
	if len(m.instances) != 3 {
		t.Fatalf("instances = %d, want 3 (no forced single mode)", len(m.instances))
	}
	pool, err := m.initialPool(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != 2 {
		t.Fatalf("initial pool = %d, want 2", len(pool))
	}
}

func TestInvalidStaticUpstreamFailsClosed(t *testing.T) {
	const secret = "topsecret"
	t.Setenv("STATIC_UPSTREAMS", "bad=socks5://user:"+secret+"@gateway.example:notaport")
	t.Setenv("UPSTREAM_PROXY", "")
	t.Setenv("CF_RELAY_URL", "")
	m := NewProxyManager(t.TempDir(), "https://probe.example/", freePortBlock(t, 1), 1, nil, time.Minute)
	defer m.Stop()
	if m.staticErr == nil {
		t.Fatal("invalid static source must record an error")
	}
	if strings.Contains(m.staticErr.Error(), secret) {
		t.Fatalf("startup error leaked credentials: %v", m.staticErr)
	}
	if _, err := m.initialPool(nil); err == nil {
		t.Fatal("initialPool must fail closed, not fetch a subscription pool")
	}
}

func TestShuffleKeepsStaticFirst(t *testing.T) {
	in := []ProxyConfig{
		{Name: "sub-a", Endpoint: "sub-a.example:443"},
		{Name: "static-a", Endpoint: "static-a.example:1080", Static: true},
		{Name: "sub-b", Endpoint: "sub-b.example:443"},
		{Name: "static-b", Endpoint: "static-b.example:1080", Static: true},
	}
	shuffleConfigs(in, 7)
	if in[0].Name != "static-a" || in[1].Name != "static-b" {
		t.Fatalf("static order = %s,%s; want static-a,static-b", in[0].Name, in[1].Name)
	}
	if in[2].Name != "sub-a" && in[2].Name != "sub-b" {
		t.Fatalf("subscription remainder = %s", in[2].Name)
	}
}

func TestDedupKeepsStaticFirst(t *testing.T) {
	cfg := func(name, endpoint, source string, static bool) ProxyConfig {
		return ProxyConfig{Name: name, Endpoint: endpoint, Source: source, Static: static}
	}
	in := []ProxyConfig{
		cfg("sub", "gateway.example:1080", "subscription:https://sub.example", false),
		cfg("static", "gateway.example:1080", "static:gateway", true),
	}
	got := dedupConfigs(in)
	if len(got) != 1 || !got[0].Static || got[0].Source != "static:gateway" {
		t.Fatalf("dedup must keep the static declaration: %+v", got)
	}
}

func TestAllocationStatsShortfall(t *testing.T) {
	m := &ProxyManager{statuses: []InstanceStatus{
		{Index: 0, Status: "ok"},
		{Index: 1, Status: "down"},
		{Index: 2, Status: "down"},
	}}
	requested, allocated, reason := m.AllocationStats()
	if requested != 3 || allocated != 1 {
		t.Fatalf("requested=%d allocated=%d, want 3/1", requested, allocated)
	}
	if !strings.Contains(reason, "1 of 3") {
		t.Fatalf("shortfall reason = %q", reason)
	}
}

func TestSourceAndRolePropagation(t *testing.T) {
	const endpoint = "static.example:1080"
	m := backendManager([]InstanceStatus{{Index: 0, Status: "ok"}}, 27951, 27961)
	m.instances[0].configs = []ProxyConfig{{
		Name: "alpha", Endpoint: endpoint, Source: "static:alpha", Static: true,
	}}
	m.instances[0].activeIndex = 0
	m.markOK(0, "alpha", time.Millisecond)
	s := m.statusOf(0)
	if s.Source != "static:alpha" || s.Role != "primary" {
		t.Fatalf("status = %+v", s)
	}
	m.markDown(0, "dead")
	s = m.statusOf(0)
	if s.Role != "standby" {
		t.Fatalf("down role = %q, want standby", s.Role)
	}
}

func TestSubscriptionSourceLabelRedacts(t *testing.T) {
	got := subscriptionSourceLabel("https://user:secret@sub.example/path?token=abc")
	if got != "subscription:https://sub.example" {
		t.Fatalf("source label = %q", got)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "token") {
		t.Fatalf("source label leaked credentials: %q", got)
	}
}

func TestAPIHealthShortfall(t *testing.T) {
	t.Setenv("THROUGHPUT_CHECK", "0")
	t.Setenv("STATIC_UPSTREAMS", "")
	t.Setenv("UPSTREAM_PROXY", "")
	m, api := startTestStack(t, 27860, 3)
	m.markOK(0, "one", time.Millisecond)
	_, health := getJSON(t, api+"/health")
	if health["requested"] != float64(3) || health["allocated"] != float64(1) ||
		health["shortfall"] != float64(2) {
		t.Fatalf("/health allocation = %v, want requested=3 allocated=1 shortfall=2", health)
	}
	if health["shortfall_reason"] == nil {
		t.Fatalf("/health must explain the shortfall: %v", health)
	}
}

func TestAggregateKeepsBestAliveSource(t *testing.T) {
	m := backendManager([]InstanceStatus{{
		Index: 0, Status: "ok", Endpoint: "static.example:1080", Degraded: true,
	}}, 27951, 27961)
	m.buildCacheLocked()
	cands := m.aggregateCandidates(false)
	if len(cands) != 1 {
		t.Fatalf("aggregate candidates = %v, want the one alive degraded source", cands)
	}
}

func TestRenderStaticUpstreamOutbound(t *testing.T) {
	cfg, err := staticUpstreamConfig("alpha", "socks5://user:pass@gateway.example:1080")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CF_RELAY_URL", "")
	t.Setenv("XRAY_MUX", "0")
	t.Setenv("XRAY_FRAGMENT", "0")
	t.Setenv("XRAY_TCP_CONGESTION", "")
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
	first, _ := outbounds[0].(map[string]any)
	if first["protocol"] != "socks" || first["tag"] != upstreamOutTag {
		t.Fatalf("outbounds[0] = %v, want static socks upstream", first)
	}
}
