package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// A claim is taken FOR THE ROLE THE ITEM AWAITS (docs/resolution.md
// § Claiming). A run that cannot assume that role is refused before any lease
// exists: claiming an item whose pending work is not yours to do wastes an
// exclusive claim on work that cannot proceed.
//
// The refusal names all three facts a person needs to act on it — what the item
// awaits, who holds that role, and what this run can take instead.
func TestCmdClaim_AnItemAwaitingAnotherRoleIsRefused(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", reworkedByAlice())
	inner.SetCapabilities("", flow.CapPush)
	be := &recordingClaimBackend{Orchestrator: inner}
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 1 {
		t.Fatalf("exit code = %d, want 1 — the claim is refused; err=%q", code, errBuf.String())
	}
	got := errBuf.String()
	for _, want := range []string{"awaits maintainer", "account of record alice", "this run can assume: contributor"} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not name %q; got %q", want, got)
		}
	}
	if be.claims != 0 {
		t.Errorf("Backend.Claim was called %d time(s) on a refused claim", be.claims)
	}
	item, err := inner.Load(context.Background(), inner.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !item.Holder.Empty() {
		t.Errorf("a refused claim left the item held by %+v", item.Holder)
	}
}

// The counterpart, without which the test above would pass on a `claim` that
// refused everything: an item awaiting a role this run CAN assume is claimed.
func TestCmdClaim_AnItemAwaitingAnAssumableRoleIsClaimed(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1",
		Awaits: flow.Awaits{Role: "contributor"},
		Journal: []flow.JournalEntry{{
			Step: "commit", Execution: 1, Route: flow.Route{Next: "plan"},
			By: "alice", Role: "maintainer", Awaits: flow.Awaits{Role: "contributor"},
		}}})
	be.SetCapabilities("", flow.CapPush)
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	assertHeldHere(t, be, "1")
}

// An item that has not started awaits nobody, and nothing bars a claim on it:
// the rule is about a role, and there is no role until the journal has an entry.
func TestCmdClaim_AnUnstartedItemIsClaimed(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be.SetCapabilities("", flow.CapPush)
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	assertHeldHere(t, be, "1")
}

// An awaited SIGNAL is nobody's move, and nobody's move is not somebody else's:
// the item is claimable, and the wait is reported through the advance. Refusing
// here would leave an item nobody could pick up until the orchestrator happened
// to observe the signal.
func TestCmdClaim_AnItemAwaitingASignalIsClaimed(t *testing.T) {
	be := fake.New(flow.Signal("pr-open", "the pull request is open"))
	be.AddItem("1", flow.Item{Type: "task", Title: "1",
		Awaits: flow.Awaits{Signal: "pr-open"},
		Journal: []flow.JournalEntry{{
			Step: "plan", Execution: 1, Route: flow.Route{Next: "pr-open"},
			By: "bob", Role: "contributor", Awaits: flow.Awaits{Signal: "pr-open"},
		}}})
	be.SetCapabilities("", flow.CapPush)
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0 — an awaited signal is nobody's move; err=%q", code, errBuf.String())
	}
	assertHeldHere(t, be, "1")
}

// An awaited role outside the declared set is refused as what it is — a record
// nothing can ever match — and NOT as "somebody else's move", which would send
// the operator looking for a runner that does not exist. The two refusals are
// told apart by their wording as well as their type (docs/resolution.md § Whose
// move it is).
func TestCmdClaim_AnUndeclaredAwaitedRoleIsRefusedAsUnknown(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", awaitingAnUndeclaredRole())
	inner.SetCapabilities("", flow.CapPush)
	be := &recordingClaimBackend{Orchestrator: inner}
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	got := errBuf.String()
	if !strings.Contains(got, `role "reviewer" is not declared by this flow`) {
		t.Errorf("the refusal does not name the unknown role; got %q", got)
	}
	if !strings.Contains(got, "declared roles are [contributor maintainer]") {
		t.Errorf("the refusal does not name the declared set; got %q", got)
	}
	if strings.Contains(got, "this run can assume") {
		t.Errorf("an undeclared role was reported as somebody else's move; got %q", got)
	}
	if be.claims != 0 {
		t.Errorf("Backend.Claim was called %d time(s) on a refused claim", be.claims)
	}
}

// A claim is IDEMPOTENT FOR ITS HOLDER (docs/resolution.md § Claiming), and the
// role rule does not take that back: an arena that stopped at a boundary — a
// park, an interruption — must be able to re-take the item it is already
// holding, which is what lets it hand the item on properly.
func TestCmdClaim_ReclaimingAHeldItemSurvivesAnUnassumableRole(t *testing.T) {
	be := fake.New()
	be.AddItem("1", awaitingMaintainer())
	heldHere(t, be, "1")
	be.SetCapabilities("", flow.CapPush)
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0 — re-claiming is idempotent for the holder; err=%q", code, errBuf.String())
	}
	assertHeldHere(t, be, "1")
}

// Undetectable capabilities filter nothing within coverage: an orchestrator that
// cannot answer the capability question has said NOTHING about the account, and
// refusing a claim on the strength of a fact nobody established would stop a run
// that may well be entitled to proceed. Mirror of
// TestCmdResolve_UndetectableCapabilitiesHandNothingOff.
func TestCmdClaim_UndetectableCapabilitiesRefuseNoCoveredRole(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", awaitingMaintainer())
	be := &undetectableCapabilities{Orchestrator: inner, err: errors.New("the forge will not say")}
	app, errBuf := handoffTestApp(t, be) // covers both roles

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0 — a covered role stays assumable; err=%q", code, errBuf.String())
	}
	assertHeldHere(t, inner, "1")
}

// A FINALIZED item's marker is the last role that moved, and it is stale: the
// work is over and nobody is awaited. Refusing a claim on it would make an
// operator's inspection of a finished item depend on which role happened to
// finish it — the same exemption the advance makes for its own handoff branch.
func TestCmdClaim_AFinalizedItemIsNotRefusedOnAStaleMarker(t *testing.T) {
	be := fake.New()
	item := awaitingMaintainer()
	item.Finalized = true
	be.AddItem("1", item)
	be.SetCapabilities("", flow.CapPush)
	app, errBuf := handoffTestAppCovering(t, be, "contributor")

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0 — a finished item awaits nobody; err=%q", code, errBuf.String())
	}
	assertHeldHere(t, be, "1")
}

// An item outside this binary's remit is not refused on its marker: no flow of
// this runner's ever derived a step for it, so the role it names is another
// flow's vocabulary — read as this one's it would be an unknown role, which is
// a refusal about a record that is not ours to judge.
func TestCmdClaim_AnItemOutsideTheRemitIsNotRefusedOnItsAwaitedRole(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "chore", Title: "1", // the fixture's flow accepts "task" only
		Awaits: flow.Awaits{Role: "reviewer"}}) // …and an empty journal keeps it outside
	be.SetCapabilities("", flow.CapPush)
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "not declared by this flow") {
		t.Errorf("another flow's awaited role was judged against this flow's declarations; got %q", errBuf.String())
	}
	assertHeldHere(t, be, "1")
}

// The marker is what decides, so a read that fails stops the claim rather than
// taking an exclusive lease blind. The alternative is the waste the rule exists
// to prevent, arrived at by a different route.
func TestCmdClaim_AnUnreadableItemIsNotClaimed(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &recordingClaimBackend{Orchestrator: inner}
	app, errBuf := handoffTestApp(t, unloadableBackend{Orchestrator: be})

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "orchestrator unreachable") {
		t.Errorf("the refusal does not say what could not be read; got %q", errBuf.String())
	}
	if be.claims != 0 {
		t.Errorf("Backend.Claim was called %d time(s) with the item unread", be.claims)
	}
	item, err := inner.Load(context.Background(), inner.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !item.Holder.Empty() {
		t.Errorf("the item was claimed though it could not be read; held by %+v", item.Holder)
	}
}

// `resolve <id>` goes through the same one route to a claim: an item awaiting a
// role this run cannot assume is refused, nothing is dispatched, and no lease is
// taken. It is NOT a handoff — a handoff is the runner's part of the resolution
// being complete, and a run that claimed nothing did no part of it.
func TestCmdResolve_AnUnclaimableItemIsRefusedRatherThanHandedOff(t *testing.T) {
	be := fake.New()
	be.AddItem("1", awaitingMaintainer())
	be.SetCapabilities("", flow.CapPush)
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 1 {
		t.Fatalf("exit code = %d, want 1 — the claim was refused; err=%q", code, errBuf.String())
	}
	got := errBuf.String()
	if !strings.Contains(got, "awaits maintainer") || !strings.Contains(got, "this run can assume: contributor") {
		t.Errorf("the refusal does not name the awaited role and what this run can take; got %q", got)
	}
	if strings.Contains(got, "handed off") {
		t.Errorf("a claim that never happened was reported as a handoff; got %q", got)
	}
	if strings.Contains(got, `running "`) {
		t.Errorf("a step was dispatched on an item that was never claimed; got %q", got)
	}
	item, err := be.Load(context.Background(), be.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !item.Holder.Empty() {
		t.Errorf("a refused claim left the item held by %+v", item.Holder)
	}
}

// The refusal is ITEM-SCOPED, and auto-selection acts on that: an eligibility
// mirror that has gone stale — the item's marker moved to another role after the
// listing was computed — costs the run one refusal and the next ref, not the
// whole run.
func TestCmdResolve_AutoSelectSkipsAnItemAwaitingAnotherRole(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", awaitingMaintainer())
	inner.AddItem("2", flow.Item{Type: "task", Title: "2"})
	inner.SetCapabilities("", flow.CapPush)
	// The mirror still offers both, which is the state this test is about: the
	// fake's own listing would have filtered #1 out by role.
	be := &conflictThenOkBackend{
		Orchestrator: inner,
		refs:         []flow.ItemRef{inner.Ref("1"), inner.Ref("2")},
	}
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	got := errBuf.String()
	if !strings.Contains(got, "awaits maintainer") || !strings.Contains(got, "trying next") {
		t.Errorf("the stale offer was not skipped with a reason; got %q", got)
	}
	if len(be.claimAttempts) != 1 || be.claimAttempts[0] != `"2"` {
		t.Errorf("Claim attempts = %v, want the second ref alone (the first is refused before the backend)", be.claimAttempts)
	}
	// And the run went on with it, rather than stopping at the skip. (It ends by
	// handing #2 off at the same boundary, which is what the contributor's step
	// routes to — the claim there is released, so what is asserted is the run.)
	if !strings.Contains(got, "resolve: driving 2 to completion") {
		t.Errorf("the run did not go on to the next ref; got %q", got)
	}
}

// `resolve <id>` takes its claim through the same route, so the same read
// decides: an item that cannot be read is not claimed and the run stops, rather
// than a lease being taken on an item whose pending move nobody could see.
func TestCmdResolve_AnUnreadableItemIsNotClaimed(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &recordingClaimBackend{Orchestrator: inner}
	app, errBuf := handoffTestApp(t, unloadableBackend{Orchestrator: be})

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "orchestrator unreachable") {
		t.Errorf("the stop does not say what could not be read; got %q", errBuf.String())
	}
	if be.claims != 0 {
		t.Errorf("Backend.Claim was called %d time(s) with the item unread", be.claims)
	}
	item, err := inner.Load(context.Background(), inner.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !item.Holder.Empty() {
		t.Errorf("the item was claimed though it could not be read; held by %+v", item.Holder)
	}
}

// `resolve <id>` on an item this arena does NOT hold meets the undeclared
// awaited role at the claim, and reports the item blocked there: the role and
// the declared set, named against the item, with no lease taken. (The same item
// held here reaches the advance, which reports the same thing — handoff_test.go
// TestCmdResolve_AnUndeclaredAwaitedRoleIsBlocked.)
func TestCmdResolve_AnUndeclaredAwaitedRoleIsBlockedBeforeTheClaim(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", awaitingAnUndeclaredRole())
	inner.SetCapabilities("", flow.CapPush)
	be := &recordingClaimBackend{Orchestrator: inner}
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"1"}); code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	got := errBuf.String()
	if !strings.Contains(got, `1 is blocked — role "reviewer" is not declared by this flow`) {
		t.Errorf("the report does not name the item and the unknown role; got %q", got)
	}
	if strings.Contains(got, "this run can assume") {
		t.Errorf("an undeclared role was reported as somebody else's move; got %q", got)
	}
	if be.claims != 0 {
		t.Errorf("Backend.Claim was called %d time(s) on a blocked item", be.claims)
	}
	item, err := inner.Load(context.Background(), inner.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !item.Holder.Empty() {
		t.Errorf("a blocked item was left held by %+v", item.Holder)
	}
}

// A marker nothing can ever match is THIS item's record being wrong, and the
// next item's may well be sound: auto-selection reports the item blocked and
// takes the next ref, exactly as it does for a role somebody else must take.
// Stopping the run instead would let one corrupt marker in a stale offer
// withhold every other item from an unattended runner.
func TestCmdResolve_AutoSelectSkipsAnItemWhoseAwaitedRoleIsUndeclared(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", awaitingAnUndeclaredRole())
	inner.AddItem("2", flow.Item{Type: "task", Title: "2"})
	inner.SetCapabilities("", flow.CapPush)
	// The mirror offers both: the fake's own listing would have filtered #1 out.
	be := &conflictThenOkBackend{
		Orchestrator: inner,
		refs:         []flow.ItemRef{inner.Ref("1"), inner.Ref("2")},
	}
	app, errBuf := handoffTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	got := errBuf.String()
	if !strings.Contains(got, `1 is blocked — role "reviewer" is not declared by this flow`) ||
		!strings.Contains(got, "trying next") {
		t.Errorf("the unmatchable marker was not reported and skipped; got %q", got)
	}
	if len(be.claimAttempts) != 1 || be.claimAttempts[0] != `"2"` {
		t.Errorf("Claim attempts = %v, want the second ref alone (the first is refused before the backend)", be.claimAttempts)
	}
	if !strings.Contains(got, "resolve: driving 2 to completion") {
		t.Errorf("the run did not go on to the next ref; got %q", got)
	}
}

// awaitingAnUndeclaredRole is an item whose recorded awaited role is outside the
// fixture flow's declared set — a typo in the record, or a flow that dropped the
// role: a marker nothing can ever match.
func awaitingAnUndeclaredRole() flow.Item {
	return flow.Item{Type: "task", Title: "1",
		Awaits: flow.Awaits{Role: "reviewer"},
		Journal: []flow.JournalEntry{{
			Step: "plan", Execution: 1, Route: flow.Route{Next: "commit"},
			By: "bob", Role: "contributor", Awaits: flow.Awaits{Role: "reviewer"},
		}}}
}

// reworkedByAlice is an item awaiting the maintainer whose maintainer role is
// already bound: alice reviewed and handed it back, bob reworked it, and it is
// alice's move again — the account of record a refusal names.
func reworkedByAlice() flow.Item {
	return flow.Item{Type: "task", Title: "1",
		Awaits: flow.Awaits{Role: "maintainer"},
		Journal: []flow.JournalEntry{
			{Step: "commit", Execution: 1, Route: flow.Route{Next: "plan"},
				By: "alice", Role: "maintainer", Awaits: flow.Awaits{Role: "contributor"}},
			{Step: "plan", Execution: 2, Route: flow.Route{Next: "commit"},
				By: "bob", Role: "contributor", Awaits: flow.Awaits{Role: "maintainer"}},
		}}
}

// assertHeldHere is the other half of every claim assertion above: the item is
// held, by this arena's own account.
func assertHeldHere(t *testing.T, be *fake.Orchestrator, id string) {
	t.Helper()
	item, err := be.Load(context.Background(), be.Ref(id))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if item.Holder.Empty() {
		t.Fatalf("item %s is not held after a claim that succeeded", id)
	}
	if item.Holder.Account != be.Account() {
		t.Errorf("item %s is held by %q, want this arena's account %q", id, item.Holder.Account, be.Account())
	}
}
