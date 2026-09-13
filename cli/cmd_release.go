package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/promise-language/flow"
)

func (app *App) cmdRelease(ctx context.Context, args []string) int {
	if !app.rejectArgs("release", args) {
		return 2
	}
	claim, err := app.Orchestrator.LookupActiveClaim(ctx)
	if err != nil {
		fmt.Fprintln(app.Err, "release:", err)
		return 1
	}
	if claim == nil {
		fmt.Fprintln(app.Err, "release: no active claim")
		return 1
	}
	if err := app.Orchestrator.Release(ctx, claim.ItemRef); err != nil {
		// A release is refused through the same typed carrier a claim is
		// (docs/cli.md § Releasing), and rendered through the same formatter —
		// so the failing check and its verbatim output reach the operator, who
		// otherwise gets "refused" without being told WHAT is in the way. There
		// is no override line to print: nothing bypasses these two checks, and
		// formatClaimRefusal omits the line when Override is empty.
		var refused flow.ErrClaimRefused
		if errors.As(err, &refused) {
			fmt.Fprintln(app.Err, formatClaimRefusal("release", refused))
			return 1
		}
		fmt.Fprintln(app.Err, "release:", err)
		return 1
	}
	fmt.Fprintf(app.Out, "released %s\n", claim.ItemRef.Display)
	return 0
}
