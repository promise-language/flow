package issue

import (
	"errors"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// A handler completes by electing its route, and the election is what the next
// step is run on: the successor, and the message telling it why. These assert
// the edges the shipped graph declares — a handler that elected anything else
// would be refused at capture, which is the whole point of writing the edges
// down (issue/build.go resolveFlow).

// wantNext is the one assertion shape: the elected successor, and a message
// that is not empty — "every election carries its message"
// (docs/resolution.md § Routing).
func wantNext(t *testing.T, res flow.StepResult, want flow.StepId) {
	t.Helper()
	if res.Route.Next != want {
		t.Errorf("elected %q, want %q", res.Route.Next, want)
	}
	if strings.TrimSpace(res.Message) == "" {
		t.Errorf("elected %q with an empty message — the successor is told why it runs", want)
	}
}

func TestElection_PlanRoutesToTheBranch(t *testing.T) {
	agent := &scriptedAgent{replies: []string{planned("the plan")}}
	res, err := testBuilder(t).stepPlan(ctxWithPlan(newFakeWorktree(), agent))
	if err != nil {
		t.Fatalf("stepPlan: %v", err)
	}
	wantNext(t, res, flow.StepId(StepBranch))
}

func TestElection_OpenBranchRoutesToImplement(t *testing.T) {
	wt := newFakeWorktree()
	wt.exists["main"] = true
	res, err := testBuilder(t).stepOpenBranch(ctxWithPlan(wt, &scriptedAgent{}))
	if err != nil {
		t.Fatalf("stepOpenBranch: %v", err)
	}
	wantNext(t, res, flow.StepId(StepImplement))
	if !strings.Contains(res.Message, string(wt.head)) {
		t.Errorf("message = %q, want it to carry the commit the branch was cut from", res.Message)
	}
}

func TestElection_ImplementRoutesToReview(t *testing.T) {
	wt := resumedWorktree()
	ctx := ctxWithPlan(wt, &scriptedAgent{replies: []string{"done"}})
	res, err := testBuilder(t).stepImplement(ctx)
	if err != nil {
		t.Fatalf("stepImplement: %v", err)
	}
	wantNext(t, res, flow.StepId(StepReview))
}

func TestElection_ReviewRoutesToCoverage(t *testing.T) {
	ctx := ctxWithPlan(resumedWorktree(), &scriptedAgent{replies: []string{"the review"}})
	res, err := testBuilder(t).stepReview(ctx)
	if err != nil {
		t.Fatalf("stepReview: %v", err)
	}
	wantNext(t, res, flow.StepId(StepCoverage))
}

func TestElection_CoverageRoutesToTheRequest(t *testing.T) {
	ctx := ctxWithPlan(resumedWorktree(), &scriptedAgent{replies: []string{"the coverage analysis"}})
	res, err := testBuilder(t).stepCoverage(ctx)
	if err != nil {
		t.Fatalf("stepCoverage: %v", err)
	}
	wantNext(t, res, flow.StepId(StepOpenPR))
}

// The request has ONE successor now: the contributor's part ends at the
// proposal whoever is running it, so there is no composition for the edge to
// differ between.
func TestElection_TheRequestRoutesToCloseBranch(t *testing.T) {
	ctx := ctxWithPlan(resumedWorktree(), &scriptedAgent{})
	res, err := testBuilder(t).stepOpenPR(ctx)
	if err != nil {
		t.Fatalf("stepOpenPR: %v", err)
	}
	wantNext(t, res, flow.StepId(StepCloseBranch))
	if res.Payload != nil {
		t.Errorf("payload = %+v — a signal step produces none", res.Payload)
	}
}

// The request opens on the RETRY after a push repair, and that path elects the
// same route as the first. Returning the zero result here instead would park
// the step as "did not complete" with the pull request already open — a park
// the next dispatch cannot clear, because opening it again is not what the
// step would do.
func TestElection_TheRequestElectsAfterAPushRepair(t *testing.T) {
	wt := resumedWorktree()
	refusal := flow.ErrDisclosureRefused{
		Act:    flow.ActPush,
		Reason: errors.New("an absolute home path names the machine's user"),
	}
	wt.openErrs = []error{refusal, nil}

	res, err := testBuilder(t).stepOpenPR(ctxWithPlan(wt, &scriptedAgent{}))
	if err != nil {
		t.Fatalf("stepOpenPR: %v", err)
	}
	wantNext(t, res, flow.StepId(StepCloseBranch))
}

// The integration phase's edges. They are declared by the one graph
// (issue/build_test.go TestResolveFlow_DeclaresTheDocumentedGraph), and these
// are the handlers electing them: a handler and a declaration that disagree
// fail the step at capture, and the step that fails is the one that already
// merged.
func TestElection_VerifyMergeRoutesToTheMerge(t *testing.T) {
	wt := newIntegrationWorktree()
	wt.envelope = []byte(`{"coverage": 95}`)
	wt.thresholds = []byte(`{"coverage": 80}`)

	res, err := testBuilder(t).stepVerifyMerge(newIntegrationCtx(wt))
	if err != nil {
		t.Fatalf("stepVerifyMerge: %v", err)
	}
	wantNext(t, res, flow.StepId(StepMerge))
}

func TestElection_MergeRoutesToRecordingTheCommit(t *testing.T) {
	res, err := testBuilder(t).stepMerge(newIntegrationCtx(newIntegrationWorktree()))
	if err != nil {
		t.Fatalf("stepMerge: %v", err)
	}
	wantNext(t, res, flow.StepId(StepRecordMerge))
	if res.Payload != nil {
		t.Errorf("payload = %+v — merging is a signal step, and signals are not handler-writable", res.Payload)
	}
}

// Recording the merge commit is where the item ends resolved. Finalizing is the
// one way a flow completes — there is no checklist that decides it.
func TestElection_RecordMergeFinalizes(t *testing.T) {
	wt := newIntegrationWorktree()
	wt.prMergeCommitSHA = "abc123"

	res, err := testBuilder(t).stepRecordMerge(newIntegrationCtx(wt))
	if err != nil {
		t.Fatalf("stepRecordMerge: %v", err)
	}
	if res.Route.Finalize != flow.DispositionResolved {
		t.Errorf("finalized as %q, want resolved", res.Route.Finalize)
	}
	if res.Route.Next != "" {
		t.Errorf("elected successor %q as well as finalizing", res.Route.Next)
	}
	if strings.TrimSpace(res.Message) == "" {
		t.Error("finalized with no closing reasons")
	}
	if !strings.Contains(res.Message, "abc123") {
		t.Errorf("message = %q, want it to name what landed", res.Message)
	}
}

// Closing the branch hands the proposal to the maintainer instead of ending the
// item: the contributor's part is complete, the item's is not. Where the
// maintainer is another principal this election is the handoff.
func TestElection_CloseBranchRoutesToTheProposalReview(t *testing.T) {
	wt := resumedWorktree()
	wt.exists["main"] = true
	res, err := testBuilder(t).stepCloseBranch(ctxWithPlan(wt, &scriptedAgent{}))
	if err != nil {
		t.Fatalf("stepCloseBranch: %v", err)
	}
	wantNext(t, res, flow.StepId(StepReviewProposal))
	if res.Route.Finalize != "" {
		t.Errorf("close branch finalized as %q — the proposal has not been judged yet", res.Route.Finalize)
	}
}
