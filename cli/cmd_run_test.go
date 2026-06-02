package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/backend/fake"
)

// takeoverBackend wraps fake.Backend to count and steer MarkManualTakeover
// calls — the test asserts on whether/when the cli's manual-takeover hook
// fires depending on the FLOW_DISPATCHED_BY_RUNNER env. T0481.
type takeoverBackend struct {
	*fake.Backend
	calls    int
	failWith error
}

func (b *takeoverBackend) MarkManualTakeover(ctx context.Context, claim flow.Claim) error {
	b.calls++
	return b.failWith
}

// Compile-time assert: takeoverBackend implements flow.ManualTakeover.
var _ flow.ManualTakeover = (*takeoverBackend)(nil)

// TestCmdRun_ManualSetsManualAndClearsPark (T0481): when the binary is run
// by hand (FLOW_DISPATCHED_BY_RUNNER unset), cli.cmdRun MUST call the
// backend's MarkManualTakeover so the operator's "I'm driving now" signal
// flows through (set Manual, resolve any FlowPark).
func TestCmdRun_ManualSetsManualAndClearsPark(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		})
	}, a)
	wrapped := &takeoverBackend{Backend: be}
	app.Backend = wrapped

	// Belt-and-braces: ensure the env var is NOT set for this case (the runner
	// would set it; an operator-typed run-step has no such pre-export).
	t.Setenv(dispatchedByRunnerEnv, "")

	code := app.cmdRun(context.Background(), nil)
	if code != 0 {
		t.Fatalf("cmdRun = %d, want 0", code)
	}
	if wrapped.calls != 1 {
		t.Errorf("MarkManualTakeover calls = %d, want 1 (operator-driven run-step)", wrapped.calls)
	}
}

// TestCmdRun_OrchestratedSkipsTakeover (T0481): when the runner spawned this
// process (FLOW_DISPATCHED_BY_RUNNER=1), the takeover side effects MUST NOT
// fire — the orchestrator owns the lease/manual decisions, and applying the
// operator-takeover signal here would flip Manual=true on an auto-dispatched
// item.
func TestCmdRun_OrchestratedSkipsTakeover(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		})
	}, a)
	wrapped := &takeoverBackend{Backend: be}
	app.Backend = wrapped

	t.Setenv(dispatchedByRunnerEnv, "1")

	code := app.cmdRun(context.Background(), nil)
	if code != 0 {
		t.Fatalf("cmdRun = %d, want 0", code)
	}
	if wrapped.calls != 0 {
		t.Errorf("MarkManualTakeover calls = %d, want 0 (orchestrator-spawned run-step)", wrapped.calls)
	}
}

// TestCmdRun_TakeoverFailureDoesNotBlockStep (T0481): a manual-takeover
// failure surfaces as a warning on Err but does NOT abort the step — the
// user's intent is the step, the takeover is bookkeeping.
func TestCmdRun_TakeoverFailureDoesNotBlockStep(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		})
	}, a)
	wrapped := &takeoverBackend{Backend: be, failWith: errors.New("tracker unreachable")}
	app.Backend = wrapped

	t.Setenv(dispatchedByRunnerEnv, "")

	code := app.cmdRun(context.Background(), nil)
	if code != 0 {
		t.Fatalf("cmdRun = %d, want 0 (takeover failure must not abort the step)", code)
	}
	if wrapped.calls != 1 {
		t.Errorf("MarkManualTakeover calls = %d, want 1 (failure path still calls)", wrapped.calls)
	}
}
