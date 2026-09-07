package github

import (
	"context"
	"fmt"
	"slices"
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

	// The key travels with the item, so the sort below needs no second read of
	// the issue. It is derived only at scope `auto`: at the wider scopes the
	// order is not a contract, and deriving one there is work for nothing.
	type keyed struct {
		info flow.ItemInfo
		key  flow.SelectionKey
		num  int
	}
	var items []keyed
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
		k := keyed{info: info, num: iss.GetNumber()}
		if scope == flow.ScopeAuto {
			k.key = b.selectionKeyOf(iss, labelNamesOf(iss.Labels))
		}
		items = append(items, k)
	}

	// At scope `auto` the listing IS the selectable set, so it is reported in
	// the order it will be taken in — anything else answers "what runs next"
	// with something that only looks like an answer. The wider scopes keep the
	// API's own order: they are read by a person who scopes and sorts them for
	// themselves, and fixing an order there would constrain the report without
	// informing anything.
	if scope == flow.ScopeAuto {
		slices.SortStableFunc(items, func(x, y keyed) int {
			if c := flow.CompareSelection(x.key, y.key); c != 0 {
				return c
			}
			// The total tiebreak. CompareSelection returns 0 for two issues
			// filed in the same second, and two callers reading one set must
			// still start on one item.
			return x.num - y.num
		})
	}

	out := make([]flow.ItemInfo, 0, len(items))
	for _, k := range items {
		out = append(out, k.info)
	}
	return out, nil
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
// given tag — NOT blocked, and NOT held by another arena.
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
	account, err := b.resolveAccount(ctx)
	if err != nil {
		return nil, err
	}

	type keyed struct {
		ref flow.ItemRef
		key flow.SelectionKey
		num int
	}
	selectable := make([]keyed, 0, len(result.Issues))
	for _, issue := range result.Issues {
		lbls := labelNamesOf(issue.Labels)
		// The contract's comparison, against the labels actually returned.
		if !flow.TagsMatch(tagsOf(lbls), tags) {
			continue
		}
		// MUST NOT return an item another arena holds. `assignee:@me` narrows
		// to this ACCOUNT, which on a one-login fleet is every arena's answer,
		// so the query alone hands the same item to all of them — and each then
		// claims it, because the claim preflight was making the same comparison.
		// This is ELIGIBILITY, not a sort key: an item someone else is running
		// is absent from the set, never merely ranked last.
		if _, held := b.heldByAnotherArena(lbls, account, b.holdsItem(ctx, issue.GetNumber())); held {
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
		// A deferred item is ABSENT, not sorted last: one sorted last is still
		// an item a fleet with spare capacity reaches, and "do not start this
		// unattended" is exactly what deferring it said. It stays resolvable by
		// name, which needs no mechanism — `resolve <item-id>` does not come
		// through here.
		if b.labels.UrgencyOf(lbls) == flow.UrgencyDeferred {
			continue
		}
		selectable = append(selectable, keyed{
			ref: b.refFromIssue(issue.GetNumber()),
			key: b.selectionKeyOf(issue, lbls),
			num: issue.GetNumber(),
		})
	}

	slices.SortStableFunc(selectable, func(x, y keyed) int {
		if c := flow.CompareSelection(x.key, y.key); c != 0 {
			return c
		}
		return x.num - y.num
	})

	refs := make([]flow.ItemRef, 0, len(selectable))
	for _, k := range selectable {
		refs = append(refs, k.ref)
	}
	return refs, nil
}

// selectionKeyOf is the ONE place an issue's position in the selection order
// comes from: the two axis labels, and the issue's own filing time as its age.
// List and ListAutoSelectable both order by it, so the two cannot disagree
// about what runs next.
func (b *Orchestrator) selectionKeyOf(iss *github.Issue, lblNames []string) flow.SelectionKey {
	return flow.SelectionKey{
		Urgency:  b.labels.UrgencyOf(lblNames),
		Priority: b.labels.PriorityOf(lblNames),
		Age:      iss.GetCreatedAt().Time,
	}
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
	holder, _ := b.holderFromLabels(lblNames)

	info := flow.ItemInfo{
		Ref:         b.refFromIssue(iss.GetNumber()),
		Type:        itemType,
		Title:       iss.GetTitle(),
		Body:        iss.GetBody(),
		URL:         iss.GetHTMLURL(),
		Status:      itemStatusFromIssue(iss),
		Disposition: dispositionFromIssue(iss),
		Holder:      holder,
		Tags:        tagsOf(lblNames),
		Priority:    b.labels.PriorityOf(lblNames),
		Urgency:     b.labels.UrgencyOf(lblNames),
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

	// Level 5: free — no OTHER ARENA holds it. The comparison is the same one
	// Claim's preflight makes, so `list` cannot report `auto` for an item a
	// claim would refuse — including an item another arena under this very
	// account is running, which an account comparison reads as our own.
	if _, held := b.heldByAnotherArena(lblNames, account, b.holdsItem(ctx, iss.GetNumber())); held {
		return flow.AvailHeld, nil
	}

	// Level 6: auto — opted in (binary label present) AND assigned to me, and
	// not deferred. Deferral is NOT a rung of its own: a deferred item is open,
	// unblocked, free and opted in, and what is true of it is that
	// auto-selection will not take it — exactly the boundary `available`
	// already marks. Urgency is the field that says which of the two an
	// `available` item is.
	if b.labels.UrgencyOf(lblNames) == flow.UrgencyDeferred {
		return flow.AvailAvailable, nil
	}
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

// holderFromLabels is the ONE reader of the item's claim record: the account
// from flow:owner:<login>, and the arena fingerprint from flow:arena:<fp>.
//
// It returns the fingerprint alongside the Holder because the two answer
// different questions. The Holder is what a caller is shown, and its arena half
// can be filled only when the fingerprint is OURS — that is the single case
// this orchestrator can honestly name the holding arena, since a digest names
// no arena and cannot be reversed into one. The fingerprint itself is what the
// exclusion compares, and a foreign one is perfectly comparable even though it
// is unnameable. Reporting a remote holder's (HostId, ArenaId) is #222's and
// #164's work and needs a record this one does not publish.
//
// Empty fingerprint means the item carries no arena label — either it is
// unclaimed, or the claim predates flow:arena: entirely; heldByAnotherArena is
// where those two are told apart.
func (b *Orchestrator) holderFromLabels(lblNames []string) (flow.Holder, string) {
	var h flow.Holder
	var fingerprint string
	for _, lbl := range lblNames {
		if account, ok := b.labels.OwnerFromLabel(lbl); ok {
			if h.Account == "" {
				h.Account = account
			}
			continue
		}
		if fp, ok := b.labels.ArenaFromLabel(lbl); ok && fingerprint == "" {
			fingerprint = fp
		}
	}
	if fingerprint != "" && fingerprint == b.arenaFingerprint() {
		h.Arena = b.arena()
	}
	return h, fingerprint
}

// heldByAnotherArena reports whether the labels record a claim held by an arena
// other than this one — INCLUDING one under this same account, which is the
// ordinary single-operator fleet and the case an account comparison cannot see.
// A lease binds item ↔ arena (docs/orchestrator.md § Required surface →
// Claiming: "at most one item per arena, at most one arena per item"), so the
// account is attribution and the arena is the exclusion.
//
// weHoldIt is this arena's own answer, from its lease file, to "am I the
// holder?" — the one fact the item cannot carry for a record written before
// flow:arena: existed.
//
//	no owner label                                    → not held
//	owner label, another account                      → held (the older rule)
//	owner label, this account, arena label ≠ ours     → held
//	owner label, this account, arena label = ours     → not held
//	owner label, this account, no arena label         → held unless weHoldIt
//
// The last row is not a tolerance for legacy records, it is the correct reading
// of one: flow:owner: is written by Claim's Phase 3 and by nothing else — not
// by seeding — so an owner label means SOME arena holds this and the item does
// not say which. The only arena that can prove it is the holder is the one
// whose own lease file says so. The holder therefore re-claims idempotently and
// every other arena is refused, which is the safe direction. The residual cost
// is that an owner label left behind by a crashed holder needs --force, already
// the documented break-glass and what #222 exists to make conditional.
//
// The reason is a clause naming the item's own state, without the issue number:
// the caller has it, and prefixes it.
func (b *Orchestrator) heldByAnotherArena(lblNames []string, account flow.AccountId, weHoldIt bool) (reason string, held bool) {
	holder, fingerprint := b.holderFromLabels(lblNames)
	switch {
	case holder.Account == "":
		return "", false
	case holder.Account != account:
		return fmt.Sprintf("carries owner label for %s (use --force to take over)", holder.Account), true
	case fingerprint == b.arenaFingerprint():
		return "", false
	case fingerprint != "":
		return fmt.Sprintf(
			"is held by another arena (%s) claiming as %s (use --force to take over)",
			fingerprint, account), true
	case weHoldIt:
		return "", false
	default:
		return fmt.Sprintf(
			"carries an owner label for %s but records no arena, and this arena does not hold it "+
				"(use --force to take over)", account), true
	}
}

// holdsItem answers "does this arena hold this item?" from the lease file —
// the arena-side half heldByAnotherArena needs for a record that names no
// arena. A local file read, so asking once per item in a listing costs nothing.
//
// A file that cannot be read answers false, which reads the item as held: for a
// listing that is the conservative direction, and it never widens what an
// unattended run may start on. Claim does NOT go through here — it must fail
// the claim closed on an unreadable lease rather than infer anything from it,
// so it reads LookupActiveClaim itself.
func (b *Orchestrator) holdsItem(ctx context.Context, issueNum int) bool {
	active, err := b.LookupActiveClaim(ctx)
	if err != nil || active == nil {
		return false
	}
	activeNum, err := b.issueNumber(active.ItemRef)
	return err == nil && activeNum == issueNum
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
