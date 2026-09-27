package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestCleanIPMapParsing(t *testing.T) {
	t.Setenv("CLEAN_IP_MAP", "")
	if cleanIPMap() != nil {
		t.Fatal("unset must yield nil")
	}
	t.Setenv("CLEAN_IP_MAP", "  Blocked.Example=1.2.3.4 , bad-entry , =x, y= , ok.io=5.6.7.8")
	m := cleanIPMap()
	if len(m) != 2 || m["blocked.example"] != "1.2.3.4" || m["ok.io"] != "5.6.7.8" {
		t.Fatalf("must keep 2 valid lowercased entries, got %v", m)
	}
	t.Setenv("CLEAN_IP_MAP", "a")
	if cleanIPMap() != nil {
		t.Fatal("all-malformed must yield nil")
	}
}

func TestRewriteCleanIPURL(t *testing.T) {
	m := map[string]string{"blocked.edge": "1.2.3.4"}
	raw := "vless://uuid@blocked.edge:443?security=tls&sni=real.example&type=ws&host=real.example&path=%2Fws#lbl"
	out := rewriteCleanIP(raw, m)
	if !strings.Contains(out, "uuid@1.2.3.4:443?") {
		t.Fatalf("host:port must be swapped, got %s", out)
	}
	for _, keep := range []string{"sni=real.example", "host=real.example", "path=%2Fws", "#lbl"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("SNI/params/fragment must survive, missing %s in %s", keep, out)
		}
	}
	// Portless links keep working without JoinHostPort brackets.
	out = rewriteCleanIP("trojan://pw@blocked.edge?sni=real.example", m)
	if !strings.Contains(out, "pw@1.2.3.4?") {
		t.Fatalf("portless swap broken: %s", out)
	}
	// Unmapped hosts and empty maps are byte-identical.
	if got := rewriteCleanIP(raw, map[string]string{"other": "9.9.9.9"}); got != raw {
		t.Fatal("unmapped host must pass through")
	}
	if got := rewriteCleanIP(raw, nil); got != raw {
		t.Fatal("nil map must pass through")
	}
}

func TestRewriteCleanIPVmess(t *testing.T) {
	m := map[string]string{"blocked.edge": "1.2.3.4"}
	link := "vmess://" + base64.StdEncoding.EncodeToString([]byte(
		`{"add":"blocked.edge","port":443,"id":"u","net":"ws","tls":"tls","sni":"real.example","host":"real.example","path":"/ws"}`))
	out := rewriteCleanIP(link, m)
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(out, "vmess://"))
	if err != nil {
		t.Fatalf("output must stay valid vmess: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["add"] != "1.2.3.4" || fields["sni"] != "real.example" || fields["host"] != "real.example" {
		t.Fatalf("only add may change: %v", fields)
	}
	// Garbage passes through for parseVmess to reject as before.
	if got := rewriteCleanIP("vmess://!!!not-base64!!!", m); got != "vmess://!!!not-base64!!!" {
		t.Fatal("garbage vmess must pass through")
	}
}

func TestParseAppliesCleanIPMap(t *testing.T) {
	t.Setenv("CLEAN_IP_MAP", "blocked.edge=1.2.3.4")
	cfg, err := parseToXrayConfig("vless://uuid@blocked.edge:443?security=tls&sni=real.example&type=tcp#x")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "1.2.3.4:443" {
		t.Fatalf("endpoint must use clean IP, got %s", cfg.Endpoint)
	}
	js := string(cfg.XrayCfg)
	if !strings.Contains(js, "1.2.3.4") || !strings.Contains(js, "real.example") {
		t.Fatalf("xray cfg must dial clean IP with original SNI: %s", js)
	}
}
