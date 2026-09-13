package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// windowResetsAt is the instant the fixtures' refusal publishes — far enough
// ahead that nothing could mistake it for "now", and fixed so a park can be
// compared against it exactly.
var windowResetsAt = time.Date(2026, 9, 13, 18, 42, 0, 0, time.UTC)

// refusedTurn is what an agent returns when the substrate refused it for the
// account's spent allowance: the kind, the window, the instant — and a COST,
// which is the fixture's whole point. The substrate reports what the
// interrupted turn would have cost; nothing may bill it.
func refusedTurn(cost float64) flow.AgentResponse {
	at := windowResetsAt
	return flow.AgentResponse{
		CostUSD: cost,
		Failure: &flow.AgentFailure{
			Kind:      flow.FailureAccountExhausted,
			Transient: true,
			Window:    "five_hour",
			ClearsAt:  &at,
			Message:   "agent account allowance exhausted (five_hour window), resets 2026-09-13T18:42:00Z",
		},
	}
}

// promptingFlow is a one-step flow whose handler prompts once and returns
// whatever handle() makes of the result. Every test below differs only in that.
func promptingFlow(handle func(flow.StepCtx, *flow.AgentResponse, error) (flow.StepResult, error)) func(*flow.Flow) {
	return func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			resp, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p1"})
			return handle(ctx, resp, err)
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}
}

// surfacing is the canonical handler: prompt, and return the error the way
// every handler in the tree does.
func surfacing(ctx flow.StepCtx, _ *flow.AgentResponse, err error) (flow.StepResult, error) {
	if err != nil {
		return flow.StepResult{}, err
	}
	return ctx.Finalize(flow.DispositionResolved, "done").Markdown("never reached"), nil
}

// The gate that decides what an interrupted turn costs — and which no test
// covered at all. An allowance that refused to spend has not bought an
// attempt, so the ledger must not move by a cent.
func TestMeteredAgent_ARefusedTurnIsNotBilled(t *testing.T) {
	a := &stubAgent{name: "stub", responses: []flow.AgentResponse{refusedTurn(3.20)}}
	app, be, claim := testApp(t, promptingFlow(surfacing), a)

	ctx := context.Background()
	if _, err := RunOne(ctx, app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}

	st, err := be.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.Ledger.Row("plan").CostUSD; got != 0 {
		t.Errorf("CostUSD = %v, want 0: the refused turn reported $3.20 and none of it may be billed", got)
	}
}

// The whole report, on one result: the kind, the instant, the account, the
// classification — and a dispatch that was never counted. Today this is a
// failed step, `resolve` exits non-zero, and an unattended runner halts on a
// condition that clears by itself at a time the system was already handed.
func TestRunOne_AnExhaustedAccountParksWithTheInstantAndTheAccount(t *testing.T) {
	useStubAgentAccount(t, agentAccountRecord{Id: "uuid-1", Email: "pat@example.com"}, nil)
	a := &stubAgent{name: "stub", responses: []flow.AgentResponse{refusedTurn(3.20)}}
	app, be, claim := testApp(t, promptingFlow(surfacing), a)

	ctx := context.Background()
	res, err := RunOne(ctx, app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}

	if res.Status != string(flow.StatusParked) || res.Park == nil {
		t.Fatalf("res = %+v, want parked — a substrate that answers and refuses is not a failed step", res)
	}
	if res.Park.Kind != flow.ParkAccountExhausted {
		t.Fatalf("Kind = %q, want %q — not infra-transient: nothing about the infrastructure failed",
			res.Park.Kind, flow.ParkAccountExhausted)
	}
	if res.Park.ClearsAt == nil || !res.Park.ClearsAt.Equal(windowResetsAt) {
		t.Errorf("Park.ClearsAt = %v, want %v", res.Park.ClearsAt, windowResetsAt)
	}
	if res.ClearsAt == nil || !res.ClearsAt.Equal(windowResetsAt) {
		t.Errorf("result.ClearsAt = %v, want %v — the instant leaves on the RESULT, where a caller that never links this SDK reads it",
			res.ClearsAt, windowResetsAt)
	}
	if res.Park.Account != flow.AgentAccountId("uuid-1") {
		t.Errorf("Park.Account = %q, want the substrate's identifier", res.Park.Account)
	}
	if res.RedispatchMayClear == nil || !*res.RedispatchMayClear {
		t.Errorf("RedispatchMayClear = %v, want a present true: re-dispatch IS what clears this park", res.RedispatchMayClear)
	}
	// The reason names the condition, the window and the instant — what
	// docs/environment.md requires of a report, and what "agent failure" omits.
	for _, want := range []string{"five_hour", "2026-09-13T18:42:00Z"} {
		if !strings.Contains(res.Park.Reason, want) {
			t.Errorf("Reason = %q, want it to name %q", res.Park.Reason, want)
		}
	}

	st, err := be.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.Ledger.Row("plan").Dispatches; got != 0 {
		t.Errorf("Dispatches = %d, want 0: an allowance that refused to spend has not bought an attempt", got)
	}
}

// The park carries the opaque identifier and NOTHING readable. The record is
// published as an issue comment, and docs/disclosure.md closes account
// identifiers — a display name is never what a comparison is made on either.
func TestRunOne_TheParkCarriesTheIdentifierAndNeverTheEmail(t *testing.T) {
	useStubAgentAccount(t, agentAccountRecord{Id: "uuid-1", Email: "pat@example.com"}, nil)
	a := &stubAgent{name: "stub", responses: []flow.AgentResponse{refusedTurn(0)}}
	app, _, claim := testApp(t, promptingFlow(surfacing), a)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Park == nil {
		t.Fatalf("res = %+v, want parked", res)
	}
	if strings.Contains(string(res.Park.Account)+res.Park.Reason+res.Park.Details, "pat@example.com") {
		t.Errorf("the park carries the e-mail: %+v", res.Park)
	}
}

// An account this host cannot name still parks. The condition is real whether
// or not anybody could say whose allowance it is — and the park is scoped to
// nothing rather than to a synthesized key, which would compare equal to
// itself and eventually to something else.
func TestRunOne_AnUnidentifiedAccountStillParksScopedToNothing(t *testing.T) {
	useStubAgentAccount(t, agentAccountRecord{}, errors.New("nothing names the account"))
	a := &stubAgent{name: "stub", responses: []flow.AgentResponse{refusedTurn(0)}}
	app, _, claim := testApp(t, promptingFlow(surfacing), a)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusParked) || res.Park == nil ||
		res.Park.Kind != flow.ParkAccountExhausted {
		t.Fatalf("res = %+v, want an account-exhausted park", res)
	}
	if res.Park.Account != "" {
		t.Errorf("Account = %q, want empty: an unidentified account is not an identity", res.Park.Account)
	}
}

// Read off the CHOKEPOINT, not off the handler's error. A handler that
// swallowed the refusal, or re-wrapped it without %w, would otherwise report
// whatever it returned instead — and the declaration holds whatever the
// handler did with it.
func TestRunOne_AnExhaustedAccountParksThroughAHandlerThatSwallowsTheError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		handle func(flow.StepCtx, *flow.AgentResponse, error) (flow.StepResult, error)
	}{
		{
			name: "swallowed entirely",
			handle: func(ctx flow.StepCtx, _ *flow.AgentResponse, _ error) (flow.StepResult, error) {
				return ctx.Finalize(flow.DispositionResolved, "done").Markdown("as if nothing happened"), nil
			},
		},
		{
			name: "re-wrapped without %w",
			handle: func(_ flow.StepCtx, _ *flow.AgentResponse, err error) (flow.StepResult, error) {
				return flow.StepResult{}, errors.New("the agent said: " + err.Error())
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &stubAgent{name: "stub", responses: []flow.AgentResponse{refusedTurn(3.20)}}
			app, be, claim := testApp(t, promptingFlow(tc.handle), a)

			ctx := context.Background()
			res, err := RunOne(ctx, app, claim)
			if err != nil {
				t.Fatalf("RunOne: %v", err)
			}
			if res.Status != string(flow.StatusParked) || res.Park == nil ||
				res.Park.Kind != flow.ParkAccountExhausted {
				t.Fatalf("res = %+v, want an account-exhausted park whatever the handler did", res)
			}
			st, err := be.Load(ctx, claim.ItemRef)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := st.Ledger.Row("plan").Dispatches; got != 0 {
				t.Errorf("Dispatches = %d, want 0", got)
			}
		})
	}
}

// The arena KEEPS ITS LEASE across the wait. It is not idle: it holds the
// draft, the session and the worktree, and an arena released while it still
// holds a resolution's state has not been freed — the work has been destroyed
// and the next dispatch buys it again. Pinned rather than built: the park path
// touches nothing that would release it, and this is what keeps it that way.
func TestRunOne_TheClaimSurvivesAnAccountExhaustedPark(t *testing.T) {
	a := &stubAgent{name: "stub", responses: []flow.AgentResponse{refusedTurn(0)}}
	app, be, claim := testApp(t, promptingFlow(surfacing), a)

	ctx := context.Background()
	res, err := RunOne(ctx, app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusParked) {
		t.Fatalf("res = %+v, want parked", res)
	}
	held, err := be.LookupActiveClaim(ctx)
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if held == nil {
		t.Fatal("the arena's claim was released by the park: the draft, the session and the worktree go with it")
	}
	if held.ItemRef.Display != claim.ItemRef.Display {
		t.Errorf("the arena holds %q, want %q", held.ItemRef.Display, claim.ItemRef.Display)
	}
}

// An item parked on an exhausted account is still ADVANCEABLE: the owning
// arena picks it up and runs the step when the window has reset.
// blockedFromAdvancing stops only on waits-on-items, and this block is
// waits-on-condition — nobody must act, and there is nothing to go work.
func TestRunOne_AnAccountExhaustedParkDoesNotStopTheOwningArenaResuming(t *testing.T) {
	a := &stubAgent{name: "stub", responses: []flow.AgentResponse{refusedTurn(0)}}
	app, be, claim := testApp(t, promptingFlow(surfacing), a)

	ctx := context.Background()
	if _, err := RunOne(ctx, app, claim); err != nil {
		t.Fatalf("first RunOne: %v", err)
	}
	// The window has reset: the next turn is answered.
	a.responses = nil

	res, err := RunOne(ctx, app, claim)
	if err != nil {
		t.Fatalf("second RunOne: %v", err)
	}
	if res.Status == string(flow.StatusBlocked) {
		t.Fatalf("res = %+v, want the step to run: the park waits on a condition, not on items", res)
	}
	st, err := be.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.Ledger.Row("plan").Dispatches; got != 1 {
		t.Errorf("Dispatches = %d, want 1 — the resumed dispatch, and only it", got)
	}
}

// ---------------------------------------------------------------------------
// The pre-dispatch check
//
// Before a dispatch there is no refusal to read, so the account's published
// usage is the only source — and a window already exhausted withholds the
// dispatch rather than spending a turn to be refused.
// ---------------------------------------------------------------------------

// stubQuota installs a reading and reports how many times it was consulted.
func stubQuota(app *App, usage []windowUsage, err error) *int {
	reads := 0
	app.Quota = func() ([]windowUsage, error) {
		reads++
		return usage, err
	}
	return &reads
}

// futureReset is the reset a QUOTA READING publishes. Relative to now, where
// the substrate's own refusal above is a fixed instant: the pre-dispatch check
// reads a reading against the clock, because a window whose published reset has
// passed has reset whatever the figure beside it says. A fixed instant would
// stop being the condition under test the moment it went by.
var futureReset = time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

func exhaustedWindow(used float64) []windowUsage {
	return []windowUsage{{Label: "5h", Length: 5 * time.Hour, Used: used, ResetsAt: futureReset}}
}

// A host that has not yet dispatched must not learn this by spending: the
// handler is never called, no dispatch is counted, and the park carries the
// window's own reset.
func TestRunOne_AnAlreadyExhaustedWindowWithholdsTheDispatch(t *testing.T) {
	useStubAgentAccount(t, agentAccountRecord{Id: "uuid-1"}, nil)
	dispatched := false
	app, be, claim := testApp(t, promptingFlow(func(ctx flow.StepCtx, _ *flow.AgentResponse, _ error) (flow.StepResult, error) {
		dispatched = true
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("ran"), nil
	}), &stubAgent{name: "stub"})
	stubQuota(app, exhaustedWindow(1.0), nil)

	ctx := context.Background()
	res, err := RunOne(ctx, app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if dispatched {
		t.Error("the handler ran: a window already exhausted withholds the dispatch rather than spending a turn to be refused")
	}
	if res.Status != string(flow.StatusParked) || res.Park == nil ||
		res.Park.Kind != flow.ParkAccountExhausted {
		t.Fatalf("res = %+v, want an account-exhausted park", res)
	}
	if res.Park.ClearsAt == nil || !res.Park.ClearsAt.Equal(futureReset) {
		t.Errorf("ClearsAt = %v, want the exhausted window's own reset %v", res.Park.ClearsAt, futureReset)
	}
	if !strings.Contains(res.Park.Reason, "5h") {
		t.Errorf("Reason = %q, want it to name the window that refused", res.Park.Reason)
	}
	st, err := be.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.Ledger.Row("plan").Dispatches; got != 0 {
		t.Errorf("Dispatches = %d, want 0", got)
	}
}

// Everything that is not an exhausted window dispatches. A mechanical step
// spends nothing, so there is no allowance to check; a quota error is
// informational and the in-band refusal still classifies the turn; and a
// window with headroom is not the condition.
func TestRunOne_ThePreDispatchCheckWithholdsNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		usage    []windowUsage
		quotaErr error
		prompts  flow.PromptPolicy
		reads    int
	}{
		{
			// The reason the pacing wait skips one: no agent is dispatched, so
			// there is no allowance to spend and no refusal to pre-empt.
			name: "a mechanical step runs anyway", usage: exhaustedWindow(1.0),
			prompts: flow.PromptsNone, reads: 0,
		},
		{
			// A pre-check's correct behaviour where published usage is
			// unreachable, not a hole: the in-band refusal still classifies.
			name: "a quota error lets the dispatch proceed", quotaErr: errors.New("unreadable"),
			prompts: flow.PromptsAgent, reads: 1,
		},
		{
			name: "a window with headroom lets the dispatch proceed", usage: exhaustedWindow(0.99),
			prompts: flow.PromptsAgent, reads: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dispatched := false
			app, _, claim := testApp(t, func(f *flow.Flow) {
				f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
					dispatched = true
					return ctx.Finalize(flow.DispositionResolved, "done").Markdown("ran"), nil
				}, flow.StepConfig{Prompts: tc.prompts, Role: "contributor", Entry: true,
					MayFinalize: []flow.Disposition{flow.DispositionResolved}})
			}, &stubAgent{name: "stub"})
			reads := stubQuota(app, tc.usage, tc.quotaErr)

			res, err := RunOne(context.Background(), app, claim)
			if err != nil {
				t.Fatalf("RunOne: %v", err)
			}
			if !dispatched {
				t.Errorf("the step was withheld; res = %+v", res)
			}
			if *reads != tc.reads {
				t.Errorf("quota read %d times, want %d", *reads, tc.reads)
			}
		})
	}
}

// A reading whose window has ALREADY RESET is not the condition — the window
// it describes is over, and the reading is simply older than it.
//
// This is the ordinary case rather than a corner: a reading is served from the
// machine-wide cache for minutes after it was taken, so a driver that waits to
// the published instant and resumes there arrives inside exactly that
// interval. Withholding the dispatch would report a condition that has ended,
// with an instant already in the past — which tells that driver to come
// straight back and be told the same thing, for as long as the reading lives.
func TestRunOne_AWindowWhoseResetHasPassedDoesNotWithholdTheDispatch(t *testing.T) {
	dispatched := false
	app, _, claim := testApp(t, promptingFlow(func(ctx flow.StepCtx, _ *flow.AgentResponse, _ error) (flow.StepResult, error) {
		dispatched = true
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("ran"), nil
	}), &stubAgent{name: "stub"})
	stubQuota(app, []windowUsage{{
		Label: "5h", Length: 5 * time.Hour, Used: 1.0, ResetsAt: time.Now().Add(-time.Minute),
	}}, nil)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if !dispatched {
		t.Errorf("the dispatch was withheld against a window that has already reset; res = %+v", res)
	}
	if res.Park != nil {
		t.Errorf("Park = %+v, want none: the park would carry an instant in the past", res.Park)
	}
}

// Nil Quota is the field's contract: no reading of any kind. A binary that has
// not asked for the reading does not get the check either.
func TestRunOne_NoQuotaReaderMeansNoPreDispatchCheck(t *testing.T) {
	dispatched := false
	app, _, claim := testApp(t, promptingFlow(func(ctx flow.StepCtx, _ *flow.AgentResponse, _ error) (flow.StepResult, error) {
		dispatched = true
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("ran"), nil
	}), &stubAgent{name: "stub"})
	app.Quota = nil

	if _, err := RunOne(context.Background(), app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if !dispatched {
		t.Error("the step was withheld with no quota reader installed")
	}
}

// The two blockedness derivations agree: an exhausted account is
// waits-on-condition — nobody must act, and there is nothing addressable to go
// work. The GitHub orchestrator derives it from the label; the fake derives it
// from the park it holds, which is what this exercises.
func TestFakeOrchestrator_AnAccountExhaustedParkWaitsOnACondition(t *testing.T) {
	a := &stubAgent{name: "stub", responses: []flow.AgentResponse{refusedTurn(0)}}
	app, be, claim := testApp(t, promptingFlow(surfacing), a)

	ctx := context.Background()
	if _, err := RunOne(ctx, app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}

	st, err := be.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !st.Blocked {
		t.Fatal("an exhausted account does not block the item at all")
	}
	if st.BlockKind != flow.WaitsOnCondition {
		t.Errorf("BlockKind = %q, want %q: nobody must act and there is nothing to go work",
			st.BlockKind, flow.WaitsOnCondition)
	}
}

// End to end through `resolve`, which is the shape the defect was reported in:
// an unattended runner halts on a condition that clears by itself at a time the
// system was already handed. The run EXITS — a window may be hours or days from
// resetting and nothing is served by a process sitting in front of it — and it
// exits ZERO, because nothing failed. The claim stays with the arena across the
// wait, holding the draft, the session and the worktree.
func TestCmdResolve_AnExhaustedAccountExitsZeroAndKeepsTheClaim(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, _, errBuf := resolveTestAppPrompts(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		_, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p1"})
		if err != nil {
			return flow.StepResult{}, err
		}
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("never reached"), nil
	}, flow.PromptsAgent)
	app.Agent = &stubAgent{name: "stub", responses: []flow.AgentResponse{refusedTurn(3.20)}}

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0 — nothing failed; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "parked") {
		t.Errorf("the run did not report a park; got:\n%s", errBuf.String())
	}
	held, err := be.LookupActiveClaim(context.Background())
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if held == nil {
		t.Fatal("the arena's claim was released across the wait: the draft, the session and the worktree go with it")
	}
}

// An exhausted window is not PACED against — it parks, and the run exits.
//
// Pacing is on by default (`--pace-five-hour` 90), and its arithmetic answers
// a window already flat with the whole remainder of that window: without this
// the default `resolve` sits in front of the reset for hours, holding the
// arena, and never reaches the check that would have parked it. "A window may
// be hours or days from resetting, and nothing is served by a process sitting
// in front of it."
//
// The deadline is the assertion: a run that paced would still be asleep.
func TestCmdResolve_AnExhaustedWindowParksRatherThanPacingOutTheWindow(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, _, errBuf := resolveTestAppPrompts(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("ran"), nil
	}, flow.PromptsAgent)
	app.Quota = func() ([]windowUsage, error) {
		return []windowUsage{{
			Label: "5h", Length: 5 * time.Hour, Used: 1.0, ResetsAt: time.Now().Add(4 * time.Hour),
		}}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if code := app.cmdResolve(ctx, []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	out := errBuf.String()
	if strings.Contains(out, "resolve: pacing —") {
		t.Errorf("the run waited out a window it should have parked on; got:\n%s", out)
	}
	if !strings.Contains(out, "plan → parked") {
		t.Errorf("the run did not park on the exhausted window; got:\n%s", out)
	}
}

// With two windows flat, the allowance returns when the LATER one does. A park
// reporting the earlier instant hands a driver a time the system already knew
// was too early, which is the one thing clears_at exists to prevent — and the
// window it names must be the one that actually binds.
func TestRunOne_TwoExhaustedWindowsParkOnTheOneThatResetsLast(t *testing.T) {
	later := futureReset.Add(72 * time.Hour)
	app, _, claim := testApp(t, promptingFlow(surfacing), &stubAgent{name: "stub"})
	stubQuota(app, []windowUsage{
		{Label: "5h", Length: 5 * time.Hour, Used: 1.0, ResetsAt: futureReset},
		{Label: "7d", Length: 7 * 24 * time.Hour, Used: 1.0, ResetsAt: later},
	}, nil)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkAccountExhausted {
		t.Fatalf("res = %+v, want an account-exhausted park", res)
	}
	if res.Park.ClearsAt == nil || !res.Park.ClearsAt.Equal(later) {
		t.Errorf("ClearsAt = %v, want the later reset %v: the allowance is back when the LAST flat window is",
			res.Park.ClearsAt, later)
	}
	if !strings.Contains(res.Park.Reason, "7d") {
		t.Errorf("Reason = %q, want it to name the window that binds", res.Park.Reason)
	}
}

// A reading whose reset did not parse carries NO instant rather than the zero
// time. The epoch reads as an instant long past — it would tell a driver to
// re-dispatch immediately into the same refusal, which is worse than saying
// nothing.
func TestRunOne_AnUnparsedResetCarriesNoInstantRatherThanTheEpoch(t *testing.T) {
	app, _, claim := testApp(t, promptingFlow(surfacing), &stubAgent{name: "stub"})
	stubQuota(app, []windowUsage{{Label: "5h", Length: 5 * time.Hour, Used: 1.0}}, nil)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkAccountExhausted {
		t.Fatalf("res = %+v, want an account-exhausted park", res)
	}
	if res.Park.ClearsAt != nil {
		t.Errorf("ClearsAt = %v, want nil: an unreadable reset is no instant, not the epoch", res.Park.ClearsAt)
	}
	if res.ClearsAt != nil {
		t.Errorf("result.ClearsAt = %v, want absent", res.ClearsAt)
	}
	if strings.Contains(res.Park.Reason, "resets") {
		t.Errorf("Reason = %q, want no reset claimed when none could be read", res.Park.Reason)
	}
}

// The OTHER transient failure. Both kinds are Transient — that is the field
// that decides what is billed and counted, and they share it deliberately —
// so the kind is the only thing separating them, and it is read at exactly one
// place. A chokepoint that recorded every transient turn as an exhausted
// account would tell an operator a healthy runner's flap resets at an instant
// nobody published, and would file it under a label that says "do nothing".
func TestRunOne_AnOrdinaryTransientAgentFailureStillParksInfraTransient(t *testing.T) {
	a := &stubAgent{name: "stub", responses: []flow.AgentResponse{{
		Failure: &flow.AgentFailure{Kind: "no-result", Transient: true, Message: "runner flapped"},
	}}}
	app, _, claim := testApp(t, promptingFlow(surfacing), a)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkInfraTransient {
		t.Fatalf("res = %+v, want an infra-transient park: nothing said the allowance was spent", res)
	}
	// No instant, on either half of the report: infrastructure ends when it
	// ends, and an instant on a park that cannot know one is a fabrication.
	if res.Park.ClearsAt != nil || res.ClearsAt != nil {
		t.Errorf("park=%v result=%v, want no instant on a kind whose end nobody published",
			res.Park.ClearsAt, res.ClearsAt)
	}
}

// The substrate may refuse WITHOUT naming a window — the field is documented
// empty for exactly that — and the reset it published is still the fact a
// driver needs. The condition is the refusal, never the window: a report that
// needed both would drop the instant whenever the substrate named only one.
func TestRunOne_ARefusalThatNamesNoWindowStillReportsTheInstant(t *testing.T) {
	at := windowResetsAt
	a := &stubAgent{name: "stub", responses: []flow.AgentResponse{{
		Failure: &flow.AgentFailure{
			Kind: flow.FailureAccountExhausted, Transient: true, ClearsAt: &at,
			Message: "agent account allowance exhausted, resets 2026-09-13T18:42:00Z",
		},
	}}}
	app, _, claim := testApp(t, promptingFlow(surfacing), a)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkAccountExhausted {
		t.Fatalf("res = %+v, want an account-exhausted park", res)
	}
	if res.ClearsAt == nil || !res.ClearsAt.Equal(windowResetsAt) {
		t.Errorf("result.ClearsAt = %v, want the published instant %v", res.ClearsAt, windowResetsAt)
	}
	// The reason is read by a person, and it is assembled from parts that may
	// each be missing. Written whole rather than by substring, because what
	// goes wrong here is punctuation left behind by an absent part.
	if want := "agent account allowance exhausted — resets 2026-09-13T18:42:00Z"; res.Park.Reason != want {
		t.Errorf("Reason = %q, want %q", res.Park.Reason, want)
	}
}

// The instant is the fact the run's report is FOR. A driver or an operator
// reading "parked" alone has to go and find out when the allowance returns; the
// park already carries it, and the run that exits on it says so.
//
// And it exits after ONE dispatch. The classification says a re-dispatch may
// clear this kind, and the instant says when — so re-dispatching before it is
// looping against an answer the system was already handed, which is why the
// clears_at arm sits ahead of the bounded retry.
func TestCmdResolve_AnExhaustedAccountNamesTheInstantAndDispatchesOnce(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	dispatches := 0
	app, _, errBuf := resolveTestAppPrompts(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		dispatches++
		_, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p1"})
		if err != nil {
			return flow.StepResult{}, err
		}
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("never reached"), nil
	}, flow.PromptsAgent)
	app.Agent = &stubAgent{name: "stub", responses: []flow.AgentResponse{
		refusedTurn(3.20), refusedTurn(3.20), refusedTurn(3.20),
	}}

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if dispatches != 1 {
		t.Errorf("the step was dispatched %d time(s), want 1 — the window says when, and it is not now", dispatches)
	}
	out := errBuf.String()
	if !strings.Contains(out, windowResetsAt.Format(time.RFC3339)) {
		t.Errorf("the run did not name the instant the park carries (%s); got:\n%s",
			windowResetsAt.Format(time.RFC3339), out)
	}
	// What the exit is FOR: the arena keeps the draft, the session and the
	// worktree, so whatever returns at that instant resumes here.
	if !strings.Contains(out, "claim and this arena are kept") {
		t.Errorf("the run did not say the claim is held across the wait; got:\n%s", out)
	}
	if strings.Contains(out, "re-dispatching") {
		t.Errorf("the run re-dispatched against a window it was told the reset time of; got:\n%s", out)
	}
}
