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
