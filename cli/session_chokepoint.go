package cli

import (
	"github.com/promise-language/flow"
)

// The treasurer's THIRD chokepoint: every new agent session is asked for and
// recorded rather than taken (docs/resolution.md § The treasurer).
//
// The other two stand before a spend. This one stands before a DISCARD: opening
// a session throws away context the resolution has already paid for
// (§ Nothing is bought twice), and a count reconciled afterwards says a
// conversation was thrown away once it is already gone. Asking first puts the
// machinery that decided this moment was special in front of a party that knows
// what the route declared — the same shape as refusing a prompt from a step that
// declared it would not prompt, and for the same reason: the refusal lands
// before the thing being refused has cost anything.
//
// It refuses a DECISION, never a circumstance. A session the machinery chose to
// start where the route declared none is refused; a session that had to be
// opened because the handle was gone — the substrate declined it, or the backend
// keeps none — was nobody's decision, and is approved and counted.

// recordSessionRequest files one request under its reason: durably, and on this
// dispatch's loaded item so a later read in the same dispatch sees it.
//
// A REQUEST, not an opening — a refused one opened nothing, and SessionCounts is
// what keeps the two apart.
//
// BEST-EFFORT, like the session write and the cost charge beside it. A ledger
// store that could not answer costs the count, never the prompt: failing a turn
// because a counter could not be incremented would spend the dispatch to save
// the figure that describes it.
func (s *stepCtx) recordSessionRequest(reason flow.SessionReason) {
	if err := s.app.Orchestrator.RecordSession(s.ctx, s.claim.ItemRef, reason); err != nil {
		s.Notify("", "could not record the agent session request: "+err.Error())
	}
	// The mirror, so the next classification in THIS dispatch compares against
	// what was just decided rather than against what the item held when it was
	// loaded — the same reason mirrorLedger exists for the row writes.
	// `status` reads the durable count and never this one.
	// flow.SessionCounts.Record is the one mapping, shared with both
	// orchestrators' stores.
	if s.state != nil {
		s.state.Ledger.Sessions.Record(reason)
	}
}

// sessionOpeningReason classifies a session about to be opened, against the
// route the resolution actually travelled.
//
// THE WHOLE CLASSIFICATION IS ONE COMPARISON, and it is the document's rule
// verbatim: a session the journal accounts for is the route's, and one it does
// not is the handle having been gone. Flow.SessionsAccountedFor is the single
// arithmetic — the same one `status` flags an excess with — so the party that
// approved a session and the report that judges it cannot disagree about what
// the route asked for.
//
// It never answers SessionRefused. A refusal keeps a conversation, and at this
// moment there is none to keep: the handle is already empty, so nothing is being
// discarded and there is nothing for a refusal to save. The refusal lives on the
// write path that discards a live handle (setResolutionSession).
func (s *stepCtx) sessionOpeningReason() flow.SessionReason {
	if s.state != nil && s.state.Ledger.Sessions.Opened() < s.flow.SessionsAccountedFor(s.state) {
		return flow.SessionDeclared
	}
	return flow.SessionHandleGone
}

// sessionDiscardAccountedFor reports whether discarding the handle `held` is a
// discard the route asked for.
//
// TWO TERMS, and the second is what makes a declaration account for ONE discard
// rather than for a dispatch's worth of them:
//
//   - The pending step declares `fresh`. That is the only way a new session
//     happens (docs/flow-registration.md § Session continuity), so a discard on a
//     step that declares nothing is machinery deciding this moment is special.
//   - The declaration has not been acted on yet, which the BOUNDARY says. `fresh`
//     is a property of one execution, and the boundary switch in RunOne stamps
//     the step's own id on the session the moment it takes the discard. Anything
//     asking for a second one inside that execution — or on the step's next
//     dispatch, where the boundary still stands — is asking past what the
//     declaration bought, which docs/resolution.md § The agent session names as
//     machinery starting over rather than as a declared boundary.
//
// They are the two terms the boundary switch itself branches on, which is what
// keeps the guard from refusing the one discard the route declared while still
// refusing every one it did not.
func (s *stepCtx) sessionDiscardAccountedFor(held flow.AgentSession) bool {
	return s.li.Session == flow.SessionFresh && held.Boundary != s.li.Result()
}
