package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// capturingBackend records every capture the SDK makes, so a test can say both
// "exactly once" and "with what the handler returned" — the two halves of
// "result and route land together".
type capturingBackend struct {
	*fake.Orchestrator
	captures []flow.ArtifactBody
	refuse   error
}

func (b *capturingBackend) ResolveArtifact(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId, body flow.ArtifactBody) error {
	b.captures = append(b.captures, body)
	if b.refuse != nil {
		return b.refuse
	}
	return b.Orchestrator.ResolveArtifact(ctx, ref, id, body)
}

// capturingApp is testApp with the capture-recording backend in front of the
// fake, returned so the test can read what was captured.
func capturingApp(t *testing.T, configure func(*flow.Flow)) (*App, *capturingBackend, flow.Claim) {
	t.Helper()
	app, be, claim := testApp(t, configure, &stubAgent{name: "stub"})
	cap := &capturingBackend{Orchestrator: be}
	app.Orchestrator = cap
	return app, cap, claim
}

// The completion path: one election, one capture, and the body captured is the
// one the handler returned.
func TestCompletion_CapturesTheReturnedBodyExactlyOnce(t *testing.T) {
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if err := ctx.RecordWorkInProgress("half a plan"); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "the plan is written").Markdown("the plan"), nil
		}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("res = %+v, want done", res)
	}
	if len(be.captures) != 1 {
		t.Fatalf("captured %d times, want exactly one", len(be.captures))
	}
	if got := be.captures[0]; got.Type != flow.ArtifactMarkdown || got.Markdown != "the plan" {
		t.Errorf("captured %+v, want the markdown the handler returned", got)
	}
	// The step has a result now, so its scaffolding is done.
	wip, err := be.LoadWorkInProgress(context.Background(), claim.ItemRef, "plan")
	if err != nil {
		t.Fatalf("LoadWorkInProgress: %v", err)
	}
	if wip != "" {
		t.Errorf("work in progress = %q, want it cleared when the step completed", wip)
	}
}

// A step that decided nothing PARKS: a re-dispatch can still do the job.
func TestCompletion_ZeroResultParks(t *testing.T) {
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		f.AddStep("forgetful", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return flow.StepResult{}, nil
		}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkStepDidNotResolve {
		t.Fatalf("res = %+v, want parked step-did-not-resolve", res)
	}
	if len(be.captures) != 0 {
		t.Errorf("captured %+v for a step that completed nothing", be.captures)
	}
}

// A step that decided something it may not decide FAILS, and nothing is
// journaled or captured in any of the three cases.
func TestCompletion_IllegalElectionsFailWithNothingCaptured(t *testing.T) {
	cases := map[string]struct {
		configure func(*flow.Flow)
		reason    string
	}{
		"undeclared successor": {func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Next("nowhere", "carry on").Markdown("the plan"), nil
			}, flow.StepConfig{Next: []flow.StepId{"review"}})
		}, "does not declare"},
		"wrong payload type": {func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Finalize(flow.DispositionResolved, "done").CommitHash("deadbeef"), nil
			}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
		}, "expected markdown"},
		"payload on a signal step": {func(f *flow.Flow) {
			f.AddSignalStep("create pr", "pr-open", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Finalize(flow.DispositionResolved, "done").Markdown("nope"), nil
			}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
		}, "not handler-writable"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			app, be, claim := capturingApp(t, tc.configure)
			res, err := RunOne(context.Background(), app, claim)
			if err != nil {
				t.Fatalf("RunOne: %v", err)
			}
			if res.Status != "failed" {
				t.Fatalf("res = %+v, want failed", res)
			}
			if !strings.Contains(res.Reason, tc.reason) {
				t.Errorf("reason = %q, want it to contain %q", res.Reason, tc.reason)
			}
			if len(be.captures) != 0 {
				t.Errorf("captured %+v — an election that was refused journals nothing", be.captures)
			}
		})
	}
}

// Capture happens after the write-contract check, so a step that violated its
// contract no longer leaves a captured artifact behind: the result is verified
// before it is captured.
func TestCompletion_WriteContractViolationCapturesNothing(t *testing.T) {
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		f.AddStep("plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			wt, err := ctx.Worktree()
			if err != nil {
				return flow.StepResult{}, err
			}
			if _, err := wt.Branch(ctx.Context(), "rogue-branch", ""); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkWriteContract {
		t.Fatalf("res = %+v, want parked on the write contract", res)
	}
	if len(be.captures) != 0 {
		t.Errorf("captured %+v — the contract check runs before capture", be.captures)
	}
}

// A deadline kills the dispatch before the handler ever returns an election,
// so there is nothing to capture — the same ordering, from the other end.
func TestCompletion_DeadlineCapturesNothing(t *testing.T) {
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		f.AddStep("slow", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			<-ctx.Context().Done()
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("too late"), ctx.Context().Err()
		}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {Timeout: 10 * time.Millisecond}}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" {
		t.Fatalf("res = %+v, want parked on the deadline", res)
	}
	if len(be.captures) != 0 {
		t.Errorf("captured %+v after a deadline kill", be.captures)
	}
}

// ResolveArtifact publishes, so it can refuse. With capture after the handler
// returns there is no in-invocation revision left, so the refusal is stashed
// and the item parks — and the park reason carries the ACT and nothing the
// guard said, because a park is published through that same guard.
func TestCompletion_DisclosureRefusalParksAndKeepsTheWork(t *testing.T) {
	const guardAnswer = "an absolute home path names the machine's user"
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "the plan is written").
				Markdown("the plan mentioning /home/someone/"), nil
		}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})
	be.refuse = flow.ErrDisclosureRefused{
		Act:    flow.ActArtifactComment,
		Reason: errors.New(guardAnswer),
	}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkBlocked {
		t.Fatalf("res = %+v, want parked blocked", res)
	}
	if !strings.Contains(res.Park.Reason, string(flow.ActArtifactComment)) {
		t.Errorf("park reason = %q, want it to name the refused act", res.Park.Reason)
	}
	if strings.Contains(res.Park.Reason, guardAnswer) {
		t.Errorf("park reason repeats the guard's answer, which is the one text that cannot be published: %q",
			res.Park.Reason)
	}
	// The work is not the casualty: what was refused, and why, is where the
	// next dispatch's prompt reads it.
	wip, err := be.LoadWorkInProgress(context.Background(), claim.ItemRef, "plan")
	if err != nil {
		t.Fatalf("LoadWorkInProgress: %v", err)
	}
	if !strings.Contains(wip, guardAnswer) {
		t.Errorf("stash = %q, want it to carry the guard's reason", wip)
	}
	if !strings.Contains(wip, "the plan mentioning /home/someone/") {
		t.Errorf("stash = %q, want it to carry the refused text", wip)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if rec := state.Artifact("plan"); rec.Resolved {
		t.Errorf("plan artifact = %+v, want unresolved after a refused capture", rec)
	}
	// "A correction round is priced as a round, not as a dispatch"
	// (docs/resolution.md § The treasurer). Charged as one, three refused
	// sentences would exhaust the default three invocations and park on the
	// budget — reporting a budget cap for a problem no grant can fix.
	if rec := state.Artifact("plan"); rec.Invocations != 0 {
		t.Errorf("invocations = %d after a refused capture, want 0 — a refused expression of "+
			"finished work is not a failed attempt at the step", rec.Invocations)
	}
}

// Every other completion outcome IS a dispatch, and counts: the refusal is the
// single exception, not a hole under the completion path.
func TestCompletion_EveryOtherOutcomeCountsTheDispatch(t *testing.T) {
	cases := map[string]struct {
		configure func(*flow.Flow)
		refuse    error
		status    string
	}{
		"done": {func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Finalize(flow.DispositionResolved, "the plan is written").Markdown("the plan"), nil
			}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
		}, nil, "done"},
		"decided nothing": {func(f *flow.Flow) {
			f.AddStep("forgetful", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return flow.StepResult{}, nil
			}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
		}, nil, "parked"},
		"undeclared election": {func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Next("nowhere", "carry on").Markdown("the plan"), nil
			}, flow.StepConfig{Next: []flow.StepId{"review"}})
		}, nil, "failed"},
		"capture failed for any other reason": {func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Finalize(flow.DispositionResolved, "the plan is written").Markdown("the plan"), nil
			}, flow.StepConfig{MayFinalize: []flow.Disposition{flow.DispositionResolved}})
		}, errors.New("the orchestrator is broken"), "failed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			app, be, claim := capturingApp(t, tc.configure)
			be.refuse = tc.refuse
			res, err := RunOne(context.Background(), app, claim)
			if err != nil {
				t.Fatalf("RunOne: %v", err)
			}
			if res.Status != tc.status {
				t.Fatalf("res = %+v, want %s", res, tc.status)
			}
			state, _ := be.Load(context.Background(), claim.ItemRef)
			if rec := state.Artifact("plan"); rec.Invocations != 1 {
				t.Errorf("invocations = %d, want the dispatch counted once", rec.Invocations)
			}
		})
	}
}

// accessorCtx builds a stepCtx directly over a hand-written item, which is the
// only way to put a journal in front of the read accessors while nothing
// appends entries yet.
func accessorCtx(t *testing.T, state *flow.Item) *stepCtx {
	t.Helper()
	f := flow.NewFlow("resolve", nil)
	f.AddStep("write plan", "plan", func(flow.StepCtx) (flow.StepResult, error) {
		return flow.StepResult{}, nil
	}, flow.StepConfig{Role: "contributor"})
	li, _ := f.Item("write plan")
	app := &App{Orchestrator: fake.New(), Agent: &stubAgent{name: "stub"}}
	claim := flow.Claim{ItemRef: flow.ItemRef{Display: "1"}, Account: "runner-account"}
	return newStepCtx(context.Background(), app, claim, f, li, state, time.Minute)
}

func TestStepCtx_JournalIsACopy(t *testing.T) {
	state := &flow.Item{Journal: []flow.JournalEntry{{Step: "plan", Message: "the plan is written"}}}
	sc := accessorCtx(t, state)

	got := sc.Journal()
	if len(got) != 1 || got[0].Step != "plan" {
		t.Fatalf("Journal() = %+v, want the one entry", got)
	}
	got[0].Message = "rewritten"
	if state.Journal[0].Message != "the plan is written" {
		t.Errorf("the item's journal was rewritten through the copy: %q", state.Journal[0].Message)
	}
}

func TestStepCtx_TransferIsTheEntryThatRoutedHere(t *testing.T) {
	if got := accessorCtx(t, &flow.Item{}).Transfer(); got != nil {
		t.Errorf("Transfer() = %+v on an empty journal, want nil — nothing routed here", got)
	}
	state := &flow.Item{Journal: []flow.JournalEntry{
		{Step: "plan", Message: "first"},
		{Step: "impl", Message: "the change is committed"},
	}}
	got := accessorCtx(t, state).Transfer()
	if got == nil || got.Step != "impl" || got.Message != "the change is committed" {
		t.Errorf("Transfer() = %+v, want the last entry", got)
	}
}

func TestStepCtx_NotesAreFilteredAndOrdered(t *testing.T) {
	state := &flow.Item{Journal: []flow.JournalEntry{
		{Step: "plan", Note: "watch the parser"},
		{Step: "branch"},
		{Step: "impl", Note: "the fixture is stale"},
	}}
	got := accessorCtx(t, state).Notes()
	if len(got) != 2 {
		t.Fatalf("Notes() = %+v, want only the note-carrying entries", got)
	}
	if got[0].Note != "watch the parser" || got[1].Note != "the fixture is stale" {
		t.Errorf("Notes() = %+v, want them in journal order", got)
	}
}

func TestStepCtx_RunNumberCountsDispatches(t *testing.T) {
	state := &flow.Item{Artifacts: map[flow.ArtifactId]flow.ArtifactRecord{}}
	if got := accessorCtx(t, state).RunNumber(); got != 1 {
		t.Errorf("RunNumber() = %d on the first dispatch, want 1", got)
	}
	state.Artifacts["plan"] = flow.ArtifactRecord{Id: "plan", Invocations: 1}
	if got := accessorCtx(t, state).RunNumber(); got != 2 {
		t.Errorf("RunNumber() = %d after one bump, want 2", got)
	}
}

func TestStepCtx_RunnerAndRole(t *testing.T) {
	sc := accessorCtx(t, &flow.Item{})
	if got := sc.Runner(); got != "runner-account" {
		t.Errorf("Runner() = %q, want the claim's account", got)
	}
	if got := sc.Role(); got != "contributor" {
		t.Errorf("Role() = %q, want the step's own tag", got)
	}
}

// The declaration decides whether the question can be asked; the journal
// answers it. An undeclared name is refused rather than answered empty,
// because empty means "declared and has not acted yet".
func TestStepCtx_RoleAccount(t *testing.T) {
	state := &flow.Item{Journal: []flow.JournalEntry{
		{Step: "plan", By: "ann", Role: "contributor"},
	}}
	sc := accessorCtx(t, state)

	var unknown flow.ErrUnknownRole
	if _, err := sc.RoleAccount("reviewer"); !errors.As(err, &unknown) {
		t.Fatalf("RoleAccount(reviewer) err = %v, want ErrUnknownRole", err)
	} else if unknown.Role != "reviewer" {
		t.Errorf("err names %q, want the role that was asked for", unknown.Role)
	}

	got, err := sc.RoleAccount("contributor")
	if err != nil {
		t.Fatalf("RoleAccount(contributor): %v", err)
	}
	if got != "ann" {
		t.Errorf("RoleAccount(contributor) = %q, want the account of record", got)
	}

	// Declared and not yet acted reads empty, with no error: that is the state
	// a handler waits on.
	empty := accessorCtx(t, &flow.Item{})
	if got, err := empty.RoleAccount("contributor"); err != nil || got != "" {
		t.Errorf("RoleAccount = (%q, %v), want empty and no error for a role that has not acted", got, err)
	}
}
