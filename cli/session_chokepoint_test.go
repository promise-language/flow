package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// ---------------------------------------------------------------------------
// The treasurer's THIRD chokepoint.
//
// Opening an agent session discards context the resolution has already paid for,
// so it is asked for and recorded rather than taken. What is refused is a
// DECISION — machinery starting over where the route declared nothing — and
// never a circumstance: a handle the substrate declined, or a backend with
// nowhere to keep one, was nobody's decision and is approved and counted
// (docs/resolution.md § The treasurer).
// ---------------------------------------------------------------------------

// wantSessions asserts the item's durable session counts. The whole struct, so
// a test that moved the wrong bucket cannot pass by naming only the one it
// expected to move.
func wantSessions(t *testing.T, be *fake.Orchestrator, claim flow.Claim, want flow.SessionCounts) {
	t.Helper()
	state, err := be.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Ledger.Sessions != want {
		t.Errorf("ledger sessions = %+v, want %+v", state.Ledger.Sessions, want)
	}
}

// The entry's session is the route's: nothing precedes it in this resolution,
// so the one conversation it opens is declared, counted, and exactly what the
// journal accounts for.
func TestSessionChokepoint_TheEntrysSessionIsDeclared(t *testing.T) {
	agent := &sessionAgent{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			// Twice: the second prompt resumes, so it opens nothing and the
			// count must not move for it.
			for range 2 {
				if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
					return flow.StepResult{}, err
				}
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	wantSessions(t, be, claim, flow.SessionCounts{Declared: 1})
}

// A step declaring `fresh` opens a second, and it is the ROUTE's: independence
// is a property of the graph, not a spending decision, so the treasurer records
// it, counts it, and approves it. The continuing steps around it open nothing.
func TestSessionChokepoint_ADeclaredFreshStepIsApprovedAndCounted(t *testing.T) {
	agent := &sessionAgent{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", promptingStep("implementation", asPlan),
			flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
				Next: []flow.StepId{"implementation"}})
		// The step the declaration exists for: a reviewer holding the reasoning
		// that produced what it judges is not reviewing.
		f.AddStep("review the work", "implementation", promptingStep("commit", asPatch),
			flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor",
				Session: flow.SessionFresh, Next: []flow.StepId{"commit"}})
		f.AddStep("record the commit", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").CommitHash("abc1234"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor",
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	runSteps(t, app, claim, 3)
	wantSessions(t, be, claim, flow.SessionCounts{Declared: 2})

	// And the count is exactly what the route accounts for, so nothing is in
	// excess: the two numbers a `status` flag compares agree.
	state, err := be.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := state.Ledger.Sessions.Opened(), app.Flow.SessionsAccountedFor(state); got != want {
		t.Errorf("opened %d sessions, route accounts for %d — want them equal", got, want)
	}
}

// A MECHANICAL `fresh` step prompts nothing, so nothing is opened in its
// dispatch and nothing is counted for it. The next prompting step opens the one
// conversation its declaration called for. Counting at the declaration rather
// than at the opening would bill a session that was never bought — and on a
// route that finalizes right after, never would be.
func TestSessionChokepoint_AMechanicalFreshStepCountsNothingUntilSomethingPrompts(t *testing.T) {
	agent := &sessionAgent{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", promptingStep("implementation", asPlan),
			flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
				Next: []flow.StepId{"implementation"}})
		// Not prompting and not being a boundary are different facts.
		f.AddStep("open the branch", "implementation", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Next("commit", "on").Patch(flow.PatchBody{
				Diff: []byte("diff --git a/x b/x\n"), BaseBranch: "main",
			}), nil
		}, flow.StepConfig{Prompts: flow.PromptsNone, Role: "contributor",
			Session: flow.SessionFresh, Next: []flow.StepId{"commit"}})
		f.AddStep("record the commit", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").CommitHash("abc1234"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor",
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	// After the mechanical step: the entry's one, and nothing for a dispatch
	// that sent no prompt.
	runSteps(t, app, claim, 2)
	wantSessions(t, be, claim, flow.SessionCounts{Declared: 1})

	runSteps(t, app, claim, 1)
	wantSessions(t, be, claim, flow.SessionCounts{Declared: 2})
}

// A `fresh` step that parks and is re-dispatched RESUMES the session it opened.
// `fresh` is a property of one execution, so the chokepoint is never reached a
// second time and nothing is counted twice.
func TestSessionChokepoint_AParkedFreshStepCountsOnce(t *testing.T) {
	agent := &sessionAgent{}
	dispatches := 0
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", promptingStep("implementation", asPlan),
			flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
				Next: []flow.StepId{"implementation"}})
		f.AddStep("review the work", "implementation", func(ctx flow.StepCtx) (flow.StepResult, error) {
			dispatches++
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			if dispatches == 1 {
				return flow.StepResult{}, ctx.Park(flow.ParkRequest{
					Kind: flow.ParkBlocked, Reason: "stopping here to reach a second dispatch",
				})
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Patch(flow.PatchBody{
				Diff: []byte("diff --git a/x b/x\n"), BaseBranch: "main",
			}), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor",
			Session: flow.SessionFresh, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	runSteps(t, app, claim, 1)
	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "parked" {
		t.Fatalf("RunOne = (%+v, %v), want parked", res, err)
	}
	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	wantSessions(t, be, claim, flow.SessionCounts{Declared: 2})
}

// A ROUTE CROSSING THE DECLARING STEP TWICE asks for one conversation per
// crossing, and the graph could never have said how many: the count and what the
// journal accounts for move together.
func TestSessionChokepoint_AFreshStepReachedTwiceCountsTwice(t *testing.T) {
	agent := &sessionAgent{}
	reviews := 0
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", promptingStep("implementation", asPlan),
			flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
				Next: []flow.StepId{"implementation"}})
		f.AddStep("review the work", "implementation", func(ctx flow.StepCtx) (flow.StepResult, error) {
			reviews++
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			body := flow.PatchBody{Diff: []byte("diff --git a/x b/x\n"), BaseBranch: "main"}
			if reviews == 1 {
				// The rework handback: back to the entry, and round again.
				return ctx.Next("plan", "needs another pass").Patch(body), nil
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Patch(body), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor",
			Session: flow.SessionFresh, Next: []flow.StepId{"plan"},
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	runSteps(t, app, claim, 4)
	// The entry's one, plus one per crossing of the declaring step. The entry's
	// SECOND execution continues, so it adds nothing.
	wantSessions(t, be, claim, flow.SessionCounts{Declared: 3})

	state, err := be.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := state.Ledger.Sessions.Opened(), app.Flow.SessionsAccountedFor(state); got != want {
		t.Errorf("opened %d sessions, route accounts for %d — a route that asked for each of them is not an excess", got, want)
	}
}

// A BACKEND WITH NOWHERE TO KEEP A HANDLE opens one every dispatch. Nobody
// decided that, so every one past the route's is approved and counted as the
// handle having been gone — and the total runs past what the route accounts for,
// which is the excess the count exists to make visible. It is a limit of the
// backend, not a defect in the flow, and the bucket is what says which.
func TestSessionChokepoint_ABackendWithNoStoreCountsEveryOpeningAsHandleGone(t *testing.T) {
	agent := &sessionAgent{}
	dispatches := 0
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			dispatches++
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			if dispatches < 3 {
				return flow.StepResult{}, ctx.Park(flow.ParkRequest{
					Kind: flow.ParkBlocked, Reason: "stopping here to reach another dispatch",
				})
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)
	app.Orchestrator = noSessionBackend{app.Orchestrator}

	for _, want := range []string{"parked", "parked", "done"} {
		res, err := RunOne(context.Background(), app, claim)
		if err != nil || res.Status != want {
			t.Fatalf("RunOne = (%+v, %v), want %s — a missing store must not stop a step", res, err, want)
		}
	}
	// The first is the route's — the entry's own, which the journal accounts
	// for. The two after it are conversations nobody asked for.
	wantSessions(t, be, claim, flow.SessionCounts{Declared: 1, HandleGone: 2})

	state, err := be.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := state.Ledger.Sessions.Opened(), app.Flow.SessionsAccountedFor(state); got <= want {
		t.Errorf("opened %d sessions against a route accounting for %d — want an excess to flag", got, want)
	}
}

// A store that CANNOT ANSWER is the same circumstance by another route: the
// handle is gone, nobody chose that, and the dispatch carries on.
func TestSessionChokepoint_AStoreThatCannotAnswerIsHandleGone(t *testing.T) {
	agent := &sessionAgent{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			for range 2 {
				if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
					return flow.StepResult{}, err
				}
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)
	app.Orchestrator = sessionLoadFailsBackend{Orchestrator: be, err: errors.New("the store is on fire")}

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	// The read failed once and was memoised, so the first prompt opens the
	// entry's declared session and the second resumes it.
	wantSessions(t, be, claim, flow.SessionCounts{Declared: 1})
}

// A HANDLE THE SUBSTRATE DECLINED is a session opened after the fact. It is
// nobody's decision — by the time it is visible the context is already gone — so
// it is counted as the handle having been gone, and never refused.
func TestSessionChokepoint_ADeclinedHandleIsCountedAsHandleGone(t *testing.T) {
	agent := &sessionAgent{}
	declining := &decliningAgent{inner: agent}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			for range 3 {
				if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
					return flow.StepResult{}, err
				}
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)
	app.Agent = declining

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	// The entry's declared one, and the one the substrate opened when it
	// refused to resume it. The third prompt resumes what the second opened.
	wantSessions(t, be, claim, flow.SessionCounts{Declared: 1, HandleGone: 1})
}

// THE REFUSAL. Machinery discarding a live conversation the route does not
// account for is refused before the context is gone: the session is KEPT, the
// dispatch proceeds on it, and the attempt is recorded — no park, because the
// fix is in the graph or in the machinery and no operator could clear it.
func TestSessionChokepoint_AMachineryChosenDiscardIsRefusedAndTheSessionKept(t *testing.T) {
	agent := &sessionAgent{}
	tel := &recordingTelemetry{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			// Machinery deciding this moment is special. No handler can reach
			// this — which is the point of the write path being the guard — so
			// the test stands in for the future site that tries.
			discardTheSession(ctx)
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "more work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)
	app.Telemetry = tel

	res, err := RunOne(context.Background(), app, claim)
	if err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done — a refusal does not stop the resolution", res, err)
	}
	// The conversation survived the attempt: the prompt after it resumed what
	// the one before it opened.
	wantResume(t, agent, 1, "sess-1")
	if agent.minted != 1 {
		t.Errorf("the substrate opened %d sessions, want 1 — the refused discard threw one away anyway", agent.minted)
	}
	// One declared opening, and the refusal recorded beside it. The refusal
	// opened nothing, so it is not in Opened.
	wantSessions(t, be, claim, flow.SessionCounts{Declared: 1, Refused: 1})
	state, err := be.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Park != nil {
		t.Errorf("park = %+v, want none — parking would stop work over something no operator can clear", state.Park)
	}
	if got := state.Ledger.Sessions.Opened(); got != 1 {
		t.Errorf("Opened() = %d, want 1 — a refused request opened no session", got)
	}
	// And it is said out loud: continuing silently would leave it invisible,
	// which is what the record is for.
	var said bool
	for _, e := range tel.events {
		if strings.Contains(e.Detail, "refused to discard") {
			said = true
		}
	}
	if !said {
		t.Errorf("telemetry = %+v, want the refusal named", tel.events)
	}
}

// discardTheSession asks the chokepoint to throw the resolution's conversation
// away. Reaching through StepCtx is what makes this a test of the WRITE PATH
// rather than of one caller: every site that would discard a handle goes through
// setResolutionSession, and this stands in for the next one that tries.
func discardTheSession(ctx flow.StepCtx) {
	if s, ok := ctx.(*stepCtx); ok {
		s.setResolutionSession(flow.AgentSession{})
	}
}

// THE DECLARED DISCARD IS NOT REFUSED. The boundary a `fresh` step sets drops
// the handle through the same write path, and the guard must let it through —
// a guard that refused the one legitimate discard would sever nothing and
// silence everything.
func TestSessionChokepoint_ADeclaredDiscardIsNotRefused(t *testing.T) {
	agent := &sessionAgent{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", promptingStep("implementation", asPlan),
			flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
				Next: []flow.StepId{"implementation"}})
		f.AddStep("review the work", "implementation", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Patch(flow.PatchBody{
				Diff: []byte("diff --git a/x b/x\n"), BaseBranch: "main",
			}), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor",
			Session: flow.SessionFresh, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	runSteps(t, app, claim, 2)
	wantSessions(t, be, claim, flow.SessionCounts{Declared: 2})
	wantFresh(t, agent, 1) // the declared boundary did discard, as it must
}

// A SECOND DISCARD INSIDE ONE EXECUTION is refused on a `fresh` step like on any
// other. The declaration buys ONE conversation per execution and the boundary
// switch has already taken it; machinery asking for another on the same step is
// starting over, which is exactly what the chokepoint refuses — and a guard that
// read only the declaration would wave it through on the one step where the
// machinery is most likely to decide a moment is special.
func TestSessionChokepoint_ASecondDiscardOnAFreshStepIsRefused(t *testing.T) {
	agent := &sessionAgent{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", promptingStep("implementation", asPlan),
			flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
				Next: []flow.StepId{"implementation"}})
		f.AddStep("review the work", "implementation", func(ctx flow.StepCtx) (flow.StepResult, error) {
			// The declared boundary is already spent: this prompt opens the one
			// conversation `fresh` asked for.
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			discardTheSession(ctx)
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "more work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Patch(flow.PatchBody{
				Diff: []byte("diff --git a/x b/x\n"), BaseBranch: "main",
			}), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor",
			Session: flow.SessionFresh, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	runSteps(t, app, claim, 1)
	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done — a refusal does not stop the resolution", res, err)
	}
	// The step's own conversation survived the attempt: the prompt after the
	// discard resumed the one the declared boundary opened.
	wantResume(t, agent, 2, "sess-2")
	if agent.minted != 2 {
		t.Errorf("the substrate opened %d sessions, want 2 — the entry's and the declared one", agent.minted)
	}
	// The entry's and the declaration's, and the refusal beside them. Nothing in
	// HandleGone: the second discard opened nothing to file there.
	wantSessions(t, be, claim, flow.SessionCounts{Declared: 2, Refused: 1})
}

// A SIGNAL STEP'S PROMPT IS UNMETERED AND STILL COUNTED. Those steps carry no
// cap policy worth metering, so the turn passes through the chokepoint ungated —
// but a conversation it opened was still bought, and a spend nothing gates is
// exactly the one a count has to see. The record sits ABOVE the artifact split
// for that reason, and below the mechanical refusal for the opposite one.
func TestSessionChokepoint_ASignalStepsUnmeteredPromptIsCounted(t *testing.T) {
	agent := &sessionAgent{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		// Prompts nothing itself: the only conversation on this route is the one
		// the signal step opens, so the count cannot be the entry's by accident.
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Next("pr-open", "the plan is written").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			Next: []flow.StepId{"pr-open"}})
		f.AddSignalStep("create pull request", "pr-open", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "the change is proposed"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor",
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	runSteps(t, app, claim, 2)
	// The turn did go out: a count of zero would otherwise be right for the
	// wrong reason.
	if len(agent.reqs) != 1 {
		t.Fatalf("the agent saw %d requests, want 1 from the signal step", len(agent.reqs))
	}
	wantSessions(t, be, claim, flow.SessionCounts{Declared: 1})
}

// A SUBSTRATE THAT NAMES NO SESSION opens one every prompt: there is no handle
// to record, so nothing is carried from one prompt to the next inside a single
// dispatch. The route accounts for the first; each one after it is the handle
// having been gone, which is the excess the count exists to make visible.
//
// This is the one shape where the chokepoint classifies twice in a dispatch, and
// so the one that holds the in-dispatch mirror honest: a classification reading
// the count the item was LOADED with would call the second opening the route's
// too, and report two declared conversations on a route that declared one.
func TestSessionChokepoint_ASubstrateNamingNoSessionCountsEachPromptAfterTheFirst(t *testing.T) {
	// stubAgent answers without a SessionID, which is what a substrate with no
	// such notion does (docs/agent.md § AgentResponse).
	agent := &stubAgent{name: "nameless"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			for range 2 {
				if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
					return flow.StepResult{}, err
				}
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done — a substrate with no sessions is expensive, not broken", res, err)
	}
	// Both prompts went out with nothing to resume, which is what makes the
	// second one an opening rather than a continuation.
	for n, req := range agent.reqs {
		if req.ResumeSessionID != "" || !req.FreshSession {
			t.Errorf("request %d = (ResumeSessionID %q, FreshSession %v), want (\"\", true)",
				n, req.ResumeSessionID, req.FreshSession)
		}
	}
	wantSessions(t, be, claim, flow.SessionCounts{Declared: 1, HandleGone: 1})

	state, err := be.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := state.Ledger.Sessions.Opened(), app.Flow.SessionsAccountedFor(state); got <= want {
		t.Errorf("opened %d sessions against a route accounting for %d — want an excess to flag", got, want)
	}
}

// sessionRecordFailsBackend has a ledger that will not take the session count.
type sessionRecordFailsBackend struct {
	*fake.Orchestrator
	err error
}

func (b sessionRecordFailsBackend) RecordSession(context.Context, flow.ItemRef, flow.SessionReason) error {
	return b.err
}

// A ledger that cannot take the count costs the FIGURE, never the prompt. The
// count describes what a dispatch spent; failing the dispatch to protect it
// would spend the thing it was measuring.
func TestSessionChokepoint_ARecordFailureCostsTheCountNotTheStep(t *testing.T) {
	agent := &sessionAgent{}
	tel := &recordingTelemetry{}
	wantErr := errors.New("the ledger is on fire")
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)
	app.Orchestrator = sessionRecordFailsBackend{Orchestrator: be, err: wantErr}
	app.Telemetry = tel

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	if len(agent.reqs) != 1 {
		t.Errorf("the agent saw %d requests, want 1 — the turn still went out", len(agent.reqs))
	}
	wantSessions(t, be, claim, flow.SessionCounts{})
	var reported bool
	for _, e := range tel.events {
		if strings.Contains(e.Detail, wantErr.Error()) {
			reported = true
		}
	}
	if !reported {
		t.Errorf("telemetry = %+v, want the ledger's failure named — a count that was not kept is worth saying", tel.events)
	}
}

// A turn the infrastructure FAILED still opened a conversation where the
// substrate named one, and that opening is counted: the session holds everything
// the dead turn paid for, and the next dispatch resumes it rather than buying it
// again.
func TestSessionChokepoint_ATransientlyFailedTurnsSessionIsCountedOnce(t *testing.T) {
	agent := &sessionAgent{}
	failing := &failThenSucceedAgent{inner: agent}
	dispatches := 0
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			dispatches++
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, failing)

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "parked" {
		t.Fatalf("first RunOne = (%+v, %v), want parked on the transient failure", res, err)
	}
	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("second RunOne = (%+v, %v), want done", res, err)
	}
	// One conversation across both dispatches: the second resumed the one the
	// failed turn had already opened.
	wantSessions(t, be, claim, flow.SessionCounts{Declared: 1})
}

// A step declaring Prompts: none reaches nothing and LEAVES NO RECORD OF HAVING
// ASKED. The refusal lands before the request goes out, so there is no session
// to count — a mechanical step whose prompt was refused must not also be billed
// for a conversation that was never opened.
func TestSessionChokepoint_ARefusedMechanicalPromptCountsNoSession(t *testing.T) {
	agent := &sessionAgent{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("open the branch", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			_, _ = ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"})
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsNone, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "parked" {
		t.Fatalf("RunOne = (%+v, %v), want parked on the refused prompt", res, err)
	}
	if len(agent.reqs) != 0 {
		t.Errorf("the agent saw %d requests, want 0", len(agent.reqs))
	}
	wantSessions(t, be, claim, flow.SessionCounts{})
}

// ---------------------------------------------------------------------------
// `status` reports the count, and flags an excess.
//
// The excess is the signal and very nearly the only one: nothing else about a
// resolution that bought its context twice looks wrong afterwards. What the
// count does not say is WHY, which is what the breakdown beside it is for.
// ---------------------------------------------------------------------------

// recordSessions files n requests of one reason against the env's item.
func recordSessions(t *testing.T, env *parkGrantEnv, reason flow.SessionReason, n int) {
	t.Helper()
	for range n {
		if err := env.be.RecordSession(context.Background(), env.claim.ItemRef, reason); err != nil {
			t.Fatalf("RecordSession: %v", err)
		}
	}
}

func TestStatusJSON_SessionsKeySet(t *testing.T) {
	env := newParkGrantEnv(t)
	recordSessions(t, env, flow.SessionDeclared, 1)
	recordSessions(t, env, flow.SessionHandleGone, 2)
	recordSessions(t, env, flow.SessionRefused, 1)

	if code := env.app.cmdStatus(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("cmdStatus = %d; stderr=%q", code, env.err.String())
	}
	spend, ok := decode(t, env.out)["spend"].(map[string]any)
	if !ok {
		t.Fatalf("spend = %v, want an object — a session opened with no cost still reports", decode(t, env.out)["spend"])
	}
	s, ok := spend["sessions"].(map[string]any)
	if !ok {
		t.Fatalf("spend.sessions = %v, want an object", spend["sessions"])
	}
	for key, want := range map[string]float64{
		"opened": 3, "declared": 1, "handle_gone": 2, "refused": 1, "expected": 1,
	} {
		got, present := s[key]
		if !present {
			t.Errorf("sessions payload missing %q: %v", key, s)
			continue
		}
		if got != want {
			t.Errorf("sessions.%s = %v, want %v", key, got, want)
		}
	}
	// No `excess` key: it is opened > expected, and a stored answer to a
	// question these two already answer is a second copy that can disagree.
	if _, present := s["excess"]; present {
		t.Errorf("sessions payload carries %q; the excess is derived from opened and expected", "excess")
	}
}

// An item that has opened none and refused none reports no session block: a
// zeroed count on an unstarted item reads as a measurement.
func TestStatusJSON_NoSessionsBlockBeforeAnyWereOpened(t *testing.T) {
	env := newParkGrantEnv(t)
	if code := env.app.cmdStatus(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("cmdStatus = %d; stderr=%q", code, env.err.String())
	}
	if spend := decode(t, env.out)["spend"]; spend != nil {
		t.Errorf("spend = %v, want null on an item that has spent nothing and opened nothing", spend)
	}
}

func TestStatusHuman_ReportsTheSessionCount(t *testing.T) {
	env := newParkGrantEnv(t)
	recordSessions(t, env, flow.SessionDeclared, 1)

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d; stderr=%q", code, env.err.String())
	}
	out := env.out.String()
	if !strings.Contains(out, "sessions: 1 opened (1 declared, 0 handle-gone)") {
		t.Errorf("status output does not report the session count:\n%s", out)
	}
	// Nothing in excess, so nothing accused.
	if strings.Contains(out, "more than the route accounts for") {
		t.Errorf("status flagged an excess on a count the route accounts for:\n%s", out)
	}
}

func TestStatusHuman_FlagsTheExcessAndTheRefusals(t *testing.T) {
	env := newParkGrantEnv(t)
	recordSessions(t, env, flow.SessionDeclared, 1)
	recordSessions(t, env, flow.SessionHandleGone, 2)
	recordSessions(t, env, flow.SessionRefused, 1)

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d; stderr=%q", code, env.err.String())
	}
	out := env.out.String()
	want := "sessions: 3 opened (1 declared, 2 handle-gone), 1 refused — 2 more than the route accounts for"
	if !strings.Contains(out, want) {
		t.Errorf("status output does not carry %q:\n%s", want, out)
	}
}

// A FINISHED ROUTE IS STILL JUDGED AGAINST ITSELF, and a finished resolution is
// exactly when an operator asks whether the item bought its conversation twice —
// nothing else about such a resolution looks wrong afterwards, and by then there
// is nothing left to watch it happen.
//
// What a route asks for is read off the JOURNAL, which a route that elected a
// disposition still has. Reading it off the ELIGIBLE step instead would lose the
// comparison at the end of every route, because a route that has ended has no
// eligible step.
func TestStatusJSON_AnEndedRouteStillReportsWhatItAccountsFor(t *testing.T) {
	env := newParkGrantEnv(t)
	// plan → commit → pr-open, which finalizes. Every step continues, so the
	// whole route accounts for the entry's one conversation.
	runSteps(t, env.app, env.claim, 3)
	recordSessions(t, env, flow.SessionDeclared, 1)
	recordSessions(t, env, flow.SessionHandleGone, 2)

	if code := env.app.cmdStatus(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("cmdStatus = %d; stderr=%q", code, env.err.String())
	}
	payload := decode(t, env.out)
	// The condition the choice of graph turns on: the route elected a
	// disposition, so there is no eligible step left to read one off.
	if got := payload["flow_state"]; got != "no-eligible-step" {
		t.Fatalf("flow_state = %v, want no-eligible-step — this test is about a route that ended", got)
	}
	spend, ok := payload["spend"].(map[string]any)
	if !ok {
		t.Fatalf("spend = %v, want an object", payload["spend"])
	}
	s, ok := spend["sessions"].(map[string]any)
	if !ok {
		t.Fatalf("spend.sessions = %v, want an object", spend["sessions"])
	}
	if got := s["opened"]; got != float64(3) {
		t.Errorf("sessions.opened = %v, want 3", got)
	}
	expected, present := s["expected"]
	if !present {
		t.Fatalf("sessions payload carries no %q on a route that ended: %v — the journal is still there to read it off", "expected", s)
	}
	if expected != float64(1) {
		t.Errorf("sessions.expected = %v, want 1 — the entry's, and every step after it continues", expected)
	}
}

// NO FLOW, NO COMPARISON. What a route asks for is read off a graph this binary
// would not have, and a zero there would read as "the route asked for none",
// which is an accusation rather than an absence.
func TestStatusJSON_NoFlowReportsTheCountWithoutAnExpectation(t *testing.T) {
	env := newParkGrantEnv(t)
	recordSessions(t, env, flow.SessionHandleGone, 2)
	// A type no flow in this binary accepts.
	env.app.Flow = flow.NewFlow("implement", []flow.ItemType{"something-else"})

	if code := env.app.cmdStatus(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("cmdStatus = %d; stderr=%q", code, env.err.String())
	}
	spend, ok := decode(t, env.out)["spend"].(map[string]any)
	if !ok {
		t.Fatalf("spend = %v, want an object", decode(t, env.out)["spend"])
	}
	s, ok := spend["sessions"].(map[string]any)
	if !ok {
		t.Fatalf("spend.sessions = %v, want an object", spend["sessions"])
	}
	if got := s["opened"]; got != float64(2) {
		t.Errorf("sessions.opened = %v, want 2 — the count is the ledger's whether or not a flow reads the route", got)
	}
	if _, present := s["expected"]; present {
		t.Errorf("sessions.expected = %v on an item no flow handles; want the key omitted", s["expected"])
	}
}
