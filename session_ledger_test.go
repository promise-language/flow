package flow

import (
	"slices"
	"testing"
)

// ---------------------------------------------------------------------------
// The treasurer's fourth ledger column and the arithmetic it is judged against.
//
// The count answers HOW MANY conversations a resolution bought; the reason a
// request was filed under answers WHY. What the route asked for is read off the
// JOURNAL and never off the graph — a route may cross the same declaring step
// more than once, so the graph offers no static total (docs/resolution.md § The
// treasurer).
// ---------------------------------------------------------------------------

func TestSessionReason_IsClosedAtThree(t *testing.T) {
	want := []SessionReason{SessionDeclared, SessionHandleGone, SessionRefused}
	if got := AllSessionReasons(); !slices.Equal(got, want) {
		t.Errorf("AllSessionReasons() = %v, want %v", got, want)
	}
	for _, r := range want {
		if !r.Valid() {
			t.Errorf("%q is in AllSessionReasons but Valid() is false", r)
		}
	}
	for _, r := range []SessionReason{"", "fresh", "continued", "opened", "Declared"} {
		if r.Valid() {
			t.Errorf("%q.Valid() = true, want false — the set is closed at three", r)
		}
	}
}

func TestSessionCounts_RecordFilesUnderTheReason(t *testing.T) {
	var s SessionCounts
	s.Record(SessionDeclared)
	s.Record(SessionDeclared)
	s.Record(SessionHandleGone)
	s.Record(SessionRefused)
	if s.Declared != 2 || s.HandleGone != 1 || s.Refused != 1 {
		t.Fatalf("counts = %+v, want {Declared:2 HandleGone:1 Refused:1}", s)
	}
	// A refusal opened nothing, which is the whole reason it is not summed.
	if s.Opened() != 3 {
		t.Errorf("Opened() = %d, want 3 — a refused request opened no session", s.Opened())
	}
}

// An unrecognised reason moves nothing. A count is a claim about what was
// spent, and inventing a bucket for a reason this version does not know would
// report a figure nothing can account for.
func TestSessionCounts_AnUnknownReasonMovesNothing(t *testing.T) {
	s := SessionCounts{Declared: 1, HandleGone: 2, Refused: 3}
	before := s
	s.Record("")
	s.Record("machinery-chose")
	if s != before {
		t.Errorf("counts = %+v after two unknown reasons, want %+v unchanged", s, before)
	}
}

// ---------------------------------------------------------------------------
// SessionsAccountedFor
// ---------------------------------------------------------------------------

// sessionFlow builds a three-step route: entry → middle → last, with the
// session policy of each supplied by the caller. Every step may finalize, so a
// journal can stop anywhere.
func sessionFlow(t *testing.T, entry, middle, last SessionPolicy) *Flow {
	t.Helper()
	f := NewFlow("sessions", nil)
	f.Role(soloRole, CapPush)
	handler := func(StepCtx) (StepResult, error) { return StepResult{}, nil }
	// The entry step may not SAY `continued` — nothing precedes it, so the
	// declaration is refused at registration — but it inherits it by saying
	// nothing, which is the shape a real graph has.
	if entry == SessionContinued {
		entry = ""
	}
	f.AddStep("entry", "plan", handler, StepConfig{
		Role: soloRole, Entry: true, Prompts: PromptsAgent, Session: entry,
		Next: []StepId{"implementation"},
	})
	f.AddStep("middle", "implementation", handler, StepConfig{
		Role: soloRole, Prompts: PromptsAgent, Session: middle,
		Next: []StepId{"review"},
	})
	f.AddStep("last", "review", handler, StepConfig{
		Role: soloRole, Prompts: PromptsAgent, Session: last,
		Next: []StepId{"plan"}, MayFinalize: []Disposition{DispositionResolved},
	})
	if err := f.ValidateGraph(); err != nil {
		t.Fatalf("ValidateGraph: %v", err)
	}
	return f
}

// sessionRing is sessionFlow's declared route, so a journal's last entry elects
// the successor the graph actually declares rather than an arbitrary step —
// otherwise the pending step, which SessionsAccountedFor reads, is the fixture's
// accident rather than the route's.
var sessionRing = map[StepId]StepId{
	"plan":           "implementation",
	"implementation": "review",
	"review":         "plan",
}

// journalOf builds a journal of completed executions: one entry per step named,
// each electing the one after it and the last electing its declared successor.
func journalOf(steps ...StepId) []JournalEntry {
	var out []JournalEntry
	seen := map[StepId]int{}
	for i, s := range steps {
		seen[s]++
		e := JournalEntry{Step: s, Execution: seen[s]}
		if i+1 < len(steps) {
			e.Route = Route{Next: steps[i+1]}
		} else {
			e.Route = Route{Next: sessionRing[s]}
		}
		out = append(out, e)
	}
	return out
}

// An unstarted route asks for the entry's one WHATEVER THE ENTRY DECLARES. With
// an empty journal the pending step IS the entry, so the pending term and the
// leading one are the same session — counting both would have the chokepoint
// approve a second opening in the entry's own dispatch as the route's, and would
// have `status` report a route asking for two conversations on a graph that has
// one step on it so far.
func TestSessionsAccountedFor_UnstartedRouteAsksForTheEntrysOne(t *testing.T) {
	for _, entry := range []SessionPolicy{SessionContinued, SessionFresh} {
		f := sessionFlow(t, entry, SessionContinued, SessionContinued)
		if got := f.SessionsAccountedFor(&Item{}); got != 1 {
			t.Errorf("SessionsAccountedFor(empty journal, entry %q) = %d, want 1 — the entry's", entry, got)
		}
	}
}

// A route of continuing steps asks for exactly one conversation however far it
// travels. That is the default the whole graph inherits.
func TestSessionsAccountedFor_AContinuingRouteAsksForOne(t *testing.T) {
	f := sessionFlow(t, SessionContinued, SessionContinued, SessionContinued)
	it := &Item{Journal: journalOf("plan", "implementation", "review")}
	if got := f.SessionsAccountedFor(it); got != 1 {
		t.Errorf("SessionsAccountedFor = %d, want 1", got)
	}
}

// A `fresh` step the route has already crossed adds one.
func TestSessionsAccountedFor_ACrossedFreshStepAddsOne(t *testing.T) {
	f := sessionFlow(t, SessionContinued, SessionFresh, SessionContinued)
	it := &Item{Journal: journalOf("plan", "implementation")}
	// The entry's one, plus the crossed `fresh` middle. The pending step
	// (`review`) continues, so it adds nothing.
	if got := f.SessionsAccountedFor(it); got != 2 {
		t.Errorf("SessionsAccountedFor = %d, want 2", got)
	}
}

// THE ENTRY IS NEVER COUNTED TWICE. Its first execution IS the resolution's
// first session, whether it declares `fresh` or inherits `continued` — so a
// journal whose first entry is a declaring entry step still asks for one.
func TestSessionsAccountedFor_AFreshEntryIsNotCountedTwice(t *testing.T) {
	f := sessionFlow(t, SessionFresh, SessionContinued, SessionContinued)
	it := &Item{Journal: journalOf("plan", "implementation")}
	if got := f.SessionsAccountedFor(it); got != 1 {
		t.Errorf("SessionsAccountedFor = %d, want 1 — the entry's session is the leading one", got)
	}
}

// The execution IN FLIGHT has opened its session and has no journal entry yet,
// so the pending step's declaration counts — and stops counting once its entry
// lands, with the total unchanged across the boundary.
func TestSessionsAccountedFor_ThePendingStepCounts(t *testing.T) {
	f := sessionFlow(t, SessionContinued, SessionFresh, SessionContinued)
	pending := &Item{Journal: journalOf("plan")} // elects `implementation`, which is fresh
	if got := f.SessionsAccountedFor(pending); got != 2 {
		t.Fatalf("SessionsAccountedFor(pending fresh) = %d, want 2", got)
	}
	landed := &Item{Journal: journalOf("plan", "implementation")}
	if got := f.SessionsAccountedFor(landed); got != 2 {
		t.Errorf("SessionsAccountedFor(after it landed) = %d, want 2 — the total must not move", got)
	}
}

// A ROUTE MAY CROSS THE SAME DECLARING STEP MANY TIMES, which is why the count
// is read off the journal: the graph here has three steps and this resolution
// asks for four conversations.
func TestSessionsAccountedFor_ACycleAsksForOnePerCrossing(t *testing.T) {
	f := sessionFlow(t, SessionContinued, SessionContinued, SessionFresh)
	it := &Item{Journal: journalOf(
		"plan", "implementation", "review",
		"plan", "implementation", "review",
		"plan", "implementation", "review",
	)}
	// The entry's one, plus one per crossing of `review`. The pending step is
	// `implementation`, which continues.
	if got := f.SessionsAccountedFor(it); got != 4 {
		t.Errorf("SessionsAccountedFor = %d, want 4 — one per crossing of the declaring step", got)
	}
}

// A finalized resolution asks for nothing more: there is no execution in
// flight, so the pending term must not fire on a route that ended.
func TestSessionsAccountedFor_AFinalizedRouteHasNoPendingTerm(t *testing.T) {
	f := sessionFlow(t, SessionContinued, SessionContinued, SessionFresh)
	it := &Item{Journal: []JournalEntry{
		{Step: "plan", Execution: 1, Route: Route{Next: "review"}},
		{Step: "review", Execution: 1, Route: Route{Finalize: DispositionResolved}},
	}}
	if got := f.SessionsAccountedFor(it); got != 2 {
		t.Errorf("SessionsAccountedFor(finalized) = %d, want 2", got)
	}
}

// AN UPPER BOUND, NEVER A FALSE ACCUSATION. A journal naming a step this flow
// does not register, and a route Position refuses, contribute nothing rather
// than failing — a flag saying a resolution bought conversations it did not is
// worse than missing one.
func TestSessionsAccountedFor_AnUnreadableRouteDegrades(t *testing.T) {
	f := sessionFlow(t, SessionContinued, SessionFresh, SessionContinued)
	unregistered := &Item{Journal: []JournalEntry{
		{Step: "plan", Execution: 1, Route: Route{Next: "implementation"}},
		{Step: "a step this flow never registered", Execution: 1, Route: Route{Next: "review"}},
	}}
	if got := f.SessionsAccountedFor(unregistered); got != 1 {
		t.Errorf("SessionsAccountedFor(unregistered step) = %d, want 1", got)
	}
	refused := &Item{Journal: []JournalEntry{
		{Step: "plan", Execution: 1, Route: Route{Next: "nothing declares this"}},
	}}
	if got := f.SessionsAccountedFor(refused); got != 1 {
		t.Errorf("SessionsAccountedFor(route Position refuses) = %d, want 1", got)
	}
}

func TestSessionsAccountedFor_NilsAnswerZero(t *testing.T) {
	f := sessionFlow(t, SessionContinued, SessionContinued, SessionContinued)
	if got := f.SessionsAccountedFor(nil); got != 0 {
		t.Errorf("SessionsAccountedFor(nil item) = %d, want 0", got)
	}
	var nilFlow *Flow
	if got := nilFlow.SessionsAccountedFor(&Item{}); got != 0 {
		t.Errorf("(*Flow)(nil).SessionsAccountedFor = %d, want 0", got)
	}
}
