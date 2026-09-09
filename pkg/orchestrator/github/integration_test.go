package github

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
)

// requireIntegration skips unless GH_INTEGRATION=1 AND the four required env
// vars are set:
//
//	GH_INTEGRATION_OWNER=<gh-login-or-org>
//	GH_INTEGRATION_REPO=<sandbox-repo>
//	GH_INTEGRATION_ISSUE=<issue-number-to-exercise>
//	GH_INTEGRATION_OWNER_LOGIN=<your-gh-login> (used as claim owner)
//
// The test will mutate labels/assignees/comments on the named issue, so
// point it at a throwaway repo + issue created for this purpose.
func requireIntegration(t *testing.T) (owner, repo string, issueNum int, ownerLogin string) {
	t.Helper()
	if os.Getenv("GH_INTEGRATION") != "1" {
		t.Skip("set GH_INTEGRATION=1 to run github integration tests")
	}
	owner = os.Getenv("GH_INTEGRATION_OWNER")
	repo = os.Getenv("GH_INTEGRATION_REPO")
	issueStr := os.Getenv("GH_INTEGRATION_ISSUE")
	ownerLogin = os.Getenv("GH_INTEGRATION_OWNER_LOGIN")
	missing := []string{}
	if owner == "" {
		missing = append(missing, "GH_INTEGRATION_OWNER")
	}
	if repo == "" {
		missing = append(missing, "GH_INTEGRATION_REPO")
	}
	if issueStr == "" {
		missing = append(missing, "GH_INTEGRATION_ISSUE")
	}
	if ownerLogin == "" {
		missing = append(missing, "GH_INTEGRATION_OWNER_LOGIN")
	}
	if len(missing) > 0 {
		t.Skipf("integration env missing: %s", strings.Join(missing, ", "))
	}
	num, err := strconv.Atoi(issueStr)
	if err != nil {
		t.Fatalf("GH_INTEGRATION_ISSUE=%q not numeric: %v", issueStr, err)
	}
	return owner, repo, num, ownerLogin
}

// TestIntegration_ClaimAppendReleaseCycle exercises the full Claim →
// AppendEntry → Release cycle against a real GitHub repository. Cleans up after
// itself (Release removes the assignee + owner label; the state comment is left
// in the issue thread).
func TestIntegration_ClaimAppendReleaseCycle(t *testing.T) {
	owner, repo, issueNum, _ := requireIntegration(t)
	cfg := Config{
		Owner:      owner,
		Repo:       repo,
		BinaryName: "flow-integration-test",
		// This test's subject is the claim/append cycle against a real
		// sandbox repository, not the guard, and without one nothing would be
		// published at all. What the guard refuses is asserted against the
		// mock, where no bytes leave the machine.
		Guard: allowing(),
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ref := b.refFromIssue(issueNum)

	// Doctor: must report push permission.
	if err := b.Doctor(ctx); err != nil {
		t.Fatalf("Doctor: %v", err)
	}

	claim, err := b.Claim(ctx, ref, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	t.Cleanup(func() {
		if err := b.Release(context.Background(), claim.ItemRef); err != nil {
			t.Logf("Release cleanup: %v", err)
		}
	})

	// Nothing seeds an item: the first entry is what brings its record into
	// being, so the item carries no artifact at all until one is appended.
	state, err := b.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load (pre-append): %v", err)
	}
	if rec, ok := state.Artifacts["plan"]; ok && rec.Resolved {
		t.Fatalf("pre-append plan = %+v, want nothing recorded", rec)
	}
	if len(state.Journal) != 0 {
		t.Fatalf("pre-append journal = %+v, want empty", state.Journal)
	}

	body := flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: fmt.Sprintf("Integration test plan @ %s", time.Now().Format(time.RFC3339))}
	if err := b.AppendEntry(ctx, claim.ItemRef, flow.JournalEntry{
		Step: "plan", Execution: 1, Result: body,
		Route: flow.Route{Next: "impl"}, Awaits: flow.Awaits{Role: "contributor"},
		By: flow.AccountId(cfg.BinaryName), Role: "contributor", At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}

	state, err = b.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load (post-append): %v", err)
	}
	if len(state.Journal) != 1 || state.Journal[0].Step != "plan" {
		t.Fatalf("post-append journal = %+v, want the one entry", state.Journal)
	}
	rec := state.Artifacts["plan"]
	if !rec.Resolved || rec.Version != 1 {
		t.Errorf("post-append plan = %+v, want Resolved version=1", rec)
	}
}
