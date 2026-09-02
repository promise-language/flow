package flow

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
	kind     stepKind
	name     string
	artifact ArtifactId // set when kind==stepArtifact
	signal   SignalId   // set when kind==stepSignal or stepAwait
	handler  StepHandler
	required bool
	budget   StepBudget // before resolveBudget merge with defaults
	writes   WriteContract
}

// resultName returns the result identifier (artifact id OR signal id) as a
// string. Used for InvocationResult / budget keying.
func (s *step) resultName() string {
	if s.kind == stepArtifact {
		return string(s.artifact)
	}
	return string(s.signal)
}

// WriteContract declares what a step is permitted to change in the worktree.
// The zero value means "writes nothing" — the strictest contract.
type WriteContract struct {
	MayBranch   bool // may switch or create branches
	MayCommit   bool // may move HEAD (new commits)
	MayEditTree bool // may leave tracked files dirty
}

// StepHandler is the function dispatched by the SDK for AddStep/AddSignalStep
// lifecycle items. AwaitSignal items have no handler.
type StepHandler func(ctx StepCtx) error

// StepConfig is the per-step configuration passed to AddStep / AddSignalStep /
// AwaitSignal. The zero value is the common case: a Required step with the
// default budget (DefaultStepBudget). It is a plain data struct on purpose —
// every knob a step has is a named field here, so a registration reads as one
// value instead of a list of mutating callbacks.
type StepConfig struct {
	// Optional marks the step's result as NOT required for IsDone. Steps are
	// Required by default (the zero value), so set this only to opt out.
	Optional bool
	// Budget overrides the per-step caps {MaxInvocations, MaxPromptsPerInvocation,
	// MaxCostUSD, Timeout}. Each zero-valued axis inherits DefaultStepBudget
	// (see resolveBudget), so set only the axes that differ from the default.
	Budget StepBudget
	// Writes declares what the step is permitted to change in the worktree.
	// The zero value means "writes nothing" — the strictest contract. See
	// WriteContract.
	Writes WriteContract
}
