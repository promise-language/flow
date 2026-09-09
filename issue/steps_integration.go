package issue

import (
	"fmt"

	"github.com/promise-language/flow"
)

// ---------------------------------------------------------------------------
// Integration step set.
//
// Three steps that carry a proposed change to a merged one: verify the merge
// result, merge, record the merge commit. They exist as a phase that can be
// composed after the contributor steps (carry-through) or — eventually — run
// by a separate maintainer principal.
// ---------------------------------------------------------------------------

// stepVerifyMerge measures the MERGE RESULT, not the branch. A branch that
// was green when proposed can be red after merging, because the mainline moved
// underneath it. Verifying the branch again would re-establish something
// already known and miss the thing that changed.
func (b *builder) stepVerifyMerge(ctx flow.StepCtx) (flow.StepResult, error) {
	if err := b.onClaimBranch(ctx); err != nil {
		return flow.StepResult{}, err
	}
	wt, err := ctx.Worktree()
	if err != nil {
		return flow.StepResult{}, err
	}
	base, err := b.baseBranch(ctx.Context())
	if err != nil {
		return flow.StepResult{}, err
	}

	ctx.Notify("", "this binary carries through to merge — this is not independent review")

	// Prepare the merge result so the gate measures what will actually land
	// rather than the branch in isolation. These are direct calls now: the two
	// type assertions they used to go through tested for interfaces that no
	// longer exist — PrepareMergeResult, RevertMergePrep and RebuildTools are
	// part of the one RequestManager capability, so an orchestrator that lands
	// through pull requests has all of them or none.
	if rq := wt.Request(); rq != nil {
		if err := rq.PrepareMergeResult(ctx.Context(), flow.BranchName(base)); err != nil {
			return flow.StepResult{}, fmt.Errorf("could not simulate merge result against %s: %w", base, err)
		}
		defer func() {
			if rerr := rq.RevertMergePrep(ctx.Context()); rerr != nil {
				ctx.Notify("", "could not revert merge simulation: "+rerr.Error())
			}
		}()

		// The merge brought main's tree into the worktree — tool source may
		// have changed, making compiled binaries stale. Rebuild before running
		// the gate so the staleness check does not refuse to measure.
		if err := rq.RebuildTools(ctx.Context()); err != nil {
			return flow.StepResult{}, fmt.Errorf("could not rebuild tools against the merge result: %w", err)
		}
	}

	verdict, err := b.runIntegrationGate(ctx, wt, "the merge result")
	if err != nil {
		return flow.StepResult{}, err
	}

	return ctx.Next(flow.StepId(StepMerge),
		"the merge result passes the integration gate; merge the pull request").
		Markdown(gateSection(verdict)), nil
}

// stepMerge merges the pull request. The pr-merged signal is set by the
// backend as a side effect of Merge succeeding.
func (b *builder) stepMerge(ctx flow.StepCtx) (flow.StepResult, error) {
	wt, err := ctx.Worktree()
	if err != nil {
		return flow.StepResult{}, err
	}
	info, err := flow.FindPR(ctx.Context(), wt)
	if err != nil {
		return flow.StepResult{}, fmt.Errorf("could not find the pull request for the claim branch: %w", err)
	}
	if err := flow.Merge(ctx.Context(), wt, info.URL); err != nil {
		return flow.StepResult{}, err
	}
	return ctx.Next(flow.StepId(StepRecordMerge), fmt.Sprintf(
		"the pull request at %s is merged; record the merge commit", info.URL)), nil
}

// stepRecordMerge records the merge commit SHA as the final artifact.
func (b *builder) stepRecordMerge(ctx flow.StepCtx) (flow.StepResult, error) {
	wt, err := ctx.Worktree()
	if err != nil {
		return flow.StepResult{}, err
	}
	info, err := flow.FindPR(ctx.Context(), wt)
	if err != nil {
		return flow.StepResult{}, fmt.Errorf("could not find the pull request for the claim branch: %w", err)
	}
	if info.MergeCommitSHA == "" {
		// Reaching this step means pr-merged is set — a merge is what elects
		// it, whether the flow performed it or a human integrated by hand. So
		// this is not a merge still queued behind something: Merge merges, and
		// returns having done it. It is the commit not yet readable, which a
		// later pass reads once the backend reports it.
		return flow.StepResult{}, fmt.Errorf(
			"the pull request at %s is merged but reports no merge commit yet — "+
				"the backend has not published it; a later pass records it",
			info.URL)
	}
	return ctx.Next(flow.StepId(StepCloseBranch), fmt.Sprintf(
		"the change is merged as %s; return the worktree to the base branch",
		info.MergeCommitSHA)).CommitHash(string(info.MergeCommitSHA)), nil
}
