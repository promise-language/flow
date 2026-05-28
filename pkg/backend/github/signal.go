package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/promise-language/flow"
)

// claimBranch returns the conventional branch name for the issue's claim
// work. v1 uses a single naming convention: flow/issue-<N>.
func (b *Backend) claimBranch(issueNum int) string {
	return fmt.Sprintf("flow/issue-%d", issueNum)
}

// findPRForBranch returns the PR with head.ref equal to branch in the
// repository (any state). Returns (nil, nil) if none exists.
func (b *Backend) findPRForBranch(ctx context.Context, branch string) (*github.PullRequest, error) {
	head := b.cfg.Owner + ":" + branch
	prs, _, err := b.gh.PullRequests.List(ctx, b.cfg.Owner, b.cfg.Repo, &github.PullRequestListOptions{
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
func (b *Backend) refreshPRSignals(ctx context.Context, issueNum int, state *flow.ItemState) error {
	pr, err := b.findPRForBranch(ctx, b.claimBranch(issueNum))
	if err != nil {
		return err
	}
	now := nowUTC()
	if pr == nil {
		// No PR yet — signals stay unset.
		return nil
	}
	merged := pr.GetMerged()
	open := pr.GetState() == "open"
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
func (b *Backend) prHasApprovedReview(ctx context.Context, prNum int) (bool, error) {
	reviews, _, err := b.gh.PullRequests.ListReviews(ctx, b.cfg.Owner, b.cfg.Repo, prNum, &github.ListOptions{PerPage: 100})
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

// markSignalSetOnState updates the in-memory state and the persisted state
// comment to reflect a backend-internal side-effect signal write (e.g.,
// Open succeeded → pr-open=true).
func (b *Backend) markSignalSetOnState(ctx context.Context, claim flow.Claim, issueNum int, sig flow.SignalId) error {
	tok, err := b.loadClaimToken(claim)
	if err != nil {
		return err
	}
	state, err := b.LoadState(ctx, claim)
	if err != nil {
		return err
	}
	state.Signals[sig] = flow.SignalState{Set: true, ObservedAt: nowUTC(), By: "side-effect"}
	doc := docFromState(state.Item.Flow, state, nowUTC())
	if tok.StateCommentID == 0 {
		return errors.New("github: cannot mark signal — no state comment id on claim")
	}
	_, err = b.updateStateComment(ctx, issueNum, tok.StateCommentID, doc, claim.Owner)
	return err
}

// observedAt is a helper used by tests to provide deterministic timestamps
// when refreshing signals. Kept here so signal.go is self-contained.
var observedAt = func() time.Time { return nowUTC() }
