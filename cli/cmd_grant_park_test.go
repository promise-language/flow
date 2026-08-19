package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/backend/fake"
)

// parkGrantEnv is the scaffolding for the identity/park tests: a flow with two
// artifact steps (whose labels differ from their ids — the whole point) and one
// signal step, seeded with explicit budgets.
type parkGrantEnv struct {
	app   *App
	be    *fake.Backend
	claim flow.Claim
	out   *bytes.Buffer
	err   *bytes.Buffer
}

func newParkGrantEnv(t *testing.T) *parkGrantEnv {
	t.Helper()
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{Budget: flow.StepBudget{
			MaxInvocations:          3,
			MaxPromptsPerInvocation: 1,
			MaxCostUSD:              10,
			Timeout:                 30 * time.Minute,
		}})
		f.AddStep("record the commit", "commit", func(ctx flow.StepCtx) error {
			return ctx.ResolveCommitHash("abc")
		}, flow.StepConfig{})
		f.AddSignalStep("create pull request", "pr-open", func(ctx flow.StepCtx) error {
			return nil
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	env := &parkGrantEnv{app: app, be: be, claim: claim, out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	app.Out, app.Err = env.out, env.err
	env.seed(t, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.StepBudget{
			MaxInvocations: 3, MaxPromptsPerInvocation: 1, MaxCostUSD: 10, Timeout: 30 * time.Minute,
		}},
		{Id: "commit", Type: flow.ArtifactCommitHash, Required: true, Budget: flow.DefaultStepBudget()},
	})
	return env
}

func (e *parkGrantEnv) seed(t *testing.T, specs []flow.ArtifactSpec) {
	t.Helper()
	if err := e.be.SeedState(context.Background(), e.claim, specs); err != nil {
		t.Fatalf("SeedState: %v", err)
	}
}

func (e *parkGrantEnv) rec(t *testing.T, id flow.ArtifactId) flow.ArtifactRecord {
	t.Helper()
	st, err := e.be.LoadState(context.Background(), e.claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return st.Artifact(id)
}

func (e *parkGrantEnv) park(t *testing.T, req flow.ParkRequest) {
	t.Helper()
	if err := e.be.Park(context.Background(), e.claim, req); err != nil {
		t.Fatalf("Park: %v", err)
	}
}

func (e *parkGrantEnv) parked(t *testing.T) *flow.ParkRequest {
	t.Helper()
	st, err := e.be.LoadState(context.Background(), e.claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return st.Park
}

func (e *parkGrantEnv) grant(args ...string) int {
	return e.app.cmdGrant(context.Background(), args)
}

// budgetExhausted is the park RunOne writes when a step burns its invocations.
func budgetExhausted(step string, axis flow.BudgetAxis) flow.ParkRequest {
	return flow.ParkRequest{Kind: flow.ParkBudgetExhausted, Step: step, Axis: axis, Reason: "test park"}
}

// ---------------------------------------------------------------------------
// Identity: the id is the only accepted name, and every refusal is silent on
// the backend.
// ---------------------------------------------------------------------------

func TestGrantTarget_RejectsStepLabel(t *testing.T) {
	env := newParkGrantEnv(t)

	code := env.grant("write plan", "--invocations", "1")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(env.err.String(), "step label, not a step id") {
		t.Errorf("stderr = %q, want 'step label, not a step id'", env.err.String())
	}
	if !strings.Contains(env.err.String(), `"plan"`) {
		t.Errorf("stderr = %q, want it to name the id", env.err.String())
	}
	if got := env.rec(t, "plan").GrantedInvocations; got != 3 {
		t.Errorf("GrantedInvocations = %d, want 3 (unchanged — a refusal must not write)", got)
	}
}

func TestGrantTarget_RejectsUnknownId(t *testing.T) {
	env := newParkGrantEnv(t)

	code := env.grant("push", "--invocations", "1")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	for _, want := range []string{"unknown step id", "valid ids", "plan", "commit"} {
		if !strings.Contains(env.err.String(), want) {
			t.Errorf("stderr = %q, want %q", env.err.String(), want)
		}
	}
	// Signal ids are not grant targets, so they must not be advertised as valid.
	if strings.Contains(env.err.String(), "pr-open") {
		t.Errorf("stderr = %q, must not list the signal id as grantable", env.err.String())
	}
}

func TestGrantTarget_SuggestsNearMiss(t *testing.T) {
	env := newParkGrantEnv(t)

	if code := env.grant("pln", "--invocations", "1"); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(env.err.String(), `did you mean "plan"`) {
		t.Errorf("stderr = %q, want a did-you-mean for 'plan'", env.err.String())
	}
}

func TestGrantTarget_RejectsSignalStep(t *testing.T) {
	env := newParkGrantEnv(t)

	code := env.grant("pr-open", "--invocations", "1")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(env.err.String(), "signal step") {
		t.Errorf("stderr = %q, want 'signal step'", env.err.String())
	}
}

func TestGrantTarget_RejectsUnseededId(t *testing.T) {
	// No SeedState call: the flow declares "plan" but the item has no budget
	// record for it yet.
	app, _, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("x")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	errBuf := &bytes.Buffer{}
	app.Out, app.Err = &bytes.Buffer{}, errBuf

	code := app.cmdGrant(context.Background(), []string{"plan", "--invocations", "1"})
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "not seeded") {
		t.Errorf("stderr = %q, want 'not seeded'", errBuf.String())
	}
}

// A record seeded before the flow dropped the step is still real budget, so the
// grant lands — with a warning rather than a refusal.
func TestGrantTarget_SeededButNoLongerInFlow(t *testing.T) {
	env := newParkGrantEnv(t)
	// A second seed is refused by the fake, so reset first and seed a set that
	// includes an id the flow no longer declares.
	if err := env.be.ResetSeed(context.Background(), env.claim); err != nil {
		t.Fatalf("ResetSeed: %v", err)
	}
	env.seed(t, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true},
		{Id: "coverage", Type: flow.ArtifactMarkdown, Required: true},
	})

	if code := env.grant("coverage", "--invocations", "2"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if !strings.Contains(env.err.String(), "no longer part of flow") {
		t.Errorf("stderr = %q, want the stale-step warning", env.err.String())
	}
	if got := env.rec(t, "coverage").GrantedInvocations; got != 2 {
		t.Errorf("GrantedInvocations = %d, want 2", got)
	}
}

// ---------------------------------------------------------------------------
// Park-driven default
// ---------------------------------------------------------------------------

func TestGrantPark_InvocationsAxis(t *testing.T) {
	env := newParkGrantEnv(t)
	for range 3 {
		if err := env.be.BumpInvocations(context.Background(), env.claim, "plan"); err != nil {
			t.Fatalf("BumpInvocations: %v", err)
		}
	}
	env.park(t, budgetExhausted("plan", flow.AxisInvocations))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	// used(3) + headroom(1) = 4.
	if got := env.rec(t, "plan").GrantedInvocations; got != 4 {
		t.Errorf("GrantedInvocations = %d, want 4", got)
	}
	if p := env.parked(t); p != nil {
		t.Errorf("park = %+v, want cleared", p)
	}
	if !strings.Contains(env.out.String(), "invocations 3 → 4") {
		t.Errorf("stdout = %q, want the delta", env.out.String())
	}
}

func TestGrantPark_CostAxisUsesStepBudgetAsHeadroom(t *testing.T) {
	env := newParkGrantEnv(t)
	if err := env.be.AddCost(context.Background(), env.claim, "plan", 12.40); err != nil {
		t.Fatalf("AddCost: %v", err)
	}
	env.park(t, budgetExhausted("plan", flow.AxisCost))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	// spent(12.40) + the step's own $10 cap = 22.40.
	if got := env.rec(t, "plan").GrantedCostUSD; got != 22.40 {
		t.Errorf("GrantedCostUSD = %v, want 22.40", got)
	}
	if p := env.parked(t); p != nil {
		t.Errorf("park = %+v, want cleared", p)
	}
}

func TestGrantPark_TimeoutAxisAddsOneMoreRun(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, budgetExhausted("plan", flow.AxisTimeout))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.rec(t, "plan").GrantedTimeout; got != time.Hour {
		t.Errorf("GrantedTimeout = %v, want 1h (30m seeded + 30m granted)", got)
	}
}

func TestGrantPark_PromptsAxisRaisesTheCap(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, budgetExhausted("plan", flow.AxisPrompts))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.rec(t, "plan").GrantedPromptsPerInvocation; got != 2 {
		t.Errorf("GrantedPromptsPerInvocation = %d, want 2", got)
	}
}

// The most valuable refusal: granting budget cannot clear a question park, so
// bare `grant` must say what will, and write nothing.
func TestGrantPark_RefusesNonBudgetPark(t *testing.T) {
	env := newParkGrantEnv(t)
	if _, err := env.be.AskQuestions(context.Background(), env.claim, []flow.AgentQuestion{
		flow.AskText("base", "which base branch?"),
	}); err != nil {
		t.Fatalf("AskQuestions: %v", err)
	}
	env.park(t, flow.ParkRequest{Kind: flow.ParkQuestion, Step: "plan", Reason: "question pending"})

	code := env.grant()
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	for _, want := range []string{"not a budget cap", "which base branch?", "Answer the question"} {
		if !strings.Contains(env.err.String(), want) {
			t.Errorf("stderr = %q, want %q", env.err.String(), want)
		}
	}
	if got := env.rec(t, "plan").GrantedInvocations; got != 3 {
		t.Errorf("GrantedInvocations = %d, want 3 (unchanged)", got)
	}
}

func TestGrantPark_StaleParkOnResolvedStep(t *testing.T) {
	env := newParkGrantEnv(t)
	if err := env.be.ResolveArtifact(context.Background(), env.claim, "plan",
		flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "done"}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}
	// Park recorded AFTER the step resolved — a record that outlived its
	// reason. The CLI must notice, since the backend only clears a park when
	// the step resolves or a grant satisfies it.
	env.park(t, budgetExhausted("plan", flow.AxisInvocations))

	code := env.grant()
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stale park is not an error); stderr=%q", code, env.err.String())
	}
	// The note is a result, not an error: it belongs on stdout (and in the
	// JSON payload), not stderr.
	if !strings.Contains(env.out.String(), "stale") {
		t.Errorf("stdout = %q, want 'stale'", env.out.String())
	}
	if env.err.String() != "" {
		t.Errorf("stderr = %q, want empty on a successful no-op", env.err.String())
	}
	if got := env.rec(t, "plan").GrantedInvocations; got != 3 {
		t.Errorf("GrantedInvocations = %d, want 3 (unchanged)", got)
	}
}

func TestGrantPark_StaleWhenHeadroomAlreadyExists(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, budgetExhausted("plan", flow.AxisInvocations))
	// Granted 3, used 0 — the park cannot be current.
	code := env.grant()
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if !strings.Contains(env.out.String(), "already has headroom") {
		t.Errorf("stdout = %q, want 'already has headroom'", env.out.String())
	}
}

func TestGrantPark_RejectsFlagForOtherAxis(t *testing.T) {
	env := newParkGrantEnv(t)
	if err := env.be.AddCost(context.Background(), env.claim, "plan", 12); err != nil {
		t.Fatalf("AddCost: %v", err)
	}
	env.park(t, budgetExhausted("plan", flow.AxisCost))

	code := env.grant("--invocations", "5")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(env.err.String(), "does not apply") {
		t.Errorf("stderr = %q, want 'does not apply'", env.err.String())
	}
	if got := env.rec(t, "plan").GrantedCostUSD; got != 10 {
		t.Errorf("GrantedCostUSD = %v, want 10 (unchanged)", got)
	}
}

func TestGrantPark_FlagOverridesHeadroom(t *testing.T) {
	env := newParkGrantEnv(t)
	for range 3 {
		if err := env.be.BumpInvocations(context.Background(), env.claim, "plan"); err != nil {
			t.Fatalf("BumpInvocations: %v", err)
		}
	}
	env.park(t, budgetExhausted("plan", flow.AxisInvocations))

	if code := env.grant("--invocations", "5"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.rec(t, "plan").GrantedInvocations; got != 8 { // used(3) + 5
		t.Errorf("GrantedInvocations = %d, want 8", got)
	}
}

// A park written before this version recorded the human label. Accepting it
// keeps items parked across the upgrade grantable.
func TestGrantPark_AcceptsLegacyLabelInParkRecord(t *testing.T) {
	env := newParkGrantEnv(t)
	for range 3 {
		if err := env.be.BumpInvocations(context.Background(), env.claim, "plan"); err != nil {
			t.Fatalf("BumpInvocations: %v", err)
		}
	}
	env.park(t, budgetExhausted("write plan", flow.AxisInvocations))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.rec(t, "plan").GrantedInvocations; got != 4 {
		t.Errorf("GrantedInvocations = %d, want 4", got)
	}
}

// A grant too small to clear the cap must leave the park in place and say so —
// otherwise a tool grants a token amount and loops forever.
func TestGrant_TooSmallLeavesParkAndReportsIt(t *testing.T) {
	env := newParkGrantEnv(t)
	if err := env.be.AddCost(context.Background(), env.claim, "plan", 12.40); err != nil {
		t.Fatalf("AddCost: %v", err)
	}
	env.park(t, budgetExhausted("plan", flow.AxisCost))

	if code := env.grant("plan", "--cost", "0.01"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if p := env.parked(t); p == nil {
		t.Fatal("park was cleared by a grant that does not clear the cap")
	}
	if !strings.Contains(env.out.String(), "still parked") {
		t.Errorf("stdout = %q, want 'still parked'", env.out.String())
	}
}

// ---------------------------------------------------------------------------
// --all sweep and --dry-run
// ---------------------------------------------------------------------------

func TestGrantAll_ToppsUpPendingOnly(t *testing.T) {
	env := newParkGrantEnv(t)
	for range 3 {
		if err := env.be.BumpInvocations(context.Background(), env.claim, "plan"); err != nil {
			t.Fatalf("BumpInvocations: %v", err)
		}
	}
	if err := env.be.ResolveArtifact(context.Background(), env.claim, "commit",
		flow.ArtifactBody{Type: flow.ArtifactCommitHash, CommitHash: "abc"}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}
	before := env.rec(t, "commit").GrantedInvocations

	if code := env.grant("--all"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.rec(t, "plan").GrantedInvocations; got != 4 {
		t.Errorf("plan GrantedInvocations = %d, want 4", got)
	}
	if got := env.rec(t, "commit").GrantedInvocations; got != before {
		t.Errorf("commit GrantedInvocations = %d, want %d (resolved steps are skipped)", got, before)
	}
}

func TestGrantAll_NoOpWhenHeadroomExists(t *testing.T) {
	env := newParkGrantEnv(t)

	if code := env.grant("--all"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.rec(t, "plan").GrantedInvocations; got != 3 {
		t.Errorf("GrantedInvocations = %d, want 3 (untouched)", got)
	}
	if !strings.Contains(env.out.String(), "already have headroom") {
		t.Errorf("stdout = %q, want the no-op line", env.out.String())
	}
}

func TestGrantAll_RejectsStepId(t *testing.T) {
	env := newParkGrantEnv(t)

	if code := env.grant("--all", "plan"); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(env.err.String(), "cannot be combined") {
		t.Errorf("stderr = %q, want 'cannot be combined'", env.err.String())
	}
}

func TestGrant_DryRunWritesNothing(t *testing.T) {
	env := newParkGrantEnv(t)
	for range 3 {
		if err := env.be.BumpInvocations(context.Background(), env.claim, "plan"); err != nil {
			t.Fatalf("BumpInvocations: %v", err)
		}
	}
	env.park(t, budgetExhausted("plan", flow.AxisInvocations))

	if code := env.grant("--dry-run"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.rec(t, "plan").GrantedInvocations; got != 3 {
		t.Errorf("GrantedInvocations = %d, want 3 (dry run must not write)", got)
	}
	if p := env.parked(t); p == nil {
		t.Error("dry run cleared the park")
	}
	if !strings.Contains(env.out.String(), "dry run") {
		t.Errorf("stdout = %q, want 'dry run'", env.out.String())
	}
}

// A no-op is still a result: JSON callers get a payload with a note, not an
// empty stdout they have to special-case.
func TestGrantPark_StaleParkStillEmitsPayload(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, budgetExhausted("plan", flow.AxisInvocations)) // granted 3, used 0

	if code := env.grant("--json"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	m := decode(t, env.out)
	if m["mode"] != grantModePark {
		t.Errorf("mode = %v, want park", m["mode"])
	}
	note, _ := m["note"].(string)
	if !strings.Contains(note, "already has headroom") {
		t.Errorf("note = %q, want the stale-park explanation", note)
	}
	if granted, _ := m["granted"].([]any); len(granted) != 0 {
		t.Errorf("granted = %v, want empty", granted)
	}
	if m["unparked"] != false {
		t.Errorf("unparked = %v, want false", m["unparked"])
	}
}

// An explicit zero on the parked axis grants nothing; reporting that as
// "already has headroom" would misdescribe the step.
func TestGrantPark_RejectsExplicitZeroOnParkedAxis(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, budgetExhausted("plan", flow.AxisPrompts))

	if code := env.grant("--prompts", "0"); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(env.err.String(), "would grant nothing") {
		t.Errorf("stderr = %q, want 'would grant nothing'", env.err.String())
	}
	if got := env.rec(t, "plan").GrantedPromptsPerInvocation; got != 1 {
		t.Errorf("GrantedPromptsPerInvocation = %d, want 1 (unchanged)", got)
	}
}

// A park naming a step with no budget record cannot be topped up, and the
// message must say that rather than surfacing a backend "not seeded" error.
func TestGrantPark_ParkOnUnseededStep(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, budgetExhausted("nonesuch", flow.AxisInvocations))

	if code := env.grant(); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(env.err.String(), "not a step of flow") {
		t.Errorf("stderr = %q, want the not-a-step refusal", env.err.String())
	}
}

// A dry run must not report an unpark as something that happened.
func TestGrant_DryRunPredictsUnparkWithoutClaimingIt(t *testing.T) {
	env := newParkGrantEnv(t)
	for range 3 {
		if err := env.be.BumpInvocations(context.Background(), env.claim, "plan"); err != nil {
			t.Fatalf("BumpInvocations: %v", err)
		}
	}
	env.park(t, budgetExhausted("plan", flow.AxisInvocations))

	if code := env.grant("--dry-run"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	out := env.out.String()
	if !strings.Contains(out, "would unpark") {
		t.Errorf("stdout = %q, want 'would unpark'", out)
	}
	if strings.Contains(out, "\nunparked") {
		t.Errorf("stdout = %q, must not claim the item was unparked", out)
	}
}

// ---------------------------------------------------------------------------
// Collateral top-up: bare `grant` must clear every axis that would re-park the
// step at once, not just the one named in the park.
// ---------------------------------------------------------------------------

// The ping-pong case. A timed-out run burns an invocation on its way out, so a
// timeout park typically arrives with the invocations axis flat too. Granting
// time alone buys a dispatch that never reaches the handler.
func TestGrantPark_TimeoutParkAlsoTopsUpExhaustedInvocations(t *testing.T) {
	env := newParkGrantEnv(t)
	ctx := context.Background()
	for range 3 { // burn all 3 seeded invocations
		if err := env.be.BumpInvocations(ctx, env.claim, "plan"); err != nil {
			t.Fatalf("BumpInvocations: %v", err)
		}
	}
	env.park(t, budgetExhausted("plan", flow.AxisTimeout))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	rec := env.rec(t, "plan")
	if rec.GrantedTimeout != time.Hour {
		t.Errorf("GrantedTimeout = %v, want 1h (30m seeded + 30m granted)", rec.GrantedTimeout)
	}
	if rec.GrantedInvocations != 4 {
		t.Errorf("GrantedInvocations = %d, want 4 (3 used + 1 headroom)", rec.GrantedInvocations)
	}
	if p := env.parked(t); p != nil {
		t.Errorf("park = %+v, want cleared", p)
	}
}

// Both pre-dispatch gates flat at once: the parked axis plus the other two.
func TestGrantPark_TimeoutParkAlsoTopsUpExhaustedCost(t *testing.T) {
	env := newParkGrantEnv(t)
	ctx := context.Background()
	for range 3 {
		if err := env.be.BumpInvocations(ctx, env.claim, "plan"); err != nil {
			t.Fatalf("BumpInvocations: %v", err)
		}
	}
	if err := env.be.AddCost(ctx, env.claim, "plan", 12); err != nil {
		t.Fatalf("AddCost: %v", err)
	}
	env.park(t, budgetExhausted("plan", flow.AxisTimeout))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	rec := env.rec(t, "plan")
	if rec.GrantedTimeout != time.Hour {
		t.Errorf("GrantedTimeout = %v, want 1h", rec.GrantedTimeout)
	}
	if rec.GrantedInvocations != 4 {
		t.Errorf("GrantedInvocations = %d, want 4", rec.GrantedInvocations)
	}
	// spent(12) + the step's own $10 cap = 22.
	if rec.GrantedCostUSD != 22 {
		t.Errorf("GrantedCostUSD = %v, want 22", rec.GrantedCostUSD)
	}
}

// An axis with headroom is left alone: the top-up is targeted, not a sweep.
func TestGrantPark_LeavesAxesWithHeadroomAlone(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, budgetExhausted("plan", flow.AxisTimeout))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	rec := env.rec(t, "plan")
	if rec.GrantedTimeout != time.Hour {
		t.Errorf("GrantedTimeout = %v, want 1h", rec.GrantedTimeout)
	}
	if rec.GrantedInvocations != 3 {
		t.Errorf("GrantedInvocations = %d, want 3 (untouched — none used)", rec.GrantedInvocations)
	}
	if rec.GrantedCostUSD != 10 {
		t.Errorf("GrantedCostUSD = %v, want 10 (untouched)", rec.GrantedCostUSD)
	}
	if rec.GrantedPromptsPerInvocation != 1 {
		t.Errorf("GrantedPromptsPerInvocation = %d, want 1 (untouched)", rec.GrantedPromptsPerInvocation)
	}
}

// A flag naming a collateral axis is now in scope — it sets that axis's
// headroom instead of being refused.
func TestGrantPark_FlagSetsCollateralAxisHeadroom(t *testing.T) {
	env := newParkGrantEnv(t)
	ctx := context.Background()
	for range 3 {
		if err := env.be.BumpInvocations(ctx, env.claim, "plan"); err != nil {
			t.Fatalf("BumpInvocations: %v", err)
		}
	}
	env.park(t, budgetExhausted("plan", flow.AxisTimeout))

	if code := env.grant("--invocations", "5"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.rec(t, "plan").GrantedInvocations; got != 8 {
		t.Errorf("GrantedInvocations = %d, want 8 (3 used + 5 headroom)", got)
	}
}

// ...but an axis that is neither parked nor blocked is still refused.
func TestGrantPark_RejectsFlagForUnblockedAxis(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, budgetExhausted("plan", flow.AxisTimeout))

	if code := env.grant("--cost", "5"); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(env.err.String(), "does not apply") {
		t.Errorf("stderr = %q, want 'does not apply'", env.err.String())
	}
	if got := env.rec(t, "plan").GrantedCostUSD; got != 10 {
		t.Errorf("GrantedCostUSD = %v, want 10 (unchanged)", got)
	}
}

// An explicit zero on a collateral axis is refused rather than silently
// producing a grant that re-parks on that axis.
func TestGrantPark_RejectsExplicitZeroOnCollateralAxis(t *testing.T) {
	env := newParkGrantEnv(t)
	ctx := context.Background()
	for range 3 {
		if err := env.be.BumpInvocations(ctx, env.claim, "plan"); err != nil {
			t.Fatalf("BumpInvocations: %v", err)
		}
	}
	env.park(t, budgetExhausted("plan", flow.AxisTimeout))

	if code := env.grant("--invocations", "0"); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(env.err.String(), "would grant nothing") {
		t.Errorf("stderr = %q, want 'would grant nothing'", env.err.String())
	}
	if got := env.rec(t, "plan").GrantedTimeout; got != 30*time.Minute {
		t.Errorf("GrantedTimeout = %v, want 30m (unchanged — the refusal writes nothing)", got)
	}
}

// Grants are carried in whole seconds, so the headroom for a step budget below
// one second must be floored rather than truncated: a grant of zero would
// report success and leave the step parked at the same deadline.
func TestTimeoutHeadroom_FloorsSubSecondAndUnsetBudgets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
		want   int64
	}{
		{"unset", 0, minTimeoutHeadroom},
		{"sub-second", 50 * time.Millisecond, minTimeoutHeadroom},
		{"whole second", time.Second, 1},
		{"typical step", 30 * time.Minute, 1800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := timeoutHeadroom(flow.StepBudget{Timeout: tc.budget})
			if got != tc.want {
				t.Errorf("timeoutHeadroom(%v) = %d, want %d", tc.budget, got, tc.want)
			}
		})
	}
}
