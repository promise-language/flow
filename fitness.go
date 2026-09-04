package flow

import (
	"context"
	"fmt"
)

// CheckFit runs the `fit` gate on wt and reports whether this machine may be
// given work. It returns nil when the machine is fit and a wrapped ErrUnfit
// when it is not.
//
// It is the ONE implementation of the question. `fit` is a gate like any other
// — run through RunGate, judged through Judge — and giving it a call of its own
// is what previously let it return a verdict with no run, so that "could not
// measure" and "measured, unacceptable" became one answer.
//
// IT FAILS CLOSED. A RunGate error, an outcome that is not a measurement, and a
// Judge error all mean unfit, because NO OUTCOME IS NOT A PASSING OUTCOME. The
// earlier reading — "cannot check → do not refuse" — treated a fit gate whose
// own check errored as fit, which is precisely the state `fit` exists to stop
// anyone proceeding from.
//
// Stopping costs nothing that matters: unfitness is reported as blocked with
// the claim held and the item unmodified, so an unfit arena waits and
// re-measures rather than losing the lease (docs/environment.md, "Unfit is a
// wait, not a verdict").
func CheckFit(ctx context.Context, wt Worktree) error {
	run, err := wt.RunGate(ctx, GateFit)
	if err != nil {
		return fmt.Errorf("fit gate could not be run (%v): %w", err, ErrUnfit)
	}
	if run.Outcome != OutcomeMeasured {
		detail := run.Detail
		if detail == "" {
			detail = "no detail reported"
		}
		return fmt.Errorf("fit gate reported %s, not a measurement (%s): %w", run.Outcome, detail, ErrUnfit)
	}
	verdict, err := wt.Judge(ctx, run)
	if err != nil {
		return fmt.Errorf("fit gate measurement could not be judged (%v): %w", err, ErrUnfit)
	}
	if !verdict.Acceptable {
		return fmt.Errorf("%s: %w", verdict.Detail, ErrUnfit)
	}
	return nil
}
