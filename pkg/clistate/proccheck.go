package clistate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ProcessAlive reports whether pid is alive AND its executable path matches
// exe. Returns false when either check fails or exe is empty.
//
// The executable comparison defeats PID reuse: a process identifier alone is
// reusable, and an unrelated process that inherited the number must not be
// mistaken for the step.
func ProcessAlive(pid int, exe string) bool {
	if exe == "" {
		return false
	}

	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 probes liveness without delivering a signal.
	err = p.Signal(syscall.Signal(0))
	if err != nil {
		// ESRCH: no such process — dead.
		// EPERM: process exists but we lack permission — alive.
		if err == syscall.EPERM {
			// Still need to verify the executable matches.
		} else {
			return false
		}
	}

	// Verify the executable path matches. Use `ps` for cross-platform
	// (darwin + linux) support without build tags.
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return false
	}
	observed := strings.TrimSpace(string(out))
	if observed == "" {
		return false
	}
	return filepath.Clean(observed) == filepath.Clean(exe)
}
