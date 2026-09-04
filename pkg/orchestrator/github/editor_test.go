package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/promise-language/flow"
)

// ItemEditor is a transaction: fields are staged on it and land together, so a
// caller never has to ask which half of an edit succeeded. These cover the
// refusals — the paths that decide whether "all of them, or none of them"
// actually holds.

// ---------------------------------------------------------------------------
// Fields: the lost-update refusal, and markers the orchestrator maintains.
// ---------------------------------------------------------------------------

func editingOrchestrator(t *testing.T, labels ...string) (*ghMock, *Orchestrator) {
	t.Helper()
	mock := newGHMock(t)
	mock.issueLabels = labels
	srv := mock.server()
	t.Cleanup(srv.Close)
	return mock, newMockedOrchestrator(t, mock, srv)
}

// A field this edit is changing that moved since Edit opened is a lost update
// waiting to happen: the caller staged their change against what they READ.
// Opening an edit is not a lock — another writer may land first — and Commit is
// where that is discovered.
func TestEditor_RefusesWhenAFieldItChangesMovedSinceTheEditOpened(t *testing.T) {
	for _, c := range []struct {
		name  string
		stage func(flow.ItemEditor)
		move  func(*ghMock)
		want  string
	}{{
		name:  "title",
		stage: func(ed flow.ItemEditor) { ed.SetTitle("mine") },
		move:  func(m *ghMock) { m.issueTitle = "somebody else's" },
		want:  "title",
	}, {
		name:  "body",
		stage: func(ed flow.ItemEditor) { ed.SetBody("mine") },
		move:  func(m *ghMock) { m.issueBody = "somebody else's" },
		want:  "body",
	}, {
		// A label the edit TOUCHES. Adding one that somebody else already added
		// in the meantime is a different item state than the caller staged
		// against.
		name:  "a label the edit touches",
		stage: func(ed flow.ItemEditor) { ed.AddTag("priority:high") },
		move:  func(m *ghMock) { m.issueLabels = append(m.issueLabels, "priority:high") },
		want:  "priority:high",
	}} {
		t.Run(c.name, func(t *testing.T) {
			mock, b := editingOrchestrator(t, "flow:implement")
			ed, err := b.Edit(t.Context(), b.refFromIssue(42))
			if err != nil {
				t.Fatalf("Edit: %v", err)
			}
			c.stage(ed)

			mock.mu.Lock()
			c.move(mock)
			titleAfterMove, bodyAfterMove := mock.issueTitle, mock.issueBody
			labelsAfterMove := append([]string(nil), mock.issueLabels...)
			mock.mu.Unlock()

			err = ed.Commit(t.Context())
			if err == nil {
				t.Fatal("Commit overwrote a field that had moved since the edit opened")
			}
			// ErrUnavailable, not ErrUnsupported: re-reading and re-staging is
			// exactly what a caller should do.
			if !errors.Is(err, flow.ErrUnavailable) {
				t.Errorf("error = %v, want ErrUnavailable — re-reading resolves it", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to name what moved (%q)", err, c.want)
			}
			// And the other writer's value survives untouched.
			mock.mu.Lock()
			defer mock.mu.Unlock()
			if mock.issueTitle != titleAfterMove || mock.issueBody != bodyAfterMove {
				t.Errorf("the refused edit wrote anyway: title=%q body=%q", mock.issueTitle, mock.issueBody)
			}
			if len(mock.issueLabels) != len(labelsAfterMove) {
				t.Errorf("labels = %v, want them unchanged at %v", mock.issueLabels, labelsAfterMove)
			}
		})
	}
}

// A label the edit does NOT touch may move freely — that is exactly what
// applying deltas to the current set is for. Refusing over it would make every
// edit fail on a repository where anything else labels issues.
func TestEditor_ALabelTheEditDoesNotTouchMayMove(t *testing.T) {
	mock, b := editingOrchestrator(t, "flow:implement")
	ed, err := b.Edit(t.Context(), b.refFromIssue(42))
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	ed.AddTag("priority:high")

	mock.mu.Lock()
	mock.issueLabels = append(mock.issueLabels, "area:api") // somebody else's classification
	mock.mu.Unlock()

	if err := ed.Commit(t.Context()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got := mock.labelNames()
	for _, want := range []string{"flow:implement", "area:api", "priority:high"} {
		if !contains(got, want) {
			t.Errorf("labels = %v, want %q kept — the delta applies to what is actually there", got, want)
		}
	}
}

// AN ORCHESTRATOR MUST REFUSE TO REMOVE A MARKER IT MAINTAINS ITSELF. The
// owner, binary, seeded, park and manual markers follow from Claim, seeding,
// Park and this editor; a caller able to delete one directly could make an item
// report a state no operation put it in.
func TestEditor_RefusesToRemoveAMarkerItMaintains(t *testing.T) {
	for _, marker := range []string{
		"flow:owner:alice",
		"flow:seeded",
		"flow:needs-answer",
		"flow:blocked",
		"flow:manual",
		"flow:budget-exhausted:plan",
		"flow:stale:plan",
		"flow:implement", // the binary marker seeding maintains
	} {
		t.Run(marker, func(t *testing.T) {
			mock, b := editingOrchestrator(t, "flow:implement", marker)
			ed, err := b.Edit(t.Context(), b.refFromIssue(42))
			if err != nil {
				t.Fatalf("Edit: %v", err)
			}
			ed.RemoveTag(flow.TagId(marker))
			if err := ed.Commit(t.Context()); err == nil {
				t.Fatalf("Commit removed %q, a marker that follows from an operation", marker)
			}
			if !contains(mock.labelNames(), marker) {
				t.Errorf("labels = %v, want %q still present", mock.labelNames(), marker)
			}
		})
	}

	// The counterpart: an operator's own classification IS removable, so the
	// refusal narrows to markers rather than to editing tags at all.
	mock, b := editingOrchestrator(t, "flow:implement", "area:api")
	ed, err := b.Edit(t.Context(), b.refFromIssue(42))
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	ed.RemoveTag("area:api")
	if err := ed.Commit(t.Context()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if contains(mock.labelNames(), "area:api") {
		t.Errorf("labels = %v, want the operator's own tag removed", mock.labelNames())
	}
}

// A tag below the floor is refused rather than written: it is a label name, and
// the same value is interpolated into this orchestrator's own search query.
func TestEditor_RefusesATagBelowTheFloor(t *testing.T) {
	for _, tag := range []flow.TagId{"", "  ", " leading", "trailing ", "two\nlines"} {
		t.Run(fmt.Sprintf("%q", string(tag)), func(t *testing.T) {
			mock, b := editingOrchestrator(t, "flow:implement")
			ed, err := b.Edit(t.Context(), b.refFromIssue(42))
			if err != nil {
				t.Fatalf("Edit: %v", err)
			}
			ed.AddTag(tag)
			if err := ed.Commit(t.Context()); err == nil {
				t.Fatalf("Commit accepted %q as a tag", string(tag))
			}
			if len(mock.labelNames()) != 1 {
				t.Errorf("labels = %v, want them untouched", mock.labelNames())
			}
		})
	}
}

// An editor with nothing staged writes nothing. A no-op Commit that PATCHed
// anyway would be an outward write — visible to everyone who can see the item —
// made by a caller that asked for no change.
func TestEditor_AnEmptyCommitWritesNothing(t *testing.T) {
	mock, b := editingOrchestrator(t, "flow:implement")
	ed, err := b.Edit(t.Context(), b.refFromIssue(42))
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if err := ed.Commit(t.Context()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.mutations) != 0 {
		t.Errorf("an empty edit reached the network: %v", mock.mutations)
	}
}

// Setting manual stops anything dispatching the item underneath the person now
// driving it, AND RESOLVES ANY UNRESOLVED PARK — the operator's `run-step` IS
// the resume. Load must report the flag truthfully, because a write nothing can
// observe is not a record.
func TestEditor_SetManualMarksTheItemAndResolvesItsPark(t *testing.T) {
	_, b, claim := newQuestionEnv(t)
	parkOnQuestion(t, b, claim)

	before, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !before.Parked() {
		t.Fatal("the item is not parked, so this test would prove nothing")
	}
	if before.Manual {
		t.Fatal("the item already reads manual")
	}

	ed, err := b.Edit(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	ed.SetManual(true)
	if err := ed.Commit(t.Context()); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	after, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !after.Manual {
		t.Error("Load does not report the item as manual — a write nothing can observe is not a record")
	}
	if after.Parked() {
		t.Errorf("the park survived the takeover: %+v — the operator's run-step IS the resume", after.Park)
	}

	// And clearing it hands the item back: one that could be taken over and
	// never returned would be stranded by the act of helping it.
	ed, err = b.Edit(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	ed.SetManual(false)
	if err := ed.Commit(t.Context()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	back, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if back.Manual {
		t.Error("the item still reads manual after the flag was cleared")
	}
}

// ---------------------------------------------------------------------------
// Blockers: refusing a reference that names nothing, and refusing a cycle.
// ---------------------------------------------------------------------------

// blockerRepo is a tiny issue graph: which issues exist, and which each is
// declared to wait on. Enough for the editor's resolve-and-walk to be exercised
// against something with real shape.
type blockerRepo struct {
	mu    sync.Mutex
	open  map[int]bool  // number → exists (all open)
	deps  map[int][]int // number → the numbers it waits on
	added [][2]int      // (issue, blocker global id) actually recorded
}

func newBlockerOrchestrator(t *testing.T, repo *blockerRepo) *Orchestrator {
	t.Helper()
	mock := newGHMock(t)
	mux := http.NewServeMux()
	prefix := fmt.Sprintf("/repos/%s/%s", mock.owner, mock.repo)

	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"name": mock.repo, "full_name": mock.owner + "/" + mock.repo, "permissions": mock.perms})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"login": "alice"})
	})
	mux.HandleFunc(prefix+"/issues/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, prefix+"/issues/")
		parts := strings.SplitN(rest, "/", 2)
		n, err := strconv.Atoi(parts[0])
		if err != nil {
			http.NotFound(w, r)
			return
		}
		repo.mu.Lock()
		defer repo.mu.Unlock()
		if !repo.open[n] {
			http.NotFound(w, r) // an identifier naming nothing
			return
		}
		if len(parts) == 1 {
			writeJSON(w, map[string]any{
				"number": n, "id": int64(n) * 1000, "state": "open",
				"title": fmt.Sprintf("Issue %d", n), "labels": toLabelObjs(nil),
				"html_url": fmt.Sprintf("https://github.com/o/r/issues/%d", n),
			})
			return
		}
		if parts[1] != "dependencies/blocked_by" {
			writeJSON(w, []any{})
			return
		}
		if r.Method == http.MethodPost {
			var doc struct {
				IssueID int64 `json:"issue_id"`
			}
			_ = decodeJSON(r, &doc)
			repo.added = append(repo.added, [2]int{n, int(doc.IssueID)})
			writeJSON(w, map[string]any{})
			return
		}
		out := []map[string]any{}
		for _, d := range repo.deps[n] {
			out = append(out, map[string]any{
				"number": d, "id": int64(d) * 1000, "state": "open", "title": fmt.Sprintf("Issue %d", d),
			})
		}
		writeJSON(w, out)
	})

	srv := startMockServer(t, mock, mux)
	t.Cleanup(srv.Close)
	return newMockedOrchestrator(t, mock, srv)
}

// An orchestrator MUST refuse a blocker it cannot resolve: an identifier naming
// nothing is a typo, and accepting it blocks the item forever on something that
// does not exist.
func TestEditor_RefusesABlockerThatNamesNothing(t *testing.T) {
	repo := &blockerRepo{open: map[int]bool{42: true}, deps: map[int][]int{}}
	b := newBlockerOrchestrator(t, repo)

	ed, err := b.Edit(t.Context(), b.refFromIssue(42))
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	ed.AddBlocker(b.refFromIssue(9999))
	err = ed.Commit(t.Context())
	if err == nil {
		t.Fatal("Commit recorded a dependency on an issue that does not exist")
	}
	if !strings.Contains(err.Error(), "9999") {
		t.Errorf("error = %q, want it to name the blocker that could not be resolved", err)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.added) != 0 {
		t.Errorf("a dependency was recorded anyway: %v", repo.added)
	}
}

// A cycle is refused, self-reference included. A ring leaves every item in it
// blocked — each one correctly, with no blocker ever finishing — and nothing
// reports it, because every individual item is in a defensible state.
func TestEditor_RefusesACycle(t *testing.T) {
	for _, c := range []struct {
		name    string
		deps    map[int][]int
		blocker int
		mustSay string
	}{{
		name: "self-reference", deps: map[int][]int{}, blocker: 42,
		mustSay: "own blocker",
	}, {
		// 43 already waits on 42, so 42 waiting on 43 closes the ring.
		name: "two items", deps: map[int][]int{43: {42}}, blocker: 43,
		mustSay: "cycle",
	}, {
		// The walk has to follow more than one edge: 43 → 44 → 42.
		name: "through a third", deps: map[int][]int{43: {44}, 44: {42}}, blocker: 43,
		mustSay: "cycle",
	}} {
		t.Run(c.name, func(t *testing.T) {
			repo := &blockerRepo{open: map[int]bool{42: true, 43: true, 44: true}, deps: c.deps}
			b := newBlockerOrchestrator(t, repo)

			ed, err := b.Edit(t.Context(), b.refFromIssue(42))
			if err != nil {
				t.Fatalf("Edit: %v", err)
			}
			ed.AddBlocker(b.refFromIssue(c.blocker))
			err = ed.Commit(t.Context())
			if err == nil {
				t.Fatal("Commit closed a dependency cycle")
			}
			if !strings.Contains(err.Error(), c.mustSay) {
				t.Errorf("error = %q, want it to say %q", err, c.mustSay)
			}
			repo.mu.Lock()
			defer repo.mu.Unlock()
			if len(repo.added) != 0 {
				t.Errorf("the cycle was recorded anyway: %v", repo.added)
			}
		})
	}
}

// The control: a blocker that closes no ring IS recorded, so the refusals above
// narrow to cycles rather than to declaring dependencies at all. And the walk
// terminates over a ring ALREADY present in the store — bounded by `seen`, so
// refusing a new edge does not hang on an old one.
func TestEditor_RecordsAnAcyclicBlockerAndSurvivesARingAlreadyThere(t *testing.T) {
	repo := &blockerRepo{
		open: map[int]bool{42: true, 43: true, 44: true},
		// 43 ↔ 44 is a ring that predates this edit. Neither reaches 42.
		deps: map[int][]int{43: {44}, 44: {43}},
	}
	b := newBlockerOrchestrator(t, repo)

	ed, err := b.Edit(t.Context(), b.refFromIssue(42))
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	ed.AddBlocker(b.refFromIssue(43))
	if err := ed.Commit(t.Context()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.added) != 1 || repo.added[0] != [2]int{42, 43000} {
		t.Errorf("recorded %v, want #43 as a blocker of #42 by its global id", repo.added)
	}
}

// decodeJSON reads a request body into v. Small enough to keep here rather than
// widen the shared mock's surface for one caller.
func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}
