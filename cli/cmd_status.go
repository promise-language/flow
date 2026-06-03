package cli

import (
	"context"
	"fmt"

	"github.com/promise-language/flow"
)

func (app *App) cmdStatus(ctx context.Context, args []string) int {
	fs := app.newFlagSet("status")
	if err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(app.Err, "status: unexpected argument %q (status takes an optional item id)\n", fs.Arg(1))
		return 2
	}

	var (
		state   *flow.ItemState
		display string
		owner   string
	)
	if fs.NArg() == 1 {
		// Inspect an arbitrary item READ-ONLY, without claiming it. Requires a
		// backend that can resolve state from the id alone (StateInspector).
		inspector, ok := app.Backend.(flow.StateInspector)
		if !ok {
			fmt.Fprintln(app.Err, "status: this backend cannot inspect an item by id without a claim; run `claim <id>` first")
			return 1
		}
		ref, err := app.resolveClaimRef(ctx, fs.Arg(0))
		if err != nil {
			fmt.Fprintln(app.Err, "status:", err)
			return 1
		}
		st, err := inspector.LoadStateByRef(ctx, ref)
		if err != nil {
			fmt.Fprintln(app.Err, "status:", err)
			return 1
		}
		state = st
		display = ref.Display
		// Show the lease holder (if any) so the read-only view still tells the
		// operator who, if anyone, currently owns the item.
		if info, _ := app.Backend.LookupClaim(ctx, ref); info != nil {
			owner = info.Owner
		}
	} else {
		claim, err := app.Backend.LookupActiveClaim(ctx, app.Owner)
		if err != nil {
			fmt.Fprintln(app.Err, "status:", err)
			return 1
		}
		if claim == nil {
			fmt.Fprintln(app.Err, "status: no active claim (run `claim <id>` first, or `status <id>` to inspect any item)")
			return 1
		}
		st, err := app.Backend.LoadState(ctx, *claim)
		if err != nil {
			fmt.Fprintln(app.Err, "status:", err)
			return 1
		}
		state = st
		display = claim.ItemRef.Display
		owner = claim.Owner
	}

	f, _ := SelectFlow(app, state)
	// The flow that HANDLES this item's type, independent of step eligibility.
	// A finalized/complete item has no eligible step (SelectFlow → nil), but it
	// still belongs to its own flow — so show THAT flow's checklist, not every
	// flow the binary defines (e.g. don't dump do-plan for a task/bug item).
	typeFlow := flowForType(app, state.Item.Type)
	if owner == "" {
		owner = "(unclaimed)"
	}
	fmt.Fprintf(app.Out, "item:  %s\n", display)
	fmt.Fprintf(app.Out, "owner: %s\n", owner)
	fmt.Fprintf(app.Out, "flow:  %s\n", statusFlowLine(state, f, typeFlow))
	fmt.Fprintln(app.Out)

	// Only the type-matching flow's checklist. The "flow:" line names it, so no
	// redundant header. If no flow handles this item's type, there's nothing.
	if typeFlow != nil {
		printFlowChecklist(app, typeFlow, state, false)
	}

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

// flowForType returns the flow that handles itemType (the first whose
// AcceptsType matches), or nil. Unlike SelectFlow it ignores step eligibility,
// so a finalized/complete item still resolves to its own flow for display.
func flowForType(app *App, itemType flow.ItemType) *flow.Flow {
	for _, f := range app.Flows {
		if f.AcceptsType(itemType) {
			return f
		}
	}
	return nil
}

// statusFlowLine renders the "flow:" value. A finalized item is reported as
// finalized (its run is complete — NOT "no flow eligible", which misleadingly
// implies unstarted/blocked). Otherwise: the currently-eligible flow's name,
// or the type-matching flow tagged with why no step is eligible, or a no-match
// note.
func statusFlowLine(state *flow.ItemState, eligible, typeFlow *flow.Flow) string {
	if state.Item.Finalized {
		if typeFlow != nil {
			return typeFlow.Name() + " (finalized)"
		}
		return "finalized"
	}
	if eligible != nil {
		return eligible.Name()
	}
	if typeFlow != nil {
		return typeFlow.Name() + " (no eligible step)"
	}
	return "(no matching flow)"
}

func printFlowChecklist(app *App, f *flow.Flow, state *flow.ItemState, header bool) {
	if header {
		fmt.Fprintf(app.Out, "%s:\n", f.Name())
	}
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
