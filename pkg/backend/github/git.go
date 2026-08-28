package github

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
)

// gitOps wraps local `git` invocations. Returned by newGitOps; tests can
// substitute the runner via exec.LookPath / PATH manipulation, or by
// constructing one with a custom runner.
type gitOps struct {
	dir string
	// runner spawns one command. dir is the working directory, or "" to
	// inherit the process's — git and gh are told where to work by flag
	// (-C, --repo), while the project's own commands are run IN the worktree
	// and have nowhere else to learn it from.
	runner func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error)
}

func newGitOps(dir string) *gitOps {
	return &gitOps{dir: dir, runner: defaultGitRunner}
}

func defaultGitRunner(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	stdout, err := cmd.Output()
	var stderr []byte
	if ee, ok := err.(*exec.ExitError); ok {
		stderr = ee.Stderr
	}
	return stdout, stderr, err
}

// run invokes `git <args>` in g.dir and returns stdout, captured stderr, err.
func (g *gitOps) run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	full := append([]string{"-C", g.dir}, args...)
	return g.runner(ctx, "", "git", full...)
}

// OriginOwnerRepo runs `git remote get-url origin` and parses out the
// (owner, repo) coordinates. Supports https://github.com/o/r.git and
// git@github.com:o/r.git forms.
func (g *gitOps) OriginOwnerRepo(ctx context.Context) (owner, repo string, err error) {
	stdout, stderr, err := g.run(ctx, "remote", "get-url", "origin")
	if err != nil {
		return "", "", fmt.Errorf("git remote get-url origin: %w (stderr=%s)", err, string(stderr))
	}
	raw := strings.TrimSpace(string(stdout))
	return parseGitHubRemote(raw)
}

var (
	// match https://github.com/owner/repo or https://github.com/owner/repo.git
	httpsRemote = regexp.MustCompile(`^https?://[^/]+/([^/]+)/([^/.]+)(?:\.git)?/?$`)
	// match git@github.com:owner/repo.git
	sshRemote = regexp.MustCompile(`^[^@]+@[^:]+:([^/]+)/([^/.]+)(?:\.git)?$`)
)

func parseGitHubRemote(raw string) (owner, repo string, err error) {
	if m := httpsRemote.FindStringSubmatch(raw); m != nil {
		return m[1], m[2], nil
	}
	if m := sshRemote.FindStringSubmatch(raw); m != nil {
		return m[1], m[2], nil
	}
	// Fall back to URL parsing (ssh://git@github.com/o/r.git).
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(parts) >= 2 {
			return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
		}
	}
	return "", "", fmt.Errorf("cannot parse GitHub remote %q", raw)
}

// CurrentBranch returns the branch HEAD is on.
func (g *gitOps) CurrentBranch(ctx context.Context) (string, error) {
	stdout, stderr, err := g.run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w (%s)", err, string(stderr))
	}
	return strings.TrimSpace(string(stdout)), nil
}

// IsDirty returns true if the worktree has uncommitted changes (staged or
// unstaged). Untracked files DO NOT count as dirty.
func (g *gitOps) IsDirty(ctx context.Context) (bool, error) {
	stdout, _, err := g.run(ctx, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return false, err
	}
	return len(strings.TrimSpace(string(stdout))) > 0, nil
}

// Checkout switches to branch `name`, optionally creating it off base.
func (g *gitOps) Checkout(ctx context.Context, name, base string, create bool) error {
	args := []string{"checkout"}
	if create {
		args = append(args, "-b")
	}
	args = append(args, name)
	if create && base != "" {
		args = append(args, base)
	}
	_, stderr, err := g.run(ctx, args...)
	if err != nil {
		return fmt.Errorf("git checkout %v: %w (%s)", args, err, string(stderr))
	}
	return nil
}

// BranchExists returns true if a local branch with this name exists.
func (g *gitOps) BranchExists(ctx context.Context, name string) (bool, error) {
	_, _, err := g.run(ctx, "rev-parse", "--verify", "refs/heads/"+name)
	if err == nil {
		return true, nil
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 128 {
		return false, nil
	}
	return false, err
}

// Commit stages everything and commits with the given message. Returns nil
// if there's nothing to commit (idempotent).
// StageAll adds every change, untracked files included, to the index.
func (g *gitOps) StageAll(ctx context.Context) error {
	if _, stderr, err := g.run(ctx, "add", "-A"); err != nil {
		return fmt.Errorf("git add -A: %w (%s)", err, string(stderr))
	}
	return nil
}

func (g *gitOps) Commit(ctx context.Context, msg string) error {
	if err := g.StageAll(ctx); err != nil {
		return err
	}
	// Detect "nothing to commit" without erroring out.
	dirty, err := g.HasStaged(ctx)
	if err != nil {
		return err
	}
	if !dirty {
		return nil
	}
	_, stderr, err := g.run(ctx, "commit", "-m", msg)
	if err != nil {
		return fmt.Errorf("git commit: %w (%s)", err, string(stderr))
	}
	return nil
}

// HasStaged returns true if the index has changes not yet committed.
func (g *gitOps) HasStaged(ctx context.Context) (bool, error) {
	_, _, err := g.run(ctx, "diff", "--cached", "--quiet")
	if err == nil {
		return false, nil
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return true, nil
	}
	return false, err
}

// PushMaterial reports what pushing `branch` to origin would publish: the
// message of every commit the remote does not already have, and those commits
// as a patch. There is no Push here — it lives on outward, because a push is a
// publication and must not be reachable without passing the seam.
//
// `--not --remotes=origin` rather than a diff against a base branch: it needs
// no base, and is correct both for a first push (nothing on origin excludes
// nothing, so every commit on the branch is reported) and for a later one
// (only what is new). When the remote-tracking refs are stale it over-reports,
// which is the safe direction — a guard shown more than will be sent can
// refuse something publishable, and one shown less reports a safety it did not
// establish.
func (g *gitOps) PushMaterial(ctx context.Context, branch string) (messages []string, patch string, err error) {
	// %x00 rather than a newline: a commit message contains newlines, so any
	// text separator would split one message into several.
	//
	// The trailing `--` says the branch is a revision and there are no
	// pathspecs. Without it git refuses any branch whose name is also a
	// tracked path ("ambiguous argument"), and the claim branch is
	// flow/issue-<n> — a name a repository can perfectly well also have a
	// file at. `git push` never had to disambiguate, so this is a failure
	// only the material query can hit, and it would fail a push that has
	// nothing wrong with it.
	logArgs := func(extra ...string) []string {
		args := append([]string{"log", "--format=%B%x00"}, extra...)
		return append(args, branch, "--not", "--remotes=origin", "--")
	}
	stdout, stderr, err := g.run(ctx, logArgs()...)
	if err != nil {
		return nil, "", fmt.Errorf("git log %s --not --remotes=origin: %w (%s)", branch, err, string(stderr))
	}
	for _, m := range strings.Split(string(stdout), "\x00") {
		if trimmed := strings.TrimSpace(m); trimmed != "" {
			messages = append(messages, trimmed)
		}
	}
	full, stderr, err := g.run(ctx, logArgs("--patch")...)
	if err != nil {
		return nil, "", fmt.Errorf("git log --patch %s --not --remotes=origin: %w (%s)", branch, err, string(stderr))
	}
	return messages, string(full), nil
}

// CapturePatch runs `git diff HEAD` and returns the unified diff. Untracked
// files are not included (matches design.md PatchBody contract).
func (g *gitOps) CapturePatch(ctx context.Context) ([]byte, error) {
	stdout, stderr, err := g.run(ctx, "diff", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("git diff HEAD: %w (%s)", err, string(stderr))
	}
	return stdout, nil
}

// RevParse resolves any revision ("HEAD", a branch, a SHA) to a commit SHA.
func (g *gitOps) RevParse(ctx context.Context, rev string) (string, error) {
	stdout, stderr, err := g.run(ctx, "rev-parse", rev)
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w (%s)", rev, err, string(stderr))
	}
	return strings.TrimSpace(string(stdout)), nil
}

func (g *gitOps) HeadSHA(ctx context.Context) (string, error) {
	stdout, _, err := g.run(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(stdout)), nil
}
