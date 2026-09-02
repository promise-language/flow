package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/promise-language/flow"
)

// dispatchedByRunnerEnv is the env-var the runner's spawnFlow sets to "1" when
// it spawns this binary on behalf of the orchestrator. Its ABSENCE in the
// child env is the cli's signal that a run-step was invoked directly by the
// operator (manual takeover). Hardcoded here rather than imported from the
// flow-sdk to keep the OSS cli free of tracker-specific dependencies — the
// tracker-side constant (flowsdk.EnvDispatchedByRunner) and this string MUST
// stay in sync (T0481).
const dispatchedByRunnerEnv = "FLOW_DISPATCHED_BY_RUNNER"

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
	claim, err := app.Backend.LookupActiveClaim(ctx, app.Owner)
	if err != nil {
		fmt.Fprintln(app.Err, "run-step:", err)
		return 1
	}
	if claim == nil {
		fmt.Fprintln(app.Err, "run-step: no active claim (run `claim <id>` first)")
		return 1
	}

	// T0481: operator-driven run-step asserts manual control over the item.
	// The runner's spawnFlow sets FLOW_DISPATCHED_BY_RUNNER=1; its absence
	// means the operator typed this command, so apply the backend's takeover
	// side effects (set Manual, resolve any FlowPark). Best-effort: a takeover
	// failure surfaces a warning but doesn't block the step — the user's
	// intent is the step itself, the takeover is bookkeeping.
	if os.Getenv(dispatchedByRunnerEnv) != "1" {
		if tx, ok := app.Backend.(flow.ManualTakeover); ok {
			if terr := tx.MarkManualTakeover(ctx, *claim); terr != nil {
				fmt.Fprintln(app.Err, "run-step: manual takeover (continuing):", terr)
			}
		}
	}

	res, err := RunOne(ctx, app, *claim)
	if err != nil {
		fmt.Fprintln(app.Err, "run-step:", err)
		return 1
	}

	switch mode {
	case OutputJSON:
		enc := json.NewEncoder(app.Out)
		if err := enc.Encode(res); err != nil {
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
