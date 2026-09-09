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
// A maintainer binary stops where the contributor's work begins.
// ---------------------------------------------------------------------------

// The graph is the same one every build registers, so what stops a
// maintainer-capability binary on a fresh item is COVERAGE: the entry step is
// the contributor's, and this binary does not perform it.
//
// The verdict is `blocked`, not `failed`: nothing failed, and no later cycle
// passes until a runner that covers the contributor role picks the item up —
// which is the handoff the boundary exists to produce.
//
// It must also stop CLEANLY, writing nothing. A dispatch would run the plan
// step under a maintainer's account and record it, which is precisely the
// crossing the coverage declaration exists to decline.
func TestMaintainerBinaryBlocksAtTheContributorEntry(t *testing.T) {
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
	for _, want := range []string{string(StepPlan), string(RoleContributor), string(RoleMaintainer)} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("reason = %q, want it to name %q", res.Reason, want)
		}
	}

	state, err := be.Load(ctx, be.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Journal) != 0 {
		t.Errorf("journal = %+v — a pre-dispatch stop records nothing", state.Journal)
	}
	if len(state.Artifacts) != 0 {
		t.Errorf("item carries %d artifact record(s) %v — nothing ran, so nothing was captured",
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
func (b *buildTestBackend) ArenaRoot() string           { return "" }

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
		flow.Artifact("proposal-review", flow.ArtifactMarkdown),
		flow.Artifact("verify-merge", flow.ArtifactMarkdown),
		flow.Artifact("merge-commit", flow.ArtifactCommitHash),
	}
}
func (b *buildTestBackend) ListAutoSelectable(context.Context, []flow.TagId, func(flow.RoleName) bool) ([]flow.ItemRef, error) {
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
func (b *buildTestBackend) Reset(context.Context, flow.ItemRef) error { return nil }
func (b *buildTestBackend) AppendEntry(context.Context, flow.ItemRef, flow.JournalEntry) error {
	return nil
}
func (b *buildTestBackend) RecordDispatch(context.Context, flow.ItemRef, flow.StepId) error {
	return nil
}
func (b *buildTestBackend) RecordResumption(context.Context, flow.ItemRef, flow.StepId) error {
	return nil
}
func (b *buildTestBackend) AddCost(context.Context, flow.ItemRef, flow.StepId, float64) error {
	return nil
}
func (b *buildTestBackend) AddDuration(context.Context, flow.ItemRef, flow.StepId, time.Duration) error {
	return nil
}
func (b *buildTestBackend) AddWaiting(context.Context, flow.ItemRef, flow.StepId, time.Duration) error {
	return nil
}
func (b *buildTestBackend) Grant(context.Context, flow.ItemRef, flow.StepId, flow.Grant) error {
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
func (b *buildTestBackend) Finalize(context.Context, flow.ItemRef, flow.Disposition) error {
	return nil
}
func (b *buildTestBackend) Get(context.Context, flow.ItemRef, flow.BinaryName, func(flow.ItemType) bool, func(flow.RoleName) bool) (*flow.ItemInfo, error) {
	return nil, flow.ErrUnsupported
}
func (b *buildTestBackend) List(context.Context, flow.ItemScope, flow.BinaryName, func(flow.ItemType) bool, func(flow.RoleName) bool) ([]flow.ItemInfo, error) {
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
// The graph the shipped flow declares.
// ---------------------------------------------------------------------------

// wantGraph asserts the edges the flow writes down: one entry, each step's
// declared successors, and which steps may finalize with which dispositions. A
// handler cannot elect a successor the flow does not declare, so these ARE the
// routes the shipped handlers take.
//
// Successors are a SET per step, not one each, because the graph has a step
// that decides between routes — review the proposal declares three outcomes and
// picks one at runtime, which is the whole reason routes are declared rather
// than sequenced. Finalizers are likewise a map: two steps may end the item, on
// different dispositions, and a graph with one hard-coded finalizer could not
// express a rejection at all.
func wantGraph(t *testing.T, f *flow.Flow, entry flow.StepId,
	edges map[flow.StepId][]flow.StepId, finalizers map[flow.StepId][]flow.Disposition) {
	t.Helper()
	seen := map[flow.StepId]bool{}
	for _, li := range f.Items() {
		id := li.Result()
		seen[id] = true
		if li.Entry != (id == entry) {
			t.Errorf("step %q Entry = %t, want %t — exactly one step is where an empty journal starts",
				id, li.Entry, id == entry)
		}
		if want := edges[id]; !slices.Equal(li.Next, want) {
			t.Errorf("step %q declares Next %v, want %v", id, li.Next, want)
		}
		if want := finalizers[id]; !slices.Equal(li.MayFinalize, want) {
			t.Errorf("step %q MayFinalize = %v, want %v", id, li.MayFinalize, want)
		}
	}
	for id := range edges {
		if !seen[id] {
			t.Errorf("the table declares edges out of %q, which the flow does not register", id)
		}
	}
	for id := range finalizers {
		if !seen[id] {
			t.Errorf("the table says %q may finalize, and the flow does not register it", id)
		}
	}
	// ValidateGraph is what proves the declaration hangs together: every
	// successor names something registered, every step is reachable from the
	// entry, finalization is reachable from every step, and every step carries a
	// DECLARED role.
	if err := f.ValidateGraph(); err != nil {
		t.Errorf("ValidateGraph() = %v, want nil", err)
	}
}

// wantRoles asserts the flow declares exactly these roles, each with the
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

// The routes docs/issue-flow.md § The graph writes down, whole: the
// contributor's run to a proposal, the boundary at close branch, and the
// maintainer's judgement with its three outcomes.
func TestResolveFlow_DeclaresTheDocumentedGraph(t *testing.T) {
	b := &builder{cfg: Config{}, role: RoleContributor}
	wantGraph(t, b.resolveFlow(Config{}), flow.StepId(StepPlan),
		map[flow.StepId][]flow.StepId{
			flow.StepId(StepPlan):      {flow.StepId(StepBranch)},
			flow.StepId(StepBranch):    {flow.StepId(StepImplement)},
			flow.StepId(StepImplement): {flow.StepId(StepReview)},
			flow.StepId(StepReview):    {flow.StepId(StepCoverage)},
			flow.StepId(StepCoverage):  {flow.StepId(StepOpenPR)},
			flow.StepId(StepOpenPR):    {flow.StepId(StepCloseBranch)},
			// The role boundary: the contributor's part ends by handing the
			// proposal to the maintainer, not by finalizing.
			flow.StepId(StepCloseBranch): {flow.StepId(StepReviewProposal)},
			// The one branching step, and the rework edge back to implement is
			// the reason the advance cannot be a checklist: implement's result
			// is already recorded by the time this route is elected.
			flow.StepId(StepReviewProposal): {
				flow.StepId(StepVerifyMerge),
				flow.StepId(StepImplement),
			},
			flow.StepId(StepVerifyMerge): {flow.StepId(StepMerge)},
			flow.StepId(StepMerge):       {flow.StepId(StepRecordMerge)},
			// record merge commit declares none: it ends the item.
		},
		map[flow.StepId][]flow.Disposition{
			flow.StepId(StepReviewProposal): {flow.DispositionRejected},
			flow.StepId(StepRecordMerge):    {flow.DispositionResolved},
		})
}

// Eleven tags, and the boundary is the edge between them: close branch is the
// contributor's last step, review the proposal the maintainer's first.
func TestResolveFlow_TagsEveryStepAndDeclaresBothRoles(t *testing.T) {
	b := &builder{cfg: Config{}, role: RoleContributor}
	f := b.resolveFlow(Config{})
	wantRoles(t, f, []Role{RoleContributor, RoleMaintainer}, map[flow.StepId]Role{
		flow.StepId(StepPlan):           RoleContributor,
		flow.StepId(StepBranch):         RoleContributor,
		flow.StepId(StepImplement):      RoleContributor,
		flow.StepId(StepReview):         RoleContributor,
		flow.StepId(StepCoverage):       RoleContributor,
		flow.StepId(StepOpenPR):         RoleContributor,
		flow.StepId(StepCloseBranch):    RoleContributor,
		flow.StepId(StepReviewProposal): RoleMaintainer,
		flow.StepId(StepVerifyMerge):    RoleMaintainer,
		flow.StepId(StepMerge):          RoleMaintainer,
		flow.StepId(StepRecordMerge):    RoleMaintainer,
	})
	// The boundary itself, asserted as an edge rather than inferred from the
	// tags: a graph whose roles were right but whose contributor half did not
	// reach the maintainer half would be two graphs in one registration.
	closeBranch, ok := f.ItemByResult(flow.StepId(StepCloseBranch))
	if !ok {
		t.Fatal("the flow does not register close branch")
	}
	if !slices.Contains(closeBranch.Next, flow.StepId(StepReviewProposal)) {
		t.Errorf("close branch declares %v, want the maintainer's review among them", closeBranch.Next)
	}
}

// The worktree contract, step by step, as docs/issue-flow.md § The branching
// sequence is declared writes it: read along any route, each step's Leaves
// hands its successor the state its Needs requires.
func TestResolveFlow_DeclaresTheBranchingSequence(t *testing.T) {
	want := map[flow.StepId]struct {
		needs  flow.NeedsState
		leaves flow.LeavesState
	}{
		flow.StepId(StepPlan):           {flow.NeedsAny, flow.LeavesAsFound},
		flow.StepId(StepBranch):         {flow.NeedsAny, flow.LeavesItemBranch},
		flow.StepId(StepImplement):      {flow.NeedsItemBranch, flow.LeavesItemBranch},
		flow.StepId(StepReview):         {flow.NeedsItemBranch, flow.LeavesItemBranch},
		flow.StepId(StepCoverage):       {flow.NeedsItemBranch, flow.LeavesItemBranch},
		flow.StepId(StepOpenPR):         {flow.NeedsItemBranch, flow.LeavesItemBranch},
		flow.StepId(StepCloseBranch):    {flow.NeedsItemBranch, flow.LeavesBase},
		flow.StepId(StepReviewProposal): {flow.NeedsAny, flow.LeavesAsFound},
		flow.StepId(StepVerifyMerge):    {flow.NeedsItemBranch, flow.LeavesAsFound},
		flow.StepId(StepMerge):          {flow.NeedsAny, flow.LeavesAsFound},
		flow.StepId(StepRecordMerge):    {flow.NeedsAny, flow.LeavesAsFound},
	}
	b := &builder{cfg: Config{}, role: RoleContributor}
	items := (&builder{cfg: b.cfg, role: b.role}).resolveFlow(Config{}).Items()
	if len(items) != len(want) {
		t.Fatalf("the flow registers %d steps, and the branching sequence covers %d", len(items), len(want))
	}
	for _, li := range items {
		w, ok := want[li.Result()]
		if !ok {
			t.Errorf("step %q declares no branching state in the document's table", li.Result())
			continue
		}
		if li.Needs != w.needs || li.Leaves != w.leaves {
			t.Errorf("step %q needs %q and leaves %q, want %q and %q",
				li.Result(), li.Needs, li.Leaves, w.needs, w.leaves)
		}
	}
}

// The graph does not vary by who is running. Coverage is what narrows a
// binary's part of it, at dispatch; building a different graph per role would
// decide the processing before the journal begins, with nothing recording why —
// and would put the boundary outside the one object that is supposed to show
// it.
func TestResolveFlow_IsTheSameGraphForEveryRole(t *testing.T) {
	shape := func(f *flow.Flow) []flow.LifecycleItem { return f.Items() }
	base := shape((&builder{cfg: Config{}, role: RoleContributor}).resolveFlow(Config{}))
	for _, tc := range []struct {
		name         string
		role         Role
		carryThrough bool
	}{
		{"contributor", RoleContributor, false},
		{"maintainer", RoleMaintainer, false},
		{"maintainer carrying through", RoleMaintainer, true},
		// The account that backs nothing builds the same graph too: the gate
		// that stops it is a preflight, not a missing step.
		{"no role", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{CarryThrough: tc.carryThrough}
			got := shape((&builder{cfg: cfg, role: tc.role}).resolveFlow(cfg))
			if len(got) != len(base) {
				t.Fatalf("registers %d steps, want the %d every other build registers", len(got), len(base))
			}
			for i := range got {
				a, want := got[i], base[i]
				if a.Description != want.Description || a.Result() != want.Result() ||
					a.Kind != want.Kind || a.Role != want.Role || a.Entry != want.Entry ||
					!slices.Equal(a.Next, want.Next) || !slices.Equal(a.MayFinalize, want.MayFinalize) ||
					a.Needs != want.Needs || a.Leaves != want.Leaves || a.Writes != want.Writes {
					t.Errorf("step %d differs from the contributor build:\n got %+v\nwant %+v", i, a, want)
				}
			}
		})
	}
}
