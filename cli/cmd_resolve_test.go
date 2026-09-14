package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// resolveTestApp builds an App with a single-step "write plan" flow over the
// given backend, pre-loaded with one fake item. Mirrors orchestrator_test.go's
// testApp but does NOT pre-claim the item (auto-select tests need an
// un-leased arena) and lets the caller swap in a wrapping backend (e.g. one
// that fails ListEligible). The step resolves its artifact, so the run
// advances and then finalizes.
func resolveTestApp(t *testing.T, be flow.Orchestrator) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	return resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
	})
}

// resolveTestAppStep is resolveTestApp with a caller-supplied handler for the
// single "write plan" step — the lever for driving the loop to an outcome
// other than done (return nil without resolving to park it, return an error to
// fail it).
func resolveTestAppStep(t *testing.T, be flow.Orchestrator, step func(flow.StepCtx) (flow.StepResult, error)) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	return resolveTestAppPrompts(t, be, step, flow.PromptsAgent)
}

// resolveTestAppPrompts is resolveTestAppStep with the step's Prompts chosen by
// the caller — the lever for a test about what a mechanical step is spared.
func resolveTestAppPrompts(t *testing.T, be flow.Orchestrator, step func(flow.StepCtx) (flow.StepResult, error), prompts flow.PromptPolicy) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	return resolveTestAppFlow(t, be, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", step, flow.StepConfig{Prompts: prompts, Entry: true, Role: "contributor", MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})
}

// resolveTestAppFlow is the fixture under the others: the isolated App with the
// contributor role declared, and a graph the caller registers — the lever for a
// test about how the loop treats one step differently from the next. "plan"
// (markdown) and "commit" (commit hash) are the artifacts a graph may produce.
// Every role the graph declares is covered; a test about declining one uses
// resolveTestAppCovering.
func resolveTestAppFlow(t *testing.T, be flow.Orchestrator, configure func(*flow.Flow)) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	return resolveTestAppCovering(t, be, nil, configure)
}

// resolveTestAppCovering is resolveTestAppFlow with the binary's coverage
// chosen by the caller — the lever for a test about a role the binary declines
// though its account could back it. Nil covers every role the graph declares.
func resolveTestAppCovering(t *testing.T, be flow.Orchestrator, covered []flow.RoleName, configure func(*flow.Flow)) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	// Isolate from real credential discovery (Keychain, claude binary) so
	// reportQuota's exec calls don't hang or hit the network, and from the
	// quota cache so one resolve test's recorded failure is not another's
	// served reading.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	useTempQuotaCache(t)
	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	app := &App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{
			flow.Artifact("plan", flow.ArtifactMarkdown),
			flow.Artifact("commit", flow.ArtifactCommitHash),
		},
		// Whatever the backend can observe, so a graph the caller registers may
		// include a wait. A backend with no signals declares none, which is
		// what every existing caller of this fixture already had.
		Signals: be.SupportedSignals(),
		Out:     out,
		Err:     errBuf,
	}
	f := flow.NewFlow("implement", []flow.ItemType{"task"})
	f.Role("contributor", flow.CapPush)
	configure(f)

	app.Flow = f
	app.Coverage = covered
	if covered == nil {
		app.Coverage = f.RoleNames()
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return app, out, errBuf
}

// failingListBackend wraps a fake backend and forces ListEligible to error.
// Used to prove a code path doesn't touch ListEligible — if it does, the
// surrounding test fails.
type failingListBackend struct {
	*fake.Orchestrator
	err error
}

func (b *failingListBackend) ListAutoSelectable(ctx context.Context, _ []flow.TagId, _ func(flow.RoleName) bool) ([]flow.ItemRef, error) {
	return nil, b.err
}

// failingLookupActiveClaimBackend forces LookupActiveClaim to error, so we
// can exercise the no-arg / LookupActiveClaim error branch added by this
// change.
type failingLookupActiveClaimBackend struct {
	*fake.Orchestrator
	err error
}

func (b *failingLookupActiveClaimBackend) LookupActiveClaim(ctx context.Context) (*flow.Claim, error) {
	return nil, b.err
}

// failingClaimBackend lets ListEligible succeed but forces the subsequent
// Claim call to fail — exercises the auto-select branch's "Claim failed on
// the chosen ref" error path.
type failingClaimBackend struct {
	*fake.Orchestrator
	claimErr error
}

func (b *failingClaimBackend) Claim(ctx context.Context, ref flow.ItemRef, overrides []flow.ClaimOverride) (flow.Claim, error) {
	return flow.Claim{}, b.claimErr
}

// resolvingFailingListBackend implements flow.RefResolver (so the explicit-id
// path takes the fast lane and never hits ListEligible) AND forces
// ListEligible to error — together they prove the explicit-id branch of
// cmdResolve never calls ListEligible.
type resolvingFailingListBackend struct {
	*fake.Orchestrator
	listErr error
}

func (b *resolvingFailingListBackend) ListAutoSelectable(ctx context.Context, _ []flow.TagId, _ func(flow.RoleName) bool) ([]flow.ItemRef, error) {
	return nil, b.listErr
}

func (b *resolvingFailingListBackend) ResolveRef(ctx context.Context, id string) (flow.ItemRef, error) {
	return flow.ItemRef{OrchestratorName: "fake", Display: id, Ref: json.RawMessage(`"` + id + `"`)}, nil
}

func TestCmdResolve_AutoSelectsWhenUnleased(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "auto-selecting") {
		t.Errorf("expected Err to mention auto-selecting; got %q", errBuf.String())
	}

	// The item must now be held, credited to the account the arena acts as.
	ref := flow.ItemRef{OrchestratorName: "fake", Ref: json.RawMessage(`"1"`)}
	info, err := be.LookupClaim(context.Background(), ref)
	if err != nil {
		t.Fatalf("LookupClaim: %v", err)
	}
	if info == nil || info.Account != be.Account() {
		t.Errorf("LookupClaim = %+v, want the arena's own account %q", info, be.Account())
	}

	// Plan artifact must have been resolved (proves the loop actually ran
	// the step, not just claimed).
	claim := flow.Claim{OrchestratorName: "fake", ItemRef: ref, Account: be.Account(), Token: json.RawMessage(`{}`)}
	state, err := be.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := state.Artifact("plan")
	if !rec.Resolved || rec.Markdown != "the plan" {
		t.Errorf("plan artifact = %+v, want resolved markdown 'the plan'", rec)
	}
}

func TestCmdResolve_ResumesActiveClaim(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	// Pre-claim as app.Owner — cmdResolve must resume this claim via
	// LookupActiveClaim and never touch ListEligible (which fails here).
	ref := flow.ItemRef{OrchestratorName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)}
	if _, err := inner.Claim(context.Background(), ref, nil); err != nil {
		t.Fatalf("pre-claim: %v", err)
	}
	be := &failingListBackend{Orchestrator: inner, err: errors.New("ListEligible must not be called on resume path")}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "auto-selecting") {
		t.Errorf("resume path must not log auto-selecting; got %q", errBuf.String())
	}
}

func TestCmdResolve_EmptyEligibleExitsClean(t *testing.T) {
	be := fake.New() // no items, no claim
	app, out, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (empty eligible is a clean exit, not an error); err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no items in the auto-selectable set") {
		t.Errorf("expected Err to mention 'no items in the auto-selectable set'; got %q", errBuf.String())
	}
	if out.Len() != 0 {
		t.Errorf("Out should be empty on empty-eligible exit; got %q", out.String())
	}
}

// TestCmdResolve_UnmatchedTypeBlocks (#10): an item whose type no flow accepts
// stops the loop at exit 1 with the mismatch named, and never reports the run
// as finalized — neither in the outcome line nor in the progress peek that
// precedes it.
func TestCmdResolve_UnmatchedTypeBlocks(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "chore", Title: "1"})
	app, _, errBuf := resolveTestApp(t, be) // flow accepts "task" only

	code := app.cmdResolve(context.Background(), nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (blocked); err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), `no flow accepts item type "chore"`) {
		t.Errorf("expected Err to name the unmatched type; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "is blocked") {
		t.Errorf("expected Err to report the item as blocked; got %q", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "finalized ✓") {
		t.Errorf("Err must not claim the item finalized; got %q", errBuf.String())
	}
	// The progress peek runs before RunOne and must not announce a finalize
	// that will not happen.
	if strings.Contains(errBuf.String(), "finalizing…") {
		t.Errorf("progress peek must not announce finalizing; got %q", errBuf.String())
	}
	// Nor may the outcome line label the stop as the finalize step: the result
	// carries no step name, and the "(finalize)" default for that would print
	// "(finalize) → blocked" — the same misreport one line further on.
	if strings.Contains(errBuf.String(), "(finalize)") {
		t.Errorf("outcome line must not label the stop as a finalize; got %q", errBuf.String())
	}
	// It says what did happen, rather than leaving the label blank: dropping
	// "(finalize)" without putting anything in its place prints "resolve:  →
	// blocked", which passes the check above and tells the operator nothing.
	if !strings.Contains(errBuf.String(), "(no step) → blocked") {
		t.Errorf("outcome line must label the stop as reaching no step; got %q", errBuf.String())
	}
}

// TestCmdResolve_ItemWithAJournalIsNarratedPastTheRemit: the peek and the
// advance read the remit through the same inRemit, which stops being consulted
// at the journal's first entry (docs/flow-registration.md § Item types). An
// item outside the remit that already carries a journal is past that point, so
// the peek must name the step RunOne is about to run — announcing the block
// here while the run goes on to run a step is the misreport the branch order
// exists to prevent, inverted.
func TestCmdResolve_ItemWithAJournalIsNarratedPastTheRemit(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "chore", Title: "1", Journal: []flow.JournalEntry{
		// One completed execution, as #239's AppendEntry will record it. Only
		// its presence is read here — the checklist still picks the step (#245).
		{Step: "plan", Execution: 1, Route: flow.Route{Next: "plan"}},
	}})
	app, _, errBuf := resolveTestApp(t, be) // remit: "task" only

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "no flow accepts this item's type") {
		t.Errorf("peek announced a block the run never takes; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), `running "write plan"…`) {
		t.Errorf("peek did not name the step about to run; got %q", errBuf.String())
	}
	// And the run agreed with the narration: it ran the step through to the
	// finalize pass rather than stopping on the type. The pass itself, not the
	// tick — the fake's item is still open, so Finalize refuses and nothing is
	// recorded complete.
	if !strings.Contains(errBuf.String(), "(finalize) → done") {
		t.Errorf("expected the run to reach the finalize pass; got %q", errBuf.String())
	}
}

// TestCmdResolve_FinalizedUnmatchedTypeNarratesFinalize (#10): the peek's
// unmatched-type branch carries RunOne's already-finalized exemption. An item
// that IS finalized takes the finalize path, so announcing "no flow accepts
// this item's type" for it is the same misreport inverted.
func TestCmdResolve_FinalizedUnmatchedTypeNarratesFinalize(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "chore", Title: "1", Finalized: true})
	// Terminal on the tracker as well, so the finalize pass actually records
	// the run complete: the tick asserted below is gated on that, and an item
	// the orchestrator still reads as open would refuse it.
	be.SetStatus("1", flow.StatusTerminal, "completed")
	app, _, errBuf := resolveTestApp(t, be) // flow accepts "task" only

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (already finalized); err=%q", code, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "no flow accepts this item's type") {
		t.Errorf("peek must not report a block that will not happen; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "finalized ✓") {
		t.Errorf("expected the finalize path to be narrated; got %q", errBuf.String())
	}
}

func TestCmdResolve_ListEligibleError(t *testing.T) {
	inner := fake.New()
	be := &failingListBackend{Orchestrator: inner, err: errors.New("boom")}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "boom") {
		t.Errorf("expected Err to surface backend error; got %q", errBuf.String())
	}
}

func TestCmdResolve_LookupActiveClaimError(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &failingLookupActiveClaimBackend{Orchestrator: inner, err: errors.New("lookup boom")}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "lookup boom") {
		t.Errorf("expected Err to surface LookupActiveClaim error; got %q", errBuf.String())
	}
}

func TestCmdResolve_AutoSelectClaimError(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &failingClaimBackend{Orchestrator: inner, claimErr: errors.New("claim boom")}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "claim boom") {
		t.Errorf("expected Err to surface Claim error; got %q", errBuf.String())
	}
	// The auto-selecting log line is printed BEFORE the failing Claim call,
	// so it must appear — proves we reached the auto-select branch (not the
	// resume branch).
	if !strings.Contains(errBuf.String(), "auto-selecting") {
		t.Errorf("expected Err to include the auto-selecting line that precedes the Claim attempt; got %q", errBuf.String())
	}
}

// conflictThenOkBackend lets ListEligible return a pre-set list of refs;
// Claim returns the tracker lease-conflict error for any ref in
// conflictRefs, and otherwise delegates to the inner fake backend. Lets us
// drive the auto-select iteration-on-conflict path deterministically.
type conflictThenOkBackend struct {
	*fake.Orchestrator
	refs          []flow.ItemRef
	conflictRefs  map[string]bool
	claimAttempts []string
	listErr       error
	nonConflict   error // if set, every Claim returns this error instead
}

func (b *conflictThenOkBackend) ListAutoSelectable(ctx context.Context, _ []flow.TagId, _ func(flow.RoleName) bool) ([]flow.ItemRef, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	return b.refs, nil
}

func (b *conflictThenOkBackend) Claim(ctx context.Context, ref flow.ItemRef, overrides []flow.ClaimOverride) (flow.Claim, error) {
	id := string(ref.Ref)
	b.claimAttempts = append(b.claimAttempts, id)
	if b.nonConflict != nil {
		return flow.Claim{}, b.nonConflict
	}
	if b.conflictRefs[id] {
		return flow.Claim{}, flow.ErrClaimRefused{
			Code:       "item-already-leased",
			ItemScoped: true,
			Reason:     fmt.Sprintf("item %s already leased to arena \"other\"", id),
		}
	}
	return b.Orchestrator.Claim(ctx, ref, overrides)
}

func TestCmdResolve_AutoSelectIteratesOnLeaseConflict(t *testing.T) {
	inner := fake.New()
	// All three exist: a ref the eligibility mirror offers is a ref that can be
	// read, and the claim reads the item before it takes a lease on it.
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	inner.AddItem("2", flow.Item{Type: "task", Title: "2"})
	inner.AddItem("3", flow.Item{Type: "task", Title: "3"})
	refs := []flow.ItemRef{
		{OrchestratorName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)},
		{OrchestratorName: "fake", Display: "2", Ref: json.RawMessage(`"2"`)},
		{OrchestratorName: "fake", Display: "3", Ref: json.RawMessage(`"3"`)},
	}
	be := &conflictThenOkBackend{
		Orchestrator: inner,
		refs:         refs,
		conflictRefs: map[string]bool{`"1"`: true, `"2"`: true},
	}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if len(be.claimAttempts) != 3 {
		t.Errorf("Claim attempts = %v, want 3 (two conflicts + one success)", be.claimAttempts)
	}
	if !strings.Contains(errBuf.String(), "1 — ") || !strings.Contains(errBuf.String(), "trying next") {
		t.Errorf("expected ref 1 conflict-skip line; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "2 — ") || !strings.Contains(errBuf.String(), "trying next") {
		t.Errorf("expected ref 2 conflict-skip line; got %q", errBuf.String())
	}
	// Ref 3 must actually be claimed and driven through the step.
	claim := flow.Claim{OrchestratorName: "fake", ItemRef: refs[2], Account: "alice", Token: json.RawMessage(`{}`)}
	state, err := inner.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec := state.Artifact("plan"); !rec.Resolved {
		t.Errorf("expected plan artifact resolved on ref 3, got %+v", rec)
	}
}

func TestCmdResolve_AutoSelectAllRefsConflictExitsZero(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	inner.AddItem("2", flow.Item{Type: "task", Title: "2"})
	refs := []flow.ItemRef{
		{OrchestratorName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)},
		{OrchestratorName: "fake", Display: "2", Ref: json.RawMessage(`"2"`)},
	}
	be := &conflictThenOkBackend{
		Orchestrator: inner,
		refs:         refs,
		conflictRefs: map[string]bool{`"1"`: true, `"2"`: true},
	}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (all-conflict is a clean no-op exit); err=%q", code, errBuf.String())
	}
	// The closing line reports THAT nothing could be claimed, not why: the per-ref
	// lines above carry the reasons, and a lease conflict is only one of them.
	if !strings.Contains(errBuf.String(), "no eligible item could be claimed") {
		t.Errorf("expected the nothing-claimed message; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "already leased to arena") {
		t.Errorf("the per-ref lines do not carry the conflict reason; got %q", errBuf.String())
	}
}

func TestCmdResolve_AutoSelectNonConflictErrorExitsOne(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	inner.AddItem("2", flow.Item{Type: "task", Title: "2"})
	refs := []flow.ItemRef{
		{OrchestratorName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)},
		{OrchestratorName: "fake", Display: "2", Ref: json.RawMessage(`"2"`)},
	}
	be := &conflictThenOkBackend{
		Orchestrator: inner,
		refs:         refs,
		nonConflict:  errors.New("dial tcp: connection refused"),
	}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 on non-conflict Claim error; err=%q", code, errBuf.String())
	}
	if len(be.claimAttempts) != 1 {
		t.Errorf("Claim attempts = %v, want 1 (non-conflict errors must NOT iterate)", be.claimAttempts)
	}
	if !strings.Contains(errBuf.String(), "connection refused") {
		t.Errorf("expected non-conflict error surfaced; got %q", errBuf.String())
	}
}

func TestAutoSelectRefusalBranching(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantRetry bool // true → the loop should try the next ref
	}{
		{"nil error", nil, false},
		{"item-scoped typed refusal", flow.ErrClaimRefused{Code: "item-already-leased", ItemScoped: true, Reason: "already leased"}, true},
		{"arena-scoped typed refusal", flow.ErrClaimRefused{Code: "not-admitted", ItemScoped: false, Reason: "arena not admitted"}, false},
		{"unrelated error", errors.New("dial tcp: connection refused"), false},
		{"wrapped item-scoped refusal", fmt.Errorf("backend: %w", flow.ErrClaimRefused{Code: "claim-race", ItemScoped: true, Reason: "race"}), true},
		{"wrapped arena-scoped refusal", fmt.Errorf("backend: %w", flow.ErrClaimRefused{Code: "not-admitted", ItemScoped: false, Reason: "not admitted"}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.err == nil {
				return // nil is not a refusal
			}
			var refused flow.ErrClaimRefused
			got := errors.As(c.err, &refused) && refused.ItemScoped
			if got != c.wantRetry {
				t.Errorf("retry=%v, want %v for err=%v", got, c.wantRetry, c.err)
			}
		})
	}
}

// TestCmdResolve_AutoSelectStopsOnArenaScopedRefusal verifies that the
// auto-select loop exits 1 on an arena-scoped (ItemScoped=false) refusal
// instead of trying the next ref.
func TestCmdResolve_AutoSelectStopsOnArenaScopedRefusal(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	inner.AddItem("2", flow.Item{Type: "task", Title: "2"})
	refs := []flow.ItemRef{
		{OrchestratorName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)},
		{OrchestratorName: "fake", Display: "2", Ref: json.RawMessage(`"2"`)},
	}
	be := &arenaScopedRefusalBackend{
		Orchestrator: inner,
		refs:         refs,
	}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (arena-scoped refusal must stop); err=%q", code, errBuf.String())
	}
	if be.claimAttempts != 1 {
		t.Errorf("Claim attempts = %d, want 1 (must NOT iterate past arena-scoped refusal)", be.claimAttempts)
	}
	if !strings.Contains(errBuf.String(), "refused") {
		t.Errorf("expected refusal message; got %q", errBuf.String())
	}
}

// arenaScopedRefusalBackend returns an arena-scoped (ItemScoped=false) refusal
// from every Claim call.
type arenaScopedRefusalBackend struct {
	*fake.Orchestrator
	refs          []flow.ItemRef
	claimAttempts int
}

func (b *arenaScopedRefusalBackend) ListAutoSelectable(ctx context.Context, _ []flow.TagId, _ func(flow.RoleName) bool) ([]flow.ItemRef, error) {
	return b.refs, nil
}

func (b *arenaScopedRefusalBackend) Claim(ctx context.Context, ref flow.ItemRef, overrides []flow.ClaimOverride) (flow.Claim, error) {
	b.claimAttempts++
	return flow.Claim{}, flow.ErrClaimRefused{
		Code:   "not-admitted",
		Reason: "arena not admitted (check \"git-identity\")",
		Check:  "git-identity",
	}
}

// ---------------------------------------------------------------------------
// Output modes. resolve is a STREAM, not a one-shot report: its human output
// is the stderr narration (printed in both modes) and its stdout carries
// per-step InvocationResult objects and nothing else — in human mode, nothing
// at all.
// ---------------------------------------------------------------------------

// decodeResultStream decodes the whole of s as a stream of InvocationResult
// objects and asserts it is line-oriented (one compact object per line), which
// is what `resolve | jq` and `resolve > steps.json` consume.
func decodeResultStream(t *testing.T, s string) []flow.InvocationResult {
	t.Helper()
	var got []flow.InvocationResult
	dec := json.NewDecoder(strings.NewReader(s))
	for dec.More() {
		var res flow.InvocationResult
		if err := dec.Decode(&res); err != nil {
			t.Fatalf("decode stream %q: %v", s, err)
		}
		got = append(got, res)
	}
	if n := strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1; s != "" && n != len(got) {
		t.Errorf("stream has %d lines for %d objects — want one compact object per line; got %q", n, len(got), s)
	}
	return got
}

func TestCmdResolve_HumanModeWritesNothingToStdout(t *testing.T) {
	t.Setenv(outputEnv, "")
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	// The fixture injects a bytes.Buffer for Out, which resolveOutput reads as
	// human — the same mode a terminal gets.
	app, out, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if out.Len() != 0 {
		t.Errorf("human mode must write nothing to stdout; got %q", out.String())
	}
	// The narration is the human output, and it is on stderr.
	if !strings.Contains(errBuf.String(), `resolve: plan → done`) {
		t.Errorf("expected the step outcome narrated on stderr; got %q", errBuf.String())
	}
}

func TestCmdResolve_ExplicitHumanFlagWritesNothingToStdout(t *testing.T) {
	t.Setenv(outputEnv, "json") // --human must win over the environment
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, out, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"--human"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if out.Len() != 0 {
		t.Errorf("--human must write nothing to stdout; got %q", out.String())
	}
}

func TestCmdResolve_JSONFlagStreamsResultsAndStillNarrates(t *testing.T) {
	t.Setenv(outputEnv, "")
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, out, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"--json"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	// One result for the step, one for the finalize pass that follows it.
	got := decodeResultStream(t, out.String())
	if len(got) != 2 {
		t.Fatalf("stdout carried %d results, want 2 (step + finalize); got %q", len(got), out.String())
	}
	if got[0].Step != "plan" || got[0].Status != "done" {
		t.Errorf("first result = %+v, want step %q status done", got[0], "plan")
	}
	if got[1].Step != "" || got[1].Status != "done" {
		t.Errorf("second result = %+v, want the empty-step finalize, status done", got[1])
	}
	// JSON mode does not silence the narration: `resolve > steps.json` must
	// still show progress on the terminal.
	if !strings.Contains(errBuf.String(), `resolve: plan → done`) {
		t.Errorf("JSON mode must still narrate to stderr; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "(finalize) → done") {
		t.Errorf("JSON mode must still narrate the finalize line; got %q", errBuf.String())
	}
}

func TestCmdResolve_FlowOutputEnvSelectsJSON(t *testing.T) {
	t.Setenv(outputEnv, "json")
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, out, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if len(decodeResultStream(t, out.String())) != 2 {
		t.Errorf("FLOW_OUTPUT=json must stream the results; got %q", out.String())
	}
}

// The mode decision must precede every claim: a contradictory pair exits 2
// without touching the backend. This backend fails LookupActiveClaim, so if
// the check ever moved after the claim work the exit code would be 1.
func TestCmdResolve_JSONAndHumanExitsTwoBeforeClaiming(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &failingLookupActiveClaimBackend{Orchestrator: inner, err: errors.New("lookup boom")}
	app, out, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"--json", "--human"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "--json and --human are mutually exclusive") {
		t.Errorf("expected the mutual-exclusion message; got %q", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "lookup boom") {
		t.Errorf("the mode check must precede any claim work; got %q", errBuf.String())
	}
	if out.Len() != 0 {
		t.Errorf("Out should be empty; got %q", out.String())
	}
}

// parseArgs's contract: --json is accepted on either side of the optional
// positional.
func TestCmdResolve_JSONFlagEitherSideOfPositional(t *testing.T) {
	t.Setenv(outputEnv, "")
	for _, args := range [][]string{{"--json", "1"}, {"1", "--json"}} {
		be := fake.New()
		be.AddItem("1", flow.Item{Type: "task", Title: "1"})
		app, out, errBuf := resolveTestApp(t, be)

		code := app.cmdResolve(context.Background(), args)
		if code != 0 {
			t.Fatalf("%v: exit code = %d, want 0; err=%q", args, code, errBuf.String())
		}
		if len(decodeResultStream(t, out.String())) != 2 {
			t.Errorf("%v: expected the JSON stream on stdout; got %q", args, out.String())
		}
	}
}

func TestCmdResolve_UnknownFlagExitsTwo(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, out, _ := resolveTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"--nope"}); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Errorf("Out should be empty on a usage error; got %q", out.String())
	}
}

// Regression guard: with an explicit <id>, cmdResolve must take the
// resolveClaimRef → Claim path and NEVER call ListEligible (the auto-select
// branch is only reached when no id is given and no claim is held).
func TestCmdResolve_ExplicitIdStillClaimsWithoutListEligible(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &resolvingFailingListBackend{Orchestrator: inner, listErr: errors.New("ListEligible must not be called on explicit-id path")}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}

	// The item must be held, credited to the account the arena acts as.
	ref := flow.ItemRef{OrchestratorName: "fake", Ref: json.RawMessage(`"1"`)}
	info, err := inner.LookupClaim(context.Background(), ref)
	if err != nil {
		t.Fatalf("LookupClaim: %v", err)
	}
	if info == nil || info.Account != be.Account() {
		t.Errorf("LookupClaim = %+v, want the arena's own account %q", info, be.Account())
	}
}

// claimCountingBackend counts Claim calls on top of resolvingFailingListBackend
// — so a test gets both halves of the id route's contract at once: every
// `resolve <id>` claims, and none of them falls through to auto-selection.
type claimCountingBackend struct {
	*resolvingFailingListBackend
	claims int
}

func (b *claimCountingBackend) Claim(ctx context.Context, ref flow.ItemRef, overrides []flow.ClaimOverride) (flow.Claim, error) {
	b.claims++
	return b.resolvingFailingListBackend.Claim(ctx, ref, overrides)
}

// The documented sequence, end to end (#201): `claim <id>` then `resolve <id>`
// then `resolve <id>` again mid-flight. The id route claims first whether or not
// the item is already held — claims-first (docs/cli.md § Resolving) and
// idempotent for the holder (docs/cli.md § Claiming) are one route, not two.
//
// This pins the CLI's SELECTION contract only: the fake orchestrator has no
// worktree preconditions (pkg/orchestrator/fake/fake.go Claim), so the refusal
// the defect actually produced — "not on main" from a held re-claim — is pinned
// by TestBackend_Claim_HeldReclaimOffBaseChangesNothing in the github package.
func TestCmdResolve_ByIdClaimsThenResumesTheHeldClaim(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &claimCountingBackend{resolvingFailingListBackend: &resolvingFailingListBackend{
		Orchestrator: inner,
		listErr:      errors.New("auto-selection must never be consulted when an id was named"),
	}}
	// A step that returns without resolving its artifact parks the run, so the
	// first `resolve` leaves the item mid-flight and still held — which is the
	// state the second `resolve <id>` has to be able to resume.
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) { return flow.StepResult{}, nil })

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("claim 1: exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("first resolve 1: exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("second resolve 1 (held, mid-flight): exit code = %d, want 0; err=%q", code, errBuf.String())
	}

	if be.claims != 3 {
		t.Errorf("Claim called %d times, want 3 (the claim plus one per `resolve <id>`)", be.claims)
	}
	if strings.Contains(errBuf.String(), "auto-selecting") {
		t.Errorf("naming an item must never fall through to auto-selection; got %q", errBuf.String())
	}

	ref := flow.ItemRef{OrchestratorName: "fake", Ref: json.RawMessage(`"1"`)}
	info, err := inner.LookupClaim(context.Background(), ref)
	if err != nil {
		t.Fatalf("LookupClaim: %v", err)
	}
	if info == nil || info.Account != be.Account() {
		t.Errorf("LookupClaim = %+v, want the item still held by this arena's account %q", info, be.Account())
	}
}

// failingLoadStateBackend forces Load to error. Both the progress peek
// and RunOne read state, so this drives the loop's RunOne-error branch — the
// one exit that leaves the loop without an InvocationResult to report.
type failingLoadStateBackend struct {
	*fake.Orchestrator
	err error
}

func (b *failingLoadStateBackend) Load(ctx context.Context, ref flow.ItemRef) (*flow.Item, error) {
	return nil, b.err
}

// The gate on the encode sits BEFORE the switch that ends the run, so every
// terminal outcome — not just the happy finalize — is reported on the machine
// stream. A park is precisely what a tool watches for (it is the operator's
// cue to act), and a fix that only encoded advancing steps would drop it
// silently while every existing test still passed.
func TestCmdResolve_ModeSplitHoldsOnEveryTerminalOutcome(t *testing.T) {
	cases := []struct {
		name       string
		step       func(flow.StepCtx) (flow.StepResult, error)
		wantCode   int
		wantStatus string
	}{
		// A park the run STOPS on: the kind decides that, and `blocked` is one
		// of the five a re-dispatch cannot clear. A clearable kind would be
		// re-dispatched under the bound (cmdResolve's parked arm) and put four
		// results on the stream rather than one, which is a different property
		// from the one this case is about.
		{"parked", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return flow.StepResult{}, ctx.Park(flow.ParkRequest{Kind: flow.ParkBlocked, Reason: "a person must act"})
		}, 0, "parked"},
		{"failed", func(ctx flow.StepCtx) (flow.StepResult, error) { return flow.StepResult{}, errors.New("handler boom") }, 1, "failed"},
	}
	for _, c := range cases {
		t.Run(c.name+"/json", func(t *testing.T) {
			t.Setenv(outputEnv, "")
			be := fake.New()
			be.AddItem("1", flow.Item{Type: "task", Title: "1"})
			app, out, errBuf := resolveTestAppStep(t, be, c.step)

			code := app.cmdResolve(context.Background(), []string{"--json"})
			if code != c.wantCode {
				t.Fatalf("exit code = %d, want %d; err=%q", code, c.wantCode, errBuf.String())
			}
			got := decodeResultStream(t, out.String())
			if len(got) != 1 {
				t.Fatalf("stdout carried %d results, want 1 (the %s step, then the run stops); got %q", len(got), c.name, out.String())
			}
			if got[0].Status != c.wantStatus {
				t.Errorf("result = %+v, want status %q", got[0], c.wantStatus)
			}
		})
		t.Run(c.name+"/human", func(t *testing.T) {
			t.Setenv(outputEnv, "")
			be := fake.New()
			be.AddItem("1", flow.Item{Type: "task", Title: "1"})
			app, out, errBuf := resolveTestAppStep(t, be, c.step)

			code := app.cmdResolve(context.Background(), []string{"--human"})
			if code != c.wantCode {
				t.Fatalf("exit code = %d, want %d; err=%q", code, c.wantCode, errBuf.String())
			}
			if out.Len() != 0 {
				t.Errorf("human mode must write nothing to stdout on a %s run; got %q", c.name, out.String())
			}
			// The outcome is still reported — on stderr, as prose.
			if !strings.Contains(errBuf.String(), "plan → "+c.wantStatus) {
				t.Errorf("expected the %s outcome narrated on stderr; got %q", c.wantStatus, errBuf.String())
			}
		})
	}
}

// A step that never produces a result at all: stdout stays empty even in JSON
// mode, because errors travel as plain text on stderr with the exit code as
// the signal. A reader of the stream must never have to tell a failure payload
// from an InvocationResult.
func TestCmdResolve_RunOneErrorKeepsStdoutClean(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &failingLoadStateBackend{Orchestrator: inner, err: errors.New("state boom")}
	app, out, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"--json"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout must carry InvocationResult objects and nothing else; got %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "state boom") {
		t.Errorf("expected the error on stderr; got %q", errBuf.String())
	}
}

// ---------------------------------------------------------------------------
// --tag on resolve
// ---------------------------------------------------------------------------

// tagFilterBackend wraps fake and implements TagFilterer.
type tagFilterBackend struct {
	*fake.Orchestrator
	taggedRefs []flow.ItemRef
	calledTags []flow.TagId
}

func (b *tagFilterBackend) ListAutoSelectable(ctx context.Context, tags []flow.TagId, _ func(flow.RoleName) bool) ([]flow.ItemRef, error) {
	b.calledTags = tags
	return b.taggedRefs, nil
}

// --tag reaches the orchestrator: filtering belongs there, because tags live
// in the orchestrator and ItemRef does not carry them.
func TestCmdResolve_TagFilterSelectsFromTaggedSet(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &tagFilterBackend{
		Orchestrator: inner,
		taggedRefs: []flow.ItemRef{
			{OrchestratorName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)},
		},
	}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"--tag", "priority:high"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if len(be.calledTags) != 1 || be.calledTags[0] != "priority:high" {
		t.Errorf("ListAutoSelectable called with %v, want [priority:high]", be.calledTags)
	}
}

// TestCmdResolve_TagAndIdMutuallyExclusive verifies the usage error.
func TestCmdResolve_TagAndIdMutuallyExclusive(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"--tag", "x", "42"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (usage error); err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "mutually exclusive") {
		t.Errorf("expected mutual-exclusion message; got %q", errBuf.String())
	}
}

// TestCmdResolve_TagEmptySetExitsClean verifies that no items with the given
// tags exits 0 — selecting nothing is not an error.
func TestCmdResolve_TagEmptySetExitsClean(t *testing.T) {
	inner := fake.New()
	be := &tagFilterBackend{
		Orchestrator: inner,
		taggedRefs:   nil, // empty
	}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"--tag", "nonexistent"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no eligible items carrying tags") {
		t.Errorf("expected tag-specific empty message; got %q", errBuf.String())
	}
}

// ---------------------------------------------------------------------------
// Invariant: resolve's auto-select NEVER calls Discover.
// ---------------------------------------------------------------------------

// discoverPanicBackend wraps fake and panics if Discover is called. This
// proves the invariant stated in the Discoverer interface doc: resolve's
// auto-select path must never call Discover.
type discoverPanicBackend struct {
	*fake.Orchestrator
}

func (b *discoverPanicBackend) List(ctx context.Context, scope flow.ItemScope, binaryName flow.BinaryName, acceptsType func(flow.ItemType) bool, assumesRole func(flow.RoleName) bool) ([]flow.ItemInfo, error) {
	panic("INVARIANT VIOLATION: resolve's auto-select called Discover")
}

// TestCmdResolve_AutoSelectNeverCallsDiscover is the invariant test from
// item 2 of issue #6.
func TestCmdResolve_AutoSelectNeverCallsDiscover(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &discoverPanicBackend{Orchestrator: inner}
	app, _, errBuf := resolveTestApp(t, be)

	// If cmdResolve ever calls Discover, the panic will fail this test.
	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
}

// ---------------------------------------------------------------------------
// Explicit-ref refusal: `resolve <id>` with a typed claim refusal must render
// via formatClaimRefusal and exit 1, not fall into the generic error path.
// ---------------------------------------------------------------------------

// refusingResolveBackend refuses Claim with a typed ErrClaimRefused and
// implements RefResolver so the explicit-id path can resolve the ref.
type refusingResolveBackend struct {
	*fake.Orchestrator
	refusal flow.ErrClaimRefused
}

func (b *refusingResolveBackend) ResolveRef(ctx context.Context, id string) (flow.ItemRef, error) {
	return flow.ItemRef{OrchestratorName: "fake", Display: id, Ref: json.RawMessage(`"` + id + `"`)}, nil
}

func (b *refusingResolveBackend) Claim(ctx context.Context, ref flow.ItemRef, overrides []flow.ClaimOverride) (flow.Claim, error) {
	return flow.Claim{}, b.refusal
}

func TestCmdResolve_ExplicitIdRefusalRendering(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &refusingResolveBackend{
		Orchestrator: inner,
		refusal: flow.ErrClaimRefused{
			Code:     "not-admitted",
			Reason:   "arena not admitted",
			Check:    "git-identity",
			Override: "force-unadmitted",
		},
	}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	got := errBuf.String()
	if !strings.Contains(got, "refused") {
		t.Errorf("expected 'refused' in output; got %q", got)
	}
	if !strings.Contains(got, `check "git-identity"`) {
		t.Errorf("expected check name; got %q", got)
	}
	if !strings.Contains(got, "--force-unadmitted") {
		t.Errorf("expected override hint; got %q", got)
	}
}

// ---------------------------------------------------------------------------
// --force-unadmitted on resolve
// ---------------------------------------------------------------------------

// overrideRecordingResolveBackend records which overrides Claim receives,
// implements RefResolver for the explicit-id path, and delegates to fake.
type overrideRecordingResolveBackend struct {
	*fake.Orchestrator
	lastOverrides []flow.ClaimOverride
}

func (b *overrideRecordingResolveBackend) ResolveRef(ctx context.Context, id string) (flow.ItemRef, error) {
	return flow.ItemRef{OrchestratorName: "fake", Display: id, Ref: json.RawMessage(`"` + id + `"`)}, nil
}

func (b *overrideRecordingResolveBackend) Claim(ctx context.Context, ref flow.ItemRef, overrides []flow.ClaimOverride) (flow.Claim, error) {
	b.lastOverrides = overrides
	return b.Orchestrator.Claim(ctx, ref, overrides)
}

func TestCmdResolve_ForceUnadmittedPassesOverride(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &overrideRecordingResolveBackend{Orchestrator: inner}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1", "--force-unadmitted"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if len(be.lastOverrides) != 1 || be.lastOverrides[0] != flow.OverrideUnadmitted {
		t.Errorf("Claim received overrides=%v, want [OverrideUnadmitted]", be.lastOverrides)
	}
}

// ---------------------------------------------------------------------------
// finalTotalSuffix
// ---------------------------------------------------------------------------

// totalSuffixInspectBackend wraps the fake backend and implements
// StateInspector with a caller-supplied state, so finalTotalSuffix tests can
// control exactly which artifacts and figures are returned.
type totalSuffixInspectBackend struct {
	*fake.Orchestrator
	state *flow.Item
	err   error
}

func (b *totalSuffixInspectBackend) Load(_ context.Context, _ flow.ItemRef) (*flow.Item, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.state, nil
}

func TestFinalTotalSuffix_NoStateInspector(t *testing.T) {
	// Plain fake.Orchestrator does not implement StateInspector.
	app := &App{Orchestrator: fake.New()}
	claim := flow.Claim{}
	got := finalTotalSuffix(context.Background(), app, claim)
	if got != "" {
		t.Errorf("expected empty string when backend is not StateInspector; got %q", got)
	}
}

func TestFinalTotalSuffix_LoadStateError(t *testing.T) {
	be := &totalSuffixInspectBackend{
		Orchestrator: fake.New(),
		err:          errors.New("state unavailable"),
	}
	app := &App{Orchestrator: be}
	got := finalTotalSuffix(context.Background(), app, flow.Claim{})
	if got != "" {
		t.Errorf("expected empty string on Load error; got %q", got)
	}
}

func TestFinalTotalSuffix_NoFigures(t *testing.T) {
	be := &totalSuffixInspectBackend{
		Orchestrator: fake.New(),
		state: &flow.Item{
			Ledger: flow.Ledger{Steps: map[flow.StepId]flow.LedgerRow{
				"plan": {Step: "plan"}, // never dispatched: nothing spent
			}},
		},
	}
	app := &App{Orchestrator: be}
	got := finalTotalSuffix(context.Background(), app, flow.Claim{})
	if got != "" {
		t.Errorf("expected empty string when all figures are zero; got %q", got)
	}
}

func TestFinalTotalSuffix_BothFigures(t *testing.T) {
	be := &totalSuffixInspectBackend{
		Orchestrator: fake.New(),
		state: &flow.Item{
			Ledger: flow.Ledger{
				Steps: map[flow.StepId]flow.LedgerRow{
					"plan":           {Step: "plan", Dispatches: 1, Active: 5 * time.Minute, CostUSD: 1.20},
					"implementation": {Step: "implementation", Dispatches: 1, Active: 9*time.Minute + 2*time.Second, CostUSD: 1.51},
				},
				TotalActive:  14*time.Minute + 2*time.Second,
				TotalCostUSD: 2.71,
			},
		},
	}
	app := &App{Orchestrator: be}
	got := finalTotalSuffix(context.Background(), app, flow.Claim{})
	if !strings.Contains(got, "14m02s") {
		t.Errorf("expected total duration 14m02s; got %q", got)
	}
	if !strings.Contains(got, "$2.71") {
		t.Errorf("expected total cost $2.71; got %q", got)
	}
	if strings.Contains(got, "≥") {
		t.Errorf("exact total must not show lower-bound prefix; got %q", got)
	}
}

func TestFinalTotalSuffix_LowerBound(t *testing.T) {
	be := &totalSuffixInspectBackend{
		Orchestrator: fake.New(),
		state: &flow.Item{
			Ledger: flow.Ledger{
				Steps: map[flow.StepId]flow.LedgerRow{
					"plan": {Step: "plan", Dispatches: 1, Active: 5 * time.Minute, CostUSD: 1.20},
					// Dispatched but no active time recorded → the total is a
					// lower bound, not the whole figure.
					"legacy": {Step: "legacy", Dispatches: 1, Active: 0, CostUSD: 0.50},
				},
				TotalActive:  5 * time.Minute,
				TotalCostUSD: 1.70,
			},
		},
	}
	app := &App{Orchestrator: be}
	got := finalTotalSuffix(context.Background(), app, flow.Claim{})
	if !strings.Contains(got, "≥") {
		t.Errorf("expected lower-bound prefix ≥; got %q", got)
	}
	if !strings.Contains(got, "$1.70") {
		t.Errorf("expected total cost $1.70; got %q", got)
	}
}

func TestFinalTotalSuffix_UndispatchedZeroDurationNotLowerBound(t *testing.T) {
	// A step that was never dispatched is not a lower bound — it has not run,
	// so there is no unrecorded time to be missing.
	be := &totalSuffixInspectBackend{
		Orchestrator: fake.New(),
		state: &flow.Item{
			Ledger: flow.Ledger{
				Steps: map[flow.StepId]flow.LedgerRow{
					"plan":    {Step: "plan", Dispatches: 1, Active: 5 * time.Minute, CostUSD: 1.20},
					"pending": {Step: "pending"},
				},
				TotalActive:  5 * time.Minute,
				TotalCostUSD: 1.20,
			},
		},
	}
	app := &App{Orchestrator: be}
	got := finalTotalSuffix(context.Background(), app, flow.Claim{})
	if strings.Contains(got, "≥") {
		t.Errorf("an undispatched step must not trigger lower-bound; got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Budget park narration includes axes (#3)
// ---------------------------------------------------------------------------

func TestCmdResolve_BudgetParkNarratesAxes(t *testing.T) {
	// Use testApp (which pre-claims) to burn the only invocation, then run
	// cmdResolve which resumes the claim and immediately parks on budget.
	handler := func(ctx flow.StepCtx) (flow.StepResult, error) {
		return flow.StepResult{}, errors.New("boom")
	}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", handler, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, &stubAgent{name: "stub"})
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {
		MaxInvocations:          1,
		MaxPromptsPerInvocation: 2,
		MaxCostUSD:              10,
		Timeout:                 30 * time.Minute,
	}}
	_ = claim

	// Burn the single invocation via RunOne directly.
	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("first RunOne = %+v, want failed", res)
	}

	// Now cmdResolve: the resume path finds the claim, and the next RunOne
	// parks on budget (invocations exhausted). The park narration must include
	// the axes line.
	errBuf := &bytes.Buffer{}
	app.Out = &bytes.Buffer{}
	app.Err = errBuf
	app.Orchestrator = be // ensure it uses the same backend

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	got := errBuf.String()
	if !strings.Contains(got, "axes:") {
		t.Errorf("expected axes line in park narration; got %q", got)
	}
	if !strings.Contains(got, "inv") {
		t.Errorf("expected invocations axis in narration; got %q", got)
	}
}

func TestCmdResolve_NonBudgetParkOmitsAxes(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		// A non-budget park, under a kind the run stops on: a clearable one
		// would be re-dispatched until the invocation cap parked it
		// treasurer-refused, and the axes line this test forbids belongs to
		// that park.
		return flow.StepResult{}, ctx.Park(flow.ParkRequest{Kind: flow.ParkBlocked, Reason: "a person must act"})
	})

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "axes:") {
		t.Errorf("non-budget park must not emit axes line; got %q", errBuf.String())
	}
}

// ---------------------------------------------------------------------------
// Pre-claim fitness gate
// ---------------------------------------------------------------------------

// fitGateBackend scripts the `fit` GATE, which is where fitness is decided
// now: there is no CheckFit method, so a test says what the gate did by
// answering as the worktree the gate would have run in.
//
// Each entry in `rounds` is one fit measurement. A nil entry means fit.
type fitGateBackend struct {
	*fake.Orchestrator
	mu     sync.Mutex
	calls  int
	rounds []fitRound
}

// fitRound is one scripted answer. runErr means the runner could not attempt
// the gate; outcome other than measured means it measured nothing; judgeErr
// means no verdict exists; detail is the refusal a measured run is judged to.
type fitRound struct {
	runErr   error
	outcome  flow.Outcome
	judgeErr error
	unfit    string // non-empty: measured, and the judge refuses with this detail
}

func (b *fitGateBackend) next() fitRound {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if len(b.rounds) == 0 {
		return fitRound{}
	}
	if b.calls <= len(b.rounds) {
		return b.rounds[b.calls-1]
	}
	return b.rounds[len(b.rounds)-1]
}

// setRounds rescripts the gate mid-run, so a test can make the machine go
// unfit only after the step it is about to fail.
func (b *fitGateBackend) setRounds(rounds []fitRound) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = 0
	b.rounds = rounds
}

// unfitFor scripts n unfit measurements followed by fit ones.
func unfitFor(n int) []fitRound {
	rounds := make([]fitRound, 0, n+1)
	for range n {
		rounds = append(rounds, fitRound{unfit: "12 MB free, floor 2 GB"})
	}
	return append(rounds, fitRound{})
}

func (b *fitGateBackend) fitCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *fitGateBackend) Worktree(ctx context.Context, ref flow.ItemRef) (flow.Worktree, error) {
	inner, err := b.Orchestrator.Worktree(ctx, ref)
	if err != nil {
		return nil, err
	}
	return &fitGateWorktree{Worktree: inner, be: b}, nil
}

type fitGateWorktree struct {
	flow.Worktree
	be   *fitGateBackend
	last fitRound
}

func (w *fitGateWorktree) RunGate(ctx context.Context, name flow.GateName) (flow.GateRun, error) {
	if name != flow.GateFit {
		return w.Worktree.RunGate(ctx, name)
	}
	w.last = w.be.next()
	if w.last.runErr != nil {
		return flow.GateRun{}, w.last.runErr
	}
	outcome := w.last.outcome
	if outcome == "" {
		outcome = flow.OutcomeMeasured
	}
	return flow.GateRun{Gate: name, Outcome: outcome, Stdout: []byte(`{}`)}, nil
}

func (w *fitGateWorktree) Judge(ctx context.Context, run flow.GateRun) (flow.GateVerdict, error) {
	if run.Gate != flow.GateFit {
		return w.Worktree.Judge(ctx, run)
	}
	if w.last.judgeErr != nil {
		return flow.GateVerdict{}, w.last.judgeErr
	}
	if w.last.unfit != "" {
		return flow.GateVerdict{Run: run, Acceptable: false, Detail: w.last.unfit, Thresholds: []byte("{}")}, nil
	}
	return flow.GateVerdict{Run: run, Acceptable: true, Thresholds: []byte("{}")}, nil
}

// TestResolve_UnfitMachineExitsBeforeClaiming verifies that when the backend
// reports the machine as permanently unfit, resolve exhausts the fitness wait
// and exits 1 without claiming the item.
func TestResolve_UnfitMachineExitsBeforeClaiming(t *testing.T) {
	old := fitnessWaitInterval
	fitnessWaitInterval = time.Millisecond
	defer func() { fitnessWaitInterval = old }()

	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &fitGateBackend{Orchestrator: inner, rounds: []fitRound{{unfit: "12 MB free, floor 2 GB"}}}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (unfit machine); err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "machine unfit") {
		t.Errorf("expected 'machine unfit' in stderr; got %q", errBuf.String())
	}
	// The item must NOT be claimed.
	claim, err := be.LookupActiveClaim(context.Background())
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if claim != nil {
		t.Errorf("item should not be claimed after unfit verdict; got %+v", claim)
	}
}

// TestResolve_FitMachineProceeds verifies that a fit verdict lets resolve
// proceed normally (item gets claimed and driven through the step).
func TestResolve_FitMachineProceeds(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &fitGateBackend{Orchestrator: inner}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	// Item must be claimed.
	claim, err := be.LookupActiveClaim(context.Background())
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if claim == nil {
		t.Error("item should be claimed after fit verdict")
	}
}

// NO OUTCOME IS NOT A PASSING OUTCOME. Every way the fit gate can fail to
// produce a judged measurement means unfit — a gate that could not be run, one
// that measured nothing, and one whose measurement could not be judged. Read
// the other way round, an erroring fit check counted as fit, which is precisely
// the state `fit` exists to stop anyone proceeding from.
//
// A gate that fails this way on EVERY call must terminate at the wait bound
// rather than loop: the two wait sites share one counter, and when they did
// not, each reset the other's budget and the run spun to the runaway guard.
func TestResolve_FitCheckFailsClosed(t *testing.T) {
	old := fitnessWaitInterval
	fitnessWaitInterval = time.Millisecond
	defer func() { fitnessWaitInterval = old }()

	for name, round := range map[string]fitRound{
		"the gate could not be run":       {runErr: errors.New("gate broken")},
		"it timed out":                    {outcome: flow.OutcomeTimedOut},
		"it could not start":              {outcome: flow.OutcomeCouldNotStart},
		"it died":                         {outcome: flow.OutcomeDied},
		"it broke its contract":           {outcome: flow.OutcomeBrokeContract},
		"its measurement was unjudgeable": {judgeErr: errors.New("no thresholds")},
	} {
		t.Run(name, func(t *testing.T) {
			inner := fake.New()
			inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
			be := &fitGateBackend{Orchestrator: inner, rounds: []fitRound{round}}
			app, _, errBuf := resolveTestApp(t, be)

			code := app.cmdResolve(context.Background(), []string{"1"})
			if code != 1 {
				t.Fatalf("exit code = %d, want 1 — no outcome is not a passing outcome; err=%q", code, errBuf.String())
			}
			// The claim is what must not be taken: work started on an unfit
			// arena is the failure `fit` runs before the lease to prevent.
			claim, err := be.LookupActiveClaim(context.Background())
			if err != nil {
				t.Fatalf("LookupActiveClaim: %v", err)
			}
			if claim != nil {
				t.Errorf("the item was claimed on an unfit arena: %+v", claim)
			}
			// It stopped at the wait bound rather than spinning to the runaway
			// guard, which is what a shared counter buys.
			if strings.Contains(errBuf.String(), "runaway guard") {
				t.Errorf("the fit wait ran to the runaway guard; err=%q", errBuf.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Mid-step fitness wait: standalone wait-and-retry on StatusBlocked
// ---------------------------------------------------------------------------

// TestResolve_FitnessWaitRetriesOnRecovery verifies that when a step returns
// ErrUnfit and the machine subsequently recovers, cmdResolve waits and retries
// rather than exiting immediately.
func TestResolve_FitnessWaitRetriesOnRecovery(t *testing.T) {
	old := fitnessWaitInterval
	fitnessWaitInterval = time.Millisecond
	defer func() { fitnessWaitInterval = old }()

	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})

	stepCalls := 0
	// The backend starts fit (unfitRounds: 0) so the pre-claim check passes.
	// After the step returns ErrUnfit, the mid-step CheckFit also returns fit,
	// so cmdResolve retries immediately.
	be := &fitGateBackend{Orchestrator: inner, rounds: unfitFor(0)}
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		stepCalls++
		if stepCalls == 1 {
			return flow.StepResult{}, fmt.Errorf("12 MB free, floor 2 GB: %w", flow.ErrUnfit)
		}
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
	})

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (should recover after fitness retry); err=%q", code, errBuf.String())
	}
	if stepCalls < 2 {
		t.Errorf("step handler called %d time(s), want >= 2 (first unfit, then recovered)", stepCalls)
	}
	if !strings.Contains(errBuf.String(), "machine fit again") {
		t.Errorf("expected 'machine fit again' in stderr; got %q", errBuf.String())
	}
}

// TestResolve_FitnessWaitHoldsWhileUnfit verifies that when the machine stays
// unfit for several rounds and then recovers, cmdResolve waits and re-measures
// each round before retrying the step.
func TestResolve_FitnessWaitHoldsWhileUnfit(t *testing.T) {
	old := fitnessWaitInterval
	fitnessWaitInterval = time.Millisecond
	defer func() { fitnessWaitInterval = old }()

	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})

	stepCalls := 0
	// Pre-claim CheckFit (call 1) is fit (unfitRounds=0 → always fit at that
	// stage). Then the step returns ErrUnfit. The mid-step re-check (calls
	// 2..4) returns unfit for 3 rounds, then fit on call 5.
	be := &fitGateBackend{Orchestrator: inner, rounds: unfitFor(0)}
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		stepCalls++
		if stepCalls == 1 {
			// After the step fails, the next three fit measurements are unfit.
			be.setRounds(unfitFor(3))
			return flow.StepResult{}, fmt.Errorf("12 MB free, floor 2 GB: %w", flow.ErrUnfit)
		}
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
	})

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (should recover after wait); err=%q", code, errBuf.String())
	}
	if stepCalls < 2 {
		t.Errorf("step handler called %d time(s), want >= 2", stepCalls)
	}
	if !strings.Contains(errBuf.String(), "waiting…") {
		t.Errorf("expected 'waiting…' in stderr during wait rounds; got %q", errBuf.String())
	}
}

// TestResolve_FitnessWaitExhaustedExits verifies that exhausting the fitness
// wait bound exits 1 rather than looping forever.
func TestResolve_FitnessWaitExhaustedExits(t *testing.T) {
	old := fitnessWaitInterval
	fitnessWaitInterval = time.Millisecond
	defer func() { fitnessWaitInterval = old }()

	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})

	// Pre-claim: fit. Mid-step: permanently unfit.
	be := &fitGateBackend{Orchestrator: inner, rounds: unfitFor(0)}
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		be.setRounds(unfitFor(maxFitnessWaits + 10))
		return flow.StepResult{}, fmt.Errorf("disk full: %w", flow.ErrUnfit)
	})

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (fitness wait exhausted); err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "machine unfit") {
		t.Errorf("expected the final unfit report in stderr when the wait was exhausted; got %q", errBuf.String())
	}
}

// TestResolve_FitnessWaitInterruptedExits verifies that cancelling the context
// during a fitness wait exits cleanly.
func TestResolve_FitnessWaitInterruptedExits(t *testing.T) {
	old := fitnessWaitInterval
	fitnessWaitInterval = time.Hour // long enough to guarantee the cancel fires first
	defer func() { fitnessWaitInterval = old }()

	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})

	// Pre-claim: fit. Mid-step: permanently unfit.
	be := &fitGateBackend{Orchestrator: inner, rounds: unfitFor(0)}
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		be.setRounds(unfitFor(999))
		return flow.StepResult{}, fmt.Errorf("disk full: %w", flow.ErrUnfit)
	})

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay — long enough for one RunOne + CheckFit cycle.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	code := app.cmdResolve(ctx, []string{"1"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (interrupted); err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "interrupted") {
		t.Errorf("expected 'interrupted' in stderr; got %q", errBuf.String())
	}
}

// TestResolve_NonFitnessBlockExitsImmediately verifies that a StatusBlocked
// result whose reason does NOT contain ErrUnfit exits immediately — it must
// not enter the fitness wait loop. If the strings.Contains guard were removed,
// every block (e.g. ErrBlocked from a preflight) would be retried.
func TestResolve_NonFitnessBlockExitsImmediately(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &fitGateBackend{Orchestrator: inner}
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
	})

	// A preflight that always returns ErrBlocked — produces StatusBlocked
	// with a reason that has nothing to do with fitness.
	app.Preflight = func(_ context.Context, _ *flow.Item) error {
		return fmt.Errorf("answer needed on %q: %w", "plan", flow.ErrBlocked)
	}

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (non-fitness block must exit); err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "is blocked") {
		t.Errorf("expected 'is blocked' in stderr; got %q", errBuf.String())
	}
	// Must NOT contain any fitness wait output — the loop must not fire.
	if strings.Contains(errBuf.String(), "waiting…") || strings.Contains(errBuf.String(), "machine unfit") {
		t.Errorf("non-fitness block must not trigger fitness wait; got %q", errBuf.String())
	}
}

// TestResolve_PreClaimTransientUnfitnessProceeds verifies that when the machine
// is temporarily unfit at pre-claim time but recovers, the item gets claimed.
func TestResolve_PreClaimTransientUnfitnessProceeds(t *testing.T) {
	old := fitnessWaitInterval
	fitnessWaitInterval = time.Millisecond
	defer func() { fitnessWaitInterval = old }()

	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	// Unfit for 3 rounds, then fit.
	be := &fitGateBackend{Orchestrator: inner, rounds: unfitFor(3)}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (should proceed after transient unfitness); err=%q", code, errBuf.String())
	}
	// Should have waited through the unfit rounds.
	if !strings.Contains(errBuf.String(), "waiting…") {
		t.Errorf("expected 'waiting…' in stderr during pre-claim wait; got %q", errBuf.String())
	}
	// Item must be claimed.
	claim, err := be.LookupActiveClaim(context.Background())
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if claim == nil {
		t.Error("item should be claimed after transient unfitness clears")
	}
}

// ---------------------------------------------------------------------------
// Blocked on items: the narration.
// ---------------------------------------------------------------------------

// blockedResolveApp is an app over an item blocked by one landed and one open
// blocker — the shape that proves the `blocked by:` line lists only what is
// still open.
func blockedResolveApp(t *testing.T) (*App, *fake.Orchestrator, *bytes.Buffer, *bytes.Buffer, *bool) {
	t.Helper()
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be.AddItem("2", flow.Item{Type: "task", Title: "landed"})
	be.AddItem("3", flow.Item{Type: "task", Title: "still open"})
	be.SetStatus("2", flow.StatusTerminal, "done")
	blockOn(t, be, be.Ref("1"), be.Ref("2"))
	blockOn(t, be, be.Ref("1"), be.Ref("3"))
	ran := false
	app, out, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		ran = true
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
	})
	return app, be, out, errBuf, &ran
}

func TestCmdResolve_BlockedOnItemsNarratesTheBlockersAndKeepsTheClaim(t *testing.T) {
	app, be, out, errBuf, ran := blockedResolveApp(t)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (a blocked item makes no progress until the blockers land); err=%q", code, errBuf.String())
	}
	got := errBuf.String()
	if !strings.Contains(got, "resolve: plan → blocked — waiting on unfinished dependencies") {
		t.Errorf("expected the step, the status and the kind-naming reason; got %q", got)
	}
	if !strings.Contains(got, "  blocked by: 3\n") {
		t.Errorf("expected a `blocked by:` line naming the open blocker; got %q", got)
	}
	if strings.Contains(got, "blocked by: 2") || strings.Contains(got, "2, 3") {
		t.Errorf("the landed blocker must not be listed; got %q", got)
	}
	if !strings.Contains(got, "1 is blocked") {
		t.Errorf("expected the closing blocked line; got %q", got)
	}
	if *ran {
		t.Error("the step ran on a blocked item")
	}
	if out.Len() != 0 {
		t.Errorf("human mode wrote to stdout: %q", out.String())
	}
	if held, _ := be.LookupActiveClaim(context.Background()); held == nil {
		t.Error("the claim was released — the stop keeps it")
	}
}

func TestCmdResolve_BlockedOnItemsJSONCarriesTheKindAndBlockers(t *testing.T) {
	t.Setenv(outputEnv, "")
	app, _, out, errBuf, _ := blockedResolveApp(t)

	code := app.cmdResolve(context.Background(), []string{"--json", "1"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	got := decodeResultStream(t, out.String())
	if len(got) != 1 {
		t.Fatalf("stdout carried %d results, want 1; got %q", len(got), out.String())
	}
	res := got[0]
	if res.Status != "blocked" || res.Step != "plan" || res.BlockKind != flow.WaitsOnItems {
		t.Errorf("result = %+v, want blocked at plan, kind waits-on-items", res)
	}
	if len(res.BlockedBy) != 2 {
		t.Errorf("BlockedBy = %+v, want both declared blockers with their statuses", res.BlockedBy)
	}
	if !strings.Contains(errBuf.String(), "blocked by: 3") {
		t.Errorf("JSON mode must still narrate the blockers; got %q", errBuf.String())
	}
}

// A block from elsewhere — a preflight gate a person must clear — prints no
// `blocked by:` line: there are no blockers to send the operator to.
func TestCmdResolve_PreflightBlockPrintsNoBlockedByLine(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, _, errBuf := resolveTestApp(t, be)
	app.Preflight = func(context.Context, *flow.Item) error {
		return fmt.Errorf("answer needed on %q: %w", "plan", flow.ErrBlocked)
	}

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "is blocked") {
		t.Errorf("expected the blocked line; got %q", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "blocked by:") {
		t.Errorf("a preflight block names no blockers; got %q", errBuf.String())
	}
}

// THE QUOTA BLOCK PRINTS ONCE, AT THE START, where it answers the question it
// is there to answer: does this run have headroom? Reprinted after the outcome
// it buries the park or the finalized line — the one line an operator must act
// on — under pacing detail they have already read and cannot act on.
//
// The three exits are checked together because "once" is a property of the run
// and not of any one of them: a finalize, a failure and a park each used to add
// their own copy, and dropping two of the three would leave the rule true of
// some runs and not others. `quota` is the command that answers the question
// deliberately now (docs/cli.md).
func TestCmdResolve_QuotaBlockPrintsOnceAtTheStart(t *testing.T) {
	parked := func(ctx flow.StepCtx) (flow.StepResult, error) {
		// Returns nil without resolving, which parks the step.
		return flow.StepResult{}, nil
	}
	failed := func(ctx flow.StepCtx) (flow.StepResult, error) {
		return flow.StepResult{}, errors.New("boom")
	}

	for _, tc := range []struct {
		name string
		step func(flow.StepCtx) (flow.StepResult, error)
		code int
		want string
	}{
		{"finalize", nil, 0, "not finalized"},
		{"failed step", failed, 1, "stopped on a failed step"},
		// "→ parked", not "parked": the driving line says "until finalized or
		// parked" BEFORE the quota block, so the bare word would find that one
		// and the ordering check below would pass on any output at all.
		{"park", parked, 0, "→ parked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			be := fake.New()
			be.AddItem("1", flow.Item{Type: "task", Title: "1"})
			var app *App
			var errBuf *bytes.Buffer
			if tc.step == nil {
				app, _, errBuf = resolveTestApp(t, be)
			} else {
				app, _, errBuf = resolveTestAppStep(t, be, tc.step)
			}

			if code := app.cmdResolve(context.Background(), nil); code != tc.code {
				t.Fatalf("exit code = %d, want %d; err=%q", code, tc.code, errBuf.String())
			}
			output := errBuf.String()
			if !strings.Contains(output, tc.want) {
				t.Errorf("expected %q in the narration; got:\n%s", tc.want, output)
			}
			if count := strings.Count(output, "quota:"); count != 1 {
				t.Errorf("quota block printed %d times, want exactly 1 — at the start; got:\n%s", count, output)
			}
			// At the START: before the first step is announced, not after the
			// outcome. A single print in the wrong place is the same defect.
			if at := strings.Index(output, "quota:"); at > strings.Index(output, tc.want) {
				t.Errorf("the quota block prints after the outcome, not at the start; got:\n%s", output)
			}
		})
	}
}

// No environment variable decides whether the quota block prints. The report
// goes to stderr, where narration goes, and the retired name — spelled as a
// literal here, because the code must no longer know it — selects nothing.
// Pacing runs either way, as it always did.
func TestCmdResolve_QuotaPrintedWhateverTheEnvironmentHolds(t *testing.T) {
	for _, value := range []string{"1", ""} {
		t.Run("env="+value, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("FLOW_DISPATCHED_BY_RUNNER", value)
			be := fake.New()
			be.AddItem("1", flow.Item{Type: "task", Title: "1"})
			app, _, errBuf := resolveTestApp(t, be)
			// Injected and failing: this asserts pacing is attempted, and the
			// warning is the evidence it was. Reaching the real reader would
			// make the assertion depend on whether this machine happens to
			// hold credentials, and would sleep when it does.
			app.Quota = func() ([]windowUsage, error) {
				return nil, fmt.Errorf("no credentials (injected)")
			}

			code := app.cmdResolve(context.Background(), nil)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
			}
			output := errBuf.String()
			// The one site the block prints at, as
			// TestCmdResolve_QuotaBlockPrintsOnceAtTheStart counts it. Zero
			// occurrences would be a re-gating that put the print behind the
			// environment, which is what this test exists to catch.
			if n := strings.Count(output, "quota:"); n != 1 {
				t.Errorf("the quota block must print at the start whatever the environment holds; got %d in:\n%s", n, output)
			}
			if !strings.Contains(output, "quota unreadable") {
				t.Errorf("pacing must still be attempted (and show the unreadable warning); got:\n%s", output)
			}
		})
	}
}

func TestCmdResolve_QuotaPrintedOnBlocked(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &fitGateBackend{Orchestrator: inner}
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
	})
	// A preflight that always returns ErrBlocked — produces StatusBlocked.
	app.Preflight = func(_ context.Context, _ *flow.Item) error {
		return fmt.Errorf("gate blocked: %w", flow.ErrBlocked)
	}

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	output := errBuf.String()
	if !strings.Contains(output, "is blocked") {
		t.Errorf("expected 'is blocked' in stderr; got %q", output)
	}
	// Once, at the start — the blocked exit adds no second copy, as no exit
	// does. See TestCmdResolve_QuotaBlockPrintsOnceAtTheStart.
	if count := strings.Count(output, "quota:"); count != 1 {
		t.Errorf("quota block printed %d times, want exactly 1 — at the start; got:\n%s", count, output)
	}
}

func TestCmdResolve_QuotaUnreadableWarnedOnce(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	// The warning is what this test is about, so the reader is injected and
	// fails. It used to rely on an empty CLAUDE_CONFIG_DIR making the real
	// reader fail — but that reader also searches the macOS Keychain, which no
	// environment variable controls, so on a machine with credentials it
	// SUCCEEDED, paced, and slept for as long as the account said. That is what
	// hung this package for ten minutes with the tree sound.
	//
	// The loop runs at least twice (step + finalize), so the dedup guard is
	// still exercised.
	app, _, errBuf := resolveTestApp(t, be)
	app.Quota = func() ([]windowUsage, error) {
		return nil, fmt.Errorf("no credentials (injected)")
	}

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	output := errBuf.String()
	count := strings.Count(output, "quota unreadable")
	if count != 1 {
		t.Errorf("expected exactly 1 'quota unreadable' warning (dedup); got %d in:\n%s", count, output)
	}
}

// A mechanical step skips the quota wait ENTIRELY: the curve measures spend the
// step cannot make, and a resolution has sat for hours before a free step under
// a curve it was nowhere near breaching. The declaration is what makes the
// skip safe — a prompt from such a step is refused before anything is sent —
// and it is read before dispatch, where the wait would otherwise be. An agent
// step still waits.
func TestCmdResolve_AMechanicalStepIsNotPaced(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prompts flow.PromptPolicy
		paced   bool
	}{
		{"agent step waits", flow.PromptsAgent, true},
		{"mechanical step does not", flow.PromptsNone, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := fake.New()
			be.AddItem("1", flow.Item{Type: "task", Title: "1"})
			quotaReads := 0
			readsBeforeStep := -1
			app, _, errBuf := resolveTestAppPrompts(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
				readsBeforeStep = quotaReads
				return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
			}, tc.prompts)
			// A reading that forces a wait: the window is all but spent with
			// almost none of it elapsed, so the delay is the (short) remainder
			// of the window. The delay is real, which is what makes the line
			// print; it is short, which is what keeps the test quick.
			//
			// Just under 1.0, not at it: an EXHAUSTED window is a different
			// condition — the pre-dispatch check withholds the dispatch
			// entirely rather than pacing it — and this test is about pacing.
			// The delay is identical either way (the target is already
			// exceeded at full elapse, so it is the whole remainder).
			app.Quota = func() ([]windowUsage, error) {
				quotaReads++
				return []windowUsage{{
					Label:    "5h",
					Length:   time.Second,
					Used:     0.99,
					ResetsAt: time.Now().Add(30 * time.Millisecond),
				}}, nil
			}

			code := app.cmdResolve(context.Background(), []string{"1"})
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
			}
			output := errBuf.String()
			waited := strings.Index(output, "pacing — waiting")
			ran := strings.Index(output, `running "write plan"`)
			if ran < 0 {
				t.Fatalf("the step was never announced; got:\n%s", output)
			}
			if tc.paced {
				// Twice: the pacing wait here, and RunOne's own pre-dispatch
				// check for an exhausted account. Both consult the same
				// reading and both skip a mechanical step.
				if readsBeforeStep != 2 {
					t.Errorf("quota read %d times before the agent step ran, want 2 — an agent step is paced and pre-checked", readsBeforeStep)
				}
				if waited < 0 || waited > ran {
					t.Errorf("an agent step must wait for quota headroom before it runs; got:\n%s", output)
				}
				return
			}
			if readsBeforeStep != 0 {
				t.Errorf("quota read %d times before the mechanical step ran, want 0 — the wait is skipped entirely, not shortened", readsBeforeStep)
			}
			if waited >= 0 && waited < ran {
				t.Errorf("a mechanical step waited for quota headroom it cannot spend; got:\n%s", output)
			}
		})
	}
}

// The decision is made for EACH pending step, on the iteration that dispatches
// it, never once for the resolution: a route that runs an agent step and then a
// mechanical one waits before the first and not before the second. That is the
// shape the stall was measured on — a resolution whose free steps sat under a
// curve its prompting steps were nowhere near breaching — and a peek hoisted
// out of the loop, or a verdict carried over from the previous iteration, would
// pace the mechanical step on the agent step's declaration.
func TestCmdResolve_PacingIsDecidedForEachPendingStep(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	quotaReads := 0
	readsBeforePlan, readsBeforeBranch := -1, -1
	app, _, errBuf := resolveTestAppFlow(t, be, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			readsBeforePlan = quotaReads
			return ctx.Next("commit", "planned").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Entry: true, Role: "contributor", Next: []flow.StepId{"commit"}})
		f.AddStep("open branch", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
			readsBeforeBranch = quotaReads
			return ctx.Finalize(flow.DispositionResolved, "done").CommitHash("abc"), nil
		}, flow.StepConfig{Prompts: flow.PromptsNone, Role: "contributor", MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})
	// Every reading forces a (short) wait, so an iteration that consults the
	// quota is visible twice over: as a read, and as a pacing line. Just under
	// 1.0 for the reason the test above gives — an exhausted window withholds
	// the dispatch instead of pacing it.
	app.Quota = func() ([]windowUsage, error) {
		quotaReads++
		return []windowUsage{{
			Label:    "5h",
			Length:   time.Second,
			Used:     0.99,
			ResetsAt: time.Now().Add(30 * time.Millisecond),
		}}, nil
	}

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	// Two per agent step: the pacing wait, and RunOne's pre-dispatch check for
	// an exhausted account.
	if readsBeforePlan != 2 {
		t.Errorf("quota read %d times before the agent step, want 2 — an agent step is paced and pre-checked", readsBeforePlan)
	}
	if readsBeforeBranch != 2 {
		t.Errorf("quota read %d times before the mechanical step, want 2 — the same two: the loop reached the mechanical step without consulting the quota again", readsBeforeBranch)
	}
	output := errBuf.String()
	planAt := strings.Index(output, `running "write plan"`)
	branchAt := strings.Index(output, `running "open branch"`)
	if planAt < 0 || branchAt < 0 || branchAt < planAt {
		t.Fatalf("want both steps announced, the plan first; got:\n%s", output)
	}
	if !strings.Contains(output[:planAt], "pacing — waiting") {
		t.Errorf("the agent step ran without waiting for quota headroom; got:\n%s", output)
	}
	if strings.Contains(output[planAt:branchAt], "pacing — waiting") {
		t.Errorf("a pacing wait sits between the agent step and the mechanical one; got:\n%s", output)
	}
}

func TestCmdResolve_PaceZeroSkipsPacing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"--pace-five-hour", "0", "--pace-seven-day", "0"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	output := errBuf.String()
	// With both targets at 0, no pacing should be attempted — no unreadable warning.
	if strings.Contains(output, "quota unreadable") {
		t.Errorf("pace targets 0 should skip pacing entirely; got:\n%s", output)
	}
}

// A claim taken while the item was workable SURVIVES the item becoming blocked
// (docs/cli.md § Claiming): a dependency declared after the claim, or a blocker
// reopened. `resolve` with no argument resumes that held claim, refuses before
// dispatching, and leaves everything where it was — so the item runs from here
// the moment its last blocker lands, with nobody having reset anything.
func TestCmdResolve_HeldClaimSurvivesTheItemBecomingBlocked(t *testing.T) {
	ctx := context.Background()
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be.AddItem("3", flow.Item{Type: "task", Title: "still open"})
	ran := false
	app, out, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		ran = true
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
	})
	// Claimed UNBLOCKED. The dependency is declared only afterwards.
	if _, err := be.Claim(ctx, be.Ref("1"), nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	blockOn(t, be, be.Ref("1"), be.Ref("3"))

	// No argument: the active claim is the selection, and it is not consulted
	// for blockedness by auto-selection because auto-selection never runs.
	code := app.cmdResolve(ctx, nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 — the held claim is not an override; err=%q", code, errBuf.String())
	}
	got := errBuf.String()
	if !strings.Contains(got, "plan → blocked — waiting on unfinished dependencies") {
		t.Errorf("expected the pre-dispatch stop at the pending step; got %q", got)
	}
	if !strings.Contains(got, "blocked by: 3") {
		t.Errorf("expected the open blocker named; got %q", got)
	}
	if ran {
		t.Error("the step ran on a blocked item")
	}
	if out.Len() != 0 {
		t.Errorf("human mode wrote to stdout: %q", out.String())
	}
	held, err := be.LookupActiveClaim(ctx)
	if err != nil || held == nil {
		t.Fatalf("the claim was released — becoming blocked does not lift a claim (err=%v)", err)
	}
	// Nothing was spent and nothing moved: the pending step is still pending.
	state, err := be.Load(ctx, be.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec, row := state.Artifact("plan"), state.Ledger.Row("plan"); rec.Resolved || row.Dispatches != 0 {
		t.Errorf("plan = %+v, want it pending and undispatched", rec)
	}
	if state.Parked() {
		t.Error("the stop parked the item — a park is a condition a person clears, and this one clears itself")
	}
}

// A route naming a step this build does not register stops the run with the
// refusal, non-zero, rather than reading as "nothing left to do". The
// difference matters at exactly one item: the finalize path is what an empty
// answer takes, and finalizing here would record an item as complete on the
// strength of a graph that could not say where it stood — terminally, and
// against a run that never dispatched anything.
//
// The backend accepts Finalize, so a swallowed refusal would exit 0 with the
// item recorded complete: that is the regression this pins.
func TestCmdResolve_ARouteToAnUnregisteredStepStopsInsteadOfFinalizing(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{
		Type: "task", Title: "1",
		Journal: []flow.JournalEntry{{
			Step: "plan", Execution: 1, Route: flow.Route{Next: "no-such-step"}, By: "tester",
		}},
	})
	be := &finalizingBackend{Orchestrator: inner}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code == 0 {
		t.Fatalf("exit code = 0 on an item the flow cannot place; err=%q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no-such-step") {
		t.Errorf("stderr = %q, want it to name the successor that resolves to nothing", errBuf.String())
	}
	if be.finalizeCalls != 0 {
		t.Errorf("Finalize called %d time(s) — an item the flow cannot place is not a finished item",
			be.finalizeCalls)
	}
}

// ---------------------------------------------------------------------------
// Re-dispatching a park the vocabulary says a re-dispatch clears
//
// docs/cli.md § Resolving. The classification is the PARK KIND's
// (flow.ParkKind.RedispatchMayClear), published on every park and read here.
// `resolve` is the driver the classification was written for: a driver that
// stops on all nine kinds identically stops on four conditions the same
// codebase says cure themselves.
// ---------------------------------------------------------------------------

// The shape the fitness wait one arm above already has: hold, try again, and
// let the run continue when it clears. A step that did not complete is the
// plainest case — "a re-dispatch is exactly what does the job it left undone".
func TestCmdResolve_AClearableParkIsReDispatchedAndTheRunContinues(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	// Terminal on the tracker, so the finalize pass actually records the run
	// complete — the tick asserted below is gated on that.
	be.SetStatus("1", flow.StatusTerminal, "completed")
	dispatches := 0
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		dispatches++
		if dispatches == 1 {
			// Returns without deciding anything: step-did-not-complete.
			return flow.StepResult{}, nil
		}
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
	})

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if dispatches != 2 {
		t.Errorf("the step was dispatched %d time(s), want 2 — the park was re-dispatched and cleared", dispatches)
	}
	out := errBuf.String()
	if !strings.Contains(out, "re-dispatching (1/") {
		t.Errorf("the retry was not narrated; got:\n%s", out)
	}
	if !strings.Contains(out, "finalized ✓") {
		t.Errorf("the run did not carry on to finalization; got:\n%s", out)
	}
}

// The bound is the fitness wait's, for the fitness wait's reason: a condition
// that never clears must terminate the run rather than spin it to the runaway
// guard. Exhausting it leaves the item PARKED — a wait bound is not a verdict —
// and the message says how many attempts it stands on.
func TestCmdResolve_AClearableParkThatNeverClearsStopsAtTheBound(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	dispatches := 0
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		dispatches++
		return flow.StepResult{}, fmt.Errorf("the runner is flapping: %w", flow.ErrTransient)
	})

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0 — a park is not a failure; err=%q", code, errBuf.String())
	}
	if want := maxRedispatches + 1; dispatches != want {
		t.Errorf("the step was dispatched %d time(s), want %d (the first, plus %d re-dispatches)",
			dispatches, want, maxRedispatches)
	}
	out := errBuf.String()
	if !strings.Contains(out, fmt.Sprintf("parked after %d re-dispatches", maxRedispatches)) {
		t.Errorf("the stop does not name the bound it stands on; got:\n%s", out)
	}
}

// The five kinds that are real reasons to stop, each dispatched exactly once.
// A driver that re-dispatched any of them would be looping: the answer is the
// same until a person, a grant or an answer arrives.
func TestCmdResolve_ANonClearingParkStopsOnTheFirstDispatch(t *testing.T) {
	for _, tc := range []struct {
		kind flow.ParkKind
		step func(*int) func(flow.StepCtx) (flow.StepResult, error)
	}{
		{flow.ParkBlocked, parkingStep(flow.ParkBlocked)},
		{flow.ParkRefused, parkingStep(flow.ParkRefused)},
		{flow.ParkTreasurerRefused, parkingStep(flow.ParkTreasurerRefused)},
		{flow.ParkWriteContract, parkingStep(flow.ParkWriteContract)},
		{flow.ParkQuestion, func(calls *int) func(flow.StepCtx) (flow.StepResult, error) {
			return func(ctx flow.StepCtx) (flow.StepResult, error) {
				*calls++
				return flow.StepResult{}, ctx.AskQuestions(flow.AskText("base", "which base branch?"))
			}
		}},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			be := fake.New()
			be.AddItem("1", flow.Item{Type: "task", Title: "1"})
			calls := 0
			app, _, errBuf := resolveTestAppStep(t, be, tc.step(&calls))

			if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
				t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
			}
			if calls != 1 {
				t.Errorf("the step was dispatched %d time(s), want 1: %q does not clear by re-dispatch", calls, tc.kind)
			}
			out := errBuf.String()
			if !strings.Contains(out, "run `status 1` to inspect") {
				t.Errorf("the stop does not send the operator to `status`; got:\n%s", out)
			}
			if strings.Contains(out, "re-dispatches") || strings.Contains(out, "re-dispatching") {
				t.Errorf("a kind no re-dispatch clears was retried or reported as retried; got:\n%s", out)
			}
		})
	}
}

// parkingStep is a handler that parks the given kind and counts its dispatches.
// One definition, so the crosswalk below and the five cases above cannot park
// two different ways.
func parkingStep(kind flow.ParkKind) func(*int) func(flow.StepCtx) (flow.StepResult, error) {
	return func(calls *int) func(flow.StepCtx) (flow.StepResult, error) {
		return func(ctx flow.StepCtx) (flow.StepResult, error) {
			*calls++
			req := flow.ParkRequest{Kind: kind, Reason: "parked as " + string(kind)}
			if kind == flow.ParkAccountExhausted {
				// The only kind that knows when it clears carries the instant
				// wherever it is written (wire.go), and the driver's answer is
				// about the park it was handed.
				at := time.Now().Add(2 * time.Hour)
				req.ClearsAt = &at
			}
			return flow.StepResult{}, ctx.Park(req)
		}
	}
}

// The crosswalk, over the SDK's own enumeration: the arm re-dispatches exactly
// the kinds the vocabulary classifies as cleared by a re-dispatch, less the one
// that publishes the instant it clears at — that one exits with the claim held,
// because the run does not sit in front of a window (docs/environment.md § The
// agent account).
//
// Read off flow.AllParkKinds and flow.ParkKind.RedispatchMayClear rather than a
// list written here, so a tenth kind cannot be added without this test speaking.
func TestCmdResolve_ReDispatchesExactlyTheClearableKinds(t *testing.T) {
	for _, kind := range flow.AllParkKinds() {
		if kind == flow.ParkQuestion {
			// ctx.Park refuses to raise a question park — it registers no
			// question — so this kind is exercised through ctx.AskQuestions in
			// the test above, where it stops on the first dispatch as its
			// classification requires.
			continue
		}
		t.Run(string(kind), func(t *testing.T) {
			be := fake.New()
			be.AddItem("1", flow.Item{Type: "task", Title: "1"})
			calls := 0
			app, _, errBuf := resolveTestAppStep(t, be, parkingStep(kind)(&calls))
			// A park raised through ctx.Park IS charged as a dispatch, so the
			// default three invocations would stop a retried kind at the
			// treasurer rather than at the bound this test is measuring.
			app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {MaxInvocations: 20}}

			if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
				t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
			}
			wantRetried := kind.RedispatchMayClear() && kind != flow.ParkAccountExhausted
			retried := calls > 1
			if retried != wantRetried {
				t.Errorf("%q: dispatched %d time(s) (retried=%v), want retried=%v — RedispatchMayClear()=%v",
					kind, calls, retried, wantRetried, kind.RedispatchMayClear())
			}
		})
	}
}

// A park carrying no instant is not a park that clears at no time — it is one
// whose reset could not be parsed (cli/orchestrator.go). It falls into the
// bounded retry like any other clearable kind rather than exiting on an instant
// nobody has.
func TestCmdResolve_AnExhaustedWindowWithNoInstantTakesTheBoundedRetry(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	calls := 0
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		calls++
		return flow.StepResult{}, ctx.Park(flow.ParkRequest{
			Kind:   flow.ParkAccountExhausted,
			Reason: "agent account allowance exhausted",
		})
	})
	// ctx.Park charges a dispatch, so the bound rather than the treasurer is
	// what this test measures.
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {MaxInvocations: 20}}

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if want := maxRedispatches + 1; calls != want {
		t.Errorf("the step was dispatched %d time(s), want %d", calls, want)
	}
	if !strings.Contains(errBuf.String(), "re-dispatching (1/") {
		t.Errorf("a park with no instant was not retried; got:\n%s", errBuf.String())
	}
}

// A skip is unchanged: a preflight refusal says this cycle will not run, not
// that the next one might, and there is nothing for a re-dispatch to clear.
func TestCmdResolve_ASkippedItemIsUnchanged(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	calls := 0
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		calls++
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
	})
	app.Preflight = func(context.Context, *flow.Item) error {
		return errors.New("the item is already finalized elsewhere")
	}

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if calls != 0 {
		t.Errorf("the step ran %d time(s) behind a preflight refusal, want 0", calls)
	}
	if !strings.Contains(errBuf.String(), "1 skipped — run `status 1` to inspect") {
		t.Errorf("the skip narration changed; got:\n%s", errBuf.String())
	}
}

// The wait is interruptible, like the pacing and fitness waits: a cancelled
// context ends the run at 1 rather than sleeping out the interval.
func TestCmdResolve_CancellingDuringTheReDispatchWaitExitsOne(t *testing.T) {
	old := redispatchInterval
	redispatchInterval = time.Hour // long enough to guarantee the cancel fires first
	defer func() { redispatchInterval = old }()

	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	ctx, cancel := context.WithCancel(context.Background())
	app, _, errBuf := resolveTestAppStep(t, be, func(flow.StepCtx) (flow.StepResult, error) {
		cancel()
		return flow.StepResult{}, fmt.Errorf("the runner is flapping: %w", flow.ErrTransient)
	})

	if code := app.cmdResolve(ctx, []string{"1"}); code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "interrupted while waiting to re-dispatch") {
		t.Errorf("the interruption was not named; got:\n%s", errBuf.String())
	}
}

// The instant is read WITH the classification, never instead of it. Only a kind
// a re-dispatch can clear is asking "when would that be worth anything", so a
// park that does not clear is reported as the stop it is even if something
// wrote an instant on it — telling the operator the run resumes on its own at
// that time would be telling them not to act on a park that needs them to.
func TestCmdResolve_AnInstantOnANonClearingParkIsNotAWait(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	calls := 0
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		calls++
		at := time.Now().Add(2 * time.Hour)
		return flow.StepResult{}, ctx.Park(flow.ParkRequest{
			Kind:     flow.ParkBlocked,
			Reason:   "a person must act",
			ClearsAt: &at,
		})
	})

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if calls != 1 {
		t.Errorf("the step was dispatched %d time(s), want 1", calls)
	}
	out := errBuf.String()
	if strings.Contains(out, "nothing clears before") {
		t.Errorf("a park a person must clear was reported as a wait; got:\n%s", out)
	}
	if !strings.Contains(out, "run `status 1` to inspect") {
		t.Errorf("the stop does not send the operator to `status`; got:\n%s", out)
	}
}

// The bound is ONE budget for the whole run, not one per park.
//
// A per-park counter — reset whenever a park cleared — is the loop the single
// counter exists to prevent: a route whose steps each park clearable, clear,
// and park again buys itself a fresh allowance at every step, and the run spins
// to the runaway guard instead of ending. It is the reason fitnessWaits is one
// counter too, and the two are easy to get wrong in the same way.
//
// The first step parks once and clears, spending one of the allowance. The
// second then gets what is LEFT, so the run stops after maxRedispatches
// re-dispatches in total rather than after maxRedispatches on each park.
func TestCmdResolve_TheReDispatchBoundIsOneAllowanceForTheWholeRun(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	planCalls, branchCalls := 0, 0
	app, _, errBuf := resolveTestAppFlow(t, be, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			planCalls++
			if planCalls == 1 {
				return flow.StepResult{}, fmt.Errorf("the runner is flapping: %w", flow.ErrTransient)
			}
			return ctx.Next("commit", "planned").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Entry: true, Role: "contributor", Next: []flow.StepId{"commit"}})
		f.AddStep("open branch", "commit", func(flow.StepCtx) (flow.StepResult, error) {
			branchCalls++
			return flow.StepResult{}, fmt.Errorf("the runner is still flapping: %w", flow.ErrTransient)
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if planCalls != 2 {
		t.Fatalf("the first step was dispatched %d time(s), want 2 (its park, and the re-dispatch that cleared it)", planCalls)
	}
	// maxRedispatches - 1 re-dispatches are left, so the second step is
	// dispatched that many times plus its own first. A per-park counter would
	// give it maxRedispatches + 1.
	if want := maxRedispatches; branchCalls != want {
		t.Errorf("the second step was dispatched %d time(s), want %d — the allowance the first step spent was not refunded",
			branchCalls, want)
	}
	if !strings.Contains(errBuf.String(), fmt.Sprintf("parked after %d re-dispatches", maxRedispatches)) {
		t.Errorf("the stop does not stand on the run's whole allowance; got:\n%s", errBuf.String())
	}
}

// WHICH BINARY drove the run. A binary that cannot say what it is cannot be
// the subject of a bug report, and `-version` only answers somebody who thinks
// to ask — this is the line that gets pasted into the report.
func TestCmdResolve_AnnouncementNamesTheBinaryVersion(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "a task"})
	app, _, errBuf := resolveTestApp(t, be)
	app.Version = "source build 5a9a603 (modified)"

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "source build 5a9a603 (modified)") {
		t.Errorf("the narration does not name the binary's version; got:\n%s", errBuf.String())
	}
}

// An empty App.Version prints NOTHING AT ALL rather than a gap — the way the
// standing leaves out the filer line rather than printing an empty one. The
// SDK is a library and has no version of its own to fall back on, so inventing
// a placeholder would put a fact in the transcript that is not one.
func TestCmdResolve_AnEmptyVersionPrintsNoLine(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "a task"})
	app, _, errBuf := resolveTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d; err=%q", code, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "version:") {
		t.Errorf("an empty App.Version printed a line anyway; got:\n%s", errBuf.String())
	}
}

// WHICH ITEM. A bare owner/repo#N is not something an operator can check — one
// who typed 275 meaning 276 learns it from the first prompt or from the pull
// request, after the run has spent. The title goes through titleLine like every
// other piece of free backend prose.
func TestCmdResolve_AnnouncementNamesTheItemTitle(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "Pin bump flow to head\tand migrate issueflow"})
	app, _, errBuf := resolveTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d; err=%q", code, errBuf.String())
	}
	// Collapsed onto one line by titleLine, and carried beside the ref so the
	// two read as one fact about one item.
	if !strings.Contains(errBuf.String(), "1: Pin bump flow to head and migrate issueflow\n") {
		t.Errorf("the narration does not name the item; got:\n%s", errBuf.String())
	}
	// Before the driving line, which is what an operator reads first.
	out := errBuf.String()
	if strings.Index(out, "1: Pin bump") > strings.Index(out, "driving") {
		t.Errorf("the item line comes after the driving line; got:\n%s", out)
	}
}

// A title that is empty or all whitespace drops the SEGMENT, rather than
// printing a ref with a dangling colon.
func TestCmdResolve_AWhitespaceTitleDropsTheItemLine(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: " \n\t "})
	app, _, errBuf := resolveTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d; err=%q", code, errBuf.String())
	}
	for _, line := range strings.Split(errBuf.String(), "\n") {
		if strings.HasPrefix(line, "1: ") || line == "1:" {
			t.Errorf("a whitespace-only title printed an item line anyway: %q", line)
		}
	}
}

// The title costs NO ADDITIONAL REQUEST. The standing announcement already
// loads the item to read Creator and discarded the rest, so the title rides out
// on that load — a second one here would be a request for a value the first
// already fetched.
//
// announceStanding is exercised directly rather than through a whole resolve,
// because a resolution legitimately loads the item several times (the claim,
// the peek before each dispatch) and a count over all of them would not isolate
// the one this is about.
func TestAnnounceStanding_LoadsTheItemOnceForBothFacts(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "a task"})
	counter := &loadCountingBackend{Orchestrator: be}
	app, _, _ := resolveTestApp(t, counter)
	ref, err := be.ResolveRef(t.Context(), "1")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}

	stand := app.announceStanding(t.Context(), flow.Claim{ItemRef: ref, Account: "acct"})

	if counter.loads != 1 {
		t.Errorf("the announcement made %d loads, want 1", counter.loads)
	}
	if stand.title != "a task" {
		t.Errorf("title = %q, want the item's", stand.title)
	}
}

// The load is BEST-EFFORT: a read that fails prints the ref alone, exactly as
// it did before the title was printed at all. An announcement is not worth
// failing a resolution over, and the standing — which is what says how far the
// run can get — must not be withheld because an unrelated read failed.
func TestAnnounceStanding_AFailedLoadStillAnnounces(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "a task"})
	ref, err := be.ResolveRef(t.Context(), "1")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	app, _, errBuf := resolveTestApp(t, &loadAlwaysFailsBackend{Orchestrator: be})
	app.Version = "v1.2.3"

	stand := app.announceStanding(t.Context(), flow.Claim{ItemRef: ref, Account: "acct"})

	if stand.title != "" {
		t.Errorf("title = %q, want empty when the load failed", stand.title)
	}
	out := errBuf.String()
	if !strings.Contains(out, "driving 1 to completion") {
		t.Errorf("the run did not announce itself; got:\n%s", out)
	}
	if !strings.Contains(out, "acting as acct") {
		t.Errorf("the standing was withheld because an unrelated read failed; got:\n%s", out)
	}
	// The version is the host's own string and needs no read at all, so it
	// prints whatever the backend is doing.
	if !strings.Contains(out, "v1.2.3") {
		t.Errorf("the version needs no read and must print regardless; got:\n%s", out)
	}
}

// THE PROGRESS LINE NEVER NAMES A STEP THAT WILL NOT RUN. When the advance
// stops before dispatch because the item waits on unfinished dependencies,
// `running "plan"…` is simply false: nothing ran. The peek already holds the
// loaded item and its block, so it says what is actually about to happen.
func TestCmdResolve_BlockedItemIsNotAnnouncedAsRunning(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "waits"})
	be.AddItem("2", flow.Item{Type: "task", Title: "the blocker"})
	item, err := be.ResolveRef(t.Context(), "1")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	blocker, err := be.ResolveRef(t.Context(), "2")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	blockOn(t, be, item, blocker)
	app, _, errBuf := resolveTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 1 {
		t.Fatalf("exit code = %d, want 1 — a blocked item is a condition somebody must clear; err=%q", code, errBuf.String())
	}
	out := errBuf.String()
	if strings.Contains(out, `running "`) {
		t.Errorf("a step was announced as running before a dispatch that never happened; got:\n%s", out)
	}
	if !strings.Contains(out, "waits on unfinished dependencies — not dispatching") {
		t.Errorf("the narration does not say why nothing is being dispatched; got:\n%s", out)
	}
	// The blockers are named by reference, so the operator has something to go
	// work instead.
	if !strings.Contains(out, "blocked by: 2") {
		t.Errorf("the narration does not name the open blockers; got:\n%s", out)
	}
}

// THE OUTCOME LINE IS SET OFF by an empty line before and after, so the one
// line an operator must act on does not sit flush between progress lines.
func TestCmdResolve_TheOutcomeLineIsSetOff(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "a task"})
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		return flow.StepResult{}, nil // parks
	})

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d; err=%q", code, errBuf.String())
	}
	lines := strings.Split(errBuf.String(), "\n")
	found := false
	for i, line := range lines {
		if !strings.Contains(line, "→ parked") {
			continue
		}
		found = true
		if i == 0 || lines[i-1] != "" {
			t.Errorf("no empty line before the outcome:\n%s", errBuf.String())
		}
		// The indented detail lines belong to the outcome; the empty line comes
		// after the last of them.
		j := i + 1
		for j < len(lines) && strings.HasPrefix(lines[j], "  ") {
			j++
		}
		if j >= len(lines) || lines[j] != "" {
			t.Errorf("no empty line after the outcome and its detail:\n%s", errBuf.String())
		}
		break
	}
	if !found {
		t.Fatalf("no outcome line at all:\n%s", errBuf.String())
	}
}

// A PARK NAMES THE ACT THAT RESUMES IT. docs/cli.md already requires a refusal
// to carry the overriding flag where one exists, and a park is held to the same
// standard — an operator who answered a parked question, re-ran, and hit the
// same budget park made a round trip this line prevents.
//
// The act is derived from the park KIND, so this checks the kind reaches the
// table rather than re-checking the table itself, which
// TestParkKindTable_CoversEveryKind owns.
func TestCmdResolve_AParkNamesItsResumingAct(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "a task"})
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) (flow.StepResult, error) {
		return flow.StepResult{}, nil // parks step-did-not-complete
	})

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d; err=%q", code, errBuf.String())
	}
	out := errBuf.String()
	if !strings.Contains(out, "to resume:") {
		t.Errorf("the park does not name what resumes it; got:\n%s", out)
	}
	if !strings.Contains(out, "a re-dispatch is what finishes the job it left") {
		t.Errorf("the resuming act does not match the park kind; got:\n%s", out)
	}
}

// loadCountingBackend counts Loads, so a read added for a display fact shows up
// as a request rather than as nothing at all.
type loadCountingBackend struct {
	*fake.Orchestrator
	loads int
}

func (b *loadCountingBackend) Load(ctx context.Context, ref flow.ItemRef) (*flow.Item, error) {
	b.loads++
	return b.Orchestrator.Load(ctx, ref)
}

// loadAlwaysFailsBackend refuses every Load, which is what a backend that has
// gone away looks like to a best-effort read.
type loadAlwaysFailsBackend struct{ *fake.Orchestrator }

func (b *loadAlwaysFailsBackend) Load(ctx context.Context, ref flow.ItemRef) (*flow.Item, error) {
	return nil, errors.New("backend unavailable (injected)")
}
