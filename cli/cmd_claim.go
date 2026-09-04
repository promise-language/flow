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

	claim, err := app.Orchestrator.Claim(ctx, ref, overrides)
	if err != nil {
		var refused flow.ErrClaimRefused
		if errors.As(err, &refused) {
			fmt.Fprintln(app.Err, formatClaimRefusal("claim", refused))
			return 1
		}
		fmt.Fprintln(app.Err, "claim:", err)
		return 1
	}
	// The account is AMBIENT — the orchestrator read it rather than being told —
	// so it is reported from the claim it minted, which is the one the work is
	// actually done as.
	fmt.Fprintf(app.Out, "claimed %s as %s\n", ref.Display, claim.Account)
	return 0
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
