package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// parkGrantEnv is the scaffolding for the identity/park tests: a flow with two
// artifact steps (whose labels differ from their ids — the whole point) and one
// signal step, under an explicit cap policy.
type parkGrantEnv struct {
	app   *App
	be    *fake.Orchestrator
	claim flow.Claim
	out   *bytes.Buffer
	err   *bytes.Buffer
}

func newParkGrantEnv(t *testing.T) *parkGrantEnv {
	t.Helper()
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Next("commit", "the plan is written").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, Next: []flow.StepId{"commit"}})
		f.AddStep("record the commit", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Next("pr-open", "the change is committed").CommitHash("abc"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Next: []flow.StepId{"pr-open"}})
		f.AddSignalStep("create pull request", "pr-open", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "the change is proposed"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, &stubAgent{name: "stub"})

	// The cap the sweep and the increment are computed against — the binary's
	// policy, which is what `grant` reads for a step's headroom.
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {
		MaxInvocations:          3,
		MaxPromptsPerInvocation: 1,
		MaxCostUSD:              10,
		Timeout:                 30 * time.Minute,
	}}

	env := &parkGrantEnv{app: app, be: be, claim: claim, out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	app.Out, app.Err = env.out, env.err
	return env
}

// budget is what the step may actually spend: the binary's policy plus every
// extension recorded on the step's ledger row. Nothing seeds caps onto an item
// any more, so this is what a `grant` moves and what every assertion below
// reads — through the same flow.EffectiveBudget the dispatch gate uses, so the
// test cannot pass against an arithmetic the runner does not share.
func (e *parkGrantEnv) budget(t *testing.T, id flow.StepId) flow.StepBudget {
	t.Helper()
	st, err := e.be.Load(context.Background(), e.claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return e.app.effectiveBudget(st, id)
}

// dispatches records n dispatches of a step, which is what "used n invocations"
// means now that the counter lives on the ledger row.
func (e *parkGrantEnv) dispatches(t *testing.T, id flow.StepId, n int) {
	t.Helper()
	for range n {
		if err := e.be.RecordDispatch(context.Background(), e.claim.ItemRef, id); err != nil {
			t.Fatalf("RecordDispatch(%s): %v", id, err)
		}
	}
}

// spend records cost against a step's ledger row.
func (e *parkGrantEnv) spend(t *testing.T, id flow.StepId, usd float64) {
	t.Helper()
	if err := e.be.AddCost(context.Background(), e.claim.ItemRef, id, usd); err != nil {
		t.Fatalf("AddCost(%s): %v", id, err)
	}
}

func (e *parkGrantEnv) park(t *testing.T, req flow.ParkRequest) {
	t.Helper()
	if err := e.be.Park(context.Background(), e.claim.ItemRef, req); err != nil {
		t.Fatalf("Park: %v", err)
	}
}

func (e *parkGrantEnv) parked(t *testing.T) *flow.ParkRequest {
	t.Helper()
	st, err := e.be.Load(context.Background(), e.claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return st.Park
}

func (e *parkGrantEnv) grant(args ...string) int {
	return e.app.cmdGrant(context.Background(), args)
}

// treasurerRefused is the park RunOne writes when a step exhausts an axis.
//
// It carries the run's own snapshot of every axis, exactly as RunOne's does:
// that snapshot is the record of the cap that was refused on, and
// flow.GrantClearsPark reads it because an orchestrator holds no policy. A park
// written without one can never be cleared, which is a state no real run
// produces.
func treasurerRefused(step flow.StepId, axis flow.BudgetAxis) flow.ParkRequest {
	// The caps are newParkGrantEnv's policy for "plan". Only the named axis is
	// flat; the others carry headroom, which is the ordinary shape — a park
	// where every axis is exhausted is a different case, and the tests about it
	// build their own snapshot.
	caps := map[flow.BudgetAxis]float64{
		flow.AxisInvocations: 3,
		flow.AxisPrompts:     1,
		flow.AxisCost:        10,
		flow.AxisTimeout:     (30 * time.Minute).Seconds(),
	}
	var axes []flow.AxisReport
	for _, a := range []flow.BudgetAxis{flow.AxisInvocations, flow.AxisPrompts, flow.AxisCost, flow.AxisTimeout} {
		used := float64(0)
		if a == axis {
			used = caps[a]
		}
		axes = append(axes, flow.NewAxisReport(a, used, caps[a]))
	}
	return flow.ParkRequest{
		Kind: flow.ParkTreasurerRefused, Step: step, Axis: axis, Reason: "test park",
		Axes: axes,
	}
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
	if got := env.budget(t, "plan").MaxInvocations; got != 3 {
		t.Errorf("MaxInvocations = %d, want 3 (unchanged — a refusal must not write)", got)
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

// A step the FLOW declares is always a grant target, whether or not the item
// has spent anything on it: nothing seeds caps onto an item any more, so
// "recorded" is no longer a precondition for raising one. The cap it lands on
// is the binary's policy plus the grant.
func TestGrantTarget_AcceptsADeclaredStepWithNoLedgerRow(t *testing.T) {
	env := newParkGrantEnv(t)

	// Nothing has been dispatched: the step has no row at all.
	st, err := env.be.Load(context.Background(), env.claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, recorded := st.Ledger.Steps["plan"]; recorded {
		t.Fatal("the step already has a ledger row; this test needs one with none")
	}

	if code := env.grant("plan", "--invocations", "1"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.budget(t, "plan").MaxInvocations; got != 4 {
		t.Errorf("MaxInvocations = %d, want 4 (3 policy + 1 granted)", got)
	}
}

// A step the flow no longer declares but the item has a LEDGER ROW for is still
// real spend, so the grant lands — with a warning rather than a refusal.
func TestGrantTarget_RecordedButNoLongerInFlow(t *testing.T) {
	env := newParkGrantEnv(t)
	// A row for an id the flow does not declare: the flow source moved on while
	// this item was mid-flight.
	env.dispatches(t, "coverage", 1)

	if code := env.grant("coverage", "--invocations", "2"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if !strings.Contains(env.err.String(), "no longer part of flow") {
		t.Errorf("stderr = %q, want the stale-step warning", env.err.String())
	}
	// The default policy plus the grant: an undeclared step has no policy of
	// its own, so it takes the package defaults.
	if want := flow.DefaultStepBudget().MaxInvocations + 2; env.budget(t, "coverage").MaxInvocations != want {
		t.Errorf("MaxInvocations = %d, want %d", env.budget(t, "coverage").MaxInvocations, want)
	}
}

// ---------------------------------------------------------------------------
// Park-driven default
// ---------------------------------------------------------------------------

func TestGrantPark_InvocationsAxis(t *testing.T) {
	env := newParkGrantEnv(t)
	env.dispatches(t, "plan", 3)
	env.park(t, treasurerRefused("plan", flow.AxisInvocations))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	// used(3) + headroom(1) = 4.
	if got := env.budget(t, "plan").MaxInvocations; got != 4 {
		t.Errorf("MaxInvocations = %d, want 4", got)
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
	env.spend(t, "plan", 12.40)
	env.park(t, treasurerRefused("plan", flow.AxisCost))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	// spent(12.40) + the step's own $10 cap = 22.40.
	if got := env.budget(t, "plan").MaxCostUSD; got != 22.40 {
		t.Errorf("MaxCostUSD = %v, want 22.40", got)
	}
	if p := env.parked(t); p != nil {
		t.Errorf("park = %+v, want cleared", p)
	}
}

func TestGrantPark_TimeoutAxisAddsOneMoreRun(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, treasurerRefused("plan", flow.AxisTimeout))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.budget(t, "plan").Timeout; got != time.Hour {
		t.Errorf("Timeout = %v, want 1h (30m policy + 30m granted)", got)
	}
}

func TestGrantPark_PromptsAxisRaisesTheCap(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, treasurerRefused("plan", flow.AxisPrompts))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	// The seeded cap is 1; a bare grant raises it by the default headroom.
	// Asserting against the constant rather than a literal keeps this test
	// about the behavior — the cap goes UP — and not about the tuning.
	want := 1 + defaultPromptHeadroom
	if got := env.budget(t, "plan").MaxPromptsPerInvocation; got != want {
		t.Errorf("MaxPromptsPerInvocation = %d, want %d", got, want)
	}
}

// The most valuable refusal: granting budget cannot clear a question park, so
// bare `grant` must say what will, and write nothing.
func TestGrantPark_RefusesNonBudgetPark(t *testing.T) {
	env := newParkGrantEnv(t)
	if _, err := askAll(env.be, context.Background(), env.claim, []flow.AgentQuestion{
		flow.AskText("which base branch?", "main, or the release branch?"),
	}); err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}
	env.park(t, flow.ParkRequest{Kind: flow.ParkQuestion, Step: "plan", Reason: "question pending"})

	code := env.grant()
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	// The header is the scannable form, so it is what the refusal names.
	for _, want := range []string{"not a budget cap", "which base branch?", "Answer the question"} {
		if !strings.Contains(env.err.String(), want) {
			t.Errorf("stderr = %q, want %q", env.err.String(), want)
		}
	}
	if got := env.budget(t, "plan").MaxInvocations; got != 3 {
		t.Errorf("MaxInvocations = %d, want 3 (unchanged)", got)
	}
}

// A question's Text is where a whole fenced evidence block belongs, and the
// refusal is one line. Without a header the message takes the first line of
// the text, not the block.
func TestGrantPark_QuestionRefusalStaysOneLine(t *testing.T) {
	env := newParkGrantEnv(t)
	if _, err := askAll(env.be, context.Background(), env.claim, []flow.AgentQuestion{
		{Text: "should the doc be amended?\n\n```\nrelease.md §11 step 1 says `--yes` skips\n```"},
	}); err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}
	env.park(t, flow.ParkRequest{Kind: flow.ParkQuestion, Step: "plan", Reason: "question pending"})

	if code := env.grant(); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(env.err.String(), "should the doc be amended?") {
		t.Errorf("stderr = %q, want the question's first line", env.err.String())
	}
	if strings.Contains(env.err.String(), "release.md §11") {
		t.Errorf("stderr = %q, want the evidence block left out of the one-line message", env.err.String())
	}
}

// A ParkRefused park is not a budget exhaustion — granting budget would not
// unpark it. The grant command must refuse and print the refusal-specific
// remedy so the operator knows the park is deterministic and that its reason
// names the cause to change, not a cap to raise.
func TestGrantPark_RefusesRefusedPark(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, flow.ParkRequest{
		Kind:   flow.ParkRefused,
		Step:   "plan",
		Reason: "guard refused staged file main.go",
	})

	code := env.grant()
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	for _, want := range []string{"not a budget cap", "deterministic", "reason"} {
		if !strings.Contains(env.err.String(), want) {
			t.Errorf("stderr = %q, want %q", env.err.String(), want)
		}
	}
	if got := env.budget(t, "plan").MaxInvocations; got != 3 {
		t.Errorf("MaxInvocations = %d, want 3 (unchanged — no budget written)", got)
	}
}

// The refused remedy does not carry the fix: it sends the operator to the
// park's reason and names `status` as where that is printed. That is a remedy
// only if the reason a mis-declared step leaves behind actually reaches
// `status`, so the journey is run whole — a real step declaring Prompts: none
// parks, `grant` refuses and points, `status` shows the instruction. The park
// comes from the chokepoint and not from a hand-written ParkRequest, so the
// reason `status` prints is the one the dispatch recorded: a park persisted
// without its reason, or a parked line that drops it, would leave the operator
// where the old remedy did — told to look somewhere that says nothing.
func TestGrantPark_RefusedRemedyPointsAtAReasonStatusPrints(t *testing.T) {
	agent := &stubAgent{name: "stub"}
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("open branch", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return flow.StepResult{}, promptFromMechanicalStep(ctx)
		}, mechanicalStepConfig())
	}, agent)
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	app.Out, app.Err = out, errOut

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusParked) || res.Park == nil || res.Park.Kind != flow.ParkRefused {
		t.Fatalf("res = %+v, want parked %q", res, flow.ParkRefused)
	}

	// grant refuses, and the refusal says where the reason is.
	if code := app.cmdGrant(context.Background(), nil); code != 2 {
		t.Fatalf("grant exit = %d, want 2; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "status") {
		t.Errorf("grant stderr = %q, want it to name `status` as where the park's reason is printed", errOut.String())
	}

	// The parked line carries the reason, and the reason carries the
	// instruction the remedy promised.
	out.Reset()
	if code := app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("status exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	var parkedLine string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "parked: ") {
			parkedLine = line
		}
	}
	if parkedLine == "" {
		t.Fatalf("status printed no parked line:\n%s", out.String())
	}
	for _, want := range []string{string(flow.ParkRefused), `"plan"`, "should not declare none"} {
		if !strings.Contains(parkedLine, want) {
			t.Errorf("status parked line = %q, want it to carry %q — the remedy sent the operator here for it", parkedLine, want)
		}
	}

	// The machine form carries the same reason, unclipped.
	out.Reset()
	if code := app.cmdStatus(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("status --json exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	var payload statusPayload
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if payload.Park == nil || payload.Park.Reason != res.Park.Reason {
		t.Errorf("status --json park = %+v, want reason %q — the one the dispatch parked with", payload.Park, res.Park.Reason)
	}
}

func TestGrantPark_StaleParkOnCompletedStep(t *testing.T) {
	env := newParkGrantEnv(t)
	appendMarkdown(t, env.be, env.claim.ItemRef, "plan", "commit", "done")
	// Park recorded AFTER the step completed — a record that outlived its
	// reason. The CLI must notice, since the backend only clears a park when
	// the step completes or a grant satisfies it.
	env.park(t, treasurerRefused("plan", flow.AxisInvocations))

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
	if got := env.budget(t, "plan").MaxInvocations; got != 3 {
		t.Errorf("MaxInvocations = %d, want 3 (unchanged)", got)
	}
}

func TestGrantPark_StaleWhenHeadroomAlreadyExists(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, treasurerRefused("plan", flow.AxisInvocations))
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
	if err := env.be.AddCost(context.Background(), env.claim.ItemRef, "plan", 12); err != nil {
		t.Fatalf("AddCost: %v", err)
	}
	env.park(t, treasurerRefused("plan", flow.AxisCost))

	code := env.grant("--invocations", "5")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(env.err.String(), "does not apply") {
		t.Errorf("stderr = %q, want 'does not apply'", env.err.String())
	}
	if got := env.budget(t, "plan").MaxCostUSD; got != 10 {
		t.Errorf("MaxCostUSD = %v, want 10 (unchanged)", got)
	}
}

func TestGrantPark_FlagOverridesHeadroom(t *testing.T) {
	env := newParkGrantEnv(t)
	env.dispatches(t, "plan", 3)
	env.park(t, treasurerRefused("plan", flow.AxisInvocations))

	if code := env.grant("--invocations", "5"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.budget(t, "plan").MaxInvocations; got != 8 { // used(3) + 5
		t.Errorf("MaxInvocations = %d, want 8", got)
	}
}

// A park written before this version recorded the human label. Accepting it
// keeps items parked across the upgrade grantable.
func TestGrantPark_AcceptsLegacyLabelInParkRecord(t *testing.T) {
	env := newParkGrantEnv(t)
	env.dispatches(t, "plan", 3)
	env.park(t, treasurerRefused("write plan", flow.AxisInvocations))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.budget(t, "plan").MaxInvocations; got != 4 {
		t.Errorf("MaxInvocations = %d, want 4", got)
	}
}

// A grant too small to clear the cap must leave the park in place and say so —
// otherwise a tool grants a token amount and loops forever.
func TestGrant_TooSmallLeavesParkAndReportsIt(t *testing.T) {
	env := newParkGrantEnv(t)
	env.spend(t, "plan", 12.40)
	env.park(t, treasurerRefused("plan", flow.AxisCost))

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
	env.dispatches(t, "plan", 3)
	appendResult(t, env.be, env.claim.ItemRef, "commit", "next", 1,
		flow.ArtifactBody{Type: flow.ArtifactCommitHash, CommitHash: "abc"})
	before := env.budget(t, "commit").MaxInvocations

	if code := env.grant("--all"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.budget(t, "plan").MaxInvocations; got != 4 {
		t.Errorf("plan MaxInvocations = %d, want 4", got)
	}
	if got := env.budget(t, "commit").MaxInvocations; got != before {
		t.Errorf("commit MaxInvocations = %d, want %d (completed steps are skipped)", got, before)
	}
}

func TestGrantAll_NoOpWhenHeadroomExists(t *testing.T) {
	env := newParkGrantEnv(t)

	if code := env.grant("--all"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.budget(t, "plan").MaxInvocations; got != 3 {
		t.Errorf("MaxInvocations = %d, want 3 (untouched)", got)
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
	env.dispatches(t, "plan", 3)
	env.park(t, treasurerRefused("plan", flow.AxisInvocations))

	if code := env.grant("--dry-run"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.budget(t, "plan").MaxInvocations; got != 3 {
		t.Errorf("MaxInvocations = %d, want 3 (dry run must not write)", got)
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
	env.park(t, treasurerRefused("plan", flow.AxisInvocations)) // granted 3, used 0

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
	env.park(t, treasurerRefused("plan", flow.AxisPrompts))

	if code := env.grant("--prompts", "0"); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(env.err.String(), "would grant nothing") {
		t.Errorf("stderr = %q, want 'would grant nothing'", env.err.String())
	}
	if got := env.budget(t, "plan").MaxPromptsPerInvocation; got != 1 {
		t.Errorf("MaxPromptsPerInvocation = %d, want 1 (unchanged)", got)
	}
}

// A park naming a step with no budget record cannot be topped up, and the
// message must say that rather than surfacing a backend "not seeded" error.
func TestGrantPark_ParkOnUnseededStep(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, treasurerRefused("nonesuch", flow.AxisInvocations))

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
	env.dispatches(t, "plan", 3)
	env.park(t, treasurerRefused("plan", flow.AxisInvocations))

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
		if err := env.be.RecordDispatch(ctx, env.claim.ItemRef, "plan"); err != nil {
			t.Fatalf("RecordDispatch: %v", err)
		}
	}
	env.park(t, treasurerRefused("plan", flow.AxisTimeout))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	eff := env.budget(t, "plan")
	if eff.Timeout != time.Hour {
		t.Errorf("Timeout = %v, want 1h (30m policy + 30m granted)", eff.Timeout)
	}
	if eff.MaxInvocations != 4 {
		t.Errorf("MaxInvocations = %d, want 4 (3 used + 1 headroom)", eff.MaxInvocations)
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
		if err := env.be.RecordDispatch(ctx, env.claim.ItemRef, "plan"); err != nil {
			t.Fatalf("RecordDispatch: %v", err)
		}
	}
	if err := env.be.AddCost(ctx, env.claim.ItemRef, "plan", 12); err != nil {
		t.Fatalf("AddCost: %v", err)
	}
	env.park(t, treasurerRefused("plan", flow.AxisTimeout))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	eff := env.budget(t, "plan")
	if eff.Timeout != time.Hour {
		t.Errorf("Timeout = %v, want 1h", eff.Timeout)
	}
	if eff.MaxInvocations != 4 {
		t.Errorf("MaxInvocations = %d, want 4", eff.MaxInvocations)
	}
	// spent(12) + the step's own $10 cap = 22.
	if eff.MaxCostUSD != 22 {
		t.Errorf("MaxCostUSD = %v, want 22", eff.MaxCostUSD)
	}
}

// An axis with headroom is left alone: the top-up is targeted, not a sweep.
func TestGrantPark_LeavesAxesWithHeadroomAlone(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, treasurerRefused("plan", flow.AxisTimeout))

	if code := env.grant(); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	eff := env.budget(t, "plan")
	if eff.Timeout != time.Hour {
		t.Errorf("Timeout = %v, want 1h", eff.Timeout)
	}
	if eff.MaxInvocations != 3 {
		t.Errorf("MaxInvocations = %d, want 3 (untouched — none used)", eff.MaxInvocations)
	}
	if eff.MaxCostUSD != 10 {
		t.Errorf("MaxCostUSD = %v, want 10 (untouched)", eff.MaxCostUSD)
	}
	if eff.MaxPromptsPerInvocation != 1 {
		t.Errorf("MaxPromptsPerInvocation = %d, want 1 (untouched)", eff.MaxPromptsPerInvocation)
	}
}

// A flag naming a collateral axis is now in scope — it sets that axis's
// headroom instead of being refused.
func TestGrantPark_FlagSetsCollateralAxisHeadroom(t *testing.T) {
	env := newParkGrantEnv(t)
	ctx := context.Background()
	for range 3 {
		if err := env.be.RecordDispatch(ctx, env.claim.ItemRef, "plan"); err != nil {
			t.Fatalf("RecordDispatch: %v", err)
		}
	}
	env.park(t, treasurerRefused("plan", flow.AxisTimeout))

	if code := env.grant("--invocations", "5"); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.budget(t, "plan").MaxInvocations; got != 8 {
		t.Errorf("MaxInvocations = %d, want 8 (3 used + 5 headroom)", got)
	}
}

// ...but an axis that is neither parked nor blocked is still refused.
func TestGrantPark_RejectsFlagForUnblockedAxis(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, treasurerRefused("plan", flow.AxisTimeout))

	if code := env.grant("--cost", "5"); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(env.err.String(), "does not apply") {
		t.Errorf("stderr = %q, want 'does not apply'", env.err.String())
	}
	if got := env.budget(t, "plan").MaxCostUSD; got != 10 {
		t.Errorf("MaxCostUSD = %v, want 10 (unchanged)", got)
	}
}

// An explicit zero on a collateral axis is refused rather than silently
// producing a grant that re-parks on that axis.
func TestGrantPark_RejectsExplicitZeroOnCollateralAxis(t *testing.T) {
	env := newParkGrantEnv(t)
	ctx := context.Background()
	for range 3 {
		if err := env.be.RecordDispatch(ctx, env.claim.ItemRef, "plan"); err != nil {
			t.Fatalf("RecordDispatch: %v", err)
		}
	}
	env.park(t, treasurerRefused("plan", flow.AxisTimeout))

	if code := env.grant("--invocations", "0"); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(env.err.String(), "would grant nothing") {
		t.Errorf("stderr = %q, want 'would grant nothing'", env.err.String())
	}
	if got := env.budget(t, "plan").Timeout; got != 30*time.Minute {
		t.Errorf("Timeout = %v, want 30m (unchanged — the refusal writes nothing)", got)
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
