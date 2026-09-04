package github

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/promise-language/flow/pkg/clistate"
)

func TestParseGitHubRemote(t *testing.T) {
	cases := []struct {
		raw   string
		owner string
		repo  string
		bad   bool
	}{
		{"https://github.com/owner/repo.git", "owner", "repo", false},
		{"https://github.com/owner/repo", "owner", "repo", false},
		{"git@github.com:owner/repo.git", "owner", "repo", false},
		{"ssh://git@github.com/owner/repo.git", "owner", "repo", false},
		{"https://gitlab.com/owner/repo.git", "owner", "repo", false}, // permissive
		{"not-a-url", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			owner, repo, err := parseGitHubRemote(c.raw)
			if c.bad {
				if err == nil {
					t.Errorf("expected error for %q", c.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err for %q: %v", c.raw, err)
			}
			if owner != c.owner || repo != c.repo {
				t.Errorf("got (%q,%q), want (%q,%q)", owner, repo, c.owner, c.repo)
			}
		})
	}
}

// initTestRepo creates a git repo in a temp dir with an initial commit,
// returning a gitOps pointed at it.
func initTestRepo(t *testing.T) *gitOps {
	t.Helper()
	dir := t.TempDir()
	g := newGitOps(dir)
	ctx := t.Context()

	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test"},
		{"config", "user.name", "test"},
	} {
		if _, stderr, err := g.run(ctx, args...); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(stderr))
		}
	}

	// Ignore the SDK state dir, which is what every real project does and what
	// StageAll now requires: an unignored state dir inside the worktree is
	// refused, not silently excluded. An absolute FLOW_DIR is outside the
	// worktree, so there is nothing to ignore.
	if d := clistate.Dir(); !filepath.IsAbs(d) {
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(d+"/\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Create and commit an initial file so HEAD exists.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("init"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := g.Commit(ctx, "initial"); err != nil {
		t.Fatalf("initial commit: %v", err)
	}
	return g
}

// A command that writes far more than any git diff or log should not exhaust
// the runner. The captured stdout is truncated to maxGitOutput.
func TestDefaultGitRunner_BoundsStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell script")
	}
	dir := t.TempDir()
	blocks := (maxGitOutput / 1024) + 10
	script := filepath.Join(dir, "chatty.sh")
	if err := os.WriteFile(script, []byte(fmt.Sprintf(
		"#!/bin/sh\ndd if=/dev/zero bs=1024 count=%d 2>/dev/null\n", blocks,
	)), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := defaultGitRunner(context.Background(), dir, script)
	if err != nil {
		t.Fatalf("defaultGitRunner: %v", err)
	}
	if len(stdout) > maxGitOutput {
		t.Errorf("stdout is %d bytes, want at most %d — the capture is unbounded", len(stdout), maxGitOutput)
	}
}

// When a command fails, its stderr is returned for diagnostics. When it
// succeeds, stderr is discarded — the caller asked for the answer, not the
// commentary around it.
func TestDefaultGitRunner_StderrOnlyOnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell script")
	}
	dir := t.TempDir()

	// A command that succeeds and writes to both streams.
	ok := filepath.Join(dir, "ok.sh")
	if err := os.WriteFile(ok, []byte("#!/bin/sh\necho answer\necho info >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := defaultGitRunner(context.Background(), dir, ok)
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if !strings.Contains(string(stdout), "answer") {
		t.Errorf("stdout = %q, want it to contain the answer", stdout)
	}
	if len(stderr) != 0 {
		t.Errorf("stderr = %q on success, want nil — diagnostics belong only with errors", stderr)
	}

	// A command that fails: stderr is returned.
	bad := filepath.Join(dir, "bad.sh")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho oops >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = defaultGitRunner(context.Background(), dir, bad)
	if err == nil {
		t.Fatal("expected an error from exit 1")
	}
	if !strings.Contains(string(stderr), "oops") {
		t.Errorf("stderr = %q on failure, want the diagnostic", stderr)
	}
}

func TestStageAll_ExcludesFlowDir(t *testing.T) {
	t.Setenv("FLOW_DIR", ".flow")
	g := initTestRepo(t)
	ctx := t.Context()

	// Modify the tracked file.
	if err := os.WriteFile(filepath.Join(g.dir, "README"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create the SDK state file that must be excluded.
	flowDir := filepath.Join(g.dir, ".flow")
	if err := os.MkdirAll(flowDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flowDir, "active.json"), []byte(`{"token":"t"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := g.StageAll(ctx); err != nil {
		t.Fatalf("StageAll: %v", err)
	}

	// Check what got staged.
	stdout, _, err := g.run(ctx, "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	staged := strings.TrimSpace(string(stdout))

	if !strings.Contains(staged, "README") {
		t.Errorf("README should be staged, got: %q", staged)
	}
	if strings.Contains(staged, ".flow") {
		t.Errorf(".flow/active.json should NOT be staged, got: %q", staged)
	}
}

func TestStageAll_ExcludesCustomFlowDir(t *testing.T) {
	t.Setenv("FLOW_DIR", ".custom-state")
	g := initTestRepo(t)
	ctx := t.Context()

	// Modify the tracked file.
	if err := os.WriteFile(filepath.Join(g.dir, "README"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create the custom state dir.
	stateDir := filepath.Join(g.dir, ".custom-state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "active.json"), []byte(`{"token":"t"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := g.StageAll(ctx); err != nil {
		t.Fatalf("StageAll: %v", err)
	}

	stdout, _, err := g.run(ctx, "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	staged := strings.TrimSpace(string(stdout))

	if !strings.Contains(staged, "README") {
		t.Errorf("README should be staged, got: %q", staged)
	}
	if strings.Contains(staged, ".custom-state") {
		t.Errorf(".custom-state/active.json should NOT be staged, got: %q", staged)
	}
}

func TestStageAll_ExcludesNestedFlowSubtree(t *testing.T) {
	t.Setenv("FLOW_DIR", ".flow")
	g := initTestRepo(t)
	ctx := t.Context()

	if err := os.WriteFile(filepath.Join(g.dir, "README"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create nested work-in-progress records like the real SDK writes.
	workDir := filepath.Join(g.dir, ".flow", "work", "issue-9")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "plan.json"), []byte(`{"step":"plan"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := g.StageAll(ctx); err != nil {
		t.Fatalf("StageAll: %v", err)
	}

	stdout, _, err := g.run(ctx, "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	staged := strings.TrimSpace(string(stdout))

	if !strings.Contains(staged, "README") {
		t.Errorf("README should be staged, got: %q", staged)
	}
	if strings.Contains(staged, ".flow") {
		t.Errorf("nested .flow/work/ should NOT be staged, got: %q", staged)
	}
}

func TestStageAll_DoesNotOverExclude(t *testing.T) {
	t.Setenv("FLOW_DIR", ".flow")
	g := initTestRepo(t)
	ctx := t.Context()

	// A file whose name starts with ".flow" but is not inside .flow/ must
	// still be staged — the pathspec must not glob beyond the directory.
	if err := os.WriteFile(filepath.Join(g.dir, ".flowconfig"), []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := g.StageAll(ctx); err != nil {
		t.Fatalf("StageAll: %v", err)
	}

	stdout, _, err := g.run(ctx, "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	staged := strings.TrimSpace(string(stdout))

	if !strings.Contains(staged, ".flowconfig") {
		t.Errorf(".flowconfig should be staged (not inside .flow/), got: %q", staged)
	}
}

func TestStageAll_AbsoluteFlowDir(t *testing.T) {
	// FLOW_DIR is an absolute path outside the worktree. The exclude pathspec
	// must be omitted — git refuses an exclude that points outside the repo.
	t.Setenv("FLOW_DIR", t.TempDir())
	g := initTestRepo(t)
	ctx := t.Context()

	// Modify the tracked file.
	if err := os.WriteFile(filepath.Join(g.dir, "README"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}

	// Write a state file in the external FLOW_DIR — it must not appear in the index.
	if err := os.WriteFile(filepath.Join(clistate.Dir(), "active.json"), []byte(`{"token":"t"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := g.StageAll(ctx); err != nil {
		t.Fatalf("StageAll with absolute FLOW_DIR: %v", err)
	}

	stdout, _, err := g.run(ctx, "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	staged := strings.TrimSpace(string(stdout))

	if !strings.Contains(staged, "README") {
		t.Errorf("README should be staged, got: %q", staged)
	}
	if strings.Contains(staged, "active.json") {
		t.Errorf("external state file should NOT be staged, got: %q", staged)
	}
}

func TestCommit_AbsoluteFlowDir(t *testing.T) {
	// The full Commit path — StageAll → HasStaged → git commit — must
	// succeed when FLOW_DIR is an absolute path outside the worktree.
	// Before the filepath.IsAbs guard this was fatal: git refused the
	// exclude pathspec, so every step's commit failed.
	t.Setenv("FLOW_DIR", t.TempDir())
	g := initTestRepo(t)
	ctx := t.Context()

	if err := os.WriteFile(filepath.Join(g.dir, "README"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}

	// Write state in the external FLOW_DIR — must not leak into the commit.
	if err := os.WriteFile(filepath.Join(clistate.Dir(), "active.json"), []byte(`{"token":"t"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := g.Commit(ctx, "commit with absolute FLOW_DIR"); err != nil {
		t.Fatalf("Commit with absolute FLOW_DIR: %v", err)
	}

	stdout, _, err := g.run(ctx, "ls-tree", "-r", "--name-only", "HEAD")
	if err != nil {
		t.Fatalf("git ls-tree: %v", err)
	}
	tree := string(stdout)

	if !strings.Contains(tree, "README") {
		t.Errorf("README should be in commit, got: %q", tree)
	}
	if strings.Contains(tree, "active.json") {
		t.Errorf("external state file should NOT be in commit, got: %q", tree)
	}
}

func TestCommit_ExcludesFlowDir(t *testing.T) {
	t.Setenv("FLOW_DIR", ".flow")
	g := initTestRepo(t)
	ctx := t.Context()

	if err := os.WriteFile(filepath.Join(g.dir, "README"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	flowDir := filepath.Join(g.dir, ".flow")
	if err := os.MkdirAll(flowDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flowDir, "active.json"), []byte(`{"token":"t"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := g.Commit(ctx, "test commit"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify committed tree does not contain .flow/.
	stdout, _, err := g.run(ctx, "ls-tree", "-r", "--name-only", "HEAD")
	if err != nil {
		t.Fatalf("git ls-tree: %v", err)
	}
	tree := string(stdout)

	if !strings.Contains(tree, "README") {
		t.Errorf("README should be in commit, got: %q", tree)
	}
	if strings.Contains(tree, ".flow") {
		t.Errorf(".flow/ should NOT be in commit, got: %q", tree)
	}
}

func TestCommit_OnlyFlowDirChange_NoCommit(t *testing.T) {
	t.Setenv("FLOW_DIR", ".flow")
	g := initTestRepo(t)
	ctx := t.Context()

	headBefore, err := g.HeadSHA(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The only change is inside .flow/ — after exclusion, nothing to commit.
	flowDir := filepath.Join(g.dir, ".flow")
	if err := os.MkdirAll(flowDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flowDir, "active.json"), []byte(`{"token":"t"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := g.Commit(ctx, "should be a no-op"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	headAfter, err := g.HeadSHA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if headBefore != headAfter {
		t.Errorf("HEAD should not have moved: before=%s after=%s", headBefore, headAfter)
	}
}

// An unignored state dir inside the worktree is refused, not worked around.
// Excluding it with a pathspec would leave it present-but-never-committed, and
// naming an already-ignored path makes git reject the whole add.
func TestStageAll_RefusesUnignoredStateDir(t *testing.T) {
	t.Setenv("FLOW_DIR", ".flow")
	dir := t.TempDir()
	g := newGitOps(dir)
	ctx := t.Context()

	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test"},
		{"config", "user.name", "test"},
	} {
		if _, stderr, err := g.run(ctx, args...); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(stderr))
		}
	}
	// Deliberately NO .gitignore.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	err := g.StageAll(ctx)
	if err == nil {
		t.Fatal("StageAll succeeded with an unignored state dir, want a refusal")
	}
	if !strings.Contains(err.Error(), ".gitignore") {
		t.Errorf("err = %v, want it to name the remedy (.gitignore)", err)
	}
	if !strings.Contains(err.Error(), ".flow") {
		t.Errorf("err = %v, want it to name the state dir", err)
	}
}
