package flow

import (
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
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Role:        "contributor",
		Entry:       true,
		Next:        []StepId{"impl"},
		MayFinalize: []Disposition{DispositionRejected},
	})
	f.AddStep("implement", "impl", noopHandler, StepConfig{
		Role: "contributor",
		Next: []StepId{"plan", "pr-open"},
	})
	f.AddSignalStep("open a pull request", "pr-open", noopHandler, StepConfig{
		Role: "contributor",
		Next: []StepId{"pr-merged"},
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
	if pos.Step.Name != want {
		t.Errorf("pending step = %q, want %q", pos.Step.Name, want)
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
	if pos.Step.Name != "" {
		t.Errorf("pending step = %q, want none on a finalized position", pos.Step.Name)
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
	if pos.Step.Name != "wait for the merge" {
		t.Errorf("pending step = %q, want %q", pos.Step.Name, "wait for the merge")
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
	f.AddStep("write plan", "plan", noopHandler, StepConfig{})
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
