package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/promise-language/flow"
)

func (app *App) cmdReseed(ctx context.Context, args []string) int {
	fs := app.newFlagSet("reseed")
	force := fs.Bool("force", false, "actually clear the seed (required)")
	if !app.parseArgs(fs, args) {
		return 2
	}
	if fs.NArg() > 0 {
		return app.usageError("reseed: unexpected argument %q (this command takes no arguments)", fs.Arg(0))
	}

	claim, err := app.Backend.LookupActiveClaim(ctx, app.Owner)
	if err != nil {
		fmt.Fprintln(app.Err, "reseed:", err)
		return 1
	}
	if claim == nil {
		fmt.Fprintln(app.Err, "reseed: no active claim")
		return 1
	}

	if !*force {
		fmt.Fprintf(app.Err, "reseed: would discard artifact records, budget counters, and park state on %s\n", claim.ItemRef.Display)
		fmt.Fprintln(app.Err, "run again with --force to proceed")
		return 1
	}

	if err := app.Backend.ResetSeed(ctx, *claim); err != nil {
		if errors.Is(err, flow.ErrResetSeedUnsupported) {
			fmt.Fprintf(app.Err, "reseed: backend %q does not support reseed\n", app.Backend.Name())
			return 1
		}
		fmt.Fprintln(app.Err, "reseed:", err)
		return 1
	}

	fmt.Fprintf(app.Out, "reseeded %s — artifact records, budget counters, and park state cleared\n", claim.ItemRef.Display)
	return 0
}
