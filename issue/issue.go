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
	// Contributor step set, in the order docs/issue-flow.md defines. The two
	// branch steps are mechanical and run no agent: one prepares the worktree
	// and records what the branch was cut from, the other returns the worktree
	// to the base once the resolution has completed.
	StepPlan        StepID = "plan"
	StepBranch      StepID = "branch"
	StepImplement   StepID = "implementation"
	StepReview      StepID = "review"
	StepCoverage    StepID = "coverage"
	StepOpenPR      StepID = "pr-open" // signal step
	StepCloseBranch StepID = "branch-closed"

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
// Every AGENT-DRIVEN step's id is a valid PromptID; PromptImplementFix is the
// extra. The mechanical steps — the two branch steps and the pull request —
// have none, because they run no agent and so have nothing to prompt.
type PromptID string

const (
	PromptPlan      PromptID = "plan"
	PromptImplement PromptID = "implementation"
	PromptReview    PromptID = "review"
	PromptCoverage  PromptID = "coverage"

	// PromptImplementFix is the re-prompt issued when the verify gate fails
	// inside the implement step. PromptContext.VerifyOutput carries the
	// failing tail, and is non-empty only when this prompt is rendered.
	PromptImplementFix PromptID = "implementation:fix"

	// PromptRevise is the re-prompt issued when the disclosure guard refuses a
	// step's prose. PromptContext.Refusal and RefusedText carry what was
	// refused and why, and are non-empty only when this prompt is rendered.
	//
	// Like PromptImplementFix it is a slot, not a step — but unlike it, it
	// belongs to no single step, because every step that publishes prose can be
	// refused.
	PromptRevise PromptID = "revise"
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

	// VerifyCmd is the project's verify COMMAND: it repairs what has one right
	// answer — formatting and the like — and then measures. It reaches prompt
	// bodies as PromptContext.VerifyCmd, so a template can name the command the
	// agent is expected to make pass.
	//
	// A producing step works with this. Nothing decides on it: it modifies the
	// tree on its way to an answer, so "it passed" is a claim about a state
	// that did not exist when the question was asked.
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

	// CarryThrough declares that this binary runs both the contributor and
	// integration phases in one resolution, ending at a merged change rather
	// than a proposed one. Requires maintainer capability (Role == RoleMaintainer
	// or detected as such); refused at construction with RoleContributor.
	//
	// This is not independent review. A single principal reviewing its own
	// agent's work buys a second pass with different prompts against a
	// different target — real value, but not a second opinion.
	CarryThrough bool

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

// maxDisclosureRevisions bounds how many times a step re-prompts its agent to
// revise prose the disclosure guard refused.
//
// The step's prompt budget is the real bound — a revision costs a prompt, not
// an invocation, which is what keeps a refused sentence from consuming an
// attempt at the step. This is the project-independent floor under it, so a
// loop against the guard stops before it has eaten the whole prompt cap and
// left nothing for the work.
//
// Not a Config knob: one fewer thing to get wrong, and the budget axes are
// already the operator's lever over it.
const maxDisclosureRevisions = 3

// verifyTailLines is how much of a failing verify's output is fed back to the
// agent in the fix re-prompt. The tail, not the head: a build log's useful part
// is the failure at the end, and the head is setup noise.
const verifyTailLines = 60
