package issue

import (
	"context"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// The maintainer-side rework handback, end to end, on the shipped graph.
//
// It is the route the defect was reported on (#293, observed on #349), and the
// flow takes it by design: close branch declares Leaves: base and returns the
// worktree to main, review the proposal declares Needs: any and judges there,
// and its rework election routes back to implement — which declares
// Needs: item-branch and {MayCommit, MayEditTree} with no MayBranch.
//
// Nothing re-established the item's branch, so implement acquired the worktree
// on main, checked the branch out itself one line later, and was parked
// "write-contract: branch moved: was \"main\", now \"flow/issue-42\"" — an
// agent-misbehaviour park, on a switch the flow's own helper made, under a kind
// that is deliberately not retried.
//
// The three steps run here in the order the route names them, through the
// binary's own `run-step` so the startup gate's lookups are the ones a real
// dispatch reads.
func TestReworkHandback_ImplementIsReEstablishedOnTheItemBranch(t *testing.T) {
	ctx := context.Background()
	wt := resumedWorktree()
	wt.exists["main"] = true // close branch returns the worktree to a base that is here
	wt.commits = 1           // the contributor's implementation is on the branch

	agent := &scriptedAgent{replies: []string{
		// review the proposal: the handback, in the sentinel the step reads.
		"I read the diff.\n" +
			"PROPOSAL-REWORK: the retry loop has no bound\n" +
			"```\ncli/retry.go:41 loops forever when the backend keeps refusing\n```",
		// implement: the rework round.
		"bounded the loop",
	}}

	be := fake.New(resolveSignals()...)
	be.SetSupportedArtifacts(resolveArtifacts()...)
	be.AddItem("42", flow.Item{Type: "task", Title: "widget is broken"})
	arena := &requestArena{Orchestrator: be, wt: wt}

	app, err := BuildApp(ctx, Config{
		BinaryName: "issue",
		VerifyCmd:  []string{"make", "check"},
		// Both roles, on one account: the crossing is what puts the maintainer's
		// judgement and the contributor's rework in one run, which is where the
		// park was observed.
		Coverage:   []Role{RoleContributor, RoleMaintainer},
		BaseBranch: "main",
	}, Deps{Orchestrator: arena, Agent: agent})
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}

	ref := be.Ref("42")
	if _, err := be.Claim(ctx, ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// The journal IS the position, so the entries are the positioning: the
	// contributor's run, up to the request that elected close branch.
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
		{
			Step:  flow.StepId(StepOpenPR),
			Route: flow.Route{Next: flow.StepId(StepCloseBranch)},
		},
	} {
		entry.Execution = 1
		entry.Message = "on to the next step"
		entry.Role = flow.RoleName(RoleContributor)
		if err := be.AppendEntry(ctx, ref, entry); err != nil {
			t.Fatalf("AppendEntry %q: %v", entry.Step, err)
		}
	}

	// 1. close branch: Leaves: base — the worktree goes back to main, and the
	//    verification at capture is what says it got there.
	res := runStep(t, app)
	if res.Status != string(flow.StatusDone) {
		t.Fatalf("close branch = %+v, want done", res)
	}
	if wt.branch != "main" {
		t.Fatalf("close branch left the worktree on %q, want the base", wt.branch)
	}

	// 2. review the proposal: Needs: any, and it hands back to implement.
	res = runStep(t, app)
	if res.Status != string(flow.StatusDone) {
		t.Fatalf("review the proposal = %+v, want done", res)
	}
	if res.NextStep != string(StepImplement) {
		t.Fatalf("the review elected %q, want the handback to %q", res.NextStep, StepImplement)
	}

	// 3. implement: the step the handback lands on, dispatched from a worktree
	//    sitting on the base. The establishment puts it on the item's branch
	//    before the write-contract snapshot, so the step completes.
	res = runStep(t, app)
	if res.Status != string(flow.StatusDone) {
		t.Fatalf("implement = %+v, want done — a park here is the defect: %s", res, res.Reason)
	}
	if res.Park != nil {
		t.Fatalf("implement parked %q — %q", res.Park.Kind, res.Park.Reason)
	}
	if wt.branch != testBranch {
		t.Errorf("the rework ran on %q, want the item's branch %q", wt.branch, testBranch)
	}
	// The second execution of implement, recorded as its own entry: reaching a
	// step a second time is a route, not an anomaly.
	state, err := be.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var implements int
	for _, e := range state.Journal {
		if e.Step == flow.StepId(StepImplement) {
			implements++
		}
	}
	if implements != 2 {
		t.Errorf("the journal records %d implement entries, want 2 (the first pass and the rework)", implements)
	}
}

// The second half of the same handback: the round ends back at open request,
// and by then the request EXISTS. The push updates it, a second request is
// never opened for the same branch, and the step completes on the request being
// current rather than on it being new (docs/issue-flow.md § Open request).
//
// The whole round runs here, first proposal included, so the second open
// request is reached the way a real run reaches it — through the journal, which
// is what Flow.Position derives the pending step from, and explicitly not the
// signals. The backend's refusal of a second Open is modelled in the worktree
// double: GitHub answers 422 "a pull request already exists for <owner>:<branch>",
// which is the error this test failed on before the step branched.
func TestReworkHandback_TheSecondRoundUpdatesTheRequestRatherThanOpeningASecond(t *testing.T) {
	ctx := context.Background()
	wt := resumedWorktree()
	wt.exists["main"] = true // close branch returns the worktree to a base that is here
	wt.commits = 1           // the contributor's implementation is on the branch

	agent := &scriptedAgent{replies: []string{
		// review the proposal: the handback, in the sentinel the step reads.
		"I read the diff.\n" +
			"PROPOSAL-REWORK: the retry loop has no bound\n" +
			"```\ncli/retry.go:41 loops forever when the backend keeps refusing\n```",
		"bounded the loop",      // implement, the rework round
		"the rework reads well", // review
		"the bound is covered",  // coverage
	}}

	be := fake.New(resolveSignals()...)
	be.SetSupportedArtifacts(resolveArtifacts()...)
	be.AddItem("42", flow.Item{Type: "task", Title: "widget is broken"})
	arena := &requestArena{Orchestrator: be, wt: wt}

	app, err := BuildApp(ctx, Config{
		BinaryName: "issue",
		VerifyCmd:  []string{"make", "check"},
		Coverage:   []Role{RoleContributor, RoleMaintainer},
		BaseBranch: "main",
	}, Deps{Orchestrator: arena, Agent: agent})
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}

	ref := be.Ref("42")
	if _, err := be.Claim(ctx, ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// Positioned one step earlier than the test above: at the FIRST open
	// request, so the request this round must not re-open is one this run really
	// opened.
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

	// The steps of the round, in the order the route names them. Each is
	// dispatched through the binary's own `run-step`, so the lookups are the ones
	// a real dispatch reads.
	for _, step := range []struct {
		name StepID
		next StepID
	}{
		{StepOpenPR, StepCloseBranch},
		{StepCloseBranch, StepReviewProposal},
		{StepReviewProposal, StepImplement}, // the handback
		{StepImplement, StepReview},
		{StepReview, StepCoverage},
		{StepCoverage, StepOpenPR}, // back to the request, and it already exists
		{StepOpenPR, StepCloseBranch},
	} {
		res := runStep(t, app)
		if res.Status != string(flow.StatusDone) {
			t.Fatalf("%s = %+v, want done: %s", step.name, res, res.Reason)
		}
		if res.Park != nil {
			t.Fatalf("%s parked %q — %q", step.name, res.Park.Kind, res.Park.Reason)
		}
		if res.NextStep != string(step.next) {
			t.Fatalf("%s elected %q, want %q", step.name, res.NextStep, step.next)
		}
		// What the orchestrator observes of the request's state once it exists,
		// and where the second round reads the fact from: the backend sets
		// pr-open as a side effect of Open, which the fake has no request surface
		// to notice.
		if step.name == StepOpenPR {
			be.SetSignal("42", flow.SignalId(StepOpenPR), true)
		}
	}

	// The property: one request for the branch, across the whole round.
	var opens, pushes int
	for _, c := range wt.calls {
		switch c {
		case "open":
			opens++
		case "push":
			pushes++
		}
	}
	if opens != 1 {
		t.Errorf("Open was called %d time(s), want exactly 1 — a second request is never "+
			"opened for the same branch; calls = %v", opens, wt.calls)
	}
	// And the second round published all the same: the first round's push lives
	// inside Open — in this double as in the backend — so the one push in the log
	// is the update the rework round made to the request already there.
	if pushes != 1 {
		t.Errorf("Push was called %d time(s), want 1 — the rework round's update; calls = %v",
			pushes, wt.calls)
	}
	if !(wt.callIndex("open") < wt.callIndex("push")) {
		t.Errorf("calls = %v — the updating push did not follow the request it updates", wt.calls)
	}
}

// The same route, with the branch NOT in the worktree: the refusal the retired
// per-handler helper carried is the establishment's now — the item blocks
// naming the step and the state, before the dispatch and before anything is
// charged, rather than failing inside a handler that had already been paid for.
func TestReworkHandback_AMissingItemBranchBlocksBeforeImplementRuns(t *testing.T) {
	ctx := context.Background()
	wt := newFakeWorktree() // no item branch, and sitting on the base
	wt.exists["main"] = true

	agent := &scriptedAgent{replies: []string{"the rework should not be reached"}}
	be := fake.New(resolveSignals()...)
	be.SetSupportedArtifacts(resolveArtifacts()...)
	be.AddItem("42", flow.Item{Type: "task", Title: "widget is broken"})
	arena := &requestArena{Orchestrator: be, wt: wt}

	app, err := BuildApp(ctx, Config{
		BinaryName: "issue",
		VerifyCmd:  []string{"make", "check"},
		Coverage:   []Role{RoleContributor, RoleMaintainer},
		BaseBranch: "main",
	}, Deps{Orchestrator: arena, Agent: agent})
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}

	ref := be.Ref("42")
	if _, err := be.Claim(ctx, ref, nil); err != nil {
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
	} {
		entry.Execution = 1
		entry.Message = "on to the next step"
		entry.Role = flow.RoleName(RoleContributor)
		if err := be.AppendEntry(ctx, ref, entry); err != nil {
			t.Fatalf("AppendEntry %q: %v", entry.Step, err)
		}
	}

	res := runStep(t, app)
	if res.Status != string(flow.StatusParked) || res.Park == nil || res.Park.Kind != flow.ParkBlocked {
		t.Fatalf("implement = %+v, want parked %q", res, flow.ParkBlocked)
	}
	for _, want := range []string{string(StepImplement), string(flow.NeedsItemBranch), testBranch} {
		if !strings.Contains(res.Park.Reason, want) {
			t.Errorf("reason = %q, want it to name %q", res.Park.Reason, want)
		}
	}
	if agent.calls != 0 {
		t.Errorf("the agent ran %d time(s) for a step whose state could not be established, want 0", agent.calls)
	}
	state, err := be.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := state.Ledger.Row(flow.StepId(StepImplement)).Dispatches; got != 0 {
		t.Errorf("Dispatches = %d, want 0: nothing was dispatched, so there is nothing to bill", got)
	}
}
