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
// that fails ListEligible).
func resolveTestApp(t *testing.T, be flow.Backend) (*App, *bytes.Buffer, *bytes.Buffer) {
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
	f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
		return ctx.ResolveMarkdown("the plan")
	})
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

func (b *failingClaimBackend) Claim(ctx context.Context, ref flow.ItemRef, owner string) (flow.Claim, error) {
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
	if _, err := inner.Claim(context.Background(), ref, "alice"); err != nil {
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
	if !strings.Contains(errBuf.String(), "no eligible items") {
		t.Errorf("expected Err to mention 'no eligible items'; got %q", errBuf.String())
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

func (b *conflictThenOkBackend) Claim(ctx context.Context, ref flow.ItemRef, owner string) (flow.Claim, error) {
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
	return b.Backend.Claim(ctx, ref, owner)
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
