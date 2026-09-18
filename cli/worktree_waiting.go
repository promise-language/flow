package cli

import (
	"context"
	"time"

	"github.com/promise-language/flow"
)

// waitingWorktree reports a run's queue time to the ledger as WAITING.
//
// It exists because the two halves of that sentence live in different places
// and neither can reach the other. The runner is the only party that knows a
// wait happened — docs/orchestrator.md § Ledger says so explicitly, "reported by
// the party that held the wait" — and it carries the measurement out on
// GateRun.Waited. But AddWaiting is keyed by StepId, and a Worktree is
// addressed by ItemRef and has no step: Orchestrator.Worktree takes a ref
// because no gate requires a claim. The step context has both, plus the
// orchestrator to write through.
//
// So the write happens HERE, which is also where the step's ACTIVE time is
// written (stampResult). One owner for both figures is the point: they are two
// sides of one accounting, and a second writer for one of them is how a total
// stops adding up.
//
// IT WRAPS RATHER THAN BEING WRAPPED INTO THE ORCHESTRATOR because the reverse
// would put a cli concern — this dispatch's StepId — inside a boundary that is
// deliberately step-free.
type waitingWorktree struct {
	flow.Worktree
	step *stepCtx
}

// reportingWaits wraps wt so that a declared wait it reports reaches the
// ledger.
func reportingWaits(s *stepCtx, wt flow.Worktree) flow.Worktree {
	return &waitingWorktree{Worktree: wt, step: s}
}

// RunGate runs the gate and files whatever queue time it reports.
//
// The wait is filed on EVERY path, including the one where the runner returned
// an error. A gate that queued twenty minutes and then could not be spawned
// still spent twenty minutes of this arena's wall clock on contention, and a
// figure that counted it only when the gate went on to succeed would understate
// contention exactly where contention is worst.
//
// A failed ledger write is dropped rather than turned into a gate failure, the
// same way stampResult drops AddDuration: the measurement is what the caller
// asked for, and losing the accounting for it must not lose the answer too.
func (w *waitingWorktree) RunGate(ctx context.Context, name flow.GateName) (flow.GateRun, error) {
	run, err := w.Worktree.RunGate(ctx, name)
	w.fileWait(run.Waited)
	return run, err
}

// Run runs the command and files whatever queue time it reports. Commands
// declare no host scope yet (#434); this is here so that when they do, the
// figure is already filed by the same owner as the gate's, rather than by a
// second writer added later.
func (w *waitingWorktree) Run(ctx context.Context, name flow.CommandName) (flow.CommandRun, error) {
	run, err := w.Worktree.Run(ctx, name)
	w.fileWait(run.Waited)
	return run, err
}

// ExaminePush forwards the optional capability instead of hiding it.
//
// THIS METHOD IS THE WHOLE REASON THE WRAPPER IS SAFE. PushExaminer is reached
// by type assertion (flow.ExaminePush), and an embedding wrapper does not
// satisfy an interface its embedded value satisfies dynamically — so without
// this, every worktree behind the wrapper would answer ErrUnsupported, and a
// caller would read "the guard refused" as "never here". Those two lead
// opposite ways: one is work for a repair step, the other is a capability the
// arena does not have.
//
// Delegating to flow.ExaminePush rather than asserting here keeps the
// assertion in one place: it already returns ErrUnsupported for a worktree that
// does not implement it, which is exactly the answer to forward.
func (w *waitingWorktree) ExaminePush(ctx context.Context) error {
	return flow.ExaminePush(ctx, w.Worktree)
}

func (w *waitingWorktree) fileWait(d time.Duration) {
	if d <= 0 {
		return
	}
	_ = w.step.app.Orchestrator.AddWaiting(w.step.ctx, w.step.claim.ItemRef, w.step.li.Result(), d)
}
