package github

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
)

// Two arenas, ONE account (#210).
//
// The exclusion that keeps two arenas off one item was written in terms of the
// account in all three places it is made — selection, availability and the
// claim preflight — so on the ordinary single-operator fleet, one login running
// several worktrees, it compared a value to itself. Three arenas on one host
// selected, claimed and ran the same item, three minutes apart, each believing
// it held an exclusive lease.
//
// The lease binds item ↔ arena (docs/orchestrator.md § Required surface →
// Claiming: "at most one item per arena, at most one arena per item"), so the
// arena is what the comparison has to be about. flow:arena:<fingerprint> is
// what puts the arena on the item, where the OTHER arena can read it.

// testArena is one arena's half of the two-arena harness: an Orchestrator with
// its own worktree path, its own .flow directory, and its own git recorder.
type testArena struct {
	t       *testing.T
	b       *Orchestrator
	rec     *gitRecorder
	flowDir string
}

// run makes this arena the current one for the duration of fn.
//
// It sets FLOW_DIR rather than leaving one t.Setenv standing, because
// clistate.Dir() reads the environment at EVERY call: two orchestrators built
// in one test would otherwise share one .flow/active.json — the very file whose
// one-per-checkout-ness is supposed to be the arena scoping — and every
// assertion below would be about a single arena wearing two names.
func (a *testArena) run(fn func()) {
	a.t.Helper()
	prev, had := os.LookupEnv("FLOW_DIR")
	if err := os.Setenv("FLOW_DIR", a.flowDir); err != nil {
		a.t.Fatalf("set FLOW_DIR: %v", err)
	}
	defer func() {
		if had {
			_ = os.Setenv("FLOW_DIR", prev)
		} else {
			_ = os.Unsetenv("FLOW_DIR")
		}
	}()
	fn()
}

// twoArenas builds two Orchestrators over ONE ghMock: same repository, same
// authenticated account ("alice"), different arenas.
//
// The harness is the part a naive version of this test gets wrong. Built over
// one worktree path and one lease file, "arena 2" is arena 1 and every
// assertion here passes against the code #210 reports. newMockedOrchestrator
// now gives each orchestrator its own absolute worktree and its own FLOW_DIR,
// so each is genuinely its own arena; the named paths below make that explicit
// per arena, and the up-front fingerprint-inequality check is what proves it
// rather than assuming it.
func twoArenas(t *testing.T) (*ghMock, *testArena, *testArena) {
	t.Helper()
	mock := newGHMock(t)
	mock.issueLabels = []string{"flow:implement"}
	mock.assignees = []string{"alice"}
	srv := mock.server()
	t.Cleanup(srv.Close)

	one := newTestArena(t, mock, srv, "one")
	two := newTestArena(t, mock, srv, "two")
	if one.b.arenaFingerprint() == two.b.arenaFingerprint() {
		t.Fatalf("both arenas fingerprint to %s — the harness built one arena twice, "+
			"and every assertion here would pass against the code #210 reports",
			one.b.arenaFingerprint())
	}
	return mock, one, two
}

func newTestArena(t *testing.T, mock *ghMock, srv *httptest.Server, name string) *testArena {
	t.Helper()
	b := newMockedOrchestrator(t, mock, srv)
	b.cfg.WorktreeDir = filepath.Join(t.TempDir(), name)
	rec := newGitRecorder()
	scriptCleanWorktree(rec)
	b.git.runner = rec.run
	return &testArena{t: t, b: b, rec: rec, flowDir: t.TempDir()}
}

// claimed has arena one take #42, leaving the item carrying the claim record
// the other arena has to read.
func (a *testArena) claimed(t *testing.T) flow.Claim {
	t.Helper()
	var c flow.Claim
	a.run(func() {
		got, err := a.b.Claim(t.Context(), a.b.refFromIssue(42), nil)
		if err != nil {
			t.Fatalf("first arena's Claim: %v", err)
		}
		c = got
	})
	return c
}

// acceptsAllTypes is the type filter for the listing assertions here: every one
// of them is about the holder, and a type refusal at level 3 would answer
// before the comparison under test is reached.
func acceptsAllTypes(flow.ItemType) bool { return true }

// The item another arena holds is ABSENT from the selectable set — eligibility,
// not a sort key. `assignee:@me` narrows to the account, which on a one-login
// fleet is every arena's own, so the query hands the same item to all of them;
// the post-filter is the only thing that can take it back.
func TestBackend_ListAutoSelectable_OmitsAnItemAnotherArenaHolds(t *testing.T) {
	mock, one, two := twoArenas(t)
	one.claimed(t)

	one.run(func() {
		refs, err := one.b.ListAutoSelectable(t.Context(), nil)
		if err != nil {
			t.Fatalf("holder's ListAutoSelectable: %v", err)
		}
		if len(refs) != 1 {
			t.Fatalf("the HOLDER got %d refs, want its own item back (labels %v)", len(refs), mock.labelNames())
		}
	})
	two.run(func() {
		refs, err := two.b.ListAutoSelectable(t.Context(), nil)
		if err != nil {
			t.Fatalf("ListAutoSelectable: %v", err)
		}
		if len(refs) != 0 {
			t.Errorf("got %v, want none — another arena holds #42 under this same account (labels %v)",
				refs, mock.labelNames())
		}
	})
}

// `list` and `status` must both report it held. They route through one
// derivation, so this is one rule read twice — but read it once and a listing
// that says `auto` for an item another arena is running stays a truthful-looking
// invitation to double-run it.
func TestBackend_ListAndGet_ReportAnItemAnotherArenaHoldsAsHeld(t *testing.T) {
	_, one, two := twoArenas(t)
	one.claimed(t)

	two.run(func() {
		items, err := two.b.List(t.Context(), flow.ScopeAll, "implement", acceptsAllTypes)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("List returned %d items, want #42", len(items))
		}
		if items[0].Availability != flow.AvailHeld {
			t.Errorf("List availability = %q, want %q", items[0].Availability, flow.AvailHeld)
		}

		info, err := two.b.Get(t.Context(), two.b.refFromIssue(42), "implement", acceptsAllTypes)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if info.Availability != flow.AvailHeld {
			t.Errorf("Get availability = %q, want %q — Get and List answer identically or neither is usable",
				info.Availability, flow.AvailHeld)
		}
		// The holder is nameable only by the arena that holds it: a digest
		// names no arena, so the reported Holder carries the account and an
		// empty arena half.
		if info.Holder.Account != "alice" {
			t.Errorf("Holder.Account = %q, want alice", info.Holder.Account)
		}
		if !info.Holder.Arena.Empty() {
			t.Errorf("Holder.Arena = %+v, want empty — a fingerprint cannot be turned back into an arena",
				info.Holder.Arena)
		}
	})
	// And the holder's own reading is unchanged: its arena half IS nameable.
	one.run(func() {
		info, err := one.b.Get(t.Context(), one.b.refFromIssue(42), "implement", acceptsAllTypes)
		if err != nil {
			t.Fatalf("holder's Get: %v", err)
		}
		if info.Availability != flow.AvailAuto {
			t.Errorf("the HOLDER reads its own item as %q, want %q", info.Availability, flow.AvailAuto)
		}
		if info.Holder.Arena != one.b.arena() {
			t.Errorf("Holder.Arena = %+v, want this arena %+v", info.Holder.Arena, one.b.arena())
		}
	})
}

// The claim preflight refuses, ITEM-SCOPED, and writes nothing. Item-scoped is
// what lets the caller's fall-through loop move to the next ref instead of
// giving up on the arena — the loop that exists precisely for this race and
// that never engaged, because nothing refused.
func TestBackend_Claim_RefusesAnItemAnotherArenaHoldsUnderTheSameAccount(t *testing.T) {
	mock, one, two := twoArenas(t)
	one.claimed(t)

	mock.mu.Lock()
	mock.mutations = nil
	mock.mu.Unlock()

	two.run(func() {
		_, err := two.b.Claim(t.Context(), two.b.refFromIssue(42), nil)
		if err == nil {
			t.Fatal("a second arena claimed an item the first holds")
		}
		var refused flow.ErrClaimRefused
		if !errors.As(err, &refused) {
			t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
		}
		if refused.Code != "already-held" {
			t.Errorf("Code = %q, want already-held", refused.Code)
		}
		if !refused.ItemScoped {
			t.Error("ItemScoped = false, want true — the ITEM is held, this arena is fine")
		}
		if refused.Override != "force" {
			t.Errorf("Override = %q, want force", refused.Override)
		}
		// The refusal has to say which of the three cases it is. "Held" alone
		// leaves an operator whose own login is on the item with nothing to act
		// on; the arena is what they have to go and look at, and the
		// fingerprint is the handle that matches this refusal to the
		// flow:arena: label on the issue.
		if !strings.Contains(refused.Reason, "another arena") {
			t.Errorf("Reason = %q, want it to name another ARENA — the account on the item is this operator's own",
				refused.Reason)
		}
		if !strings.Contains(refused.Reason, one.b.arenaFingerprint()) {
			t.Errorf("Reason = %q, want the holder's fingerprint %q, the only handle tying it to the item",
				refused.Reason, one.b.arenaFingerprint())
		}
	})

	// Nothing was written: no claim token minted, no ownership asserted on an
	// item this arena is not free to hold.
	mock.mu.Lock()
	mutations := append([]string(nil), mock.mutations...)
	mock.mu.Unlock()
	if len(mutations) != 0 {
		t.Errorf("a refused claim wrote to GitHub: %v", mutations)
	}
	if contains(mock.labelNames(), two.b.labels.Arena(two.b.arenaFingerprint())) {
		t.Errorf("labels = %v, want no arena label for the refused claimer", mock.labelNames())
	}
}

// The comparison has to hold when the LEASE IS TAKEN, not only when the
// preflight read it. Between the two sit the worktree preconditions, and the
// first is `git fetch origin` — seconds on a real repository, against a token
// race sized for two API calls. An arena that finishes its claim inside that
// window is invisible to the settle, because the settle compares flow:claim:*
// tokens and the holder removed its own token as the last act of Phase 3.
//
// So the second arena won its race uncontested and Phase 3 stripped the
// holder's arena label as stale: a take-over with no override asked for. The
// same defect as the reported one, reached through the fetch rather than
// through selection.
func TestBackend_Claim_RefusesAnArenaThatTookTheItemDuringOurPreconditions(t *testing.T) {
	mock, one, two := twoArenas(t)

	// Arena two's preflight sees an unclaimed item; arena one takes it while
	// two is fetching.
	fetched := false
	two.rec.handlers["fetch origin"] = func([]string) ([]byte, error) {
		if !fetched {
			fetched = true
			one.claimed(t)
		}
		return nil, nil
	}

	two.run(func() {
		_, err := two.b.Claim(t.Context(), two.b.refFromIssue(42), nil)
		if err == nil {
			t.Fatal("a claim taken during our preconditions must not be overrun")
		}
		var refused flow.ErrClaimRefused
		if !errors.As(err, &refused) {
			t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
		}
		if refused.Code != "already-held" || !refused.ItemScoped {
			t.Errorf("refusal = %+v, want already-held and item-scoped", refused)
		}
	})
	if !fetched {
		t.Fatal("the fetch never ran, so the window this test is about never opened")
	}

	// The holder's record is intact, and no token of the refused claimer is
	// left behind to block the next one.
	after := mock.labelNames()
	if !contains(after, one.b.labels.Arena(one.b.arenaFingerprint())) {
		t.Errorf("labels = %v, want the holder's fingerprint untouched", after)
	}
	if contains(after, two.b.labels.Arena(two.b.arenaFingerprint())) {
		t.Errorf("labels = %v, want no fingerprint for the refused claimer", after)
	}
	for _, name := range after {
		if _, ok := two.b.labels.ClaimTokenFromLabel(name); ok {
			t.Errorf("labels = %v, want the refused attempt's claim token removed", after)
		}
	}
	// And the holder still holds it.
	one.run(func() {
		if _, err := one.b.Claim(t.Context(), one.b.refFromIssue(42), nil); err != nil {
			t.Errorf("the holder's re-claim must still succeed: %v", err)
		}
	})
}

// --force still takes over, and the take-over DISPLACES the previous arena's
// record. Under one account the owner label is byte-identical between the two,
// so the arena label is the only thing that changes hands — and a take-over
// that left the old one behind would leave the item reading as held by both.
func TestBackend_Claim_ForceTakesOverFromAnotherArena(t *testing.T) {
	mock, one, two := twoArenas(t)
	one.claimed(t)

	two.run(func() {
		if _, err := two.b.Claim(t.Context(), two.b.refFromIssue(42),
			[]flow.ClaimOverride{flow.OverrideAlreadyHeld}); err != nil {
			t.Fatalf("--force must still take the item over: %v", err)
		}
	})

	after := mock.labelNames()
	if !contains(after, two.b.labels.Arena(two.b.arenaFingerprint())) {
		t.Errorf("labels = %v, want the taking-over arena's fingerprint", after)
	}
	if contains(after, one.b.labels.Arena(one.b.arenaFingerprint())) {
		t.Errorf("labels = %v, want the displaced arena's fingerprint gone", after)
	}
	if !contains(after, "flow:owner:alice") {
		t.Errorf("labels = %v, want the owner label — the account is unchanged", after)
	}
}

// The holder's own re-claim is still idempotent (#201): it succeeds, returns
// the standing lease, and changes nothing. The arena comparison must not turn
// "I hold this" into a refusal.
func TestBackend_Claim_HolderReclaimUnaffectedByTheArenaComparison(t *testing.T) {
	mock, one, _ := twoArenas(t)
	first := one.claimed(t)
	labelsAfterFirst := mock.labelNames()

	mock.mu.Lock()
	mock.mutations = nil
	mock.mu.Unlock()

	one.run(func() {
		second, err := one.b.Claim(t.Context(), one.b.refFromIssue(42), nil)
		if err != nil {
			t.Fatalf("the holder's re-claim must succeed: %v", err)
		}
		if !sameLease(first, second) {
			t.Errorf("re-claim returned %+v, want the standing lease %+v", second, first)
		}
	})

	mock.mu.Lock()
	mutations := append([]string(nil), mock.mutations...)
	mock.mu.Unlock()
	if len(mutations) != 0 {
		t.Errorf("a held re-claim wrote to GitHub: %v", mutations)
	}
	if after := mock.labelNames(); !slices.Equal(after, labelsAfterFirst) {
		t.Errorf("labels = %v, want them unchanged at %v", after, labelsAfterFirst)
	}
}

// ---------------------------------------------------------------------------
// Records written before flow:arena: existed
// ---------------------------------------------------------------------------

// A bare flow:owner:<login> says SOME arena holds this and does not say which.
// The only arena that can prove it is the holder is the one whose own lease
// file says so — so the holder re-claims idempotently.
//
// This is not a tolerance for old data: flow:owner: is written by Claim's Phase
// 3 and by nothing else — not by seeding — so the label always means a claim
// was taken.
func TestBackend_Claim_LegacyOwnerLabelIsAReclaimForTheArenaHoldingIt(t *testing.T) {
	mock, one, _ := twoArenas(t)
	lease := flow.Claim{
		OrchestratorName: one.b.Name(),
		ItemRef:          one.b.refFromIssue(42),
		Arena:            one.b.arena(),
		Account:          "alice",
		ClaimedAt:        nowUTC(),
		Token:            json.RawMessage(`{"state_comment_id":1,"claim_id":"legacy"}`),
	}
	one.run(func() {
		if err := clistate.Save(lease); err != nil {
			t.Fatalf("seed lease file: %v", err)
		}
	})
	mock.mu.Lock()
	mock.issueLabels = []string{"flow:implement", "flow:owner:alice"} // no arena label
	mock.mutations = nil
	mock.mu.Unlock()
	// Mid-work, so a fresh claim would refuse on the worktree preconditions and
	// only the idempotent return can succeed.
	one.rec.handlers["rev-parse --abbrev-ref HEAD"] = func([]string) ([]byte, error) {
		return []byte("flow/issue-42\n"), nil
	}

	one.run(func() {
		got, err := one.b.Claim(t.Context(), one.b.refFromIssue(42), nil)
		if err != nil {
			t.Fatalf("the holder of a legacy record must still re-claim idempotently: %v", err)
		}
		if !sameLease(lease, got) {
			t.Errorf("re-claim returned %+v, want the standing lease %+v", got, lease)
		}
	})
	mock.mu.Lock()
	mutations := append([]string(nil), mock.mutations...)
	mock.mu.Unlock()
	if len(mutations) != 0 {
		t.Errorf("a held re-claim wrote to GitHub: %v", mutations)
	}
}

// Every arena that is NOT the holder is refused on the same record. The
// residual cost is that an owner label left by a crashed holder needs --force,
// which is the documented break-glass and what #222 exists to make conditional;
// the alternative — reading "no arena named" as free — is the reported defect.
func TestBackend_Claim_LegacyOwnerLabelRefusesEveryOtherArena(t *testing.T) {
	mock, _, two := twoArenas(t)
	mock.mu.Lock()
	mock.issueLabels = []string{"flow:implement", "flow:owner:alice"} // no arena label
	mock.mu.Unlock()

	two.run(func() {
		_, err := two.b.Claim(t.Context(), two.b.refFromIssue(42), nil)
		if err == nil {
			t.Fatal("an owner label naming no arena must not read as free")
		}
		var refused flow.ErrClaimRefused
		if !errors.As(err, &refused) {
			t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
		}
		if refused.Code != "already-held" || !refused.ItemScoped {
			t.Errorf("refusal = %+v, want already-held and item-scoped", refused)
		}
		// And it says what is missing, because that is what tells the operator
		// this needs --force rather than waiting for the holder to finish.
		if !strings.Contains(refused.Reason, "records no arena") {
			t.Errorf("Reason = %q, want it to say the record names no arena", refused.Reason)
		}
	})
	two.run(func() {
		if _, err := two.b.Claim(t.Context(), two.b.refFromIssue(42),
			[]flow.ClaimOverride{flow.OverrideAlreadyHeld}); err != nil {
			t.Fatalf("--force is the break-glass and must still work: %v", err)
		}
	})
}

// A lease file that says we hold the item is EVIDENCE, not authority: the
// server decides. When another arena has taken the item over — under this same
// account, so the owner label reads as ours — the refusal must fire rather than
// the short-circuit handing back a lease we no longer hold.
//
// A behaviour change: before the arena half existed, the account comparison saw
// nothing wrong and the stale lease was returned as valid.
func TestBackend_Claim_StaleLeaseDoesNotSurviveATakeOverByAnotherArena(t *testing.T) {
	mock, one, two := twoArenas(t)
	one.claimed(t)
	// Arena two takes it over. Arena one's lease file still names #42.
	two.run(func() {
		if _, err := two.b.Claim(t.Context(), two.b.refFromIssue(42),
			[]flow.ClaimOverride{flow.OverrideAlreadyHeld}); err != nil {
			t.Fatalf("take-over: %v", err)
		}
	})

	one.run(func() {
		active, err := one.b.LookupActiveClaim(t.Context())
		if err != nil || active == nil {
			t.Fatalf("the displaced arena's lease file should still name #42: (%v, %v)", active, err)
		}
		_, err = one.b.Claim(t.Context(), one.b.refFromIssue(42), nil)
		if err == nil {
			t.Fatal("a lease the server has since reassigned must not be handed back as valid")
		}
		var refused flow.ErrClaimRefused
		if !errors.As(err, &refused) {
			t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
		}
		if refused.Code != "already-held" || !refused.ItemScoped {
			t.Errorf("refusal = %+v, want already-held and item-scoped", refused)
		}
	})
	if !contains(mock.labelNames(), two.b.labels.Arena(two.b.arenaFingerprint())) {
		t.Errorf("labels = %v, want the taking-over arena still recorded", mock.labelNames())
	}
}

// The other account is the case #220 described, and it is unchanged at the
// claim — but it is now excluded from SELECTION too, which is what #220 asked
// for and what the account comparison alone never did here.
func TestBackend_AnItemHeldByAnotherAccountIsRefusedAndUnselectable(t *testing.T) {
	mock, _, two := twoArenas(t)
	mock.mu.Lock()
	mock.issueLabels = []string{"flow:implement", "flow:owner:bob"}
	mock.mu.Unlock()

	two.run(func() {
		_, err := two.b.Claim(t.Context(), two.b.refFromIssue(42), nil)
		var refused flow.ErrClaimRefused
		if !errors.As(err, &refused) {
			t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
		}
		if refused.Code != "already-held" {
			t.Errorf("Code = %q, want already-held", refused.Code)
		}
		if !strings.Contains(refused.Reason, "carries owner label for bob") {
			t.Errorf("Reason = %q, want the unchanged message naming bob", refused.Reason)
		}

		refs, err := two.b.ListAutoSelectable(t.Context(), nil)
		if err != nil {
			t.Fatalf("ListAutoSelectable: %v", err)
		}
		if len(refs) != 0 {
			t.Errorf("got %v, want none — bob holds #42", refs)
		}
	})
}

// otherBinaryLabel reads any unrecognised flow: label as another binary's name,
// so a structural label missing from its skip list makes EVERY claim on an item
// carrying it refuse other-binary. flow:arena: is structural; this is the
// negative of the other-binary table.
func TestBackend_Claim_ArenaLabelIsNotReadAsAnotherBinary(t *testing.T) {
	mock, one, _ := twoArenas(t)
	mock.mu.Lock()
	mock.issueLabels = []string{"flow:implement", one.b.labels.Arena("0123456789abcdef")}
	mock.mu.Unlock()

	if other, wrong := one.b.otherBinaryLabel(mock.labelNames()); wrong {
		t.Fatalf("otherBinaryLabel read the arena label as binary %q", other)
	}
	one.run(func() {
		if _, err := one.b.Claim(t.Context(), one.b.refFromIssue(42), nil); err != nil {
			t.Fatalf("an item carrying an arena label but no owner label is claimable: %v", err)
		}
	})
}

// The arena label is a marker Claim maintains, so removing it directly is
// refused for the same reason the owner label's removal is: a caller able to
// delete one could make an item report a state no operation put it in — here,
// free for the taking while another arena runs it.
func TestEditor_RefusesToRemoveTheArenaMarker(t *testing.T) {
	marker := newLabels("flow:").Arena("0123456789abcdef")
	mock, b := editingOrchestrator(t, "flow:implement", marker)
	ed, err := b.Edit(t.Context(), b.refFromIssue(42))
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	ed.RemoveTag(flow.TagId(marker))
	if err := ed.Commit(t.Context()); err == nil {
		t.Fatalf("Commit removed %q, a marker Claim maintains", marker)
	}
	if !contains(mock.labelNames(), marker) {
		t.Errorf("labels = %v, want %q still present", mock.labelNames(), marker)
	}
}

// Release removes BOTH halves of the claim record. An arena label left behind
// makes the item read as held by this arena forever, and every other arena —
// including this one from a different worktree — would need --force.
func TestBackend_Release_RemovesTheArenaLabel(t *testing.T) {
	mock, one, _ := twoArenas(t)
	one.claimed(t)
	fingerprint := one.b.labels.Arena(one.b.arenaFingerprint())
	if !contains(mock.labelNames(), fingerprint) {
		t.Fatalf("labels = %v, want the claim to have written %q", mock.labelNames(), fingerprint)
	}

	one.run(func() {
		if err := one.b.Release(t.Context(), one.b.refFromIssue(42)); err != nil {
			t.Fatalf("Release: %v", err)
		}
	})
	if contains(mock.labelNames(), fingerprint) {
		t.Errorf("labels = %v, want %q removed", mock.labelNames(), fingerprint)
	}

	// A second Release finds neither label. GitHub answers a DELETE of a label
	// the issue does not carry with 404, and Release tolerates exactly that —
	// so releasing twice is not an error a caller has to guard against.
	mock.mu.Lock()
	mock.strictLabelRemoval = true
	mock.mu.Unlock()
	one.run(func() {
		if err := one.b.Release(t.Context(), one.b.refFromIssue(42)); err != nil {
			t.Errorf("a second Release must not error on labels already gone: %v", err)
		}
	})
}

// The two removals are two requests, so one of them can be the last thing that
// happens. Release is giving the lease UP, so the half it takes off first
// decides what a failure between them leaves — and the item must stay reading
// as HELD, because this arena's lease file is still on disk (Release returns
// before clearing it) and an item reading free while an arena still holds a
// lease on it is #210 again, by a different route.
func TestBackend_Release_PartialFailureLeavesTheItemReadingHeld(t *testing.T) {
	mock, one, two := twoArenas(t)
	one.claimed(t)

	// The arena half is the one that will not come off. Taking it off FIRST
	// means nothing else is removed either; taking it off second means the
	// owner half is already gone when this fails, and what is left on the item
	// is an arena label alone.
	mock.mu.Lock()
	mock.failRemoveLabel = map[string]bool{one.b.labels.Arena(one.b.arenaFingerprint()): true}
	mock.mu.Unlock()

	one.run(func() {
		if err := one.b.Release(t.Context(), one.b.refFromIssue(42)); err == nil {
			t.Fatal("Release must surface the failed removal, not report success")
		}
		// The lease file is untouched, so this arena can retry the release —
		// and, until it does, still re-claim its own item idempotently.
		active, err := one.b.LookupActiveClaim(t.Context())
		if err != nil || active == nil {
			t.Fatalf("a failed Release must leave the lease file: (%v, %v)", active, err)
		}
	})

	after := mock.labelNames()
	if !contains(after, "flow:owner:alice") {
		t.Fatalf("labels = %v, want the owner half still present — removed first, it leaves an arena "+
			"label alone, which reads as unclaimed while this arena still holds the lease", after)
	}
	// What every other arena reads off that half-removed record: still held.
	two.run(func() {
		refs, err := two.b.ListAutoSelectable(t.Context(), nil)
		if err != nil {
			t.Fatalf("ListAutoSelectable: %v", err)
		}
		if len(refs) != 0 {
			t.Errorf("got %v, want none — a half-released item is not free (labels %v)", refs, after)
		}
		if _, err := two.b.Claim(t.Context(), two.b.refFromIssue(42), nil); err == nil {
			t.Error("a half-released item must not be claimable by another arena")
		}
	})
}

// The mirror of the rule above, read from the other end: Claim's rollback drops
// the OWNER half first, because there the claim failed and the state to leave
// is one that reads free. What that leaves behind is an arena label with no
// owner label — half a record, and half a record is not a holder.
func TestBackend_AnArenaLabelWithoutAnOwnerLabelIsNotAHolder(t *testing.T) {
	mock, one, _ := twoArenas(t)
	mock.mu.Lock()
	mock.issueLabels = []string{"flow:implement", one.b.labels.Arena(one.b.arenaFingerprint())}
	mock.mu.Unlock()

	one.run(func() {
		info, err := one.b.Get(t.Context(), one.b.refFromIssue(42), "implement", acceptsAllTypes)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !info.Holder.Empty() {
			t.Errorf("Holder = %+v, want empty — the record's other half is not there", info.Holder)
		}
		if info.Availability != flow.AvailAuto {
			t.Errorf("Availability = %q, want %q: a Holder for an item the same read reports free "+
				"is a contradiction a reader cannot settle", info.Availability, flow.AvailAuto)
		}
		if claim, err := one.b.LookupClaim(t.Context(), one.b.refFromIssue(42)); err != nil || claim != nil {
			t.Errorf("LookupClaim = (%+v, %v), want (nil, nil)", claim, err)
		}
	})
}

// LookupClaim answers off the ITEM, never off this arena's lease file. A file
// saying "I hold #42" is what an arena displaced by a take-over goes on saying,
// and reporting ITSELF as the holder of an item another arena is running is the
// same stale-authority reading Claim's preflight was fixed to stop making.
func TestBackend_LookupClaim_DoesNotNameADisplacedArenaAsTheHolder(t *testing.T) {
	_, one, two := twoArenas(t)
	one.claimed(t)
	two.run(func() {
		if _, err := two.b.Claim(t.Context(), two.b.refFromIssue(42),
			[]flow.ClaimOverride{flow.OverrideAlreadyHeld}); err != nil {
			t.Fatalf("take-over: %v", err)
		}
	})

	one.run(func() {
		info, err := one.b.LookupClaim(t.Context(), one.b.refFromIssue(42))
		if err != nil || info == nil {
			t.Fatalf("LookupClaim = (%+v, %v), want the standing claim", info, err)
		}
		// The account is unchanged — one login, two arenas, which is the whole
		// reason the account could not separate them.
		if info.Account != "alice" {
			t.Errorf("Account = %q, want alice", info.Account)
		}
		if !info.Arena.Empty() {
			t.Errorf("Arena = %+v, want empty — this arena was displaced, and a foreign fingerprint "+
				"names no arena", info.Arena)
		}
	})
	// And the arena that DOES hold it still names itself.
	two.run(func() {
		info, err := two.b.LookupClaim(t.Context(), two.b.refFromIssue(42))
		if err != nil || info == nil {
			t.Fatalf("holder's LookupClaim = (%+v, %v)", info, err)
		}
		if info.Arena != two.b.arena() {
			t.Errorf("Arena = %+v, want the holding arena %+v", info.Arena, two.b.arena())
		}
	})
}

// The other side of the same record: the arena HOLDING a claim that names no
// arena still sees its own item, in the listing as well as at the claim.
//
// This is the branch of holdsItem the refusals above cannot reach. The item
// cannot say which arena holds it, so the only arena that can answer is the one
// whose own lease file names the item, and the listing has to ask — a fleet
// upgrading mid-flight would otherwise have every holder's own item disappear
// from its selectable set and read as held against itself, which is #210 with
// the sign flipped.
func TestBackend_Listing_HolderOfARecordNamingNoArenaStillSeesItsOwnItem(t *testing.T) {
	mock, one, _ := twoArenas(t)
	mock.mu.Lock()
	mock.issueLabels = []string{"flow:implement", "flow:owner:alice"} // no arena label
	mock.mu.Unlock()

	one.run(func() {
		if err := clistate.Save(flow.Claim{
			OrchestratorName: one.b.Name(),
			ItemRef:          one.b.refFromIssue(42),
			Arena:            one.b.arena(),
			Account:          "alice",
			ClaimedAt:        nowUTC(),
		}); err != nil {
			t.Fatalf("seed lease file: %v", err)
		}
		refs, err := one.b.ListAutoSelectable(t.Context(), nil)
		if err != nil {
			t.Fatalf("ListAutoSelectable: %v", err)
		}
		if len(refs) != 1 {
			t.Errorf("got %v, want #42 — this arena's lease file says it holds it (labels %v)",
				refs, mock.labelNames())
		}
		info, err := one.b.Get(t.Context(), one.b.refFromIssue(42), "implement", acceptsAllTypes)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if info.Availability != flow.AvailAuto {
			t.Errorf("Availability = %q, want %q — the holder's own item is not held against it",
				info.Availability, flow.AvailAuto)
		}
	})
}

// A lease file that cannot be READ answers the same question with "no", and the
// item reads as held. That is the conservative direction for a listing and the
// only one that cannot widen what an unattended run starts on: the alternative
// — treating an unreadable file as "we must be the holder" — hands the item to
// an arena that has no evidence it holds anything.
//
// The listing itself must still answer. `list` failing outright because one
// arena's state file is corrupt would take the whole command down over a
// question about one item, and Claim is where an unreadable lease is fatal
// (TestBackend_Claim_UnreadableLeaseFileRefusesWithoutClaiming) — it takes a
// lease on the answer, a listing only reports it.
func TestBackend_Listing_AnUnreadableLeaseFileReadsTheItemAsHeld(t *testing.T) {
	mock, one, _ := twoArenas(t)
	mock.mu.Lock()
	mock.issueLabels = []string{"flow:implement", "flow:owner:alice"} // no arena label
	mock.mu.Unlock()

	one.run(func() {
		if err := os.WriteFile(activeJSONPath(t), []byte("{truncated"), 0o644); err != nil {
			t.Fatalf("write lease file: %v", err)
		}
		refs, err := one.b.ListAutoSelectable(t.Context(), nil)
		if err != nil {
			t.Fatalf("ListAutoSelectable must answer despite an unreadable lease file: %v", err)
		}
		if len(refs) != 0 {
			t.Errorf("got %v, want none — an unreadable lease file is no evidence that this arena holds #42", refs)
		}
		info, err := one.b.Get(t.Context(), one.b.refFromIssue(42), "implement", acceptsAllTypes)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if info.Availability != flow.AvailHeld {
			t.Errorf("Availability = %q, want %q", info.Availability, flow.AvailHeld)
		}
	})
}

// The two halves go on in ONE request. Split across two, the item carries a
// half record for as long as the second takes — and every reader of a half
// record decides differently: an owner label alone is held-by-everyone-but-the
// -holder, an arena label alone is not a claim at all. Only the removals are
// meant to be observable halfway, and those are ordered per path (Release and
// the rollback below) so that the halfway state is the true one.
func TestBackend_Claim_WritesBothHalvesOfTheRecordInOneRequest(t *testing.T) {
	mock, one, _ := twoArenas(t)
	one.claimed(t)

	owner := one.b.labels.Owner("alice")
	arena := one.b.labels.Arena(one.b.arenaFingerprint())
	mock.mu.Lock()
	batches := append([][]string(nil), mock.labelAdds...)
	mock.mu.Unlock()

	for _, batch := range batches {
		if !contains(batch, owner) {
			continue
		}
		if !contains(batch, arena) {
			t.Errorf("the owner half was posted as %v, without the arena half; every add was %v", batch, batches)
		}
		return
	}
	t.Fatalf("no request added %q at all; adds = %v", owner, batches)
}

// A claim that posted its record and then could not write its lease file takes
// the record back off, and the item is free again — no lease was taken, so
// nothing may be left holding it.
//
// The ORDER of the two removals is asserted directly, and it has to be: both
// are best-effort within one process, so nothing observable here separates them
// — what separates them is a process that stops between the two requests, which
// is the case docs/github-schema.md pairs with Release's opposite order. Owner
// half first, so the halfway state reads FREE. Reversed, what survives is an
// owner label naming no arena, which is held by every arena except the one
// whose lease file says otherwise — and here that is nobody, since the lease
// file is precisely what could not be written. The item would need --force with
// no arena running it.
func TestBackend_Claim_RollbackOfAFailedLeaseSaveLeavesTheItemReadingFree(t *testing.T) {
	mock, one, two := twoArenas(t)

	// A lease file that cannot be written is the only way into the rollback:
	// the record is on the item by then, and Save is the next thing that runs.
	if err := os.Chmod(one.flowDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(one.flowDir, 0o755) })
	probe := filepath.Join(one.flowDir, "probe")
	if err := os.WriteFile(probe, nil, 0o644); err == nil {
		_ = os.Remove(probe)
		t.Skip("this filesystem does not enforce directory permissions (running as root?), " +
			"so the lease save cannot be made to fail")
	}

	one.run(func() {
		if _, err := one.b.Claim(t.Context(), one.b.refFromIssue(42), nil); err == nil {
			t.Fatal("Claim must fail when it cannot record the lease it just took")
		}
	})

	owner := one.b.labels.Owner("alice")
	arena := one.b.labels.Arena(one.b.arenaFingerprint())
	after := mock.labelNames()
	if contains(after, owner) || contains(after, arena) {
		t.Fatalf("labels = %v, want both halves gone — a claim that did not stand records nothing", after)
	}

	mock.mu.Lock()
	mutations := append([]string(nil), mock.mutations...)
	mock.mu.Unlock()
	ownerAt := slices.IndexFunc(mutations, func(m string) bool {
		return strings.HasPrefix(m, "DELETE ") && strings.HasSuffix(m, "/labels/"+owner)
	})
	arenaAt := slices.IndexFunc(mutations, func(m string) bool {
		return strings.HasPrefix(m, "DELETE ") && strings.HasSuffix(m, "/labels/"+arena)
	})
	if ownerAt < 0 || arenaAt < 0 {
		t.Fatalf("requests = %v, want a DELETE of each half", mutations)
	}
	if ownerAt > arenaAt {
		t.Errorf("the arena half was removed first; requests = %v — a rollback cut off between the two "+
			"must leave the item reading free, and an owner label with no arena beside it reads as held",
			mutations)
	}

	// And the item is one the next arena can take, with nothing to override.
	two.run(func() {
		if _, err := two.b.Claim(t.Context(), two.b.refFromIssue(42), nil); err != nil {
			t.Errorf("an item whose claim was rolled back must be claimable without --force: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// The fingerprint itself
// ---------------------------------------------------------------------------

// fingerprintArena is pure, so these need no hostname and no worktree.
//
// Fixtures are synthetic pairs, never a real home path: bin/precommit-guard
// refuses absolute home paths and anything address-shaped.
func TestFingerprintArena(t *testing.T) {
	h1w1 := flow.Arena{Host: "h1", Id: "/w/one"}
	h1w2 := flow.Arena{Host: "h1", Id: "/w/two"}
	h2w1 := flow.Arena{Host: "h2", Id: "/w/one"}

	// Stable: the same host and the same absolute path fingerprint the same
	// across restarts, which is what docs/orchestrator.md asks of an ArenaId.
	if fingerprintArena(h1w1) != fingerprintArena(flow.Arena{Host: "h1", Id: "/w/one"}) {
		t.Error("fingerprintArena is not stable for one (host, path)")
	}
	// Two worktrees on one host are two arenas — the reported configuration.
	if fingerprintArena(h1w1) == fingerprintArena(h1w2) {
		t.Error("two worktrees on one host fingerprint alike")
	}
	// And ArenaId alone is not an identity: the same path on two hosts is two
	// arenas, which is why the host is in the digest.
	if fingerprintArena(h1w1) == fingerprintArena(h2w1) {
		t.Error("one path on two hosts fingerprints alike")
	}
	// The pair goes in separated, so ("h1", "x/y") and ("h1x", "/y") cannot
	// collide by concatenation.
	if fingerprintArena(flow.Arena{Host: "h1", Id: "x"}) ==
		fingerprintArena(flow.Arena{Host: "h1x", Id: ""}) {
		t.Error("the host and the id run together in the digest")
	}
	// The label has to fit: GitHub caps a label name at 50 characters.
	if name := newLabels("flow:").Arena(fingerprintArena(h1w1)); len(name) > 50 {
		t.Errorf("label %q is %d characters, over GitHub's 50-character cap", name, len(name))
	}
	// Opaque: neither half of the pair is readable off it. This is the
	// disclosure requirement, not a size optimisation — an ArenaId is an
	// absolute filesystem path and a HostId is a machine name, and
	// docs/disclosure.md closes both categories.
	fp := fingerprintArena(h1w1)
	if strings.Contains(fp, "h1") || strings.Contains(fp, "one") || strings.Contains(fp, "/") {
		t.Errorf("fingerprint %q carries the arena in the clear", fp)
	}
}
