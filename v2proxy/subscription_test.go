package main

import (
	"encoding/base64"
	"testing"
)

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
