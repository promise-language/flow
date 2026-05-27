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
//   GH_INTEGRATION_OWNER=<gh-login-or-org>
//   GH_INTEGRATION_REPO=<sandbox-repo>
//   GH_INTEGRATION_ISSUE=<issue-number-to-exercise>
//   GH_INTEGRATION_OWNER_LOGIN=<your-gh-login> (used as claim owner)
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

// TestIntegration_ClaimSeedResolveCycle exercises the full Claim →
// SeedState → ResolveArtifact → Release cycle against a real GitHub
// repository. Cleans up after itself (Release removes the assignee + owner
// label; the state comment is left in the issue thread).
func TestIntegration_ClaimSeedResolveCycle(t *testing.T) {
	owner, repo, issueNum, ownerLogin := requireIntegration(t)
	cfg := Config{
		Owner:      owner,
		Repo:       repo,
		BinaryName: "flow-integration-test",
	}
	b, err := NewBackend(cfg)
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

	claim, err := b.Claim(ctx, ref, ownerLogin)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	t.Cleanup(func() {
		if err := b.Release(context.Background(), claim); err != nil {
			t.Logf("Release cleanup: %v", err)
		}
	})

	specs := []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
	}
	if err := b.SeedState(ctx, claim, specs); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	state, err := b.LoadState(ctx, claim)
	if err != nil {
		t.Fatalf("LoadState (post-seed): %v", err)
	}
	rec, ok := state.Artifacts["plan"]
	if !ok || rec.Resolved {
		t.Fatalf("post-seed plan = %+v, want present + unresolved", rec)
	}

	body := flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: fmt.Sprintf("Integration test plan @ %s", time.Now().Format(time.RFC3339))}
	if err := b.ResolveArtifact(ctx, claim, "plan", body); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}

	state, err = b.LoadState(ctx, claim)
	if err != nil {
		t.Fatalf("LoadState (post-resolve): %v", err)
	}
	rec = state.Artifacts["plan"]
	if !rec.Resolved || rec.Version != 1 {
		t.Errorf("post-resolve plan = %+v, want Resolved version=1", rec)
	}
}
