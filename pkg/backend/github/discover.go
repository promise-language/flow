package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-github/v68/github"
	"github.com/promise-language/flow"
)

// Discover implements flow.Discoverer: list items visible to the operator
// beyond the narrow eligible set that ListEligible returns. Uses the
// authoritative Issues API (not the eventually-consistent Search API), so
// results are immediately consistent after a label or assignee change.
//
// The method fetches issues in bulk and derives each item's availability
// state, tags, and holder from the issue's labels and assignees — no per-item
// round-trip.
func (b *Backend) Discover(ctx context.Context, scope flow.DiscoveryScope, binaryName string) ([]flow.DiscoveryItem, error) {
	login, err := resolveLogin(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve login: %w", err)
	}

	issues, err := b.fetchIssuesForScope(ctx, scope)
	if err != nil {
		return nil, err
	}

	var items []flow.DiscoveryItem
	for _, iss := range issues {
		if iss.IsPullRequest() {
			continue // the Issues API includes PRs; skip them
		}
		lblNames := labelNamesOf(iss.Labels)
		avail := b.deriveAvailability(lblNames, iss.Assignees, binaryName, login, iss.GetState())
		if !avail.InScope(scope) {
			continue
		}

		ref := b.refFromIssue(iss.GetNumber())
		di := flow.DiscoveryItem{
			BackendName:  ref.BackendName,
			Display:      ref.Display,
			Ref:          ref.Ref,
			Title:        iss.GetTitle(),
			Availability: avail,
			Tags:         lblNames,
		}

		// Holder: the flow:owner:<login> label is the authoritative claim.
		for _, lbl := range lblNames {
			if owner, ok := b.labels.OwnerFromLabel(lbl); ok {
				di.Holder = owner
				break
			}
		}

		// Blocked-by: items referencing blocking issues through labels.
		// GitHub's Sub-issues and "blocked by" features are not first-class
		// in the REST API, so we report the flow:blocked label as the
		// signal and leave BlockedBy empty (no identifiers to report).
		if avail == flow.AvailBlocked {
			di.BlockReason = b.deriveBlockReason(lblNames)
		}

		items = append(items, di)
	}
	return items, nil
}

// fetchIssuesForScope fetches issues from the Issues API with the narrowest
// query the scope allows. Wider scopes issue fewer filters and page more.
func (b *Backend) fetchIssuesForScope(ctx context.Context, scope flow.DiscoveryScope) ([]*github.Issue, error) {
	opt := &github.IssueListByRepoOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	switch scope {
	case flow.ScopeAll:
		opt.State = "all"
	default:
		// open, processable, workable, free, auto — all start from open issues.
		opt.State = "open"
	}

	var all []*github.Issue
	for {
		issues, resp, err := b.out.ListIssues(ctx, opt)
		if err != nil {
			return nil, fmt.Errorf("list issues: %w", err)
		}
		all = append(all, issues...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return all, nil
}

// deriveAvailability computes the availability state for an issue from its
// labels, assignees, and the operator's login.
func (b *Backend) deriveAvailability(
	lblNames []string,
	assignees []*github.User,
	binaryName string,
	myLogin string,
	issueState string,
) flow.Availability {
	if issueState == "closed" {
		return flow.AvailClosed
	}

	// Level 3: processable — this binary's label is present.
	binaryLabel := b.labels.Binary(binaryName)
	if !hasLabel(lblNames, binaryLabel) {
		return flow.AvailUnhandled
	}

	// Level 4: workable — not blocked.
	if b.isBlocked(lblNames) {
		return flow.AvailBlocked
	}

	// Level 5: free — nobody else holds it.
	holder := ""
	for _, lbl := range lblNames {
		if owner, ok := b.labels.OwnerFromLabel(lbl); ok {
			holder = owner
			break
		}
	}
	if holder != "" && holder != myLogin {
		return flow.AvailHeld
	}

	// Level 6: auto — assigned to me AND has the binary label.
	// The ListEligible query is: label:flow:<binary> assignee:@me — the
	// owner label is not part of that query, so auto must not require it.
	// An issue assigned via the GitHub UI (no claim, no owner label) is
	// still auto-selectable by resolve.
	for _, u := range assignees {
		if u.GetLogin() == myLogin {
			return flow.AvailAuto
		}
	}

	return flow.AvailAvailable
}

// isBlocked reports whether the issue carries any label that makes it blocked.
func (b *Backend) isBlocked(lblNames []string) bool {
	blocked := b.labels.Blocked()
	disabled := b.labels.Disabled()
	needsAnswer := b.labels.NeedsAnswer()
	for _, lbl := range lblNames {
		if lbl == blocked || lbl == disabled || lbl == needsAnswer {
			return true
		}
		// Budget-exhausted labels also block.
		if strings.HasPrefix(lbl, b.labels.named(labelSuffixBudgetExhPref)) {
			return true
		}
	}
	return false
}

// deriveBlockReason returns a human-readable reason from the blocking labels.
func (b *Backend) deriveBlockReason(lblNames []string) string {
	for _, lbl := range lblNames {
		switch lbl {
		case b.labels.Blocked():
			return "blocked"
		case b.labels.Disabled():
			return "disabled"
		case b.labels.NeedsAnswer():
			return "needs answer"
		}
		if strings.HasPrefix(lbl, b.labels.named(labelSuffixBudgetExhPref)) {
			step := strings.TrimPrefix(lbl, b.labels.named(labelSuffixBudgetExhPref))
			return fmt.Sprintf("budget exhausted on %q", step)
		}
	}
	return "blocked"
}
