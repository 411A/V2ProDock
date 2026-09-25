package main

// Plan-layer tests: validate the startup topology BEFORE any xray process or
// network probe runs — port layout math, free-port discovery, and the
// populate budget. A failure here means "would misconfigure", not "crashed".

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestStartupPortPlan locks the documented layout: instance i of N gets
// SOCKS=base+i and HTTP=base+N+i, all distinct, all starting.
func TestStartupPortPlan(t *testing.T) {
	const n = 3
	m := NewProxyManager(t.TempDir(), "http://probe.invalid/", 27600, n,
		[]string{"http://127.0.0.1:9/sub"}, time.Minute)
	if got := m.InstanceCount(); got != n {
		t.Fatalf("plan: %d instances, want %d", got, n)
	}
	statuses := m.GetStatuses()
	if len(statuses) != n {
		t.Fatalf("plan: %d statuses, want %d", len(statuses), n)
	}
	socks := make([]int, 0, n)
	https := make([]int, 0, n)
	seen := map[int]string{}
	for _, s := range statuses {
		if s.Status != "starting" {
			t.Fatalf("plan: instance %d status %q, want starting", s.Index, s.Status)
		}
		sp := mustPort(t, s.SOCKS)
		hp := mustPort(t, s.HTTP)
		for _, p := range []int{sp, hp} {
			if prev, dup := seen[p]; dup {
				t.Fatalf("plan: port %d assigned twice (%s)", p, prev)
			}
		}
		seen[sp] = fmt.Sprintf("socks%d", s.Index)
		seen[hp] = fmt.Sprintf("http%d", s.Index)
		socks = append(socks, sp)
		https = append(https, hp)
	}
	base := socks[0]
	for i := range n {
		if socks[i] != base+i {
			t.Fatalf("plan: SOCKS layout %v, want contiguous from %d", socks, base)
		}
		if https[i] != base+n+i {
			t.Fatalf("plan: HTTP layout %v, want base+N+i", https)
		}
	}
}

// TestFindFreePortSkipsHeld verifies discovery never hands out a port that is
// currently bound (same bind semantics: :port on all interfaces).
func TestFindFreePortSkipsHeld(t *testing.T) {
	const base = 27930
	for i := range 4 {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", base+i))
		if err != nil {
			t.Skipf("plan: test range busy: %v", err)
		}
		defer ln.Close()
	}
	if got := findFreePort(base); got < base+4 {
		t.Fatalf("plan: findFreePort(%d) = %d, must skip 4 held ports", base, got)
	}
	if got := findFreePort(base); got > maxPort {
		t.Fatalf("plan: findFreePort(%d) = %d, above maxPort", base, got)
	}
}

// TestFetchPoolPlanRejectsEmpty ensures startup fails fast with no sources
// instead of probing nothing for the whole populate budget.
func TestFetchPoolPlanRejectsEmpty(t *testing.T) {
	if _, err := fetchPoolWithRetry(nil); err == nil {
		t.Fatal("plan: empty subscription list must error before probing")
	}
	if _, _, err := FetchAnySubscription([]string{"  ", ""}); err == nil {
		t.Fatal("plan: blank subscription list must error")
	}
}

func mustPort(t *testing.T, addr string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Fallback for bare "host:port" without scheme quirks.
		if i := strings.LastIndex(addr, ":"); i >= 0 {
			port = addr[i+1:]
		} else {
			t.Fatalf("unparsable addr %q", addr)
		}
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("unparsable port in %q: %v", addr, err)
	}
	return p
}
