package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// staleTestEnv builds an App with a single "plan" artifact step, seeds it, and
// optionally resolves it. Returns the app (with captured stdout/stderr), the
// backend, and the claim.
func staleTestEnv(t *testing.T, resolve bool) (*App, *fake.Orchestrator, flow.Claim, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, a)

	ctx := context.Background()
	if err := be.SeedState(ctx, claim.ItemRef, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}
	if resolve {
		if err := be.ResolveArtifact(ctx, claim.ItemRef, "plan", flow.ArtifactBody{
			Type: flow.ArtifactMarkdown, Markdown: "the plan",
		}); err != nil {
			t.Fatalf("ResolveArtifact: %v", err)
		}
	}

	var out, errBuf bytes.Buffer
	app.Out = &out
	app.Err = &errBuf
	return app, be, claim, &out, &errBuf
}

func TestCmdStale_HappyPath(t *testing.T) {
	app, be, claim, out, errBuf := staleTestEnv(t, true)

	code := app.cmdStale(context.Background(), []string{"plan"})
	if code != 0 {
		t.Fatalf("cmdStale = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), `marked "plan" stale`) {
		t.Errorf("stdout = %q, want contains 'marked \"plan\" stale'", out.String())
	}
	// Verify the backend state was actually updated.
	state, err := be.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := state.Artifact("plan")
	if !rec.Stale {
		t.Error("expected plan artifact to be marked stale")
	}
}

func TestCmdStale_AlreadyStale(t *testing.T) {
	app, be, claim, out, _ := staleTestEnv(t, true)

	// Mark stale once via backend directly.
	if err := be.MarkStale(context.Background(), claim.ItemRef, "plan"); err != nil {
		t.Fatalf("MarkStale: %v", err)
	}

	code := app.cmdStale(context.Background(), []string{"plan"})
	if code != 0 {
		t.Fatalf("cmdStale = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "already stale") {
		t.Errorf("stdout = %q, want contains 'already stale'", out.String())
	}
}

func TestCmdStale_Pending(t *testing.T) {
	app, _, _, _, errBuf := staleTestEnv(t, false) // seeded but not resolved

	code := app.cmdStale(context.Background(), []string{"plan"})
	if code != 1 {
		t.Fatalf("cmdStale = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "pending") {
		t.Errorf("stderr = %q, want contains 'pending'", errBuf.String())
	}
}

func TestCmdStale_Skipped(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, a)

	ctx := context.Background()
	if err := be.SeedState(ctx, claim.ItemRef, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: false, Budget: flow.DefaultStepBudget()},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	var errBuf bytes.Buffer
	app.Out = &bytes.Buffer{}
	app.Err = &errBuf

	code := app.cmdStale(context.Background(), []string{"plan"})
	if code != 1 {
		t.Fatalf("cmdStale = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "skipped") {
		t.Errorf("stderr = %q, want contains 'skipped'", errBuf.String())
	}
}

func TestCmdStale_UnknownStepID(t *testing.T) {
	app, _, _, _, errBuf := staleTestEnv(t, true)

	code := app.cmdStale(context.Background(), []string{"bogus"})
	if code != 1 {
		t.Fatalf("cmdStale = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "unknown step id") {
		t.Errorf("stderr = %q, want contains 'unknown step id'", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "plan") {
		t.Errorf("stderr = %q, want lists valid ids including 'plan'", errBuf.String())
	}
}

func TestCmdStale_SignalStep(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
		f.AddSignalStep("create pr", "pr-open", func(ctx flow.StepCtx) error {
			return nil
		}, flow.StepConfig{})
	}, a)

	ctx := context.Background()
	if err := be.SeedState(ctx, claim.ItemRef, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	var errBuf bytes.Buffer
	app.Out = &bytes.Buffer{}
	app.Err = &errBuf

	code := app.cmdStale(context.Background(), []string{"pr-open"})
	if code != 1 {
		t.Fatalf("cmdStale = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "signal step") {
		t.Errorf("stderr = %q, want contains 'signal step'", errBuf.String())
	}
}

func TestCmdStale_HumanLabel(t *testing.T) {
	app, _, _, _, errBuf := staleTestEnv(t, true)

	code := app.cmdStale(context.Background(), []string{"write plan"})
	if code != 1 {
		t.Fatalf("cmdStale = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "step label, not a step id") {
		t.Errorf("stderr = %q, want contains 'step label, not a step id'", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), `did you mean "plan"`) {
		t.Errorf("stderr = %q, want contains 'did you mean \"plan\"'", errBuf.String())
	}
}

func TestCmdStale_NotSeeded(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, _, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, a)

	var errBuf bytes.Buffer
	app.Out = &bytes.Buffer{}
	app.Err = &errBuf

	code := app.cmdStale(context.Background(), []string{"plan"})
	if code != 1 {
		t.Fatalf("cmdStale = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "not seeded") {
		t.Errorf("stderr = %q, want contains 'not seeded'", errBuf.String())
	}
}

func TestCmdStale_NoActiveClaim(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, a)

	// Release the claim so there is no active one.
	if err := be.Release(context.Background(), claim.ItemRef); err != nil {
		t.Fatalf("Release: %v", err)
	}

	var errBuf bytes.Buffer
	app.Out = &bytes.Buffer{}
	app.Err = &errBuf

	code := app.cmdStale(context.Background(), []string{"plan"})
	if code != 1 {
		t.Fatalf("cmdStale = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "no active claim") {
		t.Errorf("stderr = %q, want contains 'no active claim'", errBuf.String())
	}
}

func TestCmdStale_NoArgument(t *testing.T) {
	app, _, _, _, errBuf := staleTestEnv(t, true)

	code := app.cmdStale(context.Background(), []string{})
	if code != 2 {
		t.Fatalf("cmdStale = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "requires exactly one") {
		t.Errorf("stderr = %q, want contains 'requires exactly one'", errBuf.String())
	}
}

func TestCmdStale_ExtraArgument(t *testing.T) {
	app, _, _, _, errBuf := staleTestEnv(t, true)

	code := app.cmdStale(context.Background(), []string{"plan", "extra"})
	if code != 2 {
		t.Fatalf("cmdStale = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "requires exactly one") {
		t.Errorf("stderr = %q, want contains 'requires exactly one'", errBuf.String())
	}
}

func TestCmdStale_JSONOutput(t *testing.T) {
	app, _, _, out, errBuf := staleTestEnv(t, true)

	code := app.cmdStale(context.Background(), []string{"--json", "plan"})
	if code != 0 {
		t.Fatalf("cmdStale = %d, want 0; stderr=%q", code, errBuf.String())
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON: %v; raw=%q", err, out.String())
	}
	if m["step_id"] != "plan" {
		t.Errorf("step_id = %v, want plan", m["step_id"])
	}
	if m["already_stale"] != false {
		t.Errorf("already_stale = %v, want false", m["already_stale"])
	}
}

func TestCmdStale_JSONAlreadyStale(t *testing.T) {
	app, be, claim, out, _ := staleTestEnv(t, true)

	if err := be.MarkStale(context.Background(), claim.ItemRef, "plan"); err != nil {
		t.Fatalf("MarkStale: %v", err)
	}

	code := app.cmdStale(context.Background(), []string{"--json", "plan"})
	if code != 0 {
		t.Fatalf("cmdStale = %d, want 0", code)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if m["already_stale"] != true {
		t.Errorf("already_stale = %v, want true", m["already_stale"])
	}
}

func TestCmdStale_WarningBudgetExhausted(t *testing.T) {
	app, be, claim, _, errBuf := staleTestEnv(t, true)

	ctx := context.Background()
	// Exhaust invocations by bumping to meet the granted amount.
	state, _ := be.Load(ctx, claim.ItemRef)
	rec := state.Artifact("plan")
	for i := 0; i < rec.GrantedInvocations; i++ {
		_ = be.BumpInvocations(ctx, claim.ItemRef, "plan")
	}

	code := app.cmdStale(context.Background(), []string{"plan"})
	if code != 0 {
		t.Fatalf("cmdStale = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no budget headroom") {
		t.Errorf("stderr = %q, want contains 'no budget headroom'", errBuf.String())
	}
}

func TestCmdStale_WarningItemParked(t *testing.T) {
	app, be, claim, _, errBuf := staleTestEnv(t, true)

	ctx := context.Background()
	_ = be.Park(ctx, claim.ItemRef, flow.ParkRequest{
		Kind: flow.ParkQuestion,
		Step: "plan",
	})

	code := app.cmdStale(context.Background(), []string{"plan"})
	if code != 0 {
		t.Fatalf("cmdStale = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "item is parked") {
		t.Errorf("stderr = %q, want contains 'item is parked'", errBuf.String())
	}
}

func TestCmdStale_WarningPendingQuestions(t *testing.T) {
	app, be, claim, _, errBuf := staleTestEnv(t, true)

	ctx := context.Background()
	_, err := askAll(be, ctx, claim, []flow.AgentQuestion{
		{Text: "should we proceed?"},
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	code := app.cmdStale(context.Background(), []string{"plan"})
	if code != 0 {
		t.Fatalf("cmdStale = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "1 pending question") {
		t.Errorf("stderr = %q, want contains '1 pending question'", errBuf.String())
	}
}

func TestCmdStale_SeededButNotInFlow(t *testing.T) {
	// A step ID that exists in the item's artifact records but is not part of
	// the current flow definition — e.g. leftover from a previous flow version.
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, a)

	ctx := context.Background()
	// Seed an artifact "old-step" that does not correspond to any step in the flow.
	if err := be.SeedState(ctx, claim.ItemRef, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
		{Id: "old-step", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}
	if err := be.ResolveArtifact(ctx, claim.ItemRef, "old-step", flow.ArtifactBody{
		Type: flow.ArtifactMarkdown, Markdown: "stale data",
	}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}

	var out, errBuf bytes.Buffer
	app.Out = &out
	app.Err = &errBuf

	code := app.cmdStale(context.Background(), []string{"old-step"})
	if code != 0 {
		t.Fatalf("cmdStale = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no longer part of flow") {
		t.Errorf("stderr = %q, want contains 'no longer part of flow'", errBuf.String())
	}
	// Should still mark stale despite the warning.
	state, err := be.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := state.Artifact("old-step")
	if !rec.Stale {
		t.Error("expected old-step artifact to be marked stale despite not being in flow")
	}
}

func TestCmdStale_JSONOutputIncludesWarnings(t *testing.T) {
	app, be, claim, out, errBuf := staleTestEnv(t, true)

	ctx := context.Background()
	// Create a pending question to trigger the warning.
	_, err := askAll(be, ctx, claim, []flow.AgentQuestion{
		{Text: "should we proceed?"},
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	code := app.cmdStale(context.Background(), []string{"--json", "plan"})
	if code != 0 {
		t.Fatalf("cmdStale = %d, want 0; stderr=%q", code, errBuf.String())
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON: %v; raw=%q", err, out.String())
	}
	ws, ok := m["warnings"]
	if !ok {
		t.Fatal("JSON output missing 'warnings' field")
	}
	wsList, ok := ws.([]any)
	if !ok || len(wsList) == 0 {
		t.Fatalf("warnings = %v, want non-empty list", ws)
	}
	found := false
	for _, w := range wsList {
		if s, ok := w.(string); ok && strings.Contains(s, "pending question") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one containing 'pending question'", wsList)
	}
}
