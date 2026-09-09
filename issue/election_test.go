package issue

import (
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// A handler completes by electing its route, and the election is what the next
// step is run on: the successor, and the message telling it why. These assert
// the edges the shipped compositions declare — a handler that elected anything
// else would be refused at capture, which is the whole point of writing the
// edges down (issue/build.go addContributorSteps).

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

// The pull request's successor is the one edge that differs between the two
// compositions, and the handler elects whichever one it was registered with —
// so the declaration and the election cannot disagree.
func TestElection_TheRequestRoutesWhereItsCompositionSays(t *testing.T) {
	for name, want := range map[string]flow.StepId{
		"proposing":     flow.StepId(StepCloseBranch),
		"carry-through": flow.StepId(StepVerifyMerge),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := ctxWithPlan(resumedWorktree(), &scriptedAgent{})
			res, err := testBuilder(t).stepOpenPR(ctx, want)
			if err != nil {
				t.Fatalf("stepOpenPR: %v", err)
			}
			wantNext(t, res, want)
			if res.Payload != nil {
				t.Errorf("payload = %+v — a signal step produces none", res.Payload)
			}
		})
	}
}

// Closing the branch is where both compositions end, and finalizing is the one
// way a flow completes: there is no checklist that decides it.
func TestElection_CloseBranchFinalizes(t *testing.T) {
	wt := resumedWorktree()
	wt.exists["main"] = true
	res, err := testBuilder(t).stepCloseBranch(ctxWithPlan(wt, &scriptedAgent{}))
	if err != nil {
		t.Fatalf("stepCloseBranch: %v", err)
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
}
