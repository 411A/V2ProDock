package main

// Production-hardening regressions: fail-safe env parsing, bounded xray
// download, and secret file permissions. All hermetic (httptest servers,
// temp dirs); the hang-server test shrinks the timeout var and restores it.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPortOrDefault(t *testing.T) {
	const key = "V2PRODOCK_TEST_PORT"
	cases := []struct {
		in   string
		want int
	}{
		{"", 27018},
		{"27019", 27019},
		{"1", 1},
		{"65535", 65535},
		{"0", 27018},
		{"-5", 27018},
		{"65536", 27018},
		{"abc", 27018},
		{"27 018", 27018},
		{"", 27018},
	}
	for _, tc := range cases {
		t.Setenv(key, tc.in)
		if got := portOrDefault(key, 27018); got != tc.want {
			t.Errorf("portOrDefault(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestFetchXrayZipRejectsBadArchive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not a zip"))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	if err := fetchXrayZip(srv.URL, dir, filepath.Join(dir, "xray")); err == nil {
		t.Fatal("bad archive must fail the attempt (retry/fail-fast path)")
	}
}

func TestFetchXrayZipTimeoutBounded(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Blackhole until the CLIENT goes away: client timeout cancels the
		// request context, so the handler (and server Close) releases fast.
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	t.Cleanup(hang.Close)
	old := xrayDownloadTimeout
	xrayDownloadTimeout = 100 * time.Millisecond
	defer func() { xrayDownloadTimeout = old }()
	dir := t.TempDir()
	start := time.Now()
	err := fetchXrayZip(hang.URL, dir, filepath.Join(dir, "xray"))
	if err == nil {
		t.Fatal("hanging download must time out, never wedge boot")
	}
	if el := time.Since(start); el > 20*time.Second {
		t.Fatalf("timeout took %s, must be bounded near %s", el, xrayDownloadTimeout)
	}
}

// An over-cap subscription must be REJECTED, never silently truncated:
// io.ReadAll(io.LimitReader(body, cap)) returns the first N bytes and NO error
// at the cap, so the truncation used to surface as an unreadable-JSON error
// with nothing pointing at the size limit.
func TestFetchOneURLRejectsOverCapBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		chunk := strings.Repeat("a", 64<<10)
		for range (fetchMaxBody / len(chunk)) + 2 {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	body, err := fetchOneURL(&http.Client{Timeout: 30 * time.Second}, srv.URL)
	if err == nil {
		t.Fatalf("over-cap response must error, got %d bytes back", len(body))
	}
	if body != "" {
		t.Fatalf("truncated body must not be returned to the parser, got %d bytes", len(body))
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Fatalf("error must name the size cap, got: %v", err)
	}
}

// Just under the cap must still come back whole: the overflow probe must not
// reject a legitimately large (but legal) subscription.
func TestFetchOneURLAllowsLargeLegalBody(t *testing.T) {
	payload := `{"links":"` + strings.Repeat("b", 1<<20) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	body, err := fetchOneURL(&http.Client{Timeout: 30 * time.Second}, srv.URL)
	if err != nil {
		t.Fatalf("1 MiB payload must be accepted: %v", err)
	}
	if body != payload {
		t.Fatalf("payload altered: got %d bytes, want %d", len(body), len(payload))
	}
}

func TestRenderedXrayConfigNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits not enforced on windows")
	}
	dir := t.TempDir()
	s := &ProxySelector{xrayDir: dir}
	cfg := mustParseChurn(t, churnVlessA)
	path, err := s.renderXrayConfig(cfg, 27991, 0)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0077 != 0 {
		t.Fatalf("rendered config perm %o: group/other must have no bits (UUIDs inside)", perm)
	}
}
