package github

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
)

// The pull-request path invokes `gh`, and nothing else in this package does.
// It shipped passing `-C <dir>` — git's flag for selecting a working directory,
// which gh does not have — so every invocation died in argument validation
// before gh contacted GitHub at all, and neither call site had ever run.
//
// These assert the shape of the command line rather than its effect: the whole
// failure was that the arguments were wrong, and a test that mocked the result
// would have passed against the broken version.
func ghArgsFor(t *testing.T, invoke func(*worktree) error) []string {
	t.Helper()
	var got []string
	b := &Orchestrator{cfg: Config{Owner: "acme", Repo: "widget", WorktreeDir: "/tmp/wt"}}
	b.git = &gitOps{
		dir: "/tmp/wt",
		runner: func(_ context.Context, _ string, name string, args ...string) ([]byte, []byte, error) {
			if name == "gh" {
				got = args
				return []byte("https://github.com/acme/widget/pull/1\n"), nil, nil
			}
			// Open checks the worktree is on the claim branch before it will
			// invoke gh, so the git side has to answer plausibly to get there.
			if slices.Contains(args, "rev-parse") {
				return []byte("flow/issue-42\n"), nil, nil
			}
			return nil, nil, nil
		},
	}
	b.out = newOutward("", b.git, b.cfg.Owner, b.cfg.Repo, allowing())
	_ = invoke(&worktree{b: b, issueNum: 42})
	if got == nil {
		t.Fatal("gh was never invoked")
	}
	return got
}

func TestGhInvocationsCarryNoDashC(t *testing.T) {
	// -C is git's. Passing it to gh fails before anything happens, and the
	// convention is easy to re-copy from the git helper next door.
	for name, invoke := range map[string]func(*worktree) error{
		"pr create": func(w *worktree) error {
			_, err := w.Open(context.Background(), "main", "t", "b")
			return err
		},
		"pr merge": func(w *worktree) error {
			return w.Merge(context.Background(), "https://github.com/acme/widget/pull/1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			args := ghArgsFor(t, invoke)
			if slices.Contains(args, "-C") {
				t.Errorf("gh %s carries -C, which gh rejects: %v", name, args)
			}
			if !slices.Contains(args, "--repo") {
				t.Errorf("gh %s does not name the repo: %v", name, args)
			}
			if i := slices.Index(args, "--repo"); i+1 >= len(args) || args[i+1] != "acme/widget" {
				t.Errorf("gh %s: --repo should be owner/repo, got %v", name, args)
			}
		})
	}
}

// The merge must complete before the call returns, because the step after it
// reads the merge commit the merge produced. `--auto` only queues the merge
// behind GitHub's checks — the question stepVerifyMerge just answered against
// the merge result — so the recording step finds nothing and the merge step is
// re-dispatched until the runaway guard stops it (#148). Asserting the argv is
// what keeps the flag from drifting back: a test mocking the merge's result
// would pass against either command line.
func TestGhMergeIsSynchronousSquash(t *testing.T) {
	const prURL = "https://github.com/acme/widget/pull/1"
	args := ghArgsFor(t, func(w *worktree) error {
		return w.Merge(context.Background(), prURL)
	})

	if slices.Contains(args, "--auto") {
		t.Errorf("gh pr merge carries --auto, which only queues the merge: %v", args)
	}
	if !slices.Contains(args, "--squash") {
		t.Errorf("gh pr merge does not name a strategy; --squash is the one this repo allows: %v", args)
	}
	// The URL is positional, and it has to follow the subcommand — gh reads the
	// first non-flag word after `pr merge` as the pull request to act on.
	i := slices.Index(args, "merge")
	if i < 0 || i+1 >= len(args) || args[i+1] != prURL {
		t.Errorf("gh pr merge does not name the pull request straight after the subcommand: %v", args)
	}
}

// The merge happens inside the call now, so gh's exit status IS the answer to
// whether the request landed — and the pr-merged signal this backend writes as
// a side effect of Merge has to follow that status in both directions.
//
// Set over a merge that did not happen, the signal is #148: the step reports
// done, the recording step after it looks for a merge commit that does not
// exist, and resolve re-dispatches until the runaway guard stops it. Unset
// after a merge that did happen, the flow re-merges an already-merged request
// forever. And a refusal has to carry gh's own words out, because "the base
// moved — rebase and measure again" is now an ordinary outcome and the operator
// cannot tell it from a genuinely stuck request without them.
func TestMergeSignalTracksWhetherGhMerged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ghErr    error
		ghStderr string
		wantSet  bool
	}{
		{name: "merged", wantSet: true},
		{
			name:     "refused",
			ghErr:    errors.New("exit status 1"),
			ghStderr: "Pull request is not mergeable: the base branch has moved",
			wantSet:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := newGHMock(t)
			srv := mock.server()
			defer srv.Close()
			b := newMockedOrchestrator(t, mock, srv)
			ctx := t.Context()

			// A claim and a seeded state comment, because the signal write
			// edits that document — with none there is nothing to observe.
			claim, err := b.Claim(ctx, b.refFromIssue(42), nil)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if err := b.SeedState(ctx, claim.ItemRef, []flow.ArtifactSpec{
				{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
			}); err != nil {
				t.Fatalf("SeedState: %v", err)
			}

			git := b.git.runner
			b.git.runner = func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
				if name == "gh" {
					return nil, []byte(tc.ghStderr), tc.ghErr
				}
				return git(ctx, dir, name, args...)
			}

			wt, err := b.Worktree(ctx, claim.ItemRef)
			if err != nil {
				t.Fatalf("Worktree: %v", err)
			}
			mergeErr := wt.Request().Merge(ctx, "https://github.com/o/r/pull/1")

			switch {
			case tc.ghErr == nil && mergeErr != nil:
				t.Fatalf("Merge: %v, want nil — gh merged", mergeErr)
			case tc.ghErr != nil && mergeErr == nil:
				t.Fatal("Merge returned nil though gh refused the merge — the caller records a landing that did not happen")
			case tc.ghErr != nil && !strings.Contains(mergeErr.Error(), tc.ghStderr):
				t.Errorf("Merge error = %q, want it to carry what gh said (%q)", mergeErr, tc.ghStderr)
			}

			state, err := b.Load(ctx, claim.ItemRef)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := state.SignalSet("pr-merged"); got != tc.wantSet {
				t.Errorf("pr-merged = %v, want %v", got, tc.wantSet)
			}
		})
	}
}

func TestGhOpenTargetsTheBranchAndBase(t *testing.T) {
	// --repo removes the dependency on the process working directory, which the
	// runner never sets — so the branch and base must be named explicitly or gh
	// has nothing to work from.
	args := ghArgsFor(t, func(w *worktree) error {
		_, err := w.Open(context.Background(), "main", "title", "body")
		return err
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--base main", "--title title", "--body body"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in: %s", want, joined)
		}
	}
}

// verifyArgv spawns Run(verify) against a real `bin/verify` that records the
// argv it was handed, and returns those lines: the program as invoked, then
// each argument.
//
// `verify` is a COMMAND now, spawned by the same runner gates go through, so
// there is no mockable seam left to assert against — and asserting on a mocked
// result would pass against a version that ran the wrong command, which is the
// shape of defect the `gh -C` flag shipped as.
func verifyArgv(t *testing.T, cmd func(dir string) []string) ([]string, flow.CommandRun) {
	t.Helper()
	requireRealProcesses(t)

	dir := filepath.Join(t.TempDir(), "work tree")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Relative, so it lands in the worktree the command was spawned in — which
	// is also the assertion that Run set cmd.Dir at all.
	writeScript(t, dir, "verify", `printf '%s\n' "$0" "$@" > argv`, 0o755)
	declareCommand(t, dir, flow.CommandVerify)

	b := &Orchestrator{cfg: Config{WorktreeDir: dir, VerifyCmd: cmd(dir), GateTimeout: 30 * time.Second}}
	b.git = &gitOps{dir: dir, runner: func(_ context.Context, _, name string, _ ...string) ([]byte, []byte, error) {
		t.Errorf("a command was spawned through the git runner (%s)", name)
		return nil, nil, nil
	}}

	run, err := (&worktree{b: b}).Run(context.Background(), flow.CommandVerify)
	if err != nil {
		t.Fatalf("Run(verify): %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "argv"))
	if err != nil {
		t.Fatalf("the verify command did not run (detail: %s): %v", run.Detail, err)
	}
	// One line per word, not whitespace-split: the worktree path carries a
	// space, and splitting on it is exactly the bug the space is here to catch.
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"), run
}

// Run(verify) runs the CONFIGURED command, whole. The gate side of this
// pairing — that RunGate does NOT reach it — is TestRunGate_RunsTheEntryPoint
// AndNotTheVerifyCommand, which has to spawn a real process to say so.
func TestVerifyRunsTheConfiguredCommand(t *testing.T) {
	argv, run := verifyArgv(t, func(dir string) []string {
		return []string{filepath.Join(dir, "bin", "verify"), "--wasm"}
	})

	if len(argv) != 2 || !strings.HasSuffix(argv[0], "bin/verify") {
		t.Fatalf("verify ran as %v, want the configured VerifyCmd", argv)
	}
	if argv[1] != "--wasm" {
		t.Errorf("verify ran with args %v, want the configured VerifyCmd", argv[1:])
	}
	// It ran and reported, which is the outcome a caller may branch on.
	if run.Outcome != flow.OutcomeMeasured || run.ExitCode != 0 {
		t.Errorf("run = %+v, want measured with exit 0", run)
	}
	if run.Command != flow.CommandVerify {
		t.Errorf("Command = %q, want the name that was asked for", run.Command)
	}
}

// A Config with no VerifyCmd, after withDefaults, must spawn "bin/verify"
// relative to the worktree. This closes the gap between the default-value test
// (config_test.go) and the dispatch test above: neither alone would catch a
// default that is non-empty but names the wrong command.
func TestVerifyDispatchesTheDefault(t *testing.T) {
	argv, _ := verifyArgv(t, func(string) []string {
		return Config{WorktreeDir: "/tmp/wt"}.withDefaults().VerifyCmd
	})

	if len(argv) != 1 {
		t.Fatalf("verify ran as %v, want the bare default with no arguments", argv)
	}
	if !strings.HasSuffix(argv[0], "bin/verify") {
		t.Errorf("verify dispatched %q, want the default bin/verify", argv[0])
	}
}

// An empty VerifyCmd must be refused, not passed to exec (which would panic
// on a zero-length slice). This is the failure path: if withDefaults were
// ever broken to leave the field empty, this error is what surfaces.
func TestVerifyRefusesEmptyCmd(t *testing.T) {
	dir := t.TempDir()
	declareCommand(t, dir, flow.CommandVerify)
	cfg := Config{WorktreeDir: dir} // deliberately skip withDefaults

	b := &Orchestrator{cfg: cfg}
	b.git = &gitOps{
		dir: cfg.WorktreeDir,
		runner: func(_ context.Context, _ string, _ string, _ ...string) ([]byte, []byte, error) {
			t.Fatal("runner should not be called with an empty VerifyCmd")
			return nil, nil, nil
		},
	}
	run, err := (&worktree{b: b}).Run(context.Background(), flow.CommandVerify)
	if err == nil {
		t.Fatal("Run(verify) with empty VerifyCmd should return an error")
	}
	if !errors.Is(err, flow.ErrUnsupported) {
		t.Errorf("error = %v, want ErrUnsupported — the command is not runnable here", err)
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %q, want mention of empty VerifyCmd", err)
	}
	if run.Outcome != "" {
		t.Errorf("Outcome = %q, want none — no command ran", run.Outcome)
	}
}
