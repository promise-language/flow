package github

import (
	"context"
	"slices"
	"strings"
	"testing"

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

// Verify and RunGate spawn commands, and the whole of what distinguishes them
// is which command. Asserting on a mocked result would pass against a version
// that ran the wrong one — the same shape of defect as the `gh -C` flag, which
// shipped because nothing exercised the command line.
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

func TestRunGate_AsksTheEntryPointForTheNamedGate(t *testing.T) {
	for _, gate := range []flow.GateName{"integration", "tested", "tested:wasm"} {
		t.Run(string(gate), func(t *testing.T) {
			w := &worktree{b: &Backend{cfg: Config{WorktreeDir: "/tmp/wt"}}}
			var got []string
			w.b.git = &gitOps{runner: func(_ context.Context, _, _ string, a ...string) ([]byte, []byte, error) {
				got = a
				return nil, nil, nil
			}}
			if err := w.RunGate(context.Background(), gate); err != nil {
				t.Fatalf("RunGate(%q): %v", gate, err)
			}
			// The instance travels whole. Splitting it here would ask for the
			// concept and silently run every suite under it.
			if len(got) == 0 || got[len(got)-1] != string(gate) {
				t.Errorf("gate name reached the entry point as %v, want it to end in %q", got, gate)
			}
		})
	}
}

// An undeclared concept is refused before anything is spawned. Running it would
// hand the project a name it cannot answer, and the failure would look like the
// gate refusing rather than the caller asking for something that does not exist.
func TestRunGate_RefusesAnUndeclaredNameWithoutSpawning(t *testing.T) {
	w := &worktree{b: &Backend{cfg: Config{WorktreeDir: "/tmp/wt"}}}
	spawned := false
	w.b.git = &gitOps{runner: func(context.Context, string, string, ...string) ([]byte, []byte, error) {
		spawned = true
		return nil, nil, nil
	}}
	err := w.RunGate(context.Background(), "lint")
	if err == nil || !strings.Contains(err.Error(), "not a declared gate name") {
		t.Fatalf("err = %v, want a refusal naming the problem", err)
	}
	if spawned {
		t.Error("spawned a command for an undeclared gate name")
	}
}

// Verify runs the CONFIGURED command; RunGate runs the entry point. Confusing
// them would make a decision rest on the command that modifies the tree, which
// is the distinction the two methods exist to keep.
func TestVerifyAndRunGateRunDifferentCommands(t *testing.T) {
	cfg := Config{WorktreeDir: "/tmp/wt", VerifyCmd: []string{"bin/verify", "--wasm"}}

	_, verifyArgs := runArgsFor(t, cfg, func(w *worktree) error { return w.Verify(context.Background()) })
	gateName, _ := runArgsFor(t, cfg, func(w *worktree) error {
		return w.RunGate(context.Background(), flow.GateIntegration)
	})

	if len(verifyArgs) == 0 || verifyArgs[0] != "--wasm" {
		t.Errorf("Verify ran with args %v, want the configured VerifyCmd", verifyArgs)
	}
	if gateName == "bin/verify" {
		t.Error("RunGate ran the verify command — a decision would rest on something that modifies the tree")
	}
}
