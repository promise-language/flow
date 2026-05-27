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
func (b *Backend) Claim(ctx context.Context, ref flow.ItemRef, owner string) (flow.Claim, error) {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return flow.Claim{}, err
	}
	if owner == "" {
		return flow.Claim{}, errors.New("github.Claim: owner cannot be empty (caller should pass gh login)")
	}

	// Preflight: refuse if the issue is owned by another flow binary or
	// explicitly disabled.
	issue, _, err := b.gh.Issues.Get(ctx, b.cfg.Owner, b.cfg.Repo, issueNum)
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
	if _, _, err := b.gh.Issues.AddLabelsToIssue(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, []string{claimLabel}); err != nil {
		return flow.Claim{}, fmt.Errorf("add claim label: %w", err)
	}

	// Phase 2: re-fetch labels; check race.
	issue2, _, err := b.gh.Issues.Get(ctx, b.cfg.Owner, b.cfg.Repo, issueNum)
	if err != nil {
		_, _ = b.gh.Issues.RemoveLabelForIssue(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, claimLabel)
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
		_, _ = b.gh.Issues.RemoveLabelForIssue(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, claimLabel)
		return flow.Claim{}, fmt.Errorf("claim race lost to %s", contenders[0])
	}

	// Phase 3: assert ownership.
	if _, _, err := b.gh.Issues.AddAssignees(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, []string{owner}); err != nil {
		_, _ = b.gh.Issues.RemoveLabelForIssue(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, claimLabel)
		return flow.Claim{}, fmt.Errorf("add assignee: %w", err)
	}
	// Clear any stale owner labels first (other than our own).
	for _, name := range labelNamesOf(issue2.Labels) {
		if login, ok := b.labels.OwnerFromLabel(name); ok && login != owner {
			_, _ = b.gh.Issues.RemoveLabelForIssue(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, name)
		}
	}
	if _, _, err := b.gh.Issues.AddLabelsToIssue(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, []string{
		b.labels.Owner(owner),
		b.labels.Binary(b.cfg.BinaryName),
	}); err != nil {
		// Best-effort cleanup, then surface.
		_, _ = b.gh.Issues.RemoveLabelForIssue(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, claimLabel)
		return flow.Claim{}, fmt.Errorf("add owner/binary labels: %w", err)
	}
	if _, err := b.gh.Issues.RemoveLabelForIssue(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, claimLabel); err != nil {
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
	return flow.Claim{
		BackendName: b.Name(),
		ItemRef:     ref,
		Owner:       owner,
		ClaimedAt:   nowUTC(),
		Token:       tokenJSON,
	}, nil
}

// Release strips the assignee, removes the flow:owner:<me> label, leaves the
// state comment intact.
func (b *Backend) Release(ctx context.Context, claim flow.Claim) error {
	issueNum, err := b.issueNumber(claim.ItemRef)
	if err != nil {
		return err
	}
	if _, err := b.gh.Issues.RemoveLabelForIssue(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, b.labels.Owner(claim.Owner)); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove owner label: %w", err)
	}
	if _, _, err := b.gh.Issues.RemoveAssignees(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, []string{claim.Owner}); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove assignee: %w", err)
	}
	return nil
}

// LookupClaim returns the current claim holder (if any) without taking a
// lease.
func (b *Backend) LookupClaim(ctx context.Context, ref flow.ItemRef) (*flow.ClaimInfo, error) {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return nil, err
	}
	issue, _, err := b.gh.Issues.Get(ctx, b.cfg.Owner, b.cfg.Repo, issueNum)
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
