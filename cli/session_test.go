package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// ---------------------------------------------------------------------------
// The agent session is the RESOLUTION's.
//
// A resolution is one conversation: its steps continue it, whether the next one
// runs in this process or in another `run-step` invocation on another day. A new
// session happens only where a step declares `fresh`, and nowhere else
// (docs/resolution.md § The agent session, docs/flow-registration.md § Session
// continuity).
//
// These tests assert the behaviour at the level that owns it — the chokepoint —
// rather than through any handler, because no handler is allowed to have an
// opinion about it.
// ---------------------------------------------------------------------------

// sessionAgent stands in for a substrate that honours the handle: a request
// naming a session answers with that same session, and a request naming none
// opens a new one and says which. Every request is kept, so a test can read
// exactly what the chokepoint stamped.
type sessionAgent struct {
	reqs   []flow.AgentRequest
	minted int
}

func (a *sessionAgent) Name() string { return "session-stub" }

func (a *sessionAgent) Run(ctx context.Context, req flow.AgentRequest) (*flow.AgentResponse, error) {
	a.reqs = append(a.reqs, req)
	id := req.ResumeSessionID
	if id == "" {
		a.minted++
		id = fmt.Sprintf("sess-%d", a.minted)
	}
	return &flow.AgentResponse{LastText: "ok", SessionID: id}, nil
}

// resumed reports what the nth request (0-based) carried, as a pair a failure
// message can print whole: an empty handle and FreshSession must travel
// together, and a test that asserted only one of them would pass on the
// combination that lets a substrate attach to whatever it last cached.
func (a *sessionAgent) resumed(t *testing.T, n int) (string, bool) {
	t.Helper()
	if n >= len(a.reqs) {
		t.Fatalf("the agent saw %d requests, want at least %d", len(a.reqs), n+1)
	}
	return a.reqs[n].ResumeSessionID, a.reqs[n].FreshSession
}

// wantResume asserts request n resumes the named session and does not ask for a
// clean slate.
func wantResume(t *testing.T, a *sessionAgent, n int, want string) {
	t.Helper()
	got, fresh := a.resumed(t, n)
	if got != want || fresh {
		t.Errorf("request %d = (ResumeSessionID %q, FreshSession %v), want (%q, false)", n, got, fresh, want)
	}
}

// wantFresh asserts request n opens a new session: no handle AND FreshSession,
// which is the pair that says "there is nothing to inherit" rather than "resume
// whatever you happen to have".
func wantFresh(t *testing.T, a *sessionAgent, n int) {
	t.Helper()
	got, fresh := a.resumed(t, n)
	if got != "" || !fresh {
		t.Errorf("request %d = (ResumeSessionID %q, FreshSession %v), want (\"\", true)", n, got, fresh)
	}
}

// promptingStep is a step that prompts once and completes, electing next with
// whatever payload its declared artifact type requires.
func promptingStep(next flow.StepId, payload func(flow.StepResult) flow.StepResult) flow.StepHandler {
	return func(ctx flow.StepCtx) (flow.StepResult, error) {
		if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
			return flow.StepResult{}, err
		}
		return payload(ctx.Next(next, "on")), nil
	}
}

func asPlan(r flow.StepResult) flow.StepResult { return r.Markdown("the plan") }
func asPatch(r flow.StepResult) flow.StepResult {
	return r.Patch(flow.PatchBody{Diff: []byte("diff --git a/x b/x\n"), BaseBranch: "main"})
}

// Two prompts inside one invocation chain, with no handler-held variable. This
// is what the implement step's fix rounds relied on a local `session` for; the
// local is gone and the behaviour is the chokepoint's.
func TestSession_TwoPromptsInOneInvocationChain(t *testing.T) {
	agent := &sessionAgent{}
	app, _, claim := testApp(t, func(f *flow.Flow) {
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
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	if len(agent.reqs) != 2 {
		t.Fatalf("agent saw %d requests, want 2", len(agent.reqs))
	}
	wantFresh(t, agent, 0)
	wantResume(t, agent, 1, "sess-1")
}

// THE DEFECT THIS ITEM IS TITLED FOR. A second dispatch of the same step — the
// park-and-resume case, and the separate `run-step` invocation case — continues
// the conversation the first one opened. A local variable could never do this:
// it dies with the invocation.
func TestSession_SurvivesToTheNextDispatch(t *testing.T) {
	agent := &sessionAgent{}
	dispatches := 0
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			dispatches++
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			if dispatches == 1 {
				return flow.StepResult{}, ctx.Park(flow.ParkRequest{
					Kind: flow.ParkBlocked, Reason: "stopping here to test the next dispatch",
				})
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "parked" {
		t.Fatalf("first RunOne = (%+v, %v), want parked", res, err)
	}
	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("second RunOne = (%+v, %v), want done", res, err)
	}
	wantFresh(t, agent, 0)
	wantResume(t, agent, 1, "sess-1")
}

// A step declaring `fresh` prompts with nothing inherited, and the session IT
// opens becomes the resolution's for whatever comes next. The declaration is a
// boundary, not an exemption: the conversation continues from there.
func TestSession_FreshStepOpensOneAndHandsItOn(t *testing.T) {
	agent := &sessionAgent{}
	app, _, claim := testApp(t, func(f *flow.Flow) {
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
			return ctx.Finalize(flow.DispositionResolved, "done").CommitHash("abc"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor",
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	runSteps(t, app, claim, 3)

	wantFresh(t, agent, 0)            // the entry: nothing precedes it
	wantFresh(t, agent, 1)            // the declaration: the plan's conversation is given up
	wantResume(t, agent, 2, "sess-2") // and the successor continues the reviewer's
	if agent.reqs[2].ResumeSessionID == "sess-1" {
		t.Error("the step after the boundary resumed the session `fresh` discarded")
	}
}

// `fresh` applies once per EXECUTION, not once per dispatch. A fresh step that
// prompts, parks and is re-dispatched resumes the session it opened: a resume
// that re-opened would be the machinery deciding this moment is special, which
// is exactly the session nobody declared.
func TestSession_FreshStepThatParksResumesItsOwnSession(t *testing.T) {
	agent := &sessionAgent{}
	dispatches := 0
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			dispatches++
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			if dispatches == 1 {
				return flow.StepResult{}, ctx.Park(flow.ParkRequest{
					Kind: flow.ParkBlocked, Reason: "stopping here so the step is dispatched twice",
				})
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			Session: flow.SessionFresh, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "parked" {
		t.Fatalf("first RunOne = (%+v, %v), want parked", res, err)
	}
	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("second RunOne = (%+v, %v), want done", res, err)
	}
	wantFresh(t, agent, 0)
	wantResume(t, agent, 1, "sess-1")
	if agent.minted > 1 {
		t.Errorf("the substrate opened %d sessions, want 1 — the re-dispatch honoured a boundary it had already honoured", agent.minted)
	}
}

// Once per execution means ONCE PER EXECUTION: a route that leaves the
// declaring step and comes back opens a second session, because that is a second
// execution. The rework handback is such a route in the shipped graph — `review
// the proposal` elects `implement the change`, which routes to `review the work`
// again — and the boundary honoured for the first execution must not still be
// standing when the second one starts. If it is, the second reviewer resumes the
// conversation that wrote the very changes it is judging, which is the one thing
// `fresh` is declared to prevent.
//
// It is also what the treasurer counts (docs/resolution.md § The treasurer): the
// journal accounts for one session per execution of a declaring step, so a route
// crossing it twice and opening one session is short by one against its own
// record.
func TestSession_FreshStepReachedTwiceOpensTwoSessions(t *testing.T) {
	agent := &sessionAgent{}
	judged := 0
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", promptingStep("implementation", asPlan),
			flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
				Next: []flow.StepId{"implementation"}})
		f.AddStep("review the work", "implementation", promptingStep("commit", asPatch),
			flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor",
				Session: flow.SessionFresh, Next: []flow.StepId{"commit"}})
		// The handback: the first judgement sends the route back through the
		// declaring step, the second lets it finish.
		f.AddStep("judge the proposal", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			judged++
			if judged == 1 {
				return ctx.Next("implementation", "rework").CommitHash("abc"), nil
			}
			return ctx.Finalize(flow.DispositionResolved, "done").CommitHash("abc"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor",
			Next:        []flow.StepId{"implementation"},
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	runSteps(t, app, claim, 5)

	wantFresh(t, agent, 0)            // the entry
	wantFresh(t, agent, 1)            // the declaration, first execution
	wantResume(t, agent, 2, "sess-2") // the judgement continues the reviewer's
	wantFresh(t, agent, 3)            // the declaration again: a SECOND execution
	wantResume(t, agent, 4, "sess-3")
}

// A MECHANICAL step may declare `fresh`: not prompting and not being a boundary
// are different facts. The declaration says where the conversation ends, and
// nothing requires the step that says so to be the one talking.
func TestSession_MechanicalFreshStepSeversTheSession(t *testing.T) {
	agent := &sessionAgent{}
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", promptingStep("implementation", asPlan),
			flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
				Next: []flow.StepId{"implementation"}})
		f.AddStep("start over", "implementation", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return asPatch(ctx.Next("commit", "on")), nil
		}, flow.StepConfig{Prompts: flow.PromptsNone, Role: "contributor",
			Session: flow.SessionFresh, Next: []flow.StepId{"commit"}})
		f.AddStep("record the commit", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").CommitHash("abc"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor",
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	runSteps(t, app, claim, 3)

	if len(agent.reqs) != 2 {
		t.Fatalf("agent saw %d requests, want 2 — the middle step prompts nothing", len(agent.reqs))
	}
	wantFresh(t, agent, 0)
	wantFresh(t, agent, 1)
}

// THE CONFLATION GUARD, at the level the mistake would be made. A mechanical
// step that declares nothing about the session carries it across untouched —
// this is `open branch` running between `plan` and `implement`, and the branch
// is opened precisely so the implementing conversation can continue where the
// planning one left off.
//
// An implementation that read "mechanical" as "no session" would sever the
// conversation here, invisibly, with every step still looking correctly
// declared.
func TestSession_MechanicalStepCarriesTheSessionAcross(t *testing.T) {
	agent := &sessionAgent{}
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", promptingStep("implementation", asPlan),
			flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
				Next: []flow.StepId{"implementation"}})
		f.AddStep("open branch", "implementation", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return asPatch(ctx.Next("commit", "on")), nil
		}, flow.StepConfig{Prompts: flow.PromptsNone, Role: "contributor", Next: []flow.StepId{"commit"}})
		f.AddStep("implement the change", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").CommitHash("abc"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor",
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	runSteps(t, app, claim, 3)

	wantFresh(t, agent, 0)
	wantResume(t, agent, 1, "sess-1")
}

// The entry step begins a new session whatever else is on the machine. What
// precedes it in the arena belongs to a DIFFERENT item, and an entry step that
// continued it would open every item holding another item's reasoning. Nothing
// entry-specific arranges this: the store is keyed by item, so there is nothing
// to find.
func TestSession_EntryStepDoesNotInheritAnotherItemsSession(t *testing.T) {
	agent := &sessionAgent{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	other := flow.Item{Ref: be.Ref("2"), Type: "task", Title: "another item"}
	be.AddItem("2", other)
	if err := be.SaveAgentSession(context.Background(), other.Ref,
		flow.AgentSession{SessionID: "another-items-conversation"}); err != nil {
		t.Fatalf("SaveAgentSession for the other item: %v", err)
	}

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	wantFresh(t, agent, 0)
}

// No step is exempt, including the one path that owns no artifact budget: a
// signal step's prompt passes through the metered wrapper unmetered and is
// still handed the session and still records what it opened. A path that
// stamped but did not record would resume correctly and then throw the
// conversation away, which is the defect wearing the fix's clothes.
func TestSession_SignalStepIsHandedItAndRecordsIt(t *testing.T) {
	agent := &sessionAgent{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddSignalStep("create pull request", "pr-open", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	wantFresh(t, agent, 0)
	got, err := be.LoadAgentSession(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("LoadAgentSession: %v", err)
	}
	if got.SessionID != "sess-1" {
		t.Errorf("session after the signal step = %+v, want the handle it opened kept for whatever comes next", got)
	}
}

// A result the disclosure guard refused is revised INSIDE the dispatch, and the
// revision round continues the conversation that composed the refused text.
// That is what makes the round cheap: the turn is amending a sentence it can
// still see, not re-deriving a plan it has forgotten. A fresh session here would
// buy the whole step's context over again and cost what the refusal cost.
func TestSession_TheRevisionRoundContinuesTheRefusedTurnsConversation(t *testing.T) {
	agent := &sessionAgent{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			wip, err := ctx.WorkInProgress()
			if err != nil {
				return flow.StepResult{}, err
			}
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			text := "the plan mentioning /home/someone/"
			if wip != "" {
				text = "the plan, revised"
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown(text), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)
	app.Orchestrator = &capturingBackend{Orchestrator: be, refusals: []error{refusedComment()}}

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done — the revision landed in this dispatch", res, err)
	}
	if len(agent.reqs) != 2 {
		t.Fatalf("the agent saw %d requests, want 2 — the refused turn and its revision", len(agent.reqs))
	}
	wantFresh(t, agent, 0)
	wantResume(t, agent, 1, "sess-1")
	if agent.minted > 1 {
		t.Errorf("the substrate opened %d sessions, want 1 — the revision started over instead of amending", agent.minted)
	}
}

// A turn that FAILED still opened a conversation, and the handle it named is
// kept. The next dispatch resumes it instead of buying the same context again:
// a transient failure suspends the cost and dispatch axes because a flapping
// runner never used them, and a substrate that named its session did use one.
func TestSession_ATransientlyFailedTurnStillKeepsItsHandle(t *testing.T) {
	agent := &sessionAgent{}
	failing := &failThenSucceedAgent{inner: agent}
	dispatches := 0
	app, _, claim := testApp(t, func(f *flow.Flow) {
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
	if dispatches != 2 {
		t.Fatalf("the step was dispatched %d times, want 2", dispatches)
	}
	wantFresh(t, agent, 0)
	wantResume(t, agent, 1, "sess-1")
	if agent.minted > 1 {
		t.Errorf("the substrate opened %d sessions, want 1 — the dead turn's conversation was thrown away and bought again", agent.minted)
	}
}

// failThenSucceedAgent turns the FIRST turn into a transient infra failure that
// still names its session: the conversation was opened, the turn did not
// finish. Every later turn is the inner agent's.
type failThenSucceedAgent struct {
	inner *sessionAgent
	turns int
}

func (a *failThenSucceedAgent) Name() string { return "fail-then-succeed" }

func (a *failThenSucceedAgent) Run(ctx context.Context, req flow.AgentRequest) (*flow.AgentResponse, error) {
	resp, err := a.inner.Run(ctx, req)
	a.turns++
	if a.turns == 1 && resp != nil {
		resp.Failure = &flow.AgentFailure{Kind: "no-result", Message: "the host interfered", Transient: true}
		resp.LastText = ""
	}
	return resp, err
}

// noSessionBackend refuses the agent-session store rather than lacking it —
// there are no optional capabilities, so "no store here" is an answer the method
// gives. On noWorkBackend's pattern, and for the same reason.
type noSessionBackend struct{ flow.Orchestrator }

func (noSessionBackend) SaveAgentSession(context.Context, flow.ItemRef, flow.AgentSession) error {
	return fmt.Errorf("this orchestrator keeps no agent-session store: %w", flow.ErrUnsupported)
}

func (noSessionBackend) LoadAgentSession(context.Context, flow.ItemRef) (flow.AgentSession, error) {
	return flow.AgentSession{}, fmt.Errorf("this orchestrator keeps no agent-session store: %w", flow.ErrUnsupported)
}

// A backend with nowhere to keep a handle is NOT INCORRECT, ONLY EXPENSIVE:
// every dispatch opens a session, every step starts from the prompt it was
// given, and nothing fails. The mechanism is absent rather than broken.
func TestSession_BackendWithNoStoreOpensOneEveryDispatch(t *testing.T) {
	agent := &sessionAgent{}
	dispatches := 0
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			dispatches++
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			if dispatches == 1 {
				return flow.StepResult{}, ctx.Park(flow.ParkRequest{
					Kind: flow.ParkBlocked, Reason: "stopping here to reach a second dispatch",
				})
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)
	app.Orchestrator = noSessionBackend{app.Orchestrator}

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "parked" {
		t.Fatalf("first RunOne = (%+v, %v), want parked", res, err)
	}
	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("second RunOne = (%+v, %v), want done — a missing store must not stop a step", res, err)
	}
	wantFresh(t, agent, 0)
	wantFresh(t, agent, 1)
}

// sessionSaveFailsBackend has a store that will not take a write.
type sessionSaveFailsBackend struct {
	*fake.Orchestrator
	err error
}

func (b sessionSaveFailsBackend) SaveAgentSession(context.Context, flow.ItemRef, flow.AgentSession) error {
	return b.err
}

// A store that cannot record the handle costs a re-opened conversation and
// nothing else: the step completes, and the failure is reported rather than
// swallowed. The prompt is what makes a dispatch right; the handle decides only
// what it costs.
func TestSession_SaveFailureCostsTheHandleNotTheStep(t *testing.T) {
	agent := &sessionAgent{}
	tel := &recordingTelemetry{}
	wantErr := errors.New("disk went away")
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)
	app.Orchestrator = sessionSaveFailsBackend{Orchestrator: be, err: wantErr}
	app.Telemetry = tel

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	var reported bool
	for _, e := range tel.events {
		if strings.Contains(e.Detail, wantErr.Error()) {
			reported = true
		}
	}
	if !reported {
		t.Errorf("telemetry = %+v, want the store's failure named — a handle that was not kept is worth saying", tel.events)
	}
}

// A write the store REFUSED still binds for the rest of the dispatch. The
// durable record is what a later dispatch reads, and losing it costs one
// re-opened conversation; losing it for the prompts still to come in THIS
// invocation costs one per prompt, which is the implement step's fix rounds each
// starting from a verify tail with no memory of the edits they are fixing —
// precisely the defect, reappearing wherever the store is unwell.
func TestSession_AFailedWriteStillChainsTheRestOfTheDispatch(t *testing.T) {
	agent := &sessionAgent{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("implement the change", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			for range 3 {
				if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
					return flow.StepResult{}, err
				}
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)
	app.Orchestrator = sessionSaveFailsBackend{Orchestrator: be, err: errors.New("disk went away")}

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	wantFresh(t, agent, 0)
	wantResume(t, agent, 1, "sess-1")
	wantResume(t, agent, 2, "sess-1")
	if agent.minted != 1 {
		t.Errorf("the substrate opened %d sessions across 3 prompts, want 1 — a store that would not take the write is not a reason to buy the conversation again this turn", agent.minted)
	}
}

// A handler cannot choose which conversation the resolution is having. Mirrors
// the Worktree rule: the field the step wrote is overwritten, not narrowed.
func TestSession_HandlerCannotChooseTheSession(t *testing.T) {
	agent := &sessionAgent{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
				Prompt:          "work",
				ResumeSessionID: "a-session-the-handler-picked",
			}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)
	// A record for THIS item, so the override is visible as a replacement rather
	// than only as a clearing.
	if err := be.SaveAgentSession(context.Background(), claim.ItemRef,
		flow.AgentSession{SessionID: "the-resolutions-own"}); err != nil {
		t.Fatalf("SaveAgentSession: %v", err)
	}

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	wantResume(t, agent, 0, "the-resolutions-own")
}

// A step declaring `Prompts: none` is refused before anything is sent, and the
// refusal costs NOTHING: no prompt, no charge, and no session record either. A
// violation that quietly wrote to the resolution's state would be a violation
// with a price.
func TestSession_MechanicalPromptIsRefusedAndWritesNoSession(t *testing.T) {
	agent := &sessionAgent{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("open branch", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsNone, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" {
		t.Fatalf("res = %+v, want parked — a mechanical step that prompts is refused", res)
	}
	if len(agent.reqs) != 0 {
		t.Errorf("the agent saw %d requests, want none — nothing is sent", len(agent.reqs))
	}
	got, err := be.LoadAgentSession(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("LoadAgentSession: %v", err)
	}
	if got != (flow.AgentSession{}) {
		t.Errorf("session record = %+v, want nothing written by a refused prompt", got)
	}
}

// A dispatch the TREASURER REFUSED discards nothing. The boundary is applied
// after the pre-dispatch gate for exactly this: the step never ran, so the
// resolution did not reach the moment its author said the conversation ends —
// and giving the conversation up anyway would throw away context already paid
// for, on behalf of a step that was not allowed to speak.
//
// The refusal is a park a person clears with `grant`, or routes around. Either
// way the handle has to still be there when they do.
func TestSession_ARefusedDispatchDoesNotDiscardTheSession(t *testing.T) {
	agent := &sessionAgent{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", promptingStep("implementation", asPlan),
			flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
				Next: []flow.StepId{"implementation"}})
		f.AddStep("review the work", "implementation", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			return asPatch(ctx.Finalize(flow.DispositionResolved, "done")), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor",
			Session: flow.SessionFresh, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)
	// The declaring step is over its cost cap before it is ever dispatched, so
	// the gate refuses it with nothing spent on it.
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"implementation": {MaxCostUSD: 1}}
	if err := be.AddCost(context.Background(), claim.ItemRef, "implementation", 2); err != nil {
		t.Fatalf("AddCost: %v", err)
	}

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("first RunOne = (%+v, %v), want the plan to complete", res, err)
	}
	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("second RunOne: %v", err)
	}
	if res.Status != "parked" {
		t.Fatalf("res = %+v, want parked — the step is over its cost cap", res)
	}
	if len(agent.reqs) != 1 {
		t.Fatalf("the agent saw %d requests, want 1 — the refused step never prompted", len(agent.reqs))
	}
	got, err := be.LoadAgentSession(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("LoadAgentSession: %v", err)
	}
	if got.SessionID != "sess-1" {
		t.Errorf("session after the refusal = %+v, want the plan's conversation still held", got)
	}
	if got.Boundary != "" {
		t.Errorf("Boundary = %q, want none — a step that did not run honoured no declaration", got.Boundary)
	}
}

// sessionLoadFailsBackend has a store that will not answer a read.
type sessionLoadFailsBackend struct {
	*fake.Orchestrator
	err error
}

func (b sessionLoadFailsBackend) LoadAgentSession(context.Context, flow.ItemRef) (flow.AgentSession, error) {
	return flow.AgentSession{}, b.err
}

// A store that CANNOT ANSWER reads as absence and is said out loud. Absence is
// the only answer the chokepoint can act on — the handle is offered and never
// depended on, so nothing here may turn a missing one into a failed dispatch —
// but this absence costs a re-opened conversation, and a resolution paying that
// silently would look exactly like one with no store at all.
func TestSession_AStoreThatCannotAnswerIsAbsenceAndIsReported(t *testing.T) {
	agent := &sessionAgent{}
	tel := &recordingTelemetry{}
	wantErr := errors.New("the store is on fire")
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)
	app.Orchestrator = sessionLoadFailsBackend{Orchestrator: be, err: wantErr}
	app.Telemetry = tel

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done — a store that cannot answer must not stop a step", res, err)
	}
	wantFresh(t, agent, 0)
	var reported int
	for _, e := range tel.events {
		if strings.Contains(e.Detail, wantErr.Error()) {
			reported++
		}
	}
	if reported != 1 {
		t.Errorf("the read failure was reported %d times, want once: %+v", reported, tel.events)
	}
}

// And a backend that simply HAS NO STORE says nothing. The distinction is the
// implementation of "the mechanism is absent rather than broken": a store that
// was asked and could not answer is worth a line on every dispatch, and one that
// never existed would be the same line on every dispatch of every item, saying
// only that the operator chose a backend without the feature.
func TestSession_ABackendWithNoStoreReportsNothing(t *testing.T) {
	agent := &sessionAgent{}
	tel := &recordingTelemetry{}
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true,
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)
	app.Orchestrator = noSessionBackend{app.Orchestrator}
	app.Telemetry = tel

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	for _, e := range tel.events {
		if strings.Contains(e.Detail, "agent session") {
			t.Errorf("a backend with no store reported %q; absent is not failed", e.Detail)
		}
	}
}

// sessionSaveCountingBackend counts what the store was actually asked to write.
type sessionSaveCountingBackend struct {
	*fake.Orchestrator
	saves int
}

func (b *sessionSaveCountingBackend) SaveAgentSession(ctx context.Context, ref flow.ItemRef, s flow.AgentSession) error {
	b.saves++
	return b.Orchestrator.SaveAgentSession(ctx, ref, s)
}

// A handle the store ALREADY HOLDS is not written again. The implement step
// prompts once per fix round, and a substrate that honours the handle answers
// every one of them with the same id: a write per prompt would be a write that
// cannot change anything, repeated against a store that can still fail — and
// each failure is a line of telemetry saying a handle was lost that was never
// at risk.
func TestSession_AnUnchangedHandleIsNotWrittenAgain(t *testing.T) {
	agent := &sessionAgent{}
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
	counting := &sessionSaveCountingBackend{Orchestrator: be}
	app.Orchestrator = counting

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	if len(agent.reqs) != 3 {
		t.Fatalf("the agent saw %d requests, want 3", len(agent.reqs))
	}
	if counting.saves != 1 {
		t.Errorf("the store was written %d times across 3 prompts, want 1 — only the turn that opened the conversation changed anything", counting.saves)
	}
}

// decliningAgent honours nothing: the first time it is OFFERED a handle it
// answers with a conversation of its own instead, which is what a substrate that
// pruned the session, expired it, or never recorded it on this machine does.
// Every request still reaches the inner agent, so the same helpers read them.
type decliningAgent struct {
	inner    *sessionAgent
	declined bool
}

func (a *decliningAgent) Name() string { return "declines-once" }

func (a *decliningAgent) Run(ctx context.Context, req flow.AgentRequest) (*flow.AgentResponse, error) {
	resp, err := a.inner.Run(ctx, req)
	if req.ResumeSessionID != "" && !a.declined && resp != nil {
		a.declined = true
		a.inner.minted++
		resp.SessionID = fmt.Sprintf("sess-%d", a.inner.minted)
	}
	return resp, err
}

// A handle the substrate DECLINED is replaced by the one it opened instead, and
// the dead one is never offered again.
//
// This is the other half of "offered and never depended on": the SDK may not
// fail when a resume is refused, and it may not keep offering a handle the
// substrate has already answered by ignoring. A resolution that went on offering
// the dead one would re-buy its context at every prompt for the rest of the item
// while its record still claimed it was holding a conversation — and at the
// substrate the same handle would cost an extra spawn each time
// (claude.Client's declined-resume fallback), for a turn that can never resume.
func TestSession_AHandleTheSubstrateDeclinedIsReplaced(t *testing.T) {
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
	}, declining)

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	wantFresh(t, agent, 0)            // the entry opens sess-1
	wantResume(t, agent, 1, "sess-1") // which is offered back, and declined
	wantResume(t, agent, 2, "sess-2") // so the one the substrate opened is the resolution's now
	got, err := be.LoadAgentSession(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("LoadAgentSession: %v", err)
	}
	if got.SessionID != "sess-2" {
		t.Errorf("the stored handle = %+v, want sess-2 — the next dispatch must not resume a session the substrate has already refused", got)
	}
}

// runSteps dispatches n times, failing on the first dispatch that errors. The
// multi-step fixtures above need the route walked, and none of them is about
// how a dispatch ends.
func runSteps(t *testing.T, app *App, claim flow.Claim, n int) {
	t.Helper()
	for i := range n {
		res, err := RunOne(context.Background(), app, claim)
		if err != nil {
			t.Fatalf("RunOne %d: %v", i+1, err)
		}
		if res.Status == "failed" || res.Status == "parked" {
			t.Fatalf("RunOne %d = %+v, want it to advance", i+1, res)
		}
	}
}
