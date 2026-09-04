package issue

import (
	"context"
	"strings"
	"testing"
	"time"

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
		Orchestrator: &buildTestBackend{role: RoleContributor},
		Agent:        &scriptedAgent{},
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
		Orchestrator: &buildTestBackend{role: RoleMaintainer},
		Agent:        &scriptedAgent{},
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
		Orchestrator: &buildTestBackend{role: RoleMaintainer},
		Agent:        &scriptedAgent{},
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

func (b *buildTestBackend) Name() flow.OrchestratorName { return "stub" }
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
func (b *buildTestBackend) ListAutoSelectable(context.Context, []flow.TagId) ([]flow.ItemRef, error) {
	return nil, nil
}
func (b *buildTestBackend) Claim(context.Context, flow.ItemRef, []flow.ClaimOverride) (flow.Claim, error) {
	return flow.Claim{}, nil
}
func (b *buildTestBackend) Release(context.Context, flow.ItemRef) error { return nil }
func (b *buildTestBackend) LookupClaim(context.Context, flow.ItemRef) (*flow.ClaimInfo, error) {
	return nil, nil
}
func (b *buildTestBackend) LookupActiveClaim(context.Context) (*flow.Claim, error) {
	return nil, nil
}
func (b *buildTestBackend) Load(context.Context, flow.ItemRef) (*flow.Item, error) {
	return nil, nil
}
func (b *buildTestBackend) SeedState(context.Context, flow.ItemRef, []flow.ArtifactSpec) error {
	return nil
}
func (b *buildTestBackend) ResetSeed(context.Context, flow.ItemRef) error { return nil }
func (b *buildTestBackend) ResolveArtifact(context.Context, flow.ItemRef, flow.ArtifactId, flow.ArtifactBody) error {
	return nil
}
func (b *buildTestBackend) MarkStale(context.Context, flow.ItemRef, flow.ArtifactId) error {
	return nil
}
func (b *buildTestBackend) BumpInvocations(context.Context, flow.ItemRef, flow.ArtifactId) error {
	return nil
}
func (b *buildTestBackend) BumpPrompts(context.Context, flow.ItemRef, flow.ArtifactId) error {
	return nil
}
func (b *buildTestBackend) AddCost(context.Context, flow.ItemRef, flow.ArtifactId, float64) error {
	return nil
}
func (b *buildTestBackend) AddDuration(context.Context, flow.ItemRef, flow.ArtifactId, time.Duration) error {
	return nil
}
func (b *buildTestBackend) Grant(context.Context, flow.ItemRef, flow.ArtifactId, flow.Grant) error {
	return nil
}
func (b *buildTestBackend) Park(context.Context, flow.ItemRef, flow.ParkRequest) error { return nil }
func (b *buildTestBackend) AskQuestion(context.Context, flow.ItemRef, flow.AgentQuestion) (flow.Question, error) {
	return flow.Question{}, nil
}
func (b *buildTestBackend) Worktree(context.Context, flow.ItemRef) (flow.Worktree, error) {
	return nil, nil
}

// There are no optional capabilities, so a double is the WHOLE surface or it is
// not an orchestrator at all. These refuse rather than pretend: ErrUnsupported
// says "never here", which is an answer a caller can act on.
func (b *buildTestBackend) SaveWorkInProgress(context.Context, flow.ItemRef, flow.StepId, string) error {
	return flow.ErrUnsupported
}
func (b *buildTestBackend) LoadWorkInProgress(context.Context, flow.ItemRef, flow.StepId) (string, error) {
	return "", nil
}
func (b *buildTestBackend) ClearWorkInProgress(context.Context, flow.ItemRef, flow.StepId) error {
	return nil
}
func (b *buildTestBackend) PostAnswer(context.Context, flow.ItemRef, flow.QuestionId, string) error {
	return flow.ErrUnsupported
}
func (b *buildTestBackend) Finalize(context.Context, flow.ItemRef) error { return nil }
func (b *buildTestBackend) Get(context.Context, flow.ItemRef, flow.BinaryName, func(flow.ItemType) bool) (*flow.ItemInfo, error) {
	return nil, flow.ErrUnsupported
}
func (b *buildTestBackend) List(context.Context, flow.ItemScope, flow.BinaryName, func(flow.ItemType) bool) ([]flow.ItemInfo, error) {
	return nil, nil
}
func (b *buildTestBackend) Edit(context.Context, flow.ItemRef) (flow.ItemEditor, error) {
	return nil, flow.ErrUnsupported
}
func (b *buildTestBackend) SupportedGates() []flow.GateDef {
	return []flow.GateDef{flow.Gate(flow.GateIntegration, true), flow.Gate(flow.GateFit, true)}
}
func (b *buildTestBackend) SupportedCommands() []flow.CommandDef {
	return []flow.CommandDef{flow.Command(flow.CommandVerify)}
}

func (b *buildTestBackend) ResolveRef(_ context.Context, input string) (flow.ItemRef, error) {
	return flow.ItemRef{OrchestratorName: b.Name(), Display: input}, nil
}

var _ flow.Orchestrator = (*buildTestBackend)(nil)
