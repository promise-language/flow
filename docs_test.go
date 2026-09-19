package flow

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The two things about the decided documentation that nothing else checks.
//
// `Backend` became `Orchestrator` in the contract and the code, and the prose
// followed one occurrence at a time: the interface sense renamed, and the
// storage sense named its store instead, because a store is something an
// orchestrator uses and not something it is (docs/orchestrator.md § What an
// orchestrator is). Both halves of that regress silently. A find-and-replace
// in the other direction puts the retired name back, and a package rename
// leaves a citation behind that goes on reading correctly while pointing at
// nothing — CONTRIBUTING.md named `pkg/backend/fake` from the rename until
// somebody happened to read the line.
//
// The scanned set is deliberate. `docs/archive/` is superseded,
// `docs/proposals/` is not binding and `docs/org/` is vendored from another
// repository and never edited here (docs/index.md) — each keeps the old name
// legitimately, so none of them is scanned.

// decidedDocs is the set whose every occurrence was decided: README.md,
// CONTRIBUTING.md, and the normative documents, which are the Markdown files
// at the ROOT of docs/.
func decidedDocs(t *testing.T) []string {
	t.Helper()
	docs := []string{"README.md", "CONTRIBUTING.md"}
	entries, err := os.ReadDir("docs")
	if err != nil {
		t.Fatalf("cannot read docs/: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			docs = append(docs, filepath.Join("docs", e.Name()))
		}
	}
	// A scan over nothing passes, which is worse than no scan at all.
	if len(docs) < 3 {
		t.Fatal("docs/ holds no top-level document: the scans below would pass by reading nothing")
	}
	return docs
}

func readDoc(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	return strings.Split(string(body), "\n")
}

// TestDecidedDocs_RetiredNameDoesNotReturn refuses "backend" anywhere in the
// decided set. The name described a store, while the interface it was attached
// to leases items to arenas, runs gates and commands in a worktree and lands
// what those produce — only the first of those is a backend. Every occurrence
// here was replaced by whatever the sentence was actually about: the
// orchestrator, or the store named directly ("a large tracker", "the
// repository's default", "GitHub's own interface"). A reintroduction is a
// sentence nobody has decided, which is the defect rather than the word.
//
// The match is on the substring, so "backends" is caught and "server-backed" —
// a different word, and live at README.md — is not.
func TestDecidedDocs_RetiredNameDoesNotReturn(t *testing.T) {
	for _, doc := range decidedDocs(t) {
		for i, line := range readDoc(t, doc) {
			if strings.Contains(strings.ToLower(line), "backend") {
				t.Errorf("%s:%d says \"backend\" — decide what the sentence is about, "+
					"the orchestrator or the store named directly:\n\t%s",
					doc, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// citedPath matches a path into this repository, anchored on a top-level
// directory. The anchor is what keeps a bare filename in prose ("app.go") and
// the tail of an import line ("/main.go") from being read as a path — neither
// resolves on its own, and guessing where they live would report failures the
// documentation never made.
var citedPath = regexp.MustCompile(`\b(?:pkg|cli|claude|tools|examples)(?:/[A-Za-z0-9_.-]+)+`)

// TestDecidedDocs_CitedPathsExist checks that every path the decided set names
// is in the tree. This is the failure the rename actually produced and nothing
// caught: the compiler has no opinion about a path in prose, so
// "the in-memory `pkg/backend/fake`" stayed put, and a reader following it
// found nothing where the documentation promised a worked example.
func TestDecidedDocs_CitedPathsExist(t *testing.T) {
	sites := map[string][]string{}
	for _, doc := range decidedDocs(t) {
		for i, line := range readDoc(t, doc) {
			for _, m := range citedPath.FindAllString(line, -1) {
				p := strings.TrimRight(m, ".,;:)")
				sites[p] = append(sites[p], fmt.Sprintf("%s:%d", doc, i+1))
			}
		}
	}
	if len(sites) == 0 {
		t.Fatal("the decided set cites no repository path: the scan would pass by finding nothing")
	}
	for p, where := range sites {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s names %s, which is not in the tree", strings.Join(where, ", "), p)
		}
	}
}
