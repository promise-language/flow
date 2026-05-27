package cli

import (
	"context"
	"fmt"
)

func (app *App) cmdRelease(ctx context.Context, args []string) int {
	claim, err := LoadActiveClaim()
	if err != nil {
		fmt.Fprintln(app.Err, "release:", err)
		return 1
	}
	if claim == nil {
		fmt.Fprintln(app.Err, "release: no active lease")
		return 1
	}
	if err := app.Backend.Release(ctx, *claim); err != nil {
		fmt.Fprintln(app.Err, "release:", err)
		return 1
	}
	if err := ClearActiveClaim(); err != nil {
		fmt.Fprintln(app.Err, "release:", err)
		return 1
	}
	fmt.Fprintf(app.Out, "released %s\n", claim.ItemRef.Display)
	return 0
}
