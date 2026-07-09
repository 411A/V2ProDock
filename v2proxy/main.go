package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("Starting V2Ray Proxy...")

	xrayDir := "/root/xray"
	testURL := "http://httpbin.org/ip"
	subURL := ""

	if v := os.Getenv("SUBSCRIPTION_URL"); v != "" {
		subURL = v
	}
	if v := os.Getenv("HEALTH_CHECK_URL"); v != "" {
		testURL = v
	}
	if v := os.Getenv("XRAY_DIR"); v != "" {
		xrayDir = v
	}

	if subURL == "" {
		subFile := filepath.Join("/root/config", "subscription.txt")
		if data, err := os.ReadFile(subFile); err == nil {
			subURL = string(data)
		}
	}
	if subURL == "" {
		fmt.Print("Enter subscription URL: ")
		fmt.Scanln(&subURL)
		if subURL == "" {
			log.Fatal("Subscription URL is required")
		}
		os.MkdirAll("/root/config", 0755)
		os.WriteFile(filepath.Join("/root/config", "subscription.txt"), []byte(subURL), 0644)
	}

	if err := EnsureXray(xrayDir); err != nil {
		log.Fatalf("Xray setup failed: %v", err)
	}

	selector := NewProxySelector(xrayDir, testURL, 60*time.Second)

	log.Printf("Fetching: %s", subURL)
	configs, err := FetchSubscription(subURL)
	if err != nil {
		log.Fatalf("Subscription fetch failed: %v", err)
	}
	log.Printf("Parsed %d configs", len(configs))
	selector.UpdateConfigs(configs)

	log.Println("Finding working proxy...")
	if err := selector.StartWithBest(); err != nil {
		log.Fatalf("No working proxy: %v", err)
	}
	log.Println(selector.GetStatus())

	// Start HTTP proxy via Go (forwards through SOCKS5)
	startHTTPProxy("0.0.0.0:27020", "127.0.0.1:27019")

	go subscriptionLoop(subURL, selector)
	go healthCheckLoop(selector)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down...")
	selector.Stop()
}

func subscriptionLoop(subURL string, selector *ProxySelector) {
	ticker := time.NewTicker(120 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		log.Println("Refreshing subscription...")
		configs, err := FetchSubscription(subURL)
		if err != nil {
			log.Printf("Refresh failed: %v", err)
			continue
		}
		log.Printf("Refreshed: %d configs", len(configs))
		selector.UpdateConfigs(configs)
	}
}

func healthCheckLoop(selector *ProxySelector) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if !selector.ShouldCheck() {
			continue
		}
		if selector.HealthCheck() {
			continue
		}
		log.Println("Proxy failed, switching...")
		if err := selector.SwitchToNext(); err != nil {
			log.Printf("Switch failed: %v", err)
		}
	}
}
