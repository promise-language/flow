package cli

import (
	"context"
	"fmt"
)

func (app *App) cmdRelease(ctx context.Context, args []string) int {
	if !app.rejectArgs("release", args) {
		return 2
	}
	claim, err := app.Backend.LookupActiveClaim(ctx, app.Owner)
	if err != nil {
		fmt.Fprintln(app.Err, "release:", err)
		return 1
	}
	if claim == nil {
		fmt.Fprintln(app.Err, "release: no active claim")
		return 1
	}
	if err := app.Backend.Release(ctx, *claim); err != nil {
		fmt.Fprintln(app.Err, "release:", err)
		return 1
	}
	fmt.Fprintf(app.Out, "released %s\n", claim.ItemRef.Display)
	return 0
}
