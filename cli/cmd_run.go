package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/promise-language/flow"
)

// Refusal codes `run-step` raises itself, before any step is dispatched. They
// join the orchestrator's own codes in one vocabulary because a caller reads
// them from one field and cannot tell which layer produced them — nor should
// it have to.
const (
	// refusalNoClaim: this arena holds no claim, so there is no item to
	// advance. Arena-scoped by construction: the condition names no item, so
	// "try a different one" is not a move a caller could make.
	refusalNoClaim flow.ClaimRefusalCode = "no-claim"
	// refusalLeaseUnreadable: the lease exists and cannot be read. Also
	// arena-scoped, and for a stronger reason than no-claim — a lease store
	// that cannot answer is a broken arena, and every item would meet it.
	refusalLeaseUnreadable flow.ClaimRefusalCode = "lease-unreadable"
	// refusalStepFailed: the advance itself could not complete, for something
	// that was not a typed refusal. Arena-scoped because the cause is unknown:
	// a caller that moved to the next item on an unclassified failure would
	// walk its whole queue through the same broken arena.
	refusalStepFailed flow.ClaimRefusalCode = "step-failed"
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
		return app.reportRun(mode, flow.InvocationResult{
			Status: string(flow.StatusFailed),
			Reason: "active claim could not be read",
			Refusal: &flow.Refusal{
				Code:   string(refusalLeaseUnreadable),
				Reason: "active claim could not be read",
				Detail: err.Error(),
			},
		})
	}
	// The ONLY claim check run-step makes, and it is local. Whether the lease
	// still stands on the server is not asked here: nothing re-reads it while a
	// step runs, and the party that decided to dispatch this step already knew
	// the item and could see its holder — pushing that read in here puts it
	// after the decision it should have informed, and pays for it per step.
	//
	// What is left is the question this command cannot proceed without: an
	// arena holding no claim does not know which item to advance.
	if claim == nil {
		return app.reportRun(mode, flow.InvocationResult{
			Status: string(flow.StatusFailed),
			Reason: "no active claim",
			Refusal: &flow.Refusal{
				Code:     string(refusalNoClaim),
				Reason:   "no active claim (run `claim <id>` first)",
				Override: "",
			},
		})
	}

	res, err := RunOne(ctx, app, *claim)
	if err != nil {
		// A typed refusal keeps its own scope; anything else is reported with
		// the conservative one. RefusalOf returns nil for an untyped error, so
		// the fallback is built here rather than guessed at there.
		refusal := flow.RefusalOf(err)
		if refusal == nil {
			refusal = &flow.Refusal{
				Code:   string(refusalStepFailed),
				Reason: err.Error(),
			}
		}
		return app.reportRun(mode, flow.InvocationResult{
			Item:    claim.ItemRef.Display,
			Status:  string(flow.StatusFailed),
			Reason:  refusal.Reason,
			Refusal: refusal,
		})
	}

	return app.reportRun(mode, res)
}

// reportRun writes the one report this command produces and returns the exit
// code for it.
//
// EVERY outcome comes through here, a refusal included. `run-step` is a
// one-shot report and the report IS the output (docs/cli.md § Output), so a
// failure that printed prose to stderr instead would be the same event told
// two ways — and would leave a caller sequencing steps itself with nothing to
// branch on but the text.
func (app *App) reportRun(mode OutputMode, res flow.InvocationResult) int {
	switch mode {
	case OutputJSON:
		enc := json.NewEncoder(app.Out)
		if err := enc.Encode(res); err != nil {
			// The report itself could not be written. There is nowhere left to
			// put a structured answer, so this one line is the exception that
			// proves the rule above.
			fmt.Fprintln(app.Err, "run-step: encode result:", err)
			return 1
		}
	default:
		fmt.Fprintln(app.Out, humanRunLine(res))
	}
	switch res.Status {
	// "blocked" is a stop that needs a human, not a failure of the run — but
	// it must not exit 0, or a caller waiting on the flow to progress would
	// read "nothing to do" and keep re-running it forever.
	case string(flow.StatusFailed), string(flow.StatusBlocked):
		return 1
	default:
		return 0
	}
}

// humanRunLine renders one result for a person.
func humanRunLine(res flow.InvocationResult) string {
	// A refusal has no step to name, so it leads with what refused it and why.
	// The scope is spelled out rather than printed as a bare bool: "this arena"
	// versus "this item" is the whole of what a reader needs from it.
	if res.Refusal != nil {
		line := fmt.Sprintf("run-step refused (%s) — %s", res.Refusal.Code, res.Refusal.Reason)
		if res.Refusal.ItemScoped {
			line += "\n  scope: this item; another item may succeed"
		} else {
			line += "\n  scope: this arena; no item will succeed until it is cleared"
		}
		if res.Refusal.Detail != "" {
			line += "\n  " + res.Refusal.Detail
		}
		if res.Refusal.Override != "" {
			line += "\n  override: " + res.Refusal.Override
		}
		return line
	}
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
	return line
}
