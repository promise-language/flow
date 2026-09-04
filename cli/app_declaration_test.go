package cli

import (
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// Gates and commands are declared and validated at startup, exactly as signals
// and artifacts are. The reason is the whole point of putting the check here: a
// flow naming a gate nothing can run must fail BEFORE an item is claimed, not
// part-way through one when a step has already burned a turn getting there.
//
// Each case below is one refusal the startup gate owes. A validate() that
// accepted any of them would let a binary start and then discover, mid-run,
// that the measurement it depends on does not exist.

// declaringApp is a minimal well-formed App over the given orchestrator: one
// flow, one artifact, nothing that would fail validation for another reason.
func declaringApp(be flow.Orchestrator, gates ...flow.GateName) App {
	f := flow.NewFlow("x", []flow.ItemType{"task"})
	f.AddStep("plan", "plan", func(flow.StepCtx) error { return nil }, flow.StepConfig{})
	return App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Gates:        gates,
		Flows:        []*flow.Flow{f},
	}
}

func TestApp_Validate_AcceptsTheDefaultDeclaration(t *testing.T) {
	// The baseline the refusals below are deviations from: with the standard
	// declaration and no extra gates named, startup passes. Without this a
	// refusal test proves nothing — every App would fail validation.
	app := declaringApp(fake.New())
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// `integration` and `fit` are both required and MUST appear: nothing lands
// without integration passing, and fit must run before a claim is taken. An
// orchestrator declaring only one of them is refused, naming the missing one.
func TestApp_Validate_RejectsAMissingRequiredGate(t *testing.T) {
	for _, missing := range flow.RequiredGates() {
		t.Run(string(missing), func(t *testing.T) {
			var kept []flow.GateDef
			for _, want := range flow.RequiredGates() {
				if want != missing {
					kept = append(kept, flow.Gate(want, true))
				}
			}
			be := fake.New()
			be.SetSupportedGates(kept...)

			app := declaringApp(be)
			err := app.validate()
			if err == nil {
				t.Fatalf("validate accepted an orchestrator declaring no %q gate", missing)
			}
			if !strings.Contains(err.Error(), string(missing)) {
				t.Errorf("error = %q, want it to name the missing gate %q", err, missing)
			}
		})
	}
}

// `verify` is required — a step should not fail over something verify would
// have fixed — so an orchestrator that declares no command at all is refused.
func TestApp_Validate_RejectsAMissingRequiredCommand(t *testing.T) {
	be := fake.New()
	be.SetSupportedCommands() // declares nothing

	app := declaringApp(be)
	err := app.validate()
	if err == nil {
		t.Fatal("validate accepted an orchestrator declaring no verify command")
	}
	if !strings.Contains(err.Error(), string(flow.CommandVerify)) {
		t.Errorf("error = %q, want it to name %q", err, flow.CommandVerify)
	}
}

// The set of command names is CLOSED AT THREE: a project does not invent a
// fourth, because the flow decides when each runs and would have no place to
// run one it did not know about. An orchestrator declaring one anyway is
// refused rather than silently carrying a command nothing will ever ask for.
func TestApp_Validate_RejectsAnInventedCommandName(t *testing.T) {
	be := fake.New()
	be.SetSupportedCommands(flow.Command(flow.CommandVerify), flow.Command("deploy"))

	app := declaringApp(be)
	err := app.validate()
	if err == nil {
		t.Fatal("validate accepted an orchestrator declaring a command outside the closed set")
	}
	if !strings.Contains(err.Error(), "deploy") {
		t.Errorf("error = %q, want it to name the invented command", err)
	}
}

func TestApp_Validate_RejectsAnInvalidGateName(t *testing.T) {
	be := fake.New()
	be.SetSupportedGates(
		flow.Gate(flow.GateIntegration, true),
		flow.Gate(flow.GateFit, true),
		flow.Gate("", false),
	)

	app := declaringApp(be)
	if err := app.validate(); err == nil {
		t.Fatal("validate accepted an orchestrator declaring the empty gate name")
	}
}

// A gate a flow NAMES but the orchestrator cannot run is the case the whole
// check exists for: the binary starts, claims an item, and the step asking for
// that gate discovers there is nothing to run.
func TestApp_Validate_RejectsAGateTheOrchestratorCannotRun(t *testing.T) {
	be := fake.New()
	be.SetSupportedGates(flow.Gate(flow.GateIntegration, true), flow.Gate(flow.GateFit, true))

	app := declaringApp(be, "tested")
	err := app.validate()
	if err == nil {
		t.Fatal("validate accepted App.Gates naming a gate outside SupportedGates()")
	}
	if !strings.Contains(err.Error(), "tested") {
		t.Errorf("error = %q, want it to name the gate that cannot be run", err)
	}
}

// The counterpart: a gate the orchestrator DOES declare beyond the required two
// is accepted, so the check narrows to what it is for rather than refusing
// every composition.
func TestApp_Validate_AcceptsADeclaredExtraGate(t *testing.T) {
	be := fake.New()
	be.SetSupportedGates(
		flow.Gate(flow.GateIntegration, true),
		flow.Gate(flow.GateFit, true),
		flow.Gate("tested", false),
	)

	app := declaringApp(be, "tested")
	if err := app.validate(); err != nil {
		t.Fatalf("validate refused a gate the orchestrator declares: %v", err)
	}
}
