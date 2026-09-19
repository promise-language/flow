package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promise-language/flow"
)

// The project-scope exclusion, in two halves.
//
// The MECHANISM — that a second arena is excluded, that a round past its bound
// is collected, that an arena adopts its own record — is driven against the mock
// GitHub, because the exclusion IS the refusal GitHub gives a duplicate label
// name and a stand-in asserting that a stand-in excludes would measure nothing.
//
// The BRACKET — that the round opens at the rebase, survives the revert, and is
// given back at the merge and at every stop — is driven through the
// acquireLanding seam with fakeLanding, for the reason hostscope_test.go drives
// RunGate through fakeHostScope: the subject is which calls take and give back
// the exclusion, not what the exclusion is made of.

// ---------------------------------------------------------------------------
// The bracket
// ---------------------------------------------------------------------------

// fakeLanding stands in for the mainline's exclusion. The slot genuinely
// excludes: a stand-in that only counted would let two rounds through and then
// report that two got through, which measures the stand-in and not the bracket.
type fakeLanding struct {
	mu sync.Mutex

	slot chan struct{}

	held     int
	takes    int
	releases int
	confirms int
	arenas   []flow.Arena
	waitFor  time.Duration
	failWith error
	// collected models a round that overran its bound and was taken over: the
	// hold's own confirm then refuses, which is the backstop the real mechanism
	// reads off the record.
	collected bool
}

func (f *fakeLanding) acquire(ctx context.Context, _ *Orchestrator, holder flow.Arena) (*landingHold, time.Duration, error) {
	f.mu.Lock()
	f.takes++
	f.arenas = append(f.arenas, holder)
	fail, wait := f.failWith, f.waitFor
	f.mu.Unlock()

	if fail != nil {
		return nil, wait, fail
	}
	select {
	case f.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, wait, ctx.Err()
	}
	f.mu.Lock()
	f.held++
	f.mu.Unlock()

	var once sync.Once
	return &landingHold{
		confirm: func(context.Context) error {
			f.mu.Lock()
			f.confirms++
			collected := f.collected
			f.mu.Unlock()
			if collected {
				return fmt.Errorf("the exclusion is no longer this arena's: %w", flow.ErrUnavailable)
			}
			return nil
		},
		release: func() {
			once.Do(func() {
				f.mu.Lock()
				f.held--
				f.releases++
				f.mu.Unlock()
				<-f.slot
			})
		},
	}, wait, nil
}

func (f *fakeLanding) snapshot() (takes, releases, stillHeld int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.takes, f.releases, f.held
}

func useFakeLanding(t *testing.T) *fakeLanding {
	t.Helper()
	f := &fakeLanding{slot: make(chan struct{}, 1)}
	prev := acquireLanding
	acquireLanding = f.acquire
	t.Cleanup(func() { acquireLanding = prev })
	return f
}

// landingBackend is a mocked orchestrator whose git recorder answers the four
// calls a merge-result preparation makes, plus the revert's reset.
func landingBackend(t *testing.T) (*Orchestrator, *ghMock, *gitRecorder) {
	t.Helper()
	mock := newGHMock(t)
	srv := mock.server()
	t.Cleanup(srv.Close)
	b := newMockedOrchestrator(t, mock, srv)

	rec := newGitRecorder()
	scriptCleanWorktree(rec)
	rec.handlers["rev-parse HEAD"] = func([]string) ([]byte, error) {
		return []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"), nil
	}
	rec.handlers["merge origin/main --no-edit"] = func([]string) ([]byte, error) { return nil, nil }
	rec.handlers["reset --hard bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"] = func([]string) ([]byte, error) {
		return nil, nil
	}
	b.git.runner = rec.run
	return b, mock, rec
}

func landingWorktree(t *testing.T, b *Orchestrator) *worktree {
	t.Helper()
	wt, err := b.Worktree(t.Context(), b.refFromIssue(42))
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	return wt.(*worktree)
}

// THE SPAN. The serialized section is the round and not the landing act: it
// opens at the rebase, and the revert in the middle — which every request-landing
// round makes, so the branch is pushed as the branch — does NOT end it. A revert
// that gave the exclusion back would leave the measurement open to invalidation
// for exactly the window the exclusion exists to close.
func TestLanding_RoundSpansPrepareThroughMerge(t *testing.T) {
	f := useFakeLanding(t)
	b, _, _ := landingBackend(t)
	w := landingWorktree(t, b)
	ctx := t.Context()

	if err := w.PrepareMergeResult(ctx, "main"); err != nil {
		t.Fatalf("PrepareMergeResult: %v", err)
	}
	if _, _, held := f.snapshot(); held != 1 {
		t.Fatalf("after the rebase the exclusion is held %d times, want 1", held)
	}

	if err := w.RevertMergePrep(ctx); err != nil {
		t.Fatalf("RevertMergePrep: %v", err)
	}
	if _, releases, held := f.snapshot(); held != 1 || releases != 0 {
		t.Fatalf("the revert ended the round (held=%d, releases=%d); the measurement it protects is still unlanded",
			held, releases)
	}

	if err := w.Merge(ctx, "https://github.com/o/r/pull/1"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	takes, releases, held := f.snapshot()
	if held != 0 || releases != 1 {
		t.Errorf("after the merge: held=%d releases=%d, want 0 and 1 — the round is the lock's whole life", held, releases)
	}
	// One take for the whole round: Merge found the round already open.
	if takes != 1 {
		t.Errorf("the round took the exclusion %d times, want 1", takes)
	}
	// And the land asked the hold whether the exclusion was still this
	// arena's — the backstop against a round that overran its bound.
	f.mu.Lock()
	confirms := f.confirms
	f.mu.Unlock()
	if confirms != 1 {
		t.Errorf("the merge confirmed the hold %d times, want 1 — nothing checked the round had not been collected", confirms)
	}
}

// The holder is the ARENA — (HostId, ArenaId) — never a checkout path, which
// could not say which of a host's arenas holds the mainline.
func TestLanding_HeldAsTheArena(t *testing.T) {
	f := useFakeLanding(t)
	b, _, _ := landingBackend(t)
	w := landingWorktree(t, b)

	if err := w.PrepareMergeResult(t.Context(), "main"); err != nil {
		t.Fatalf("PrepareMergeResult: %v", err)
	}
	f.mu.Lock()
	arenas := append([]flow.Arena(nil), f.arenas...)
	f.mu.Unlock()
	if len(arenas) != 1 {
		t.Fatalf("arenas = %v, want exactly one take", arenas)
	}
	if arenas[0] != b.arena() || arenas[0].Empty() {
		t.Errorf("held as %+v, want this orchestrator's arena %+v", arenas[0], b.arena())
	}
}

// A preparation that FAILED ends the round: it measured nothing and will land
// nothing, so an exclusion held past it starves the queue for a round that is
// over.
func TestLanding_FailedPreparationEndsTheRound(t *testing.T) {
	f := useFakeLanding(t)
	b, _, rec := landingBackend(t)
	rec.handlers["merge origin/main --no-edit"] = func([]string) ([]byte, error) {
		return nil, fmt.Errorf("CONFLICT (content): merge conflict in a.go")
	}
	rec.handlers["merge --abort"] = func([]string) ([]byte, error) { return nil, nil }
	w := landingWorktree(t, b)

	if err := w.PrepareMergeResult(t.Context(), "main"); err == nil {
		t.Fatal("PrepareMergeResult = nil though the merge simulation conflicted")
	}
	if _, releases, held := f.snapshot(); held != 0 || releases != 1 {
		t.Errorf("after a failed preparation: held=%d releases=%d, want 0 and 1", held, releases)
	}
}

// The round ends whatever becomes of the merge. A merge GitHub refused landed
// nothing, and holding the mainline past it is the stalled-arena failure.
func TestLanding_RefusedMergeStillEndsTheRound(t *testing.T) {
	f := useFakeLanding(t)
	b, mock, _ := landingBackend(t)
	mock.mergeRefusal = "Pull request is not mergeable"
	w := landingWorktree(t, b)
	ctx := t.Context()

	if err := w.PrepareMergeResult(ctx, "main"); err != nil {
		t.Fatalf("PrepareMergeResult: %v", err)
	}
	if err := w.Merge(ctx, "https://github.com/o/r/pull/1"); err == nil {
		t.Fatal("Merge = nil though GitHub refused it")
	}
	if _, releases, held := f.snapshot(); held != 0 || releases != 1 {
		t.Errorf("after a refused merge: held=%d releases=%d, want 0 and 1", held, releases)
	}
}

// A ROUND WHOSE HOLD NO LONGER CONFIRMS ENDS ANYWAY. The refusal is the
// backstop against a collection; what must not also happen is the arena being
// left believing it holds a mainline that is somebody else's, which would make
// every later round in this process skip the take.
func TestLanding_CollectedRoundStillEndsInMemory(t *testing.T) {
	f := useFakeLanding(t)
	b, _, _ := landingBackend(t)
	w := landingWorktree(t, b)
	ctx := t.Context()

	if err := w.PrepareMergeResult(ctx, "main"); err != nil {
		t.Fatalf("PrepareMergeResult: %v", err)
	}
	f.mu.Lock()
	f.collected = true
	f.mu.Unlock()

	if err := w.Merge(ctx, "https://github.com/o/r/pull/1"); err == nil {
		t.Fatal("Merge = nil though the hold no longer confirms")
	}
	if _, releases, held := f.snapshot(); held != 0 || releases != 1 {
		t.Errorf("after a collected round: held=%d releases=%d, want 0 and 1", held, releases)
	}
	b.mu.Lock()
	stillOpen := b.landingHeld != nil
	b.mu.Unlock()
	if stillOpen {
		t.Error("the round is still open in memory; the next one would skip the take")
	}
}

// A caller that lands something it never simulated still lands inside the
// exclusion. It is weaker than a round — there is no merge-result measurement
// to protect — and it is the most that can be said about a landing nobody
// measured; what it must not be is unserialized.
func TestLanding_MergeAloneOpensAndClosesTheRound(t *testing.T) {
	f := useFakeLanding(t)
	b, _, _ := landingBackend(t)
	w := landingWorktree(t, b)

	if err := w.Merge(t.Context(), "https://github.com/o/r/pull/1"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	takes, releases, held := f.snapshot()
	if takes != 1 || releases != 1 || held != 0 {
		t.Errorf("takes=%d releases=%d held=%d, want 1, 1 and 0", takes, releases, held)
	}
}

// TWO ACQUISITIONS BY ONE ARENA NAME ONE RECORD — the second adopts what the
// first created, which TestTakeLanding_ArenaAdoptsItsOwnRecord establishes — so
// the one that loses the race to BECOME the round drops its hold rather than
// releasing it. Releasing would delete the mainline's record out from under a
// round that is still running, and the arena would then be landing
// unserialized while believing otherwise.
func TestHoldLanding_ARacedAcquisitionIsDroppedNotReleased(t *testing.T) {
	b, _, _ := landingBackend(t)
	released := false

	prev := acquireLanding
	acquireLanding = func(context.Context, *Orchestrator, flow.Arena) (*landingHold, time.Duration, error) {
		// The window holdLanding has: another call opens the round while this
		// acquisition is still in flight.
		b.mu.Lock()
		b.landingHeld = &landingHold{
			confirm: func(context.Context) error { return nil },
			release: func() {},
		}
		b.mu.Unlock()
		return &landingHold{
			confirm: func(context.Context) error { return nil },
			release: func() { released = true },
		}, 0, nil
	}
	t.Cleanup(func() { acquireLanding = prev })

	if err := b.holdLanding(t.Context(), b.refFromIssue(42)); err != nil {
		t.Fatalf("holdLanding: %v", err)
	}
	if released {
		t.Error("the raced acquisition was released; it names the open round's own record")
	}
}

// A PUSH IS NOT A LANDING HERE. This backend lands through a request, so the
// push publishes the branch for review and a branch is not trunk
// (docs/resolution-standalone.md § Where verify is required). A push that took
// the exclusion would queue every contributor in the fleet behind one mainline.
func TestLanding_PushAndOpenTakeNothing(t *testing.T) {
	f := useFakeLanding(t)
	b, _, rec := landingBackend(t)
	// The push assembles what it would carry before it publishes: the branch,
	// the commit messages, and the diff.
	rec.handlers["log --format=%B%x00 main --not --remotes=origin --"] = func([]string) ([]byte, error) {
		return []byte("the work\x00"), nil
	}
	rec.handlers["log --format= --patch --diff-merges=first-parent main --not --remotes=origin --"] =
		func([]string) ([]byte, error) { return []byte("--- a\n+++ b\n"), nil }
	rec.handlers["push -u origin main"] = func([]string) ([]byte, error) { return nil, nil }
	w := landingWorktree(t, b)

	if err := w.Push(t.Context()); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if takes, _, _ := f.snapshot(); takes != 0 {
		t.Errorf("a branch push took the mainline's exclusion %d times, want 0", takes)
	}
}

// AN EXCLUSION THAT CANNOT BE TAKEN IS A REFUSAL, NEVER A FREE PASS: nothing is
// fetched, nothing is merged locally, and the error names what could not be
// taken so a reader is not sent looking for a defect in the change.
func TestLanding_UntakeableExclusionLandsNothing(t *testing.T) {
	f := useFakeLanding(t)
	f.failWith = errors.New("github is unreachable")
	b, mock, rec := landingBackend(t)
	w := landingWorktree(t, b)
	ctx := t.Context()

	err := w.PrepareMergeResult(ctx, "main")
	if err == nil {
		t.Fatal("PrepareMergeResult = nil though the exclusion could not be taken")
	}
	if !strings.Contains(err.Error(), "exclusion") {
		t.Errorf("error = %q, does not name the exclusion it could not take", err)
	}
	if rec.called("fetch") || rec.called("merge") {
		t.Error("the merge result was prepared without the exclusion — the round ran unserialized")
	}

	if err := w.Merge(ctx, "https://github.com/o/r/pull/1"); err == nil {
		t.Fatal("Merge = nil though the exclusion could not be taken")
	}
	mock.mu.Lock()
	sent := append([]string(nil), mock.mutations...)
	mock.mu.Unlock()
	for _, m := range sent {
		if strings.Contains(m, "/merge") {
			t.Errorf("a merge was sent without the exclusion: %v", sent)
		}
	}
}

// Every stop gives the mainline back: a lock in a stalled arena starves every
// landing behind it, and work that stopped mid-landing comes back behind
// whatever landed meanwhile rather than resuming a lock nothing kept for it.
func TestLanding_EveryStopEndsTheRound(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(t *testing.T, b *Orchestrator, ref flow.ItemRef)
	}{
		{
			name: "park",
			stop: func(t *testing.T, b *Orchestrator, ref flow.ItemRef) {
				if err := b.Park(t.Context(), ref, flow.ParkRequest{
					Kind: flow.ParkInfraTransient, Step: "merge", Reason: "the runner went away",
				}); err != nil {
					t.Fatalf("Park: %v", err)
				}
			},
		},
		{
			name: "release",
			stop: func(t *testing.T, b *Orchestrator, ref flow.ItemRef) {
				// Release refuses this arena (HEAD is on the claim branch in a
				// real one; here the recorder says main and there is no claim
				// record), and the release of the mainline must not depend on
				// it succeeding: an arena on its way out holding the mainline is
				// the failure this exists to prevent.
				_ = b.Release(t.Context(), ref)
			},
		},
		{
			name: "finalize",
			stop: func(t *testing.T, b *Orchestrator, ref flow.ItemRef) {
				_ = b.Finalize(t.Context(), ref, flow.DispositionResolved)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := useFakeLanding(t)
			b, _, _ := landingBackend(t)
			w := landingWorktree(t, b)
			if err := w.PrepareMergeResult(t.Context(), "main"); err != nil {
				t.Fatalf("PrepareMergeResult: %v", err)
			}

			tc.stop(t, b, b.refFromIssue(42))

			if _, releases, held := f.snapshot(); held != 0 || releases != 1 {
				t.Errorf("after %s: held=%d releases=%d, want 0 and 1 — the exclusion outlived a stop",
					tc.name, held, releases)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The wait, reported as waiting
// ---------------------------------------------------------------------------

// A queue reaches the ledger as WAITING, against the step this dispatch is —
// the one RecordDispatch named — and never as active time.
func TestLanding_QueueIsFiledAsWaiting(t *testing.T) {
	f := useFakeLanding(t)
	f.waitFor = 7 * time.Minute
	b, _, _ := landingBackend(t)
	ctx := t.Context()

	claim, err := b.Claim(ctx, b.refFromIssue(42), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.RecordDispatch(ctx, claim.ItemRef, "merge"); err != nil {
		t.Fatalf("RecordDispatch: %v", err)
	}

	w := landingWorktree(t, b)
	if err := w.PrepareMergeResult(ctx, "main"); err != nil {
		t.Fatalf("PrepareMergeResult: %v", err)
	}

	state, err := b.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	row := state.Ledger.Row("merge")
	if row.Waiting != 7*time.Minute {
		t.Errorf("row.Waiting = %s, want 7m — the queue is the ledger's waiting figure", row.Waiting)
	}
	if row.Active != 0 {
		t.Errorf("row.Active = %s, want zero — waiting is never charged as work", row.Active)
	}
	if state.Ledger.TotalWaiting != 7*time.Minute {
		t.Errorf("TotalWaiting = %s, want 7m", state.Ledger.TotalWaiting)
	}
}

// The queue is filed even when the round then gave up on it. A round that
// queued and could not take the exclusion still spent that wall clock on
// contention, and a figure that counted it only where the round went on to land
// would understate contention exactly where contention is worst.
func TestLanding_QueueIsFiledEvenWhenTheExclusionIsNeverTaken(t *testing.T) {
	f := useFakeLanding(t)
	f.waitFor = 4 * time.Minute
	f.failWith = errors.New("gave up waiting")
	b, _, _ := landingBackend(t)
	ctx := t.Context()

	claim, err := b.Claim(ctx, b.refFromIssue(42), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.RecordDispatch(ctx, claim.ItemRef, "merge"); err != nil {
		t.Fatalf("RecordDispatch: %v", err)
	}

	w := landingWorktree(t, b)
	if err := w.PrepareMergeResult(ctx, "main"); err == nil {
		t.Fatal("PrepareMergeResult = nil though the exclusion was never taken")
	}

	state, err := b.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := state.Ledger.Row("merge").Waiting; got != 4*time.Minute {
		t.Errorf("row.Waiting = %s, want 4m on the path where the round gave up", got)
	}
}

// THE FIGURE IS FILED THROUGH A CONTEXT OF ITS OWN, detached from the round's.
//
// The largest wait this is ever handed is the one the round's own deadline
// produced while it sat in the queue — so a write made through that context
// fails before it leaves, and the ledger comes out emptiest exactly where
// contention was worst, which is the shape #435 records. The round's context is
// already dead here, and the figure still lands.
func TestLanding_QueueIsFiledThroughTheRoundsOwnDeadContext(t *testing.T) {
	f := useFakeLanding(t)
	f.waitFor = 9 * time.Minute
	f.failWith = context.DeadlineExceeded
	b, _, _ := landingBackend(t)
	live := t.Context()

	claim, err := b.Claim(live, b.refFromIssue(42), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.RecordDispatch(live, claim.ItemRef, "merge"); err != nil {
		t.Fatalf("RecordDispatch: %v", err)
	}

	// The round's context, already over — the state the queue left it in.
	dead, cancel := context.WithCancel(live)
	cancel()

	w := landingWorktree(t, b)
	if err := w.PrepareMergeResult(dead, "main"); err == nil {
		t.Fatal("PrepareMergeResult = nil though the round gave up waiting")
	}

	state, err := b.Load(live, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := state.Ledger.Row("merge").Waiting; got != 9*time.Minute {
		t.Errorf("row.Waiting = %s, want 9m — the figure that says why the round gave up "+
			"was filed through the context that ended it", got)
	}
}

// THE FIGURE IS KEYED BY THE STEP THIS DISPATCH IS, which is the last one
// RecordDispatch named. A round spans two dispatches — the merge-result
// measurement and the land — so an orchestrator that noted the step once and
// never again would file the land's queue against the measurement's row, and
// the two rows are what a treasurer reads contention off.
func TestLanding_QueueIsFiledAgainstTheLatestDispatch(t *testing.T) {
	f := useFakeLanding(t)
	f.waitFor = 6 * time.Minute
	b, _, _ := landingBackend(t)
	ctx := t.Context()

	claim, err := b.Claim(ctx, b.refFromIssue(42), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	for _, step := range []flow.StepId{"verify merge result", "merge"} {
		if err := b.RecordDispatch(ctx, claim.ItemRef, step); err != nil {
			t.Fatalf("RecordDispatch(%s): %v", step, err)
		}
	}

	w := landingWorktree(t, b)
	if err := w.Merge(ctx, "https://github.com/o/r/pull/1"); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	state, err := b.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := state.Ledger.Row("merge").Waiting; got != 6*time.Minute {
		t.Errorf("row.Waiting on merge = %s, want 6m", got)
	}
	if got := state.Ledger.Row("verify merge result").Waiting; got != 0 {
		t.Errorf("row.Waiting on verify merge result = %s, want zero — "+
			"the land's queue was charged to the measurement's row", got)
	}
}

// Nothing dispatched this in this process — a caller driving the worktree
// surface directly — so there is no row to key the figure by. The round still
// happens; inventing a step would file contention against one that never ran.
func TestLanding_QueueIsNotFiledWithoutADispatch(t *testing.T) {
	f := useFakeLanding(t)
	f.waitFor = 3 * time.Minute
	b, mock, _ := landingBackend(t)
	w := landingWorktree(t, b)

	if err := w.PrepareMergeResult(t.Context(), "main"); err != nil {
		t.Fatalf("PrepareMergeResult: %v", err)
	}
	if _, _, held := f.snapshot(); held != 1 {
		t.Errorf("the round was not taken, though only the accounting was missing")
	}
	mock.mu.Lock()
	sent := append([]string(nil), mock.mutations...)
	mock.mu.Unlock()
	for _, m := range sent {
		if strings.Contains(m, "/comments") {
			t.Errorf("a ledger write was made with no dispatch to key it by: %v", sent)
		}
	}
}

// An uncontended round pays nothing and files nothing. A clock around an
// uncontended acquire would report the round trip as contention and buy a
// ledger write on every single landing.
func TestLanding_UncontendedRoundFilesNothing(t *testing.T) {
	useFakeLanding(t)
	b, _, _ := landingBackend(t)
	ctx := t.Context()

	claim, err := b.Claim(ctx, b.refFromIssue(42), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.RecordDispatch(ctx, claim.ItemRef, "merge"); err != nil {
		t.Fatalf("RecordDispatch: %v", err)
	}
	w := landingWorktree(t, b)
	if err := w.PrepareMergeResult(ctx, "main"); err != nil {
		t.Fatalf("PrepareMergeResult: %v", err)
	}

	state, err := b.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := state.Ledger.Row("merge").Waiting; got != 0 {
		t.Errorf("row.Waiting = %s on an uncontended round, want zero", got)
	}
}

// ---------------------------------------------------------------------------
// The mechanism
// ---------------------------------------------------------------------------

// twoLandingArenas builds two orchestrators pointed at one mock repository,
// each with its own worktree path — so the two fingerprints differ and the
// exclusion has something real to decide between.
func twoLandingArenas(t *testing.T) (*Orchestrator, *Orchestrator, *ghMock) {
	t.Helper()
	mock := newGHMock(t)
	srv := mock.server()
	t.Cleanup(srv.Close)
	a := newMockedOrchestrator(t, mock, srv)
	bb := newMockedOrchestrator(t, mock, srv)
	if a.arenaFingerprint() == bb.arenaFingerprint() {
		t.Fatal("the two harness arenas share a fingerprint; they would not exclude each other")
	}
	return a, bb, mock
}

// THE EXCLUSION EXCLUDES. A second arena meeting a live record does not get the
// mainline, and the first's record is what it reads — the holder written in the
// same request as the name, because a holder its own tools cannot recognise is
// a deadlock rather than a missing diagnostic.
func TestTakeLanding_SecondArenaIsExcluded(t *testing.T) {
	a, bb, mock := twoLandingArenas(t)
	ctx := t.Context()

	hold, waited, err := takeLanding(ctx, a, a.arena())
	if err != nil {
		t.Fatalf("takeLanding: %v", err)
	}
	if waited != 0 {
		t.Errorf("waited = %s on an uncontended take, want exactly zero", waited)
	}
	mock.mu.Lock()
	desc := mock.repoLabels["flow:landing"]
	mock.mu.Unlock()
	if held, _, ok := parseLandingHolder(desc); !ok || held != a.arenaFingerprint() {
		t.Fatalf("the record reads %q, want the first arena's fingerprint %q", desc, a.arenaFingerprint())
	}
	if strings.Contains(desc, string(a.arena().Id)) || strings.Contains(desc, string(a.arena().Host)) {
		t.Errorf("the record publishes the arena pair (%q); it must carry the digest only", desc)
	}

	// The second arena cannot have it while the first does. Its context runs
	// out rather than its patience: the queue has no bound of its own.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	prevPoll := landingPoll
	landingPoll = time.Millisecond
	defer func() { landingPoll = prevPoll }()

	if _, _, err := takeLanding(short, bb, bb.arena()); err == nil {
		t.Fatal("the second arena took the mainline while the first held it")
	}

	// Given back, it is free again.
	hold.release()
	mock.mu.Lock()
	_, stillThere := mock.repoLabels["flow:landing"]
	mock.mu.Unlock()
	if stillThere {
		t.Fatal("the record outlived its release")
	}
	hold2, _, err := takeLanding(ctx, bb, bb.arena())
	if err != nil {
		t.Fatalf("the second arena still cannot take a released exclusion: %v", err)
	}
	hold2.release()
}

// A QUEUE ENDS WHEN THE QUEUE REACHES IT, and what it cost is reported. This is
// the one case that proves the wait is measured rather than assumed: it is
// non-zero only because the second arena actually waited for the first.
func TestTakeLanding_QueuedRoundReportsWhatItWaited(t *testing.T) {
	a, bb, _ := twoLandingArenas(t)
	ctx := t.Context()
	prevPoll := landingPoll
	landingPoll = time.Millisecond
	defer func() { landingPoll = prevPoll }()

	hold, _, err := takeLanding(ctx, a, a.arena())
	if err != nil {
		t.Fatalf("takeLanding: %v", err)
	}

	// The clock the record and the wait are read against, wound forward once
	// the first arena lets go, so the measured wait is a value and not a race.
	base := time.Now().UTC()
	prevNow := nowUTC
	var mu sync.Mutex
	offset := time.Duration(0)
	nowUTC = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return base.Add(offset)
	}
	defer func() { nowUTC = prevNow }()

	done := make(chan time.Duration, 1)
	go func() {
		_, waited, err := takeLanding(ctx, bb, bb.arena())
		if err != nil {
			done <- -1
			return
		}
		done <- waited
	}()

	// Let the queued arena go round at least once, then move the clock and let
	// it through.
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	offset = 3 * time.Minute
	mu.Unlock()
	hold.release()

	select {
	case waited := <-done:
		if waited != 3*time.Minute {
			t.Errorf("waited = %s, want the 3m the queue actually took", waited)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the queued arena never got the mainline")
	}
}

// AN ARENA RE-TAKES ITS OWN RECORD. Across a round that means the process that
// began it is gone — one arena runs one round at a time — so the record is this
// arena's to give back, and the release it gets is a real one. A no-op would
// leave a stop holding the mainline until the bound collected it.
func TestTakeLanding_ArenaAdoptsItsOwnRecord(t *testing.T) {
	a, _, mock := twoLandingArenas(t)
	ctx := t.Context()

	if _, _, err := takeLanding(ctx, a, a.arena()); err != nil {
		t.Fatalf("takeLanding: %v", err)
	}
	// A second acquisition by the same arena — the shape a later process meets.
	hold, waited, err := takeLanding(ctx, a, a.arena())
	if err != nil {
		t.Fatalf("takeLanding on this arena's own record: %v", err)
	}
	if waited != 0 {
		t.Errorf("waited = %s re-taking this arena's own record, want zero", waited)
	}
	hold.release()
	mock.mu.Lock()
	_, stillThere := mock.repoLabels["flow:landing"]
	mock.mu.Unlock()
	if stillThere {
		t.Error("re-taking this arena's own record returned a release that gives nothing back")
	}
}

// THE RE-TAKEN RECORD CARRIES THIS ROUND'S INSTANT, NOT THE ABANDONED ONE'S.
// The instant is what the bound is measured from, so a round that inherited it
// would begin with part of its bound spent — and one begun after a longer gap
// with none of it left, collectible by the first peer to look, from under a
// merge-result measurement that is still running.
func TestTakeLanding_RetakingItsOwnRecordRestartsTheBound(t *testing.T) {
	a, bb, mock := twoLandingArenas(t)
	ctx := t.Context()

	// A round this arena began and never gave back, abandoned long enough ago
	// that a peer would collect it.
	abandoned := nowUTC().Add(-2 * a.landingRoundBound())
	mock.mu.Lock()
	mock.repoLabels = map[string]string{"flow:landing": renderLandingHolder(a.arenaFingerprint(), abandoned)}
	mock.mu.Unlock()

	if _, _, err := takeLanding(ctx, a, a.arena()); err != nil {
		t.Fatalf("takeLanding on this arena's own abandoned record: %v", err)
	}

	mock.mu.Lock()
	desc := mock.repoLabels["flow:landing"]
	mock.mu.Unlock()
	held, at, ok := parseLandingHolder(desc)
	if !ok || held != a.arenaFingerprint() {
		t.Fatalf("the record reads %q, want this arena's fingerprint %q", desc, a.arenaFingerprint())
	}
	if !at.After(abandoned) {
		t.Fatalf("the re-taken record still carries the abandoned round's instant %s; "+
			"this round's bound is spent before it starts", at)
	}

	// And the bound now runs from this round, so a peer finds a live record and
	// queues rather than collecting the mainline out from under it.
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	prevPoll := landingPoll
	landingPoll = time.Millisecond
	defer func() { landingPoll = prevPoll }()
	if _, _, err := takeLanding(short, bb, bb.arena()); err == nil {
		t.Error("a peer collected the re-taken record; the round was never given its own bound")
	}
}

// The rewrite is NOT a second way to take the exclusion: it fails when there is
// no record, and the take falls back to the create — the one act GitHub refuses
// when the name is taken — rather than reporting a hold over nothing.
func TestTakeLanding_RecordVanishingUnderTheRetakeFallsBackToTheCreate(t *testing.T) {
	a, _, mock := twoLandingArenas(t)

	mock.mu.Lock()
	mock.repoLabels = map[string]string{
		"flow:landing": renderLandingHolder(a.arenaFingerprint(), nowUTC().Add(-time.Minute)),
	}
	mock.dropRepoLabelOnRead = "flow:landing"
	mock.mu.Unlock()

	hold, _, err := takeLanding(t.Context(), a, a.arena())
	if err != nil {
		t.Fatalf("takeLanding: %v", err)
	}
	mock.mu.Lock()
	desc := mock.repoLabels["flow:landing"]
	mock.mu.Unlock()
	if held, _, ok := parseLandingHolder(desc); !ok || held != a.arenaFingerprint() {
		t.Errorf("the record reads %q after a take that fell back to the create", desc)
	}
	hold.release()
}

// A RECORD PAST THE ROUND'S OWN BOUND IS COLLECTED. Not a timer on the lock:
// the round carries its own declared bound, so a record older than that belongs
// to an arena that parked, crashed or went quiet — and a lock in a stalled
// arena starves every landing behind it.
func TestTakeLanding_CollectsARecordPastTheRoundBound(t *testing.T) {
	a, bb, mock := twoLandingArenas(t)
	ctx := t.Context()

	if _, _, err := takeLanding(ctx, a, a.arena()); err != nil {
		t.Fatalf("takeLanding: %v", err)
	}

	// Inside the bound, the record stands and the peer queues.
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	prevPoll := landingPoll
	landingPoll = time.Millisecond
	defer func() { landingPoll = prevPoll }()
	if _, _, err := takeLanding(short, bb, bb.arena()); err == nil {
		t.Fatal("a live record was collected; only one past the round's bound may be")
	}

	// Past it, the peer collects and takes over.
	prevNow := nowUTC
	nowUTC = func() time.Time { return prevNow().Add(a.landingRoundBound() + time.Minute) }
	defer func() { nowUTC = prevNow }()

	if _, _, err := takeLanding(ctx, bb, bb.arena()); err != nil {
		t.Fatalf("a record past the round's bound was not collected: %v", err)
	}
	mock.mu.Lock()
	desc := mock.repoLabels["flow:landing"]
	mock.mu.Unlock()
	if held, _, _ := parseLandingHolder(desc); held != bb.arenaFingerprint() {
		t.Errorf("the record reads %q, want the collecting arena's fingerprint", desc)
	}
}

// A record nothing can read has no way to expire on its own, so it is collected
// for the reason an untimestamped claim token is.
func TestTakeLanding_CollectsAnUnreadableRecord(t *testing.T) {
	a, _, mock := twoLandingArenas(t)
	mock.mu.Lock()
	mock.repoLabels = map[string]string{"flow:landing": "written by something that is not this"}
	mock.mu.Unlock()

	hold, _, err := takeLanding(t.Context(), a, a.arena())
	if err != nil {
		t.Fatalf("an unreadable record was not collected: %v", err)
	}
	hold.release()
}

// WHAT THE HOLDER ASKS BEFORE IT MERGES is whether the record is still its
// own, and every answer that is not yes is a refusal. This is the backstop that
// keeps a collection from costing a wrong landing, so the cases that are NOT a
// peer's fingerprint matter as much as the one that is: a confirm that said yes
// to a record that is absent, or to one nothing can read, would let a round
// land on the strength of a record nobody wrote.
func TestLandingHold_ConfirmRefusesEveryRecordButItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// reseed replaces the record after the hold was taken, and reports
		// whether the confirm should pass.
		reseed func(m *ghMock, a, bb *Orchestrator)
		want   bool
		// transient marks the refusals that say nothing about the change, so
		// the work comes back to re-measure rather than being judged unfit.
		transient bool
	}{
		{
			name:   "still this arena's",
			reseed: func(*ghMock, *Orchestrator, *Orchestrator) {},
			want:   true,
		},
		{
			name: "collected and re-taken by a peer",
			reseed: func(m *ghMock, _, bb *Orchestrator) {
				m.repoLabels["flow:landing"] = renderLandingHolder(bb.arenaFingerprint(), nowUTC())
			},
			transient: true,
		},
		{
			name: "collected and taken by nobody",
			reseed: func(m *ghMock, _, _ *Orchestrator) {
				delete(m.repoLabels, "flow:landing")
			},
			transient: true,
		},
		{
			name: "overwritten with something nothing can read",
			reseed: func(m *ghMock, _, _ *Orchestrator) {
				m.repoLabels["flow:landing"] = "not a record"
			},
			transient: true,
		},
		{
			// No assertion on the KIND of refusal here: what matters is that a
			// holder which could not find out refuses, because the alternative
			// is landing on a question nobody answered.
			name: "a GitHub that will not say",
			reseed: func(m *ghMock, _, _ *Orchestrator) {
				m.failRepoLabelRead = true
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, bb, mock := twoLandingArenas(t)
			hold, _, err := takeLanding(t.Context(), a, a.arena())
			if err != nil {
				t.Fatalf("takeLanding: %v", err)
			}
			mock.mu.Lock()
			tc.reseed(mock, a, bb)
			mock.mu.Unlock()

			err = hold.confirm(t.Context())
			if tc.want {
				if err != nil {
					t.Fatalf("confirm = %v over this arena's own record, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("confirm = nil though the record is no longer this arena's — the round would land unserialized")
			}
			if tc.transient && !errors.Is(err, flow.ErrUnavailable) {
				t.Errorf("confirm = %v, want it to read as transient: nothing about the change was found wanting", err)
			}
		})
	}
}

// A ROUND THAT IS NOT OPEN CANNOT BE CONFIRMED, and the honest answer is the
// same refusal. Nothing establishes this arena may land, and the one way to
// reach here is that something ended the round in between — an arena that has
// stopped is exactly an arena that may not land.
func TestConfirmLanding_RefusesWhenNoRoundIsOpen(t *testing.T) {
	b, _, _ := landingBackend(t)
	err := b.confirmLanding(t.Context())
	if err == nil {
		t.Fatal("confirmLanding = nil with no round open; nothing had established this arena may land")
	}
	if !errors.Is(err, flow.ErrUnavailable) {
		t.Errorf("err = %v, want it to read as transient", err)
	}
}

// A RELEASE THAT CANNOT READ THE RECORD GIVES BACK NOTHING. It cannot establish
// what is there is still its own, and a delete on a guess is the one outcome
// worse than a record the bound will collect: the round that holds the mainline
// now would lose it to a third arena while it is still inside its own.
func TestLandingReleaser_LeavesARecordItCannotReadAlone(t *testing.T) {
	a, _, mock := twoLandingArenas(t)
	hold, _, err := takeLanding(t.Context(), a, a.arena())
	if err != nil {
		t.Fatalf("takeLanding: %v", err)
	}
	mock.mu.Lock()
	mock.failRepoLabelRead = true
	mock.mu.Unlock()

	hold.release()

	mock.mu.Lock()
	_, stillThere := mock.repoLabels["flow:landing"]
	mock.mu.Unlock()
	if !stillThere {
		t.Error("the release deleted a record it could not read; it cannot tell its own from the round that took over")
	}
}

// A RELEASE NEVER TAKES SOMEBODY ELSE'S. A round that overran its bound has
// already been collected, and whoever holds the mainline now is landing under
// it — deleting that would put two arenas inside one round.
func TestLandingReleaser_LeavesAnotherArenasRecordAlone(t *testing.T) {
	a, bb, mock := twoLandingArenas(t)

	hold, _, err := takeLanding(t.Context(), a, a.arena())
	if err != nil {
		t.Fatalf("takeLanding: %v", err)
	}
	// Collected and re-taken by the peer while the first arena was still going.
	mock.mu.Lock()
	mock.repoLabels["flow:landing"] = renderLandingHolder(bb.arenaFingerprint(), time.Now().UTC())
	mock.mu.Unlock()

	hold.release()

	mock.mu.Lock()
	desc := mock.repoLabels["flow:landing"]
	mock.mu.Unlock()
	if held, _, _ := parseLandingHolder(desc); held != bb.arenaFingerprint() {
		t.Errorf("the record reads %q after the first arena's release; it took the peer's exclusion", desc)
	}
}

// An arena that cannot name itself cannot hold the mainline. Comparing an empty
// fingerprint would let every unnameable arena adopt every other one's record.
func TestTakeLanding_RefusesAnUnnameableArena(t *testing.T) {
	a, _, _ := twoLandingArenas(t)
	if _, _, err := takeLanding(t.Context(), a, flow.Arena{}); err == nil {
		t.Fatal("takeLanding = nil for an arena that cannot name itself")
	}
}

// ONLY ONE REFUSAL MEANS SOMEBODY HOLDS THE MAINLINE, and every other one is
// an error the take reports at once.
//
// This is what `already_exists` is read by its CODE for. 422 is shared with
// refusals that must stay errors — a name too long, a colour that will not
// parse — and a take that read the status alone would queue behind a name it
// can never create, for as long as anyone let it run, reporting contention on
// a repository where nothing is contending.
//
// The assertion is that the take came back, not merely that it failed: a take
// that queued would also end in an error, and the error it ends in is the
// deadline.
func TestTakeLanding_ACreateRefusedForAnythingButTheNameIsAnError(t *testing.T) {
	const otherValidationFailure = `{"message":"Validation Failed","errors":[` +
		`{"resource":"Label","code":"invalid","field":"color"}]}`

	for _, tc := range []struct {
		name   string
		status int
		body   string
		// seed puts a record in the way, so the create that meets the refusal
		// is the one INSIDE the queue loop rather than the first attempt. The
		// two are separate calls, and a refusal read wrongly at either one
		// hangs the round.
		seed func(a, bb *Orchestrator) string
	}{
		{
			name:   "a validation failure that is not the name, at the first attempt",
			status: http.StatusUnprocessableEntity,
			body:   otherValidationFailure,
		},
		{
			name:   "a validation failure that is not the name, after collecting a dead record",
			status: http.StatusUnprocessableEntity,
			body:   otherValidationFailure,
			seed: func(_, bb *Orchestrator) string {
				return renderLandingHolder(bb.arenaFingerprint(), nowUTC().Add(-24*time.Hour))
			},
		},
		{
			name:   "a GitHub that will not take the write, at the first attempt",
			status: http.StatusInternalServerError,
			body:   `{"message":"boom"}`,
		},
		{
			name:   "a GitHub that will not take the write, after collecting a dead record",
			status: http.StatusInternalServerError,
			body:   `{"message":"boom"}`,
			seed: func(_, bb *Orchestrator) string {
				return renderLandingHolder(bb.arenaFingerprint(), nowUTC().Add(-24*time.Hour))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, bb, mock := twoLandingArenas(t)
			mock.mu.Lock()
			mock.repoLabels = map[string]string{}
			if tc.seed != nil {
				mock.repoLabels["flow:landing"] = tc.seed(a, bb)
			}
			mock.refuseRepoLabelCreateStatus = tc.status
			mock.refuseRepoLabelCreateBody = tc.body
			mock.mu.Unlock()

			prevPoll := landingPoll
			landingPoll = time.Millisecond
			defer func() { landingPoll = prevPoll }()
			ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
			defer cancel()

			_, _, err := takeLanding(ctx, a, a.arena())
			if err == nil {
				t.Fatal("takeLanding = nil though the create was refused")
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("the take queued behind a refusal that is not the name being taken (%v); "+
					"it can never create this label and would wait for as long as it is given", err)
			}
		})
	}
}

// A REWRITE THAT FAILS IS AN ERROR, NOT A QUEUE. The only refusal the re-take
// may go on from is the record being gone, which is a create again. Anything
// else left to fall through would meet its own record on the create, be told
// the name is taken, and queue behind itself until the round ran out of time.
func TestTakeLanding_ARewriteRefusedIsAnErrorAndNotAQueue(t *testing.T) {
	a, _, mock := twoLandingArenas(t)
	mock.mu.Lock()
	mock.repoLabels = map[string]string{
		"flow:landing": renderLandingHolder(a.arenaFingerprint(), nowUTC()),
	}
	mock.failRepoLabelWrite = true
	mock.mu.Unlock()

	prevPoll := landingPoll
	landingPoll = time.Millisecond
	defer func() { landingPoll = prevPoll }()
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()

	_, _, err := takeLanding(ctx, a, a.arena())
	if err == nil {
		t.Fatal("takeLanding = nil though this arena's own record could not be rewritten")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the take queued behind its OWN record (%v); nothing else was ever going to release it", err)
	}
}

// A GitHub that will not answer is an error and no exclusion, never a silent
// free pass: a caller that proceeded unserialized would produce exactly the
// failure this prevents, with nothing in the report to say so.
func TestTakeLanding_UnreadableHolderIsAnError(t *testing.T) {
	a, bb, mock := twoLandingArenas(t)
	if _, _, err := takeLanding(t.Context(), a, a.arena()); err != nil {
		t.Fatalf("takeLanding: %v", err)
	}
	mock.mu.Lock()
	mock.failRepoLabelRead = true
	mock.mu.Unlock()

	if _, _, err := takeLanding(t.Context(), bb, bb.arena()); err == nil {
		t.Fatal("takeLanding = nil though who holds the mainline could not be read")
	}
}

// ---------------------------------------------------------------------------
// The collection backstop
// ---------------------------------------------------------------------------

// A ROUND THAT WAS COLLECTED REFUSES TO LAND. The bound has to be a guess about
// what a round costs, and a guess that is too short would otherwise be two
// arenas inside one round. Refused here it costs one re-measure: the work comes
// back behind whatever landed meanwhile and re-enters through the drift
// election.
func TestLanding_CollectedRoundRefusesToMerge(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)
	other := newMockedOrchestrator(t, mock, srv)
	rec := newGitRecorder()
	scriptCleanWorktree(rec)
	b.git.runner = rec.run
	ctx := t.Context()

	w := landingWorktree(t, b)
	// The round is open and ours...
	if err := b.holdLanding(ctx, b.refFromIssue(42)); err != nil {
		t.Fatalf("holdLanding: %v", err)
	}
	// ...and then it overran, was collected, and a peer took the mainline.
	mock.mu.Lock()
	mock.repoLabels["flow:landing"] = renderLandingHolder(other.arenaFingerprint(), time.Now().UTC())
	mock.mu.Unlock()

	err := w.Merge(ctx, "https://github.com/o/r/pull/1")
	if err == nil {
		t.Fatal("Merge = nil though this arena no longer held the mainline — it landed something nobody measured")
	}
	if !errors.Is(err, flow.ErrUnavailable) {
		t.Errorf("error = %v, want it to read as transient: nothing about the change was found wanting", err)
	}
	mock.mu.Lock()
	sent := append([]string(nil), mock.mutations...)
	mock.mu.Unlock()
	for _, m := range sent {
		if strings.Contains(m, "/merge") {
			t.Errorf("a merge was sent by a round that had been collected: %v", sent)
		}
	}
}

// ---------------------------------------------------------------------------
// The record's one spelling
// ---------------------------------------------------------------------------

func TestLandingHolderRecord_RoundTrips(t *testing.T) {
	at := time.Unix(1755000000, 0).UTC()
	held, got, ok := parseLandingHolder(renderLandingHolder("0123456789abcdef", at))
	if !ok {
		t.Fatal("a record this package wrote does not read back")
	}
	if held != "0123456789abcdef" {
		t.Errorf("fingerprint = %q", held)
	}
	if !got.Equal(at) {
		t.Errorf("instant = %s, want %s", got, at)
	}
}

// Every unreadable shape lands on NOT ok, which is the answer that is wrong at
// worst by one duplicated round and never by a lost mainline.
func TestLandingHolderRecord_UnreadableShapes(t *testing.T) {
	for _, desc := range []string{
		"",
		"   ",
		"0123456789abcdef",         // no instant
		"0123456789abcdef notahex", // an instant nothing can read
		" 6899a680",                // no fingerprint
	} {
		if _, _, ok := parseLandingHolder(desc); ok {
			t.Errorf("parseLandingHolder(%q) reads as a record", desc)
		}
	}
}

// The record fits the description GitHub allows, which is what keeps the holder
// writable in the same request as the name.
func TestLandingHolderRecord_FitsALabelDescription(t *testing.T) {
	got := renderLandingHolder(fingerprintArena(flow.Arena{Host: "build01", Id: "/very/long/worktree/path"}), time.Now())
	if len(got) > 100 {
		t.Errorf("the record is %d characters (%q); a label description holds 100", len(got), got)
	}
}
