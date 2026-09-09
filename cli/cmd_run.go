package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/promise-language/flow"
)

func (app *App) cmdRun(ctx context.Context, args []string) int {
	fs := app.newFlagSet("run-step")
	of := addOutputFlags(fs)
	if !app.parseArgs(fs, args) {
		return 2
	}
	if fs.NArg() > 0 {
		app.usageError("run-step: unexpected argument %q (this command takes no arguments)", fs.Arg(0))
		return 2
	}
	mode, ok := of.mode(app, "run-step")
	if !ok {
		return 2
	}
	claim, err := app.Orchestrator.LookupActiveClaim(ctx)
	if err != nil {
		return app.refuseArena(mode, "", err.Error())
	}
	// The one check this command owes: does this arena hold a claim at all?
	// Without one it does not even know which item to run against. Detecting
	// that the claim has since been LOST is the dispatcher's read, before it
	// dispatches — not this command's, and never mid-run.
	if claim == nil {
		return app.refuseArena(mode, "", "no active claim (run `claim <id>` first)")
	}

	res, err := RunOne(ctx, app, *claim)
	if err != nil {
		return app.refuseArena(mode, claim.ItemRef.Display, err.Error())
	}

	switch mode {
	case OutputJSON:
		if err := app.writeStepJSON(res); err != nil {
			fmt.Fprintln(app.Err, "run-step: encode result:", err)
			return 1
		}
	default:
		line := fmt.Sprintf("%s → %s", res.Step, res.Status)
		if res.Reason != "" {
			line += " — " + res.Reason
		}
		if suffix := formatResultSuffix(res); suffix != "" {
			line += " " + suffix
		}
		if res.Park != nil && len(res.Park.Axes) > 0 {
			line += "\n  axes: " + flow.FormatAxes(res.Park.Axes)
		}
		if bl := blockedByLine(openBlockers(res.BlockKind, res.BlockedBy)); bl != "" {
			line += "\n  " + bl
		}
		fmt.Fprintln(app.Out, line)
	}
	switch res.Status {
	// "blocked" is a stop that needs a human, not a failure of the run — but
	// it must not exit 0, or a caller waiting on the flow to progress would
	// read "nothing to do" and keep re-running it forever.
	case "failed", "blocked":
		return 1
	default:
		return 0
	}
}

// writeStepJSON is the one encoder for run-step's machine channel: compact,
// one object per line — the shape resolve's stream carries, so a caller
// driving one step at a time parses what a caller reading resolve parses.
func (app *App) writeStepJSON(res flow.InvocationResult) error {
	return json.NewEncoder(app.Out).Encode(res)
}

// refuseArena reports a run-step that never reached a step. Same object, same
// stream, every outcome (docs/cli.md § One-shot reports). The scope is always
// the arena: run-step's refusals are about this checkout, and a lost claim is
// not among them — that check belongs to whoever dispatches, before it
// dispatches, and is not re-read mid-run.
func (app *App) refuseArena(mode OutputMode, item, reason string) int {
	fmt.Fprintln(app.Err, "run-step: "+reason)
	if mode == OutputJSON {
		arena := false
		// "failed", not "skipped": docs/cli.md § Exit codes gives 1 to a
		// command that could not complete, while `skipped` exits 0 and would
		// invite a caller to loop on it forever.
		if err := app.writeStepJSON(flow.InvocationResult{
			Item:       item,
			Status:     string(flow.StatusFailed),
			Reason:     reason,
			ItemScoped: &arena,
		}); err != nil {
			fmt.Fprintln(app.Err, "run-step: encode result:", err)
		}
	}
	return 1
}
