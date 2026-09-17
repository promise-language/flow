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
	// The mirror, so sessionOpeningReason's next comparison and `status`'s
	// excess check read what was just decided rather than what the item held
	// when it was loaded. flow.SessionCounts.Record is the one mapping, shared
	// with both orchestrators' stores.
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

// sessionDiscardAccountedFor reports whether discarding the handle the
// resolution is holding is one the route asked for.
//
// The pending step's own declaration is the whole test: `fresh` is the only way
// a new session happens, and the boundary switch in RunOne is the one write that
// acts on it (docs/flow-registration.md § Session continuity). A discard from
// anywhere else is machinery deciding this moment is special, which is what the
// third chokepoint exists to refuse.
func (s *stepCtx) sessionDiscardAccountedFor() bool {
	return s.li.Session == flow.SessionFresh
}
