package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/promise-language/flow"
)

// isLeaseItemConflict reports whether err from Backend.Claim is the tracker
// lease ledger's "item already leased to a different arena" refusal. The
// signal travels as the runner's 502 body text (the runner flattens the
// tracker's 409 into 502 Bad Gateway), so we match on the stable error
// suffix from the tracker's ErrItemAlreadyLeased.Error() format string
// (see lease_ledger.go: `item %q already leased to arena %q; incoming
// arena %q refused`).
//
// Used by the auto-select branch to iterate to the next eligible ref on a
// conflict instead of livelock-cycling on the same item.
func isLeaseItemConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already leased to arena")
}

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
// if this arena already holds it); with no argument it resumes the arena's
// active claim. If there is no active claim AND no <item-id> was given, it
// auto-selects refs[0] from Backend.ListEligible — the backend defines the
// ordering. An empty eligible set is a clean exit (0), not an error — there
// is simply no work to do. Each step's result is streamed as JSON so the
// operator sees progress live.
//
// NB: the tracker backend's ListEligible currently returns
// GET /api/items?status=open ordered by UpdatedAt DESC; it does NOT yet
// filter deferred/manual or sort by urgency/priority the way the
// orchestrator's selectEligibleTasks does. Until that mirrors land, the
// auto-select policy is "most-recently-updated open item the arena can
// claim" — fine for development, not yet ready as the production auto loop.
// Tracked separately.
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
			// No id given and arena holds no claim: auto-select the next
			// eligible item. The backend's ListEligible order is the
			// selection policy (see doc comment above) — the CLI just
			// claims whatever it returns first.
			refs, err := app.Backend.ListEligible(ctx)
			if err != nil {
				fmt.Fprintln(app.Err, "resolve:", err)
				return 1
			}
			if len(refs) == 0 {
				fmt.Fprintln(app.Err, "resolve: no active claim and no eligible items")
				return 0
			}
			// Cheap desync: two arenas launched in lockstep would otherwise
			// hit ListEligible + Claim at the same instant on every iteration
			// of `do resolve --auto`. A few hundred ms of jitter staggers
			// them. Not a correctness mechanism — the iteration below handles
			// the conflict regardless — just throughput hygiene.
			time.Sleep(time.Duration(rand.IntN(300)) * time.Millisecond)
			// Multiple arenas running `do resolve --auto` see the same
			// refs[0] from the eligibility mirror (T0451) and race on Claim.
			// The loser must fall through to the next ref instead of
			// livelock-cycling on the same item. Only "item already leased
			// to another arena" is retryable — other errors (network, auth,
			// the arena's own bijection refusal) still exit 1.
			var newClaim flow.Claim
			claimed := false
			for i, ref := range refs {
				fmt.Fprintf(app.Err, "resolve: no active claim — auto-selecting %s (%d/%d)\n", ref.Display, i+1, len(refs))
				c, err := app.Backend.Claim(ctx, ref, app.Owner)
				if err == nil {
					newClaim = c
					claimed = true
					break
				}
				if isLeaseItemConflict(err) {
					fmt.Fprintf(app.Err, "resolve: %s already leased by another arena — trying next\n", ref.Display)
					continue
				}
				fmt.Fprintln(app.Err, "resolve:", err)
				return 1
			}
			if !claimed {
				fmt.Fprintln(app.Err, "resolve: every eligible item is leased to another arena — nothing to do")
				return 0
			}
			claim = &newClaim
		} else {
			claim = c
		}
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
