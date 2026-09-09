package github

import (
	"testing"
	"time"

	"github.com/promise-language/flow"
)

// The journal end-to-end: what AppendEntry writes to the state comment, and
// what loadItem reads back out of it. The round trip through the wire is
// state_comment_test.go's; this is the orchestrator's half.

// storedDoc returns the state document as it stands on the issue, failing the
// test when there is none. Read from the mock's own comments rather than
// through Load, so a test can tell "the document says so" from "Load says so".
func storedDoc(t *testing.T, mock *ghMock) *stateDoc {
	t.Helper()
	mock.mu.Lock()
	defer mock.mu.Unlock()
	for _, c := range mock.comments {
		doc, _, found, err := extractStateDoc(c.Body)
		if err != nil {
			t.Fatalf("extractStateDoc: %v", err)
		}
		if found && doc != nil {
			return doc
		}
	}
	t.Fatal("no state comment on the issue")
	return nil
}

// newJournalEnv claims issue 42 with nothing recorded on it.
func newJournalEnv(t *testing.T) (*ghMock, *Orchestrator, flow.Claim) {
	t.Helper()
	mock := newGHMock(t)
	srv := mock.server()
	t.Cleanup(srv.Close)
	b := newMockedOrchestrator(t, mock, srv)
	claim, err := b.Claim(t.Context(), b.refFromIssue(42), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return mock, b, claim
}

// Entries land in order and Load returns exactly what was appended: the pending
// step is derived from the last one, so a journal read short or reordered
// re-routes the item.
func TestBackend_AppendEntry_LoadReturnsEntriesInOrder(t *testing.T) {
	mock, b, claim := newJournalEnv(t)
	ctx := t.Context()

	appendResult(t, b, claim.ItemRef, "plan", 1, flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the plan"})
	appendResult(t, b, claim.ItemRef, "impl", 1, flow.ArtifactBody{Type: flow.ArtifactCommitHash, CommitHash: "0123456789abcdef0123456789abcdef01234567"})
	appendResult(t, b, claim.ItemRef, "plan", 2, flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the revised plan"})

	state, err := b.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []struct {
		step flow.StepId
		exec int
	}{{"plan", 1}, {"impl", 1}, {"plan", 2}}
	if len(state.Journal) != len(want) {
		t.Fatalf("journal has %d entries, want %d: %+v", len(state.Journal), len(want), state.Journal)
	}
	for i, w := range want {
		if state.Journal[i].Step != w.step || state.Journal[i].Execution != w.exec {
			t.Errorf("entry %d = %s/%d, want %s/%d",
				i, state.Journal[i].Step, state.Journal[i].Execution, w.step, w.exec)
		}
	}
	// And the document itself carries them — Load is not reconstructing.
	if doc := storedDoc(t, mock); len(doc.Journal) != len(want) {
		t.Errorf("state comment carries %d entries, want %d", len(doc.Journal), len(want))
	}
}

// A second append never rewrites the first. The earlier entry is the record of
// what was decided then, and the later one's result is what the step stands at.
func TestBackend_AppendEntry_SecondAppendDoesNotRewriteTheFirst(t *testing.T) {
	_, b, claim := newJournalEnv(t)
	ctx := t.Context()

	appendMarkdown(t, b, claim.ItemRef, "plan", "first")
	appendResult(t, b, claim.ItemRef, "plan", 2, flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "second"})

	state, err := b.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Journal) != 2 {
		t.Fatalf("journal has %d entries, want 2", len(state.Journal))
	}
	if state.Journal[0].Execution != 1 || state.Journal[1].Execution != 2 {
		t.Errorf("executions = %d, %d; want 1, 2", state.Journal[0].Execution, state.Journal[1].Execution)
	}
	if got := state.Artifact("plan").Version; got != 2 {
		t.Errorf("projection Version = %d, want 2 — the latest entry's result stands", got)
	}
	if got := state.Artifact("plan").Markdown; got != "second" {
		t.Errorf("projection = %q, want %q", got, "second")
	}
}

// The projection and the flow binding are set in the SAME round as the entry:
// one document update, so a reader can never see a captured artifact whose
// route was not recorded.
func TestBackend_AppendEntry_ProjectionAndFlowLandWithTheEntry(t *testing.T) {
	mock, b, claim := newJournalEnv(t)

	before := storedDocOrNil(mock)
	if before != nil && (len(before.Journal) != 0 || before.Flow != "") {
		t.Fatalf("pre-append document = %+v, want nothing recorded", before)
	}

	appendMarkdown(t, b, claim.ItemRef, "plan", "the plan")

	doc := storedDoc(t, mock)
	if len(doc.Journal) != 1 {
		t.Fatalf("journal = %+v, want the one entry", doc.Journal)
	}
	if doc.Flow != "implement" {
		t.Errorf("flow = %q, want implement — the first entry binds it", doc.Flow)
	}
	if len(doc.Artifacts) != 1 || doc.Artifacts[0].Id != "plan" || !doc.Artifacts[0].Resolved {
		t.Errorf("artifacts = %+v, want the projection of the entry", doc.Artifacts)
	}
}

// storedDocOrNil is storedDoc without the failure, for asserting that nothing
// is recorded yet.
func storedDocOrNil(mock *ghMock) *stateDoc {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	for _, c := range mock.comments {
		if doc, _, found, err := extractStateDoc(c.Body); err == nil && found {
			return doc
		}
	}
	return nil
}

// A signal step's entry carries no body: its result is the observation itself,
// so nothing is published and no artifact is projected — but the entry lands.
func TestBackend_AppendEntry_SignalEntryProjectsNoArtifact(t *testing.T) {
	mock, b, claim := newJournalEnv(t)

	if err := b.AppendEntry(t.Context(), claim.ItemRef, flow.JournalEntry{
		Step: "pr-open", Execution: 1,
		Route:  flow.Route{Next: "pr-merged"},
		Awaits: flow.Awaits{Signal: "pr-merged"},
		By:     "ann", Role: "contributor",
	}); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}

	doc := storedDoc(t, mock)
	if len(doc.Journal) != 1 || doc.Journal[0].Type != journalSignalType {
		t.Fatalf("journal = %+v, want one entry typed %q", doc.Journal, journalSignalType)
	}
	if len(doc.Artifacts) != 0 {
		t.Errorf("artifacts = %+v, want none — a signal result is the observation", doc.Artifacts)
	}
	state, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// An awaited signal is nobody's move, and the marker says so.
	if state.Awaits.Signal != "pr-merged" || state.Awaits.Role != "" {
		t.Errorf("Awaits = %+v, want the signal with no role", state.Awaits)
	}
}

func TestBackend_AppendEntry_RefusesAnEntryNamingNoStep(t *testing.T) {
	_, b, claim := newJournalEnv(t)
	err := b.AppendEntry(t.Context(), claim.ItemRef, flow.JournalEntry{Execution: 1})
	if err == nil {
		t.Fatal("AppendEntry with no step = nil, want a refusal")
	}
}

// loadItem populates everything derived from the journal in one read: the
// entries, the ledger, the awaited marker with its account of record, the flow
// binding, and the creator — which comes off the ISSUE, not the caller.
func TestBackend_LoadItem_PopulatesTheDerivedFields(t *testing.T) {
	_, b, claim := newJournalEnv(t)
	ctx := t.Context()

	first := resultEntry("plan", 1, flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the plan"})
	first.By, first.Role = "ann", "contributor"
	first.Awaits = flow.Awaits{Role: "reviewer"}
	if err := b.AppendEntry(ctx, claim.ItemRef, first); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	second := resultEntry("review", 1, flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "looks fine"})
	second.By, second.Role = "bo", "reviewer"
	second.Awaits = flow.Awaits{Role: "contributor"}
	if err := b.AppendEntry(ctx, claim.ItemRef, second); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	if err := b.AddCost(ctx, claim.ItemRef, "plan", 2.50); err != nil {
		t.Fatalf("AddCost: %v", err)
	}

	state, err := b.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Journal) != 2 {
		t.Fatalf("journal has %d entries, want 2", len(state.Journal))
	}
	if state.Ledger.Row("plan").CostUSD != 2.50 {
		t.Errorf("ledger row = %+v, want $2.50 on plan", state.Ledger.Row("plan"))
	}
	// The awaited role, with the account of record read back out of the
	// journal — never stored beside it.
	if state.Awaits.Role != "contributor" || state.Awaits.Account != "ann" {
		t.Errorf("Awaits = %+v, want contributor held by ann", state.Awaits)
	}
	if state.Flow != "implement" {
		t.Errorf("Flow = %q, want implement", state.Flow)
	}
	// carol filed the issue; alice is the operator. The creator is the filer.
	if state.Creator != "carol" {
		t.Errorf("Creator = %q, want carol — the account that filed the item", state.Creator)
	}
	if state.Finalized || state.FinalizedAs != "" {
		t.Errorf("finalized = %v / %q on an open item, want false / empty", state.Finalized, state.FinalizedAs)
	}
}

// Finalize records the disposition the finalizing election carried, and Load
// reports it: the read is required with the write.
func TestBackend_Finalize_RecordsTheDisposition(t *testing.T) {
	mock, b, claim := newJournalEnv(t)
	ctx := t.Context()

	appendMarkdown(t, b, claim.ItemRef, "plan", "the plan")
	mock.mu.Lock()
	mock.issueState = "closed"
	mock.mu.Unlock()

	if err := b.Finalize(ctx, claim.ItemRef, flow.DispositionRejected); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	state, err := b.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !state.Finalized {
		t.Error("Finalized = false after Finalize")
	}
	if state.FinalizedAs != flow.DispositionRejected {
		t.Errorf("FinalizedAs = %q, want rejected", state.FinalizedAs)
	}
	// A finished flow awaits nobody, whatever the last entry's marker said.
	if !state.Awaits.Empty() {
		t.Errorf("Awaits = %+v on a finalized item, want the zero value", state.Awaits)
	}
}

// Reset clears the flow's whole record: journal, ledger, park, projection, the
// flow binding and the finalization.
func TestBackend_Reset_ClearsTheFlowsRecord(t *testing.T) {
	mock, b, claim := newJournalEnv(t)
	ctx := t.Context()

	appendMarkdown(t, b, claim.ItemRef, "plan", "the plan")
	if err := b.RecordDispatch(ctx, claim.ItemRef, "plan"); err != nil {
		t.Fatalf("RecordDispatch: %v", err)
	}
	if err := b.AddCost(ctx, claim.ItemRef, "plan", 4.25); err != nil {
		t.Fatalf("AddCost: %v", err)
	}
	if err := b.Park(ctx, claim.ItemRef, flow.ParkRequest{
		Kind: flow.ParkRefused, Step: "impl", Reason: "deterministic",
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}

	if err := b.Reset(ctx, claim.ItemRef); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	state, err := b.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Journal) != 0 {
		t.Errorf("journal = %+v after Reset, want empty", state.Journal)
	}
	if len(state.Ledger.Steps) != 0 || state.Ledger.TotalCostUSD != 0 {
		t.Errorf("ledger = %+v after Reset, want empty", state.Ledger)
	}
	if state.Parked() {
		t.Errorf("park = %+v after Reset, want cleared", state.Park)
	}
	if len(state.Artifacts) != 0 {
		t.Errorf("artifacts = %+v after Reset, want none", state.Artifacts)
	}
	if !state.Awaits.Empty() {
		t.Errorf("Awaits = %+v after Reset, want the zero value", state.Awaits)
	}
	if state.Finalized || state.FinalizedAs != "" {
		t.Errorf("finalized = %v / %q after Reset, want false / empty", state.Finalized, state.FinalizedAs)
	}
	// The document says so too, and the park label went with the record.
	doc := storedDoc(t, mock)
	if len(doc.Journal) != 0 || len(doc.Artifacts) != 0 || doc.Park != nil {
		t.Errorf("state comment after Reset = %+v, want the flow's record gone", doc)
	}
}

// Reset on an item with no record at all is a no-op rather than an error:
// there is nothing to clear, which is the state Reset is trying to reach.
func TestBackend_Reset_OnAnItemWithNoRecord(t *testing.T) {
	_, b, claim := newJournalEnv(t)
	if err := b.Reset(t.Context(), claim.ItemRef); err != nil {
		t.Fatalf("Reset on an item with no record: %v", err)
	}
}

// --- The role predicate on the listing ---

// awaitingRoleEnv claims the item and leaves it awaiting `role`.
func awaitingRoleEnv(t *testing.T, role flow.RoleName) (*ghMock, *Orchestrator, flow.Claim) {
	t.Helper()
	mock, b, claim := newJournalEnv(t)
	e := resultEntry("plan", 1, flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the plan"})
	e.Awaits = flow.Awaits{Role: role}
	e.By, e.Role = "ann", "contributor"
	e.At = time.Date(2026, 5, 26, 15, 0, 0, 0, time.UTC)
	if err := b.AppendEntry(t.Context(), claim.ItemRef, e); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	return mock, b, claim
}

// An item awaiting a role this account cannot assume sits at `awaits`: it is
// still this binary's work — above outside-remit — and out of `actionable`.
func TestBackend_Get_AwaitsWhenTheRoleIsNotAssumable(t *testing.T) {
	_, b, claim := awaitingRoleEnv(t, "maintainer")

	onlyContributor := func(r flow.RoleName) bool { return r == "contributor" }
	info, err := b.Get(t.Context(), claim.ItemRef, "implement", nil, onlyContributor)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Availability != flow.AvailAwaits {
		t.Errorf("Availability = %q, want awaits", info.Availability)
	}
	if info.Awaits.Role != "maintainer" || info.Awaits.Account != "" {
		t.Errorf("Awaits = %+v, want maintainer with no account of record", info.Awaits)
	}
	if !info.Availability.InScope(flow.ScopeProcessable) || info.Availability.InScope(flow.ScopeActionable) {
		t.Errorf("%q sits at the wrong rung: processable=%v actionable=%v",
			info.Availability,
			info.Availability.InScope(flow.ScopeProcessable),
			info.Availability.InScope(flow.ScopeActionable))
	}
}

func TestBackend_Get_OutsideRemitWhenTheTypeIsNotAccepted(t *testing.T) {
	_, b, claim := newJournalEnv(t)
	acceptsNone := func(flow.ItemType) bool { return false }
	info, err := b.Get(t.Context(), claim.ItemRef, "implement", acceptsNone, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Availability != flow.AvailOutsideRemit {
		t.Errorf("Availability = %q, want outside-remit", info.Availability)
	}
	// Below awaits: an item outside the remit is not this binary's at all,
	// while an awaits item is this binary's and somebody else's move.
	if info.Availability.InScope(flow.ScopeProcessable) {
		t.Error("an outside-remit item must not be processable")
	}
}

// A nil predicate filters nothing, matching the acceptsType convention.
func TestBackend_Get_NilRolePredicateFiltersNothing(t *testing.T) {
	_, b, claim := awaitingRoleEnv(t, "maintainer")
	info, err := b.Get(t.Context(), claim.ItemRef, "implement", nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Availability == flow.AvailAwaits {
		t.Error("a nil predicate reported awaits; it must filter nothing")
	}
}

// Eligibility, not a sort key: an item whose awaited role this account cannot
// assume is ABSENT from the auto-selectable set, never merely ranked last.
func TestBackend_ListAutoSelectable_ExcludesAnUnassumableRole(t *testing.T) {
	mock, b, claim := awaitingRoleEnv(t, "maintainer")
	ctx := t.Context()

	// The search query narrows to items carrying this binary's label and
	// assigned to this account; the claim above put both on the issue.
	mock.mu.Lock()
	labels := append([]string(nil), mock.issueLabels...)
	mock.mu.Unlock()
	if !hasLabel(labels, b.labels.Binary("implement")) {
		t.Fatalf("labels = %v, want the binary label the search selects on", labels)
	}

	assumable, err := b.ListAutoSelectable(ctx, nil, func(r flow.RoleName) bool { return r == "contributor" })
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	if len(assumable) != 0 {
		t.Errorf("ListAutoSelectable = %+v, want nothing — the awaited role is maintainer", assumable)
	}

	// The same item comes back once the role is one this account can assume,
	// and with no predicate at all.
	both, err := b.ListAutoSelectable(ctx, nil, func(r flow.RoleName) bool { return r == "maintainer" })
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	if len(both) != 1 || both[0].Display != claim.ItemRef.Display {
		t.Errorf("ListAutoSelectable = %+v, want the item back", both)
	}
	none, err := b.ListAutoSelectable(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	if len(none) != 1 {
		t.Errorf("ListAutoSelectable with a nil predicate = %+v, want the item", none)
	}
}
