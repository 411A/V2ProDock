package main

// Integration-layer tests: the HTTP API surface against a real manager and
// real listeners on loopback. No xray binary, no internet — but real ports,
// real JSON, and real mux routing (including the POST-only /refresh rule).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func testClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

// startTestStack boots a manager plus its API on free ports and waits until
// the API answers. Callers must use distinct base ports per test because the
// API listener lives until the test process exits (no Close handle by design:
// production never stops the API either).
func startTestStack(t *testing.T, base int, n int) (m *ProxyManager, apiURL string) {
	t.Helper()
	m = NewProxyManager(t.TempDir(), "http://probe.invalid/", base, n,
		[]string{"http://127.0.0.1:9/sub"}, time.Minute)
	port := startAPI(m, base+2*n+10)
	apiURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	client := testClient()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(apiURL + "/health")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return m, apiURL
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("integration: API on %s never answered", apiURL)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := testClient().Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return resp.StatusCode, body
}

func TestAPIHealthContract(t *testing.T) {
	_, api := startTestStack(t, 27700, 2)
	code, body := getJSON(t, api+"/health")
	if code != http.StatusOK {
		t.Fatalf("/health status %d", code)
	}
	// Nothing is ok yet: the contract says degraded, with exact counts.
	if body["status"] != "degraded" {
		t.Errorf("/health status = %v, want degraded", body["status"])
	}
	if body["instances"] != float64(2) || body["alive"] != float64(0) {
		t.Errorf("/health counts wrong: %v", body)
	}
}

func TestAPIProxiesAndAll(t *testing.T) {
	_, api := startTestStack(t, 27720, 2)
	resp, err := testClient().Get(api + "/all")
	if err != nil {
		t.Fatal(err)
	}
	var all []InstanceStatus
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(all) != 2 {
		t.Fatalf("/all returned %d instances, want 2", len(all))
	}
	for _, s := range all {
		if s.SOCKS == "" || s.HTTP == "" || s.Status != "starting" {
			t.Errorf("/all entry malformed: %+v", s)
		}
	}
	resp, err = testClient().Get(api + "/proxies")
	if err != nil {
		t.Fatal(err)
	}
	var alive []InstanceStatus
	if err := json.NewDecoder(resp.Body).Decode(&alive); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(alive) != 0 {
		t.Errorf("/proxies must be empty before populate, got %d", len(alive))
	}
}

func TestAPIProxiesExposeStability(t *testing.T) {
	// Hermes ranks stable-slow above flappy-fast: the stability fields must
	// survive JSON encoding on every /proxies entry.
	m, api := startTestStack(t, 27800, 1)
	m.markOK(0, "stable-one", 5*time.Millisecond)
	resp, err := testClient().Get(api + "/proxies")
	if err != nil {
		t.Fatal(err)
	}
	var alive []InstanceStatus
	if err := json.NewDecoder(resp.Body).Decode(&alive); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(alive) != 1 {
		t.Fatalf("/proxies returned %d entries, want 1", len(alive))
	}
	if alive[0].OkStreak != 0 {
		t.Fatalf("OkStreak = %d, want 0 (never health-checked)", alive[0].OkStreak)
	}
}

func TestAPIVPNReportsDisabled(t *testing.T) {
	if _, err := os.Stat(vpnStatusPath()); err == nil {
		t.Skip("integration: live vpn status file present, disabled-path untestable")
	}
	_, api := startTestStack(t, 27740, 1)
	_, body := getJSON(t, api+"/vpn")
	if body["enabled"] != false {
		t.Errorf("/vpn enabled = %v, want false without sidecar", body["enabled"])
	}
}

func TestAPIRefreshPOST(t *testing.T) {
	_, api := startTestStack(t, 27760, 1)
	resp, err := testClient().Post(api+"/refresh", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /refresh status %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "refreshing" {
		t.Errorf("POST /refresh body = %v", body)
	}
}

func TestAPIRefreshGET405(t *testing.T) {
	_, api := startTestStack(t, 27780, 1)
	resp, err := testClient().Get(api + "/refresh")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /refresh status %d, want 405 (POST-only mux)", resp.StatusCode)
	}
}
