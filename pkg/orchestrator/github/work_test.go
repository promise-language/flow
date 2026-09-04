package github

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
)

// The store is keyed by the issue number, which is this orchestrator's item
// identity everywhere else. Reading under another issue's key must report
// absence: a record that crossed items would put one issue's reasoning into
// another issue's agent, and from there into a public comment.
func TestWorkInProgress_KeyedByIssueAndStep(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)
	ctx := t.Context()

	ref42, ref43 := b.refFromIssue(42), b.refFromIssue(43)

	if got, err := b.LoadWorkInProgress(ctx, ref42, "plan"); got != "" || err != nil {
		t.Errorf("Load with nothing stored = (%q, %v), want (\"\", nil)", got, err)
	}
	if err := b.SaveWorkInProgress(ctx, ref42, "plan", "issue 42's reasoning"); err != nil {
		t.Fatalf("SaveWorkInProgress: %v", err)
	}
	got, err := b.LoadWorkInProgress(ctx, ref42, "plan")
	if err != nil || got != "issue 42's reasoning" {
		t.Fatalf("Load under its own key = (%q, %v), want the stored body", got, err)
	}
	if got, err := b.LoadWorkInProgress(ctx, ref43, "plan"); got != "" || err != nil {
		t.Errorf("Load under another issue = (%q, %v), want (\"\", nil)", got, err)
	}
	if got, err := b.LoadWorkInProgress(ctx, ref42, "review"); got != "" || err != nil {
		t.Errorf("Load under another step = (%q, %v), want (\"\", nil)", got, err)
	}

	if err := b.ClearWorkInProgress(ctx, ref42, "plan"); err != nil {
		t.Fatalf("ClearWorkInProgress: %v", err)
	}
	if got, _ := b.LoadWorkInProgress(ctx, ref42, "plan"); got != "" {
		t.Errorf("Load after Clear = %q, want nothing", got)
	}
	if err := b.ClearWorkInProgress(ctx, ref42, "plan"); err != nil {
		t.Errorf("second Clear = %v, want nil — clearing what is not there is not an error", err)
	}
}

// Releasing a claim ends that reasoning's life. Left behind, it is prose on
// disk with nothing left to resume — a disclosure sitting around for no
// benefit.
func TestWorkInProgress_ReleaseLeavesNoRecords(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)
	ctx := t.Context()

	ref := b.refFromIssue(42)
	if _, err := b.Claim(ctx, ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	for _, step := range []flow.StepId{"plan", "review"} {
		if err := b.SaveWorkInProgress(ctx, ref, step, "reasoning"); err != nil {
			t.Fatalf("SaveWorkInProgress(%s): %v", step, err)
		}
	}
	if err := b.Release(ctx, ref); err != nil {
		t.Fatalf("Release: %v", err)
	}
	for _, step := range []flow.StepId{"plan", "review"} {
		if got, err := b.LoadWorkInProgress(ctx, ref, step); got != "" || err != nil {
			t.Errorf("Load %s after Release = (%q, %v), want nothing left", step, got, err)
		}
	}
}

// The store must work with NO disclosure guard installed. Everything this
// package sends outward goes through `outward`, which refuses every act when
// no guard is present (TestNoGuardPublishesNothing), so a store that survives
// here is a store with no route outward — which is what lets it hold the one
// text that has nowhere else to go: prose a guard refused.
func TestWorkInProgress_IsNotAPublishingPath(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)
	b.out.guard = nil
	ctx := t.Context()
	ref := b.refFromIssue(42)

	const refused = "the plan the guard would not take"
	if err := b.SaveWorkInProgress(ctx, ref, "plan", refused); err != nil {
		t.Fatalf("SaveWorkInProgress with no guard installed: %v", err)
	}
	if got, err := b.LoadWorkInProgress(ctx, ref, "plan"); got != refused || err != nil {
		t.Fatalf("Load with no guard installed = (%q, %v), want the stored body", got, err)
	}
	if err := b.ClearWorkInProgress(ctx, ref, "plan"); err != nil {
		t.Fatalf("ClearWorkInProgress with no guard installed: %v", err)
	}
	mock.mu.Lock()
	leaked := append([]string(nil), mock.mutations...)
	mock.mu.Unlock()
	if len(leaked) > 0 {
		t.Errorf("the work store reached GitHub: %v", leaked)
	}
}

// A ref that names no issue has no key, and every method says so. The
// alternative is a fallback key — issue 0 — shared by every item with a
// malformed ref, which is exactly the cross-item read the keying exists to
// prevent, arriving as the agent's own reasoning.
func TestWorkInProgress_RefusesARefThatNamesNoIssue(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)
	ctx := t.Context()
	ref := flow.ItemRef{OrchestratorName: b.Name(), Display: "o/r#?", Ref: json.RawMessage(`{"issue":0}`)}

	if err := b.SaveWorkInProgress(ctx, ref, "plan", "reasoning"); err == nil {
		t.Error("SaveWorkInProgress stored a record for a ref that names no issue")
	}
	if got, err := b.LoadWorkInProgress(ctx, ref, "plan"); err == nil {
		t.Errorf("LoadWorkInProgress = (%q, nil), want an error rather than a fallback key", got)
	}
	if err := b.ClearWorkInProgress(ctx, ref, "plan"); err == nil {
		t.Error("ClearWorkInProgress reported success for a ref that names no issue")
	}
	if _, err := os.Stat(clistate.WorkDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("something was written under %s; stat err = %v", clistate.WorkDir(), err)
	}
}
