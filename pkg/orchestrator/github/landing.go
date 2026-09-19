package github

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/promise-language/flow"
)

// The PROJECT-SCOPE exclusion: the one landing round this repository's mainline
// admits at a time (docs/gates-and-commands.md § Two scopes).
//
// WHAT IT IS FOR IS A LOOP TERMINATING, not a conflict. Landing is rebase →
// measure the merge result → land, and a landing elsewhere invalidates every
// merge result measured against the old mainline. With two arenas landing at
// once and nothing serializing them, each one's landing sends the other back to
// rebase and measure again, indefinitely — the work sound, the gate green every
// time, and nothing ever landing.
//
// THE SERIALIZED SECTION IS THE ROUND, NOT THE LANDING ACT. A lock over the act
// alone leaves the measurement open to invalidation, so it is taken at
// PrepareMergeResult — the rebase — and given back at Merge. In between sit
// RebuildTools and the integration gate, which is the expensive part this
// exists to keep from being wasted.
//
// WHY NOT pkg/hostscope. That exclusion reaches one MACHINE and is a file lock,
// so the kernel releases it when the holder dies. This one has to reach every
// machine working the same mainline, and there is no kernel in common — so the
// record lives where the mainline does. What replaces the kernel is below.
//
// WHY A LABEL. A label name is unique within a repository and its creation is
// atomic, so creating one is an atomic create-if-absent that every machine can
// see and that needs no server — which is the whole requirement. It is the
// substrate the claim race already runs on (claim.go § Claim), not a second
// mechanism invented here. The holder rides in the same request as the name, so
// taking the exclusion and saying whose it is are one act.
//
// IT IS NEVER ATTACHED TO AN ITEM. The resource being protected is the
// mainline, which is not any one item's.

// landingPoll is how often a queued round asks again.
//
// A package var so a test can shrink it; the real value is sized for a queue
// that is minutes long, because a round is. It bounds nothing — the wait is
// bounded by the caller's context and by the round bound below, and by nothing
// of its own, for the reason docs/gates-and-commands.md § Two scopes gives: a
// cap set below what a real round costs turns every busy period into false
// failures.
var landingPoll = 5 * time.Second

// landingRoundSlack is what a round costs BEYOND the one measurement inside it:
// the fetch and local merge, ./make, the merge request itself, and the
// disagreement between two machines' clocks — because the age below is read
// against the collecting arena's clock and not the holder's, the margin
// claimTokenTTL already argues for.
const landingRoundSlack = 10 * time.Minute

// landingWaitReportTimeout bounds the ledger write that files a queue, which is
// made on a context detached from the round's. See fileLandingWait.
const landingWaitReportTimeout = 30 * time.Second

// landingReleaseTimeout bounds giving the exclusion back. The release runs on a
// context of its own for the same reason the wait report does — the context
// that ended the round is often the reason the round ended — and a release with
// no bound at all could hold a dispatch open on a GitHub that has stopped
// answering.
const landingReleaseTimeout = 30 * time.Second

// landingHold is one held exclusion: the three things a holder can do with it,
// in one value.
//
// ONE VALUE AND NOT THREE FUNCTIONS, because confirm and release are questions
// about THIS acquisition and nothing else. Reached separately they could be
// answered by a different mechanism from the one that took it — which is
// exactly what a stand-in for the take would produce — and a round would then
// confirm against a record its own take never wrote.
type landingHold struct {
	// confirm reports whether the exclusion is STILL this arena's. See
	// Orchestrator.confirmLanding for why a holder has to ask.
	confirm func(ctx context.Context) error
	// release gives it back. Idempotent and safe to defer.
	release func()
}

// acquireLanding is the seam this file is tested through: a package var, the
// cacheDir and acquireHostScope pattern, and deliberately NOT an environment
// variable — an environment variable is never an input
// (docs/org/cli-guide.md § 2).
//
// Unlike acquireHostScope it is NOT redirected package-wide, because the hazard
// is different: the host-scope exclusion is the developer's own machine, and a
// test that took it would block every arena on it. This one is a label in a
// repository, and every test in this package points at a mock server — so the
// real implementation is exactly what the mechanism's own cases drive. What the
// seam is for is the other half: a test about the BRACKET (that the round is
// taken at the rebase, survives the revert, and is given back at the merge)
// says so with a stand-in, rather than by mocking four endpoints to talk about
// none of them.
var acquireLanding = takeLanding

// takeLanding blocks until this arena holds the mainline's exclusion, and
// reports how long that took. A zero wait is an exclusion that was free.
//
// holder is the arena taking it, and what is published is its FINGERPRINT —
// docs/disclosure.md closes the categories a flow may not publish and two of
// them are exactly what an arena is made of. Equality is the only operation
// this needs, which is all a digest supports.
//
// IT REFUSES RATHER THAN DEGRADING. An arena that cannot name itself, or a
// GitHub that will not answer, gets an error and no exclusion — never a silent
// free pass. The caller's obligation is the other half: a party that cannot
// take the exclusion does not land.
//
// THE WAIT IS BOUNDED BY ctx AND BY THE ROUND BOUND, AND BY NOTHING ELSE.
func takeLanding(ctx context.Context, b *Orchestrator, holder flow.Arena) (hold *landingHold, waited time.Duration, err error) {
	if holder.Empty() {
		return nil, 0, fmt.Errorf("landing: this arena cannot name itself, so it cannot hold the mainline's exclusion")
	}
	name := b.labels.Landing()
	mine := fingerprintArena(holder)

	// ASKED FOR WITHOUT WAITING FIRST, so a round that queued for nothing
	// reports nothing. The alternative is a clock around the request, which
	// would report the round trip as contention and pay a ledger write for it
	// on every single landing.
	switch err := b.out.CreateRepoLabel(ctx, name, renderLandingHolder(mine, nowUTC())); {
	case err == nil:
		return b.landingHold(name, mine), 0, nil
	case !errors.Is(err, errLabelExists):
		return nil, 0, fmt.Errorf("landing: cannot take the mainline's exclusion: %w", err)
	}

	started := nowUTC()
	for {
		// CONTENDED — BUT BY WHOM?
		desc, present, err := b.out.GetRepoLabel(ctx, name)
		if err != nil {
			return nil, nowUTC().Sub(started), fmt.Errorf("landing: cannot read who holds the mainline's exclusion: %w", err)
		}
		switch held, at, ok := parseLandingHolder(desc); {
		case !present:
			// Released between the two requests. Fall through and try again.
		case ok && held == mine:
			// OUR OWN ARENA'S, which across a round means the process that
			// began it is gone: one arena runs one round at a time, because a
			// second process in this worktree is refused outright
			// (docs/resolution.md § A claim binds worktrees, not processes),
			// and a round in flight in THIS process never reaches here —
			// holdLanding answers from what it already holds.
			//
			// So this is adopted rather than nested into: a real release, not
			// the no-op the host-scope exclusion hands a nested party. The
			// record is this arena's to give back, and a no-op here would leave
			// it standing until the bound collected it — a stop holding the
			// mainline, which is the one thing this must never do.
			//
			// THE INSTANT IS REWRITTEN AND NEVER INHERITED. The instant is what
			// the bound is measured from, and the one standing in the record is
			// the ABANDONED round's — so a round that adopted it as it stands
			// would begin with part of its bound already spent, or with none of
			// it left, and be collected from under a measurement still running.
			// That costs exactly what the exclusion exists to prevent: the
			// expensive merge-result run is thrown away, and the round comes
			// back to re-measure. This round starts now, so the record says now.
			//
			// IN PLACE, not delete-then-retake: a delete leaves the mainline
			// momentarily unheld, and a peer polling in that gap takes an
			// exclusion this arena is about to believe it holds.
			switch err := b.out.SetRepoLabelDescription(ctx, name, renderLandingHolder(mine, nowUTC())); {
			case err == nil:
				return b.landingHold(name, mine), 0, nil
			case !errors.Is(err, errLabelMissing):
				return nil, nowUTC().Sub(started), fmt.Errorf(
					"landing: cannot re-take this arena's own record of the mainline's exclusion: %w", err)
			}
			// The record went away under the rewrite. Taking it is a create
			// again, which is what the rest of this iteration does.
		case !ok || nowUTC().Sub(at) > b.landingRoundBound():
			// A HOLDER THAT CANNOT STILL BE INSIDE ITS ROUND. Not a timer on
			// the lock: the round carries its own declared bound — one gate
			// timeout plus what the rest of it costs — so a record older than
			// that belongs to an arena that parked, crashed or went quiet, and
			// a lock in a stalled arena starves every landing behind it
			// (docs/gates-and-commands.md § Two scopes). A record that says
			// nothing readable can never expire on its own and is collected for
			// the same reason an untimestamped claim token is.
			//
			// Best-effort, like every other collection here: two arenas seeing
			// the same dead record both remove it and both then race to create,
			// which the create itself settles.
			_ = b.out.DeleteRepoLabel(ctx, name)
		}

		switch err := b.out.CreateRepoLabel(ctx, name, renderLandingHolder(mine, nowUTC())); {
		case err == nil:
			return b.landingHold(name, mine), nowUTC().Sub(started), nil
		case !errors.Is(err, errLabelExists):
			return nil, nowUTC().Sub(started), fmt.Errorf("landing: cannot take the mainline's exclusion: %w", err)
		}

		select {
		case <-ctx.Done():
			// The queue this round sat in before giving up is still reported:
			// see fileLandingWait, which files it on this path too.
			return nil, nowUTC().Sub(started), fmt.Errorf(
				"landing: gave up waiting for the mainline's exclusion: %w", ctx.Err())
		case <-time.After(landingPoll):
		}
	}
}

// landingHold builds the handle for an exclusion this arena has just taken.
//
// THE RELEASE CHECKS THE RECORD IS STILL OURS BEFORE DELETING IT. A round that
// overran its bound has already been collected, and whoever holds the exclusion
// now is landing under it — deleting that would hand the mainline to a third
// arena while two are inside it. The read cannot close the window entirely (the
// collection could land between the read and the delete), but it takes the
// common case from "possible" to "requires the collection to arrive inside one
// request", and the ordinary release — a round still comfortably inside its
// bound — cannot reach it at all.
//
// THE RELEASE RUNS ON A CONTEXT OF ITS OWN, detached from the round's: the
// context that ended the round is frequently the reason it ended, and a release
// that could not run on that path would leave the mainline held by a dispatch
// that is over.
func (b *Orchestrator) landingHold(name, mine string) *landingHold {
	var once sync.Once
	return &landingHold{
		confirm: func(ctx context.Context) error {
			desc, present, err := b.out.GetRepoLabel(ctx, name)
			if err != nil {
				return fmt.Errorf("landing: cannot confirm this arena still holds the mainline's exclusion: %w", err)
			}
			if held, _, ok := parseLandingHolder(desc); present && ok && held == mine {
				return nil
			}
			return fmt.Errorf(
				"landing: the mainline's exclusion is no longer this arena's, so what was measured is not what "+
					"would land — bring the branch up and measure again: %w", flow.ErrUnavailable)
		},
		release: func() {
			once.Do(func() {
				ctx, cancel := context.WithTimeout(context.Background(), landingReleaseTimeout)
				defer cancel()
				desc, present, err := b.out.GetRepoLabel(ctx, name)
				if err != nil || !present {
					// Unreadable or already gone. Nothing to give back that
					// this call can establish is ours, and a delete on a guess
					// is the one outcome worse than a record the bound will
					// collect.
					return
				}
				if held, _, ok := parseLandingHolder(desc); ok && held != mine {
					return
				}
				_ = b.out.DeleteRepoLabel(ctx, name)
			})
		},
	}
}

// landingRoundBound is how long a round can legitimately take: the one
// integration measurement inside it, which is what cfg.GateTimeout bounds, plus
// what the rest of the round costs.
func (b *Orchestrator) landingRoundBound() time.Duration {
	return b.cfg.GateTimeout + landingRoundSlack
}

// renderLandingHolder and parseLandingHolder are the record's one spelling:
// the arena fingerprint and the instant it was taken, separated by a space, so
// the writer and the reader cannot disagree about the format. Hex seconds for
// the instant, as claim tokens spell theirs.
//
// It fits GitHub's 100-character description easily — sixteen hex digits, a
// space, and eight more — which is what keeps the holder writable in the same
// request as the name.
func renderLandingHolder(fingerprint string, at time.Time) string {
	return fmt.Sprintf("%s %08x", fingerprint, uint64(at.Unix())&0xffffffff)
}

// parseLandingHolder reads a record back. NOT ok covers an absent, truncated or
// unparseable one — and every one of those lands on "collect", which is the
// answer that is wrong at worst by one duplicated round, never by a lost
// mainline. A record nothing can read has no way to expire on its own.
func parseLandingHolder(desc string) (fingerprint string, at time.Time, ok bool) {
	f, secs, found := strings.Cut(strings.TrimSpace(desc), " ")
	if !found || f == "" {
		return "", time.Time{}, false
	}
	n, err := strconv.ParseUint(secs, 16, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	return f, time.Unix(int64(n), 0).UTC(), true
}

// ---------------------------------------------------------------------------
// The bracket
// ---------------------------------------------------------------------------

// holdLanding takes the exclusion for the round this dispatch is in, unless
// this arena is already inside one, and files whatever the queue cost.
//
// THE HOLD LIVES ON THE ORCHESTRATOR, not on the worktree: Orchestrator.Worktree
// builds a fresh *worktree per call, so a release kept there could not survive
// from the rebase to the merge — which is the entire span this exists to cover.
//
// It is idempotent within a round. The round is opened by whichever of the two
// acts comes first — PrepareMergeResult ordinarily, Merge where a caller lands
// something it did not simulate — so both take it and only the first pays.
func (b *Orchestrator) holdLanding(ctx context.Context, ref flow.ItemRef) error {
	b.mu.Lock()
	inRound := b.landingHeld != nil
	b.mu.Unlock()
	if inRound {
		return nil
	}

	hold, waited, err := acquireLanding(ctx, b, b.arena())
	// Filed on EVERY path, the refusal included. A round that queued twenty
	// minutes and then could not take the exclusion still spent twenty minutes
	// of this arena's wall clock on contention, and a figure that counted it
	// only when the round went on to land would understate contention exactly
	// where contention is worst.
	b.fileLandingWait(ctx, ref, waited)
	if err != nil {
		return fmt.Errorf(
			"the landing round holds the mainline's exclusion and it could not be taken, so nothing was landed: %w", err)
	}

	b.mu.Lock()
	if b.landingHeld != nil {
		// Another call opened the round while this one queued. DROP this hold
		// rather than releasing it: both acquisitions are this arena's, so both
		// name the SAME record — one created it and the other adopted it — and
		// releasing here would delete the mainline's record out from under a
		// round that is still running. A dropped hold is two closures over
		// nothing and leaks no resource; the hold that is kept gives the one
		// record back when the round ends.
		b.mu.Unlock()
		return nil
	}
	b.landingHeld = hold
	b.mu.Unlock()
	return nil
}

// releaseLanding ends the round. Safe to call when no round is open, which is
// what lets every stop call it without first asking.
func (b *Orchestrator) releaseLanding() {
	b.mu.Lock()
	hold := b.landingHeld
	b.landingHeld = nil
	b.mu.Unlock()
	if hold != nil {
		hold.release()
	}
}

// confirmLanding refuses when the exclusion is no longer this arena's — the
// round overran its bound, was collected, and something else has landed since.
//
// THIS IS WHAT KEEPS A COLLECTION FROM COSTING A WRONG LANDING. The bound has
// to be a judgement about what a round costs, and one that is too short is
// otherwise two arenas inside the round at once. Refused here, a round that was
// collected costs one re-measure instead: the work comes back behind whatever
// landed meanwhile and re-enters through the drift election
// (docs/orchestrator.md § Drift is evidence for judgment).
//
// It asks the HOLD rather than GitHub directly, because the question is about
// this acquisition: asked of the store it would be answered by a mechanism the
// take need not have used, and a round could confirm against a record its own
// take never wrote.
//
// A round that is no longer open cannot be confirmed, and the honest answer is
// the same refusal. holdLanding runs immediately before this on every path that
// reaches it, so the only way to find no round here is that something ended it
// in between — a park on another goroutine — and an arena that has stopped is
// exactly an arena that may not land.
//
// The refusal is TRANSIENT and says so. A wait bound is not a verdict, and
// neither is this: nothing about the change was found wanting.
func (b *Orchestrator) confirmLanding(ctx context.Context) error {
	b.mu.Lock()
	hold := b.landingHeld
	b.mu.Unlock()
	if hold == nil {
		return fmt.Errorf(
			"landing: no landing round is open, so nothing establishes this arena may land: %w", flow.ErrUnavailable)
	}
	return hold.confirm(ctx)
}

// fileLandingWait reports a queue to the ledger AS WAITING.
//
// The orchestrator files it itself, which docs/orchestrator.md § Ledger is
// explicit about: "Reported by the party that held the wait, the only party
// that knows it was one: an orchestrator whose Push waited its turn reports
// that wait here." The host-scope wait travels the other way — out on
// GateRun.Waited, filed by cli/worktree_waiting.go — because a Worktree is
// addressed by ItemRef and has no step to key a ledger row by. This one has
// one: RecordDispatch named the pending step at the head of the dispatch, and
// the round happens inside it.
//
// IT WRITES ON A DETACHED CONTEXT, the reason cli's fileWait gives (#435): the
// largest wait this is ever handed is the one the round's own deadline produced
// while it sat in the queue, and filing through that context would drop
// precisely the figure that says why the round gave up.
//
// A failed write is dropped rather than turned into a landing failure, the same
// way stampResult drops AddDuration: losing the accounting must not lose the
// act it accounts for.
func (b *Orchestrator) fileLandingWait(ctx context.Context, ref flow.ItemRef, d time.Duration) {
	if d <= 0 {
		return
	}
	step, ok := b.pendingStepFor(ref)
	if !ok {
		// Nothing dispatched this in this process — a caller driving the
		// worktree surface directly. There is no row to key the figure by, and
		// inventing one would file contention against a step that never ran.
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), landingWaitReportTimeout)
	defer cancel()
	_ = b.AddWaiting(ctx, ref, step, d)
}

// notePendingStep remembers which step this dispatch is, so a wait the
// orchestrator holds can be filed against the row that incurred it.
//
// It is not a second copy of anything: RecordDispatch is handed the step by the
// caller at the head of every dispatch, and this keeps the argument it was
// given rather than deriving the step from somewhere else.
func (b *Orchestrator) notePendingStep(ref flow.ItemRef, step flow.StepId) {
	n, err := b.issueNumber(ref)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pendingStep == nil {
		b.pendingStep = map[int]flow.StepId{}
	}
	b.pendingStep[n] = step
}

func (b *Orchestrator) pendingStepFor(ref flow.ItemRef) (flow.StepId, bool) {
	n, err := b.issueNumber(ref)
	if err != nil {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	step, ok := b.pendingStep[n]
	return step, ok
}
