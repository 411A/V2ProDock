package main

import (
	"fmt"
	"io"
	"log"
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
		log.Printf("Xray ready at %s", xrayBin)
		return nil
	}

	return downloadXray(xrayDir, xrayBin)
}

func downloadXray(xrayDir, xrayBin string) error {
	os.MkdirAll(xrayDir, 0755)

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

	url := fmt.Sprintf("https://github.com/XTLS/Xray-core/releases/latest/download/Xray-%s-%s.zip", osName2, archName)
	log.Printf("Downloading xray: %s", url)

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	tmpFile := filepath.Join(xrayDir, "xray.zip")
	out, err := os.Create(tmpFile)
	if err != nil {
		return err
	}
	buf := make([]byte, 32*1024)
	_, err = io.CopyBuffer(out, resp.Body, buf)
	out.Close()
	if err != nil {
		return err
	}

	if err := extractZip(tmpFile, xrayDir); err != nil {
		return fmt.Errorf("extract failed: %w", err)
	}
	os.Remove(tmpFile)

	if runtime.GOOS != "windows" {
		os.Chmod(xrayBin, 0755)
	}

	if _, err := os.Stat(xrayBin); err != nil {
		return fmt.Errorf("xray binary not found after extraction at %s", xrayBin)
	}

	log.Printf("Xray installed: %s", xrayBin)
	return nil
}

func extractZip(src, dst string) error {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		// Try unzip first
		cmd := exec.Command("unzip", "-o", src, "-d", dst)
		if output, err := cmd.CombinedOutput(); err != nil {
			// unzip might not be available, try tar or python
			if strings.Contains(string(output), "not found") || strings.Contains(string(output), "No such file") {
				cmd2 := exec.Command("python3", "-c",
					fmt.Sprintf("import zipfile; zipfile.ZipFile('%s').extractall('%s')", src, dst))
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
