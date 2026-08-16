package cli

import (
	"context"
	"fmt"
)

func (app *App) cmdList(ctx context.Context, args []string) int {
	fs := app.newFlagSet("list")
	of := addOutputFlags(fs)
	if err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(app.Err, "list: unexpected argument %q (this command takes no arguments)\n", fs.Arg(0))
		return 2
	}
	mode, ok := of.mode(app, "list")
	if !ok {
		return 2
	}

	refs, err := app.Backend.ListEligible(ctx)
	if err != nil {
		fmt.Fprintln(app.Err, "list:", err)
		return 1
	}

	payload := listPayload{Items: make([]listItemPayload, 0, len(refs))}
	for _, r := range refs {
		owner := ""
		if info, _ := app.Backend.LookupClaim(ctx, r); info != nil {
			owner = info.Owner
		}
		payload.Items = append(payload.Items, listItemPayload{
			Display: r.Display,
			Owner:   owner,
			Backend: r.BackendName,
		})
	}

	return app.emit(mode, payload, func() {
		if len(payload.Items) == 0 {
			fmt.Fprintln(app.Out, "(no eligible items)")
			return
		}
		for _, it := range payload.Items {
			owner := it.Owner
			if owner == "" {
				owner = "—"
			}
			fmt.Fprintf(app.Out, "%s\t%s\n", it.Display, owner)
		}
	})
}
