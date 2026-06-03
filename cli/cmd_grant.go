package cli

import (
	"context"
	"fmt"

	"github.com/promise-language/flow"
)

func (app *App) cmdGrant(ctx context.Context, args []string) int {
	fs := app.newFlagSet("grant")
	invocations := fs.Int("invocations", 0, "additional invocations to grant")
	prompts := fs.Int("prompts", 0, "additional prompts-per-invocation to grant")
	cost := fs.Float64("cost", 0, "additional cost (USD) to grant")
	timeout := fs.Int("timeout", 0, "additional timeout in seconds")
	if err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(app.Err, "grant: missing artifact id (e.g., `grant plan --invocations 3`).")
		fmt.Fprintln(app.Err, "       <artifact-id> is the id passed to AddStep, NOT the human step name.")
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(app.Err, "grant: unexpected argument %q (grant takes exactly one artifact id)\n", fs.Arg(1))
		return 2
	}
	if *invocations < 0 || *prompts < 0 || *cost < 0 || *timeout < 0 {
		fmt.Fprintln(app.Err, "grant: --invocations / --prompts / --cost / --timeout must be >= 0")
		return 2
	}
	key := fs.Arg(0)

	claim, err := app.Backend.LookupActiveClaim(ctx, app.Owner)
	if err != nil {
		fmt.Fprintln(app.Err, "grant:", err)
		return 1
	}
	if claim == nil {
		fmt.Fprintln(app.Err, "grant: no active claim")
		return 1
	}

	g := flow.Grant{
		Invocations:          *invocations,
		PromptsPerInvocation: *prompts,
		CostUSD:              *cost,
		TimeoutAdd:           int64(*timeout),
	}
	if g == (flow.Grant{}) {
		fmt.Fprintln(app.Err, "grant: at least one of --invocations / --prompts / --cost / --timeout must be set")
		return 2
	}
	if err := app.Backend.Grant(ctx, *claim, key, g); err != nil {
		fmt.Fprintln(app.Err, "grant:", err)
		return 1
	}
	fmt.Fprintf(app.Out, "granted %+v to %q\n", g, key)
	return 0
}
