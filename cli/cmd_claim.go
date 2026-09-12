package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/promise-language/flow"
)

func (app *App) cmdClaim(ctx context.Context, args []string) int {
	fs := app.newFlagSet("claim")
	force := fs.Bool("force", false, "override worktree preconditions: clean-tree, base-branch, and already-held checks")
	forceUnadmitted := fs.Bool("force-unadmitted", false, "override the arena admission check (audited)")
	if !app.parseArgs(fs, args) {
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
		fmt.Fprintln(app.Err, "claim:", err)
		return 1
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
			fmt.Fprintln(app.Err, formatClaimRefusal("claim", refused))
			return 1
		}
		// An awaited role the flow does not declare stops the claim too, but it
		// is reported as ITSELF — the role and the declared set — rather than as
		// one more unmet precondition: no other account and no other arena would
		// fare better, and what clears it is a person fixing the flow or the
		// record (docs/resolution.md § Whose move it is).
		var unknown flow.ErrUnknownRole
		if errors.As(err, &unknown) {
			fmt.Fprintln(app.Err, "claim:", unknown)
			return 1
		}
		fmt.Fprintln(app.Err, "claim:", err)
		return 1
	}
	// The account is AMBIENT — the orchestrator read it rather than being told —
	// so it is reported from the claim it minted, which is the one the work is
	// actually done as.
	fmt.Fprintf(app.Out, "claimed %s as %s\n", ref.Display, claim.Account)
	warnOpenBlockers(ctx, app, ref)
	return 0
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
