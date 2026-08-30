package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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

func (b *failingClaimBackend) Claim(ctx context.Context, ref flow.ItemRef, owner string, force bool) (flow.Claim, error) {
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
	if _, err := inner.Claim(context.Background(), ref, "alice", false); err != nil {
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

func (b *conflictThenOkBackend) Claim(ctx context.Context, ref flow.ItemRef, owner string, force bool) (flow.Claim, error) {
	id := string(ref.Ref)
	b.claimAttempts = append(b.claimAttempts, id)
	if b.nonConflict != nil {
		return flow.Claim{}, b.nonConflict
	}
	if b.conflictRefs[id] {
		// Mirror tracker ErrItemAlreadyLeased wrapped through the runner
		// 502 + Backend.Claim prefixes — the substring isLeaseItemConflict
		// matches must survive all wraps.
		return flow.Claim{}, errors.New(`tracker backend: Claim: runner POST /v1/lease: 502 Bad Gateway: lease: record manual lease on tracker: tracker 409 Conflict: item "x" already leased to arena "other"; incoming arena "self" refused`)
	}
	return b.Backend.Claim(ctx, ref, owner, force)
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
	if !strings.Contains(errBuf.String(), "1 already leased by another arena") {
		t.Errorf("expected ref 1 conflict-skip line; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "2 already leased by another arena") {
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

func TestIsLeaseItemConflict(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain conflict (raw ledger format)", errors.New(`item "T0042" already leased to arena "other"; incoming arena "self" refused`), true},
		{"fully wrapped through runner 502 + tracker backend prefix", errors.New(`tracker backend: Claim: runner POST /v1/lease: 502 Bad Gateway: lease: record manual lease on tracker: tracker 409 Conflict: item "T0042" already leased to arena "other"; incoming arena "self" refused`), true},
		{"unrelated error", errors.New("dial tcp: connection refused"), false},
		{"arena bijection (a different conflict — NOT retryable per-ref)", errors.New(`arena "self" already leases item "T0099"`), false},
	}
	for _, c := range cases {
		if got := isLeaseItemConflict(c.err); got != c.want {
			t.Errorf("isLeaseItemConflict(%q) = %v, want %v", c.err, got, c.want)
		}
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
	if !strings.Contains(errBuf.String(), `resolve: write plan → done`) {
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
	if got[0].Step != "write plan" || got[0].Status != "done" {
		t.Errorf("first result = %+v, want step %q status done", got[0], "write plan")
	}
	if got[1].Step != "" || got[1].Status != "done" {
		t.Errorf("second result = %+v, want the empty-step finalize, status done", got[1])
	}
	// JSON mode does not silence the narration: `resolve > steps.json` must
	// still show progress on the terminal.
	if !strings.Contains(errBuf.String(), `resolve: write plan → done`) {
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
			if !strings.Contains(errBuf.String(), "write plan → "+c.wantStatus) {
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
