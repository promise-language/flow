package issue

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/promise-language/flow"
)

// roleDecls is this lifecycle's role vocabulary, and the ONE place it is
// written down: the flows are declared from it (BuildApp), and the pre-flow
// decision about which step set to build is made from it (resolveRole). A
// second table, or a second permissions→role rule beside it, would be a second
// answer to "what may this account do here" — and the two would disagree the
// first time either moved.
//
// The line between the two is `merge`. Merging someone else's pull request is
// the act that separates them, and push alone does not confer it on a protected
// default branch — which is exactly the configuration a repository with
// maintainers has.
//
// LEAST PRIVILEGED FIRST, and resolveRole reads that order: the last role an
// account covers is the most privileged one it may assume.
var roleDecls = []flow.RoleDecl{
	{Name: flow.RoleName(RoleContributor), Capabilities: []flow.Capability{flow.CapPush}},
	{Name: flow.RoleName(RoleMaintainer), Capabilities: []flow.Capability{flow.CapPush, flow.CapMerge}},
}

// contributorRole and maintainerRole are the step tags — the same two names
// roleDecls declares, as the flow.RoleName the StepConfig field takes. Spelled
// once here rather than converted at each of the ten registrations, so a tag
// and its declaration cannot be spelled differently.
const (
	contributorRole = flow.RoleName(RoleContributor)
	maintainerRole  = flow.RoleName(RoleMaintainer)
)

// roleDeclFor returns the declaration for one of this package's roles.
//
// It panics on a role with no declaration, because the only callers are this
// package's own flow builders naming their own constants: a miss is a
// programming error in a table three lines long, not a runtime condition.
func roleDeclFor(r Role) flow.RoleDecl {
	for _, d := range roleDecls {
		if d.Name == flow.RoleName(r) {
			return d
		}
	}
	panic(fmt.Sprintf("issue: role %q has no declaration in roleDecls", r))
}

// resolveRole decides which step set to run.
//
// Config.Role wins outright when set — the roles describe capability, not
// intent, and a maintainer working their own change legitimately wants the
// contributor set. Otherwise the account's capabilities are detected and the
// roles they cover are derived from the same declarations the flow is built
// from (docs/resolution.md § Accounts, capabilities and roles: the roles a
// runner could assume are derived from its detected capabilities, never
// assigned by hand).
//
// An account covering NO declared role resolves to the EMPTY role, and the
// error return is reserved for a question that could not be answered — an
// unknown Config.Role, a detection that failed. The two readings are not the
// same: "this account backs no role here" is an answer, and it is a handoff
// rather than a misconfiguration — "a covered role the account cannot back is
// the ordinary split the boundary exists to produce, not a misconfiguration"
// (docs/resolution-standalone.md § Declaring what a binary may do). BuildApp
// turns it into a gate that refuses every dispatch, so nothing runs on an
// account that cannot finish it while `list`, `status`, `answer` and `doctor`
// still work.
//
// What it must NOT do is silently call such an account a contributor — which is
// what a most-privileged-flag-down collapse did: that starts a resolution that
// cannot get past its first push, with the misconfiguration surfacing as a push
// failure several steps in.
func resolveRole(ctx context.Context, cfg Config, backend flow.Orchestrator) (Role, error) {
	switch cfg.Role {
	case RoleContributor, RoleMaintainer:
		return cfg.Role, nil
	case "":
		// fall through to detection
	default:
		return "", fmt.Errorf("issue: unknown Config.Role %q (want %q or %q)",
			cfg.Role, RoleContributor, RoleMaintainer)
	}

	caps, err := backend.DetectCapabilities(ctx, "")
	if err != nil {
		return "", fmt.Errorf("issue: detect repository capabilities: %w", err)
	}
	assumable := flow.AssumableRoles(roleDecls, caps)
	if len(assumable) == 0 {
		return "", nil
	}
	// roleDecls is ordered least privileged first, so the last covered role is
	// the most privileged one this account may assume.
	return Role(assumable[len(assumable)-1]), nil
}

// roleOrNone renders a resolved role for a message, including the empty one an
// account backing nothing resolves to: "%q" on that role would print a pair of
// quotes around nothing, which reads as a bug in the message rather than as the
// account's standing.
func roleOrNone(r Role) string {
	if r == "" {
		return "no role this lifecycle declares"
	}
	return fmt.Sprintf("the %q role", r)
}

// performedRoles is the COVERAGE this binary declares: the roles it may assume,
// derived once from the two config fields that already mean it.
//
// "Capability is the ceiling and coverage is the choice within it: a runner may
// decline a role its account could back, and nothing it declares can add a role
// its account cannot" (docs/resolution.md § Accounts, capabilities and roles).
// So `role` — detected, or set by an operator who is deliberately declining —
// is the ceiling, and CarryThrough is the one choice available inside it: a
// maintainer-capable binary either continues across the boundary or stops at
// the proposal like any contributor.
//
// It is the ONE derivation. Coverage decides what this binary refuses at
// dispatch, and a second reading of the same two fields somewhere else would be
// a second answer to what this binary may do.
//
// The empty role covers nothing. That is the honest answer for an account that
// backs no declared role, and it is a handoff rather than a misconfiguration —
// BuildApp answers it ahead of this gate with the access the operator is
// missing, which is the more useful sentence.
func performedRoles(role Role, carryThrough bool) []flow.RoleName {
	switch {
	case role == RoleContributor:
		return []flow.RoleName{contributorRole}
	case role == RoleMaintainer && carryThrough:
		// One principal covering the roles on both sides, crossing without a
		// handoff (docs/resolution.md § One principal, several roles). Nothing
		// declares carrying through as such: it is exactly this coverage.
		return []flow.RoleName{contributorRole, maintainerRole}
	case role == RoleMaintainer:
		return []flow.RoleName{maintainerRole}
	}
	return nil
}

// coverageGate refuses, before any dispatch, a pending step whose role this
// binary does not cover.
//
// This is what keeps the one graph honest. With three graphs, a binary simply
// did not have the steps it may not perform; with one, every step is present
// and coverage is what says which are this binary's — so without this gate,
// collapsing the graphs would turn every maintainer-capable operator into a
// carry-through runner with no way to decline, and a contributor binary would
// walk straight past the proposal into the merge.
//
// The refusal names the step, the role it belongs to, and what this binary
// performs, because those three are what an operator needs to decide between
// re-running elsewhere and changing the configuration. It wraps flow.ErrBlocked:
// nothing failed, and no later cycle passes until a runner that covers the role
// picks the item up — which is a handoff, exactly what the boundary exists to
// produce.
//
// A finalized position and a Position error both pass through. RunOne's own
// branches own those: the first finalizes and releases, the second is the flow's
// defect and is reported as one, and answering either here would be this gate
// deciding something that is not its question.
func coverageGate(f *flow.Flow, covered []flow.RoleName) flow.PreflightFunc {
	return func(_ context.Context, item *flow.Item) error {
		pos, err := f.Position(item)
		if err != nil || pos.Finalized {
			return nil
		}
		if slices.Contains(covered, pos.Step.Role) {
			return nil
		}
		return fmt.Errorf(
			"issue: %q is the %q role's step, and this binary performs %s — "+
				"the item awaits a runner that covers %q: %w",
			pos.Step.Result(), pos.Step.Role, performedList(covered), pos.Step.Role, flow.ErrBlocked)
	}
}

// performedList renders a coverage set for that refusal. The empty set is a
// sentence rather than an empty list, for the reason roleOrNone exists: "%v" on
// nothing prints a pair of brackets, which reads as a bug in the message rather
// than as the binary's standing.
func performedList(covered []flow.RoleName) string {
	switch len(covered) {
	case 0:
		return "no role this lifecycle declares"
	case 1:
		return fmt.Sprintf("the %q role", covered[0])
	}
	quoted := make([]string, len(covered))
	for i, r := range covered {
		quoted[i] = fmt.Sprintf("%q", r)
	}
	return "the " + strings.Join(quoted, " and ") + " roles"
}

// noAssumableRole is the one wording for "this binary's account backs none of
// the roles this lifecycle declares", named rather than inlined so the refusal
// and the test that asserts what an operator is told read the same sentence.
//
// It names what the least privileged role needs and sends the reader to
// `doctor` rather than reciting what was detected: `doctor` is where the missing
// permission is reported, and docs/cli.md § Doctor requires every condition a
// boundary refuses on to be one `doctor` reports.
var noAssumableRole = fmt.Sprintf(
	"the account this binary acts as backs none of the roles this lifecycle declares — "+
		"%q needs %v on the repository, which `doctor` reports; grant the account "+
		"write access, or set Config.Role to run a step set deliberately",
	RoleContributor, roleDeclFor(RoleContributor).Capabilities)

// noAssumableRoleGate refuses every dispatch when the account backs no declared
// role.
//
// Refusing here rather than at construction is the same judgement coverageGate
// makes, for the same reason: a binary that cannot perform a step can still
// answer `list`, `status`, `grant`, `answer` and `doctor`, and those are exactly
// the commands an operator reaches for on a machine whose credentials are wrong.
// It wraps flow.ErrBlocked, so the invocation reports `blocked` rather than
// `failed` — nothing failed, and no later cycle passes until a person grants the
// access.
//
// It runs AHEAD of coverageGate, whose refusal would also be true here and is
// the less useful of the two: an operator whose account backs nothing needs to
// be told about the access, not about which role's step the route happens to
// stand on.
func noAssumableRoleGate(context.Context, *flow.Item) error {
	return fmt.Errorf("issue: %s: %w", noAssumableRole, flow.ErrBlocked)
}

// resolveBaseBranch decides what the working branch is cut from.
//
// There is no safe literal default. "main" is wrong on a master or trunk repo,
// and the failure mode is quiet: the branch is cut from a base that does not
// exist or is stale, and nothing notices until the pull request is opened
// against it. So it is either configured or detected, never assumed.
func resolveBaseBranch(ctx context.Context, cfg Config, backend flow.Orchestrator) (flow.BranchName, error) {
	if cfg.BaseBranch != "" {
		return cfg.BaseBranch, nil
	}
	detector, ok := backend.(BranchDetector)
	if !ok {
		return "", fmt.Errorf(
			"issue: backend %T cannot report the repository's default branch — "+
				"set Config.BaseBranch explicitly", backend)
	}
	branch, err := detector.DefaultBranch(ctx)
	if err != nil {
		return "", fmt.Errorf("issue: detect default branch: %w", err)
	}
	return branch, nil
}

// baseBranch resolves the base the working branch is cut from, once, on first
// use.
//
// Lazily and not in BuildApp, because BuildApp runs before EVERY command —
// including `doctor`, whose whole purpose is to diagnose the auth and network
// failures this call can hit. A binary that cannot start when the network is
// down cannot tell you the network is down.
func (b *builder) baseBranch(ctx context.Context) (flow.BranchName, error) {
	if p := b.base.Load(); p != nil {
		return *p, nil
	}
	branch, err := resolveBaseBranch(ctx, b.cfg, b.backend)
	if err != nil {
		return "", err
	}
	b.base.Store(&branch)
	return branch, nil
}

// principal returns the identity the backend acts as, or "" when it cannot say.
//
// Resolved on demand for the same reason as baseBranch, and best-effort: it is
// only a hint to answer readers that can use one, and the github backend
// excludes its own writing by marker instead. Failing to resolve it must not
// stop a step.
func (b *builder) principal(ctx context.Context) string {
	if p := b.self.Load(); p != nil {
		return *p
	}
	pr, ok := b.backend.(Principal)
	if !ok {
		login := ""
		b.self.Store(&login) // settled: this backend has no identity to report
		return login
	}
	// The caller's context, not a fresh Background: this runs inside a
	// preflight, and a hung `gh api user` on a detached context would block it
	// with nothing able to cancel.
	login, err := pr.Login(ctx)
	if err != nil {
		// Do NOT cache a failure. Storing "" here would disable the hint for
		// the life of the process over one transient call.
		return ""
	}
	b.self.Store(&login)
	return login
}
