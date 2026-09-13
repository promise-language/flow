package github

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promise-language/flow"

	"github.com/promise-language/flow/pkg/machinecache"
)

// #219: the state document had no compare-and-set. Two concurrent writers
// silently lost each other — "a park recorded by one process and a spend charge
// recorded by another do not merge; the second PATCH wins entirely and the first
// is gone with no error and no trace."

// seededItem claims the item and brings its state document into being, which is
// what every test below needs before there is anything to lose.
func seededItem(t *testing.T) (*Orchestrator, *ghMock, flow.ItemRef) {
	t.Helper()
	b, mock, _ := newSeamBackend(t)
	ref := b.refFromIssue(mock.issueNum)
	if _, err := b.Claim(t.Context(), ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.RecordDispatch(t.Context(), ref, "plan"); err != nil {
		t.Fatalf("RecordDispatch: %v", err)
	}
	return b, mock, ref
}

// The case #219 names. One process charges a spend; another parks the item
// between that process's read and its write. Both changes must survive, because
// both are in-place deltas against whatever document is actually there.
func TestStateWrite_AForeignWriteIsReplayedAndBothLand(t *testing.T) {
	b, mock, ref := seededItem(t)
	ctx := t.Context()

	// The foreign writer lands exactly once, in the window between this
	// process's read of the document and its PATCH of it: the mock signals the
	// read by serving it, and the interception is on the conditional
	// revalidation that follows.
	// ONCE, and the flag is taken before the park runs: the park's own write
	// revalidates too, so a hook that re-entered would recurse for good.
	var mu sync.Mutex
	fired := false
	mock.setBeforeCommentRead(func() {
		mu.Lock()
		if fired {
			mu.Unlock()
			return
		}
		fired = true
		mu.Unlock()
		if err := b.Park(ctx, ref, flow.ParkRequest{
			Kind:   flow.ParkInfraTransient,
			Step:   "plan",
			Reason: "the park a concurrent process recorded",
		}); err != nil {
			t.Errorf("the foreign park failed: %v", err)
		}
	})

	if err := b.AddCost(ctx, ref, "plan", 1.25); err != nil {
		t.Fatalf("AddCost: %v", err)
	}
	mock.setBeforeCommentRead(nil)

	state, err := b.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Park == nil {
		t.Error("the park recorded by the other process is gone — the second write won entirely, which is #219")
	}
	if row := state.Ledger.Row("plan"); row.CostUSD < 1.25 {
		t.Errorf("ledger row for plan = %+v, want the charge this process made", row)
	}
}

// A writer that keeps losing does not spin: past the attempts it answers the
// retryable condition, and it leaves the other writer's document alone rather
// than overwriting it on the way out.
func TestStateWrite_ExhaustedReplayReturnsAConditionAndWritesNothing(t *testing.T) {
	b, mock, ref := seededItem(t)
	ctx := t.Context()

	// A foreign write before EVERY revalidation, so no attempt can ever land.
	var mu sync.Mutex
	writes := 0
	mock.setBeforeCommentRead(func() {
		mu.Lock()
		writes++
		mu.Unlock()
		mock.mu.Lock()
		for i := range mock.comments {
			if stateBeginRe.MatchString(mock.comments[i].Body) {
				mock.comments[i].Body += "\n<!-- a foreign writer was here -->"
			}
		}
		mock.mu.Unlock()
	})
	err := b.AddCost(ctx, ref, "plan", 2)
	mock.setBeforeCommentRead(nil)

	if err == nil {
		t.Fatal("a write that never won returned no error")
	}
	if !errors.Is(err, flow.ErrUnavailable) {
		t.Errorf("error = %v, want flow.ErrUnavailable — losing a race is a condition, not a verdict", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// Two reads per attempt: one to compute the document from, one to compare
	// against immediately before the write. The count is asserted exactly
	// rather than loosely because what this test pins is that a loser STOPS —
	// a bound that drifts is a spin nobody notices.
	if wantReads := 2 * stateWriteAttempts; writes != wantReads {
		t.Errorf("the state comment was read %d time(s), want %d (%d attempts × 2)",
			writes, wantReads, stateWriteAttempts)
	}
	// The foreign writer's document is intact: nothing was written over it.
	mock.mu.Lock()
	defer mock.mu.Unlock()
	for _, c := range mock.comments {
		if stateBeginRe.MatchString(c.Body) && !strings.Contains(c.Body, "a foreign writer was here") {
			t.Error("the losing writer overwrote the document it lost to")
		}
	}
}

// Two processes writing the same item concurrently, each through its own
// Orchestrator over one cache directory — the case actually observed, several
// arenas on one machine. Both changes must be there afterwards.
func TestStateWrite_TwoProcessesBothLand(t *testing.T) {
	b, mock, ref := seededItem(t)
	ctx := context.Background()

	// The sibling's own Orchestrator, over the same server and the same cache
	// directory: a different process, not a second call from this one.
	sibling := newMockedOrchestrator(t, mock, srvOf(t, mock))
	sibling.out.cache = b.out.cache

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = b.AddCost(ctx, ref, "plan", 3)
	}()
	go func() {
		defer wg.Done()
		errs[1] = sibling.AddDuration(ctx, ref, "plan", 90*time.Second)
	}()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	state, err := b.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	row := state.Ledger.Row("plan")
	if row.CostUSD < 3 {
		t.Errorf("the cost charge is gone: %+v", row)
	}
	if row.Active < 90*time.Second {
		t.Errorf("the duration charge is gone: %+v", row)
	}
}

// A lock a killed process left behind must not wedge an item's state forever.
func TestStateWrite_AStaleLockIsBroken(t *testing.T) {
	b, _, ref := seededItem(t)
	path, ok := b.out.cache.stateLockPath(42)
	if !ok {
		t.Fatal("no lock path")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	stale := time.Now().Add(-2 * stateWriteLockTTL)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}

	if err := b.AddCost(t.Context(), ref, "plan", 1); err != nil {
		t.Fatalf("AddCost behind a stale lock: %v", err)
	}
	if _, held := machinecache.AcquireLock(path, stateWriteLockTTL, time.Now()); !held {
		t.Error("the lock is still held after the write that broke it finished")
	}
}

// The create/refuse split is what `create` carries, and both halves have to
// keep working: the writes that bring a document into being still do, and the
// writes that can only FOLLOW one still refuse an absent document.
func TestStateWrite_CreateAndRefuseBothSurviveTheCollapse(t *testing.T) {
	t.Run("create=true brings the document into being", func(t *testing.T) {
		b, mock, _ := newSeamBackend(t)
		ref := b.refFromIssue(mock.issueNum)
		if _, err := b.Claim(t.Context(), ref, nil); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if err := b.RecordDispatch(t.Context(), ref, "plan"); err != nil {
			t.Fatalf("RecordDispatch on an item with no document: %v", err)
		}
		state, err := b.Load(t.Context(), ref)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if state.Ledger.Row("plan").Dispatches == 0 {
			t.Error("the first write did not create the document")
		}
	})

	t.Run("create=false refuses an absent document", func(t *testing.T) {
		b, mock, _ := newSeamBackend(t)
		ref := b.refFromIssue(mock.issueNum)
		if _, err := b.Claim(t.Context(), ref, nil); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		err := b.mutateStateDoc(t.Context(), ref, "Grant", func(*stateDoc) error { return nil })
		if !errors.Is(err, errNoStateComment) {
			t.Errorf("error = %v, want errNoStateComment — a grant against an item nothing dispatched has no row to raise", err)
		}
	})
}

// A document that cannot be parsed stops the write; it is never replaced.
//
// That document is the ledger, the journal and the park. A mutation that read
// an unreadable one and carried on would compute its delta against an EMPTY
// document and PATCH that over the top — corruption turned into data loss, with
// the run reporting success. Both halves of the collapsed read-modify-write
// have to refuse it, and they reach the refusal by different branches: the one
// that MAY create a document must not read "unparseable" as "absent", and the
// one that may not must not fall through to errNoStateComment either.
func TestStateWrite_AnUnparseableDocumentStopsTheWriteRatherThanReplacingIt(t *testing.T) {
	b, mock, ref := seededItem(t)

	// A truncated comment: the begin marker still finds it, and nothing inside
	// can be read. The size cap on a comment makes this the realistic shape.
	var want string
	mock.mu.Lock()
	for i := range mock.comments {
		if stateBeginRe.MatchString(mock.comments[i].Body) {
			mock.comments[i].Body = stateEndRe.ReplaceAllString(mock.comments[i].Body, "<!-- truncated -->")
			want = mock.comments[i].Body
		}
	}
	mock.mu.Unlock()
	if want == "" {
		t.Fatal("there was no state document to corrupt")
	}

	if err := b.RecordDispatch(t.Context(), ref, "plan"); err == nil {
		t.Error("RecordDispatch over an unparseable document reported success — create must not read unreadable as absent")
	}
	if err := b.AddCost(t.Context(), ref, "plan", 1); err == nil {
		t.Error("AddCost over an unparseable document reported success")
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	for _, c := range mock.comments {
		if stateBeginRe.MatchString(c.Body) && c.Body != want {
			t.Errorf("the unparseable document was written over, and what it held is gone:\n%s", c.Body)
		}
	}
}

// A document found by a SCAN carries no tag, and the write still lands.
//
// A comment list answers one tag for the page rather than one per comment, so
// the compare-and-set has nothing to compare and the write proceeds — which is
// the behaviour this package always had, and the case every fresh process hits
// on its first write to an item somebody else seeded. A seam that treated "no
// tag" as "somebody wrote" would replay three times and then park, on an item
// nothing else is touching at all.
func TestStateWrite_ADocumentFoundByAScanHasNoTagAndStillWrites(t *testing.T) {
	b, mock, ref := seededItem(t)

	// Forget the id in BOTH places, which is what a fresh process on another
	// machine has: the memo is empty and the shared record knows nothing.
	b.mu.Lock()
	b.stateCommentCache = map[int]int64{}
	b.mu.Unlock()
	b.out.cache.update(func(rec *seamRecord) { rec.StateComments = nil })
	mock.resetRequests()

	if err := b.AddCost(t.Context(), ref, "plan", 1.5); err != nil {
		t.Fatalf("AddCost over a scan-found document: %v", err)
	}
	if n := mock.requestCount("GET /repos/o/r/issues/42/comments"); n == 0 {
		t.Error("the document was not found by a scan, so this test is not about the scan path any more")
	}
	state, err := b.Load(t.Context(), ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if row := state.Ledger.Row("plan"); row.CostUSD < 1.5 {
		t.Errorf("ledger row = %+v, want the charge — a scan-found document has no tag to compare, not a conflict", row)
	}
}

// The compare-and-set is the comment's own ETag, so the WIRE FORMAT is
// untouched: docs/github-schema.md's document gains no version field and no new
// marker, and a reader written against the old schema reads the new one.
func TestStateWrite_AddsNothingToTheWireFormat(t *testing.T) {
	b, mock, ref := seededItem(t)
	if err := b.AddCost(t.Context(), ref, "plan", 1); err != nil {
		t.Fatalf("AddCost: %v", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	for _, c := range mock.comments {
		if !stateBeginRe.MatchString(c.Body) {
			continue
		}
		for _, forbidden := range []string{"etag", "version:", "revision"} {
			if strings.Contains(strings.ToLower(c.Body), forbidden) {
				t.Errorf("the state document gained %q:\n%s", forbidden, c.Body)
			}
		}
	}
}
