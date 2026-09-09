package issue

import (
	"errors"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// The maintainer's review is the one step in the graph that chooses between
// routes, so what it elects — and what it refuses to elect — is the whole of
// its behaviour. Each election carries the review's prose as the artifact, so
// the record says why wherever the decision is met.

// reviewCtx is a fakeCtx standing where close branch left the item: the
// contributor's results recorded, the worktree back on the base.
func reviewCtx(agent flow.Agent) *fakeCtx {
	return &fakeCtx{
		item:  flow.Item{Ref: itemRefFor("42"), Type: "task", Title: "widget is broken"},
		wt:    newFakeWorktree(),
		agent: agent,
		arts: map[flow.ArtifactId]flow.ArtifactRecord{
			"plan":           {Resolved: true, Type: flow.ArtifactMarkdown, Markdown: planned("the plan")},
			"branch":         {Resolved: true, Type: flow.ArtifactCommitHash, CommitHash: "base"},
			"implementation": {Resolved: true, Type: flow.ArtifactCommitHash, CommitHash: "sha-1"},
			"review":         {Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "looks good"},
			"coverage":       {Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "95%"},
			"branch-closed":  {Resolved: true, Type: flow.ArtifactFlag},
		},
	}
}

// The default: nothing wrong, so the proposal goes on to be measured against
// the mainline it will land on.
func TestStepReviewProposal_AcceptedRoutesToTheMergeResult(t *testing.T) {
	agent := &scriptedAgent{replies: []string{"the plan is implemented and the tests cover it"}}
	res, err := testBuilder(t).stepReviewProposal(reviewCtx(agent))
	if err != nil {
		t.Fatalf("stepReviewProposal: %v", err)
	}
	wantNext(t, res, flow.StepId(StepVerifyMerge))
	if res.Payload == nil || res.Payload.Markdown != "the plan is implemented and the tests cover it" {
		t.Errorf("payload = %+v, want the review's own prose", res.Payload)
	}
}

// The handback. The message is what the contributor acts on, so it carries the
// finding AND the specifics: "a handback with a vague message spends a full
// contributor round to rediscover what this step already knew".
func TestStepReviewProposal_ReworkRoutesBackToImplement(t *testing.T) {
	agent := &scriptedAgent{replies: []string{
		"I read the diff.\n" +
			"PROPOSAL-REWORK: the retry loop has no bound\n" +
			"```\ncli/retry.go:41 loops forever when the backend keeps refusing\n```",
	}}
	res, err := testBuilder(t).stepReviewProposal(reviewCtx(agent))
	if err != nil {
		t.Fatalf("stepReviewProposal: %v", err)
	}
	wantNext(t, res, flow.StepId(StepImplement))
	for _, want := range []string{"the retry loop has no bound", "cli/retry.go:41"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message = %q, want it to carry %q — the contributor acts on this", res.Message, want)
		}
	}
	if res.Payload == nil || res.Payload.Type != flow.ArtifactMarkdown {
		t.Errorf("payload = %+v, want the review recorded as markdown", res.Payload)
	}
}

// The rejection ENDS the item, and the reasons travel in the finalizing
// message: rejection is terminal in a way a handback is not.
func TestStepReviewProposal_RejectFinalizes(t *testing.T) {
	agent := &scriptedAgent{replies: []string{
		"PROPOSAL-REJECT: the item asks for something the normative documents forbid\n" +
			"```\ndocs/design.md §3: no macros. This adds a macro system.\n```",
	}}
	res, err := testBuilder(t).stepReviewProposal(reviewCtx(agent))
	if err != nil {
		t.Fatalf("stepReviewProposal: %v", err)
	}
	if res.Route.Finalize != flow.DispositionRejected {
		t.Errorf("finalized as %q, want rejected", res.Route.Finalize)
	}
	if res.Route.Next != "" {
		t.Errorf("elected successor %q as well as finalizing", res.Route.Next)
	}
	for _, want := range []string{"normative documents forbid", "docs/design.md §3"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message = %q, want the reasons the item was declined to carry %q", res.Message, want)
		}
	}
	if res.Payload == nil {
		t.Error("a rejection still records the review — the reasoning outlives the decision")
	}
}

// Two decisions is not a decision. Picking one here would be this step making
// the choice it exists to record, and the two are opposites: one continues the
// resolution, the other ends it.
func TestStepReviewProposal_BothSentinelsIsRefused(t *testing.T) {
	agent := &scriptedAgent{replies: []string{
		"PROPOSAL-REWORK: bound the loop\n```\ncli/retry.go:41\n```\n" +
			"PROPOSAL-REJECT: or perhaps drop it\n```\ndocs/design.md §3\n```",
	}}
	res, err := testBuilder(t).stepReviewProposal(reviewCtx(agent))
	if err == nil {
		t.Fatalf("stepReviewProposal = %+v, want a refusal naming both decisions", res)
	}
	for _, want := range []string{ProposalReworkSentinel, ProposalRejectSentinel} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err, want)
		}
	}
	if res != (flow.StepResult{}) {
		t.Errorf("elected %+v — a step that decided nothing completes nothing", res)
	}
}

// A turn that said nothing is not a judgement, and letting it through would
// route the proposal to the merge on an empty record.
func TestStepReviewProposal_EmptyTurnFails(t *testing.T) {
	agent := &scriptedAgent{replies: []string{"   \n  "}}
	res, err := testBuilder(t).stepReviewProposal(reviewCtx(agent))
	if err == nil {
		t.Fatalf("stepReviewProposal = %+v, want a refusal on an empty turn", res)
	}
	if !strings.Contains(err.Error(), string(PromptReviewProposal)) {
		t.Errorf("err = %q, want it to name the prompt slot that produced nothing", err)
	}
}

// A request a person merged by hand is not an anomaly: the decision this step
// records was taken and acted on outside the flow, so it elects the route
// onward — and spends no turn doing it.
func TestStepReviewProposal_AlreadyMergedElectsWithoutAnAgentTurn(t *testing.T) {
	agent := &scriptedAgent{replies: []string{"this should never be read"}}
	ctx := reviewCtx(agent)
	ctx.signals = map[flow.SignalId]bool{flow.SignalId(StepMerge): true}

	res, err := testBuilder(t).stepReviewProposal(ctx)
	if err != nil {
		t.Fatalf("stepReviewProposal: %v", err)
	}
	wantNext(t, res, flow.StepId(StepVerifyMerge))
	if agent.calls != 0 {
		t.Errorf("the agent ran %d time(s) — the judgement had already been made and acted on", agent.calls)
	}
	if res.Payload == nil || strings.TrimSpace(res.Payload.Markdown) == "" {
		t.Errorf("payload = %+v, want the observation recorded — the record still completes", res.Payload)
	}
}

// The step declares Writes{}, and acquiring the worktree is what takes the
// write-contract snapshot: a review that reached for the tree would be checked
// against a contract permitting nothing.
func TestStepReviewProposal_AcquiresNoWorktree(t *testing.T) {
	ctx := reviewCtx(&scriptedAgent{replies: []string{"looks right"}})
	ctx.wtErr = errors.New("no worktree here")

	if _, err := testBuilder(t).stepReviewProposal(ctx); err != nil {
		t.Fatalf("stepReviewProposal = %v on a context with no worktree, want the judgement made anyway", err)
	}
}
