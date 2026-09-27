package main

// Stable aggregate endpoints: one SOCKS5 port + one HTTP port whose listeners
// NEVER restart, surviving every xray switch underneath. Each accepted
// connection is routed to the fastest currently-alive instance at dial time,
// so long-polling clients (Telegram bots via httpx) keep ONE fixed config and
// ride out upstream churn with a reconnect instead of a rewrite+restart.
//
// Cost per connection: two goroutines + 64KiB pooled buffers (the same relay
// primitive as the HTTP bridges). Fail-fast when nothing is alive: the client
// sees connection refused/dial-timeout immediately and retries, instead of
// hanging on a dead port.

import (
	"fmt"
	"net"
	"time"
)

func startAggregator(m *ProxyManager, socksPort, httpPort int) {
	if socksPort > 0 {
		go serveAggregate(m, socksPort, false)
	}
	if httpPort > 0 {
		go serveAggregate(m, httpPort, true)
	}
}

func serveAggregate(m *ProxyManager, port int, httpBackend bool) {
	kind := "SOCKS5"
	if httpBackend {
		kind = "HTTP"
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		errLog("aggregate %s bind :%d failed: %v", kind, port, err)
		return
	}
	infoLog("Aggregate %s on :%d (fastest alive, per-connection failover)", kind, port)
	for {
		c, err := ln.Accept()
		if err != nil {
			debugLog("aggregate :%d accept: %v", port, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go handleAggregateConn(m, c, httpBackend)
	}
}

func handleAggregateConn(m *ProxyManager, client net.Conn, httpBackend bool) {
	backend, ok := m.pickBestBackend(httpBackend)
	if !ok {
		// No alive instance: fail fast so the client retries now, not after
		// a long-poll timeout wedged on a black hole.
		_ = client.Close()
		return
	}
	up, err := net.DialTimeout("tcp", backend, bridgeDialTimeout)
	if err != nil {
		_ = client.Close()
		return
	}
	go relay(up, client)
	go relay(client, up)
}
