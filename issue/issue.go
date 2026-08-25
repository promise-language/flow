// Package issue is the reusable GitHub-issue-backed resolution lifecycle.
//
// It is named after the DATA, not a verb: the issue is where the state lives,
// and the verb depends on who is running the binary. A contributor resolves the
// issue; a maintainer reviews, merges, and inspects the result. Same data, same
// binary, different step sets — selected from the role the authenticated user
// actually holds on the repository (see Role).
//
// # What this package owns, and what it does not
//
// It owns step STRUCTURE: which steps exist, what each one does with the
// worktree and the agent, how they resolve artifacts, and how the flow behaves
// when a step has to ask a human something.
//
// It does NOT own prompt BODIES. Those are project-specific — which build
// commands, which pipeline stages, which policies a change must respect — and
// they stay in the consuming project. A project supplies them through
// Config.Prompts as text/template sources executed against a PromptContext,
// which embeds prompt.Context so the shared partials (item header, ask
// guidance, plan resolution) compose in for free. A library that hardcoded
// prompts would force every consumer to fork the steps to change a sentence,
// which is the outcome this package exists to avoid.
//
// # Shape of a consumer
//
//	backend, _ := ghbackend.NewBackend(ghbackend.Config{BinaryName: "issue", ...})
//	app, err := issue.BuildApp(issue.Config{
//	    BinaryName: "issue",
//	    VerifyCmd:  []string{"bin/verify", "--wasm"},
//	    Prompts:    myPrompts,          // //go:embed templates/*.tmpl
//	    Budgets:    myBudgets,          // optional per-step overrides
//	}, issue.Deps{Backend: backend, Agent: claude.New()})
//	if err != nil { ... }
//	os.Exit(cli.Run(app))
package issue

import (
	"context"
	"time"

	"github.com/promise-language/flow"
)

// StepID names a canonical step. Its value is also the id of the artifact (or
// signal) the step resolves, which is what `grant` and `status` accept.
type StepID string

const (
	// Contributor step set.
	StepPlan       StepID = "plan"
	StepImplement  StepID = "implementation"
	StepReview     StepID = "review"
	StepCoverage   StepID = "coverage"
	StepVerifyImpl StepID = "verify-impl"
	StepOpenPR     StepID = "pr-open" // signal step

	// Maintainer step set. Declared here so both roles share one vocabulary;
	// the handlers land with the maintainer slice.
	StepReviewMaint StepID = "review-maint"
	StepInspect     StepID = "inspection"
	StepVerifyMerge StepID = "verify-merge"
	StepMerge       StepID = "pr-merged" // signal step
	StepRecordMerge StepID = "merge-commit"
)

// PromptID keys Config.Prompts. It is deliberately NOT StepID: a step can have
// more than one prompt. The implement step re-prompts the agent with failing
// verify output, and that re-prompt is a slot, not a step of its own.
//
// Every StepID's value is a valid PromptID; PromptImplementFix is the extra.
type PromptID string

const (
	PromptPlan       PromptID = "plan"
	PromptImplement  PromptID = "implementation"
	PromptReview     PromptID = "review"
	PromptCoverage   PromptID = "coverage"
	PromptVerifyImpl PromptID = "verify-impl"

	// PromptImplementFix is the re-prompt issued when the verify gate fails
	// inside the implement step. PromptContext.VerifyOutput carries the
	// failing tail, and is non-empty only when this prompt is rendered.
	PromptImplementFix PromptID = "implementation:fix"
)

// Role is the capability the authenticated principal holds on the repository,
// collapsed to the two step sets this lifecycle has. It is about capability,
// not intent: a maintainer working on their own change deliberately runs the
// contributor set, which is what Config.Role is for.
type Role string

const (
	RoleContributor Role = "contributor"
	RoleMaintainer  Role = "maintainer"
)

// Answer is one human reply to a question the flow asked.
//
// An alias, not a distinct type: backends produce these and this package
// consumes them, and Go's signatures are nominal, so a look-alike declared here
// would mean no backend could ever satisfy AnswerReader. See flow.Answer.
type Answer = flow.Answer

// ---------------------------------------------------------------------------
// Optional backend capabilities.
//
// This package takes a flow.Backend, not a concrete one. The three interfaces
// below are how it reaches capabilities that only some backends have. Each is
// probed with a type assertion and degrades explicitly — never silently.
// ---------------------------------------------------------------------------

// RoleProber reports what the authenticated principal may do on the repo, so
// the step set can be chosen from it.
//
// Backends with no permission model do not implement this — on a tracker whose
// runner holds full rights by construction there is no role question to ask.
// A backend that does not implement it requires Config.Role to be set, and
// BuildApp refuses at startup otherwise rather than guessing a step set.
type RoleProber interface {
	RepoPermissions(ctx context.Context) (flow.RepoPermissions, error)
}

// BranchDetector reports the repository's default branch, used as the base for
// the working branch. There is no safe literal to fall back on: "main" is
// wrong on master and trunk repos, and a branch cut from the wrong base is not
// discovered until the PR is opened against it. Config.BaseBranch overrides.
type BranchDetector interface {
	DefaultBranch(ctx context.Context) (string, error)
}

// AnswerReader reads human replies to a question the flow asked, so a parked
// step can resume. See the package's park-for-answer contract in answers.go.
//
// `since` is the moment the question was posted; implementations return only
// replies after it, and never the flow's own writing.
//
// `self` is the principal the flow acts as, offered for backends with no finer
// instrument. Prefer excluding by a machine marker on the flow's own writes
// where one exists: a human commonly runs the flow under their own account, so
// excluding by author would discard that human's answer and strand the step.
//
// It takes the Item rather than a Claim because the gate that calls it is a
// preflight, and a preflight is handed only the loaded ItemState — there is no
// claim in scope at that point.
type AnswerReader interface {
	ReadAnswers(ctx context.Context, item flow.Item, since time.Time, self string) ([]Answer, error)
}

// Principal reports the identity the backend acts as, so AnswerReader can tell
// the flow's own comments apart from a human's.
type Principal interface {
	Login(ctx context.Context) (string, error)
}

// Config is what a consuming project supplies. Only Prompts is really
// project-specific; the rest is wiring with workable defaults.
type Config struct {
	// BinaryName is the flow binary's name, used for label scoping.
	BinaryName string

	// VerifyCmd is the project's verify command. It reaches prompt bodies as
	// PromptContext.VerifyCmd, so a template can name the command the agent is
	// expected to make pass.
	VerifyCmd []string

	// DefaultType maps items with no explicit type onto one. It is a FALLBACK
	// for untyped items, not the set of types the flow handles — see ItemTypes.
	DefaultType string

	// ItemTypes is the set of item types the flow handles. Empty means
	// {"task", "bug"} plus DefaultType when that names something else.
	//
	// It exists separately from DefaultType because conflating them silently
	// drops work: an item typed `feature` would match no flow, and the run
	// would report success having done nothing at all.
	ItemTypes []string

	// Prompts is the project's per-slot prompt body, as a text/template source
	// executed against PromptContext. A slot with no entry falls back to the
	// library default, which is deliberately generic — good enough to run, not
	// good enough to ship.
	Prompts map[PromptID]string

	// Budgets overrides the per-step budget. Steps absent from the map take
	// flow's package defaults.
	Budgets map[StepID]flow.StepBudget

	// Role forces the step set. Zero value means detect it from the backend
	// (see RoleProber). Set it when a maintainer is deliberately working their
	// own change through the contributor flow.
	Role Role

	// BaseBranch overrides the base the working branch is cut from. Zero value
	// means detect it (see BranchDetector).
	BaseBranch string

	// MaxFixRounds bounds the implement step's verify-fix loop. Zero means
	// DefaultMaxFixRounds. The loop is ALSO bounded by the step's prompt
	// budget, which parks the step rather than failing it; this bound exists so
	// a project can stop sooner than its budget would.
	MaxFixRounds int
}

// Deps is the runtime wiring: everything with an identity or a side effect.
type Deps struct {
	Backend   flow.Backend
	Agent     flow.Agent
	Telemetry flow.Telemetry
}

// DefaultMaxFixRounds bounds the verify-fix loop when Config.MaxFixRounds is
// unset. Five is chosen to sit under, not at, a typical prompt budget: the loop
// should normally end because the gate went green or the project's own bound
// hit, and only fall through to a budget park when something is genuinely
// looping.
const DefaultMaxFixRounds = 5

// verifyTailLines is how much of a failing verify's output is fed back to the
// agent in the fix re-prompt. The tail, not the head: a build log's useful part
// is the failure at the end, and the head is setup noise.
const verifyTailLines = 60
