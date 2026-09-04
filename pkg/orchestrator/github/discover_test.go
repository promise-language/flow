package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	b := newMockedOrchestrator(t, mock, srv)

	ref, err := b.ResolveRef(t.Context(), "42")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if ref.Display != "o/r#42" {
		t.Errorf("Display = %q, want %q", ref.Display, "o/r#42")
	}
	if ref.OrchestratorName != "github" {
		t.Errorf("OrchestratorName = %q, want github", ref.OrchestratorName)
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
	b := newMockedOrchestrator(t, mock, srv)

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
	b := newMockedOrchestrator(t, mock, srv)

	ctx := t.Context()

	acceptsTask := func(t flow.ItemType) bool { return t == "task" }

	// ScopeOpen: both issues visible.
	items, err := b.List(ctx, flow.ScopeOpen, "implement", acceptsTask)
	if err != nil {
		t.Fatalf("List(open): %v", err)
	}
	if len(items) != 2 {
		t.Errorf("List(open) returned %d items, want 2", len(items))
	}

	// ScopeProcessable: #42 has type:task (accepted by acceptsTask). #43 has
	// no type:* label and DefaultType is "" → type "" is not accepted → unhandled.
	items, err = b.List(ctx, flow.ScopeProcessable, "implement", acceptsTask)
	if err != nil {
		t.Fatalf("List(processable): %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List(processable) returned %d items, want 1", len(items))
	}
	if items[0].Ref.Display != "o/r#42" {
		t.Errorf("item.Ref.Display = %q, want o/r#42", items[0].Ref.Display)
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
	b := newMockedOrchestrator(t, mock, srv)

	acceptsTask := func(t flow.ItemType) bool { return t == "task" }

	items, err := b.List(t.Context(), flow.ScopeProcessable, "implement", acceptsTask)
	if err != nil {
		t.Fatalf("List(processable): %v", err)
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
	b := newMockedOrchestrator(t, mock, srv)
	b.cfg.DefaultType = "task"

	acceptsTask := func(t flow.ItemType) bool { return t == "task" }

	items, err := b.List(t.Context(), flow.ScopeProcessable, "implement", acceptsTask)
	if err != nil {
		t.Fatalf("List: %v", err)
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
	b := newMockedOrchestrator(t, mock, srv)

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
			iss := &gh.Issue{State: &tt.state, Assignees: ghUsers(tt.assignees)}
			blocked, _, _ := b.blockedness(nil, tt.labels)
			got, err := b.availabilityOf(t.Context(), iss, tt.labels, tt.itemType,
				blocked, flow.BinaryName(tt.binaryName), tt.acceptsType)
			if err != nil {
				t.Fatalf("availabilityOf: %v", err)
			}
			if got != tt.want {
				t.Errorf("availabilityOf() = %q, want %q", got, tt.want)
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
	b := newMockedOrchestrator(t, mock, srv)

	acceptsAll := func(flow.ItemType) bool { return true }
	items, err := b.List(t.Context(), flow.ScopeOpen, "implement", acceptsAll)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1 (PR should be filtered out)", len(items))
	}
	if items[0].Ref.Display != "o/r#42" {
		t.Errorf("remaining item = %q, want o/r#42", items[0].Ref.Display)
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
	b := newMockedOrchestrator(t, mock, srv)

	acceptsAll := func(flow.ItemType) bool { return true }
	items, err := b.List(t.Context(), flow.ScopeOpen, "implement", acceptsAll)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Holder.Account != "bob" {
		t.Errorf("Holder = %q, want %q", items[0].Holder.Account, "bob")
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
	b := newMockedOrchestrator(t, mock, srv)

	acceptsAll := func(flow.ItemType) bool { return true }
	items, err := b.List(t.Context(), flow.ScopeProcessable, "implement", acceptsAll)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Availability != flow.AvailBlocked {
		t.Errorf("Availability = %q, want %q", items[0].Availability, flow.AvailBlocked)
	}
	if items[0].BlockReason != "waiting for an answer" {
		t.Errorf("BlockReason = %q, want %q", items[0].BlockReason, "waiting for an answer")
	}
}

// TestBackend_BlockednessReason verifies each blocking label produces a
// distinct reason naming the KIND of block and never an item.
func TestBackend_BlockednessReason(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)

	tests := []struct {
		labels []string
		want   string
	}{
		{[]string{"flow:implement", "flow:blocked"}, "blocked pending operator action"},
		{[]string{"flow:implement", "flow:disabled"}, "disabled by an operator"},
		{[]string{"flow:implement", "flow:needs-answer"}, "waiting for an answer"},
		{[]string{"flow:implement", "flow:budget-exhausted:plan"}, "budget exhausted on a step"},
	}
	for _, tt := range tests {
		blocked, kind, got := b.blockedness(nil, tt.labels)
		if !blocked {
			t.Errorf("blockedness(%v) reported not blocked", tt.labels)
		}
		if kind != flow.WaitsOnPerson {
			t.Errorf("blockedness(%v) kind = %q, want %q", tt.labels, kind, flow.WaitsOnPerson)
		}
		if got != tt.want {
			t.Errorf("blockedness(%v) reason = %q, want %q", tt.labels, got, tt.want)
		}
	}
}

// searchingOrchestrator serves a /search/issues that returns issue 42 carrying
// the given labels, and records the query it was asked. Search is the narrowing
// step; what the labels say is the comparison.
func searchingOrchestrator(t *testing.T, mock *ghMock, labels []string, query *string) *Orchestrator {
	t.Helper()
	mux := http.NewServeMux()
	prefix := fmt.Sprintf("/repos/%s/%s", mock.owner, mock.repo)
	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"name": mock.repo, "full_name": mock.owner + "/" + mock.repo, "permissions": mock.perms,
		})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"login": "alice"})
	})
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		*query = r.URL.Query().Get("q")
		writeJSON(w, map[string]any{
			"total_count": 1,
			"items": []map[string]any{
				{"number": 42, "title": "A task", "state": "open", "labels": toLabelObjs(labels)},
			},
		})
	})
	srv := startMockServer(t, mock, mux)
	t.Cleanup(srv.Close)
	return newMockedOrchestrator(t, mock, srv)
}

// The exact match is the contract, and the search query is only a narrowing
// step. Search is case-insensitive and index-lagged, so an item it returns for
// "Priority:High" has not been shown to carry "priority:high" — and one --tag
// value meaning two different things across `list` and `resolve` is the defect
// TagsMatch exists to close.
func TestBackend_ListAutoSelectable_TagsMatchExactly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
		want   int
	}{
		{"the tag, exactly", []string{"flow:implement", "priority:high"}, 1},
		{"a case-differing tag search returned anyway", []string{"flow:implement", "Priority:High"}, 0},
		{"no such tag at all", []string{"flow:implement"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var query string
			b := searchingOrchestrator(t, newGHMock(t), tc.labels, &query)

			refs, err := b.ListAutoSelectable(t.Context(), []flow.TagId{"priority:high"})
			if err != nil {
				t.Fatalf("ListAutoSelectable: %v", err)
			}
			if len(refs) != tc.want {
				t.Fatalf("got %d refs, want %d (labels %v)", len(refs), tc.want, tc.labels)
			}
			if tc.want == 1 && refs[0].Display != "o/r#42" {
				t.Errorf("Display = %q, want o/r#42", refs[0].Display)
			}
		})
	}
}

// Every tag term reaches the query QUOTED. Concatenated bare, a tag carrying a
// space does not fail — it silently becomes a different query, and the caller
// gets a plausible wrong answer rather than an error.
func TestBackend_ListAutoSelectable_QuotesTheSearchTerms(t *testing.T) {
	var query string
	b := searchingOrchestrator(t, newGHMock(t), []string{"flow:implement", "priority:high"}, &query)

	if _, err := b.ListAutoSelectable(t.Context(), []flow.TagId{"priority:high"}); err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	if !strings.Contains(query, `label:"priority:high"`) {
		t.Errorf("query = %q, want the tag as a quoted label: term", query)
	}
}

// A tag below the floor is REFUSED, not interpolated — and nothing is searched,
// because a query built from it would have answered about something else.
func TestBackend_ListAutoSelectable_RefusesAnInvalidTag(t *testing.T) {
	for _, tag := range []flow.TagId{"", "  ", " leading", "trailing ", "two\nlines"} {
		t.Run(fmt.Sprintf("%q", string(tag)), func(t *testing.T) {
			var query string
			b := searchingOrchestrator(t, newGHMock(t), []string{"flow:implement"}, &query)

			if _, err := b.ListAutoSelectable(t.Context(), []flow.TagId{tag}); err == nil {
				t.Fatalf("ListAutoSelectable accepted the invalid tag %q", string(tag))
			}
			if query != "" {
				t.Errorf("a search was issued for an invalid tag: %q", query)
			}
		})
	}
}

// MUST NOT return a blocked item. Selection is where the dependency is honoured
// — the SDK does not filter afterwards, because a rule enforced in two places
// is a rule with two owners and one of them wrong.
func TestBackend_ListAutoSelectable_OmitsBlockedItems(t *testing.T) {
	for _, blocking := range []string{"flow:disabled", "flow:needs-answer", "flow:blocked", "flow:budget-exhausted:plan"} {
		t.Run(blocking, func(t *testing.T) {
			var query string
			b := searchingOrchestrator(t, newGHMock(t), []string{"flow:implement", blocking}, &query)

			refs, err := b.ListAutoSelectable(t.Context(), nil)
			if err != nil {
				t.Fatalf("ListAutoSelectable: %v", err)
			}
			if len(refs) != 0 {
				t.Errorf("got %d refs, want none — %s blocks the item", len(refs), blocking)
			}
		})
	}
}
