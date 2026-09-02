package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	gh "github.com/google/go-github/v68/github"
	"github.com/promise-language/flow"
)

// TestBackend_ResolveRef_ValidNumber verifies that ResolveRef constructs an
// ItemRef from a valid issue number without an API call.
func TestBackend_ResolveRef_ValidNumber(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	ref, err := b.ResolveRef(t.Context(), "42")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if ref.Display != "o/r#42" {
		t.Errorf("Display = %q, want %q", ref.Display, "o/r#42")
	}
	if ref.BackendName != "github" {
		t.Errorf("BackendName = %q, want github", ref.BackendName)
	}
	// The ref must be decodable by issueNumber.
	n, err := b.issueNumber(ref)
	if err != nil {
		t.Fatalf("issueNumber: %v", err)
	}
	if n != 42 {
		t.Errorf("issueNumber = %d, want 42", n)
	}
}

// TestBackend_ResolveRef_InvalidInputs tests rejection of non-numeric and
// non-positive inputs.
func TestBackend_ResolveRef_InvalidInputs(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	for _, input := range []string{"abc", "0", "-1", "", "3.14"} {
		_, err := b.ResolveRef(t.Context(), input)
		if err == nil {
			t.Errorf("ResolveRef(%q) = nil error, want rejection", input)
		}
	}
}

// startMockServer creates an httptest.Server from a custom mux, wrapped
// with the mock's mutation recorder.
func startMockServer(t *testing.T, mock *ghMock, mux http.Handler) *httptest.Server {
	t.Helper()
	return httptest.NewServer(mock.recordMutations(mux))
}

// TestBackend_Discover_BasicScopes tests that Discover returns items filtered
// by scope.
func TestBackend_Discover_BasicScopes(t *testing.T) {
	mock := newGHMock(t)
	mock.issueLabels = []string{"flow:implement", "type:task"}
	mock.assignees = []string{"alice"}

	mux := http.NewServeMux()
	prefix := fmt.Sprintf("/repos/%s/%s", mock.owner, mock.repo)

	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"name":        mock.repo,
			"full_name":   mock.owner + "/" + mock.repo,
			"permissions": mock.perms,
		})
	})

	// Issues API endpoint — returns two issues
	mux.HandleFunc(prefix+"/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{
			{
				"number":     42,
				"title":      "Test issue",
				"state":      "open",
				"labels":     toLabelObjs([]string{"flow:implement", "type:task"}),
				"assignees":  toLoginObjs([]string{"alice"}),
				"html_url":   "https://github.com/o/r/issues/42",
				"updated_at": "2025-01-01T00:00:00Z",
			},
			{
				"number":     43,
				"title":      "Unrelated issue",
				"state":      "open",
				"labels":     toLabelObjs([]string{"bug"}),
				"assignees":  toLoginObjs([]string{}),
				"html_url":   "https://github.com/o/r/issues/43",
				"updated_at": "2025-01-01T00:00:00Z",
			},
		})
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"login": "alice"})
	})

	srv := startMockServer(t, mock, mux)
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	ctx := t.Context()

	acceptsTask := func(t flow.ItemType) bool { return t == "task" }

	// ScopeOpen: both issues visible.
	items, err := b.Discover(ctx, flow.ScopeOpen, "implement", acceptsTask)
	if err != nil {
		t.Fatalf("Discover(open): %v", err)
	}
	if len(items) != 2 {
		t.Errorf("Discover(open) returned %d items, want 2", len(items))
	}

	// ScopeProcessable: #42 has type:task (accepted by acceptsTask). #43 has
	// no type:* label and DefaultType is "" → type "" is not accepted → unhandled.
	items, err = b.Discover(ctx, flow.ScopeProcessable, "implement", acceptsTask)
	if err != nil {
		t.Fatalf("Discover(processable): %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("Discover(processable) returned %d items, want 1", len(items))
	}
	if items[0].Display != "o/r#42" {
		t.Errorf("item.Display = %q, want o/r#42", items[0].Display)
	}
	// auto, not available: #42 carries flow:implement AND is assigned to the
	// authenticated user (the mock's /user answers "alice", and the fixture
	// assigns alice), which is exactly ListEligible's `label:flow:<binary>
	// assignee:@me`. resolve would pick this item, so the marker must say so.
	if items[0].Availability != flow.AvailAuto {
		t.Errorf("Availability = %q, want %q", items[0].Availability, flow.AvailAuto)
	}
	// Tags must be ALL labels, not just flow:*.
	if len(items[0].Tags) != 2 {
		t.Errorf("Tags = %v, want [flow:implement type:task]", items[0].Tags)
	}
}

// TestBackend_Discover_UnseededAcceptedType verifies the core fix: an issue
// whose type IS accepted but that has NOT been seeded (no flow:<binary> label)
// appears at processable scope as "available", not filtered out as "unhandled".
// This is the end-to-end version of the scenario the issue describes.
func TestBackend_Discover_UnseededAcceptedType(t *testing.T) {
	mock := newGHMock(t)

	mux := http.NewServeMux()
	prefix := fmt.Sprintf("/repos/%s/%s", mock.owner, mock.repo)

	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"name":        mock.repo,
			"full_name":   mock.owner + "/" + mock.repo,
			"permissions": mock.perms,
		})
	})

	mux.HandleFunc(prefix+"/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{
			{
				"number":     10,
				"title":      "Unseeded task",
				"state":      "open",
				"labels":     toLabelObjs([]string{"type:task"}),
				"assignees":  toLoginObjs([]string{}),
				"html_url":   "https://github.com/o/r/issues/10",
				"updated_at": "2025-01-01T00:00:00Z",
			},
			{
				"number":     11,
				"title":      "Unseeded task assigned to me",
				"state":      "open",
				"labels":     toLabelObjs([]string{"type:task"}),
				"assignees":  toLoginObjs([]string{"alice"}),
				"html_url":   "https://github.com/o/r/issues/11",
				"updated_at": "2025-01-01T00:00:00Z",
			},
		})
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"login": "alice"})
	})

	srv := startMockServer(t, mock, mux)
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	acceptsTask := func(t flow.ItemType) bool { return t == "task" }

	items, err := b.Discover(t.Context(), flow.ScopeProcessable, "implement", acceptsTask)
	if err != nil {
		t.Fatalf("Discover(processable): %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 — unseeded items with accepted type must appear", len(items))
	}

	// #10: no binary label, not assigned → available.
	if items[0].Availability != flow.AvailAvailable {
		t.Errorf("#10 Availability = %q, want %q", items[0].Availability, flow.AvailAvailable)
	}

	// #11: no binary label, assigned to me → still available, NOT auto.
	// Auto requires the binary label (applied during seeding).
	if items[1].Availability != flow.AvailAvailable {
		t.Errorf("#11 Availability = %q, want %q — unseeded+assigned must not be auto",
			items[1].Availability, flow.AvailAvailable)
	}
}

// TestBackend_Discover_DefaultTypeUnseeded verifies that when DefaultType is
// configured, an issue with no type:* label derives its type from the default
// and is available (not unhandled) if acceptsType matches.
func TestBackend_Discover_DefaultTypeUnseeded(t *testing.T) {
	mock := newGHMock(t)

	mux := http.NewServeMux()
	prefix := fmt.Sprintf("/repos/%s/%s", mock.owner, mock.repo)

	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"name":        mock.repo,
			"full_name":   mock.owner + "/" + mock.repo,
			"permissions": mock.perms,
		})
	})

	mux.HandleFunc(prefix+"/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{
			{
				"number":     20,
				"title":      "No type label",
				"state":      "open",
				"labels":     toLabelObjs([]string{}),
				"assignees":  toLoginObjs([]string{}),
				"html_url":   "https://github.com/o/r/issues/20",
				"updated_at": "2025-01-01T00:00:00Z",
			},
		})
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"login": "alice"})
	})

	srv := startMockServer(t, mock, mux)
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)
	b.cfg.DefaultType = "task"

	acceptsTask := func(t flow.ItemType) bool { return t == "task" }

	items, err := b.Discover(t.Context(), flow.ScopeProcessable, "implement", acceptsTask)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1 — DefaultType should make the item available", len(items))
	}
	if items[0].Availability != flow.AvailAvailable {
		t.Errorf("Availability = %q, want %q", items[0].Availability, flow.AvailAvailable)
	}
}

// TestBackend_Discover_AvailabilityStates verifies deriveAvailability.
func TestBackend_Discover_AvailabilityStates(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	acceptsAll := func(flow.ItemType) bool { return true }
	acceptsNone := func(flow.ItemType) bool { return false }

	tests := []struct {
		name        string
		labels      []string
		assignees   []string
		state       string
		myLogin     string
		binaryName  string
		acceptsType func(flow.ItemType) bool
		itemType    flow.ItemType
		want        flow.Availability
	}{
		{"closed issue", []string{"flow:implement"}, nil, "closed", "alice", "implement", acceptsAll, "task", flow.AvailClosed},
		{"type not accepted = unhandled", []string{"bug"}, nil, "open", "alice", "implement", acceptsNone, "bug", flow.AvailUnhandled},
		{"type accepted, no binary label = available", []string{"bug"}, nil, "open", "alice", "implement", acceptsAll, "bug", flow.AvailAvailable},
		{"type accepted, no binary label, assigned = available (not auto)", []string{"bug"}, []string{"alice"}, "open", "alice", "implement", acceptsAll, "bug", flow.AvailAvailable},
		{"blocked label", []string{"flow:implement", "flow:blocked"}, nil, "open", "alice", "implement", acceptsAll, "task", flow.AvailBlocked},
		{"disabled label", []string{"flow:implement", "flow:disabled"}, nil, "open", "alice", "implement", acceptsAll, "task", flow.AvailBlocked},
		{"needs-answer label", []string{"flow:implement", "flow:needs-answer"}, nil, "open", "alice", "implement", acceptsAll, "task", flow.AvailBlocked},
		{"budget-exhausted", []string{"flow:implement", "flow:budget-exhausted:plan"}, nil, "open", "alice", "implement", acceptsAll, "task", flow.AvailBlocked},
		{"held by another", []string{"flow:implement", "flow:owner:bob"}, []string{"bob"}, "open", "alice", "implement", acceptsAll, "task", flow.AvailHeld},
		{"available — not assigned", []string{"flow:implement"}, nil, "open", "alice", "implement", acceptsAll, "task", flow.AvailAvailable},
		{"auto — assigned, binary label present", []string{"flow:implement"}, []string{"alice"}, "open", "alice", "implement", acceptsAll, "task", flow.AvailAuto},
		{"auto — owned+assigned", []string{"flow:implement", "flow:owner:alice"}, []string{"alice"}, "open", "alice", "implement", acceptsAll, "task", flow.AvailAuto},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := b.deriveAvailability(
				tt.labels,
				ghUsers(tt.assignees),
				tt.binaryName,
				tt.myLogin,
				tt.state,
				tt.acceptsType,
				tt.itemType,
			)
			if got != tt.want {
				t.Errorf("deriveAvailability() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ghUsers builds []*github.User from logins, for test convenience.
func ghUsers(logins []string) []*gh.User {
	out := make([]*gh.User, 0, len(logins))
	for _, l := range logins {
		login := l // capture
		out = append(out, &gh.User{Login: &login})
	}
	return out
}

// TestBackend_Discover_ItemRefConversion verifies DiscoveryItem.ItemRef().
func TestBackend_Discover_ItemRefConversion(t *testing.T) {
	di := flow.DiscoveryItem{
		BackendName: "github",
		Display:     "o/r#42",
		Ref:         json.RawMessage(`{"issue":42}`),
	}
	ref := di.ItemRef()
	if ref.Display != "o/r#42" || ref.BackendName != "github" {
		t.Errorf("ItemRef() = %+v, unexpected", ref)
	}
}

// TestBackend_Discover_SkipsPullRequests verifies that the Issues API's
// inclusion of pull requests is filtered out by Discover.
func TestBackend_Discover_SkipsPullRequests(t *testing.T) {
	mock := newGHMock(t)

	mux := http.NewServeMux()
	prefix := fmt.Sprintf("/repos/%s/%s", mock.owner, mock.repo)

	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"name":        mock.repo,
			"full_name":   mock.owner + "/" + mock.repo,
			"permissions": mock.perms,
		})
	})

	mux.HandleFunc(prefix+"/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{
			{
				"number":     42,
				"title":      "Real issue",
				"state":      "open",
				"labels":     toLabelObjs([]string{"flow:implement"}),
				"assignees":  toLoginObjs([]string{}),
				"html_url":   "https://github.com/o/r/issues/42",
				"updated_at": "2025-01-01T00:00:00Z",
			},
			{
				"number":       99,
				"title":        "A pull request",
				"state":        "open",
				"labels":       toLabelObjs([]string{"flow:implement"}),
				"assignees":    toLoginObjs([]string{}),
				"html_url":     "https://github.com/o/r/pull/99",
				"updated_at":   "2025-01-01T00:00:00Z",
				"pull_request": map[string]any{"url": "https://api.github.com/repos/o/r/pulls/99"},
			},
		})
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"login": "alice"})
	})

	srv := startMockServer(t, mock, mux)
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	acceptsAll := func(flow.ItemType) bool { return true }
	items, err := b.Discover(t.Context(), flow.ScopeOpen, "implement", acceptsAll)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1 (PR should be filtered out)", len(items))
	}
	if items[0].Display != "o/r#42" {
		t.Errorf("remaining item = %q, want o/r#42", items[0].Display)
	}
}

// TestBackend_Discover_HolderFromOwnerLabel verifies that Holder is populated
// from the flow:owner:<login> label.
func TestBackend_Discover_HolderFromOwnerLabel(t *testing.T) {
	mock := newGHMock(t)

	mux := http.NewServeMux()
	prefix := fmt.Sprintf("/repos/%s/%s", mock.owner, mock.repo)

	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"name":        mock.repo,
			"full_name":   mock.owner + "/" + mock.repo,
			"permissions": mock.perms,
		})
	})

	mux.HandleFunc(prefix+"/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{
			{
				"number":     42,
				"title":      "Held issue",
				"state":      "open",
				"labels":     toLabelObjs([]string{"flow:implement", "flow:owner:bob"}),
				"assignees":  toLoginObjs([]string{"bob"}),
				"html_url":   "https://github.com/o/r/issues/42",
				"updated_at": "2025-01-01T00:00:00Z",
			},
		})
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"login": "alice"})
	})

	srv := startMockServer(t, mock, mux)
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	acceptsAll := func(flow.ItemType) bool { return true }
	items, err := b.Discover(t.Context(), flow.ScopeOpen, "implement", acceptsAll)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Holder != "bob" {
		t.Errorf("Holder = %q, want %q", items[0].Holder, "bob")
	}
	if items[0].Availability != flow.AvailHeld {
		t.Errorf("Availability = %q, want %q", items[0].Availability, flow.AvailHeld)
	}
}

// TestBackend_Discover_BlockReason verifies that blocked items carry a reason.
func TestBackend_Discover_BlockReason(t *testing.T) {
	mock := newGHMock(t)

	mux := http.NewServeMux()
	prefix := fmt.Sprintf("/repos/%s/%s", mock.owner, mock.repo)

	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"name":        mock.repo,
			"full_name":   mock.owner + "/" + mock.repo,
			"permissions": mock.perms,
		})
	})

	mux.HandleFunc(prefix+"/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{
			{
				"number":     42,
				"title":      "Blocked issue",
				"state":      "open",
				"labels":     toLabelObjs([]string{"flow:implement", "flow:needs-answer"}),
				"assignees":  toLoginObjs([]string{}),
				"html_url":   "https://github.com/o/r/issues/42",
				"updated_at": "2025-01-01T00:00:00Z",
			},
		})
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"login": "alice"})
	})

	srv := startMockServer(t, mock, mux)
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	acceptsAll := func(flow.ItemType) bool { return true }
	items, err := b.Discover(t.Context(), flow.ScopeProcessable, "implement", acceptsAll)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Availability != flow.AvailBlocked {
		t.Errorf("Availability = %q, want %q", items[0].Availability, flow.AvailBlocked)
	}
	if items[0].BlockReason != "needs answer" {
		t.Errorf("BlockReason = %q, want %q", items[0].BlockReason, "needs answer")
	}
}

// TestBackend_DeriveBlockReason verifies each blocking label produces a
// distinct reason string.
func TestBackend_DeriveBlockReason(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	tests := []struct {
		labels []string
		want   string
	}{
		{[]string{"flow:implement", "flow:blocked"}, "blocked"},
		{[]string{"flow:implement", "flow:disabled"}, "disabled"},
		{[]string{"flow:implement", "flow:needs-answer"}, "needs answer"},
		{[]string{"flow:implement", "flow:budget-exhausted:plan"}, `budget exhausted on "plan"`},
	}
	for _, tt := range tests {
		got := b.deriveBlockReason(tt.labels)
		if got != tt.want {
			t.Errorf("deriveBlockReason(%v) = %q, want %q", tt.labels, got, tt.want)
		}
	}
}

// TestBackend_ListEligibleWithTags verifies the tag-filtered search.
func TestBackend_ListEligibleWithTags(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	refs, err := b.ListEligibleWithTags(t.Context(), []string{"priority:high"})
	if err != nil {
		t.Fatalf("ListEligibleWithTags: %v", err)
	}
	// The mock's /search/issues always returns issue 42.
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1", len(refs))
	}
	if refs[0].Display != "o/r#42" {
		t.Errorf("Display = %q, want o/r#42", refs[0].Display)
	}
}
