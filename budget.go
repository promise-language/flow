package flow

import "time"

// StepBudget is the resolved set of caps for one step, computed by combining
// StepConfig values with package defaults.
type StepBudget struct {
	MaxInvocations          int
	MaxPromptsPerInvocation int
	MaxCostUSD              float64
	Timeout                 time.Duration
}

// defaultStepBudget is the package-level default applied to any axis the flow
// author did not override. {3 invocations, 1 prompt/invocation, $10, 30m}.
var defaultStepBudget = StepBudget{
	MaxInvocations:          3,
	MaxPromptsPerInvocation: 1,
	MaxCostUSD:              10,
	Timeout:                 30 * time.Minute,
}

// DefaultStepBudget returns the package-level default budget. Exposed for
// inspection / tests.
func DefaultStepBudget() StepBudget { return defaultStepBudget }

// resolveBudget overlays any opt-set axes onto the package defaults. Unset
// fields in `over` (zero value) leave the default in place.
func resolveBudget(over StepBudget) StepBudget {
	out := defaultStepBudget
	if over.MaxInvocations != 0 {
		out.MaxInvocations = over.MaxInvocations
	}
	if over.MaxPromptsPerInvocation != 0 {
		out.MaxPromptsPerInvocation = over.MaxPromptsPerInvocation
	}
	if over.MaxCostUSD != 0 {
		out.MaxCostUSD = over.MaxCostUSD
	}
	if over.Timeout != 0 {
		out.Timeout = over.Timeout
	}
	return out
}
