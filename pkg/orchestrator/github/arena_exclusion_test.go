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
// The harness is the part a naive version of this test gets wrong.
// newMockedOrchestrator never sets cfg.WorktreeDir, so every orchestrator in
// this package answers the same arena() and the same fingerprint; and it points
// FLOW_DIR at one tempdir, so two of them share one lease file. Built that way,
// "arena 2" is arena 1 and the assertions pass against the broken code. Hence
// the distinct worktree paths, the distinct FLOW_DIRs, and the up-front check
// that the two fingerprints actually differ.
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
