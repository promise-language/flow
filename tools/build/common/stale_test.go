package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// StaleReason's four answers. They are not interchangeable: a caller deciding
// whether to rebuild needs "the source moved" (rebuilding fixes it) apart from
// "not built via ./make" and "the repo is gone" (rebuilding cannot).
func TestStaleReason(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "tools", "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tools", "build", "x.go"), []byte("package build\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	current, err := ToolsSourceHash(repo)
	if err != nil {
		t.Fatalf("ToolsSourceHash: %v", err)
	}

	if got := StaleReason(repo, current); got != "" {
		t.Errorf("matching hash reported stale: %q", got)
	}
	if got := StaleReason(repo, "deadbeef"); !strings.Contains(got, "tools source has changed") {
		t.Errorf("changed source: %q, want it to name the change", got)
	}
	if got := StaleReason("", ""); !strings.Contains(got, "not built via") {
		t.Errorf("unstamped binary: %q, want it to say so", got)
	}
	if got := StaleReason(filepath.Join(repo, "gone"), "abc"); !strings.Contains(got, "unreachable") {
		t.Errorf("missing repo: %q, want it to report unreachability", got)
	}
}
