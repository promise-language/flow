package flow

import (
	"context"
	"fmt"
)

// CheckFit runs the fit gate on wt and returns a wrapped ErrUnfit when the
// machine is judged unacceptable. Returns nil on any error or non-measured
// outcome (fail-open: a broken fit gate must not block all work).
func CheckFit(ctx context.Context, wt Worktree) error {
	run, err := wt.RunGate(ctx, GateFit)
	if err != nil {
		return nil // fail-open: cannot check → do not refuse
	}
	if run.Outcome != OutcomeMeasured {
		return nil // fail-open: gate did not measure → do not refuse
	}
	verdict, err := wt.Judge(ctx, run)
	if err != nil {
		return nil // fail-open: judge broken → do not refuse
	}
	if !verdict.Acceptable {
		return fmt.Errorf("%s: %w", verdict.Detail, ErrUnfit)
	}
	return nil
}
