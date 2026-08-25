package flow

import (
	"context"
	"errors"
)

// ErrBlocked marks a preflight refusal that a human has to clear, as opposed
// to a condition that will pass on its own next cycle.
//
// A bare preflight error means "not now" — the item is skipped, the invocation
// reports success, and the scheduler is expected to come back. That is right
// for a transient gate (an operator flag, a mid-flight close) but wrong for a
// gate that will still be shut on every future run until somebody does
// something: a step waiting on an answer that nobody has written yet reports
// success forever, and the run that was supposed to surface the request looks
// like a no-op.
//
// A preflight error wrapping ErrBlocked reports Status "blocked" instead, and
// the CLI exits non-zero, so an operator (or CI) sees that the run stopped and
// why. It consumes no budget and runs no handler — the only difference from a
// plain skip is the verdict.
//
//	return fmt.Errorf("answer needed on %q: %w", step, flow.ErrBlocked)
var ErrBlocked = errors.New("blocked: needs human action")

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
// A preflight that must stop the run until a human acts wraps ErrBlocked; see
// its docstring for why that is a distinct verdict from a plain skip.
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
