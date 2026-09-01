package fake_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/backend/fake"
)

func newItem(id string) flow.Item {
	return flow.Item{ID: id, Type: "task", Title: "Test item " + id}
}

func itemRef(id string) flow.ItemRef {
	return flow.ItemRef{
		BackendName: "fake",
		Display:     id,
		Ref:         json.RawMessage(`"` + id + `"`),
	}
}

func TestBackend_ClaimAndLookup(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	b.AddItem(newItem("1"))

	claim, err := b.Claim(ctx, itemRef("1"), "alice", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claim.Owner != "alice" {
		t.Errorf("claim.Owner = %q, want alice", claim.Owner)
	}

	info, err := b.LookupClaim(ctx, itemRef("1"))
	if err != nil {
		t.Fatalf("LookupClaim: %v", err)
	}
	if info == nil || info.Owner != "alice" {
		t.Errorf("LookupClaim info = %+v, want owner alice", info)
	}
}

func TestBackend_SeedRefusesSecondSeed(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	b.AddItem(newItem("1"))
	claim, _ := b.Claim(ctx, itemRef("1"), "alice", nil)

	specs := []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
	}
	if err := b.SeedState(ctx, claim, specs); err != nil {
		t.Fatalf("first SeedState: %v", err)
	}
	if err := b.SeedState(ctx, claim, specs); err == nil {
		t.Errorf("second SeedState should refuse; got nil")
	}
}

func TestBackend_ResolveArtifactRoundTrip(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	b.AddItem(newItem("1"))
	claim, _ := b.Claim(ctx, itemRef("1"), "alice", nil)
	_ = b.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
	})

	body := flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the plan"}
	if err := b.ResolveArtifact(ctx, claim, "plan", body); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}

	state, err := b.LoadState(ctx, claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	rec := state.Artifact("plan")
	if !rec.Resolved {
		t.Errorf("artifact not marked resolved")
	}
	if rec.Markdown != "the plan" {
		t.Errorf("Markdown = %q, want %q", rec.Markdown, "the plan")
	}
	if rec.Version != 1 {
		t.Errorf("Version = %d, want 1", rec.Version)
	}
}

func TestBackend_ResolveArtifactRejectsTypeMismatch(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	b.AddItem(newItem("1"))
	claim, _ := b.Claim(ctx, itemRef("1"), "alice", nil)
	_ = b.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
	})

	err := b.ResolveArtifact(ctx, claim, "plan", flow.ArtifactBody{Type: flow.ArtifactPatch})
	if err == nil {
		t.Fatalf("expected type-mismatch error")
	}
}

func TestBackend_BudgetCountersAndGrant(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	b.AddItem(newItem("1"))
	claim, _ := b.Claim(ctx, itemRef("1"), "alice", nil)
	_ = b.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Budget: flow.DefaultStepBudget()},
	})

	for range 2 {
		if err := b.BumpInvocations(ctx, claim, "plan"); err != nil {
			t.Fatalf("BumpInvocations: %v", err)
		}
	}
	_ = b.AddCost(ctx, claim, "plan", 3.5)
	_ = b.AddCost(ctx, claim, "plan", 1.5)

	state, _ := b.LoadState(ctx, claim)
	rec := state.Artifact("plan")
	if rec.Invocations != 2 {
		t.Errorf("Invocations = %d, want 2", rec.Invocations)
	}
	if rec.CostUSDSpent != 5.0 {
		t.Errorf("CostUSDSpent = %v, want 5.0", rec.CostUSDSpent)
	}

	if err := b.Grant(ctx, claim, "plan", flow.Grant{Invocations: 5, CostUSD: 20}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	state, _ = b.LoadState(ctx, claim)
	rec = state.Artifact("plan")
	want := flow.DefaultStepBudget().MaxInvocations + 5
	if rec.GrantedInvocations != want {
		t.Errorf("GrantedInvocations = %d, want %d", rec.GrantedInvocations, want)
	}
	if rec.GrantedCostUSD != flow.DefaultStepBudget().MaxCostUSD+20 {
		t.Errorf("GrantedCostUSD = %v, want %v", rec.GrantedCostUSD, flow.DefaultStepBudget().MaxCostUSD+20)
	}
}

func TestBackend_SignalSet(t *testing.T) {
	ctx := context.Background()
	b := fake.New(flow.Signal("pr-open", "test"))
	b.AddItem(newItem("1"))
	claim, _ := b.Claim(ctx, itemRef("1"), "alice", nil)

	b.SetSignal("1", "pr-open", true)
	state, _ := b.LoadState(ctx, claim)
	if !state.SignalSet("pr-open") {
		t.Errorf("pr-open should be set after SetSignal")
	}
}

func TestBackend_AskQuestionsAssignsIDsAndAnswerFlow(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	b.AddItem(newItem("1"))
	claim, _ := b.Claim(ctx, itemRef("1"), "alice", nil)

	qs := []flow.AgentQuestion{
		flow.AskYesNo("ship", "Ship it?"),
		flow.AskChoice("lib", "Which lib?", "a", "b"),
	}
	out, err := b.AskQuestions(ctx, claim, qs)
	if err != nil {
		t.Fatalf("AskQuestions: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("AskQuestions returned %d, want 2", len(out))
	}
	if out[0].ID == "" || out[1].ID == "" || out[0].ID == out[1].ID {
		t.Errorf("IDs not assigned uniquely: %q, %q", out[0].ID, out[1].ID)
	}
	if out[0].Text != "Ship it?" || out[1].Format != flow.FormatChoice {
		t.Errorf("returned questions don't match input: %+v", out)
	}

	// State surfaces them as pending.
	state, _ := b.LoadState(ctx, claim)
	if len(state.Questions) != 2 {
		t.Errorf("LoadState.Questions len = %d, want 2", len(state.Questions))
	}
	if len(state.PendingQuestions()) != 2 {
		t.Errorf("PendingQuestions len = %d, want 2 before answer", len(state.PendingQuestions()))
	}

	// Answer one — Pending drops to 1.
	if err := b.AnswerQuestion("1", out[0].ID, "yes"); err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	state, _ = b.LoadState(ctx, claim)
	if len(state.PendingQuestions()) != 1 {
		t.Errorf("PendingQuestions len = %d, want 1 after answering one", len(state.PendingQuestions()))
	}
}

func TestBackend_ParkRecordsRequest(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	b.AddItem(newItem("1"))
	claim, _ := b.Claim(ctx, itemRef("1"), "alice", nil)

	req := flow.ParkRequest{
		Kind:   flow.ParkBudgetExhausted,
		Step:   "plan",
		Axis:   flow.AxisInvocations,
		Reason: "exhausted",
	}
	if err := b.Park(ctx, claim, req); err != nil {
		t.Fatalf("Park: %v", err)
	}
	got := b.ParkRequest("1")
	if got == nil || got.Kind != flow.ParkBudgetExhausted || got.Axis != flow.AxisInvocations {
		t.Errorf("ParkRequest = %+v, want budget-exhausted/invocations", got)
	}
}

// parkedItem seeds an item whose "plan" step has burned its 3 invocations and
// parked on the invocations axis — the state a `grant` acts on.
func parkedItem(t *testing.T, b *fake.Backend) flow.Claim {
	t.Helper()
	ctx := context.Background()
	b.AddItem(newItem("1"))
	claim, err := b.Claim(ctx, itemRef("1"), "alice", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true,
			Budget: flow.StepBudget{MaxInvocations: 3, MaxCostUSD: 10}},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}
	for range 3 {
		if err := b.BumpInvocations(ctx, claim, "plan"); err != nil {
			t.Fatalf("BumpInvocations: %v", err)
		}
	}
	if err := b.Park(ctx, claim, flow.ParkRequest{
		Kind: flow.ParkBudgetExhausted, Step: "plan", Axis: flow.AxisInvocations,
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	return claim
}

// LoadState surfaces the park so a caller can see WHY the item stopped.
func TestBackend_LoadStateSurfacesPark(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	claim := parkedItem(t, b)

	st, err := b.LoadState(ctx, claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !st.Parked() {
		t.Fatal("ItemState.Park = nil, want the recorded park")
	}
	if st.Park.Step != "plan" || st.Park.Axis != flow.AxisInvocations {
		t.Errorf("park = %+v, want plan/invocations", st.Park)
	}
}

// The Backend.Grant contract: a grant that gives the parked axis headroom
// clears the park; one that does not, leaves it.
func TestBackend_GrantClearsParkOnlyWhenSatisfied(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	claim := parkedItem(t, b)

	// Cost is not the parked axis — the park must survive.
	if err := b.Grant(ctx, claim, "plan", flow.Grant{CostUSD: 50}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if b.ParkRequest("1") == nil {
		t.Fatal("park cleared by a grant on an unrelated axis")
	}

	if err := b.Grant(ctx, claim, "plan", flow.Grant{Invocations: 1}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if p := b.ParkRequest("1"); p != nil {
		t.Errorf("park = %+v, want cleared once invocations had headroom", p)
	}
	st, err := b.LoadState(ctx, claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.Parked() {
		t.Error("LoadState still reports a park after it was cleared")
	}
}

// Resolving the parked step makes its park obsolete; keeping it would make
// LoadState report a reason that no longer holds.
func TestBackend_ResolveClearsParkForThatStep(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	claim := parkedItem(t, b)

	if err := b.ResolveArtifact(ctx, claim, "plan",
		flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "done"}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}
	if p := b.ParkRequest("1"); p != nil {
		t.Errorf("park = %+v, want cleared by the resolve", p)
	}
}

// gateWorktree claims an item and returns its worktree.
func gateWorktree(t *testing.T, b *fake.Backend) flow.Worktree {
	t.Helper()
	ctx := context.Background()
	b.AddItem(newItem("1"))
	claim, err := b.Claim(ctx, itemRef("1"), "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := b.Worktree(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	return wt
}

// The fake is what a caller's tests are written against, so the outcomes have
// to reach them AS outcomes. A fake that turned "died" into an error would let
// a caller be written that cannot tell a dead host from a missing binary — the
// exact collapse the outcome set exists to prevent — and every one of that
// caller's tests would still pass.
func TestBackend_SetGateOutcomeReachesTheWorktreeAsAnOutcome(t *testing.T) {
	ctx := context.Background()
	for _, outcome := range []flow.GateOutcome{
		flow.OutcomeMeasured,
		flow.OutcomeTimedOut,
		flow.OutcomeCouldNotStart,
		flow.OutcomeDied,
		flow.OutcomeBrokeContract,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			b := fake.New()
			b.SetGateOutcome(outcome)
			run, err := gateWorktree(t, b).RunGate(ctx, flow.GateIntegration)
			if err != nil {
				t.Fatalf("RunGate: %v — every way a gate fails is an outcome, not an error", err)
			}
			if run.Outcome != outcome {
				t.Errorf("Outcome = %q, want %q", run.Outcome, outcome)
			}
			if run.Gate != flow.GateIntegration {
				t.Errorf("Gate = %q, want the name that was asked for", run.Gate)
			}
		})
	}
}

// The default has to be the one outcome that lets an unrelated test get on with
// its subject, and the envelope it reports has to be one that parses: the fake
// models the protocol, and a caller that reads Stdout would otherwise be
// written against a shape no real gate produces.
func TestBackend_GateMeasuresByDefaultAndPrintsSomethingThatParses(t *testing.T) {
	run, err := gateWorktree(t, fake.New()).RunGate(context.Background(), flow.GateTested)
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if run.Outcome != flow.OutcomeMeasured {
		t.Fatalf("Outcome = %q, want %q", run.Outcome, flow.OutcomeMeasured)
	}
	var envelope map[string]any
	if err := json.Unmarshal(run.Stdout, &envelope); err != nil || envelope == nil {
		t.Errorf("Stdout = %q, want one JSON object: %v", run.Stdout, err)
	}
}

// measuredRun is a run the fake's judge will answer about: the only kind
// anything may be asked to judge.
func measuredRun(gate flow.GateName) flow.GateRun {
	return flow.GateRun{Gate: gate, Outcome: flow.OutcomeMeasured, Stdout: []byte(`{}`)}
}

// The default has to let an unrelated test get on with its subject, and the
// terms it reports have to parse: the fake models the protocol, so a caller
// that carries a verdict's thresholds is written against a shape a real judge
// produces.
func TestBackend_JudgesAcceptableByDefaultWithTermsThatParse(t *testing.T) {
	run := measuredRun(flow.GateTested)
	v, err := gateWorktree(t, fake.New()).Judge(context.Background(), run)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if !v.Acceptable {
		t.Error("Acceptable = false, want the default")
	}
	var thresholds map[string]any
	if err := json.Unmarshal(v.Thresholds, &thresholds); err != nil || thresholds == nil {
		t.Errorf("Thresholds = %q, want one JSON object: %v", v.Thresholds, err)
	}
	// The measurement travels with the verdict, or nothing can re-check it.
	if v.Run.Gate != run.Gate || v.Run.Outcome != run.Outcome {
		t.Errorf("Run = %+v, want the measurement that was judged", v.Run)
	}
}

// A REFUSAL IS A VERDICT. A fake that returned one as an error would let a
// caller be written that cannot tell "the project says no" from "the judge
// could not answer" — and that caller would treat a broken judging layer as a
// failing tree, refusing sound changes for a reason nowhere in them.
func TestBackend_SetGateVerdictRefusesWithoutErroring(t *testing.T) {
	b := fake.New()
	b.SetGateVerdict(false)
	v, err := gateWorktree(t, b).Judge(context.Background(), measuredRun(flow.GateIntegration))
	if err != nil {
		t.Fatalf("Judge: %v — a refusal is an answer, not an error", err)
	}
	if v.Acceptable {
		t.Error("Acceptable = true, want the refusal that was configured")
	}
}

// The two requests no judge could answer. Only a measured run may be judged:
// the other outcomes mean no measurement exists, and a judge asked about one
// would have to invent an answer — which, read as a refusal, blames a change
// for a gate that never ran.
func TestBackend_JudgeRefusesWhatCannotBeJudged(t *testing.T) {
	for _, c := range []struct {
		name string
		run  flow.GateRun
	}{
		{"a run that measured nothing", flow.GateRun{Gate: flow.GateTested, Outcome: flow.OutcomeDied}},
		{"a run carrying no outcome at all", flow.GateRun{Gate: flow.GateTested}},
		{"an undeclared gate name", measuredRun("lint")},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, err := gateWorktree(t, fake.New()).Judge(context.Background(), c.run)
			if err == nil {
				t.Fatal("Judge answered a request no judge could answer")
			}
			if v.Acceptable {
				t.Error("Acceptable = true beside an error")
			}
			if v.Thresholds != nil {
				t.Errorf("Thresholds = %q, want none — nothing was compared", v.Thresholds)
			}
		})
	}
}

// A refusal configured AFTER the worktree was handed out still reaches it,
// which is the order a caller's test is naturally written in: claim, then set
// up the failure it is about to exercise.
//
// This is the one that regresses invisibly. A fake that only fixed the verdict
// at Worktree() time would leave such a test exercising the default — the
// ACCEPTABLE path — while its name and its author both say refusal, and it
// would go on passing for as long as the caller kept letting the change
// through.
func TestBackend_SetGateVerdictReachesAWorktreeAlreadyHandedOut(t *testing.T) {
	b := fake.New()
	wt := gateWorktree(t, b)
	b.SetGateVerdict(false)
	v, err := wt.Judge(context.Background(), measuredRun(flow.GateTested))
	if err != nil {
		t.Fatalf("Judge: %v — a refusal is an answer, not an error", err)
	}
	if v.Acceptable {
		t.Error("Acceptable = true: the refusal did not reach a worktree that already existed")
	}
}

// An undeclared name is a request no runner could attempt, so it is the one
// thing that is an error — and the GateRun beside it must carry no outcome. A
// caller that read a measurement out of it would act on a gate that never ran.
func TestBackend_RunGateRefusesAnUndeclaredNameWithNoOutcome(t *testing.T) {
	run, err := gateWorktree(t, fake.New()).RunGate(context.Background(), "lint")
	if err == nil {
		t.Fatal("RunGate accepted an undeclared gate name")
	}
	if run.Outcome != "" {
		t.Errorf("Outcome = %q, want none — no gate ran", run.Outcome)
	}
}

// The fake is what SDK tests drive the work-in-progress path against, so it has
// to model the property that path turns on: a record belongs to one item and
// one step, and is invisible under any other key.
func TestBackend_WorkInProgressIsKeyedByItemAndStep(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	b.AddItem(newItem("1"))
	b.AddItem(newItem("2"))
	claim1, _ := b.Claim(ctx, itemRef("1"), "alice", nil)
	claim2, _ := b.Claim(ctx, itemRef("2"), "alice", nil)

	if got, err := b.LoadWorkInProgress(ctx, claim1, "plan"); got != "" || err != nil {
		t.Errorf("Load with nothing stored = (%q, %v), want (\"\", nil)", got, err)
	}
	if err := b.SaveWorkInProgress(ctx, claim1, "plan", "item 1's reasoning"); err != nil {
		t.Fatalf("SaveWorkInProgress: %v", err)
	}
	if got, _ := b.LoadWorkInProgress(ctx, claim1, "plan"); got != "item 1's reasoning" {
		t.Errorf("Load under its own key = %q, want the stored body", got)
	}
	if got, _ := b.LoadWorkInProgress(ctx, claim2, "plan"); got != "" {
		t.Errorf("Load under another item = %q, want nothing", got)
	}
	if got, _ := b.LoadWorkInProgress(ctx, claim1, "review"); got != "" {
		t.Errorf("Load under another step = %q, want nothing", got)
	}
	if err := b.ClearWorkInProgress(ctx, claim1, "plan"); err != nil {
		t.Fatalf("ClearWorkInProgress: %v", err)
	}
	if err := b.ClearWorkInProgress(ctx, claim1, "plan"); err != nil {
		t.Errorf("second Clear = %v, want nil", err)
	}
}

// Releasing ends that reasoning's life — there is nothing left to resume, and
// what stays behind is prose nobody asked for.
func TestBackend_ReleaseDropsWorkInProgress(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	b.AddItem(newItem("1"))
	claim, _ := b.Claim(ctx, itemRef("1"), "alice", nil)

	if err := b.SaveWorkInProgress(ctx, claim, "plan", "reasoning"); err != nil {
		t.Fatalf("SaveWorkInProgress: %v", err)
	}
	if err := b.Release(ctx, claim); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got, err := b.LoadWorkInProgress(ctx, claim, "plan"); got != "" || err != nil {
		t.Errorf("Load after Release = (%q, %v), want nothing left", got, err)
	}
}
