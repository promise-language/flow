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
	if deps.Backend == nil {
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
		// Parse the body too. A template that does not compile fails at step
		// dispatch otherwise — mid-claim, after the item has been taken — which
		// is exactly the class of failure this function exists to move to
		// startup.
		if _, err := template.New(string(id)).Parse(body); err != nil {
			return cli.App{}, fmt.Errorf("issue: Config.Prompts[%q] does not parse: %w", id, err)
		}
	}

	role, err := resolveRole(ctx, cfg, deps.Backend)
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

	b := &builder{cfg: cfg, role: role, backend: deps.Backend}
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
	switch {
	case cfg.CarryThrough:
		flows = []*flow.Flow{b.carryThroughFlow(cfg)}
	case role == RoleMaintainer:
		flows = []*flow.Flow{b.unimplementedMaintainerFlow(cfg)}
	default:
		flows = []*flow.Flow{b.contributorFlow(cfg)}
	}

	app := cli.App{
		Name:         cfg.BinaryName,
		Backend:      deps.Backend,
		Agent:        deps.Agent,
		Telemetry:    deps.Telemetry,
		Artifacts:    artifactsFor(role, cfg.CarryThrough),
		Signals:      signalsFor(role, cfg.CarryThrough),
		Flows:        flows,
		CarryThrough: cfg.CarryThrough,
		// cli.App wants the display form (it reaches prompts and messages);
		// cfg.VerifyCmd is argv because that is what a backend execs.
		VerifyCmd: strings.Join(cfg.VerifyCmd, " "),
		Preflight: answerGate(deps.Backend, b.principal),
	}
	return app, nil
}

// contributorFlow is the canonical contributor step set, in order.
func (b *builder) contributorFlow(cfg Config) *flow.Flow {
	f := flow.NewFlow("resolve", itemTypes(cfg))
	b.addContributorSteps(f, cfg)
	// Closing the branch needs no "did the resolution complete" test of its
	// own: DeriveNext returns the first PENDING step in registration order, so
	// a run that parked, was blocked or failed never reaches a step registered
	// after the request. The ordering is the condition.
	f.AddStep("close branch", flow.ArtifactId(StepCloseBranch), b.stepCloseBranch,
		flow.StepConfig{Budget: cfg.budgetFor(StepCloseBranch),
			Writes: flow.WriteContract{MayBranch: true}})
	return f
}

// addContributorSteps registers the plan-through-openPR steps that every
// contributor-capable flow uses. Factored out so the carry-through flow
// composes it with the integration steps without duplicating the list.
func (b *builder) addContributorSteps(f *flow.Flow, cfg Config) {
	f.AddStep("write plan", flow.ArtifactId(StepPlan), b.stepPlan,
		flow.StepConfig{Budget: cfg.budgetFor(StepPlan)})
	f.AddStep("open branch", flow.ArtifactId(StepBranch), b.stepOpenBranch,
		flow.StepConfig{Budget: cfg.budgetFor(StepBranch),
			Writes: flow.WriteContract{MayBranch: true, MayCommit: true}})
	f.AddStep("implement the change", flow.ArtifactId(StepImplement), b.stepImplement,
		flow.StepConfig{Budget: cfg.budgetFor(StepImplement),
			Writes: flow.WriteContract{MayCommit: true, MayEditTree: true}})
	f.AddStep("review the work", flow.ArtifactId(StepReview), b.stepReview,
		flow.StepConfig{Budget: cfg.budgetFor(StepReview),
			Writes: flow.WriteContract{MayCommit: true, MayEditTree: true}})
	f.AddStep("analyze coverage", flow.ArtifactId(StepCoverage), b.stepCoverage,
		flow.StepConfig{Budget: cfg.budgetFor(StepCoverage),
			Writes: flow.WriteContract{MayCommit: true, MayEditTree: true}})
	f.AddSignalStep("create pull request", flow.SignalId(StepOpenPR), b.stepOpenPR,
		flow.StepConfig{Budget: cfg.budgetFor(StepOpenPR),
			Writes: flow.WriteContract{MayBranch: true, MayCommit: true}})
}

// addIntegrationSteps registers the three integration steps: verify the merge
// result, merge, record the merge commit.
func (b *builder) addIntegrationSteps(f *flow.Flow, cfg Config) {
	f.AddStep("verify merge result", flow.ArtifactId(StepVerifyMerge), b.stepVerifyMerge,
		flow.StepConfig{Budget: cfg.budgetFor(StepVerifyMerge),
			Writes: flow.WriteContract{MayBranch: true, MayCommit: true}})
	f.AddSignalStep("merge pull request", flow.SignalId(StepMerge), b.stepMerge,
		flow.StepConfig{Budget: cfg.budgetFor(StepMerge)})
	f.AddStep("record merge commit", flow.ArtifactId(StepRecordMerge), b.stepRecordMerge,
		flow.StepConfig{Budget: cfg.budgetFor(StepRecordMerge)})
}

// carryThroughFlow composes the contributor steps and the integration steps
// into one flow that ends at a merged change rather than a proposed one.
func (b *builder) carryThroughFlow(cfg Config) *flow.Flow {
	f := flow.NewFlow("resolve", itemTypes(cfg))
	b.addContributorSteps(f, cfg)
	b.addIntegrationSteps(f, cfg)
	f.AddStep("close branch", flow.ArtifactId(StepCloseBranch), b.stepCloseBranch,
		flow.StepConfig{Budget: cfg.budgetFor(StepCloseBranch),
			Writes: flow.WriteContract{MayBranch: true}})
	return f
}

// unimplementedMaintainerFlow stands in for the maintainer step set until it
// lands: one step that refuses when dispatched, so every read-only command
// still works while `run-step` says plainly what is missing.
func (b *builder) unimplementedMaintainerFlow(cfg Config) *flow.Flow {
	f := flow.NewFlow("review", itemTypes(cfg))
	f.AddStep("review the implementation", flow.ArtifactId(StepReviewMaint),
		func(ctx flow.StepCtx) error {
			return fmt.Errorf(
				"issue: the maintainer step set is not implemented yet — "+
					"set Config.Role to %q to run the contributor steps deliberately",
				RoleContributor)
		},
		// Optional, and that is load-bearing rather than cosmetic. A REQUIRED
		// artifact makes the item seed on first dispatch, and seeding is
		// one-shot: an admin who ran this once would have the issue
		// permanently checklisted with a maintainer artifact, and switching to
		// the contributor role afterwards would never re-seed — every step
		// would then die on "artifact not seeded" with no way back short of
		// hand-editing the state comment.
		flow.StepConfig{Optional: true, Budget: cfg.budgetFor(StepReviewMaint)})
	return f
}

// budgetFor returns the project's override for a step, or the zero budget —
// which flow resolves to its package defaults, axis by axis.
func (c Config) budgetFor(id StepID) flow.StepBudget {
	if c.Budgets == nil {
		return flow.StepBudget{}
	}
	return c.Budgets[id]
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
