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

	// ScopeOpen: both issues visible.
	items, err := b.Discover(ctx, flow.ScopeOpen, "implement")
	if err != nil {
		t.Fatalf("Discover(open): %v", err)
	}
	if len(items) != 2 {
		t.Errorf("Discover(open) returned %d items, want 2", len(items))
	}

	// ScopeProcessable: only #42 has the flow:implement label.
	items, err = b.Discover(ctx, flow.ScopeProcessable, "implement")
	if err != nil {
		t.Fatalf("Discover(processable): %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("Discover(processable) returned %d items, want 1", len(items))
	}
	if items[0].Display != "o/r#42" {
		t.Errorf("item.Display = %q, want o/r#42", items[0].Display)
	}
	if items[0].Availability != flow.AvailAvailable {
		t.Errorf("Availability = %q, want %q", items[0].Availability, flow.AvailAvailable)
	}
	// Tags must be ALL labels, not just flow:*.
	if len(items[0].Tags) != 2 {
		t.Errorf("Tags = %v, want [flow:implement type:task]", items[0].Tags)
	}
}

// TestBackend_Discover_AvailabilityStates verifies deriveAvailability.
func TestBackend_Discover_AvailabilityStates(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	tests := []struct {
		name       string
		labels     []string
		assignees  []string
		state      string
		myLogin    string
		binaryName string
		want       flow.Availability
	}{
		{"closed issue", []string{"flow:implement"}, nil, "closed", "alice", "implement", flow.AvailClosed},
		{"no binary label = unhandled", []string{"bug"}, nil, "open", "alice", "implement", flow.AvailUnhandled},
		{"blocked label", []string{"flow:implement", "flow:blocked"}, nil, "open", "alice", "implement", flow.AvailBlocked},
		{"disabled label", []string{"flow:implement", "flow:disabled"}, nil, "open", "alice", "implement", flow.AvailBlocked},
		{"needs-answer label", []string{"flow:implement", "flow:needs-answer"}, nil, "open", "alice", "implement", flow.AvailBlocked},
		{"budget-exhausted", []string{"flow:implement", "flow:budget-exhausted:plan"}, nil, "open", "alice", "implement", flow.AvailBlocked},
		{"held by another", []string{"flow:implement", "flow:owner:bob"}, []string{"bob"}, "open", "alice", "implement", flow.AvailHeld},
		{"available — not assigned", []string{"flow:implement"}, nil, "open", "alice", "implement", flow.AvailAvailable},
		{"auto — assigned, no owner label", []string{"flow:implement"}, []string{"alice"}, "open", "alice", "implement", flow.AvailAuto},
		{"auto — owned+assigned", []string{"flow:implement", "flow:owner:alice"}, []string{"alice"}, "open", "alice", "implement", flow.AvailAuto},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := b.deriveAvailability(
				tt.labels,
				ghUsers(tt.assignees),
				tt.binaryName,
				tt.myLogin,
				tt.state,
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
