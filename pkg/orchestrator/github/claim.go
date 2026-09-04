package github

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
)

// Claim acquires an exclusive lease on the given issue. Algorithm:
//
//  1. POST a random claim label flow:claim:<hex>.
//  2. GET the issue's labels; if multiple flow:claim:* are present, the
//     lexicographically smallest hex wins.
//  3. Losers DELETE their own claim label and return an error.
//  4. Winner: POST self as assignee, POST flow:owner:<login>, DELETE
//     flow:claim:<hex>.
//  5. POST or supersede the state comment.
//
// NEITHER THE ARENA NOR THE ACCOUNT IS A PARAMETER. Both are ambient, fixed by
// where the call runs: the arena is the checkout this process sits in, and the
// account is whoever its credentials act as, resolved through the one
// derivation `list` also compares against. A caller-supplied account could only
// agree or be wrong — and it was wrong, whenever $USER differed from the gh
// login, which wrote a flow:owner label for a user GitHub may not recognise.
func (b *Orchestrator) Claim(ctx context.Context, ref flow.ItemRef, overrides []flow.ClaimOverride) (flow.Claim, error) {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return flow.Claim{}, err
	}
	owner, err := b.resolveAccount(ctx)
	if err != nil {
		return flow.Claim{}, err
	}

	// Preflight: refuse if the issue is owned by another flow binary or
	// explicitly disabled.
	issue, err := b.out.GetIssue(ctx, issueNum)
	if err != nil {
		return flow.Claim{}, fmt.Errorf("get issue %d: %w", issueNum, err)
	}
	names := labelNamesOf(issue.Labels)
	if hasLabel(names, b.labels.Disabled()) {
		return flow.Claim{}, flow.ErrClaimRefused{
			Code: "disabled", ItemScoped: true,
			Reason: fmt.Sprintf("issue #%d carries %s label", issueNum, b.labels.Disabled()),
		}
	}
	if otherBinary, wrong := b.otherBinaryLabel(names); wrong {
		return flow.Claim{}, flow.ErrClaimRefused{
			Code: "other-binary", ItemScoped: true,
			Reason: fmt.Sprintf("issue #%d is owned by other flow binary %q", issueNum, otherBinary),
		}
	}
	if !slices.Contains(overrides, flow.OverrideAlreadyHeld) {
		// Refuse when another person holds the issue — either via an
		// assignee or a flow:owner:<login> label. The caller must pass
		// OverrideAlreadyHeld to take over deliberately.
		for _, u := range issue.Assignees {
			if login := flow.AccountId(u.GetLogin()); login != "" && login != owner {
				return flow.Claim{}, flow.ErrClaimRefused{
					Code: "already-held", ItemScoped: true,
					Reason:   fmt.Sprintf("issue #%d is assigned to %s (use --force to take over)", issueNum, login),
					Override: "force",
				}
			}
		}
		for _, name := range names {
			if login, ok := b.labels.OwnerFromLabel(name); ok && login != owner {
				return flow.Claim{}, flow.ErrClaimRefused{
					Code: "already-held", ItemScoped: true,
					Reason:   fmt.Sprintf("issue #%d carries owner label for %s (use --force to take over)", issueNum, login),
					Override: "force",
				}
			}
		}
	}

	// Worktree preconditions — arena-scoped, not item-scoped.
	// Run before Phase 1: nothing is written to the item on refusal.

	if !slices.Contains(overrides, flow.OverrideStaleBase) {
		// 1. Fetch — every check below is worthless against stale refs.
		if err := b.git.Fetch(ctx, "origin"); err != nil {
			return flow.Claim{}, flow.ErrClaimRefused{
				Code: "fetch-failed", ItemScoped: false,
				Reason:   "git fetch origin failed",
				Detail:   err.Error(),
				Check:    "fetch",
				Override: "force",
			}
		}

		// 2. HEAD must be on the base branch.
		base, err := b.DefaultBranch(ctx)
		if err != nil {
			return flow.Claim{}, fmt.Errorf("resolve default branch: %w", err)
		}
		current, err := b.git.CurrentBranch(ctx)
		if err != nil {
			return flow.Claim{}, fmt.Errorf("current branch: %w", err)
		}
		if flow.BranchName(current) != base {
			return flow.Claim{}, flow.ErrClaimRefused{
				Code: "not-on-base", ItemScoped: false,
				Reason: fmt.Sprintf("HEAD is on %q, want %q — run: git checkout %s",
					current, base, base),
				Check:    "base-branch",
				Override: "force",
			}
		}

		// 3. Local base must be at origin's tip.
		localSHA, err := b.git.RevParse(ctx, string(base))
		if err != nil {
			return flow.Claim{}, fmt.Errorf("rev-parse %s: %w", base, err)
		}
		remoteSHA, err := b.git.RevParse(ctx, "origin/"+string(base))
		if err != nil {
			return flow.Claim{}, fmt.Errorf("rev-parse origin/%s: %w", base, err)
		}
		if localSHA != remoteSHA {
			return flow.Claim{}, flow.ErrClaimRefused{
				Code: "base-stale", ItemScoped: false,
				Reason: fmt.Sprintf("%s (%s) differs from origin/%s (%s) — run: git pull --ff-only",
					base, localSHA[:8], base, remoteSHA[:8]),
				Check:    "base-branch",
				Override: "force",
			}
		}
	}

	if !slices.Contains(overrides, flow.OverrideDirtyTree) {
		// 4. Tree must be clean, including untracked files.
		porcelain, err := b.git.StatusPorcelain(ctx)
		if err != nil {
			return flow.Claim{}, fmt.Errorf("check dirty tree: %w", err)
		}
		if porcelain != "" {
			return flow.Claim{}, flow.ErrClaimRefused{
				Code: "dirty-tree", ItemScoped: false,
				Reason:   "worktree has uncommitted or untracked changes",
				Detail:   porcelain,
				Check:    "clean-tree",
				Override: "force",
			}
		}
	}

	// Phase 1: post a random claim label.
	token := randomClaimToken()
	claimLabel := b.labels.ClaimToken(token)
	if err := b.out.AddLabels(ctx, issueNum, []string{claimLabel}); err != nil {
		return flow.Claim{}, fmt.Errorf("add claim label: %w", err)
	}

	// Phase 2: re-fetch labels; check race.
	issue2, err := b.out.GetIssue(ctx, issueNum)
	if err != nil {
		_ = b.out.RemoveLabel(ctx, issueNum, claimLabel)
		return flow.Claim{}, fmt.Errorf("get issue (post-claim): %w", err)
	}
	contenders := b.claimContenders(labelNamesOf(issue2.Labels))
	if len(contenders) == 0 {
		// Race condition — our label was stripped before we could read it.
		return flow.Claim{}, flow.ErrClaimRefused{
			Code: "claim-race", ItemScoped: true,
			Reason: "claim race: label removed before observation",
		}
	}
	sort.Strings(contenders)
	if contenders[0] != token {
		// Lost the race — clean up our label.
		_ = b.out.RemoveLabel(ctx, issueNum, claimLabel)
		return flow.Claim{}, flow.ErrClaimRefused{
			Code: "claim-race", ItemScoped: true,
			Reason: fmt.Sprintf("claim race lost to %s", contenders[0]),
		}
	}

	// Phase 3: assert ownership.
	if err := b.out.AddAssignees(ctx, issueNum, []string{string(owner)}); err != nil {
		_ = b.out.RemoveLabel(ctx, issueNum, claimLabel)
		return flow.Claim{}, fmt.Errorf("add assignee: %w", err)
	}
	// Clear any stale owner labels first (other than our own).
	for _, name := range labelNamesOf(issue2.Labels) {
		if login, ok := b.labels.OwnerFromLabel(name); ok && login != owner {
			if err := b.out.RemoveLabel(ctx, issueNum, name); err != nil {
				_ = b.out.RemoveLabel(ctx, issueNum, claimLabel)
				return flow.Claim{}, fmt.Errorf("remove stale owner label %s: %w", name, err)
			}
		}
	}
	if err := b.out.AddLabels(ctx, issueNum, []string{
		b.labels.Owner(string(owner)),
		b.labels.Binary(b.cfg.BinaryName),
	}); err != nil {
		// Best-effort cleanup, then surface.
		_ = b.out.RemoveLabel(ctx, issueNum, claimLabel)
		return flow.Claim{}, fmt.Errorf("add owner/binary labels: %w", err)
	}
	if err := b.out.RemoveLabel(ctx, issueNum, claimLabel); err != nil {
		// Non-fatal — the claim label is just transient.
		_ = err
	}

	// Find / supersede the state comment if needed.
	stateBody, stateID, err := b.fetchStateComment(ctx, issueNum, 0)
	if err != nil {
		return flow.Claim{}, fmt.Errorf("locate state comment: %w", err)
	}
	if stateBody != "" {
		_, existingOwner, _, perr := extractStateDoc(stateBody)
		if perr == nil && existingOwner != "" && flow.AccountId(existingOwner) != owner {
			// Different author — post a fresh state comment authored by us
			// with state copied forward (zero state copy: the caller's
			// SeedState pass will repopulate from cfg.Artifacts).
			// Mark the previous comment off-topic via reactions API.
			// For v1, we simply create a fresh comment with empty doc; the
			// next LoadState + Seed cycle handles the rest.
			newDoc := stateDoc{Flow: b.cfg.BinaryName, Schema: stateSchemaVersion, SeededAt: nowUTC()}
			id, _, postErr := b.postStateComment(ctx, issueNum, newDoc, owner)
			if postErr != nil {
				return flow.Claim{}, fmt.Errorf("supersede state comment: %w", postErr)
			}
			stateID = id
		}
	}

	tokenJSON, _ := b.saveClaimToken(claimToken{StateCommentID: stateID, ClaimID: token})
	c := flow.Claim{
		OrchestratorName: b.Name(),
		ItemRef:          ref,
		Arena:            b.arena(),
		Account:          owner,
		ClaimedAt:        nowUTC(),
		Token:            tokenJSON,
	}
	// The github orchestrator's lease store IS the worktree-local
	// .flow/active.json file — one file per checkout, which does the arena
	// scoping a fleet-serving orchestrator must do explicitly. Write it here so
	// LookupActiveClaim and the CLI commands that consume the active claim can
	// find it.
	if err := clistate.Save(c); err != nil {
		// Best-effort rollback of the github-side ownership we just took.
		_ = b.out.RemoveLabel(ctx, issueNum, b.labels.Owner(string(owner)))
		_ = b.out.RemoveAssignees(ctx, issueNum, []string{string(owner)})
		return flow.Claim{}, fmt.Errorf("github.Claim: save active claim: %w", err)
	}
	return c, nil
}

// arena is the (HostId, ArenaId) pair this checkout is. Both halves are
// implicit for this orchestrator — the arena IS the local checkout — but the
// pair is still recorded on the claim, because a handle to a lease that could
// not name what the lease binds leaves every holder unidentifiable wherever one
// account runs more than one arena.
//
// The HostId is derived from the machine and normalized; the ArenaId is the
// absolute worktree path, which is stable across restarts and unique within the
// host — exactly what the contract asks of it.
func (b *Orchestrator) arena() flow.Arena {
	id := b.cfg.WorktreeDir
	if abs, err := filepath.Abs(id); err == nil {
		id = abs
	}
	return flow.Arena{Host: flow.DeriveHostId(), Id: flow.ArenaId(id)}
}

// Release strips the assignee, removes the flow:owner:<account> label, clears
// the worktree-local active-claim file, and leaves the state comment intact.
//
// Addressed by ref: the account is ambient, so it is read rather than taken off
// a claim value the caller might be holding after the lease was revoked.
func (b *Orchestrator) Release(ctx context.Context, ref flow.ItemRef) error {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return err
	}
	owner, err := b.resolveAccount(ctx)
	if err != nil {
		return err
	}
	if err := b.out.RemoveLabel(ctx, issueNum, b.labels.Owner(string(owner))); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove owner label: %w", err)
	}
	if err := b.out.RemoveAssignees(ctx, issueNum, []string{string(owner)}); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove assignee: %w", err)
	}
	if err := clistate.Clear(); err != nil {
		return fmt.Errorf("github.Release: clear active claim file: %w", err)
	}
	return nil
}

// Finalize records that the flow run on this item is complete: persists the
// finalized flag in the state comment, returns the worktree to the base branch
// (if not already there), and releases the claim.
//
// It REFUSES an item whose ItemStatus is not terminal. Finalizing does not MAKE
// an item terminal — nothing here closes one, and an issue reaches terminal by
// GitHub's own means (a merge that closes it, or a person). So Finalize records
// that the flow is finished with an item already finished, and refusing an open
// one is what keeps the two facts from drifting: a finalized item still open
// claims the work is over while the orchestrator says it is not.
//
// The refusal is ErrUnavailable, not ErrUnsupported: the item may reach
// terminal later, so asking again is exactly what a caller should do.
//
// The state-comment write comes first so a failure there leaves the claim
// intact; the worktree return precedes the release so a checkout failure also
// keeps the claim recoverable.
func (b *Orchestrator) Finalize(ctx context.Context, ref flow.ItemRef) error {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return fmt.Errorf("github.Finalize: %w", err)
	}
	owner, err := b.resolveAccount(ctx)
	if err != nil {
		return fmt.Errorf("github.Finalize: %w", err)
	}

	issue, err := b.out.GetIssue(ctx, issueNum)
	if err != nil {
		return fmt.Errorf("github.Finalize: get issue %d: %w", issueNum, err)
	}
	if status := itemStatusFromIssue(issue); status != flow.StatusTerminal {
		return fmt.Errorf(
			"github.Finalize: issue #%d is %s, and only a %s item's flow run may be recorded complete: %w",
			issueNum, status, flow.StatusTerminal, flow.ErrUnavailable)
	}

	// Mark the item finalized in the state comment so Load returns
	// Item.Finalized=true — the read is required with the write, because a
	// write nothing can observe is not a record.
	body, stateID, err := b.fetchStateComment(ctx, issueNum, b.cachedStateCommentID(issueNum))
	if err != nil {
		return fmt.Errorf("github.Finalize: fetch state comment: %w", err)
	}
	if body != "" {
		doc, _, found, perr := extractStateDoc(body)
		if perr != nil {
			return fmt.Errorf("github.Finalize: parse state comment: %w", perr)
		}
		if found && doc != nil {
			doc.Finalized = true
			if _, err := b.updateStateComment(ctx, issueNum, stateID, *doc, owner); err != nil {
				return fmt.Errorf("github.Finalize: update state comment: %w", err)
			}
		}
	}

	base, err := b.DefaultBranch(ctx)
	if err != nil {
		return fmt.Errorf("github.Finalize: resolve default branch: %w", err)
	}

	current, err := b.git.CurrentBranch(ctx)
	if err != nil {
		return fmt.Errorf("github.Finalize: current branch: %w", err)
	}

	if flow.BranchName(current) != base {
		dirty, err := b.git.IsDirty(ctx)
		if err != nil {
			return fmt.Errorf("github.Finalize: check dirty: %w", err)
		}
		if dirty {
			return fmt.Errorf("github.Finalize: worktree is dirty on %s — refusing to discard uncommitted changes", current)
		}

		exists, err := b.git.BranchExists(ctx, string(base))
		if err != nil {
			return fmt.Errorf("github.Finalize: check base branch: %w", err)
		}
		if !exists {
			return fmt.Errorf("github.Finalize: base branch %q does not exist locally", base)
		}

		if err := b.git.Checkout(ctx, string(base), "", false); err != nil {
			return fmt.Errorf("github.Finalize: checkout %s: %w", base, err)
		}
	}

	return b.Release(ctx, ref)
}

// LookupActiveClaim returns the claim THIS ARENA holds right now, or nil.
//
// It takes no key. The github orchestrator's lease store is the worktree-local
// .flow/active.json file, and one file per checkout IS the arena scoping — so
// the file already answers "what is this arena working on?" exactly. The old
// signature took an account and returned one claim, which worked here only by
// accident: one person resolving twenty-five items from one GitHub account has
// twenty-five claims, and the file was silently supplying the arena the
// signature omitted.
func (b *Orchestrator) LookupActiveClaim(ctx context.Context) (*flow.Claim, error) {
	c, err := clistate.Load()
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil
	}
	if c.OrchestratorName != b.Name() {
		// Someone else's claim file — not ours.
		return nil, nil
	}
	if c.Arena != b.arena() {
		// A file this checkout did not write. Treat as "no claim here" rather
		// than an error: the CLI prompts the operator to claim afresh.
		return nil, nil
	}
	return c, nil
}

// LookupClaim reports who holds this item, without taking a lease.
//
// The account comes from the flow:owner:<account> label. The ARENA is reported
// only when this checkout is the holder, because no label records it —
// docs/orchestrator.md grants exactly that for this orchestrator ("both halves
// are implicit… .flow/active.json is the whole lease store"), so there is no
// new label to invent for it.
func (b *Orchestrator) LookupClaim(ctx context.Context, ref flow.ItemRef) (*flow.ClaimInfo, error) {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return nil, err
	}
	issue, err := b.out.GetIssue(ctx, issueNum)
	if err != nil {
		return nil, fmt.Errorf("get issue %d: %w", issueNum, err)
	}
	for _, lbl := range issue.Labels {
		account, ok := b.labels.OwnerFromLabel(lbl.GetName())
		if !ok {
			continue
		}
		info := &flow.ClaimInfo{Account: account, ClaimedAt: issue.GetUpdatedAt().Time}
		// The arena, when this checkout is the one holding it.
		if active, aerr := b.LookupActiveClaim(ctx); aerr == nil && active != nil {
			if activeNum, nerr := b.issueNumber(active.ItemRef); nerr == nil && activeNum == issueNum {
				info.Arena = active.Arena
			}
		}
		return info, nil
	}
	return nil, nil
}

// claimContenders returns the random hex parts of all flow:claim:* labels
// on the issue.
func (b *Orchestrator) claimContenders(names []string) []string {
	out := make([]string, 0, 2)
	for _, n := range names {
		if hex, ok := b.labels.ClaimTokenFromLabel(n); ok {
			out = append(out, hex)
		}
	}
	return out
}

// otherBinaryLabel returns (binaryName, true) when the issue carries a
// flow:<other-binary> label that doesn't match cfg.BinaryName.
func (b *Orchestrator) otherBinaryLabel(names []string) (string, bool) {
	for _, n := range names {
		if !strings.HasPrefix(n, b.labels.prefix) {
			continue
		}
		rest := strings.TrimPrefix(n, b.labels.prefix)
		// Skip known structural labels.
		switch {
		case rest == labelSuffixSeeded,
			rest == labelSuffixBlocked,
			rest == labelSuffixNeedsAnswer,
			rest == labelSuffixDisabled,
			rest == labelSuffixInfraTransient,
			strings.HasPrefix(rest, labelSuffixOwnerPrefix),
			strings.HasPrefix(rest, labelSuffixClaimPrefix),
			strings.HasPrefix(rest, labelSuffixStalePrefix),
			strings.HasPrefix(rest, labelSuffixBudgetExhPref),
			strings.HasPrefix(rest, labelSuffixTypePrefix):
			continue
		}
		// What's left is a binary-name label.
		if rest != b.cfg.BinaryName {
			return rest, true
		}
	}
	return "", false
}

func hasLabel(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func randomClaimToken() string {
	buf := make([]byte, 16) // 128 bits
	if _, err := rand.Read(buf); err != nil {
		// Random failure: fall back to timestamp; collision impact is
		// minimal in single-runner scenarios.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
