package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/promise-language/flow"
)

// Worktree returns the worktree impl for a claim. The Worktree is stateless
// beyond the underlying local git + the backend's GH client — fine to call
// repeatedly within an invocation; cli/StepCtx caches the return.
func (b *Backend) Worktree(ctx context.Context, claim flow.Claim) (flow.Worktree, error) {
	issueNum, err := b.issueNumber(claim.ItemRef)
	if err != nil {
		return nil, err
	}
	return &worktree{
		b:        b,
		claim:    claim,
		issueNum: issueNum,
	}, nil
}

type worktree struct {
	b        *Backend
	claim    flow.Claim
	issueNum int
}

func (w *worktree) Branch(ctx context.Context, name, base string) (bool, error) {
	cur, err := w.b.git.CurrentBranch(ctx)
	if err != nil {
		return false, err
	}
	if cur == name {
		return false, nil
	}
	dirty, err := w.b.git.IsDirty(ctx)
	if err != nil {
		return false, err
	}
	if dirty {
		return false, errors.New("worktree is dirty; cannot switch branches")
	}
	exists, err := w.b.git.BranchExists(ctx, name)
	if err != nil {
		return false, err
	}
	if exists {
		return false, w.b.git.Checkout(ctx, name, "", false)
	}
	if err := w.b.git.Checkout(ctx, name, base, true); err != nil {
		return false, err
	}
	return true, nil
}

func (w *worktree) CurrentBranch(ctx context.Context) (string, error) {
	return w.b.git.CurrentBranch(ctx)
}

func (w *worktree) Commit(ctx context.Context, msg string) error {
	return w.b.git.Commit(ctx, msg)
}

func (w *worktree) Push(ctx context.Context) error {
	return w.b.git.Push(ctx)
}

// Request exposes the pull-request management surface. The github backend
// supports pull requests, so this returns the worktree itself (its Open/
// Merge methods satisfy flow.RequestManager).
func (w *worktree) Request() flow.RequestManager { return w }

func (w *worktree) Open(ctx context.Context, base, title, body string) (string, error) {
	// Ensure the claim branch is current + pushed.
	expected := w.b.claimBranch(w.issueNum)
	branch, err := w.b.git.CurrentBranch(ctx)
	if err != nil {
		return "", err
	}
	if branch != expected {
		return "", fmt.Errorf("worktree.Open: current branch %q != claim branch %q", branch, expected)
	}
	if err := w.b.git.Push(ctx); err != nil {
		return "", err
	}

	// Use `gh pr create` to handle cross-repo (fork) cases cleanly. The Go
	// client requires owner-qualified head; gh handles that natively.
	// --repo, not -C: `-C` is git's flag for selecting a working directory and
	// gh has no such flag — passing it fails argument validation before gh does
	// anything ("unknown shorthand flag: 'C'"). --repo also removes the
	// dependency on the process working directory entirely, which the runner
	// does not set.
	args := []string{"--repo", w.b.cfg.repoFullName(), "pr", "create", "--base", base, "--title", title, "--body", body, "--head", expected}
	stdout, stderr, err := w.b.git.runner(ctx, "", "gh", args...)
	if err != nil {
		return "", fmt.Errorf("gh pr create: %w (stderr=%s)", err, strings.TrimSpace(string(stderr)))
	}
	// gh pr create prints the PR URL on stdout.
	url := strings.TrimSpace(string(stdout))

	// Side-effect: backend marks pr-open in the state comment.
	if err := w.b.markSignalSetOnState(ctx, w.claim, "pr-open"); err != nil {
		// Non-fatal — the LoadState poll will pick up the signal on the
		// next run.
		_ = err
	}
	return url, nil
}

func (w *worktree) Merge(ctx context.Context, url string) error {
	// --repo, not -C: see Open. gh has no -C flag.
	args := []string{"--repo", w.b.cfg.repoFullName(), "pr", "merge", url, "--squash", "--auto"}
	_, stderr, err := w.b.git.runner(ctx, "", "gh", args...)
	if err != nil {
		return fmt.Errorf("gh pr merge: %w (stderr=%s)", err, strings.TrimSpace(string(stderr)))
	}
	if err := w.b.markSignalSetOnState(ctx, w.claim, "pr-merged"); err != nil {
		_ = err
	}
	return nil
}

// Verify runs cfg.VerifyCmd in cfg.WorktreeDir. Exit-0 → success.
func (w *worktree) Verify(ctx context.Context) error {
	if len(w.b.cfg.VerifyCmd) == 0 {
		return errors.New("worktree.Verify: cfg.VerifyCmd is empty")
	}
	return w.run(ctx, "verify", w.b.cfg.VerifyCmd)
}

// RunGate runs the named gate in cfg.WorktreeDir. Exit-0 → pass.
//
// Gates are reached through the project's gate entry point by name rather than
// each being configured separately. That is what makes the parts addressable:
// a step fixing one failing suite asks for that suite, without the project
// having had to enumerate every part in advance.
func (w *worktree) RunGate(ctx context.Context, name flow.GateName) error {
	if !name.Valid() {
		return fmt.Errorf("worktree.RunGate: %q is not a declared gate name", name)
	}
	return w.run(ctx, "gate "+string(name), append(append([]string{}, gateEntryPoint...), string(name)))
}

// gateEntryPoint is how a project exposes its gates. Not configurable: the
// names are fixed, so the way to reach them is too.
var gateEntryPoint = []string{"bin/gate"}

// run executes one configured command in the worktree.
//
// Shared by Verify and Gate because the mechanics are identical — what differs
// is what a caller may conclude, which is a property of the two names and not
// of how they are spawned.
func (w *worktree) run(ctx context.Context, what string, args []string) error {
	stdout, stderr, err := w.b.git.runner(ctx, w.b.cfg.WorktreeDir, args[0], args[1:]...)
	if err != nil {
		return fmt.Errorf("%s failed: %w\noutput:\n%s%s", what, err, string(stdout), string(stderr))
	}
	return nil
}

func (w *worktree) CapturePatch(ctx context.Context) ([]byte, error) {
	return w.b.git.CapturePatch(ctx)
}

func (w *worktree) RevParse(ctx context.Context, rev string) (string, error) {
	return w.b.git.RevParse(ctx, rev)
}

func (w *worktree) Stage(ctx context.Context) error { return w.b.git.StageAll(ctx) }
