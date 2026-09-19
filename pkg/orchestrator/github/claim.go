package github

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
)

// Claim acquires an exclusive lease on the given issue. Algorithm:
//
//  1. POST a claim label flow:claim:<token> (see newClaimToken).
//  2. GET the issue's labels; DELETE any flow:claim:* token abandoned by an
//     attempt that is no longer running, then, if multiple remain, the
//     lexicographically smallest token wins.
//  3. Losers DELETE their own claim label and return an error.
//  4. Winner: POST self as assignee, POST flow:owner:<login> and
//     flow:arena:<fingerprint>, DELETE flow:claim:<token>.
//  5. POST or supersede the state comment.
//
// NEITHER THE ARENA NOR THE ACCOUNT IS A PARAMETER. Both are ambient in the
// sense that neither is passed in: the arena is the checkout this binary
// belongs to — fixed when the orchestrator is constructed, and never read off
// the process working directory — and the account is whoever its credentials
// act as, resolved through the one derivation `list` also compares against. A
// caller-supplied account could only agree or be wrong — and it was wrong,
// whenever $USER differed from the gh login, which wrote a flow:owner label for
// a user GitHub may not recognise.
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
	// LookupActiveClaim is the single source for "what does this arena hold?"
	// — requireOwnClaim (artifact.go) is its other consumer, and turns the same
	// state into its two typed refusals instead of a lease. Read ONCE here,
	// OUTSIDE every override, because three decisions need it: the arena-side
	// refusal just below, the already-held refusal for a record that names no
	// arena, and the holder's idempotent return.
	active, err := b.LookupActiveClaim(ctx)
	if err != nil {
		return flow.Claim{}, fmt.Errorf("github.Claim: read active claim: %w", err)
	}
	// Whether THIS arena is the holder, from its own lease file. Declared out
	// here because the already-held comparison is made twice — once on the
	// preflight read below, once on the Phase 2 re-read — and both need it.
	weHold := false
	if active != nil {
		activeNum, err := b.issueNumber(active.ItemRef)
		if err != nil {
			return flow.Claim{}, err
		}
		weHold = activeNum == issueNum
		// The arena side of the one-to-one binding (docs/orchestrator.md §
		// Required surface → Claiming): an arena holding one item is not free,
		// and claiming a second would overwrite the lease file while the first
		// item's uncommitted work, branch and build outputs stay in this tree —
		// state that exists nowhere else and cannot be recovered by re-reading
		// anything. Refused before any override is consulted, because no
		// override reaches it: --force means "take this from another party",
		// and there is nobody to take it from here — overwriting our own active
		// claim is how the first item gets orphaned. Not item-scoped: no other
		// item would fare better in an occupied arena. Re-claiming the item we
		// hold is the idempotent path further down, not this.
		if !weHold {
			return flow.Claim{}, flow.ErrClaimRefused{
				Code: "arena-occupied", ItemScoped: false,
				Reason: fmt.Sprintf("this arena already holds %s — finish it, or run: release",
					active.ItemRef.Display),
				Check: "active-claim",
			}
		}
	}
	if !slices.Contains(overrides, flow.OverrideAlreadyHeld) {
		// Refuse when another person holds the issue via an assignee. The
		// caller must pass OverrideAlreadyHeld to take over deliberately.
		for _, u := range issue.Assignees {
			if login := flow.AccountId(u.GetLogin()); login != "" && login != owner {
				return flow.Claim{}, flow.ErrClaimRefused{
					Code: "already-held", ItemScoped: true,
					Reason:   fmt.Sprintf("issue #%d is assigned to %s (use --force to take over)", issueNum, login),
					Override: "force",
				}
			}
		}
		// Refuse when another ARENA holds the issue — the claim record on the
		// item, read by the one predicate `list` also uses, so the two cannot
		// disagree. It compares arenas and not accounts because a lease binds
		// item ↔ arena: three worktrees under one login all answer "that is my
		// own account" to an account comparison, and all three then claim.
		if reason, held := b.heldByAnotherArena(names, owner, weHold); held {
			return flow.Claim{}, flow.ErrClaimRefused{
				Code: "already-held", ItemScoped: true,
				Reason:   fmt.Sprintf("issue #%d %s", issueNum, reason),
				Override: "force",
			}
		}
		// Idempotent for the holder (docs/resolution.md § Claiming, docs/cli.md
		// § Claiming): re-claiming an item this arena already holds succeeds and
		// CHANGES NOTHING — so the lease is returned as it stands, with no token
		// minted, no label written and no state comment touched. Returning here is
		// also what keeps the worktree preconditions below on the FRESH-claim path
		// they belong to: a holder mid-work sits on the item's own branch by
		// design, and applying "HEAD must be on the base branch" to a re-claim
		// makes the documented claim-then-resolve sequence impossible.
		//
		// Position matters, and only the READ above moved — the order of the
		// decisions is unchanged. The return still sits after the
		// disabled/other-binary preflights and after the already-held refusal,
		// so an item another party took over refuses typed rather than handing
		// back our now-stale lease file; and it stays inside the
		// !OverrideAlreadyHeld block, so `claim --force` still re-asserts
		// ownership through Phases 1-3 rather than silently no-op'ing on that
		// stale file. "Another party" now includes another arena under this
		// same account, which the refusal above could not see before and which the
		// stale file would otherwise have been believed about.
		if weHold {
			return *active, nil
		}
	}

	// Worktree preconditions — arena-scoped, not item-scoped.
	// Run before Phase 1: nothing is written to the item on refusal.

	// THE TREE COMES FIRST, in the order Release already uses and for the reason
	// refuseOffBase records: the recovery that refusal names is `git checkout
	// <base>`, which on a dirty tree does not fail but carries the modifications
	// onto the base branch. Answered the other way round, the first thing an
	// operator is told to do is the thing that misplaces their work (#206). It
	// also spares the fetch on a tree that was going to be refused anyway.
	if !slices.Contains(overrides, flow.OverrideDirtyTree) {
		// 1. Tree must be clean, including untracked files.
		if refused, err := b.refuseDirtyTree(ctx, dirtyTreeRecoveryAtClaim, "force"); err != nil {
			return flow.Claim{}, err
		} else if refused != nil {
			return flow.Claim{}, *refused
		}
	}

	if !slices.Contains(overrides, flow.OverrideStaleBase) {
		// 2. Fetch — every check below is worthless against stale refs.
		if err := b.git.Fetch(ctx, "origin"); err != nil {
			return flow.Claim{}, flow.ErrClaimRefused{
				Code: "fetch-failed", ItemScoped: false,
				Reason:   "git fetch origin failed",
				Detail:   err.Error(),
				Check:    "fetch",
				Override: "force",
			}
		}

		// 3. HEAD must be on the base branch.
		base, err := b.DefaultBranch(ctx)
		if err != nil {
			return flow.Claim{}, fmt.Errorf("resolve default branch: %w", err)
		}
		if refused, err := b.refuseOffBase(ctx, base, "force"); err != nil {
			return flow.Claim{}, err
		} else if refused != nil {
			return flow.Claim{}, *refused
		}

		// 4. Local base must be at origin's tip.
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

	// Phase 1: post a random claim label.
	token := newClaimToken()
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
	// The already-held comparison AGAIN, on the re-read — and it is the one
	// that decides, because the preflight's is stale by the time the lease is
	// taken. Between the two reads sit the worktree preconditions, and one of
	// those is `git fetch origin`: the window is seconds wide on a real
	// repository, not the two API calls the token race is sized for.
	//
	// An arena that finished its own claim inside that window leaves a record
	// this settle cannot see — the race settles among flow:claim:* tokens only,
	// and the holder removed its token as the last act of Phase 3. So without
	// this the second arena wins its race UNCONTESTED, strips the holder's
	// flow:arena: label as stale in Phase 3, and takes over an item another
	// arena is running with no override asked for and nothing refused. That is
	// #210's failure reached through the fetch instead of through selection,
	// and docs/orchestrator.md § What an orchestrator may refuse gives it no
	// room: "a claim held by another — unless the operator passes the
	// already-held override".
	//
	// It costs no request: issue2 is the read the race already makes.
	if !slices.Contains(overrides, flow.OverrideAlreadyHeld) {
		if reason, held := b.heldByAnotherArena(labelNamesOf(issue2.Labels), owner, weHold); held {
			_ = b.out.RemoveLabel(ctx, issueNum, claimLabel)
			return flow.Claim{}, flow.ErrClaimRefused{
				Code: "already-held", ItemScoped: true,
				Reason:   fmt.Sprintf("issue #%d %s", issueNum, reason),
				Override: "force",
			}
		}
	}

	contenders := b.claimContenders(labelNamesOf(issue2.Labels))
	// Collect abandoned tokens before settling. A claim attempt that died
	// between posting its label and removing it again leaves a token no
	// process holds and nothing expires; because the smallest token wins,
	// one such token blocks the item for every later claimer, permanently.
	// A token older than any attempt could be — or one carrying no creation
	// time — is abandoned, so it is removed and takes no part in the race.
	//
	// We never test our own token: a clock adjustment mid-attempt must not
	// make a claimer delete its own label. Removal is best-effort, like
	// every other cleanup here — the decision does not depend on it, so two
	// claimers seeing the same abandoned token both disregard it and settle
	// on the same winner.
	live := make([]string, 0, len(contenders))
	for _, c := range contenders {
		if c != token && claimTokenAbandoned(c) {
			_ = b.out.RemoveLabel(ctx, issueNum, b.labels.ClaimToken(c))
			continue
		}
		live = append(live, c)
	}
	if len(live) == 0 {
		// Our own token is not in the read. Two events produce that and this
		// read cannot tell them apart: another actor stripped the label, or the
		// read is too stale to show a POST that landed. Remove it either way —
		// a stripped label is already gone and the removal 404s harmlessly,
		// while a label that is still there and unremoved is an orphaned token
		// the next claimer could only collect after its TTL.
		_ = b.out.RemoveLabel(ctx, issueNum, claimLabel)
		return flow.Claim{}, flow.ErrClaimRefused{
			Code: "claim-race", ItemScoped: true,
			Reason: "claim race: our claim token was absent on re-read",
		}
	}
	sort.Strings(live)
	if live[0] != token {
		// Lost the race — clean up our label.
		_ = b.out.RemoveLabel(ctx, issueNum, claimLabel)
		return flow.Claim{}, flow.ErrClaimRefused{
			Code: "claim-race", ItemScoped: true,
			Reason: claimRaceReason(live[0]),
		}
	}

	// Phase 3: assert ownership.
	if err := b.out.AddAssignees(ctx, issueNum, []string{string(owner)}); err != nil {
		_ = b.out.RemoveLabel(ctx, issueNum, claimLabel)
		return flow.Claim{}, fmt.Errorf("add assignee: %w", err)
	}
	// Clear the previous holder's markers first — any owner label naming
	// another account, and any arena label naming another arena. The arena half
	// is what a take-over under ONE account has to displace: the owner label is
	// then byte-identical between the two arenas, so nothing else on the item
	// changes hands.
	fingerprint := b.arenaFingerprint()
	for _, name := range labelNamesOf(issue2.Labels) {
		stale := false
		if login, ok := b.labels.OwnerFromLabel(name); ok && login != owner {
			stale = true
		}
		if fp, ok := b.labels.ArenaFromLabel(name); ok && fp != fingerprint {
			stale = true
		}
		if !stale {
			continue
		}
		if err := b.out.RemoveLabel(ctx, issueNum, name); err != nil {
			_ = b.out.RemoveLabel(ctx, issueNum, claimLabel)
			return flow.Claim{}, fmt.Errorf("remove stale holder label %s: %w", name, err)
		}
	}
	// The owner and arena labels go in ONE request: they are two halves of one
	// claim record, and a window with one present and not the other is a window
	// in which every reader of the record decides differently.
	if err := b.out.AddLabels(ctx, issueNum, []string{
		b.labels.Owner(string(owner)),
		b.labels.Arena(fingerprint),
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
	stateBody, stateID, _, err := b.fetchStateComment(ctx, issueNum, 0)
	if err != nil {
		return flow.Claim{}, fmt.Errorf("locate state comment: %w", err)
	}
	if stateBody != "" {
		_, existingOwner, _, perr := extractStateDoc(stateBody)
		if perr == nil && existingOwner != "" && flow.AccountId(existingOwner) != owner {
			// Different author — post a fresh state comment authored by us,
			// empty: nothing seeds an item, so the next entry, ledger row,
			// park or question is what brings its record back into being.
			newDoc := stateDoc{Flow: b.cfg.BinaryName, Schema: stateSchemaVersion}
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
	// The github orchestrator's lease store is the worktree-local
	// .flow/active.json file — one file per checkout, which is what scopes
	// "what does THIS arena hold?". It cannot scope the other direction: a file
	// only the holding checkout can see says nothing to the arena next door,
	// which is why the item carries flow:arena:<fingerprint> too. Write it here
	// so LookupActiveClaim and the CLI commands that consume the active claim
	// can find it.
	if err := clistate.Save(c); err != nil {
		// Best-effort rollback of the github-side ownership we just took. The
		// owner half comes off first here — the reverse of Release, and for the
		// same reason: this claim FAILED, so a rollback that stops halfway must
		// leave the item reading as free, and an arena label with no owner
		// label beside it is not a claim record (holderFromLabels).
		_ = b.out.RemoveLabel(ctx, issueNum, b.labels.Owner(string(owner)))
		_ = b.out.RemoveLabel(ctx, issueNum, b.labels.Arena(fingerprint))
		_ = b.out.RemoveAssignees(ctx, issueNum, []string{string(owner)})
		return flow.Claim{}, fmt.Errorf("github.Claim: save active claim: %w", err)
	}
	return c, nil
}

// arena is the (HostId, ArenaId) pair this checkout is. Both halves are
// derived rather than configured for this orchestrator — the arena IS the local
// checkout — but the pair is still recorded on the claim, because a handle to a
// lease that could not name what the lease binds leaves every holder
// unidentifiable wherever one account runs more than one arena.
//
// It is also published, as fingerprintArena's digest: the local file this pair
// is written to answers only the arena that wrote it, and the exclusion that
// keeps two arenas off one item has to be decidable by the OTHER one.
//
// The HostId is derived from the machine and normalized; the ArenaId is the
// absolute worktree path, which is stable across restarts and unique within the
// host — exactly what the contract asks of it.
//
// cfg.WorktreeDir is used AS IT STANDS: New has already made it absolute, by
// deriving the checkout the binary lives in or refusing a relative value. A
// filepath.Abs here would be a second place a location is decided, and the only
// thing it could add is the process working directory — which is what it did
// add, turning "." into the operator's cwd and calling that the arena (#286).
func (b *Orchestrator) arena() flow.Arena {
	return flow.ArenaAt(b.cfg.WorktreeDir)
}

// fingerprintArena reduces an arena to the opaque, comparable value that goes
// on the item as flow:arena:<fingerprint>.
//
// A DIGEST rather than the pair, and that is a requirement, not a compaction.
// docs/disclosure.md closes the categories a flow must not publish, and two of
// the five are precisely what an arena is made of — "Local filesystem paths"
// (the ArenaId is the absolute worktree path) and "Host and account
// identifiers — machine names, arena names, internal hostnames". The same
// document lists labels as a guarded surface. Equality is the only operation
// the three exclusion sites need, and a digest supports exactly that: the same
// host and the same absolute path yield the same value across restarts, which
// is the stability docs/orchestrator.md asks of an ArenaId, and no reader of
// the issue learns either half.
//
// Sixteen hex digits: flow:arena: (11) plus 16 is 27 characters, inside
// GitHub's 50-character label cap, and 64 bits is far more than a fleet's worth
// of arenas needs to stay distinct.
func fingerprintArena(a flow.Arena) string {
	sum := sha256.Sum256([]byte(string(a.Host) + "\x00" + string(a.Id)))
	return hex.EncodeToString(sum[:])[:16]
}

// arenaFingerprint is fingerprintArena over this checkout's own arena.
func (b *Orchestrator) arenaFingerprint() string { return fingerprintArena(b.arena()) }

// refuseOffBase is the "HEAD is on the base branch" precondition, shared by
// Claim and Release: the refusal is returned when HEAD is elsewhere, nil when
// the condition holds, and a plain error when git could not answer. override
// names the flag that bypasses it, or "" when nothing does — Release's case.
//
// One body for both directions of the lease, because the condition is the same
// one read at two moments: a fresh claim must start from the trunk, and a
// release must leave the arena on it, or the next claim inherits the released
// item's branch.
//
// BOTH CALLERS CHECK THE TREE FIRST, and that is a requirement of this refusal
// rather than a habit of theirs. The recovery it names is `git checkout <base>`,
// and on a dirty tree git does not refuse that: it carries the modifications
// across, leaving the base branch holding work that belongs to something else.
// So the instruction is correct only on a clean tree, and refuseDirtyTree is
// what has to have answered before this one is asked (#206). docs/cli.md
// § Claiming carries the rule.
func (b *Orchestrator) refuseOffBase(ctx context.Context, base flow.BranchName, override string) (*flow.ErrClaimRefused, error) {
	current, err := b.git.CurrentBranch(ctx)
	if err != nil {
		return nil, fmt.Errorf("current branch: %w", err)
	}
	if flow.BranchName(current) == base {
		return nil, nil
	}
	return &flow.ErrClaimRefused{
		Code: "not-on-base", ItemScoped: false,
		Reason: fmt.Sprintf("HEAD is on %q, want %q — run: git checkout %s",
			current, base, base),
		Check:    "base-branch",
		Override: override,
	}, nil
}

// The act each dirty-tree refusal names. Two, because the tree means different
// things at the two ends of the lease and the same sentence cannot be right at
// both.
const (
	// At a CLAIM the tree holds whatever was in the arena before this item, and
	// setting it aside is safe: it is going somewhere the operator can get it
	// back from, and the item that is about to be claimed has no relationship to
	// it. Spelled --include-untracked because untracked files count towards this
	// refusal, and a stash that left them behind would not clear it.
	dirtyTreeRecoveryAtClaim = "commit them, or set them aside with: git stash push --include-untracked"

	// At a RELEASE — and at the release Finalize ends with — the tree holds the
	// ITEM's own work, and the two normative documents name the way past
	// deliberately and identically: "commit the work to the item's branch or
	// discard it, return to the base, release. That is the moment somebody
	// decides what happens to the work, instead of it becoming nobody's"
	// (docs/orchestrator.md § Required surface → `Release`, docs/cli.md
	// § Releasing). A stash is exactly what those two rule out — work no item
	// holds and no branch carries, which is the orphaned state this refusal
	// exists to prevent — and naming one here would send an operator releasing a
	// parked item to empty its branch into refs/stash and hand the item on with
	// nothing on the branch left "for whoever comes back to it".
	dirtyTreeRecoveryAtRelease = "commit them to the item's branch, or discard them"
)

// refuseDirtyTree is the "tree is clean, untracked files included" precondition,
// shared by Claim, Release and Finalize. Same shape as refuseOffBase: the
// refusal when the tree is dirty, nil when clean, an error when git could not
// say. Detail carries `git status --porcelain` verbatim, so the operator sees
// what is in the way rather than being told something is.
//
// Untracked files COUNT. At a release there is no item left to attribute them
// to, and a file nothing tracks is exactly the kind of leftover the next claim
// would otherwise start on top of. The project must therefore ignore .flow/, or
// the lease file itself would answer here — StageAll already refuses a
// project that does not.
//
// THE REASON NAMES A WAY OUT, as the other two worktree refusals name theirs —
// base-stale names `git pull --ff-only`, not-on-base names `git checkout
// <base>`. This is the refusal an operator meets first at all three call sites,
// and "the tree is dirty" without an act leaves them to invent one; the act they
// reach for is the checkout, which is the one that misplaces the work (#206).
//
// The act is the CALLER'S, because the right one differs by end of the lease,
// and one wording for both would be wrong at one of them — see the two
// dirtyTreeRecovery constants.
func (b *Orchestrator) refuseDirtyTree(ctx context.Context, recovery, override string) (*flow.ErrClaimRefused, error) {
	porcelain, err := b.git.StatusPorcelain(ctx)
	if err != nil {
		return nil, fmt.Errorf("check dirty tree: %w", err)
	}
	if porcelain == "" {
		return nil, nil
	}
	return &flow.ErrClaimRefused{
		Code: "dirty-tree", ItemScoped: false,
		Reason:   "worktree has uncommitted or untracked changes — " + recovery,
		Detail:   porcelain,
		Check:    "clean-tree",
		Override: override,
	}, nil
}

// Release strips the assignee, removes the flow:owner:<account> and
// flow:arena:<fingerprint> labels, clears the worktree-local active-claim file,
// and leaves the state comment intact. It deletes no branch and no commit.
//
// It is the EXCEPTION to the binding's lifetime, not a step in it. The lease
// binds item ↔ arena from the claim until the item is finalized
// (docs/resolution.md § Claiming), and a release breaks that binding early —
// so it must leave the arena genuinely free, and it REFUSES while the arena is
// in no state to be handed on:
//
//   - a dirty tree (untracked files included), because the changes in it would
//     belong to nobody afterwards — the item is unheld and selectable by any
//     arena, and nothing points at the work;
//   - HEAD off the base branch, because the next claim would inherit the
//     released item's branch.
//
// The same two conditions Claim enforces, with the same typed codes. The
// ORDINARY way past them is git, deliberately — commit the work to the item's
// branch or discard it, check out the base, release — because that is the
// moment somebody decides what happens to the work, instead of it becoming
// nobody's. OverrideDirtyTree and OverrideStaleBase bypass them anyway, and
// what that costs is exactly what the refusals exist to prevent: work no item
// holds and no branch carries, in a tree an unforced Claim then refuses. It is
// the operator's emergency — an arena being decommissioned, a lease record
// naming nothing — and never a step's (docs/cli.md § Releasing).
//
// The dirty check runs first, for the reason refuseOffBase records — the same
// order Claim uses, because it is the same pair of conditions read at the other
// end of the lease.
//
// A ZERO ref names no item: the arena's own record alone comes off and NOTHING
// on the backend is touched or even read. That is the exit for an arena whose
// lease record cannot be parsed — a record nothing can read names no item to
// release, so there is nothing to address (#212). The worktree preconditions
// still run: the tree may hold work belonging to whatever the unreadable record
// named, and a release that cannot say which item it was leaves even less to
// attribute it to.
//
// A ref this arena does NOT hold needs OverrideAlreadyHeld. The holding arena's
// tree, drafts and session are not readable from here at all, so the two
// preconditions cannot be evaluated against it — refusing is the honest answer
// and the override is the operator saying they know what is there.
//
// Addressed by ref: the account is ambient, so it is read rather than taken off
// a claim value the caller might be holding after the lease was revoked.
func (b *Orchestrator) Release(ctx context.Context, ref flow.ItemRef, overrides []flow.ClaimOverride) error {
	// Giving the lease up ends any landing round with it, and the mainline's
	// exclusion is never held across a stop (landing.go). Before the refusals
	// below, because every ambiguity about a lock resolves toward releasing it:
	// an arena on its way out holding the mainline is the failure this must not
	// produce, and a refused release is not a round.
	b.releaseLanding()
	if !slices.Contains(overrides, flow.OverrideDirtyTree) {
		if refused, err := b.refuseDirtyTree(ctx, dirtyTreeRecoveryAtRelease, "force"); err != nil {
			return fmt.Errorf("github.Release: %w", err)
		} else if refused != nil {
			return *refused
		}
	}
	if !slices.Contains(overrides, flow.OverrideStaleBase) {
		// No fetch and no base-stale check: a stale local trunk strands nothing.
		base, err := b.DefaultBranch(ctx)
		if err != nil {
			return fmt.Errorf("github.Release: resolve default branch: %w", err)
		}
		if refused, err := b.refuseOffBase(ctx, base, "force"); err != nil {
			return fmt.Errorf("github.Release: %w", err)
		} else if refused != nil {
			return *refused
		}
	}
	// A ref that names nothing: the local record is the whole of what comes
	// off, and no request is made. Placed after the preconditions and before
	// every read, because there is no issue number to read anything WITH — this
	// is the arena whose lease record could not be parsed, and the only thing
	// known about it is that it is this arena's.
	if len(ref.Ref) == 0 {
		if err := clistate.Clear(); err != nil {
			return fmt.Errorf("github.Release: clear active claim file: %w", err)
		}
		return nil
	}
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return err
	}
	owner, err := b.resolveAccount(ctx)
	if err != nil {
		return err
	}
	// A DISPLACED arena — one whose item another arena under this same account
	// took over with --force — is refused every other claim by the check at the
	// top of Claim, so release is its only exit. But its record on the item is
	// already gone: the take-over replaced the arena half, and the owner half is
	// byte-identical between the two arenas. Removing the owner label here would
	// take the TAKER's record apart, leaving a bare arena label, which
	// docs/github-schema.md § Labels defines as not a claim — the item would read
	// free while the taker runs it. So when the item names this account with an
	// arena that is not ours, only the local lease file is cleared. Every other
	// case — our own record, no record, a record naming no arena — proceeds.
	//
	// Displacement is BOTH halves: this arena's lease says it holds the item,
	// and the item's record names a holder that is not this arena. The second
	// half alone also describes the arena deliberately releasing somebody else's
	// record by id — identical labels, opposite intent — and answering that by
	// clearing the caller's own lease and reporting success is how #212's second
	// shape would have been closed wrongly. Every caller that existed before
	// addressed Release with its own active claim, so the first half held in all
	// of them and requiring it narrows nothing that was reachable.
	//
	// "Not this arena" is a different account OR a different arena under this
	// one. The take-over that displaces can be either, and an arena displaced
	// across accounts is in exactly the same position as one displaced across
	// arenas: release is its only exit, and the record is not its to dismantle.
	issue, err := b.out.GetIssue(ctx, issueNum)
	if err != nil {
		return fmt.Errorf("github.Release: get issue %d: %w", issueNum, err)
	}
	// The arena-side half of both decisions below, read once: "does this arena
	// hold this item?", answered by the helper the listing already asks.
	weHold := b.holdsItem(ctx, issueNum)
	holder, fingerprint := b.holderFromLabels(labelNamesOf(issue.Labels))
	// An owner label with no arena label beside it is the record written before
	// the arena half existed (docs/github-schema.md § Labels). It names no arena
	// to differ from ours, so it is NOT a displacement — it is this arena's own
	// record in the older spelling, and its owner half still has to come off.
	recordIsForeign := holder.Account != "" &&
		(holder.Account != owner || (fingerprint != "" && fingerprint != b.arenaFingerprint()))
	if weHold && recordIsForeign {
		if err := clistate.Clear(); err != nil {
			return fmt.Errorf("github.Release: clear active claim file: %w", err)
		}
		return nil
	}
	// A record belonging to somebody else needs the override, and "somebody
	// else" is the SAME predicate Claim refuses on — one comparison read at the
	// two ends of the lease, rather than a second one free to disagree with it.
	// It already handles the half-record that names no arena, which is why it
	// takes the arena-side answer as an argument.
	//
	// The reason it is refused at all: that arena's tree, drafts and session are
	// not readable from here, so the two preconditions above cannot be evaluated
	// against the arena the release would actually free — refusing is the honest
	// answer, and --force is the operator saying they know what is there
	// (docs/cli.md § Releasing).
	if !slices.Contains(overrides, flow.OverrideAlreadyHeld) {
		if reason, held := b.heldByAnotherArena(labelNamesOf(issue.Labels), owner, weHold); held {
			return flow.ErrClaimRefused{
				Code: "already-held", ItemScoped: true,
				Reason: fmt.Sprintf("issue #%d %s — its arena's tree cannot be read from here, "+
					"so the release preconditions cannot be checked", issueNum, reason),
				Check:    "active-claim",
				Override: "force",
			}
		}
	}
	// The arena half comes off FIRST, and the order is the correctness of the
	// pair — the two removals are separate requests, so one of them can be the
	// last thing that happens. Release is GIVING THE LEASE UP, so the partial
	// state to leave is the one that still reads as held: flow:owner:<login>
	// with no arena label is held by every arena except the one whose own lease
	// file says otherwise, and Release returns before clearing that file, so
	// this arena retries and every other one stays off the item. The opposite
	// order leaves flow:arena:<ours> standing alone, which every reader takes
	// for unclaimed while this arena still holds a lease on it — two arenas on
	// one item, the thing the pair exists to prevent.
	//
	// The rollback in Claim runs the same two removals the other way round for
	// the same reason read from the other end: there the claim FAILED, so the
	// state to leave is the one that reads free.
	//
	// The three values come from the record ON THE ITEM rather than from this
	// arena's own identity, because a forced release addresses a record that may
	// be somebody else's. For this arena's own claim they are the same bytes, so
	// the ordinary release is unchanged; for a foreign one they are the
	// difference between taking the record apart and removing labels that were
	// never there while leaving the ones that were.
	releasedArena, releasedOwner := fingerprint, holder.Account
	if releasedArena == "" {
		releasedArena = b.arenaFingerprint()
	}
	if releasedOwner == "" {
		releasedOwner = owner
	}
	if err := b.out.RemoveLabel(ctx, issueNum, b.labels.Arena(releasedArena)); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove arena label: %w", err)
	}
	// Left behind, the owner label makes the item read as held forever, and
	// every other arena needs --force to touch it.
	if err := b.out.RemoveLabel(ctx, issueNum, b.labels.Owner(string(releasedOwner))); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove owner label: %w", err)
	}
	if err := b.out.RemoveAssignees(ctx, issueNum, []string{string(releasedOwner)}); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove assignee: %w", err)
	}
	// NOT when this arena holds a DIFFERENT item. clistate.Clear is arena-wide —
	// it takes the lease file, the draft tree and the session with it — so
	// running it while releasing somebody else's record would wipe the state of
	// whatever THIS arena is part-way through, which is exactly the "pay twice"
	// the release refusals exist to prevent (docs/cli.md § Releasing).
	//
	// Stated as "unless a different item is held" rather than "only when this
	// one is": holding nothing, and holding a record nothing can read, are both
	// cases where clearing can destroy no resolution's state, and where the
	// clearing is the whole point — Finalize ends here with the lease already
	// written off, and an arena whose lease is unreadable is #212's own exit.
	if active, err := b.LookupActiveClaim(ctx); err == nil && active != nil {
		if activeNum, nerr := b.issueNumber(active.ItemRef); nerr == nil && activeNum != issueNum {
			return nil
		}
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
// The clean-tree check comes BEFORE the state-comment write, and it is the
// same check Release makes. Release refuses a dirty tree, untracked files
// included, so a tree that would fail it must be found out before anything is
// recorded: checked only afterwards, an untracked file left by a step — verify
// may mutate the worktree, and reverting merge prep is a hard reset that leaves
// such files — would be recorded as finalized and then refused at the release,
// a finalized-but-held item in the happy path. The state-comment write then
// comes before the worktree return so a checkout failure leaves the claim
// recoverable, and the return precedes the release for the same reason.
func (b *Orchestrator) Finalize(ctx context.Context, ref flow.ItemRef, d flow.Disposition) error {
	// The flow is finished with the item, so any landing round is over too.
	// Same rule and same position as Release above.
	b.releaseLanding()
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

	// No override named, unlike Claim's and Release's calls: Finalize takes no
	// overrides and ends in Release(ctx, ref, nil), so there is no flag that
	// bypasses this one. Naming "force" here would point at a flag that does
	// not reach it.
	if refused, err := b.refuseDirtyTree(ctx, dirtyTreeRecoveryAtRelease, ""); err != nil {
		return fmt.Errorf("github.Finalize: %w", err)
	} else if refused != nil {
		return fmt.Errorf("github.Finalize: %w", *refused)
	}

	// Mark the item finalized in the state comment so Load returns
	// Item.Finalized=true — the read is required with the write, because a
	// write nothing can observe is not a record.
	body, stateID, _, err := b.fetchStateComment(ctx, issueNum, b.cachedStateCommentID(issueNum))
	if err != nil {
		return fmt.Errorf("github.Finalize: fetch state comment: %w", err)
	}
	if body != "" {
		doc, _, found, perr := extractStateDoc(body)
		if perr != nil {
			return fmt.Errorf("github.Finalize: parse state comment: %w", perr)
		}
		if found && doc != nil {
			// Setting the flag CHANGES WHAT THE ITEM AWAITS: a finalized item
			// awaits nobody, whatever its last entry elected (awaitsFromDoc). So
			// the marker derived from that answer moves here too, read off the
			// document before the flag is set.
			//
			// Ordinarily there is nothing to move — the finalizing entry's Awaits
			// is empty, so AppendEntry already removed the marker and this costs no
			// request. What it catches is finalization reached with a route still
			// mid-flight (an item closed on GitHub under a flow whose required
			// signals are unset), and a best-effort removal that did not land.
			clearedAwaits := b.awaitsLabel(awaitsFromDoc(doc))
			doc.Finalized = true
			// The disposition the finalizing election carried, recorded beside
			// the flag: the read is required with the write, so Load reports
			// both and neither is inferred from the other.
			doc.Disposition = string(d)
			if _, err := b.updateStateComment(ctx, issueNum, stateID, *doc, owner); err != nil {
				return fmt.Errorf("github.Finalize: update state comment: %w", err)
			}
			b.moveAwaitsLabel(ctx, issueNum, clearedAwaits, "")
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

	return b.Release(ctx, ref, nil)
}

// LookupActiveClaim returns the claim THIS ARENA holds right now, or nil.
//
// It takes no key. The github orchestrator's lease store is the worktree-local
// .flow/active.json file, and one file per checkout is what scopes THIS
// question — so the file already answers "what is this arena working on?"
// exactly. It answers no question about another arena, which is what
// flow:arena:<fingerprint> on the item is for. The old
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
// Both halves come off the ITEM, through holderFromLabels — the same read
// `list` and the claim preflight make, so the three cannot disagree about who
// holds what. The lease file is not consulted: it is this arena's own record of
// what it believes it holds, and a belief the server has since overruled is
// exactly what it goes on saying. An arena displaced by a take-over would
// otherwise report ITSELF as the holder of an item another arena is running.
//
// The ARENA half is still reported only when this checkout is the holder —
// flow:arena:<fingerprint> records WHICH arena holds the item, but records it
// as a digest, so it decides equality and nothing else, and a foreign
// fingerprint is comparable and unnameable. Naming a remote holder's
// (HostId, ArenaId) needs a record this orchestrator does not publish, and
// whether it may publish one is a disclosure question (#222, #164) rather than
// something to settle here.
func (b *Orchestrator) LookupClaim(ctx context.Context, ref flow.ItemRef) (*flow.ClaimInfo, error) {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return nil, err
	}
	issue, err := b.out.GetIssue(ctx, issueNum)
	if err != nil {
		return nil, fmt.Errorf("get issue %d: %w", issueNum, err)
	}
	holder, _ := b.holderFromLabels(labelNamesOf(issue.Labels))
	if holder.Account == "" {
		return nil, nil
	}
	return &flow.ClaimInfo{
		Arena:     holder.Arena,
		Account:   holder.Account,
		ClaimedAt: issue.GetUpdatedAt().Time,
	}, nil
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
//
// It reads BY EXCLUSION: every label under the prefix that structuralLabels
// (label.go) does not name is a binary name. So a structural marker missing
// from that table does not lose a feature — it turns Claim into a standing
// other-binary refusal on every item carrying it, naming a binary nobody
// wrote, and auto-selection would hand the highest-priority item to a runner
// that declines it for as long as the label is there. That is why the skip
// list is the table and not a second copy of it, and why the table has a
// completeness test.
func (b *Orchestrator) otherBinaryLabel(names []string) (string, bool) {
	for _, n := range names {
		rest, ok := strings.CutPrefix(n, b.labels.prefix)
		if !ok {
			continue
		}
		if _, structural := b.labels.structural(n); structural {
			continue
		}
		// What's left is a binary-name label.
		if rest != b.cfg.BinaryName {
			return rest, true
		}
	}
	return "", false
}

// hasLabelPrefix is hasLabel for a VALUED suffix, whose label carries a value
// after the prefix and so cannot be matched exactly.
func hasLabelPrefix(names []string, want string) bool {
	for _, n := range names {
		if strings.HasPrefix(n, want) {
			return true
		}
	}
	return false
}

func hasLabel(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

const (
	// claimTokenTTL bounds how long a claim attempt can plausibly be in
	// flight. An attempt is seconds long; a token older than this was left
	// behind by a process that died between posting its label and removing
	// it again, and is collected by the next claimer rather than blocking
	// the item forever.
	//
	// The margin between "seconds" and ten minutes is for the clocks, not
	// for the attempt: age is read against the collecting claimer's own
	// clock, so the window also has to cover the disagreement between two
	// claimers racing from different machines.
	claimTokenTTL = 10 * time.Minute

	// claimTokenLen is the token's fixed width: claimTokenTimeLen hex digits
	// of creation time followed by 16 of randomness.
	claimTokenLen     = 24
	claimTokenTimeLen = 8
)

// randRead is the randomness source for claim tokens. Patched by tests.
var randRead = rand.Read

// newClaimToken mints a claim-race token: 8 hex digits of creation time (unix
// seconds) then 16 of randomness. The time leads so lexicographic order is
// chronological order — the earliest attempt still in flight wins — and so a
// later claimer can tell a live attempt from one that was abandoned.
//
// The clock is read through nowUTC, the package's one time source.
func newClaimToken() string {
	secs := uint64(nowUTC().Unix()) & 0xffffffff
	buf := make([]byte, 8) // 64 bits
	if _, err := randRead(buf); err != nil {
		// Random failure: fall back to sub-second time for the random half.
		// Collision impact is minimal in single-runner scenarios. The width
		// is the same as the real thing — a token of another length orders
		// ill-definedly against every other one.
		return fmt.Sprintf("%08x%016x", secs, uint64(nowUTC().UnixNano()))
	}
	return fmt.Sprintf("%08x%s", secs, hex.EncodeToString(buf))
}

// claimTokenAge returns how long ago the token was minted. ok is false when
// the token carries no creation time to read — a different width, a non-hex
// character, or the untimestamped format that predates this one.
func claimTokenAge(token string) (time.Duration, bool) {
	if len(token) != claimTokenLen {
		return 0, false
	}
	if _, err := hex.DecodeString(token); err != nil {
		return 0, false
	}
	// ParseUint, not ParseInt: the latter accepts sign forms.
	secs, err := strconv.ParseUint(token[:claimTokenTimeLen], 16, 64)
	if err != nil {
		return 0, false
	}
	return nowUTC().Sub(time.Unix(int64(secs), 0).UTC()), true
}

// claimTokenAbandoned reports whether a token found on the item was left
// behind by an attempt that is no longer running. A token older than any
// attempt could be is abandoned by definition; so is one carrying no creation
// time at all, which predates this format and therefore has no way to expire.
func claimTokenAbandoned(token string) bool {
	age, ok := claimTokenAge(token)
	return !ok || age > claimTokenTTL
}

// claimRaceReason describes the token that won the race. Abandoned tokens are
// collected before the race is settled, so a token that wins is a live
// attempt, and the refusal says so rather than naming a bare hash.
func claimRaceReason(winner string) string {
	age, ok := claimTokenAge(winner)
	if !ok {
		return fmt.Sprintf("claim race lost to %s", winner)
	}
	if age < 0 {
		age = 0
	}
	return fmt.Sprintf("claim race lost to %s: a claim attempt started %s ago; re-run in a moment",
		winner, age.Truncate(time.Second))
}
