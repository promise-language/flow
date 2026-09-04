package github

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// Item dependencies: the half of blockedness that comes from OTHER ITEMS
// rather than from a label on this one.
//
// Discover used to set a block reason and deliberately leave BlockedBy empty,
// citing a REST limitation that had since gone away. The endpoint answers, so
// every read now carries the declared blockers with their own statuses, and
// blocked-for-dependency is DERIVED from them on every read rather than stored:
// the item whose last blocker finishes is workable at the next read, with
// nobody having acted.

// dependingOrchestrator serves issue 42 plus the dependency endpoint, and a
// search that returns 42 — enough for Get, Load and ListAutoSelectable to be
// asked about the same item.
//
// deps is what GET .../dependencies/blocked_by answers; status is the HTTP
// status it answers with (200 unless a test is exercising an unavailable
// feature).
func dependingOrchestrator(t *testing.T, labels []string, status int, deps []map[string]any) (*ghMock, *Orchestrator) {
	t.Helper()
	mock := newGHMock(t)
	mock.issueLabels = labels
	mock.assignees = []string{"alice"}

	mux := http.NewServeMux()
	prefix := fmt.Sprintf("/repos/%s/%s", mock.owner, mock.repo)
	issue := func() map[string]any {
		return map[string]any{
			"number": 42, "title": "A task", "state": "open",
			"labels":     toLabelObjs(labels),
			"assignees":  toLoginObjs(mock.assignees),
			"html_url":   "https://github.com/o/r/issues/42",
			"updated_at": "2026-01-01T00:00:00Z",
		}
	}
	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"name": mock.repo, "full_name": mock.owner + "/" + mock.repo, "permissions": mock.perms})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"login": "alice"})
	})
	mux.HandleFunc(prefix+"/issues/42/dependencies/blocked_by", func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, deps)
	})
	mux.HandleFunc(prefix+"/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{issue()})
	})
	mux.HandleFunc(prefix+"/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{}) // no state comment: these tests are about the item's own fields
	})
	mux.HandleFunc(prefix+"/issues/42", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, issue())
	})
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"total_count": 1, "items": []map[string]any{issue()}})
	})
	srv := startMockServer(t, mock, mux)
	t.Cleanup(srv.Close)
	return mock, newMockedOrchestrator(t, mock, srv)
}

func openBlocker(n int) map[string]any {
	return map[string]any{"number": n, "id": int64(n) * 1000, "state": "open", "title": fmt.Sprintf("Issue %d", n)}
}

func finishedBlocker(n int) map[string]any {
	return map[string]any{"number": n, "id": int64(n) * 1000, "state": "closed", "title": fmt.Sprintf("Issue %d", n)}
}

// An unfinished dependency blocks the item, and the block is waits-on-items:
// the one kind a caller can go act on ELSEWHERE, which is why it outranks the
// label causes.
func TestBackend_AnUnfinishedDependencyBlocksTheItem(t *testing.T) {
	_, b := dependingOrchestrator(t, []string{"flow:implement"}, http.StatusOK,
		[]map[string]any{finishedBlocker(7), openBlocker(8)})

	info, err := b.Get(t.Context(), b.refFromIssue(42), "implement", func(flow.ItemType) bool { return true })
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !info.Blocked {
		t.Fatal("the item reads unblocked while a declared blocker is still open")
	}
	if info.BlockKind != flow.WaitsOnItems {
		t.Errorf("BlockKind = %q, want %q — the actor is whoever can work the blockers", info.BlockKind, flow.WaitsOnItems)
	}
	if info.Availability != flow.AvailBlocked {
		t.Errorf("Availability = %q, want %q", info.Availability, flow.AvailBlocked)
	}
	// EVERY declared blocker travels, each with its own status. A set that
	// quietly dropped the satisfied entry could not be edited — nothing could
	// see what was there to remove.
	if len(info.BlockedBy) != 2 {
		t.Fatalf("BlockedBy = %+v, want both declared blockers", info.BlockedBy)
	}
	byDisplay := map[string]flow.ItemStatus{}
	for _, blk := range info.BlockedBy {
		byDisplay[blk.Ref.Display] = blk.Status
	}
	if got := byDisplay["o/r#7"]; got != flow.StatusTerminal {
		t.Errorf("blocker #7 status = %q, want %q", got, flow.StatusTerminal)
	}
	if got := byDisplay["o/r#8"]; got != flow.StatusOpen {
		t.Errorf("blocker #8 status = %q, want %q", got, flow.StatusOpen)
	}
	// The reason names the KIND and never an item: BlockedBy carries the
	// references, and prose repeating them is a second copy nothing updates
	// when a blocker lands.
	if info.BlockReason == "" {
		t.Error("a blocked item carries no reason for a person")
	}
	for _, d := range []string{"#7", "#8"} {
		if strings.Contains(info.BlockReason, d) {
			t.Errorf("BlockReason = %q names an item; the references are BlockedBy's job", info.BlockReason)
		}
	}
}

// Blocked-for-dependency is DERIVED, never stored. Once every blocker has
// finished the item is workable at the next read, with nobody having acted —
// and the finished blockers stay listed, because retracting one is an edit
// somebody has to make.
func TestBackend_FinishedDependenciesStopBlockingButStayListed(t *testing.T) {
	_, b := dependingOrchestrator(t, []string{"flow:implement"}, http.StatusOK,
		[]map[string]any{finishedBlocker(7), finishedBlocker(8)})

	info, err := b.Get(t.Context(), b.refFromIssue(42), "implement", func(flow.ItemType) bool { return true })
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Blocked {
		t.Errorf("the item still reads blocked with every dependency finished (%q)", info.BlockReason)
	}
	if len(info.BlockedBy) != 2 {
		t.Errorf("BlockedBy = %+v, want the two declared blockers to stay listed", info.BlockedBy)
	}
}

// Load carries the same blockers Get does. The two are different projections of
// one item and a field that read one way through each would make them two
// answers about it.
func TestBackend_LoadCarriesTheDeclaredBlockers(t *testing.T) {
	_, b := dependingOrchestrator(t, []string{"flow:implement"}, http.StatusOK,
		[]map[string]any{openBlocker(8)})

	item, err := b.Load(t.Context(), b.refFromIssue(42))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(item.BlockedBy) != 1 || item.BlockedBy[0].Ref.Display != "o/r#8" {
		t.Fatalf("BlockedBy = %+v, want the declared blocker", item.BlockedBy)
	}
	if item.BlockedBy[0].Status != flow.StatusOpen {
		t.Errorf("blocker status = %q, want %q", item.BlockedBy[0].Status, flow.StatusOpen)
	}
	if item.BlockReason == "" {
		t.Error("Load reports no block reason for an item waiting on an unfinished dependency")
	}
}

// MUST NOT return a blocked item. The orchestrator that knows about the
// dependency is the one that keeps it out of the selectable set; the SDK does
// not filter afterwards, because a rule enforced in two places is a rule with
// two owners and one of them wrong.
//
// The label causes are covered elsewhere; this is the dependency cause, which
// no label advertises and which selection could therefore not have seen.
func TestBackend_ListAutoSelectable_OmitsAnItemWaitingOnAnItem(t *testing.T) {
	_, blocked := dependingOrchestrator(t, []string{"flow:implement"}, http.StatusOK,
		[]map[string]any{openBlocker(8)})
	refs, err := blocked.ListAutoSelectable(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("got %d refs, want none — an unattended resolve would have started on a blocked item", len(refs))
	}

	// The control: the same item with the dependency finished IS selectable, so
	// the exclusion is the blocker's doing and not the helper's.
	_, free := dependingOrchestrator(t, []string{"flow:implement"}, http.StatusOK,
		[]map[string]any{finishedBlocker(8)})
	refs, err = free.ListAutoSelectable(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	if len(refs) != 1 {
		t.Errorf("got %d refs, want 1 — every dependency has finished", len(refs))
	}
}

// A repository where the dependency feature is unavailable answers 404 or 410.
// That is NO BLOCKERS, not an error: an orchestrator with no dependency notion
// reports no blockers and is fully conformant, and failing every listing over
// it would take `list` and `status` down on such a repo.
func TestBackend_AnUnavailableDependencyEndpointReadsAsNoBlockers(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			_, b := dependingOrchestrator(t, []string{"flow:implement"}, status, nil)

			info, err := b.Get(t.Context(), b.refFromIssue(42), "implement", func(flow.ItemType) bool { return true })
			if err != nil {
				t.Fatalf("Get: %v — an absent feature must not fail the read", err)
			}
			if info.Blocked || len(info.BlockedBy) != 0 {
				t.Errorf("blocked=%v blockedBy=%+v, want an unblocked item with no blockers", info.Blocked, info.BlockedBy)
			}
		})
	}
}

// Any other failure IS an error. Reporting a rate-limited or broken dependency
// read as "no blockers" would let selection start on an item it could not see
// the blockers of, which is the failure the exclusion exists to prevent.
func TestBackend_AFailedDependencyReadIsNotNoBlockers(t *testing.T) {
	_, b := dependingOrchestrator(t, []string{"flow:implement"}, http.StatusForbidden, nil)

	if _, err := b.Get(t.Context(), b.refFromIssue(42), "implement", func(flow.ItemType) bool { return true }); err == nil {
		t.Fatal("Get succeeded although the blockers could not be read")
	}
	if _, err := b.ListAutoSelectable(t.Context(), nil); err == nil {
		t.Fatal("ListAutoSelectable returned a set although the blockers could not be read")
	}
}

// Get MUST ANSWER IDENTICALLY TO List for the same item at the same moment —
// one derivation serving both, never two. An item that reads blocked in `list`
// and available in `status` is a contradiction an operator cannot resolve, and
// nothing in the item caused it.
//
// The blocker is what makes this worth asserting rather than assuming: it is
// the one input neither call has locally, so a second derivation is exactly
// where the two would drift.
func TestBackend_GetAnswersIdenticallyToList(t *testing.T) {
	for _, deps := range [][]map[string]any{
		nil,
		{openBlocker(8)},
		{finishedBlocker(7)},
	} {
		_, b := dependingOrchestrator(t, []string{"flow:implement"}, http.StatusOK, deps)
		acceptsAll := func(flow.ItemType) bool { return true }

		listed, err := b.List(t.Context(), flow.ScopeAll, "implement", acceptsAll)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(listed) != 1 {
			t.Fatalf("List returned %d items, want 1", len(listed))
		}
		got, err := b.Get(t.Context(), b.refFromIssue(42), "implement", acceptsAll)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !reflect.DeepEqual(*got, listed[0]) {
			t.Errorf("with blockers %v:\n  Get  = %+v\n  List = %+v\nthe two are one derivation and must not differ",
				deps, *got, listed[0])
		}
	}
}

// Where Item and ItemInfo overlap they mean the same thing and MUST agree. A
// field that read one way through List and another through Load would make the
// two calls into two answers about one item.
func TestBackend_LoadAgreesWithGetWhereTheyOverlap(t *testing.T) {
	_, b := dependingOrchestrator(t, []string{"flow:implement", "area:api"}, http.StatusOK,
		[]map[string]any{openBlocker(8)})

	info, err := b.Get(t.Context(), b.refFromIssue(42), "implement", func(flow.ItemType) bool { return true })
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	item, err := b.Load(t.Context(), b.refFromIssue(42))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if item.Title != info.Title || item.Body != info.Body || item.URL != info.URL {
		t.Errorf("Load(%q/%q/%q) disagrees with Get(%q/%q/%q)",
			item.Title, item.Body, item.URL, info.Title, info.Body, info.URL)
	}
	if item.Type != info.Type || item.Status != info.Status || item.Disposition != info.Disposition {
		t.Errorf("Load(type %q status %q disposition %q) disagrees with Get(type %q status %q disposition %q)",
			item.Type, item.Status, item.Disposition, info.Type, info.Status, info.Disposition)
	}
	if item.Holder != info.Holder || item.Manual != info.Manual {
		t.Errorf("Load(holder %+v manual %v) disagrees with Get(holder %+v manual %v)",
			item.Holder, item.Manual, info.Holder, info.Manual)
	}
	if !reflect.DeepEqual(item.Tags, info.Tags) {
		t.Errorf("Load tags %v disagree with Get tags %v", item.Tags, info.Tags)
	}
	if !reflect.DeepEqual(item.BlockedBy, info.BlockedBy) {
		t.Errorf("Load blockers %+v disagree with Get blockers %+v", item.BlockedBy, info.BlockedBy)
	}
	if item.BlockReason != info.BlockReason {
		t.Errorf("Load reason %q disagrees with Get reason %q", item.BlockReason, info.BlockReason)
	}
}
