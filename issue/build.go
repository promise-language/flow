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
// Every CONFIGURATION that cannot work fails here, at startup, before any item
// is claimed: an unknown role, a backend that cannot report what it needs, an
// undetectable base branch, carrying through on an account that cannot
// integrate. That matters because the alternative is discovering a
// misconfiguration partway through a claimed item.
//
// What this binary may not PERFORM is a different question and is refused at
// dispatch, by a gate: a binary whose account backs no role, or whose coverage
// does not reach the step the route stands on, still answers `list`, `status`,
// `grant`, `answer` and `doctor` — the commands an operator reaches for
// precisely then.
//
// One caveat worth stating plainly: when Config.Role is UNSET, BuildApp makes a
// live call to detect it, and BuildApp runs before every command — so on an
// expired token `doctor` cannot start to tell you the token expired. Role has
// to be known here because it fixes this binary's coverage, and cli.App is
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

	// Role decides which of the one graph's steps this binary may perform, and
	// cli.App is fixed once built, so it has to be known here. With Config.Role
	// set that costs nothing; otherwise it is one probe. Base branch and
	// principal resolve lazily on the steps that need them, so a binary
	// configured with an explicit Role starts — and `doctor` runs — with no
	// network at all.
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

	// CarryThrough on anything but the maintainer role asks to integrate without
	// the capability to integrate. That is a configuration that cannot produce
	// correct behaviour, so it is a startup error naming the field — the empty
	// role included, where the same sentence is true and the alternative would
	// be a configuration silently dropped.
	if cfg.CarryThrough && role != RoleMaintainer {
		return cli.App{}, fmt.Errorf(
			"issue: Config.CarryThrough requires maintainer capability, "+
				"but the account this binary acts as backs %s — a binary that "+
				"intends to integrate must be able to", roleOrNone(role))
	}

	// The COVERAGE this binary declares: the roles it may assume here, narrowed
	// from what its account can back by what it is configured to do. Derived
	// once, from the two config fields that already mean it, and read only by
	// the gate below — capability is the ceiling and coverage is the choice
	// within it (docs/resolution.md § Accounts, capabilities and roles).
	//
	// Taken BEFORE the contributor fallback below, so it answers about the
	// account rather than about the fallback.
	covered := performedRoles(role, cfg.CarryThrough)

	// An account that backs none of the declared roles resolves to the empty
	// role, and that is a HANDOFF rather than a misconfiguration: "a covered
	// role the account cannot back is the ordinary split the boundary exists to
	// produce, not a misconfiguration" (docs/resolution-standalone.md
	// § Declaring what a binary may do). So this binary starts and refuses at
	// DISPATCH rather than at construction: refusing construction would take
	// `list`, `status`, `grant`, `answer` and `doctor` down with it, the
	// commands an operator reaches for precisely when the credentials are
	// wrong, and `doctor` is where the missing permission is reported
	// (docs/cli.md § Doctor).
	//
	// The fallback that follows is about the PROMPTS, not the step set: there
	// is one graph and it is built either way, and nothing of it can run while
	// the gate stands. What a binary with no standing would tell an agent it is
	// acting as is the least this lifecycle has.
	noRole := role == ""
	var roleGate flow.PreflightFunc
	if noRole {
		role = RoleContributor
		roleGate = noAssumableRoleGate
	}

	b := &builder{cfg: cfg, role: role, backend: deps.Orchestrator}
	if cfg.BaseBranch != "" {
		b.base.Store(&cfg.BaseBranch)
	}

	// ONE GRAPH, whatever this binary performs. A binary registers exactly one
	// flow (docs/flow-registration.md § What a flow is), and which steps a
	// runner may perform is coverage — a narrowing applied at dispatch, not a
	// second graph chosen at build time. Choosing among graphs would decide the
	// processing before the journal begins, invisibly and with nothing recording
	// why; carrying through in particular is not a mode but "one principal
	// covering the roles on both sides of a boundary"
	// (docs/resolution.md § One principal, several roles), which is the same
	// graph either way.
	f := b.resolveFlow(cfg)

	app := cli.App{
		Name:         cfg.BinaryName,
		Orchestrator: deps.Orchestrator,
		Agent:        deps.Agent,
		Telemetry:    deps.Telemetry,
		Artifacts:    resolveArtifacts(),
		Signals:      resolveSignals(),
		Flow:         f,
		CarryThrough: cfg.CarryThrough,
		// cli.App wants the display form (it reaches prompts and messages);
		// cfg.VerifyCmd is argv because that is what a backend execs.
		VerifyCmd: strings.Join(cfg.VerifyCmd, " "),
		// Three gates, in this order, because each answers something the next
		// one presupposes. The no-role gate first: an account backing nothing
		// has no coverage to speak of, and telling such an operator which role's
		// step they stopped at would answer past the access they are missing.
		// The coverage gate next: what it refuses, it refuses whatever the
		// item's questions say — an item awaiting a role this binary does not
		// perform is not this binary's to answer for.
		Preflight: flow.ChainPreflight(
			roleGate,
			coverageGate(f, covered),
			answerGate(deps.Orchestrator, b.principal)),
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

// declareRoles declares the named roles on f, from the one roleDecls table.
//
// A flow declares the roles its steps are tagged with, and the graph has steps
// of both — so both are declared, on every build. What a given binary may
// perform is not this: coverage is a runtime narrowing (issue/role.go), and
// declaring fewer roles to express it would drop the boundary out of the graph
// that is supposed to make the boundary reviewable.
func declareRoles(f *flow.Flow, roles ...Role) {
	for _, r := range roles {
		d := roleDeclFor(r)
		f.Role(d.Name, d.Capabilities...)
	}
}

// resolveFlow is THE graph — docs/issue-flow.md § The graph, whole: the
// contributor's plan-to-proposal run, the boundary at close branch, and the
// maintainer's judgement of what that run proposed.
//
// One flow with both roles declared, not one per role. The boundary between
// them is an edge in this graph (close branch → review the proposal), which is
// what makes it reviewable before anything runs: every handback, every role
// crossing and every way the flow can end is in the declaration. Which of these
// steps a given binary may PERFORM is a separate question, answered at dispatch
// by the coverage gate (issue/role.go) — a narrowing, never a different graph.
func (b *builder) resolveFlow(cfg Config) *flow.Flow {
	f := flow.NewFlow("resolve", itemTypes(cfg))
	declareRoles(f, RoleContributor, RoleMaintainer)
	b.addContributorSteps(f)
	b.addCloseBranch(f)
	b.addMaintainerSteps(f)
	return f
}

// addContributorSteps registers plan through open request: the run that turns
// an item into a proposal.
//
// Every registration declares its Needs and its Leaves, so the resolution's
// branching story is readable along the route rather than improvised step by
// step (docs/issue-flow.md § The branching sequence is declared): plan touches
// nothing, open branch establishes the item's branch, and every producing step
// from there both requires and returns it.
//
// Every registration also declares its Prompts. The mechanical steps —
// docs/issue-flow.md § The graph marks them in bold — declare none, and the
// declaration is what a driver relies on to run them without waiting for
// quota: a prompt from one is refused before anything is sent.
func (b *builder) addContributorSteps(f *flow.Flow) {
	f.AddStep("write plan", flow.ArtifactId(StepPlan), b.stepPlan,
		flow.StepConfig{
			Role:    contributorRole,
			Entry:   true,
			Next:    []flow.StepId{flow.StepId(StepBranch)},
			Prompts: flow.PromptsAgent,
			// The one step in the flow that modifies nothing: a plan written by
			// a step that had already started changing things would describe
			// work done rather than decide work to do.
			Needs:  flow.NeedsAny,
			Leaves: flow.LeavesAsFound,
		})
	f.AddStep("open branch", flow.ArtifactId(StepBranch), b.stepOpenBranch,
		flow.StepConfig{
			Role:    contributorRole,
			Next:    []flow.StepId{flow.StepId(StepImplement)},
			Prompts: flow.PromptsNone,
			Needs:   flow.NeedsAny,
			Writes:  flow.WriteContract{MayBranch: true, MayCommit: true},
			Leaves:  flow.LeavesItemBranch,
		})
	f.AddStep("implement the change", flow.ArtifactId(StepImplement), b.stepImplement,
		flow.StepConfig{
			Role:    contributorRole,
			Next:    []flow.StepId{flow.StepId(StepReview)},
			Prompts: flow.PromptsAgent,
			Needs:   flow.NeedsItemBranch,
			Writes:  flow.WriteContract{MayCommit: true, MayEditTree: true},
			Leaves:  flow.LeavesItemBranch,
		})
	f.AddStep("review the work", flow.ArtifactId(StepReview), b.stepReview,
		flow.StepConfig{
			Role:    contributorRole,
			Next:    []flow.StepId{flow.StepId(StepCoverage)},
			Prompts: flow.PromptsAgent,
			Needs:   flow.NeedsItemBranch,
			Writes:  flow.WriteContract{MayCommit: true, MayEditTree: true},
			Leaves:  flow.LeavesItemBranch,
		})
	f.AddStep("analyze coverage", flow.ArtifactId(StepCoverage), b.stepCoverage,
		flow.StepConfig{
			Role:    contributorRole,
			Next:    []flow.StepId{flow.StepId(StepOpenPR)},
			Prompts: flow.PromptsAgent,
			Needs:   flow.NeedsItemBranch,
			Writes:  flow.WriteContract{MayCommit: true, MayEditTree: true},
			Leaves:  flow.LeavesItemBranch,
		})
	// Two successors, and the second is a failure route rather than a decision:
	// close branch is where the contributor's part ends whoever is running it,
	// and repair disclosure is what the step elects instead of prompting when
	// what the branch carries is refused.
	f.AddSignalStep("create pull request", flow.SignalId(StepOpenPR), b.stepOpenPR,
		flow.StepConfig{
			Role: contributorRole,
			Next: []flow.StepId{
				flow.StepId(StepCloseBranch),
				flow.StepId(StepRepairDisclosure),
			},
			// Mechanical, and the declaration is the deliverable: this is the
			// longest cheap step in the flow — the gate suite, the commit and
			// the push — and what it earns by declaring none is what the whole
			// split was for (docs/flow-registration.md § Step configuration).
			Prompts: flow.PromptsNone,
			Needs:   flow.NeedsItemBranch,
			Writes:  flow.WriteContract{MayBranch: true, MayCommit: true},
			Leaves:  flow.LeavesItemBranch,
		})
	// The back edge, and it is legal: ValidateGraph has no acyclicity
	// constraint, and finalization stays reachable from both — open request
	// keeps its route to close branch (flow/graph.go
	// validateFinalizationReachable).
	//
	// No MayBranch: a rebase neither creates, switches nor publishes a branch,
	// which is what keeps docs/issue-flow.md § Branches are moved only by
	// mechanical steps literally true with an agent-driven step in the graph.
	// MayEditTree, because the agent edits files as it rewrites the history.
	f.AddStep("repair disclosure", flow.ArtifactId(StepRepairDisclosure), b.stepRepairDisclosure,
		flow.StepConfig{
			Role:    contributorRole,
			Next:    []flow.StepId{flow.StepId(StepOpenPR)},
			Prompts: flow.PromptsAgent,
			Needs:   flow.NeedsItemBranch,
			Writes:  flow.WriteContract{MayCommit: true, MayEditTree: true},
			Leaves:  flow.LeavesItemBranch,
		})
}

// addCloseBranch registers the step the contributor's part ends at.
//
// It does NOT finalize. Returning the worktree to the base is the last thing
// the contributor does, not the last thing that happens to the item: the
// proposal still has to be judged, so this elects the maintainer's review —
// and where the maintainer is another principal, that election is the handoff
// (docs/issue-flow.md § Close branch).
func (b *builder) addCloseBranch(f *flow.Flow) {
	f.AddStep("close branch", flow.ArtifactId(StepCloseBranch), b.stepCloseBranch,
		flow.StepConfig{
			// The contributor's. Returning the worktree to its base is
			// housekeeping on the branch the contributor cut — it needs nothing
			// the merge needs, so tagging it maintainer would put a capability
			// requirement on the one step that has none.
			Role:    contributorRole,
			Next:    []flow.StepId{flow.StepId(StepReviewProposal)},
			Prompts: flow.PromptsNone,
			Needs:   flow.NeedsItemBranch,
			Writes:  flow.WriteContract{MayBranch: true},
			Leaves:  flow.LeavesBase,
		})
}

// addMaintainerSteps registers the four steps that judge a proposal and, when
// it should land, carry it to a merged change.
func (b *builder) addMaintainerSteps(f *flow.Flow) {
	// The step the boundary lands on, and the only one in this flow with more
	// than one declared successor: its election IS the decision, and the three
	// routes are the three honest outcomes — integrate, hand back for rework,
	// or reject (docs/issue-flow.md § Review the proposal).
	f.AddStep("review the proposal", flow.ArtifactId(StepReviewProposal), b.stepReviewProposal,
		flow.StepConfig{
			Role: maintainerRole,
			Next: []flow.StepId{
				flow.StepId(StepVerifyMerge),
				flow.StepId(StepImplement),
			},
			MayFinalize: []flow.Disposition{flow.DispositionRejected},
			Prompts:     flow.PromptsAgent,
			// It judges what is proposed and touches nothing: the diff, the
			// plan, the briefings and the gate result all reach it as records.
			Needs:  flow.NeedsAny,
			Leaves: flow.LeavesAsFound,
		})
	// The three that carry a judged proposal to a merged change are mechanical:
	// they run the gate, land the change and record what landed, and none of
	// them reaches the agent.
	f.AddStep("verify merge result", flow.ArtifactId(StepVerifyMerge), b.stepVerifyMerge,
		flow.StepConfig{
			Role:    maintainerRole,
			Next:    []flow.StepId{flow.StepId(StepMerge)},
			Prompts: flow.PromptsNone,
			Needs:   flow.NeedsItemBranch,
			Writes:  flow.WriteContract{MayBranch: true, MayCommit: true},
			Leaves:  flow.LeavesAsFound,
		})
	f.AddSignalStep("merge pull request", flow.SignalId(StepMerge), b.stepMerge,
		flow.StepConfig{
			Role:    maintainerRole,
			Next:    []flow.StepId{flow.StepId(StepRecordMerge)},
			Prompts: flow.PromptsNone,
			Needs:   flow.NeedsAny,
			Leaves:  flow.LeavesAsFound,
		})
	// The finalizer. What landed has exactly one name, and recording it is the
	// last thing the resolution owes — finalization is elected, never derived
	// from a checklist (docs/resolution.md § Finalizing).
	f.AddStep("record merge commit", flow.ArtifactId(StepRecordMerge), b.stepRecordMerge,
		flow.StepConfig{
			Role:        maintainerRole,
			MayFinalize: []flow.Disposition{flow.DispositionResolved},
			Prompts:     flow.PromptsNone,
			Needs:       flow.NeedsAny,
			Leaves:      flow.LeavesAsFound,
		})
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

// resolveArtifacts is THE artifact vocabulary this lifecycle uses — one graph,
// one vocabulary. cli.App refuses at startup if the flow names anything outside
// it, and the ids are the backend's own schema
// (pkg/orchestrator/github: SupportedArtifacts).
//
// It is not split by role. A maintainer reads the contributor's plan and
// briefings and a contributor's request carries the gate result the maintainer
// re-runs; a vocabulary that varied by who was running would make the same
// item's record mean different things to the two halves of one resolution.
func resolveArtifacts() []flow.ArtifactDef {
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
		// A flag too, and for the same reason: the repair's deliverable is the
		// rewritten history on the branch, and the record says it happened —
		// which is the half of this route that a resolution rewriting its own
		// history to get a push out used to have no trace of.
		flow.Artifact(flow.ArtifactId(StepRepairDisclosure), flow.ArtifactFlag),
		flow.Artifact(flow.ArtifactId(StepReviewProposal), flow.ArtifactMarkdown),
		flow.Artifact(flow.ArtifactId(StepVerifyMerge), flow.ArtifactMarkdown),
		flow.Artifact(flow.ArtifactId(StepRecordMerge), flow.ArtifactCommitHash),
	}
}

// resolveSignals is the signal vocabulary, on the same terms.
func resolveSignals() []flow.SignalDef {
	return []flow.SignalDef{
		flow.Signal(flow.SignalId(StepOpenPR), "pull request for the claim branch is open"),
		flow.Signal(flow.SignalId(StepMerge), "pull request has been merged"),
	}
}
