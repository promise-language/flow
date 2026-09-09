package flow

import (
	"fmt"
	"slices"
)

// Flow is one graph of lifecycle items (steps + signal waits) with one
// declared entry. A binary registers exactly one — what differs by item is the
// route through the graph, never which graph (docs/flow-registration.md § What
// a flow is). `types` is its remit: which item types are this binary's work,
// gating listing and selection and nothing else.
type Flow struct {
	name         string
	types        []ItemType
	steps        []*step
	stepByName   map[string]*step
	stepByResult map[StepId]*step // keyed by the step's result id — ArtifactId or SignalId
	// entry is the one step carrying StepConfig{Entry: true}, recorded as it
	// is registered. Derived from the declarations rather than a second copy
	// of them: it is what makes a second entry refusable at the moment it is
	// declared, and it is what ValidateGraph walks from.
	entry          *step
	requireSignals []SignalId
}

// NewFlow constructs an empty flow. `types` declares which Item.Type values
// this flow handles; nil/empty means universal (applies to all item types).
// Slice (not variadic) so call sites read as
// `NewFlow("merge", []flow.ItemType{"task", "bug"})` rather than the
// visually-confusing variadic form.
func NewFlow(name string, types []ItemType) *Flow {
	return &Flow{
		name:         name,
		types:        types,
		stepByName:   map[string]*step{},
		stepByResult: map[StepId]*step{},
	}
}

func (f *Flow) Name() string      { return f.name }
func (f *Flow) Types() []ItemType { return f.types }

// Steps returns the ordered registration list (state-independent), keyed by
// step name.
func (f *Flow) Steps() []string {
	out := make([]string, len(f.steps))
	for i, s := range f.steps {
		out[i] = s.name
	}
	return out
}

// RequireSignals returns the eligibility preconditions for this flow. Not in
// the lifecycle.
func (f *Flow) RequireSignals() []SignalId {
	out := make([]SignalId, len(f.requireSignals))
	copy(out, f.requireSignals)
	return out
}

// AddStep registers a handler-produced artifact step. The handler MUST call
// the matching ctx.Resolve* (per the artifact's ArtifactType in App.Artifacts)
// before returning nil. Duplicate name or duplicate result panics.
func (f *Flow) AddStep(name string, result ArtifactId, do StepHandler, cfg StepConfig) {
	if name == "" {
		panic("flow.AddStep: empty step name")
	}
	if result == "" {
		panic("flow.AddStep: empty artifact id")
	}
	if do == nil {
		panic(fmt.Sprintf("flow.AddStep: step %q has nil handler; use AwaitSignal for handlerless steps", name))
	}
	if _, dup := f.stepByName[name]; dup {
		panic(fmt.Sprintf("flow.AddStep: duplicate step name %q in flow %q", name, f.name))
	}
	if _, dup := f.stepByResult[StepId(result)]; dup {
		panic(fmt.Sprintf("flow.AddStep: duplicate result %q in flow %q", result, f.name))
	}
	s := f.prepareStep("AddStep", stepArtifact, name, cfg)
	s.artifact = result
	s.handler = do
	f.appendStep(s, StepId(result))
}

// AddSignalStep registers a side-effect step that completes when `signal` is
// set on the item. The handler MUST NOT call any ctx.Resolve* — signals are
// never handler-writable. Duplicate name or duplicate signal panics.
func (f *Flow) AddSignalStep(name string, signal SignalId, do StepHandler, cfg StepConfig) {
	if name == "" {
		panic("flow.AddSignalStep: empty step name")
	}
	if signal == "" {
		panic("flow.AddSignalStep: empty signal id")
	}
	if do == nil {
		panic(fmt.Sprintf("flow.AddSignalStep: step %q has nil handler; use AwaitSignal for pure waits", name))
	}
	if _, dup := f.stepByName[name]; dup {
		panic(fmt.Sprintf("flow.AddSignalStep: duplicate step name %q in flow %q", name, f.name))
	}
	if _, dup := f.stepByResult[StepId(signal)]; dup {
		panic(fmt.Sprintf("flow.AddSignalStep: duplicate result %q in flow %q", signal, f.name))
	}
	s := f.prepareStep("AddSignalStep", stepSignal, name, cfg)
	s.signal = signal
	s.handler = do
	f.appendStep(s, StepId(signal))
}

// AwaitSignal registers a pure wait — no handler. The lifecycle item
// completes when `signal` is set on the item by any means (another flow's
// AddSignalStep, or an external event the orchestrator observes).
func (f *Flow) AwaitSignal(name string, signal SignalId, cfg StepConfig) {
	if name == "" {
		panic("flow.AwaitSignal: empty step name")
	}
	if signal == "" {
		panic("flow.AwaitSignal: empty signal id")
	}
	if _, dup := f.stepByName[name]; dup {
		panic(fmt.Sprintf("flow.AwaitSignal: duplicate step name %q in flow %q", name, f.name))
	}
	if _, dup := f.stepByResult[StepId(signal)]; dup {
		panic(fmt.Sprintf("flow.AwaitSignal: duplicate result %q in flow %q", signal, f.name))
	}
	// A signal wait belongs to no role: it has no handler, performs nothing,
	// and elects nothing, so there is no standing it could require. A tag here
	// is a declaration nothing would ever match, so it is refused where it is
	// written rather than left to mean nothing at runtime.
	if cfg.Role != "" {
		panic(fmt.Sprintf("flow.AwaitSignal: signal wait %q in flow %q declares Role %q; signal waits belong to no role",
			name, f.name, cfg.Role))
	}
	// Nor may a wait finalize, for the same reason and with a sharper
	// consequence. Only a step finalizes, by electing it as its route
	// (docs/resolution.md § Finalizing); a wait's route is static — when the
	// signal is observed its entry is appended carrying the one declared
	// successor. Left declarable, the declaration would also be BELIEVED:
	// ValidateGraph counts anything carrying a MayFinalize as a finalizer, so a
	// wait with one would satisfy finalize-reachability for a graph in which
	// nothing can ever end the flow — the exact defect that check exists to
	// catch.
	if len(cfg.MayFinalize) > 0 {
		panic(fmt.Sprintf("flow.AwaitSignal: signal wait %q in flow %q declares MayFinalize %v; a wait elects nothing, so it cannot finalize",
			name, f.name, cfg.MayFinalize))
	}
	s := f.prepareStep("AwaitSignal", stepAwait, name, cfg)
	s.signal = signal
	f.appendStep(s, StepId(signal))
}

// prepareStep normalizes the config, refuses every declaration a flow cannot
// hold, and builds the part of the step record that does not depend on the
// kind. Shared by the three registrars so one rule cannot become three that
// disagree.
//
// Everything here panics rather than returning an error: these are programming
// errors caught while the program is being assembled, not runtime conditions
// (docs/flow-registration.md § Uniqueness invariants). `registrar` names the
// call that was made, because a registration panic is read without a stack that
// says which of the three it came from.
func (f *Flow) prepareStep(registrar string, kind stepKind, name string, cfg StepConfig) *step {
	cfg = cfg.normalized()

	// The second entry is refusable here — the first one is already recorded.
	// Zero entries is not: it is unknowable until registration has ended, so
	// ValidateGraph refuses that one.
	if cfg.Entry && f.entry != nil {
		panic(fmt.Sprintf("flow.%s: step %q in flow %q declares Entry, but step %q already does; exactly one entry",
			registrar, name, f.name, f.entry.name))
	}
	if !cfg.Capture.Valid() {
		panic(fmt.Sprintf("flow.%s: step %q in flow %q has Capture %q, which is not one of %v",
			registrar, name, f.name, cfg.Capture, AllCaptureSources()))
	}
	if !cfg.Needs.Valid() {
		panic(fmt.Sprintf("flow.%s: step %q in flow %q has Needs %q, which is not one of %v",
			registrar, name, f.name, cfg.Needs, AllNeedsStates()))
	}
	if !cfg.Leaves.Valid() {
		panic(fmt.Sprintf("flow.%s: step %q in flow %q has Leaves %q, which is not one of %v",
			registrar, name, f.name, cfg.Leaves, AllLeavesStates()))
	}
	seenDisposition := map[Disposition]bool{}
	for _, d := range cfg.MayFinalize {
		if !d.Valid() {
			panic(fmt.Sprintf("flow.%s: step %q in flow %q may finalize as %q, which is not one of %v",
				registrar, name, f.name, d, AllDispositions()))
		}
		if seenDisposition[d] {
			panic(fmt.Sprintf("flow.%s: step %q in flow %q lists disposition %q twice in MayFinalize",
				registrar, name, f.name, d))
		}
		seenDisposition[d] = true
	}
	// Next is checked for shape only. Whether an id names a registered item
	// cannot be known while registering — a route forward names a step not
	// declared yet — so ValidateGraph resolves them once the whole graph is in.
	seenNext := map[StepId]bool{}
	for _, id := range cfg.Next {
		if id == "" {
			panic(fmt.Sprintf("flow.%s: step %q in flow %q declares an empty successor id in Next",
				registrar, name, f.name))
		}
		if seenNext[id] {
			panic(fmt.Sprintf("flow.%s: step %q in flow %q lists successor %q twice in Next",
				registrar, name, f.name, id))
		}
		seenNext[id] = true
	}

	// The two slices are COPIED in. A registration hands the flow a slice the
	// caller still holds, and the graph is the thing startup validation
	// certifies: keeping the caller's backing array would let a declaration be
	// rewritten after it was checked, silently and from outside the package.
	// Flow.RequireSignals already copies on the way out for the same reason.
	return &step{
		kind:        kind,
		name:        name,
		role:        cfg.Role,
		entry:       cfg.Entry,
		next:        slices.Clone(cfg.Next),
		mayFinalize: slices.Clone(cfg.MayFinalize),
		capture:     cfg.Capture,
		writes:      cfg.Writes,
		needs:       cfg.Needs,
		leaves:      cfg.Leaves,
	}
}

// appendStep records a fully-built step in registration order and indexes it
// by name and result. Shared tail of AddStep / AddSignalStep / AwaitSignal.
func (f *Flow) appendStep(s *step, resultKey StepId) {
	f.steps = append(f.steps, s)
	f.stepByName[s.name] = s
	f.stepByResult[resultKey] = s
	if s.entry {
		f.entry = s
	}
}

// RequireSignal adds an eligibility precondition. The flow is only selected
// by cli.App once this signal is already set on the item.
func (f *Flow) RequireSignal(signal SignalId) {
	if signal == "" {
		panic("flow.RequireSignal: empty signal id")
	}
	f.requireSignals = append(f.requireSignals, signal)
}

// InRemit reports whether the given item type is this flow's REMIT — which
// item types are this binary's work. An empty Types() set means universal
// (every type is).
//
// The remit gates listing and selection, and nothing else, and it is consulted
// before the journal's first entry and never after
// (docs/flow-registration.md § Item types). It does not choose processing:
// what a type means for an item's route is the entry step's business, elected
// and recorded like every other decision.
func (f *Flow) InRemit(t ItemType) bool {
	if len(f.types) == 0 {
		return true
	}
	return slices.Contains(f.types, t)
}

// Position is where the item stands: the pending lifecycle item, or the
// finalization the flow ended with.
type Position struct {
	// Step is the pending lifecycle item — what runs next. The zero value when
	// Finalized: a finished flow has no pending step.
	Step LifecycleItem
	// Finalized reports that a step elected finalization. That is the ONLY way
	// a flow completes: there is no completion test beside the route — no
	// checklist of required results, and no way to finish other than a step
	// deciding to (docs/flow-registration.md § Routing and completion).
	Finalized bool
	// Disposition is what the finalizing election ended the flow with. Set only
	// when Finalized.
	Disposition Disposition
}

// Position derives where the item stands from its journal and nothing else:
// the route its last entry elected, or the declared entry step when the
// journal is empty (docs/resolution.md § Deriving the next step).
//
// One derivation answering both questions the documents ask of the journal —
// what runs next, and whether the flow is done — so there is no second place
// completion can be decided.
//
// Nothing else is consulted. Not the artifact records ("no resolved bit, no
// checklist, no required flag" — docs/artifacts-and-signals.md § The record),
// not the signals, and not how many times the pending step has already
// completed: "a step runs when the route names it, and for no other reason …
// reaching a step a second time is not an anomaly but a route".
//
// Both refusals are loud rather than answered empty: an empty position reads as
// "nothing left to do", which is the one answer that would finish an item the
// flow never ran.
func (f *Flow) Position(it *Item) (Position, error) {
	entry, ok := it.LastEntry()
	if !ok {
		if f.entry == nil {
			return Position{}, fmt.Errorf("flow %q has an empty journal and declares no entry step: exactly one lifecycle item must be registered with StepConfig{Entry: true}",
				f.name)
		}
		return Position{Step: toLifecycleItem(f.entry)}, nil
	}
	if entry.Route.Finalizes() {
		return Position{Finalized: true, Disposition: entry.Route.Finalize}, nil
	}
	succ, ok := f.stepByResult[entry.Route.Next]
	if !ok {
		return Position{}, fmt.Errorf("flow %q: the journal's last entry, from step %q, elects successor %q, which names no registered lifecycle item",
			f.name, entry.Step, entry.Route.Next)
	}
	return Position{Step: toLifecycleItem(succ)}, nil
}

// Pending, stepPending, DeriveNext, IsDone and TerminalReason below are the
// OUTGOING derivation: position as a checklist walked in registration order,
// which Flow.Position replaces. They are not a second copy of one rule kept in
// sync with it — nothing reads both, and Position becomes the only derivation
// once the route is elected (#233), appended (#239), persisted (#240) and
// declared by the shipped flow (#245). The checklist cannot go before then:
// until something appends an entry, Position would report "no entry declared"
// for every item, which the advance reads as "nothing left to do".

// Pending returns true iff the named lifecycle item is unresolved on the
// given Item.
func (f *Flow) Pending(it *Item, name string) bool {
	st := f.stepByName[name]
	if st == nil {
		return false
	}
	return f.stepPending(it, st)
}

// stepPending — internal predicate that knows how to derive "is this step
// complete?" from the Item, per step kind.
func (f *Flow) stepPending(state *Item, st *step) bool {
	switch st.kind {
	case stepArtifact:
		// Operator opt-out: when the orchestrator surfaces an ArtifactRecord
		// for this id with Required=false AND it isn't already resolved,
		// the operator has explicitly removed it from the checklist
		// ("don't run this step"). Skip — DeriveNext moves on to the
		// next step instead of dispatching a handler against an item
		// the operator marked as not-required.
		//
		// A resolved record with Required=false (run completed, then
		// operator unchecked) is also skipped from a future re-run
		// here, but the resolved branch below would short-circuit
		// anyway, so the opt-out check only matters for unresolved
		// entries.
		if rec, ok := state.Artifacts[st.artifact]; ok && !rec.Required && !rec.Resolved {
			return false
		}
		rec := state.Artifact(st.artifact)
		if !rec.Resolved {
			return true
		}
		if rec.Stale {
			return true
		}
		// Resolved and not flagged stale by the orchestrator → nothing to do.
		return false
	case stepSignal, stepAwait:
		return !state.SignalSet(st.signal)
	}
	return false
}

// DeriveNext returns the first unresolved lifecycle item in registration
// order, with ok==true. ok==false means the flow has nothing more to do on
// this item.
func (f *Flow) DeriveNext(it *Item) (string, bool) {
	for _, st := range f.steps {
		if f.stepPending(it, st) {
			return st.name, true
		}
	}
	return "", false
}

// LifecycleKind discriminates the three lifecycle item shapes the cli
// orchestrator needs to dispatch by.
type LifecycleKind int

const (
	LifecycleArtifact LifecycleKind = iota + 1 // AddStep — handler resolves an artifact
	LifecycleSignal                            // AddSignalStep — handler side-effects; orchestrator writes signal
	LifecycleAwait                             // AwaitSignal — no handler; pure wait
)

// LifecycleItem is the orchestrator-facing view of one entry in the flow's
// ordered list. Returned by Flow.Item / Flow.Items.
type LifecycleItem struct {
	Name       string
	Kind       LifecycleKind
	ArtifactId ArtifactId // set when Kind==LifecycleArtifact
	SignalId   SignalId   // set when Kind==LifecycleSignal or LifecycleAwait
	// Required is hard-set to true for every lifecycle item. Step optionality
	// is gone — routing subsumes it — but the checklist that reads this has
	// not been retired yet (cli/cmd_status.go still renders it), so the field
	// stays and reports the one value there now is. It goes with the checklist
	// itself, in #240 and #242.
	Required bool
	Handler  StepHandler // nil when Kind==LifecycleAwait

	Role        RoleName      // the declared role that performs this step
	Entry       bool          // true on the one step an empty journal starts at
	Next        []StepId      // the successors the step's handler may elect
	MayFinalize []Disposition // the dispositions the step may end the flow with
	Capture     CaptureSource // where an artifact step's result comes from
	Needs       NeedsState    // the worktree state established before dispatch
	Writes      WriteContract // what the step may change in the worktree
	Leaves      LeavesState   // the worktree state the step must end in
}

// Result returns the step's identity: its ArtifactId when it produces an
// artifact, its SignalId when it completes on a signal. Inside a flow the two
// are a single namespace, which is what makes a StepId unambiguous without a
// discriminator.
func (li LifecycleItem) Result() StepId {
	if li.Kind == LifecycleArtifact {
		return StepId(li.ArtifactId)
	}
	return StepId(li.SignalId)
}

// Item returns the LifecycleItem for the named step. ok==false if the name is
// unknown to this flow.
func (f *Flow) Item(name string) (LifecycleItem, bool) {
	st, ok := f.stepByName[name]
	if !ok {
		return LifecycleItem{}, false
	}
	return toLifecycleItem(st), true
}

// ItemByResult returns the LifecycleItem whose Result() equals key. ok==false
// if no step produces that result.
//
// The result id is a step's identity — it keys the budget record and it is the
// only name `grant` accepts — so callers resolving an operator-supplied or
// park-recorded id look it up here rather than through Item (which is keyed by
// the human label).
func (f *Flow) ItemByResult(key StepId) (LifecycleItem, bool) {
	st, ok := f.stepByResult[key]
	if !ok {
		return LifecycleItem{}, false
	}
	return toLifecycleItem(st), true
}

// Items returns the ordered slice of LifecycleItems. Stable; safe to range.
func (f *Flow) Items() []LifecycleItem {
	out := make([]LifecycleItem, len(f.steps))
	for i, st := range f.steps {
		out[i] = toLifecycleItem(st)
	}
	return out
}

func toLifecycleItem(st *step) LifecycleItem {
	li := LifecycleItem{
		Name: st.name,
		// Every lifecycle item is required: there is no step optionality to
		// report. See LifecycleItem.Required.
		Required: true,
		Handler:  st.handler,
		Role:     st.role,
		Entry:    st.entry,
		// Copied out, symmetrically with prepareStep's copy in: a
		// LifecycleItem is a READ of the declaration, and a caller ranging
		// Items() must not be able to rewrite the graph through the view it
		// was handed.
		Next:        slices.Clone(st.next),
		MayFinalize: slices.Clone(st.mayFinalize),
		Capture:     st.capture,
		Needs:       st.needs,
		Writes:      st.writes,
		Leaves:      st.leaves,
	}
	switch st.kind {
	case stepArtifact:
		li.Kind = LifecycleArtifact
		li.ArtifactId = st.artifact
	case stepSignal:
		li.Kind = LifecycleSignal
		li.SignalId = st.signal
	case stepAwait:
		li.Kind = LifecycleAwait
		li.SignalId = st.signal
	}
	return li
}

// IsReady returns true iff all RequireSignal preconditions are set on the
// given Item.
func (f *Flow) IsReady(it *Item) bool {
	for _, sig := range f.requireSignals {
		if !it.SignalSet(sig) {
			return false
		}
	}
	return true
}

// IsDone returns true iff every lifecycle item is resolved. There is no
// per-step opt-out to skip: what an operator can still strike off is the
// artifact RECORD (stepPending's Required=false branch), which lives on the
// item and not in the declaration.
func (f *Flow) IsDone(it *Item) bool {
	for _, st := range f.steps {
		if f.stepPending(it, st) {
			return false
		}
	}
	return true
}

// TerminalReason returns a short reason string when the flow has stopped
// making progress. Empty string means "still pending / ready to dispatch."
func (f *Flow) TerminalReason(it *Item) string {
	if f.IsDone(it) {
		return "done"
	}
	if !f.IsReady(it) {
		return "awaiting-preconditions"
	}
	if _, ok := f.DeriveNext(it); !ok {
		return "no-pending-steps"
	}
	return ""
}

// SeedSpec returns the ArtifactSpec slice the orchestrator should pre-load at
// seed time.
//
// `budgets` is the caller's cap POLICY, keyed by step id — a step with no
// entry, and every zero axis of one that has an entry, takes the package
// default (ResolveStepBudget). It is a parameter rather than something read
// off the steps because budgets are not a step declaration: what a resolution
// may spend belongs to whoever funds it (docs/flow-registration.md § Step
// configuration).
func (f *Flow) SeedSpec(artifactDefs map[ArtifactId]ArtifactDef, budgets map[StepId]StepBudget) []ArtifactSpec {
	out := make([]ArtifactSpec, 0, len(f.steps))
	for _, st := range f.steps {
		if st.kind != stepArtifact {
			continue
		}
		def, ok := artifactDefs[st.artifact]
		if !ok {
			// validation should have caught this; defensive default
			def = ArtifactDef{Id: st.artifact, Type: ArtifactMarkdown}
		}
		out = append(out, ArtifactSpec{
			Id:       st.artifact,
			Type:     def.Type,
			Required: true,
			Budget:   ResolveStepBudget(budgets[st.result()]),
		})
	}
	return out
}
