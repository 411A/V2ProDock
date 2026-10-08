package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func EnsureXray(xrayDir string) error {
	xrayBin := filepath.Join(xrayDir, "xray")
	if runtime.GOOS == "windows" {
		xrayBin += ".exe"
	}

	if _, err := os.Stat(xrayBin); err == nil {
		debugLog("Xray ready at %s", xrayBin)
		return nil
	}

	return downloadXray(xrayDir, xrayBin)
}

func downloadXray(xrayDir, xrayBin string) error {
	if err := os.MkdirAll(xrayDir, 0755); err != nil {
		return fmt.Errorf("mkdir failed: %w", err)
	}

	arch := runtime.GOARCH
	osName := runtime.GOOS

	var archName string
	switch arch {
	case "amd64", "x86_64":
		archName = "64"
	case "arm64", "aarch64":
		archName = "arm64-v8a"
	case "arm":
		archName = "arm32-v7a"
	default:
		archName = "64"
	}

	var osName2 string
	switch osName {
	case "linux":
		osName2 = "linux"
	case "darwin":
		osName2 = "macos"
	case "windows":
		osName2 = "windows"
	default:
		osName2 = "linux"
	}

	url := fmt.Sprintf(xrayDownloadURLTmpl, osName2, archName)
	infoLog("Downloading xray: %s", url)

	var err error
	for attempt := 1; attempt <= xrayDownloadAttempts; attempt++ {
		if err = fetchXrayZip(url, xrayDir, xrayBin); err == nil {
			return nil
		}
		warnLog("xray download attempt %d/%d failed: %v", attempt, xrayDownloadAttempts, err)
	}
	return err
}

// fetchXrayZip performs one download+extract+verify pass. The HTTP client
// carries an explicit timeout: plain http.Get has none, so a blackholed
// route wedged boot forever with zero log output.
func fetchXrayZip(url, xrayDir, xrayBin string) error {
	client := &http.Client{Timeout: xrayDownloadTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	tmpFile := filepath.Join(xrayDir, "xray.zip")
	out, err := os.Create(tmpFile)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, resp.Body)
	// Close error matters: a failed flush on a written file means a TRUNCATED
	// archive, which surfaces later as a confusing "unzip failed" instead of
	// the real cause.
	if closeErr := out.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("write %s failed: %w", tmpFile, copyErr)
	}

	if err := extractZip(tmpFile, xrayDir); err != nil {
		return fmt.Errorf("extract failed: %w", err)
	}
	_ = os.Remove(tmpFile)

	if runtime.GOOS != "windows" {
		if err := os.Chmod(xrayBin, 0755); err != nil {
			return fmt.Errorf("chmod failed: %w", err)
		}
	}

	if _, err := os.Stat(xrayBin); err != nil {
		return fmt.Errorf("xray binary not found after extraction at %s", xrayBin)
	}

	infoLog("Xray installed: %s", xrayBin)
	return nil
}

// unzipAbsent reports whether an unzip invocation failed because the binary
// itself is missing, rather than because the archive is bad.
//
// exec.Command resolves the path at Run time, so a missing binary returns
// exec.ErrNotFound with EMPTY CombinedOutput - the child never ran, so it
// printed nothing. Probing that output with strings.Contains("not found") can
// therefore never match, which made the python3 fallback below unreachable on
// every host without unzip: boot died on a raw exec error instead of falling
// back. Found by booting the daemon on a host without unzip.
func unzipAbsent(err error, output []byte) bool {
	return errors.Is(err, exec.ErrNotFound) ||
		strings.Contains(string(output), "not found") ||
		strings.Contains(string(output), "No such file")
}

func extractZip(src, dst string) error {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		// Try unzip first
		cmd := exec.Command("unzip", "-o", src, "-d", dst)
		if output, err := cmd.CombinedOutput(); err != nil {
			// unzip might not be available, try python
			if unzipAbsent(err, output) {
				cmd2 := exec.Command("python3", "-c",
					fmt.Sprintf("import zipfile;zipfile.ZipFile('%s').extractall('%s')", src, dst))
				if output2, err2 := cmd2.CombinedOutput(); err2 != nil {
					return fmt.Errorf("extract failed: %s %s", string(output), string(output2))
				}
				return nil
			}
			return fmt.Errorf("unzip failed: %w", err)
		}
		return nil
	}

	// Windows: use PowerShell
	psCmd := fmt.Sprintf("Expand-Archive -Path '%s' -DestinationPath '%s' -Force", src, dst)
	cmd := exec.Command("powershell", "-Command", psCmd)
	return cmd.Run()
}
