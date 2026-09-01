package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/promise-language/flow"
)

func (app *App) cmdStatus(ctx context.Context, args []string) int {
	fs := app.newFlagSet("status")
	of := addOutputFlags(fs)
	if !app.parseArgs(fs, args) {
		return 2
	}
	if fs.NArg() > 1 {
		return app.usageError("status: unexpected argument %q (status takes an optional item id)", fs.Arg(1))
	}
	mode, ok := of.mode(app, "status")
	if !ok {
		return 2
	}

	var (
		state     *flow.ItemState
		display   string
		owner     string
		overrides []string
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
		overrides = claim.Overrides
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

	payload := statusPayload{
		Item:      display,
		Title:     state.Item.Title,
		Owner:     owner,
		Overrides: overrides,
		Flow:      flowName(f, typeFlow),
		FlowState: statusFlowState(state, f, typeFlow),
		Finalized: state.Item.Finalized,
		Park:      parkPayloadOf(state.Park),
		Steps:     stepPayloads(typeFlow, state),
		Questions: questionPayloads(state),
	}

	return app.emit(mode, payload, func() {
		fmt.Fprintf(app.Out, "item:  %s\n", payload.Item)
		// Right under the id, where "which task IS this?" is actually asked.
		// Dropped entirely when the backend has no title, so the header never
		// carries an empty field.
		if line := titleLine(payload.Title); line != "" {
			fmt.Fprintf(app.Out, "title: %s\n", line)
		}
		fmt.Fprintf(app.Out, "owner: %s\n", payload.Owner)
		if len(payload.Overrides) > 0 {
			fmt.Fprintf(app.Out, "overrides: %s\n", strings.Join(payload.Overrides, ", "))
		}
		fmt.Fprintf(app.Out, "flow:  %s\n", statusFlowLine(state, f, typeFlow))
		fmt.Fprintln(app.Out)

		// Only the type-matching flow's checklist. The "flow:" line names it, so
		// no redundant header. If no flow handles this item's type, there's
		// nothing.
		printChecklist(app, payload.Steps)

		if payload.Park != nil {
			fmt.Fprintf(app.Out, "\nparked: %s\n", parkLine(payload.Park))
		}
		if len(payload.Questions) > 0 {
			fmt.Fprintln(app.Out, "\nquestions:")
			for _, q := range payload.Questions {
				marker := "[ ]"
				if q.Answered {
					marker = "[x]"
				}
				fmt.Fprintf(app.Out, "  %s %s — %s\n", marker, q.ID, q.Text)
			}
		}
	})
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

// flowName is the bare flow name for the payload: the eligible flow, else the
// type-matching one, else empty.
func flowName(eligible, typeFlow *flow.Flow) string {
	if eligible != nil {
		return eligible.Name()
	}
	if typeFlow != nil {
		return typeFlow.Name()
	}
	return ""
}

// statusFlowState is the machine-readable counterpart of statusFlowLine: the
// same decision, as a closed enum instead of a rendered string.
func statusFlowState(state *flow.ItemState, eligible, typeFlow *flow.Flow) string {
	if state.Item.Finalized {
		return flowStateFinalized
	}
	if eligible != nil {
		return flowStateEligible
	}
	if typeFlow != nil {
		if !state.HasRequiredArtifacts() {
			return flowStateNotSeeded
		}
		return flowStateNoEligibleStep
	}
	return flowStateNoMatchingFlow
}

// statusFlowLine renders the "flow:" value. A finalized item is reported as
// finalized (its run is complete — NOT "no flow eligible", which misleadingly
// implies unstarted/blocked). "(finalized)" is shown ONLY when the persistent
// finalized flag is set — never for a not-yet-seeded item (whose derived
// Finalized() is vacuously true). Otherwise: the currently-eligible flow's
// name; or, when no step is eligible, the type-matching flow tagged with why —
// "(not seeded)" if its finalization checklist has not been seeded yet, else
// "(no eligible step)"; or a no-match note.
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
		if !state.HasRequiredArtifacts() {
			return typeFlow.Name() + " (not seeded)"
		}
		return typeFlow.Name() + " (no eligible step)"
	}
	return "(no matching flow)"
}

// stepPayloads projects a flow's lifecycle items onto the state. Returns an
// empty (non-nil) slice when no flow handles the item's type, so the JSON
// carries [] rather than null.
func stepPayloads(f *flow.Flow, state *flow.ItemState) []stepPayload {
	if f == nil {
		return []stepPayload{}
	}
	items := f.Items()
	out := make([]stepPayload, 0, len(items))
	for _, li := range items {
		sp := stepPayload{
			ID:       li.Result(),
			Label:    li.Name,
			Required: li.Required,
		}
		switch li.Kind {
		case flow.LifecycleArtifact:
			rec := state.Artifact(li.ArtifactId)
			sp.Kind = kindArtifact
			sp.State = artifactState(state, li.ArtifactId)
			sp.Budget = &budgetPayload{
				Invocations:          intAxis{Used: rec.Invocations, Granted: rec.GrantedInvocations},
				CostUSD:              costAxis{Used: rec.CostUSDSpent, Granted: rec.GrantedCostUSD},
				PromptsPerInvocation: intAxis{Used: rec.PromptsThisInvocation, Granted: rec.GrantedPromptsPerInvocation},
				TimeoutSeconds:       grantedOnly{Granted: int(rec.GrantedTimeout.Seconds())},
			}
		case flow.LifecycleSignal, flow.LifecycleAwait:
			if li.Kind == flow.LifecycleSignal {
				sp.Kind = kindSignal
			} else {
				sp.Kind = kindAwait
			}
			sp.State = statePending
			if state.SignalSet(li.SignalId) {
				sp.State = stateResolved
			}
			// Budget stays nil: signal steps own no budget record, which is
			// exactly what makes them invalid grant targets.
		}
		out = append(out, sp)
	}
	return out
}

// artifactState mirrors Flow.stepPending's view of one artifact record.
func artifactState(state *flow.ItemState, id flow.ArtifactId) string {
	rec, seeded := state.Artifacts[id]
	// Operator opt-out: a seeded record marked not-required and not resolved
	// has been struck off the checklist.
	if seeded && !rec.Required && !rec.Resolved {
		return stateSkipped
	}
	switch {
	case rec.Stale:
		return stateStale
	case rec.Resolved:
		return stateResolved
	}
	return statePending
}

func questionPayloads(state *flow.ItemState) []questionPayload {
	out := make([]questionPayload, 0, len(state.Questions))
	for _, q := range state.Questions {
		out = append(out, questionPayload{ID: q.ID, Text: q.Text, Answered: q.Answer != ""})
	}
	return out
}

func parkPayloadOf(p *flow.ParkRequest) *parkPayload {
	if p == nil {
		return nil
	}
	pp := &parkPayload{
		Kind:   string(p.Kind),
		Step:   p.Step,
		Axis:   string(p.Axis),
		Reason: p.Reason,
	}
	for _, a := range p.Axes {
		pp.Axes = append(pp.Axes, axisReportPayload{
			Axis:      string(a.Axis),
			Used:      a.Used,
			Granted:   a.Granted,
			Exhausted: a.Exhausted,
		})
	}
	return pp
}

// parkLine renders a park for humans: "budget-exhausted on "implementation"
// (invocations) — ran 3 times without resolving "implementation"".
//
// A budget park carries a second, indented line listing every axis, because
// the headline names only the axis that tripped first. Granting that one and
// nothing else is what turned a single blocked step into one operator
// round-trip per axis; the axes tagged "(flat)" are the ones a grant has to
// cover to actually get the step moving.
func parkLine(p *parkPayload) string {
	var b strings.Builder
	b.WriteString(p.Kind)
	if p.Step != "" {
		fmt.Fprintf(&b, " on %q", p.Step)
	}
	if p.Axis != "" {
		fmt.Fprintf(&b, " (%s)", p.Axis)
	}
	if p.Reason != "" {
		fmt.Fprintf(&b, " — %s", p.Reason)
	}
	if line := axesLine(p.Axes); line != "" {
		fmt.Fprintf(&b, "\n  axes: %s", line)
	}
	return b.String()
}

// axesLine joins the per-axis meters into one " · "-separated run.
func axesLine(axes []axisReportPayload) string {
	if len(axes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(axes))
	for _, a := range axes {
		parts = append(parts, flow.AxisReport{
			Axis:      flow.BudgetAxis(a.Axis),
			Used:      a.Used,
			Granted:   a.Granted,
			Exhausted: a.Exhausted,
		}.Format())
	}
	return strings.Join(parts, " · ")
}

// statusTitleMax bounds the human "title:" line, in runes. Item.Title is free
// prose the backend supplies — a pasted paragraph or a 300-char sentence would
// swamp the three-line header the operator actually reads and wrap it across
// the terminal. JSON carries the title unclipped, so nothing that needs the
// whole string loses it.
const statusTitleMax = 72

// titleLine renders a title as ONE bounded line: every whitespace run
// (newlines and tabs included) collapses to a single space so a multi-line
// title cannot break the header block, and the result is clipped to
// statusTitleMax RUNES — not bytes, which would split a multi-byte character
// mid-sequence and print a replacement glyph. Returns "" for a title that is
// empty or all whitespace; the caller drops the line entirely.
func titleLine(title string) string {
	s := strings.Join(strings.Fields(title), " ")
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= statusTitleMax {
		return s
	}
	return string(r[:statusTitleMax]) + "…"
}

// printChecklist renders the lifecycle checklist ID-FIRST: the id is the
// step's identity (it keys the budget record and it is the only name `grant`
// accepts), so it leads the line and the human label trails it. Steps that own
// no budget say so, which is what makes the listing sufficient on its own to
// know what can be granted.
func printChecklist(app *App, steps []stepPayload) {
	width := 0
	for _, s := range steps {
		if len(s.ID) > width {
			width = len(s.ID)
		}
	}
	for _, s := range steps {
		fmt.Fprintf(app.Out, "  %s %-*s  %s", stepMarker(s.State), width, s.ID, s.Label)
		if note := budgetNote(s); note != "" {
			fmt.Fprintf(app.Out, "  %s", note)
		}
		fmt.Fprintln(app.Out)
	}
}

func stepMarker(state string) string {
	switch state {
	case stateResolved:
		return "[x]"
	case stateStale:
		return "[~]"
	case stateSkipped:
		return "[-]"
	}
	return "[ ]"
}

// budgetNote is the trailing column: consumption for artifact steps that have
// any, and the "not a grant target" tag for signal steps.
func budgetNote(s stepPayload) string {
	if s.Budget == nil {
		return "(signal — no budget)"
	}
	b := s.Budget
	if b.Invocations.Used == 0 && b.CostUSD.Used == 0 {
		return ""
	}
	parts := make([]string, 0, 2)
	if b.Invocations.Granted > 0 || b.Invocations.Used > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d inv", b.Invocations.Used, b.Invocations.Granted))
	}
	if b.CostUSD.Granted > 0 || b.CostUSD.Used > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f/$%.2f", b.CostUSD.Used, b.CostUSD.Granted))
	}
	return strings.Join(parts, " · ")
}
