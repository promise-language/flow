package cli

import (
	"context"
	"testing"

	"github.com/promise-language/flow"
)

// What the runner writes to the treasurer's ledger, and what it reads back out
// of it before dispatching. The ledger itself is the orchestrators' (their own
// tests); this file is the wiring between them and cli.RunOne.

// The pre-dispatch gate refuses a step that has already spent its cost cap, and
// refuses it BEFORE the handler runs. The cap is flow.EffectiveBudget's — the
// binary's policy plus what has been granted — so a step with a policy and no
// grant is still capped, which is the shape every item now starts in.
//
// A cost cap that only bit inside a dispatch would buy the step one more turn
// every time it was asked, which is the invocations gate's failure mode on the
// axis that costs money.
func TestRunOne_ParksBeforeDispatchWhenTheCostCapIsSpent(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			t.Error("the handler ran for a step whose cost cap is already spent")
			return flow.StepResult{}, nil
		}, flow.StepConfig{Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, &stubAgent{name: "stub"})
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {MaxCostUSD: 2}}

	ctx := context.Background()
	if err := be.AddCost(ctx, claim.ItemRef, "plan", 2.50); err != nil {
		t.Fatalf("AddCost: %v", err)
	}

	res, err := RunOne(ctx, app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" {
		t.Fatalf("status = %q, want parked. res=%+v", res.Status, res)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkTreasurerRefused || res.Park.Axis != flow.AxisCost {
		t.Fatalf("Park = %+v, want treasurer-refused on cost", res.Park)
	}
	// The park carries the run's own snapshot of the axis, which is the record
	// flow.GrantClearsPark judges a later top-up against: without the cap that
	// was refused on, no grant can ever clear this park.
	var snap flow.AxisReport
	for _, a := range res.Park.Axes {
		if a.Axis == flow.AxisCost {
			snap = a
		}
	}
	if snap.Used != 2.50 || snap.Granted != 2 {
		t.Errorf("cost axis snapshot = %+v, want 2.50 spent against a cap of 2", snap)
	}
	if be.ParkRequest("1") == nil {
		t.Error("the park was not recorded on the orchestrator")
	}
}

// A ledger row is keyed by StepId, not by an artifact, so a SIGNAL step is
// counted like any other: a step dispatched again and again while its signal
// never arrives is exactly what a treasurer has to be able to see, and the
// artifact-only carve-out this replaces made it invisible.
func TestRunOne_ASignalStepsDispatchesAndTimeReachTheLedger(t *testing.T) {
	rounds := 0
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddSignalStep("create pull request", "pr-open", func(ctx flow.StepCtx) (flow.StepResult, error) {
			rounds++
			if rounds == 1 {
				// The request could not be opened, so nothing completed and the
				// route did not move: the next advance dispatches this same step
				// again, which is the case the counter exists for.
				return flow.StepResult{}, ctx.Park(flow.ParkRequest{
					Kind: flow.ParkBlocked, Reason: "the request could not be opened",
				})
			}
			return ctx.Finalize(flow.DispositionResolved, "the change is proposed"), nil
		}, flow.StepConfig{Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, &stubAgent{name: "stub"})

	ctx := context.Background()
	for _, want := range []string{"parked", "done"} {
		res, err := RunOne(ctx, app, claim)
		if err != nil {
			t.Fatalf("RunOne: %v", err)
		}
		if res.Status != want {
			t.Fatalf("res = %+v, want %s", res, want)
		}
	}

	state, err := be.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	row := state.Ledger.Row("pr-open")
	if row.Dispatches != 2 {
		t.Errorf("Dispatches = %d on the signal step's row, want 2", row.Dispatches)
	}
	if row.Active <= 0 {
		t.Errorf("Active = %v on the signal step's row, want the time it spent recorded", row.Active)
	}
}
