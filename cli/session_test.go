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
