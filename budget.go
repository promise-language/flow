package flow

import "time"

// StepBudget is the resolved set of caps for one step, computed by combining
// the binary's cap policy with the package defaults.
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
// costs is capped by MaxCostUSD — checked before each prompt and passed to the
// agent as that turn's own ceiling (AgentRequest.MaxCostUSD), so a turn stops
// at the first model call that crosses the cap rather than running to the end
// and being caught a whole turn later — and what it actually occupies is gated
// by Timeout; the number of prompts it takes to get there moves neither. Capping
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

// ResolveStepBudget overlays any set axes onto the package defaults. Unset
// fields in `over` (zero value) leave the default in place, so the zero
// StepBudget resolves to the defaults whole.
//
// Exported because the caps are no longer part of a step's declaration: they
// are the binary's policy (cli.App.StepBudgets today, the treasurer's once it
// lands), so the resolution has to happen wherever that policy is read rather
// than only inside this package.
func ResolveStepBudget(over StepBudget) StepBudget {
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

// EffectiveBudget is what a step may actually spend: the binary's policy,
// resolved against the package defaults, plus every extension recorded on the
// step's ledger row.
//
// THE ONE PLACE A CAP IS COMPUTED. Nothing seeds caps onto an item any more —
// the ledger records what was granted and the binary holds the policy — so
// every reader that used to compare against a stored `Granted*` field asks here
// instead. A second arithmetic would be a second answer to "may this run", and
// the gate that refuses a dispatch and the `grant` that tops it up must agree
// exactly or an operator grants into a cap nothing reads.
//
// Grant amounts are in each axis's own unit (GrantRecord), so timeout seconds
// become a Duration here and nowhere else.
func EffectiveBudget(base StepBudget, row LedgerRow) StepBudget {
	out := ResolveStepBudget(base)
	out.MaxInvocations += int(row.GrantedOn(AxisInvocations))
	out.MaxPromptsPerInvocation += int(row.GrantedOn(AxisPrompts))
	out.MaxCostUSD += row.GrantedOn(AxisCost)
	out.Timeout += time.Duration(row.GrantedOn(AxisTimeout) * float64(time.Second))
	return out
}

// SessionsAccountedFor is how many agent sessions the route this item has
// actually travelled asks for: one at the entry, and one more for each execution
// of a step declaring SessionFresh (docs/resolution.md § The treasurer).
//
// READ OFF THE JOURNAL, NEVER THE GRAPH. A route may cross the same declaring
// step more than once — nothing forbids a cycle and the disclosure repair uses a
// back edge — so the graph offers no static total, while the journal records
// exactly one entry per execution.
//
// THE ONE ARITHMETIC. The chokepoint classifies an opening by comparing the
// ledger's count against this, and `status` flags an excess by comparing the
// same two numbers. A second sum would let the party that approved a session and
// the report that judged it disagree about what the route asked for.
//
// Three terms:
//   - The entry's one. The first journal entry IS the entry step's first
//     execution, and the resolution's first session is opened for it whether the
//     entry declares `fresh` or inherits `continued` — so index 0 is never
//     counted again below (docs/flow-registration.md § Session continuity).
//   - One per later journal entry whose step declares `fresh`.
//   - One for the pending step when it declares `fresh`, because the execution
//     in flight has opened its session and has no journal entry yet. The pending
//     step is inside the treasurer's vantage by the same section.
//
// It is deliberately an UPPER BOUND — an unregistered step id and a route
// Position refuses contribute nothing rather than failing — because a false
// excess flag accuses a resolution of waste it did not commit, which is worse
// than missing an excess of one.
func (f *Flow) SessionsAccountedFor(it *Item) int {
	if f == nil || it == nil {
		return 0
	}
	n := 1
	// From 1: index 0 is the entry's execution, and its session is the one
	// above.
	for i := 1; i < len(it.Journal); i++ {
		if li, ok := f.ItemByResult(it.Journal[i].Step); ok && li.Session == SessionFresh {
			n++
		}
	}
	// Only on a started resolution: with an empty journal the pending step IS the
	// entry, whose session is the one above.
	if len(it.Journal) > 0 {
		if pos, err := f.Position(it); err == nil && !pos.Finalized && pos.Step.Session == SessionFresh {
			n++
		}
	}
	return n
}
