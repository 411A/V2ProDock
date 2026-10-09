//go:build !windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
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

// processFinished reports whether p has ALREADY exited, distinguishing a dead
// process from an unreaped ZOMBIE. processAlive() cannot: signal 0 succeeds for a
// zombie, so a child that crashed a moment ago still reads as alive and any
// "is it dead?" decision made on it is wrong.
//
// On linux the state is field 3 of /proc/<pid>/stat. Elsewhere (and if /proc is
// unreadable) this degrades to !processAlive, which is the old behaviour - it can
// only ever answer "definitely dead", never falsely claim death.
func processFinished(p *os.Process) bool {
	if p == nil {
		return true
	}
	if raw, err := os.ReadFile("/proc/" + strconv.Itoa(p.Pid) + "/stat"); err == nil {
		// comm (field 2) is parenthesised and may contain spaces, so scan past
		// the final ')' before reading the state byte.
		if i := bytes.LastIndexByte(raw, ')'); i >= 0 && i+2 < len(raw) {
			switch raw[i+2] {
			case 'Z', 'X', 'x': // zombie, dead, dead
				return true
			}
			return false
		}
	}
	return !processAlive(p)
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
