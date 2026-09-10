package issue

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/cli"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// Open request declares Prompts: none, and the declaration is ENFORCED rather
// than merely stated: the SDK reads a prompt off the chokepoint and parks
// ParkRefused — a park no re-dispatch can clear (docs/flow-registration.md
// § Step configuration).
//
// So the declaration is only worth having if the handler really never asks on
// any path, and the paths that used to ask are exactly the ones a unit test on
// the handler cannot report: a prompt refused off the chokepoint returns an
// error the handler may swallow. These two tests go through the real dispatch,
// where the park is read off the chokepoint instead.

// requestArena is a fake orchestrator whose WORKTREE is this package's, so a
// test can script the refusal that used to be repaired in place. Everything
// else — the journal, the ledger, the park, the claim — is the fake's.
type requestArena struct {
	*fake.Orchestrator
	wt *fakeWorktree
}

func (a *requestArena) Worktree(context.Context, flow.ItemRef) (flow.Worktree, error) {
	return a.wt, nil
}

// atTheRequest positions item 42 where its route reaches open request: the plan
// recorded, the branch cut, the producing steps done. The journal is what
// position is derived from, so the entries are the positioning.
func atTheRequest(t *testing.T, wt *fakeWorktree, agent flow.Agent) (cli.App, *requestArena, flow.Claim) {
	t.Helper()
	ctx := context.Background()
	be := fake.New(resolveSignals()...)
	be.SetSupportedArtifacts(resolveArtifacts()...)
	be.AddItem("42", flow.Item{Type: "task", Title: "widget is broken"})
	arena := &requestArena{Orchestrator: be, wt: wt}

	app, err := BuildApp(ctx, Config{
		BinaryName: "issue",
		VerifyCmd:  []string{"make", "check"},
		Role:       RoleContributor,
		BaseBranch: "main",
	}, Deps{Orchestrator: arena, Agent: agent})
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}

	ref := be.Ref("42")
	claim, err := be.Claim(ctx, ref, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	for _, entry := range []flow.JournalEntry{
		{
			Step:   flow.StepId(StepPlan),
			Result: flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: planned("the plan")},
			Route:  flow.Route{Next: flow.StepId(StepBranch)},
		},
		{
			Step:   flow.StepId(StepBranch),
			Result: flow.ArtifactBody{Type: flow.ArtifactCommitHash, CommitHash: "base"},
			Route:  flow.Route{Next: flow.StepId(StepImplement)},
		},
		{
			Step:   flow.StepId(StepImplement),
			Result: flow.ArtifactBody{Type: flow.ArtifactCommitHash, CommitHash: "sha-1"},
			Route:  flow.Route{Next: flow.StepId(StepReview)},
		},
		{
			Step:   flow.StepId(StepReview),
			Result: flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the review"},
			Route:  flow.Route{Next: flow.StepId(StepCoverage)},
		},
		{
			Step:   flow.StepId(StepCoverage),
			Result: flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the coverage analysis"},
			Route:  flow.Route{Next: flow.StepId(StepOpenPR)},
		},
	} {
		entry.Execution = 1
		entry.Message = "on to the next step"
		entry.Role = flow.RoleName(RoleContributor)
		if err := be.AppendEntry(ctx, ref, entry); err != nil {
			t.Fatalf("AppendEntry %q: %v", entry.Step, err)
		}
	}
	return app, arena, claim
}

// The regression this item would otherwise ship: a commit the pre-commit hook
// refuses used to be repaired in place, and a step declaring none that repairs
// in place parks ParkRefused with the branch untouched — a stop nothing clears.
//
// It parks BLOCKED instead, on the refusal itself, and spends nothing: the
// refused work is still in the tree, so there is nothing to elect (a step that
// ends over a dirty tree has not completed), and the item stops on something a
// person can act on rather than on a mis-declared step.
func TestTheRequestIsMechanicalOnTheCommitRefusalPath(t *testing.T) {
	wt := resumedWorktree()
	wt.commits = 1              // implement already committed
	wt.dirty = []byte("diff\n") // review and coverage changed something
	wt.commitErrs = []error{hookErr}

	agent := &recordingAgent{}
	app, arena, claim := atTheRequest(t, wt, agent)

	res, err := cli.RunOne(context.Background(), &app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	// The kind is the SDK's, not the handler's: the tree still carries the work
	// the hook refused, so the write-contract check parks first and the
	// handler's own ParkBlocked never surfaces. Both are the same stop for the
	// same reason — the step did not complete, because it could not leave the
	// tree committed — and what matters here is the kind it is NOT: a prompt
	// from this step would park refused, which no re-dispatch clears.
	if res.Park == nil || res.Park.Kind == flow.ParkRefused {
		t.Fatalf("res = %+v, want a park that is not %q", res, flow.ParkRefused)
	}
	if res.Park.Kind != flow.ParkWriteContract {
		t.Errorf("park kind = %q, want %q", res.Park.Kind, flow.ParkWriteContract)
	}
	if len(agent.reqs) != 0 {
		t.Errorf("open request sent %d prompt(s) — it declares Prompts: none", len(agent.reqs))
	}
	if res.CostUSD == nil || *res.CostUSD != 0 {
		t.Errorf("cost_usd = %v, want a present zero: the whole point of the declaration", res.CostUSD)
	}
	// The park is on the refusal, and the refusal is kept where it may be kept:
	// with the step, unpublished.
	if strings.Contains(res.Reason, hookErr.Error()) {
		t.Errorf("the park reason quotes what the hook refused: %q", res.Reason)
	}
	wip, err := arena.LoadWorkInProgress(context.Background(), claim.ItemRef, flow.StepId(StepOpenPR))
	if err != nil {
		t.Fatalf("LoadWorkInProgress: %v", err)
	}
	if !strings.Contains(wip, hookErr.Error()) {
		t.Errorf("the step's own record = %q, want the hook's message kept for the next run", wip)
	}
	state, err := arena.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if last, _ := state.LastEntry(); last.Step != flow.StepId(StepCoverage) {
		t.Errorf("the last entry is %q — a park journals nothing, so the request is still pending", last.Step)
	}
}

// The same on the push-refusal path — the one the approved-prompt list named,
// and the reason stepOpenPR was on it.
func TestTheRequestIsMechanicalOnThePushRefusalPath(t *testing.T) {
	wt := resumedWorktree()
	wt.commits = 1
	wt.openErrs = []error{pushRefusal}

	agent := &recordingAgent{}
	app, arena, claim := atTheRequest(t, wt, agent)

	res, err := cli.RunOne(context.Background(), &app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Park != nil && res.Park.Kind == flow.ParkRefused {
		t.Fatalf("the dispatch parked %q: %s", res.Park.Kind, res.Reason)
	}
	if len(agent.reqs) != 0 {
		t.Errorf("open request sent %d prompt(s) — it declares Prompts: none", len(agent.reqs))
	}
	state, err := arena.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	last, _ := state.LastEntry()
	if last.Step != flow.StepId(StepOpenPR) || last.Route.Next != flow.StepId(StepRepairDisclosure) {
		t.Errorf("the last entry is %q → %q, want the request electing the repair", last.Step, last.Route.Next)
	}
}

// The round, driven end to end through the real dispatcher: refused, repaired,
// proposed. Three properties live only here, and every one of them is invisible
// to a handler-level test.
//
//   - THE ROUTE IS NAVIGABLE. Both edges are new and one of them is a back
//     edge. A handler returning ctx.Next(StepRepairDisclosure) proves nothing
//     about whether the next dispatch derives that step and runs it.
//   - THE REPAIR'S DECLARATION COVERS WHAT IT DOES. It prompts, it commits, it
//     leaves the item's branch, and it resolves a flag. Declare any of those
//     short and the dispatch stops — refused, write-contract, mismatch — where
//     a handler test sees a clean return and a payload nobody checked.
//   - THE JOURNAL RECORDS THE REPAIR. This is the half of #321 the split was
//     most for: a resolution that rewrote its own history to get a push out
//     used to record one pr-open and no trace of why.
//
// The bound on the cycle is asserted where it is decided, on the handler
// (TestStepOpenPR_RefusedAfterARepairRound_Parks). What is asserted here is
// that the ordinary round REACHES the proposal: a repair that answers the
// refusal leaves the request able to push, and the item leaves the cycle by the
// edge it came in by.
func TestTheRepairRoundIsJournalledAndEndsAtTheProposal(t *testing.T) {
	ctx := context.Background()
	wt := resumedWorktree()
	wt.commits = 1
	// Refused once at the push, and once more when the repair asks the guard
	// what the push would carry. Both slices are then exhausted, which is the
	// rewritten history: the third dispatch's push is not refused.
	wt.openErrs = []error{pushRefusal}
	wt.examineErrs = []error{pushRefusal}

	agent := &recordingAgent{}
	app, arena, claim := atTheRequest(t, wt, agent)

	for i, want := range []struct {
		step, next flow.StepId
	}{
		{flow.StepId(StepOpenPR), flow.StepId(StepRepairDisclosure)},
		{flow.StepId(StepRepairDisclosure), flow.StepId(StepOpenPR)},
		{flow.StepId(StepOpenPR), flow.StepId(StepCloseBranch)},
	} {
		res := runStep(t, app)
		if res.Status != string(flow.StatusDone) {
			t.Fatalf("dispatch %d: %s → %s: %s", i+1, res.Step, res.Status, res.Reason)
		}
		state, err := arena.Load(ctx, claim.ItemRef)
		if err != nil {
			t.Fatalf("dispatch %d: Load: %v", i+1, err)
		}
		last, ok := state.LastEntry()
		if !ok {
			t.Fatalf("dispatch %d journalled nothing", i+1)
		}
		if last.Step != want.step || last.Route.Next != want.next {
			t.Fatalf("dispatch %d journalled %q → %q, want %q → %q",
				i+1, last.Step, last.Route.Next, want.step, want.next)
		}
		if last.Step == flow.StepId(StepRepairDisclosure) {
			// The entry is the trace, and it says a repair happened without
			// saying what was refused: it is published, through the guard that
			// refused it (docs/disclosure.md § A refusal does not travel).
			if strings.Contains(last.Message, pushRefusal.Reason.Error()) {
				t.Errorf("the journalled repair quotes what the guard refused: %q", last.Message)
			}
			if last.Result.Type != flow.ArtifactFlag {
				t.Errorf("the repair recorded a %v, want a flag", last.Result.Type)
			}
		}
	}

	// One prompt across the whole round, and it is the repair's. The request
	// declares none on both of its dispatches; the repair is where the turn was
	// always spent, and now it is where the record says it was spent.
	if len(agent.reqs) != 1 {
		t.Fatalf("the round sent %d prompt(s), want 1 (the repair's)", len(agent.reqs))
	}
	if !strings.Contains(agent.reqs[0].Prompt, pushRefusal.Reason.Error()) {
		t.Errorf("the repair prompt does not carry the guard's answer: %q", agent.reqs[0].Prompt)
	}
	// And nothing the guard refused is anywhere in what the round PUBLISHED.
	// Each half of the route was checked for it above; this is the whole record
	// the item carries away, which is what the guard would see again.
	state, err := arena.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, entry := range state.Journal {
		if strings.Contains(entry.Message, pushRefusal.Reason.Error()) ||
			strings.Contains(entry.Result.Markdown, pushRefusal.Reason.Error()) {
			t.Errorf("the journalled %q carries what the guard refused: %+v", entry.Step, entry)
		}
	}
	if !wt.opened {
		t.Error("the round ended without the request being opened")
	}
	if wt.examines != 1 {
		t.Errorf("the guard was asked about a push %d times, want 1 — one repair round", wt.examines)
	}
}

// runStep advances the item one step the way an operator does, through the
// binary's own `run-step` command.
//
// Not cli.RunOne: the App a caller holds is not the App the binary runs. The
// lookups a dispatch reads — the declared artifact type among them — are built
// by the startup gate, which only the entry point runs, so an artifact election
// dispatched through RunOne is judged against a type nobody declared and
// refused as a mismatch (#335). Every step in this round elects one.
func runStep(t *testing.T, app cli.App) flow.InvocationResult {
	t.Helper()
	var out, errOut bytes.Buffer
	app.Out, app.Err = &out, &errOut
	cli.RunWithArgs(app, []string{"run-step", "--json"})
	var res flow.InvocationResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("run-step: %v (stdout %q, stderr %q)", err, out.String(), errOut.String())
	}
	return res
}

// The mirror, and it is what makes the two above mean something: with the SAME
// declaration the shipped graph gives open request, a handler that DOES prompt
// parks ParkRefused. The declaration is read off the real registration rather
// than restated, so this cannot pass by testing a step nobody ships.
func TestAPromptFromTheRequestsDeclarationParksRefused(t *testing.T) {
	shipped, ok := (&builder{cfg: Config{}, role: RoleContributor}).
		resolveFlow(Config{}).ItemByResult(flow.StepId(StepOpenPR))
	if !ok {
		t.Fatal("the flow does not register open request")
	}

	wt := resumedWorktree()
	wt.commits = 1
	agent := &recordingAgent{}
	be := fake.New(resolveSignals()...)
	be.SetSupportedArtifacts(resolveArtifacts()...)
	be.AddItem("42", flow.Item{Type: "task", Title: "widget is broken"})
	arena := &requestArena{Orchestrator: be, wt: wt}

	f := flow.NewFlow("resolve", []flow.ItemType{"task"})
	declareRoles(f, RoleContributor)
	f.AddSignalStep(shipped.Description, flow.SignalId(StepOpenPR),
		func(ctx flow.StepCtx) (flow.StepResult, error) {
			// Exactly what the old handler did on the refusal path, and it must
			// not matter that this one swallows the refusal: the park is read
			// off the chokepoint, not off what the handler returned.
			_, _ = ctx.Agent().Run(ctx.Context(), flow.AgentRequest{Prompt: "rewrite the history"})
			return ctx.Finalize(flow.DispositionResolved, "opened anyway"), nil
		},
		flow.StepConfig{
			Role:        shipped.Role,
			Entry:       true,
			Prompts:     shipped.Prompts,
			Needs:       shipped.Needs,
			Writes:      shipped.Writes,
			Leaves:      shipped.Leaves,
			MayFinalize: []flow.Disposition{flow.DispositionResolved},
		})

	app := cli.App{
		Name:         "issue",
		Orchestrator: arena,
		Agent:        agent,
		Artifacts:    resolveArtifacts(),
		Signals:      resolveSignals(),
		Flow:         f,
		VerifyCmd:    "make check",
	}
	claim, err := be.Claim(context.Background(), be.Ref("42"), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	res, err := cli.RunOne(context.Background(), &app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkRefused {
		t.Fatalf("res = %+v, want parked %q: the declaration holds whatever the handler does with the refusal",
			res, flow.ParkRefused)
	}
	if len(agent.reqs) != 0 {
		t.Errorf("the prompt reached the agent: %+v — the refusal comes before anything is sent", agent.reqs)
	}
}

// recordingAgent fails the point of these tests loudly if it is ever reached:
// it records every request and answers nothing useful.
type recordingAgent struct{ reqs []flow.AgentRequest }

func (a *recordingAgent) Name() string { return "recording" }
func (a *recordingAgent) Run(_ context.Context, req flow.AgentRequest) (*flow.AgentResponse, error) {
	a.reqs = append(a.reqs, req)
	return &flow.AgentResponse{LastText: "done", SessionID: "session-1"}, nil
}
