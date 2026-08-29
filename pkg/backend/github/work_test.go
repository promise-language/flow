package github

import (
	"testing"

	"github.com/promise-language/flow"
)

// The store is keyed by the issue number, which is this backend's item
// identity everywhere else. Reading under another issue's key must report
// absence: a record that crossed items would put one issue's reasoning into
// another issue's agent, and from there into a public comment.
func TestWorkInProgress_KeyedByIssueAndStep(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)
	ctx := t.Context()

	claim42 := flow.Claim{BackendName: b.Name(), ItemRef: b.refFromIssue(42), Owner: "alice"}
	claim43 := flow.Claim{BackendName: b.Name(), ItemRef: b.refFromIssue(43), Owner: "alice"}

	if got, err := b.LoadWorkInProgress(ctx, claim42, "plan"); got != "" || err != nil {
		t.Errorf("Load with nothing stored = (%q, %v), want (\"\", nil)", got, err)
	}
	if err := b.SaveWorkInProgress(ctx, claim42, "plan", "issue 42's reasoning"); err != nil {
		t.Fatalf("SaveWorkInProgress: %v", err)
	}
	got, err := b.LoadWorkInProgress(ctx, claim42, "plan")
	if err != nil || got != "issue 42's reasoning" {
		t.Fatalf("Load under its own key = (%q, %v), want the stored body", got, err)
	}
	if got, err := b.LoadWorkInProgress(ctx, claim43, "plan"); got != "" || err != nil {
		t.Errorf("Load under another issue = (%q, %v), want (\"\", nil)", got, err)
	}
	if got, err := b.LoadWorkInProgress(ctx, claim42, "review"); got != "" || err != nil {
		t.Errorf("Load under another step = (%q, %v), want (\"\", nil)", got, err)
	}

	if err := b.ClearWorkInProgress(ctx, claim42, "plan"); err != nil {
		t.Fatalf("ClearWorkInProgress: %v", err)
	}
	if got, _ := b.LoadWorkInProgress(ctx, claim42, "plan"); got != "" {
		t.Errorf("Load after Clear = %q, want nothing", got)
	}
	if err := b.ClearWorkInProgress(ctx, claim42, "plan"); err != nil {
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
	b := newMockedBackend(t, mock, srv)
	ctx := t.Context()

	claim, err := b.Claim(ctx, b.refFromIssue(42), "alice", false)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	for _, step := range []string{"plan", "review"} {
		if err := b.SaveWorkInProgress(ctx, claim, step, "reasoning"); err != nil {
			t.Fatalf("SaveWorkInProgress(%s): %v", step, err)
		}
	}
	if err := b.Release(ctx, claim); err != nil {
		t.Fatalf("Release: %v", err)
	}
	for _, step := range []string{"plan", "review"} {
		if got, err := b.LoadWorkInProgress(ctx, claim, step); got != "" || err != nil {
			t.Errorf("Load %s after Release = (%q, %v), want nothing left", step, got, err)
		}
	}
}

// The backend must actually satisfy the optional capability: the SDK reaches it
// by type assertion, so a signature that drifted would degrade silently to "no
// store" and every step would go back to losing its work.
func TestBackendImplementsWorkInProgress(t *testing.T) {
	var _ flow.WorkInProgress = (*Backend)(nil)
}
