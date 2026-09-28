package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	// Apply memory tuning before anything else
	if v := os.Getenv("GOGC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			debug.SetGCPercent(n)
		}
	}
	if v := os.Getenv("GOMEMLIMIT"); v != "" {
		if limit, err := parseBytes(v); err == nil {
			debug.SetMemoryLimit(limit)
		}
	}

	bannerLog("Starting V2Ray Proxy...")

	xrayDir := defaultXrayDir
	testURL := defaultHealthCheckURL
	subURL := ""
	portBase := defaultPortBase
	instanceCount := defaultInstanceCount
	apiPort := defaultAPIPort

	if v := os.Getenv("SUBSCRIPTION_URL"); v != "" {
		subURL = v
	}
	if v := os.Getenv("HEALTH_CHECK_URL"); v != "" {
		testURL = v
	}
	if v := os.Getenv("XRAY_DIR"); v != "" {
		xrayDir = v
	}
	portBase = portOrDefault("PORT_BASE", portBase)
	if v := os.Getenv("PROXY_INSTANCES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			instanceCount = n
		}
	}
	apiPort = portOrDefault("API_PORT", apiPort)
	if v := os.Getenv("HEALTH_CHECK_URL"); v != "" && isPlainHTTP(v) {
		warnLog("HEALTH_CHECK_URL %q uses plain HTTP: DPI RST-injects port-80 even through working tunnels (false downs + churn). Use an https:// generate_204 URL.", v)
	}

	// Subscription URL resolution: merge SUBSCRIPTION_URLS + SUBSCRIPTION_URL + file > stdin.
	// Both env vars accept comma/newline/space-separated lists; dead URLs are
	// skipped automatically via parallel failover fetch.
	var subURLs []string
	subURLs = append(subURLs, splitURLs(os.Getenv("SUBSCRIPTION_URLS"))...)
	subURLs = append(subURLs, splitURLs(subURL)...)
	if len(subURLs) == 0 {
		subFile := filepath.Join(configDir, subscriptionFile)
		if data, err := os.ReadFile(subFile); err == nil {
			subURLs = append(subURLs, splitURLs(string(data))...)
		}
	}

	if len(subURLs) == 0 {
		fmt.Print("Enter subscription URL: ")
		if _, err := fmt.Scanln(&subURL); err != nil {
			errLog("scan failed: %v", err)
		}
		subURLs = splitURLs(subURL)
		if len(subURLs) == 0 {
			errLog("Subscription URL is required")
			os.Exit(1)
		}
		if err := os.MkdirAll(configDir, 0755); err != nil {
			errLog("mkdir failed: %v", err)
			os.Exit(1)
		}
		// 0600: subscription URLs often carry provider tokens, and configDir
		// is usually a host-shared volume.
		if err := os.WriteFile(filepath.Join(configDir, subscriptionFile), []byte(subURL), 0600); err != nil {
			errLog("write subscription failed: %v", err)
			os.Exit(1)
		}
	}
	infoLog("configured %d subscription source(s)", len(subURLs))

	if err := EnsureXray(xrayDir); err != nil {
		errLog("Xray setup failed: %v", err)
		os.Exit(1)
	}

	manager := NewProxyManager(xrayDir, testURL, portBase, instanceCount, subURLs, healthCheckInterval)

	// Start API first so /health and /proxies answer even while proxies populate.
	apiActualPort := startAPI(manager, apiPort)
	infoLog("API available at http://0.0.0.0:%d/proxies", apiActualPort)

	// Start HTTP bridges first (fixed ports): claimed instances serve at once,
	// stragglers start serving the moment they land their proxy.
	for _, inst := range manager.instances {
		startHTTPProxy(
			fmt.Sprintf("0.0.0.0:%d", inst.HTTPPort()),
			fmt.Sprintf("127.0.0.1:%d", inst.SOCKSPort()),
		)
	}

	// Stable aggregate endpoints BEFORE populate: fixed ports routing
	// per-connection to the fastest alive instance. They must exist while
	// the pool is still grinding (thin pools take minutes) — with nothing
	// alive yet they fail fast per connection until instances land.
	// Long-polling clients pin these and never care which upstream serves.
	aggSocks, aggHTTP := pickAggregatePorts(
		aggregatePort("AGGREGATE_SOCKS_PORT", defaultAggSocksPort),
		aggregatePort("AGGREGATE_HTTP_PORT", defaultAggHttpPort),
		apiActualPort, manager.usedPorts())
	var aggSocksAddr, aggHTTPAddr string
	if aggSocks > 0 {
		aggSocksAddr = fmt.Sprintf("0.0.0.0:%d", aggSocks)
	}
	if aggHTTP > 0 {
		aggHTTPAddr = fmt.Sprintf("0.0.0.0:%d", aggHTTP)
	}
	manager.setAggregate(aggSocksAddr, aggHTTPAddr)
	startAggregator(manager, aggSocks, aggHTTP)
	if aggSocksAddr != "" || aggHTTPAddr != "" {
		bannerLog(fmt.Sprintf("Stable endpoints (pin bots here): SOCKS5 %s  HTTP %s", aggSocksAddr, aggHTTPAddr))
	}
	if n := len(cleanIPMap()); n > 0 {
		infoLog("Clean-IP map active: %d entries (CLEAN_IP_MAP; SNI untouched)", n)
	}

	debugLog("Starting %d instance(s)...", manager.InstanceCount())
	if err := manager.Start(); err != nil {
		errLog("Manager start failed: %v", err)
		os.Exit(1)
	}
	// Start only returns when ALL instances hold a working proxy.
	printSummaryTable(manager.GetStatuses())

	bannerLog(fmt.Sprintf("Working proxies: %d/%d", manager.AliveCount(), manager.InstanceCount()))

	go subscriptionLoop(manager)
	go healthCheckLoop(manager)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	infoLog("Shutting down...")
	manager.Stop()
}

func subscriptionLoop(manager *ProxyManager) {
	ticker := time.NewTicker(subscriptionRefreshInterval)
	defer ticker.Stop()
	for range ticker.C {
		debugLog("Refreshing subscriptions...")
		manager.RefreshSubscriptions()
	}
}

func healthCheckLoop(manager *ProxyManager) {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()
	for range ticker.C {
		manager.HealthCheckAll()
		manager.WatchdogCheck()
	}
}

func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	multiplier := int64(1)
	if strings.HasSuffix(s, "MIB") {
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "MIB")
	} else if strings.HasSuffix(s, "MB") {
		multiplier = 1000 * 1000
		s = strings.TrimSuffix(s, "MB")
	} else if strings.HasSuffix(s, "GIB") {
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GIB")
	} else if strings.HasSuffix(s, "GB") {
		multiplier = 1000 * 1000 * 1000
		s = strings.TrimSuffix(s, "GB")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, err
	}
	return n * multiplier, nil
}
