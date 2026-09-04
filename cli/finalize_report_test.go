package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// nothingLeftToDo builds an app whose flow can never select a step, so RunOne
// reaches the completion branch on the first call. The RequireSignal is never
// set, which is the same shape the existing finalize tests use.
func nothingLeftToDo(t *testing.T) (*App, *fake.Orchestrator, flow.Claim) {
	t.Helper()
	return testApp(t, func(f *flow.Flow) {
		f.RequireSignal("pr-open")
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			t.Fatal("no step should dispatch: the flow's precondition is never met")
			return nil
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
}

// Reaching the end of the flow is NOT the same as the run being recorded
// complete. Finalize refuses an item the orchestrator does not yet consider
// finished — an issue nobody has closed — and that refusal is not a failure of
// this run: the flow has done everything it can, and the item reaches terminal
// by the orchestrator's own means.
//
// So the result is `done` with Finalized FALSE. A caller printing "finalized ✓"
// off the status alone would be reporting something that did not happen.
func TestRunOne_ANotYetTerminalItemIsDoneButNotFinalized(t *testing.T) {
	app, be, claim := nothingLeftToDo(t) // the fake's items start open

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusDone) {
		t.Fatalf("status = %q, want done — a refusal to finalize is not a failed run (reason: %s)",
			res.Status, res.Reason)
	}
	if res.Finalized {
		t.Error("Finalized = true, but Finalize refused — the flag would report a record that was never written")
	}
	// And nothing was recorded on the item either. A finalized item still open
	// would claim the work is over while the orchestrator says it is not.
	item, err := be.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if item.Finalized {
		t.Error("the item reads finalized after Finalize refused")
	}
	// The claim is kept: asking again later is exactly the right response to
	// ErrUnavailable, and a released lease would strand the item.
	if item.Holder.Empty() {
		t.Error("the claim was released even though nothing was finalized")
	}
}

// The counterpart, so the flag is not simply always false: an item the
// orchestrator considers finished finalizes, and says so.
func TestRunOne_ATerminalItemFinalizesAndReportsIt(t *testing.T) {
	app, be, claim := nothingLeftToDo(t)
	be.SetStatus(claim.ItemRef.Display, flow.StatusTerminal, "completed")

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusDone) || !res.Finalized {
		t.Fatalf("status = %q Finalized = %v, want done + finalized (reason: %s)",
			res.Status, res.Finalized, res.Reason)
	}
	item, err := be.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !item.Finalized {
		t.Error("the item does not read finalized after a successful Finalize")
	}
}

// refusingFinalizer fails Finalize with something that is NOT ErrUnavailable —
// a genuine fault rather than "not yet".
type refusingFinalizer struct {
	*fake.Orchestrator
	err error
}

func (b *refusingFinalizer) Finalize(context.Context, flow.ItemRef) error { return b.err }

// A Finalize failure that is not the not-yet-terminal refusal stays a FAILURE.
// Collapsing the two would send an operator hunting for a defect that is not
// there in one direction, and hide a real one in the other.
func TestRunOne_AFinalizeFaultIsStillAFailure(t *testing.T) {
	app, be, claim := nothingLeftToDo(t)
	app.Orchestrator = &refusingFinalizer{Orchestrator: be, err: errors.New("the state comment could not be written")}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusFailed) {
		t.Fatalf("status = %q, want failed — this is not the not-yet-terminal refusal", res.Status)
	}
	if res.Finalized {
		t.Error("Finalized = true after Finalize failed outright")
	}
	if !strings.Contains(res.Reason, "state comment") {
		t.Errorf("reason = %q, want the orchestrator's own account of the failure", res.Reason)
	}
}

// A handler that failed on a machine that is also unfit reports BOTH, and the
// handler's account first.
//
// CheckFit fails CLOSED, so this branch is now reached when the fit gate itself
// is the broken thing. Reporting the fitness verdict alone would throw away the
// only account of what actually failed and blame the machine for it.
func TestRunOne_AFailureOnAnUnfitMachineReportsBoth(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("broken", "plan", func(ctx flow.StepCtx) error {
			ctx.Worktree() // acquire a worktree so the post-handler fitness check runs
			return errors.New("no space left on device")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	be.SetGateVerdict(false)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusBlocked) {
		t.Fatalf("status = %q, want blocked", res.Status)
	}
	if !strings.Contains(res.Reason, "no space left on device") {
		t.Errorf("reason = %q, want the handler's own error — it is the only account of what failed", res.Reason)
	}
	if !strings.Contains(res.Reason, flow.ErrUnfit.Error()) {
		t.Errorf("reason = %q, want it to say the machine is unfit too", res.Reason)
	}
}
