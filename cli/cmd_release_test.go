package cli

import (
	"bytes"
	"context"
	"errors"
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

func (b *releaseRefusing) Release(context.Context, flow.ItemRef) error { return b.err }

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
		Code:   "dirty-tree",
		Reason: "worktree has uncommitted or untracked changes",
		Detail: " M cli/cmd_release.go\n?? scratch.md",
		Check:  "clean-tree",
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
	// Nothing bypasses a release precondition, so there is no override to
	// offer — and offering one would point the operator at a flag that does
	// not exist.
	if strings.Contains(out, "override with") {
		t.Errorf("offered an override for a release; got %q", out)
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
