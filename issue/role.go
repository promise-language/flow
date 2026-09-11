package issue

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/promise-language/flow"
)

// roleDecls is this lifecycle's role vocabulary, and the ONE place it is
// written down: the flow is declared from it (BuildApp), and every question
// about what a role requires of an account — `doctor`'s standing report,
// `resolve`'s handoff — is answered from the declaration the flow carries. A
// second table, or a second permissions→role rule beside it, would be a second
// answer to "what may this account do here" — and the two would disagree the
// first time either moved.
//
// The line between the two is `merge`. Merging someone else's pull request is
// the act that separates them, and push alone does not confer it on a protected
// default branch — which is exactly the configuration a repository with
// maintainers has.
//
// Least privileged first, which is the order the flow declares them in and
// the order every report lists them.
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

// coverageGate refuses, before any dispatch, a pending step whose role this
// binary does not cover.
//
// This is what keeps the one graph honest. With three graphs, a binary simply
// did not have the steps it may not perform; with one, every step is present
// and coverage is what says which are this binary's — so without this gate,
// collapsing the graphs would turn every maintainer-capable operator into a
// runner carrying every item through with no way to decline, and a contributor
// binary would walk straight past the proposal into the merge.
//
// `covered` is Config.Coverage as BuildApp converted it — the SAME slice it
// hands cli.App, so the gate and the app cannot disagree about what this binary
// performs.
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
// sentence rather than an empty list: "%v" on nothing prints a pair of
// brackets, which reads as a bug in the message rather than as the binary's
// standing.
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
