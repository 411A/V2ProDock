package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("Starting V2Ray Proxy...")

	xrayDir := "/root/xray"
	testURL := "http://httpbin.org/ip"
	subURL := ""
	portBase := defaultPortBase
	instanceCount := 1
	apiPort := 27018

	if v := os.Getenv("SUBSCRIPTION_URL"); v != "" {
		subURL = v
	}
	if v := os.Getenv("HEALTH_CHECK_URL"); v != "" {
		testURL = v
	}
	if v := os.Getenv("XRAY_DIR"); v != "" {
		xrayDir = v
	}
	if v := os.Getenv("PORT_BASE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			portBase = n
		}
	}
	if v := os.Getenv("PROXY_INSTANCES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			instanceCount = n
		}
	}
	if v := os.Getenv("API_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			apiPort = n
		}
	}

	// Subscription URL resolution: SUBSCRIPTION_URLS > SUBSCRIPTION_URL > file > stdin
	var subURLs []string
	if v := os.Getenv("SUBSCRIPTION_URLS"); v != "" {
		subURLs = strings.Split(v, ",")
		for i := range subURLs {
			subURLs[i] = strings.TrimSpace(subURLs[i])
		}
	} else if subURL != "" {
		// Single URL, will be shared across all instances
		subURLs = []string{subURL}
	} else {
		subFile := filepath.Join("/root/config", "subscription.txt")
		if data, err := os.ReadFile(subFile); err == nil {
			subURL = strings.TrimSpace(string(data))
			subURLs = []string{subURL}
		}
	}

	if len(subURLs) == 0 || subURLs[0] == "" {
		fmt.Print("Enter subscription URL: ")
		fmt.Scanln(&subURL)
		if subURL == "" {
			log.Fatal("Subscription URL is required")
		}
		os.MkdirAll("/root/config", 0755)
		os.WriteFile(filepath.Join("/root/config", "subscription.txt"), []byte(subURL), 0644)
		subURLs = []string{subURL}
	}

	if err := EnsureXray(xrayDir); err != nil {
		log.Fatalf("Xray setup failed: %v", err)
	}

	manager := NewProxyManager(xrayDir, testURL, portBase, instanceCount, subURLs, 60*time.Second)

	log.Printf("Starting %d instance(s)...", manager.InstanceCount())
	if err := manager.Start(); err != nil {
		log.Fatalf("Manager start failed: %v", err)
	}

	// Start HTTP proxy for each instance (SOCKS5 -> HTTP bridge)
	for _, inst := range manager.instances {
		startHTTPProxy(
			fmt.Sprintf("0.0.0.0:%d", inst.HTTPPort()),
			fmt.Sprintf("127.0.0.1:%d", inst.SOCKSPort()),
		)
	}

	// Start API server
	apiActualPort := startAPI(manager, apiPort)
	log.Printf("API available at http://0.0.0.0:%d/proxies", apiActualPort)

	// Print summary
	for _, s := range manager.GetStatuses() {
		log.Printf("Instance %d: %s | SOCKS5=%s HTTP=%s | status=%s latency=%dms",
			s.Index, s.Name, s.SOCKS, s.HTTP, s.Status, s.LatMs)
	}

	go subscriptionLoop(manager)
	go healthCheckLoop(manager)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down...")
	manager.Stop()
}

func subscriptionLoop(manager *ProxyManager) {
	ticker := time.NewTicker(120 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		log.Println("Refreshing subscriptions...")
		manager.RefreshSubscriptions()
	}
}

func healthCheckLoop(manager *ProxyManager) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		manager.HealthCheckAll()
	}
}
