package flow

import (
	"fmt"
	"time"
)

// The journal is the durable record of the route: an append-only sequence of
// completed step executions, and the only thing position is ever derived from
// (docs/resolution.md § The journal). This file is the model — one type per
// thing an entry carries; Flow.Position is what reads it.
//
// Only COMPLETION appends. A park, a skip, a failure, an interruption — none of
// them is an entry; each is recorded beside the journal, and the journal reads
// the same before and after it.

// Route is the election one entry carries: where the flow goes after the
// execution that entry records. Exactly one field is set — a step either hands
// the item to a successor or ends the flow, and an election means nothing else
// (docs/resolution.md § Routing).
//
// What may be elected is declared at registration — StepConfig.Next and
// StepConfig.MayFinalize — and validated whole at startup (Flow.ValidateGraph).
// This type is the runtime election within that declaration, not a second
// declaration of it.
type Route struct {
	// Next is the elected successor, named by its result id — a step's identity
	// everywhere.
	Next StepId
	// Finalize is the disposition the flow ends with. Set only by a finalizing
	// election, which is the one way a flow completes: there is no completion
	// test beside the route (docs/flow-registration.md § Routing and
	// completion).
	Finalize Disposition
}

// Finalizes reports whether this route ends the flow.
func (r Route) Finalizes() bool { return r.Finalize != "" }

// Valid returns nil when the route is one of the two shapes an election has,
// and an error naming what is wrong otherwise.
//
// An error rather than a bool: a route is checked where a decision is being
// recorded, and "invalid" alone would leave the caller to describe a refusal it
// cannot see the reason for.
func (r Route) Valid() error {
	switch {
	case r.Next != "" && r.Finalize != "":
		return fmt.Errorf("route elects both successor %q and finalization %q: an election is one or the other", r.Next, r.Finalize)
	case r.Next == "" && r.Finalize == "":
		return fmt.Errorf("route elects nothing: an entry carries either a successor or a finalization disposition")
	case r.Finalize != "" && !r.Finalize.Valid():
		return fmt.Errorf("route finalizes as %q, which is not one of %v", r.Finalize, AllDispositions())
	}
	return nil
}

// Spend is what one execution cost. ACTIVE time only — time blocked on a
// declared exclusion is recorded apart from work, as evidence about contention
// rather than about the work (docs/resolution.md § The treasurer), and that
// belongs to the ledger.
type Spend struct {
	CostUSD  float64
	Duration time.Duration
}

// JournalEntry is one completed step execution.
//
// An execution spans from the first dispatch of the pending step, across any
// parks and resumes, to the completion that appends it: several dispatches may
// serve one execution, and only the completion writes.
//
// The entry carries NO result-kind discriminator. Whether a step produces an
// artifact or a signal is already in the flow's declaration, and an entry read
// beside the flow that recorded it needs no second copy of that — the same
// argument StepId makes for merging the two id namespaces. The wire's `type`
// field (docs/github-schema.md § Journal entries) exists because a wire reader
// has no flow; it is derived at write time.
//
// Nor does it carry the AWAITED marker yet — the successor's role, or the
// signal when the successor is a wait (docs/orchestrator.md § Writing
// payloads). That one is not derivable by its reader: the orchestrator has no
// flow, so the SDK must hand it the value for the `flow:awaits:<…>` label it
// maintains. It lands with the write path that needs it, AppendEntry (#239);
// here, where the entry is only read back, there is nothing to compute it from
// and nothing that would read it.
type JournalEntry struct {
	// Step is the step's result id — a step's identity everywhere.
	Step StepId
	// Execution is which completed execution of this step this is, 1-based. A
	// step the route reaches again appends again, and the later entry's result
	// stands as the step's current one.
	Execution int
	// Result is the captured artifact value; the zero value on a signal step or
	// a signal wait, whose result is the observation itself.
	Result ArtifactBody
	// Route is the election: the successor, or finalization with its
	// disposition.
	Route Route
	// Message is why the successor is being run, from the step that decided —
	// or, on a finalizing entry, the closing reasons.
	Message string
	// Note is an optional standing note, addressed to every subsequent step
	// rather than only the next one. It informs; it never binds.
	Note string
	// By is the account that ran the step, and Role the declared role it acted
	// in.
	By   AccountId
	Role RoleName
	// At is when the execution completed.
	At time.Time
	// Spend is what the execution cost.
	Spend Spend
}

// LastEntry returns the journal's last entry; ok==false on an empty journal.
//
// The append-only journal's last entry is what position, the message to the
// pending step and the awaited role are all read from, so "the last entry
// decides" is written down once, here, rather than at each reader.
func (i *Item) LastEntry() (JournalEntry, bool) {
	if i == nil || len(i.Journal) == 0 {
		return JournalEntry{}, false
	}
	return i.Journal[len(i.Journal)-1], true
}

// AccountForRole returns the account of record for a role on this item: the By
// of the last entry appended in that role, and the empty account when the role
// has not acted.
//
// The journal decides who acted — "the binding is read from the journal, which
// already carries who ran every step" (docs/resolution.md § Whose move it is) —
// and it decides here only, so a route returning to a role and a handler asking
// who holds it read one answer.
//
// The empty account means "declared and not yet acted", which is a state a
// caller waits on. It never means "no such role": a role reference is checked
// against the flow's declaration before it reaches this (StepCtx.RoleAccount →
// Flow.DeclaresRole), so an unknown name is refused rather than answered empty.
func (i *Item) AccountForRole(role RoleName) AccountId {
	if i == nil || role == "" {
		return ""
	}
	for k := len(i.Journal) - 1; k >= 0; k-- {
		if i.Journal[k].Role == role {
			return i.Journal[k].By
		}
	}
	return ""
}
