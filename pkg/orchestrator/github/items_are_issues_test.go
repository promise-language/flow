package github

import (
	"errors"
	"testing"

	"github.com/promise-language/flow"
)

// An item is an issue (docs/github-schema.md § Items are issues). The Issues
// API returns pull requests too, out of one number space, so a number typed at
// `claim`, `resolve` or `status` can name either — and every by-ref path
// refuses the one that is not an item, where `List` merely skips it.
//
// The refusals below all come from ONE predicate, refusePullRequest
// (discover.go); these tests exist because it is reached from three places and
// a fourth caller forgetting it is exactly how the defect arose.

// The claim, which is the one that costs something: a claimed pull request gets
// an assignee, an owner label, an arena label and a state comment, and #140 —
// a merged PR — is carrying that record today.
func TestBackend_Claim_RefusesAPullRequest(t *testing.T) {
	b, mock, rec := newClaimPrecondBackend(t)
	scriptCleanWorktree(rec)
	mock.mu.Lock()
	mock.issueIsPullRequest = true
	mock.mu.Unlock()

	_, err := b.Claim(t.Context(), b.refFromIssue(42), nil)
	if err == nil {
		t.Fatal("claiming a pull request must refuse")
	}
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
	}
	if refused.Code != "not-an-item" {
		t.Errorf("Code = %q, want %q", refused.Code, "not-an-item")
	}
	if !refused.ItemScoped {
		t.Error("ItemScoped = false, want true (this ref is the problem; another might succeed)")
	}
	if refused.Override != "" {
		t.Errorf("Override = %q, want none — no flag makes a pull request an item", refused.Override)
	}

	// NOTHING was written. The refusal precedes Phase 1, so no claim token is
	// minted and no ownership asserted — which is the difference between this
	// fix and one that cleans up after itself.
	mock.mu.Lock()
	mutations := append([]string(nil), mock.mutations...)
	mock.mu.Unlock()
	if len(mutations) != 0 {
		t.Errorf("a refused claim wrote to GitHub: %v", mutations)
	}
}

// The check sits ABOVE the idempotent holder return, for the reason
// TestBackend_Claim_HeldReclaimStillRefusesStopLabels records for the two label
// preflights: a holder mid-work is exactly who must be stopped. An arena
// holding a pull request is the state this whole item is about, and a
// short-circuit hoisted over the check would hand it a standing lease and let
// it resolve on.
func TestBackend_Claim_RefusesAPullRequestTheArenaAlreadyHolds(t *testing.T) {
	b, mock, rec := newClaimPrecondBackend(t)
	scriptCleanWorktree(rec)
	ctx := t.Context()

	if _, err := b.Claim(ctx, b.refFromIssue(42), nil); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	// Mid-work: on the item's own branch, holding the lease this arena wrote.
	mock.mu.Lock()
	mock.issueIsPullRequest = true
	mock.mu.Unlock()
	rec.handlers["rev-parse --abbrev-ref HEAD"] = func([]string) ([]byte, error) {
		return []byte("flow/issue-42\n"), nil
	}

	_, err := b.Claim(ctx, b.refFromIssue(42), nil)
	if err == nil {
		t.Fatal("re-claiming a pull request must refuse, not return the standing lease")
	}
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
	}
	if refused.Code != "not-an-item" {
		t.Errorf("Code = %q, want %q", refused.Code, "not-an-item")
	}
}

// It is also above the WORKTREE preconditions, which are arena-scoped: told the
// tree is dirty, an operator cleans the tree and types the same wrong number
// again. The item-scoped answer is the one that ends the attempt.
func TestBackend_Claim_RefusesAPullRequestBeforeTheWorktreePreconditions(t *testing.T) {
	b, mock, rec := newClaimPrecondBackend(t)
	scriptCleanWorktree(rec)
	rec.handlers["status --porcelain --untracked-files=normal"] = func([]string) ([]byte, error) {
		return []byte("M dirty.go\n"), nil
	}
	mock.mu.Lock()
	mock.issueIsPullRequest = true
	mock.mu.Unlock()

	_, err := b.Claim(t.Context(), b.refFromIssue(42), nil)
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
	}
	if refused.Code != "not-an-item" {
		t.Errorf("Code = %q, want %q — the item-scoped answer outranks the dirty tree", refused.Code, "not-an-item")
	}
}

// Get must answer identically to List for the same item at the same moment
// (docs/orchestrator.md § Required surface). List skips a pull request, so Get
// cannot describe one.
func TestBackend_Get_RefusesAPullRequest(t *testing.T) {
	mock := newGHMock(t)
	mock.issueIsPullRequest = true
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)

	info, err := b.Get(t.Context(), b.refFromIssue(42), "implement", func(flow.ItemType) bool { return true }, nil)
	if err == nil {
		t.Fatal("Get on a pull request must refuse")
	}
	if info != nil {
		t.Errorf("info = %+v, want nil — a refusal returns no item", info)
	}
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
	}
	if refused.Code != "not-an-item" {
		t.Errorf("Code = %q, want %q", refused.Code, "not-an-item")
	}
	if !refused.ItemScoped {
		t.Error("ItemScoped = false, want true")
	}
}

// Load is the path a typed number actually takes: `status <n>` loads
// (cli/cmd_status.go), it does not Get. Without the refusal here the command
// renders a merged pull request as a full item.
func TestBackend_Load_RefusesAPullRequest(t *testing.T) {
	mock := newGHMock(t)
	mock.issueIsPullRequest = true
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)

	item, err := b.Load(t.Context(), b.refFromIssue(42))
	if err == nil {
		t.Fatal("Load on a pull request must refuse")
	}
	if item != nil {
		t.Errorf("item = %+v, want nil", item)
	}
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
	}
	if refused.Code != "not-an-item" {
		t.Errorf("Code = %q, want %q", refused.Code, "not-an-item")
	}
	if !refused.ItemScoped {
		t.Error("ItemScoped = false, want true")
	}
}

// RELEASE IS NOT REFUSED, and that is the reason the check is at the three
// by-ref sites rather than inside the GetIssue wrapper every one of them calls.
// An arena that took a lease on a pull request before this fix — #140's arena —
// has exactly one supported exit, and it is release. A wrapper-level refusal
// would make that record permanently unclearable, which is the defect #212
// describes from the other end.
func TestBackend_Release_StillReleasesAPullRequest(t *testing.T) {
	b, mock, rec := newClaimPrecondBackend(t)
	scriptCleanWorktree(rec)
	ctx := t.Context()

	// The lease as it exists today: taken while nothing checked the kind.
	if _, err := b.Claim(ctx, b.refFromIssue(42), nil); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	owner := b.labels.Owner("alice")
	arena := b.labels.Arena(b.arenaFingerprint())
	if !contains(mock.labelNames(), owner) || !contains(mock.labelNames(), arena) {
		t.Fatalf("labels = %v, want the claim record present before the release", mock.labelNames())
	}
	mock.mu.Lock()
	mock.issueIsPullRequest = true
	mock.mu.Unlock()

	if err := b.Release(ctx, b.refFromIssue(42)); err != nil {
		t.Fatalf("Release on a pull request must still succeed — it is the only exit: %v", err)
	}
	if contains(mock.labelNames(), owner) || contains(mock.labelNames(), arena) {
		t.Errorf("labels = %v, want both halves of the claim record removed", mock.labelNames())
	}
	mock.mu.Lock()
	assignees := append([]string(nil), mock.assignees...)
	mock.mu.Unlock()
	if contains(assignees, "alice") {
		t.Errorf("assignees = %v, want the holder removed", assignees)
	}
}
