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
	name              string
	types             []ItemType
	steps             []*step
	stepByDescription map[string]*step
	stepByResult      map[StepId]*step // keyed by the step's result id — ArtifactId or SignalId
	// entry is the one step carrying StepConfig{Entry: true}, recorded as it
	// is registered. Derived from the declarations rather than a second copy
	// of them: it is what makes a second entry refusable at the moment it is
	// declared, and it is what ValidateGraph walks from.
	entry          *step
	requireSignals []SignalId
	// roles is the declared role vocabulary, in declaration order, and
	// roleIndex is the membership test over it. The declarations are the SINGLE
	// SOURCE OF TRUTH for what this flow declares: every reference — a step's
	// tag, a lookup by name, the awaited role an item records — is matched
	// against them and nothing else (docs/resolution.md § Whose move it is).
	roles     []RoleDecl
	roleIndex map[RoleName]bool
}

// NewFlow constructs an empty flow. `types` declares which Item.Type values
// this flow handles; nil/empty means universal (applies to all item types).
// Slice (not variadic) so call sites read as
// `NewFlow("merge", []flow.ItemType{"task", "bug"})` rather than the
// visually-confusing variadic form.
func NewFlow(name string, types []ItemType) *Flow {
	return &Flow{
		name:              name,
		types:             types,
		stepByDescription: map[string]*step{},
		stepByResult:      map[StepId]*step{},
		roleIndex:         map[RoleName]bool{},
	}
}

func (f *Flow) Name() string      { return f.name }
func (f *Flow) Types() []ItemType { return f.types }

// Steps returns the ordered registration list (state-independent), as the
// steps' descriptions.
func (f *Flow) Steps() []string {
	out := make([]string, len(f.steps))
	for i, s := range f.steps {
		out[i] = s.description
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

// AddStep registers a handler-produced artifact step. The handler completes by
// returning a StepResult whose payload matches the artifact's ArtifactType in
// App.Artifacts. The first argument is a DESCRIPTION — display text, never an
// identity; the step's identity is `result`. Duplicate description or duplicate
// result panics.
func (f *Flow) AddStep(description string, result ArtifactId, do StepHandler, cfg StepConfig) {
	if description == "" {
		panic("flow.AddStep: empty step description")
	}
	if result == "" {
		panic("flow.AddStep: empty artifact id")
	}
	if do == nil {
		panic(fmt.Sprintf("flow.AddStep: step %q has nil handler; use AwaitSignal for handlerless steps", description))
	}
	if _, dup := f.stepByDescription[description]; dup {
		panic(fmt.Sprintf("flow.AddStep: duplicate step description %q in flow %q", description, f.name))
	}
	if _, dup := f.stepByResult[StepId(result)]; dup {
		panic(fmt.Sprintf("flow.AddStep: duplicate result %q in flow %q", result, f.name))
	}
	s := f.prepareStep("AddStep", stepArtifact, description, cfg)
	s.artifact = result
	s.handler = do
	f.appendStep(s, StepId(result))
}

// AddSignalStep registers a side-effect step that completes when `signal` is
// set on the item. Its StepResult carries no payload — signals are never
// handler-writable. Duplicate description or duplicate signal panics.
func (f *Flow) AddSignalStep(description string, signal SignalId, do StepHandler, cfg StepConfig) {
	if description == "" {
		panic("flow.AddSignalStep: empty step description")
	}
	if signal == "" {
		panic("flow.AddSignalStep: empty signal id")
	}
	if do == nil {
		panic(fmt.Sprintf("flow.AddSignalStep: step %q has nil handler; use AwaitSignal for pure waits", description))
	}
	if _, dup := f.stepByDescription[description]; dup {
		panic(fmt.Sprintf("flow.AddSignalStep: duplicate step description %q in flow %q", description, f.name))
	}
	if _, dup := f.stepByResult[StepId(signal)]; dup {
		panic(fmt.Sprintf("flow.AddSignalStep: duplicate result %q in flow %q", signal, f.name))
	}
	s := f.prepareStep("AddSignalStep", stepSignal, description, cfg)
	s.signal = signal
	s.handler = do
	f.appendStep(s, StepId(signal))
}

// AwaitSignal registers a pure wait — no handler. The lifecycle item
// completes when `signal` is set on the item by any means (another flow's
// AddSignalStep, or an external event the orchestrator observes).
func (f *Flow) AwaitSignal(description string, signal SignalId, cfg StepConfig) {
	if description == "" {
		panic("flow.AwaitSignal: empty step description")
	}
	if signal == "" {
		panic("flow.AwaitSignal: empty signal id")
	}
	if _, dup := f.stepByDescription[description]; dup {
		panic(fmt.Sprintf("flow.AwaitSignal: duplicate step description %q in flow %q", description, f.name))
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
			description, f.name, cfg.Role))
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
			description, f.name, cfg.MayFinalize))
	}
	s := f.prepareStep("AwaitSignal", stepAwait, description, cfg)
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
func (f *Flow) prepareStep(registrar string, kind stepKind, description string, cfg StepConfig) *step {
	cfg = cfg.normalized()

	// The second entry is refusable here — the first one is already recorded.
	// Zero entries is not: it is unknowable until registration has ended, so
	// ValidateGraph refuses that one.
	if cfg.Entry && f.entry != nil {
		panic(fmt.Sprintf("flow.%s: step %q in flow %q declares Entry, but step %q already does; exactly one entry",
			registrar, description, f.name, f.entry.description))
	}
	if !cfg.Capture.Valid() {
		panic(fmt.Sprintf("flow.%s: step %q in flow %q has Capture %q, which is not one of %v",
			registrar, description, f.name, cfg.Capture, AllCaptureSources()))
	}
	if !cfg.Needs.Valid() {
		panic(fmt.Sprintf("flow.%s: step %q in flow %q has Needs %q, which is not one of %v",
			registrar, description, f.name, cfg.Needs, AllNeedsStates()))
	}
	if !cfg.Leaves.Valid() {
		panic(fmt.Sprintf("flow.%s: step %q in flow %q has Leaves %q, which is not one of %v",
			registrar, description, f.name, cfg.Leaves, AllLeavesStates()))
	}
	seenDisposition := map[Disposition]bool{}
	for _, d := range cfg.MayFinalize {
		if !d.Valid() {
			panic(fmt.Sprintf("flow.%s: step %q in flow %q may finalize as %q, which is not one of %v",
				registrar, description, f.name, d, AllDispositions()))
		}
		if seenDisposition[d] {
			panic(fmt.Sprintf("flow.%s: step %q in flow %q lists disposition %q twice in MayFinalize",
				registrar, description, f.name, d))
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
				registrar, description, f.name))
		}
		if seenNext[id] {
			panic(fmt.Sprintf("flow.%s: step %q in flow %q lists successor %q twice in Next",
				registrar, description, f.name, id))
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
		description: description,
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
	f.stepByDescription[s.description] = s
	f.stepByResult[resultKey] = s
	if s.entry {
		f.entry = s
	}
}

// Role declares one role: the name this flow's steps are tagged with, and the
// capabilities an account must hold to assume it — exactly the shape
// docs/flow-registration.md § Roles writes down.
//
//	f.Role("contributor", flow.CapPush)
//	f.Role("maintainer", flow.CapPush, flow.CapMerge)
//
// Order against the step registrations is NOT enforced. Nothing here needs the
// roles first: whether a step's tag names a declaration is checked once
// registration has ended (ValidateGraph), which is the only point at which the
// declared set is knowable whole.
//
// Everything below panics rather than returning an error, like the step
// registrars: these are programming errors caught while the program is being
// assembled (docs/flow-registration.md § Uniqueness invariants). What is NOT
// refused here is an empty capability set — docs/flow-registration.md
// § Startup validation puts that check at startup, alongside the tags it makes
// meaningless, rather than at the declaration.
func (f *Flow) Role(name RoleName, caps ...Capability) {
	if name == "" {
		panic("flow.Role: empty role name")
	}
	if f.roleIndex[name] {
		panic(fmt.Sprintf("flow.Role: duplicate role %q in flow %q", name, f.name))
	}
	for _, c := range caps {
		if !c.Valid() {
			panic(fmt.Sprintf("flow.Role: role %q in flow %q requires capability %q, which is not one of %v",
				name, f.name, c, AllCapabilities()))
		}
	}
	f.roleIndex[name] = true
	// Cloned in, for the reason prepareStep clones its slices: a declaration
	// the caller still holds the backing array to could be rewritten after
	// startup validation certified it.
	f.roles = append(f.roles, RoleDecl{Name: name, Capabilities: slices.Clone(caps)})
}

// Roles returns the declared roles in declaration order, deeply cloned —
// symmetrically with the copy Role makes on the way in, and for the same
// reason: a caller ranging the declarations must not be able to rewrite them
// through the view it was handed.
func (f *Flow) Roles() []RoleDecl {
	out := make([]RoleDecl, len(f.roles))
	for i, r := range f.roles {
		out[i] = RoleDecl{Name: r.Name, Capabilities: slices.Clone(r.Capabilities)}
	}
	return out
}

// RoleNames returns the declared role names in declaration order. What fills
// ErrUnknownRole.Declared — an unknown-role refusal that could not name the
// alternatives leaves the reader to go and find them.
func (f *Flow) RoleNames() []RoleName {
	out := make([]RoleName, len(f.roles))
	for i, r := range f.roles {
		out[i] = r.Name
	}
	return out
}

// DeclaresRole reports whether this flow declares the given role.
//
// It answers from the DECLARATIONS, not from what the steps happen to be tagged
// with. The difference is the whole point: a tag scan makes every typo its own
// declaration, so the one check that would catch it — "this reference names
// nothing" — could never fail. Matching happens only against the declared set
// (docs/resolution.md § Whose move it is), and this is that set.
//
// One predicate rather than a set every caller re-derives: a second answer to
// what is declared is a second place that judgement can differ.
//
// The empty name is never declared: a signal wait carries no role, so a lookup
// on "" is a lookup on nothing.
func (f *Flow) DeclaresRole(role RoleName) bool {
	if role == "" {
		return false
	}
	return f.roleIndex[role]
}

// RequireSignal adds an eligibility precondition. An item is only begun once
// every required signal is already set on it — a gate on eligibility, not a
// lifecycle item: it does not appear in the graph and is never routed to
// (docs/flow-registration.md § Signal preconditions).
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

// AwaitsAfter is what the item awaits once an entry carrying this route lands:
// the successor's declared role, or the signal when the successor is a pure
// wait. The zero value on a finalizing route — a finished flow awaits nobody.
//
// THE SDK COMPUTES IT BECAUSE THE ORCHESTRATOR CANNOT. The step-to-role mapping
// is the flow's, and an orchestrator holds no flow (docs/orchestrator.md
// § Writing payloads), so the value is handed across on the entry rather than
// derived on the far side. It sits beside Position because both read one
// election and answer a different question about it.
//
// A route naming no registered lifecycle item awaits nothing. Position refuses
// that route loudly, which is where the defect is reported; answering with a
// role invented for an id that names nothing would be worse than answering
// empty.
//
// Account is never set here. The entry records a decision; who holds the role is
// read from the journal (Item.AccountForRole), and a value written into the
// decision would be a second copy of that answer.
func (f *Flow) AwaitsAfter(r Route) Awaits {
	if r.Finalizes() {
		return Awaits{}
	}
	succ, ok := f.stepByResult[r.Next]
	if !ok {
		return Awaits{}
	}
	if succ.kind == stepAwait {
		return Awaits{Signal: succ.signal}
	}
	return Awaits{Role: succ.role}
}

// Pending, stepPending, DeriveNext, IsDone and TerminalReason below are the
// OUTGOING derivation: position as a checklist walked in registration order,
// which Flow.Position replaces. They are not a second copy of one rule kept in
// sync with it — nothing reads both, and Position becomes the only derivation
// once the route is elected (#233), appended (#239), persisted (#240) and
// declared by the shipped flow (#245). The checklist cannot go before then:
// until something appends an entry, Position would report "no entry declared"
// for every item, which the advance reads as "nothing left to do".

// Pending returns true iff the lifecycle item with this description is
// unresolved on the given Item.
func (f *Flow) Pending(it *Item, description string) bool {
	st := f.stepByDescription[description]
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
		// Unresolved means pending, and that is the whole test. The
		// Required=false opt-out and the stale bit both went with seeding and
		// MarkStale — nothing writes either any more, so a branch reading them
		// would skip every step on an item with no seeded records at all.
		return !state.Artifact(st.artifact).Resolved
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
			return st.description, true
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
	// Description is the human label the registration gave this item. Display
	// only: nothing is keyed on it, and the step's identity is Result().
	Description string
	Kind        LifecycleKind
	ArtifactId  ArtifactId // set when Kind==LifecycleArtifact
	SignalId    SignalId   // set when Kind==LifecycleSignal or LifecycleAwait
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

// Item returns the LifecycleItem with this description. ok==false if the
// description is unknown to this flow.
func (f *Flow) Item(description string) (LifecycleItem, bool) {
	st, ok := f.stepByDescription[description]
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
// the human description).
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
		Description: st.description,
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
// per-step opt-out to skip: routing subsumed step optionality, and nothing
// writes a not-required record any more.
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
