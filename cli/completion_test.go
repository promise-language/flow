package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	// entries is what was appended, whole: the capture is now one write with
	// the route, so a test that only saw the body could not tell the two apart.
	entries []flow.JournalEntry
	// refusals scripts what each successive append answers before the fake is
	// consulted: a nil entry accepts, and an exhausted script accepts. A script
	// rather than one error, because the revision round is a SECOND append in
	// the same dispatch, and the tests here have to refuse the first and accept
	// the second.
	refusals []error
	// saveErr models a backend that cannot write a work-in-progress record,
	// which is the store the refused-capture path stashes into. saveErrAfter is
	// how many saves it lets through first, so a test can fail the stash on the
	// LAST round of a dispatch rather than on the first — the zero value fails
	// every save, which is what a store that is simply gone does.
	saveErr      error
	saveErrAfter int
	saves        int
}

func (b *capturingBackend) AppendEntry(ctx context.Context, ref flow.ItemRef, e flow.JournalEntry) error {
	b.captures = append(b.captures, e.Result)
	b.entries = append(b.entries, e)
	if len(b.refusals) > 0 {
		err := b.refusals[0]
		b.refusals = b.refusals[1:]
		if err != nil {
			return err
		}
	}
	return b.Orchestrator.AppendEntry(ctx, ref, e)
}

// refusedComment is the refusal the guard returns for a result it will not
// publish as a comment, with the answer a test can look for in the stash and
// must not find in anything published.
const guardAnswer = "an absolute home path names the machine's user"

func refusedComment() flow.ErrDisclosureRefused {
	return flow.ErrDisclosureRefused{Act: flow.ActArtifactComment, Reason: errors.New(guardAnswer)}
}

// revisingPlan is a plan step that records what it read as its work in
// progress on each run, and produces `first` on a run that found nothing
// stashed and `revised` on one that did — the shape of a step amending a
// refused result rather than re-deriving it.
func revisingPlan(seen *[]string, first, revised string) func(*flow.Flow) {
	return func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			wip, err := ctx.WorkInProgress()
			if err != nil {
				return flow.StepResult{}, err
			}
			*seen = append(*seen, wip)
			text := first
			if wip != "" {
				text = revised
			}
			return ctx.Finalize(flow.DispositionResolved, "the plan is written").Markdown(text), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}
}

// numberedPlan is revisingPlan for a refusal that recurs: it records what it
// read and numbers every run of the step, so a test spanning several rounds and
// several dispatches can say which run produced what.
func numberedPlan(seen *[]string) func(*flow.Flow) {
	return func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			wip, err := ctx.WorkInProgress()
			if err != nil {
				return flow.StepResult{}, err
			}
			*seen = append(*seen, wip)
			return ctx.Finalize(flow.DispositionResolved, "the plan is written").
				Markdown(fmt.Sprintf("attempt %d", len(*seen))), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}
}

func (b *capturingBackend) SaveWorkInProgress(ctx context.Context, ref flow.ItemRef, step flow.StepId, body string) error {
	b.saves++
	if b.saveErr != nil && b.saves > b.saveErrAfter {
		return b.saveErr
	}
	return b.Orchestrator.SaveWorkInProgress(ctx, ref, step, body)
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
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkStepDidNotComplete {
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
		// The route the entry declares is `commit`, and the handler elects
		// `nowhere`. The successor has to be a REGISTERED step: startup refuses a
		// route naming nothing (Flow.ValidateGraph), so a graph whose declared
		// successor did not exist would never reach the dispatch this is about.
		"undeclared successor": {func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Next("nowhere", "carry on").Markdown("the plan"), nil
			}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, Next: []flow.StepId{"commit"}})
			f.AddStep("record the commit", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Finalize(flow.DispositionResolved, "done").CommitHash("abc"), nil
			}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", MayFinalize: []flow.Disposition{flow.DispositionResolved}})
		}, "does not declare"},
		"wrong payload type": {func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Finalize(flow.DispositionResolved, "done").CommitHash("deadbeef"), nil
			}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
		}, "expected markdown"},
		"payload on a signal step": {func(f *flow.Flow) {
			f.AddSignalStep("create pr", "pr-open", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Finalize(flow.DispositionResolved, "done").Markdown("nope"), nil
			}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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

// AppendEntry publishes, so it can refuse. A refused capture is revised INSIDE
// the dispatch: the refusal and the refused text are stashed as the step's work
// in progress, the handler runs again in the same dispatch, reads them back,
// amends, and the second append lands. One journal entry — the revised one —
// one charged dispatch (a correction round is priced as a round, not as a
// dispatch), no park anywhere, and the stash cleared by the completion.
func TestCompletion_RefusedCaptureIsRevisedInTheSameDispatch(t *testing.T) {
	const refusedText = "the plan mentioning /home/someone/"
	tel := &recordingTelemetry{}
	var seen []string // what each handler run read as its work in progress
	app, be, claim := capturingApp(t, revisingPlan(&seen, refusedText, "the plan, revised"))
	app.Telemetry = tel
	be.refusals = []error{refusedComment()} // the first append only

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("res = %+v, want done — the revision landed in this dispatch", res)
	}
	if res.Park != nil || res.RedispatchMayClear != nil {
		t.Errorf("res = %+v, want no park and no classification on a dispatch that completed", res)
	}
	if len(seen) != 2 {
		t.Fatalf("handler ran %d times, want 2 — the refused run and its revision", len(seen))
	}
	if seen[0] != "" {
		t.Errorf("first run read %q as work in progress, want nothing stashed yet", seen[0])
	}
	// The second run is answering something the first did not know: the
	// guard's answer and the text it refused, which is what makes the revision
	// an amendment rather than a re-derivation.
	if !strings.Contains(seen[1], guardAnswer) || !strings.Contains(seen[1], refusedText) {
		t.Errorf("revision read %q, want the guard's reason and the refused text", seen[1])
	}
	if len(be.entries) != 2 {
		t.Fatalf("appended %d times, want 2 — the refused attempt and the revision", len(be.entries))
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if len(state.Journal) != 1 {
		t.Fatalf("journal = %+v, want the one entry the revision appended", state.Journal)
	}
	if got := state.Journal[0].Result.Markdown; got != "the plan, revised" {
		t.Errorf("journaled %q, want the revised text", got)
	}
	// "A correction round is priced as a round, not as a dispatch"
	// (docs/resolution.md § The treasurer): the dispatch that completed is
	// charged once, whatever rounds it spent getting there.
	row := state.Ledger.Row("plan")
	if row.Dispatches != 1 {
		t.Errorf("Dispatches = %d, want 1 — the round is not an attempt", row.Dispatches)
	}
	if row.Resumptions != 0 {
		t.Errorf("Resumptions = %d, want 0 — nothing was parked, so nothing was resumed", row.Resumptions)
	}
	if state.Park != nil {
		t.Errorf("Park = %+v, want none — the revision landed without a park", state.Park)
	}
	// The step has a result now, so its scaffolding — the refused record — is
	// done with, cleared by the same write that recorded the result.
	wip, err := be.LoadWorkInProgress(context.Background(), claim.ItemRef, "plan")
	if err != nil {
		t.Fatalf("LoadWorkInProgress: %v", err)
	}
	if wip != "" {
		t.Errorf("stash = %q after the revision completed, want it cleared", wip)
	}
	// The round is reported, naming the act and the round — and nothing the
	// guard said, because a notice can reach the tracker too.
	var noticed bool
	for _, e := range tel.events {
		if strings.Contains(e.Detail, string(flow.ActArtifactComment)) && strings.Contains(e.Detail, "round 1") {
			noticed = true
		}
		if strings.Contains(e.Detail, guardAnswer) {
			t.Errorf("a notice repeats the guard's answer: %q", e.Detail)
		}
	}
	if !noticed {
		t.Errorf("the revision round was never reported; events = %+v", tel.events)
	}
}

// The bound. A result refused again after its revision round parks BLOCKED:
// a second refusal of the same work is a loop, not a transient, and the kind
// classifies as one no re-dispatch clears — which is what stops a driver from
// buying a third round on its own. Exactly two handler runs, nothing journaled,
// no dispatch charged (a refused round is not an attempt, whichever round it
// is), the stash holding the LATEST refusal for whoever re-runs the step, and a
// reason that names the act and the round and nothing the guard said, because
// a park is published through that same guard.
func TestCompletion_RefusedCaptureTwiceParksBlocked(t *testing.T) {
	var seen []string
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			wip, err := ctx.WorkInProgress()
			if err != nil {
				return flow.StepResult{}, err
			}
			seen = append(seen, wip)
			// A different sentence each time, so the stash can be seen to
			// carry the latest refusal and not the first.
			return ctx.Finalize(flow.DispositionResolved, "the plan is written").
				Markdown(fmt.Sprintf("attempt %d mentioning /home/someone/", len(seen))), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})
	be.refusals = []error{refusedComment(), refusedComment()}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkBlocked {
		t.Fatalf("res = %+v, want parked blocked after the revision round was spent", res)
	}
	if res.RedispatchMayClear == nil || *res.RedispatchMayClear {
		t.Errorf("RedispatchMayClear = %v, want a present false: a second refusal of the same work is a loop, and a driver must not buy a third round", res.RedispatchMayClear)
	}
	if len(seen) != 2 {
		t.Fatalf("handler ran %d times, want exactly 2 — one revision round, then a person", len(seen))
	}
	if !strings.Contains(seen[1], "attempt 1 mentioning") {
		t.Errorf("revision read %q, want the first attempt's refusal", seen[1])
	}
	for _, want := range []string{string(flow.ActArtifactComment), "round 1"} {
		if !strings.Contains(res.Park.Reason, want) {
			t.Errorf("park reason = %q, want it to name %q", res.Park.Reason, want)
		}
	}
	if strings.Contains(res.Park.Reason, guardAnswer) {
		t.Errorf("park reason repeats the guard's answer, which is the one text that cannot be published: %q", res.Park.Reason)
	}
	// The work is not the casualty: what was refused, and why, is where the
	// re-run's prompt reads it — and it is the SECOND attempt, the one a person
	// re-running the step is answering for.
	wip, err := be.LoadWorkInProgress(context.Background(), claim.ItemRef, "plan")
	if err != nil {
		t.Fatalf("LoadWorkInProgress: %v", err)
	}
	if !strings.Contains(wip, guardAnswer) || !strings.Contains(wip, "attempt 2 mentioning") {
		t.Errorf("stash = %q, want the guard's reason and the second attempt's text", wip)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if rec := state.Artifact("plan"); rec.Resolved {
		t.Errorf("plan artifact = %+v, want unresolved after two refused captures", rec)
	}
	// Nothing was journaled: result and route land together or not at all, and
	// both refusals are the "not at all".
	if len(state.Journal) != 0 {
		t.Errorf("journal = %+v after two refused captures, want empty", state.Journal)
	}
	if state.Park == nil || state.Park.Kind != flow.ParkBlocked {
		t.Errorf("Park = %+v on the loaded item, want the blocked park", state.Park)
	}
	// Charged as one, three refused sentences would exhaust the default three
	// invocations and park on the budget — reporting a budget cap for a problem
	// no grant can fix.
	if row := state.Ledger.Row("plan"); row.Dispatches != 0 {
		t.Errorf("Dispatches = %d after two refused captures, want 0 — a refused expression of "+
			"finished work is not a failed attempt at the step", row.Dispatches)
	}
}

// A person's re-run buys the next round. The dispatch after the blocked park
// reads the second refusal's record as its work in progress, revises, and
// completes: one resumption, one charged dispatch, the park cleared.
func TestCompletion_ReRunAfterTheBlockedParkRevisesFromTheKeptRefusal(t *testing.T) {
	var seen []string
	app, be, claim := capturingApp(t, numberedPlan(&seen))
	be.refusals = []error{refusedComment(), refusedComment()}

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Park == nil || res.Park.Kind != flow.ParkBlocked {
		t.Fatalf("first RunOne = (%+v, %v), want parked blocked", res, err)
	}

	// The re-run: the script is exhausted, so the append is accepted.
	res, err := RunOne(context.Background(), app, claim)
	if err != nil || res.Status != "done" {
		t.Fatalf("second RunOne = (%+v, %v), want done", res, err)
	}
	if len(seen) != 3 {
		t.Fatalf("handler ran %d times across both dispatches, want 3", len(seen))
	}
	if !strings.Contains(seen[2], guardAnswer) || !strings.Contains(seen[2], "attempt 2") {
		t.Errorf("the re-run read %q, want the second refusal's record — the latest refusal, not the first", seen[2])
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if len(state.Journal) != 1 || state.Journal[0].Result.Markdown != "attempt 3" {
		t.Errorf("journal = %+v, want the one entry the re-run appended", state.Journal)
	}
	row := state.Ledger.Row("plan")
	if row.Dispatches != 1 {
		t.Errorf("Dispatches = %d, want 1 — the dispatch that completed, and neither refused round", row.Dispatches)
	}
	if row.Resumptions != 1 {
		t.Errorf("Resumptions = %d, want 1 — the re-run picked the item up from the blocked park", row.Resumptions)
	}
	if state.Park != nil {
		t.Errorf("Park = %+v after the re-run completed, want none", state.Park)
	}
}

// The stash is best-effort, and it has to be: a backend that cannot write the
// record must not cost the item its park as well as its work. Losing the park
// would end the run with nobody told anything, which is the outcome the whole
// refusal path exists to avoid.
//
// And there is NO revision round without the record: the handler would compose
// the same text from the same context and be refused identically, so the round
// is not spent. The item parks step-did-not-complete — the kind under which a
// re-dispatch does the job — and the reason names the guard, so the remedy can
// tell this site from a handler that decided nothing.
func TestCompletion_RefusedCaptureWhoseStashFailsParksWithoutARound(t *testing.T) {
	tel := &recordingTelemetry{}
	runs := 0
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			runs++
			return ctx.Finalize(flow.DispositionResolved, "the plan is written").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})
	app.Telemetry = tel
	be.refusals = []error{refusedComment()}
	be.saveErr = errors.New("disk went away")

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkStepDidNotComplete {
		t.Fatalf("res = %+v, want parked step-did-not-complete despite the failed stash", res)
	}
	if runs != 1 {
		t.Errorf("handler ran %d times, want 1 — a round without the record could not differ from the attempt before it", runs)
	}
	// Nobody has to act, and a re-dispatch is what does the job: the kind's
	// classification is what a scheduler reads.
	if res.RedispatchMayClear == nil || !*res.RedispatchMayClear {
		t.Errorf("RedispatchMayClear = %v, want a present true: the next dispatch starts over", res.RedispatchMayClear)
	}
	for _, want := range []string{"disclosure guard", string(flow.ActArtifactComment)} {
		if !strings.Contains(res.Park.Reason, want) {
			t.Errorf("park reason = %q, want it to name %q", res.Park.Reason, want)
		}
	}
	if strings.Contains(res.Park.Reason, guardAnswer) {
		t.Errorf("park reason repeats the guard's answer: %q", res.Park.Reason)
	}
	var reported bool
	for _, e := range tel.events {
		if e.Detail == "could not record refused text: disk went away" {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the failed stash was never reported; events = %+v", tel.events)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if row := state.Ledger.Row("plan"); row.Dispatches != 0 {
		t.Errorf("Dispatches = %d, want 0 — the refusal is not charged whether or not it could be kept", row.Dispatches)
	}
}

// A stash that fails on the LAST round does not buy the dispatch a re-dispatch.
// The round is spent either way — this dispatch composed the text twice and the
// guard refused it twice — so the park is the blocked one the bound exists to
// reach. Parking step-did-not-complete here because the last record could not be
// written would hand the next dispatch a fresh round and make the bound
// per-dispatch rather than per-refusal, which is the loop again with an extra
// step in it: nothing is charged for a refused round, so nothing else stops it.
//
// What the failed stash does change is the reason, because the re-run reads the
// record: it says the latest refusal was not kept, and what is still stored is
// the FIRST round's.
func TestCompletion_RefusedCaptureTwiceWhoseLastStashFailsStillParksBlocked(t *testing.T) {
	var seen []string
	app, be, claim := capturingApp(t, revisingPlan(&seen, "attempt 1 mentioning /home/someone/", "attempt 2 mentioning /home/someone/"))
	be.refusals = []error{refusedComment(), refusedComment()}
	// The first round's stash takes, the revision's does not.
	be.saveErr = errors.New("disk went away")
	be.saveErrAfter = 1

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkBlocked {
		t.Fatalf("res = %+v, want parked blocked — the round was spent whatever became of the stash", res)
	}
	if res.RedispatchMayClear == nil || *res.RedispatchMayClear {
		t.Errorf("RedispatchMayClear = %v, want a present false: a dispatch that spent its round must not hand the next one a fresh one", res.RedispatchMayClear)
	}
	if len(seen) != 2 {
		t.Fatalf("handler ran %d times, want 2 — one revision round, then a person", len(seen))
	}
	for _, want := range []string{string(flow.ActArtifactComment), "round 1", "could not be kept"} {
		if !strings.Contains(res.Park.Reason, want) {
			t.Errorf("park reason = %q, want it to name %q", res.Park.Reason, want)
		}
	}
	if strings.Contains(res.Park.Reason, guardAnswer) {
		t.Errorf("park reason repeats the guard's answer: %q", res.Park.Reason)
	}
	// The first round's record survives, which is what the re-run will read.
	wip, err := be.LoadWorkInProgress(context.Background(), claim.ItemRef, "plan")
	if err != nil {
		t.Fatalf("LoadWorkInProgress: %v", err)
	}
	if !strings.Contains(wip, "attempt 1 mentioning") {
		t.Errorf("stash = %q, want the first round's refused text — the revision's could not be written", wip)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if len(state.Journal) != 0 {
		t.Errorf("journal = %+v after two refused captures, want empty", state.Journal)
	}
	if row := state.Ledger.Row("plan"); row.Dispatches != 0 {
		t.Errorf("Dispatches = %d, want 0 — a refused round is not an attempt, whichever round it is", row.Dispatches)
	}
}

// A mechanical step gets the same round. It is free and deterministic, so a
// mechanical result refused twice reaches the blocked park in seconds with no
// agent called — a more honest end than a re-dispatchable park on a refusal
// that cannot change.
func TestCompletion_MechanicalStepRefusedTwiceParksBlockedWithNoPrompt(t *testing.T) {
	runs := 0
	agent := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("record the base", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
			runs++
			return ctx.Finalize(flow.DispositionResolved, "recorded").CommitHash("deadbeef"), nil
		}, flow.StepConfig{Prompts: flow.PromptsNone, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, agent)
	cap := &capturingBackend{Orchestrator: be, refusals: []error{refusedComment(), refusedComment()}}
	app.Orchestrator = cap

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkBlocked {
		t.Fatalf("res = %+v, want parked blocked", res)
	}
	if runs != 2 {
		t.Errorf("handler ran %d times, want 2 — the round is the same for a mechanical step", runs)
	}
	if agent.calls != 0 || len(agent.reqs) != 0 {
		t.Errorf("the agent was asked %d times by a step declaring Prompts: none", len(agent.reqs))
	}
	if res.CostUSD == nil || *res.CostUSD != 0 {
		t.Errorf("cost_usd = %v, want a present zero — the round cost nothing", res.CostUSD)
	}
}

// The round is not a dispatch, but every turn it spends is real money, and the
// dispatch that completes has to report all of it. The step context is shared
// across the rounds precisely so the meter accumulates: a meter reset per round
// would price the journal entry at a fraction of what the work cost, leave the
// item's cost axis — the treasurer's bound on a step that keeps spending —
// blind to the refused round, and hand the revision turn a ceiling the dispatch
// had already eaten into.
func TestCompletion_ARevisedDispatchIsPricedAtEveryRoundItSpent(t *testing.T) {
	agent := &stubAgent{name: "stub", responses: []flow.AgentResponse{
		{LastText: "ok", CostUSD: 2},
		{LastText: "ok", CostUSD: 0.5},
	}}
	var seen []string
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			wip, err := ctx.WorkInProgress()
			if err != nil {
				return flow.StepResult{}, err
			}
			seen = append(seen, wip)
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "work"}); err != nil {
				return flow.StepResult{}, err
			}
			text := "the plan mentioning /home/someone/"
			if wip != "" {
				text = "the plan, revised"
			}
			return ctx.Finalize(flow.DispositionResolved, "the plan is written").Markdown(text), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})
	app.Agent = agent
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {MaxCostUSD: 10}}
	be.refusals = []error{refusedComment()}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" || len(seen) != 2 {
		t.Fatalf("res = %+v after %d handler runs, want done after 2", res, len(seen))
	}
	if res.CostUSD == nil {
		t.Errorf("cost_usd is absent, want the dispatch's $2.50")
	} else if *res.CostUSD != 2.5 {
		t.Errorf("cost_usd = %v, want $2.50 — the refused round's turn and the revision's", *res.CostUSD)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	row := state.Ledger.Row("plan")
	if row.CostUSD != 2.5 {
		t.Errorf("ledger CostUSD = %v, want $2.50 — the axis the treasurer bounds a spending step by", row.CostUSD)
	}
	if row.Dispatches != 1 {
		t.Errorf("Dispatches = %d, want 1 — priced as one dispatch whatever it spent", row.Dispatches)
	}
	if len(state.Journal) != 1 || state.Journal[0].Spend.CostUSD != 2.5 {
		t.Errorf("journaled spend = %+v, want the whole dispatch's $2.50 — what this result cost to express", state.Journal)
	}
	// The revision turn is handed the headroom the refused round left, not the
	// whole grant: the substrate stops it at the cap the step is actually
	// approaching.
	if len(agent.reqs) != 2 {
		t.Fatalf("the agent saw %d requests, want 2", len(agent.reqs))
	}
	if agent.reqs[1].MaxCostUSD != 8 {
		t.Errorf("the revision turn's MaxCostUSD = %v, want 8 — the $10 grant less the refused round's $2", agent.reqs[1].MaxCostUSD)
	}
}

// The revision round ends however a handler ends, and a flapping runner in it
// must not cost the item what the dispatch is holding. It parks infra-transient
// on the round's own failure and burns no invocation — neither the refused
// round nor the failed one was an attempt at the step — and the refusal and the
// text it refused stay in the stash, so the dispatch that picks the item up
// amends rather than re-derives.
func TestCompletion_ATransientFailureInTheRevisionRoundKeepsTheRefusedWork(t *testing.T) {
	var seen []string
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			wip, err := ctx.WorkInProgress()
			if err != nil {
				return flow.StepResult{}, err
			}
			seen = append(seen, wip)
			if wip != "" {
				return flow.StepResult{}, fmt.Errorf("the runner went away: %w", flow.ErrTransient)
			}
			return ctx.Finalize(flow.DispositionResolved, "the plan is written").
				Markdown("the plan mentioning /home/someone/"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})
	be.refusals = []error{refusedComment()}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkInfraTransient {
		t.Fatalf("res = %+v, want parked infra-transient — the revision round met a flapping runner", res)
	}
	if len(seen) != 2 {
		t.Fatalf("handler ran %d times, want 2 — the refused run and the round that failed", len(seen))
	}
	wip, err := be.LoadWorkInProgress(context.Background(), claim.ItemRef, "plan")
	if err != nil {
		t.Fatalf("LoadWorkInProgress: %v", err)
	}
	if !strings.Contains(wip, guardAnswer) || !strings.Contains(wip, "the plan mentioning") {
		t.Errorf("stash = %q, want the refusal and the refused text still there for the next dispatch", wip)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if len(state.Journal) != 0 {
		t.Errorf("journal = %+v, want empty — neither round produced a recordable result", state.Journal)
	}
	if row := state.Ledger.Row("plan"); row.Dispatches != 0 {
		t.Errorf("Dispatches = %d, want 0 — a flapping runner must not burn the invocations axis", row.Dispatches)
	}
}

// The round shares the dispatch's DEADLINE, and every round sees the same one.
// The clock is the dispatch's because a grant is measured in it: a round that
// renewed it would let a step refused repeatedly run for a multiple of the cap
// the operator granted, on the axis the grant is denominated in. Read off the
// context the handler is actually given, so a future refactor that took the
// timeout inside the loop fails here rather than in production.
func TestCompletion_TheRevisionRoundSharesTheDispatchDeadline(t *testing.T) {
	var deadlines []time.Time
	var seen []string
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			wip, err := ctx.WorkInProgress()
			if err != nil {
				return flow.StepResult{}, err
			}
			seen = append(seen, wip)
			dl, ok := ctx.Context().Deadline()
			if !ok {
				t.Error("the handler's context carries no deadline, so the dispatch has no timeout at all")
			}
			deadlines = append(deadlines, dl)
			return ctx.Finalize(flow.DispositionResolved, "the plan is written").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {Timeout: time.Minute}}
	be.refusals = []error{refusedComment()}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil || res.Status != "done" {
		t.Fatalf("RunOne = (%+v, %v), want done", res, err)
	}
	if len(deadlines) != 2 {
		t.Fatalf("handler ran %d times, want 2 — the refused run and its revision", len(deadlines))
	}
	if !deadlines[0].Equal(deadlines[1]) {
		t.Errorf("the revision round was given a fresh deadline (%v, then %v): the rounds must spend one "+
			"dispatch's timeout between them, not one each", deadlines[0], deadlines[1])
	}
}

// And what that costs when the refused round has eaten the clock: the revision
// runs out of time and parks on the TIMEOUT — charged the dispatch, reporting
// the axis that actually bound it, which is not the axis a refusal reports.
// That is the honest report, and it is a real difference from the park this
// change replaced, which charged nothing. What it must not cost is the work:
// the refusal and the text it refused are stashed BEFORE the round begins, so
// the dispatch that picks the item up still amends rather than re-derives.
func TestCompletion_ARevisionRoundThatRunsOutOfTimeParksOnTheTimeoutAndKeepsTheWork(t *testing.T) {
	var seen []string
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			wip, err := ctx.WorkInProgress()
			if err != nil {
				return flow.StepResult{}, err
			}
			seen = append(seen, wip)
			if wip != "" {
				// The round inherits what is left of the dispatch's clock, and
				// spends it.
				<-ctx.Context().Done()
				return flow.StepResult{}, ctx.Context().Err()
			}
			return ctx.Finalize(flow.DispositionResolved, "the plan is written").
				Markdown("the plan mentioning /home/someone/"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {Timeout: 50 * time.Millisecond}}
	be.refusals = []error{refusedComment()}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkTreasurerRefused || res.Park.Axis != flow.AxisTimeout {
		t.Fatalf("res = %+v, want a treasurer-refused park on the timeout axis", res)
	}
	if len(seen) != 2 {
		t.Fatalf("handler ran %d times, want 2 — the refused run and the round that ran out of time", len(seen))
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if row := state.Ledger.Row("plan"); row.Dispatches != 1 {
		t.Errorf("Dispatches = %d, want 1 — a timeout IS an attempt at the step, whichever round it fell in", row.Dispatches)
	}
	if len(state.Journal) != 0 {
		t.Errorf("journal = %+v, want empty — nothing the guard accepted was ever produced", state.Journal)
	}
	wip, err := be.LoadWorkInProgress(context.Background(), claim.ItemRef, "plan")
	if err != nil {
		t.Fatalf("LoadWorkInProgress: %v", err)
	}
	if !strings.Contains(wip, guardAnswer) || !strings.Contains(wip, "the plan mentioning") {
		t.Errorf("stash = %q, want the refusal and the refused text kept for the next dispatch", wip)
	}
}

// A person's re-run buys a whole round, not just one more attempt: the bound is
// counted per dispatch, so a re-run refused again revises before it parks. A
// bound counted off anything durable — the ledger, the executions, the stashed
// record — would park the re-run on its first refusal with no round spent, and
// the item would then need a person for every refused sentence rather than one
// for every refusal a revision could not fix.
func TestCompletion_TheReRunGetsItsOwnRevisionRound(t *testing.T) {
	var seen []string
	app, be, claim := capturingApp(t, numberedPlan(&seen))
	be.refusals = []error{refusedComment(), refusedComment(), refusedComment()}

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Park == nil || res.Park.Kind != flow.ParkBlocked {
		t.Fatalf("first RunOne = (%+v, %v), want parked blocked", res, err)
	}
	res, err := RunOne(context.Background(), app, claim)
	if err != nil || res.Status != "done" {
		t.Fatalf("second RunOne = (%+v, %v), want done — the re-run's own round satisfied the guard", res, err)
	}
	if len(seen) != 4 {
		t.Fatalf("handler ran %d times across both dispatches, want 4 — two rounds each", len(seen))
	}
	if !strings.Contains(seen[3], "attempt 3") {
		t.Errorf("the re-run's second round read %q, want the refusal its own first round earned", seen[3])
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if len(state.Journal) != 1 || state.Journal[0].Result.Markdown != "attempt 4" {
		t.Errorf("journal = %+v, want the one entry the re-run's revision appended", state.Journal)
	}
	if row := state.Ledger.Row("plan"); row.Dispatches != 1 {
		t.Errorf("Dispatches = %d, want 1 — three refusals bought nothing, and only the dispatch that completed is charged", row.Dispatches)
	}
	if state.Park != nil {
		t.Errorf("Park = %+v after the re-run completed, want none", state.Park)
	}
}

// Two sites park under step-did-not-complete — a handler that returned without
// completing, and a result the disclosure guard refused at capture whose
// refusal could not be kept with the step — and the grant remedy sends the
// operator to the park's reason to tell which (remedyFor). So the refused one
// must name the guard, and the other must not: a reason merged into a generic
// "did not complete" leaves an operator re-running a step whose text is refused
// identically each time, with nothing saying why.
func TestCompletion_StepDidNotCompleteReasonNamesTheSite(t *testing.T) {
	refusedAtCapture := func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "the plan is written").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}
	decidedNothing := func(f *flow.Flow) {
		f.AddStep("forgetful", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return flow.StepResult{}, nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}
	parkOf := func(t *testing.T, configure func(*flow.Flow), refusals []error, saveErr error) *flow.ParkRequest {
		t.Helper()
		app, be, claim := capturingApp(t, configure)
		be.refusals = refusals
		be.saveErr = saveErr
		res, err := RunOne(context.Background(), app, claim)
		if err != nil || res.Park == nil || res.Park.Kind != flow.ParkStepDidNotComplete {
			t.Fatalf("RunOne = (%+v, %v), want parked step-did-not-complete", res, err)
		}
		return res.Park
	}

	// The refused-capture site reaches this kind only when the stash failed;
	// with the record kept it revises, and parks blocked if that fails too.
	refused := parkOf(t, refusedAtCapture, []error{refusedComment()}, errors.New("disk went away"))
	if !strings.Contains(refused.Reason, "disclosure guard") {
		t.Errorf("refused-capture park reason = %q, want it to name the disclosure guard — the remedy sends the operator here to learn which site parked", refused.Reason)
	}
	forgetful := parkOf(t, decidedNothing, nil, nil)
	if strings.Contains(forgetful.Reason, "disclosure guard") {
		t.Errorf("zero-result park reason = %q, want no mention of a guard that refused nothing", forgetful.Reason)
	}
}

// What the stash carries for each kind of payload. The next dispatch is asked
// to revise text it can only read here, so a payload rendered as nothing is a
// author asked to fix a sentence they were never shown.
func TestRefusedPayload_CarriesWhatMustBeRevised(t *testing.T) {
	cases := map[string]struct {
		body flow.ArtifactBody
		want []string
	}{
		"commit hash": {flow.ArtifactBody{Type: flow.ArtifactCommitHash, CommitHash: "deadbeef"}, []string{"deadbeef"}},
		"markdown":    {flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the plan"}, []string{"the plan"}},
		"json":        {flow.ArtifactBody{Type: flow.ArtifactJSON, JSON: []byte(`{"a":1}`)}, []string{`{"a":1}`}},
		"file": {flow.ArtifactBody{Type: flow.ArtifactFile, File: flow.FileBody{
			Name: "report.txt", Content: []byte("what it said"),
		}}, []string{"report.txt", "what it said"}},
		"patch": {flow.ArtifactBody{Type: flow.ArtifactPatch, Patch: flow.PatchBody{
			Diff: []byte("diff --git a/x b/x"),
		}}, []string{"diff --git a/x b/x"}},
		// A flag carries no payload, so what was refused is the fact of the
		// write — said in words rather than left blank, which reads as a
		// rendering that failed.
		"flag": {flow.ArtifactBody{Type: flow.ArtifactFlag}, []string{"no payload"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := refusedPayload(tc.body)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("refusedPayload = %q, want it to carry %q", got, want)
				}
			}
		})
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
			}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
		}, nil, "done"},
		"decided nothing": {func(f *flow.Flow) {
			f.AddStep("forgetful", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return flow.StepResult{}, nil
			}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
		}, nil, "parked"},
		// As above: the declared successor exists, and the handler elects one
		// that does not.
		"undeclared election": {func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Next("nowhere", "carry on").Markdown("the plan"), nil
			}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, Next: []flow.StepId{"commit"}})
			f.AddStep("record the commit", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Finalize(flow.DispositionResolved, "done").CommitHash("abc"), nil
			}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", MayFinalize: []flow.Disposition{flow.DispositionResolved}})
		}, nil, "failed"},
		"capture failed for any other reason": {func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Finalize(flow.DispositionResolved, "the plan is written").Markdown("the plan"), nil
			}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
		}, errors.New("the orchestrator is broken"), "failed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			app, be, claim := capturingApp(t, tc.configure)
			if tc.refuse != nil {
				be.refusals = []error{tc.refuse}
			}
			res, err := RunOne(context.Background(), app, claim)
			if err != nil {
				t.Fatalf("RunOne: %v", err)
			}
			if res.Status != tc.status {
				t.Fatalf("res = %+v, want %s", res, tc.status)
			}
			state, _ := be.Load(context.Background(), claim.ItemRef)
			if row := state.Ledger.Row("plan"); row.Dispatches != 1 {
				t.Errorf("invocations = %d, want the dispatch counted once", row.Dispatches)
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
	f.Role("contributor", flow.CapPush)
	f.AddStep("write plan", "plan", func(flow.StepCtx) (flow.StepResult, error) {
		return flow.StepResult{}, nil
	}, flow.StepConfig{Prompts: flow.PromptsAgent, Entry: true, Role: "contributor", MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	li, _ := f.Item("write plan")
	app := &App{Orchestrator: fake.New(), Agent: &stubAgent{name: "stub"}}
	claim := flow.Claim{ItemRef: flow.ItemRef{Display: "1"}, Account: "runner-account"}
	return newStepCtx(context.Background(), app, claim, f, li, state, flow.StepBudget{Timeout: time.Minute})
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
	state := &flow.Item{}
	if got := accessorCtx(t, state).RunNumber(); got != 1 {
		t.Errorf("RunNumber() = %d on the first dispatch, want 1", got)
	}
	state.Ledger = flow.Ledger{Steps: map[flow.StepId]flow.LedgerRow{
		"plan": {Step: "plan", Dispatches: 1},
	}}
	if got := accessorCtx(t, state).RunNumber(); got != 2 {
		t.Errorf("RunNumber() = %d after one dispatch, want 2", got)
	}
	// Another step's row is not this step's: rows are keyed by StepId.
	state.Ledger.Steps["impl"] = flow.LedgerRow{Step: "impl", Dispatches: 5}
	if got := accessorCtx(t, state).RunNumber(); got != 2 {
		t.Errorf("RunNumber() = %d, want 2 — another step's dispatches are not this step's", got)
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
	// The alternatives travel with the refusal. The raiser can enumerate them —
	// it holds the flow — and a handler told only that its name is unknown has
	// to go and read the registration to find out what it should have asked
	// for.
	if !slices.Equal(unknown.Declared, []flow.RoleName{"contributor"}) {
		t.Errorf("ErrUnknownRole.Declared = %v, want the flow's declared set", unknown.Declared)
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

// --- What the appended entry carries ---

// ONE APPEND, carrying everything derived from the completion: the elected
// route, the message and the standing note, what the item now awaits, who ran
// the step and in what role, and what the execution cost. A field missing here
// is a field no orchestrator can persist, because this is the only write.
func TestCompletion_TheEntryCarriesTheWholeCompletion(t *testing.T) {
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		// `contributor` comes from testApp; the successor's role is the second
		// one, and the point of the fixture: what the entry awaits is the role
		// the NEXT step declares.
		f.Role("reviewer", flow.CapApprove)
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Next("commit", "the plan is written").
				Markdown("the plan").
				WithNote("the base branch is release-2"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Entry: true, Role: "contributor", Next: []flow.StepId{"commit"}})
		f.AddStep("record the commit", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "done").CommitHash("abc"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "reviewer", MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})
	// The agent bills, so the entry's Spend has something to carry.
	app.Agent = &stubAgent{name: "stub", responses: []flow.AgentResponse{{LastText: "ok", CostUSD: 1.25}}}

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("res = %+v, want done", res)
	}
	if len(be.entries) != 1 {
		t.Fatalf("appended %d entries, want exactly one", len(be.entries))
	}
	e := be.entries[0]
	if e.Step != "plan" || e.Execution != 1 {
		t.Errorf("entry identity = %s/%d, want plan/1", e.Step, e.Execution)
	}
	if e.Route != (flow.Route{Next: "commit"}) {
		t.Errorf("Route = %+v, want the elected successor", e.Route)
	}
	if e.Message != "the plan is written" {
		t.Errorf("Message = %q, want the handler's", e.Message)
	}
	if e.Note != "the base branch is release-2" {
		t.Errorf("Note = %q, want the standing note", e.Note)
	}
	// The successor's declared role, computed by the SDK because the
	// orchestrator holds no flow.
	if e.Awaits != (flow.Awaits{Role: "reviewer"}) {
		t.Errorf("Awaits = %+v, want the successor's role with no account", e.Awaits)
	}
	if e.By != claim.Account {
		t.Errorf("By = %q, want the claim's account %q", e.By, claim.Account)
	}
	if e.Role != "contributor" {
		t.Errorf("Role = %q, want the step's own tag", e.Role)
	}
	if e.Result.Type != flow.ArtifactMarkdown || e.Result.Markdown != "the plan" {
		t.Errorf("Result = %+v, want the markdown the handler returned", e.Result)
	}
	if e.Spend.CostUSD != 1.25 {
		t.Errorf("Spend.CostUSD = %v, want 1.25 — this execution's cost", e.Spend.CostUSD)
	}
	if e.Spend.Duration <= 0 {
		t.Errorf("Spend.Duration = %v, want the execution's active time", e.Spend.Duration)
	}
	if e.At.IsZero() {
		t.Error("At is zero; an entry records when the execution completed")
	}
}

// A finalizing election awaits nobody, and carries the disposition through to
// Finalize.
func TestCompletion_FinalizingEntryAwaitsNobody(t *testing.T) {
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionRejected, "not worth doing").Markdown("why not"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Entry: true, Role: "contributor", MayFinalize: []flow.Disposition{flow.DispositionRejected}})
	})

	if _, err := RunOne(context.Background(), app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if len(be.entries) != 1 {
		t.Fatalf("appended %d entries, want one", len(be.entries))
	}
	e := be.entries[0]
	if e.Route.Finalize != flow.DispositionRejected {
		t.Errorf("Route = %+v, want a finalizing election", e.Route)
	}
	if !e.Awaits.Empty() {
		t.Errorf("Awaits = %+v on a finalizing entry, want the zero value", e.Awaits)
	}
}

// A step the route reaches again appends a SECOND execution. The number is
// counted off the JOURNAL — the record — rather than off a stored counter,
// which would be a second answer to a question the entries already settle.
func TestExecutionOf_CountsPriorEntriesForThatStep(t *testing.T) {
	empty := &flow.Item{}
	if got := executionOf(empty, "plan"); got != 1 {
		t.Errorf("executionOf on an empty journal = %d, want 1", got)
	}

	state := &flow.Item{Journal: []flow.JournalEntry{
		{Step: "plan", Execution: 1},
		{Step: "impl", Execution: 1},
		{Step: "plan", Execution: 2},
		{Step: "review", Execution: 1},
	}}
	if got := executionOf(state, "plan"); got != 3 {
		t.Errorf("executionOf(plan) = %d, want 3 — two prior executions plus this one", got)
	}
	// Another step's entries are not this step's.
	if got := executionOf(state, "impl"); got != 2 {
		t.Errorf("executionOf(impl) = %d, want 2", got)
	}
	if got := executionOf(state, "never-run"); got != 1 {
		t.Errorf("executionOf(never-run) = %d, want 1", got)
	}
}

// A step that elects nothing parks `step-did-not-complete` and appends NOTHING:
// only completion appends, and a park is recorded beside the journal.
func TestCompletion_ZeroResultAppendsNothing(t *testing.T) {
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		f.AddStep("forgetful", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return flow.StepResult{}, nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkStepDidNotComplete {
		t.Fatalf("res = %+v, want parked step-did-not-complete", res)
	}
	if len(be.entries) != 0 {
		t.Errorf("appended %+v for a step that completed nothing", be.entries)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if len(state.Journal) != 0 {
		t.Errorf("journal = %+v, want empty — the park is recorded beside it", state.Journal)
	}
	// The dispatch is still counted: the step ran, it just decided nothing.
	if got := state.Ledger.Row("plan").Dispatches; got != 1 {
		t.Errorf("Dispatches = %d, want 1", got)
	}
}

// A dispatch that picks the item up from a park on this very step is a
// RESUMPTION, counted apart from dispatches: one number says how often the step
// was attempted, the other how often something had to unstick it.
func TestRunOne_ResumingAParkRecordsOneResumption(t *testing.T) {
	runs := 0
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			runs++
			if runs == 1 {
				return flow.StepResult{}, nil // elects nothing → parks
			}
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, &stubAgent{name: "stub"})

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "parked" {
		t.Fatalf("first RunOne = (%+v, %v), want parked", res, err)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if got := state.Ledger.Row("plan").Resumptions; got != 0 {
		t.Fatalf("Resumptions = %d before any resume, want 0", got)
	}

	if res, err := RunOne(context.Background(), app, claim); err != nil || res.Status != "done" {
		t.Fatalf("second RunOne = (%+v, %v), want done", res, err)
	}
	state, _ = be.Load(context.Background(), claim.ItemRef)
	row := state.Ledger.Row("plan")
	if row.Resumptions != 1 {
		t.Errorf("Resumptions = %d, want 1 — the second dispatch picked the item up from a park", row.Resumptions)
	}
	if row.Dispatches != 2 {
		t.Errorf("Dispatches = %d, want 2 — a resumption is counted APART from the dispatch, not instead of it", row.Dispatches)
	}
}

// A dispatch that is not resuming anything records no resumption.
func TestRunOne_AnOrdinaryDispatchRecordsNoResumption(t *testing.T) {
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, &stubAgent{name: "stub"})

	if _, err := RunOne(context.Background(), app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if got := state.Ledger.Row("plan").Resumptions; got != 0 {
		t.Errorf("Resumptions = %d, want 0 — nothing was resumed", got)
	}
}
