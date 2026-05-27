package flow

import (
	"fmt"
	"slices"
)

// Flow is one ordered list of lifecycle items (steps + signal waits). A flow
// is selected by cli.App for a given item if its Types() match item.Type and
// all RequireSignal preconditions are satisfied.
type Flow struct {
	name           string
	types          []ItemType
	steps          []*step
	stepByName     map[string]*step
	stepByResult   map[string]*step // keyed by ArtifactId OR SignalId as string
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
		stepByResult: map[string]*step{},
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
func (f *Flow) AddStep(name string, result ArtifactId, do StepHandler, opts ...StepOption) {
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
	if _, dup := f.stepByResult[string(result)]; dup {
		panic(fmt.Sprintf("flow.AddStep: duplicate result %q in flow %q", result, f.name))
	}
	s := &step{
		kind:     stepArtifact,
		name:     name,
		artifact: result,
		handler:  do,
		required: true, // default — Optional overrides via StepOption
	}
	for _, o := range opts {
		o(s)
	}
	f.steps = append(f.steps, s)
	f.stepByName[name] = s
	f.stepByResult[string(result)] = s
}

// AddSignalStep registers a side-effect step that completes when `signal` is
// set on the item. The handler MUST NOT call any ctx.Resolve* — signals are
// never handler-writable. Duplicate name or duplicate signal panics.
func (f *Flow) AddSignalStep(name string, signal SignalId, do StepHandler, opts ...StepOption) {
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
	if _, dup := f.stepByResult[string(signal)]; dup {
		panic(fmt.Sprintf("flow.AddSignalStep: duplicate result %q in flow %q", signal, f.name))
	}
	s := &step{
		kind:     stepSignal,
		name:     name,
		signal:   signal,
		handler:  do,
		required: true,
	}
	for _, o := range opts {
		o(s)
	}
	f.steps = append(f.steps, s)
	f.stepByName[name] = s
	f.stepByResult[string(signal)] = s
}

// AwaitSignal registers a pure wait — no handler. The lifecycle item
// completes when `signal` is set on the item by any means (another flow's
// AddSignalStep, or an external event the backend observes).
func (f *Flow) AwaitSignal(name string, signal SignalId, opts ...StepOption) {
	if name == "" {
		panic("flow.AwaitSignal: empty step name")
	}
	if signal == "" {
		panic("flow.AwaitSignal: empty signal id")
	}
	if _, dup := f.stepByName[name]; dup {
		panic(fmt.Sprintf("flow.AwaitSignal: duplicate step name %q in flow %q", name, f.name))
	}
	if _, dup := f.stepByResult[string(signal)]; dup {
		panic(fmt.Sprintf("flow.AwaitSignal: duplicate result %q in flow %q", signal, f.name))
	}
	s := &step{
		kind:     stepAwait,
		name:     name,
		signal:   signal,
		required: true,
	}
	for _, o := range opts {
		o(s)
	}
	f.steps = append(f.steps, s)
	f.stepByName[name] = s
	f.stepByResult[string(signal)] = s
}

// RequireSignal adds an eligibility precondition. The flow is only selected
// by cli.App once this signal is already set on the item.
func (f *Flow) RequireSignal(signal SignalId) {
	if signal == "" {
		panic("flow.RequireSignal: empty signal id")
	}
	f.requireSignals = append(f.requireSignals, signal)
}

// AcceptsType returns true if this flow handles the given item type. An empty
// Types() set means universal (every type matches).
func (f *Flow) AcceptsType(t ItemType) bool {
	if len(f.types) == 0 {
		return true
	}
	return slices.Contains(f.types, t)
}

// Pending returns true iff the named lifecycle item is unresolved on the
// given ItemState.
func (f *Flow) Pending(s *ItemState, name string) bool {
	st := f.stepByName[name]
	if st == nil {
		return false
	}
	return f.stepPending(s, st)
}

// stepPending — internal predicate that knows how to derive "is this step
// complete?" from the ItemState, per step kind.
func (f *Flow) stepPending(state *ItemState, st *step) bool {
	switch st.kind {
	case stepArtifact:
		rec := state.Artifact(st.artifact)
		if !rec.Resolved {
			return true
		}
		if rec.Stale {
			return true
		}
		// StaleAfter / StaleOnCommit comparisons happen here once the SDK
		// has produced-at timestamps for the dependency artifacts. The
		// minimum derivation is: if Stale is set on the record, retry.
		return false
	case stepSignal, stepAwait:
		return !state.SignalSet(st.signal)
	}
	return false
}

// DeriveNext returns the first unresolved lifecycle item in registration
// order, with ok==true. ok==false means the flow has nothing more to do on
// this item.
func (f *Flow) DeriveNext(s *ItemState) (string, bool) {
	for _, st := range f.steps {
		if f.stepPending(s, st) {
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
	LifecycleSignal                            // AddSignalStep — handler side-effects; backend writes signal
	LifecycleAwait                             // AwaitSignal — no handler; pure wait
)

// LifecycleItem is the orchestrator-facing view of one entry in the flow's
// ordered list. Returned by Flow.Item / Flow.Items.
type LifecycleItem struct {
	Name       string
	Kind       LifecycleKind
	ArtifactId ArtifactId // set when Kind==LifecycleArtifact
	SignalId   SignalId   // set when Kind==LifecycleSignal or LifecycleAwait
	Required   bool
	Handler    StepHandler // nil when Kind==LifecycleAwait
	Budget     StepBudget  // resolved (merged with defaults)
}

// Result returns the result identifier as a string — either the ArtifactId or
// the SignalId, depending on Kind. Used for InvocationResult / budget keying.
func (li LifecycleItem) Result() string {
	if li.Kind == LifecycleArtifact {
		return string(li.ArtifactId)
	}
	return string(li.SignalId)
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
		Name:     st.name,
		Required: st.required,
		Handler:  st.handler,
		Budget:   resolveBudget(st.budget),
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
// given ItemState.
func (f *Flow) IsReady(s *ItemState) bool {
	for _, sig := range f.requireSignals {
		if !s.SignalSet(sig) {
			return false
		}
	}
	return true
}

// IsDone returns true iff every required lifecycle item is resolved.
func (f *Flow) IsDone(s *ItemState) bool {
	for _, st := range f.steps {
		if !st.required {
			continue
		}
		if f.stepPending(s, st) {
			return false
		}
	}
	return true
}

// TerminalReason returns a short reason string when the flow has stopped
// making progress. Empty string means "still pending / ready to dispatch."
func (f *Flow) TerminalReason(s *ItemState) string {
	if f.IsDone(s) {
		return "done"
	}
	if !f.IsReady(s) {
		return "awaiting-preconditions"
	}
	if _, ok := f.DeriveNext(s); !ok {
		return "no-pending-steps"
	}
	return ""
}

// SeedSpec returns the ArtifactSpec slice the backend should pre-load at seed
// time. Reads the per-step StepOption values (merged with defaults).
func (f *Flow) SeedSpec(artifactDefs map[ArtifactId]ArtifactDef) []ArtifactSpec {
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
			Required: st.required,
			Budget:   resolveBudget(st.budget),
		})
	}
	return out
}

// StepBudget returns the resolved budget for the named step, merging
// StepOption values with package defaults.
func (f *Flow) StepBudget(name string) (StepBudget, bool) {
	st, ok := f.stepByName[name]
	if !ok {
		return StepBudget{}, false
	}
	return resolveBudget(st.budget), true
}
