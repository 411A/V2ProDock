//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// isolateChild puts the xray child in its own process group so a stop kills
// the whole tree (negative-PID signals), never just the parent.
func isolateChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// processAlive reports whether p is still running (SIG 0 probe, no delivery).
func processAlive(p *os.Process) bool {
	if p == nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func sigFor(sig os.Signal) syscall.Signal {
	if sig == os.Interrupt {
		return syscall.SIGTERM
	}
	return syscall.SIGKILL
}

// signalGroup delivers sig to the whole process group (negative PID).
// Precondition: pid owns its group (all our children are launched via
// isolateChild, so pgid == pid). If the group is gone (ESRCH), falls back to
// the single pid; a dead process reports nil either way.
func signalGroup(pid int, sig os.Signal) error {
	s := sigFor(sig)
	if err := syscall.Kill(-pid, s); err == nil {
		return nil
	}
	if err := syscall.Kill(pid, s); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

// killGroup force-kills the whole process group, with the same fallback.
func killGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
