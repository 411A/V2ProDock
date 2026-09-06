package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

// readVPNStatus parses the shared vpn-gateway status file.
// ok=false means VPN disabled/not booted yet (fail-closed discovery).
func readVPNStatus(path string) (map[string]interface{}, bool) {
	var st map[string]interface{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, false
	}
	if st == nil {
		return nil, false
	}
	return st, true
}

func vpnStatusPath() string {
	return filepath.Join(configDir, "vpn", "status.json")
}

func startAPI(manager *ProxyManager, basePort int) int {
	port := findFreePort(basePort)

	mux := http.NewServeMux()

	mux.HandleFunc("/proxies", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		proxies := manager.GetAliveStatuses()
		if err := json.NewEncoder(w).Encode(proxies); err != nil {
			debugLog("encode /proxies failed: %v", err)
		}
	})

	mux.HandleFunc("/all", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		statuses := manager.GetStatuses()
		if err := json.NewEncoder(w).Encode(statuses); err != nil {
			debugLog("encode /all failed: %v", err)
		}
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		total := manager.InstanceCount()
		alive := manager.AliveCount()
		starting := manager.StartingCount()
		status := "ok"
		if alive == 0 {
			status = "degraded"
		}
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"status":    status,
			"instances": total,
			"alive":     alive,
			"starting":  starting,
		}); err != nil {
			debugLog("encode /health failed: %v", err)
		}
	})

	// /vpn exposes the sidecar gateway pinning proof written to the shared
	// ./config volume (./config/vpn/status.json). It is the strict answer to
	// "does VPN traffic really go FROM the working configs": verified=true
	// means egress-via-tun0 == egress-via-upstream-SOCKS on the last poll.
	mux.HandleFunc("/vpn", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		st, ok := readVPNStatus(vpnStatusPath())
		if !ok {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"enabled": false,
				"ikev2":   "500/udp,4500/udp",
				"note":    "vpn-gateway not started or VPN_ENABLED=0; set VPN_ENABLED=1 + VPN_PASSWORD and compose up",
			})
			return
		}
		st["enabled"] = true
		st["ikev2"] = "500/udp,4500/udp"
		st["guarantee"] = "fail-closed: VPN subnet forwards to tun0 only; verified means tun egress == upstream SOCKS egress"
		if err := json.NewEncoder(w).Encode(st); err != nil {
			debugLog("encode /vpn failed: %v", err)
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
			debugLog("encode /refresh failed: %v", err)
		}
	})

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadTimeout:       apiReadTimeout,
		WriteTimeout:      apiWriteTimeout,
		ReadHeaderTimeout: apiReadHeaderTimeout,
		IdleTimeout:       apiIdleTimeout,
		MaxHeaderBytes:    apiMaxHeaderBytes,
	}

	go func() {
		debugLog("API server on :%d", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errLog("API server error: %v", err)
		}
	}()

	return port
}
