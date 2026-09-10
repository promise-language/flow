package fake_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

func newItem(id string) flow.Item {
	return flow.Item{Ref: itemRef(id), Type: "task", Title: "Test item " + id}
}

// addItem registers an item under its id and hands back the ref every method
// now addresses it by.
func addItem(b *fake.Orchestrator, id string) flow.ItemRef {
	b.AddItem(id, newItem(id))
	return itemRef(id)
}

func itemRef(id string) flow.ItemRef {
	return flow.ItemRef{
		OrchestratorName: "fake",
		Display:          id,
		Ref:              json.RawMessage(`"` + id + `"`),
	}
}

// Load reports the same three block fields Get does. The advance reads them off
// Load before every dispatch and the listing reads Get, so a Load that
// disagreed with Get would have the advance and the listing disagree about
// whether the item can run. Nothing is stored: the blocker finishing is visible
// at the next read, and reopening it blocks the item again, with nobody
// touching the item either time.
func TestBackend_LoadAgreesWithGetOnBlockedness(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := addItem(b, "item")
	blocker := addItem(b, "blocker")
	ed, err := b.Edit(ctx, ref)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	ed.AddBlocker(blocker)
	if err := ed.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	agree := func(when string, wantBlocked bool, wantKind flow.BlockKind) {
		t.Helper()
		info, err := b.Get(ctx, ref, "binary", nil, nil)
		if err != nil {
			t.Fatalf("%s: Get: %v", when, err)
		}
		item, err := b.Load(ctx, ref)
		if err != nil {
			t.Fatalf("%s: Load: %v", when, err)
		}
		if item.Blocked != info.Blocked || item.BlockKind != info.BlockKind || item.BlockReason != info.BlockReason {
			t.Errorf("%s: Load(blocked %v kind %q reason %q) disagrees with Get(blocked %v kind %q reason %q)",
				when, item.Blocked, item.BlockKind, item.BlockReason, info.Blocked, info.BlockKind, info.BlockReason)
		}
		if item.Blocked != wantBlocked || item.BlockKind != wantKind {
			t.Errorf("%s: Load reports blocked=%v kind=%q, want blocked=%v kind=%q",
				when, item.Blocked, item.BlockKind, wantBlocked, wantKind)
		}
		if len(item.BlockedBy) != 1 || item.BlockedBy[0].Ref.Display != "blocker" {
			t.Errorf("%s: BlockedBy = %+v, want the one declared blocker, whatever its status", when, item.BlockedBy)
		}
	}
	agree("blocker open", true, flow.WaitsOnItems)
	b.SetStatus("blocker", flow.StatusTerminal, "done")
	agree("blocker landed", false, "")
	b.SetStatus("blocker", flow.StatusOpen, "reopened")
	agree("blocker reopened", true, flow.WaitsOnItems)
}

func TestBackend_ClaimAndLookup(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	_ = addItem(b, "1")

	claim, err := b.Claim(ctx, itemRef("1"), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claim.Account != b.Account() {
		t.Errorf("claim.Account = %q, want the ambient account %q", claim.Account, b.Account())
	}

	info, err := b.LookupClaim(ctx, itemRef("1"))
	if err != nil {
		t.Fatalf("LookupClaim: %v", err)
	}
	if info == nil || info.Account != b.Account() {
		t.Errorf("LookupClaim info = %+v, want account %q", info, b.Account())
	}
	if info.Arena != claim.Arena {
		t.Errorf("LookupClaim arena = %+v, want the claiming arena %+v", info.Arena, claim.Arena)
	}
}

// The account is ambient, not a parameter: whatever the arena's credentials act
// as is what the claim is credited to, and a caller has no way to disagree.
func TestBackend_ClaimCreditsTheAmbientAccount(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	b.SetAccount("octocat")
	ref := addItem(b, "1")

	claim, err := b.Claim(ctx, ref, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claim.Account != "octocat" {
		t.Errorf("claim.Account = %q, want octocat", claim.Account)
	}
}

// A lease binds item ↔ arena, so the conflict is with another ARENA, not
// another account. It returns a typed ErrClaimRefused with ItemScoped=true, so
// the auto-select loop can advance to a different item.
func TestBackend_ClaimConflictIsTypedRefusal(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	_ = addItem(b, "1")
	if _, err := b.Claim(ctx, itemRef("1"), nil); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	b.SetArena(flow.Arena{Host: "otherhost", Id: "/other/arena"})
	_, err := b.Claim(ctx, itemRef("1"), nil)
	if err == nil {
		t.Fatal("expected error when claiming an item held by another arena")
	}
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is %T, want ErrClaimRefused", err)
	}
	if !refused.ItemScoped {
		t.Error("ItemScoped = false, want true (a different item could succeed)")
	}
}

// Load returns the journal whole and IN ORDER, and hands back a copy. Position
// is derived from the last entry alone (flow.Position), so a journal read short
// or reordered re-routes the item, and a caller that could write through the
// returned slice would rewrite the store's record of the route it is only
// reading.
func TestBackend_LoadReturnsTheJournalInOrderAndUnaliased(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	item := newItem("1")
	item.Journal = []flow.JournalEntry{
		{Step: "plan", Execution: 1, Route: flow.Route{Next: "impl"}},
		{Step: "impl", Execution: 1, Route: flow.Route{Next: "plan"}},
		{Step: "plan", Execution: 2, Route: flow.Route{Next: "impl"}},
	}
	b.AddItem("1", item)
	ref := itemRef("1")

	state, err := b.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var got []flow.StepId
	for _, e := range state.Journal {
		got = append(got, e.Step)
	}
	want := []flow.StepId{"plan", "impl", "plan"}
	if !slices.Equal(got, want) {
		t.Fatalf("journal steps = %v, want %v", got, want)
	}
	if last := state.Journal[len(state.Journal)-1]; last.Execution != 2 {
		t.Errorf("last entry execution = %d, want 2 — the entries are not the ones appended", last.Execution)
	}

	// Rewriting the loaded journal must not reach the store.
	state.Journal[0].Route = flow.Route{Finalize: flow.DispositionRejected}
	state.Journal = state.Journal[:1]

	again, err := b.Load(ctx, ref)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if len(again.Journal) != len(want) {
		t.Fatalf("journal length = %d after a caller truncated its copy, want %d", len(again.Journal), len(want))
	}
	if again.Journal[0].Route != (flow.Route{Next: "impl"}) {
		t.Errorf("entry 0 route = %+v, want the recorded election — a caller rewrote the store's route", again.Journal[0].Route)
	}
}

// claimed registers an item and takes the claim every write below needs.
func claimed(t *testing.T, b *fake.Orchestrator, id string) flow.ItemRef {
	t.Helper()
	ref := addItem(b, id)
	if _, err := b.Claim(context.Background(), ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return ref
}

// entry is one completed execution of `step` producing markdown and routing on
// to `next`. The shape most tests below need, spelled once.
func entry(step flow.StepId, exec int, markdown string, next flow.StepId) flow.JournalEntry {
	return flow.JournalEntry{
		Step:      step,
		Execution: exec,
		Result:    flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: markdown},
		Route:     flow.Route{Next: next},
		Awaits:    flow.Awaits{Role: "contributor"},
		By:        "ann",
		Role:      "contributor",
	}
}

// --- AppendEntry ---

// The journal is append-only and ordered: Load returns exactly what was
// appended, in the order it was appended, because the pending step is derived
// from the last entry and a journal read short or reordered re-routes the item.
func TestBackend_AppendEntry_AppendsInOrder(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	want := []flow.JournalEntry{
		entry("plan", 1, "the plan", "impl"),
		entry("impl", 1, "the diff", "review"),
		entry("plan", 2, "the revised plan", "impl"),
	}
	for i, e := range want {
		if err := b.AppendEntry(ctx, ref, e); err != nil {
			t.Fatalf("AppendEntry %d: %v", i, err)
		}
	}

	state, err := b.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Journal) != len(want) {
		t.Fatalf("Journal has %d entries, want %d", len(state.Journal), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(state.Journal[i], want[i]) {
			t.Errorf("entry %d = %+v, want %+v", i, state.Journal[i], want[i])
		}
	}
}

// A second execution of a step appends; it never rewrites the first. The
// earlier entry is the record of what was decided then.
func TestBackend_AppendEntry_NeverRewritesAnExistingEntry(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	if err := b.AppendEntry(ctx, ref, entry("plan", 1, "first", "impl")); err != nil {
		t.Fatalf("first AppendEntry: %v", err)
	}
	if err := b.AppendEntry(ctx, ref, entry("plan", 2, "second", "impl")); err != nil {
		t.Fatalf("second AppendEntry: %v", err)
	}

	state, _ := b.Load(ctx, ref)
	if len(state.Journal) != 2 {
		t.Fatalf("Journal has %d entries, want 2 — the second execution appends", len(state.Journal))
	}
	if got := state.Journal[0].Result.Markdown; got != "first" {
		t.Errorf("the first entry now reads %q, want %q — an appended journal never rewrites", got, "first")
	}
	// The later entry's result is what the step's projection stands at.
	if got := state.Artifact("plan").Markdown; got != "second" {
		t.Errorf("projection = %q, want %q — the latest entry's result stands", got, "second")
	}
	if got := state.Artifact("plan").Version; got != 2 {
		t.Errorf("projection Version = %d, want 2 (the execution number)", got)
	}
}

// The projection and the awaited marker move in the SAME call: AppendEntry is
// one write, and a caller that saw the result without the route (or the reverse)
// would be reading a state that never existed.
func TestBackend_AppendEntry_ProjectionAndAwaitsMoveTogether(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	before, _ := b.Load(ctx, ref)
	if before.Artifact("plan").Resolved {
		t.Fatal("artifact resolved before anything was appended")
	}
	if !before.Awaits.Empty() {
		t.Errorf("Awaits = %+v before the first entry, want the zero value", before.Awaits)
	}

	e := entry("plan", 1, "the plan", "impl")
	e.Awaits = flow.Awaits{Role: "maintainer"}
	if err := b.AppendEntry(ctx, ref, e); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}

	after, _ := b.Load(ctx, ref)
	rec := after.Artifact("plan")
	if !rec.Resolved || rec.Markdown != "the plan" {
		t.Errorf("projection = %+v, want the entry's result", rec)
	}
	if rec.ResolvedBy != "ann" {
		t.Errorf("ResolvedBy = %q, want ann — the provenance comes off the entry", rec.ResolvedBy)
	}
	if after.Awaits.Role != "maintainer" {
		t.Errorf("Awaits.Role = %q, want maintainer", after.Awaits.Role)
	}
	// The account of record is filled in from the journal, never from the entry:
	// the entry records a decision, not who holds the role next.
	if after.Awaits.Account != "" {
		t.Errorf("Awaits.Account = %q, want empty — maintainer has not acted", after.Awaits.Account)
	}
}

// The account of record is the By of the last entry appended in that role.
func TestBackend_AppendEntry_AwaitsCarriesTheAccountOfRecord(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	first := entry("plan", 1, "the plan", "impl")
	first.By = "ann"
	first.Role = "contributor"
	first.Awaits = flow.Awaits{Role: "reviewer"}
	if err := b.AppendEntry(ctx, ref, first); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	second := entry("impl", 1, "the diff", "review")
	second.By = "bo"
	second.Role = "reviewer"
	second.Awaits = flow.Awaits{Role: "contributor"}
	if err := b.AppendEntry(ctx, ref, second); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}

	state, _ := b.Load(ctx, ref)
	if state.Awaits.Role != "contributor" || state.Awaits.Account != "ann" {
		t.Errorf("Awaits = %+v, want contributor held by ann", state.Awaits)
	}
}

// The FIRST entry binds the flow: an item with an empty journal is bound to
// nothing, and the binding is a consequence of work having been recorded.
func TestBackend_AppendEntry_FirstEntryBindsTheFlow(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	b.SetBoundFlow("implement")
	ref := claimed(t, b, "1")

	before, _ := b.Load(ctx, ref)
	if before.Flow != "" {
		t.Errorf("Flow = %q before the first entry, want empty", before.Flow)
	}
	if err := b.AppendEntry(ctx, ref, entry("plan", 1, "the plan", "impl")); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	after, _ := b.Load(ctx, ref)
	if after.Flow != "implement" {
		t.Errorf("Flow = %q after the first entry, want implement", after.Flow)
	}
}

func TestBackend_AppendEntry_RefusesAnUnclaimedItem(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := addItem(b, "1") // never claimed

	err := b.AppendEntry(ctx, ref, entry("plan", 1, "the plan", "impl"))
	if !errors.Is(err, flow.ErrUnavailable) {
		t.Fatalf("AppendEntry on an unclaimed item = %v, want ErrUnavailable", err)
	}
	state, _ := b.Load(ctx, ref)
	if len(state.Journal) != 0 {
		t.Errorf("Journal has %d entries after a refused append, want 0", len(state.Journal))
	}
}

func TestBackend_AppendEntry_RefusesAnotherArenasClaim(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	b.SetArena(flow.Arena{Host: "elsewhere", Id: "other"})
	err := b.AppendEntry(ctx, ref, entry("plan", 1, "the plan", "impl"))
	if !errors.Is(err, flow.ErrUnavailable) {
		t.Fatalf("AppendEntry from another arena = %v, want ErrUnavailable", err)
	}
}

// The fake stores the bytes, so it is the party that must verify them: an empty
// body on an artifact step stands for nothing it could look up.
func TestBackend_AppendEntry_RefusesAnEmptyArtifactBody(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	empty := []flow.ArtifactBody{
		{Type: flow.ArtifactMarkdown},
		{Type: flow.ArtifactMarkdown, Markdown: "   \n"},
		{Type: flow.ArtifactCommitHash},
		{Type: flow.ArtifactJSON},
		{Type: flow.ArtifactFile, File: flow.FileBody{Name: "x.txt"}},
		{Type: flow.ArtifactPatch},
	}
	for _, body := range empty {
		e := entry("plan", 1, "", "impl")
		e.Result = body
		if err := b.AppendEntry(ctx, ref, e); err == nil {
			t.Errorf("AppendEntry with an empty %s body = nil, want a refusal", body.Type)
		}
	}

	// A FLAG has no payload at all, so it is never empty: the fact of the write
	// is the record. And a signal step's result is the observation itself.
	e := entry("flagged", 1, "", "impl")
	e.Result = flow.ArtifactBody{Type: flow.ArtifactFlag}
	if err := b.AppendEntry(ctx, ref, e); err != nil {
		t.Errorf("AppendEntry with a flag body = %v, want nil", err)
	}
	e = entry("pr-open", 1, "", "impl")
	e.Result = flow.ArtifactBody{}
	if err := b.AppendEntry(ctx, ref, e); err != nil {
		t.Errorf("AppendEntry with no body (a signal step) = %v, want nil", err)
	}
}

func TestBackend_AppendEntry_RefusesAnEntryNamingNoStep(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	e := entry("", 1, "the plan", "impl")
	if err := b.AppendEntry(ctx, ref, e); err == nil {
		t.Fatal("AppendEntry with no step = nil, want a refusal")
	}
}

// Completion ends the step's scaffolding: the result and the end of the draft
// are one write, so no caller can observe one without the other.
func TestBackend_AppendEntry_ClearsTheStepsDraft(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	if err := b.SaveWorkInProgress(ctx, ref, "plan", "half a plan"); err != nil {
		t.Fatalf("SaveWorkInProgress: %v", err)
	}
	if err := b.AppendEntry(ctx, ref, entry("plan", 1, "the plan", "impl")); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	got, err := b.LoadWorkInProgress(ctx, ref, "plan")
	if err != nil {
		t.Fatalf("LoadWorkInProgress: %v", err)
	}
	if got != "" {
		t.Errorf("draft = %q after the step completed, want empty", got)
	}
}

// --- Reset ---

func TestBackend_Reset_ClearsTheFlowsWholeRecord(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	b.SetBoundFlow("implement")
	ref := claimed(t, b, "1")

	if err := b.AppendEntry(ctx, ref, entry("plan", 1, "the plan", "impl")); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	_ = b.RecordDispatch(ctx, ref, "plan")
	_ = b.AddCost(ctx, ref, "plan", 4.25)
	_ = b.AddDuration(ctx, ref, "plan", time.Minute)
	_ = b.SaveWorkInProgress(ctx, ref, "impl", "half a diff")
	if err := b.Park(ctx, ref, flow.ParkRequest{Kind: flow.ParkRefused, Step: "impl", Reason: "no"}); err != nil {
		t.Fatalf("Park: %v", err)
	}

	if err := b.Reset(ctx, ref); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	state, _ := b.Load(ctx, ref)
	if len(state.Journal) != 0 {
		t.Errorf("Journal has %d entries after Reset, want 0", len(state.Journal))
	}
	if len(state.Ledger.Steps) != 0 || state.Ledger.TotalCostUSD != 0 || state.Ledger.TotalActive != 0 {
		t.Errorf("Ledger = %+v after Reset, want empty", state.Ledger)
	}
	if state.Park != nil {
		t.Errorf("Park = %+v after Reset, want nil", state.Park)
	}
	if state.Artifact("plan").Resolved {
		t.Errorf("artifact still resolved after Reset")
	}
	if !state.Awaits.Empty() {
		t.Errorf("Awaits = %+v after Reset, want the zero value", state.Awaits)
	}
	if state.Flow != "" {
		t.Errorf("Flow = %q after Reset, want empty — an empty journal is bound to nothing", state.Flow)
	}
	if draft, _ := b.LoadWorkInProgress(ctx, ref, "impl"); draft != "" {
		t.Errorf("draft = %q after Reset, want empty", draft)
	}
}

// A reset item is at the start again: the entry step pends, which is what makes
// `reseed` a re-run rather than a deletion.
func TestBackend_Reset_LeavesTheItemPendingItsEntryStep(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	f := flow.NewFlow("implement", []flow.ItemType{"task"})
	f.Role("contributor", flow.CapPush)
	noop := func(flow.StepCtx) (flow.StepResult, error) { return flow.StepResult{}, nil }
	f.AddStep("write plan", "plan", noop, flow.StepConfig{
		Prompts: flow.PromptsAgent,
		Role:    "contributor", Entry: true, Next: []flow.StepId{"impl"}})
	f.AddStep("implement", "impl", noop, flow.StepConfig{
		Prompts: flow.PromptsAgent,
		Role:    "contributor", MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	if err := f.ValidateGraph(); err != nil {
		t.Fatalf("ValidateGraph: %v", err)
	}

	if err := b.AppendEntry(ctx, ref, entry("plan", 1, "the plan", "impl")); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	state, _ := b.Load(ctx, ref)
	if pos, err := f.Position(state); err != nil || pos.Step.Description != "implement" {
		t.Fatalf("before Reset: Position = %+v, %v; want the implement step", pos, err)
	}

	if err := b.Reset(ctx, ref); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	state, _ = b.Load(ctx, ref)
	pos, err := f.Position(state)
	if err != nil {
		t.Fatalf("after Reset: Position: %v", err)
	}
	if pos.Step.Description != "write plan" {
		t.Errorf("after Reset the pending step is %q, want the entry step", pos.Step.Description)
	}
}

// The questions themselves survive a reset — one leaves the pending set by
// being answered, not by being deleted — but the outstanding-question marker
// goes with the park.
func TestBackend_Reset_KeepsTheQuestionsAndClearsTheirMarker(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	q, err := b.AskQuestion(ctx, ref, flow.AgentQuestion{Header: "which", Text: "which base?"})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}
	if err := b.Park(ctx, ref, flow.ParkRequest{Kind: flow.ParkQuestion, Step: "plan"}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if err := b.Reset(ctx, ref); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	state, _ := b.Load(ctx, ref)
	if state.Park != nil {
		t.Errorf("Park = %+v after Reset, want nil — the marker goes", state.Park)
	}
	if len(state.Questions) != 1 || state.Questions[0].ID != q.ID {
		t.Errorf("Questions = %+v after Reset, want the unanswered ask still reachable by id", state.Questions)
	}
}

// --- The ledger ---

func TestBackend_LedgerArithmeticAccumulates(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	for range 2 {
		if err := b.RecordDispatch(ctx, ref, "plan"); err != nil {
			t.Fatalf("RecordDispatch: %v", err)
		}
	}
	if err := b.RecordResumption(ctx, ref, "plan"); err != nil {
		t.Fatalf("RecordResumption: %v", err)
	}
	_ = b.AddCost(ctx, ref, "plan", 3.5)
	_ = b.AddCost(ctx, ref, "plan", 1.5)
	_ = b.AddDuration(ctx, ref, "plan", 5*time.Minute)
	_ = b.AddDuration(ctx, ref, "plan", 3*time.Minute+30*time.Second)
	// A different step keeps its own row; the totals cover both.
	_ = b.RecordDispatch(ctx, ref, "impl")
	_ = b.AddCost(ctx, ref, "impl", 2.0)

	state, _ := b.Load(ctx, ref)
	row := state.Ledger.Row("plan")
	if row.Step != "plan" {
		t.Errorf("Row.Step = %q, want plan", row.Step)
	}
	if row.Dispatches != 2 {
		t.Errorf("Dispatches = %d, want 2", row.Dispatches)
	}
	if row.Resumptions != 1 {
		t.Errorf("Resumptions = %d, want 1", row.Resumptions)
	}
	if row.CostUSD != 5.0 {
		t.Errorf("CostUSD = %v, want 5.0", row.CostUSD)
	}
	if want := 8*time.Minute + 30*time.Second; row.Active != want {
		t.Errorf("Active = %v, want %v", row.Active, want)
	}
	if row.LastRunAt.IsZero() {
		t.Error("LastRunAt is zero after a dispatch")
	}
	if state.Ledger.TotalCostUSD != 7.0 {
		t.Errorf("TotalCostUSD = %v, want 7.0 (5.0 + 2.0)", state.Ledger.TotalCostUSD)
	}
	if want := 8*time.Minute + 30*time.Second; state.Ledger.TotalActive != want {
		t.Errorf("TotalActive = %v, want %v", state.Ledger.TotalActive, want)
	}
}

// Waiting is evidence about contention, not about the work: it never lands in
// Active, and the two totals are kept apart for the same reason.
func TestBackend_AddWaitingNeverLandsInActive(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	_ = b.AddDuration(ctx, ref, "plan", 2*time.Minute)
	_ = b.AddWaiting(ctx, ref, "plan", 9*time.Minute)
	_ = b.AddWaiting(ctx, ref, "plan", time.Minute)

	state, _ := b.Load(ctx, ref)
	row := state.Ledger.Row("plan")
	if row.Active != 2*time.Minute {
		t.Errorf("Active = %v, want 2m — waiting must not land here", row.Active)
	}
	if row.Waiting != 10*time.Minute {
		t.Errorf("Waiting = %v, want 10m", row.Waiting)
	}
	if state.Ledger.TotalActive != 2*time.Minute || state.Ledger.TotalWaiting != 10*time.Minute {
		t.Errorf("totals = active %v / waiting %v, want 2m / 10m",
			state.Ledger.TotalActive, state.Ledger.TotalWaiting)
	}
}

// A ledger row is keyed by StepId, not by an artifact, which is what gives a
// signal step — producing no artifact at all — a row of its own.
func TestBackend_LedgerRecordsASignalStep(t *testing.T) {
	ctx := context.Background()
	b := fake.New(flow.Signal("pr-open", "test"))
	ref := claimed(t, b, "1")

	_ = b.RecordDispatch(ctx, ref, "pr-open")
	state, _ := b.Load(ctx, ref)
	if got := state.Ledger.Row("pr-open").Dispatches; got != 1 {
		t.Errorf("Row(pr-open).Dispatches = %d, want 1", got)
	}
}

func TestBackend_Grant_RecordsTheExtension(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	if err := b.Grant(ctx, ref, "plan", flow.Grant{Invocations: 5, CostUSD: 20}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := b.Grant(ctx, ref, "plan", flow.Grant{CostUSD: 2.50}); err != nil {
		t.Fatalf("second Grant: %v", err)
	}

	state, _ := b.Load(ctx, ref)
	row := state.Ledger.Row("plan")
	if got := row.GrantedOn(flow.AxisInvocations); got != 5 {
		t.Errorf("GrantedOn(invocations) = %v, want 5", got)
	}
	if got := row.GrantedOn(flow.AxisCost); got != 22.50 {
		t.Errorf("GrantedOn(cost) = %v, want 22.50 (20 + 2.50)", got)
	}
	// Zero on an axis is "no change", so nothing is recorded there: a row of
	// zero-amount entries would be a history of grants that granted nothing.
	if got := row.GrantedOn(flow.AxisTimeout); got != 0 {
		t.Errorf("GrantedOn(timeout) = %v, want 0 — the grant named no timeout", got)
	}
	if len(row.Granted) != 3 {
		t.Errorf("Granted has %d records, want 3", len(row.Granted))
	}
	// The cap the binary will read is its policy plus these.
	eff := flow.EffectiveBudget(flow.StepBudget{}, row)
	if want := flow.DefaultStepBudget().MaxInvocations + 5; eff.MaxInvocations != want {
		t.Errorf("EffectiveBudget.MaxInvocations = %d, want %d", eff.MaxInvocations, want)
	}
	if want := flow.DefaultStepBudget().MaxCostUSD + 22.50; eff.MaxCostUSD != want {
		t.Errorf("EffectiveBudget.MaxCostUSD = %v, want %v", eff.MaxCostUSD, want)
	}
}

func TestBackend_SignalSet(t *testing.T) {
	ctx := context.Background()
	b := fake.New(flow.Signal("pr-open", "test"))
	ref := addItem(b, "1")
	if _, err := b.Claim(ctx, ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	b.SetSignal("1", "pr-open", true)
	state, _ := b.Load(ctx, ref)
	if !state.SignalSet("pr-open") {
		t.Errorf("pr-open should be set after SetSignal")
	}
}

func TestBackend_AskQuestionsAssignsIDsAndAnswerFlow(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := addItem(b, "1")
	if _, err := b.Claim(ctx, ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	qs := []flow.AgentQuestion{
		flow.AskYesNo("ship", "Ship it?"),
		flow.AskChoice("lib", "Which lib?", "a", "b"),
	}
	var out []flow.Question
	for _, q := range qs {
		rec, err := b.AskQuestion(ctx, ref, q)
		if err != nil {
			t.Fatalf("AskQuestion: %v", err)
		}
		out = append(out, rec)
	}
	if len(out) != 2 {
		t.Fatalf("recorded %d questions, want 2", len(out))
	}
	if out[0].ID == "" || out[1].ID == "" || out[0].ID == out[1].ID {
		t.Errorf("IDs not assigned uniquely: %q, %q", out[0].ID, out[1].ID)
	}
	if out[0].Text != "Ship it?" || out[1].Format != flow.FormatChoice {
		t.Errorf("returned questions don't match input: %+v", out)
	}

	// State surfaces them as pending.
	state, _ := b.Load(ctx, ref)
	if len(state.Questions) != 2 {
		t.Errorf("Load.Questions len = %d, want 2", len(state.Questions))
	}
	if len(state.PendingQuestions()) != 2 {
		t.Errorf("PendingQuestions len = %d, want 2 before answer", len(state.PendingQuestions()))
	}

	// Answer one — Pending drops to 1.
	if err := b.AnswerQuestion("1", out[0].ID, "yes"); err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	state, _ = b.Load(ctx, ref)
	if len(state.PendingQuestions()) != 1 {
		t.Errorf("PendingQuestions len = %d, want 1 after answering one", len(state.PendingQuestions()))
	}
}

// PostAnswer records the answer against the question it answers AND LEAVES THE
// PARK, the same rule the GitHub backend obeys.
//
// The park carries the ask time, which is the only window a resumed step reads
// its answers through: an orchestrator that cleared it here would delete the
// answer's own delivery, and the resumed step would re-derive the question it
// was just answered. Both orchestrators are asserted against the rule because a
// fake that disagreed with the backend is how the loop stayed invisible to
// every test.
func TestBackend_PostAnswerRecordsTheAnswerAndLeavesThePark(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := addItem(b, "1")
	if _, err := b.Claim(ctx, ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	first, err := b.AskQuestion(ctx, ref, flow.AskText("base", "Which base branch?"))
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}
	if _, err := b.AskQuestion(ctx, ref, flow.AskYesNo("ship", "Ship it?")); err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}
	if err := b.Park(ctx, ref, flow.ParkRequest{
		Kind: flow.ParkQuestion, Step: "plan", Reason: "question: " + first.Header,
		Details: flow.MarkQuestionAsked(first.AskedAt),
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}

	if err := b.PostAnswer(ctx, ref, first.ID, "main"); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}
	state, err := b.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	answered := false
	for _, q := range state.Questions {
		if q.ID == first.ID {
			answered = q.Answer == "main"
		}
	}
	if !answered {
		t.Errorf("questions = %+v, want the answer recorded against %q", state.Questions, first.ID)
	}
	// Answering one of two is not answering the item.
	if got := len(state.PendingQuestions()); got != 1 {
		t.Errorf("PendingQuestions = %d, want 1 — the second question is still waiting", got)
	}
	if state.Park == nil {
		t.Fatal("the park cleared on the answer")
	}
	// The marker is RFC3339, so the stamp comes back truncated to the second.
	wantAskedAt := first.AskedAt.UTC().Truncate(time.Second)
	if got := flow.QuestionAskedAt(state.Park); !got.Equal(wantAskedAt) {
		t.Errorf("asked-at = %v, want %v (the window the resume reads answers through)", got, wantAskedAt)
	}

	// And the LAST answer — the one that used to take the park with it — leaves
	// it standing too. Only the asking step completing, a superseding park, or
	// a reset drops it.
	for _, q := range state.PendingQuestions() {
		if err := b.PostAnswer(ctx, ref, q.ID, "yes"); err != nil {
			t.Fatalf("PostAnswer: %v", err)
		}
	}
	state, err = b.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.PendingQuestions()) != 0 {
		t.Fatalf("PendingQuestions = %d, want 0", len(state.PendingQuestions()))
	}
	if state.Park == nil {
		t.Error("the park cleared with the last answer — the resume has no answer window left")
	}
}

// An unknown id is refused, and an already-answered one too: either accepted
// silently would report an answer that moved nothing.
func TestBackend_PostAnswerRefusesUnknownAndAnsweredQuestions(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := addItem(b, "1")
	if _, err := b.Claim(ctx, ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	q, err := b.AskQuestion(ctx, ref, flow.AskText("base", "Which base branch?"))
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}
	if err := b.PostAnswer(ctx, ref, "no-such-question", "main"); err == nil {
		t.Error("PostAnswer accepted an id naming no question on the item")
	}
	if err := b.PostAnswer(ctx, ref, q.ID, "main"); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}
	if err := b.PostAnswer(ctx, ref, q.ID, "the release branch"); err == nil {
		t.Error("PostAnswer accepted a second answer to one question")
	}
	state, err := b.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Questions[0].Answer != "main" {
		t.Errorf("Answer = %q, want the first answer kept", state.Questions[0].Answer)
	}
}

// "Waiting for an answer" is decided by the QUESTIONS, not by the presence of a
// question park.
//
// The park now outlives the answer — it carries the window the resume reads its
// replies through. Blockedness read off the bare park would therefore report an
// item whose every question is answered as still waiting on a person, and
// ListAutoSelectable would skip it: never selected, so the asking step never
// resumes, so the park never clears. The stall the park was kept to prevent,
// moved one step later.
func TestBackend_AnsweringUnblocksTheItemWhileTheParkStands(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := addItem(b, "1")
	if _, err := b.Claim(ctx, ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	q, err := b.AskQuestion(ctx, ref, flow.AskText("base", "Which base branch?"))
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}
	if err := b.Park(ctx, ref, flow.ParkRequest{
		Kind: flow.ParkQuestion, Step: "plan", Reason: "question: " + q.Header,
		Details: flow.MarkQuestionAsked(q.AskedAt),
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}

	// Unanswered: blocked, and off the selectable list.
	state, err := b.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.BlockReason == "" {
		t.Error("an item with an unanswered question reports no block reason")
	}
	selectable, err := b.ListAutoSelectable(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	if len(selectable) != 0 {
		t.Errorf("ListAutoSelectable = %v, want nothing while the question is unanswered", selectable)
	}

	if err := b.PostAnswer(ctx, ref, q.ID, "main"); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}

	state, err = b.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.BlockReason != "" {
		t.Errorf("BlockReason = %q after the answer, want none — the human has acted", state.BlockReason)
	}
	selectable, err = b.ListAutoSelectable(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	if len(selectable) != 1 {
		t.Errorf("ListAutoSelectable = %v, want the answered item back — nothing else can resume the step", selectable)
	}
	// And the park is still there for the resume to read its answer through.
	if state.Park == nil {
		t.Error("the park cleared with the answer")
	}
}

// A question park that registered no question is still waiting: there is
// nothing to have answered, so nobody has. Without this the "answered" rule
// above would report the unanswerable park of flow#166 as workable.
func TestBackend_AQuestionParkWithNoQuestionStaysBlocked(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := addItem(b, "1")
	if _, err := b.Claim(ctx, ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.Park(ctx, ref, flow.ParkRequest{
		Kind: flow.ParkQuestion, Step: "plan", Reason: "question: which base branch?",
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	state, err := b.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.BlockReason == "" {
		t.Error("a question park with no registered question reports no block reason")
	}
	selectable, err := b.ListAutoSelectable(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	if len(selectable) != 0 {
		t.Errorf("ListAutoSelectable = %v, want nothing — the park still needs a person", selectable)
	}
}

// ANSWERING ONE OF TWO IS NOT ANSWERING THE ITEM.
//
// Blockedness is now derived from the questions, so it inherits the rule the
// needs-answer marker already obeys: the wait ends with the LAST answer and not
// before. A derivation that unblocked on the first — "somebody has replied" —
// would put the item back on the selectable list with a question still
// outstanding, and the resumed step would spend a turn re-asking the one nobody
// answered. That is the reported loop, reached by another road.
func TestBackend_AnsweringOneOfTwoLeavesTheItemBlocked(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := addItem(b, "1")
	if _, err := b.Claim(ctx, ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	first, err := b.AskQuestion(ctx, ref, flow.AskText("base", "Which base branch?"))
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}
	second, err := b.AskQuestion(ctx, ref, flow.AskYesNo("ship", "Ship it?"))
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}
	if err := b.Park(ctx, ref, flow.ParkRequest{
		Kind: flow.ParkQuestion, Step: "plan", Reason: "question: " + first.Header,
		Details: flow.MarkQuestionAsked(first.AskedAt),
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}

	if err := b.PostAnswer(ctx, ref, first.ID, "main"); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}
	state, err := b.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.BlockReason == "" {
		t.Error("BlockReason cleared with one question still unanswered")
	}
	selectable, err := b.ListAutoSelectable(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	if len(selectable) != 0 {
		t.Errorf("ListAutoSelectable = %v, want nothing — %q is still unanswered", selectable, second.ID)
	}

	// The last answer is what ends the wait.
	if err := b.PostAnswer(ctx, ref, second.ID, "yes"); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}
	state, err = b.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.BlockReason != "" {
		t.Errorf("BlockReason = %q after both answers, want none", state.BlockReason)
	}
}

func TestBackend_ParkRecordsRequest(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := addItem(b, "1")
	if _, err := b.Claim(ctx, ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	req := flow.ParkRequest{
		Kind:   flow.ParkTreasurerRefused,
		Step:   "plan",
		Axis:   flow.AxisInvocations,
		Reason: "exhausted",
	}
	if err := b.Park(ctx, ref, req); err != nil {
		t.Fatalf("Park: %v", err)
	}
	got := b.ParkRequest("1")
	if got == nil || got.Kind != flow.ParkTreasurerRefused || got.Axis != flow.AxisInvocations {
		t.Errorf("ParkRequest = %+v, want treasurer-refused/invocations", got)
	}
}

// parkedItem is an item whose "plan" step has burned its 3 invocations and
// parked on the invocations axis — the state a `grant` acts on. The park
// carries the run's own snapshot of every axis, which is what GrantClearsPark
// reads the refused cap from.
func parkedItem(t *testing.T, b *fake.Orchestrator) flow.ItemRef {
	t.Helper()
	ctx := context.Background()
	ref := claimed(t, b, "1")
	for range 3 {
		if err := b.RecordDispatch(ctx, ref, "plan"); err != nil {
			t.Fatalf("RecordDispatch: %v", err)
		}
	}
	if err := b.Park(ctx, ref, flow.ParkRequest{
		Kind: flow.ParkTreasurerRefused, Step: "plan", Axis: flow.AxisInvocations,
		Axes: []flow.AxisReport{
			flow.NewAxisReport(flow.AxisInvocations, 3, 3),
			flow.NewAxisReport(flow.AxisCost, 1.25, 10),
		},
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	return ref
}

// Load surfaces the park so a caller can see WHY the item stopped.
func TestBackend_LoadSurfacesPark(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := parkedItem(t, b)

	st, err := b.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !st.Parked() {
		t.Fatal("ItemState.Park = nil, want the recorded park")
	}
	if st.Park.Step != "plan" || st.Park.Axis != flow.AxisInvocations {
		t.Errorf("park = %+v, want plan/invocations", st.Park)
	}
}

// The Backend.Grant contract: a grant that gives the parked axis headroom
// clears the park; one that does not, leaves it.
func TestBackend_GrantClearsParkOnlyWhenSatisfied(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := parkedItem(t, b)

	// Cost is not the parked axis — the park must survive.
	if err := b.Grant(ctx, ref, "plan", flow.Grant{CostUSD: 50}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if b.ParkRequest("1") == nil {
		t.Fatal("park cleared by a grant on an unrelated axis")
	}

	if err := b.Grant(ctx, ref, "plan", flow.Grant{Invocations: 1}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if p := b.ParkRequest("1"); p != nil {
		t.Errorf("park = %+v, want cleared once invocations had headroom", p)
	}
	st, err := b.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.Parked() {
		t.Error("Load still reports a park after it was cleared")
	}
}

// Completing the parked step makes its park obsolete; keeping it would make
// Load report a reason that no longer holds.
func TestBackend_AppendEntryClearsParkForThatStep(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := parkedItem(t, b)

	if err := b.AppendEntry(ctx, ref, entry("plan", 1, "done", "impl")); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	if p := b.ParkRequest("1"); p != nil {
		t.Errorf("park = %+v, want cleared by the completion", p)
	}
}

// A park on a DIFFERENT step survives a completion: the reason it records still
// holds, and clearing it would report the item resumable when it is not.
func TestBackend_AppendEntryLeavesAParkOnAnotherStep(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := parkedItem(t, b)

	if err := b.AppendEntry(ctx, ref, entry("impl", 1, "the diff", "review")); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	if p := b.ParkRequest("1"); p == nil || p.Step != "plan" {
		t.Errorf("park = %+v, want the plan park still standing", p)
	}
}

// A question park is not a treasurer's park: no grant clears it, because
// nothing about a budget answers the question.
func TestBackend_GrantLeavesAQuestionPark(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	if err := b.Park(ctx, ref, flow.ParkRequest{Kind: flow.ParkQuestion, Step: "plan"}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if err := b.Grant(ctx, ref, "plan", flow.Grant{Invocations: 99, CostUSD: 999}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if p := b.ParkRequest("1"); p == nil || p.Kind != flow.ParkQuestion {
		t.Errorf("park = %+v, want the question park still standing", p)
	}
}

// gateWorktree claims an item and returns its worktree.
func gateWorktree(t *testing.T, b *fake.Orchestrator) flow.Worktree {
	t.Helper()
	ctx := context.Background()
	ref := addItem(b, "1")
	_, err := b.Claim(ctx, ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := b.Worktree(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	return wt
}

// The fake is what a caller's tests are written against, so the outcomes have
// to reach them AS outcomes. A fake that turned "died" into an error would let
// a caller be written that cannot tell a dead host from a missing binary — the
// exact collapse the outcome set exists to prevent — and every one of that
// caller's tests would still pass.
func TestBackend_SetGateOutcomeReachesTheWorktreeAsAnOutcome(t *testing.T) {
	ctx := context.Background()
	for _, outcome := range []flow.Outcome{
		flow.OutcomeMeasured,
		flow.OutcomeTimedOut,
		flow.OutcomeCouldNotStart,
		flow.OutcomeDied,
		flow.OutcomeBrokeContract,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			b := fake.New()
			b.SetGateOutcome(outcome)
			run, err := gateWorktree(t, b).RunGate(ctx, flow.GateIntegration)
			if err != nil {
				t.Fatalf("RunGate: %v — every way a gate fails is an outcome, not an error", err)
			}
			if run.Outcome != outcome {
				t.Errorf("Outcome = %q, want %q", run.Outcome, outcome)
			}
			if run.Gate != flow.GateIntegration {
				t.Errorf("Gate = %q, want the name that was asked for", run.Gate)
			}
		})
	}
}

// The default has to be the one outcome that lets an unrelated test get on with
// its subject, and the envelope it reports has to be one that parses: the fake
// models the protocol, and a caller that reads Stdout would otherwise be
// written against a shape no real gate produces.
func TestBackend_GateMeasuresByDefaultAndPrintsSomethingThatParses(t *testing.T) {
	run, err := gateWorktree(t, fake.New()).RunGate(context.Background(), flow.GateTested)
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if run.Outcome != flow.OutcomeMeasured {
		t.Fatalf("Outcome = %q, want %q", run.Outcome, flow.OutcomeMeasured)
	}
	var envelope map[string]any
	if err := json.Unmarshal(run.Stdout, &envelope); err != nil || envelope == nil {
		t.Errorf("Stdout = %q, want one JSON object: %v", run.Stdout, err)
	}
}

// measuredRun is a run the fake's judge will answer about: the only kind
// anything may be asked to judge.
func measuredRun(gate flow.GateName) flow.GateRun {
	return flow.GateRun{Gate: gate, Outcome: flow.OutcomeMeasured, Stdout: []byte(`{}`)}
}

// The default has to let an unrelated test get on with its subject, and the
// terms it reports have to parse: the fake models the protocol, so a caller
// that carries a verdict's thresholds is written against a shape a real judge
// produces.
func TestBackend_JudgesAcceptableByDefaultWithTermsThatParse(t *testing.T) {
	run := measuredRun(flow.GateTested)
	v, err := gateWorktree(t, fake.New()).Judge(context.Background(), run)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if !v.Acceptable {
		t.Error("Acceptable = false, want the default")
	}
	var thresholds map[string]any
	if err := json.Unmarshal(v.Thresholds, &thresholds); err != nil || thresholds == nil {
		t.Errorf("Thresholds = %q, want one JSON object: %v", v.Thresholds, err)
	}
	// The measurement travels with the verdict, or nothing can re-check it.
	if v.Run.Gate != run.Gate || v.Run.Outcome != run.Outcome {
		t.Errorf("Run = %+v, want the measurement that was judged", v.Run)
	}
}

// A REFUSAL IS A VERDICT. A fake that returned one as an error would let a
// caller be written that cannot tell "the project says no" from "the judge
// could not answer" — and that caller would treat a broken judging layer as a
// failing tree, refusing sound changes for a reason nowhere in them.
func TestBackend_SetGateVerdictRefusesWithoutErroring(t *testing.T) {
	b := fake.New()
	b.SetGateVerdict(false)
	v, err := gateWorktree(t, b).Judge(context.Background(), measuredRun(flow.GateIntegration))
	if err != nil {
		t.Fatalf("Judge: %v — a refusal is an answer, not an error", err)
	}
	if v.Acceptable {
		t.Error("Acceptable = true, want the refusal that was configured")
	}
}

// The two requests no judge could answer. Only a measured run may be judged:
// the other outcomes mean no measurement exists, and a judge asked about one
// would have to invent an answer — which, read as a refusal, blames a change
// for a gate that never ran.
func TestBackend_JudgeRefusesWhatCannotBeJudged(t *testing.T) {
	for _, c := range []struct {
		name string
		run  flow.GateRun
	}{
		{"a run that measured nothing", flow.GateRun{Gate: flow.GateTested, Outcome: flow.OutcomeDied}},
		{"a run carrying no outcome at all", flow.GateRun{Gate: flow.GateTested}},
		{"an undeclared gate name", measuredRun("lint")},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, err := gateWorktree(t, fake.New()).Judge(context.Background(), c.run)
			if err == nil {
				t.Fatal("Judge answered a request no judge could answer")
			}
			if v.Acceptable {
				t.Error("Acceptable = true beside an error")
			}
			if v.Thresholds != nil {
				t.Errorf("Thresholds = %q, want none — nothing was compared", v.Thresholds)
			}
		})
	}
}

// A refusal configured AFTER the worktree was handed out still reaches it,
// which is the order a caller's test is naturally written in: claim, then set
// up the failure it is about to exercise.
//
// This is the one that regresses invisibly. A fake that only fixed the verdict
// at Worktree() time would leave such a test exercising the default — the
// ACCEPTABLE path — while its name and its author both say refusal, and it
// would go on passing for as long as the caller kept letting the change
// through.
func TestBackend_SetGateVerdictReachesAWorktreeAlreadyHandedOut(t *testing.T) {
	b := fake.New()
	wt := gateWorktree(t, b)
	b.SetGateVerdict(false)
	v, err := wt.Judge(context.Background(), measuredRun(flow.GateTested))
	if err != nil {
		t.Fatalf("Judge: %v — a refusal is an answer, not an error", err)
	}
	if v.Acceptable {
		t.Error("Acceptable = true: the refusal did not reach a worktree that already existed")
	}
}

// An undeclared name is a request no runner could attempt, so it is the one
// thing that is an error — and the GateRun beside it must carry no outcome. A
// caller that read a measurement out of it would act on a gate that never ran.
func TestBackend_RunGateRefusesAnUndeclaredNameWithNoOutcome(t *testing.T) {
	run, err := gateWorktree(t, fake.New()).RunGate(context.Background(), "lint")
	if err == nil {
		t.Fatal("RunGate accepted an undeclared gate name")
	}
	if run.Outcome != "" {
		t.Errorf("Outcome = %q, want none — no gate ran", run.Outcome)
	}
}

// The fake is what SDK tests drive the work-in-progress path against, so it has
// to model the property that path turns on: a record belongs to one item and
// one step, and is invisible under any other key.
func TestBackend_WorkInProgressIsKeyedByItemAndStep(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	_ = addItem(b, "1")
	_ = addItem(b, "2")
	ref1, ref2 := itemRef("1"), itemRef("2")
	if _, err := b.Claim(ctx, ref1, nil); err != nil {
		t.Fatalf("Claim 1: %v", err)
	}
	if _, err := b.Claim(ctx, ref2, nil); err != nil {
		t.Fatalf("Claim 2: %v", err)
	}

	if got, err := b.LoadWorkInProgress(ctx, ref1, "plan"); got != "" || err != nil {
		t.Errorf("Load with nothing stored = (%q, %v), want (\"\", nil)", got, err)
	}
	if err := b.SaveWorkInProgress(ctx, ref1, "plan", "item 1's reasoning"); err != nil {
		t.Fatalf("SaveWorkInProgress: %v", err)
	}
	if got, _ := b.LoadWorkInProgress(ctx, ref1, "plan"); got != "item 1's reasoning" {
		t.Errorf("Load under its own key = %q, want the stored body", got)
	}
	if got, _ := b.LoadWorkInProgress(ctx, ref2, "plan"); got != "" {
		t.Errorf("Load under another item = %q, want nothing", got)
	}
	if got, _ := b.LoadWorkInProgress(ctx, ref1, "review"); got != "" {
		t.Errorf("Load under another step = %q, want nothing", got)
	}
	if err := b.ClearWorkInProgress(ctx, ref1, "plan"); err != nil {
		t.Fatalf("ClearWorkInProgress: %v", err)
	}
	if err := b.ClearWorkInProgress(ctx, ref1, "plan"); err != nil {
		t.Errorf("second Clear = %v, want nil", err)
	}
}

// Releasing ends that reasoning's life — there is nothing left to resume, and
// what stays behind is prose nobody asked for.
func TestBackend_ReleaseDropsWorkInProgress(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := addItem(b, "1")
	if _, err := b.Claim(ctx, ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := b.SaveWorkInProgress(ctx, ref, "plan", "reasoning"); err != nil {
		t.Fatalf("SaveWorkInProgress: %v", err)
	}
	if err := b.Release(ctx, ref); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got, err := b.LoadWorkInProgress(ctx, ref, "plan"); got != "" || err != nil {
		t.Errorf("Load after Release = (%q, %v), want nothing left", got, err)
	}
}

// ---------------------------------------------------------------------------
// DetectCapabilities.
// ---------------------------------------------------------------------------

// The ambient account holds everything by default. A fake exists to let a
// flow's own logic be exercised, and an ambient account that could assume no
// role would refuse before any of it ran.
func TestBackend_DetectCapabilities_AmbientDefaultsToEverything(t *testing.T) {
	got, err := fake.New().DetectCapabilities(context.Background(), "")
	if err != nil {
		t.Fatalf("DetectCapabilities: %v", err)
	}
	if !slices.Equal(got, flow.AllCapabilities()) {
		t.Errorf("DetectCapabilities(ambient) = %v, want every capability", got)
	}
}

func TestBackend_SetCapabilities(t *testing.T) {
	b := fake.New()
	b.SetCapabilities("", flow.CapPush)
	b.SetCapabilities("bob", flow.CapPush, flow.CapMerge)

	if got, _ := b.DetectCapabilities(context.Background(), ""); !slices.Equal(got, []flow.Capability{flow.CapPush}) {
		t.Errorf("DetectCapabilities(ambient) = %v, want the narrowed set", got)
	}
	if got, _ := b.DetectCapabilities(context.Background(), "bob"); !slices.Equal(got, []flow.Capability{flow.CapPush, flow.CapMerge}) {
		t.Errorf("DetectCapabilities(bob) = %v, want what was set for bob", got)
	}
}

// An account nothing was set for has no capabilities, and that is an ANSWER
// rather than a failure: it is what the real backend says about somebody who is
// not a collaborator, and role derivation acts on it.
func TestBackend_DetectCapabilities_UnsetAccountHasNone(t *testing.T) {
	got, err := fake.New().DetectCapabilities(context.Background(), "stranger")
	if err != nil {
		t.Fatalf("DetectCapabilities: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("DetectCapabilities(stranger) = %v, want nothing", got)
	}
}

// The answer is a copy: a caller sorting or truncating what it was handed must
// not rewrite what the next call reports.
func TestBackend_DetectCapabilities_AnswerIsACopy(t *testing.T) {
	b := fake.New()
	got, err := b.DetectCapabilities(context.Background(), "")
	if err != nil {
		t.Fatalf("DetectCapabilities: %v", err)
	}
	got[0] = "rewritten"
	if again, _ := b.DetectCapabilities(context.Background(), ""); !slices.Equal(again, flow.AllCapabilities()) {
		t.Errorf("the stored set followed the caller's slice: %v", again)
	}
}

// --- Finalize ---

// Finalize records the disposition the finalizing election carried, beside the
// flag. The read is required with the write: an orchestrator that accepted the
// disposition and could not report it back would be accepting a value nobody
// can observe.
func TestBackend_Finalize_RecordsTheDisposition(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")
	b.SetStatus("1", flow.StatusTerminal, "done")

	if err := b.Finalize(ctx, ref, flow.DispositionRejected); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	state, _ := b.Load(ctx, ref)
	if !state.Finalized {
		t.Error("Finalized = false after Finalize")
	}
	if state.FinalizedAs != flow.DispositionRejected {
		t.Errorf("FinalizedAs = %q, want rejected", state.FinalizedAs)
	}
	// A finished flow awaits nobody.
	if !state.Awaits.Empty() {
		t.Errorf("Awaits = %+v after Finalize, want the zero value", state.Awaits)
	}
}

// The non-terminal refusal survives the new argument: a disposition is what the
// flow decided, not permission to record a run complete on an open item.
func TestBackend_Finalize_StillRefusesANonTerminalItem(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	err := b.Finalize(ctx, ref, flow.DispositionResolved)
	if !errors.Is(err, flow.ErrUnavailable) {
		t.Fatalf("Finalize on an open item = %v, want ErrUnavailable", err)
	}
	state, _ := b.Load(ctx, ref)
	if state.Finalized || state.FinalizedAs != "" {
		t.Errorf("a refused Finalize recorded finalized=%v as=%q", state.Finalized, state.FinalizedAs)
	}
}

// --- The role predicate ---

// awaitingRole is an item whose journal leaves it awaiting `role`.
func awaitingRole(t *testing.T, b *fake.Orchestrator, id string, role flow.RoleName) flow.ItemRef {
	t.Helper()
	ref := claimed(t, b, id)
	e := entry("plan", 1, "the plan", "impl")
	e.Awaits = flow.Awaits{Role: role}
	if err := b.AppendEntry(context.Background(), ref, e); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	return ref
}

// An item awaiting a role this account cannot assume sits at `awaits`: somebody
// else's move. It is above `outside-remit` — the item IS this binary's work —
// and below `blocked`, which is an item this account would act on if it could.
func TestBackend_Availability_AwaitsWhenTheRoleIsNotAssumable(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := awaitingRole(t, b, "1", "maintainer")

	onlyContributor := func(r flow.RoleName) bool { return r == "contributor" }
	info, err := b.Get(ctx, ref, "binary", nil, onlyContributor)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Availability != flow.AvailAwaits {
		t.Errorf("Availability = %q, want awaits", info.Availability)
	}
	if info.Awaits.Role != "maintainer" {
		t.Errorf("Awaits.Role = %q, want maintainer", info.Awaits.Role)
	}
	// It is still in `processable` — this binary's work — and out of
	// `actionable`, which is the boundary the rung names.
	if !info.Availability.InScope(flow.ScopeProcessable) {
		t.Error("an awaits item should still be processable")
	}
	if info.Availability.InScope(flow.ScopeActionable) {
		t.Error("an awaits item must not be actionable")
	}
}

func TestBackend_Availability_AssumableRoleIsUnaffected(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := awaitingRole(t, b, "1", "contributor")

	onlyContributor := func(r flow.RoleName) bool { return r == "contributor" }
	info, err := b.Get(ctx, ref, "binary", nil, onlyContributor)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Availability == flow.AvailAwaits {
		t.Errorf("Availability = %q, want a rung above awaits — the role is assumable", info.Availability)
	}
}

// A nil predicate filters nothing, matching the acceptsType convention.
func TestBackend_Availability_NilPredicateFiltersNothing(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := awaitingRole(t, b, "1", "maintainer")

	info, err := b.Get(ctx, ref, "binary", nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Availability == flow.AvailAwaits {
		t.Error("a nil predicate reported awaits; it must filter nothing")
	}
}

// An awaited SIGNAL is nobody's move, not somebody else's: it reports blocked
// (waits-on-condition), never awaits, whatever the role predicate says.
func TestBackend_Availability_AnAwaitedSignalIsNotAwaits(t *testing.T) {
	ctx := context.Background()
	b := fake.New(flow.Signal("pr-merged", "test"))
	ref := claimed(t, b, "1")
	e := entry("pr-open", 1, "", "pr-merged")
	e.Result = flow.ArtifactBody{}
	e.Awaits = flow.Awaits{Signal: "pr-merged"}
	if err := b.AppendEntry(ctx, ref, e); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}

	nobody := func(flow.RoleName) bool { return false }
	info, err := b.Get(ctx, ref, "binary", nil, nobody)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Availability == flow.AvailAwaits {
		t.Errorf("Availability = awaits for a signal wait; nobody's move is not somebody else's")
	}
	if info.Awaits.Signal != "pr-merged" {
		t.Errorf("Awaits.Signal = %q, want pr-merged", info.Awaits.Signal)
	}
}

// Eligibility, not a sort key: an item whose awaited role this account cannot
// assume is OMITTED from the auto-selectable set rather than ranked last.
func TestBackend_ListAutoSelectable_OmitsAnUnassumableRole(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	mine := awaitingRole(t, b, "mine", "contributor")
	_ = awaitingRole(t, b, "theirs", "maintainer")

	onlyContributor := func(r flow.RoleName) bool { return r == "contributor" }
	refs, err := b.ListAutoSelectable(ctx, nil, onlyContributor)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	if len(refs) != 1 || refs[0].Display != mine.Display {
		t.Errorf("ListAutoSelectable = %+v, want only the contributor item", refs)
	}

	// With no predicate both come back: nil filters nothing.
	refs, err = b.ListAutoSelectable(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	if len(refs) != 2 {
		t.Errorf("ListAutoSelectable with a nil predicate = %+v, want both items", refs)
	}
}

// --- Drift ---

// The fake reports the pair a test recorded: a reading is evidence for a route
// election, so a test exercising the behind-the-mainline route says how far
// behind.
func TestBackend_WorktreeDriftReportsTheRecordedPair(t *testing.T) {
	ctx := context.Background()
	b := fake.New()
	ref := claimed(t, b, "1")

	wt, err := b.Worktree(ctx, ref)
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if d, err := wt.Drift(ctx); err != nil || !d.Level() {
		t.Errorf("Drift on a fresh worktree = (%+v, %v), want level", d, err)
	}

	want := flow.Drift{Ahead: 3, Behind: 12, At: time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)}
	b.SetDrift(want)
	got, err := wt.Drift(ctx)
	if err != nil {
		t.Fatalf("Drift: %v", err)
	}
	if got != want {
		t.Errorf("Drift = %+v, want %+v", got, want)
	}
	if got.Level() {
		t.Error("Level() = true on a drifted pair")
	}
}
