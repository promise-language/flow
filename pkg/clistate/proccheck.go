package clistate

import (
	"fmt"
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

	// Verify the executable path matches.
	observed := exePath(pid)
	if observed == "" {
		return false
	}
	return filepath.Clean(observed) == filepath.Clean(exe)
}

// exePath returns the executable path for pid. It tries /proc/<pid>/exe first
// (Linux), then falls back to `ps -o comm=` (macOS). Returns "" on failure.
func exePath(pid int) string {
	// Linux: /proc/<pid>/exe is a symlink to the executable.
	if target, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return target
	}
	// macOS: `ps -o comm=` returns the full path.
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
