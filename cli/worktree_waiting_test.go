package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// gateRunningFlow drives one step that runs one gate and finalizes, which is
// the shortest route through a real dispatch — and a real dispatch is what
// these cases need: the wait is filed by the layer that knows the StepId, and a
// worktree taken outside a step has none.
func gateRunningFlow(f *flow.Flow) {
	f.AddStep("plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
		wt, err := ctx.Worktree()
		if err != nil {
			return flow.StepResult{}, err
		}
		if _, err := wt.RunGate(ctx.Context(), flow.GateIntegration); err != nil {
			return flow.StepResult{}, err
		}
		return ctx.Finalize(flow.DispositionResolved, "measured"), nil
	}, flow.StepConfig{Prompts: flow.PromptsNone, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
}

// A gate that queued reaches the ledger as WAITING, and does not inflate the
// step's active time.
//
// This is the write docs/orchestrator.md § Ledger requires and that nothing
// made before now: "Time blocked on a declared exclusion goes to AddWaiting,
// never here." A wait folded into the active total would read as a step that
// took twenty minutes to do a minute's work, and the contention that caused it
// would be invisible.
func TestRunOne_AGateThatQueuedIsFiledAsWaiting(t *testing.T) {
	app, be, claim := testApp(t, gateRunningFlow, &stubAgent{name: "stub"})
	be.SetGateWait(20 * time.Minute)

	ctx := context.Background()
	if _, err := RunOne(ctx, app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}

	state, err := be.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	row := state.Ledger.Row("plan")
	if row.Waiting != 20*time.Minute {
		t.Errorf("Waiting = %v, want the 20m the gate queued for", row.Waiting)
	}
	if row.Active >= 20*time.Minute {
		t.Errorf("Active = %v — the wait was charged as work", row.Active)
	}
	if state.Ledger.TotalWaiting != 20*time.Minute {
		t.Errorf("TotalWaiting = %v, want 20m", state.Ledger.TotalWaiting)
	}
}

// A gate that queued for nothing files nothing. Every ledger write is a write
// on the orchestrator, and a zero filed on every run would pay for one per gate
// on a machine where nothing ever contended.
func TestRunOne_AGateThatDidNotQueueFilesNoWait(t *testing.T) {
	app, be, claim := testApp(t, gateRunningFlow, &stubAgent{name: "stub"})

	ctx := context.Background()
	if _, err := RunOne(ctx, app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}

	state, err := be.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if row := state.Ledger.Row("plan"); row.Waiting != 0 {
		t.Errorf("Waiting = %v on a gate that never queued, want none", row.Waiting)
	}
	if state.Ledger.TotalWaiting != 0 {
		t.Errorf("TotalWaiting = %v, want none", state.Ledger.TotalWaiting)
	}
	// The step still recorded the work it did — filing no wait must not have
	// cost the active figure.
	if row := state.Ledger.Row("plan"); row.Active <= 0 {
		t.Errorf("Active = %v, want the time the step spent", row.Active)
	}
}

// --- the wait that outlives the context it was paid on ----------------------

// deadlineHonouringOrchestrator answers a ledger write the way a real one does:
// a write handed a context that is already done never reaches the item. The
// fake ignores its context, so a case about a dead context cannot be written
// against it — it would pass whether or not the write was made on one.
type deadlineHonouringOrchestrator struct {
	flow.Orchestrator
	waiting time.Duration
}

func (o *deadlineHonouringOrchestrator) AddWaiting(ctx context.Context, _ flow.ItemRef, _ flow.StepId, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o.waiting += d
	return nil
}

// queuedWorktree reports a run that queued and then got nowhere, which is what
// RunGate returns when the wait ends in the caller giving up.
type queuedWorktree struct {
	flow.Worktree
	waited time.Duration
}

func (q queuedWorktree) RunGate(context.Context, flow.GateName) (flow.GateRun, error) {
	return flow.GateRun{Waited: q.waited}, context.DeadlineExceeded
}

// THE WAIT THAT MATTERS MOST IS THE ONE FILED AFTER THE DEADLINE FIRED. RunGate
// is given the step's own context, so a step that timed out while its gate sat
// in the host-scope queue ends the wait and the context in the same instant —
// and a ledger write made through that context reaches nothing. The figure is
// then lost exactly where it is largest, and the step reads as slow work rather
// than as an arena that spent its dispatch queueing (#435).
func TestWaitingWorktree_FilesTheWaitAfterTheStepDeadlineFired(t *testing.T) {
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	orch := &deadlineHonouringOrchestrator{}
	step := &stepCtx{
		ctx: expired,
		app: &App{Orchestrator: orch},
		li:  flow.LifecycleItem{ArtifactId: "plan"},
	}

	wt := reportingWaits(step, queuedWorktree{waited: 20 * time.Minute})
	if _, err := wt.RunGate(expired, "tested"); err == nil {
		t.Fatal("RunGate reported success for a run that never got the exclusion")
	}
	if orch.waiting != 20*time.Minute {
		t.Errorf("Waiting filed = %v, want the 20m the run spent queued — the write went out on the context the deadline had already ended", orch.waiting)
	}
}

// --- the optional capability the wrapper must not swallow -------------------

// examiningWorktree is a worktree that answers the push guard, which
// pkg/orchestrator/fake deliberately does not.
type examiningWorktree struct {
	flow.Worktree
	refusal error
	asked   *int
}

func (e examiningWorktree) ExaminePush(context.Context) error {
	*e.asked++
	return e.refusal
}

// WITHOUT waitingWorktree.ExaminePush THIS IS SILENTLY BROKEN. flow.ExaminePush
// reaches PushExaminer by type assertion, and an embedding wrapper does not
// satisfy an interface its embedded value satisfies dynamically — so every
// worktree behind the wrapper would answer ErrUnsupported.
//
// The two answers lead opposite ways: a refusal is work for a repair step, an
// unsupported examine is a capability the arena does not have. A step that read
// one as the other would either prompt blind or treat a refused push as
// permitted, which is why this is asserted rather than assumed.
func TestWaitingWorktree_ForwardsThePushGuard(t *testing.T) {
	refusal := errors.New("the guard refused this push")
	asked := 0

	// The embedded Worktree is nil: nothing here calls a method on it, and a
	// stand-in would only obscure that the assertion is the whole subject.
	wrapped := reportingWaits(&stepCtx{}, examiningWorktree{refusal: refusal, asked: &asked})
	err := flow.ExaminePush(context.Background(), wrapped)
	if !errors.Is(err, refusal) {
		t.Errorf("err = %v, want the guard's own refusal — the wrapper swallowed the capability", err)
	}
	if asked != 1 {
		t.Errorf("the inner worktree was asked %d times, want once", asked)
	}
}

// A worktree that genuinely has no push guard still reads as unsupported
// through the wrapper. Answering a refusal here would be worse than swallowing
// one: it would report a guard that does not exist.
func TestWaitingWorktree_KeepsUnsupportedUnsupported(t *testing.T) {
	be := fake.New()
	wt, err := be.Worktree(context.Background(), be.Ref("1"))
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, ok := wt.(flow.PushExaminer); ok {
		t.Skip("the fake worktree grew a push guard; this case needs one without")
	}

	wrapped := reportingWaits(&stepCtx{}, wt)
	if err := flow.ExaminePush(context.Background(), wrapped); !errors.Is(err, flow.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported — the wrapper invented a guard", err)
	}
}
