//go:build windows

package main

import (
	"os"
	"os/exec"
)

// processFinished mirrors the unix helper. Windows has no zombie state, and
// processAlive is best-effort there (probing would mean killing), so this can
// only answer "definitely finished" - never falsely claim death.
func processFinished(p *os.Process) bool { return !processAlive(p) }

// isolateChild is a no-op on Windows (no process groups via syscall).
func isolateChild(_ *exec.Cmd) {}

// processAlive is best-effort on Windows: Go cannot probe liveness without
// killing, so report alive and let the port wait + probe verdict decide.
// Instant-crash leftovers are still reaped by the pre-stop in startXray.
func processAlive(p *os.Process) bool {
	return p != nil
}

// signalGroup degrades to a single-process interrupt on Windows.
func signalGroup(pid int, sig os.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(sig)
}

// killGroup degrades to a single-process kill on Windows.
func killGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
