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
	"github.com/promise-language/flow/pkg/backend/fake"
)

// resolveTestApp builds an App with a single-step "write plan" flow over the
// given backend, pre-loaded with one fake item. Mirrors orchestrator_test.go's
// testApp but does NOT pre-claim the item (auto-select tests need an
// un-leased arena) and lets the caller swap in a wrapping backend (e.g. one
// that fails ListEligible). The step resolves its artifact, so the run
// advances and then finalizes.
func resolveTestApp(t *testing.T, be flow.Backend) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	return resolveTestAppStep(t, be, func(ctx flow.StepCtx) error {
		return ctx.ResolveMarkdown("the plan")
	})
}

// resolveTestAppStep is resolveTestApp with a caller-supplied handler for the
// single "write plan" step — the lever for driving the loop to an outcome
// other than done (return nil without resolving to park it, return an error to
// fail it).
func resolveTestAppStep(t *testing.T, be flow.Backend, step func(flow.StepCtx) error) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	app := &App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Out:       out,
		Err:       errBuf,
		Owner:     "alice",
	}
	f := flow.NewFlow("implement", []flow.ItemType{"task"})
	f.AddStep("write plan", "plan", step, flow.StepConfig{})

	app.Flows = []*flow.Flow{f}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return app, out, errBuf
}

// failingListBackend wraps a fake backend and forces ListEligible to error.
// Used to prove a code path doesn't touch ListEligible — if it does, the
// surrounding test fails.
type failingListBackend struct {
	*fake.Backend
	err error
}

func (b *failingListBackend) ListEligible(ctx context.Context) ([]flow.ItemRef, error) {
	return nil, b.err
}

// failingLookupActiveClaimBackend forces LookupActiveClaim to error, so we
// can exercise the no-arg / LookupActiveClaim error branch added by this
// change.
type failingLookupActiveClaimBackend struct {
	*fake.Backend
	err error
}

func (b *failingLookupActiveClaimBackend) LookupActiveClaim(ctx context.Context, owner string) (*flow.Claim, error) {
	return nil, b.err
}

// failingClaimBackend lets ListEligible succeed but forces the subsequent
// Claim call to fail — exercises the auto-select branch's "Claim failed on
// the chosen ref" error path.
type failingClaimBackend struct {
	*fake.Backend
	claimErr error
}

func (b *failingClaimBackend) Claim(ctx context.Context, ref flow.ItemRef, owner string, overrides []flow.ClaimOverride) (flow.Claim, error) {
	return flow.Claim{}, b.claimErr
}

// resolvingFailingListBackend implements flow.RefResolver (so the explicit-id
// path takes the fast lane and never hits ListEligible) AND forces
// ListEligible to error — together they prove the explicit-id branch of
// cmdResolve never calls ListEligible.
type resolvingFailingListBackend struct {
	*fake.Backend
	listErr error
}

func (b *resolvingFailingListBackend) ListEligible(ctx context.Context) ([]flow.ItemRef, error) {
	return nil, b.listErr
}

func (b *resolvingFailingListBackend) ResolveRef(ctx context.Context, id string) (flow.ItemRef, error) {
	return flow.ItemRef{BackendName: "fake", Display: id, Ref: json.RawMessage(`"` + id + `"`)}, nil
}

func TestCmdResolve_AutoSelectsWhenUnleased(t *testing.T) {
	be := fake.New()
	be.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "auto-selecting") {
		t.Errorf("expected Err to mention auto-selecting; got %q", errBuf.String())
	}

	// Item must now be claimed by app.Owner.
	ref := flow.ItemRef{BackendName: "fake", Ref: json.RawMessage(`"1"`)}
	info, err := be.LookupClaim(context.Background(), ref)
	if err != nil {
		t.Fatalf("LookupClaim: %v", err)
	}
	if info == nil || info.Owner != "alice" {
		t.Errorf("LookupClaim = %+v, want owner=alice", info)
	}

	// Plan artifact must have been resolved (proves the loop actually ran
	// the step, not just claimed).
	claim := flow.Claim{BackendName: "fake", ItemRef: ref, Owner: "alice", Token: json.RawMessage(`{}`)}
	state, err := be.LoadState(context.Background(), claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	rec := state.Artifact("plan")
	if !rec.Resolved || rec.Markdown != "the plan" {
		t.Errorf("plan artifact = %+v, want resolved markdown 'the plan'", rec)
	}
}

func TestCmdResolve_ResumesActiveClaim(t *testing.T) {
	inner := fake.New()
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	// Pre-claim as app.Owner — cmdResolve must resume this claim via
	// LookupActiveClaim and never touch ListEligible (which fails here).
	ref := flow.ItemRef{BackendName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)}
	if _, err := inner.Claim(context.Background(), ref, "alice", nil); err != nil {
		t.Fatalf("pre-claim: %v", err)
	}
	be := &failingListBackend{Backend: inner, err: errors.New("ListEligible must not be called on resume path")}
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
	be.AddItem(flow.Item{ID: "1", Type: "chore", Title: "1"})
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

// TestCmdResolve_FinalizedUnmatchedTypeNarratesFinalize (#10): the peek's
// unmatched-type branch carries RunOne's already-finalized exemption. An item
// that IS finalized takes the finalize path, so announcing "no flow accepts
// this item's type" for it is the same misreport inverted.
func TestCmdResolve_FinalizedUnmatchedTypeNarratesFinalize(t *testing.T) {
	be := fake.New()
	be.AddItem(flow.Item{ID: "1", Type: "chore", Title: "1", Finalized: true})
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
	be := &failingListBackend{Backend: inner, err: errors.New("boom")}
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
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	be := &failingLookupActiveClaimBackend{Backend: inner, err: errors.New("lookup boom")}
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
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	be := &failingClaimBackend{Backend: inner, claimErr: errors.New("claim boom")}
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
	*fake.Backend
	refs          []flow.ItemRef
	conflictRefs  map[string]bool
	claimAttempts []string
	listErr       error
	nonConflict   error // if set, every Claim returns this error instead
}

func (b *conflictThenOkBackend) ListEligible(ctx context.Context) ([]flow.ItemRef, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	return b.refs, nil
}

func (b *conflictThenOkBackend) Claim(ctx context.Context, ref flow.ItemRef, owner string, overrides []flow.ClaimOverride) (flow.Claim, error) {
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
	return b.Backend.Claim(ctx, ref, owner, overrides)
}

func TestCmdResolve_AutoSelectIteratesOnLeaseConflict(t *testing.T) {
	inner := fake.New()
	inner.AddItem(flow.Item{ID: "3", Type: "task", Title: "3"})
	refs := []flow.ItemRef{
		{BackendName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)},
		{BackendName: "fake", Display: "2", Ref: json.RawMessage(`"2"`)},
		{BackendName: "fake", Display: "3", Ref: json.RawMessage(`"3"`)},
	}
	be := &conflictThenOkBackend{
		Backend:      inner,
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
	claim := flow.Claim{BackendName: "fake", ItemRef: refs[2], Owner: "alice", Token: json.RawMessage(`{}`)}
	state, err := inner.LoadState(context.Background(), claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if rec := state.Artifact("plan"); !rec.Resolved {
		t.Errorf("expected plan artifact resolved on ref 3, got %+v", rec)
	}
}

func TestCmdResolve_AutoSelectAllRefsConflictExitsZero(t *testing.T) {
	inner := fake.New()
	refs := []flow.ItemRef{
		{BackendName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)},
		{BackendName: "fake", Display: "2", Ref: json.RawMessage(`"2"`)},
	}
	be := &conflictThenOkBackend{
		Backend:      inner,
		refs:         refs,
		conflictRefs: map[string]bool{`"1"`: true, `"2"`: true},
	}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (all-conflict is a clean no-op exit); err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "every eligible item is leased to another arena") {
		t.Errorf("expected 'every eligible item is leased' message; got %q", errBuf.String())
	}
}

func TestCmdResolve_AutoSelectNonConflictErrorExitsOne(t *testing.T) {
	inner := fake.New()
	refs := []flow.ItemRef{
		{BackendName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)},
		{BackendName: "fake", Display: "2", Ref: json.RawMessage(`"2"`)},
	}
	be := &conflictThenOkBackend{
		Backend:     inner,
		refs:        refs,
		nonConflict: errors.New("dial tcp: connection refused"),
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
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	inner.AddItem(flow.Item{ID: "2", Type: "task", Title: "2"})
	refs := []flow.ItemRef{
		{BackendName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)},
		{BackendName: "fake", Display: "2", Ref: json.RawMessage(`"2"`)},
	}
	be := &arenaScopedRefusalBackend{
		Backend: inner,
		refs:    refs,
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
	*fake.Backend
	refs          []flow.ItemRef
	claimAttempts int
}

func (b *arenaScopedRefusalBackend) ListEligible(ctx context.Context) ([]flow.ItemRef, error) {
	return b.refs, nil
}

func (b *arenaScopedRefusalBackend) Claim(ctx context.Context, ref flow.ItemRef, owner string, overrides []flow.ClaimOverride) (flow.Claim, error) {
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
	be.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
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
	be.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
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
	be.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
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
	if !strings.Contains(errBuf.String(), "finalized ✓") {
		t.Errorf("JSON mode must still narrate the finalize line; got %q", errBuf.String())
	}
}

func TestCmdResolve_FlowOutputEnvSelectsJSON(t *testing.T) {
	t.Setenv(outputEnv, "json")
	be := fake.New()
	be.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
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
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	be := &failingLookupActiveClaimBackend{Backend: inner, err: errors.New("lookup boom")}
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
		be.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
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
	be.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
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
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	be := &resolvingFailingListBackend{Backend: inner, listErr: errors.New("ListEligible must not be called on explicit-id path")}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}

	// Item must be claimed by app.Owner.
	ref := flow.ItemRef{BackendName: "fake", Ref: json.RawMessage(`"1"`)}
	info, err := inner.LookupClaim(context.Background(), ref)
	if err != nil {
		t.Fatalf("LookupClaim: %v", err)
	}
	if info == nil || info.Owner != "alice" {
		t.Errorf("LookupClaim = %+v, want owner=alice", info)
	}
}

// failingLoadStateBackend forces LoadState to error. Both the progress peek
// and RunOne read state, so this drives the loop's RunOne-error branch — the
// one exit that leaves the loop without an InvocationResult to report.
type failingLoadStateBackend struct {
	*fake.Backend
	err error
}

func (b *failingLoadStateBackend) LoadState(ctx context.Context, claim flow.Claim) (*flow.ItemState, error) {
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
		step       func(flow.StepCtx) error
		wantCode   int
		wantStatus string
	}{
		// Returning without resolving the artifact parks the step.
		{"parked", func(ctx flow.StepCtx) error { return nil }, 0, "parked"},
		{"failed", func(ctx flow.StepCtx) error { return errors.New("handler boom") }, 1, "failed"},
	}
	for _, c := range cases {
		t.Run(c.name+"/json", func(t *testing.T) {
			t.Setenv(outputEnv, "")
			be := fake.New()
			be.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
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
			be.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
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
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	be := &failingLoadStateBackend{Backend: inner, err: errors.New("state boom")}
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
	*fake.Backend
	taggedRefs []flow.ItemRef
	calledTags []string
}

func (b *tagFilterBackend) ListEligibleWithTags(ctx context.Context, tags []string) ([]flow.ItemRef, error) {
	b.calledTags = tags
	return b.taggedRefs, nil
}

// TestCmdResolve_TagFilterSelectsFromTaggedSet verifies that --tag uses
// TagFilterer and not ListEligible.
func TestCmdResolve_TagFilterSelectsFromTaggedSet(t *testing.T) {
	inner := fake.New()
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	be := &tagFilterBackend{
		Backend: inner,
		taggedRefs: []flow.ItemRef{
			{BackendName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)},
		},
	}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"--tag", "priority:high"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if len(be.calledTags) != 1 || be.calledTags[0] != "priority:high" {
		t.Errorf("TagFilterer called with %v, want [priority:high]", be.calledTags)
	}
}

// TestCmdResolve_TagAndIdMutuallyExclusive verifies the usage error.
func TestCmdResolve_TagAndIdMutuallyExclusive(t *testing.T) {
	be := fake.New()
	be.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
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
		Backend:    inner,
		taggedRefs: nil, // empty
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

// TestCmdResolve_TagWithoutTagFiltererRefused verifies that --tag is refused
// when the backend doesn't implement TagFilterer.
func TestCmdResolve_TagWithoutTagFiltererRefused(t *testing.T) {
	be := fake.New() // no TagFilterer
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"--tag", "x"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "does not support --tag") {
		t.Errorf("expected TagFilterer-not-supported message; got %q", errBuf.String())
	}
}

// ---------------------------------------------------------------------------
// Invariant: resolve's auto-select NEVER calls Discover.
// ---------------------------------------------------------------------------

// discoverPanicBackend wraps fake and panics if Discover is called. This
// proves the invariant stated in the Discoverer interface doc: resolve's
// auto-select path must never call Discover.
type discoverPanicBackend struct {
	*fake.Backend
}

func (b *discoverPanicBackend) Discover(ctx context.Context, scope flow.DiscoveryScope, binaryName string) ([]flow.DiscoveryItem, error) {
	panic("INVARIANT VIOLATION: resolve's auto-select called Discover")
}

// TestCmdResolve_AutoSelectNeverCallsDiscover is the invariant test from
// item 2 of issue #6.
func TestCmdResolve_AutoSelectNeverCallsDiscover(t *testing.T) {
	inner := fake.New()
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	be := &discoverPanicBackend{Backend: inner}
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
	*fake.Backend
	refusal flow.ErrClaimRefused
}

func (b *refusingResolveBackend) ResolveRef(ctx context.Context, id string) (flow.ItemRef, error) {
	return flow.ItemRef{BackendName: "fake", Display: id, Ref: json.RawMessage(`"` + id + `"`)}, nil
}

func (b *refusingResolveBackend) Claim(ctx context.Context, ref flow.ItemRef, owner string, overrides []flow.ClaimOverride) (flow.Claim, error) {
	return flow.Claim{}, b.refusal
}

func TestCmdResolve_ExplicitIdRefusalRendering(t *testing.T) {
	inner := fake.New()
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	be := &refusingResolveBackend{
		Backend: inner,
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
	*fake.Backend
	lastOverrides []flow.ClaimOverride
}

func (b *overrideRecordingResolveBackend) ResolveRef(ctx context.Context, id string) (flow.ItemRef, error) {
	return flow.ItemRef{BackendName: "fake", Display: id, Ref: json.RawMessage(`"` + id + `"`)}, nil
}

func (b *overrideRecordingResolveBackend) Claim(ctx context.Context, ref flow.ItemRef, owner string, overrides []flow.ClaimOverride) (flow.Claim, error) {
	b.lastOverrides = overrides
	return b.Backend.Claim(ctx, ref, owner, overrides)
}

func TestCmdResolve_ForceUnadmittedPassesOverride(t *testing.T) {
	inner := fake.New()
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	be := &overrideRecordingResolveBackend{Backend: inner}
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
	*fake.Backend
	state *flow.ItemState
	err   error
}

func (b *totalSuffixInspectBackend) LoadStateByRef(_ context.Context, _ flow.ItemRef) (*flow.ItemState, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.state, nil
}

func TestFinalTotalSuffix_NoStateInspector(t *testing.T) {
	// Plain fake.Backend does not implement StateInspector.
	app := &App{Backend: fake.New()}
	claim := flow.Claim{}
	got := finalTotalSuffix(context.Background(), app, claim)
	if got != "" {
		t.Errorf("expected empty string when backend is not StateInspector; got %q", got)
	}
}

func TestFinalTotalSuffix_LoadStateError(t *testing.T) {
	be := &totalSuffixInspectBackend{
		Backend: fake.New(),
		err:     errors.New("state unavailable"),
	}
	app := &App{Backend: be}
	got := finalTotalSuffix(context.Background(), app, flow.Claim{})
	if got != "" {
		t.Errorf("expected empty string on LoadStateByRef error; got %q", got)
	}
}

func TestFinalTotalSuffix_NoFigures(t *testing.T) {
	be := &totalSuffixInspectBackend{
		Backend: fake.New(),
		state: &flow.ItemState{
			Artifacts: map[flow.ArtifactId]flow.ArtifactRecord{
				"plan": {}, // zero duration, zero cost
			},
		},
	}
	app := &App{Backend: be}
	got := finalTotalSuffix(context.Background(), app, flow.Claim{})
	if got != "" {
		t.Errorf("expected empty string when all figures are zero; got %q", got)
	}
}

func TestFinalTotalSuffix_BothFigures(t *testing.T) {
	be := &totalSuffixInspectBackend{
		Backend: fake.New(),
		state: &flow.ItemState{
			Artifacts: map[flow.ArtifactId]flow.ArtifactRecord{
				"plan":           {DurationWorked: 5 * time.Minute, CostUSDSpent: 1.20, Resolved: true},
				"implementation": {DurationWorked: 9*time.Minute + 2*time.Second, CostUSDSpent: 1.51, Resolved: true},
			},
		},
	}
	app := &App{Backend: be}
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
		Backend: fake.New(),
		state: &flow.ItemState{
			Artifacts: map[flow.ArtifactId]flow.ArtifactRecord{
				"plan":   {DurationWorked: 5 * time.Minute, CostUSDSpent: 1.20, Resolved: true},
				"legacy": {DurationWorked: 0, CostUSDSpent: 0.50, Resolved: true}, // resolved but no duration → lower bound
			},
		},
	}
	app := &App{Backend: be}
	got := finalTotalSuffix(context.Background(), app, flow.Claim{})
	if !strings.Contains(got, "≥") {
		t.Errorf("expected lower-bound prefix ≥; got %q", got)
	}
	if !strings.Contains(got, "$1.70") {
		t.Errorf("expected total cost $1.70; got %q", got)
	}
}

func TestFinalTotalSuffix_UnresolvedZeroDurationNotLowerBound(t *testing.T) {
	// An unresolved artifact with zero duration is not a lower bound —
	// it just hasn't run yet.
	be := &totalSuffixInspectBackend{
		Backend: fake.New(),
		state: &flow.ItemState{
			Artifacts: map[flow.ArtifactId]flow.ArtifactRecord{
				"plan":    {DurationWorked: 5 * time.Minute, CostUSDSpent: 1.20, Resolved: true},
				"pending": {DurationWorked: 0, CostUSDSpent: 0, Resolved: false},
			},
		},
	}
	app := &App{Backend: be}
	got := finalTotalSuffix(context.Background(), app, flow.Claim{})
	if strings.Contains(got, "≥") {
		t.Errorf("unresolved artifact with zero duration must not trigger lower-bound; got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Budget park narration includes axes (#3)
// ---------------------------------------------------------------------------

func TestCmdResolve_BudgetParkNarratesAxes(t *testing.T) {
	// Use testApp (which pre-claims) to burn the only invocation, then run
	// cmdResolve which resumes the claim and immediately parks on budget.
	handler := func(ctx flow.StepCtx) error {
		return errors.New("boom")
	}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", handler, flow.StepConfig{Budget: flow.StepBudget{
			MaxInvocations:          1,
			MaxPromptsPerInvocation: 2,
			MaxCostUSD:              10,
			Timeout:                 30 * time.Minute,
		}})
	}, &stubAgent{name: "stub"})
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
	app.Backend = be // ensure it uses the same backend

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
	be.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) error {
		return nil // returns without resolving → did-not-resolve park
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

// unfitBackend wraps a fake backend and implements flow.FitnessChecker with
// an unconditionally-unfit verdict.
type unfitBackend struct {
	*fake.Backend
}

func (b *unfitBackend) CheckFit(ctx context.Context) (flow.GateVerdict, error) {
	return flow.GateVerdict{
		Acceptable: false,
		Detail:     "12 MB free, floor 2 GB",
		Thresholds: []byte("{}"),
	}, nil
}

// TestResolve_UnfitMachineExitsBeforeClaiming verifies that when the backend
// reports the machine as permanently unfit, resolve exhausts the fitness wait
// and exits 1 without claiming the item.
func TestResolve_UnfitMachineExitsBeforeClaiming(t *testing.T) {
	old := fitnessWaitInterval
	fitnessWaitInterval = time.Millisecond
	defer func() { fitnessWaitInterval = old }()

	inner := fake.New()
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	be := &unfitBackend{Backend: inner}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (unfit machine); err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "machine unfit") {
		t.Errorf("expected 'machine unfit' in stderr; got %q", errBuf.String())
	}
	// The item must NOT be claimed.
	claim, err := be.LookupActiveClaim(context.Background(), "alice")
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if claim != nil {
		t.Errorf("item should not be claimed after unfit verdict; got %+v", claim)
	}
}

// fitBackend wraps a fake backend and implements flow.FitnessChecker with an
// unconditionally-fit verdict.
type fitBackend struct {
	*fake.Backend
}

func (b *fitBackend) CheckFit(ctx context.Context) (flow.GateVerdict, error) {
	return flow.GateVerdict{Acceptable: true}, nil
}

// TestResolve_FitMachineProceeds verifies that a fit verdict lets resolve
// proceed normally (item gets claimed and driven through the step).
func TestResolve_FitMachineProceeds(t *testing.T) {
	inner := fake.New()
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	be := &fitBackend{Backend: inner}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	// Item must be claimed.
	claim, err := be.LookupActiveClaim(context.Background(), "alice")
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if claim == nil {
		t.Error("item should be claimed after fit verdict")
	}
}

// fitErrorBackend wraps a fake backend and implements flow.FitnessChecker
// that always returns an error — exercises the fail-open path.
type fitErrorBackend struct {
	*fake.Backend
}

func (b *fitErrorBackend) CheckFit(ctx context.Context) (flow.GateVerdict, error) {
	return flow.GateVerdict{}, errors.New("gate broken")
}

// TestResolve_FitnessCheckErrorProceeds verifies that when CheckFit returns
// an error, resolve proceeds (fail-open) rather than blocking.
func TestResolve_FitnessCheckErrorProceeds(t *testing.T) {
	inner := fake.New()
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	be := &fitErrorBackend{Backend: inner}
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (fail-open on gate error); err=%q", code, errBuf.String())
	}
	// Item must be claimed — the error did not block.
	claim, err := be.LookupActiveClaim(context.Background(), "alice")
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if claim == nil {
		t.Error("item should be claimed when CheckFit returns an error (fail-open)")
	}
}

// ---------------------------------------------------------------------------
// Mid-step fitness wait: standalone wait-and-retry on StatusBlocked
// ---------------------------------------------------------------------------

// transientUnfitBackend returns an unfit verdict for the first N CheckFit
// calls, then switches to fit. It also allows the step handler to return
// ErrUnfit on the first run by controlling the fake's gate verdict.
type transientUnfitBackend struct {
	*fake.Backend
	mu          sync.Mutex
	checkCalls  int
	unfitRounds int // how many CheckFit calls return unfit before switching
}

func (b *transientUnfitBackend) CheckFit(ctx context.Context) (flow.GateVerdict, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.checkCalls++
	if b.checkCalls <= b.unfitRounds {
		return flow.GateVerdict{
			Acceptable: false,
			Detail:     "12 MB free, floor 2 GB",
			Thresholds: []byte("{}"),
		}, nil
	}
	return flow.GateVerdict{Acceptable: true}, nil
}

// TestResolve_FitnessWaitRetriesOnRecovery verifies that when a step returns
// ErrUnfit and the machine subsequently recovers, cmdResolve waits and retries
// rather than exiting immediately.
func TestResolve_FitnessWaitRetriesOnRecovery(t *testing.T) {
	old := fitnessWaitInterval
	fitnessWaitInterval = time.Millisecond
	defer func() { fitnessWaitInterval = old }()

	inner := fake.New()
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})

	stepCalls := 0
	// The backend starts fit (unfitRounds: 0) so the pre-claim check passes.
	// After the step returns ErrUnfit, the mid-step CheckFit also returns fit,
	// so cmdResolve retries immediately.
	be := &transientUnfitBackend{Backend: inner, unfitRounds: 0}
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) error {
		stepCalls++
		if stepCalls == 1 {
			return fmt.Errorf("12 MB free, floor 2 GB: %w", flow.ErrUnfit)
		}
		return ctx.ResolveMarkdown("the plan")
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
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})

	stepCalls := 0
	// Pre-claim CheckFit (call 1) is fit (unfitRounds=0 → always fit at that
	// stage). Then the step returns ErrUnfit. The mid-step re-check (calls
	// 2..4) returns unfit for 3 rounds, then fit on call 5.
	be := &transientUnfitBackend{Backend: inner, unfitRounds: 0}
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) error {
		stepCalls++
		if stepCalls == 1 {
			// After the step fails, make the next 3 CheckFit calls unfit.
			be.mu.Lock()
			be.checkCalls = 0
			be.unfitRounds = 3
			be.mu.Unlock()
			return fmt.Errorf("12 MB free, floor 2 GB: %w", flow.ErrUnfit)
		}
		return ctx.ResolveMarkdown("the plan")
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
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})

	// Pre-claim: fit (unfitRounds=0). Mid-step: permanently unfit.
	be := &transientUnfitBackend{Backend: inner, unfitRounds: 0}
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) error {
		be.mu.Lock()
		be.checkCalls = 0
		be.unfitRounds = maxFitnessWaits + 10
		be.mu.Unlock()
		return fmt.Errorf("disk full: %w", flow.ErrUnfit)
	})

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (fitness wait exhausted); err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "is blocked") {
		t.Errorf("expected 'is blocked' in stderr when wait exhausted; got %q", errBuf.String())
	}
}

// TestResolve_FitnessWaitInterruptedExits verifies that cancelling the context
// during a fitness wait exits cleanly.
func TestResolve_FitnessWaitInterruptedExits(t *testing.T) {
	old := fitnessWaitInterval
	fitnessWaitInterval = time.Hour // long enough to guarantee the cancel fires first
	defer func() { fitnessWaitInterval = old }()

	inner := fake.New()
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})

	// Pre-claim: fit (unfitRounds=0). Mid-step: permanently unfit.
	be := &transientUnfitBackend{Backend: inner, unfitRounds: 0}
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) error {
		be.mu.Lock()
		be.checkCalls = 0
		be.unfitRounds = 999
		be.mu.Unlock()
		return fmt.Errorf("disk full: %w", flow.ErrUnfit)
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
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	be := &fitBackend{Backend: inner}
	app, _, errBuf := resolveTestAppStep(t, be, func(ctx flow.StepCtx) error {
		return ctx.ResolveMarkdown("the plan")
	})

	// A preflight that always returns ErrBlocked — produces StatusBlocked
	// with a reason that has nothing to do with fitness.
	app.Preflight = func(_ context.Context, _ *flow.ItemState) error {
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
	inner.AddItem(flow.Item{ID: "1", Type: "task", Title: "1"})
	// Unfit for 3 rounds, then fit.
	be := &transientUnfitBackend{Backend: inner, unfitRounds: 3}
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
	claim, err := be.LookupActiveClaim(context.Background(), "alice")
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if claim == nil {
		t.Error("item should be claimed after transient unfitness clears")
	}
}
