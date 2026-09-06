package cli

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
	err := app.requireRunnable("resolve")
	if err == nil {
		t.Fatal("requireRunnable accepted an orchestrator declaring the empty gate name")
	}
	// The offender is quoted, or the empty name is not on the line at all and
	// the reader is told a gate is wrong without being told which.
	if !strings.Contains(err.Error(), `""`) {
		t.Errorf("error = %q, want it to name the offending gate as %q", err, `""`)
	}
	// The repair is the closed vocabulary, not a build: no amount of building
	// makes an unknown name known.
	if !strings.Contains(err.Error(), string(flow.GateTested)) {
		t.Errorf("error = %q, want it to enumerate what may be declared", err)
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
			// stdout is the machine channel — `resolve` streams results there —
			// so the refusal belongs entirely on stderr.
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want empty", out.String())
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

// Every offender, not the first one. An implementor who spelled two names
// wrong should learn both in one pass — a refusal that stops at the first
// makes them re-run the tool to be told the second, and the same two
// functions render `doctor`'s rows, where reporting everything is the rule.
func TestApp_RequireRunnable_NamesEveryOffendingDeclaration(t *testing.T) {
	required := []flow.GateDef{flow.Gate(flow.GateIntegration, true), flow.Gate(flow.GateFit, true)}
	for _, tc := range []struct {
		name string
		be   *declaringOrchestrator
		want []string
	}{
		{
			name: "gates",
			be: &declaringOrchestrator{
				Orchestrator: fake.New(),
				gates: append(append([]flow.GateDef{}, required...),
					flow.Gate("lint", false), flow.Gate("smoke", false)),
				commands: []flow.CommandDef{flow.Command(flow.CommandVerify)},
			},
			want: []string{`"lint"`, `"smoke"`},
		},
		{
			name: "commands",
			be: &declaringOrchestrator{
				Orchestrator: fake.New(),
				gates:        required,
				commands: []flow.CommandDef{
					flow.Command(flow.CommandVerify), flow.Command("deploy"), flow.Command("publish")},
			},
			want: []string{`"deploy"`, `"publish"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := declaringApp(tc.be)
			err := app.requireRunnable("resolve")
			if err == nil {
				t.Fatalf("requireRunnable accepted two %s this SDK cannot address", tc.name)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal = %q, want it to name %s too", err, w)
				}
			}
		})
	}
}

// docs/cli.md § 6, applied to all four: a refusal names what to run next. Only
// the missing-gate one is reached by the end-to-end above (it is checked
// first), so the other three are asked for here — each one an environment a
// person can actually be sitting in front of.
//
// The check name is asserted because it is the address of the pointer: the
// refusal closes on `doctor`, and "gates"/"commands" is the row the reader is
// being sent to find. The doctor tests pin those two row names from the other
// side, so a rename breaks one of the two rather than silently sending the
// reader to a report with no such row.
func TestApp_RequireRunnable_EveryRefusalNamesItsRepairAndDoctor(t *testing.T) {
	required := []flow.GateDef{flow.Gate(flow.GateIntegration, true), flow.Gate(flow.GateFit, true)}
	verify := []flow.CommandDef{flow.Command(flow.CommandVerify)}
	for _, tc := range []struct {
		name     string
		gates    []flow.GateDef
		commands []flow.CommandDef
		check    string   // the doctor row the refusal sends its reader to
		want     []string // the offender named, and the repair
	}{
		{
			name:     "missing gate",
			gates:    []flow.GateDef{flow.Gate(flow.GateIntegration, true)},
			commands: verify,
			check:    "gates",
			want:     []string{string(flow.GateFit), "build the project's tools"},
		},
		{
			name:     "missing command",
			gates:    required,
			commands: nil,
			check:    "commands",
			want:     []string{string(flow.CommandVerify), "build the project's tools"},
		},
		{
			// Nothing to build here — the machine is not what is wrong — so the
			// repair is the closed vocabulary, and it must be the whole of it.
			name:     "unknown gate",
			gates:    append(append([]flow.GateDef{}, required...), flow.Gate("lint", false)),
			commands: verify,
			check:    "gates",
			want:     []string{`"lint"`, "the gate concept set is closed", string(flow.GateTested)},
		},
		{
			name:     "unknown command",
			gates:    required,
			commands: append(append([]flow.CommandDef{}, verify...), flow.Command("deploy")),
			check:    "commands",
			want:     []string{`"deploy"`, "the command set is closed", string(flow.CommandCleanup)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := declaringApp(&declaringOrchestrator{
				Orchestrator: fake.New(), gates: tc.gates, commands: tc.commands})
			err := app.requireRunnable("claim")
			if err == nil {
				t.Fatalf("requireRunnable accepted an orchestrator with a %s", tc.name)
			}
			got := err.Error()
			// The house refusal shape, prefixed with the command that was asked
			// for: the reader has to know which invocation this answers.
			if !strings.HasPrefix(got, "claim: refused — ") {
				t.Errorf("refusal = %q, want the house shape prefixed with the command", got)
			}
			if !strings.Contains(got, fmt.Sprintf("(check %q)", tc.check)) {
				t.Errorf("refusal = %q, want it to name the %q check", got, tc.check)
			}
			for _, w := range append(tc.want, "doctor") {
				if !strings.Contains(got, w) {
					t.Errorf("refusal = %q, want it to name %q", got, w)
				}
			}
		})
	}
}

// The other half of the split, held from the startup side: what stays at
// startup stays at startup. A flow NAMING a gate the orchestrator cannot run
// is this binary's own composition — wrong wherever it is deployed — so it
// refuses every command, `list` included, and at exit 2 rather than the
// boundary's 1. Without this, moving the whole block out of validate() would
// pass every other test in this file.
func TestRunWithArgs_AFlowNamingAnUnrunnableGateStillFailsAtStartup(t *testing.T) {
	for _, args := range [][]string{{"list"}, {"status", "1"}, {"resolve"}} {
		t.Run(args[0], func(t *testing.T) {
			be := &declaringOrchestrator{
				Orchestrator: fake.New(),
				gates:        []flow.GateDef{flow.Gate(flow.GateIntegration, true), flow.Gate(flow.GateFit, true)},
				commands:     []flow.CommandDef{flow.Command(flow.CommandVerify)},
			}
			app := declaringApp(be, "tested")
			out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
			app.Out, app.Err = out, errBuf
			app.Name = "issue"

			code := RunWithArgs(app, args)
			if code != 2 {
				t.Errorf("exit code = %d, want 2 (out=%q err=%q)", code, out.String(), errBuf.String())
			}
			got := errBuf.String()
			if !strings.HasPrefix(got, "startup error:") {
				t.Errorf("stderr = %q, want the startup refusal — this is configuration, not the environment", got)
			}
			if !strings.Contains(got, "tested") {
				t.Errorf("stderr = %q, want it to name the gate no flow can run", got)
			}
		})
	}
}

// commandRunsGates answers from perCommandUsage, and a command with no entry
// there answers false — the permissive direction. That makes the registry
// load-bearing for the gate check, so it has to be the same set the dispatch
// switch handles: a gate-running command added to the switch alone would skip
// its own boundary silently, which is the one failure this mechanism cannot
// report on itself.
func TestPerCommandUsage_IsTheDispatchSet(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "app.go", nil, 0)
	if err != nil {
		t.Fatalf("parse app.go: %v", err)
	}

	dispatched := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		if id, ok := sw.Tag.(*ast.Ident); !ok || id.Name != "cmd" {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, e := range cc.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					dispatched[strings.Trim(lit.Value, `"`)] = true
				}
			}
		}
		return true
	})
	if len(dispatched) == 0 {
		t.Fatal("parsed no commands out of the dispatch switch — the probe is broken, not the code")
	}

	for cmd := range dispatched {
		if _, ok := perCommandUsage[cmd]; !ok {
			t.Errorf("RunWithArgs dispatches %q with no perCommandUsage entry — commandRunsGates(%q) answers false, so a gate check it needs is skipped", cmd, cmd)
		}
	}
	for cmd := range perCommandUsage {
		if !dispatched[cmd] {
			t.Errorf("perCommandUsage lists %q, which RunWithArgs does not dispatch", cmd)
		}
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
