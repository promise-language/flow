package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

func TestCmdDoctor_OKGlyph(t *testing.T) {
	arena(t)
	be := fake.New()
	app := App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows: []*flow.Flow{
			newDummyFlow("x"),
		},
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := &bytes.Buffer{}
	app.Out = out
	app.Err = &bytes.Buffer{}

	code := app.cmdDoctor(context.Background(), nil, nil)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.HasPrefix(out.String(), glyphOK) {
		t.Errorf("doctor OK output should start with %q glyph; got %q", glyphOK, out.String())
	}
}

func TestCmdDoctor_FailGlyph(t *testing.T) {
	arena(t)
	be := &failingBackend{Orchestrator: fake.New(), err: errors.New("simulated")}
	app := App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:        []*flow.Flow{newDummyFlow("x")},
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	app.Out = out
	app.Err = errBuf

	code := app.cmdDoctor(context.Background(), nil, nil)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	// The report is one list and belongs on one stream — docs/cli.md
	// § "One-shot reports" puts it on stdout. A failing line is still a line
	// of that list, and splitting it off interleaves the report with itself.
	if !strings.HasPrefix(out.String(), glyphFail) {
		t.Errorf("doctor fail output should start with %q glyph; got %q", glyphFail, out.String())
	}
	if errBuf.Len() != 0 {
		t.Errorf("the report belongs on one stream; stderr carried %q", errBuf.String())
	}
}

// THE property of this command: doctor buys nothing. Not a capped turn, not a
// tool-free one-word turn — nothing. It runs before every item, in CI, and on
// every machine an operator touches, and a preflight that bills the account for
// each run is one that gets turned off, at which point it prevents nothing.
//
// The free capability is asked exactly once; Run — the only call that costs
// money — is never reached.
func TestCmdDoctor_AgentCheckSpendsNothing(t *testing.T) {
	agent := &doctoringAgent{stubAgent: stubAgent{name: "stub"}}
	app, out := doctorApp(t, fake.New(), agent)

	if code := app.cmdDoctor(context.Background(), nil, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out.String())
	}
	if len(agent.reqs) != 0 {
		t.Errorf("doctor requested %d agent turn(s), want 0 — a mechanical command must never spend: %+v",
			len(agent.reqs), agent.reqs)
	}
	if agent.doctorCalls != 1 {
		t.Errorf("agent.Doctor called %d times, want exactly 1", agent.doctorCalls)
	}
	if line := doctorLine(t, out.String(), "agent"); !strings.HasPrefix(line, glyphOK) {
		t.Errorf("agent line should pass; got %q", line)
	}
}

// Not one path through doctor spends, including the ones where checks fail: a
// Run reached from ANY branch is the defect, so the agent under test fails the
// test outright if it is ever asked for a turn.
func TestCmdDoctor_NoPathSpendsATurn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		orch  func() flow.Orchestrator
		agent func(t *testing.T) flow.Agent
	}{
		{"everything fine",
			func() flow.Orchestrator { return fake.New() },
			func(t *testing.T) flow.Agent { return &spendTrapAgent{t: t} }},
		{"agent broken",
			func() flow.Orchestrator { return fake.New() },
			func(t *testing.T) flow.Agent { return &spendTrapAgent{t: t, err: errors.New("claude: not found")} }},
		{"orchestrator unreachable",
			func() flow.Orchestrator {
				return &failingBackend{Orchestrator: fake.New(), err: errors.New("simulated")}
			},
			func(t *testing.T) flow.Agent { return &spendTrapAgent{t: t} }},
		{"agent cannot be checked for free",
			func() flow.Orchestrator { return fake.New() },
			func(t *testing.T) flow.Agent { return &runTrapAgent{t: t} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := doctorApp(t, tc.orch(), tc.agent(t))
			app.cmdDoctor(context.Background(), nil, nil) // the exit code is not this test's subject
		})
	}
}

// An agent that cannot be invoked — the reference impl reports a missing,
// unexecutable or too-old binary this way — fails the check, carrying the
// reason onto the line. That reason is the whole value of the check: the
// failure it replaces was diagnosed from a mid-item symptom.
func TestCmdDoctor_AgentDoctorFailureIsReported(t *testing.T) {
	agent := &doctoringAgent{
		stubAgent: stubAgent{name: "stub"},
		err:       errors.New("claude is version 2.0.9; this SDK requires 2.1.217 or later"),
	}
	app, out := doctorApp(t, fake.New(), agent)

	code := app.cmdDoctor(context.Background(), nil, nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; output:\n%s", code, out.String())
	}
	line := doctorLine(t, out.String(), "agent")
	if !strings.HasPrefix(line, glyphFail) || !strings.Contains(line, "2.1.217") {
		t.Errorf("agent line should fail carrying the reason; got %q", line)
	}
	if len(agent.reqs) != 0 {
		t.Errorf("a failing agent check still spent %d turn(s)", len(agent.reqs))
	}
}

// An agent with no free check SKIPS, and a skip is not a failure. The SDK
// cannot check a black-box Agent without buying a turn, and it may not buy one
// — so the honest report is "not checked", which is a fact about that Agent's
// interface and not about this machine. Failing here would make every custom
// agent report an unfit machine; spending here would put back the charge this
// command exists without.
func TestCmdDoctor_AgentWithoutDoctorCapabilitySkips(t *testing.T) {
	agent := &stubAgent{name: "opaque"}
	app, out := doctorApp(t, fake.New(), agent)

	code := app.cmdDoctor(context.Background(), nil, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 — an unavailable check is not an unfit machine; output:\n%s",
			code, out.String())
	}
	line := doctorLine(t, out.String(), "agent")
	if !strings.HasPrefix(line, glyphSkip) {
		t.Errorf("agent line should skip; got %q", line)
	}
	if !strings.Contains(line, "AgentDoctor") {
		t.Errorf("the skip should name the capability that is missing; got %q", line)
	}
	if len(agent.reqs) != 0 {
		t.Errorf("doctor fell back to spending a turn on an agent it could not check for free: %+v", agent.reqs)
	}
}

// arena moves the test into a directory shaped like an arena someone has set
// up: a docs/ holding one document. doctor reads the arena it RUNS in — that is
// what an arena is — so a test that does not set one up is testing this
// package's source directory.
func arena(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "cli.md"), []byte("# cli\n"), 0o644); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	t.Chdir(dir)
	return dir
}

// doctorApp builds a validated App around an orchestrator and agent, with both
// streams captured.
func doctorApp(t *testing.T, orch flow.Orchestrator, agent flow.Agent) (*App, *bytes.Buffer) {
	t.Helper()
	arena(t)
	app := &App{
		Orchestrator: orch,
		Agent:        agent,
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:        []*flow.Flow{newDummyFlow("x")},
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := &bytes.Buffer{}
	app.Out, app.Err = out, &bytes.Buffer{}
	return app, out
}

// doctorLine returns the report line for the named check.
func doctorLine(t *testing.T, report, name string) string {
	t.Helper()
	for _, line := range strings.Split(report, "\n") {
		if _, rest, found := strings.Cut(line, " "); found && strings.HasPrefix(rest, name+": ") {
			return line
		}
	}
	t.Fatalf("no %q line in report:\n%s", name, report)
	return ""
}

// doctoringAgent is an agent carrying the free flow.AgentDoctor capability. It
// embeds a stub whose Run records every request, so a test can assert both
// halves of the same property: Doctor was asked, and Run — the only call that
// costs money — was not.
type doctoringAgent struct {
	stubAgent
	err         error
	doctorCalls int
}

func (a *doctoringAgent) Doctor(context.Context) error {
	a.doctorCalls++
	return a.err
}

// spendTrapAgent answers the free check and fails the test outright if anything
// asks it for a turn.
type spendTrapAgent struct {
	t   *testing.T
	err error
}

func (a *spendTrapAgent) Name() string                 { return "trap" }
func (a *spendTrapAgent) Doctor(context.Context) error { return a.err }
func (a *spendTrapAgent) Run(context.Context, flow.AgentRequest) (*flow.AgentResponse, error) {
	a.t.Helper()
	a.t.Errorf("doctor requested an agent turn — a mechanical command must never spend")
	return &flow.AgentResponse{}, nil
}

// runTrapAgent is the same trap without the free capability: the branch that
// must SKIP rather than fall back to spending.
type runTrapAgent struct{ t *testing.T }

func (a *runTrapAgent) Name() string { return "opaque-trap" }
func (a *runTrapAgent) Run(context.Context, flow.AgentRequest) (*flow.AgentResponse, error) {
	a.t.Helper()
	a.t.Errorf("doctor spent a turn on an agent it could not check for free")
	return &flow.AgentResponse{}, nil
}

func newDummyFlow(name string) *flow.Flow {
	f := flow.NewFlow(name, nil)
	f.AddStep("step", "plan", func(flow.StepCtx) error { return nil }, flow.StepConfig{})
	return f
}

// doctor reports the gates and commands the orchestrator declares. They are
// what startup validation checked, and an operator asking why a run refused
// needs to see the same list the check saw.
func TestCmdDoctor_ReportsDeclaredGatesAndCommands(t *testing.T) {
	arena(t)
	app := App{
		Orchestrator: fake.New(),
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:        []*flow.Flow{newDummyFlow("x")},
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := &bytes.Buffer{}
	app.Out = out
	app.Err = &bytes.Buffer{}

	if code := app.cmdDoctor(context.Background(), nil, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	// The two required gates and the required command, by name.
	for _, want := range []string{"integration", "fit", "verify"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor output does not name %q; got %q", want, out.String())
		}
	}
}

// When CarryThrough is set, doctor should print the carry-through caveat.
func TestCmdDoctor_ReportsCarryThrough(t *testing.T) {
	arena(t)
	be := fake.New()
	app := App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:        []*flow.Flow{newDummyFlow("x")},
		CarryThrough: true,
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := &bytes.Buffer{}
	app.Out = out
	app.Err = &bytes.Buffer{}

	code := app.cmdDoctor(context.Background(), nil, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "carry-through: enabled") {
		t.Errorf("doctor output should report carry-through; got %q", out.String())
	}
	if !strings.Contains(out.String(), "not independent review") {
		t.Errorf("doctor output should state the caveat; got %q", out.String())
	}
}

// When CarryThrough is not set, doctor should not mention it.
func TestCmdDoctor_OmitsCarryThroughWhenDisabled(t *testing.T) {
	arena(t)
	be := fake.New()
	app := App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:        []*flow.Flow{newDummyFlow("x")},
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := &bytes.Buffer{}
	app.Out = out
	app.Err = &bytes.Buffer{}

	code := app.cmdDoctor(context.Background(), nil, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.Contains(out.String(), "carry-through") {
		t.Errorf("doctor output should not mention carry-through when disabled; got %q", out.String())
	}
}

// failingBackend wraps the fake backend and forces ListEligible to error so
// cmdDoctor's fallback probe fails.
type failingBackend struct {
	*fake.Orchestrator
	err error
}

func (b *failingBackend) ListAutoSelectable(ctx context.Context, _ []flow.TagId) ([]flow.ItemRef, error) {
	return nil, b.err
}

// The one row of doctor's set that neither the orchestrator's declarations nor
// a gate can answer: an agent that cannot read what the project defines as
// correct produces something plausible instead of something right, and nothing
// notices until review.
func TestCmdDoctor_NormativeDocs(t *testing.T) {
	app, out := doctorApp(t, fake.New(), &stubAgent{name: "stub"})
	if code := app.cmdDoctor(context.Background(), nil, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out.String())
	}
	if line := doctorLine(t, out.String(), "normative docs"); !strings.HasPrefix(line, glyphOK) {
		t.Errorf("docs line should pass in an arena holding documents; got %q", line)
	}
}

func TestCmdDoctor_NormativeDocsMissing(t *testing.T) {
	for _, tc := range []struct{ name, dir string }{
		{"no docs directory", ""},
		{"an empty docs directory", "docs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.dir != "" {
				if err := os.MkdirAll(filepath.Join(dir, tc.dir), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			}
			t.Chdir(dir)
			app := &App{
				Orchestrator: fake.New(),
				Agent:        &stubAgent{name: "stub"},
				Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
				Flows:        []*flow.Flow{newDummyFlow("x")},
			}
			if err := app.validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			out := &bytes.Buffer{}
			app.Out, app.Err = out, &bytes.Buffer{}

			if code := app.cmdDoctor(context.Background(), nil, nil); code != 1 {
				t.Fatalf("exit code = %d, want 1; output:\n%s", code, out.String())
			}
			if line := doctorLine(t, out.String(), "normative docs"); !strings.HasPrefix(line, glyphFail) {
				t.Errorf("docs line should fail; got %q", line)
			}
		})
	}
}

// A missing required gate or command is a failure with the name on the line.
// The checks are asked directly here, one declaration short each, which is how
// an orchestrator implementor debugging their own declarations reaches them.
func TestCmdDoctor_MissingRequiredGateOrCommand(t *testing.T) {
	app := &App{Orchestrator: &declaringOrchestrator{
		Orchestrator: fake.New(),
		gates:        []flow.GateDef{flow.Gate(flow.GateIntegration, true)},
		commands:     []flow.CommandDef{flow.Command(flow.CommandSetup)},
	}}

	gates := app.checkGates()
	if gates.status != checkFail || !strings.Contains(gates.detail, string(flow.GateFit)) {
		t.Errorf("gates check = %+v, want a failure naming %q", gates, flow.GateFit)
	}
	cmds := app.checkCommands()
	if cmds.status != checkFail || !strings.Contains(cmds.detail, string(flow.CommandVerify)) {
		t.Errorf("commands check = %+v, want a failure naming %q", cmds, flow.CommandVerify)
	}
}

// Environment fitness did not move OUT of doctor when it moved out of startup:
// on the clone that reported this — provisioned, tools not built — the binary
// starts, and `doctor` is the command that says why nothing can be claimed
// there. It fails, on the gate and command rows, and each names the repair.
func TestCmdDoctor_FailsOnACloneWhoseToolsAreNotBuilt(t *testing.T) {
	be := &declaringOrchestrator{Orchestrator: fake.New()} // declares nothing
	app, out := doctorApp(t, be, &stubAgent{name: "stub"})

	code := app.cmdDoctor(context.Background(), nil, nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; output:\n%s", code, out.String())
	}
	for name, want := range map[string]string{
		"gates":    string(flow.GateIntegration),
		"commands": string(flow.CommandVerify),
	} {
		line := doctorLine(t, out.String(), name)
		if !strings.HasPrefix(line, glyphFail) || !strings.Contains(line, want) {
			t.Errorf("%s line should fail naming %q; got %q", name, want, line)
		}
		if !strings.Contains(line, "build the project's tools") {
			t.Errorf("%s line should name the repair; got %q", name, line)
		}
	}
}

// What it can run is reported by name, not as a bare "ok": an operator asking
// why a run refused needs to see the same list the refusal saw.
func TestCmdDoctor_ReportsWhatTheOrchestratorCanRun(t *testing.T) {
	app, out := doctorApp(t, fake.New(), &stubAgent{name: "stub"})
	if code := app.cmdDoctor(context.Background(), nil, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out.String())
	}
	if line := doctorLine(t, out.String(), "gates"); !strings.Contains(line, "fit") {
		t.Errorf("gates line should name the declared gates; got %q", line)
	}
	if line := doctorLine(t, out.String(), "commands"); !strings.Contains(line, "verify") {
		t.Errorf("commands line should name the declared commands; got %q", line)
	}
}

// declaringOrchestrator is the fake with a caller-chosen set of declarations,
// counting how often it was asked for them.
//
// The counts are the point of the counters: asking an orchestrator what it can
// run REACHES THE ENVIRONMENT (the github one spawns the project's gate entry
// point), so "was it asked at all" is what distinguishes a startup that checks
// only configuration from one that probes the machine on every invocation.
type declaringOrchestrator struct {
	*fake.Orchestrator
	gates    []flow.GateDef
	commands []flow.CommandDef

	gateCalls    int
	commandCalls int
}

func (o *declaringOrchestrator) SupportedGates() []flow.GateDef {
	o.gateCalls++
	return o.gates
}

func (o *declaringOrchestrator) SupportedCommands() []flow.CommandDef {
	o.commandCalls++
	return o.commands
}

// doctor runs on a machine nothing else will start on. That is the whole point
// of it: a binary that refuses to start says only that it will not start, and
// refusing to run the one command whose job is explaining why would leave an
// operator with nothing else.
func TestCmdDoctor_RunsAndReportsWhenStartupRefused(t *testing.T) {
	app, out := doctorApp(t, fake.New(), &stubAgent{name: "stub"})
	refusal := errors.New("App.Artifacts is empty")

	code := app.cmdDoctor(context.Background(), nil, refusal)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; output:\n%s", code, out.String())
	}
	line := doctorLine(t, out.String(), "startup")
	if !strings.HasPrefix(line, glyphFail) || !strings.Contains(line, "App.Artifacts") {
		t.Errorf("startup line should fail carrying the refusal; got %q", line)
	}
	// And the rest of the report is still produced — an operator fixing an
	// unfit machine wants the whole list, not the first thing that went wrong.
	for _, name := range []string{"orchestrator", "agent", "normative docs"} {
		doctorLine(t, out.String(), name)
	}
}

// A clean startup says so by starting. A line repeating it on every run is one
// an operator learns to skip, and the check set docs/cli.md closes does not
// include it.
func TestCmdDoctor_SaysNothingAboutAStartupThatPassed(t *testing.T) {
	app, out := doctorApp(t, fake.New(), &stubAgent{name: "stub"})
	if code := app.cmdDoctor(context.Background(), nil, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "startup") {
		t.Errorf("a clean startup should not be a line; got:\n%s", out.String())
	}
}

// An App so broken that validation never reached its fields still gets a
// report, not a panic.
func TestCmdDoctor_ReportsAnAppMissingItsParts(t *testing.T) {
	arena(t)
	app := &App{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	out := app.Out.(*bytes.Buffer)

	code := app.cmdDoctor(context.Background(), nil, errors.New("App.Orchestrator is required"))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; output:\n%s", code, out.String())
	}
	for _, name := range []string{"startup", "orchestrator", "agent"} {
		if line := doctorLine(t, out.String(), name); !strings.HasPrefix(line, glyphFail) {
			t.Errorf("%s line should fail; got %q", name, line)
		}
	}
}
