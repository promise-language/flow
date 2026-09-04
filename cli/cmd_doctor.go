package cli

import (
	"context"
	"fmt"
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
func (app *App) cmdDoctor(ctx context.Context, args []string) int {
	if !app.rejectArgs("doctor", args) {
		return 2
	}

	checks := []check{app.checkOrchestrator(ctx), app.checkAgent(ctx)}

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

// reportCapabilities prints what this orchestrator declares.
//
// There is nothing here about OPTIONAL capabilities any more: every method is
// required, so the only thing worth reporting is what it says it can run.
func (app *App) reportCapabilities() {
	gates := app.Orchestrator.SupportedGates()
	names := make([]string, 0, len(gates))
	for _, g := range gates {
		names = append(names, string(g.Name))
	}
	fmt.Fprintf(app.Out, "  gates: %s\n", strings.Join(names, ", "))
	cmds := app.Orchestrator.SupportedCommands()
	cnames := make([]string, 0, len(cmds))
	for _, c := range cmds {
		cnames = append(cnames, string(c.Name))
	}
	fmt.Fprintf(app.Out, "  commands: %s\n", strings.Join(cnames, ", "))
	if app.CarryThrough {
		fmt.Fprintf(app.Out, "  carry-through: enabled — carries to merge (not independent review)\n")
	}
}
