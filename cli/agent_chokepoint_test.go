package cli

import (
	"context"
	"errors"
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
		Flows:        []*flow.Flow{newDummyFlow("x")},
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
		Flows:        []*flow.Flow{newDummyFlow("x")},
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := app.Agent.Name(); got != "claude" {
		t.Errorf("App.Agent.Name() = %q, want %q", got, "claude")
	}
}

// A step handler still spends: the wrapper is a lock on the field, not on the
// dispatch. The request reaches the agent the binary supplied, unchanged.
func TestStepHandler_StillReachesTheRealAgent(t *testing.T) {
	agent := &stubAgent{name: "stub"}
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("spend", "plan", func(ctx flow.StepCtx) error {
			_, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "real work"})
			return err
		}, flow.StepConfig{Budget: flow.StepBudget{MaxInvocations: 1, MaxPromptsPerInvocation: 1}})
	}, agent)

	if _, err := RunOne(context.Background(), app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if len(agent.reqs) != 1 || agent.reqs[0].Prompt != "real work" {
		t.Fatalf("the step's turn did not reach the agent: %+v", agent.reqs)
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
		Flows:        []*flow.Flow{newDummyFlow("x")},
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
