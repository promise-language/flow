package github

import (
	"errors"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
)

// The arena and the account are AMBIENT. Neither is a parameter of Claim: the
// arena is the checkout this process sits in, and the account is whoever its
// credentials act as. A caller-supplied account could only agree or be wrong —
// and it was wrong whenever $USER differed from the gh login, which wrote a
// flow:owner label for a user GitHub may not recognise while `list` computed
// availability against a different account.

// $USER is deliberately not the authenticated login here. The claim path
// resolves the account through GET /user, so the label and the assignee are the
// login GitHub knows, and nothing about the local environment reaches them.
func TestBackend_Claim_CreditsTheAuthenticatedLoginAndNotTheLocalUser(t *testing.T) {
	t.Setenv("USER", "someone-else")
	t.Setenv("USERNAME", "someone-else")

	mock := newGHMock(t)
	srv := mock.server() // GET /user answers "alice"
	t.Cleanup(srv.Close)
	b := newMockedOrchestrator(t, mock, srv)

	claim, err := b.Claim(t.Context(), b.refFromIssue(42), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claim.Account != "alice" {
		t.Errorf("Claim.Account = %q, want the authenticated login", claim.Account)
	}
	if !contains(mock.labelNames(), "flow:owner:alice") {
		t.Errorf("labels = %v, want flow:owner:alice — a label for a user GitHub may not recognise is the reported defect",
			mock.labelNames())
	}
	mock.mu.Lock()
	assignees := append([]string(nil), mock.assignees...)
	mock.mu.Unlock()
	if !contains(assignees, "alice") {
		t.Errorf("assignees = %v, want the authenticated login", assignees)
	}
}

// ONE derivation, so the label `claim` writes and the holder `list` compares
// against cannot be two different values. An item this arena just claimed must
// not read as held by somebody else — which is what a second derivation
// produced.
func TestBackend_AnItemThisArenaClaimedDoesNotReadAsHeldElsewhere(t *testing.T) {
	t.Setenv("USER", "someone-else")

	mock := newGHMock(t)
	srv := mock.server()
	t.Cleanup(srv.Close)
	b := newMockedOrchestrator(t, mock, srv)

	ref := b.refFromIssue(42)
	if _, err := b.Claim(t.Context(), ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	info, err := b.Get(t.Context(), ref, "implement", func(flow.ItemType) bool { return true })
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Availability == flow.AvailHeld {
		t.Fatalf("the item this arena holds reads as held by another account (holder %q)", info.Holder.Account)
	}
	if info.Holder.Account != "alice" {
		t.Errorf("Holder.Account = %q, want the account the claim was written under", info.Holder.Account)
	}
}

// LookupActiveClaim takes NO KEY: one arena holds at most one claim, so the
// question has exactly one answer. The github orchestrator's lease store is the
// worktree-local claim file, and one file per checkout IS the arena scoping —
// so a file this checkout did not write answers "no claim here" rather than
// handing over somebody else's lease.
func TestBackend_LookupActiveClaim_IsScopedToThisArena(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	t.Cleanup(srv.Close)
	b := newMockedOrchestrator(t, mock, srv)

	mine := flow.Claim{
		OrchestratorName: b.Name(),
		ItemRef:          b.refFromIssue(42),
		Arena:            b.arena(),
		Account:          "alice",
	}
	if err := clistate.Save(mine); err != nil {
		t.Fatalf("clistate.Save: %v", err)
	}
	got, err := b.LookupActiveClaim(t.Context())
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if got == nil || got.ItemRef.Display != mine.ItemRef.Display {
		t.Fatalf("LookupActiveClaim = %+v, want this arena's own claim", got)
	}

	// The same file, written by a different arena. Rewriting the arena is what
	// distinguishes this from "no file at all": the claim is perfectly valid,
	// it is simply not this arena's to resume.
	elsewhere := mine
	elsewhere.Arena = flow.Arena{Host: "build07", Id: "/some/other/checkout"}
	if err := clistate.Save(elsewhere); err != nil {
		t.Fatalf("clistate.Save: %v", err)
	}
	got, err = b.LookupActiveClaim(t.Context())
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if got != nil {
		t.Errorf("LookupActiveClaim = %+v, want nil — another arena's lease is not this one's to resume", got)
	}

	// And a claim minted by a different orchestrator entirely is likewise not
	// ours to interpret.
	foreign := mine
	foreign.OrchestratorName = "tracker"
	if err := clistate.Save(foreign); err != nil {
		t.Fatalf("clistate.Save: %v", err)
	}
	if got, _ := b.LookupActiveClaim(t.Context()); got != nil {
		t.Errorf("LookupActiveClaim = %+v, want nil for another orchestrator's claim", got)
	}
}

// The claim names the arena the lease binds to. A handle to a lease that could
// not name what the lease binds leaves every holder unidentifiable wherever one
// account runs more than one arena.
func TestBackend_Claim_NamesTheArenaItBinds(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	t.Cleanup(srv.Close)
	b := newMockedOrchestrator(t, mock, srv)

	claim, err := b.Claim(t.Context(), b.refFromIssue(42), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claim.Arena.Empty() {
		t.Fatalf("Claim.Arena = %+v, want both halves — an ArenaId alone is a component, not a name", claim.Arena)
	}
	if claim.Arena != b.arena() {
		t.Errorf("Claim.Arena = %+v, want the arena this checkout is (%+v)", claim.Arena, b.arena())
	}
	// LookupClaim reports the arena too, but only because this checkout is the
	// holder — no label records it, so there is nothing to report otherwise.
	info, err := b.LookupClaim(t.Context(), b.refFromIssue(42))
	if err != nil || info == nil {
		t.Fatalf("LookupClaim = %+v, %v", info, err)
	}
	if info.Account != "alice" {
		t.Errorf("ClaimInfo.Account = %q, want alice", info.Account)
	}
	if info.Arena != b.arena() {
		t.Errorf("ClaimInfo.Arena = %+v, want this checkout's arena", info.Arena)
	}
}

// Finalize REFUSES an item that is not terminal. Finalizing does not MAKE one
// terminal — nothing here closes an issue — so a finalized item still open
// would claim the work is over while the orchestrator says it is not.
//
// The refusal is ErrUnavailable and not ErrUnsupported: the item may reach
// terminal later, so asking again is exactly what a caller should do.
func TestBackend_Finalize_RefusesAnItemThatIsNotTerminal(t *testing.T) {
	b, mock, rec := newFinalizeBackend(t)
	mock.mu.Lock()
	mock.issueState = "open" // nobody has closed it
	mock.mu.Unlock()

	claim := claimForFinalize(b)
	if err := clistate.Save(claim); err != nil {
		t.Fatalf("clistate.Save: %v", err)
	}

	err := b.Finalize(t.Context(), claim.ItemRef)
	if err == nil {
		t.Fatal("Finalize recorded a flow run complete on an item the orchestrator does not consider finished")
	}
	if !errors.Is(err, flow.ErrUnavailable) {
		t.Errorf("error = %v, want ErrUnavailable — the item may reach terminal later", err)
	}
	if errors.Is(err, flow.ErrUnsupported) {
		t.Error("the refusal reads as permanent; a caller would stop asking")
	}
	if !strings.Contains(err.Error(), string(flow.StatusTerminal)) {
		t.Errorf("error = %q, want it to say what state the item must be in", err)
	}

	// Nothing was done: no finalized flag, no checkout, and the claim survives.
	mock.mu.Lock()
	for _, c := range mock.comments {
		if doc, _, found, _ := extractStateDoc(c.Body); found && doc != nil && doc.Finalized {
			t.Error("the state comment carries finalized: true after the refusal")
		}
	}
	mock.mu.Unlock()
	if rec.called("checkout") {
		t.Error("the worktree was returned to base although nothing was finalized")
	}
	if c, _ := clistate.Load(); c == nil {
		t.Error("the claim was released although nothing was finalized")
	}
	if contains(mock.labelNames(), "flow:owner:alice") == false {
		t.Error("the owner label was stripped although nothing was finalized")
	}
}
