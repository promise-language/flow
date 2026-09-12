package cli

import (
	"context"
	"fmt"
	"slices"

	"github.com/promise-language/flow"
)

// whoseMove is what an item's awaited marker says about THIS run: nobody's
// move, this run's, a role the flow does not declare, or a role somebody else
// must take.
//
// ONE ORDERING, read by the claim refusal below and by the handoff decision in
// cmdResolve. The order is the doc's: an undeclared role is classified BEFORE
// assumability, because "an awaited role that does not exist looks identical to
// a role whose runner has not arrived" (docs/resolution.md § Whose move it is) —
// answered the other way round, a typo would be handed off to nobody and the
// item would sit unofferable with nothing naming why.
type whoseMove int

const (
	// moveNobody — the item awaits nothing, or awaits a signal. Not somebody
	// else's move: the wait is reported through the advance, and no claim is
	// refused on account of it.
	moveNobody whoseMove = iota
	// moveOurs — the awaited role is one this run may assume.
	moveOurs
	// moveUndeclared — the awaited role is outside the flow's declared set.
	// Refused loudly; a person fixes the flow or the record.
	moveUndeclared
	// moveTheirs — a declared role this run may not assume.
	moveTheirs
)

// awaitsNobody reports that the marker names no one to move: an item that has
// not started, or one whose successor is a signal wait — nobody's move is not
// somebody else's (docs/orchestrator.md § `ItemInfo`).
func awaitsNobody(a flow.Awaits) bool { return a.Role == "" || a.Signal != "" }

// whoseMoveIs classifies an awaited marker against the roles this run may
// assume. It is told the roles rather than deriving them, so the caller that
// already holds the derivation (cmdResolve's standing) cannot end up with a
// second reading of the same capabilities.
func whoseMoveIs(f *flow.Flow, a flow.Awaits, roles []flow.RoleName) whoseMove {
	if awaitsNobody(a) {
		return moveNobody
	}
	if !f.DeclaresRole(a.Role) {
		return moveUndeclared
	}
	if slices.Contains(roles, a.Role) {
		return moveOurs
	}
	return moveTheirs
}

// takeClaim is THE route to a claim from the CLI: it applies the role rule
// docs/resolution.md § Claiming states — "A claim is taken for the role the item
// awaits … a runner that cannot assume the role … is refused" — and then calls
// the orchestrator.
//
// The check is the SDK's, not the backend's. An orchestrator "stores [the role]
// and interprets none of it: which roles exist, which capabilities they require,
// and who may assume one are all derivations the SDK owns"
// (docs/orchestrator.md § Identities), so Orchestrator.Claim keeps its signature
// and no orchestrator implementation knows about this.
//
// Re-claiming an item this arena already holds passes STRAIGHT THROUGH: a claim
// is idempotent for its holder (docs/resolution.md § Claiming), and an arena
// that stopped at a boundary — a park, an interruption — must be able to pick
// its own held item up and hand it off properly rather than be locked out of the
// item it is holding. A lookup that FAILS is not read as "we hold it": the check
// runs, which is the safe direction — the worst it costs a genuine holder is a
// refusal naming the role it is stopped at.
//
// A failed Load stops the claim rather than claiming blind: the marker is what
// decides, and taking an exclusive lease while unable to read it is the waste
// the rule exists to prevent.
func (app *App) takeClaim(ctx context.Context, ref flow.ItemRef, overrides []flow.ClaimOverride) (flow.Claim, error) {
	if held, err := app.Orchestrator.LookupActiveClaim(ctx); err == nil && held != nil && sameItemRef(held.ItemRef, ref) {
		return app.Orchestrator.Claim(ctx, ref, overrides)
	}
	item, err := app.Orchestrator.Load(ctx, ref)
	if err != nil {
		return flow.Claim{}, fmt.Errorf("load item: %w", err)
	}
	if err := app.refuseUnassumableRole(ctx, ref, item); err != nil {
		return flow.Claim{}, err
	}
	return app.Orchestrator.Claim(ctx, ref, overrides)
}

// refuseUnassumableRole answers whether the item's awaited role bars this run
// from claiming it. Nil means the claim may proceed.
//
// Nothing is refused where there is no move to refuse, and each such case is
// settled BEFORE the capability question, which costs a round trip to the
// backend:
//
//   - no item to read at all — Load's contract does not forbid it,
//   - a marker naming nobody: an item that has not started, or a signal wait,
//   - a FINALIZED item's marker, which names the last role that moved and is
//     stale — the work is over and nobody is awaited,
//   - an item outside this binary's remit, whose marker is another flow's
//     vocabulary: no flow of this runner's ever derived a step for it.
func (app *App) refuseUnassumableRole(ctx context.Context, ref flow.ItemRef, item *flow.Item) error {
	if item == nil || awaitsNobody(item.Awaits) || item.Finalized || !inRemit(app, item) {
		return nil
	}
	roles, known := app.assumableRoles(ctx)
	switch whoseMoveIs(app.Flow, item.Awaits, roles) {
	case moveUndeclared:
		return flow.ErrUnknownRole{Role: item.Awaits.Role, Declared: app.Flow.RoleNames()}
	case moveTheirs:
		// ErrClaimRefused, rather than a type of its own: docs/cli.md § Claiming
		// puts this refusal in one list with the backend's own and asks the same
		// three things of each, and `resolve`'s auto-selection already branches
		// on ItemScoped. ITEM-SCOPED because it is: another item may well await a
		// role this run can take.
		//
		// No Override. "A role is assumable only where the runner both declares
		// it and its account backs it" (docs/resolution.md § Accounts,
		// capabilities and roles) — there is nothing a flag could unlock, and the
		// backend's own permissions would refuse the work anyway.
		return flow.ErrClaimRefused{
			Code:       "awaits-other-role",
			ItemScoped: true,
			Reason: fmt.Sprintf("%s awaits %s; this run can assume: %s",
				ref.Display, awaitedPhrase(item.Awaits), rolesPhrase(roles, known)),
		}
	}
	return nil
}

// sameItemRef reports whether two refs address one item.
//
// Compared by the orchestrator that minted them and the name IT gives the item:
// Ref is orchestrator-internal addressing carried as raw JSON, so comparing it
// would compare an encoding rather than an identity.
func sameItemRef(a, b flow.ItemRef) bool {
	return a.OrchestratorName == b.OrchestratorName && a.Display == b.Display
}
