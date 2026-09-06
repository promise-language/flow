package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/promise-language/flow"
)

// Output glyphs visually distinct from the rest of the SDK output so the
// terminal scanline conveys pass/fail at a glance.
const (
	glyphOK   = "✅" // ✅ white heavy check mark on green square
	glyphFail = "❌" // ❌ heavy multiplication X
	glyphSkip = "➖" // ➖ heavy minus — the check could not be made
)

// Doctor is the optional preflight hook an Orchestrator may implement. If the
// configured Orchestrator satisfies this, `doctor` calls it; otherwise the
// command does a minimal end-to-end check (list auto-selectable items).
type Doctor interface {
	Doctor(ctx context.Context) error
}

// checkStatus is one check's outcome. A skip is neither pass nor fail: the SDK
// could not make the check, which is a fact about what it was given and not
// about this machine — so it does not affect the exit code.
type checkStatus int

const (
	checkPass checkStatus = iota
	checkFail
	checkSkip
)

// check is one line of doctor's report.
type check struct {
	name   string
	detail string
	status checkStatus
}

func (c check) glyph() string {
	switch c.status {
	case checkFail:
		return glyphFail
	case checkSkip:
		return glyphSkip
	}
	return glyphOK
}

// cmdDoctor answers one question: is this environment fit to be given an item?
//
// It reports EVERY check rather than stopping at the first failure — an
// operator fixing an unfit machine wants the whole list in one pass, not one
// failure per round-trip.
//
// IT SPENDS NOTHING AND MUTATES NOTHING. Both properties are load-bearing:
// doctor runs before every item, in CI, and on machines that are mid-item, so a
// check that bills the account or touches the tree is one an operator learns
// not to run — and a preflight nobody runs prevents nothing.
func (app *App) cmdDoctor(ctx context.Context, args []string, startupErr error) int {
	if !app.rejectArgs("doctor", args) {
		return 2
	}

	// `doctor` runs on machines nothing else will start on — that is the point
	// of it — so a startup refusal is reported as the first check rather than
	// being the reason there is no report. It appears ONLY when there is one:
	// a binary that started said so by starting, and a line repeating it on
	// every clean run is a line an operator learns to skip. What follows it is whatever can
	// still be asked: an App that failed validation may be missing the very
	// things the other checks call, so each one that would dereference
	// something absent says so instead.
	var checks []check
	if startupErr != nil {
		checks = append(checks, check{name: "startup", status: checkFail, detail: startupErr.Error()})
	}
	if app.Orchestrator == nil {
		checks = append(checks, check{name: "orchestrator", status: checkFail,
			detail: "no orchestrator is configured (App.Orchestrator is nil)"})
	} else {
		checks = append(checks, app.checkOrchestrator(ctx))
	}
	if app.Agent == nil {
		checks = append(checks, check{name: "agent", status: checkFail,
			detail: "no agent is configured (App.Agent is nil)"})
	} else {
		checks = append(checks, app.checkAgent(ctx))
	}
	if app.Orchestrator != nil {
		checks = append(checks, app.checkCommands(), app.checkGates())
	}
	checks = append(checks, checkDocs())

	// The whole report is one list, so it goes to one stream. docs/cli.md
	// § "One-shot reports" puts doctor's report on stdout.
	failed := false
	for _, c := range checks {
		fmt.Fprintf(app.Out, "%s %s: %s\n", c.glyph(), c.name, c.detail)
		if c.status == checkFail {
			failed = true
		}
	}
	app.reportCapabilities()
	if failed {
		return 1
	}
	return 0
}

// checkOrchestrator is the connectivity check: the orchestrator's own Doctor
// hook when it has one, else ListAutoSelectable as a probe.
func (app *App) checkOrchestrator(ctx context.Context) check {
	const name = "orchestrator"
	if d, ok := app.Orchestrator.(Doctor); ok {
		if err := d.Doctor(ctx); err != nil {
			return check{name: name, status: checkFail, detail: err.Error()}
		}
		return check{name: name, detail: fmt.Sprintf("%s reachable and usable", app.Orchestrator.Name())}
	}
	if _, err := app.Orchestrator.ListAutoSelectable(ctx, nil); err != nil {
		return check{name: name, status: checkFail,
			detail: fmt.Sprintf("orchestrator.ListAutoSelectable failed: %s", err)}
	}
	return check{name: name, detail: fmt.Sprintf(
		"%s reachable (no orchestrator Doctor probe; probed via ListAutoSelectable)", app.Orchestrator.Name())}
}

// checkAgent asks the agent whether it can be invoked, and BUYS NOTHING.
//
// The check used to be a probe turn — one tool-free question, capped at fifty
// cents — on the reasoning that a binary which exists and rejects this SDK's
// argv is exactly as broken as a missing one, and that only a real invocation
// tells them apart. The reasoning was sound and the conclusion was still wrong:
// `doctor` is mechanical, it runs before every item and on every machine an
// operator touches, and a preflight that bills the account for each run is one
// that gets turned off. A check that is turned off prevents nothing, so the
// probe turn was not a cheap turn — it was the whole command's usefulness,
// spent by the run. See docs/agent.md § Nothing mechanical may spend.
//
// So the check is the agent's own flow.AgentDoctor hook, which reports for free
// whether this SDK could start it: the reference impl spawns the binary and
// asks its version, which catches an absent, unexecutable, wrong-architecture
// or too-old install without a model call. An agent that offers no such hook
// SKIPS — the SDK cannot check a black-box Agent without spending, and that is
// a fact about the interface, not about this machine.
//
// What no free check settles is whether a turn would SUCCEED: credentials,
// quota and model availability are answered only by spending. The detail line
// says what was checked, so the report never implies more than it looked at.
func (app *App) checkAgent(ctx context.Context) check {
	const name = "agent"
	d, ok := app.agentImpl().(flow.AgentDoctor)
	if !ok {
		return check{name: name, status: checkSkip, detail: fmt.Sprintf(
			"agent %q cannot be checked without spending a turn (no AgentDoctor)", app.Agent.Name())}
	}
	if err := d.Doctor(ctx); err != nil {
		return check{name: name, status: checkFail,
			detail: fmt.Sprintf("%s could not be invoked: %s", app.Agent.Name(), err)}
	}
	return check{name: name, detail: fmt.Sprintf(
		"%s can be invoked (checked without a turn; credentials and quota are not)", app.Agent.Name())}
}

// checkCommands and checkGates ask the orchestrator what it can run.
//
// A step finishes its work and goes to verify it; a gate is asked for a
// measurement. Either failing on "there is no such thing here" costs the whole
// step that reached it, and the report names which one is absent rather than
// leaving an operator to infer it from a mid-item error.
//
// THE DECLARED LIST IS THE CHECK, and it is not a static claim: an orchestrator
// derives what it can run from the machine it runs on — listing the command
// binaries it finds, asking the gate entry point which gates it supports. So
// `verify` missing from SupportedCommands() IS "the verify command is not on
// this machine", and `fit` missing from SupportedGates() IS "the gate entry
// point is absent, or could not say". The SDK does not need to know where
// either lives to check that both are there, and a second copy of paths only
// the orchestrator knows is exactly the thing that goes stale.
//
// It is also why `doctor` does not run either one. What running would add over
// the declaration is a machine-level fact the orchestrator has already
// established — and the verify command may modify the tree, which doctor must
// not do on a machine that is mid-item.
//
// Which required declarations are absent is asked through missingGates /
// missingCommands, the same computation the boundary refusal in
// requireRunnable uses. Only the message differs, and it differs on purpose:
// this is a line in a report a person is reading top to bottom, that is the
// reason a command they asked for did not run.
func (app *App) checkCommands() check {
	const name = "commands"
	declared := app.Orchestrator.SupportedCommands()
	if missing := missingCommands(declared); len(missing) > 0 {
		return check{name: name, status: checkFail, detail: fmt.Sprintf(
			"%s does not declare the required command(s): %s — %s",
			app.Orchestrator.Name(), joinNames(missing), repairUnbuiltTools)}
	}
	names := make([]string, 0, len(declared))
	for _, c := range declared {
		names = append(names, string(c.Name))
	}
	return check{name: name, detail: strings.Join(names, ", ")}
}

func (app *App) checkGates() check {
	const name = "gates"
	declared := app.Orchestrator.SupportedGates()
	if missing := missingGates(declared); len(missing) > 0 {
		return check{name: name, status: checkFail, detail: fmt.Sprintf(
			"%s does not declare the required gate(s): %s — %s",
			app.Orchestrator.Name(), joinNames(missing), repairUnbuiltTools)}
	}
	names := make([]string, 0, len(declared))
	for _, g := range declared {
		names = append(names, string(g.Name))
	}
	return check{name: name, detail: strings.Join(names, ", ")}
}

// checkDocs checks that the project's normative documentation is present: a
// `docs/` directory, in the arena, holding at least one document.
//
// The arena is the directory the process runs in — that is what an arena IS in
// this contract, so no orchestrator has to report it and no claim is needed to
// look.
//
// It is the one row of doctor's set that nothing else can answer. What an
// orchestrator declares is what it can RUN; what a gate measures is the tree's
// soundness. Neither notices that an agent is about to work on a project whose
// definition of correct it cannot read — and that failure is invisible until
// review, because what comes back is plausible rather than wrong.
func checkDocs() check {
	const name = "normative docs"
	dir, err := os.Getwd()
	if err != nil {
		return check{name: name, status: checkSkip,
			detail: fmt.Sprintf("cannot read the arena's directory: %s", err)}
	}
	docs := filepath.Join(dir, "docs")
	entries, err := os.ReadDir(docs)
	if err != nil {
		return check{name: name, status: checkFail, detail: fmt.Sprintf("cannot read %s: %s", docs, err)}
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			count++
		}
	}
	if count == 0 {
		return check{name: name, status: checkFail,
			detail: fmt.Sprintf("%s holds no document (no top-level *.md)", docs)}
	}
	return check{name: name, detail: fmt.Sprintf("%s holds %d document(s)", docs, count)}
}

// reportCapabilities prints what does not belong on a check line: a mode this
// binary was started in, which is neither pass nor fail.
//
// The gate and command lists used to be printed here. They are checks now — the
// same facts, but with a verdict attached — and printing them twice would leave
// a reader deciding which copy to believe.
func (app *App) reportCapabilities() {
	if app.CarryThrough {
		fmt.Fprintf(app.Out, "  carry-through: enabled — carries to merge (not independent review)\n")
	}
}
