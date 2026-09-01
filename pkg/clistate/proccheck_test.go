package clistate_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/promise-language/flow/pkg/clistate"
)

func TestProcessAlive_Self(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if !clistate.ProcessAlive(os.Getpid(), exe) {
		t.Errorf("ProcessAlive(self) = false, want true")
	}
}

func TestProcessAlive_DeadPID(t *testing.T) {
	// Start a subprocess and wait for it to exit, then check its PID.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if clistate.ProcessAlive(pid, "/bin/true") {
		t.Errorf("ProcessAlive(dead pid) = true, want false")
	}
}

func TestProcessAlive_WrongExe(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// Our own PID is alive, but the exe path does not match.
	_ = exe
	if clistate.ProcessAlive(os.Getpid(), "/no/such/binary") {
		t.Errorf("ProcessAlive(self, wrong exe) = true, want false")
	}
}

func TestProcessAlive_EmptyExe(t *testing.T) {
	if clistate.ProcessAlive(os.Getpid(), "") {
		t.Errorf("ProcessAlive(self, empty exe) = true, want false")
	}
}
