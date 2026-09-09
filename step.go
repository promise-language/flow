package flow

import "slices"

// stepKind discriminates the three lifecycle item shapes.
type stepKind int

const (
	stepArtifact stepKind = iota + 1 // AddStep
	stepSignal                       // AddSignalStep
	stepAwait                        // AwaitSignal
)

// step is the internal record for one lifecycle item in a flow's ordered
// list. Exposed surface is via Flow's Add*/Steps/DeriveNext helpers.
type step struct {
	kind stepKind
	// description is the human label the registration gave this item. Display
	// text, never an identity: the step's identity is its result id, which is
	// what the journal, a grant and a park all key on (docs/flow-registration.md
	// § Three kinds of lifecycle item).
	description string
	artifact    ArtifactId // set when kind==stepArtifact
	signal      SignalId   // set when kind==stepSignal or stepAwait
	handler     StepHandler
	role        RoleName
	entry       bool
	next        []StepId
	mayFinalize []Disposition
	capture     CaptureSource
	writes      WriteContract
	needs       NeedsState
	leaves      LeavesState
}

// resultName returns the result identifier (artifact id OR signal id) as a
// string. Used for InvocationResult / budget keying.
func (s *step) resultName() string {
	if s.kind == stepArtifact {
		return string(s.artifact)
	}
	return string(s.signal)
}

// result returns the step's identity: its ArtifactId when it produces an
// artifact, its SignalId when it completes on a signal. Inside a flow the two
// are one namespace, which is what a StepId names.
func (s *step) result() StepId { return StepId(s.resultName()) }

// WriteContract declares what a step is permitted to change in the worktree.
// The zero value means "writes nothing" — the strictest contract.
type WriteContract struct {
	MayBranch   bool // may switch or create branches
	MayCommit   bool // may move HEAD (new commits)
	MayEditTree bool // may leave tracked files dirty
}

// CaptureSource declares where an artifact step's one result comes from —
// docs/artifacts-and-signals.md § Capture. Closed at two. It says where the
// payload comes from, never what shape it has: the shape is the artifact's
// declared ArtifactType.
type CaptureSource string

const (
	// CaptureReturned — the handler ends by returning the payload. The
	// ordinary case for a result that exists only as the step's output: a
	// plan, a briefing, a structured report.
	CaptureReturned CaptureSource = "returned"
	// CaptureTree — the payload is read from the worktree as the step left
	// it: the commit the branch head names, the patch the tree carries. The
	// honest source for a result whose subject IS the worktree, because a
	// returned copy could disagree with the tree it claims to describe and
	// the tree would be right.
	CaptureTree CaptureSource = "tree"
)

// AllCaptureSources returns every declared source, in declaration order.
// Consumers enumerate it rather than mirroring the set.
func AllCaptureSources() []CaptureSource {
	return []CaptureSource{CaptureReturned, CaptureTree}
}

// Valid reports whether c is one of the two. The empty source is not one —
// StepConfig.normalized resolves it to CaptureReturned before it is stored, so
// a value reaching here empty was never declared.
func (c CaptureSource) Valid() bool { return slices.Contains(AllCaptureSources(), c) }

// NeedsState is the worktree state a step REQUIRES — established mechanically
// before dispatch, from durable state, never trusted to be current
// (docs/flow-registration.md § Step configuration). Closed at three.
//
// Separate from LeavesState because the two are closed at DIFFERENT threes:
// `any` is not something a step can leave, and `as-found` is not something a
// step can require. One shared type would make both illegal halves spellable
// and push the real rule into a per-field check.
type NeedsState string

const (
	// NeedsAny — the step requires nothing established; it runs wherever the
	// worktree already is.
	NeedsAny NeedsState = "any"
	// NeedsBase — the item's base branch, checked out.
	NeedsBase NeedsState = "base"
	// NeedsItemBranch — the item's resolution branch, checked out. A state
	// that cannot be established (the branch does not exist) blocks the item
	// naming the step and the missing state.
	NeedsItemBranch NeedsState = "item-branch"
)

// AllNeedsStates returns every declared state, in declaration order.
func AllNeedsStates() []NeedsState {
	return []NeedsState{NeedsAny, NeedsBase, NeedsItemBranch}
}

// Valid reports whether n is one of the three. The empty state is not one —
// StepConfig.normalized resolves it to NeedsAny before it is stored.
func (n NeedsState) Valid() bool { return slices.Contains(AllNeedsStates(), n) }

// LeavesState is the worktree state a step must END IN, verified before its
// result is captured. Closed at three, and always clean — cleanliness is the
// commit contract's guarantee (docs/resolution.md § The commit contract)
// rather than a fourth value.
type LeavesState string

const (
	// LeavesAsFound — the step is not required to end anywhere in
	// particular; whatever state it was handed is the state it may hand on.
	LeavesAsFound LeavesState = "as-found"
	// LeavesBase — the item's base branch, checked out and clean.
	LeavesBase LeavesState = "base"
	// LeavesItemBranch — the item's resolution branch, checked out and clean.
	LeavesItemBranch LeavesState = "item-branch"
)

// AllLeavesStates returns every declared state, in declaration order.
func AllLeavesStates() []LeavesState {
	return []LeavesState{LeavesAsFound, LeavesBase, LeavesItemBranch}
}

// Valid reports whether l is one of the three. The empty state is not one —
// StepConfig.normalized resolves it to LeavesAsFound before it is stored.
func (l LeavesState) Valid() bool { return slices.Contains(AllLeavesStates(), l) }

// StepHandler is the function dispatched by the SDK for AddStep/AddSignalStep
// lifecycle items. AwaitSignal items have no handler.
//
// A handler completes by RETURNING its election — the route it elects and, on
// an artifact step, the payload to capture — rather than by writing anything
// mid-run: the SDK captures result and route together, so a step never lands
// half of its completion (docs/step-handler.md § Handler signature).
//
// The error is for the ways a handler stops WITHOUT completing: the sentinels
// (ErrPark, ErrQuestion, ErrWaitsOnItems, ErrTransient, ErrRefused) and plain
// failure. Returning the zero StepResult with a nil error completes nothing and
// is refused as ErrStepDidNotComplete.
type StepHandler func(ctx StepCtx) (StepResult, error)

// StepConfig is the per-step configuration passed to AddStep / AddSignalStep /
// AwaitSignal. It is a plain data struct on purpose — every knob a step has is
// a named field here, so a registration reads as one value instead of a list
// of mutating callbacks.
//
// The zero value is legal and is the loosest declaration in every axis: no
// role, not the entry, no declared successors, no finalization, a
// handler-returned result, any worktree state going in, nothing verified
// coming out, and nothing writable. Each defaulting field's empty value
// resolves to the loosest member of its set — see normalized, which is the one
// place that mapping is written down.
//
// There is no budget field. What a resolution may spend is policy held with
// the treasurer, never a step declaration (docs/flow-registration.md § Step
// configuration).
type StepConfig struct {
	// Role is the declared role that performs this step. Required on steps;
	// absent on signal waits, which belong to no role and panic if given one.
	Role RoleName
	// Entry marks the step where an empty journal starts. Exactly one step in
	// the graph carries it: a second panics at registration, and zero is
	// refused by ValidateGraph — it is not knowable while registering, only
	// once registration has ended.
	Entry bool
	// Next declares the step's successors — the only steps its handler may
	// elect. Named by StepId, the result id, because that is a step's identity
	// everywhere; the description is for eyes.
	Next []StepId
	// MayFinalize is the set of finalization dispositions this step may elect.
	// Empty means the step cannot end the flow. Absent on signal waits, which
	// elect nothing and panic if given one.
	MayFinalize []Disposition
	// Capture declares an artifact step's result source. The zero value means
	// CaptureReturned.
	Capture CaptureSource
	// Needs is the worktree state established before the step is dispatched.
	// The zero value means NeedsAny — nothing established.
	Needs NeedsState
	// Writes declares what the step is permitted to change in the worktree.
	// The zero value means "writes nothing" — the strictest contract. See
	// WriteContract.
	Writes WriteContract
	// Leaves is the worktree state the step must end in, verified before its
	// result is captured. The zero value means LeavesAsFound — nothing
	// verified.
	Leaves LeavesState
}

// normalized returns the config with every defaulting field resolved to the
// member its empty value means — the loosest one in each set.
//
// One helper, called by all three registrars, so there is exactly one
// definition of what an unset field means. A second copy would be a second
// answer to "what does StepConfig{} declare", and the two would diverge on the
// first field added.
func (c StepConfig) normalized() StepConfig {
	if c.Capture == "" {
		c.Capture = CaptureReturned
	}
	if c.Needs == "" {
		c.Needs = NeedsAny
	}
	if c.Leaves == "" {
		c.Leaves = LeavesAsFound
	}
	return c
}
