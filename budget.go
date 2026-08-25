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
// author did not override. {3 invocations, 50 prompts/invocation, $20, 30m}.
//
// The prompt cap is a runaway backstop, not a budget. What a step actually
// costs is gated by MaxCostUSD and what it actually occupies is gated by
// Timeout; the number of prompts it takes to get there moves neither. Capping
// prompts at 1 therefore bought no protection the other two axes did not
// already provide, while making a prompts park the normal outcome for any
// step that talks to the agent twice — and each of those cost an operator a
// round-trip to raise, repeatedly, on the same steps. 50 sits far above any
// legitimate step so the cap only fires on a genuine loop, which is the one
// thing the other two axes are slow to catch.
var defaultStepBudget = StepBudget{
	MaxInvocations:          3,
	MaxPromptsPerInvocation: 50,
	MaxCostUSD:              20,
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
