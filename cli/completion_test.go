package cli

import (
	"context"
	"errors"
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
	refuse  error
	// saveErr models a backend that cannot write a work-in-progress record,
	// which is the store the refused-capture path stashes into.
	saveErr error
}

func (b *capturingBackend) AppendEntry(ctx context.Context, ref flow.ItemRef, e flow.JournalEntry) error {
	b.captures = append(b.captures, e.Result)
	b.entries = append(b.entries, e)
	if b.refuse != nil {
		return b.refuse
	}
	return b.Orchestrator.AppendEntry(ctx, ref, e)
}

func (b *capturingBackend) SaveWorkInProgress(ctx context.Context, ref flow.ItemRef, step flow.StepId, body string) error {
	if b.saveErr != nil {
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
		}, flow.StepConfig{Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		}, flow.StepConfig{Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		"undeclared successor": {func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Next("nowhere", "carry on").Markdown("the plan"), nil
			}, flow.StepConfig{Entry: true, Next: []flow.StepId{"review"}})
		}, "does not declare"},
		"wrong payload type": {func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Finalize(flow.DispositionResolved, "done").CommitHash("deadbeef"), nil
			}, flow.StepConfig{Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
		}, "expected markdown"},
		"payload on a signal step": {func(f *flow.Flow) {
			f.AddSignalStep("create pr", "pr-open", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Finalize(flow.DispositionResolved, "done").Markdown("nope"), nil
			}, flow.StepConfig{Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		}, flow.StepConfig{Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		}, flow.StepConfig{Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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

// AppendEntry publishes, so it can refuse. With capture after the handler
// returns there is no in-invocation revision left, so the refusal is stashed
// and the item parks — and the park reason carries the ACT and nothing the
// guard said, because a park is published through that same guard.
func TestCompletion_DisclosureRefusalParksAndKeepsTheWork(t *testing.T) {
	const guardAnswer = "an absolute home path names the machine's user"
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "the plan is written").
				Markdown("the plan mentioning /home/someone/"), nil
		}, flow.StepConfig{Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
	// Nothing was journaled: result and route land together or not at all, and
	// the refusal is the "not at all".
	if len(state.Journal) != 0 {
		t.Errorf("journal = %+v after a refused capture, want empty", state.Journal)
	}
	// "A correction round is priced as a round, not as a dispatch"
	// (docs/resolution.md § The treasurer). Charged as one, three refused
	// sentences would exhaust the default three invocations and park on the
	// budget — reporting a budget cap for a problem no grant can fix.
	if row := state.Ledger.Row("plan"); row.Dispatches != 0 {
		t.Errorf("invocations = %d after a refused capture, want 0 — a refused expression of "+
			"finished work is not a failed attempt at the step", row.Dispatches)
	}
}

// The stash is best-effort, and it has to be: a backend that cannot write the
// record must not cost the item its park as well as its work. Losing the park
// would end the run with nobody told anything, which is the outcome the whole
// refusal path exists to avoid.
func TestCompletion_DisclosureRefusalParksEvenWhenTheStashFails(t *testing.T) {
	tel := &recordingTelemetry{}
	app, be, claim := capturingApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "the plan is written").Markdown("the plan"), nil
		}, flow.StepConfig{Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	})
	app.Telemetry = tel
	be.refuse = flow.ErrDisclosureRefused{Act: flow.ActArtifactComment, Reason: errors.New("a home path")}
	be.saveErr = errors.New("disk went away")

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil || res.Park.Kind != flow.ParkBlocked {
		t.Fatalf("res = %+v, want parked blocked despite the failed stash", res)
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
			}, flow.StepConfig{Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
		}, nil, "done"},
		"decided nothing": {func(f *flow.Flow) {
			f.AddStep("forgetful", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return flow.StepResult{}, nil
			}, flow.StepConfig{Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
		}, nil, "parked"},
		"undeclared election": {func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Next("nowhere", "carry on").Markdown("the plan"), nil
			}, flow.StepConfig{Entry: true, Next: []flow.StepId{"review"}})
		}, nil, "failed"},
		"capture failed for any other reason": {func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Finalize(flow.DispositionResolved, "the plan is written").Markdown("the plan"), nil
			}, flow.StepConfig{Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
	}, flow.StepConfig{Entry: true, Role: "contributor"})
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
		f.Role("contributor", flow.CapPush)
		f.Role("reviewer", flow.CapApprove)
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "p"}); err != nil {
				return flow.StepResult{}, err
			}
			return ctx.Next("commit", "the plan is written").
				Markdown("the plan").
				WithNote("the base branch is release-2"), nil
		}, flow.StepConfig{Entry: true, Role: "contributor", Next: []flow.StepId{"commit"}})
		f.AddStep("record the commit", "commit", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "done").CommitHash("abc"), nil
		}, flow.StepConfig{Role: "reviewer", MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		f.Role("contributor", flow.CapPush)
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionRejected, "not worth doing").Markdown("why not"), nil
		}, flow.StepConfig{Entry: true, Role: "contributor", MayFinalize: []flow.Disposition{flow.DispositionRejected}})
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
		}, flow.StepConfig{Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		}, flow.StepConfig{Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		}, flow.StepConfig{Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	}, &stubAgent{name: "stub"})

	if _, err := RunOne(context.Background(), app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	state, _ := be.Load(context.Background(), claim.ItemRef)
	if got := state.Ledger.Row("plan").Resumptions; got != 0 {
		t.Errorf("Resumptions = %d, want 0 — nothing was resumed", got)
	}
}
