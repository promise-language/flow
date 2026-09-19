package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
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

// maxRedispatches bounds how many times cmdResolve re-dispatches an item that
// parked under a kind the vocabulary classifies as cleared by a re-dispatch
// (flow.ParkKind.RedispatchMayClear). It is the fitness wait's SHAPE, for the
// fitness wait's reason: a condition that never clears must terminate the run
// rather than spin it to the runaway guard, and exhausting the bound leaves the
// item PARKED — a wait bound is not a verdict.
//
// Its own number, and lower than maxFitnessWaits, because the two wait on
// different things. A fitness wait re-MEASURES and dispatches nothing, so
// twenty of them cost twenty readings; a re-dispatch runs the step, spends one
// of maxResolveSteps, and on a charged kind spends an invocation the treasurer
// is counting — which bounds it below this one long before this one is reached.
const maxRedispatches = 5

// redispatchInterval is the delay between those re-dispatches. Var (not const)
// for the reason fitnessWaitInterval is one: a test must not sleep 30 s per
// round to exercise the loop.
var redispatchInterval = 30 * time.Second

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

	// redispatches is ONE counter for the whole run too, and for the same
	// reason: a per-park counter would let a step that parks clearable, clears,
	// then parks clearable again keep buying itself a fresh budget, which is a
	// loop with extra steps.
	redispatches := 0

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
		c, err := app.takeClaim(ctx, ref, resolveOverrides)
		if err != nil {
			var refused flow.ErrClaimRefused
			if errors.As(err, &refused) {
				fmt.Fprintln(app.Err, formatClaimRefusal("resolve", refused))
				return 1
			}
			// A recorded awaited role outside the declared set matches nothing
			// and never will: no claim is taken, and the report names the role
			// and the alternatives so a person can fix the flow or the record.
			var unknown flow.ErrUnknownRole
			if errors.As(err, &unknown) {
				fmt.Fprintf(app.Err, "resolve: %s is blocked — %s\n", ref.Display, unknown)
				return 1
			}
			fmt.Fprintln(app.Err, conditionOrError("resolve", err))
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
				c, err := app.takeClaim(ctx, ref, resolveOverrides)
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
				// An awaited role outside the declared set is THIS item's record
				// being wrong, and the next item's may well be sound: the item is
				// reported blocked — a person fixes the flow or the record — and
				// the run goes on to the next ref, exactly as the role refusal
				// above does. Stopping here would let one corrupt marker in a
				// stale offer withhold every other item from an unattended run.
				var unknown flow.ErrUnknownRole
				if errors.As(err, &unknown) {
					fmt.Fprintf(app.Err, "resolve: %s is blocked — %s — trying next\n", ref.Display, unknown)
					continue
				}
				fmt.Fprintln(app.Err, conditionOrError("resolve", err))
				return 1
			}
			if !claimed {
				// The reasons are the per-ref lines above, one for each refusal.
				// A lease lost to another arena is only one of them now that a
				// stale offer is skipped the same way — an item whose marker moved
				// to a role this run cannot assume, or names a role the flow does
				// not declare — and naming a cause here would name the wrong one.
				fmt.Fprintln(app.Err, "resolve: no eligible item could be claimed — each was refused above — nothing to do")
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
	// Before the first dispatch AND before the first pacing wait, so an
	// operator learns how far this run can take the item rather than inferring
	// it from where it stops (docs/cli.md § The announcement names the run's
	// standing). The value is kept: the handoff report names the same facts,
	// and deriving them twice is how the two ends of one run come to disagree.
	stand := app.announceStanding(ctx, *claim)

	targets := paceTargets{FiveHour: *paceFiveHour / 100, SevenDay: *paceSevenDay / 100}

	// THE QUOTA BLOCK PRINTS ONCE, HERE, where it answers whether the run has
	// headroom. Reprinted after the outcome it buried the park or finalized
	// line — the one line an operator must act on — under pacing detail they
	// had already read (docs/cli.md § Resolving). `quota` is how the question
	// is asked deliberately the rest of the time.
	reportQuota(app.Err)

	// What the run cost at the orchestrator's own seam is a fact about the
	// WHOLE run, so it is reported when the run ends — and through one defer
	// rather than at each of the dozen returns below, which is how one of them
	// comes to be the path that misses it.
	defer app.reportServiceSpend()

	enc := json.NewEncoder(app.Out)
	quotaWarned := false
	for range maxResolveSteps {
		// Best-effort peek at the step about to run. RunOne re-derives this
		// itself; the peek never gates execution (a transient read error just
		// skips the label and paces as before). It answers two questions: what
		// to name on the progress line, and whether this iteration can spend —
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
		//
		// What "cannot assume" means here is what the standing carries
		// (cli/app.go roleStandings): a role this binary does not COVER, or one
		// it covers that the account cannot BACK. Both are handoffs — a binary
		// declining a role its account could back (a maintainer-capable account
		// pinned to the contributor's coverage) ends at the boundary like one
		// whose account lacks the merge, and the item records the role it
		// awaits. A run that crosses instead says so before it does: that is
		// the default branch.
		//
		// The SAME classification the claim refusal reads (cli/claim.go
		// whoseMoveIs), over the standing derived once above: the rule that
		// decides whether an item may be claimed for a role and the rule that
		// decides whether this run may advance it are one rule, and a run that
		// held its claim through a boundary must reach the same verdict the
		// claim would have.
		if st != nil && acts && !st.Finalized {
			switch whoseMoveIs(app.Flow, st.Awaits, stand.roles) {
			case moveUndeclared:
				// A recorded awaited role outside the declared set matches
				// nothing and never will, and read as "the runner has not
				// arrived" it would leave the item unofferable with nothing
				// naming why. A person fixes the flow or the record.
				fmt.Fprintf(app.Err, "resolve: %s is blocked — %s\n", claim.ItemRef.Display,
					flow.ErrUnknownRole{Role: st.Awaits.Role, Declared: app.Flow.RoleNames()})
				return 1
			case moveTheirs:
				return app.handOff(ctx, *claim, st.Awaits, stand)
			case moveOurs:
				app.reportCrossing(*claim, st)
			}
		}

		// Whether this iteration is going to dispatch a handler at all — the
		// other half of the question the pacing wait answers, and the half no
		// declaration can answer, because there is no step to have declared
		// anything. Three of RunOne's pre-dispatch exits are decided from what
		// the peek already holds:
		//
		//   - !acts      — outside the remit: RunOne blocks, or, on an item
		//                  already finalized, takes the finalize path.
		//   - next == "" — no step eligible: RunOne finalizes.
		//   - blocked    — waiting on unfinished items: RunOne stops clean.
		//
		// None of them dispatches anything, so none can spend, and holding one
		// under a curve that measures spend stalls it against nothing — the
		// reason a mechanical step skips the wait, on the iteration every
		// completed resolution ends with.
		//
		// NOT EVERY PRE-DISPATCH EXIT IS HERE, and the ones missing are missing
		// deliberately: a preflight refusal and the treasurer's invocation and
		// cost gates also stop before a dispatch, but the first is a call with
		// its own cost that running twice per iteration would pay for twice,
		// and the second would be the driver keeping a second copy of the
		// treasurer's arithmetic — the thing the park classification below is
		// careful not to do. They are the remaining work docs/cli.md § Resolving
		// carries, not an oversight of this expression (#450).
		//
		// A FAILED PEEK is not one of them: st == nil means we do not know,
		// RunOne re-derives and may well dispatch, so a transient read error
		// paces exactly as it did before rather than silently disabling the
		// wait.
		//
		// The same predicates the narration below reads, so the two cannot
		// disagree about whether a dispatch is coming.
		dispatches := st == nil || (acts && next != "" && !blockedFromAdvancing(st))

		// Pace against subscription quota. The check is before dispatch so the
		// delay costs nothing that is in flight.
		//
		// A mechanical step skips the wait ENTIRELY, not a shortened one: the
		// curve measures spend the step cannot make, and holding a free step
		// for hours under it stalls the resolution against nothing. An
		// iteration that dispatches nothing at all skips it for the same
		// reason and to a greater degree — there is not even a step to be free
		// (docs/cli.md § Resolving). The check stays here, before dispatch —
		// moving the wait inside the dispatch would have a handler hold its
		// claim and worktree while it waited, withdrawing the arena from the
		// fleet.
		//
		// An EXHAUSTED window is not paced against — it PARKS, and the park is
		// RunOne's (docs/environment.md § The agent account). Pacing holds a
		// step back to stay under a target the operator chose; an allowance
		// that is spent is not a target, and the pacing arithmetic answers it
		// with the whole remainder of the window. Pacing is on by default, so
		// without this the default `resolve` sits in front of a window for
		// hours holding the arena — "nothing is served by a process sitting in
		// front of it" — and the pre-dispatch check downstream never sees the
		// condition it exists for.
		if dispatches && !mechanical && app.Quota != nil && (targets.FiveHour > 0 || targets.SevenDay > 0) {
			if usage, qerr := app.Quota(); qerr == nil {
				// One instant for both readings: a wait and a park decided
				// against two different "now"s could each answer for a window
				// the other did not see.
				now := time.Now()
				if d := paceDelay(usage, targets, now); d > 0 &&
					bindingExhaustedWindow(usage, now) == nil {
					fmt.Fprintf(app.Err, "resolve: pacing — waiting %s for quota headroom\n", formatDurationCompact(d))
					// The narration above is visible only in the terminal that
					// launched this run. The registration is what a `status` in
					// any other terminal can read, and without it a deliberate
					// hold is indistinguishable from a stalled run.
					app.registerWait(claim.ItemRef, "quota headroom", now.Add(d))
					select {
					case <-time.After(d):
					case <-ctx.Done():
						fmt.Fprintln(app.Err, "resolve: interrupted while pacing")
						clistate.ClearRunning()
						return 1
					}
					clistate.ClearRunning()
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
			case blockedFromAdvancing(st):
				// NOTHING IS ABOUT TO RUN, so nothing is announced as running.
				// The advance stops before dispatch on an item waiting for
				// unfinished dependencies, and `running "plan"…` above the stop
				// is a line that says the step ran when it never started.
				//
				// The peek already holds the loaded item, and this is RunOne's
				// own predicate rather than a restatement of it — the same
				// reuse statusFlowState makes, so the narration and the advance
				// cannot disagree about whether a dispatch is coming.
				fmt.Fprintf(app.Err, "resolve: %s waits on unfinished dependencies — not dispatching\n",
					claim.ItemRef.Display)
				if line := blockedByLine(openBlockers(st.BlockKind, st.BlockedBy)); line != "" {
					fmt.Fprintf(app.Err, "  %s\n", line)
				}
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
			fmt.Fprintln(app.Err, conditionOrError("resolve", err))
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
		// SET OFF BY AN EMPTY LINE EITHER SIDE. This is the one line an
		// operator must act on, and flush between progress lines it reads as
		// one more of them. Written here, at the single site every outcome
		// passes through, so no outcome can be the one that misses it.
		fmt.Fprintln(app.Err)
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
		// THE PARK NAMES THE ACT THAT RESUMES IT. docs/cli.md already requires
		// a refusal to carry the failing check's output and the overriding flag
		// where one exists, and a park is held to the same standard: an
		// operator who answered a parked question, re-ran, and hit the same
		// budget park made a round trip this one line prevents. The act comes
		// from the park KIND, through the one table the listing's work mark
		// also reads.
		if res.Park != nil {
			if act := resumingAct(res.Park.Kind, selfPath(app.Name)); act != "" {
				fmt.Fprintf(app.Err, "  to resume: %s\n", act)
			}
		}
		fmt.Fprintln(app.Err)

		switch flow.InvocationStatus(res.Status) {
		case flow.StatusFailed:
			fmt.Fprintf(app.Err, "resolve: %s stopped on a failed step\n", claim.ItemRef.Display)
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
			return 1
		case flow.StatusSkipped:
			// A preflight refusal — an already-finalized item, an item outside
			// this binary's coverage — or a manual hold, an item an operator
			// has taken hand control of. Nothing to re-dispatch: the next cycle
			// answers identically until somebody acts.
			fmt.Fprintf(app.Err, "resolve: %s %s — run `status %s` to inspect\n", claim.ItemRef.Display, res.Status, claim.ItemRef.Display)
			return 0
		case flow.StatusParked:
			// Whether this park is worth another dispatch is the PARK KIND's own
			// answer, published on the result (flow.ParkKind.RedispatchMayClear,
			// carried out by parkAndReturn). It is read here and nowhere else:
			// a second table in the driver is how a vocabulary and its driver come
			// to disagree about a question only one of them owns. An absent field
			// reads as false, which is the direction wire.go already fixes for an
			// unrecognised kind.
			//
			// docs/cli.md § Resolving: the five non-clearing kinds — blocked,
			// question, treasurer-refused, refused and write-contract — stop here
			// immediately. Those are the real reasons to stop.
			mayClear := res.RedispatchMayClear != nil && *res.RedispatchMayClear
			switch {
			case mayClear && res.ClearsAt != nil:
				// The one condition that knows when it clears. The instant is
				// read WITH the classification and never instead of it — it
				// says when a re-dispatch would be worth anything, which is a
				// question only a kind that re-dispatch can clear is asking
				// (wire.go: "a caller reads the two together").
				//
				// The run does NOT sit in front of it: a window may be hours or days from
				// resetting, and nothing is served by a process waiting it out
				// (docs/environment.md § The agent account). It exits, and the
				// claim and its arena stay — so the draft, the session and the
				// worktree are here when whatever returns at that instant
				// resumes. Naming the instant is the fact a timer or an operator
				// needs, and the one today's message omitted.
				fmt.Fprintf(app.Err, "resolve: %s parked — %s — nothing clears before %s, so the run ends here; "+
					"the claim and this arena are kept, so a run started then resumes from this state\n",
					claim.ItemRef.Display, res.Reason, res.ClearsAt.UTC().Format(time.RFC3339))
				return 0
			case mayClear && redispatches < maxRedispatches:
				// The shape the fitness wait one arm above already has: hold,
				// try again, and bound the trying. A step that left a job
				// undone, a flapping runner, a remote that went away — the
				// re-dispatch is what does the job, and stopping for an
				// operator on a condition the codebase classifies as
				// self-curing is stopping for no reason.
				redispatches++
				fmt.Fprintf(app.Err, "resolve: %s — re-dispatching (%d/%d)…\n",
					res.Reason, redispatches, maxRedispatches)
				select {
				case <-time.After(redispatchInterval):
				case <-ctx.Done():
					fmt.Fprintln(app.Err, "resolve: interrupted while waiting to re-dispatch")
					return 1
				}
				continue
			default:
				// A park that stays a park: either a kind no re-dispatch
				// clears, or one that did not clear within the bound.
				// Exhausting the bound is still a park, never a verdict, so the
				// message says how many attempts it stands on and sends the
				// operator to the same place.
				bound := ""
				if mayClear {
					bound = fmt.Sprintf(" after %d re-dispatches", redispatches)
				}
				fmt.Fprintf(app.Err, "resolve: %s parked%s — run `status %s` to inspect\n",
					claim.ItemRef.Display, bound, claim.ItemRef.Display)
				return 0
			}
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
					// ErrUnavailable means ask again later, not that anything
					// went wrong here: the flow did everything it can.
					return 0
				}
				suffix := finalTotalSuffix(ctx, app, *claim)
				fmt.Fprintf(app.Err, "resolve: %s finalized ✓%s\n", claim.ItemRef.Display, suffix)
				return 0
			}
			// Otherwise a step advanced; loop to run the next one.
		}
	}
	fmt.Fprintf(app.Err, "resolve: stopped after %d step attempts without finalizing (runaway guard); run `status` to inspect\n", maxResolveSteps)
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
	// roles is what this run may assume: the roles this binary covers that
	// the account backs (cli/app.go assumableRoles). rolesKnown is whether the
	// capability half of that could be detected: an orchestrator that cannot
	// has said NOTHING about the account, which is not the same as an account
	// that can assume nothing — the wording distinguishes them, and the
	// derivation answers with the covered roles as they stand.
	roles      []flow.RoleName
	rolesKnown bool
	// creator is the filing account, empty when it could not be read.
	creator flow.AccountId
	// title is the item's title, empty when it could not be read or when the
	// backend has none. It rides here because it comes out of the SAME load the
	// creator does — the announcement's one read — and a second load to print a
	// title the first one already fetched would be a request for nothing.
	title string
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
		s.title = item.Title
	}
	// What is running this and what it is working on lead, because they are
	// what identifies the transcript; the standing follows, because it is what
	// says how far the run can get.
	app.announceRun(claim.ItemRef, s)
	fmt.Fprintf(app.Err, "resolve: driving %s to completion (until finalized or parked)…\n", claim.ItemRef.Display)
	app.reportStanding(s)
	return s
}

// announceRun prints what an operator reading this scrollback an hour later
// needs before anything is spent: which binary drove the run, and what the item
// was about.
//
// WHICH BINARY. A binary that cannot say what it is cannot be the subject of a
// bug report (docs/org/cli-guide.md §7), and `-version` only answers somebody
// who thinks to ask — the narration is what gets pasted into the report. A
// modified local build and a release are different facts about a transcript.
//
// IT IS A PRINT, NOT A CHECK. The value is one the host already holds: no
// marker read, no network, nothing that can fail, delay or refuse before the
// run starts. An empty App.Version prints nothing at all rather than a gap, the
// way the standing leaves out the filer line rather than printing an empty one.
//
// WHICH ITEM. The announcement is the one line before a run that spends real
// money and time, and a bare `owner/repo#N` is not something an operator can
// check: one who typed 275 meaning 276 learns it from the first prompt or from
// the pull request. `list` and `status` both render the title and only
// `resolve` dropped it. It goes through titleLine like every other free
// backend prose, and a title that is empty or all whitespace drops the segment.
//
// It PRINTS; it does not ask. An interactive acknowledgement here would break
// every unattended `resolve`.
func (app *App) announceRun(ref flow.ItemRef, s standing) {
	if app.Version != "" {
		fmt.Fprintf(app.Err, "%s version: %s\n", app.Name, app.Version)
	}
	if line := titleLine(s.title); line != "" {
		fmt.Fprintf(app.Err, "%s: %s\n", ref.Display, line)
	}
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

// rolesPhrase renders the standing's assumable set. The rendering itself is the
// free function below, which the claim refusal also reads: a refusal and an
// announcement that worded the same set differently would have an operator
// comparing two descriptions of one fact.
func (s standing) rolesPhrase() string {
	return rolesPhrase(s.roles, s.rolesKnown)
}

// rolesPhrase renders an assumable set for a person. "none" and "unknown" are
// different answers and read differently: one says the account backs no
// declared role, the other that nothing could be detected about it — which is
// what `known` distinguishes.
func rolesPhrase(roles []flow.RoleName, known bool) string {
	if !known {
		return "unknown — this orchestrator cannot detect capabilities"
	}
	if len(roles) == 0 {
		return "none"
	}
	names := make([]string, 0, len(roles))
	for _, r := range roles {
		names = append(names, string(r))
	}
	return strings.Join(names, ", ")
}

// awaitedPhrase renders what an item awaits: the role, and its account of record
// when the role has one — a route that returns to a role returns to the account
// that acted in it, so whoever reads a handoff or a refusal is told who to
// expect and not only what (docs/resolution.md § Whose move it is).
//
// One rendering for both, so the report that ends a run at a boundary and the
// refusal that stops one starting cannot name the same marker differently.
func awaitedPhrase(a flow.Awaits) string {
	if a.Account == "" {
		return string(a.Role)
	}
	return string(a.Role) + ", account of record " + string(a.Account)
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
		// A refused release is typed, and its DETAIL is what tells the operator
		// how to clear it — which files are in the way, or which branch HEAD is
		// on (docs/cli.md § Releasing). So the consequence is stated on its own
		// line and the refusal is RENDERED beneath it, rather than folded into
		// the line as well: ErrClaimRefused.Error() is the reason and the check,
		// which is the whole of what the rendering's first line says, so folding
		// it in prints that sentence twice and buries the detail under a repeat.
		// An untyped failure has no rendering, so it keeps the fold — the
		// backend's own account of it is the only account there is.
		var refused flow.ErrClaimRefused
		if errors.As(err, &refused) {
			fmt.Fprintf(app.Err, "resolve: %s awaits %s, but the claim could not be released\n",
				claim.ItemRef.Display, awaits.Role)
			fmt.Fprintln(app.Err, formatClaimRefusal("resolve", refused))
		} else {
			fmt.Fprintf(app.Err, "resolve: %s awaits %s, but the claim could not be released: %s\n",
				claim.ItemRef.Display, awaits.Role, err)
		}
		return 1
	}
	// The SAME suffix the finalization prints, so the two totals cannot
	// disagree about what the run cost.
	fmt.Fprintf(app.Err, "resolve: %s handed off — awaits %s%s\n",
		claim.ItemRef.Display, awaitedPhrase(awaits), finalTotalSuffix(ctx, app, claim))
	app.reportStanding(s)
	return 0
}

// reportCrossing says, before the dispatch, that this run is about to cross a
// role boundary it also performed the other side of — the moment the doc names
// for stating what carrying through does not provide
// (docs/resolution-standalone.md § Declaring what a binary may do: "a
// resolution about to cross into the integrating role says so before it
// crosses").
//
// It prints when all three hold: the pending role differs from the role the
// journal's last entry was performed in; that entry was performed by the
// account this run acts as; and no entry of the journal has this account in the
// pending role. The third keeps the line to the crossing the doc names. A
// rework handback returns to a role the account already held and prints
// nothing; a boundary reached after another account's entry is a handoff being
// picked up, and prints nothing either — an independent maintainer is not told
// their review is not independent.
//
// Nothing prints for an orchestrator that records no account. Carrying through
// is defined by what the journal shows, and a journal that names nobody shows
// no one principal on both sides.
func (app *App) reportCrossing(claim flow.Claim, item *flow.Item) {
	pending := item.Awaits.Role
	last, ok := item.LastEntry()
	if !ok || claim.Account == "" || last.By != claim.Account || last.Role == pending {
		return
	}
	for _, e := range item.Journal {
		if e.By == claim.Account && e.Role == pending {
			return
		}
	}
	fmt.Fprintf(app.Err, "resolve: crossing from %s into %s — %s performed the %s's part, so this is not independent review\n",
		last.Role, pending, claim.Account, last.Role)
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
		// Registered for the reason the pacing hold is: this run is alive and
		// deliberately idle, and `status` in another terminal must be able to
		// tell that from a run that died. No until-instant — this wait ends on
		// a re-measurement, not on a clock, and inventing one would be a
		// prediction rather than a fact.
		app.registerWait(ref, "the machine to become fit", time.Time{})
		select {
		case <-time.After(fitnessWaitInterval):
		case <-ctx.Done():
			fmt.Fprintln(app.Err, "resolve: interrupted while waiting for fitness")
			clistate.ClearRunning()
			return 1, false
		}
		clistate.ClearRunning()
	}
}

// registerWait records that this run is holding before a dispatch: alive,
// holding the claim, with no step executing.
//
// It writes the SAME registration RunOne writes for an executing step, with
// Step empty and the wait filled in — one record, so `status` reads one place
// and the two states cannot both be claimed at once. Advisory, like that write:
// a failure means `status` will not show the wait, which is a degradation and
// not a breakage, so the run is never stopped for it.
func (app *App) registerWait(ref flow.ItemRef, reason string, until time.Time) {
	exe, _ := os.Executable()
	absExe, _ := filepath.Abs(exe)
	_ = clistate.SaveRunning(clistate.RunningRecord{
		Item:      ref.Display,
		PID:       os.Getpid(),
		Exe:       absExe,
		Waiting:   reason,
		WaitUntil: until,
	})
}
