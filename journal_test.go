package flow

import (
	"reflect"
	"strings"
	"testing"
)

// journalFlow is the graph every Position test derives against: an entry step
// that may hand on or finalize, an implement step that may hand back to it, and
// a wait whose route is static. Built once so each test differs only in the
// journal it hands in — which is the whole point of the derivation.
func journalFlow(t *testing.T) *Flow {
	t.Helper()
	f := NewFlow("implement", []ItemType{"task"})
	f.Role("contributor", CapPush)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        "contributor",
		Entry:       true,
		Next:        []StepId{"impl"},
		MayFinalize: []Disposition{DispositionRejected},
	})
	f.AddStep("implement", "impl", noopHandler, StepConfig{
		Prompts: PromptsAgent,
		Role:    "contributor",
		Next:    []StepId{"plan", "pr-open"},
	})
	f.AddSignalStep("open a pull request", "pr-open", noopHandler, StepConfig{
		Prompts: PromptsAgent,
		Role:    "contributor",
		Next:    []StepId{"pr-merged"},
	})
	f.AwaitSignal("wait for the merge", "pr-merged", StepConfig{
		Next: []StepId{"impl"},
	})
	// The graph the tests route over is a valid one, so a Position failure is
	// never a declaration failure wearing its clothes.
	if err := f.ValidateGraph(); err != nil {
		t.Fatalf("ValidateGraph: %v", err)
	}
	return f
}

// entryFor is one completed execution of `step` electing `route`. The fields
// Position does not read are left zero: a test that set them would suggest they
// are part of the derivation, and none of them is.
func entryFor(step StepId, route Route) JournalEntry {
	return JournalEntry{Step: step, Execution: 1, Route: route}
}

// wantPending asserts the derived position is the named pending step and not a
// finalization.
func wantPending(t *testing.T, f *Flow, it *Item, want string) {
	t.Helper()
	pos, err := f.Position(it)
	if err != nil {
		t.Fatalf("Position: %v", err)
	}
	if pos.Finalized {
		t.Fatalf("Position reports finalized (%q), want pending step %q", pos.Disposition, want)
	}
	if pos.Step.Description != want {
		t.Errorf("pending step = %q, want %q", pos.Step.Description, want)
	}
}

// wantPositionError asserts Position refused, and that the message names the
// flow and every fragment the caller gives — a refusal for the wrong reason
// fails here rather than passing as the right one.
func wantPositionError(t *testing.T, f *Flow, it *Item, fragments ...string) {
	t.Helper()
	pos, err := f.Position(it)
	if err == nil {
		t.Fatalf("Position = %+v, want an error mentioning %v", pos, fragments)
	}
	msg := err.Error()
	if !strings.Contains(msg, f.name) {
		t.Errorf("error %q does not name the flow %q", msg, f.name)
	}
	for _, want := range fragments {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func TestPosition_EmptyJournalPendsTheEntryStep(t *testing.T) {
	f := journalFlow(t)
	wantPending(t, f, &Item{}, "write plan")
}

func TestPosition_LastEntrySuccessorIsThePendingStep(t *testing.T) {
	f := journalFlow(t)
	it := &Item{Journal: []JournalEntry{
		entryFor("plan", Route{Next: "impl"}),
	}}
	wantPending(t, f, it, "implement")
}

// The journal is append-only and the LAST entry decides: an earlier election is
// the record of what was decided then, never a vote on what happens now.
func TestPosition_LastEntryDecidesWhenAStepHasSeveralEntries(t *testing.T) {
	f := journalFlow(t)
	it := &Item{Journal: []JournalEntry{
		entryFor("plan", Route{Next: "impl"}),
		{Step: "impl", Execution: 1, Route: Route{Next: "plan"}},
		{Step: "plan", Execution: 2, Route: Route{Next: "impl"}},
		{Step: "impl", Execution: 2, Route: Route{Next: "pr-open"}},
	}}
	wantPending(t, f, it, "open a pull request")
}

// Rework arrives as an ordinary election. No step is latched done, so a route
// back to a step that has already completed pends it again.
func TestPosition_RouteReturningToAStepPendsItAgain(t *testing.T) {
	f := journalFlow(t)
	it := &Item{Journal: []JournalEntry{
		entryFor("plan", Route{Next: "impl"}),
		{Step: "impl", Execution: 1, Route: Route{Next: "plan"}},
	}}
	wantPending(t, f, it, "write plan")
}

// The checklist is not an eligibility rule any more: an item whose artifact
// records are all resolved, but whose journal is empty, still pends the entry
// step. "No resolved bit, no checklist, no required flag"
// (docs/artifacts-and-signals.md § The record).
func TestPosition_ResolvedArtifactRecordsDoNotAdvanceTheJournal(t *testing.T) {
	f := journalFlow(t)
	it := &Item{
		Artifacts: map[ArtifactId]ArtifactRecord{
			"plan": resolvedArtifact("plan", ArtifactMarkdown),
			"impl": resolvedArtifact("impl", ArtifactMarkdown),
		},
		Signals: map[SignalId]SignalState{
			"pr-open":   {Set: true},
			"pr-merged": {Set: true},
		},
	}
	wantPending(t, f, it, "write plan")
}

func TestPosition_FinalizingEntryReportsFinalizedWithItsDisposition(t *testing.T) {
	f := journalFlow(t)
	it := &Item{Journal: []JournalEntry{
		entryFor("plan", Route{Finalize: DispositionRejected}),
	}}
	pos, err := f.Position(it)
	if err != nil {
		t.Fatalf("Position: %v", err)
	}
	if !pos.Finalized {
		t.Fatalf("Position = %+v, want finalized", pos)
	}
	if pos.Disposition != DispositionRejected {
		t.Errorf("disposition = %q, want %q", pos.Disposition, DispositionRejected)
	}
	if pos.Step.Description != "" {
		t.Errorf("pending step = %q, want none on a finalized position", pos.Step.Description)
	}
}

// A wait elects nothing: when the signal is observed its entry carries the one
// successor it declared, and position reads that entry like any other.
func TestPosition_SignalWaitEntryRoutesToItsDeclaredSuccessor(t *testing.T) {
	f := journalFlow(t)
	it := &Item{Journal: []JournalEntry{
		entryFor("pr-merged", Route{Next: "impl"}),
	}}
	wantPending(t, f, it, "implement")
}

// A wait is routed TO like any other lifecycle item, and the position comes
// back whole rather than as a name: the caller dispatches on Kind and waits on
// the step's result, so a position that reported only the label would hand a
// handlerless wait to the agent as though it were a step to run.
func TestPosition_RouteToASignalWaitPendsTheWaitWhole(t *testing.T) {
	f := journalFlow(t)
	it := &Item{Journal: []JournalEntry{
		entryFor("pr-open", Route{Next: "pr-merged"}),
	}}
	pos, err := f.Position(it)
	if err != nil {
		t.Fatalf("Position: %v", err)
	}
	if pos.Step.Description != "wait for the merge" {
		t.Errorf("pending step = %q, want %q", pos.Step.Description, "wait for the merge")
	}
	if pos.Step.Kind != LifecycleAwait {
		t.Errorf("pending kind = %v, want LifecycleAwait (%v)", pos.Step.Kind, LifecycleAwait)
	}
	if pos.Step.Result() != "pr-merged" {
		t.Errorf("pending result = %q, want %q", pos.Step.Result(), "pr-merged")
	}
}

func TestPosition_RefusesAnEmptyJournalWhenNoEntryIsDeclared(t *testing.T) {
	f := NewFlow("implement", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{Prompts: PromptsAgent})
	wantPositionError(t, f, &Item{}, "StepConfig{Entry: true}")
}

func TestPosition_RefusesASuccessorNamingNoRegisteredItem(t *testing.T) {
	f := journalFlow(t)
	it := &Item{Journal: []JournalEntry{
		entryFor("plan", Route{Next: "coverage"}),
	}}
	wantPositionError(t, f, it, `"plan"`, `"coverage"`, "names no registered lifecycle item")
}

func TestRoute_RefusesBothElections(t *testing.T) {
	err := Route{Next: "impl", Finalize: DispositionResolved}.Valid()
	if err == nil {
		t.Fatal("Valid() = nil, want a refusal of a route electing both")
	}
	for _, want := range []string{`"impl"`, `"resolved"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestRoute_RefusesNeitherElection(t *testing.T) {
	err := Route{}.Valid()
	if err == nil {
		t.Fatal("Valid() = nil, want a refusal of a route electing nothing")
	}
	if !strings.Contains(err.Error(), "elects nothing") {
		t.Errorf("error %q does not say the route elects nothing", err)
	}
}

func TestRoute_RefusesAnUndeclaredDisposition(t *testing.T) {
	err := Route{Finalize: "abandoned"}.Valid()
	if err == nil {
		t.Fatal("Valid() = nil, want a refusal of an undeclared disposition")
	}
	for _, want := range []string{`"abandoned"`, string(DispositionResolved)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestRoute_FinalizesReportsTheShape(t *testing.T) {
	if !(Route{Finalize: DispositionResolved}).Finalizes() {
		t.Error("a route carrying a disposition should report Finalizes")
	}
	if (Route{Next: "impl"}).Finalizes() {
		t.Error("a route carrying a successor should not report Finalizes")
	}
}

func TestItem_LastEntryOnAnEmptyJournal(t *testing.T) {
	if _, ok := (&Item{}).LastEntry(); ok {
		t.Error("LastEntry on an empty journal should report ok=false")
	}
	it := &Item{Journal: []JournalEntry{
		entryFor("plan", Route{Next: "impl"}),
		{Step: "impl", Execution: 1, Route: Route{Next: "pr-open"}},
	}}
	last, ok := it.LastEntry()
	if !ok {
		t.Fatal("LastEntry on a populated journal should report ok=true")
	}
	if last.Step != "impl" {
		t.Errorf("LastEntry().Step = %q, want %q", last.Step, "impl")
	}
}

// AccountForRole reads the binding out of the journal — the record of who ran
// every step — rather than from anything that stores it separately.
func TestAccountForRole_LatestEntryInTheRoleWins(t *testing.T) {
	it := &Item{Journal: []JournalEntry{
		{Step: "plan", By: "ann", Role: "contributor"},
		{Step: "review", By: "bo", Role: "maintainer"},
		{Step: "impl", By: "cass", Role: "contributor"},
	}}
	if got := it.AccountForRole("contributor"); got != "cass" {
		t.Errorf("AccountForRole(contributor) = %q, want the account of the latest entry in that role", got)
	}
	if got := it.AccountForRole("maintainer"); got != "bo" {
		t.Errorf("AccountForRole(maintainer) = %q, want bo", got)
	}
}

// Empty means "declared and has not acted yet" — a state a caller waits on. It
// never means "no such role": that refusal is StepCtx.RoleAccount's, against
// the flow's declaration.
func TestAccountForRole_EmptyWhenTheRoleHasNotActed(t *testing.T) {
	it := &Item{Journal: []JournalEntry{{Step: "plan", By: "ann", Role: "contributor"}}}
	if got := it.AccountForRole("maintainer"); got != "" {
		t.Errorf("AccountForRole(maintainer) = %q, want empty — the role has not acted", got)
	}
}

func TestAccountForRole_EmptyJournal(t *testing.T) {
	if got := (&Item{}).AccountForRole("contributor"); got != "" {
		t.Errorf("AccountForRole on an empty journal = %q, want empty", got)
	}
	// The empty role is nobody's: a step with no tag declares nothing, so a
	// lookup on "" must not match the entries that carry no role either.
	it := &Item{Journal: []JournalEntry{{Step: "plan", By: "ann"}}}
	if got := it.AccountForRole(""); got != "" {
		t.Errorf("AccountForRole(\"\") = %q, want empty", got)
	}
}

// --- AwaitsAfter ---
//
// The awaited marker is the one value on an entry the SDK computes, because the
// step-to-role mapping is the flow's and an orchestrator holds no flow. These
// cases pin the three answers a route can have.

func TestAwaitsAfter_RoleSuccessorAwaitsThatRole(t *testing.T) {
	f := journalFlow(t)
	got := f.AwaitsAfter(Route{Next: "impl"})
	if got.Role != "contributor" {
		t.Errorf("AwaitsAfter(->impl).Role = %q, want contributor", got.Role)
	}
	if got.Signal != "" {
		t.Errorf("AwaitsAfter(->impl).Signal = %q, want empty — a role step is nobody's signal", got.Signal)
	}
	// Never set here: the entry records a decision, and who holds the role is
	// read from the journal (AccountForRole).
	if got.Account != "" {
		t.Errorf("AwaitsAfter(->impl).Account = %q, want empty", got.Account)
	}
}

func TestAwaitsAfter_SignalWaitSuccessorAwaitsTheSignal(t *testing.T) {
	f := journalFlow(t)
	got := f.AwaitsAfter(Route{Next: "pr-merged"})
	if got.Signal != "pr-merged" {
		t.Errorf("AwaitsAfter(->pr-merged).Signal = %q, want pr-merged", got.Signal)
	}
	// A wait belongs to no role: nobody's move is not somebody else's.
	if got.Role != "" {
		t.Errorf("AwaitsAfter(->pr-merged).Role = %q, want empty", got.Role)
	}
	if got.Empty() {
		t.Error("AwaitsAfter(->pr-merged).Empty() = true, want false — the item awaits the signal")
	}
}

// A signal STEP is not a signal wait: it has a handler and a role, and the item
// awaits whoever runs it.
func TestAwaitsAfter_SignalStepAwaitsItsRole(t *testing.T) {
	f := journalFlow(t)
	got := f.AwaitsAfter(Route{Next: "pr-open"})
	if got.Role != "contributor" || got.Signal != "" {
		t.Errorf("AwaitsAfter(->pr-open) = %+v, want role=contributor with no signal", got)
	}
}

func TestAwaitsAfter_FinalizingRouteAwaitsNobody(t *testing.T) {
	f := journalFlow(t)
	got := f.AwaitsAfter(Route{Finalize: DispositionRejected})
	if !got.Empty() {
		t.Errorf("AwaitsAfter(finalize) = %+v, want the zero value — a finished flow awaits nobody", got)
	}
}

// A route naming nothing registered awaits nothing. Position refuses that route
// loudly, which is where the defect is reported; answering with a role invented
// for an id that names nothing would be worse than answering empty.
func TestAwaitsAfter_UnknownSuccessorAwaitsNobody(t *testing.T) {
	f := journalFlow(t)
	if got := f.AwaitsAfter(Route{Next: "no-such-step"}); !got.Empty() {
		t.Errorf("AwaitsAfter(->no-such-step) = %+v, want the zero value", got)
	}
}

// --- LedgerRow ---

func TestLedgerRow_GrantedOnSumsPerAxis(t *testing.T) {
	row := LedgerRow{Step: "plan", Granted: []GrantRecord{
		{Axis: AxisInvocations, Amount: 2},
		{Axis: AxisCost, Amount: 5.50},
		{Axis: AxisInvocations, Amount: 3},
	}}
	if got := row.GrantedOn(AxisInvocations); got != 5 {
		t.Errorf("GrantedOn(invocations) = %v, want 5 (2+3)", got)
	}
	if got := row.GrantedOn(AxisCost); got != 5.50 {
		t.Errorf("GrantedOn(cost) = %v, want 5.50", got)
	}
	// No extension is an extension of nothing, not an unknown.
	if got := row.GrantedOn(AxisTimeout); got != 0 {
		t.Errorf("GrantedOn(timeout) = %v, want 0 — nothing was granted on it", got)
	}
}

func TestLedger_RowOfAnUndispatchedStepIsZero(t *testing.T) {
	l := Ledger{Steps: map[StepId]LedgerRow{"plan": {Step: "plan", Dispatches: 2}}}
	if got := l.Row("impl"); !reflect.DeepEqual(got, LedgerRow{}) {
		t.Errorf("Row(impl) = %+v, want the zero row — it has never been dispatched", got)
	}
	if got := l.Row("plan").Dispatches; got != 2 {
		t.Errorf("Row(plan).Dispatches = %d, want 2", got)
	}
	// The nil map answers the same way rather than panicking: a ledger nobody
	// has written to is a ledger in which nothing has been spent.
	if got := (Ledger{}).Row("plan"); !reflect.DeepEqual(got, LedgerRow{}) {
		t.Errorf("Row on a nil ledger = %+v, want the zero row", got)
	}
}
