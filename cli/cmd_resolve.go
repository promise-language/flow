package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/promise-language/flow"
)

// maxResolveSteps backstops cmdResolve's loop. A healthy flow advances through
// its steps and finalizes well under this; the cap only trips on a step that
// neither progresses (resolves/parks/fails) nor finalizes — i.e. a bug, not a
// long-but-legitimate run. It is a runaway guard, NOT a timeout: it bounds the
// number of step ATTEMPTS, not wall-clock, so it never false-kills a slow
// agent turn. When hit, the message says so and points at `status`.
const maxResolveSteps = 50

// cmdResolve drives the FULL lifecycle: it repeatedly advances the item one
// step at a time (the same RunOne the orchestrator runs in production) until
// the item is FINALIZED (no eligible flow remains) or the run stops — a step
// parked (asked a question / hit a budget cap / timed out), was skipped
// (preflight refusal, e.g. an already-finalized item), or failed.
//
// With an explicit <item-id> it claims that item first (idempotent re-acquire
// if this arena already holds it); with no argument it resolves the arena's
// active claim. Each step's result is streamed as JSON so the operator sees
// progress live.
func (app *App) cmdResolve(ctx context.Context, args []string) int {
	fs := app.newFlagSet("resolve")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(app.Err, "resolve: unexpected argument %q (resolve takes an optional item id)\n", fs.Arg(1))
		return 2
	}

	var claim *flow.Claim
	if fs.NArg() == 1 {
		ref, err := app.resolveClaimRef(ctx, fs.Arg(0))
		if err != nil {
			fmt.Fprintln(app.Err, "resolve:", err)
			return 1
		}
		c, err := app.Backend.Claim(ctx, ref, app.Owner)
		if err != nil {
			fmt.Fprintln(app.Err, "resolve:", err)
			return 1
		}
		claim = &c
	} else {
		c, err := app.Backend.LookupActiveClaim(ctx, app.Owner)
		if err != nil {
			fmt.Fprintln(app.Err, "resolve:", err)
			return 1
		}
		if c == nil {
			fmt.Fprintln(app.Err, "resolve: no active claim (give an item id: `resolve <id>`)")
			return 1
		}
		claim = c
	}

	// Progress goes to Err so stdout stays a clean machine-readable stream of
	// per-step InvocationResult JSON. A single agent step can run for many
	// minutes; without these lines the silence reads as a hang and invites the
	// operator to kill a healthy run. We announce each step BEFORE running it
	// (so the long pause is attributed to a named step) and report the outcome
	// after.
	fmt.Fprintf(app.Err, "resolve: driving %s to completion (until finalized or parked)…\n", claim.ItemRef.Display)
	enc := json.NewEncoder(app.Out)
	step := 0
	for range maxResolveSteps {
		step++
		// Best-effort peek so we can name the step we're about to run. RunOne
		// re-derives this itself; the peek is purely for the progress line and
		// never gates execution (a transient read error just skips the label).
		if st, serr := app.Backend.LoadState(ctx, *claim); serr == nil {
			if f, next := SelectFlow(app, st); f != nil {
				fmt.Fprintf(app.Err, "resolve: [step %d] running %q…\n", step, next)
			} else {
				fmt.Fprintf(app.Err, "resolve: [step %d] no step eligible — finalizing…\n", step)
			}
		}

		res, err := RunOne(ctx, app, *claim)
		if err != nil {
			fmt.Fprintln(app.Err, "resolve:", err)
			return 1
		}
		_ = enc.Encode(res)

		label := res.Step
		if label == "" {
			label = "(finalize)"
		}
		outcome := fmt.Sprintf("resolve: [step %d] %s → %s", step, label, res.Status)
		if res.Reason != "" {
			outcome += " — " + res.Reason
		}
		fmt.Fprintln(app.Err, outcome)

		switch res.Status {
		case "failed":
			fmt.Fprintf(app.Err, "resolve: %s stopped on a failed step\n", claim.ItemRef.Display)
			return 1
		case "parked", "skipped":
			// Parked (question/budget/timeout) or skipped (preflight refusal,
			// e.g. an already-finalized item). Stop and let the operator act.
			fmt.Fprintf(app.Err, "resolve: %s %s — run `status %s` to inspect\n", claim.ItemRef.Display, res.Status, claim.ItemRef.Display)
			return 0
		case "done":
			// Finalize case: RunOne ran no step (empty Step) because no eligible
			// flow remained — the item is fully resolved.
			if res.Step == "" {
				fmt.Fprintf(app.Err, "resolve: %s finalized ✓\n", claim.ItemRef.Display)
				return 0
			}
			// Otherwise a step advanced; loop to run the next one.
		}
	}
	fmt.Fprintf(app.Err, "resolve: stopped after %d step attempts without finalizing (runaway guard); run `status` to inspect\n", maxResolveSteps)
	return 1
}
