package cli

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// The role predicate the CLI hands the orchestrator is derived in ONE place
// (App.assumesRole): the account's detected capabilities decide which of the
// flow's declared roles it could assume at all. These pin what that predicate
// answers and that it actually reaches the orchestrator.

// recordingRoleBackend captures the predicate every listing call was handed, so
// a test can assert both that one arrived and what it answers.
type recordingRoleBackend struct {
	*fake.Orchestrator
	listRole  func(flow.RoleName) bool
	listSeen  bool
	autoRole  func(flow.RoleName) bool
	autoSeen  bool
	autoRefs  []flow.ItemRef
	detectErr error
}

func (b *recordingRoleBackend) List(ctx context.Context, scope flow.ItemScope, binary flow.BinaryName,
	acceptsType func(flow.ItemType) bool, assumesRole func(flow.RoleName) bool) ([]flow.ItemInfo, error) {
	b.listRole, b.listSeen = assumesRole, true
	return b.Orchestrator.List(ctx, scope, binary, acceptsType, assumesRole)
}

func (b *recordingRoleBackend) ListAutoSelectable(ctx context.Context, tags []flow.TagId,
	assumesRole func(flow.RoleName) bool) ([]flow.ItemRef, error) {
	b.autoRole, b.autoSeen = assumesRole, true
	if b.autoRefs != nil {
		return b.autoRefs, nil
	}
	return b.Orchestrator.ListAutoSelectable(ctx, tags, assumesRole)
}

func (b *recordingRoleBackend) DetectCapabilities(ctx context.Context, account flow.AccountId) ([]flow.Capability, error) {
	if b.detectErr != nil {
		return nil, b.detectErr
	}
	return b.Orchestrator.DetectCapabilities(ctx, account)
}

// roleApp builds an app over a two-role flow whose account holds only `push`,
// so `contributor` is assumable and `maintainer` is not.
func roleApp(t *testing.T) (*App, *recordingRoleBackend) {
	t.Helper()
	app, be, _ := testApp(t, func(f *flow.Flow) {
		// `contributor` comes from testApp; `maintainer` is the second role this
		// test needs, and the one no account here can assume.
		f.Role("maintainer", flow.CapMerge)
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Entry: true, Role: "contributor", MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, &stubAgent{name: "stub"})
	be.SetCapabilities("", flow.CapPush)
	rec := &recordingRoleBackend{Orchestrator: be}
	app.Orchestrator = rec
	app.Out, app.Err = &bytes.Buffer{}, &bytes.Buffer{}
	return app, rec
}

// Capability is the CEILING: a role whose declared capabilities the account
// holds is assumable, and one it does not is not.
func TestAssumesRole_CapabilityIsTheCeiling(t *testing.T) {
	app, _ := roleApp(t)
	pred := app.assumesRole(context.Background())
	if pred == nil {
		t.Fatal("assumesRole = nil though the orchestrator answered the capability question")
	}
	if !pred("contributor") {
		t.Error("contributor is not assumable, but the account holds push")
	}
	if pred("maintainer") {
		t.Error("maintainer is assumable, but the account does not hold merge")
	}
	// A role the flow never declared is not assumable: the predicate answers
	// from the declared set, and an undeclared name is in no one's remit.
	if pred("nobody") {
		t.Error("an undeclared role reported assumable")
	}
}

// An orchestrator that cannot answer the capability question yields NIL, which
// filters nothing — the acceptsType convention. Filtering on a ceiling nobody
// could measure would hide every item behind a backend that cannot ask.
func TestAssumesRole_NilWhenCapabilitiesCannotBeDetected(t *testing.T) {
	app, be := roleApp(t)
	be.detectErr = errors.New("no network")
	if pred := app.assumesRole(context.Background()); pred != nil {
		t.Error("assumesRole returned a predicate though the capability question could not be answered")
	}
}

// `list` hands the predicate through to the orchestrator: the CLI derives it,
// the orchestrator applies it, and a listing that dropped it would rank an
// item nobody here can move as available.
func TestCmdList_PassesTheRolePredicateToTheOrchestrator(t *testing.T) {
	app, be := roleApp(t)
	if code := app.cmdList(context.Background(), nil); code != 0 {
		t.Fatalf("cmdList = %d; stderr=%q", code, app.Err.(*bytes.Buffer).String())
	}
	if !be.listSeen {
		t.Fatal("List was never called")
	}
	if be.listRole == nil {
		t.Fatal("List was handed a nil role predicate though capabilities were detectable")
	}
	if !be.listRole("contributor") || be.listRole("maintainer") {
		t.Error("the predicate List received is not the one assumesRole derives")
	}
}

// `resolve` hands the same predicate to the selection, so an item awaiting a
// role this account cannot assume is never picked up unattended.
func TestCmdResolve_PassesTheRolePredicateToTheSelection(t *testing.T) {
	app, be := roleApp(t)
	ctx := context.Background()
	// Auto-selection runs only when the arena holds no claim, so release the
	// one testApp took.
	claim, err := be.LookupActiveClaim(ctx)
	if err != nil || claim == nil {
		t.Fatalf("LookupActiveClaim = (%+v, %v), want the claim testApp took", claim, err)
	}
	if err := be.Release(ctx, claim.ItemRef); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Nothing selectable: the command stops early, and the assertion is about
	// what the selection was asked, not what it returned.
	be.autoRefs = []flow.ItemRef{}
	_ = app.cmdResolve(ctx, nil)
	if !be.autoSeen {
		t.Fatal("ListAutoSelectable was never called")
	}
	if be.autoRole == nil {
		t.Fatal("ListAutoSelectable was handed a nil role predicate")
	}
	if !be.autoRole("contributor") || be.autoRole("maintainer") {
		t.Error("the predicate the selection received is not the one assumesRole derives")
	}
}

// End to end through the fake: an item awaiting a role this account cannot
// assume sits at `awaits` and is absent from the auto-selectable set.
func TestSelection_AnAwaitsItemIsNeverAutoSelected(t *testing.T) {
	app, be := roleApp(t)
	ctx := context.Background()

	claim, err := be.LookupActiveClaim(ctx)
	if err != nil || claim == nil {
		t.Fatalf("LookupActiveClaim = (%+v, %v), want the claim testApp took", claim, err)
	}
	e := resultEntry("plan", "next", 1, flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the plan"})
	e.Awaits = flow.Awaits{Role: "maintainer"}
	if err := be.AppendEntry(ctx, claim.ItemRef, e); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}

	pred := app.assumesRole(ctx)
	info, err := be.Get(ctx, claim.ItemRef, flow.BinaryName(app.Name), app.Flow.InRemit, pred)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Availability != flow.AvailAwaits {
		t.Errorf("Availability = %q, want awaits", info.Availability)
	}

	refs, err := be.ListAutoSelectable(ctx, nil, pred)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	if slices.ContainsFunc(refs, func(r flow.ItemRef) bool { return r.Display == claim.ItemRef.Display }) {
		t.Errorf("ListAutoSelectable = %+v, want the awaits item ABSENT — eligibility, not a sort key", refs)
	}
}
