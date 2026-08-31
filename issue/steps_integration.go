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
func (b *builder) stepVerifyMerge(ctx flow.StepCtx) error {
	if err := b.onClaimBranch(ctx); err != nil {
		return err
	}
	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	base, err := b.baseBranch(ctx.Context())
	if err != nil {
		return err
	}

	ctx.Notify("", "this binary carries through to merge — this is not independent review")

	// If the worktree supports merge-result simulation, prepare it so the gate
	// measures the merge result rather than just the branch.
	if prep, ok := wt.(flow.MergeResultPreparer); ok {
		if err := prep.PrepareMergeResult(ctx.Context(), base); err != nil {
			return fmt.Errorf("could not simulate merge result against %s: %w", base, err)
		}
		defer func() {
			if rerr := prep.RevertMergePrep(ctx.Context()); rerr != nil {
				ctx.Notify("", "could not revert merge simulation: "+rerr.Error())
			}
		}()
	}

	ctx.Notify("", fmt.Sprintf("running the %s gate on the merge result", flow.GateIntegration))
	run, err := wt.RunGate(ctx.Context(), flow.GateIntegration)
	if err != nil {
		return fmt.Errorf(
			"no %s gate ran on the merge result, so nothing was measured — this is not the "+
				"change failing: %w", flow.GateIntegration, err)
	}
	if run.Outcome != flow.OutcomeMeasured {
		return fmt.Errorf(
			"the %s gate reports %q on the merge result, so nothing was measured — this is not "+
				"the change failing%s", flow.GateIntegration, run.Outcome, detailSuffix(run.Detail))
	}
	verdict, err := wt.Judge(ctx.Context(), run)
	if err != nil {
		return fmt.Errorf(
			"the %s gate measured the merge result but no verdict exists, which is not a refusal — "+
				"the project's judging layer could not answer: %w", flow.GateIntegration, err)
	}
	if !verdict.Acceptable {
		return fmt.Errorf(
			"the %s gate's verdict on the merge result is not acceptable — a failing gate does "+
				"not land%s", flow.GateIntegration, detailSuffix(verdict.Detail))
	}

	return ctx.ResolveMarkdown(gateSection(verdict))
}

// stepMerge merges the pull request. The pr-merged signal is set by the
// backend as a side effect of Merge succeeding.
func (b *builder) stepMerge(ctx flow.StepCtx) error {
	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	info, err := flow.FindPR(ctx.Context(), wt)
	if err != nil {
		return fmt.Errorf("could not find the pull request for the claim branch: %w", err)
	}
	return flow.Merge(ctx.Context(), wt, info.URL)
}

// stepRecordMerge records the merge commit SHA as the final artifact.
func (b *builder) stepRecordMerge(ctx flow.StepCtx) error {
	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	info, err := flow.FindPR(ctx.Context(), wt)
	if err != nil {
		return fmt.Errorf("could not find the pull request for the claim branch: %w", err)
	}
	if info.MergeCommitSHA == "" {
		return fmt.Errorf(
			"the pull request at %s has not merged yet — the --auto flag may have queued it "+
				"but CI has not finished; the next LoadState cycle will refresh the pr-merged signal",
			info.URL)
	}
	return ctx.ResolveCommitHash(info.MergeCommitSHA)
}
