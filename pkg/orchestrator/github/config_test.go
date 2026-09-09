package github

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
)

// A gate's timeout is the only thing standing between a wedged gate and a
// runner that never returns, and a zero duration is not "no deadline" here —
// it is a deadline that has already expired. Left unfilled, every gate in
// every project reports OutcomeTimedOut without a process ever being spawned,
// and the outcome that means "retry this unchanged" is handed out for a
// configuration that will produce it again forever.
func TestConfigGateTimeoutIsFilledIn(t *testing.T) {
	got := Config{}.withDefaults()
	if got.GateTimeout <= 0 {
		t.Fatalf("GateTimeout = %v — an unset timeout is one that has already expired", got.GateTimeout)
	}

	// The declared value is a project's to set, and withDefaults must not
	// overwrite one that was.
	declared := Config{GateTimeout: 3 * time.Second}.withDefaults()
	if declared.GateTimeout != 3*time.Second {
		t.Errorf("GateTimeout = %v, want the declared 3s", declared.GateTimeout)
	}
}

// The default VerifyCmd must point at the compiled binary, not a shell
// script that no longer exists. A consumer that sets no VerifyCmd gets this
// default, so it must be a command that can actually run.
func TestConfigVerifyCmdDefaultIsTheBinary(t *testing.T) {
	got := Config{}.withDefaults()
	if len(got.VerifyCmd) != 1 || got.VerifyCmd[0] != "bin/verify" {
		t.Errorf("default VerifyCmd = %v, want [bin/verify]", got.VerifyCmd)
	}

	// A declared value must not be overwritten.
	declared := Config{VerifyCmd: []string{"make", "check"}}.withDefaults()
	if len(declared.VerifyCmd) != 2 || declared.VerifyCmd[0] != "make" {
		t.Errorf("VerifyCmd = %v, want the declared [make check]", declared.VerifyCmd)
	}
}

// The default sits UNDER the step's own budget, which is what makes a wedged
// gate get caught by its own deadline and reported as OutcomeTimedOut. Raise
// it past the step timeout and the step dies first: the gate then has no
// outcome at all, and the failure is attributed to the step rather than to the
// wait it was actually waiting on.
func TestConfigGateTimeoutFitsInsideAStep(t *testing.T) {
	gate := Config{}.withDefaults().GateTimeout
	step := flow.DefaultStepBudget().Timeout
	if gate >= step {
		t.Errorf("default GateTimeout %v >= default step timeout %v — the step dies before the gate's own deadline fires", gate, step)
	}
}

// WorktreeDir is the one field withDefaults must NOT fill in. `"."` is not a
// location — it is wherever the process happened to be started — and the moment
// it is stored here every consumer of the field inherits it: the arena
// identity, the claim state, gate discovery and the verify command all
// silently became the operator's working directory (#286). New resolves the
// field instead, deriving or refusing.
func TestConfigWorktreeDirIsNotDefaulted(t *testing.T) {
	if got := (Config{}).withDefaults().WorktreeDir; got != "" {
		t.Errorf("WorktreeDir = %q, want it left empty for New to resolve", got)
	}
	// A declared value must not be overwritten either.
	declared := Config{WorktreeDir: "/w/checkout"}.withDefaults()
	if declared.WorktreeDir != "/w/checkout" {
		t.Errorf("WorktreeDir = %q, want the declared /w/checkout", declared.WorktreeDir)
	}
}

// A relative WorktreeDir is REFUSED, not resolved. Resolving one would re-import
// the process working directory through the back door — the caller would have
// configured a path and got wherever the operator stood.
func TestNewRefusesARelativeWorktreeDir(t *testing.T) {
	_, err := New(Config{
		WorktreeDir: "some/checkout",
		Owner:       "acme",
		Repo:        "widget",
		BinaryName:  "issue",
		Token:       "fake-token",
	})
	if err == nil {
		t.Fatal("New accepted a relative WorktreeDir")
	}
	if !strings.Contains(err.Error(), "WorktreeDir") {
		t.Errorf("err = %v, want it to name the field the caller has to fix", err)
	}
	// Refused BEFORE git is spawned: the anchor is resolved first, so a bad one
	// costs no subprocess and no network round trip.
	if strings.Contains(err.Error(), "git ") || strings.Contains(err.Error(), "resolve repo") {
		t.Errorf("err = %v, want the refusal to precede any git invocation", err)
	}
}

// An absolute WorktreeDir is the caller's answer and is kept exactly: New
// resolves the repository in it, and every later reader — arena(), the state
// dir, gate discovery — inherits that one value.
func TestNewKeepsAnAbsoluteWorktreeDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"remote", "add", "origin", "https://github.com/acme/widget.git"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	b, err := New(Config{WorktreeDir: dir, BinaryName: "issue", Token: "fake-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.cfg.WorktreeDir != dir {
		t.Errorf("WorktreeDir = %q, want the configured %q", b.cfg.WorktreeDir, dir)
	}
	if b.cfg.Owner != "acme" || b.cfg.Repo != "widget" {
		t.Errorf("resolved %s/%s, want acme/widget from the worktree's own origin", b.cfg.Owner, b.cfg.Repo)
	}
	// The arena identity is that same path, not a second derivation of it.
	if got := string(b.arena().Id); got != dir {
		t.Errorf("ArenaId = %q, want the worktree %q", got, dir)
	}
}
