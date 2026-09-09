package issue

import (
	"fmt"
	"strings"

	"github.com/promise-language/flow"
)

// ---------------------------------------------------------------------------
// Maintainer step: the judgement the role boundary lands on.
// ---------------------------------------------------------------------------

// stepReviewProposal judges the proposal as what will land, and its election IS
// the decision (docs/issue-flow.md § Review the proposal).
//
// The three routes are the three honest outcomes: on to the merge result, back
// to implement with what must change, or a rejection that ends the item. Each
// carries the review's own prose as the artifact, so a reader meets the
// reasoning wherever they come across the decision — the route says what was
// decided, and the record says why.
//
// It acquires NO worktree. The step declares Writes{}, and acquiring the
// worktree is what takes the write-contract snapshot: a step that touched the
// tree to read what it judges would be checked against a contract permitting
// nothing, and would fail on the reading rather than on the judgement. What it
// judges reaches it as records — the plan, the briefings, the gate result the
// request carried.
func (b *builder) stepReviewProposal(ctx flow.StepCtx) (flow.StepResult, error) {
	// A request already merged — a human integrated by hand — is not an anomaly
	// and is not a judgement to make: the decision this step exists for was
	// taken outside the flow and acted on. So the observation elects the route
	// onward, with no agent turn, and the record still completes.
	if ctx.Signal(flow.SignalId(StepMerge)) {
		return ctx.Next(flow.StepId(StepVerifyMerge),
			"the pull request is already merged, so the proposal was accepted outside this flow; "+
				"measure the merge result").
			Markdown("The pull request was already merged when this step ran, so the proposal " +
				"was judged and integrated outside this flow. No review turn was spent: the " +
				"decision this step records had already been made and acted on."), nil
	}

	pc, err := b.promptContext(ctx)
	if err != nil {
		return flow.StepResult{}, err
	}
	body, err := renderPrompt(b.cfg, PromptReviewProposal, pc)
	if err != nil {
		return flow.StepResult{}, err
	}
	// Through the shared chokepoint, so this step honours the ask contract and
	// the disclosure-revision loop every other agent-driven step does. A review
	// that discovers it needs a human decision parks on the question like any
	// other step, rather than inventing a fourth outcome.
	//
	// Plan mode: the deliverable is a judgement, and a reviewer that started
	// editing would be doing the contributor's work under the maintainer's tag
	// — and against a write contract that permits nothing.
	resp, err := b.runAgent(ctx, flow.AgentRequest{
		Prompt:         body,
		PermissionMode: "plan",
		Effort:         "high",
	})
	if err != nil {
		return flow.StepResult{}, err
	}
	if strings.TrimSpace(resp.LastText) == "" {
		return flow.StepResult{}, fmt.Errorf("agent returned nothing for %q", PromptReviewProposal)
	}

	decision, summary, block, err := detectProposalDecision(resp.LastText)
	if err != nil {
		return flow.StepResult{}, err
	}
	switch decision {
	case ProposalRework:
		// Back to implement, on the item's existing branch, as further commits.
		// The message is what the contributor acts on, so it carries both halves
		// the sentinel required: the one-line finding and the specifics under
		// it.
		return ctx.Next(flow.StepId(StepImplement), fmt.Sprintf(
			"the proposal needs work before it can land: %s\n\n%s", summary, block)).
			Markdown(resp.LastText), nil
	case ProposalReject:
		// Terminal, and deliberately distinct from a handback: rejection says
		// this item is not resolved by this proposal or any successor to it,
		// with the reasons in the finalizing message (docs/resolution.md
		// § Finalizing).
		return ctx.Finalize(flow.DispositionRejected, fmt.Sprintf(
			"the proposal is rejected and the item is not resolved by it: %s\n\n%s", summary, block)).
			Markdown(resp.LastText), nil
	}
	return ctx.Next(flow.StepId(StepVerifyMerge),
		"the proposal should land; measure the merge result before integrating it").
		Markdown(resp.LastText), nil
}
