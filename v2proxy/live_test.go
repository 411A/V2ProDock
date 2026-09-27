package main

// Live-subscription end-to-end: the FULL production path against REAL public
// configs (fetch -> parse -> dedup -> render -> load -> probe -> switch).
//
// Gated by BOTH envs (internet + real xray required; never in default/CI runs):
//   V2PRODOCK_LIVE_SUBS=1  V2PRODOCK_REAL_XRAY=/path/to/xray
//   [V2PRODOCK_LIVE_URLS="url1 url2"]  (default: the curated public pools)
//   go test -run TestLiveSubscriptionE2E -v -timeout 590s .
//
// Vantage honesty: public pools rot fast and may be fully dead from any given
// network (proven: 0/60 working with all servers TCP-dead from one vantage,
// while our stack reported every one correctly bounded). So nothing here
// requires a working public node:
//   - fetch/parse/dedup thresholds FAIL on source-shape drift (actionable);
//   - fragment load-safety holds for every real-world config SHAPE (render +
//     genuine-binary load, both modes) regardless of upstream liveness;
//   - dead-pool switching must stay bounded with serving intact;
//   - working nodes, when found opportunistically, must survive the chain.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

var defaultLiveURLs = []string{
	"https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/main/verified/configs.txt",
	"https://raw.githubusercontent.com/masir-sefid/Sub/main/@Masir_Sefid.txt",
}

func needLiveSubs(t *testing.T) ([]string, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode: live-sub e2e skipped")
	}
	if os.Getenv("V2PRODOCK_LIVE_SUBS") == "" {
		t.Skip("V2PRODOCK_LIVE_SUBS not set: live-sub e2e skipped")
	}
	bin := needRealXray(t)
	urls := defaultLiveURLs
	if v := strings.TrimSpace(os.Getenv("V2PRODOCK_LIVE_URLS")); v != "" {
		urls = splitURLs(v)
	}
	if len(urls) == 0 {
		t.Fatal("no live URLs")
	}
	return urls, bin
}

func tlsSample(pool []ProxyConfig, n int) []ProxyConfig {
	out := make([]ProxyConfig, 0, n)
	for _, c := range pool {
		if len(out) >= n {
			break
		}
		var out_ map[string]any
		if err := json.Unmarshal(c.XrayCfg, &out_); err != nil {
			continue
		}
		if upstreamUsesTLS(out_) {
			out = append(out, c)
		}
	}
	return out
}

// loadReal renders cfg (fragment per env) and requires the GENUINE binary to
// accept it (bind follows). Liveness-independent: xray binds its inbound no
// matter how dead the upstream is — so this proves RENDERER + CHAIN safety
// on real-world config diversity (grpc/ws/reality/vision, custom sockopts).
func loadReal(t *testing.T, dir string, sel *ProxySelector, cfg ProxyConfig, seq uint64) {
	t.Helper()
	port := freeLoopbackPort(t)
	path, err := sel.renderXrayConfig(cfg, port, seq)
	if err != nil {
		t.Fatalf("render %s: %v", cfg.Key(), err)
	}
	defer os.Remove(path)
	cmd, err := launchXray(dir, path)
	if err != nil {
		t.Fatalf("genuine xray rejected %s (proto shape?): %v", cfg.Key(), err)
	}
	defer stopXrayCmdPort(cmd, port)
	if !waitForPort(port, 5*time.Second) {
		t.Fatalf("xray never bound for %s", cfg.Key())
	}
}

func TestLiveSubscriptionE2E(t *testing.T) {
	urls, bin := needLiveSubs(t)

	// 1. Production fetch + parse + dedup.
	pool := dedupConfigs(FetchMergedSubscriptions(urls))
	t.Logf("live pool: %d unique configs from %d source(s)", len(pool), len(urls))
	if len(pool) < 50 {
		t.Fatalf("pool too thin (%d) — sources changed shape, investigate", len(pool))
	}
	protos := map[string]int{}
	tlsN := 0
	for _, c := range pool {
		var out_ map[string]any
		if err := json.Unmarshal(c.XrayCfg, &out_); err != nil {
			continue
		}
		proto, _ := out_["protocol"].(string)
		protos[proto]++
		if upstreamUsesTLS(out_) {
			tlsN++
		}
	}
	t.Logf("protocols=%v tls_marked=%d", protos, tlsN)

	dir := t.TempDir()
	copyRealXray(t, bin, dir)
	sel := NewProxySelector(dir, "", freeLoopbackPort(t), freeLoopbackPort(t), time.Minute)

	// 2. Fragment load-safety on real TLS shapes: every sampled config must
	//    load in the genuine binary both WITHOUT and WITH the chain.
	sample := tlsSample(pool, 12)
	if len(sample) == 0 {
		t.Fatal("no TLS-marked configs sampled — pool shape changed")
	}
	t.Setenv("XRAY_FRAGMENT", "")
	for i, c := range sample {
		loadReal(t, dir, sel, c, uint64(1000+i))
	}
	t.Logf("plain render+load: %d/%d real TLS shapes accepted", len(sample), len(sample))
	t.Setenv("XRAY_FRAGMENT", "1")
	for i, c := range sample {
		loadReal(t, dir, sel, c, uint64(2000+i))
	}
	t.Logf("chained render+load: %d/%d real TLS shapes accepted (no shape regresses)", len(sample), len(sample))

	// 3. Opportunistic working set (cap 25 tries): wherever a node works
	//    plain, the chain must keep it. Zero found = vantage note, not fail.
	t.Setenv("XRAY_FRAGMENT", "")
	var working []ProxyConfig
	for _, c := range sample {
		if len(working) >= 3 {
			break
		}
		if res := sel.probeSnapshotOnTempPort(c); res.Working {
			working = append(working, c)
		}
	}
	t.Logf("opportunistic working set: %d", len(working))
	t.Setenv("XRAY_FRAGMENT", "1")
	for _, c := range working {
		if res := sel.probeSnapshotOnTempPort(c); !res.Working {
			t.Fatalf("FRAGMENT REGRESSION: %s works plain but not chained (%v)", c.Key(), res.Error)
		}
	}

	// 4. Dead-pool switching stays bounded with serving intact and no orphans.
	head := pool
	if len(head) > 40 {
		head = head[:40]
	}
	sel.UpdateConfigs(head)
	if err := sel.startXray(0); err != nil {
		t.Fatalf("seed start: %v", err)
	}
	sel.mu.Lock()
	sel.activeIndex = 0
	seedPID := 0
	if sel.xrayCmd != nil && sel.xrayCmd.Process != nil {
		seedPID = sel.xrayCmd.Process.Pid
	}
	sel.mu.Unlock()
	swStart := time.Now()
	swErr := sel.SwitchToNextExcluding(nil)
	swEl := time.Since(swStart)
	t.Logf("dead-pool switch: err=%v in %s", swErr, swEl.Round(time.Millisecond))
	if swEl > switchBudget+30*time.Second {
		t.Fatalf("dead-pool switch escaped all bounds: %s", swEl)
	}
	if got := sel.currentPID(); got != seedPID {
		t.Fatalf("failed switch must leave serving child alone (%d -> %d)", seedPID, got)
	}
	if n := len(listXrayPIDs(dir)); n > 1 {
		t.Fatalf("temp children leaked: %d xray processes", n)
	}
	// (If the pool head hid a working node the switch lands it — either
	// outcome is correct here; loopback tests already prove the success path.)
}
