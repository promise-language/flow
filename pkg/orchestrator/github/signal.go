package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/promise-language/flow"
)

// claimBranch returns the conventional branch name for the issue's claim
// work. v1 uses a single naming convention: flow/issue-<N>.
func (b *Orchestrator) claimBranch(issueNum int) string {
	return fmt.Sprintf("flow/issue-%d", issueNum)
}

// findPRForBranch returns the PR with head.ref equal to branch in the
// repository (any state). Returns (nil, nil) if none exists.
func (b *Orchestrator) findPRForBranch(ctx context.Context, branch string) (*github.PullRequest, error) {
	head := b.cfg.Owner + ":" + branch
	prs, err := b.out.ListPullRequests(ctx, &github.PullRequestListOptions{
		State:       "all",
		Head:        head,
		ListOptions: github.ListOptions{PerPage: 5},
	})
	if err != nil {
		return nil, fmt.Errorf("list PRs head=%s: %w", head, err)
	}
	if len(prs) == 0 {
		return nil, nil
	}
	// Return the most recent (List returns newest first by default).
	return prs[0], nil
}

// refreshPRSignals fetches the PR (if any) for the claim branch and
// reconciles the pr-open / pr-merged / pr-closed / pr-approved signals
// in-place on state. Errors are returned for the caller to log; the
// surrounding LoadState path treats them as best-effort.
func (b *Orchestrator) refreshPRSignals(ctx context.Context, issueNum int, state *flow.Item) error {
	pr, err := b.findPRForBranch(ctx, b.claimBranch(issueNum))
	if err != nil {
		return err
	}
	now := nowUTC()
	if pr == nil {
		// No PR yet — signals stay unset.
		return nil
	}
	merged := pr.GetMerged() || pr.MergedAt != nil
	open := pr.GetState() == "open" || state.SignalSet("pr-open")
	closed := pr.GetState() == "closed"
	state.Signals["pr-open"] = flow.SignalState{Set: open, ObservedAt: now, By: "poll"}
	state.Signals["pr-merged"] = flow.SignalState{Set: merged, ObservedAt: now, By: "poll"}
	state.Signals["pr-closed"] = flow.SignalState{Set: closed, ObservedAt: now, By: "poll"}

	// pr-approved: check review state.
	approved, err := b.prHasApprovedReview(ctx, pr.GetNumber())
	if err != nil {
		// Treat review-fetch failure as "approved unknown", leave existing.
		return nil
	}
	state.Signals["pr-approved"] = flow.SignalState{Set: approved, ObservedAt: now, By: "poll"}
	return nil
}

// prHasApprovedReview returns true if the latest review on the PR is APPROVED.
func (b *Orchestrator) prHasApprovedReview(ctx context.Context, prNum int) (bool, error) {
	reviews, err := b.out.ListReviews(ctx, prNum, &github.ListOptions{PerPage: 100})
	if err != nil {
		return false, err
	}
	// Track latest review per reviewer; approved iff at least one reviewer's
	// latest state is APPROVED.
	latest := map[string]string{}
	for _, r := range reviews {
		user := r.GetUser().GetLogin()
		latest[user] = r.GetState()
	}
	for _, st := range latest {
		if strings.ToUpper(st) == "APPROVED" {
			return true, nil
		}
	}
	return false, nil
}

// markSignalSetOnState records an orchestrator-internal side-effect signal write
// (e.g. Open succeeded → pr-open=true) in the persisted state comment.
// It edits the existing document in place rather than rebuilding one from an
// Item snapshot: a rebuild carries only what Item models, so it would
// silently drop everything else the document holds — the park record among
// them, which would let opening a PR quietly unpark a budget-exhausted item.
// It never depends on a cached state-comment id: fetchStateComment resolves the
// comment by scanning the issue when the cache is empty, which is how every
// other mutator already behaves.
func (b *Orchestrator) markSignalSetOnState(ctx context.Context, ref flow.ItemRef, sig flow.SignalId) error {
	return b.mutateStateDoc(ctx, ref, "markSignalSetOnState", func(doc *stateDoc) error {
		entry := stateSignalDoc{
			Id:          string(sig),
			Set:         true,
			ObservedAt:  nowUTC(),
			ObservedVia: "side-effect",
		}
		for i := range doc.Signals {
			if doc.Signals[i].Id == string(sig) {
				doc.Signals[i] = entry
				return nil
			}
		}
		doc.Signals = append(doc.Signals, entry)
		return nil
	})
}

// observedAt is a helper used by tests to provide deterministic timestamps
// when refreshing signals. Kept here so signal.go is self-contained.
var observedAt = func() time.Time { return nowUTC() }
