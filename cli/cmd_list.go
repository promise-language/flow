package cli

import (
	"context"
	"fmt"
)

func (app *App) cmdList(ctx context.Context, args []string) int {
	if !app.rejectArgs("list", args) {
		return 2
	}
	refs, err := app.Backend.ListEligible(ctx)
	if err != nil {
		fmt.Fprintln(app.Err, "list:", err)
		return 1
	}
	if len(refs) == 0 {
		fmt.Fprintln(app.Out, "(no eligible items)")
		return 0
	}
	for _, r := range refs {
		info, _ := app.Backend.LookupClaim(ctx, r)
		owner := "—"
		if info != nil {
			owner = info.Owner
		}
		fmt.Fprintf(app.Out, "%s\t%s\n", r.Display, owner)
	}
	return 0
}
