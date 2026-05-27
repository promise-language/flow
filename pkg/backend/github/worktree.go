package github

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
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

func (w *worktree) OpenPR(ctx context.Context, base, title, body string) (string, error) {
	// Ensure the claim branch is current + pushed.
	expected := w.b.claimBranch(w.issueNum)
	branch, err := w.b.git.CurrentBranch(ctx)
	if err != nil {
		return "", err
	}
	if branch != expected {
		return "", fmt.Errorf("worktree.OpenPR: current branch %q != claim branch %q", branch, expected)
	}
	if err := w.b.git.Push(ctx); err != nil {
		return "", err
	}

	// Use `gh pr create` to handle cross-repo (fork) cases cleanly. The Go
	// client requires owner-qualified head; gh handles that natively.
	args := []string{"-C", w.b.cfg.WorktreeDir, "pr", "create", "--base", base, "--title", title, "--body", body, "--head", expected}
	stdout, stderr, err := w.b.git.runner(ctx, "gh", args...)
	if err != nil {
		return "", fmt.Errorf("gh pr create: %w (stderr=%s)", err, strings.TrimSpace(string(stderr)))
	}
	// gh pr create prints the PR URL on stdout.
	url := strings.TrimSpace(string(stdout))

	// Side-effect: backend marks pr-open in the state comment.
	if err := w.b.markSignalSetOnState(ctx, w.claim, w.issueNum, "pr-open"); err != nil {
		// Non-fatal — the LoadState poll will pick up the signal on the
		// next run.
		_ = err
	}
	return url, nil
}

func (w *worktree) MergePR(ctx context.Context, url string) error {
	args := []string{"-C", w.b.cfg.WorktreeDir, "pr", "merge", url, "--squash", "--auto"}
	_, stderr, err := w.b.git.runner(ctx, "gh", args...)
	if err != nil {
		return fmt.Errorf("gh pr merge: %w (stderr=%s)", err, strings.TrimSpace(string(stderr)))
	}
	if err := w.b.markSignalSetOnState(ctx, w.claim, w.issueNum, "pr-merged"); err != nil {
		_ = err
	}
	return nil
}

// Validate runs cfg.VerifyCmd in cfg.WorktreeDir. Exit-0 → success.
func (w *worktree) Validate(ctx context.Context) error {
	if len(w.b.cfg.VerifyCmd) == 0 {
		return errors.New("worktree.Validate: cfg.VerifyCmd is empty")
	}
	args := w.b.cfg.VerifyCmd
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = w.b.cfg.WorktreeDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("verify failed: %w\noutput:\n%s", err, string(out))
	}
	return nil
}

func (w *worktree) CapturePatch(ctx context.Context) ([]byte, error) {
	return w.b.git.CapturePatch(ctx)
}
