package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

func findFreePort(start int) int {
	for port := start; port <= maxPort; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			ln.Close()
			return port
		}
	}
	return start
}

func startAPI(manager *ProxyManager, basePort int) int {
	port := findFreePort(basePort)

	mux := http.NewServeMux()

	mux.HandleFunc("/proxies", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		proxies := manager.GetAliveStatuses()
		if err := json.NewEncoder(w).Encode(proxies); err != nil {
			log.Printf("encode /proxies failed: %v", err)
		}
	})

	mux.HandleFunc("/all", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		statuses := manager.GetStatuses()
		if err := json.NewEncoder(w).Encode(statuses); err != nil {
			log.Printf("encode /all failed: %v", err)
		}
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		total := manager.InstanceCount()
		alive := manager.AliveCount()
		status := "ok"
		if alive == 0 {
			status = "degraded"
		}
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"status":    status,
			"instances": total,
			"alive":     alive,
		}); err != nil {
			log.Printf("encode /health failed: %v", err)
		}
	})

	mux.HandleFunc("/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		go manager.RefreshSubscriptions()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"status": "refreshing"}); err != nil {
			log.Printf("encode /refresh failed: %v", err)
		}
	})

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		ReadHeaderTimeout: 3 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    2048,
	}

	go func() {
		log.Printf("API server on :%d", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("API server error: %v", err)
		}
	}()

	return port
}
