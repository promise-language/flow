package flow

import "context"

// PreflightFunc is an optional cross-flow gate run by cli.RunOne on every
// dispatch, AFTER Backend.LoadState and the terminal-done short-circuit
// (so a completed item still retires) and BEFORE seed / handler dispatch.
// Non-nil error short-circuits the invocation with
// InvocationResult{Status: "skipped"} and the error as reason; no handler
// runs, no budget is consumed.
//
// Use Preflight for preconditions common to every flow in a binary that
// may change between Backend.ListEligible (which filters at scheduling
// time) and RunOne dispatch:
//
//   - per-item "manual" / "do not auto-process" flags flipped by an
//     operator after dispatch
//   - the item closed by the user mid-flight
//   - claim-vs-current-state divergence (the runner's lease is still
//     valid but the server's view of the item has moved on)
//
// For per-flow preconditions, use Flow.RequireSignal. For per-step
// preconditions, check inside the handler. Preflight is the right hook
// only for cross-flow, binary-wide gates.
//
// Preflight is NOT a place to decide "this item is terminal." Terminal
// detection is owned by cli.SelectFlow (which already returns Status:
// "done" when no flow has any pending lifecycle item). A preflight that
// returns nil lets SelectFlow's existing terminal check run; a preflight
// that returns an error marks the invocation skipped, leaving the item
// available for the next eligibility cycle.
type PreflightFunc func(ctx context.Context, state *ItemState) error

// ChainPreflight composes multiple preflights into one. Returns the first
// non-nil error; nil if all checks pass. nil entries in the chain are
// skipped, so callers can compose conditionally without rebuilding the
// slice.
func ChainPreflight(checks ...PreflightFunc) PreflightFunc {
	return func(ctx context.Context, state *ItemState) error {
		for _, c := range checks {
			if c == nil {
				continue
			}
			if err := c(ctx, state); err != nil {
				return err
			}
		}
		return nil
	}
}
