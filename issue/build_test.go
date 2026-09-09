package issue

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/cli"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
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

	items := app.Flow.Items()
	// contributor steps: plan, branch, implement, review, coverage, openPR
	// integration steps: verifyMerge, merge, recordMerge
	// closeBranch
	wantCount := 10
	if len(items) != wantCount {
		names := make([]string, len(items))
		for i, li := range items {
			names[i] = li.Description
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
		if items[i].Description != want {
			t.Errorf("step %d = %q, want %q", i, items[i].Description, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Budgets are the project's policy, handed to the SDK — not step declarations.
// ---------------------------------------------------------------------------

// Config.Budgets used to reach the SDK one StepConfig at a time; it now reaches
// it once, as App.StepBudgets keyed by the step's result id. Nothing downstream
// fails loudly if that hand-off is dropped — every step would simply fall back
// to the package defaults — so the mapping is asserted here.
func TestBuildApp_BudgetsBecomeAppPolicy(t *testing.T) {
	app, err := BuildApp(context.Background(), Config{
		BinaryName: "test",
		VerifyCmd:  []string{"bin/verify"},
		Role:       RoleContributor,
		BaseBranch: "main",
		Budgets: map[StepID]flow.StepBudget{
			StepImplement: {MaxInvocations: 9, Timeout: 90 * time.Minute},
		},
	}, Deps{Orchestrator: &buildTestBackend{role: RoleContributor}, Agent: &scriptedAgent{}})
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}

	got, ok := app.StepBudgets[flow.StepId(StepImplement)]
	if !ok {
		t.Fatalf("StepBudgets has no entry for %q; policy = %v", StepImplement, app.StepBudgets)
	}
	if got.MaxInvocations != 9 || got.Timeout != 90*time.Minute {
		t.Errorf("StepBudgets[%q] = %+v, want {MaxInvocations: 9, Timeout: 90m}", StepImplement, got)
	}
	// A step the project says nothing about must stay absent: the resolution
	// happens at read time, where an absent entry means the defaults whole.
	if _, ok := app.StepBudgets[flow.StepId(StepPlan)]; ok {
		t.Errorf("StepBudgets invented an entry for %q, which the project did not configure", StepPlan)
	}
}

// ---------------------------------------------------------------------------
// The maintainer stand-in refuses before it seeds.
// ---------------------------------------------------------------------------

// A maintainer-capability binary without carry-through has no step set, and
// what it must NOT do is seed the item on its way to saying so.
//
// Seeding is one-shot. An item checklisted with the `review-maint` artifact
// would never re-seed, so an admin who ran this once and then set
// Config.Role to contributor would find an item carrying none of the
// contributor artifacts, with every step dead on "artifact not seeded" and no
// way back short of hand-editing the state comment.
//
// The verdict is `blocked`, not `failed`: nothing failed, and no later cycle
// passes until a person sets Config.Role.
func TestMaintainerStandInBlocksWithoutSeeding(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "test#1"})

	app, err := BuildApp(context.Background(), Config{
		BinaryName: "test",
		VerifyCmd:  []string{"bin/verify"},
		Role:       RoleMaintainer,
		BaseBranch: "main",
	}, Deps{Orchestrator: be, Agent: &scriptedAgent{}})
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}

	ctx := context.Background()
	claim, err := be.Claim(ctx, be.Ref("1"), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	res, err := cli.RunOne(ctx, &app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusBlocked) {
		t.Errorf("status = %q, want %q; res = %+v", res.Status, flow.StatusBlocked, res)
	}
	if !strings.Contains(res.Reason, missingMaintainerSteps) {
		t.Errorf("reason = %q, want it to carry %q", res.Reason, missingMaintainerSteps)
	}

	state, err := be.Load(ctx, be.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Artifacts) != 0 {
		t.Errorf("item was seeded with %d artifact record(s) %v — seeding is one-shot, "+
			"so this permanently checklists the issue for a step set that does not exist",
			len(state.Artifacts), state.Artifacts)
	}
}

// ---------------------------------------------------------------------------
// buildTestBackend is the minimum viable backend for BuildApp tests.
// ---------------------------------------------------------------------------

type buildTestBackend struct {
	role Role
}

func (b *buildTestBackend) Name() flow.OrchestratorName { return "stub" }

// DetectCapabilities answers with exactly what the double's role requires, read
// from the one roleDecls table. A double that invented its own mapping could
// report a capability set no declared role covers, and every BuildApp test
// would fail on a permission model that exists nowhere but here.
func (b *buildTestBackend) DetectCapabilities(context.Context, flow.AccountId) ([]flow.Capability, error) {
	if b.role == "" {
		return nil, nil
	}
	return roleDeclFor(b.role).Capabilities, nil
}
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

// ---------------------------------------------------------------------------
// The graph the shipped compositions declare.
// ---------------------------------------------------------------------------

// wantGraph asserts the edges a composition writes down: one entry, one
// successor per step, and the one step that may finalize. A handler cannot
// elect a successor the flow does not declare, so these ARE the routes the
// shipped handlers take.
func wantGraph(t *testing.T, f *flow.Flow, edges map[flow.StepId]flow.StepId, entry, finalizer flow.StepId) {
	t.Helper()
	for _, li := range f.Items() {
		if li.Entry != (li.Result() == entry) {
			t.Errorf("step %q Entry = %t, want %t — exactly one step is where an empty journal starts",
				li.Result(), li.Entry, li.Result() == entry)
		}
		want, isFinalizer := edges[li.Result()], li.Result() == finalizer
		switch {
		case isFinalizer:
			if len(li.Next) != 0 {
				t.Errorf("finalizing step %q declares successors %v", li.Result(), li.Next)
			}
			if len(li.MayFinalize) != 1 || li.MayFinalize[0] != flow.DispositionResolved {
				t.Errorf("step %q MayFinalize = %v, want [resolved]", li.Result(), li.MayFinalize)
			}
		default:
			if len(li.Next) != 1 || li.Next[0] != want {
				t.Errorf("step %q declares Next %v, want [%s]", li.Result(), li.Next, want)
			}
			if len(li.MayFinalize) != 0 {
				t.Errorf("step %q may finalize as %v — only the last step ends the item",
					li.Result(), li.MayFinalize)
			}
		}
	}
	// ValidateGraph is what proves every registered step carries a DECLARED
	// role: it refuses an untagged step and a tag naming no declaration, so a
	// composition that passes it has both halves lined up.
	if err := f.ValidateGraph(); err != nil {
		t.Errorf("ValidateGraph() = %v, want nil", err)
	}
}

// wantRoles asserts a composition declares exactly these roles, each with the
// capabilities roleDecls gives it, and that every step carries the expected
// tag. ValidateGraph says the tags line up with SOMETHING declared; this says
// WHICH — that the merge steps sit behind the merge capability and the rest do
// not.
func wantRoles(t *testing.T, f *flow.Flow, declared []Role, tags map[flow.StepId]Role) {
	t.Helper()
	var want []flow.RoleName
	for _, r := range declared {
		want = append(want, flow.RoleName(r))
	}
	if got := f.RoleNames(); !slices.Equal(got, want) {
		t.Errorf("declared roles = %v, want %v", got, want)
	}
	for _, d := range f.Roles() {
		if wantCaps := roleDeclFor(Role(d.Name)).Capabilities; !slices.Equal(d.Capabilities, wantCaps) {
			t.Errorf("role %q requires %v, want %v from roleDecls", d.Name, d.Capabilities, wantCaps)
		}
	}
	for _, li := range f.Items() {
		if got, want := li.Role, flow.RoleName(tags[li.Result()]); got != want {
			t.Errorf("step %q is tagged %q, want %q", li.Result(), got, want)
		}
	}
}

// The contributor composition performs one role and declares one. Declaring
// the maintainer here too would put a name on the graph that no step of it can
// perform.
func TestContributorFlow_DeclaresAndTagsOneRole(t *testing.T) {
	b := &builder{cfg: Config{}, role: RoleContributor}
	wantRoles(t, b.contributorFlow(Config{}), []Role{RoleContributor}, map[flow.StepId]Role{
		flow.StepId(StepPlan):        RoleContributor,
		flow.StepId(StepBranch):      RoleContributor,
		flow.StepId(StepImplement):   RoleContributor,
		flow.StepId(StepReview):      RoleContributor,
		flow.StepId(StepCoverage):    RoleContributor,
		flow.StepId(StepOpenPR):      RoleContributor,
		flow.StepId(StepCloseBranch): RoleContributor,
	})
}

// Carrying through is one principal covering both roles and crossing the
// boundary without a handoff — and the boundary is still declared. Closing the
// branch stays the contributor's: it needs nothing the merge needed.
func TestCarryThroughFlow_DeclaresBothRolesAndTagsTheBoundary(t *testing.T) {
	b := &builder{cfg: Config{}, role: RoleMaintainer}
	wantRoles(t, b.carryThroughFlow(Config{}), []Role{RoleContributor, RoleMaintainer}, map[flow.StepId]Role{
		flow.StepId(StepPlan):        RoleContributor,
		flow.StepId(StepBranch):      RoleContributor,
		flow.StepId(StepImplement):   RoleContributor,
		flow.StepId(StepReview):      RoleContributor,
		flow.StepId(StepCoverage):    RoleContributor,
		flow.StepId(StepOpenPR):      RoleContributor,
		flow.StepId(StepVerifyMerge): RoleMaintainer,
		flow.StepId(StepMerge):       RoleMaintainer,
		flow.StepId(StepRecordMerge): RoleMaintainer,
		flow.StepId(StepCloseBranch): RoleContributor,
	})
}

func TestMaintainerStubFlow_DeclaresAndTagsTheMaintainer(t *testing.T) {
	b := &builder{cfg: Config{}, role: RoleMaintainer}
	wantRoles(t, b.unimplementedMaintainerFlow(Config{}), []Role{RoleMaintainer}, map[flow.StepId]Role{
		flow.StepId(StepReviewMaint): RoleMaintainer,
	})
}

func TestContributorFlow_DeclaresItsRoutes(t *testing.T) {
	b := &builder{cfg: Config{}, role: RoleContributor}
	wantGraph(t, b.contributorFlow(Config{}), map[flow.StepId]flow.StepId{
		flow.StepId(StepPlan):      flow.StepId(StepBranch),
		flow.StepId(StepBranch):    flow.StepId(StepImplement),
		flow.StepId(StepImplement): flow.StepId(StepReview),
		flow.StepId(StepReview):    flow.StepId(StepCoverage),
		flow.StepId(StepCoverage):  flow.StepId(StepOpenPR),
		// The one edge that differs between the compositions.
		flow.StepId(StepOpenPR): flow.StepId(StepCloseBranch),
	}, flow.StepId(StepPlan), flow.StepId(StepCloseBranch))
}

func TestCarryThroughFlow_DeclaresItsRoutes(t *testing.T) {
	b := &builder{cfg: Config{}, role: RoleMaintainer}
	wantGraph(t, b.carryThroughFlow(Config{}), map[flow.StepId]flow.StepId{
		flow.StepId(StepPlan):      flow.StepId(StepBranch),
		flow.StepId(StepBranch):    flow.StepId(StepImplement),
		flow.StepId(StepImplement): flow.StepId(StepReview),
		flow.StepId(StepReview):    flow.StepId(StepCoverage),
		flow.StepId(StepCoverage):  flow.StepId(StepOpenPR),
		// Carrying through, the request hands to the integration phase.
		flow.StepId(StepOpenPR):      flow.StepId(StepVerifyMerge),
		flow.StepId(StepVerifyMerge): flow.StepId(StepMerge),
		flow.StepId(StepMerge):       flow.StepId(StepRecordMerge),
		flow.StepId(StepRecordMerge): flow.StepId(StepCloseBranch),
	}, flow.StepId(StepPlan), flow.StepId(StepCloseBranch))
}

// A graph of one: the stub is the entry and the only way out, so what it may
// do is finalize. A stub that could not end the flow is not a stub — it is a
// graph an item never leaves.
func TestMaintainerStubFlow_IsAValidGraph(t *testing.T) {
	b := &builder{cfg: Config{}, role: RoleMaintainer}
	f := b.unimplementedMaintainerFlow(Config{})
	wantGraph(t, f, nil, flow.StepId(StepReviewMaint), flow.StepId(StepReviewMaint))
}
