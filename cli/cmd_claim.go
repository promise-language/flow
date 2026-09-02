package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

	claim, err := app.Backend.Claim(ctx, ref, app.Owner, overrides)
	if err != nil {
		var refused flow.ErrClaimRefused
		if errors.As(err, &refused) {
			fmt.Fprintln(app.Err, formatClaimRefusal("claim", refused))
			return 1
		}
		fmt.Fprintln(app.Err, "claim:", err)
		return 1
	}
	fmt.Fprintf(app.Out, "claimed %s as %s\n", ref.Display, app.Owner)
	_ = claim // backend persists its own lease state (see Backend.LookupActiveClaim)
	return 0
}

// resolveClaimRef turns the user-typed item id into a backend ItemRef. When
// the backend implements flow.RefResolver (e.g. the tracker, where the ref is
// just the id) it resolves directly — no ListEligible round-trip, and the item
// need not be in the eligible set to be claimed. Otherwise it falls back to
// listing eligible items and substring-matching the display string.
func (app *App) resolveClaimRef(ctx context.Context, itemID string) (flow.ItemRef, error) {
	if rr, ok := app.Backend.(flow.RefResolver); ok {
		return rr.ResolveRef(ctx, itemID)
	}
	refs, err := app.Backend.ListEligible(ctx)
	if err != nil {
		return flow.ItemRef{}, err
	}
	ref, ok := matchItemRef(refs, itemID)
	if !ok {
		return flow.ItemRef{}, fmt.Errorf("no eligible item matching %q", itemID)
	}
	return ref, nil
}

// matchItemRef returns the first ref whose Display contains itemID as a
// substring or matches the trailing path segment. Sufficient for v1; the
// github backend's Display is "owner/repo#42", so "42" matches uniquely.
func matchItemRef(refs []flow.ItemRef, itemID string) (flow.ItemRef, bool) {
	for _, r := range refs {
		if r.Display == itemID {
			return r, true
		}
		if strings.Contains(r.Display, itemID) {
			return r, true
		}
	}
	return flow.ItemRef{}, false
}
