package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// What an orchestrator declares it can run is checked at the boundary of the
// commands that will run one — `claim`, `run-step`, `resolve` — and nowhere
// else. It is not configuration: an orchestrator reads it off the machine, so
// asking is touching the environment, and an empty answer means the project's
// tools have not been built rather than that the binary is misconfigured.
//
// Each case below is one refusal that boundary owes. A boundary that accepted
// any of them would claim an item and then discover, mid-run, that the
// measurement it depends on does not exist.

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
func TestApp_RequireRunnable_RejectsAMissingRequiredGate(t *testing.T) {
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
			err := app.requireRunnable("resolve")
			if err == nil {
				t.Fatalf("requireRunnable accepted an orchestrator declaring no %q gate", missing)
			}
			if !strings.Contains(err.Error(), string(missing)) {
				t.Errorf("error = %q, want it to name the missing gate %q", err, missing)
			}
		})
	}
}

// `verify` is required — a step should not fail over something verify would
// have fixed — so an orchestrator that declares no command at all is refused.
func TestApp_RequireRunnable_RejectsAMissingRequiredCommand(t *testing.T) {
	be := fake.New()
	be.SetSupportedCommands() // declares nothing

	app := declaringApp(be)
	err := app.requireRunnable("resolve")
	if err == nil {
		t.Fatal("requireRunnable accepted an orchestrator declaring no verify command")
	}
	if !strings.Contains(err.Error(), string(flow.CommandVerify)) {
		t.Errorf("error = %q, want it to name %q", err, flow.CommandVerify)
	}
}

// The set of command names is CLOSED AT THREE: a project does not invent a
// fourth, because the flow decides when each runs and would have no place to
// run one it did not know about. An orchestrator declaring one anyway is
// refused rather than silently carrying a command nothing will ever ask for.
func TestApp_RequireRunnable_RejectsAnInventedCommandName(t *testing.T) {
	be := fake.New()
	be.SetSupportedCommands(flow.Command(flow.CommandVerify), flow.Command("deploy"))

	app := declaringApp(be)
	err := app.requireRunnable("resolve")
	if err == nil {
		t.Fatal("requireRunnable accepted an orchestrator declaring a command outside the closed set")
	}
	if !strings.Contains(err.Error(), "deploy") {
		t.Errorf("error = %q, want it to name the invented command", err)
	}
}

func TestApp_RequireRunnable_RejectsAnInvalidGateName(t *testing.T) {
	be := fake.New()
	be.SetSupportedGates(
		flow.Gate(flow.GateIntegration, true),
		flow.Gate(flow.GateFit, true),
		flow.Gate("", false),
	)

	app := declaringApp(be)
	if err := app.requireRunnable("resolve"); err == nil {
		t.Fatal("requireRunnable accepted an orchestrator declaring the empty gate name")
	}
}

// The counterpart: the standard declaration is runnable, so the boundary lets
// every gate-running command through on a machine whose tools are built.
func TestApp_RequireRunnable_AcceptsTheDefaultDeclaration(t *testing.T) {
	app := declaringApp(fake.New())
	if err := app.requireRunnable("resolve"); err != nil {
		t.Fatalf("requireRunnable refused the standard declaration: %v", err)
	}
}

// A gate a flow NAMES but the orchestrator cannot run is the case the startup
// check exists for: the binary starts, claims an item, and the step asking for
// that gate discovers there is nothing to run. That one IS configuration — the
// flow's own composition — so it stays at startup.
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

// The wrong-moment half, made checkable. An orchestrator that can run NOTHING
// still passes startup: a checkout whose project tools are not built declares
// nothing, and that is the state between the two halves of bring-up, not a
// misconfiguration.
func TestApp_Validate_DoesNotCheckGatesOrCommands(t *testing.T) {
	be := fake.New()
	be.SetSupportedGates()
	be.SetSupportedCommands()

	app := declaringApp(be)
	if err := app.validate(); err != nil {
		t.Fatalf("validate refused an orchestrator that declares nothing: %v", err)
	}
}

// And it does not even ASK. Asking reaches the environment, so a binary whose
// flows name no extra gate must not pay for the answer at startup — the probe
// is what spawns the project's gate entry point on a real orchestrator.
func TestApp_Validate_DoesNotProbeWhenNoFlowNamesAGate(t *testing.T) {
	be := &declaringOrchestrator{
		Orchestrator: fake.New(),
		gates:        []flow.GateDef{flow.Gate(flow.GateIntegration, true), flow.Gate(flow.GateFit, true)},
		commands:     []flow.CommandDef{flow.Command(flow.CommandVerify)},
	}

	app := declaringApp(be)
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if be.gateCalls != 0 || be.commandCalls != 0 {
		t.Errorf("startup probed the environment: SupportedGates() %d call(s), SupportedCommands() %d call(s), want 0 and 0",
			be.gateCalls, be.commandCalls)
	}

	// Naming one is what asks the question — and only the gate half of it.
	be.gates = append(be.gates, flow.Gate("tested", false))
	named := declaringApp(be, "tested")
	if err := named.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if be.gateCalls == 0 {
		t.Error("App.Gates named a gate and startup never asked whether it can be run")
	}
}

// The wrong-command half. A read-only command works on a clone whose only
// tools are the artifact's — that is the reported defect, and `list` is the
// invocation that reported it.
//
// The exit code is not the subject: several of these fail for their own reasons
// (no active claim), which is fine. What must not happen is a refusal about
// gates, or the probe that produces one.
func TestRunWithArgs_ReadOnlyCommandsRunWithoutGates(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		// doctor asks on purpose, and prints the answer: reporting what the
		// orchestrator can run is its own job, and the whole point of the
		// command is running on a machine nothing else will. Its report is on
		// stdout; stderr must still carry no refusal.
		probes bool
	}{
		{name: "list", args: []string{"list"}},
		{name: "status", args: []string{"status", "1"}},
		{name: "answer", args: []string{"answer", "1", "yes"}},
		{name: "release", args: []string{"release"}},
		{name: "reseed", args: []string{"reseed"}},
		{name: "grant", args: []string{"grant"}},
		{name: "stale", args: []string{"stale", "plan"}},
		{name: "doctor", args: []string{"doctor"}, probes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, be, out, errBuf := gatelessApp(t)
			RunWithArgs(*app, tc.args)

			streams := map[string]string{"stderr": errBuf.String()}
			if !tc.probes {
				streams["stdout"] = out.String()
			}
			for where, stream := range streams {
				if strings.Contains(stream, "cannot run gates") || strings.Contains(stream, "required gate") {
					t.Errorf("%s refused over a gate it never runs, on %s:\n%s", tc.name, where, stream)
				}
			}
			if !tc.probes && be.gateCalls != 0 {
				t.Errorf("%s asked the orchestrator what gates it can run %d time(s), want 0",
					tc.name, be.gateCalls)
			}
		})
	}
}

// Help is not a command that runs anything, and the reported clone could not
// even print it: `--help` died on the startup error before help was reached.
func TestRunWithArgs_HelpNeverNeedsAGate(t *testing.T) {
	for _, args := range [][]string{
		{"--help"}, {"-h"}, {"help"}, {"help", "claim"},
		{"claim", "--help"}, {"resolve", "--help"}, {"run-step", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			app, be, out, errBuf := gatelessApp(t)
			code := RunWithArgs(*app, args)
			if code != 0 {
				t.Errorf("exit code = %d, want 0 (err=%q)", code, errBuf.String())
			}
			if !strings.Contains(out.String(), "usage:") {
				t.Errorf("out = %q, want usage text", out.String())
			}
			if be.gateCalls != 0 {
				t.Errorf("printing help asked the orchestrator what it can run %d time(s), want 0", be.gateCalls)
			}
		})
	}
}

// The commands that WILL run a gate meet the check at their own boundary. The
// refusal names the command, both missing gates, the repair, and `doctor` —
// docs/cli.md § 6: whoever reads it must know what to run next.
func TestRunWithArgs_GateRunningCommandsRefuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "claim", args: []string{"claim", "1"}},
		{name: "run-step", args: []string{"run-step"}},
		{name: "resolve", args: []string{"resolve"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _, out, errBuf := gatelessApp(t)
			code := RunWithArgs(*app, tc.args)

			// 1, not 2: the invocation was well formed. This is a condition a
			// human must clear, which is what exit 1 means.
			if code != 1 {
				t.Errorf("exit code = %d, want 1 (out=%q err=%q)", code, out.String(), errBuf.String())
			}
			got := errBuf.String()
			want := []string{
				tc.name, string(flow.GateIntegration), string(flow.GateFit),
				"build the project's tools", "doctor",
			}
			for _, w := range want {
				if !strings.Contains(got, w) {
					t.Errorf("refusal = %q, want it to name %q", got, w)
				}
			}
		})
	}
}

// Adding a command forces a decision about this, rather than defaulting it to
// "runs no gate" in a list nobody remembers to update.
func TestCommandRunsGates_MatchesTheRegistry(t *testing.T) {
	want := map[string]bool{"claim": true, "run-step": true, "resolve": true}
	for cmd, h := range perCommandUsage {
		if h.runsGates != want[cmd] {
			t.Errorf("%q runsGates = %v, want %v", cmd, h.runsGates, want[cmd])
		}
		if commandRunsGates(cmd) != h.runsGates {
			t.Errorf("commandRunsGates(%q) disagrees with the registry entry", cmd)
		}
	}
	for cmd := range want {
		if _, ok := perCommandUsage[cmd]; !ok {
			t.Errorf("%q runs a gate but is not a command", cmd)
		}
	}
	if commandRunsGates("frobnicate") {
		t.Error("an unknown command must not be treated as one that runs a gate")
	}
}

// gatelessApp is the reported clone: a valid App over an orchestrator that
// declares nothing, because the project's tools have not been built.
func gatelessApp(t *testing.T) (*App, *declaringOrchestrator, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	be := &declaringOrchestrator{Orchestrator: fake.New()}
	app := declaringApp(be)
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	app.Out, app.Err = out, errBuf
	app.Name = "issue"
	return &app, be, out, errBuf
}
