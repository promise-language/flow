package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// handoffTestApp builds the two-role fixture the handoff tests turn: a
// contributor step that elects a maintainer step, with BOTH roles covered so
// that what the arena's account can do is the lever — push-only ends the run
// at the boundary, push+merge crosses it without stopping (docs/resolution.md
// § One principal, several roles). handoffTestAppCovering is the same fixture
// with the coverage chosen, for a binary declining a role its account backs.
func handoffTestApp(t *testing.T, be flow.Orchestrator) (*App, *bytes.Buffer) {
	t.Helper()
	return handoffTestAppCovering(t, be, "contributor", "maintainer")
}

func handoffTestAppCovering(t *testing.T, be flow.Orchestrator, covered ...flow.RoleName) (*App, *bytes.Buffer) {
	t.Helper()
	app, _, errBuf := resolveTestAppCovering(t, be, covered, func(f *flow.Flow) {
		f.Role("maintainer", flow.CapMerge)
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Next("commit", "planned").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Entry: true, Role: "contributor", Next: []flow.StepId{"commit"}})
		f.AddStep("close branch", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "landed").CommitHash("abc"), nil
		}, flow.StepConfig{Prompts: flow.PromptsNone, Role: "maintainer", MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})
	return app, errBuf
}

// crossingLine is what a run says before it crosses a boundary it performed
// the other side of: the two roles, the account, and what carrying through
// does not provide (docs/cli.md § The announcement names the run's standing).
const crossingLine = "resolve: crossing from contributor into maintainer — fake-account performed the contributor's part, so this is not independent review"

// Coverage is the choice within the ceiling: a binary declining a role its
// account could back hands off at the boundary exactly as one whose account
// lacks the merge does. The ceiling alone would carry every maintainer-capable
// operator through with no way to decline (docs/resolution-standalone.md
// § Declaring what a binary may do).
func TestCmdResolve_ADeclinedRoleHandsOffWhateverTheAccountCanDo(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be.SetCapabilities("", flow.CapPush, flow.CapMerge) // the account could merge
	app, errBuf := handoffTestAppCovering(t, be, "contributor")

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 — a handoff is a clean end; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "handed off — awaits maintainer") {
		t.Errorf("a declined role was not handed off; got %q", errBuf.String())
	}
	if strings.Contains(errBuf.String(), `running "close branch"…`) {
		t.Errorf("the declined role's step was dispatched; got %q", errBuf.String())
	}
	// The standing names what this run may assume — the covered role the
	// account backs — not everything the account could do.
	if !strings.Contains(errBuf.String(), "roles it can assume: contributor\n") {
		t.Errorf("the announcement does not name the covered role alone; got %q", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "crossing") {
		t.Errorf("a handoff is not a crossing; got %q", errBuf.String())
	}
	item, err := be.Load(context.Background(), be.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !item.Holder.Empty() {
		t.Errorf("the claim is still held by %+v after the handoff", item.Holder)
	}
}

// Undetectable capabilities filter nothing WITHIN coverage; they do not widen
// it. A binary that declined the maintainer's role still hands off at the
// boundary when the orchestrator cannot say what the account holds, because
// coverage is configuration and was never in question.
func TestCmdResolve_UndetectableCapabilitiesStillHandOffADeclinedRole(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &undetectableCapabilities{Orchestrator: inner, err: errors.New("the forge will not say")}
	app, errBuf := handoffTestAppCovering(t, be, "contributor")

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "handed off — awaits maintainer") {
		t.Errorf("an uncovered role was not handed off on unknown capabilities; got %q", errBuf.String())
	}
	if strings.Contains(errBuf.String(), `running "close branch"…`) {
		t.Errorf("the declined role's step was dispatched; got %q", errBuf.String())
	}
}

// A run about to cross into the maintainer's role, having performed the
// contributor's part itself, says so before it crosses — after the
// contributor's last step reports and before the maintainer's first is
// announced, which is the moment someone could still choose otherwise. Once,
// not on every later maintainer step: the crossing is the boundary, and the
// steps behind it are the maintainer's like any other.
func TestCmdResolve_ACrossingIsAnnouncedBeforeTheMaintainersFirstStep(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be.SetCapabilities("", flow.CapPush, flow.CapMerge)
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	got := errBuf.String()
	if n := strings.Count(got, crossingLine); n != 1 {
		t.Fatalf("the crossing line was printed %d time(s), want exactly 1; got %q", n, got)
	}
	crossing := strings.Index(got, crossingLine)
	planned := strings.Index(got, "resolve: plan → done")
	running := strings.Index(got, `running "close branch"…`)
	if planned < 0 || running < 0 {
		t.Fatalf("expected the contributor's outcome and the maintainer's dispatch to be narrated; got %q", got)
	}
	if crossing < planned {
		t.Errorf("the crossing was announced before the contributor's step finished — nothing had been crossed yet; got %q", got)
	}
	if crossing > running {
		t.Errorf("the crossing was announced after the maintainer's step was dispatched; got %q", got)
	}
	// Announced before any first dispatch it is not: a fresh item has no side
	// to have performed.
	if first := strings.Index(got, `running "write plan"…`); crossing < first {
		t.Errorf("a fresh item announced a crossing; got %q", got)
	}
}

// A run that never leaves its role crosses nothing, and says nothing.
func TestCmdResolve_NoCrossingIsAnnouncedWithinOneRole(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, _, errBuf := resolveTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "crossing") {
		t.Errorf("a single-role run announced a crossing; got %q", errBuf.String())
	}
}

// Picking up another account's proposal is a handoff being completed, not a
// crossing: the maintainer's review IS independent, and telling that operator
// otherwise is false in the direction that matters.
func TestCmdResolve_PickingUpAnotherAccountsProposalIsNotACrossing(t *testing.T) {
	be := fake.New()
	be.AddItem("1", awaitingMaintainer()) // the contributor's part was bob's
	be.SetCapabilities("", flow.CapPush, flow.CapMerge)
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "crossing") {
		t.Errorf("another account's proposal was reported as this run's own crossing; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), `running "close branch"…`) {
		t.Errorf("the maintainer's step never ran; got %q", errBuf.String())
	}
}

// The crossing is read off the JOURNAL, not off what this process has done so
// far: an item a contributor-only binary took to the proposal, picked up now by
// a run covering both roles on the same account, is one principal on both
// sides just as much as a single run through both — and the line is owed
// before the maintainer's first step either way. Keyed on this run's own
// dispatches instead, re-running with wider coverage would review one's own
// proposal with no word said.
func TestCmdResolve_ResumingOnesOwnProposalAcrossTheBoundaryIsACrossing(t *testing.T) {
	be := fake.New()
	mine := awaitingMaintainer()
	mine.Journal[0].By = "fake-account" // the contributor's part was this account's, in an earlier run
	be.AddItem("1", mine)
	be.SetCapabilities("", flow.CapPush, flow.CapMerge)
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	got := errBuf.String()
	if n := strings.Count(got, crossingLine); n != 1 {
		t.Fatalf("the crossing line was printed %d time(s), want exactly 1; got %q", n, got)
	}
	running := strings.Index(got, `running "close branch"…`)
	if running < 0 {
		t.Fatalf("the maintainer's step never ran; got %q", got)
	}
	if strings.Index(got, crossingLine) > running {
		t.Errorf("the crossing was announced after the maintainer's step was dispatched; got %q", got)
	}
}

// A rework handback returns to a role the account already held on this item,
// and nothing is crossed: the account is back where it was, and it was told
// about the arrangement when it first crossed.
func TestCmdResolve_AReworkHandbackIsNotACrossing(t *testing.T) {
	be := fake.New()
	// One account performed both sides already: the contributor's plan, then
	// the maintainer's review, which handed the item back for rework.
	be.AddItem("1", flow.Item{Type: "task", Title: "1",
		Awaits: flow.Awaits{Role: "contributor", Account: "fake-account"},
		Journal: []flow.JournalEntry{
			{Step: "plan", Execution: 1, Route: flow.Route{Next: "commit"},
				By: "fake-account", Role: "contributor", Awaits: flow.Awaits{Role: "maintainer"}},
			{Step: "commit", Execution: 1, Route: flow.Route{Next: "plan"},
				By: "fake-account", Role: "maintainer", Awaits: flow.Awaits{Role: "contributor"}},
		}})
	be.SetCapabilities("", flow.CapPush, flow.CapMerge)
	app, _, errBuf := resolveTestAppFlow(t, be, func(f *flow.Flow) {
		f.Role("maintainer", flow.CapMerge)
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Next("commit", "reworked").Markdown("the plan, again"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Entry: true, Role: "contributor", Next: []flow.StepId{"commit"}})
		f.AddStep("close branch", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "landed").CommitHash("abc"), nil
		}, flow.StepConfig{Prompts: flow.PromptsNone, Role: "maintainer", Next: []flow.StepId{"plan"},
			MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	// Neither the handback into the contributor's role nor the return into the
	// maintainer's is a crossing: the account held both already.
	if strings.Contains(errBuf.String(), "crossing") {
		t.Errorf("a rework handback was reported as a crossing; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), `running "close branch"…`) {
		t.Errorf("the run did not return to the maintainer's step; got %q", errBuf.String())
	}
}

// An orchestrator that records no account gives the line nothing to say:
// carrying through is defined by what the journal shows, and a journal that
// names nobody shows no one principal on both sides.
func TestCmdResolve_NoCrossingIsAnnouncedWithoutAnAccountOfRecord(t *testing.T) {
	be := fake.New()
	be.SetAccount("")
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be.SetCapabilities("", flow.CapPush, flow.CapMerge)
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "crossing") {
		t.Errorf("a crossing was announced with no account to attribute either side to; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), `running "close branch"…`) {
		t.Errorf("the maintainer's step never ran; got %q", errBuf.String())
	}
}

// A step whose role this account cannot assume is a HANDOFF, not the end of the
// work: the report names who moves next, the claim goes back, and nothing
// claims the item was finalized. Falling through to the no-eligible-step branch
// reported the opposite of what happened and left the item held by an arena
// that was done with it.
func TestCmdResolve_ARoleBoundaryEndsTheRunAsAHandoff(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be.SetCapabilities("", flow.CapPush) // no merge: the maintainer's part is not this run's
	app, errBuf := handoffTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 — a handoff is a clean end; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "handed off — awaits maintainer") {
		t.Errorf("the report does not name the role the item now awaits; got %q", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "finalized ✓") {
		t.Errorf("a handoff is not a finalization; got %q", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "no step eligible") {
		t.Errorf("a step somebody else must run is not 'no step eligible'; got %q", errBuf.String())
	}
	// The claim is the promise: an item held by an arena that has finished with
	// it is one the next role cannot pick up.
	item, err := be.Load(context.Background(), be.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !item.Holder.Empty() {
		t.Errorf("the claim is still held by %+v after the handoff", item.Holder)
	}
	if item.Finalized {
		t.Error("the item reads finalized after a handoff")
	}
	// The same facts the announcement opened with, reported again: a handoff
	// that named neither the account nor its roles would report a stop without
	// reporting whose move it now is.
	if n := strings.Count(errBuf.String(), "acting as fake-account — roles it can assume: contributor"); n != 2 {
		t.Errorf("the standing was printed %d times, want 2 (the announcement and the handoff); got %q", n, errBuf.String())
	}
}

// The counterpart: an account backing BOTH roles crosses the boundary in one
// resolution. Without it the test above would pass on a `resolve` that handed
// every item off.
func TestCmdResolve_APrincipalCoveringBothRolesCrossesTheBoundary(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be.SetCapabilities("", flow.CapPush, flow.CapMerge)
	app, errBuf := handoffTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "handed off") {
		t.Errorf("an account backing both roles hands nothing off; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), `running "close branch"…`) {
		t.Errorf("the maintainer's step never ran; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "roles it can assume: contributor, maintainer") {
		t.Errorf("the announcement does not name both roles; got %q", errBuf.String())
	}
}

// A route that returns to a role returns to its ACCOUNT OF RECORD, and the
// handoff names it: an operator reading the report is being told who to expect,
// not only what.
func TestCmdResolve_AHandoffNamesTheAccountOfRecord(t *testing.T) {
	be := fake.New()
	// The maintainer has acted before and handed the item back for rework, so
	// the role is bound to an account. The contributor's step below returns it.
	be.AddItem("1", flow.Item{Type: "task", Title: "1",
		Awaits: flow.Awaits{Role: "contributor"},
		Journal: []flow.JournalEntry{{
			Step: "commit", Execution: 1, Route: flow.Route{Next: "plan"},
			By: "alice", Role: "maintainer", Awaits: flow.Awaits{Role: "contributor"},
		}}})
	be.SetCapabilities("", flow.CapPush)
	app, errBuf := handoffTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "handed off — awaits maintainer, account of record alice") {
		t.Errorf("the handoff does not name the role's account of record; got %q", errBuf.String())
	}
}

// An item already awaiting a role this run cannot assume hands off on the first
// pass: nothing is dispatched, and the claim taken to look at it goes back.
func TestCmdResolve_AnItemAlreadyAwaitingAnotherRoleHandsOffImmediately(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1",
		Awaits: flow.Awaits{Role: "maintainer"},
		Journal: []flow.JournalEntry{{
			Step: "plan", Execution: 1, Route: flow.Route{Next: "commit"},
			By: "bob", Role: "contributor", Awaits: flow.Awaits{Role: "maintainer"},
		}}})
	be.SetCapabilities("", flow.CapPush)
	app, errBuf := handoffTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if strings.Contains(errBuf.String(), `running "close branch"…`) {
		t.Errorf("a step the run cannot take was dispatched anyway; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "handed off — awaits maintainer") {
		t.Errorf("expected an immediate handoff; got %q", errBuf.String())
	}
	item, err := be.Load(context.Background(), be.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !item.Holder.Empty() {
		t.Errorf("the claim is still held by %+v after the handoff", item.Holder)
	}
}

// A recorded awaited role the flow does not declare matches nothing and never
// will. Read as "the runner has not arrived" it would leave the item
// unofferable with nothing naming why, so it reports the item BLOCKED, names
// the role and the declared set, and keeps the claim (docs/resolution.md
// § Whose move it is).
func TestCmdResolve_AnUndeclaredAwaitedRoleIsBlocked(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1", Awaits: flow.Awaits{Role: "reviewer"}})
	be.SetCapabilities("", flow.CapPush)
	app, errBuf := handoffTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 — a condition a person must clear; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), `role "reviewer" is not declared by this flow`) {
		t.Errorf("the refusal does not name the unknown role; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "declared roles are [contributor maintainer]") {
		t.Errorf("the refusal does not name the declared set; got %q", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "handed off") {
		t.Errorf("an undeclared role is not somebody else's move; got %q", errBuf.String())
	}
	item, err := be.Load(context.Background(), be.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if item.Holder.Empty() {
		t.Error("the claim was released on a refusal a person must clear")
	}
}

// An awaited SIGNAL is nobody's move, so it is not a handoff: the run goes on
// to the advance, which reports the wait. Handing off on one would release the
// claim and name a role that is not waiting for anything.
func TestCmdResolve_AnAwaitedSignalIsNotAHandoff(t *testing.T) {
	be := fake.New(flow.Signal("pr-open", "the pull request is open"))
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be.SetCapabilities("", flow.CapPush)
	app, _, errBuf := resolveTestAppFlow(t, be, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Next("pr-open", "planned").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Entry: true, Role: "contributor", Next: []flow.StepId{"pr-open"}})
		f.AwaitSignal("await the pull request", "pr-open", flow.StepConfig{Next: []flow.StepId{"commit"}})
		f.AddStep("close branch", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "landed").CommitHash("abc"), nil
		}, flow.StepConfig{Prompts: flow.PromptsNone, Role: "contributor", MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})

	code := app.cmdResolve(context.Background(), []string{"1"})
	if strings.Contains(errBuf.String(), "handed off") {
		t.Fatalf("an unset signal was reported as a handoff; got %q", errBuf.String())
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (the wait is reported, not handed off); err=%q", code, errBuf.String())
	}
	// The run did reach the wait, rather than passing this test by stopping
	// somewhere earlier.
	if !strings.Contains(errBuf.String(), `running "await the pull request"…`) {
		t.Fatalf("the run never reached the wait; got %q", errBuf.String())
	}
	item, err := be.Load(context.Background(), be.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if item.Holder.Empty() {
		t.Error("the claim was released on a wait: work in progress keeps its claim")
	}
}

// undetectableCapabilities refuses the capability question, as a backend that
// cannot ask it does.
type undetectableCapabilities struct {
	*fake.Orchestrator
	err error
}

func (b *undetectableCapabilities) DetectCapabilities(context.Context, flow.AccountId) ([]flow.Capability, error) {
	return nil, b.err
}

// Capabilities that could not be DETECTED are not capabilities the account
// lacks. Handing an item off on an unanswered question would end the run on the
// strength of a fact nobody established — the same convention auto-selection's
// nil predicate already carries.
func TestCmdResolve_UndetectableCapabilitiesHandNothingOff(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &undetectableCapabilities{Orchestrator: inner, err: errors.New("the forge will not say")}
	app, errBuf := handoffTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "handed off") {
		t.Errorf("handed off on a capability question nobody answered; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "roles it can assume: unknown") {
		t.Errorf("the announcement must say the roles are unknown, not that there are none; got %q", errBuf.String())
	}
}

// refusingRelease lets everything else through but fails the release.
type refusingRelease struct {
	*fake.Orchestrator
	err error
}

func (b *refusingRelease) Release(context.Context, flow.ItemRef) error { return b.err }

// Dropping the claim is the promise a handoff makes. A release that fails stops
// the run at exit 1 rather than reporting a handoff that did not happen.
func TestCmdResolve_AHandoffThatCannotReleaseTheClaimFails(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	inner.SetCapabilities("", flow.CapPush)
	be := &refusingRelease{Orchestrator: inner, err: errors.New("the lease label would not come off")}
	app, errBuf := handoffTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 — the claim is still held; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "the claim could not be released") {
		t.Errorf("the failure is not reported; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "the lease label would not come off") {
		t.Errorf("the orchestrator's own account of the failure is missing; got %q", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "handed off") {
		t.Errorf("reported a handoff that did not happen; got %q", errBuf.String())
	}
}

// The announcement comes BEFORE the first dispatch — an operator learns how far
// a run can take an item before it spends anything, rather than from where it
// stops.
func TestCmdResolve_TheStandingIsAnnouncedBeforeTheFirstDispatch(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, _, errBuf := resolveTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	acting := strings.Index(errBuf.String(), "acting as fake-account")
	running := strings.Index(errBuf.String(), `running "write plan"…`)
	if acting < 0 || running < 0 {
		t.Fatalf("expected both the standing and the first step to be narrated; got %q", errBuf.String())
	}
	if acting > running {
		t.Errorf("the standing was announced after the first dispatch; got %q", errBuf.String())
	}
}

// …and before the first pacing WAIT too. A run that announces its standing only
// after waiting hours for quota headroom has told the operator nothing they
// could act on.
func TestCmdResolve_TheStandingIsAnnouncedBeforeTheFirstPacingWait(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, _, errBuf := resolveTestApp(t, be)
	app.Quota = func() ([]windowUsage, error) {
		return []windowUsage{{
			Label: "5h", Length: time.Second, Used: 1.0, ResetsAt: time.Now().Add(30 * time.Millisecond),
		}}, nil
	}

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	acting := strings.Index(errBuf.String(), "acting as fake-account")
	pacing := strings.Index(errBuf.String(), "resolve: pacing —")
	if acting < 0 || pacing < 0 {
		t.Fatalf("expected both the standing and a pacing wait; got %q", errBuf.String())
	}
	if acting > pacing {
		t.Errorf("the standing was announced after the first pacing wait; got %q", errBuf.String())
	}
}

// An account backing no declared role is a different answer from an account
// nothing could be detected about, and it reads differently.
func TestCmdResolve_AnAccountBackingNoRoleSaysNone(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be.SetCapabilities("") // detected, and it can do nothing here
	app, errBuf := handoffTestApp(t, be)

	app.cmdResolve(context.Background(), []string{"1"})
	if !strings.Contains(errBuf.String(), "roles it can assume: none") {
		t.Errorf("expected the empty set to read as none; got %q", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "roles it can assume: unknown") {
		t.Errorf("an answered question must not read as an unanswered one; got %q", errBuf.String())
	}
}

// The run is about to spend on text somebody else wrote, and that is the one
// fact about an item's origin a title cannot carry.
func TestCmdResolve_TheAnnouncementNamesTheFilerWhenItIsNotTheAccountActing(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1", Creator: "carol"})
	app, _, errBuf := resolveTestApp(t, be)

	app.cmdResolve(context.Background(), []string{"1"})
	if !strings.Contains(errBuf.String(), "resolve: filed by carol") {
		t.Errorf("the announcement does not name the filing account; got %q", errBuf.String())
	}
}

// A run against one's own item is not told who filed it: the line exists to say
// that the author is somebody else.
func TestCmdResolve_TheAnnouncementOmitsTheFilerWhenItIsTheAccountActing(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1", Creator: "fake-account"})
	app, _, errBuf := resolveTestApp(t, be)

	app.cmdResolve(context.Background(), []string{"1"})
	if strings.Contains(errBuf.String(), "filed by") {
		t.Errorf("naming the filer of one's own item says nothing new; got %q", errBuf.String())
	}
}

// loadFailsOnce fails the Nth Load, so the announcement's best-effort read can
// be made to fail without breaking the advance that follows it.
type loadFailsOnce struct {
	*fake.Orchestrator
	calls int
	err   error
}

func (b *loadFailsOnce) Load(ctx context.Context, ref flow.ItemRef) (*flow.Item, error) {
	b.calls++
	if b.calls == 1 {
		return nil, b.err
	}
	return b.Orchestrator.Load(ctx, ref)
}

// The filer costs one best-effort read. A read that fails drops THAT line and
// keeps the others: the account and its roles are what the run's reach follows
// from, and trading the whole announcement for part of it would withhold them
// over an unrelated failure.
func TestCmdResolve_AnUnreadableItemStillAnnouncesTheAccountAndItsRoles(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1", Creator: "carol"})
	be := &loadFailsOnce{Orchestrator: inner, err: errors.New("the tracker timed out")}
	app, _, errBuf := resolveTestApp(t, be)

	app.cmdResolve(context.Background(), []string{"1"})
	if !strings.Contains(errBuf.String(), "acting as fake-account — roles it can assume: contributor") {
		t.Errorf("the standing was withheld because an unrelated read failed; got %q", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "filed by") {
		t.Errorf("a filer was named from a read that failed; got %q", errBuf.String())
	}
}

// Reaching the end of the flow is not the run being RECORDED complete. The tick
// is gated on the record, because a reader who trusts the summary concludes the
// item is closed — and the result object one line above already said it was not.
func TestCmdResolve_AnItemTheOrchestratorWillNotFinalizeIsNotReportedFinalized(t *testing.T) {
	be := fake.New() // the fake's items start open, and Finalize refuses an open item
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 — 'not yet' is not a failure; err=%q", code, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "finalized ✓") {
		t.Errorf("the tick was printed for an item nothing finalized; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "not finalized") {
		t.Errorf("the summary does not say what happened; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "claim kept") {
		t.Errorf("the summary does not say the claim was kept; got %q", errBuf.String())
	}
	item, err := be.Load(context.Background(), be.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if item.Holder.Empty() {
		t.Error("the claim was released even though nothing was finalized")
	}
}

// The counterpart, so the tick is not simply never printed: an item the
// orchestrator considers finished finalizes, and the summary says so.
func TestCmdResolve_AFinalizedRunStillReportsTheTick(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be.SetStatus("1", flow.StatusTerminal, "completed")
	app, _, errBuf := resolveTestApp(t, be)

	code := app.cmdResolve(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "finalized ✓") {
		t.Errorf("a recorded finalization must still report the tick; got %q", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "not finalized") {
		t.Errorf("the two summaries contradict each other; got %q", errBuf.String())
	}
}

// awaitingMaintainer is the item the handoff branch meets on its FIRST pass:
// the contributor has moved and the marker already names the next role, so the
// run reaches the boundary without dispatching anything.
func awaitingMaintainer() flow.Item {
	return flow.Item{Type: "task", Title: "1",
		Awaits: flow.Awaits{Role: "maintainer"},
		Journal: []flow.JournalEntry{{
			Step: "plan", Execution: 1, Route: flow.Route{Next: "commit"},
			By: "bob", Role: "contributor", Awaits: flow.Awaits{Role: "maintainer"},
		}}}
}

// The handoff is decided BEFORE the pacing block, so a run that will not
// advance the item does not first wait for quota headroom it is never going to
// spend. The maintainer's step prompts, so the wait would apply to it — a
// mechanical step is spared the pacing for its own reason and would hide this
// one.
func TestCmdResolve_AHandoffIsDecidedBeforeAnyPacingWait(t *testing.T) {
	be := fake.New()
	be.AddItem("1", awaitingMaintainer())
	be.SetCapabilities("", flow.CapPush)
	app, _, errBuf := resolveTestAppFlow(t, be, func(f *flow.Flow) {
		f.Role("maintainer", flow.CapMerge)
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Next("commit", "planned").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Entry: true, Role: "contributor", Next: []flow.StepId{"commit"}})
		f.AddStep("close branch", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "landed").CommitHash("abc"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "maintainer", MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})
	// Saturated, so any run that reaches the pacing block waits and says so.
	app.Quota = func() ([]windowUsage, error) {
		return []windowUsage{{
			Label: "5h", Length: time.Second, Used: 1.0, ResetsAt: time.Now().Add(50 * time.Millisecond),
		}}, nil
	}

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "handed off — awaits maintainer") {
		t.Fatalf("expected the handoff; got %q", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "resolve: pacing —") {
		t.Errorf("the run waited for headroom it was never going to spend; got %q", errBuf.String())
	}
}

// The handoff carries the run's totals, as the finalization does: a terminal
// outcome that named no figures would make what an item has cost so far depend
// on which end the run happened to stop at.
func TestCmdResolve_AHandoffReportsWhatTheItemHasCost(t *testing.T) {
	be := fake.New()
	item := awaitingMaintainer()
	item.Ledger = flow.Ledger{TotalActive: 90 * time.Second, TotalCostUSD: 1.25}
	be.AddItem("1", item)
	be.SetCapabilities("", flow.CapPush)
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "handed off — awaits maintainer") {
		t.Fatalf("expected the handoff; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "$1.25") {
		t.Errorf("the handoff does not report what the item has cost; got %q", errBuf.String())
	}
}

// An item this binary's flow does not accept is not somebody else's move: no
// flow of this runner's ever derived a step for it, so the awaited marker on it
// says nothing about who should act. It reports the type, as it did before the
// handoff branch existed, and keeps the claim.
func TestCmdResolve_AnItemOutsideTheRemitIsNotHandedOff(t *testing.T) {
	be := fake.New()
	item := awaitingMaintainer()
	item.Type = "chore" // the fixture's flow accepts "task" only
	item.Journal = nil  // …and a journal would put it back in the remit
	be.AddItem("1", item)
	be.SetCapabilities("", flow.CapPush)
	app, errBuf := handoffTestApp(t, be)

	app.cmdResolve(context.Background(), []string{"1"})
	if strings.Contains(errBuf.String(), "handed off") {
		t.Errorf("an item no flow here accepts was handed off; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no flow accepts this item's type") {
		t.Errorf("the run does not report the type; got %q", errBuf.String())
	}
	itemState, err := be.Load(context.Background(), be.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if itemState.Holder.Empty() {
		t.Error("the claim was released on an item nobody was handed")
	}
}

// A FINALIZED item is not handed off, whatever marker it still carries: the
// work is over, and reporting the last role that was awaited as the one to move
// next would send an operator to wait on somebody with nothing to do.
func TestCmdResolve_AFinalizedItemIsNotHandedOff(t *testing.T) {
	be := fake.New()
	item := awaitingMaintainer()
	item.Finalized = true
	be.AddItem("1", item)
	be.SetCapabilities("", flow.CapPush)
	app, errBuf := handoffTestApp(t, be)

	app.cmdResolve(context.Background(), []string{"1"})
	if strings.Contains(errBuf.String(), "handed off") {
		t.Errorf("an item whose work is over was handed to a role that has nothing to do; got %q", errBuf.String())
	}
}
