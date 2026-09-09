package issue

import (
	"context"
	"fmt"

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
// An account covering NO declared role is a startup error naming what was
// detected. Silently calling it a contributor — which is what a
// most-privileged-flag-down collapse did — starts a resolution that cannot get
// past its first push, with the misconfiguration surfacing as a push failure
// several steps in.
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
		return "", fmt.Errorf(
			"issue: the account this binary acts as can assume no role this lifecycle "+
				"declares — detected capabilities %v; %q needs %v — grant the account "+
				"write access, or set Config.Role to run a step set deliberately",
			caps, RoleContributor, roleDeclFor(RoleContributor).Capabilities)
	}
	// roleDecls is ordered least privileged first, so the last covered role is
	// the most privileged one this account may assume.
	return Role(assumable[len(assumable)-1]), nil
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
