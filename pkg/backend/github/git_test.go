package github

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
