package issue

import (
	"context"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// ---------------------------------------------------------------------------
// CarryThrough validation.
// ---------------------------------------------------------------------------

func TestCarryThroughContributorRefused(t *testing.T) {
	cfg := Config{
		BinaryName:   "test",
		VerifyCmd:    []string{"bin/verify"},
		Role:         RoleContributor,
		CarryThrough: true,
	}
	deps := Deps{
		Backend: &buildTestBackend{role: RoleContributor},
		Agent:   &scriptedAgent{},
	}

	_, err := BuildApp(context.Background(), cfg, deps)
	if err == nil {
		t.Fatal("expected error for CarryThrough with RoleContributor")
	}
	if !strings.Contains(err.Error(), "CarryThrough") {
		t.Errorf("error should mention CarryThrough: %s", err)
	}
	if !strings.Contains(err.Error(), string(RoleContributor)) {
		t.Errorf("error should mention the contributor role: %s", err)
	}
}

func TestCarryThroughMaintainerAccepted(t *testing.T) {
	cfg := Config{
		BinaryName:   "test",
		VerifyCmd:    []string{"bin/verify"},
		Role:         RoleMaintainer,
		CarryThrough: true,
	}
	deps := Deps{
		Backend: &buildTestBackend{role: RoleMaintainer},
		Agent:   &scriptedAgent{},
	}

	app, err := BuildApp(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}
	if !app.CarryThrough {
		t.Error("app.CarryThrough should be true")
	}
}

func TestCarryThroughFlowComposition(t *testing.T) {
	cfg := Config{
		BinaryName:   "test",
		VerifyCmd:    []string{"bin/verify"},
		Role:         RoleMaintainer,
		CarryThrough: true,
	}
	deps := Deps{
		Backend: &buildTestBackend{role: RoleMaintainer},
		Agent:   &scriptedAgent{},
	}

	app, err := BuildApp(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}

	if len(app.Flows) != 1 {
		t.Fatalf("expected 1 flow, got %d", len(app.Flows))
	}

	items := app.Flows[0].Items()
	// contributor steps: plan, branch, implement, review, coverage, openPR
	// integration steps: verifyMerge, merge, recordMerge
	// closeBranch
	wantCount := 10
	if len(items) != wantCount {
		names := make([]string, len(items))
		for i, li := range items {
			names[i] = li.Name
		}
		t.Fatalf("flow has %d steps %v, want %d", len(items), names, wantCount)
	}

	// Check that the integration steps come after contributor steps and before
	// closeBranch.
	wantOrder := []string{
		"write plan",
		"open branch",
		"implement the change",
		"review the work",
		"analyze coverage",
		"create pull request",
		"verify merge result",
		"merge pull request",
		"record merge commit",
		"close branch",
	}
	for i, want := range wantOrder {
		if items[i].Name != want {
			t.Errorf("step %d = %q, want %q", i, items[i].Name, want)
		}
	}
}

// ---------------------------------------------------------------------------
// buildTestBackend is the minimum viable backend for BuildApp tests.
// ---------------------------------------------------------------------------

type buildTestBackend struct {
	role Role
}

func (b *buildTestBackend) Name() string { return "stub" }
func (b *buildTestBackend) SupportedSignals() []flow.SignalDef {
	return []flow.SignalDef{
		flow.Signal("pr-open", "pull request is open"),
		flow.Signal("pr-merged", "pull request has been merged"),
	}
}
func (b *buildTestBackend) SupportedArtifacts() []flow.ArtifactDef {
	return []flow.ArtifactDef{
		flow.Artifact("plan", flow.ArtifactMarkdown),
		flow.Artifact("branch", flow.ArtifactCommitHash),
		flow.Artifact("implementation", flow.ArtifactCommitHash),
		flow.Artifact("review", flow.ArtifactMarkdown),
		flow.Artifact("coverage", flow.ArtifactMarkdown),
		flow.Artifact("branch-closed", flow.ArtifactFlag),
		flow.Artifact("review-maint", flow.ArtifactMarkdown),
		flow.Artifact("verify-merge", flow.ArtifactMarkdown),
		flow.Artifact("merge-commit", flow.ArtifactCommitHash),
	}
}
func (b *buildTestBackend) ListEligible(context.Context) ([]flow.ItemRef, error) { return nil, nil }
func (b *buildTestBackend) Claim(context.Context, flow.ItemRef, string, []flow.ClaimOverride) (flow.Claim, error) {
	return flow.Claim{}, nil
}
func (b *buildTestBackend) Release(context.Context, flow.Claim) error { return nil }
func (b *buildTestBackend) LookupClaim(context.Context, flow.ItemRef) (*flow.ClaimInfo, error) {
	return nil, nil
}
func (b *buildTestBackend) LookupActiveClaim(context.Context, string) (*flow.Claim, error) {
	return nil, nil
}
func (b *buildTestBackend) LoadState(context.Context, flow.Claim) (*flow.ItemState, error) {
	return nil, nil
}
func (b *buildTestBackend) SeedState(context.Context, flow.Claim, []flow.ArtifactSpec) error {
	return nil
}
func (b *buildTestBackend) ResetSeed(context.Context, flow.Claim) error { return nil }
func (b *buildTestBackend) ResolveArtifact(context.Context, flow.Claim, flow.ArtifactId, flow.ArtifactBody) error {
	return nil
}
func (b *buildTestBackend) MarkStale(context.Context, flow.Claim, flow.ArtifactId) error { return nil }
func (b *buildTestBackend) BumpInvocations(context.Context, flow.Claim, string) error    { return nil }
func (b *buildTestBackend) BumpPrompts(context.Context, flow.Claim, string) error        { return nil }
func (b *buildTestBackend) AddCost(context.Context, flow.Claim, string, float64) error   { return nil }
func (b *buildTestBackend) Grant(context.Context, flow.Claim, string, flow.Grant) error  { return nil }
func (b *buildTestBackend) Park(context.Context, flow.Claim, flow.ParkRequest) error     { return nil }
func (b *buildTestBackend) AskQuestions(context.Context, flow.Claim, []flow.AgentQuestion) ([]flow.Question, error) {
	return nil, nil
}
func (b *buildTestBackend) Worktree(context.Context, flow.Claim) (flow.Worktree, error) {
	return nil, nil
}
