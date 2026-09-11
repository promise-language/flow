package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
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

	// fitnessWaits is ONE counter for the whole run — the pre-claim wait below
	// and the mid-run wait further down share it. Two counters let a fit gate
	// that fails on every call loop until the runaway guard, because each site
	// kept resetting the other's budget.
	fitnessWaits := 0

	var claim *flow.Claim
	if fs.NArg() == 1 {
		ref, err := app.resolveClaimRef(ctx, fs.Arg(0))
		if err != nil {
			fmt.Fprintln(app.Err, "resolve:", err)
			return 1
		}
		// Pre-claim fitness: an unfit machine must not take an item at all — a
		// fitness answer that arrives after the work started is the failure
		// `fit` exists to prevent. No gate requires a claim, so this runs on the
		// item's worktree before the lease is taken.
		if code, ok := app.awaitFit(ctx, ref, &fitnessWaits); !ok {
			return code
		}
		c, err := app.Orchestrator.Claim(ctx, ref, resolveOverrides)
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
		c, err := app.Orchestrator.LookupActiveClaim(ctx)
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
			// Tag filtering belongs in the orchestrator: tags live there and
			// ItemRef does not carry them, so a caller has nothing to filter on.
			// The comparison it applies is flow.TagsMatch — the same one `list`
			// uses — which is what makes the two commands symmetrical.
			want, ok := app.tagFilter("resolve", tags)
			if !ok {
				return 1
			}
			refs, err := app.Orchestrator.ListAutoSelectable(ctx, want, app.assumesRole(ctx))
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
				if code, fitOK := app.awaitFit(ctx, ref, &fitnessWaits); !fitOK {
					return code
				}
				c, err := app.Orchestrator.Claim(ctx, ref, resolveOverrides)
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

	// Before the first dispatch AND before the first pacing wait, so an
	// operator learns how far this run can take the item rather than inferring
	// it from where it stops (docs/cli.md § The announcement names the run's
	// standing). The value is kept: the handoff report names the same facts,
	// and deriving them twice is how the two ends of one run come to disagree.
	stand := app.announceStanding(ctx, *claim)

	targets := paceTargets{FiveHour: *paceFiveHour / 100, SevenDay: *paceSevenDay / 100}

	reportQuota(app.Err)

	enc := json.NewEncoder(app.Out)
	quotaWarned := false
	for range maxResolveSteps {
		// Best-effort peek at the step about to run. RunOne re-derives this
		// itself; the peek never gates execution (a transient read error just
		// skips the label and paces as before). It answers two questions: what
		// to name on the progress line, and whether the step is mechanical —
		// which decides whether to pace at all, so it is read BEFORE the pacing
		// block rather than after it.
		//
		// We name the step rather than number it: a positional counter ("[step
		// 1]") collides with the flow's own named steps (plan/implement/…) and
		// misreads as "flow step 1" when it really means "resolve iteration 1".
		var (
			st         *flow.Item
			acts       bool
			next       string
			mechanical bool
		)
		if loaded, serr := app.Orchestrator.Load(ctx, claim.ItemRef); serr == nil {
			st = loaded
			// In RunOne's own order: the remit first, and only then the step,
			// because that is the order the advance decides in and this line is
			// a report of what it is about to do.
			acts = inRemit(app, st)
			if acts {
				// Best-effort, so a Position refusal just leaves the line
				// unlabelled: RunOne re-derives and reports it properly a
				// moment later, and narrating it twice would say it first as a
				// missing step name.
				if f, n, err := SelectFlow(app, st); err == nil && f != nil {
					next = n
					// The declaration, read through the one definition of
					// mechanical the envelope and the chokepoint also read.
					if li, err := lifecycleItemOf(f, n); err == nil {
						mechanical = li.Mechanical()
					}
				}
			}
		}

		// A handoff, decided from the item's own awaited marker. It is a CLEAN
		// END rather than a stop: the roles this account can assume are done
		// with the item, so the claim goes back and the report names who moves
		// next (docs/cli.md § Resolving, docs/resolution.md § Whose move it is).
		//
		// Decided HERE — before the pacing block and before the dispatch —
		// because a run that will not advance the item must not first wait
		// hours for quota headroom it is never going to spend.
		//
		// An awaited SIGNAL is not a handoff: nobody's move is not somebody
		// else's, and it reports blocked through the advance like any other
		// wait.
		if st != nil && acts && !st.Finalized && st.Awaits.Role != "" && st.Awaits.Signal == "" {
			role := st.Awaits.Role
			switch {
			case !app.Flow.DeclaresRole(role):
				// A recorded awaited role outside the declared set matches
				// nothing and never will, and read as "the runner has not
				// arrived" it would leave the item unofferable with nothing
				// naming why. A person fixes the flow or the record.
				fmt.Fprintf(app.Err, "resolve: %s is blocked — %s\n", claim.ItemRef.Display,
					flow.ErrUnknownRole{Role: role, Declared: app.Flow.RoleNames()})
				reportQuota(app.Err)
				return 1
			case !stand.assumes(role):
				return app.handOff(ctx, *claim, st.Awaits, stand)
			}
		}

		// Pace against subscription quota. The check is before dispatch so the
		// delay costs nothing that is in flight.
		//
		// A mechanical step skips the wait ENTIRELY, not a shortened one: the
		// curve measures spend the step cannot make, and holding a free step
		// for hours under it stalls the resolution against nothing. The check
		// stays here, before dispatch — moving the wait inside the dispatch
		// would have a handler hold its claim and worktree while it waited,
		// withdrawing the arena from the fleet.
		if !mechanical && app.Quota != nil && (targets.FiveHour > 0 || targets.SevenDay > 0) {
			if usage, qerr := app.Quota(); qerr == nil {
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

		// The progress line, after any wait so the long pause is attributed to
		// pacing and the step's own run is announced when it actually begins.
		if st != nil {
			switch {
			case next != "":
				fmt.Fprintf(app.Err, "resolve: running %q…\n", next)
			case !acts && !st.Finalized:
				// The item is outside the remit, so RunOne will block rather
				// than finalize. Announcing "finalizing…" here would tell the
				// operator the run is completing right before it reports that
				// nothing ever started. The finalized exemption is RunOne's, and
				// is repeated here for the same reason the branch exists: an
				// already-finalized item DOES take the finalize path, so saying
				// otherwise about it would be the same misreport inverted.
				fmt.Fprintf(app.Err, "resolve: no flow accepts this item's type…\n")
			default:
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
		// A stop on the item's own blockers names them on their own line, as
		// the axes line does for a budget park: the reason says the kind, and
		// the references say what to go work instead.
		if line := blockedByLine(openBlockers(res.BlockKind, res.BlockedBy)); line != "" {
			fmt.Fprintf(app.Err, "  %s\n", line)
		}

		switch flow.InvocationStatus(res.Status) {
		case flow.StatusFailed:
			fmt.Fprintf(app.Err, "resolve: %s stopped on a failed step\n", claim.ItemRef.Display)
			reportQuota(app.Err)
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
			if strings.Contains(res.Reason, flow.ErrUnfit.Error()) && fitnessWaits < maxFitnessWaits {
				// The SAME helper and the SAME counter as the pre-claim site.
				// One implementation, so the two cannot disagree about what
				// counts as fit, and one bound, so a persistently broken gate
				// terminates here rather than at the runaway guard.
				if code, ok := app.awaitFit(ctx, claim.ItemRef, &fitnessWaits); !ok {
					return code
				}
				fmt.Fprintln(app.Err, "resolve: machine fit again — retrying…")
				continue
			}
			// A gate only a human can clear, or the fitness wait exhausted.
			fmt.Fprintf(app.Err, "resolve: %s is blocked — %s\n", claim.ItemRef.Display, res.Reason)
			reportQuota(app.Err)
			return 1
		case flow.StatusParked, flow.StatusSkipped:
			// Parked (question/budget/timeout) or skipped (preflight refusal,
			// e.g. an already-finalized item). Stop and let the operator act.
			fmt.Fprintf(app.Err, "resolve: %s %s — run `status %s` to inspect\n", claim.ItemRef.Display, res.Status, claim.ItemRef.Display)
			reportQuota(app.Err)
			return 0
		case flow.StatusDone:
			// Finalize case: RunOne ran no step (empty Step) because no eligible
			// flow remained.
			//
			// Reaching the end of the flow is NOT the run being recorded
			// complete: Finalize refuses an item the orchestrator does not yet
			// consider finished, and the result says which happened
			// (flow.InvocationResult.Finalized). Branching on the empty step
			// alone printed the tick for both, so a reader who trusted the
			// summary concluded the item was closed while the result object one
			// line above said it was not.
			if res.Step == "" {
				if !res.Finalized {
					fmt.Fprintf(app.Err, "resolve: %s not finalized — no eligible step remains, and the orchestrator does not yet consider the item finished; nothing finalized, claim kept — run `status %s` to inspect\n",
						claim.ItemRef.Display, claim.ItemRef.Display)
					reportQuota(app.Err)
					// ErrUnavailable means ask again later, not that anything
					// went wrong here: the flow did everything it can.
					return 0
				}
				suffix := finalTotalSuffix(ctx, app, *claim)
				fmt.Fprintf(app.Err, "resolve: %s finalized ✓%s\n", claim.ItemRef.Display, suffix)
				reportQuota(app.Err)
				return 0
			}
			// Otherwise a step advanced; loop to run the next one.
		}
	}
	fmt.Fprintf(app.Err, "resolve: stopped after %d step attempts without finalizing (runaway guard); run `status` to inspect\n", maxResolveSteps)
	reportQuota(app.Err)
	return 1
}

// standing is what a run acts with: the repository account, the roles that
// account can assume, and the account that filed the item.
//
// One value, derived once, read at both ends of the run — the announcement
// before the first dispatch and the handoff report at the end. Re-deriving it
// at the second site is how the two come to disagree about what the operator
// was told.
type standing struct {
	account flow.AccountId
	// roles is meaningful only when rolesKnown. An orchestrator that cannot
	// detect capabilities has said NOTHING about the account, which is not the
	// same as an account that can assume nothing — the wording distinguishes
	// them, and so does assumes below.
	roles      []flow.RoleName
	rolesKnown bool
	// creator is the filing account, empty when it could not be read.
	creator flow.AccountId
}

// assumes reports whether this run may take a move belonging to role.
//
// Undetectable capabilities answer TRUE: a ceiling nobody could measure filters
// nothing, which is the convention assumesRole's nil predicate already carries
// into auto-selection. Handing an item off on an unanswered question would end
// the run on the strength of a fact nobody established.
func (s standing) assumes(role flow.RoleName) bool {
	return !s.rolesKnown || slices.Contains(s.roles, role)
}

// announceStanding derives the run's standing and prints it, before anything is
// dispatched and before anything is waited on (docs/cli.md § The announcement
// names the run's standing).
//
// The repository account is the CLAIM's: it is the ambient account
// DetectCapabilities is asked about and the one every write of this run is made
// by — the same value StepCtx.Runner reports. Nothing is detected for it.
//
// The filing account costs one best-effort Load. A read that fails drops that
// one line and keeps the others: the account and its roles are what the run's
// reach follows from, and withholding them because an unrelated read failed
// would trade the whole announcement for part of it.
func (app *App) announceStanding(ctx context.Context, claim flow.Claim) standing {
	s := standing{account: claim.Account}
	s.roles, s.rolesKnown = app.assumableRoles(ctx)
	if item, err := app.Orchestrator.Load(ctx, claim.ItemRef); err == nil {
		s.creator = item.Creator
	}
	app.reportStanding(s)
	return s
}

// reportStanding prints the standing. One writer for the announcement and for
// the handoff's repeat of it, so the two cannot word the same facts
// differently.
func (app *App) reportStanding(s standing) {
	fmt.Fprintf(app.Err, "resolve: acting as %s — roles it can assume: %s\n", accountName(s.account), s.rolesPhrase())
	// Only when it differs from the account acting: naming the filer of one's
	// own item says nothing an operator did not already know.
	if s.creator != "" && s.creator != s.account {
		fmt.Fprintf(app.Err, "resolve: filed by %s\n", s.creator)
	}
}

// rolesPhrase renders the assumable set for a person. "none" and "unknown" are
// different answers and read differently: one says the account backs no
// declared role, the other that nothing could be detected about it.
func (s standing) rolesPhrase() string {
	if !s.rolesKnown {
		return "unknown — this orchestrator cannot detect capabilities"
	}
	if len(s.roles) == 0 {
		return "none"
	}
	names := make([]string, 0, len(s.roles))
	for _, r := range s.roles {
		names = append(names, string(r))
	}
	return strings.Join(names, ", ")
}

// accountName renders an account for display, naming the one case where there
// is nothing to render rather than printing an empty gap.
func accountName(a flow.AccountId) string {
	if a == "" {
		return "an account this orchestrator does not name"
	}
	return string(a)
}

// handOff ends the run at a role boundary: the claim is released and the report
// names the role the item now awaits, together with the standing that finished
// with it (docs/cli.md § Resolving).
//
// Releasing is the promise being kept, not a courtesy — an item held by an
// arena that is done with it is one the next role cannot pick up — so a Release
// that fails stops the run at exit 1 rather than reporting a handoff that did
// not happen.
//
// Returns the run's exit code: 0 for the handoff, 1 when the claim could not be
// given up.
func (app *App) handOff(ctx context.Context, claim flow.Claim, awaits flow.Awaits, s standing) int {
	if err := app.Orchestrator.Release(ctx, claim.ItemRef); err != nil {
		fmt.Fprintf(app.Err, "resolve: %s awaits %s, but the claim could not be released: %s\n",
			claim.ItemRef.Display, awaits.Role, err)
		reportQuota(app.Err)
		return 1
	}
	awaited := string(awaits.Role)
	// The account of record when the role has one — a route that returns to a
	// role returns to the account that acted in it, and an operator reading the
	// handoff is being told who to expect, not only what.
	if awaits.Account != "" {
		awaited += ", account of record " + string(awaits.Account)
	}
	// The SAME suffix the finalization prints, so the two totals cannot
	// disagree about what the run cost.
	fmt.Fprintf(app.Err, "resolve: %s handed off — awaits %s%s\n",
		claim.ItemRef.Display, awaited, finalTotalSuffix(ctx, app, claim))
	app.reportStanding(s)
	reportQuota(app.Err)
	return 0
}

// finalTotalSuffix loads the item and computes the total duration and cost
// across all artifacts. Returns "" when no figures are available.
//
// Addressed by ref because Finalize has already been called and may have
// released the claim — and because reading an item is not a privileged act.
func finalTotalSuffix(ctx context.Context, app *App, claim flow.Claim) string {
	state, err := app.Orchestrator.Load(ctx, claim.ItemRef)
	if err != nil {
		return "" // best-effort; finalization already succeeded
	}
	// The ledger's own totals: the treasurer keeps them, and re-summing the
	// rows here would be a second answer to a question it already holds.
	totalDur := state.Ledger.TotalActive
	totalCost := state.Ledger.TotalCostUSD
	lowerBound := false
	for _, row := range state.Ledger.Steps {
		if row.Dispatches > 0 && row.Active == 0 {
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

// awaitFit measures whether this machine may be given work, and waits while it
// may not.
//
// It IS the fit path — both call sites go through it, so there is one rule and
// one wait counter. It FAILS CLOSED: flow.CheckFit reports unfit for a RunGate
// error, for every outcome that is not a measurement, and for a Judge error,
// because no outcome is not a passing outcome. The earlier reading — "cannot
// check → do not refuse" — treated an erroring fit gate as a fit machine, which
// is the exact state `fit` exists to stop anyone proceeding from.
//
// Waiting rather than refusing is what docs/environment.md asks for: unfitness
// is a condition that ends on its own, and the item is left unparked and
// unmodified while the machine recovers. The wait is BOUNDED — an item held
// indefinitely on a machine nobody is fixing is one no other machine can take —
// and exhausting the bound is still "unfit", never a verdict about the change.
//
// Returns (exitCode, false) when the caller should stop, (0, true) when the
// machine is fit and work may proceed.
func (app *App) awaitFit(ctx context.Context, ref flow.ItemRef, waits *int) (int, bool) {
	wt, err := app.Orchestrator.Worktree(ctx, ref)
	if err != nil {
		// No worktree, no measurement — and no measurement is not a pass.
		fmt.Fprintf(app.Err, "resolve: cannot reach a worktree to measure machine fitness: %s\n", err)
		return 1, false
	}
	for {
		fitErr := flow.CheckFit(ctx, wt)
		if fitErr == nil {
			return 0, true
		}
		if *waits >= maxFitnessWaits {
			fmt.Fprintf(app.Err, "resolve: machine unfit — %s\n", fitErr)
			return 1, false
		}
		*waits++
		fmt.Fprintf(app.Err, "resolve: machine unfit (%d/%d) — %s — waiting…\n",
			*waits, maxFitnessWaits, fitErr)
		select {
		case <-time.After(fitnessWaitInterval):
		case <-ctx.Done():
			fmt.Fprintln(app.Err, "resolve: interrupted while waiting for fitness")
			return 1, false
		}
	}
}
