package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/backend/fake"
)

// noWorkBackend hides the fake's work-in-progress store by wrapping it in the
// interface every backend must satisfy. A backend that simply does not
// implement the optional one is the case a step has to keep working against.
type noWorkBackend struct{ flow.Backend }

// clearFailsBackend answers every clear with an error. The artifact has landed
// by the time the clear runs, so this is the case that must NOT be reported as
// a failed step.
type clearFailsBackend struct {
	*fake.Backend
	err error
}

func (b clearFailsBackend) ClearWorkInProgress(context.Context, flow.Claim, string) error {
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
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			gotBody, gotErr = ctx.WorkInProgress()
			saveErr = ctx.RecordWorkInProgress("half a plan")
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.Backend = noWorkBackend{app.Backend}

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
	if !errors.Is(saveErr, flow.ErrWorkInProgressUnsupported) {
		t.Errorf("RecordWorkInProgress = %v, want ErrWorkInProgressUnsupported", saveErr)
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
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			dispatches++
			seen, err := ctx.WorkInProgress()
			if err != nil {
				return err
			}
			if dispatches == 1 {
				if err := ctx.RecordWorkInProgress("what I worked out"); err != nil {
					return err
				}
				sameInvocation, _ = ctx.WorkInProgress()
				return ctx.Park(flow.ParkRequest{Kind: flow.ParkQuestion, Reason: "question: which one?"})
			}
			nextInvocation = seen
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
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
func TestWorkInProgress_ClearedWhenTheStepResolves(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			if err := ctx.RecordWorkInProgress("half a plan"); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	got, err := be.LoadWorkInProgress(context.Background(), claim, "plan")
	if err != nil {
		t.Fatalf("LoadWorkInProgress: %v", err)
	}
	if got != "" {
		t.Errorf("record after resolve = %q, want it cleared", got)
	}
}

// The artifact is already written when the clear runs, so a failure there
// cannot make the invocation a failure: that would report a failed step for
// work that landed. Keying, not clearing, is what makes a leftover harmless.
func TestWorkInProgress_ClearFailureDoesNotFailTheStep(t *testing.T) {
	tel := &recordingTelemetry{}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			if err := ctx.RecordWorkInProgress("half a plan"); err != nil {
				return err
			}
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.Telemetry = tel
	app.Backend = clearFailsBackend{Backend: be, err: errors.New("disk went away")}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Errorf("res = %+v, want done — the artifact landed", res)
	}
	state, _ := be.LoadState(context.Background(), claim)
	if rec := state.Artifact("plan"); !rec.Resolved {
		t.Error("plan is not resolved, but ResolveArtifact succeeded")
	}
	var reported bool
	for _, e := range tel.events {
		if e.Detail == "could not clear work in progress: disk went away" {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the failed clear was never reported; events = %+v", tel.events)
	}
}

// A record belongs to the step that wrote it. Reading another step's would
// hand one step reasoning it did not produce and cannot judge — and the record
// left here is exactly what a crash between resolve and clear leaves behind.
func TestWorkInProgress_IsNotVisibleToAnotherStep(t *testing.T) {
	var reviewSaw string
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			if err := ctx.RecordWorkInProgress("the plan step's reasoning"); err != nil {
				return err
			}
			return ctx.Park(flow.ParkRequest{Kind: flow.ParkQuestion, Reason: "question: which one?"})
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	// A second step, added after the helper's own validate so the artifact it
	// produces can be declared alongside it.
	app.Flows[0].AddStep("review", "review", func(ctx flow.StepCtx) error {
		reviewSaw, _ = ctx.WorkInProgress()
		return ctx.ResolveMarkdown("the review")
	}, flow.StepConfig{})
	app.Artifacts = append(app.Artifacts, flow.Artifact("review", flow.ArtifactMarkdown))
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "parked" {
		t.Fatalf("first RunOne = (%+v, %v), want parked", res, err)
	}
	// Resolve the plan through the backend, which does NOT clear the record —
	// the state a run killed between the write and the cleanup leaves behind.
	if err := be.ResolveArtifact(context.Background(), claim, "plan",
		flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the plan"}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}
	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Step != "review" {
		t.Fatalf("second RunOne = (%+v, %v), want the review step", res, err)
	}
	if reviewSaw != "" {
		t.Errorf("review read %q — that is the plan step's reasoning", reviewSaw)
	}
	if got, _ := be.LoadWorkInProgress(context.Background(), claim, "plan"); got == "" {
		t.Fatal("the plan's record is gone, so this proved nothing about keying")
	}
}

// loadFailsBackend answers every read with an error and counts the attempts.
// A store that is there but broken is not a store that is absent: the step is
// told, once.
type loadFailsBackend struct {
	*fake.Backend
	err   error
	loads int
}

func (b *loadFailsBackend) LoadWorkInProgress(context.Context, flow.Claim, string) (string, error) {
	b.loads++
	return "", b.err
}

// saveFailsBackend takes nothing and says so.
type saveFailsBackend struct {
	*fake.Backend
	err error
}

func (b saveFailsBackend) SaveWorkInProgress(context.Context, flow.Claim, string, string) error {
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
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			body, first = ctx.WorkInProgress()
			_, second = ctx.WorkInProgress()
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	store := &loadFailsBackend{Backend: be, err: wantErr}
	app.Backend = store

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
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			saveErr = ctx.RecordWorkInProgress("half a plan")
			readBack, _ = ctx.WorkInProgress()
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.Backend = saveFailsBackend{Backend: be, err: wantErr}

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
