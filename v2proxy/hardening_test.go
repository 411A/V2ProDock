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
