package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/promise-language/flow"
)

func (app *App) cmdClaim(ctx context.Context, args []string) int {
	fs := app.newFlagSet("claim")
	force := fs.Bool("force", false, "claim even if the arena worktree has unsaved work (override the clean-tree check)")
	if err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(app.Err, "claim: missing item id (e.g., `claim 42`)")
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(app.Err, "claim: unexpected argument %q (claim takes exactly one item id)\n", fs.Arg(1))
		return 2
	}
	itemID := fs.Arg(0)

	ref, err := app.resolveClaimRef(ctx, itemID)
	if err != nil {
		fmt.Fprintln(app.Err, "claim:", err)
		return 1
	}

	claim, err := app.Backend.Claim(ctx, ref, app.Owner, *force)
	if err != nil {
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
