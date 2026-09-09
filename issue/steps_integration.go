package issue

import (
	"fmt"

	"github.com/promise-language/flow"
)

// ---------------------------------------------------------------------------
// Integration steps.
//
// The three that carry a proposal the maintainer's review accepted to a merged
// change: verify the merge result, merge, record the merge commit. They sit
// behind that review in the one graph, so which principal runs them is a
// question about coverage rather than about which steps exist — the same
// account continuing across the boundary, or a maintainer picking the item up
// where the contributor left it.
//
// The judgement that routes here is steps_maintainer.go's.
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

	// The caveat belongs to the ARRANGEMENT, not to the step. It was
	// unconditional while these steps existed only in a carry-through
	// composition; on the one graph an independent maintainer reaches them too,
	// and telling that operator their review is not independent is false in the
	// direction that matters. What carrying through does not provide — "a single
	// principal reviewing their own agent's work is not independent review" —
	// is said by the binary that is doing it (docs/resolution-standalone.md
	// § Declaring what a binary may do).
	if b.cfg.CarryThrough {
		ctx.Notify("", "this binary carries through to merge — this is not independent review")
	}

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
//
// A request ALREADY merged is not merged again. The hand-integrated path is
// documented rather than exceptional — "a merged request found already in place
// — a human integrated by hand — is not an anomaly" (docs/issue-flow.md
// § Review the proposal) — and the route reaches this step regardless, because
// a step runs when the route names it and for no other reason. Calling
// flow.Merge on a merged request is how that path dies at the last step it had
// nothing left to do.
//
// The already-merged fact is read off the signal, which is this step's own
// result: the backend observes it from the request's state on every load, so a
// merge performed by a person is visible here exactly as one performed by this
// step. flow.PRInfo carries no merged flag to read instead, and its
// MergeCommitSHA is not one — an open request has a test-merge sha, so a guard
// on that would skip every merge this flow was asked to make.
func (b *builder) stepMerge(ctx flow.StepCtx) (flow.StepResult, error) {
	wt, err := ctx.Worktree()
	if err != nil {
		return flow.StepResult{}, err
	}
	info, err := flow.FindPR(ctx.Context(), wt)
	if err != nil {
		return flow.StepResult{}, fmt.Errorf("could not find the pull request for the claim branch: %w", err)
	}
	if ctx.Signal(flow.SignalId(StepMerge)) {
		return ctx.Next(flow.StepId(StepRecordMerge), fmt.Sprintf(
			"the pull request at %s was already merged, so this step performed no merge; "+
				"record the merge commit", info.URL)), nil
	}
	if err := flow.Merge(ctx.Context(), wt, info.URL); err != nil {
		return flow.StepResult{}, err
	}
	return ctx.Next(flow.StepId(StepRecordMerge), fmt.Sprintf(
		"the pull request at %s is merged; record the merge commit", info.URL)), nil
}

// stepRecordMerge records the merge commit SHA and finalizes the item as
// resolved. What landed has exactly one name, and this is where it is written
// down — the last thing the resolution owes.
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
	// The one election that ends the item resolved. Finalizing is what completes
	// a flow — there is no checklist that decides it (docs/resolution.md
	// § Finalizing) — and the worktree is not returned here: the contributor
	// already did that at close branch, and this step's declared contract leaves
	// the tree as it found it.
	return ctx.Finalize(flow.DispositionResolved, fmt.Sprintf(
		"the change is merged as %s, which resolves the item",
		info.MergeCommitSHA)).CommitHash(string(info.MergeCommitSHA)), nil
}
