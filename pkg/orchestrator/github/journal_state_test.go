package github

import (
	"errors"
	"strings"
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
	// The projection is DERIVED, not stored: the document carries no artifacts
	// array at all under schema v2, and the record comes off the journal.
	recs := recordsFromJournal(doc.Journal)
	if len(recs) != 1 || !recs["plan"].Resolved {
		t.Errorf("projection = %+v, want the one entry's record", recs)
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
	if recs := recordsFromJournal(doc.Journal); len(recs) != 0 {
		t.Errorf("projection = %+v, want none — a signal result is the observation", recs)
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

// The draft ends where the result begins, inside the one write. It matters
// beyond hygiene under the journal: a route can return to a step it already
// completed, and a draft left from the earlier execution would reach that
// dispatch's agent as its own prior thinking.
func TestBackend_AppendEntry_ClearsTheStepsDraft(t *testing.T) {
	_, b, claim := newJournalEnv(t)
	ctx := t.Context()

	if err := b.SaveWorkInProgress(ctx, claim.ItemRef, "plan", "half a plan"); err != nil {
		t.Fatalf("SaveWorkInProgress: %v", err)
	}
	if err := b.SaveWorkInProgress(ctx, claim.ItemRef, "review", "half a review"); err != nil {
		t.Fatalf("SaveWorkInProgress: %v", err)
	}

	appendMarkdown(t, b, claim.ItemRef, "plan", "the plan")

	if got, err := b.LoadWorkInProgress(ctx, claim.ItemRef, "plan"); err != nil || got != "" {
		t.Errorf("draft after the step completed = (%q, %v), want empty", got, err)
	}
	// Only this step's. Another step's draft is its own scaffolding and is not
	// finished by this one completing.
	if got, _ := b.LoadWorkInProgress(ctx, claim.ItemRef, "review"); got != "half a review" {
		t.Errorf("another step's draft = %q, want it untouched", got)
	}
}

// A refused append leaves the draft alone: the caller stashes what was refused
// there and parks, so clearing it would discard the very text the resume needs.
func TestBackend_AppendEntry_RefusedAppendKeepsTheDraft(t *testing.T) {
	_, b, claim := newJournalEnv(t)
	ctx := t.Context()

	if err := b.SaveWorkInProgress(ctx, claim.ItemRef, "plan", "half a plan"); err != nil {
		t.Fatalf("SaveWorkInProgress: %v", err)
	}
	if err := b.AppendEntry(ctx, claim.ItemRef,
		resultEntry("plan", 1, flow.ArtifactBody{Type: flow.ArtifactMarkdown})); err == nil {
		t.Fatal("AppendEntry with an empty body = nil, want a refusal")
	}
	if got, _ := b.LoadWorkInProgress(ctx, claim.ItemRef, "plan"); got != "half a plan" {
		t.Errorf("draft after a refused append = %q, want it kept", got)
	}
}

// commentCount is how many comments stand on the issue. The refusal tests read
// it before and after: every refusal lands BEFORE the publish, so a refused
// append leaves no comment for a step whose route was never recorded.
func commentCount(mock *ghMock) int {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	return len(mock.comments)
}

// This orchestrator stores the bytes, so it is the party that must verify them:
// an empty body has no out-of-band content to stand for, and is refused naming
// what is missing (docs/orchestrator.md § What an orchestrator may refuse).
func TestBackend_AppendEntry_RefusesAnEmptyBody(t *testing.T) {
	mock, b, claim := newJournalEnv(t)
	before := commentCount(mock)

	// One case per shape, each under a step whose DECLARED type it matches (or
	// none), so what is being refused is the emptiness and not the type.
	for _, tc := range []struct {
		step flow.StepId
		body flow.ArtifactBody
	}{
		{"plan", flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "  \n\t "}},
		{"implementation", flow.ArtifactBody{Type: flow.ArtifactCommitHash}},
		{"inspection", flow.ArtifactBody{Type: flow.ArtifactJSON}},
		{"spilled-file", flow.ArtifactBody{Type: flow.ArtifactFile, File: flow.FileBody{Name: "notes.txt"}}},
		{"spilled-patch", flow.ArtifactBody{Type: flow.ArtifactPatch}},
	} {
		err := b.AppendEntry(t.Context(), claim.ItemRef, resultEntry(tc.step, 1, tc.body))
		if err == nil {
			t.Errorf("AppendEntry with an empty %s body = nil, want a refusal", tc.body.Type)
		}
	}
	if got := commentCount(mock); got != before {
		t.Errorf("a refused append published %d comment(s); every refusal comes before the publish", got-before)
	}
	if doc := storedDocOrNil(mock); doc != nil && len(doc.Journal) != 0 {
		t.Errorf("a refused append journaled %d entries, want 0", len(doc.Journal))
	}

	// A FLAG is never empty: it has no payload at all, and the fact of the
	// write is the whole record.
	appendResult(t, b, claim.ItemRef, "branch-closed", 1, flow.ArtifactBody{Type: flow.ArtifactFlag})
}

// The body's shape against the schema this orchestrator declares. Startup
// validation answers for the flow's DECLARATION; this answers for the value
// actually recorded, which a declaration cannot.
func TestBackend_AppendEntry_RefusesADeclaredIdRecordedWithAnotherType(t *testing.T) {
	mock, b, claim := newJournalEnv(t)
	before := commentCount(mock)

	// `plan` is declared markdown.
	err := b.AppendEntry(t.Context(), claim.ItemRef, resultEntry("plan", 1, flow.ArtifactBody{
		Type: flow.ArtifactCommitHash, CommitHash: "0123456789abcdef0123456789abcdef01234567",
	}))
	var mismatch flow.ErrTypeMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("AppendEntry = %v, want ErrTypeMismatch", err)
	}
	if mismatch.Expected != flow.ArtifactMarkdown || mismatch.Got != flow.ArtifactCommitHash {
		t.Errorf("mismatch = %+v, want markdown expected / commit-hash got", mismatch)
	}
	if got := commentCount(mock); got != before {
		t.Errorf("a refused append published %d comment(s), want none", got-before)
	}

	// A step id the schema does not declare has no declared type to disagree
	// with, so it is recorded as given — which is what lets this orchestrator
	// carry the file and patch shapes it declares no artifact for.
	appendResult(t, b, claim.ItemRef, "spilled-file", 1, flow.ArtifactBody{
		Type: flow.ArtifactFile,
		File: flow.FileBody{Name: "notes.txt", Content: []byte("bytes")},
	})
}

// Holding the lease is a precondition the ORCHESTRATOR checks, not a value the
// caller supplies. An arena that lost the item to an `already-held` takeover
// would otherwise keep journaling under revoked authority.
func TestBackend_AppendEntry_RefusesAnUnclaimedItem(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	t.Cleanup(srv.Close)
	b := newMockedOrchestrator(t, mock, srv)

	before := commentCount(mock)
	err := b.AppendEntry(t.Context(), b.refFromIssue(42),
		resultEntry("plan", 1, flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the plan"}))
	if !errors.Is(err, flow.ErrUnavailable) {
		t.Fatalf("AppendEntry on an unclaimed item = %v, want ErrUnavailable", err)
	}
	if got := commentCount(mock); got != before {
		t.Errorf("a refused append published %d comment(s), want none", got-before)
	}
	if doc := storedDocOrNil(mock); doc != nil && len(doc.Journal) != 0 {
		t.Error("a refused append brought a state document into being")
	}
}

// The arena holding another item is not the arena for this one: one arena holds
// at most one claim, and an append addresses the item it names.
func TestBackend_AppendEntry_RefusesAnItemThisArenaDoesNotHold(t *testing.T) {
	_, b, _ := newJournalEnv(t) // claims #42
	err := b.AppendEntry(t.Context(), b.refFromIssue(43),
		resultEntry("plan", 1, flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the plan"}))
	if !errors.Is(err, flow.ErrUnavailable) {
		t.Fatalf("AppendEntry on an item this arena does not hold = %v, want ErrUnavailable", err)
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
	if len(doc.Journal) != 0 || doc.Park != nil {
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

// A reset takes the item's DRAFTS with the record they belonged to. Scratch
// prose kept past the journal it was working towards has nothing left to
// resume, and a route that returns to a step would hand the next dispatch's
// agent the earlier execution's thinking as its own.
//
// Every step's, and this issue's only — and whether or not there is a state
// comment, because the drafts are worktree-local and the record they belonged
// to may never have reached the issue.
func TestBackend_Reset_ClearsTheItemsDrafts(t *testing.T) {
	_, b, claim := newJournalEnv(t)
	ctx := t.Context()

	for _, step := range []flow.StepId{"plan", "implementation"} {
		if err := b.SaveWorkInProgress(ctx, claim.ItemRef, step, "half a "+string(step)); err != nil {
			t.Fatalf("SaveWorkInProgress(%s): %v", step, err)
		}
	}
	// Another issue's draft is not part of this item's record.
	other := b.refFromIssue(43)
	if err := b.SaveWorkInProgress(ctx, other, "plan", "issue 43's reasoning"); err != nil {
		t.Fatalf("SaveWorkInProgress(#43): %v", err)
	}

	if err := b.Reset(ctx, claim.ItemRef); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	for _, step := range []flow.StepId{"plan", "implementation"} {
		if got, err := b.LoadWorkInProgress(ctx, claim.ItemRef, step); got != "" || err != nil {
			t.Errorf("draft for %s after Reset = (%q, %v), want (\"\", nil)", step, got, err)
		}
	}
	if got, err := b.LoadWorkInProgress(ctx, other, "plan"); got != "issue 43's reasoning" || err != nil {
		t.Errorf("another issue's draft = (%q, %v), want it untouched", got, err)
	}
}

// The projection is derived, but hydration is unchanged and still runs off it:
// the state comment is an INDEX, and a markdown body lives in its own comment.
// Without the pass a resolved artifact loads empty and the next step proceeds on
// nothing with no error to say so.
func TestBackend_Load_HydratesMarkdownFromTheDerivedProjection(t *testing.T) {
	_, b, claim := newJournalEnv(t)

	appendMarkdown(t, b, claim.ItemRef, "plan", "the plan")
	// A second execution of the same step: the projection keeps the later one,
	// and hydration has to fill THAT body rather than the first comment it meets.
	appendResult(t, b, claim.ItemRef, "plan", 2,
		flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the revised plan"})

	state, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := state.Artifacts["plan"]
	if !rec.Resolved || rec.Version != 2 {
		t.Fatalf("plan = %+v, want resolved at version 2", rec)
	}
	if rec.Markdown != "the revised plan" {
		t.Errorf("plan body = %q, want the later execution's", rec.Markdown)
	}
}

// THE SPILLED PATH. A markdown artifact over the comment ceiling keeps only a
// truncated PREVIEW in its comment; loading the preview as though it were the
// body hands a step a plan cut off mid-sentence that still reads as complete, so
// the full text is fetched off the orphan branch instead.
func TestBackend_Load_HydratesASpilledMarkdownBodyInFull(t *testing.T) {
	_, b, claim := newJournalEnv(t)
	b.cfg.MaxCommentBytes = 256

	big := strings.Repeat("verbose output line\n", 400)
	appendResult(t, b, claim.ItemRef, "plan", 1,
		flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: big})

	state, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := state.Artifacts["plan"]
	if rec.Markdown != big {
		t.Errorf("plan body is %d bytes, want the whole %d — the preview was loaded as the artifact",
			len(rec.Markdown), len(big))
	}
	if strings.Contains(rec.Markdown, spillNoticePrefix) {
		t.Errorf("the loaded body carries the spill notice, so it is the comment rather than the file")
	}
}

// --- The awaited marker ---
//
// The label is a cheap index over the journal, maintained at every append: the
// listing reads it instead of fetching a state comment per item, so an index
// that disagrees with the entry it indexes sends `list` looking at the wrong
// items — or at none.

// awaitsLabelOn returns the item's flow:awaits:* labels, however many there are.
// However many, because "exactly one" is the property under test.
func awaitsLabelsOn(b *Orchestrator, mock *ghMock) []string {
	var out []string
	for _, n := range mock.labelNames() {
		if strings.HasPrefix(n, b.labels.AwaitsPrefix()) {
			out = append(out, n)
		}
	}
	return out
}

// The first append adds the marker, a later one MOVES it — removed then added,
// so the item never advertises two moves at once — and a finalizing append,
// whose Awaits is empty, removes it through the same one path.
func TestBackend_AppendEntry_MaintainsTheAwaitedLabel(t *testing.T) {
	mock, b, claim := newJournalEnv(t)
	ctx := t.Context()

	first := resultEntry("plan", 1, flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the plan"})
	first.Awaits = flow.Awaits{Role: "contributor"}
	if err := b.AppendEntry(ctx, claim.ItemRef, first); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	if got := awaitsLabelsOn(b, mock); len(got) != 1 || got[0] != "flow:awaits:contributor" {
		t.Fatalf("labels = %v, want exactly flow:awaits:contributor", got)
	}
	// The retired seeded marker is NOT written beside it any more: the binary
	// label is what separates `auto` from `available`.
	if contains(mock.labelNames(), "flow:seeded") {
		t.Errorf("labels = %v, want no flow:seeded — nothing writes it", mock.labelNames())
	}
	if !contains(mock.labelNames(), b.labels.Binary("implement")) {
		t.Errorf("labels = %v, want the binary marker", mock.labelNames())
	}

	// Electing a different role moves the marker rather than adding a second.
	second := resultEntry("impl", 1, flow.ArtifactBody{Type: flow.ArtifactCommitHash, CommitHash: "0123456789abcdef0123456789abcdef01234567"})
	second.Awaits = flow.Awaits{Role: "maintainer"}
	if err := b.AppendEntry(ctx, claim.ItemRef, second); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	if got := awaitsLabelsOn(b, mock); len(got) != 1 || got[0] != "flow:awaits:maintainer" {
		t.Errorf("labels = %v, want exactly flow:awaits:maintainer", got)
	}

	// A signal wait is nobody's move, and the label says which signal.
	third := flow.JournalEntry{
		Step: "pr-open", Execution: 1,
		Route:  flow.Route{Next: "pr-merged"},
		Awaits: flow.Awaits{Signal: "pr-merged"},
		By:     "tester", Role: "contributor",
	}
	if err := b.AppendEntry(ctx, claim.ItemRef, third); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	if got := awaitsLabelsOn(b, mock); len(got) != 1 || got[0] != "flow:awaits:signal:pr-merged" {
		t.Errorf("labels = %v, want exactly flow:awaits:signal:pr-merged", got)
	}

	// A FINALIZING entry awaits nothing, so the same one path removes the
	// marker and adds none. Finalize itself needs no label code.
	last := flow.JournalEntry{
		Step: "merge", Execution: 1,
		Route: flow.Route{Finalize: flow.DispositionResolved},
		By:    "tester", Role: "maintainer",
	}
	if err := b.AppendEntry(ctx, claim.ItemRef, last); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	if got := awaitsLabelsOn(b, mock); len(got) != 0 {
		t.Errorf("labels = %v, want the awaited marker gone after a finalizing entry", got)
	}
}

// An append electing the SAME role writes no label request at all: the marker
// already says what the item awaits, and a remove-then-add would be two
// requests against the secondary rate limit for no change.
func TestBackend_AppendEntry_LeavesAnUnchangedAwaitedLabelAlone(t *testing.T) {
	mock, b, claim := newJournalEnv(t)

	appendMarkdown(t, b, claim.ItemRef, "plan", "the plan")
	mock.resetRequests()
	appendResult(t, b, claim.ItemRef, "impl", 1,
		flow.ArtifactBody{Type: flow.ArtifactCommitHash, CommitHash: "0123456789abcdef0123456789abcdef01234567"})

	if n := mock.requestCount("DELETE /repos/o/r/issues/42/labels/flow:awaits:contributor"); n != 0 {
		t.Errorf("the unchanged marker was removed %d times, want 0", n)
	}
	if got := awaitsLabelsOn(b, mock); len(got) != 1 || got[0] != "flow:awaits:contributor" {
		t.Errorf("labels = %v, want the marker still there", got)
	}
}

// THE ERROR PATH. The label is best-effort and the entry is not: a label call
// that fails must not fail the append, because the entry HAS LANDED and
// reporting a failure sends the caller back to append it twice.
func TestBackend_AppendEntry_AFailingAwaitedLabelDoesNotFailTheAppend(t *testing.T) {
	mock, b, claim := newJournalEnv(t)
	ctx := t.Context()

	appendMarkdown(t, b, claim.ItemRef, "plan", "the plan")

	// GitHub rejects the removal of the marker the next append has to move.
	mock.mu.Lock()
	mock.failRemoveLabel = map[string]bool{"flow:awaits:contributor": true}
	mock.strictLabelRemoval = true
	mock.mu.Unlock()

	second := resultEntry("impl", 1, flow.ArtifactBody{Type: flow.ArtifactCommitHash, CommitHash: "0123456789abcdef0123456789abcdef01234567"})
	second.Awaits = flow.Awaits{Role: "maintainer"}
	if err := b.AppendEntry(ctx, claim.ItemRef, second); err != nil {
		t.Fatalf("AppendEntry failed over a label: %v", err)
	}
	// The record — which is the source of truth — carries the new election.
	doc := storedDoc(t, mock)
	if len(doc.Journal) != 2 || doc.Journal[1].Awaits != "maintainer" {
		t.Errorf("journal = %+v, want the second entry recorded awaiting maintainer", doc.Journal)
	}
	state, err := b.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Awaits.Role != "maintainer" {
		t.Errorf("Awaits = %+v, want maintainer — the journal answers, not the label", state.Awaits)
	}
}

// Reset clears the journal, so the marker derived from it goes too — a label
// saying whose move it is on an item with no record is a move nobody can make.
func TestBackend_Reset_RemovesTheAwaitedLabel(t *testing.T) {
	mock, b, claim := newJournalEnv(t)

	appendMarkdown(t, b, claim.ItemRef, "plan", "the plan")
	if got := awaitsLabelsOn(b, mock); len(got) != 1 {
		t.Fatalf("labels before Reset = %v, want the marker", got)
	}
	if err := b.Reset(t.Context(), claim.ItemRef); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := awaitsLabelsOn(b, mock); len(got) != 0 {
		t.Errorf("labels after Reset = %v, want the awaited marker gone", got)
	}
}

// THE POINT OF THE LABEL. An item carrying no flow:awaits:* marker awaits
// nobody, and answering that costs NO state-comment fetch — the listing reads a
// label it already has in hand. The gate used to be flow:seeded, so an item
// that had started but awaited nothing still cost a fetch; this is strictly
// fewer.
func TestBackend_Get_AwaitsCostsNoFetchWithoutTheLabel(t *testing.T) {
	mock, b, claim := newJournalEnv(t)
	ctx := t.Context()
	mock.resetRequests()

	info, err := b.Get(ctx, claim.ItemRef, "implement", nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !info.Awaits.Empty() {
		t.Errorf("Awaits = %+v, want the zero value on an item with no marker", info.Awaits)
	}
	// No comment read of ANY shape — neither the listing nor a cached id.
	for path, n := range mock.snapshotRequests() {
		if strings.Contains(path, "comments") {
			t.Errorf("%s was asked for %d times, want 0 — the label answers without a state-comment fetch", path, n)
		}
	}
}

// And with the marker, the listing reports the role AND the account of record —
// which is only in the journal, which is why the label replaces the gate and
// not the read.
func TestBackend_Get_AwaitsReadsTheAccountOfRecord(t *testing.T) {
	mock, b, claim := awaitingRoleEnv(t, "contributor")
	mock.resetRequests()

	info, err := b.Get(t.Context(), claim.ItemRef, "implement", nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Awaits.Role != "contributor" || info.Awaits.Account != "ann" {
		t.Errorf("Awaits = %+v, want contributor held by ann", info.Awaits)
	}
	// The counterpart of the test above, which asserts the fetch is SKIPPED:
	// without this, that one would pass on a read that never happens at all.
	if n := mock.requestCount("GET /repos/o/r/issues/comments/1002"); n == 0 {
		t.Errorf("the state comment was not fetched; requests = %v", mock.snapshotRequests())
	}
	if got := awaitsLabelsOn(b, mock); len(got) != 1 || got[0] != "flow:awaits:contributor" {
		t.Errorf("labels = %v, want the marker the read was gated on", got)
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
