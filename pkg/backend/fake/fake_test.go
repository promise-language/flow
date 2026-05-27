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

	claim, err := b.Claim(ctx, itemRef("1"), "alice")
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
	claim, _ := b.Claim(ctx, itemRef("1"), "alice")

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
	claim, _ := b.Claim(ctx, itemRef("1"), "alice")
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
	claim, _ := b.Claim(ctx, itemRef("1"), "alice")
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
	claim, _ := b.Claim(ctx, itemRef("1"), "alice")
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
	claim, _ := b.Claim(ctx, itemRef("1"), "alice")

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
	claim, _ := b.Claim(ctx, itemRef("1"), "alice")

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
	claim, _ := b.Claim(ctx, itemRef("1"), "alice")

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
