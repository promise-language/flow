package github

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/promise-language/flow"
)

// signalMock is a minimal mock for testing refreshPRSignals in isolation.
// It serves only the /pulls and /pulls/{n}/reviews endpoints.
type signalMock struct {
	mu    sync.Mutex
	pr    map[string]any // if non-nil, returned as [pr] from /pulls
	owner string
	repo  string
}

func (m *signalMock) server() *httptest.Server {
	prefix := "/repos/" + m.owner + "/" + m.repo
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.pr == nil {
			writeJSON(w, []any{})
			return
		}
		writeJSON(w, []any{m.pr})
	})
	// Reviews endpoint — returns empty.
	mux.HandleFunc(prefix+"/pulls/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{})
	})
	return httptest.NewServer(mux)
}

func newSignalBackend(t *testing.T, m *signalMock, srv *httptest.Server) *Backend {
	t.Helper()
	t.Setenv("FLOW_DIR", t.TempDir())
	git := newGitOps(".")
	b := &Backend{
		cfg: Config{
			Owner:       m.owner,
			Repo:        m.repo,
			BinaryName:  "implement",
			Token:       "fake-token",
			LabelPrefix: "flow:",
		},
		out:               newOutward("fake-token", git, m.owner, m.repo, allowing()),
		git:               git,
		labels:            newLabels("flow:"),
		stateCommentCache: map[int]int64{},
	}
	if _, err := b.WithBaseURL(srv.URL+"/", srv.URL+"/"); err != nil {
		t.Fatalf("WithBaseURL: %v", err)
	}
	return b
}

func newState() *flow.ItemState {
	return &flow.ItemState{
		Signals:   map[flow.SignalId]flow.SignalState{},
		Artifacts: map[flow.ArtifactId]flow.ArtifactRecord{},
	}
}

func TestRefreshPRSignals_PROpen_SetsSignal(t *testing.T) {
	m := &signalMock{
		owner: "o", repo: "r",
		pr: map[string]any{
			"number": 1,
			"state":  "open",
			"merged": false,
		},
	}
	srv := m.server()
	defer srv.Close()
	b := newSignalBackend(t, m, srv)

	state := newState()
	if err := b.refreshPRSignals(t.Context(), 42, state); err != nil {
		t.Fatalf("refreshPRSignals: %v", err)
	}
	if !state.SignalSet("pr-open") {
		t.Fatal("pr-open should be set when PR is open")
	}
	if state.SignalSet("pr-merged") {
		t.Fatal("pr-merged should not be set")
	}
}

func TestRefreshPRSignals_PRMerged_LatchKeepsPROpen(t *testing.T) {
	m := &signalMock{
		owner: "o", repo: "r",
		pr: map[string]any{
			"number": 1,
			"state":  "closed",
			"merged": true,
		},
	}
	srv := m.server()
	defer srv.Close()
	b := newSignalBackend(t, m, srv)

	// State already has pr-open set (from a prior poll or side-effect).
	state := newState()
	state.Signals["pr-open"] = flow.SignalState{Set: true, By: "poll"}

	if err := b.refreshPRSignals(t.Context(), 42, state); err != nil {
		t.Fatalf("refreshPRSignals: %v", err)
	}
	if !state.SignalSet("pr-open") {
		t.Fatal("pr-open should remain set after merge (latch)")
	}
	if !state.SignalSet("pr-merged") {
		t.Fatal("pr-merged should be set")
	}
	if !state.SignalSet("pr-closed") {
		t.Fatal("pr-closed should be set")
	}
}

func TestRefreshPRSignals_PRClosedWithoutMerge_LatchKeepsPROpen(t *testing.T) {
	m := &signalMock{
		owner: "o", repo: "r",
		pr: map[string]any{
			"number": 1,
			"state":  "closed",
			"merged": false,
		},
	}
	srv := m.server()
	defer srv.Close()
	b := newSignalBackend(t, m, srv)

	state := newState()
	state.Signals["pr-open"] = flow.SignalState{Set: true, By: "poll"}

	if err := b.refreshPRSignals(t.Context(), 42, state); err != nil {
		t.Fatalf("refreshPRSignals: %v", err)
	}
	if !state.SignalSet("pr-open") {
		t.Fatal("pr-open should remain set after close (latch)")
	}
	if state.SignalSet("pr-merged") {
		t.Fatal("pr-merged should not be set for a non-merge close")
	}
	if !state.SignalSet("pr-closed") {
		t.Fatal("pr-closed should be set")
	}
}

func TestRefreshPRSignals_NoPR_SignalStaysUnset(t *testing.T) {
	m := &signalMock{owner: "o", repo: "r", pr: nil}
	srv := m.server()
	defer srv.Close()
	b := newSignalBackend(t, m, srv)

	state := newState()
	if err := b.refreshPRSignals(t.Context(), 42, state); err != nil {
		t.Fatalf("refreshPRSignals: %v", err)
	}
	if state.SignalSet("pr-open") {
		t.Fatal("pr-open should stay unset when no PR exists")
	}
}

func TestRefreshPRSignals_NeverPolledOpen_StaysFalse(t *testing.T) {
	// Edge case: PR was opened and closed between polls, and the
	// side-effect write never recorded pr-open. The latch should NOT
	// synthesize a set from nothing.
	m := &signalMock{
		owner: "o", repo: "r",
		pr: map[string]any{
			"number": 1,
			"state":  "closed",
			"merged": false,
		},
	}
	srv := m.server()
	defer srv.Close()
	b := newSignalBackend(t, m, srv)

	state := newState()
	// pr-open was never set.
	if err := b.refreshPRSignals(t.Context(), 42, state); err != nil {
		t.Fatalf("refreshPRSignals: %v", err)
	}
	if state.SignalSet("pr-open") {
		t.Fatal("pr-open should stay false — latch must not synthesize a set from nothing")
	}
}
