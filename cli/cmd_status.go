package cli

import (
	"context"
	"fmt"

	"github.com/promise-language/flow"
)

func (app *App) cmdStatus(ctx context.Context, args []string) int {
	if !app.rejectArgs("status", args) {
		return 2
	}
	claim, err := app.Backend.LookupActiveClaim(ctx, app.Owner)
	if err != nil {
		fmt.Fprintln(app.Err, "status:", err)
		return 1
	}
	if claim == nil {
		fmt.Fprintln(app.Err, "status: no active claim (run `claim <id>` first)")
		return 1
	}
	state, err := app.Backend.LoadState(ctx, *claim)
	if err != nil {
		fmt.Fprintln(app.Err, "status:", err)
		return 1
	}

	f, _ := SelectFlow(app, state)
	flowName := "(none eligible)"
	if f != nil {
		flowName = f.Name()
	}
	fmt.Fprintf(app.Out, "item:  %s\n", claim.ItemRef.Display)
	fmt.Fprintf(app.Out, "owner: %s\n", claim.Owner)
	fmt.Fprintf(app.Out, "flow:  %s\n", flowName)
	fmt.Fprintln(app.Out)

	if f == nil {
		// Show all flows' status so the user can see why none matched.
		for _, ff := range app.Flows {
			printFlowChecklist(app, ff, state)
		}
		return 0
	}
	printFlowChecklist(app, f, state)

	if len(state.Questions) > 0 {
		fmt.Fprintln(app.Out, "\nquestions:")
		for _, q := range state.Questions {
			marker := "[ ]"
			if q.Answer != "" {
				marker = "[x]"
			}
			fmt.Fprintf(app.Out, "  %s %s — %s\n", marker, q.ID, q.Text)
		}
	}
	return 0
}

func printFlowChecklist(app *App, f *flow.Flow, state *flow.ItemState) {
	fmt.Fprintf(app.Out, "%s:\n", f.Name())
	for _, li := range f.Items() {
		marker := "[ ]"
		switch li.Kind {
		case flow.LifecycleArtifact:
			rec := state.Artifact(li.ArtifactId)
			if rec.Resolved && !rec.Stale {
				marker = "[x]"
			} else if rec.Stale {
				marker = "[~]"
			}
			fmt.Fprintf(app.Out, "  %s %s (%s)\n", marker, li.Name, li.ArtifactId)
		case flow.LifecycleSignal:
			if state.SignalSet(li.SignalId) {
				marker = "[x]"
			}
			fmt.Fprintf(app.Out, "  %s %s → %s\n", marker, li.Name, li.SignalId)
		case flow.LifecycleAwait:
			if state.SignalSet(li.SignalId) {
				marker = "[x]"
			}
			fmt.Fprintf(app.Out, "  %s await %s\n", marker, li.SignalId)
		}
	}
}
