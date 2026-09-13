package github

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
)

// Keyed by the issue and by NOTHING ELSE. That is the property the whole item
// turns on: the session belongs to the resolution, so it is the same record
// whichever step is asking, and a record under another issue's key reads as
// absence — one resolution's conversation must never arrive as another's memory.
func TestAgentSession_KeyedByIssueAlone(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)
	ctx := t.Context()

	ref42, ref43 := b.refFromIssue(42), b.refFromIssue(43)

	if got, err := b.LoadAgentSession(ctx, ref42); got != (flow.AgentSession{}) || err != nil {
		t.Errorf("Load with nothing stored = (%+v, %v), want the zero value and nil", got, err)
	}
	want := flow.AgentSession{SessionID: "sess-42", Boundary: "review"}
	if err := b.SaveAgentSession(ctx, ref42, want); err != nil {
		t.Fatalf("SaveAgentSession: %v", err)
	}
	got, err := b.LoadAgentSession(ctx, ref42)
	if err != nil || got != want {
		t.Fatalf("Load under its own key = (%+v, %v), want %+v", got, err, want)
	}
	if got, err := b.LoadAgentSession(ctx, ref43); got != (flow.AgentSession{}) || err != nil {
		t.Errorf("Load under another issue = (%+v, %v), want the zero value and nil", got, err)
	}

	if err := b.ClearAgentSession(ctx, ref42); err != nil {
		t.Fatalf("ClearAgentSession: %v", err)
	}
	if got, _ := b.LoadAgentSession(ctx, ref42); got != (flow.AgentSession{}) {
		t.Errorf("Load after Clear = %+v, want the zero value", got)
	}
	if err := b.ClearAgentSession(ctx, ref42); err != nil {
		t.Errorf("second Clear = %v, want nil — clearing what is not there is not an error", err)
	}
}

// Releasing a claim ends that conversation's life, for the reason it ends the
// drafts': a handle left behind names a session holding the resolution's whole
// reasoning, kept once there is nothing left to continue.
func TestAgentSession_ReleaseLeavesNoRecord(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)
	ctx := t.Context()

	ref := b.refFromIssue(42)
	if _, err := b.Claim(ctx, ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SaveAgentSession(ctx, ref, flow.AgentSession{SessionID: "sess-1"}); err != nil {
		t.Fatalf("SaveAgentSession: %v", err)
	}
	if err := b.Release(ctx, ref); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got, err := b.LoadAgentSession(ctx, ref); got != (flow.AgentSession{}) || err != nil {
		t.Errorf("Load after Release = (%+v, %v), want nothing left", got, err)
	}
}

// A reset takes the item's session with the record it belonged to: a
// conversation kept past the journal it was working towards has nothing left to
// continue, and the next dispatch would resume reasoning about a route that no
// longer exists. This issue's only.
func TestAgentSession_ResetClearsTheItemsSession(t *testing.T) {
	_, b, claim := newJournalEnv(t)
	ctx := t.Context()

	if err := b.SaveAgentSession(ctx, claim.ItemRef, flow.AgentSession{SessionID: "sess-1", Boundary: "review"}); err != nil {
		t.Fatalf("SaveAgentSession: %v", err)
	}
	other := b.refFromIssue(43)
	if err := b.SaveAgentSession(ctx, other, flow.AgentSession{SessionID: "sess-43"}); err != nil {
		t.Fatalf("SaveAgentSession(#43): %v", err)
	}

	if err := b.Reset(ctx, claim.ItemRef); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if got, err := b.LoadAgentSession(ctx, claim.ItemRef); got != (flow.AgentSession{}) || err != nil {
		t.Errorf("session after Reset = (%+v, %v), want the zero value and nil", got, err)
	}
	if got, _ := b.LoadAgentSession(ctx, other); got.SessionID != "sess-43" {
		t.Errorf("another issue's session = %+v, want it untouched", got)
	}
}

// The store must work with NO disclosure guard installed, for the reason the
// draft store must: everything this package sends outward goes through
// `outward`, so a store that survives here is a store with no route outward —
// which is the structural form of "never published".
func TestAgentSession_IsNotAPublishingPath(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)
	b.out.guard = nil
	ctx := t.Context()
	ref := b.refFromIssue(42)

	if err := b.SaveAgentSession(ctx, ref, flow.AgentSession{SessionID: "sess-1"}); err != nil {
		t.Fatalf("SaveAgentSession with no guard installed: %v", err)
	}
	if got, err := b.LoadAgentSession(ctx, ref); got.SessionID != "sess-1" || err != nil {
		t.Fatalf("Load with no guard installed = (%+v, %v), want the stored handle", got, err)
	}
	if err := b.ClearAgentSession(ctx, ref); err != nil {
		t.Fatalf("ClearAgentSession with no guard installed: %v", err)
	}
	mock.mu.Lock()
	leaked := append([]string(nil), mock.mutations...)
	mock.mu.Unlock()
	if len(leaked) > 0 {
		t.Errorf("the session store reached GitHub: %v", leaked)
	}
}

// A ref that names no issue has no key, and every method says so. A fallback
// key — issue 0, shared by every malformed ref — is exactly the cross-item read
// the keying exists to prevent, arriving as the agent's own memory.
func TestAgentSession_RefusesARefThatNamesNoIssue(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)
	ctx := t.Context()
	ref := flow.ItemRef{OrchestratorName: b.Name(), Display: "o/r#?", Ref: json.RawMessage(`{"issue":0}`)}

	if err := b.SaveAgentSession(ctx, ref, flow.AgentSession{SessionID: "sess-1"}); err == nil {
		t.Error("SaveAgentSession stored a record for a ref that names no issue")
	}
	if got, err := b.LoadAgentSession(ctx, ref); err == nil {
		t.Errorf("LoadAgentSession = (%+v, nil), want an error rather than a fallback key", got)
	}
	if err := b.ClearAgentSession(ctx, ref); err == nil {
		t.Error("ClearAgentSession reported success for a ref that names no issue")
	}
	sessionDir, derr := clistate.SessionDir()
	if derr != nil {
		t.Fatalf("clistate.SessionDir: %v", derr)
	}
	if _, err := os.Stat(sessionDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("something was written under %s; stat err = %v", sessionDir, err)
	}
}
