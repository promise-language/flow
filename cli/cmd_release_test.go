package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// releaseRefusing wraps the fake with a Release that refuses the way a backend
// holding a local checkout does. The fake models no worktree, so the refusal
// has to come from here — what is under test is what the COMMAND does with a
// typed refusal, not where the backend got it.
type releaseRefusing struct {
	*fake.Orchestrator
	err error
}

func (b *releaseRefusing) Release(context.Context, flow.ItemRef, []flow.ClaimOverride) error {
	return b.err
}

// unreadableLease models the state #212 is about: the lease record exists and
// cannot be parsed, so LookupActiveClaim answers with the parse error and
// nothing can say which item this arena holds. Release still has to run.
type unreadableLease struct {
	*fake.Orchestrator
	err error

	releasedRef      flow.ItemRef
	releasedOverride []flow.ClaimOverride
	released         bool
}

func (b *unreadableLease) LookupActiveClaim(context.Context) (*flow.Claim, error) {
	return nil, b.err
}

func (b *unreadableLease) Release(ctx context.Context, ref flow.ItemRef, ov []flow.ClaimOverride) error {
	b.releasedRef, b.releasedOverride, b.released = ref, ov, true
	return b.Orchestrator.Release(ctx, ref, ov)
}

// recordingRelease reports what the command handed the backend, for the cases
// where WHICH ref and WHICH overrides travelled is the whole assertion.
type recordingRelease struct {
	*fake.Orchestrator
	ref       flow.ItemRef
	overrides []flow.ClaimOverride
	called    bool
	err       error
}

func (b *recordingRelease) Release(ctx context.Context, ref flow.ItemRef, ov []flow.ClaimOverride) error {
	b.ref, b.overrides, b.called = ref, ov, true
	if b.err != nil {
		return b.err
	}
	return b.Orchestrator.Release(ctx, ref, ov)
}

func releaseTestApp(t *testing.T, be flow.Orchestrator) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errBuf bytes.Buffer
	return &App{Orchestrator: be, Out: &out, Err: &errBuf}, &out, &errBuf
}

// The positive control for the refusals below: a release that is NOT refused
// names the item it dropped, exits 0, and leaves the arena holding nothing.
// Without it every assertion here would still pass against a cmdRelease that
// refused unconditionally — and the arena would be stuck, since release is the
// only way a claim moves short of resolution (docs/cli.md § Releasing).
func TestCmdRelease_DropsTheClaimAndNamesTheItem(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	if _, err := be.Claim(context.Background(), itemRefFor("1"), nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	app, out, errBuf := releaseTestApp(t, be)

	if code := app.cmdRelease(context.Background(), nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if got, want := out.String(), "released 1\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	// Reported, and done: a command that printed the line without reaching the
	// backend leaves an arena that reads free and is not.
	active, err := be.LookupActiveClaim(context.Background())
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if active != nil {
		t.Errorf("active claim = %+v, want none — release reported a drop it did not make", active)
	}
	// And the arena is free for the next item, which is what the release was for.
	be.AddItem("2", flow.Item{Type: "task", Title: "2"})
	if _, err := be.Claim(context.Background(), itemRefFor("2"), nil); err != nil {
		t.Errorf("the arena is still occupied after a reported release: %v", err)
	}
}

// A refused release is rendered as a refusal, not as a bare error line: the
// operator needs the failing check and the verbatim output under it, because
// that is the whole of what tells them which files to deal with
// (docs/cli.md § Releasing).
func TestCmdRelease_RendersATypedRefusal(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	if _, err := inner.Claim(context.Background(), itemRefFor("1"), nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	be := &releaseRefusing{Orchestrator: inner, err: flow.ErrClaimRefused{
		Code:     "dirty-tree",
		Reason:   "worktree has uncommitted or untracked changes",
		Detail:   " M cli/cmd_release.go\n?? scratch.md",
		Check:    "clean-tree",
		Override: "force",
	}}
	app, _, errBuf := releaseTestApp(t, be)

	if code := app.cmdRelease(context.Background(), nil); code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	out := errBuf.String()
	if !strings.HasPrefix(out, "release: refused") {
		t.Errorf("output does not read as a refusal; got %q", out)
	}
	if !strings.Contains(out, `check "clean-tree"`) {
		t.Errorf("the failing check is missing; got %q", out)
	}
	if !strings.Contains(out, "\n   M cli/cmd_release.go") || !strings.Contains(out, "\n  ?? scratch.md") {
		t.Errorf("the detail is missing or not indented under the refusal; got %q", out)
	}
	// The release preconditions ARE overridable now, and the flag is the
	// operator's emergency (docs/cli.md § Releasing). Naming it is the point:
	// an operator whose arena is being decommissioned otherwise reads a refusal
	// with no way out and reaches for the hand-deletion instead.
	if !strings.Contains(out, "override with --force") {
		t.Errorf("the refusal does not name --force; got %q", out)
	}
}

// The state #212 is about, and the whole of what it asks for: a lease record
// this arena cannot parse leaves it with no way to say what it holds, and
// release is the command that has to get it out anyway. It goes ahead against
// a ref that names NOTHING, so the backend takes apart the local record alone.
func TestCmdRelease_ClearsALeaseRecordItCannotRead(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	if _, err := inner.Claim(context.Background(), itemRefFor("1"), nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	be := &unreadableLease{
		Orchestrator: inner,
		err:          errors.New(`parse /w/.flow/active.json: unexpected end of JSON input`),
	}
	app, out, errBuf := releaseTestApp(t, be)

	if code := app.cmdRelease(context.Background(), nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if !be.released {
		t.Fatal("Release was never reached — the command stopped on the parse error, which is the defect")
	}
	// A ref naming an item would be a guess: the record that would have said
	// which one is the record nothing can read.
	if len(be.releasedRef.Ref) != 0 {
		t.Errorf("released ref = %+v, want the zero ref", be.releasedRef)
	}
	// The operator is told both halves, because "released 1" here would be a
	// claim about the item that nothing checked.
	if !strings.Contains(out.String(), "unreadable lease record") {
		t.Errorf("stdout does not say what was cleared; got %q", out.String())
	}
	if !strings.Contains(out.String(), "NOT touched") {
		t.Errorf("stdout does not say the item's own record was left alone; got %q", out.String())
	}
	// The path is the only diagnosis available, so it still reaches the
	// operator — on stderr, where a report that is not the command's answer goes.
	if !strings.Contains(errBuf.String(), "active.json") {
		t.Errorf("the parse error naming the path was swallowed; got %q", errBuf.String())
	}
}

// "Cannot RIGHT NOW" is not "cannot be read", and the two are different answers
// a caller acts differently on (docs/orchestrator.md § Required does not mean
// always possible). An orchestrator whose lease ledger lives off-host answers
// ErrUnavailable while the service is down — it has NOT said this arena's record
// is unreadable, and the next attempt will name the item. Falling through to the
// zero ref there would ask the backend to take apart a lease nothing could name
// afterwards, on the strength of a 503.
func TestCmdRelease_ATransientLookupFailureStopsRatherThanClearing(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	if _, err := inner.Claim(context.Background(), itemRefFor("1"), nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	be := &unreadableLease{
		Orchestrator: inner,
		err:          fmt.Errorf("read the lease ledger: %w", flow.ErrUnavailable),
	}
	app, out, errBuf := releaseTestApp(t, be)

	if code := app.cmdRelease(context.Background(), nil); code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	if be.released {
		t.Errorf("Release was called with %+v — a service that will answer next time did not say the record is unreadable",
			be.releasedRef)
	}
	if out.String() != "" {
		t.Errorf("a stopped release reported a clearing it did not make; got %q", out.String())
	}
	// Named as the condition it is, so the operator retries rather than reading
	// it as a broken lease file.
	if !strings.Contains(errBuf.String(), "waiting on a condition") {
		t.Errorf("stderr does not name the condition; got %q", errBuf.String())
	}
}

// The recovery #212 exists to provide, end to end: an unreadable lease AND a
// tree that will not come clean — the arena being decommissioned, the machine
// that is not coming back. The bare form carries --force, so the zero ref and
// the three overrides travel together. Without this the flag reaches only the
// by-id form, and the state the issue is about keeps its one way out being a
// hand-deletion.
func TestCmdRelease_ForceReachesTheBareFormOnAnUnreadableLease(t *testing.T) {
	be := &unreadableLease{
		Orchestrator: fake.New(),
		err:          errors.New(`parse /w/.flow/active.json: unexpected end of JSON input`),
	}
	app, out, errBuf := releaseTestApp(t, be)

	if code := app.cmdRelease(context.Background(), []string{"--force"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if !be.released {
		t.Fatal("Release was never reached")
	}
	if len(be.releasedRef.Ref) != 0 {
		t.Errorf("released ref = %+v, want the zero ref — --force does not invent an item either",
			be.releasedRef)
	}
	want := []flow.ClaimOverride{flow.OverrideDirtyTree, flow.OverrideAlreadyHeld, flow.OverrideStaleBase}
	if !slices.Equal(be.releasedOverride, want) {
		t.Errorf("overrides = %v, want %v", be.releasedOverride, want)
	}
	if !strings.Contains(out.String(), "unreadable lease record") {
		t.Errorf("stdout does not say what was cleared; got %q", out.String())
	}
}

// An arena holding nothing has nothing to drop, and saying so is not the same
// answer as the unreadable record above: there the command goes ahead against a
// ref naming nothing, here it stops. A single `err != nil || claim == nil`
// branch would collapse the two and turn every empty arena into a clearing.
func TestCmdRelease_NoActiveClaimIsRefused(t *testing.T) {
	be := &recordingRelease{Orchestrator: fake.New()}
	app, out, errBuf := releaseTestApp(t, be)

	if code := app.cmdRelease(context.Background(), nil); code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	if be.called {
		t.Errorf("the backend was asked to release %+v; there was no claim to drop", be.ref)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want nothing: no clearing was made", out.String())
	}
	if !strings.Contains(errBuf.String(), "no active claim") {
		t.Errorf("stderr = %q, want 'no active claim'", errBuf.String())
	}
}

// An unreadable lease does not suspend the preconditions. The tree may hold
// work belonging to whatever the record named, and a release that cannot say
// which item it was leaves even less to attribute it to — so the refusal stands
// and --force is what carries it.
func TestCmdRelease_AnUnreadableLeaseIsStillRefusedOnADirtyTree(t *testing.T) {
	inner := fake.New()
	be := &releaseRefusingWithUnreadableLease{
		unreadableLease: unreadableLease{
			Orchestrator: inner,
			err:          errors.New("parse active.json: unexpected end of JSON input"),
		},
		refusal: flow.ErrClaimRefused{
			Code:     "dirty-tree",
			Reason:   "worktree has uncommitted or untracked changes",
			Check:    "clean-tree",
			Override: "force",
		},
	}
	app, out, errBuf := releaseTestApp(t, be)

	if code := app.cmdRelease(context.Background(), nil); code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	if out.String() != "" {
		t.Errorf("a refused release reported a clearing it did not make; got %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "override with --force") {
		t.Errorf("the way out is not named; got %q", errBuf.String())
	}
}

type releaseRefusingWithUnreadableLease struct {
	unreadableLease
	refusal flow.ErrClaimRefused
}

func (b *releaseRefusingWithUnreadableLease) Release(context.Context, flow.ItemRef, []flow.ClaimOverride) error {
	return b.refusal
}

// The second shape: a claim record that lives on the item and not in this
// arena's lease file. It is addressed by id, the way `claim <id>` and
// `status <id>` address theirs.
func TestCmdRelease_ByItemIdReleasesTheNamedItem(t *testing.T) {
	be := fake.New()
	be.AddItem("7", flow.Item{Type: "task", Title: "7"})
	if _, err := be.Claim(context.Background(), itemRefFor("7"), nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	app, out, errBuf := releaseTestApp(t, be)

	if code := app.cmdRelease(context.Background(), []string{"7"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if got, want := out.String(), "released 7\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if info, err := be.LookupClaim(context.Background(), itemRefFor("7")); err != nil || info != nil {
		t.Errorf("LookupClaim = (%+v, %v), want unheld — the named claim was not dropped", info, err)
	}
}

// --force is one thing, and it is the same three overrides `claim --force`
// builds. A release that overrode a different set from the claim which must
// follow it would hand on a tree that claim then refuses.
func TestCmdRelease_ForceCarriesTheSameOverridesAsClaim(t *testing.T) {
	inner := fake.New()
	inner.AddItem("7", flow.Item{Type: "task", Title: "7"})
	if _, err := inner.Claim(context.Background(), itemRefFor("7"), nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	be := &recordingRelease{Orchestrator: inner}
	app, _, errBuf := releaseTestApp(t, be)

	if code := app.cmdRelease(context.Background(), []string{"7", "--force"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	want := []flow.ClaimOverride{flow.OverrideDirtyTree, flow.OverrideAlreadyHeld, flow.OverrideStaleBase}
	if !slices.Equal(be.overrides, want) {
		t.Errorf("overrides = %v, want %v", be.overrides, want)
	}
	if be.ref.Display != "7" {
		t.Errorf("released ref = %q, want 7", be.ref.Display)
	}
}

// Without --force nothing is bypassed, and the absence has to be visible: an
// override list built unconditionally would make every release a forced one.
func TestCmdRelease_WithoutForcePassesNoOverrides(t *testing.T) {
	inner := fake.New()
	inner.AddItem("7", flow.Item{Type: "task", Title: "7"})
	if _, err := inner.Claim(context.Background(), itemRefFor("7"), nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	be := &recordingRelease{Orchestrator: inner}
	app, _, errBuf := releaseTestApp(t, be)

	if code := app.cmdRelease(context.Background(), []string{"7"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if len(be.overrides) != 0 {
		t.Errorf("overrides = %v, want none", be.overrides)
	}
}

// refusingRefRecordingRelease reuses the id resolver that already refuses
// (refusingRefBackend, cmd_claim_test.go) and only adds the record of whether
// Release was reached.
type refusingRefRecordingRelease struct {
	refusingRefBackend
	released bool
}

func (b *refusingRefRecordingRelease) Release(ctx context.Context, ref flow.ItemRef, ov []flow.ClaimOverride) error {
	b.released = true
	return b.refusingRefBackend.Release(ctx, ref, ov)
}

// An id that does not resolve is a plain failure, not a refusal: nothing was
// refused, the command could not find out what was being asked about. And it
// must not fall through to the zero ref, which would silently turn "release
// this item" into "clear this arena's record".
func TestCmdRelease_AnUnresolvableIdIsAPlainFailure(t *testing.T) {
	be := &refusingRefRecordingRelease{refusingRefBackend: refusingRefBackend{fake.New()}}
	app, _, errBuf := releaseTestApp(t, be)

	if code := app.cmdRelease(context.Background(), []string{"nope"}); code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	if be.released {
		t.Error("the backend was asked to release a ref that never resolved")
	}
	if strings.Contains(errBuf.String(), "refused") {
		t.Errorf("an unresolvable id was dressed as a refusal; got %q", errBuf.String())
	}
}

// The other half of the binding, end to end: an arena that holds one item
// cannot be sent at a second. The run stops before it dispatches anything, it
// names the item in the way, and the standing claim is untouched — an operator
// whose second `resolve` had quietly displaced the first would have orphaned
// the work in this worktree without being told.
func TestCmdResolve_RefusesASecondItemWhileTheArenaHoldsOne(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be.AddItem("2", flow.Item{Type: "task", Title: "2"})
	if _, err := be.Claim(context.Background(), itemRefFor("1"), nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	app, _, errBuf := resolveTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"2"}); code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	out := errBuf.String()
	if !strings.Contains(out, "refused") {
		t.Errorf("output does not read as a refusal; got %q", out)
	}
	if !strings.Contains(out, "1") || !strings.Contains(out, "release") {
		t.Errorf("the refusal must name the held item and the way out; got %q", out)
	}

	active, err := be.LookupActiveClaim(context.Background())
	if err != nil || active == nil {
		t.Fatalf("active claim = (%+v, %v), want item 1 still held", active, err)
	}
	if active.ItemRef.Display != "1" {
		t.Errorf("active claim = %q, want 1 — the refused resolve moved the lease", active.ItemRef.Display)
	}
}

// An untyped failure keeps the plain line: only a refusal is rendered as one.
func TestCmdRelease_AnUntypedFailureKeepsThePlainLine(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	if _, err := inner.Claim(context.Background(), itemRefFor("1"), nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	be := &releaseRefusing{Orchestrator: inner, err: errors.New("the lease label would not come off")}
	app, _, errBuf := releaseTestApp(t, be)

	if code := app.cmdRelease(context.Background(), nil); code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	out := errBuf.String()
	if strings.Contains(out, "refused") {
		t.Errorf("an ordinary failure was dressed as a refusal; got %q", out)
	}
	if !strings.Contains(out, "the lease label would not come off") {
		t.Errorf("the backend's own account of the failure is missing; got %q", out)
	}
}

// The backend's answer for an item that carries no claim record has to reach
// the operator as a refusal, not as the "released <id>" line. `release
// <item-id>` accepts any number, so this is what a mistyped id gets — and a
// command that printed a release it did not make would leave the operator
// believing a record was dropped that is still sitting on the item.
func TestCmdRelease_NotClaimedIsRenderedAsARefusalWithNoOverride(t *testing.T) {
	inner := fake.New()
	inner.AddItem("300", flow.Item{Type: "task", Title: "300"})
	be := &releaseRefusing{Orchestrator: inner, err: flow.ErrClaimRefused{
		Code:       "not-claimed",
		ItemScoped: true,
		Reason: "issue #300 carries no claim record, and this arena's lease does not name it — " +
			"there is nothing to release",
	}}
	app, out, errBuf := releaseTestApp(t, be)

	if code := app.cmdRelease(context.Background(), []string{"300"}); code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want nothing — no release was made", out.String())
	}
	if !strings.Contains(errBuf.String(), "nothing to release") {
		t.Errorf("stderr does not say what happened; got %q", errBuf.String())
	}
	// Nothing is being withheld, so nothing is offered: an override line here
	// would point at a flag that reaches no record.
	if strings.Contains(errBuf.String(), "override with") {
		t.Errorf("offered an override for an item with no record; got %q", errBuf.String())
	}
}
