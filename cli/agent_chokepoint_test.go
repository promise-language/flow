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

// The runtime half of "nothing mechanical may spend": after startup validation
// the agent on the App refuses a turn. Command code reaches App.Agent in one
// field access, and a turn spent there looks exactly like a step's turn until
// the bill arrives — `doctor` did precisely that, on every run, forever.
func TestAppAgent_RefusesATurnOutsideAStepHandler(t *testing.T) {
	inner := &stubAgent{name: "stub"}
	app := &App{
		Orchestrator: fake.New(),
		Agent:        inner,
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flow:         newDummyFlow("x"),
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	resp, err := app.Agent.Run(context.Background(), flow.AgentRequest{Prompt: "spend something"})
	if err == nil {
		t.Fatalf("App.Agent.Run returned no error (resp %+v) — a command must not be able to spend", resp)
	}
	if !errors.Is(err, ErrAgentOutsideStep) {
		t.Errorf("error %v does not wrap ErrAgentOutsideStep; a caller cannot tell a program error "+
			"from an agent or transport failure it should retry", err)
	}
	if len(inner.reqs) != 0 {
		t.Errorf("the refusal still reached the real agent: %+v", inner.reqs)
	}
	// The refusal has to say what to do instead, or it reads as a bug in the
	// SDK rather than as a rule the caller broke.
	for _, want := range []string{"ctx.Agent()", "step"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}

// Name() still answers: doctor reports which agent is configured, and a
// diagnostic that cannot name the thing it is diagnosing is worse than none.
func TestAppAgent_StillAnswersName(t *testing.T) {
	app := &App{
		Orchestrator: fake.New(),
		Agent:        &stubAgent{name: "claude"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flow:         newDummyFlow("x"),
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := app.Agent.Name(); got != "claude" {
		t.Errorf("App.Agent.Name() = %q, want %q", got, "claude")
	}
}

// A step handler still spends: the wrapper is a lock on the field, not on the
// dispatch. The request reaches the agent the binary supplied, carrying what
// the chokepoint sets on the way through — the grant's cost headroom, and the
// arena the turn runs in.
func TestStepHandler_StillReachesTheRealAgent(t *testing.T) {
	agent := &stubAgent{name: "stub"}
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("spend", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			_, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "real work"})
			return flow.StepResult{}, err
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)
	app.StepBudgets = map[flow.StepId]flow.StepBudget{
		"plan": {MaxInvocations: 1, MaxPromptsPerInvocation: 1},
	}

	if _, err := RunOne(context.Background(), app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if len(agent.reqs) != 1 || agent.reqs[0].Prompt != "real work" {
		t.Fatalf("the step's turn did not reach the agent: %+v", agent.reqs)
	}
}

// promptFromMechanicalStep is the helper a handler prompts through, named so the
// park reason can be checked for the frame that asked: the site is the caller
// of Run, which is this function, not the handler that called it.
func promptFromMechanicalStep(ctx flow.StepCtx) error {
	_, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "should never be sent"})
	return err
}

// mechanicalStepConfig is the registration under test: a step declaring
// Prompts: none, which is the whole of what makes it mechanical.
func mechanicalStepConfig() flow.StepConfig {
	return flow.StepConfig{
		Role:        "contributor",
		Entry:       true,
		Prompts:     flow.PromptsNone,
		MayFinalize: []flow.Disposition{flow.DispositionResolved},
	}
}

// assertMechanicalRefusalParked is what every refused prompt from a mechanical
// step must leave behind, whatever the handler did with the error: the stub
// saw no request, the dispatch parked ParkRefused — the kind that classifies
// itself as not clearing by re-dispatch — with the reason naming the step and
// the site, nothing was billed, and the dispatch was not charged.
func assertMechanicalRefusalParked(t *testing.T, res flow.InvocationResult, agent *stubAgent, be *fake.Orchestrator, claim flow.Claim, step string) {
	t.Helper()
	if len(agent.reqs) != 0 {
		t.Errorf("the prompt reached the real agent: %+v — enforcement that bills for the violation it reports falsifies the guarantee", agent.reqs)
	}
	if res.Status != string(flow.StatusParked) || res.Park == nil || res.Park.Kind != flow.ParkRefused {
		t.Fatalf("res = %+v, want parked with kind %q", res, flow.ParkRefused)
	}
	if res.Park.Step != flow.StepId(step) {
		t.Errorf("park names step %q, want %q", res.Park.Step, step)
	}
	if res.RedispatchMayClear == nil || *res.RedispatchMayClear {
		t.Errorf("redispatch_may_clear = %v, want a present false — a mis-declared step answers identically every time, so a driver that re-dispatches is looping", res.RedispatchMayClear)
	}
	// The site is "function (file:line)" with the file's BASE name: the full
	// path is a fact about the machine the binary was built on, and the reason
	// is published on the item. Wanting "(agent_chokepoint_test.go:" rather than
	// the bare file name is what makes a regression to the full path visible.
	// "should not declare none" is the instruction remedyFor(ParkRefused) sends
	// the operator to the reason for, so the reason must carry it.
	for _, want := range []string{`"` + step + `"`, "Prompts: none", "should not declare none", "promptFromMechanicalStep", "(agent_chokepoint_test.go:"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("park reason does not mention %q — a reader cannot tell whether to fix the handler or the declaration without both the step and the site: %q", want, res.Reason)
		}
	}
	// A park moves nothing, so the step is still what the route points at, and
	// it is mechanical: the envelope says both, so a driver that fixes the
	// declaration or the handler knows the re-dispatch need not wait for quota.
	assertNext(t, res, step, true)
	if res.CostUSD == nil || *res.CostUSD != 0 {
		t.Errorf("cost_usd = %v, want a present zero: the step ran and spent nothing", res.CostUSD)
	}
	state, err := be.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := state.Ledger.Row(flow.StepId(step)).Dispatches; got != 0 {
		t.Errorf("Dispatches = %d, want 0 — a step that never spent is not charged an invocation for the violation", got)
	}
	if len(state.Journal) != 0 {
		t.Errorf("journal = %+v, want empty — the step parked, so nothing completed", state.Journal)
	}
	if state.Park == nil || state.Park.Kind != flow.ParkRefused {
		t.Errorf("item park = %+v, want the refused park recorded on the item", state.Park)
	}
}

// A step declaring Prompts: none that asks for a prompt is refused BEFORE
// anything is sent, and the handler is told so through a sentinel it can
// recognise. The dispatch parks rather than fails — journal position and the
// claim survive for whoever corrects it — and the park carries the kind that
// says re-dispatching cannot clear it (docs/flow-registration.md § Step
// configuration).
func TestMechanicalStep_PromptIsRefusedBeforeAnythingIsSent(t *testing.T) {
	agent := &stubAgent{name: "stub"}
	var handlerSaw error
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("open branch", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			handlerSaw = promptFromMechanicalStep(ctx)
			return flow.StepResult{}, handlerSaw
		}, mechanicalStepConfig())
	}, agent)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if !errors.Is(handlerSaw, ErrMechanicalStepPrompt) {
		t.Errorf("the handler received %v, want it to wrap ErrMechanicalStepPrompt so a handler can tell the refusal from an agent failure", handlerSaw)
	}
	if errors.Is(handlerSaw, ErrAgentOutsideStep) {
		t.Errorf("the refusal reads as outside-a-step: %v — it is a sibling with a different reason, not the same one", handlerSaw)
	}
	assertMechanicalRefusalParked(t, res, agent, be, claim, "plan")
}

// A signal step takes the unmetered pass-through in the chokepoint — no caps
// apply to it — and the mechanical refusal sits ABOVE that, so a signal step
// declaring none is refused the same way.
func TestMechanicalSignalStep_PromptIsRefusedBeforeThePassThrough(t *testing.T) {
	agent := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddSignalStep("merge", "pr-open", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return flow.StepResult{}, promptFromMechanicalStep(ctx)
		}, mechanicalStepConfig())
	}, agent)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	assertMechanicalRefusalParked(t, res, agent, be, claim, "pr-open")
}

// The declaration holds whatever the handler does with the error. The park is
// read off the chokepoint, not off what the handler returned: a handler that
// swallows the refusal and completes would otherwise journal a result on the
// strength of a prompt that never ran, and one that re-wraps it as transient
// would park infra-transient — the kind a driver re-dispatches forever.
func TestMechanicalStep_ParksWhateverTheHandlerDoesWithTheRefusal(t *testing.T) {
	cases := []struct {
		name    string
		handler func(ctx flow.StepCtx) (flow.StepResult, error)
	}{
		{"swallowed and completed", func(ctx flow.StepCtx) (flow.StepResult, error) {
			_ = promptFromMechanicalStep(ctx)
			return ctx.Finalize(flow.DispositionResolved, "done anyway").Markdown("the plan"), nil
		}},
		{"re-wrapped without %w", func(ctx flow.StepCtx) (flow.StepResult, error) {
			err := promptFromMechanicalStep(ctx)
			return flow.StepResult{}, fmt.Errorf("agent failed: %v", err)
		}},
		{"turned into a transient", func(ctx flow.StepCtx) (flow.StepResult, error) {
			_ = promptFromMechanicalStep(ctx)
			return flow.StepResult{}, fmt.Errorf("runner flapped: %w", flow.ErrTransient)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := &stubAgent{name: "stub"}
			app, be, claim := testApp(t, func(f *flow.Flow) {
				f.AddStep("open branch", "plan", tc.handler, mechanicalStepConfig())
			}, agent)

			res, err := RunOne(context.Background(), app, claim)
			if err != nil {
				t.Fatalf("RunOne: %v", err)
			}
			assertMechanicalRefusalParked(t, res, agent, be, claim, "plan")
		})
	}
}

// The refusal is ahead of the prompt counter and the cap checks: a mechanical
// step that asks is refused for the real defect, not for a cap it could never
// have reached — and a refused prompt is not counted as one, so the park's
// axes show nothing spent.
func TestMechanicalStep_RefusalIsNotCountedAsAPrompt(t *testing.T) {
	agent := &stubAgent{name: "stub"}
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("open branch", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			// Two asks: the second must be refused the same way, not counted
			// against a per-invocation cap of one and parked treasurer-refused.
			_ = promptFromMechanicalStep(ctx)
			return flow.StepResult{}, promptFromMechanicalStep(ctx)
		}, mechanicalStepConfig())
	}, agent)
	app.StepBudgets = map[flow.StepId]flow.StepBudget{
		"plan": {MaxInvocations: 3, MaxPromptsPerInvocation: 1},
	}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkRefused {
		t.Fatalf("res = %+v, want a refused park, not a treasurer one: the cap was never reached because nothing was counted", res)
	}
	if len(agent.reqs) != 0 {
		t.Errorf("a prompt reached the agent: %+v", agent.reqs)
	}
}

// validate() runs more than once in a process — tests build several Apps, and a
// binary may re-validate. Wrapping a wrapper would bury the impl one layer
// deeper each time, and the dispatch would eventually hand a step handler a
// refusal instead of an agent.
func TestValidate_DoesNotDoubleWrapTheAgent(t *testing.T) {
	inner := &stubAgent{name: "stub"}
	app := &App{
		Orchestrator: fake.New(),
		Agent:        inner,
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flow:         newDummyFlow("x"),
	}
	for i := 0; i < 3; i++ {
		if err := app.validate(); err != nil {
			t.Fatalf("validate #%d: %v", i+1, err)
		}
	}
	if got := app.agentImpl(); got != flow.Agent(inner) {
		t.Errorf("agentImpl() = %#v, want the agent the binary supplied", got)
	}
}
