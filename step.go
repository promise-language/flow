package flow

import "time"

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
	kind          stepKind
	name          string
	artifact      ArtifactId // set when kind==stepArtifact
	signal        SignalId   // set when kind==stepSignal or stepAwait
	handler       StepHandler
	required      bool
	staleAfter    []ArtifactId
	staleOnCommit bool
	budget        StepBudget // before resolveBudget merge with defaults
}

// resultName returns the result identifier (artifact id OR signal id) as a
// string. Used for InvocationResult / budget keying.
func (s *step) resultName() string {
	if s.kind == stepArtifact {
		return string(s.artifact)
	}
	return string(s.signal)
}

// StepHandler is the function dispatched by the SDK for AddStep/AddSignalStep
// lifecycle items. AwaitSignal items have no handler.
type StepHandler func(ctx StepCtx) error

// StepOption mutates a step at registration time. Composable; the SDK applies
// them in order.
type StepOption func(*step)

// Required marks a step's result as required for IsDone. Default for AddStep.
var Required StepOption = func(s *step) { s.required = true }

// Optional marks a step's result as optional — IsDone ignores it.
var Optional StepOption = func(s *step) { s.required = false }

// StaleAfter marks this step's artifact as stale whenever the named artifact
// has been produced more recently.
func StaleAfter(id ArtifactId) StepOption {
	return func(s *step) { s.staleAfter = append(s.staleAfter, id) }
}

// StaleOnCommit marks this step's artifact as stale whenever HEAD has moved
// since it was produced.
var StaleOnCommit StepOption = func(s *step) { s.staleOnCommit = true }

// MaxInvocations caps how many times this step may run over an item's life.
// Default 3. Exhaustion parks the item with Axis=invocations.
func MaxInvocations(n int) StepOption {
	return func(s *step) { s.budget.MaxInvocations = n }
}

// MaxPromptsPerInvocation caps ctx.Agent().Run calls within one invocation.
// Default 1.
func MaxPromptsPerInvocation(n int) StepOption {
	return func(s *step) { s.budget.MaxPromptsPerInvocation = n }
}

// MaxCostUSD caps cumulative AgentResponse.CostUSD across all invocations.
// Default $10.
func MaxCostUSD(d float64) StepOption {
	return func(s *step) { s.budget.MaxCostUSD = d }
}

// Timeout caps per-invocation wall clock. Default 30m.
func Timeout(d time.Duration) StepOption {
	return func(s *step) { s.budget.Timeout = d }
}
