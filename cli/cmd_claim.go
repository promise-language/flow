package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/promise-language/flow"
)

func (app *App) cmdClaim(ctx context.Context, args []string) int {
	fs := app.newFlagSet("claim")
	of := addOutputFlags(fs)
	force := fs.Bool("force", false, "override worktree preconditions: clean-tree, base-branch, and already-held checks")
	forceUnadmitted := fs.Bool("force-unadmitted", false, "override the arena admission check (audited)")
	if !app.parseArgs(fs, args) {
		return 2
	}
	// BEFORE the arity checks, not after. A contradictory --json --human is a
	// property of the invocation itself and is "detected before the command
	// takes any action" (docs/cli.md § Output); deciding it after the
	// positional count would report a missing item id to somebody whose actual
	// mistake was asking for both modes.
	mode, ok := of.mode(app, "claim")
	if !ok {
		return 2
	}
	if fs.NArg() == 0 {
		return app.usageError("claim: missing item id (e.g., `claim 42`)")
	}
	if fs.NArg() > 1 {
		return app.usageError("claim: unexpected argument %q (claim takes exactly one item id)", fs.Arg(1))
	}
	itemID := fs.Arg(0)

	ref, err := app.resolveClaimRef(ctx, itemID)
	if err != nil {
		// No ref yet, so the report names no item: the string that could not be
		// resolved is not an identity and reporting it as one would put a value
		// in `item` that addresses nothing. Unclassified: a string that addresses
		// nothing here says nothing about whether another one would.
		return app.claimFailed(mode, "", err.Error(), nil)
	}

	var overrides []flow.ClaimOverride
	if *force {
		overrides = append(overrides, flow.OverrideDirtyTree, flow.OverrideAlreadyHeld, flow.OverrideStaleBase)
	}
	if *forceUnadmitted {
		overrides = append(overrides, flow.OverrideUnadmitted)
	}

	claim, err := app.takeClaim(ctx, ref, overrides)
	if err != nil {
		var refused flow.ErrClaimRefused
		if errors.As(err, &refused) {
			// The prose stays on stderr in both modes, unchanged. What is new is
			// the REPORT beside it: the code and the scope the refusal has always
			// carried in-process reach a caller outside it, so a driver tells a
			// lost race from a broken arena without parsing this line
			// (docs/cli.md § Claiming).
			fmt.Fprintln(app.Err, formatClaimRefusal("claim", refused))
			app.emit(mode, refusedClaimPayload(ref.Display, refused), func() {})
			return 1
		}
		// An awaited role the flow does not declare stops the claim too, but it
		// is reported as ITSELF — the role and the declared set — rather than as
		// one more unmet precondition: no other account and no other arena would
		// fare better, and what clears it is a person fixing the flow or the
		// record (docs/resolution.md § Whose move it is).
		//
		// ITEM-SCOPED, and said so on the wire. The stop is THIS ITEM's record
		// being wrong — the doc reports it as the item blocked — so a different
		// item may well be sound, which is exactly what `item_scoped: true`
		// means (docs/cli.md § Output). It is also the answer cmdResolve's
		// auto-selection already gives it in-process, where an undeclared role
		// moves on to the next ref rather than ending the run; omitted here, the
		// fleet driver outside the process would read the documented "absent is
		// false" and take the whole arena out of rotation over one corrupt
		// marker — the failure that loop's comment exists to name.
		var unknown flow.ErrUnknownRole
		if errors.As(err, &unknown) {
			itemScoped := true
			return app.claimFailed(mode, ref.Display, unknown.Error(), &itemScoped)
		}
		return app.claimFailed(mode, ref.Display, err.Error(), nil)
	}
	// The account is AMBIENT — the orchestrator read it rather than being told —
	// so it is reported from the claim it minted, which is the one the work is
	// actually done as.
	payload := claimPayload{Item: ref.Display, Claimed: true, Account: string(claim.Account)}
	code := app.emit(mode, payload, func() {
		fmt.Fprintf(app.Out, "claimed %s as %s\n", payload.Item, payload.Account)
	})
	// The warning is narration on stderr and belongs after the result in both
	// modes, the way it always has.
	warnOpenBlockers(ctx, app, ref)
	return code
}

// claimFailed reports a stop that carries no typed refusal — an id nothing
// resolves, an awaited role the flow does not declare, a backend that could not
// answer. Same object and same stream as every other outcome (docs/cli.md
// § One-shot reports), so a driver never has to tell "refused" from "stdout was
// empty".
//
// `scoped` is the classification when the stop has one and nil when it has
// not; nil omits `item_scoped`, which docs/cli.md § Output says a caller reads
// as false — the fail-closed direction, and the right one for a stop nothing
// classified.
//
// The command's name is the HUMAN LINE's, not the report's: `reason` goes on
// the wire bare, the way run-step's refuseArena puts it there, so a consumer
// reads the reason rather than the rendering of it.
func (app *App) claimFailed(mode OutputMode, item, reason string, scoped *bool) int {
	fmt.Fprintln(app.Err, "claim: "+reason)
	app.emit(mode, claimPayload{Item: item, Claimed: false, Reason: reason, ItemScoped: scoped}, func() {})
	return 1
}

// warnOpenBlockers tells the operator, on a claim that succeeded, that the next
// advance will refuse (docs/cli.md § Claiming). Open blockers do NOT refuse the
// claim — a claim is an arena reservation and taking one does no work — so this
// is narration on stderr beside the `claimed …` result on stdout, and the exit
// code stays 0.
//
// Best-effort by construction: the lease is already minted, so a failing Load
// prints nothing and changes nothing. Reporting it as a failure would leave an
// arena holding an item the operator was told they did not get.
func warnOpenBlockers(ctx context.Context, app *App, ref flow.ItemRef) {
	state, err := app.Orchestrator.Load(ctx, ref)
	if err != nil || state == nil {
		return
	}
	open := openBlockers(state.BlockKind, state.BlockedBy)
	if len(open) == 0 {
		return
	}
	fmt.Fprintf(app.Err, "claim: %s waits on unfinished items — %s — the next advance will stop on them\n",
		ref.Display, blockedByLine(open))
}

// resolveClaimRef turns the user-typed item id into an ItemRef.
//
// One route, and it is ResolveRef. The old fallback — listing the selectable
// set and substring-matching Display — resolved by projection, so it answered
// with AN item rather than THE item, and it confined `claim` to whatever
// happened to be in that set.
func (app *App) resolveClaimRef(ctx context.Context, itemID string) (flow.ItemRef, error) {
	return app.Orchestrator.ResolveRef(ctx, itemID)
}
