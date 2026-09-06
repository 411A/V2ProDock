package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadVPNStatusMissing(t *testing.T) {
	if _, ok := readVPNStatus(filepath.Join(t.TempDir(), "nope.json")); ok {
		t.Fatalf("missing file must report ok=false (VPN disabled)")
	}
}

func TestReadVPNStatusBadJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(p, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, ok := readVPNStatus(p); ok {
		t.Fatalf("corrupt status.json must report ok=false")
	}
}

func TestReadVPNStatusContract(t *testing.T) {
	// Contract with vpn-gateway/watchdog.sh write_status(): verified egress proof.
	p := filepath.Join(t.TempDir(), "status.json")
	body := `{"updated":"2026-01-01T00:00:00Z","upstream_socks":"v2prodock:27019",` +
		`"upstream_name":"server-1","tun":"tun0","egress_ip":"1.2.3.4","verified":true,"error":""}`
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	st, ok := readVPNStatus(p)
	if !ok {
		t.Fatalf("valid status.json must report ok=true")
	}
	if st["upstream_socks"] != "v2prodock:27019" {
		t.Errorf("upstream_socks not parsed: %v", st["upstream_socks"])
	}
	if st["verified"] != true {
		t.Errorf("verified must be true when tunnel carries working-config egress: %v", st["verified"])
	}
	if st["egress_ip"] != "1.2.3.4" {
		t.Errorf("egress_ip not parsed: %v", st["egress_ip"])
	}
}

func TestVPNStatusPath(t *testing.T) {
	if got := vpnStatusPath(); got == "" {
		t.Fatalf("vpnStatusPath must be non-empty")
	}
}
