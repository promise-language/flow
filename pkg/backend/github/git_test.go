package github

import (
	"os"
	"path/filepath"
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

	// Create and commit an initial file so HEAD exists.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("init"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := g.Commit(ctx, "initial"); err != nil {
		t.Fatalf("initial commit: %v", err)
	}
	return g
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
