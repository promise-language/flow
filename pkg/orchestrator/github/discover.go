package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-github/v68/github"
	"github.com/promise-language/flow"
)

// This file holds the ONE derivation behind `List`, `Get` and selection.
//
// docs/orchestrator.md requires Get to "answer identically to List for the same
// item at the same moment — one derivation serving both, never two". Making
// that structural rather than a promise is the whole point of itemInfoFor: an
// item that read `blocked` in `list` and `available` in `status` is a
// contradiction an operator cannot resolve, and nothing in the item caused it.

// List returns items at the given scope, with per-item availability, tags,
// holder and blockers. Uses the authoritative Issues API (not the
// eventually-consistent Search API), so results are immediately consistent
// after a label or assignee change.
//
// The auto-select path must never call it — List reports blocked items, and
// widening auto-select would let a bare `resolve` pick an arbitrary open issue
// and begin work on it.
func (b *Orchestrator) List(ctx context.Context, scope flow.ItemScope, binary flow.BinaryName, acceptsType func(flow.ItemType) bool) ([]flow.ItemInfo, error) {
	issues, err := b.fetchIssuesForScope(ctx, scope)
	if err != nil {
		return nil, err
	}

	var items []flow.ItemInfo
	for _, iss := range issues {
		if iss.IsPullRequest() {
			continue // the Issues API includes PRs; skip them
		}
		info, err := b.itemInfoFor(ctx, iss, binary, acceptsType)
		if err != nil {
			return nil, err
		}
		if !info.Availability.InScope(scope) {
			continue
		}
		items = append(items, info)
	}
	return items, nil
}

// Get answers about one item through the same derivation List uses.
func (b *Orchestrator) Get(ctx context.Context, ref flow.ItemRef, binary flow.BinaryName, acceptsType func(flow.ItemType) bool) (*flow.ItemInfo, error) {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return nil, err
	}
	iss, err := b.out.GetIssue(ctx, issueNum)
	if err != nil {
		return nil, fmt.Errorf("get issue %d: %w", issueNum, err)
	}
	info, err := b.itemInfoFor(ctx, iss, binary, acceptsType)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// ListAutoSelectable returns the issues an unattended `resolve` may start on:
// open, carrying this binary's label, assigned to this account, carrying every
// given tag — and NOT blocked.
//
// The search query narrows server-side; the exact tag match is done here,
// through flow.TagsMatch. Search is case-insensitive and index-lagged, so it is
// a narrowing step and never the comparison: without the post-filter one --tag
// value means two different things across `list` and `resolve`, which are meant
// to read as symmetrical.
func (b *Orchestrator) ListAutoSelectable(ctx context.Context, tags []flow.TagId) ([]flow.ItemRef, error) {
	for _, t := range tags {
		// Refused rather than interpolated. A TagId is spliced into the query
		// below, where a value carrying a space does not fail — it silently
		// becomes a different query, and the caller gets a plausible wrong
		// answer instead of an error.
		if !t.Valid() {
			return nil, fmt.Errorf("github: %q is not a valid tag (a tag is non-empty, single-line, and carries no edge whitespace)", string(t))
		}
	}

	q := fmt.Sprintf("repo:%s/%s is:issue is:open label:%s assignee:@me",
		b.cfg.Owner, b.cfg.Repo, quoteSearchTerm(b.labels.Binary(b.cfg.BinaryName)))
	for _, tag := range tags {
		q += " label:" + quoteSearchTerm(string(tag))
	}
	result, err := b.out.SearchIssues(ctx, q, &github.SearchOptions{ListOptions: github.ListOptions{PerPage: 100}})
	if err != nil {
		return nil, fmt.Errorf("search issues: %w", err)
	}

	refs := make([]flow.ItemRef, 0, len(result.Issues))
	for _, issue := range result.Issues {
		lbls := labelNamesOf(issue.Labels)
		// The contract's comparison, against the labels actually returned.
		if !flow.TagsMatch(tagsOf(lbls), tags) {
			continue
		}
		blockers, err := b.blockersOf(ctx, issue.GetNumber())
		if err != nil {
			return nil, err
		}
		// MUST NOT return a blocked item. The orchestrator that knows about
		// the dependency is the one that keeps it out of the selectable set;
		// a rule enforced in two places is a rule with two owners and one of
		// them wrong.
		if blocked, _, _ := b.blockedness(blockers, lbls); blocked {
			continue
		}
		refs = append(refs, b.refFromIssue(issue.GetNumber()))
	}
	return refs, nil
}

// quoteSearchTerm wraps a search term in quotes so a value carrying a space
// stays one term. Concatenating unquoted is how a tag with a space silently
// became a different query.
func quoteSearchTerm(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// itemInfoFor is the single per-item derivation. List maps it over a search,
// Get calls it on one issue, and ListAutoSelectable uses the same blockedness
// rule underneath.
func (b *Orchestrator) itemInfoFor(ctx context.Context, iss *github.Issue, binary flow.BinaryName, acceptsType func(flow.ItemType) bool) (flow.ItemInfo, error) {
	lblNames := labelNamesOf(iss.Labels)
	itemType := itemTypeFromLabels(b.labels, lblNames, b.cfg.DefaultType)

	blockers, err := b.blockersOf(ctx, iss.GetNumber())
	if err != nil {
		return flow.ItemInfo{}, err
	}
	blocked, kind, reason := b.blockedness(blockers, lblNames)

	info := flow.ItemInfo{
		Ref:         b.refFromIssue(iss.GetNumber()),
		Type:        itemType,
		Title:       iss.GetTitle(),
		Body:        iss.GetBody(),
		URL:         iss.GetHTMLURL(),
		Status:      itemStatusFromIssue(iss),
		Disposition: dispositionFromIssue(iss),
		Holder:      b.holderFromLabels(lblNames),
		Tags:        tagsOf(lblNames),
		BlockedBy:   blockers,
		Blocked:     blocked,
		BlockKind:   kind,
		BlockReason: reason,
		Manual:      hasLabel(lblNames, b.labels.Manual()),
	}
	info.Availability, err = b.availabilityOf(ctx, iss, lblNames, itemType, blocked, binary, acceptsType)
	if err != nil {
		return flow.ItemInfo{}, err
	}
	return info, nil
}

// fetchIssuesForScope fetches issues from the Issues API with the narrowest
// query the scope allows. Wider scopes issue fewer filters and page more.
func (b *Orchestrator) fetchIssuesForScope(ctx context.Context, scope flow.ItemScope) ([]*github.Issue, error) {
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

// availabilityOf computes where the item sits on the listing ladder, FOR THE
// ASKING BinaryName. Each item gets one state at the lowest boundary it fails.
func (b *Orchestrator) availabilityOf(
	ctx context.Context,
	iss *github.Issue,
	lblNames []string,
	itemType flow.ItemType,
	blocked bool,
	binary flow.BinaryName,
	acceptsType func(flow.ItemType) bool,
) (flow.Availability, error) {
	if iss.GetState() == "closed" {
		return flow.AvailClosed, nil
	}

	// Level 3: processable — type acceptance, matching the normative
	// definition of unhandled: "no flow in this binary accepts the type."
	if acceptsType != nil && !acceptsType(itemType) {
		return flow.AvailUnhandled, nil
	}

	// Level 4: workable — not blocked.
	if blocked {
		return flow.AvailBlocked, nil
	}

	account, err := b.resolveAccount(ctx)
	if err != nil {
		return "", err
	}

	// Level 5: free — nobody else holds it. The comparison uses the same
	// account derivation the claim path writes with, so `list` cannot report an
	// item held by a login `claim` would never have written.
	holder := b.holderFromLabels(lblNames)
	if holder.Account != "" && holder.Account != account {
		return flow.AvailHeld, nil
	}

	// Level 6: auto — opted in (binary label present) AND assigned to me.
	// The binary label is applied during seeding. Unseeded items whose type IS
	// accepted fall to available, not auto.
	if hasLabel(lblNames, b.labels.Binary(string(binary))) {
		for _, u := range iss.Assignees {
			if flow.AccountId(u.GetLogin()) == account {
				return flow.AvailAuto, nil
			}
		}
	}

	return flow.AvailAvailable, nil
}

// holderFromLabels reads the claim holder off the flow:owner:<account> label.
//
// The GitHub orchestrator's arena is the local checkout and `.flow/active.json`
// is the whole lease store, so the label carries the ACCOUNT only —
// docs/orchestrator.md grants exactly that ("both halves are implicit… one file
// per checkout does the scoping a fleet-serving orchestrator must do
// explicitly"). The arena half is reported by LookupClaim when this checkout is
// the holder, and is empty otherwise, because no label records it.
func (b *Orchestrator) holderFromLabels(lblNames []string) flow.Holder {
	for _, lbl := range lblNames {
		if account, ok := b.labels.OwnerFromLabel(lbl); ok {
			return flow.Holder{Account: account}
		}
	}
	return flow.Holder{}
}

// tagsOf reports EVERY label as a TagId — the operator's classification and
// this orchestrator's own markers alike, never filtered to what a flow
// recognises.
func tagsOf(lblNames []string) []flow.TagId {
	if len(lblNames) == 0 {
		return nil
	}
	out := make([]flow.TagId, 0, len(lblNames))
	for _, n := range lblNames {
		out = append(out, flow.TagId(n))
	}
	return out
}

// itemStatusFromIssue collapses GitHub's own lifecycle to the two values this
// contract needs: whether more work is possible.
func itemStatusFromIssue(iss *github.Issue) flow.ItemStatus {
	if iss.GetState() == "closed" {
		return flow.StatusTerminal
	}
	return flow.StatusOpen
}

// dispositionFromIssue carries GitHub's own name for the position alongside the
// status, for display. Nothing here interprets it.
func dispositionFromIssue(iss *github.Issue) string {
	if iss.GetState() != "closed" {
		return "open"
	}
	if reason := iss.GetStateReason(); reason != "" {
		return reason // "completed", "not_planned", "reopened"
	}
	return "closed"
}

// blockedness DERIVES whether the item is blocked, who must act, and the
// one-line reason.
//
// Blocked-for-dependency is derived, never stored: the item whose last blocker
// finishes is workable at the next read, with nobody having acted. A stored bit
// beside a stored edge reads as well-formed, selection honours it, and nothing
// ever lifts it.
//
// The park-state labels ARE stored (docs/github-schema.md says so) — but they
// record a park, not a blocked bit, and they are cleared by whatever cleared
// the park.
//
// The reason names the KIND of block and never an item: BlockedBy carries the
// references, and prose repeating them is a second copy nothing can act on and
// nothing updates when a blocker lands.
func (b *Orchestrator) blockedness(blockers []flow.Blocker, lblNames []string) (bool, flow.BlockKind, string) {
	// waits-on-items whenever ANY blocker is still open. It outranks the label
	// causes because it is the one a caller can go act on elsewhere.
	for _, blk := range blockers {
		if blk.Status != flow.StatusTerminal {
			return true, flow.WaitsOnItems, "waiting on unfinished dependencies"
		}
	}
	for _, lbl := range lblNames {
		switch lbl {
		case b.labels.Disabled():
			return true, flow.WaitsOnPerson, "disabled by an operator"
		case b.labels.NeedsAnswer():
			return true, flow.WaitsOnPerson, "waiting for an answer"
		case b.labels.Blocked():
			return true, flow.WaitsOnPerson, "blocked pending operator action"
		}
		if strings.HasPrefix(lbl, b.labels.named(labelSuffixBudgetExhPref)) {
			return true, flow.WaitsOnPerson, "budget exhausted on a step"
		}
	}
	// An infra-transient park clears on its own and there is nothing
	// addressable to go work — the definition of waits-on-condition.
	if hasLabel(lblNames, b.labels.InfraTransient()) {
		return true, flow.WaitsOnCondition, "waiting on a transient infrastructure condition"
	}
	return false, "", ""
}

// blockersOf reports every blocker DECLARED on the issue, each with its own
// ItemStatus.
//
// Through GitHub's issue-dependency API. go-github v68 has no binding for it,
// so it goes through `outward` as a raw request — keeping the chokepoint the
// only thing in this package that talks to GitHub.
//
// A repository with the feature unavailable answers 404/410, which is reported
// as no blockers rather than as an error: an orchestrator with no dependency
// notion reports no blockers and is fully conformant.
func (b *Orchestrator) blockersOf(ctx context.Context, issueNum int) ([]flow.Blocker, error) {
	deps, err := b.out.ListBlockedBy(ctx, issueNum)
	if err != nil {
		return nil, fmt.Errorf("list blockers of #%d: %w", issueNum, err)
	}
	if len(deps) == 0 {
		return nil, nil
	}
	out := make([]flow.Blocker, 0, len(deps))
	for _, d := range deps {
		// Each blocker's status comes WITH it: the list alone answers the wrong
		// question, and the orchestrator cannot derive blockedness without
		// already knowing which blockers are unfinished — so the answer exists
		// before anyone asks. What it must not do is resolve anything beyond
		// that; the blocker's title, holder and own blockers are a lookup the
		// caller can make itself.
		out = append(out, flow.Blocker{
			Ref:    b.refFromIssue(d.GetNumber()),
			Status: itemStatusFromIssue(d),
		})
	}
	return out, nil
}
