package github

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
func (b *Backend) Claim(ctx context.Context, ref flow.ItemRef, owner string, force bool) (flow.Claim, error) {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return flow.Claim{}, err
	}
	if owner == "" {
		return flow.Claim{}, errors.New("github.Claim: owner cannot be empty (caller should pass gh login)")
	}

	// Preflight: refuse if the issue is owned by another flow binary or
	// explicitly disabled.
	issue, err := b.out.GetIssue(ctx, issueNum)
	if err != nil {
		return flow.Claim{}, fmt.Errorf("get issue %d: %w", issueNum, err)
	}
	names := labelNamesOf(issue.Labels)
	if hasLabel(names, b.labels.Disabled()) {
		return flow.Claim{}, fmt.Errorf("issue #%d carries %s label", issueNum, b.labels.Disabled())
	}
	if otherBinary, wrong := b.otherBinaryLabel(names); wrong {
		return flow.Claim{}, fmt.Errorf("issue #%d is owned by other flow binary %q", issueNum, otherBinary)
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
		return flow.Claim{}, errors.New("claim race: label removed before observation")
	}
	sort.Strings(contenders)
	if contenders[0] != token {
		// Lost the race — clean up our label.
		_ = b.out.RemoveLabel(ctx, issueNum, claimLabel)
		return flow.Claim{}, fmt.Errorf("claim race lost to %s", contenders[0])
	}

	// Phase 3: assert ownership.
	if err := b.out.AddAssignees(ctx, issueNum, []string{owner}); err != nil {
		_ = b.out.RemoveLabel(ctx, issueNum, claimLabel)
		return flow.Claim{}, fmt.Errorf("add assignee: %w", err)
	}
	// Clear any stale owner labels first (other than our own).
	for _, name := range labelNamesOf(issue2.Labels) {
		if login, ok := b.labels.OwnerFromLabel(name); ok && login != owner {
			_ = b.out.RemoveLabel(ctx, issueNum, name)
		}
	}
	if err := b.out.AddLabels(ctx, issueNum, []string{
		b.labels.Owner(owner),
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
		if perr == nil && existingOwner != "" && existingOwner != owner {
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
		BackendName: b.Name(),
		ItemRef:     ref,
		Owner:       owner,
		ClaimedAt:   nowUTC(),
		Token:       tokenJSON,
	}
	// The github backend's lease store IS the worktree-local
	// .flow/active.json file. Write it here so LookupActiveClaim and the
	// CLI commands that consume the active claim can find it.
	if err := clistate.Save(c); err != nil {
		// Best-effort rollback of the github-side ownership we just took.
		_ = b.out.RemoveLabel(ctx, issueNum, b.labels.Owner(owner))
		_ = b.out.RemoveAssignees(ctx, issueNum, []string{owner})
		return flow.Claim{}, fmt.Errorf("github.Claim: save active claim: %w", err)
	}
	return c, nil
}

// Release strips the assignee, removes the flow:owner:<me> label, clears
// the worktree-local active-claim file, leaves the state comment intact.
func (b *Backend) Release(ctx context.Context, claim flow.Claim) error {
	issueNum, err := b.issueNumber(claim.ItemRef)
	if err != nil {
		return err
	}
	if err := b.out.RemoveLabel(ctx, issueNum, b.labels.Owner(claim.Owner)); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove owner label: %w", err)
	}
	if err := b.out.RemoveAssignees(ctx, issueNum, []string{claim.Owner}); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove assignee: %w", err)
	}
	if err := clistate.Clear(); err != nil {
		return fmt.Errorf("github.Release: clear active claim file: %w", err)
	}
	return nil
}

// LookupActiveClaim returns the active claim held by owner. The github
// backend's lease store is the worktree-local .flow/active.json file; this
// reads that file and confirms the owner matches.
func (b *Backend) LookupActiveClaim(ctx context.Context, owner string) (*flow.Claim, error) {
	c, err := clistate.Load()
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil
	}
	if c.BackendName != b.Name() {
		// Someone else's claim file — not ours.
		return nil, nil
	}
	if owner != "" && c.Owner != owner {
		// Claim is held by a different owner on this worktree. Treat as
		// "no claim for me" rather than an error — the CLI will prompt
		// the user to claim afresh.
		return nil, nil
	}
	return c, nil
}

// LookupClaim returns the current claim holder (if any) without taking a
// lease.
func (b *Backend) LookupClaim(ctx context.Context, ref flow.ItemRef) (*flow.ClaimInfo, error) {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return nil, err
	}
	issue, err := b.out.GetIssue(ctx, issueNum)
	if err != nil {
		return nil, fmt.Errorf("get issue %d: %w", issueNum, err)
	}
	for _, lbl := range issue.Labels {
		if login, ok := b.labels.OwnerFromLabel(lbl.GetName()); ok {
			return &flow.ClaimInfo{Owner: login, ClaimedAt: issue.GetUpdatedAt().Time}, nil
		}
	}
	return nil, nil
}

// claimContenders returns the random hex parts of all flow:claim:* labels
// on the issue.
func (b *Backend) claimContenders(names []string) []string {
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
func (b *Backend) otherBinaryLabel(names []string) (string, bool) {
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
