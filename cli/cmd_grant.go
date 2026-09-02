package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/promise-language/flow"
)

// defaultInvocationHeadroom / defaultPromptHeadroom are the top-up increments
// used when the operator names no amount. One invocation is deliberately
// stingy: a park means the step already burned its allowance, and an operator
// watching a runaway wants the next attempt to be observable, not a blank
// cheque. The cost and timeout defaults come from the step's own configured
// budget (one more run's worth) — see grantIncrement.
//
// Prompts is not stingy, because it is not a spend gate: cost and timeout
// bound what a step can actually consume, and the number of prompts it takes
// to get there moves neither. A one-prompt top-up just re-parks the step on
// the same axis, which is the loop this value exists to avoid.
const (
	defaultInvocationHeadroom = 1
	defaultPromptHeadroom     = 25
	// minCostHeadroom floors the cost top-up for a step whose configured cap
	// is zero/unset, so `grant` on a cost park always buys SOME room.
	minCostHeadroom = 1.0
	// minTimeoutHeadroom floors the timeout top-up for the same reason, and
	// also catches truncation: grants are carried in whole seconds, so a step
	// budget under 1s would otherwise round to a grant of zero and report
	// success while buying nothing.
	minTimeoutHeadroom = 1
)

// plannedGrant is one step's computed budget delta, before it is applied.
type plannedGrant struct {
	id     string
	grant  flow.Grant
	before flow.ArtifactRecord
}

// empty reports whether this plan would write nothing.
func (p plannedGrant) empty() bool { return p.grant == (flow.Grant{}) }

func (app *App) cmdGrant(ctx context.Context, args []string) int {
	fs := app.newFlagSet("grant")
	invocations := fs.Int("invocations", 0, "invocations to grant (park/--all: headroom over consumption)")
	prompts := fs.Int("prompts", 0, "prompts-per-invocation to grant")
	cost := fs.Float64("cost", 0, "cost (USD) to grant (park/--all: headroom over spend)")
	timeout := fs.Int("timeout", 0, "timeout seconds to grant")
	all := fs.Bool("all", false, "top up every pending step instead of the parked one")
	dryRun := fs.Bool("dry-run", false, "print what would be granted; write nothing")
	of := addOutputFlags(fs)
	if !app.parseArgs(fs, args) {
		return 2
	}
	// Which axes the operator actually typed. Park mode reads amounts as
	// headroom, so "--cost 0" (a stated zero) has to be distinguishable from
	// an absent --cost, which a plain zero-value check cannot do.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	mode, ok := of.mode(app, "grant")
	if !ok {
		return 2
	}

	if fs.NArg() > 1 {
		return app.usageError("grant: unexpected argument %q (grant takes at most one step id)", fs.Arg(1))
	}
	if fs.NArg() == 1 && *all {
		return app.usageError("grant: --all sweeps every pending step; it cannot be combined with the step id %q", fs.Arg(0))
	}
	if *invocations < 0 || *prompts < 0 || *cost < 0 || *timeout < 0 {
		return app.usageError("grant: --invocations / --prompts / --cost / --timeout must be >= 0")
	}
	if fs.NArg() == 1 && strings.TrimSpace(fs.Arg(0)) == "" {
		return app.usageError("grant: empty step id")
	}

	claim, err := app.Backend.LookupActiveClaim(ctx, app.Owner)
	if err != nil {
		fmt.Fprintln(app.Err, "grant:", err)
		return 1
	}
	if claim == nil {
		fmt.Fprintln(app.Err, "grant: no active claim")
		return 1
	}
	state, err := app.Backend.LoadState(ctx, *claim)
	if err != nil {
		fmt.Fprintln(app.Err, "grant:", err)
		return 1
	}
	f := flowForType(app, state.Item.Type)
	if f == nil {
		fmt.Fprintf(app.Err, "grant: no flow in this binary handles item type %q — nothing to grant\n", state.Item.Type)
		return 1
	}

	amounts := grantAmounts{
		invocations: *invocations,
		prompts:     *prompts,
		cost:        *cost,
		timeout:     *timeout,
		set:         set,
	}

	var (
		payload = grantPayload{Granted: []grantDelta{}, Unchanged: []string{}, DryRun: *dryRun}
		out     planOutcome
	)
	switch {
	case fs.NArg() == 1:
		payload.Mode = grantModeManual
		out = app.planManual(f, state, fs.Arg(0), amounts)
	case *all:
		payload.Mode = grantModeAll
		out = app.planAll(f, state, amounts)
	default:
		payload.Mode = grantModePark
		payload.Park = parkPayloadOf(state.Park)
		out = app.planPark(f, state, claim.ItemRef.Display, amounts)
	}
	// A refusal has already explained itself on stderr; the exit code is the
	// signal. Anything else falls through and emits a payload — including the
	// "nothing to do" cases, so a piped caller always gets one JSON object per
	// successful invocation instead of having to treat empty stdout as a case.
	if out.refused {
		return 2
	}
	payload.Note = out.note
	plans := out.plans

	// Apply. A zero-delta plan is never written: on a backend that stores state
	// in a comment, every Grant is a rewrite, and "top up to what it already
	// has" should cost nothing.
	for _, p := range plans {
		if p.empty() {
			payload.Unchanged = append(payload.Unchanged, p.id)
			continue
		}
		if !*dryRun {
			if err := app.Backend.Grant(ctx, *claim, p.id, p.grant); err != nil {
				fmt.Fprintln(app.Err, "grant:", err)
				return 1
			}
		}
		payload.Granted = append(payload.Granted, deltaOf(p))
	}

	payload.Unparked = app.reportUnparked(ctx, *claim, state, plans, *dryRun)

	return app.emit(mode, payload, func() { printGrantHuman(app, payload, state) })
}

// planOutcome is what a planner returns: work to do, a reason there is none,
// or a refusal.
//
// The distinction between `note` and `refused` is the distinction between "you
// asked for something reasonable and there was nothing to do" (exit 0, with a
// payload) and "you asked for something that cannot be done" (exit 2, stderr
// only).
type planOutcome struct {
	plans   []plannedGrant
	note    string
	refused bool
}

func refuse() planOutcome                   { return planOutcome{refused: true} }
func nothingToDo(note string) planOutcome   { return planOutcome{note: note} }
func planned(p ...plannedGrant) planOutcome { return planOutcome{plans: p} }

// grantAmounts carries the flag values plus which of them the operator
// actually typed — a `--cost 0` is a stated amount, a missing --cost is not.
type grantAmounts struct {
	invocations int
	prompts     int
	cost        float64
	timeout     int
	set         map[string]bool
}

// axisForFlag maps a flag name onto the budget axis it feeds; axisFlagNames is
// the same set in a stable order, for messages that must not vary run to run.
var (
	axisForFlag = map[string]flow.BudgetAxis{
		"invocations": flow.AxisInvocations,
		"prompts":     flow.AxisPrompts,
		"cost":        flow.AxisCost,
		"timeout":     flow.AxisTimeout,
	}
	axisFlagNames = []string{"invocations", "prompts", "cost", "timeout"}
)

// zeroOn reports whether the operator explicitly asked for zero on this axis —
// a stated amount that would grant nothing.
func (a grantAmounts) zeroOn(axis flow.BudgetAxis) bool {
	switch axis {
	case flow.AxisInvocations:
		return a.set["invocations"] && a.invocations == 0
	case flow.AxisPrompts:
		return a.set["prompts"] && a.prompts == 0
	case flow.AxisCost:
		return a.set["cost"] && a.cost == 0
	case flow.AxisTimeout:
		return a.set["timeout"] && a.timeout == 0
	}
	return false
}

// planManual builds the single-step additive grant: the historical behavior,
// now behind the identity checks.
func (app *App) planManual(f *flow.Flow, state *flow.ItemState, arg string, a grantAmounts) planOutcome {
	id, ok := app.resolveGrantTarget(f, state, arg)
	if !ok {
		return refuse()
	}
	g := flow.Grant{
		Invocations:          a.invocations,
		PromptsPerInvocation: a.prompts,
		CostUSD:              a.cost,
		TimeoutAdd:           int64(a.timeout),
	}
	if g == (flow.Grant{}) {
		// Naming a step id but no amount is a malformed invocation, not a
		// refusal about this item — it takes the same shape as every other one.
		app.usageError("grant: at least one of --invocations / --prompts / --cost / --timeout must be set")
		return refuse()
	}
	return planned(plannedGrant{id: id, grant: g, before: state.Artifact(flow.ArtifactId(id))})
}

// planPark tops up exactly the axis that parked the step. `grant` is almost
// always typed because an item parked, so this is what bare `grant` does.
func (app *App) planPark(f *flow.Flow, state *flow.ItemState, display string, a grantAmounts) planOutcome {
	park := state.Park
	if park == nil {
		// Name the item the way every other command names it (the ref display,
		// e.g. owner/repo#123) — Item.Title is free prose and would bury the
		// message in a sentence.
		fmt.Fprintf(app.Err, "grant: no park recorded on %s — nothing to top up.\n", display)
		fmt.Fprintln(app.Err, "       Use `grant <step-id> --invocations N` to grant explicitly,")
		fmt.Fprintln(app.Err, "       or `grant --all` to sweep every pending step.")
		return refuse()
	}
	if park.Kind != flow.ParkBudgetExhausted {
		fmt.Fprintf(app.Err, "grant: item is parked on %s%s, not a budget cap — granting budget would not unpark it.\n",
			park.Kind, parkDetailSuffix(park, state))
		fmt.Fprintln(app.Err, "      ", remedyFor(park.Kind))
		return refuse()
	}

	id, ok := app.resolveParkStep(f, state, park)
	if !ok {
		return refuse()
	}
	rec := state.Artifact(flow.ArtifactId(id))
	if rec.Resolved && !rec.Stale {
		return nothingToDo(fmt.Sprintf("park on %q is stale — the step has resolved since; nothing to grant", id))
	}
	// The axes a bare `grant` tops up on its own: the one that parked the step,
	// plus any other already at its cap. See parkIncrement for why the second
	// group is not optional.
	auto := parkTopUpAxes(park.Axis, rec)
	// The axes a FLAG may name: the automatic set, plus any axis the park
	// itself reported flat. The two differ because the live record cannot
	// always tell: PromptsThisInvocation resets on the invocation bump, so a
	// step whose run burned every prompt reads as 0/N by the time the operator
	// gets here. The park's snapshot is the only record of it. Refusing
	// --prompts against an axis the park just printed as "(flat)" would make
	// the report unactionable — which is the whole point of printing it.
	grantable := grantableAxes(auto, park)
	// A flag naming anything outside that set is still refused. Granting an
	// axis nobody is blocked on is how a tool ends up "topping up" a step that
	// then re-parks on the axis it was blocked on.
	// Fixed order, not map order: with two wrong flags the message must always
	// name the same one.
	for _, name := range axisFlagNames {
		if a.set[name] && !containsAxis(grantable, axisForFlag[name]) {
			fmt.Fprintf(app.Err, "grant: --%s does not apply — %q is parked on the %s axis and has headroom on %s. Use --%s, or name the step explicitly.\n",
				name, id, park.Axis, axisForFlag[name], park.Axis)
			return refuse()
		}
	}
	// An explicitly flagged axis joins the grant even when it is not in the
	// automatic set — the operator read the report and asked for it. An axis
	// the report merely flagged is NOT granted without a flag: prompts and
	// timeout cannot block the next dispatch, so topping them up unasked would
	// spend budget nobody requested.
	axes := append([]flow.BudgetAxis(nil), auto...)
	for _, name := range axisFlagNames {
		axis := axisForFlag[name]
		if a.set[name] && !containsAxis(axes, axis) {
			axes = append(axes, axis)
		}
	}
	// An explicit zero on an axis the step is blocked on asks for a grant that
	// grants nothing. Reporting that as "already has headroom" would be a lie
	// about the step.
	for _, axis := range axes {
		if !a.zeroOn(axis) {
			continue
		}
		switch {
		case axis == park.Axis:
			fmt.Fprintf(app.Err, "grant: --%s 0 would grant nothing on a step parked for lack of it\n", axis)
		case containsAxis(auto, axis):
			fmt.Fprintf(app.Err, "grant: --%s 0 would grant nothing — %q is also out of %s and would re-park at once\n", axis, id, axis)
		default:
			fmt.Fprintf(app.Err, "grant: --%s 0 would grant nothing on %q\n", axis, id)
		}
		return refuse()
	}

	g := parkIncrement(axes, rec, stepBudgetFor(f, id), a)
	if g == (flow.Grant{}) {
		return nothingToDo(fmt.Sprintf("%q already has headroom on the %s axis — park is stale; nothing to grant", id, park.Axis))
	}
	return planned(plannedGrant{id: id, grant: g, before: rec})
}

// grantableAxes widens the automatic set with every axis the park reported
// exhausted, so a flag can name anything the operator saw tagged "(flat)" in
// the park report. Reporting an axis as flat and then refusing to grant it is
// the contradiction this avoids.
func grantableAxes(auto []flow.BudgetAxis, park *flow.ParkRequest) []flow.BudgetAxis {
	// Copy, never alias: the caller appends to `auto` as well, and two slices
	// growing into one backing array would overwrite each other's tail.
	out := append([]flow.BudgetAxis(nil), auto...)
	for _, r := range park.Axes {
		if r.Exhausted && !containsAxis(out, r.Axis) {
			out = append(out, r.Axis)
		}
	}
	return out
}

// parkTopUpAxes is the axis set bare `grant` acts on: the parked axis first,
// then every other axis that is already at its cap.
func parkTopUpAxes(parked flow.BudgetAxis, rec flow.ArtifactRecord) []flow.BudgetAxis {
	axes := []flow.BudgetAxis{parked}
	for _, axis := range blockingAxes(rec) {
		if axis != parked {
			axes = append(axes, axis)
		}
	}
	return axes
}

// blockingAxes reports the axes that would park the step on its very NEXT
// dispatch, mirroring RunOne's pre-dispatch gates exactly — a step whose
// record trips one of these never reaches its handler.
//
// Prompts and timeout are absent by design, and not by oversight. Prompts is a
// per-invocation cap whose counter resets on every invocation, so a fresh
// dispatch always gets its first prompt through; timeout is a per-run duration
// that kills a run already under way rather than blocking one from starting.
// Neither can re-park a step the instant it restarts, so neither belongs in a
// collateral top-up — they are granted only when they are the parked axis.
func blockingAxes(rec flow.ArtifactRecord) []flow.BudgetAxis {
	var out []flow.BudgetAxis
	if rec.GrantedInvocations > 0 && rec.Invocations >= rec.GrantedInvocations {
		out = append(out, flow.AxisInvocations)
	}
	if rec.GrantedCostUSD > 0 && rec.CostUSDSpent >= rec.GrantedCostUSD {
		out = append(out, flow.AxisCost)
	}
	return out
}

func containsAxis(axes []flow.BudgetAxis, want flow.BudgetAxis) bool {
	for _, a := range axes {
		if a == want {
			return true
		}
	}
	return false
}

// parkIncrement computes the delta that actually gets the step moving again:
// every axis in `axes`, merged into ONE grant so the backend does a single
// write and the park clears once.
//
// Topping up only the parked axis is what made a timeout park unrecoverable: a
// timed-out run burns an invocation on its way out, so by the time the operator
// sees the timeout park the invocations axis is usually flat too. Granting time
// alone bought one dispatch that re-parked on invocations before the handler
// ran, and granting invocations alone bought one that died at the same
// deadline — the item ping-ponged between the two axes forever. Each axis is
// computed at most once, so summing the per-axis grants cannot double up.
func parkIncrement(axes []flow.BudgetAxis, rec flow.ArtifactRecord, budget flow.StepBudget, a grantAmounts) flow.Grant {
	var g flow.Grant
	for _, axis := range axes {
		inc := grantIncrement(axis, rec, budget, a)
		g.Invocations += inc.Invocations
		g.PromptsPerInvocation += inc.PromptsPerInvocation
		g.CostUSD += inc.CostUSD
		g.TimeoutAdd += inc.TimeoutAdd
	}
	return g
}

// stepBudgetFor returns the step's configured budget, falling back to the
// package defaults for a step that is seeded on the item but no longer in the
// flow — its ItemByResult lookup misses, and a zero budget would silently make
// the cost and timeout headroom zero.
func stepBudgetFor(f *flow.Flow, id string) flow.StepBudget {
	if li, ok := f.ItemByResult(id); ok {
		return li.Budget
	}
	return flow.DefaultStepBudget()
}

// planAll sweeps every pending step, raising each axis to at least
// consumption + headroom. The max() shape means a step that already has room
// yields a zero delta and no write at all.
func (app *App) planAll(f *flow.Flow, state *flow.ItemState, a grantAmounts) planOutcome {
	var plans []plannedGrant
	for _, li := range f.Items() {
		if li.Kind != flow.LifecycleArtifact {
			continue
		}
		rec, seeded := state.Artifacts[li.ArtifactId]
		if !seeded {
			continue
		}
		// Only steps with work left: a resolved step needs no budget, and one
		// the operator struck off the checklist (skipped) is not ours to fund.
		switch artifactState(state, li.ArtifactId) {
		case statePending, stateStale:
		default:
			continue
		}
		plans = append(plans, plannedGrant{
			id:     li.Result(),
			grant:  sweepIncrement(rec, li.Budget, a),
			before: rec,
		})
	}
	if len(plans) == 0 {
		return nothingToDo("no pending steps on this item — nothing to top up")
	}
	return planned(plans...)
}

// grantIncrement computes the delta for ONE axis: the amount that raises the
// cap to consumption + headroom. Returns the zero Grant when the step already
// has room, which the caller reports as a stale park rather than a write.
func grantIncrement(axis flow.BudgetAxis, rec flow.ArtifactRecord, budget flow.StepBudget, a grantAmounts) flow.Grant {
	switch axis {
	case flow.AxisInvocations:
		h := defaultInvocationHeadroom
		if a.set["invocations"] {
			h = a.invocations
		}
		if d := rec.Invocations + h - rec.GrantedInvocations; d > 0 {
			return flow.Grant{Invocations: d}
		}
	case flow.AxisCost:
		h := costHeadroom(budget)
		if a.set["cost"] {
			h = a.cost
		}
		if d := rec.CostUSDSpent + h - rec.GrantedCostUSD; d > 0 {
			return flow.Grant{CostUSD: d}
		}
	case flow.AxisPrompts:
		// Prompts is a per-invocation cap, not a meter that fills up
		// (PromptsThisInvocation resets on every invocation), so there is no
		// consumption to clear — the cap itself has to go up.
		h := defaultPromptHeadroom
		if a.set["prompts"] {
			h = a.prompts
		}
		if h > 0 {
			return flow.Grant{PromptsPerInvocation: h}
		}
	case flow.AxisTimeout:
		// Likewise a duration, not a meter: add one more run's worth.
		h := timeoutHeadroom(budget)
		if a.set["timeout"] {
			h = int64(a.timeout)
		}
		if h > 0 {
			return flow.Grant{TimeoutAdd: h}
		}
	}
	return flow.Grant{}
}

// sweepIncrement is the --all counterpart: every axis at once, each one raised
// only as far as it needs to go.
func sweepIncrement(rec flow.ArtifactRecord, budget flow.StepBudget, a grantAmounts) flow.Grant {
	var g flow.Grant
	invH := defaultInvocationHeadroom
	if a.set["invocations"] {
		invH = a.invocations
	}
	if d := rec.Invocations + invH - rec.GrantedInvocations; d > 0 {
		g.Invocations = d
	}
	costH := costHeadroom(budget)
	if a.set["cost"] {
		costH = a.cost
	}
	if d := rec.CostUSDSpent + costH - rec.GrantedCostUSD; d > 0 {
		g.CostUSD = d
	}
	promptFloor := max(budget.MaxPromptsPerInvocation, 1)
	if a.set["prompts"] {
		promptFloor = a.prompts
	}
	if d := promptFloor - rec.GrantedPromptsPerInvocation; d > 0 {
		g.PromptsPerInvocation = d
	}
	// Timeout is left alone unless asked for: it is a per-run duration, so
	// there is no "consumed" value that a sweep could top up.
	if a.set["timeout"] {
		g.TimeoutAdd = int64(a.timeout)
	}
	return g
}

func costHeadroom(budget flow.StepBudget) float64 {
	if budget.MaxCostUSD > 0 {
		return budget.MaxCostUSD
	}
	return minCostHeadroom
}

// timeoutHeadroom is the timeout counterpart, in whole seconds: one more run's
// worth, floored so an unset — or sub-second — step budget still buys time
// rather than truncating to a grant of nothing.
func timeoutHeadroom(budget flow.StepBudget) int64 {
	if s := int64(budget.Timeout.Seconds()); s > 0 {
		return s
	}
	return minTimeoutHeadroom
}

// resolveGrantTarget turns an operator-supplied argument into a step id, or
// refuses with a message that names the legal ids. Every refusal happens
// before any backend write.
func (app *App) resolveGrantTarget(f *flow.Flow, state *flow.ItemState, arg string) (string, bool) {
	// The step id is the identity: exact match, no prefix or case folding.
	if li, ok := f.ItemByResult(arg); ok {
		switch li.Kind {
		case flow.LifecycleSignal, flow.LifecycleAwait:
			fmt.Fprintf(app.Err, "grant: step %q is a signal step and carries no budget (nothing to grant)\n", arg)
			return "", false
		}
		if _, seeded := state.Artifacts[li.ArtifactId]; !seeded {
			fmt.Fprintf(app.Err, "grant: item not seeded — no budget record for %q (run `run-step` once first)\n", arg)
			return "", false
		}
		return arg, true
	}
	// The human label is NOT an identity. Say so, and name the id.
	if li, ok := f.Item(arg); ok {
		fmt.Fprintf(app.Err, "grant: %q is a step label, not a step id — did you mean %q?\n", arg, li.Result())
		return "", false
	}
	// Seeded but no longer in the flow: the flow source moved on while this
	// item was mid-flight. The budget record is real and the intent is
	// unambiguous, so honor it — loudly.
	if _, seeded := state.Artifacts[flow.ArtifactId(arg)]; seeded {
		fmt.Fprintf(app.Err, "grant: warning — %q is seeded on this item but no longer part of flow %q; granting anyway\n", arg, f.Name())
		return arg, true
	}
	ids := grantableIDs(f)
	fmt.Fprintf(app.Err, "grant: unknown step id %q\n", arg)
	fmt.Fprintf(app.Err, "       valid ids for flow %q: %s\n", f.Name(), idList(ids))
	if guess, ok := didYouMean(arg, ids); ok {
		fmt.Fprintf(app.Err, "       did you mean %q?\n", guess)
	}
	return "", false
}

// resolveParkStep maps a park's recorded step onto a grantable id. Parks
// written by this version record the id; one written by an older version
// recorded the human label, so that is accepted too rather than stranding an
// item parked before the upgrade.
func (app *App) resolveParkStep(f *flow.Flow, state *flow.ItemState, park *flow.ParkRequest) (string, bool) {
	id := ""
	switch li, ok := f.ItemByResult(park.Step); {
	case ok && li.Kind != flow.LifecycleArtifact:
		fmt.Fprintf(app.Err, "grant: park names signal step %q, which carries no budget — nothing to grant\n", park.Step)
		return "", false
	case ok:
		id = park.Step
	default:
		// A park written before ParkRequest.Step carried the id recorded the
		// human label; map it rather than strand an item parked across the
		// upgrade.
		if byLabel, found := f.Item(park.Step); found && byLabel.Kind == flow.LifecycleArtifact {
			id = byLabel.Result()
		} else if _, seeded := state.Artifacts[flow.ArtifactId(park.Step)]; seeded {
			id = park.Step
		} else {
			fmt.Fprintf(app.Err, "grant: park names step %q, which is not a step of flow %q — grant it explicitly by id\n", park.Step, f.Name())
			fmt.Fprintf(app.Err, "       valid ids: %s\n", idList(grantableIDs(f)))
			return "", false
		}
	}
	// The budget lives on the seeded record; without one there is nothing for
	// the backend to add to, and its error would name the artifact rather than
	// the real problem.
	if _, seeded := state.Artifacts[flow.ArtifactId(id)]; !seeded {
		fmt.Fprintf(app.Err, "grant: park names %q but the item has no budget record for it (not seeded) — nothing to grant\n", id)
		return "", false
	}
	return id, true
}

// grantableIDs lists the ids that can take a grant — artifact steps only.
func grantableIDs(f *flow.Flow) []string {
	var out []string
	for _, li := range f.Items() {
		if li.Kind == flow.LifecycleArtifact {
			out = append(out, li.Result())
		}
	}
	return out
}

// idList renders the legal-id list, including the case where a flow has none
// (all-signal flows) — where an empty string would read as a formatting bug.
func idList(ids []string) string {
	if len(ids) == 0 {
		return "(none — this flow has no budgeted steps)"
	}
	return strings.Join(ids, ", ")
}

// didYouMean returns the closest candidate to arg: a case-insensitive match,
// or one within a single edit. Deliberately narrow — a suggestion that is
// merely plausible invites a tool to retry it blind.
func didYouMean(arg string, candidates []string) (string, bool) {
	for _, c := range candidates {
		if strings.EqualFold(arg, c) {
			return c, true
		}
	}
	for _, c := range candidates {
		if withinOneEdit(arg, c) {
			return c, true
		}
	}
	return "", false
}

// withinOneEdit reports whether a and b differ by at most one insertion,
// deletion, or substitution.
func withinOneEdit(a, b string) bool {
	if a == b {
		return true
	}
	la, lb := len(a), len(b)
	if la > lb {
		a, b = b, a
		la, lb = lb, la
	}
	if lb-la > 1 {
		return false
	}
	i := 0
	for i < la && a[i] == b[i] {
		i++
	}
	if la == lb {
		// substitution: the rest must match
		return a[i+1:] == b[i+1:]
	}
	// insertion in b
	return a[i:] == b[i+1:]
}

// reportUnparked answers "is the item still parked?" — from the backend where
// it can, by prediction when nothing was written. A dry run predicts with the
// same rule the backend applies (flow.GrantClearsPark), so the preview and the
// real thing cannot disagree.
func (app *App) reportUnparked(ctx context.Context, claim flow.Claim, before *flow.ItemState, plans []plannedGrant, dryRun bool) bool {
	if before.Park == nil {
		return false
	}
	if dryRun {
		for _, p := range plans {
			if flow.GrantClearsPark(before.Park, p.id, applyGrant(p.before, p.grant), p.grant) {
				return true
			}
		}
		return false
	}
	after, err := app.Backend.LoadState(ctx, claim)
	if err != nil {
		// The grants landed; we just can't confirm the park state. Say no
		// rather than claim an unpark we did not observe.
		return false
	}
	return after.Park == nil
}

// applyGrant returns the record as it would look after g — the local mirror
// used for dry-run prediction.
func applyGrant(rec flow.ArtifactRecord, g flow.Grant) flow.ArtifactRecord {
	rec.GrantedInvocations += g.Invocations
	rec.GrantedPromptsPerInvocation += g.PromptsPerInvocation
	rec.GrantedCostUSD += g.CostUSD
	rec.GrantedTimeout += time.Duration(g.TimeoutAdd) * time.Second
	return rec
}

func deltaOf(p plannedGrant) grantDelta {
	d := grantDelta{ID: p.id}
	if p.grant.Invocations != 0 {
		d.Invocations = &intDelta{From: p.before.GrantedInvocations, To: p.before.GrantedInvocations + p.grant.Invocations}
	}
	if p.grant.PromptsPerInvocation != 0 {
		d.PromptsPerInvocation = &intDelta{
			From: p.before.GrantedPromptsPerInvocation,
			To:   p.before.GrantedPromptsPerInvocation + p.grant.PromptsPerInvocation,
		}
	}
	if p.grant.CostUSD != 0 {
		d.CostUSD = &costDelta{From: p.before.GrantedCostUSD, To: p.before.GrantedCostUSD + p.grant.CostUSD}
	}
	if p.grant.TimeoutAdd != 0 {
		from := int(p.before.GrantedTimeout.Seconds())
		d.TimeoutSeconds = &intDelta{From: from, To: from + int(p.grant.TimeoutAdd)}
	}
	return d
}

// parkDetailSuffix adds the concrete blocker to a non-budget park message —
// for a question park, the question itself, which is what the operator has to
// act on.
func parkDetailSuffix(park *flow.ParkRequest, state *flow.ItemState) string {
	if park.Kind != flow.ParkQuestion {
		return ""
	}
	pending := state.PendingQuestions()
	if len(pending) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%s: %q)", pending[0].ID, pending[0].Text)
}

// remedyFor names what actually clears each park kind, so the refusal ends
// with the next action instead of just a no.
func remedyFor(kind flow.ParkKind) string {
	switch kind {
	case flow.ParkQuestion:
		return "Answer the question on the item; the step re-runs from the top once it has an answer."
	case flow.ParkInfraTransient, flow.ParkRemoteUnreachable:
		return "Nothing to grant — this park consumed no budget. Re-run the step once the infrastructure is back."
	case flow.ParkStepDidNotResolve:
		return "The handler returned without resolving its artifact — a code fix, not a budget one."
	case flow.ParkRefused:
		return "Nothing to grant — the failure is deterministic and consumed no budget. Fix the environment or precondition, then re-run."
	}
	return "Clear the blocker on the item, then re-run the step."
}

func printGrantHuman(app *App, payload grantPayload, state *flow.ItemState) {
	// The note is a RESULT ("nothing to grant"), not an error, so it goes to
	// stdout only — writing it to stderr as well would print it twice in a
	// terminal, where both streams land in the same place.
	if payload.Note != "" {
		fmt.Fprintln(app.Out, payload.Note)
	}
	verb := "granted"
	if payload.DryRun {
		verb = "would grant"
	}
	for _, d := range payload.Granted {
		fmt.Fprintf(app.Out, "%s %s: %s\n", verb, d.ID, strings.Join(deltaParts(d), ", "))
	}
	// Untouched steps get ONE line, not one line each plus a tally: the detail
	// that matters is what changed.
	switch {
	case len(payload.Granted) == 0 && len(payload.Unchanged) > 0:
		fmt.Fprintln(app.Out, "all pending steps already have headroom")
	case len(payload.Unchanged) > 0:
		fmt.Fprintf(app.Out, "unchanged (already had headroom): %s\n", strings.Join(payload.Unchanged, ", "))
	}
	if payload.DryRun {
		fmt.Fprintln(app.Out, "dry run — nothing written")
	}
	// A grant too small to clear the cap leaves the item parked. Saying so is
	// the difference between an operator granting again and a tool looping.
	if state.Park != nil && !payload.Unparked && !payload.DryRun && len(payload.Granted) > 0 {
		fmt.Fprintf(app.Out, "still parked: %s\n", parkLine(parkPayloadOf(state.Park)))
	}
	if payload.Unparked {
		// A dry run predicted it; nothing was written, so do not report it as
		// something that happened.
		if payload.DryRun {
			fmt.Fprintln(app.Out, "would unpark the item")
		} else {
			fmt.Fprintln(app.Out, "unparked")
		}
	}
}

func deltaParts(d grantDelta) []string {
	var parts []string
	if d.Invocations != nil {
		parts = append(parts, fmt.Sprintf("invocations %d → %d", d.Invocations.From, d.Invocations.To))
	}
	if d.PromptsPerInvocation != nil {
		parts = append(parts, fmt.Sprintf("prompts/invocation %d → %d", d.PromptsPerInvocation.From, d.PromptsPerInvocation.To))
	}
	if d.CostUSD != nil {
		parts = append(parts, fmt.Sprintf("cost $%.2f → $%.2f", d.CostUSD.From, d.CostUSD.To))
	}
	if d.TimeoutSeconds != nil {
		parts = append(parts, fmt.Sprintf("timeout %ds → %ds", d.TimeoutSeconds.From, d.TimeoutSeconds.To))
	}
	return parts
}
