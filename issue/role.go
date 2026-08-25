package issue

import (
	"context"
	"fmt"

	"github.com/promise-language/flow"
)

// resolveRole decides which step set to run.
//
// Config.Role wins outright when set — the roles describe capability, not
// intent, and a maintainer working their own change legitimately wants the
// contributor set. Otherwise the backend is probed, and a backend with no
// permission model is a configuration error rather than a guess: picking the
// wrong step set silently would route a contributor into merge steps they
// cannot perform, and the failure would not surface until the merge call.
func resolveRole(ctx context.Context, cfg Config, backend flow.Backend) (Role, error) {
	switch cfg.Role {
	case RoleContributor, RoleMaintainer:
		return cfg.Role, nil
	case "":
		// fall through to detection
	default:
		return "", fmt.Errorf("issue: unknown Config.Role %q (want %q or %q)",
			cfg.Role, RoleContributor, RoleMaintainer)
	}

	prober, ok := backend.(RoleProber)
	if !ok {
		return "", fmt.Errorf(
			"issue: backend %T cannot report repository permissions, so the step set "+
				"cannot be detected — set Config.Role explicitly", backend)
	}
	perms, err := prober.RepoPermissions(ctx)
	if err != nil {
		return "", fmt.Errorf("issue: probe repository permissions: %w", err)
	}
	return roleFromPermissions(perms), nil
}

// roleFromPermissions collapses the permission bag onto a step set.
//
// Tested most-privileged-first because the flags are cumulative as GitHub
// reports them: an admin carries maintain, push, triage and pull as well, so
// testing push first would call every admin a contributor.
//
// The line is drawn at `maintain`. Merging someone else's pull request is the
// act that separates the two step sets, and push alone does not confer it on a
// protected default branch — which is exactly the configuration a repository
// with maintainers has.
func roleFromPermissions(p flow.RepoPermissions) Role {
	if p.Admin || p.Maintain {
		return RoleMaintainer
	}
	return RoleContributor
}

// resolveBaseBranch decides what the working branch is cut from.
//
// There is no safe literal default. "main" is wrong on a master or trunk repo,
// and the failure mode is quiet: the branch is cut from a base that does not
// exist or is stale, and nothing notices until the pull request is opened
// against it. So it is either configured or detected, never assumed.
func resolveBaseBranch(ctx context.Context, cfg Config, backend flow.Backend) (string, error) {
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
func (b *builder) baseBranch(ctx context.Context) (string, error) {
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
