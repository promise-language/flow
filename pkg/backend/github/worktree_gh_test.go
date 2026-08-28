package github

import (
	"context"
	"slices"
	"strings"
	"testing"
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
	b := &Backend{cfg: Config{Owner: "acme", Repo: "widget", WorktreeDir: "/tmp/wt"}}
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
	b.out = newOutward("", b.git, b.cfg.Owner, b.cfg.Repo)
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

// Verify spawns the configured command, and the whole of what it does is which
// command. Asserting on a mocked result would pass against a version that ran
// the wrong one — the same shape of defect as the `gh -C` flag, which shipped
// because nothing exercised the command line.
//
// Gates do not come through here. This seam reports (stdout, stderr, error),
// which is all a COMMAND needs and strictly less than a runner has to observe;
// they are spawned in gate.go and tested against real processes in gate_test.go.
func runArgsFor(t *testing.T, cfg Config, invoke func(*worktree) error) (name string, args []string) {
	t.Helper()
	var gotName string
	var gotArgs []string
	b := &Backend{cfg: cfg}
	b.git = &gitOps{
		dir: cfg.WorktreeDir,
		runner: func(_ context.Context, _ string, n string, a ...string) ([]byte, []byte, error) {
			gotName, gotArgs = n, a
			return nil, nil, nil
		},
	}
	_ = invoke(&worktree{b: b})
	return gotName, gotArgs
}

// Verify runs the CONFIGURED command, whole. The gate side of this pairing —
// that RunGate does NOT reach it — is TestRunGate_RunsTheEntryPointAndNotThe
// VerifyCommand, which has to spawn a real process to say so.
func TestVerifyRunsTheConfiguredCommand(t *testing.T) {
	cfg := Config{WorktreeDir: "/tmp/wt", VerifyCmd: []string{"bin/verify", "--wasm"}}

	name, args := runArgsFor(t, cfg, func(w *worktree) error { return w.Verify(context.Background()) })

	if name != "bin/verify" {
		t.Errorf("Verify ran %q, want the configured VerifyCmd", name)
	}
	if len(args) == 0 || args[0] != "--wasm" {
		t.Errorf("Verify ran with args %v, want the configured VerifyCmd", args)
	}
}
