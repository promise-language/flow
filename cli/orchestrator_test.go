package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/backend/fake"
	"github.com/promise-language/flow/pkg/clistate"
)

// ---------------------------------------------------------------------------
// Write-contract gate tests
// ---------------------------------------------------------------------------

func TestWriteContract_BranchViolation(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("plan", "plan", func(ctx flow.StepCtx) error {
			wt, err := ctx.Worktree()
			if err != nil {
				return err
			}
			// Switch branch — violates MayBranch=false.
			if _, err := wt.Branch(ctx.Context(), "rogue-branch", ""); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("done")
		}, flow.StepConfig{Writes: flow.WriteContract{}}) // zero = writes nothing
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusParked) {
		t.Fatalf("status = %q, want parked", res.Status)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkWriteContract {
		t.Fatalf("park = %+v, want ParkWriteContract", res.Park)
	}
	if !strings.Contains(res.Park.Reason, "branch moved") {
		t.Errorf("reason = %q, want contains 'branch moved'", res.Park.Reason)
	}
}

func TestWriteContract_CommitViolation(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("plan", "plan", func(ctx flow.StepCtx) error {
			wt, err := ctx.Worktree()
			if err != nil {
				return err
			}
			// Commit — violates MayCommit=false.
			if err := wt.Commit(ctx.Context(), "rogue commit"); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("done")
		}, flow.StepConfig{Writes: flow.WriteContract{}})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusParked) {
		t.Fatalf("status = %q, want parked", res.Status)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkWriteContract {
		t.Fatalf("park = %+v, want ParkWriteContract", res.Park)
	}
	if !strings.Contains(res.Park.Reason, "commit moved") {
		t.Errorf("reason = %q, want contains 'commit moved'", res.Park.Reason)
	}
}

func TestWriteContract_DirtyTreeViolation(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("plan", "plan", func(ctx flow.StepCtx) error {
			// Acquire the worktree to trigger snapshot capture.
			if _, err := ctx.Worktree(); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("done")
		}, flow.StepConfig{Writes: flow.WriteContract{}})
	}, &stubAgent{name: "stub"})

	// Dirty the worktree BEFORE dispatch so IsDirty returns true after the
	// handler. We need to set it after the worktree is created but before
	// the check runs. The fake's Worktree() creates the fakeWorktree lazily;
	// we pre-create it by fetching once, then set dirty.
	wt, _ := be.Worktree(context.Background(), claim)
	_ = wt
	be.SetDirty(true)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusParked) {
		t.Fatalf("status = %q, want parked", res.Status)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkWriteContract {
		t.Fatalf("park = %+v, want ParkWriteContract", res.Park)
	}
	if !strings.Contains(res.Park.Reason, "uncommitted changes") {
		t.Errorf("reason = %q, want contains 'uncommitted changes'", res.Park.Reason)
	}
}

func TestWriteContract_AllowedCommit(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("impl", "plan", func(ctx flow.StepCtx) error {
			wt, err := ctx.Worktree()
			if err != nil {
				return err
			}
			if err := wt.Commit(ctx.Context(), "allowed commit"); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("done")
		}, flow.StepConfig{Writes: flow.WriteContract{MayCommit: true}})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusDone) {
		t.Fatalf("status = %q, want done", res.Status)
	}
}

func TestWriteContract_NoWorktreeAcquired(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("plan", "plan", func(ctx flow.StepCtx) error {
			// Handler never calls ctx.Worktree() — no check should run.
			return ctx.ResolveMarkdown("done")
		}, flow.StepConfig{Writes: flow.WriteContract{}})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusDone) {
		t.Fatalf("status = %q, want done", res.Status)
	}
}

func TestWriteContract_TransientSkipsCheck(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("plan", "plan", func(ctx flow.StepCtx) error {
			wt, err := ctx.Worktree()
			if err != nil {
				return err
			}
			// Switch branch — would be a violation, but handler returns
			// ErrTransient, which returns early before the check.
			if _, err := wt.Branch(ctx.Context(), "rogue", ""); err != nil {
				return err
			}
			return flow.ErrTransient
		}, flow.StepConfig{Writes: flow.WriteContract{}})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusParked) {
		t.Fatalf("status = %q, want parked", res.Status)
	}
	// Should be infra-transient, NOT write-contract.
	if res.Park == nil || res.Park.Kind != flow.ParkInfraTransient {
		t.Fatalf("park = %+v, want ParkInfraTransient", res.Park)
	}
}

func TestWriteContract_ViolationChargesInvocation(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("plan", "plan", func(ctx flow.StepCtx) error {
			wt, err := ctx.Worktree()
			if err != nil {
				return err
			}
			if _, err := wt.Branch(ctx.Context(), "rogue", ""); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("done")
		}, flow.StepConfig{Writes: flow.WriteContract{}})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusParked) {
		t.Fatalf("status = %q, want parked", res.Status)
	}

	// Invocation must have been charged.
	state, _ := be.LoadState(context.Background(), claim)
	rec := state.Artifact("plan")
	if rec.Invocations != 1 {
		t.Errorf("Invocations = %d, want 1 (violation must charge)", rec.Invocations)
	}
}

func TestWriteContract_AllowedBranch(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("branch", "plan", func(ctx flow.StepCtx) error {
			wt, err := ctx.Worktree()
			if err != nil {
				return err
			}
			if _, err := wt.Branch(ctx.Context(), "feature-x", ""); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("done")
		}, flow.StepConfig{Writes: flow.WriteContract{MayBranch: true}})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusDone) {
		t.Fatalf("status = %q, want done", res.Status)
	}
}

func TestWriteContract_AllowedDirtyTree(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("edit", "plan", func(ctx flow.StepCtx) error {
			if _, err := ctx.Worktree(); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("done")
		}, flow.StepConfig{Writes: flow.WriteContract{MayEditTree: true}})
	}, &stubAgent{name: "stub"})

	wt, _ := be.Worktree(context.Background(), claim)
	_ = wt
	be.SetDirty(true)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusDone) {
		t.Fatalf("status = %q, want done", res.Status)
	}
}

func TestWriteContract_RefusedSkipsCheck(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("plan", "plan", func(ctx flow.StepCtx) error {
			wt, err := ctx.Worktree()
			if err != nil {
				return err
			}
			// Switch branch — would be a violation, but handler returns
			// ErrRefused, which returns early before the check.
			if _, err := wt.Branch(ctx.Context(), "rogue", ""); err != nil {
				return err
			}
			return flow.ErrRefused
		}, flow.StepConfig{Writes: flow.WriteContract{}})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusParked) {
		t.Fatalf("status = %q, want parked", res.Status)
	}
	// Should be refused, NOT write-contract.
	if res.Park == nil || res.Park.Kind != flow.ParkRefused {
		t.Fatalf("park = %+v, want ParkRefused", res.Park)
	}
}

func TestWriteContract_PartialContract_CommitAllowedBranchNot(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("impl", "plan", func(ctx flow.StepCtx) error {
			wt, err := ctx.Worktree()
			if err != nil {
				return err
			}
			// Commit is allowed, but branch switch is not.
			if err := wt.Commit(ctx.Context(), "ok commit"); err != nil {
				return err
			}
			if _, err := wt.Branch(ctx.Context(), "rogue", ""); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("done")
		}, flow.StepConfig{Writes: flow.WriteContract{MayCommit: true}}) // MayBranch defaults false
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusParked) {
		t.Fatalf("status = %q, want parked", res.Status)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkWriteContract {
		t.Fatalf("park = %+v, want ParkWriteContract", res.Park)
	}
	if !strings.Contains(res.Park.Reason, "branch moved") {
		t.Errorf("reason = %q, want contains 'branch moved'", res.Park.Reason)
	}
}

func TestWriteContract_RemedyForParkWriteContract(t *testing.T) {
	got := remedyFor(flow.ParkWriteContract)
	if !strings.Contains(got, "declared contract") {
		t.Errorf("remedyFor(ParkWriteContract) = %q, want contains 'declared contract'", got)
	}
}

// ---------------------------------------------------------------------------

// stubAgent is a fake flow.Agent that returns canned responses and records
// the requests it was handed.
type stubAgent struct {
	name      string
	responses []flow.AgentResponse
	calls     int
	reqs      []flow.AgentRequest
}

func (a *stubAgent) Name() string { return a.name }

func (a *stubAgent) Run(ctx context.Context, req flow.AgentRequest) (*flow.AgentResponse, error) {
	a.reqs = append(a.reqs, req)
	if a.calls >= len(a.responses) {
		return &flow.AgentResponse{LastText: "default"}, nil
	}
	r := a.responses[a.calls]
	a.calls++
	return &r, nil
}

// testApp builds a minimal App with the fake backend pre-populated with one
// item and a single-flow registration.
func testApp(t *testing.T, configure func(*flow.Flow), agent flow.Agent) (*App, *fake.Backend, flow.Claim) {
	t.Helper()
	return testAppItem(t, flow.Item{ID: "1", Type: "task", Title: "test#1"}, []flow.ItemType{"task"}, configure, agent)
}

// testAppItem is testApp over a caller-supplied item and flow type set. The
// item's type is what routes flow selection, so a test about a type no flow
// accepts has to set both ends; every other test takes the task/task default.
func testAppItem(t *testing.T, item flow.Item, types []flow.ItemType, configure func(*flow.Flow), agent flow.Agent) (*App, *fake.Backend, flow.Claim) {
	t.Helper()
	be := fake.New(flow.Signal("pr-open", "test"))
	be.AddItem(item)

	app := &App{
		Backend: be,
		Agent:   agent,
		Artifacts: []flow.ArtifactDef{
			flow.Artifact("plan", flow.ArtifactMarkdown),
			flow.Artifact("commit", flow.ArtifactCommitHash),
			flow.Artifact("implementation", flow.ArtifactPatch),
		},
		Signals: []flow.SignalDef{
			flow.Signal("pr-open", "test"),
		},
	}
	f := flow.NewFlow("implement", types)
	configure(f)
	app.Flows = []*flow.Flow{f}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	app.Owner = "alice"

	// Capture stdout/stderr.
	app.Out = newDiscardWriter()
	app.Err = newDiscardWriter()

	ctx := context.Background()
	ref := flow.ItemRef{BackendName: "fake", Display: item.ID, Ref: json.RawMessage(`"` + item.ID + `"`)}
	claim, err := be.Claim(ctx, ref, "alice", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return app, be, claim
}

// bareBackend wraps a Backend via the interface (not a concrete embed), hiding
// all optional interfaces the concrete type may implement — StateInspector,
// QuestionAnswerer, etc. Used by tests that need to verify "not supported"
// paths.
type bareBackend struct{ flow.Backend }

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func newDiscardWriter() *discardWriter { return &discardWriter{} }

func TestRunOne_SeedsAndDispatchesFirstStep(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})

	}, a)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" || res.Step != "plan" {
		t.Errorf("res = %+v, want step=plan status=done", res)
	}

	state, _ := be.LoadState(context.Background(), claim)
	rec := state.Artifact("plan")
	if !rec.Resolved || rec.Markdown != "the plan" {
		t.Errorf("plan artifact = %+v, want resolved markdown 'the plan'", rec)
	}
	if rec.Invocations != 1 {
		t.Errorf("Invocations = %d, want 1", rec.Invocations)
	}
}

// recordingTelemetry captures every StepProgress call for assertion.
type recordingTelemetry struct {
	events []telemetryEvent
}

type telemetryEvent struct {
	Step   string
	Detail string
}

func (r *recordingTelemetry) StepProgress(ctx context.Context, claim flow.Claim, step, detail string) {
	r.events = append(r.events, telemetryEvent{Step: step, Detail: detail})
}

func TestRunOne_AutoEmitsStepEntry(t *testing.T) {
	tel := &recordingTelemetry{}
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {

			ctx.Notify("write plan", "writing")
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})

	}, &stubAgent{name: "stub"})
	app.Telemetry = tel

	if _, err := RunOne(context.Background(), app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if len(tel.events) != 2 {
		t.Fatalf("events = %+v, want 2 (auto + handler Notify)", tel.events)
	}
	if tel.events[0].Step != "plan" || tel.events[0].Detail != "" {
		t.Errorf("auto-emit event[0] = %+v, want {step=plan, detail=\"\"}", tel.events[0])
	}
	if tel.events[1].Step != "write plan" || tel.events[1].Detail != "writing" {
		t.Errorf("handler event[1] = %+v, want {step=write plan, detail=writing}", tel.events[1])
	}
}

func TestRunOne_AutoEmitSkipsWhenTelemetryNil(t *testing.T) {
	// Sanity: with no telemetry installed RunOne still completes successfully.
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("noop", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("ok")
		}, flow.StepConfig{})

	}, &stubAgent{name: "stub"})
	if app.Telemetry != nil {
		t.Fatal("test setup: expected nil telemetry")
	}
	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Errorf("status = %q, want done", res.Status)
	}
}

// RunOne must write a running record before dispatch and clear it after.
// The handler observes the record mid-flight; after RunOne returns it must
// be gone.
func TestRunOne_WritesAndClearsRunningRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_DIR", filepath.Join(dir, ".flow"))

	var mu sync.Mutex
	var midFlightRec *clistate.RunningRecord

	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			// Read the running record from inside the handler.
			rec, err := clistate.LoadRunning()
			mu.Lock()
			midFlightRec = rec
			mu.Unlock()
			if err != nil {
				return fmt.Errorf("LoadRunning inside handler: %w", err)
			}
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	if _, err := RunOne(context.Background(), app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}

	// Mid-flight: the record must have been present and named the step.
	mu.Lock()
	rec := midFlightRec
	mu.Unlock()
	if rec == nil {
		t.Fatal("running record was nil inside the handler — SaveRunning did not write before dispatch")
	}
	if rec.Step != "plan" {
		t.Errorf("running record step = %q, want %q", rec.Step, "plan")
	}
	if rec.PID == 0 {
		t.Error("running record PID = 0, want the current process PID")
	}

	// After RunOne: the record must be cleared.
	after, err := clistate.LoadRunning()
	if err != nil {
		t.Fatalf("LoadRunning after RunOne: %v", err)
	}
	if after != nil {
		t.Errorf("running record still present after RunOne: %+v", after)
	}
}

// seedFailBackend forces SeedState to fail — modeling a transport/tracker
// error while declaring the checklist.
type seedFailBackend struct{ *fake.Backend }

func (b seedFailBackend) SeedState(ctx context.Context, claim flow.Claim, specs []flow.ArtifactSpec) error {
	return errors.New("boom: seed unavailable")
}

// noopSeedBackend models the pre-fix bug: SeedState silently no-ops, leaving
// the item with no required-artifact checklist.
type noopSeedBackend struct{ *fake.Backend }

func (b noopSeedBackend) SeedState(ctx context.Context, claim flow.Claim, specs []flow.ArtifactSpec) error {
	return nil
}

// TestRunOne_SeedFailureErrorsOutNoStep — when seeding fails, RunOne errors
// out and the step handler NEVER runs. Seeding is mandatory; no fallback.
func TestRunOne_SeedFailureErrorsOutNoStep(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			t.Fatal("step handler ran despite seeding failure — must not happen")
			return nil
		}, flow.StepConfig{})

	}, a)
	app.Backend = seedFailBackend{Backend: be}

	_, err := RunOne(context.Background(), app, claim)
	if err == nil {
		t.Fatal("RunOne returned nil error on seed failure; want a hard error")
	}
	if !strings.Contains(err.Error(), "seed") {
		t.Errorf("err = %v, want it to mention the seed failure", err)
	}
}

// TestRunOne_UnseededAfterNoopSeedErrorsOut — if SeedState reports success but
// the item still has no required-artifact checklist, RunOne refuses to run any
// step and errors out (seeding is mandatory).
func TestRunOne_UnseededAfterNoopSeedErrorsOut(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			t.Fatal("step handler ran against an unseeded item — must not happen")
			return nil
		}, flow.StepConfig{})

	}, a)
	app.Backend = noopSeedBackend{Backend: be}

	_, err := RunOne(context.Background(), app, claim)
	if err == nil {
		t.Fatal("RunOne returned nil error for an unseeded item; want a hard error")
	}
	if !strings.Contains(err.Error(), "seeding is mandatory") {
		t.Errorf("err = %v, want it to name the mandatory-seed refusal", err)
	}
}

func TestRunOne_PreflightSkipsBeforeFlowSelection(t *testing.T) {
	handlerCalled := false
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			handlerCalled = true
			return ctx.ResolveMarkdown("should not run")
		}, flow.StepConfig{})

	}, &stubAgent{name: "stub"})

	app.Preflight = func(ctx context.Context, state *flow.ItemState) error {
		return errors.New("manual flag set")
	}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "skipped" {
		t.Errorf("status = %q, want skipped", res.Status)
	}
	if res.Reason != "preflight: manual flag set" {
		t.Errorf("reason = %q, want 'preflight: manual flag set'", res.Reason)
	}
	if handlerCalled {
		t.Error("handler must not run when preflight refuses")
	}

	// Budget must NOT have been consumed — the artifact isn't seeded yet
	// (seed only happens after preflight passes), so Invocations stays 0
	// after re-loading state.
	state, _ := be.LoadState(context.Background(), claim)
	if rec := state.Artifact("plan"); rec.Invocations != 0 {
		t.Errorf("Invocations = %d, want 0 (preflight skip must not consume budget)", rec.Invocations)
	}
}

func TestRunOne_PreflightPassThrough(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})

	}, &stubAgent{name: "stub"})

	called := 0
	app.Preflight = func(ctx context.Context, state *flow.ItemState) error {
		called++
		return nil
	}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if called != 1 {
		t.Errorf("preflight called %d times, want 1", called)
	}
	if res.Status != "done" {
		t.Errorf("status = %q, want done", res.Status)
	}
}

func TestChainPreflight(t *testing.T) {
	ctx := context.Background()
	state := &flow.ItemState{Item: flow.Item{ID: "x"}}

	calls := []string{}
	a := flow.PreflightFunc(func(context.Context, *flow.ItemState) error {
		calls = append(calls, "a")
		return nil
	})
	b := flow.PreflightFunc(func(context.Context, *flow.ItemState) error {
		calls = append(calls, "b")
		return errors.New("b refused")
	})
	c := flow.PreflightFunc(func(context.Context, *flow.ItemState) error {
		calls = append(calls, "c")
		return nil
	})

	chain := flow.ChainPreflight(a, nil, b, c) // nil entries skipped
	if err := chain(ctx, state); err == nil || err.Error() != "b refused" {
		t.Errorf("err = %v, want 'b refused'", err)
	}
	if len(calls) != 2 || calls[0] != "a" || calls[1] != "b" {
		t.Errorf("calls = %v, want [a b] (must short-circuit on first error)", calls)
	}
}

func TestRunOne_ParksOnInvocationsExhaustion(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		// max 1 invocation, but handler returns error each time
		f.AddStep("flaky", "plan", func(ctx flow.StepCtx) error {
			return errors.New("boom")
		}, flow.StepConfig{Budget: flow.StepBudget{MaxInvocations: 1}})
	}, &stubAgent{name: "stub"})

	// First run consumes the only invocation and returns "failed".
	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "failed" {
		t.Errorf("first run status = %q, want failed", res.Status)
	}

	// Second run should park with budget-exhausted/invocations.
	res, err = RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" {
		t.Fatalf("second run status = %q, want parked. result=%+v", res.Status, res)
	}
	if res.Park == nil || res.Park.Axis != flow.AxisInvocations {
		t.Errorf("Park = %+v, want axis=invocations", res.Park)
	}
	if be.ParkRequest("1") == nil {
		t.Errorf("backend Park not recorded")
	}
}

func TestRunOne_RespectsPromptsBudget(t *testing.T) {
	a := &stubAgent{
		name: "stub",
		responses: []flow.AgentResponse{
			{LastText: "first", CostUSD: 0.1},
			{LastText: "second", CostUSD: 0.1}, // would-be-second prompt
		},
	}
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("loopy", "plan", func(ctx flow.StepCtx) error {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p1"}); err != nil {
				return err
			}

			_, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p2"})
			return err
			// Explicit cap: this test is about the gate firing, not about
			// whatever the package default happens to be.
		}, flow.StepConfig{Budget: flow.StepBudget{MaxPromptsPerInvocation: 1}})

	}, a)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" {
		t.Fatalf("status = %q, want parked. res=%+v", res.Status, res)
	}
	if res.Park == nil || res.Park.Axis != flow.AxisPrompts {
		t.Errorf("Park = %+v, want axis=prompts", res.Park)
	}
	if a.calls != 1 {
		t.Errorf("agent calls = %d, want 1 (second blocked by budget)", a.calls)
	}
}

// Each turn is handed the headroom LEFT in the grant, not the grant itself:
// passing the whole cap to a second prompt would let the step spend the grant
// twice over.
func TestRunOne_AgentRequestCarriesRemainingCostHeadroom(t *testing.T) {
	a := &stubAgent{
		name: "stub",
		responses: []flow.AgentResponse{
			{LastText: "first", CostUSD: 2},
			{LastText: "second"},
		},
	}
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("two prompts", "plan", func(ctx flow.StepCtx) error {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p1"}); err != nil {
				return err
			}
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p2"}); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{Budget: flow.StepBudget{MaxCostUSD: 5}})
	}, a)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("status = %q, want done. res=%+v", res.Status, res)
	}
	if len(a.reqs) != 2 {
		t.Fatalf("agent requests = %d, want 2", len(a.reqs))
	}
	if a.reqs[0].MaxCostUSD != 5 {
		t.Errorf("first MaxCostUSD = %v, want 5 (the whole grant)", a.reqs[0].MaxCostUSD)
	}
	if a.reqs[1].MaxCostUSD != 3 {
		t.Errorf("second MaxCostUSD = %v, want 3 (grant minus the $2 already spent)", a.reqs[1].MaxCostUSD)
	}
}

// A handler that set its own ceiling asked for a TIGHTER turn than the step's
// grant allows. The meter narrows to the headroom, it never widens: overwriting
// a $1 request with the $5 grant would spend four dollars the handler said it
// did not want spent.
func TestRunOne_HandlerCostCeilingIsNarrowedNotWidened(t *testing.T) {
	a := &stubAgent{
		name: "stub",
		responses: []flow.AgentResponse{
			{LastText: "first"},
			{LastText: "second"},
		},
	}
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("two prompts", "plan", func(ctx flow.StepCtx) error {
			// Tighter than the grant: must survive.
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p1", MaxCostUSD: 1}); err != nil {
				return err
			}
			// Looser than the grant: must be cut down to it.
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p2", MaxCostUSD: 9}); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{Budget: flow.StepBudget{MaxCostUSD: 5}})
	}, a)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("status = %q, want done. res=%+v", res.Status, res)
	}
	if len(a.reqs) != 2 {
		t.Fatalf("agent requests = %d, want 2", len(a.reqs))
	}
	if a.reqs[0].MaxCostUSD != 1 {
		t.Errorf("first MaxCostUSD = %v, want 1 (the handler's own tighter ceiling)", a.reqs[0].MaxCostUSD)
	}
	if a.reqs[1].MaxCostUSD != 5 {
		t.Errorf("second MaxCostUSD = %v, want 5 (the grant, which is tighter than the handler's 9)", a.reqs[1].MaxCostUSD)
	}
}

// The turn the substrate stopped at the cap IS the step reaching its cost cap:
// it parks on cost, and the spend it reports is the real one — including the
// response that crossed the cap.
func TestRunOne_CostCapFailureParksOnCost(t *testing.T) {
	a := &stubAgent{
		name: "stub",
		responses: []flow.AgentResponse{{
			LastText: "stopped",
			CostUSD:  21.868663,
			Failure:  &flow.AgentFailure{Kind: flow.FailureCostCap, Message: "budget"},
		}},
	}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("spendy", "plan", func(ctx flow.StepCtx) error {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p1"}); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("never reached")
		}, flow.StepConfig{Budget: flow.StepBudget{MaxCostUSD: 20}})
	}, a)

	ctx := context.Background()
	res, err := RunOne(ctx, app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil {
		t.Fatalf("res = %+v, want parked", res)
	}
	if res.Park.Kind != flow.ParkBudgetExhausted || res.Park.Axis != flow.AxisCost {
		t.Errorf("Park = %+v, want budget-exhausted on the cost axis", res.Park)
	}
	// The stopped turn still bills: a park that forgot the spend would let the
	// next dispatch re-run the same turn against a meter that never moved.
	st, err := be.LoadState(ctx, claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := st.Artifact("plan").CostUSDSpent; got != 21.868663 {
		t.Errorf("CostUSDSpent = %v, want 21.868663", got)
	}
	var cost flow.AxisReport
	for _, ax := range res.Park.Axes {
		if ax.Axis == flow.AxisCost {
			cost = ax
		}
	}
	if cost.Used != 21.868663 || cost.Granted != 20 || !cost.Exhausted {
		t.Errorf("cost axis report = %+v, want 21.868663/20 exhausted", cost)
	}
}

// zeroCostGrantBackend hands back state whose cost axis carries no grant —
// the shape a backend that does not meter cost produces.
type zeroCostGrantBackend struct{ *fake.Backend }

func (b zeroCostGrantBackend) LoadState(ctx context.Context, claim flow.Claim) (*flow.ItemState, error) {
	st, err := b.Backend.LoadState(ctx, claim)
	if err != nil {
		return nil, err
	}
	for id, rec := range st.Artifacts {
		rec.GrantedCostUSD = 0
		st.Artifacts[id] = rec
	}
	return st, nil
}

// With no cost grant the cap was never ours to claim: a cost-cap response is
// an ordinary agent failure, not a park on an axis this step does not meter.
func TestRunOne_CostCapWithoutAGrantIsAPlainFailure(t *testing.T) {
	a := &stubAgent{
		name: "stub",
		responses: []flow.AgentResponse{{
			LastText: "stopped",
			CostUSD:  21.868663,
			Failure:  &flow.AgentFailure{Kind: flow.FailureCostCap, Message: "budget"},
		}},
	}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("spendy", "plan", func(ctx flow.StepCtx) error {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p1"}); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("never reached")
		}, flow.StepConfig{})
	}, a)
	app.Backend = zeroCostGrantBackend{Backend: be}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("res = %+v, want failed (no cost grant to park against)", res)
	}
	if !strings.Contains(res.Reason, flow.FailureCostCap) {
		t.Errorf("Reason = %q, want it to name the %s failure", res.Reason, flow.FailureCostCap)
	}
	// Nothing was capped by us, so nothing was passed down either.
	if len(a.reqs) != 1 || a.reqs[0].MaxCostUSD != 0 {
		t.Errorf("reqs = %+v, want one request with MaxCostUSD 0", a.reqs)
	}
}

// Without a grant there is no headroom to narrow to, so the handler's own
// ceiling is the only one there is and must survive untouched. Narrowing
// unconditionally would compute a NEGATIVE headroom once the step has spent
// anything (0 - spent), and hand the turn a ceiling tighter than any real
// budget — or, at zero spend, a 0 that means unbounded.
func TestRunOne_NoCostGrantLeavesTheHandlerCeilingAlone(t *testing.T) {
	a := &stubAgent{
		name: "stub",
		responses: []flow.AgentResponse{
			{LastText: "first", CostUSD: 2},
			{LastText: "second"},
		},
	}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("two prompts", "plan", func(ctx flow.StepCtx) error {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p1", MaxCostUSD: 3}); err != nil {
				return err
			}
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p2", MaxCostUSD: 3}); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, a)
	app.Backend = zeroCostGrantBackend{Backend: be}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("status = %q, want done. res=%+v", res.Status, res)
	}
	if len(a.reqs) != 2 {
		t.Fatalf("agent requests = %d, want 2", len(a.reqs))
	}
	for i, req := range a.reqs {
		if req.MaxCostUSD != 3 {
			t.Errorf("req[%d] MaxCostUSD = %v, want 3 (the handler's ceiling, ungranted step)", i, req.MaxCostUSD)
		}
	}
}

// A grant spent to the last cent leaves zero headroom, and zero means
// UNBOUNDED to the substrate. So the pre-prompt gate has to fire first: the
// step parks without a second dispatch. A weakened gate would turn the fully
// spent grant into a turn with no ceiling at all — worse than the overrun the
// cap exists to stop.
func TestRunOne_SpentGrantParksInsteadOfDispatchingAnUncappedTurn(t *testing.T) {
	a := &stubAgent{
		name: "stub",
		responses: []flow.AgentResponse{
			{LastText: "first", CostUSD: 5}, // spends the grant exactly
			{LastText: "second"},
		},
	}
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("two prompts", "plan", func(ctx flow.StepCtx) error {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p1"}); err != nil {
				return err
			}
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p2"}); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("never reached")
		}, flow.StepConfig{Budget: flow.StepBudget{MaxCostUSD: 5}})
	}, a)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil {
		t.Fatalf("res = %+v, want parked", res)
	}
	if res.Park.Axis != flow.AxisCost {
		t.Errorf("Park = %+v, want axis=cost", res.Park)
	}
	if len(a.reqs) != 1 {
		t.Fatalf("agent requests = %d, want 1 (the second must never be dispatched)", len(a.reqs))
	}
	if a.reqs[0].MaxCostUSD != 5 {
		t.Errorf("first MaxCostUSD = %v, want 5", a.reqs[0].MaxCostUSD)
	}
}

func TestRunOne_ParksOnTimeout(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("slow", "plan", func(ctx flow.StepCtx) error {
			<-ctx.Context().Done()
			return ctx.Context().Err()
		}, flow.StepConfig{Budget: flow.StepBudget{Timeout: 50 * time.Millisecond}})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Axis != flow.AxisTimeout {
		t.Errorf("res = %+v, want parked/timeout", res)
	}
}

// countingPatchBackend hands out worktrees that count CapturePatch calls, so a
// test can assert the park path never captures.
type countingPatchBackend struct {
	*fake.Backend
	wt *countingPatchWorktree
}

func (b *countingPatchBackend) Worktree(ctx context.Context, claim flow.Claim) (flow.Worktree, error) {
	inner, err := b.Backend.Worktree(ctx, claim)
	if err != nil {
		return nil, err
	}
	b.wt.Worktree = inner
	return b.wt, nil
}

type countingPatchWorktree struct {
	flow.Worktree
	captures int
}

func (w *countingPatchWorktree) CapturePatch(ctx context.Context) ([]byte, error) {
	w.captures++
	return w.Worktree.CapturePatch(ctx)
}

// A timeout park must NOT capture a patch. The deadline kill carries no
// verify-green signal, and the common shape — a step that commits and then
// runs a long verify — has an empty `git diff HEAD`, so the old opportunistic
// capture uploaded a zero-byte patch. The park itself is unchanged: same kind,
// axis, and invocation accounting.
func TestRunOne_TimeoutParkDoesNotCapturePatch(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("slow", "plan", func(ctx flow.StepCtx) error {
			// Commit first, so the worktree is clean when the deadline fires
			// — exactly the merged-land shape from the bug report.
			wt, err := ctx.Worktree()
			if err != nil {
				return err
			}
			if err := wt.Commit(ctx.Context(), "work"); err != nil {
				return err
			}
			<-ctx.Context().Done()
			return ctx.Context().Err()
		}, flow.StepConfig{Budget: flow.StepBudget{Timeout: 50 * time.Millisecond}})
	}, &stubAgent{name: "stub"})

	counting := &countingPatchBackend{Backend: be, wt: &countingPatchWorktree{}}
	app.Backend = counting

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkBudgetExhausted || res.Park.Axis != flow.AxisTimeout {
		t.Errorf("res = %+v, want parked with kind=budget-exhausted axis=timeout", res)
	}
	if counting.wt.captures != 0 {
		t.Errorf("CapturePatch called %d times on a timeout park, want 0", counting.wt.captures)
	}
	state, _ := be.LoadState(context.Background(), claim)
	if rec := state.Artifact("plan"); rec.Invocations != 1 {
		t.Errorf("Invocations = %d, want 1 (timeout still counts as an invocation)", rec.Invocations)
	}
}

func TestRunOne_SignalStepAwaitsSignal(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddSignalStep("create pr", "pr-open", func(ctx flow.StepCtx) error {

			return nil
		}, flow.StepConfig{})

	}, &stubAgent{name: "stub"})

	// First run dispatches the handler (returns nil). The signal isn't
	// set, so the step stays pending — but RunOne returns done for this
	// invocation because the handler didn't error.
	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Errorf("first run = %+v, want done", res)
	}
	state, _ := be.LoadState(context.Background(), claim)
	if state.SignalSet("pr-open") {
		t.Fatalf("signal should not be set yet")
	}

	// Backend observes signal — flow should now be done.
	be.SetSignal("1", "pr-open", true)
	res, err = RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Errorf("after signal set = %+v, want done with no eligible flow", res)
	}
}

func TestRunOne_AwaitSignalSkipsHandlerless(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AwaitSignal("await merge", "pr-open", flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "skipped" {
		t.Errorf("res = %+v, want skipped", res)
	}

	be.SetSignal("1", "pr-open", true)
	res, _ = RunOne(context.Background(), app, claim)
	if res.Status != "done" {
		t.Errorf("after signal = %+v, want done", res)
	}
}

func TestRunOne_QuestionsPersistedAndPark(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("ask", "plan", func(ctx flow.StepCtx) error {
			return ctx.AskQuestions(
				flow.AskYesNo("ship", "Ship it?"),
				flow.AskChoice("lib", "Which?", "a", "b"),
			)
		}, flow.StepConfig{})

	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkQuestion {
		t.Errorf("res = %+v, want parked kind=question", res)
	}
	state, _ := be.LoadState(context.Background(), claim)
	if len(state.Questions) != 2 {
		t.Errorf("Questions persisted = %d, want 2", len(state.Questions))
	}
}

func TestRunOne_WrongResolveReturnsTypeMismatch(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		// Declared markdown, handler calls ResolveCommitHash.
		f.AddStep("wrong", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveCommitHash("deadbeef")
		}, flow.StepConfig{})

	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "failed" {
		t.Errorf("res = %+v, want failed", res)
	}
}

func TestRunOne_NilReturnWithoutResolveParks(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("forgetful", "plan", func(ctx flow.StepCtx) error {
			return nil
		}, flow.StepConfig{})

	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkStepDidNotResolve {
		t.Errorf("res = %+v, want parked step-did-not-resolve", res)
	}
}

func TestApp_Validate_RejectsUnknownArtifact(t *testing.T) {
	be := fake.New()
	f := flow.NewFlow("x", nil)
	f.AddStep("step", "missing-artifact", func(flow.StepCtx) error { return nil }, flow.StepConfig{})
	app := App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:     []*flow.Flow{f},
	}
	if err := app.validate(); err == nil {
		t.Errorf("expected validation error for unknown artifact")
	}
}

func TestApp_Validate_RejectsUnsupportedArtifact(t *testing.T) {
	be := fake.New()
	// Restrict the fake to only "plan" — "report" is then unrecordable.
	be.SetSupportedArtifacts(flow.Artifact("plan", flow.ArtifactMarkdown))
	f := flow.NewFlow("x", []flow.ItemType{"task"})
	f.AddStep("plan", "plan", func(flow.StepCtx) error { return nil }, flow.StepConfig{})
	f.AddStep("report", "report", func(flow.StepCtx) error { return nil }, flow.StepConfig{})
	app := App{
		Backend: be,
		Agent:   &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{
			flow.Artifact("plan", flow.ArtifactMarkdown),
			flow.Artifact("report", flow.ArtifactMarkdown),
		},
		Flows: []*flow.Flow{f},
	}
	if err := app.validate(); err == nil {
		t.Errorf("expected validation error for artifact the backend cannot record")
	}
}

func TestApp_Validate_RejectsArtifactTypeMismatch(t *testing.T) {
	be := fake.New()
	// The backend records "plan" only as Markdown; declaring it as JSON is a
	// type the backend cannot store for that id — caught at startup, not at
	// resolve-time.
	be.SetSupportedArtifacts(flow.Artifact("plan", flow.ArtifactMarkdown))
	f := flow.NewFlow("x", []flow.ItemType{"task"})
	f.AddStep("plan", "plan", func(flow.StepCtx) error { return nil }, flow.StepConfig{})
	app := App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactJSON)},
		Flows:     []*flow.Flow{f},
	}
	if err := app.validate(); err == nil {
		t.Errorf("expected validation error for artifact declared with a type the backend cannot record")
	}
}

func TestApp_Validate_RejectsUnsupportedSignal(t *testing.T) {
	be := fake.New() // no signals supported
	f := flow.NewFlow("x", nil)
	f.AddSignalStep("sig", "pr-open", func(flow.StepCtx) error { return nil }, flow.StepConfig{})
	app := App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Signals:   []flow.SignalDef{flow.Signal("pr-open", "x")},
		Flows:     []*flow.Flow{f},
	}
	if err := app.validate(); err == nil {
		t.Errorf("expected validation error for signal not in SupportedSignals")
	}
}

func TestSelectFlow_RequireSignalGate(t *testing.T) {
	be := fake.New(flow.Signal("pr-open", "x"))
	a := flow.NewFlow("contributor", []flow.ItemType{"task"})
	a.AddStep("plan", "plan", func(flow.StepCtx) error { return nil }, flow.StepConfig{})
	b := flow.NewFlow("maintainer", []flow.ItemType{"task"})
	b.RequireSignal("pr-open")
	b.AddStep("merge", "commit", func(flow.StepCtx) error { return nil }, flow.StepConfig{})

	app := &App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown), flow.Artifact("commit", flow.ArtifactCommitHash)},
		Signals:   []flow.SignalDef{flow.Signal("pr-open", "x")},
		Flows:     []*flow.Flow{a, b},
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	state := &flow.ItemState{
		Item:      flow.Item{ID: "1", Type: "task"},
		Artifacts: map[flow.ArtifactId]flow.ArtifactRecord{},
		Signals:   map[flow.SignalId]flow.SignalState{},
	}
	picked, _ := SelectFlow(app, state)
	if picked == nil || picked.Name() != "contributor" {
		t.Errorf("expected contributor before pr-open; got %v", picked)
	}

	// Resolve contributor's only step.
	state.Artifacts["plan"] = flow.ArtifactRecord{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Resolved: true}
	// Maintainer still gated on pr-open.
	picked, _ = SelectFlow(app, state)
	if picked != nil {
		t.Errorf("expected no eligible flow without pr-open; got %v", picked.Name())
	}

	state.Signals["pr-open"] = flow.SignalState{Set: true}
	picked, _ = SelectFlow(app, state)
	if picked == nil || picked.Name() != "maintainer" {
		t.Errorf("expected maintainer once pr-open set; got %v", picked)
	}
}

// pendingArtifactBackend wraps fake.Backend so LoadState reports a required-
// but-unresolved artifact on the loaded state — modelling a status=done item
// whose finalization (summary / inspection) hasn't completed yet. T0481.
type pendingArtifactBackend struct {
	*fake.Backend
	pending flow.ArtifactId
}

func (b *pendingArtifactBackend) LoadState(ctx context.Context, claim flow.Claim) (*flow.ItemState, error) {
	state, err := b.Backend.LoadState(ctx, claim)
	if err != nil {
		return state, err
	}
	if b.pending != "" {
		state.Artifacts[b.pending] = flow.ArtifactRecord{
			Id:       b.pending,
			Type:     flow.ArtifactMarkdown,
			Required: true,
			Resolved: false,
		}
	}
	return state, nil
}

// finalizingBackend wraps fake.Backend and counts Finalize calls so tests can
// distinguish the premature-finalize regression from the happy path.
type finalizingBackend struct {
	*fake.Backend
	finalizeCalls int
}

func (b *finalizingBackend) Finalize(ctx context.Context, claim flow.Claim) error {
	b.finalizeCalls++
	return nil
}

// TestRunOne_RefusesFinalizeWhenRequiredArtifactPending (T0481): when
// SelectFlow finds no eligible step but the loaded state still has a
// required-but-unresolved artifact, RunOne must refuse to Finalize+release —
// returning a "failed" InvocationResult that names the pending artifact —
// rather than silently dropping the operator's lease before the operator can
// hand-run the remaining steps (the T0474 stall).
func TestRunOne_RefusesFinalizeWhenRequiredArtifactPending(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		// RequireSignal "pr-open" never set, so IsReady → false →
		// SelectFlow returns nil. The flow's compiled-in step is unreachable.
		f.RequireSignal("pr-open")
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			t.Fatal("step handler ran despite gated flow — must not happen")
			return nil
		}, flow.StepConfig{})

	}, a)
	wrapped := &finalizingBackend{Backend: be}
	app.Backend = &pendingArtifactBackend{Backend: be, pending: "summary"}
	// Compose: the outer pendingArtifactBackend's LoadState is what RunOne
	// sees; the finalizing wrapper is only used to PROVE Finalize is NOT
	// called. Swap in the finalizing one as the concrete Finalizer the type
	// assertion picks up.
	_ = wrapped

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed (premature-finalize guard). res=%+v", res.Status, res)
	}
	if !strings.Contains(res.Reason, "summary") {
		t.Errorf("reason = %q, want it to name the pending artifact %q", res.Reason, "summary")
	}
	if !strings.Contains(res.Reason, "refusing premature finalize") {
		t.Errorf("reason = %q, want a 'refusing premature finalize' phrase", res.Reason)
	}
}

// unmatchedTypeApp builds an app whose single flow accepts {task,bug} over an
// item typed "chore" — the shape of an ordinary GitHub issue carrying no
// type:* label against a binary that registers task/bug flows. The step handler
// fails the test: nothing may run for an item no flow accepts.
func unmatchedTypeApp(t *testing.T, item flow.Item) (*App, *fake.Backend, flow.Claim) {
	t.Helper()
	return testAppItem(t, item, []flow.ItemType{"task", "bug"}, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			t.Fatal("step handler ran for an item no flow accepts — must not happen")
			return nil
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
}

// TestRunOne_BlocksWhenNoFlowAcceptsItemType (#10): an item whose type matches
// no registered flow was never seeded and never ran a step, so it must NOT be
// reported done and finalized — that reports success for work never attempted,
// terminally. It is blocked, with a reason naming the item's type, the
// registered types, and both ways a person can clear it.
func TestRunOne_BlocksWhenNoFlowAcceptsItemType(t *testing.T) {
	app, be, claim := unmatchedTypeApp(t, flow.Item{ID: "1", Type: "chore", Title: "test#1"})
	wrapped := &finalizingBackend{Backend: be}
	app.Backend = wrapped

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "blocked" {
		t.Fatalf("status = %q, want blocked. res=%+v", res.Status, res)
	}
	for _, want := range []string{`item type "chore"`, "registered: bug, task", "correct the item's type"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("reason = %q, want it to contain %q", res.Reason, want)
		}
	}
	if wrapped.finalizeCalls != 0 {
		t.Errorf("finalizeCalls = %d, want 0 — an unmatched item must not be finalized", wrapped.finalizeCalls)
	}
	// And nothing was seeded: the blind spot in the pending-artifact guard is
	// exactly that an unmatched item has no records for it to iterate.
	state, err := be.LoadState(context.Background(), claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Artifacts) != 0 {
		t.Errorf("artifacts = %+v, want none seeded", state.Artifacts)
	}
}

// TestRunOne_BlocksUnmatchedTypeEvenWhenSeeded (#10): the type mismatch is the
// root cause, so it is reported ahead of the pending-artifact guard — an item
// that WAS seeded (by an earlier run, or a since-changed type) and now matches
// nothing reports the mismatch, not "required artifact still pending".
func TestRunOne_BlocksUnmatchedTypeEvenWhenSeeded(t *testing.T) {
	app, be, claim := unmatchedTypeApp(t, flow.Item{ID: "1", Type: "chore", Title: "test#1"})
	app.Backend = &pendingArtifactBackend{Backend: be, pending: "summary"}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "blocked" {
		t.Fatalf("status = %q, want blocked (type mismatch beats the artifact guard). res=%+v", res.Status, res)
	}
	if !strings.Contains(res.Reason, `item type "chore"`) {
		t.Errorf("reason = %q, want it to name the unmatched type", res.Reason)
	}
}

// TestRunOne_FinalizedItemWithUnmatchedTypeStaysDone (#10): the block is for
// items with work still owed. An already-finalized item's run is over —
// including one finalized by this very defect before it was fixed — and
// blocking it would strand it with no route onward.
func TestRunOne_FinalizedItemWithUnmatchedTypeStaysDone(t *testing.T) {
	app, be, claim := unmatchedTypeApp(t, flow.Item{ID: "1", Type: "chore", Title: "test#1", Finalized: true})
	wrapped := &finalizingBackend{Backend: be}
	app.Backend = wrapped

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Errorf("status = %q, want done for an already-finalized item. res=%+v", res.Status, res)
	}
	if wrapped.finalizeCalls != 1 {
		t.Errorf("finalizeCalls = %d, want 1 (the existing terminal path still runs)", wrapped.finalizeCalls)
	}
}

// TestRunOne_BlocksWhenItemTypeIsEmpty (#10): the reported case is not a typo'd
// type but the absence of one — an ordinary GitHub issue carries no `type:*`
// label, so the backend types it "". The empty type is a mismatch like any
// other, not a wildcard and not a licence to finalize, and the reason renders it
// so the operator can see that the item carries no type at all.
func TestRunOne_BlocksWhenItemTypeIsEmpty(t *testing.T) {
	app, be, claim := unmatchedTypeApp(t, flow.Item{ID: "1", Type: "", Title: "test#1"})
	wrapped := &finalizingBackend{Backend: be}
	app.Backend = wrapped

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "blocked" {
		t.Fatalf("status = %q, want blocked. res=%+v", res.Status, res)
	}
	if !strings.Contains(res.Reason, `item type ""`) {
		t.Errorf("reason = %q, want it to render the empty type", res.Reason)
	}
	if wrapped.finalizeCalls != 0 {
		t.Errorf("finalizeCalls = %d, want 0 — an untyped item must not be finalized", wrapped.finalizeCalls)
	}
}

// TestRunOne_UniversalFlowIsNotATypeMismatch (#10): the block keys off
// AcceptsType, not off the declared type list, and a flow declaring no types
// accepts everything. Such an app has an empty registered-types list, so a check
// written against that list instead would block every item it owns — turning the
// guard on the very configuration it is meant to leave alone.
func TestRunOne_UniversalFlowIsNotATypeMismatch(t *testing.T) {
	app, be, claim := testAppItem(t,
		flow.Item{ID: "1", Type: "chore", Title: "test#1"},
		nil, // universal flow: declares no types, accepts all of them
		func(f *flow.Flow) {
			// RequireSignal never set, so SelectFlow returns nil and RunOne
			// reaches the pre-dispatch region with the flow still accepting
			// the item's type.
			f.RequireSignal("pr-open")
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
				return ctx.ResolveMarkdown("ignored")
			}, flow.StepConfig{})
		}, &stubAgent{name: "stub"})
	wrapped := &finalizingBackend{Backend: be}
	app.Backend = wrapped

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("status = %q, want done — a universal flow accepts %q. res=%+v", res.Status, "chore", res)
	}
	if wrapped.finalizeCalls != 1 {
		t.Errorf("finalizeCalls = %d, want 1 (the finalize path is unchanged for an accepted type)", wrapped.finalizeCalls)
	}
}

func TestRegisteredTypes(t *testing.T) {
	cases := []struct {
		name  string
		flows []*flow.Flow
		want  string
	}{
		{"no flows", nil, "none"},
		{
			"universal flow declares no types",
			[]*flow.Flow{flow.NewFlow("any", nil)},
			"none",
		},
		{
			"sorted and deduplicated across flows",
			[]*flow.Flow{
				flow.NewFlow("a", []flow.ItemType{"task", "bug"}),
				flow.NewFlow("b", []flow.ItemType{"bug", "chore"}),
			},
			"bug, chore, task",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := registeredTypes(&App{Flows: tc.flows}); got != tc.want {
				t.Errorf("registeredTypes = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunOne_FinalizesWhenAllRequiredArtifactsResolved (T0481 regression
// guard for the happy path): when SelectFlow returns nil AND the loaded
// state has no required-but-unresolved artifact, RunOne MUST take the
// existing Finalize+release path. Pairs with the refusal test above so a
// future refactor can't accidentally swallow the happy-path branch.
func TestRunOne_FinalizesWhenAllRequiredArtifactsResolved(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		// Same shape as the refusal test: RequireSignal never set, so
		// SelectFlow returns nil unconditionally.
		f.RequireSignal("pr-open")
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("ignored")
		}, flow.StepConfig{})

	}, a)
	// Backend with Finalize implemented, no pending artifacts injected.
	wrapped := &finalizingBackend{Backend: be}
	app.Backend = wrapped

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Errorf("status = %q, want done (Finalize path). res=%+v", res.Status, res)
	}
	if wrapped.finalizeCalls != 1 {
		t.Errorf("finalizeCalls = %d, want 1 (Finalize must run when nothing is pending)", wrapped.finalizeCalls)
	}
}

// A granted timeout on the artifact record must win over the step's
// compiled-in budget — otherwise `grant --timeout` is write-only and a
// timeout-parked step re-parks forever at the same deadline.
func TestRunOne_GrantedTimeoutOverridesStepBudget(t *testing.T) {
	var deadlines []time.Duration
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("slow", "plan", func(ctx flow.StepCtx) error {
			dl, ok := ctx.Context().Deadline()
			if !ok {
				t.Error("handler context has no deadline")
				return nil
			}
			deadlines = append(deadlines, time.Until(dl))
			// Outruns the 50ms step budget, fits inside the granted second.
			select {
			case <-ctx.Context().Done():
				return ctx.Context().Err()
			case <-time.After(150 * time.Millisecond):
			}
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{Budget: flow.StepBudget{Timeout: 50 * time.Millisecond}})
	}, &stubAgent{name: "stub"})

	ctx := context.Background()
	res, err := RunOne(ctx, app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Axis != flow.AxisTimeout {
		t.Fatalf("first run = %+v, want parked/timeout", res)
	}

	// Grant a second of extra wall-clock, as `do grant --timeout 1` does.
	if err := be.Grant(ctx, claim, "plan", flow.Grant{TimeoutAdd: 1}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	res, err = RunOne(ctx, app, claim)
	if err != nil {
		t.Fatalf("RunOne after grant: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("second run = %+v, want done (granted timeout ignored?)", res)
	}
	if len(deadlines) != 2 {
		t.Fatalf("handler ran %d times, want 2", len(deadlines))
	}
	if deadlines[1] <= deadlines[0] {
		t.Errorf("deadlines = %v, want the second run to get more time than the first", deadlines)
	}
}

// End-to-end on the ping-pong loop: a real timeout park, a real bare `grant`,
// and a rerun that reaches the handler. The timeout kill bumps invocations on
// its way out, so this only passes if grant tops up BOTH axes and the
// orchestrator reads the granted timeout back off the record.
func TestRunOne_BareGrantRecoversAStepOutOfTimeAndInvocations(t *testing.T) {
	var runs int
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("slow", "plan", func(ctx flow.StepCtx) error {
			runs++
			select {
			case <-ctx.Context().Done():
				return ctx.Context().Err()
			case <-time.After(150 * time.Millisecond):
			}
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{Budget: flow.StepBudget{
			MaxInvocations: 1,
			Timeout:        50 * time.Millisecond,
		}})
	}, &stubAgent{name: "stub"})

	ctx := context.Background()
	res, err := RunOne(ctx, app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Axis != flow.AxisTimeout {
		t.Fatalf("first run = %+v, want parked/timeout", res)
	}
	// The kill consumed the step's only invocation, so both axes are flat.
	st, err := be.LoadState(ctx, claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if rec := st.Artifact("plan"); rec.Invocations != rec.GrantedInvocations {
		t.Fatalf("invocations = %d/%d, want the axis exhausted by the timeout kill",
			rec.Invocations, rec.GrantedInvocations)
	}

	if code := app.cmdGrant(ctx, nil); code != 0 {
		t.Fatalf("grant exit = %d, want 0", code)
	}

	res, err = RunOne(ctx, app, claim)
	if err != nil {
		t.Fatalf("RunOne after grant: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("second run = %+v, want done", res)
	}
	if runs != 2 {
		t.Errorf("handler ran %d times, want 2 (a grant that re-parks pre-dispatch never reaches it)", runs)
	}
}

// outOfBandPatchBackend models a backend whose patches live server-side: its
// Worktree.CapturePatch returns no bytes (the runner attaches the diff to the
// item itself), and ResolveArtifact validates that the evidence is really
// there instead of writing body content. `evidence` says whether the
// out-of-band attachment happened.
type outOfBandPatchBackend struct {
	*fake.Backend
	evidence bool
}

func (b *outOfBandPatchBackend) ResolveArtifact(ctx context.Context, claim flow.Claim, id flow.ArtifactId, body flow.ArtifactBody) error {
	if body.Type == flow.ArtifactPatch && !b.evidence {
		return fmt.Errorf("backend: ResolveArtifact %q: no implementation evidence on item", id)
	}
	return b.Backend.ResolveArtifact(ctx, claim, id, body)
}

func (b *outOfBandPatchBackend) Worktree(ctx context.Context, claim flow.Claim) (flow.Worktree, error) {
	inner, err := b.Backend.Worktree(ctx, claim)
	if err != nil {
		return nil, err
	}
	return &nilCapturingWorktree{Worktree: inner}, nil
}

// nilCapturingWorktree returns no patch bytes by design — the diff is attached
// out-of-band, so there is nothing client-side to hand back.
type nilCapturingWorktree struct{ flow.Worktree }

func (w *nilCapturingWorktree) CapturePatch(ctx context.Context) ([]byte, error) { return nil, nil }

// An EMPTY PatchBody is legal. A backend that attaches the diff out-of-band
// has nothing to put in the body: the handler resolves with a zero body to say
// "I'm done — verify the side effect", and the backend confirms it. cli must
// pass that through rather than rejecting it one step before the check that
// actually knows where the evidence lives.
func TestResolvePatch_EmptyBodyResolvesForOutOfBandBackend(t *testing.T) {
	var resolveErr error
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("attach", "implementation", func(ctx flow.StepCtx) error {
			// Same shape as an out-of-band handler: capture yields no bytes,
			// resolve with an empty body anyway.
			wt, err := ctx.Worktree()
			if err != nil {
				return err
			}
			patch, err := wt.CapturePatch(ctx.Context())
			if err != nil {
				return err
			}
			if len(patch) != 0 {
				t.Errorf("CapturePatch returned %d bytes, want 0 for an out-of-band backend", len(patch))
			}
			resolveErr = ctx.ResolvePatch(flow.PatchBody{})
			return resolveErr
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.Backend = &outOfBandPatchBackend{Backend: be, evidence: true}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if resolveErr != nil {
		t.Fatalf("ResolvePatch(empty body) = %v, want nil (out-of-band attachment is legal)", resolveErr)
	}
	if res.Status != "done" {
		t.Fatalf("res = %+v, want done", res)
	}
	state, _ := be.LoadState(context.Background(), claim)
	if rec := state.Artifact("implementation"); !rec.Resolved {
		t.Errorf("implementation artifact = %+v, want resolved", rec)
	}
}

// The backend — not cli — decides an empty body is wrong, and its message is
// the one the handler sees.
func TestResolvePatch_BackendRejectsMissingEvidence(t *testing.T) {
	var resolveErr error
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("attach", "implementation", func(ctx flow.StepCtx) error {
			resolveErr = ctx.ResolvePatch(flow.PatchBody{})
			return resolveErr
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.Backend = &outOfBandPatchBackend{Backend: be, evidence: false}

	if _, err := RunOne(context.Background(), app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if resolveErr == nil || !strings.Contains(resolveErr.Error(), "no implementation evidence") {
		t.Fatalf("ResolvePatch error = %v, want the backend's own evidence message", resolveErr)
	}
	state, _ := be.LoadState(context.Background(), claim)
	if rec := state.Artifact("implementation"); rec.Resolved {
		t.Errorf("implementation artifact = %+v, want unresolved", rec)
	}
}

// A non-empty diff still resolves normally.
func TestResolvePatch_AcceptsNonEmptyDiff(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("attach", "implementation", func(ctx flow.StepCtx) error {
			return ctx.ResolvePatch(flow.PatchBody{Diff: []byte("diff --git a/x b/x\n"), BaseBranch: "main"})
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("res = %+v, want done", res)
	}
	state, _ := be.LoadState(context.Background(), claim)
	if rec := state.Artifact("implementation"); !rec.Resolved || len(rec.Patch.Diff) == 0 {
		t.Errorf("implementation artifact = %+v, want resolved with a non-empty diff", rec)
	}
}

// A preflight that wraps flow.ErrBlocked reports "blocked", not "skipped".
// The distinction is the whole point: a skip claims the next cycle might pass,
// and exits 0, so a caller waiting on the flow reads "nothing to do" and
// re-runs forever against a gate only a human can clear.
func TestRunOne_PreflightErrBlockedReportsBlocked(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("never runs", "plan", func(ctx flow.StepCtx) error {
			t.Fatal("handler must not run when preflight refuses")
			return nil
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.Preflight = func(context.Context, *flow.ItemState) error {
		return fmt.Errorf("answer needed on %q: %w", "plan", flow.ErrBlocked)
	}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "blocked" {
		t.Errorf("status = %q, want blocked", res.Status)
	}
	if !strings.Contains(res.Reason, "answer needed") {
		t.Errorf("reason = %q, want it to carry the preflight's message", res.Reason)
	}
}

// A plain preflight error keeps the existing "skipped" behavior — the new
// verdict must not reclassify every gate.
func TestRunOne_PlainPreflightErrorStillSkips(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("never runs", "plan", func(ctx flow.StepCtx) error { return nil }, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.Preflight = func(context.Context, *flow.ItemState) error {
		return errors.New("operator set the manual flag")
	}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "skipped" {
		t.Errorf("status = %q, want skipped", res.Status)
	}
}

// A park reason is a one-line field. The question goes in Header and its
// supporting evidence in Text, so a reason built from Text would splice a
// multi-line block into every status line and blocked message that shows it.
func TestQuestionReason_PrefersHeaderOverEvidence(t *testing.T) {
	q := flow.AgentQuestion{
		Header: "amend the doc, adjust the item, or reject?",
		Text:   "§3 states:\n\"No macros.\"\nThis item asks for a macro system.",
	}
	got := questionReason([]flow.AgentQuestion{q})
	if !strings.Contains(got, "amend the doc") {
		t.Errorf("reason = %q, want the question", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("reason = %q, want a single line", got)
	}
}

func TestQuestionReason_FallsBackToTextWithoutHeader(t *testing.T) {
	got := questionReason([]flow.AgentQuestion{{Text: "which database?"}})
	if !strings.Contains(got, "which database?") {
		t.Errorf("reason = %q, want the text when there is no header", got)
	}
}

// A question park raised through ctx.Park must carry the ask time, exactly as
// the ctx.AskQuestions route does. Without it a reader scanning for answers has
// no boundary and takes every comment already on the item — including ones
// written long before the question — for a reply.
func TestRunOne_HandlerQuestionParkCarriesAskTime(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("asks", "plan", func(ctx flow.StepCtx) error {
			return ctx.Park(flow.ParkRequest{
				Kind:   flow.ParkQuestion,
				Reason: "which database?",
			})
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkQuestion {
		t.Fatalf("res = %+v, want a question park", res)
	}
	if flow.QuestionAskedAt(res.Park).IsZero() {
		t.Errorf("Details = %q, want an asked-at marker", res.Park.Details)
	}
	if flow.QuestionAskedAt(be.ParkRequest("1")).IsZero() {
		t.Error("the marker did not reach the backend")
	}
}

// A non-question park must not be stamped: the marker means "a question was
// asked at this time", and putting it on a budget park would be a lie.
func TestRunOne_NonQuestionParkIsNotStamped(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("blocks", "plan", func(ctx flow.StepCtx) error {
			return ctx.Park(flow.ParkRequest{Kind: flow.ParkBlocked, Reason: "waiting on infra"})
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if !flow.QuestionAskedAt(res.Park).IsZero() {
		t.Errorf("Details = %q, want no ask marker on a non-question park", res.Park.Details)
	}
}

// A handler returning ErrRefused parks with ParkRefused and does NOT consume
// an invocation — symmetric with ErrTransient. The park reason carries the
// refusal's own message so the operator sees what was refused.
func TestRunOne_ErrRefusedParksWithoutBurningBudget(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("guarded", "plan", func(ctx flow.StepCtx) error {
			return fmt.Errorf("guard refused staged file main.go: %w", flow.ErrRefused)
		}, flow.StepConfig{Budget: flow.StepBudget{MaxInvocations: 1}})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" {
		t.Fatalf("status = %q, want parked", res.Status)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkRefused {
		t.Fatalf("Park = %+v, want kind=refused", res.Park)
	}
	if !strings.Contains(res.Park.Reason, "guard refused staged file") {
		t.Errorf("Park.Reason = %q, want the refusal's own message", res.Park.Reason)
	}

	// The invocation must NOT have been counted.
	state, _ := be.LoadState(context.Background(), claim)
	if rec := state.Artifact("plan"); rec.Invocations != 0 {
		t.Errorf("Invocations = %d, want 0 (ErrRefused must not burn budget)", rec.Invocations)
	}

	// A second dispatch must NOT pre-gate on budget — the budget is untouched.
	res2, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("second RunOne: %v", err)
	}
	if res2.Status != "parked" || res2.Park == nil || res2.Park.Kind != flow.ParkRefused {
		t.Fatalf("second run = %+v, want parked/refused again (not budget-exhausted)", res2)
	}
}

// Regression guard: a plain (non-sentinel) error still bumps invocations.
// This test exists so a future refactor of the ErrRefused branch cannot
// accidentally skip the bump for all errors.
func TestRunOne_PlainErrorStillBumpsInvocations(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("broken", "plan", func(ctx flow.StepCtx) error {
			return errors.New("something went wrong")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	state, _ := be.LoadState(context.Background(), claim)
	if rec := state.Artifact("plan"); rec.Invocations != 1 {
		t.Errorf("Invocations = %d, want 1 (plain error must consume budget)", rec.Invocations)
	}
}

// ErrTransient still works as before — parks with ParkInfraTransient and
// does not bump. Regression guard for the ErrRefused addition.
func TestRunOne_ErrTransientStillParksInfraTransient(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("flaky", "plan", func(ctx flow.StepCtx) error {
			return fmt.Errorf("runner offline: %w", flow.ErrTransient)
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkInfraTransient {
		t.Fatalf("res = %+v, want parked/infra-transient", res)
	}
	state, _ := be.LoadState(context.Background(), claim)
	if rec := state.Artifact("plan"); rec.Invocations != 0 {
		t.Errorf("Invocations = %d, want 0 (ErrTransient must not burn budget)", rec.Invocations)
	}
}

// clearMarkerBackend wraps a fake.Backend and records ClearQuestionMarker
// calls so tests can observe the gate-path label clearing.
type clearMarkerBackend struct {
	*fake.Backend
	cleared []flow.ItemRef
	mu      sync.Mutex
}

func (b *clearMarkerBackend) ClearQuestionMarker(ctx context.Context, ref flow.ItemRef) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cleared = append(b.cleared, ref)
}

func (b *clearMarkerBackend) wasCleared() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.cleared) > 0
}

// TestRunOne_GatePathClearsQuestionMarker verifies that when RunOne dispatches
// an item parked on ParkQuestion and the preflight passes (meaning an answer
// exists), ClearQuestionMarker is called before handler dispatch.
func TestRunOne_GatePathClearsQuestionMarker(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	// Park on a question.
	if err := be.Park(context.Background(), claim, flow.ParkRequest{
		Kind: flow.ParkQuestion,
		Step: "plan",
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}

	// Wrap the backend to track ClearQuestionMarker.
	wrapped := &clearMarkerBackend{Backend: be}
	app.Backend = wrapped

	// The preflight passes (no gate installed), simulating a state where an
	// answer was posted out-of-band. The park record still shows ParkQuestion
	// because answering does not clear the park record — only step resolution
	// does.
	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	// The step should have dispatched and resolved.
	if res.Status != string(flow.StatusDone) {
		t.Fatalf("status = %q, want done", res.Status)
	}
	if !wrapped.wasCleared() {
		t.Error("ClearQuestionMarker was not called; gate-path label clearing did not fire")
	}
}

// TestRunOne_BudgetParkDoesNotClearQuestionMarker verifies that the gate-path
// label clearing only fires for ParkQuestion, not for other park kinds like
// ParkBudgetExhausted. Regression guard for the condition in orchestrator.go.
func TestRunOne_BudgetParkDoesNotClearQuestionMarker(t *testing.T) {
	invocations := 0
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			invocations++
			if invocations == 1 {
				// First dispatch: exhaust budget so the item parks.
				return flow.ErrBudgetExhausted{Axis: flow.AxisInvocations}
			}
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{Budget: flow.DefaultStepBudget()})
	}, &stubAgent{name: "stub"})

	// First dispatch seeds the artifact and parks on budget.
	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne (park): %v", err)
	}
	if res.Status != "parked" {
		t.Fatalf("first dispatch status = %q, want parked", res.Status)
	}

	// Grant budget so the step can proceed on the next dispatch.
	if err := be.Grant(context.Background(), claim, "plan", flow.Grant{
		Invocations: 5,
	}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	wrapped := &clearMarkerBackend{Backend: be}
	app.Backend = wrapped

	res, err = RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne (resume): %v", err)
	}
	if res.Status != string(flow.StatusDone) {
		t.Fatalf("status = %q, want done", res.Status)
	}
	if wrapped.wasCleared() {
		t.Error("ClearQuestionMarker was called for a budget park; should only fire for ParkQuestion")
	}
}

// TestRunOne_NotifyDefaultUsesResultID verifies that ctx.Notify("", detail)
// emits a StepProgress keyed by the result id, not the human label.
func TestRunOne_NotifyDefaultUsesResultID(t *testing.T) {
	tel := &recordingTelemetry{}
	app, _, claim := testApp(t, func(f *flow.Flow) {
		// Label "write plan" differs from artifact id "plan".
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			ctx.Notify("", "drafting") // empty step → default
			return ctx.ResolveMarkdown("done")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.Telemetry = tel

	if _, err := RunOne(context.Background(), app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	// events: [0] auto-emit, [1] handler Notify("","drafting")
	var found bool
	for _, ev := range tel.events {
		if ev.Detail == "drafting" {
			if ev.Step != "plan" {
				t.Errorf("Notify default step = %q, want result id %q", ev.Step, "plan")
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no event with detail 'drafting'; events = %+v", tel.events)
	}
}

// TestRunOne_TimeoutParkReasonUsesResultID verifies that a timeout park's
// Reason text names the step by its result id, not the human label.
func TestRunOne_TimeoutParkReasonUsesResultID(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		// Label "write plan" differs from artifact id "plan".
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			<-ctx.Context().Done()
			return ctx.Context().Err()
		}, flow.StepConfig{Budget: flow.StepBudget{Timeout: 50 * time.Millisecond}})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Park == nil {
		t.Fatal("expected a park result for timeout")
	}
	// The reason must contain the result id "plan", not the label "write plan".
	if !strings.Contains(res.Park.Reason, `"plan"`) {
		t.Errorf("Park.Reason = %q, want it to name the result id %q", res.Park.Reason, "plan")
	}
	if strings.Contains(res.Park.Reason, "write plan") {
		t.Errorf("Park.Reason = %q, must not contain the label %q", res.Park.Reason, "write plan")
	}
}

// ErrUnfit sentinel: handler returns a wrapped ErrUnfit. The orchestrator must
// report blocked (not parked, not failed), write no park, and not consume any
// invocation budget.
func TestRunOne_ErrUnfitBlocksWithoutParkOrBudget(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("broken", "plan", func(ctx flow.StepCtx) error {
			return fmt.Errorf("12 MB free, floor 2 GB: %w", flow.ErrUnfit)
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", res.Status)
	}
	if res.Park != nil {
		t.Fatalf("Park = %+v, want nil (ErrUnfit must not park)", res.Park)
	}
	if !strings.Contains(res.Reason, "12 MB free") {
		t.Errorf("Reason = %q, want it to contain the handler's message", res.Reason)
	}
	state, _ := be.LoadState(context.Background(), claim)
	if rec := state.Artifact("plan"); rec.Invocations != 0 {
		t.Errorf("Invocations = %d, want 0 (ErrUnfit must not burn budget)", rec.Invocations)
	}
}

// Post-handler catch-all: a plain error on an unfit machine (gate verdict
// unacceptable) is reported as blocked — not failed — and budget is not
// consumed. The handler acquires a worktree so the catch-all fires.
func TestRunOne_PlainErrorOnUnfitMachineReportsBlocked(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("broken", "plan", func(ctx flow.StepCtx) error {
			ctx.Worktree() // acquire worktree so post-handler fitness check runs
			return errors.New("no space left on device")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	be.SetGateVerdict(false)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", res.Status)
	}
	if res.Park != nil {
		t.Fatalf("Park = %+v, want nil (unfit catch-all must not park)", res.Park)
	}
	state, _ := be.LoadState(context.Background(), claim)
	if rec := state.Artifact("plan"); rec.Invocations != 0 {
		t.Errorf("Invocations = %d, want 0 (unfit catch-all must not burn budget)", rec.Invocations)
	}
}

// Counterpart to the unfit catch-all: a plain error on a fit machine follows
// the normal failure path — status failed, budget consumed.
func TestRunOne_PlainErrorOnFitMachineStillFails(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("broken", "plan", func(ctx flow.StepCtx) error {
			ctx.Worktree() // acquire worktree so post-handler fitness check runs
			return errors.New("something went wrong")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	state, _ := be.LoadState(context.Background(), claim)
	if rec := state.Artifact("plan"); rec.Invocations != 1 {
		t.Errorf("Invocations = %d, want 1 (plain error on fit machine must consume budget)", rec.Invocations)
	}
}

// Post-handler catch-all requires a worktree: a plain error on an unfit
// machine WITHOUT a worktree follows the normal failure path (failed, budget
// consumed) because there is nowhere to run the fit gate.
func TestRunOne_PlainErrorNoWorktreeOnUnfitMachineStillFails(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("broken", "plan", func(ctx flow.StepCtx) error {
			// Do NOT call ctx.Worktree() — the catch-all checks sctx.worktree != nil.
			return errors.New("no space left on device")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	be.SetGateVerdict(false)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed (no worktree → catch-all does not fire)", res.Status)
	}
	state, _ := be.LoadState(context.Background(), claim)
	if rec := state.Artifact("plan"); rec.Invocations != 1 {
		t.Errorf("Invocations = %d, want 1 (no catch-all → budget consumed)", rec.Invocations)
	}
}
