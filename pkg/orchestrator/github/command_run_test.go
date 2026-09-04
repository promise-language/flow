package github

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
)

// Worktree.Run returns a RUN, not a bare error, and these pin what the runner
// is allowed to observe.
//
// The whole point of the change: a failing check, a lock timeout and a missing
// binary used to be one value. They have different budget consequences — a
// retry that is free, a retry that is pointless, and a real result that costs a
// round — so a caller that cannot tell them apart charges all three the same.
//
// Where a command differs from a gate is that there is NO ENVELOPE. A command
// reports by exiting, so a non-zero exit is a MEASUREMENT and never
// broke_contract.

// commandWorktree wires a worktree whose verify command is the given script,
// in a directory whose name carries a space — if anything ever put a shell
// between the SDK and the command, the exec line would word-split here.
//
// script == "" writes no bin/verify at all, which is the missing-binary case.
func commandWorktree(t *testing.T, script string, timeout time.Duration) *worktree {
	t.Helper()
	requireRealProcesses(t)

	dir := filepath.Join(t.TempDir(), "work tree")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	declareCommand(t, dir, flow.CommandVerify)
	if script != "" {
		writeScript(t, dir, "verify", script, 0o755)
	}
	b := &Orchestrator{cfg: Config{
		WorktreeDir: dir,
		VerifyCmd:   []string{filepath.Join(dir, "bin", "verify")},
		GateTimeout: timeout,
	}}
	b.git = &gitOps{dir: dir, runner: func(_ context.Context, _, name string, _ ...string) ([]byte, []byte, error) {
		t.Errorf("a command was spawned through the git runner (%s)", name)
		return nil, nil, nil
	}}
	return &worktree{b: b}
}

func TestRunCommand_ClassifiesWhatItObserved(t *testing.T) {
	for _, c := range []struct {
		name     string
		script   string
		timeout  time.Duration
		outcome  flow.Outcome
		exitCode int
	}{{
		// Ran and reported nothing wrong.
		name: "clean run", script: `echo ok`, timeout: 30 * time.Second,
		outcome: flow.OutcomeMeasured, exitCode: 0,
	}, {
		// RAN AND REPORTED FAILURES. This is a measurement, not a broken run:
		// the command did its job and what remains is not sound. Folding it
		// into a non-measured outcome would charge a real result as a fault of
		// the host.
		name: "reports failures", script: `echo "FAIL pkg/x"; exit 1`, timeout: 30 * time.Second,
		outcome: flow.OutcomeMeasured, exitCode: 1,
	}, {
		// Nothing ran. Must never fold into died, which carries "retry is
		// correct": a retry loop pointed at a missing binary never terminates.
		name: "missing binary", script: "", timeout: 30 * time.Second,
		outcome: flow.OutcomeCouldNotStart, exitCode: -1,
	}, {
		name: "killed at the timeout", script: `sleep 30`, timeout: 100 * time.Millisecond,
		outcome: flow.OutcomeTimedOut, exitCode: -1,
	}, {
		name: "killed by a signal", script: `kill -9 $$`, timeout: 30 * time.Second,
		outcome: flow.OutcomeDied, exitCode: -1,
	}} {
		t.Run(c.name, func(t *testing.T) {
			w := commandWorktree(t, c.script, c.timeout)
			run, err := w.Run(context.Background(), flow.CommandVerify)
			if err != nil {
				t.Fatalf("Run returned an error; every way a command fails is an outcome: %v", err)
			}
			if run.Outcome != c.outcome {
				t.Errorf("Outcome = %q, want %q (detail: %s)", run.Outcome, c.outcome, run.Detail)
			}
			if run.ExitCode != c.exitCode {
				t.Errorf("ExitCode = %d, want %d", run.ExitCode, c.exitCode)
			}
			if run.Command != flow.CommandVerify {
				t.Errorf("Command = %q, want the name that was asked for", run.Command)
			}
		})
	}
}

// The failing run carries what the command printed, because that is what a fix
// round re-prompts an agent with. A measurement with no output is a fix round
// asking someone to repair something they cannot see.
func TestRunCommand_AMeasuredFailureCarriesWhatItPrinted(t *testing.T) {
	w := commandWorktree(t, `echo "FAIL pkg/x"; exit 3`, 30*time.Second)
	run, err := w.Run(context.Background(), flow.CommandVerify)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(run.Stdout), "FAIL pkg/x") {
		t.Errorf("Stdout = %q, want what the command printed", run.Stdout)
	}
	if run.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want the command's own status", run.ExitCode)
	}
}

// A caller whose context is already gone gets could_not_start, and nothing is
// spawned. Reporting it as a measurement would attribute the caller's own
// cancellation to the tree.
func TestRunCommand_ACancelledCallerSpawnsNothing(t *testing.T) {
	w := commandWorktree(t, `touch ran`, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	run, err := w.Run(ctx, flow.CommandVerify)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Outcome != flow.OutcomeCouldNotStart {
		t.Errorf("Outcome = %q, want %q", run.Outcome, flow.OutcomeCouldNotStart)
	}
	if _, err := os.Stat(filepath.Join(w.b.cfg.WorktreeDir, "ran")); err == nil {
		t.Error("the command ran for a caller that had already gone away")
	}
}

// A NON-NIL ERROR MEANS NO COMMAND WAS RUN AND NO OUTCOME EXISTS. The two ways
// to get one are a name outside the closed set and a name this orchestrator
// declares nothing for — and "declared but unreachable" would be worse than
// undeclared, because a caller would read a missing outcome as a measurement.
func TestRun_RefusesANameItCannotRun(t *testing.T) {
	for _, c := range []struct {
		name    flow.CommandName
		typed   bool // ErrUnsupported: never here, so retrying is pointless
		mustSay string
	}{
		{"deploy", false, "not one of the three command names"},
		{flow.CommandSetup, true, "setup"},
		{flow.CommandCleanup, true, "cleanup"},
	} {
		t.Run(string(c.name), func(t *testing.T) {
			w := commandWorktree(t, `echo ok`, 30*time.Second)
			run, err := w.Run(context.Background(), c.name)
			if err == nil {
				t.Fatalf("Run(%q) succeeded; this orchestrator cannot run it", c.name)
			}
			if run.Outcome != "" {
				t.Errorf("Outcome = %q, want none — no command ran", run.Outcome)
			}
			if c.typed && !errors.Is(err, flow.ErrUnsupported) {
				t.Errorf("error = %v, want ErrUnsupported", err)
			}
			if !strings.Contains(err.Error(), c.mustSay) {
				t.Errorf("error = %q, want it to say %q", err, c.mustSay)
			}
		})
	}
}

// declareCommand makes an arena hold a command, the way this project holds one:
// a main under tools/build/cmd/<name>. The orchestrator reads that directory to
// answer SupportedCommands, so a fixture that skips it is an arena with no
// commands — and Run refuses one it cannot find, correctly.
func declareCommand(t *testing.T, root string, name flow.CommandName) {
	t.Helper()
	dir := filepath.Join(root, "tools", "build", "cmd", string(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("declare command %q: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("declare command %q: %v", name, err)
	}
}
