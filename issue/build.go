package issue

import (
	"context"
	"fmt"
	"strings"
	"text/template"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/cli"
)

// BuildApp assembles the cli.App for a consuming project.
//
// Everything that can fail does so here, at startup, before any item is
// claimed: an unknown role, a backend that cannot report the one it needs, an
// undetectable base branch. A flow binary that starts is a flow binary that can
// run, which matters because the alternative is discovering a misconfiguration
// partway through a claimed item.
//
// One caveat worth stating plainly: when Config.Role is UNSET, BuildApp makes a
// live call to detect it, and BuildApp runs before every command — so on an
// expired token `doctor` cannot start to tell you the token expired. Role has
// to be known here because it selects the step set and cli.App's flow list is
// fixed once built. Set Config.Role to remove the call entirely; base branch
// and principal are already lazy and cost nothing at startup.
func BuildApp(ctx context.Context, cfg Config, deps Deps) (cli.App, error) {
	if deps.Orchestrator == nil {
		return cli.App{}, fmt.Errorf("issue: Deps.Backend is required")
	}
	if deps.Agent == nil {
		return cli.App{}, fmt.Errorf("issue: Deps.Agent is required")
	}
	if len(cfg.VerifyCmd) == 0 {
		return cli.App{}, fmt.Errorf("issue: Config.VerifyCmd is required — " +
			"it is what a producing step works with, and what the prompts tell the agent to satisfy")
	}

	// Role decides which step set is registered, and cli.App's flow list is
	// fixed once built, so it has to be known here. With Config.Role set that
	// costs nothing; otherwise it is one probe. Base branch and principal
	// resolve lazily on the steps that need them, so a binary configured with
	// an explicit Role starts — and `doctor` runs — with no network at all.
	// A typo'd prompt key is invisible at run time: PromptID is a string type,
	// so `issue.PromptID("implementaion")` compiles, misses every lookup, and
	// the step silently runs on the generic library default. The project's
	// prompt is the whole reason this package exists, so a key that names no
	// slot is a startup error.
	for id, body := range cfg.Prompts {
		if _, ok := defaultPrompts[id]; !ok {
			return cli.App{}, fmt.Errorf(
				"issue: Config.Prompts has key %q, which is not a prompt slot — "+
					"a misspelled key would silently fall back to the library default", id)
		}
		// Parse the body WITH the fragments the library will append at render
		// time. A project body that compiles on its own but breaks once the
		// library adds {{.DeferCommit}} or similar would fail at step dispatch
		// — mid-claim, after the item has been taken — which is exactly the
		// class of failure this function exists to move to startup.
		toparse := body
		if frags, ok := requiredFragments[id]; ok {
			toparse = appendFragments(body, frags)
		}
		if _, err := template.New(string(id)).Parse(toparse); err != nil {
			return cli.App{}, fmt.Errorf("issue: Config.Prompts[%q] does not parse: %w", id, err)
		}
	}

	role, err := resolveRole(ctx, cfg, deps.Orchestrator)
	if err != nil {
		return cli.App{}, err
	}

	// CarryThrough with RoleContributor asks to integrate without the
	// capability to integrate. That is a configuration that cannot produce
	// correct behaviour, so it is a startup error naming the field.
	if cfg.CarryThrough && role == RoleContributor {
		return cli.App{}, fmt.Errorf(
			"issue: Config.CarryThrough requires maintainer capability, "+
				"but the resolved role is %q — a binary that intends to integrate "+
				"must be able to", RoleContributor)
	}

	b := &builder{cfg: cfg, role: role, backend: deps.Orchestrator}
	if cfg.BaseBranch != "" {
		b.base.Store(&cfg.BaseBranch)
	}

	// The maintainer step set lands with the second slice. Its steps refuse at
	// DISPATCH rather than BuildApp refusing outright: the step set is the only
	// thing missing, and failing construction would take `status`, `list`,
	// `grant` and `doctor` down with it — the commands a maintainer most needs
	// in order to see what a contributor's run left behind.
	//
	// Refusing beats silently running the contributor set, which would have a
	// maintainer opening a pull request against their own review.
	var flows []*flow.Flow
	// roleGate refuses every dispatch while the step set the role needs does
	// not exist. Nil for the roles whose steps do.
	var roleGate flow.PreflightFunc
	switch {
	case cfg.CarryThrough:
		flows = []*flow.Flow{b.carryThroughFlow(cfg)}
	case role == RoleMaintainer:
		flows = []*flow.Flow{b.unimplementedMaintainerFlow(cfg)}
		roleGate = missingMaintainerStepsGate
	default:
		flows = []*flow.Flow{b.contributorFlow(cfg)}
	}

	app := cli.App{
		Name:         cfg.BinaryName,
		Orchestrator: deps.Orchestrator,
		Agent:        deps.Agent,
		Telemetry:    deps.Telemetry,
		Artifacts:    artifactsFor(role, cfg.CarryThrough),
		Signals:      signalsFor(role, cfg.CarryThrough),
		Flows:        flows,
		CarryThrough: cfg.CarryThrough,
		// cli.App wants the display form (it reaches prompts and messages);
		// cfg.VerifyCmd is argv because that is what a backend execs.
		VerifyCmd: strings.Join(cfg.VerifyCmd, " "),
		// The role gate runs AHEAD of the answer gate: what it refuses, it
		// refuses whatever the item's questions say.
		Preflight: flow.ChainPreflight(roleGate, answerGate(deps.Orchestrator, b.principal)),
	}
	// Budgets are the project's policy, not a step declaration
	// (docs/flow-registration.md § Step configuration). Config.Budgets is
	// already where the project writes them; this is the one place they are
	// handed to the SDK. A step the project says nothing about is funded at the
	// package defaults.
	if len(cfg.Budgets) > 0 {
		app.StepBudgets = make(map[flow.StepId]flow.StepBudget, len(cfg.Budgets))
		for id, budget := range cfg.Budgets {
			app.StepBudgets[flow.StepId(id)] = budget
		}
	}
	return app, nil
}

// contributorFlow is the canonical contributor step set, in order.
func (b *builder) contributorFlow(cfg Config) *flow.Flow {
	f := flow.NewFlow("resolve", itemTypes(cfg))
	b.addContributorSteps(f)
	// Closing the branch needs no "did the resolution complete" test of its
	// own: DeriveNext returns the first PENDING step in registration order, so
	// a run that parked, was blocked or failed never reaches a step registered
	// after the request. The ordering is the condition.
	f.AddStep("close branch", flow.ArtifactId(StepCloseBranch), b.stepCloseBranch,
		flow.StepConfig{Writes: flow.WriteContract{MayBranch: true}})
	return f
}

// addContributorSteps registers the plan-through-openPR steps that every
// contributor-capable flow uses. Factored out so the carry-through flow
// composes it with the integration steps without duplicating the list.
func (b *builder) addContributorSteps(f *flow.Flow) {
	f.AddStep("write plan", flow.ArtifactId(StepPlan), b.stepPlan,
		flow.StepConfig{})
	f.AddStep("open branch", flow.ArtifactId(StepBranch), b.stepOpenBranch,
		flow.StepConfig{Writes: flow.WriteContract{MayBranch: true, MayCommit: true}})
	f.AddStep("implement the change", flow.ArtifactId(StepImplement), b.stepImplement,
		flow.StepConfig{Writes: flow.WriteContract{MayCommit: true, MayEditTree: true}})
	f.AddStep("review the work", flow.ArtifactId(StepReview), b.stepReview,
		flow.StepConfig{Writes: flow.WriteContract{MayCommit: true, MayEditTree: true}})
	f.AddStep("analyze coverage", flow.ArtifactId(StepCoverage), b.stepCoverage,
		flow.StepConfig{Writes: flow.WriteContract{MayCommit: true, MayEditTree: true}})
	f.AddSignalStep("create pull request", flow.SignalId(StepOpenPR), b.stepOpenPR,
		flow.StepConfig{Writes: flow.WriteContract{MayBranch: true, MayCommit: true}})
}

// addIntegrationSteps registers the three integration steps: verify the merge
// result, merge, record the merge commit.
func (b *builder) addIntegrationSteps(f *flow.Flow) {
	f.AddStep("verify merge result", flow.ArtifactId(StepVerifyMerge), b.stepVerifyMerge,
		flow.StepConfig{Writes: flow.WriteContract{MayBranch: true, MayCommit: true}})
	f.AddSignalStep("merge pull request", flow.SignalId(StepMerge), b.stepMerge,
		flow.StepConfig{})
	f.AddStep("record merge commit", flow.ArtifactId(StepRecordMerge), b.stepRecordMerge,
		flow.StepConfig{})
}

// carryThroughFlow composes the contributor steps and the integration steps
// into one flow that ends at a merged change rather than a proposed one.
func (b *builder) carryThroughFlow(cfg Config) *flow.Flow {
	f := flow.NewFlow("resolve", itemTypes(cfg))
	b.addContributorSteps(f)
	b.addIntegrationSteps(f)
	f.AddStep("close branch", flow.ArtifactId(StepCloseBranch), b.stepCloseBranch,
		flow.StepConfig{Writes: flow.WriteContract{MayBranch: true}})
	return f
}

// missingMaintainerSteps is the one wording for "this binary has maintainer
// capability and the maintainer step set does not exist yet". Shared by the
// preflight gate that refuses before anything runs and by the stub handler
// behind it, so an operator reads one sentence whichever path they reach.
var missingMaintainerSteps = fmt.Sprintf(
	"the maintainer step set is not implemented yet — "+
		"set Config.Role to %q to run the contributor steps deliberately",
	RoleContributor)

// missingMaintainerStepsGate refuses every dispatch of the maintainer flow,
// BEFORE the mandatory seed gate runs.
//
// Refusing here rather than in the stub handler is load-bearing, not
// politeness. Every lifecycle item is required, so reaching the seed gate
// would checklist the item with the `review-maint` artifact — and seeding is
// ONE-SHOT. An admin who ran this once and then set Config.Role to contributor
// would find an item seeded with none of the contributor artifacts, and every
// step would die on "artifact not seeded" with no way back short of
// hand-editing the state comment.
//
// It wraps flow.ErrBlocked, so the invocation reports `blocked` rather than
// `failed`. That is the accurate verdict: nothing failed, and no later cycle
// will pass until a person sets Config.Role.
func missingMaintainerStepsGate(context.Context, *flow.Item) error {
	return fmt.Errorf("issue: %s: %w", missingMaintainerSteps, flow.ErrBlocked)
}

// unimplementedMaintainerFlow stands in for the maintainer step set until it
// lands: one step that refuses when dispatched, so every read-only command
// still works while `run-step` says plainly what is missing.
//
// The handler is the backstop, not the gate — missingMaintainerStepsGate stops
// the dispatch before this can run. It stays because AddStep panics on a nil
// handler, and because a step that could somehow be reached must still refuse
// rather than resolve.
func (b *builder) unimplementedMaintainerFlow(cfg Config) *flow.Flow {
	f := flow.NewFlow("review", itemTypes(cfg))
	f.AddStep("review the implementation", flow.ArtifactId(StepReviewMaint),
		func(ctx flow.StepCtx) error {
			return fmt.Errorf("issue: %s", missingMaintainerSteps)
		},
		flow.StepConfig{})
	return f
}

// itemTypes is the set of item types the flow handles.
//
// DefaultType is folded in but does not replace the set: it names what an
// UNTYPED item becomes, which is a different question from which types the
// flow accepts. Treating them as the same silently drops every item carrying
// some third type — the run reports success having selected no flow at all.
func itemTypes(cfg Config) []flow.ItemType {
	seen := map[string]bool{}
	var out []flow.ItemType
	add := func(t string) {
		if t == "" || seen[t] {
			return
		}
		seen[t] = true
		out = append(out, flow.ItemType(t))
	}
	for _, t := range cfg.ItemTypes {
		add(t)
	}
	if len(out) == 0 {
		add("task")
		add("bug")
	}
	add(cfg.DefaultType)
	return out
}

// contributorArtifacts is the artifact vocabulary the contributor steps use.
// It is a subset of what the backend supports; cli.App validates it at startup.
func contributorArtifacts() []flow.ArtifactDef {
	return []flow.ArtifactDef{
		flow.Artifact(flow.ArtifactId(StepPlan), flow.ArtifactMarkdown),
		// Both branch-cut and implementation name a COMMIT. The deliverable is
		// what sits on the branch, and a copy of it — a patch — can be empty on
		// a resumed branch, is read back by nothing, and can disagree with the
		// thing it copies.
		flow.Artifact(flow.ArtifactId(StepBranch), flow.ArtifactCommitHash),
		flow.Artifact(flow.ArtifactId(StepImplement), flow.ArtifactCommitHash),
		flow.Artifact(flow.ArtifactId(StepReview), flow.ArtifactMarkdown),
		flow.Artifact(flow.ArtifactId(StepCoverage), flow.ArtifactMarkdown),
		// A flag: closing the branch restores rather than produces, and every
		// step still owes exactly one result.
		flow.Artifact(flow.ArtifactId(StepCloseBranch), flow.ArtifactFlag),
	}
}

// integrationArtifacts is the artifact vocabulary the integration steps use.
func integrationArtifacts() []flow.ArtifactDef {
	return []flow.ArtifactDef{
		flow.Artifact(flow.ArtifactId(StepVerifyMerge), flow.ArtifactMarkdown),
		flow.Artifact(flow.ArtifactId(StepRecordMerge), flow.ArtifactCommitHash),
	}
}

// integrationSignals is the signal vocabulary the integration steps use.
func integrationSignals() []flow.SignalDef {
	return []flow.SignalDef{
		flow.Signal(flow.SignalId(StepMerge), "pull request has been merged"),
	}
}

// artifactsFor is the artifact vocabulary the registered flow uses. cli.App
// refuses at startup if a flow names anything outside it.
func artifactsFor(role Role, carryThrough bool) []flow.ArtifactDef {
	if carryThrough {
		return append(contributorArtifacts(), integrationArtifacts()...)
	}
	if role == RoleMaintainer {
		return []flow.ArtifactDef{
			flow.Artifact(flow.ArtifactId(StepReviewMaint), flow.ArtifactMarkdown),
		}
	}
	return contributorArtifacts()
}

// signalsFor mirrors artifactsFor.
func signalsFor(role Role, carryThrough bool) []flow.SignalDef {
	if carryThrough {
		return append(contributorSignals(), integrationSignals()...)
	}
	if role == RoleMaintainer {
		return nil
	}
	return contributorSignals()
}

func contributorSignals() []flow.SignalDef {
	return []flow.SignalDef{
		flow.Signal(flow.SignalId(StepOpenPR), "pull request for the claim branch is open"),
	}
}
