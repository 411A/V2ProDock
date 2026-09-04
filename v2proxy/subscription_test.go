package main

import (
	"encoding/base64"
	"testing"
)

func TestDedupEndpointCollision(t *testing.T) {
	in := []ProxyConfig{
		{Name: "one", Raw: "vless://u1@1.2.3.4:443#one", Endpoint: endpointKey("1.2.3.4", 443)},
		{Name: "two", Raw: "vless://u2@1.2.3.4:443#two", Endpoint: endpointKey("1.2.3.4", 443)},
		{Name: "three", Raw: "vless://u3@5.6.7.8:443#three", Endpoint: endpointKey("5.6.7.8", 443)},
	}
	got := dedupConfigs(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 unique endpoints, got %d", len(got))
	}
	if got[0].Name != "one" || got[1].Name != "three" {
		t.Fatalf("expected first-seen to win, got %+v", got)
	}
	if endpointKey("Example.COM", 443) != "example.com:443" {
		t.Fatalf("endpointKey must lowercase host, got %s", endpointKey("Example.COM", 443))
	}
	if (ProxyConfig{Raw: "x"}).Key() != "x" {
		t.Fatalf("Key must fall back to Raw when Endpoint is empty")
	}
}

func TestDecodeBase64Content(t *testing.T) {
	raw := "vless://uuid@127.0.0.1:443?security=reality#test"
	if res := decodeBase64Content(raw); res != raw {
		t.Errorf("expected plain URL to remain untouched, got %s", res)
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(raw))
	if res := decodeBase64Content(encoded); res != raw {
		t.Errorf("expected base64 string to decode to raw, got %s", res)
	}
}

func TestParseToXrayConfigVless(t *testing.T) {
	vlessURL := "vless://a1b2c3d4-e5f6-7890-abcd-ef1234567890@example.com:443?type=grpc&security=reality&pbk=pubkey123&sni=example.com&serviceName=testservice#VLESS_Test"
	cfg, err := parseToXrayConfig(vlessURL)
	if err != nil {
		t.Fatalf("failed to parse vless: %v", err)
	}
	if cfg.Name != "VLESS_Test" {
		t.Errorf("expected name VLESS_Test, got %s", cfg.Name)
	}
}

func TestSplitURLs(t *testing.T) {
	in := "http://192.168.1.87:27141/subscription.txt,https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/main/top100.txt"
	got := splitURLs(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 URLs, got %d: %v", len(got), got)
	}
	if got[0] != "http://192.168.1.87:27141/subscription.txt" {
		t.Errorf("first URL wrong: %s", got[0])
	}
	dup := splitURLs("https://a.example, https://a.example\nhttps://b.example;https://b.example")
	if len(dup) != 2 {
		t.Errorf("expected deduped 2 URLs, got %v", dup)
	}
	if len(splitURLs("  , \n ")) != 0 {
		t.Errorf("expected empty for blank input")
	}
}

func TestFetchAnyNoURLs(t *testing.T) {
	if _, _, err := FetchAnySubscription(nil); err == nil {
		t.Errorf("expected error for no URLs")
	}
	if got := FetchMergedSubscriptions(nil); len(got) != 0 {
		t.Errorf("expected nil for no URLs, got %d", len(got))
	}
}

func TestParseToXrayConfigVmess(t *testing.T) {
	vmessJSON := `{"add":"1.2.3.4","port":443,"id":"uuid123","aid":0,"net":"ws","path":"/ws","host":"example.com","tls":"tls","ps":"VMess_Test"}`
	b64 := base64.StdEncoding.EncodeToString([]byte(vmessJSON))
	vmessURL := "vmess://" + b64

	cfg, err := parseToXrayConfig(vmessURL)
	if err != nil {
		t.Fatalf("failed to parse vmess: %v", err)
	}
	if cfg.Name != "VMess_Test" {
		t.Errorf("expected name VMess_Test, got %s", cfg.Name)
	}
}
