package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/promise-language/flow"
)

func (app *App) cmdLease(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(app.Err, "lease: missing item id (e.g., `lease 42`)")
		return 2
	}
	itemID := args[0]

	// Find the ItemRef for this id by listing eligible items and matching
	// the display string. Backends with cheaper paths can implement a
	// dedicated lookup hook later; for now this is enough for v1.
	refs, err := app.Backend.ListEligible(ctx)
	if err != nil {
		fmt.Fprintln(app.Err, "lease:", err)
		return 1
	}
	ref, ok := matchItemRef(refs, itemID)
	if !ok {
		fmt.Fprintf(app.Err, "lease: no eligible item matching %q\n", itemID)
		return 1
	}

	claim, err := app.Backend.Claim(ctx, ref, app.Owner)
	if err != nil {
		fmt.Fprintln(app.Err, "lease:", err)
		return 1
	}
	if err := SaveActiveClaim(claim); err != nil {
		fmt.Fprintln(app.Err, "lease:", err)
		// Best-effort release so we don't leave a phantom claim.
		_ = app.Backend.Release(ctx, claim)
		return 1
	}
	fmt.Fprintf(app.Out, "leased %s as %s\n", ref.Display, app.Owner)
	return 0
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
