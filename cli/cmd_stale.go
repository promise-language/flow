package cli

import (
	"context"
	"fmt"

	"github.com/promise-language/flow"
)

// stalePayload is the JSON output of the stale command.
type stalePayload struct {
	StepID       string   `json:"step_id"`
	Item         string   `json:"item"`
	AlreadyStale bool     `json:"already_stale"`
	Warnings     []string `json:"warnings,omitempty"`
}

func (app *App) cmdStale(ctx context.Context, args []string) int {
	fs := app.newFlagSet("stale")
	of := addOutputFlags(fs)
	if !app.parseArgs(fs, args) {
		return 2
	}
	mode, ok := of.mode(app, "stale")
	if !ok {
		return 2
	}
	if fs.NArg() != 1 {
		if fs.NArg() == 0 {
			return app.usageError("stale: requires exactly one step id")
		}
		return app.usageError("stale: requires exactly one step id, got %d", fs.NArg())
	}

	claim, err := app.Orchestrator.LookupActiveClaim(ctx)
	if err != nil {
		fmt.Fprintln(app.Err, "stale:", err)
		return 1
	}
	if claim == nil {
		fmt.Fprintln(app.Err, "stale: no active claim")
		return 1
	}

	state, err := app.Orchestrator.Load(ctx, claim.ItemRef)
	if err != nil {
		fmt.Fprintln(app.Err, "stale:", err)
		return 1
	}
	f := flowForType(app, state.Type)
	if f == nil {
		fmt.Fprintf(app.Err, "stale: no flow in this binary handles item type %q\n", state.Type)
		return 1
	}

	stepID, ok := app.resolveStaleTarget(f, state, fs.Arg(0))
	if !ok {
		return 1
	}

	// Check artifact state.
	switch artifactState(state, stepID) {
	case stateResolved:
		// proceed
	case stateStale:
		payload := stalePayload{
			StepID:       string(stepID),
			Item:         claim.ItemRef.Display,
			AlreadyStale: true,
		}
		return app.emit(mode, payload, func() {
			fmt.Fprintf(app.Out, "%q is already stale on %s\n", stepID, claim.ItemRef.Display)
		})
	case statePending:
		fmt.Fprintf(app.Err, "stale: %q is pending (not yet resolved) — nothing to mark stale\n", stepID)
		return 1
	case stateSkipped:
		fmt.Fprintf(app.Err, "stale: %q is skipped (not required) — nothing to mark stale\n", stepID)
		return 1
	}

	// Emit warnings on stderr before the backend write.
	var warnings []string
	if pending := state.PendingQuestions(); len(pending) > 0 {
		w := fmt.Sprintf("%d pending question(s); answer them before re-running", len(pending))
		fmt.Fprintln(app.Err, "stale: warning:", w)
		warnings = append(warnings, w)
	}
	rec := state.Artifact(flow.ArtifactId(stepID))
	if rec.GrantedInvocations > 0 && rec.Invocations >= rec.GrantedInvocations {
		w := fmt.Sprintf("step %q has no budget headroom; use 'grant' to add budget before re-running", stepID)
		fmt.Fprintln(app.Err, "stale: warning:", w)
		warnings = append(warnings, w)
	}
	if state.Park != nil {
		w := fmt.Sprintf("item is parked (%s on %q); marking stale does not clear the park", state.Park.Kind, state.Park.Step)
		fmt.Fprintln(app.Err, "stale: warning:", w)
		warnings = append(warnings, w)
	}

	if err := app.Orchestrator.MarkStale(ctx, claim.ItemRef, stepID); err != nil {
		fmt.Fprintln(app.Err, "stale:", err)
		return 1
	}

	payload := stalePayload{
		StepID:   string(stepID),
		Item:     claim.ItemRef.Display,
		Warnings: warnings,
	}
	return app.emit(mode, payload, func() {
		fmt.Fprintf(app.Out, "marked %q stale on %s\n", stepID, claim.ItemRef.Display)
	})
}

// resolveStaleTarget turns an operator-supplied argument into a step id for
// the stale command, or refuses with a message that names the legal ids.
func (app *App) resolveStaleTarget(f *flow.Flow, state *flow.Item, arg string) (flow.ArtifactId, bool) {
	if li, ok := f.ItemByResult(flow.StepId(arg)); ok {
		switch li.Kind {
		case flow.LifecycleSignal, flow.LifecycleAwait:
			fmt.Fprintf(app.Err, "stale: %q is a signal step and cannot be marked stale\n", arg)
			return "", false
		}
		if _, seeded := state.Artifacts[li.ArtifactId]; !seeded {
			fmt.Fprintf(app.Err, "stale: item not seeded — no artifact record for %q (run `run-step` once first)\n", arg)
			return "", false
		}
		return li.ArtifactId, true
	}
	// The human label is NOT an identity. Say so, and name the id.
	if li, ok := f.Item(arg); ok {
		fmt.Fprintf(app.Err, "stale: %q is a step label, not a step id — did you mean %q?\n", arg, li.Result())
		return "", false
	}
	// Seeded but no longer in the flow.
	if _, seeded := state.Artifacts[flow.ArtifactId(arg)]; seeded {
		fmt.Fprintf(app.Err, "stale: warning — %q is seeded on this item but no longer part of flow %q; marking stale anyway\n", arg, f.Name())
		return flow.ArtifactId(arg), true
	}
	ids := grantableIDs(f)
	fmt.Fprintf(app.Err, "stale: unknown step id %q\n", arg)
	fmt.Fprintf(app.Err, "       valid ids for flow %q: %s\n", f.Name(), idList(ids))
	if guess, ok := didYouMean(arg, ids); ok {
		fmt.Fprintf(app.Err, "       did you mean %q?\n", guess)
	}
	return "", false
}
