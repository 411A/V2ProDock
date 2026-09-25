package main

// Performance-layer tests: benchmarks for every hot path (subscription
// parsing, pool math, probe ordering). Run with:
//   go test -run='^$' -bench=. -benchtime=1000x .
// There are no absolute time assertions — benchmarks fail only on correctness
// bugs in the setup; regressions are caught by comparing -benchstat runs.

import (
	"encoding/base64"
	"fmt"
	"testing"
)

var benchVlessURL = "vless://a1b2c3d4-e5f6-7890-abcd-ef1234567890@example.com:443?type=grpc&security=reality&pbk=pubkey123&sni=example.com&serviceName=testservice#VLESS_Bench"

var benchVmessURL = "vmess://" + base64.StdEncoding.EncodeToString([]byte(
	`{"add":"1.2.3.4","port":443,"id":"uuid123","aid":0,"net":"ws","path":"/ws","host":"example.com","tls":"tls","ps":"VMess_Bench"}`))

func benchPool(n int) []ProxyConfig {
	out := make([]ProxyConfig, 0, n)
	for i := range n {
		out = append(out, ProxyConfig{
			Name:     fmt.Sprintf("bench-%d", i),
			Raw:      fmt.Sprintf("vless://u%d@10.0.0.%d:443#bench-%d", i, i%250+1, i),
			Endpoint: fmt.Sprintf("10.0.0.%d:443", i%250+1),
		})
	}
	return out
}

func BenchmarkParseVlessURL(b *testing.B) {
	for b.Loop() {
		if _, err := parseToXrayConfig(benchVlessURL); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseVmessURL(b *testing.B) {
	for b.Loop() {
		if _, err := parseToXrayConfig(benchVmessURL); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDedupConfigs(b *testing.B) {
	pool := benchPool(1000)
	b.ResetTimer()
	for b.Loop() {
		if got := dedupConfigs(pool); len(got) != 250 {
			b.Fatalf("dedup = %d, want 250 unique endpoints", len(got))
		}
	}
}

func BenchmarkSplitURLs(b *testing.B) {
	in := "https://a.example/sub,https://b.example/sub\nhttps://c.example/sub;https://a.example/sub"
	for b.Loop() {
		if got := splitURLs(in); len(got) != 3 {
			b.Fatalf("split = %v", got)
		}
	}
}

func BenchmarkSwitchOrder(b *testing.B) {
	for b.Loop() {
		if got := switchOrder(64, 7, 3); len(got) != 63 {
			b.Fatalf("order len = %d", len(got))
		}
	}
}

func BenchmarkEndpointKey(b *testing.B) {
	for b.Loop() {
		if got := endpointKey("Example.COM ", 443); got != "example.com:443" {
			b.Fatalf("key = %q", got)
		}
	}
}

func BenchmarkShortName(b *testing.B) {
	in := "https://example.com:443/some-path-with-a-long-tail-1234567890"
	for b.Loop() {
		if shortName(in) == "" {
			b.Fatal("empty short name")
		}
	}
}

func BenchmarkCmdlineIsManaged(b *testing.B) {
	good := "xray\x00run\x00-c\x00/root/xray/config-27019.json\x00"
	for b.Loop() {
		if !cmdlineIsManaged(good, "/root/xray") {
			b.Fatal("must match")
		}
	}
}

func BenchmarkShuffleConfigs(b *testing.B) {
	pool := benchPool(500)
	b.ResetTimer()
	for b.Loop() {
		shuffleConfigs(pool, int64(b.N))
	}
}

func BenchmarkDecodeBase64Content(b *testing.B) {
	enc := base64.StdEncoding.EncodeToString([]byte(benchVlessURL + "\n" + benchVlessURL))
	for b.Loop() {
		if got := decodeBase64Content(enc); got == enc {
			b.Fatal("must decode")
		}
	}
}
