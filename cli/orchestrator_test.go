package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// itemRefFor is the ref the fake mints for an id — the address every
// Orchestrator method now takes, in place of the store id Item used to carry.
func itemRefFor(id string) flow.ItemRef {
	return fake.New().Ref(id)
}

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
	wt, _ := be.Worktree(context.Background(), claim.ItemRef)
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
	state, _ := be.Load(context.Background(), claim.ItemRef)
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

func TestWriteContract_MayBranch_HeadChanges(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("close-branch", "plan", func(ctx flow.StepCtx) error {
			wt, err := ctx.Worktree()
			if err != nil {
				return err
			}
			// Switch to main — what close-branch does when leaving the claim
			// branch.
			if _, err := wt.Branch(ctx.Context(), "main", ""); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("done")
		}, flow.StepConfig{Writes: flow.WriteContract{MayBranch: true}})
	}, &stubAgent{name: "stub"})

	// Start on the claim branch with a different SHA than main, so the
	// commit check would fire without the fix.
	be.SetInitialBranch("feature-x")
	be.SetBranchHeads(map[string]string{
		"feature-x": "aaa111",
		"main":      "bbb222",
	})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusDone) {
		t.Fatalf("status = %q, want done; park = %+v", res.Status, res.Park)
	}
}

func TestWriteContract_MayBranch_NoBranchChange_CommitCaught(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("sneaky", "plan", func(ctx flow.StepCtx) error {
			wt, err := ctx.Worktree()
			if err != nil {
				return err
			}
			// Do NOT switch branch, but do commit — MayBranch alone should
			// not exempt this.
			if err := wt.Commit(ctx.Context(), "sneaky commit"); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("done")
		}, flow.StepConfig{Writes: flow.WriteContract{MayBranch: true}})
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

func TestWriteContract_AllowedDirtyTree(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("edit", "plan", func(ctx flow.StepCtx) error {
			if _, err := ctx.Worktree(); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("done")
		}, flow.StepConfig{Writes: flow.WriteContract{MayEditTree: true}})
	}, &stubAgent{name: "stub"})

	wt, _ := be.Worktree(context.Background(), claim.ItemRef)
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
func testApp(t *testing.T, configure func(*flow.Flow), agent flow.Agent) (*App, *fake.Orchestrator, flow.Claim) {
	t.Helper()
	return testAppItem(t, flow.Item{Ref: itemRefFor("1"), Type: "task", Title: "test#1"}, []flow.ItemType{"task"}, configure, agent)
}

// testAppItem is testApp over a caller-supplied item and flow type set. The
// item's type is what routes flow selection, so a test about a type no flow
// accepts has to set both ends; every other test takes the task/task default.
func testAppItem(t *testing.T, item flow.Item, types []flow.ItemType, configure func(*flow.Flow), agent flow.Agent) (*App, *fake.Orchestrator, flow.Claim) {
	t.Helper()
	be := fake.New(flow.Signal("pr-open", "test"))
	be.AddItem(item.Ref.Display, item)

	app := &App{
		Orchestrator: be,
		Agent:        agent,
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
	app.Flow = f
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	// Capture stdout/stderr.
	app.Out = newDiscardWriter()
	app.Err = newDiscardWriter()

	ctx := context.Background()
	claim, err := be.Claim(ctx, item.Ref, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return app, be, claim
}

// bareBackend wraps a Backend via the interface (not a concrete embed), hiding
// all optional interfaces the concrete type may implement — StateInspector,
// QuestionAnswerer, etc. Used by tests that need to verify "not supported"
// paths.
type bareBackend struct{ flow.Orchestrator }

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

	state, _ := be.Load(context.Background(), claim.ItemRef)
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
type seedFailBackend struct{ *fake.Orchestrator }

func (b seedFailBackend) SeedState(ctx context.Context, ref flow.ItemRef, specs []flow.ArtifactSpec) error {
	return errors.New("boom: seed unavailable")
}

// noopSeedBackend models the pre-fix bug: SeedState silently no-ops, leaving
// the item with no required-artifact checklist.
type noopSeedBackend struct{ *fake.Orchestrator }

func (b noopSeedBackend) SeedState(ctx context.Context, ref flow.ItemRef, specs []flow.ArtifactSpec) error {
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
	app.Orchestrator = seedFailBackend{Orchestrator: be}

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
	app.Orchestrator = noopSeedBackend{Orchestrator: be}

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

	app.Preflight = func(ctx context.Context, state *flow.Item) error {
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
	state, _ := be.Load(context.Background(), claim.ItemRef)
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
	app.Preflight = func(ctx context.Context, state *flow.Item) error {
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
	state := &flow.Item{Ref: itemRefFor("x")}

	calls := []string{}
	a := flow.PreflightFunc(func(context.Context, *flow.Item) error {
		calls = append(calls, "a")
		return nil
	})
	b := flow.PreflightFunc(func(context.Context, *flow.Item) error {
		calls = append(calls, "b")
		return errors.New("b refused")
	})
	c := flow.PreflightFunc(func(context.Context, *flow.Item) error {
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
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {MaxInvocations: 1}}

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
		}, flow.StepConfig{})

	}, a)
	// Explicit cap: this test is about the gate firing, not about whatever the
	// package default happens to be.
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {MaxPromptsPerInvocation: 1}}

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
		}, flow.StepConfig{})
	}, a)
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {MaxCostUSD: 5}}

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
		}, flow.StepConfig{})
	}, a)
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {MaxCostUSD: 5}}

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
		}, flow.StepConfig{})
	}, a)
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {MaxCostUSD: 20}}

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
	st, err := be.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
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
type zeroCostGrantBackend struct{ *fake.Orchestrator }

func (b zeroCostGrantBackend) Load(ctx context.Context, ref flow.ItemRef) (*flow.Item, error) {
	st, err := b.Orchestrator.Load(ctx, ref)
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
	app.Orchestrator = zeroCostGrantBackend{Orchestrator: be}

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
	app.Orchestrator = zeroCostGrantBackend{Orchestrator: be}

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
		}, flow.StepConfig{})
	}, a)
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {MaxCostUSD: 5}}

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
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {Timeout: 50 * time.Millisecond}}

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
	*fake.Orchestrator
	wt *countingPatchWorktree
}

func (b *countingPatchBackend) Worktree(ctx context.Context, ref flow.ItemRef) (flow.Worktree, error) {
	inner, err := b.Orchestrator.Worktree(ctx, ref)
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
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {Timeout: 50 * time.Millisecond}}

	counting := &countingPatchBackend{Orchestrator: be, wt: &countingPatchWorktree{}}
	app.Orchestrator = counting

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
	state, _ := be.Load(context.Background(), claim.ItemRef)
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
	state, _ := be.Load(context.Background(), claim.ItemRef)
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
	state, _ := be.Load(context.Background(), claim.ItemRef)
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
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flow:         f,
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
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{
			flow.Artifact("plan", flow.ArtifactMarkdown),
			flow.Artifact("report", flow.ArtifactMarkdown),
		},
		Flow: f,
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
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactJSON)},
		Flow:         f,
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
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Signals:      []flow.SignalDef{flow.Signal("pr-open", "x")},
		Flow:         f,
	}
	if err := app.validate(); err == nil {
		t.Errorf("expected validation error for signal not in SupportedSignals")
	}
}

// TestSelectFlow_RequireSignalGate: the flow's signal preconditions gate
// selection. With one flow per binary there is nothing to pick BETWEEN, so what
// the gate decides is whether the one flow is eligible at all — an unsatisfied
// precondition leaves the item with no step to run, and satisfying it hands
// back the pending one.
func TestSelectFlow_RequireSignalGate(t *testing.T) {
	be := fake.New(flow.Signal("pr-open", "x"))
	f := flow.NewFlow("maintainer", []flow.ItemType{"task"})
	f.RequireSignal("pr-open")
	f.AddStep("merge", "commit", func(flow.StepCtx) error { return nil }, flow.StepConfig{})

	app := &App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("commit", flow.ArtifactCommitHash)},
		Signals:      []flow.SignalDef{flow.Signal("pr-open", "x")},
		Flow:         f,
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	state := &flow.Item{
		Ref: itemRefFor("1"), Type: "task",
		Artifacts: map[flow.ArtifactId]flow.ArtifactRecord{},
		Signals:   map[flow.SignalId]flow.SignalState{},
	}
	if picked, _ := SelectFlow(app, state); picked != nil {
		t.Errorf("expected no eligible flow without pr-open; got %v", picked.Name())
	}

	state.Signals["pr-open"] = flow.SignalState{Set: true}
	picked, next := SelectFlow(app, state)
	if picked == nil || picked.Name() != "maintainer" {
		t.Fatalf("expected maintainer once pr-open set; got %v", picked)
	}
	if next != "merge" {
		t.Errorf("next = %q, want %q", next, "merge")
	}
}

// pendingArtifactBackend wraps fake.Orchestrator so Load reports a required-
// but-unresolved artifact on the loaded state — modelling a status=done item
// whose finalization (summary / inspection) hasn't completed yet. T0481.
type pendingArtifactBackend struct {
	*fake.Orchestrator
	pending flow.ArtifactId
}

func (b *pendingArtifactBackend) Load(ctx context.Context, ref flow.ItemRef) (*flow.Item, error) {
	state, err := b.Orchestrator.Load(ctx, ref)
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

// finalizingBackend wraps fake.Orchestrator and counts Finalize calls so tests can
// distinguish the premature-finalize regression from the happy path.
type finalizingBackend struct {
	*fake.Orchestrator
	finalizeCalls int
}

func (b *finalizingBackend) Finalize(ctx context.Context, ref flow.ItemRef) error {
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
	wrapped := &finalizingBackend{Orchestrator: be}
	app.Orchestrator = &pendingArtifactBackend{Orchestrator: be, pending: "summary"}
	// Compose: the outer pendingArtifactBackend's Load is what RunOne
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

// unmatchedTypeApp builds an app whose flow has {task,bug} in its remit over an
// item typed "chore" — the shape of an ordinary GitHub issue carrying no
// type:* label against a binary that works task/bug items. The step handler
// fails the test: nothing may run for an item outside the remit.
func unmatchedTypeApp(t *testing.T, item flow.Item) (*App, *fake.Orchestrator, flow.Claim) {
	t.Helper()
	return testAppItem(t, item, []flow.ItemType{"task", "bug"}, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			t.Fatal("step handler ran for an item outside the remit — must not happen")
			return nil
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
}

// TestRunOne_BlocksWhenItemTypeIsOutsideTheRemit (#10): an item whose type is
// outside the remit was never seeded and never ran a step, so it must NOT be
// reported done and finalized — that reports success for work never attempted,
// terminally. It is blocked, with a reason naming the item's type, the
// registered types, and both ways a person can clear it.
func TestRunOne_BlocksWhenItemTypeIsOutsideTheRemit(t *testing.T) {
	app, be, claim := unmatchedTypeApp(t, flow.Item{Ref: itemRefFor("1"), Type: "chore", Title: "test#1"})
	wrapped := &finalizingBackend{Orchestrator: be}
	app.Orchestrator = wrapped

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
	state, err := be.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
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
	app, be, claim := unmatchedTypeApp(t, flow.Item{Ref: itemRefFor("1"), Type: "chore", Title: "test#1"})
	app.Orchestrator = &pendingArtifactBackend{Orchestrator: be, pending: "summary"}

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

// TestRunOne_RemitIsNotConsultedOnceTheJournalHasEntries: the remit is
// consulted before the journal's first entry and never after
// (docs/flow-registration.md § Item types). An item whose type is outside the
// remit but whose journal already carries an entry is past that point: the
// route is the authority, retyping mid-resolution redirects nothing, and the
// advance proceeds instead of reporting the item unworkable.
func TestRunOne_RemitIsNotConsultedOnceTheJournalHasEntries(t *testing.T) {
	ran := false
	app, be, claim := testAppItem(t,
		flow.Item{
			Ref: itemRefFor("1"), Type: "chore", Title: "test#1",
			// One completed execution, as #239's AppendEntry will record it.
			// The election is not read here — the checklist derivation is still
			// what picks the step (#245) — only its presence is.
			Journal: []flow.JournalEntry{{
				Step: "plan", Execution: 1, Route: flow.Route{Next: "plan"},
			}},
		},
		[]flow.ItemType{"task", "bug"},
		func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
				ran = true
				return ctx.ResolveMarkdown("the plan")
			}, flow.StepConfig{})
		}, &stubAgent{name: "stub"})
	wrapped := &finalizingBackend{Orchestrator: be}
	app.Orchestrator = wrapped

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status == "blocked" {
		t.Fatalf("status = blocked — an item with a journal is past the remit. res=%+v", res)
	}
	if !ran {
		t.Errorf("the pending step did not run; res=%+v", res)
	}
}

// TestRunOne_FinalizedItemWithUnmatchedTypeStaysDone (#10): the block is for
// items with work still owed. An already-finalized item's run is over —
// including one finalized by this very defect before it was fixed — and
// blocking it would strand it with no route onward.
func TestRunOne_FinalizedItemWithUnmatchedTypeStaysDone(t *testing.T) {
	app, be, claim := unmatchedTypeApp(t, flow.Item{Ref: itemRefFor("1"), Type: "chore", Title: "test#1", Finalized: true})
	wrapped := &finalizingBackend{Orchestrator: be}
	app.Orchestrator = wrapped

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
	app, be, claim := unmatchedTypeApp(t, flow.Item{Ref: itemRefFor("1"), Type: "", Title: "test#1"})
	wrapped := &finalizingBackend{Orchestrator: be}
	app.Orchestrator = wrapped

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
// InRemit, not off the declared type list, and a flow declaring no types has
// every type in its remit. Such an app has an empty registered-types list, so a check
// written against that list instead would block every item it owns — turning the
// guard on the very configuration it is meant to leave alone.
func TestRunOne_UniversalFlowIsNotATypeMismatch(t *testing.T) {
	app, be, claim := testAppItem(t,
		flow.Item{Ref: itemRefFor("1"), Type: "chore", Title: "test#1"},
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
	wrapped := &finalizingBackend{Orchestrator: be}
	app.Orchestrator = wrapped

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
		name string
		flow *flow.Flow
		want string
	}{
		{
			"universal flow declares no types",
			flow.NewFlow("any", nil),
			"none",
		},
		{
			"sorted and deduplicated",
			flow.NewFlow("a", []flow.ItemType{"task", "bug", "task"}),
			"bug, task",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := registeredTypes(&App{Flow: tc.flow}); got != tc.want {
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
	wrapped := &finalizingBackend{Orchestrator: be}
	app.Orchestrator = wrapped

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
// The three tiers effectiveTimeout resolves through, one test because the
// point is the ORDER: a granted timeout beats the policy, the policy beats the
// package default, and a step the policy says nothing about lands on the
// default rather than on zero.
func TestEffectiveTimeout_GrantedThenPolicyThenDefault(t *testing.T) {
	app := &App{StepBudgets: map[flow.StepId]flow.StepBudget{
		"plan": {Timeout: 5 * time.Minute},
	}}
	planned := flow.LifecycleItem{Kind: flow.LifecycleArtifact, ArtifactId: "plan"}
	unplanned := flow.LifecycleItem{Kind: flow.LifecycleArtifact, ArtifactId: "commit"}

	if got := app.effectiveTimeout(planned, flow.ArtifactRecord{GrantedTimeout: time.Hour}); got != time.Hour {
		t.Errorf("with a granted timeout: %v, want 1h — the grant wins over the policy", got)
	}
	if got := app.effectiveTimeout(planned, flow.ArtifactRecord{}); got != 5*time.Minute {
		t.Errorf("with no grant: %v, want 5m — the policy wins over the default", got)
	}
	if got := app.effectiveTimeout(unplanned, flow.ArtifactRecord{}); got != flow.DefaultStepBudget().Timeout {
		t.Errorf("step absent from the policy: %v, want the package default %v",
			got, flow.DefaultStepBudget().Timeout)
	}
}

// A step with no policy entry is funded at the package defaults WHOLE, and a
// step with a partial entry inherits the defaults axis by axis. This is what
// makes the "seeded on the item but no longer in the flow" case inherent:
// there is no lookup to miss, so nothing can silently read as a zero cap.
func TestAppStepBudget_MissingAndPartialEntries(t *testing.T) {
	app := &App{StepBudgets: map[flow.StepId]flow.StepBudget{
		"plan": {MaxCostUSD: 42},
	}}
	if got := app.stepBudget("retired-step"); got != flow.DefaultStepBudget() {
		t.Errorf("absent step budget = %+v, want the package defaults %+v", got, flow.DefaultStepBudget())
	}
	got := app.stepBudget("plan")
	if got.MaxCostUSD != 42 {
		t.Errorf("MaxCostUSD = %v, want 42 from the policy", got.MaxCostUSD)
	}
	if got.MaxInvocations != flow.DefaultStepBudget().MaxInvocations || got.Timeout != flow.DefaultStepBudget().Timeout {
		t.Errorf("unset axes = %+v, want the package defaults on each", got)
	}
}

// A nil policy is the ordinary case for a binary that configures no budgets at
// all, and it must not be a zero cap.
func TestAppStepBudget_NilPolicyIsAllDefaults(t *testing.T) {
	app := &App{}
	if got := app.stepBudget("plan"); got != flow.DefaultStepBudget() {
		t.Errorf("budget = %+v, want the package defaults %+v", got, flow.DefaultStepBudget())
	}
}

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
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {Timeout: 50 * time.Millisecond}}

	ctx := context.Background()
	res, err := RunOne(ctx, app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Axis != flow.AxisTimeout {
		t.Fatalf("first run = %+v, want parked/timeout", res)
	}

	// Grant a second of extra wall-clock, as `do grant --timeout 1` does.
	if err := be.Grant(ctx, claim.ItemRef, "plan", flow.Grant{TimeoutAdd: 1}); err != nil {
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
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {
		MaxInvocations: 1,
		Timeout:        50 * time.Millisecond,
	}}

	ctx := context.Background()
	res, err := RunOne(ctx, app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Axis != flow.AxisTimeout {
		t.Fatalf("first run = %+v, want parked/timeout", res)
	}
	// The kill consumed the step's only invocation, so both axes are flat.
	st, err := be.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
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
	*fake.Orchestrator
	evidence bool
}

func (b *outOfBandPatchBackend) ResolveArtifact(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId, body flow.ArtifactBody) error {
	if body.Type == flow.ArtifactPatch && !b.evidence {
		return fmt.Errorf("backend: ResolveArtifact %q: no implementation evidence on item", id)
	}
	return b.Orchestrator.ResolveArtifact(ctx, ref, id, body)
}

func (b *outOfBandPatchBackend) Worktree(ctx context.Context, ref flow.ItemRef) (flow.Worktree, error) {
	inner, err := b.Orchestrator.Worktree(ctx, ref)
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
	app.Orchestrator = &outOfBandPatchBackend{Orchestrator: be, evidence: true}

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
	state, _ := be.Load(context.Background(), claim.ItemRef)
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
	app.Orchestrator = &outOfBandPatchBackend{Orchestrator: be, evidence: false}

	if _, err := RunOne(context.Background(), app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if resolveErr == nil || !strings.Contains(resolveErr.Error(), "no implementation evidence") {
		t.Fatalf("ResolvePatch error = %v, want the backend's own evidence message", resolveErr)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
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
	state, _ := be.Load(context.Background(), claim.ItemRef)
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
	app.Preflight = func(context.Context, *flow.Item) error {
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
	app.Preflight = func(context.Context, *flow.Item) error {
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

// emptyAskBackend records nothing: AskQuestion reports success and returns a
// question carrying no id. That is the shape that produced the reported defect — a question
// park whose recovery path (`answer`) has no registered question to name.
type emptyAskBackend struct {
	*fake.Orchestrator
	parks int
}

func (b *emptyAskBackend) AskQuestion(context.Context, flow.ItemRef, flow.AgentQuestion) (flow.Question, error) {
	return flow.Question{}, nil
}

func (b *emptyAskBackend) Park(ctx context.Context, ref flow.ItemRef, req flow.ParkRequest) error {
	b.parks++
	return b.Orchestrator.Park(ctx, ref, req)
}

// An orchestrator that hands back a question with no id has registered nothing
// an operator can name. The step must fail rather than park: an item parked on
// a question `answer --question <id>` cannot reach is one nothing clears.
func TestRunOne_AskQuestionWithoutAnIdFailsInsteadOfParking(t *testing.T) {
	var handlerErr error
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("asks", "plan", func(ctx flow.StepCtx) error {
			handlerErr = ctx.AskQuestions(flow.AskText("base", "which base branch?"))
			return handlerErr
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	backend := &emptyAskBackend{Orchestrator: be}
	app.Orchestrator = backend

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "failed" {
		t.Errorf("status = %q, want failed", res.Status)
	}
	if handlerErr == nil {
		t.Fatal("ctx.AskQuestions returned nil; want an error")
	}
	var question flow.ErrQuestion
	if errors.As(handlerErr, &question) {
		t.Errorf("ctx.AskQuestions returned ErrQuestion %+v; want a plain error, so nothing parks", question)
	}
	if !strings.Contains(handlerErr.Error(), "without a question id") {
		t.Errorf("err = %v, want it to say the question came back without an id", handlerErr)
	}
	if backend.parks != 0 {
		t.Errorf("Park called %d times, want 0", backend.parks)
	}
	if state, _ := be.Load(context.Background(), claim.ItemRef); state.Parked() {
		t.Errorf("item parked on %+v, want no park", state.Park)
	}
}

// ctx.Park registers no question, so a question park raised through it leaves
// an item `answer` cannot clear — the same unanswerable state the ask route is
// guarded against above, through the other door. The step must fail instead,
// naming the route that works.
func TestRunOne_HandlerQuestionParkFailsTheStep(t *testing.T) {
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
	assertQuestionParkRefused(t, be, claim, res)
}

// The sentinel is exported, so a handler can return it without going through
// ctx.Park. The guard lives at the translation site — the write site — and so
// catches this door too; this test fails if it is ever moved into stepCtx.Park.
func TestRunOne_HandBuiltQuestionParkSentinelFailsTheStep(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("asks", "plan", func(ctx flow.StepCtx) error {
			return flow.ErrPark{Req: flow.ParkRequest{
				Kind:   flow.ParkQuestion,
				Reason: "which database?",
			}}
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	assertQuestionParkRefused(t, be, claim, res)
}

// The refusal turns on the kind and nothing else. A handler that stamps the ask
// time itself — the one shape the route used to let through untouched, since the
// stamping it did was conditional on the mark being absent — has still registered
// no question, so admitting it writes exactly the park `answer` cannot clear.
func TestRunOne_QuestionParkStampedByTheHandlerIsStillRefused(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("asks", "plan", func(ctx flow.StepCtx) error {
			return ctx.Park(flow.ParkRequest{
				Kind:    flow.ParkQuestion,
				Step:    "plan",
				Reason:  "which database?",
				Details: flow.MarkQuestionAsked(time.Now()),
			})
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	assertQuestionParkRefused(t, be, claim, res)
}

// assertQuestionParkRefused: the step failed naming ctx.AskQuestions, and
// nothing reached the orchestrator — no park written, no question registered.
func assertQuestionParkRefused(t *testing.T, be *fake.Orchestrator, claim flow.Claim, res flow.InvocationResult) {
	t.Helper()
	if res.Status != "failed" {
		t.Errorf("status = %q, want failed", res.Status)
	}
	if res.Park != nil {
		t.Errorf("Park = %+v, want nothing parked", res.Park)
	}
	if !strings.Contains(res.Reason, "ctx.AskQuestions") {
		t.Errorf("reason = %q, want it to name ctx.AskQuestions as the route that works", res.Reason)
	}
	if req := be.ParkRequest("1"); req != nil {
		t.Errorf("the backend was parked with %+v, want the park never written", req)
	}
	state, err := be.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Parked() {
		t.Errorf("item parked on %+v, want no park", state.Park)
	}
	if qs := state.PendingQuestions(); len(qs) != 0 {
		t.Errorf("PendingQuestions = %+v, want none registered", qs)
	}
}

// The refusal above reaches the question kind and no further: every other kind
// still parks through ctx.Park, with the kind it asked for, on the backend.
// The park itself is what has to be asserted — a check on the request's fields
// alone reads a nil park as clean, and so would pass just as well against a
// guard that had refused the park outright.
func TestRunOne_NonQuestionParkStillParks(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("blocks", "plan", func(ctx flow.StepCtx) error {
			return ctx.Park(flow.ParkRequest{Kind: flow.ParkBlocked, Reason: "waiting on infra"})
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkBlocked {
		t.Fatalf("res = %+v, want a blocked park", res)
	}
	if req := be.ParkRequest("1"); req == nil || req.Kind != flow.ParkBlocked {
		t.Errorf("the backend was parked with %+v, want a blocked park", req)
	}
}

// askedAtBackend answers AskQuestion with a stamp of its own choosing, so a
// test can tell the ask time the SDK took from the backend apart from one it
// read off the local clock. A zero askedAt is the backend that registered the
// question but has no server-side time to report (flow.Question.AskedAt is
// documented optional) — the case the local clock stands in for.
type askedAtBackend struct {
	*fake.Orchestrator
	askedAt time.Time
}

func (b *askedAtBackend) AskQuestion(ctx context.Context, ref flow.ItemRef, q flow.AgentQuestion) (flow.Question, error) {
	rec, err := b.Orchestrator.AskQuestion(ctx, ref, q)
	if err != nil {
		return rec, err
	}
	rec.AskedAt = b.askedAt
	return rec, nil
}

// With ctx.Park refused, ctx.AskQuestions is the only route left that writes a
// question park — so it is the only place the ask time can now come from.
// Without it the answer gate has no boundary and takes every comment already on
// the item, written long before the question, for a reply. The stamp is the
// BACKEND's clock: the replies it later reports are stamped by that same clock,
// and a local time compared against it discards answers a slightly fast runner
// can never get back.
func TestRunOne_AskRouteParkCarriesTheBackendsAskTime(t *testing.T) {
	askedAt := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("asks", "plan", func(ctx flow.StepCtx) error {
			return ctx.AskQuestions(flow.AskText("base", "which base branch?"))
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.Orchestrator = &askedAtBackend{Orchestrator: be, askedAt: askedAt}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkQuestion {
		t.Fatalf("res = %+v, want a question park", res)
	}
	if got := flow.QuestionAskedAt(res.Park); !got.Equal(askedAt) {
		t.Errorf("asked at %v, want the backend's own %v", got, askedAt)
	}
	// The gate reads the park off the item, not off this result.
	if got := flow.QuestionAskedAt(be.ParkRequest("1")); !got.Equal(askedAt) {
		t.Errorf("the backend was parked with asked-at %v, want %v", got, askedAt)
	}
}

// A backend that registered the question but reported no time of its own leaves
// the local clock as the only source. It is still stamped — an unmarked question
// park is the unbounded read above — and backed off by the skew allowance,
// because a mark that lands early costs one re-ask while one that lands late
// discards the answer permanently.
func TestRunOne_AskRouteWithoutABackendAskTimeStampsTheLocalClock(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("asks", "plan", func(ctx flow.StepCtx) error {
			return ctx.AskQuestions(flow.AskText("base", "which base branch?"))
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.Orchestrator = &askedAtBackend{Orchestrator: be}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkQuestion {
		t.Fatalf("res = %+v, want a question park", res)
	}
	got := flow.QuestionAskedAt(be.ParkRequest("1"))
	if got.IsZero() {
		t.Fatalf("Details = %q, want an asked-at marker", be.ParkRequest("1").Details)
	}
	if latest := time.Now().Add(-flow.LocalClockSkewAllowance); got.After(latest) {
		t.Errorf("asked at %v, want no later than %v — the local clock backed off by the skew allowance", got, latest)
	}
}

// A handler returning ErrRefused parks with ParkRefused and does NOT consume
// an invocation — symmetric with ErrTransient. The park reason carries the
// refusal's own message so the operator sees what was refused.
func TestRunOne_ErrRefusedParksWithoutBurningBudget(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("guarded", "plan", func(ctx flow.StepCtx) error {
			return fmt.Errorf("guard refused staged file main.go: %w", flow.ErrRefused)
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {MaxInvocations: 1}}

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
	state, _ := be.Load(context.Background(), claim.ItemRef)
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
	state, _ := be.Load(context.Background(), claim.ItemRef)
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
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if rec := state.Artifact("plan"); rec.Invocations != 0 {
		t.Errorf("Invocations = %d, want 0 (ErrTransient must not burn budget)", rec.Invocations)
	}
}

// clearMarkerBackend wraps a fake.Orchestrator and records ClearQuestionMarker
// calls so tests can observe the gate-path label clearing.
type clearMarkerBackend struct {
	*fake.Orchestrator
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
		}, flow.StepConfig{})
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
	if err := be.Grant(context.Background(), claim.ItemRef, "plan", flow.Grant{
		Invocations: 5,
	}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	wrapped := &clearMarkerBackend{Orchestrator: be}
	app.Orchestrator = wrapped

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
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {Timeout: 50 * time.Millisecond}}

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
	state, _ := be.Load(context.Background(), claim.ItemRef)
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
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if rec := state.Artifact("plan"); rec.Invocations != 0 {
		t.Errorf("Invocations = %d, want 0 (unfit catch-all must not burn budget)", rec.Invocations)
	}
}

// ---------------------------------------------------------------------------
// Blocked on items (docs/resolution.md § Blocked on items).
// ---------------------------------------------------------------------------

// blockOn declares `blocker` as a blocker of `item` through the editor — the
// contract's one way to record a dependency.
func blockOn(t *testing.T, be flow.Orchestrator, item, blocker flow.ItemRef) {
	t.Helper()
	ed, err := be.Edit(context.Background(), item)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	ed.AddBlocker(blocker)
	if err := ed.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// openBlockerDisplays is what a result says the item still waits on.
func openBlockerDisplays(res flow.InvocationResult) []string {
	return blockerDisplays(res.BlockedBy)
}

// The check before dispatch. An item waiting on an unfinished item stops
// clean: blocked, kind waits-on-items, naming the blockers and the pending
// step — and nothing else happens. No handler, no seed, no invocation, no
// park, no running record, and the claim is kept.
func TestRunOne_BlockedOnItemsStopsBeforeDispatch(t *testing.T) {
	t.Setenv("FLOW_DIR", filepath.Join(t.TempDir(), ".flow"))
	handlerRan := false
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			handlerRan = true
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	be.AddItem("2", flow.Item{Type: "task", Title: "the blocker"})
	blockOn(t, be, claim.ItemRef, be.Ref("2"))

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != string(flow.StatusBlocked) {
		t.Fatalf("status = %q, want blocked; res=%+v", res.Status, res)
	}
	if res.BlockKind != flow.WaitsOnItems {
		t.Errorf("BlockKind = %q, want %q", res.BlockKind, flow.WaitsOnItems)
	}
	if got := openBlockerDisplays(res); len(got) != 1 || got[0] != "2" {
		t.Errorf("open blockers = %v, want [2]", got)
	}
	if res.Step != "plan" || res.Flow != "implement" {
		t.Errorf("res names step %q on flow %q, want the pending step %q on %q", res.Step, res.Flow, "plan", "implement")
	}
	if res.Reason == "" || strings.Contains(res.Reason, "2") {
		t.Errorf("Reason = %q, want the orchestrator's kind-naming reason, never the blocker copied into prose", res.Reason)
	}
	if res.Park != nil {
		t.Errorf("Park = %+v, want nil — the stop is not a park", res.Park)
	}
	if res.InvocationID != "" || res.CostUSD != nil || res.DurationSeconds != 0 {
		t.Errorf("res = %+v carries dispatch-time fields, but nothing was dispatched", res)
	}
	if handlerRan {
		t.Error("the handler ran on a blocked item — an agent turn was spent on work that cannot proceed")
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if state.HasRequiredArtifacts() {
		t.Error("the item was seeded — a blocked stop records nothing")
	}
	if rec := state.Artifact("plan"); rec.Invocations != 0 {
		t.Errorf("Invocations = %d, want 0", rec.Invocations)
	}
	if be.ParkRequest("1") != nil {
		t.Errorf("park recorded: %+v — a blocked stop is not a park", be.ParkRequest("1"))
	}
	if held, _ := be.LookupActiveClaim(context.Background()); held == nil {
		t.Error("the claim was released — it is an arena reservation, and the stop keeps it")
	}
	if running, _ := clistate.LoadRunning(); running != nil {
		t.Errorf("running record left behind: %+v", running)
	}
}

// Both directions, without anyone touching the item. The blocker landing makes
// the item workable at the next read; reopened, it blocks again at the next
// read — from where the route now stands, since the plan ran in between. The
// claim survives all of it.
func TestRunOne_BlockerLandingAndReopeningIsSymmetric(t *testing.T) {
	planRuns := 0
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			planRuns++
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
		f.AddStep("record commit", "commit", func(ctx flow.StepCtx) error {
			t.Fatal("the commit step must not run while the item is blocked")
			return nil
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	be.AddItem("2", flow.Item{Type: "task", Title: "the blocker"})
	blockOn(t, be, claim.ItemRef, be.Ref("2"))

	run := func(when string) flow.InvocationResult {
		t.Helper()
		res, err := RunOne(context.Background(), app, claim)
		if err != nil {
			t.Fatalf("%s: RunOne: %v", when, err)
		}
		if held, _ := be.LookupActiveClaim(context.Background()); held == nil {
			t.Errorf("%s: the claim was released", when)
		}
		return res
	}

	if res := run("blocked"); res.Status != "blocked" || res.Step != "plan" {
		t.Fatalf("blocked: res = %+v, want blocked at plan", res)
	}
	// Nobody touches the item: the blocker lands.
	be.SetStatus("2", flow.StatusTerminal, "done")
	if res := run("blocker landed"); res.Status != "done" || res.Step != "plan" || planRuns != 1 {
		t.Fatalf("blocker landed: res = %+v (plan ran %d times), want the plan to run", res, planRuns)
	}
	// Nobody touches the item: the blocker is reopened.
	be.SetStatus("2", flow.StatusOpen, "reopened")
	res := run("blocker reopened")
	if res.Status != "blocked" || res.BlockKind != flow.WaitsOnItems {
		t.Fatalf("blocker reopened: res = %+v, want blocked on items again", res)
	}
	if res.Step != "commit" {
		t.Errorf("blocker reopened: pending step = %q, want %q — the route stood where the plan left it", res.Step, "commit")
	}
	if planRuns != 1 {
		t.Errorf("plan ran %d times, want 1 — a re-block does not rewind the route", planRuns)
	}
}

// An item both waiting on items and awaiting an answer reports waits-on-items:
// the precedence the derivation gives it, so the report agrees with `status`.
// The preflight never runs.
func TestRunOne_BlockedOnItemsOutranksAnUnansweredQuestion(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			t.Fatal("handler must not run")
			return nil
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	preflightRan := false
	app.Preflight = func(context.Context, *flow.Item) error {
		preflightRan = true
		return fmt.Errorf("answer needed on %q: %w", "plan", flow.ErrBlocked)
	}
	be.AddItem("2", flow.Item{Type: "task", Title: "the blocker"})
	blockOn(t, be, claim.ItemRef, be.Ref("2"))

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "blocked" || res.BlockKind != flow.WaitsOnItems {
		t.Fatalf("res = %+v, want blocked on items", res)
	}
	if strings.Contains(res.Reason, "preflight") {
		t.Errorf("Reason = %q is the preflight's, want the item's own blockedness first", res.Reason)
	}
	if preflightRan {
		t.Error("the preflight ran on an item already blocked on items")
	}
}

// The check sits after the no-flow block: an item with no pending step has
// nothing to be blocked from, and still finalizes.
func TestRunOne_BlockedItemWithNoPendingStepStillFinalizes(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.RequireSignal("pr-open") // never set, so no step is ever pending
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("ignored")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	be.AddItem("2", flow.Item{Type: "task", Title: "the blocker"})
	blockOn(t, be, claim.ItemRef, be.Ref("2"))
	wrapped := &finalizingBackend{Orchestrator: be}
	app.Orchestrator = wrapped

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" || wrapped.finalizeCalls != 1 {
		t.Errorf("res = %+v (finalize calls %d), want the finalize path", res, wrapped.finalizeCalls)
	}
}

// A step declares the blockers it finds and stops on them. The blocker is
// recorded on the item, the stop is the same clean stop the pre-dispatch check
// makes: blocked, kind waits-on-items, no park, no invocation charged, the
// artifact unresolved, and the step's work in progress kept for the resume.
func TestRunOne_HandlerWaitsOnItemsRecordsTheBlockerAndStopsClean(t *testing.T) {
	handlerRuns := 0
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			handlerRuns++
			if handlerRuns > 1 {
				// The resume: the blocker has landed, and the step finishes.
				return ctx.ResolveMarkdown("the plan")
			}
			if err := ctx.RecordWorkInProgress("half a plan"); err != nil {
				return err
			}
			return ctx.WaitOnItems(itemRefFor("2"))
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	be.AddItem("2", flow.Item{Type: "task", Title: "the blocker"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "blocked" || res.BlockKind != flow.WaitsOnItems {
		t.Fatalf("res = %+v, want blocked on items", res)
	}
	if got := openBlockerDisplays(res); len(got) != 1 || got[0] != "2" {
		t.Errorf("open blockers = %v, want [2] — the report comes from the reloaded item", got)
	}
	if res.Park != nil || be.ParkRequest("1") != nil {
		t.Errorf("parked (%+v / %+v) — the stop is not a park", res.Park, be.ParkRequest("1"))
	}
	if res.CostUSD == nil {
		t.Error("CostUSD = nil, want the dispatched step's spend reported — the handler ran")
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if !state.Blocked || len(state.BlockedBy) != 1 || state.BlockedBy[0].Ref.Display != "2" {
		t.Errorf("item = blocked %v by %+v, want the declared blocker recorded on the item", state.Blocked, state.BlockedBy)
	}
	rec := state.Artifact("plan")
	if rec.Invocations != 0 {
		t.Errorf("Invocations = %d, want 0 — a wait is not charged", rec.Invocations)
	}
	if rec.Resolved {
		t.Error("the artifact resolved on a stop")
	}
	if wip, _ := be.LoadWorkInProgress(context.Background(), claim.ItemRef, "plan"); wip != "half a plan" {
		t.Errorf("work in progress = %q, want it kept for the resume", wip)
	}
	// The next advance finds the item blocked before dispatch: no second turn.
	res2, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("second RunOne: %v", err)
	}
	if res2.Status != "blocked" || handlerRuns != 1 {
		t.Errorf("second run = %+v with the handler run %d times, want blocked before dispatch", res2, handlerRuns)
	}
	// And once the blocker lands, the step runs from where it stood.
	be.SetStatus("2", flow.StatusTerminal, "done")
	if res3, _ := RunOne(context.Background(), app, claim); res3.Status != "done" || handlerRuns != 2 {
		t.Errorf("after the blocker landed: %+v with the handler run %d times, want it dispatched again and completing", res3, handlerRuns)
	}
}

// Declared blockers that have already finished are accepted — naming an item
// that has landed is not an error — and the item then reads unblocked. That is
// a stop on nothing: reported `blocked`, it would tell the operator to wait
// for nothing, on an item `status` says is not blocked. It is the step's
// failure — a turn spent declaring a wait that does not hold — charged as one,
// with no block fields and no park. The blocker stays recorded (it is
// harmless and retractable), and the next advance runs the step.
func TestRunOne_HandlerWaitsOnFinishedItemsFailsAndTheNextAdvanceRuns(t *testing.T) {
	handlerRuns := 0
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			handlerRuns++
			if handlerRuns == 1 {
				return ctx.WaitOnItems(itemRefFor("2"))
			}
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	be.AddItem("2", flow.Item{Type: "task", Title: "already landed"})
	be.SetStatus("2", flow.StatusTerminal, "done")

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "failed" || res.Park != nil {
		t.Fatalf("res = %+v, want a failure with no park — nothing blocks the item, so nothing stopped the step", res)
	}
	if res.BlockKind != "" || len(res.BlockedBy) != 0 {
		t.Errorf("res = %+v, want no block fields on a failure", res)
	}
	if !strings.Contains(res.Reason, "2") || !strings.Contains(res.Reason, "finished") {
		t.Errorf("Reason = %q, want it to name what was declared and that it has finished", res.Reason)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if state.Blocked {
		t.Errorf("item reads blocked (%+v) — every declared blocker has finished", state.BlockedBy)
	}
	if len(state.BlockedBy) != 1 || state.BlockedBy[0].Status != flow.StatusTerminal {
		t.Errorf("BlockedBy = %+v, want the declared blocker recorded, listed as terminal", state.BlockedBy)
	}
	if rec := state.Artifact("plan"); rec.Invocations != 1 {
		t.Errorf("Invocations = %d, want 1 — a turn that declared a wait that does not hold is charged like any failure", rec.Invocations)
	}
	if be.ParkRequest("1") != nil {
		t.Errorf("park recorded: %+v — a failure is not a park", be.ParkRequest("1"))
	}
	if res2, _ := RunOne(context.Background(), app, claim); res2.Status != "done" || handlerRuns != 2 {
		t.Errorf("next run = %+v with the handler run %d times, want the step to run and complete", res2, handlerRuns)
	}
}

// A ref the orchestrator refuses to record fails the step, naming the ref.
// Nothing else is recorded: no blocker, no park.
func TestRunOne_WaitOnItemsUnresolvableRefFailsNamingIt(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.WaitOnItems(itemRefFor("nope"))
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed; res=%+v", res.Status, res)
	}
	if !strings.Contains(res.Reason, "nope") {
		t.Errorf("Reason = %q, want it to name the ref that could not be recorded", res.Reason)
	}
	if res.BlockKind != "" || len(res.BlockedBy) != 0 || res.Park != nil {
		t.Errorf("res = %+v, want no block fields and no park on a failure", res)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if len(state.BlockedBy) != 0 || be.ParkRequest("1") != nil {
		t.Errorf("recorded blockers %+v / park %+v, want nothing recorded", state.BlockedBy, be.ParkRequest("1"))
	}
}

// Blockers recorded before the refused one stay: adding one is idempotent, and
// retracting them would be a second write that could fail the same way.
func TestRunOne_WaitOnItemsKeepsBlockersRecordedBeforeARefusedOne(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.WaitOnItems(itemRefFor("2"), itemRefFor("nope"))
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	be.AddItem("2", flow.Item{Type: "task", Title: "the blocker"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "failed" || !strings.Contains(res.Reason, "nope") {
		t.Fatalf("res = %+v, want failed naming %q", res, "nope")
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if len(state.BlockedBy) != 1 || state.BlockedBy[0].Ref.Display != "2" {
		t.Errorf("BlockedBy = %+v, want the blocker recorded before the refusal to stay", state.BlockedBy)
	}
}

// blockerEditsBackend records what each committed edit staged as blockers, so
// a test can see that refs land one edit apiece — the GitHub editor refuses
// more than one dependency change per commit.
type blockerEditsBackend struct {
	*fake.Orchestrator
	commits [][]flow.ItemRef
}

func (b *blockerEditsBackend) Edit(ctx context.Context, ref flow.ItemRef) (flow.ItemEditor, error) {
	inner, err := b.Orchestrator.Edit(ctx, ref)
	if err != nil {
		return nil, err
	}
	return &blockerEditsEditor{ItemEditor: inner, be: b}, nil
}

type blockerEditsEditor struct {
	flow.ItemEditor
	be     *blockerEditsBackend
	staged []flow.ItemRef
}

func (e *blockerEditsEditor) AddBlocker(ref flow.ItemRef) {
	e.staged = append(e.staged, ref)
	e.ItemEditor.AddBlocker(ref)
}

func (e *blockerEditsEditor) Commit(ctx context.Context) error {
	e.be.commits = append(e.be.commits, e.staged)
	return e.ItemEditor.Commit(ctx)
}

func TestRunOne_WaitOnItemsCommitsOneEditPerRef(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.WaitOnItems(itemRefFor("2"), itemRefFor("3"))
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	be.AddItem("2", flow.Item{Type: "task", Title: "first"})
	be.AddItem("3", flow.Item{Type: "task", Title: "second"})
	recording := &blockerEditsBackend{Orchestrator: be}
	app.Orchestrator = recording

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "blocked" {
		t.Fatalf("res = %+v, want blocked", res)
	}
	if len(recording.commits) != 2 {
		t.Fatalf("committed %d edits, want 2 — one per ref", len(recording.commits))
	}
	for i, want := range []string{"2", "3"} {
		if len(recording.commits[i]) != 1 || recording.commits[i][0].Display != want {
			t.Errorf("edit %d staged %+v, want exactly %q", i, recording.commits[i], want)
		}
	}
	if got := openBlockerDisplays(res); len(got) != 2 {
		t.Errorf("open blockers = %v, want both recorded", got)
	}
}

// Waiting on nothing is not a state an item can be in.
func TestRunOne_WaitOnItemsWithNoRefsIsAnError(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.WaitOnItems()
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "failed" || !strings.Contains(res.Reason, "no items") {
		t.Fatalf("res = %+v, want failed for a wait on nothing", res)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if len(state.BlockedBy) != 0 || state.Blocked {
		t.Errorf("item = blocked %v by %+v, want nothing recorded", state.Blocked, state.BlockedBy)
	}
}

// Only waits-on-items stops before dispatch. A person-kind block — here a
// question park still awaiting its answer, which the orchestrator derives as
// blocked, kind waits-on-person — belongs to the answer preflight and the park
// machinery, and the check leaves it to them: a guard on `Blocked` alone would
// report every parked item as blocked on nothing and never dispatch the step
// that reads the answer.
func TestRunOne_PersonKindBlockIsNotStoppedBeforeDispatch(t *testing.T) {
	handlerRuns := 0
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			handlerRuns++
			if handlerRuns == 1 {
				return ctx.AskQuestions(flow.AskYesNo("ship", "Ship it?"))
			}
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "parked" {
		t.Fatalf("first run = %+v, %v; want a question park", res, err)
	}
	// The premise: the orchestrator reads the unanswered question as a block
	// of the person kind, so the item IS blocked when the second advance loads
	// it — just not on items.
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if !state.Blocked || state.BlockKind != flow.WaitsOnPerson {
		t.Fatalf("item = blocked %v kind %q, want blocked on a person — the fake's derivation changed under this test",
			state.Blocked, state.BlockKind)
	}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("second RunOne: %v", err)
	}
	if res.Status != "done" || handlerRuns != 2 {
		t.Errorf("second run = %+v with the handler run %d times, want the step dispatched and completing", res, handlerRuns)
	}
	if res.BlockKind != "" || len(res.BlockedBy) != 0 {
		t.Errorf("res = %+v carries block fields on a result that did not stop on items", res)
	}
}

// armedLoadFailureBackend fails Load once armed. The handler arms it, so the
// load that fails is the one after the handler ran — the reload a declared
// stop reads its report from.
type armedLoadFailureBackend struct {
	*fake.Orchestrator
	armed bool
}

func (b *armedLoadFailureBackend) Load(ctx context.Context, ref flow.ItemRef) (*flow.Item, error) {
	if b.armed {
		return nil, errors.New("tracker unreachable")
	}
	return b.Orchestrator.Load(ctx, ref)
}

// A reload that fails after the blockers were declared is an error of the
// advance, not a result: the stop reports from that reload, and with no item
// to read there is nothing to report — a made-up `blocked` would claim a
// derivation nothing performed, and a `failed` would charge the step for the
// tracker's outage. The declaration itself survived the failed report: the
// next advance, with the orchestrator answering again, finds the blocker
// before dispatch and spends no second turn.
func TestRunOne_ReloadFailureAfterDeclaringBlockersIsAnError(t *testing.T) {
	var wrapped *armedLoadFailureBackend
	handlerRuns := 0
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			handlerRuns++
			wrapped.armed = true
			return ctx.WaitOnItems(itemRefFor("2"))
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	be.AddItem("2", flow.Item{Type: "task", Title: "the blocker"})
	wrapped = &armedLoadFailureBackend{Orchestrator: be}
	app.Orchestrator = wrapped

	_, err := RunOne(context.Background(), app, claim)
	if err == nil || !strings.Contains(err.Error(), "reload after declaring blockers") || !strings.Contains(err.Error(), "tracker unreachable") {
		t.Fatalf("err = %v, want the reload failure, named as such", err)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if len(state.BlockedBy) != 1 || state.BlockedBy[0].Ref.Display != "2" {
		t.Errorf("BlockedBy = %+v, want the declared blocker recorded before the reload failed", state.BlockedBy)
	}
	if rec := state.Artifact("plan"); rec.Invocations != 0 {
		t.Errorf("Invocations = %d, want 0 — nothing is charged for a report that could not be made", rec.Invocations)
	}

	wrapped.armed = false
	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("second RunOne: %v", err)
	}
	if res.Status != "blocked" || res.BlockKind != flow.WaitsOnItems || handlerRuns != 1 {
		t.Errorf("second run = %+v with the handler run %d times, want blocked before dispatch on the blocker already recorded",
			res, handlerRuns)
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
	state, _ := be.Load(context.Background(), claim.ItemRef)
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
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if rec := state.Artifact("plan"); rec.Invocations != 1 {
		t.Errorf("Invocations = %d, want 1 (no catch-all → budget consumed)", rec.Invocations)
	}
}
