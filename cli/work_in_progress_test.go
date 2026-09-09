package cli

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// noWorkBackend hides the fake's work-in-progress store by wrapping it in the
// interface every backend must satisfy. A backend that simply does not
// implement the optional one is the case a step has to keep working against.
// noWorkBackend refuses the work-in-progress store rather than lacking it:
// there are no optional capabilities, so "no store here" is an answer the
// method gives.
type noWorkBackend struct{ flow.Orchestrator }

func (noWorkBackend) SaveWorkInProgress(context.Context, flow.ItemRef, flow.StepId, string) error {
	return fmt.Errorf("this orchestrator keeps no work-in-progress store: %w", flow.ErrUnsupported)
}

func (noWorkBackend) LoadWorkInProgress(context.Context, flow.ItemRef, flow.StepId) (string, error) {
	return "", nil
}

// appendFailsBackend refuses every append. The result, the route and the end of
// the draft are ONE write now, so a refusal must leave all three untouched —
// which is what makes the draft still there for the resume.
type appendFailsBackend struct {
	*fake.Orchestrator
	err error
}

func (b appendFailsBackend) AppendEntry(context.Context, flow.ItemRef, flow.JournalEntry) error {
	return b.err
}

// A backend with no store reads as absence and refuses a write by name. Reads
// degrade because the record is optional; the write says so because a caller
// that believed it stashed something and did not would park expecting to
// resume from a draft that was never written.
func TestWorkInProgress_BackendWithoutAStore(t *testing.T) {
	var (
		gotBody string
		gotErr  error
		saveErr error
	)
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			gotBody, gotErr = ctx.WorkInProgress()
			saveErr = ctx.RecordWorkInProgress("half a plan")
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, &stubAgent{name: "stub"})
	app.Orchestrator = noWorkBackend{app.Orchestrator}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Errorf("res = %+v, want done — a missing store must not stop a step", res)
	}
	if gotBody != "" || gotErr != nil {
		t.Errorf("WorkInProgress() = (%q, %v), want (\"\", nil) with no store", gotBody, gotErr)
	}
	if !errors.Is(saveErr, flow.ErrUnsupported) {
		t.Errorf("RecordWorkInProgress = %v, want ErrUnsupported", saveErr)
	}
}

// The record has to reach the NEXT dispatch — that is the whole point — and it
// has to be readable inside the invocation that wrote it, or a step cannot
// build on what it just stashed.
func TestWorkInProgress_SurvivesToTheNextDispatch(t *testing.T) {
	var (
		sameInvocation string
		nextInvocation string
		dispatches     int
	)
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			dispatches++
			seen, err := ctx.WorkInProgress()
			if err != nil {
				return flow.StepResult{}, err
			}
			if dispatches == 1 {
				if err := ctx.RecordWorkInProgress("what I worked out"); err != nil {
					return flow.StepResult{}, err
				}
				sameInvocation, _ = ctx.WorkInProgress()
				return flow.StepResult{}, ctx.Park(flow.ParkRequest{Kind: flow.ParkBlocked, Reason: "stopping here to test the next dispatch"})
			}
			nextInvocation = seen
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, &stubAgent{name: "stub"})

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "parked" {
		t.Fatalf("first RunOne = (%+v, %v), want parked", res, err)
	}
	if sameInvocation != "what I worked out" {
		t.Errorf("read back in the same invocation = %q, want the body just recorded", sameInvocation)
	}
	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("second RunOne = (%+v, %v), want done", res, err)
	}
	if nextInvocation != "what I worked out" {
		t.Errorf("read on the next dispatch = %q, want the stashed body", nextInvocation)
	}
}

// Scaffolding that outlives its work becomes stale prose a later reader
// mistakes for a record.
func TestWorkInProgress_ClearedWhenTheStepCompletes(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if err := ctx.RecordWorkInProgress("half a plan"); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, &stubAgent{name: "stub"})

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	got, err := be.LoadWorkInProgress(context.Background(), claim.ItemRef, "plan")
	if err != nil {
		t.Fatalf("LoadWorkInProgress: %v", err)
	}
	if got != "" {
		t.Errorf("record after the completion = %q, want it cleared", got)
	}
	// And the draft went with the entry, not before it: the journal carries the
	// completion the clear belongs to.
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if len(state.Journal) != 1 {
		t.Errorf("journal = %+v, want the one entry the clear belongs to", state.Journal)
	}
}

// Result, route and the end of the draft land together or not at all. A refused
// append leaves the draft in place, because nothing was recorded and the resume
// has to pick up where the step left off — the clear cannot be a separate write
// that succeeded against a result that did not.
func TestWorkInProgress_RefusedAppendKeepsTheDraft(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if err := ctx.RecordWorkInProgress("half a plan"); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, &stubAgent{name: "stub"})
	app.Orchestrator = appendFailsBackend{Orchestrator: be, err: errors.New("disk went away")}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status == string(flow.StatusDone) {
		t.Errorf("res = %+v, want a non-done status — nothing was recorded", res)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if rec := state.Artifact("plan"); rec.Resolved {
		t.Error("the projection moved though the append was refused")
	}
	if len(state.Journal) != 0 {
		t.Errorf("journal = %+v, want empty — the append was refused", state.Journal)
	}
	got, err := be.LoadWorkInProgress(context.Background(), claim.ItemRef, "plan")
	if err != nil {
		t.Fatalf("LoadWorkInProgress: %v", err)
	}
	if got != "half a plan" {
		t.Errorf("draft = %q, want it kept for the resume", got)
	}
}

// A record belongs to the step that wrote it. Reading another step's would hand
// one step reasoning it did not produce and cannot judge — so the keying has to
// hold even for a record that outlives the step that wrote it.
func TestWorkInProgress_IsNotVisibleToAnotherStep(t *testing.T) {
	var reviewSaw string
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if err := ctx.RecordWorkInProgress("the plan step's reasoning"); err != nil {
				return flow.StepResult{}, err
			}
			return flow.StepResult{}, ctx.Park(flow.ParkRequest{Kind: flow.ParkBlocked, Reason: "stopping here so the record outlives the step"})
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	// A second step, added after the helper's own validate so the artifact it
	// produces can be declared alongside it.
	app.Flow.AddStep("review", "review", func(ctx flow.StepCtx) (flow.StepResult, error) {
		reviewSaw, _ = ctx.WorkInProgress()
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the review"), nil
	}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	app.Artifacts = append(app.Artifacts, flow.Artifact("review", flow.ArtifactMarkdown))
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "parked" {
		t.Fatalf("first RunOne = (%+v, %v), want parked", res, err)
	}
	// Complete the plan through the backend, then put a record back under its
	// step id: a record that outlived the step that wrote it, which is the
	// state the keying has to hold under.
	appendMarkdown(t, be, claim.ItemRef, "plan", "the plan")
	if err := be.SaveWorkInProgress(context.Background(), claim.ItemRef, "plan", "the plan step's reasoning"); err != nil {
		t.Fatalf("SaveWorkInProgress: %v", err)
	}
	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Step != "review" {
		t.Fatalf("second RunOne = (%+v, %v), want the review step", res, err)
	}
	if reviewSaw != "" {
		t.Errorf("review read %q — that is the plan step's reasoning", reviewSaw)
	}
	if got, _ := be.LoadWorkInProgress(context.Background(), claim.ItemRef, "plan"); got == "" {
		t.Fatal("the plan's record is gone, so this proved nothing about keying")
	}
}

// loadFailsBackend answers every read with an error and counts the attempts.
// A store that is there but broken is not a store that is absent: the step is
// told, once.
type loadFailsBackend struct {
	*fake.Orchestrator
	err   error
	loads int
}

func (b *loadFailsBackend) LoadWorkInProgress(context.Context, flow.ItemRef, flow.StepId) (string, error) {
	b.loads++
	return "", b.err
}

// saveFailsBackend takes nothing and says so.
type saveFailsBackend struct {
	*fake.Orchestrator
	err error
}

func (b saveFailsBackend) SaveWorkInProgress(context.Context, flow.ItemRef, flow.StepId, string) error {
	return b.err
}

// A read that failed is reported to the step rather than answered as "nothing
// stashed": absence means re-derive, and a step told that when the record is
// actually sitting in an unreachable store has been told something false. It
// costs one read per invocation — the memo holds the failure too, so a step
// that asks twice does not hammer a store that is down.
func TestWorkInProgress_ReadFailureReachesTheStep(t *testing.T) {
	var (
		body    string
		first   error
		second  error
		wantErr = errors.New("store is unreachable")
	)
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			body, first = ctx.WorkInProgress()
			_, second = ctx.WorkInProgress()
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, &stubAgent{name: "stub"})
	store := &loadFailsBackend{Orchestrator: be, err: wantErr}
	app.Orchestrator = store

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Errorf("res = %+v, want done — the step decides what a failed read costs", res)
	}
	if !errors.Is(first, wantErr) || body != "" {
		t.Errorf("WorkInProgress() = (%q, %v), want (\"\", the store's error)", body, first)
	}
	if !errors.Is(second, wantErr) {
		t.Errorf("second WorkInProgress() = %v, want the same error", second)
	}
	if store.loads != 1 {
		t.Errorf("read the store %d times in one invocation, want 1 — the load is memoised", store.loads)
	}
}

// A write that failed is named, and leaves nothing behind: a step that read
// back work the store never took would build its next invocation on a draft
// that does not exist anywhere.
func TestWorkInProgress_SaveFailureReachesTheStepAndStashesNothing(t *testing.T) {
	var (
		saveErr  error
		readBack string
		wantErr  = errors.New("disk went away")
	)
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			saveErr = ctx.RecordWorkInProgress("half a plan")
			readBack, _ = ctx.WorkInProgress()
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, &stubAgent{name: "stub"})
	app.Orchestrator = saveFailsBackend{Orchestrator: be, err: wantErr}

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	if !errors.Is(saveErr, wantErr) {
		t.Errorf("RecordWorkInProgress = %v, want the store's own error", saveErr)
	}
	if readBack != "" {
		t.Errorf("read back %q after a write that failed, want nothing", readBack)
	}
}
