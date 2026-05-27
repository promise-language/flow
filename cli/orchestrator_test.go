package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/backend/fake"
)

// stubAgent is a fake flow.Agent that returns canned responses.
type stubAgent struct {
	name      string
	responses []flow.AgentResponse
	calls     int
}

func (a *stubAgent) Name() string { return a.name }

func (a *stubAgent) Run(ctx context.Context, req flow.AgentRequest) (*flow.AgentResponse, error) {
	if a.calls >= len(a.responses) {
		return &flow.AgentResponse{LastText: "default"}, nil
	}
	r := a.responses[a.calls]
	a.calls++
	return &r, nil
}

// testApp builds a minimal App with the fake backend pre-populated with one
// item and a single-flow registration.
func testApp(t *testing.T, configure func(*flow.Flow), agent flow.Agent) (*App, *fake.Backend, flow.Claim) {
	t.Helper()
	be := fake.New(flow.Signal("pr-open", "test"))
	item := flow.Item{ID: "1", Type: "task", Title: "test#1"}
	be.AddItem(item)

	app := &App{
		Backend: be,
		Agent:   agent,
		Artifacts: []flow.ArtifactDef{
			flow.Artifact("plan", flow.ArtifactMarkdown),
			flow.Artifact("commit", flow.ArtifactCommitHash),
		},
		Signals: []flow.SignalDef{
			flow.Signal("pr-open", "test"),
		},
	}
	f := flow.NewFlow("implement", []flow.ItemType{"task"})
	configure(f)
	app.Flows = []*flow.Flow{f}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	app.Owner = "alice"

	// Capture stdout/stderr.
	app.Out = newDiscardWriter()
	app.Err = newDiscardWriter()

	ctx := context.Background()
	ref := flow.ItemRef{BackendName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)}
	claim, err := be.Claim(ctx, ref, "alice")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return app, be, claim
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func newDiscardWriter() *discardWriter { return &discardWriter{} }

func TestRunOne_SeedsAndDispatchesFirstStep(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		})
	}, a)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" || res.Step != "write plan" {
		t.Errorf("res = %+v, want step=write plan status=done", res)
	}

	state, _ := be.LoadState(context.Background(), claim)
	rec := state.Artifact("plan")
	if !rec.Resolved || rec.Markdown != "the plan" {
		t.Errorf("plan artifact = %+v, want resolved markdown 'the plan'", rec)
	}
	if rec.Invocations != 1 {
		t.Errorf("Invocations = %d, want 1", rec.Invocations)
	}
}

func TestRunOne_ParksOnInvocationsExhaustion(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		// max 1 invocation, but handler returns error each time
		f.AddStep("flaky", "plan", func(ctx flow.StepCtx) error {
			return errors.New("boom")
		}, flow.MaxInvocations(1))
	}, &stubAgent{name: "stub"})

	// First run consumes the only invocation and returns "failed".
	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "failed" {
		t.Errorf("first run status = %q, want failed", res.Status)
	}

	// Second run should park with budget-exhausted/invocations.
	res, err = RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" {
		t.Fatalf("second run status = %q, want parked. result=%+v", res.Status, res)
	}
	if res.Park == nil || res.Park.Axis != flow.AxisInvocations {
		t.Errorf("Park = %+v, want axis=invocations", res.Park)
	}
	if be.ParkRequest("1") == nil {
		t.Errorf("backend Park not recorded")
	}
}

func TestRunOne_RespectsPromptsBudget(t *testing.T) {
	a := &stubAgent{
		name: "stub",
		responses: []flow.AgentResponse{
			{LastText: "first", CostUSD: 0.1},
			{LastText: "second", CostUSD: 0.1}, // would-be-second prompt
		},
	}
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("loopy", "plan", func(ctx flow.StepCtx) error {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p1"}); err != nil {
				return err
			}
			// Second call must exhaust the prompts axis (default 1).
			_, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p2"})
			return err
		})
	}, a)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" {
		t.Fatalf("status = %q, want parked. res=%+v", res.Status, res)
	}
	if res.Park == nil || res.Park.Axis != flow.AxisPrompts {
		t.Errorf("Park = %+v, want axis=prompts", res.Park)
	}
	if a.calls != 1 {
		t.Errorf("agent calls = %d, want 1 (second blocked by budget)", a.calls)
	}
}

func TestRunOne_ParksOnTimeout(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("slow", "plan", func(ctx flow.StepCtx) error {
			<-ctx.Context().Done()
			return ctx.Context().Err()
		}, flow.Timeout(50*time.Millisecond))
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Axis != flow.AxisTimeout {
		t.Errorf("res = %+v, want parked/timeout", res)
	}
}

func TestRunOne_SignalStepAwaitsSignal(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddSignalStep("create pr", "pr-open", func(ctx flow.StepCtx) error {
			// Side effect: imagine OpenPR succeeded but signal lags.
			return nil
		})
	}, &stubAgent{name: "stub"})

	// First run dispatches the handler (returns nil). The signal isn't
	// set, so the step stays pending — but RunOne returns done for this
	// invocation because the handler didn't error.
	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Errorf("first run = %+v, want done", res)
	}
	state, _ := be.LoadState(context.Background(), claim)
	if state.SignalSet("pr-open") {
		t.Fatalf("signal should not be set yet")
	}

	// Backend observes signal — flow should now be done.
	be.SetSignal("1", "pr-open", true)
	res, err = RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Errorf("after signal set = %+v, want done with no eligible flow", res)
	}
}

func TestRunOne_AwaitSignalSkipsHandlerless(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AwaitSignal("await merge", "pr-open")
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "skipped" {
		t.Errorf("res = %+v, want skipped", res)
	}

	be.SetSignal("1", "pr-open", true)
	res, _ = RunOne(context.Background(), app, claim)
	if res.Status != "done" {
		t.Errorf("after signal = %+v, want done", res)
	}
}

func TestRunOne_QuestionsPersistedAndPark(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("ask", "plan", func(ctx flow.StepCtx) error {
			return ctx.AskQuestions(
				flow.AskYesNo("ship", "Ship it?"),
				flow.AskChoice("lib", "Which?", "a", "b"),
			)
		})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkQuestion {
		t.Errorf("res = %+v, want parked kind=question", res)
	}
	state, _ := be.LoadState(context.Background(), claim)
	if len(state.Questions) != 2 {
		t.Errorf("Questions persisted = %d, want 2", len(state.Questions))
	}
}

func TestRunOne_WrongResolveReturnsTypeMismatch(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		// Declared markdown, handler calls ResolveCommitHash.
		f.AddStep("wrong", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveCommitHash("deadbeef")
		})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "failed" {
		t.Errorf("res = %+v, want failed", res)
	}
}

func TestRunOne_NilReturnWithoutResolveParks(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("forgetful", "plan", func(ctx flow.StepCtx) error {
			return nil // forgot to resolve
		})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkStepDidNotResolve {
		t.Errorf("res = %+v, want parked step-did-not-resolve", res)
	}
}

func TestActiveClaimRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_DIR", filepath.Join(dir, ".flow"))

	if got, err := LoadActiveClaim(); got != nil || err != nil {
		t.Errorf("LoadActiveClaim on empty dir = (%v, %v), want (nil, nil)", got, err)
	}

	claim := flow.Claim{
		BackendName: "fake",
		Owner:       "alice",
		ItemRef: flow.ItemRef{
			BackendName: "fake",
			Display:     "test#1",
			Ref:         json.RawMessage(`"1"`),
		},
	}
	if err := SaveActiveClaim(claim); err != nil {
		t.Fatalf("SaveActiveClaim: %v", err)
	}
	got, err := LoadActiveClaim()
	if err != nil || got == nil {
		t.Fatalf("LoadActiveClaim: (%v, %v)", got, err)
	}
	if got.Owner != "alice" {
		t.Errorf("Owner = %q, want alice", got.Owner)
	}
	if err := ClearActiveClaim(); err != nil {
		t.Fatalf("ClearActiveClaim: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".flow", "active.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("active.json should be gone, stat err = %v", err)
	}
}

func TestApp_Validate_RejectsUnknownArtifact(t *testing.T) {
	be := fake.New()
	f := flow.NewFlow("x", nil)
	f.AddStep("step", "missing-artifact", func(flow.StepCtx) error { return nil })
	app := App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:     []*flow.Flow{f},
	}
	if err := app.validate(); err == nil {
		t.Errorf("expected validation error for unknown artifact")
	}
}

func TestApp_Validate_RejectsUnsupportedSignal(t *testing.T) {
	be := fake.New() // no signals supported
	f := flow.NewFlow("x", nil)
	f.AddSignalStep("sig", "pr-open", func(flow.StepCtx) error { return nil })
	app := App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("dummy", flow.ArtifactFlag)},
		Signals:   []flow.SignalDef{flow.Signal("pr-open", "x")},
		Flows:     []*flow.Flow{f},
	}
	if err := app.validate(); err == nil {
		t.Errorf("expected validation error for signal not in SupportedSignals")
	}
}

func TestSelectFlow_RequireSignalGate(t *testing.T) {
	be := fake.New(flow.Signal("pr-open", "x"))
	a := flow.NewFlow("contributor", []flow.ItemType{"task"})
	a.AddStep("plan", "plan", func(flow.StepCtx) error { return nil })
	b := flow.NewFlow("maintainer", []flow.ItemType{"task"})
	b.RequireSignal("pr-open")
	b.AddStep("merge", "commit", func(flow.StepCtx) error { return nil })

	app := &App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown), flow.Artifact("commit", flow.ArtifactCommitHash)},
		Signals:   []flow.SignalDef{flow.Signal("pr-open", "x")},
		Flows:     []*flow.Flow{a, b},
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	state := &flow.ItemState{
		Item:      flow.Item{ID: "1", Type: "task"},
		Artifacts: map[flow.ArtifactId]flow.ArtifactRecord{},
		Signals:   map[flow.SignalId]flow.SignalState{},
	}
	picked, _ := SelectFlow(app, state)
	if picked == nil || picked.Name() != "contributor" {
		t.Errorf("expected contributor before pr-open; got %v", picked)
	}

	// Resolve contributor's only step.
	state.Artifacts["plan"] = flow.ArtifactRecord{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Resolved: true}
	// Maintainer still gated on pr-open.
	picked, _ = SelectFlow(app, state)
	if picked != nil {
		t.Errorf("expected no eligible flow without pr-open; got %v", picked.Name())
	}

	state.Signals["pr-open"] = flow.SignalState{Set: true}
	picked, _ = SelectFlow(app, state)
	if picked == nil || picked.Name() != "maintainer" {
		t.Errorf("expected maintainer once pr-open set; got %v", picked)
	}
}
