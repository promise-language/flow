package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/promise-language/flow"
)

// maxResolveSteps backstops cmdResolve's loop. A healthy flow advances through
// its steps and finalizes well under this; the cap only trips on a step that
// neither progresses (resolves/parks/fails) nor finalizes — i.e. a bug, not a
// long-but-legitimate run. It is a runaway guard, NOT a timeout: it bounds the
// number of step ATTEMPTS, not wall-clock, so it never false-kills a slow
// agent turn. When hit, the message says so and points at `status`.
const maxResolveSteps = 50

// maxFitnessWaits bounds how many times cmdResolve re-measures a machine
// fitness condition before giving up. The bound protects the claim: an item
// held indefinitely on a machine nobody is fixing is one no other machine
// can take (docs/environment.md). Exhausting the bound is still "unfit",
// never a refusal (gates-and-commands.md: "A wait bound is not a verdict").
const maxFitnessWaits = 20

// fitnessWaitInterval is the delay between fitness re-measurements in
// cmdResolve's standalone wait loop. Short enough to resume quickly when
// the condition clears; long enough to avoid busy-polling.
// Var (not const) so tests can shorten it without sleeping 30 s per round.
var fitnessWaitInterval = 30 * time.Second

// cmdResolve drives the FULL lifecycle: it repeatedly advances the item one
// step at a time (the same RunOne the orchestrator runs in production) until
// the item is FINALIZED (no eligible flow remains) or the run stops — a step
// parked (asked a question / hit a budget cap / timed out), was skipped
// (preflight refusal, e.g. an already-finalized item), or failed.
//
// With an explicit <item-id> it claims that item first (idempotent re-acquire
// if this arena already holds it); with no argument it resumes the arena's
// active claim. If there is no active claim AND no <item-id> was given, it
// auto-selects refs[0] from Backend.ListEligible — the tracker backend's
// ListEligible mirrors the orchestrator's selectEligibleTasks (same per-item
// filter and same leased/urgency/priority sort), so the CLI picks the same
// "next" the orchestrator would. An empty eligible set is a clean exit (0),
// not an error — there is simply no work to do. Progress is narrated on Err in
// both output modes; in JSON mode each step's result is also streamed to Out.
func (app *App) cmdResolve(ctx context.Context, args []string) int {
	fs := app.newFlagSet("resolve")
	of := addOutputFlags(fs)
	forceUnadmitted := fs.Bool("force-unadmitted", false, "override the arena admission check (audited)")
	paceFiveHour := fs.Float64("pace-five-hour", 90, "target % of elapsed for 5h window (0=no pacing, 100=raw elapsed)")
	paceSevenDay := fs.Float64("pace-seven-day", 95, "target % of elapsed for 7d window (0=no pacing, 100=raw elapsed)")
	var tags stringSliceFlag
	fs.Var(&tags, "tag", "filter eligible set by tag (repeatable, conjunctive)")
	if !app.parseArgs(fs, args) {
		return 2
	}
	if fs.NArg() > 1 {
		return app.usageError("resolve: unexpected argument %q (resolve takes an optional item id)", fs.Arg(1))
	}
	// Naming an item id AND a tag is a usage error: the id already answers
	// the question the tag would ask.
	if fs.NArg() == 1 && len(tags) > 0 {
		return app.usageError("resolve: --tag and an explicit item id are mutually exclusive")
	}
	// Decided before any claim work: a contradictory --json --human must exit 2
	// without leasing anything.
	mode, ok := of.mode(app, "resolve")
	if !ok {
		return 2
	}

	var resolveOverrides []flow.ClaimOverride
	if *forceUnadmitted {
		resolveOverrides = append(resolveOverrides, flow.OverrideUnadmitted)
	}

	// Pre-claim fitness check: an unfit machine should not take an item at
	// all. Standalone drives its own lifecycle and holds
	// (docs/environment.md): the wait is bounded and re-measures rather
	// than assuming the condition persists.
	// Fail-open: err != nil proceeds (cannot check → do not refuse).
	if fc, ok := app.Backend.(flow.FitnessChecker); ok {
		for fitnessWaits := 0; ; fitnessWaits++ {
			verdict, err := fc.CheckFit(ctx)
			if err != nil || verdict.Acceptable {
				break // fit (or cannot check → fail-open)
			}
			if fitnessWaits >= maxFitnessWaits {
				fmt.Fprintf(app.Err, "resolve: machine unfit — %s\n", verdict.Detail)
				return 1
			}
			fmt.Fprintf(app.Err, "resolve: machine unfit (%d/%d) — %s — waiting…\n",
				fitnessWaits+1, maxFitnessWaits, verdict.Detail)
			select {
			case <-time.After(fitnessWaitInterval):
			case <-ctx.Done():
				fmt.Fprintln(app.Err, "resolve: interrupted while waiting for fitness")
				return 1
			}
		}
	}

	var claim *flow.Claim
	if fs.NArg() == 1 {
		ref, err := app.resolveClaimRef(ctx, fs.Arg(0))
		if err != nil {
			fmt.Fprintln(app.Err, "resolve:", err)
			return 1
		}
		c, err := app.Backend.Claim(ctx, ref, app.Owner, resolveOverrides)
		if err != nil {
			var refused flow.ErrClaimRefused
			if errors.As(err, &refused) {
				fmt.Fprintln(app.Err, formatClaimRefusal("resolve", refused))
				return 1
			}
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
			//
			// When --tag is specified, use the TagFilterer interface to
			// narrow the eligible set server-side. Without TagFilterer,
			// --tag is refused.
			var refs []flow.ItemRef
			var err error
			if len(tags) > 0 {
				tf, ok := app.Backend.(flow.TagFilterer)
				if !ok {
					fmt.Fprintln(app.Err, "resolve: this backend does not support --tag (no TagFilterer capability)")
					return 1
				}
				refs, err = tf.ListEligibleWithTags(ctx, tags)
			} else {
				refs, err = app.Backend.ListEligible(ctx)
			}
			if err != nil {
				fmt.Fprintln(app.Err, "resolve:", err)
				return 1
			}
			if len(refs) == 0 {
				if len(tags) > 0 {
					fmt.Fprintf(app.Err, "resolve: no active claim and no eligible items carrying tags %v\n", []string(tags))
				} else {
					fmt.Fprintln(app.Err, "resolve: no active claim and no items in the auto-selectable set (eligible items matching this binary's label and assignee)")
				}
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
				c, err := app.Backend.Claim(ctx, ref, app.Owner, resolveOverrides)
				if err == nil {
					newClaim = c
					claimed = true
					break
				}
				var refused flow.ErrClaimRefused
				if errors.As(err, &refused) && refused.ItemScoped {
					fmt.Fprintf(app.Err, "resolve: %s — %s — trying next\n", ref.Display, refused.Reason)
					continue
				}
				if errors.As(err, &refused) {
					fmt.Fprintln(app.Err, formatClaimRefusal("resolve", refused))
					return 1
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

	// Progress goes to Err in BOTH modes, so stdout carries per-step
	// InvocationResult objects and nothing else — and in human mode nothing at
	// all. That split is what makes `resolve > steps.json` do the obvious
	// thing (JSON accumulating in the file, progress still on the terminal)
	// and lets `resolve --json 2>/dev/null` yield the machine stream alone.
	// The mode is decided by stdout, never by stderr: bare `resolve
	// 2>/dev/null` on a terminal prints nothing at all, because a terminal
	// stdout selects human.
	//
	// A single agent step can run for many minutes; without these lines the
	// silence reads as a hang and invites the operator to kill a healthy run.
	// We announce each step BEFORE running it (so the long pause is attributed
	// to a named step) and report the outcome after.
	fmt.Fprintf(app.Err, "resolve: driving %s to completion (until finalized or parked)…\n", claim.ItemRef.Display)

	isRunner := os.Getenv(dispatchedByRunnerEnv) == "1"
	targets := paceTargets{FiveHour: *paceFiveHour / 100, SevenDay: *paceSevenDay / 100}

	if !isRunner {
		reportQuota(app.Err)
	}

	enc := json.NewEncoder(app.Out)
	fitnessWaits := 0
	quotaWarned := false
	for range maxResolveSteps {
		// Pace against subscription quota. The check is before dispatch so the
		// delay costs nothing that is in flight.
		if targets.FiveHour > 0 || targets.SevenDay > 0 {
			if usage, qerr := readQuota(); qerr == nil {
				if d := paceDelay(usage, targets, time.Now()); d > 0 {
					fmt.Fprintf(app.Err, "resolve: pacing — waiting %s for quota headroom\n", formatDurationCompact(d))
					select {
					case <-time.After(d):
					case <-ctx.Done():
						fmt.Fprintln(app.Err, "resolve: interrupted while pacing")
						return 1
					}
				}
			} else if !quotaWarned {
				fmt.Fprintf(app.Err, "resolve: ⚠ quota unreadable — %s — pacing disabled\n", qerr)
				quotaWarned = true
			}
		}

		// Best-effort peek so we can name the step we're about to run. RunOne
		// re-derives this itself; the peek is purely for the progress line and
		// never gates execution (a transient read error just skips the label).
		// We name the step rather than number it: a positional counter ("[step
		// 1]") collides with the flow's own named steps (plan/implement/…) and
		// misreads as "flow step 1" when it really means "resolve iteration 1".
		if st, serr := app.Backend.LoadState(ctx, *claim); serr == nil {
			if f, next := SelectFlow(app, st); f != nil {
				fmt.Fprintf(app.Err, "resolve: running %q…\n", next)
			} else if !st.Item.Finalized && flowForType(app, st.Item.Type) == nil {
				// No flow accepts the type, so RunOne will block rather than
				// finalize. Announcing "finalizing…" here would tell the
				// operator the run is completing right before it reports that
				// nothing ever started. The finalized exemption is RunOne's, and
				// is repeated here for the same reason the branch exists: an
				// already-finalized item DOES take the finalize path, so saying
				// otherwise about it would be the same misreport inverted.
				fmt.Fprintf(app.Err, "resolve: no flow accepts this item's type…\n")
			} else {
				fmt.Fprintf(app.Err, "resolve: no step eligible — finalizing…\n")
			}
		}

		res, err := RunOne(ctx, app, *claim)
		if err != nil {
			fmt.Fprintln(app.Err, "resolve:", err)
			return 1
		}
		if mode == OutputJSON {
			_ = enc.Encode(res)
		}

		// A result carrying no step name came from the pre-dispatch region of
		// RunOne, which has two exits: the finalize path, and a stop that
		// happens before any step can be selected. Only the first is a
		// finalize, and labelling the second "(finalize)" is the same misreport
		// the peek above avoids — "(finalize) → blocked" tells the operator the
		// run reached the finalize on an item where nothing ever started.
		label := res.Step
		switch {
		case label != "":
		case res.Status == "blocked":
			label = "(no step)"
		default:
			label = "(finalize)"
		}
		outcome := fmt.Sprintf("resolve: %s → %s", label, res.Status)
		if res.Reason != "" {
			outcome += " — " + res.Reason
		}
		if suffix := formatResultSuffix(res); suffix != "" {
			outcome += " " + suffix
		}
		fmt.Fprintln(app.Err, outcome)
		if res.Park != nil && len(res.Park.Axes) > 0 {
			fmt.Fprintf(app.Err, "  axes: %s\n", flow.FormatAxes(res.Park.Axes))
		}

		switch flow.InvocationStatus(res.Status) {
		case flow.StatusFailed:
			fmt.Fprintf(app.Err, "resolve: %s stopped on a failed step\n", claim.ItemRef.Display)
			if !isRunner {
				reportQuota(app.Err)
			}
			return 1
		case flow.StatusBlocked:
			// An environment condition is re-measured, never assumed to persist
			// (docs/environment.md). When the block came from an unfit machine,
			// cmdResolve holds and re-measures rather than exiting — disk frees,
			// builds finish, logs rotate — and work resumes when fit reports
			// clear, with nobody having to say so. The wait is bounded:
			// exhausting it is still "unfit", not a verdict.
			//
			// A block that did NOT come from fitness (e.g. ErrBlocked, a
			// preflight gate only a human can clear) exits immediately — looping
			// would re-run it to the runaway guard.
			if strings.Contains(res.Reason, flow.ErrUnfit.Error()) {
				if fc, ok := app.Backend.(flow.FitnessChecker); ok && fitnessWaits < maxFitnessWaits {
					v, ferr := fc.CheckFit(ctx)
					if ferr == nil && !v.Acceptable {
						fitnessWaits++
						fmt.Fprintf(app.Err, "resolve: machine unfit (%d/%d) — %s — waiting…\n",
							fitnessWaits, maxFitnessWaits, v.Detail)
						select {
						case <-time.After(fitnessWaitInterval):
						case <-ctx.Done():
							fmt.Fprintln(app.Err, "resolve: interrupted while waiting for fitness")
							return 1
						}
						continue
					}
					// Machine recovered — retry the step.
					fmt.Fprintln(app.Err, "resolve: machine fit again — retrying…")
					fitnessWaits = 0
					continue
				}
			}
			// A gate only a human can clear, or the fitness wait exhausted.
			fmt.Fprintf(app.Err, "resolve: %s is blocked — %s\n", claim.ItemRef.Display, res.Reason)
			if !isRunner {
				reportQuota(app.Err)
			}
			return 1
		case flow.StatusParked, flow.StatusSkipped:
			// Parked (question/budget/timeout) or skipped (preflight refusal,
			// e.g. an already-finalized item). Stop and let the operator act.
			fmt.Fprintf(app.Err, "resolve: %s %s — run `status %s` to inspect\n", claim.ItemRef.Display, res.Status, claim.ItemRef.Display)
			if !isRunner {
				reportQuota(app.Err)
			}
			return 0
		case flow.StatusDone:
			// Finalize case: RunOne ran no step (empty Step) because no eligible
			// flow remained — the item is fully resolved.
			if res.Step == "" {
				suffix := finalTotalSuffix(ctx, app, *claim)
				fmt.Fprintf(app.Err, "resolve: %s finalized ✓%s\n", claim.ItemRef.Display, suffix)
				if !isRunner {
					reportQuota(app.Err)
				}
				return 0
			}
			// Otherwise a step advanced; loop to run the next one.
		}
	}
	fmt.Fprintf(app.Err, "resolve: stopped after %d step attempts without finalizing (runaway guard); run `status` to inspect\n", maxResolveSteps)
	if !isRunner {
		reportQuota(app.Err)
	}
	return 1
}

// finalTotalSuffix loads the item's state and computes the total duration and
// cost across all artifacts. Returns "" when no figures are available.
//
// Uses StateInspector.LoadStateByRef (no claim needed) because Finalize has
// already been called and may have released the claim.
func finalTotalSuffix(ctx context.Context, app *App, claim flow.Claim) string {
	si, ok := app.Backend.(flow.StateInspector)
	if !ok {
		return ""
	}
	state, err := si.LoadStateByRef(ctx, claim.ItemRef)
	if err != nil {
		return "" // best-effort; finalization already succeeded
	}
	var totalDur time.Duration
	var totalCost float64
	lowerBound := false
	for _, art := range state.Artifacts {
		totalDur += art.DurationWorked
		totalCost += art.CostUSDSpent
		if art.Resolved && art.DurationWorked == 0 {
			lowerBound = true
		}
	}
	if totalDur == 0 && totalCost == 0 {
		return ""
	}
	dur := formatDurationCompact(totalDur)
	cost := fmt.Sprintf("$%.2f", totalCost)
	if lowerBound {
		return fmt.Sprintf(" (≥%s, ≥%s)", dur, cost)
	}
	return fmt.Sprintf(" (%s, %s)", dur, cost)
}
