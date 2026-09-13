package flow

import (
	"context"
	"time"
)

// AgentRequest is the spawn payload for one Agent.Run call. ResumeSessionID
// empty means "don't actively resume a specific session id" (the Agent
// impl may still attach to whatever session the substrate has cached);
// non-empty resumes that exact session id. FreshSession is the stronger
// "discard any inherited session state" signal — Agent impls should
// honor it by spawning the underlying tool from a clean slate.
//
// BOTH ARE SET BY THE SDK AT THE CHOKEPOINT, NEVER BY A STEP, from the
// resolution's own session (docs/resolution.md § The agent session): the session
// belongs to the resolution, so which conversation a prompt continues is not the
// handler's to choose, exactly as Worktree is not. A step that wants a new one
// declares StepConfig.Session: fresh and the machinery does the rest.
//
// The handle is OFFERED AND NEVER DEPENDED ON. A substrate may decline to
// resume, expire the conversation, or have no such notion; a backend may have
// nowhere to keep a handle. So a step whose result differs depending on whether
// the substrate honoured ResumeSessionID has made an optimisation load-bearing
// and is wrong for that reason — the prompt and the draft are what make a
// dispatch right, and the handle decides only what it costs.
type AgentRequest struct {
	Prompt          string
	ResumeSessionID string
	FreshSession    bool
	PermissionMode  string // default | acceptEdits | bypassPermissions | plan | auto
	Model           string
	Effort          string // low | medium | high | max
	Worktree        string // cwd for the agent process

	// MaxCostUSD is the ceiling on what THIS turn may spend, in USD. Zero
	// means unbounded. It is the headroom left in the step's cost grant, so
	// an impl that can enforce it turns the cost axis from a gate on
	// starting a turn into the cap docs/resolution.md describes — a step
	// that reaches it stops there rather than discovering the overrun at the
	// next dispatch, which is one whole unbounded turn too late.
	//
	// The bound is not exact, and no impl should be described as if it were:
	// a substrate learns what a model call cost only once that call has
	// returned, so the turn stops at the FIRST call that crosses the cap and
	// the overrun is bounded by one model call rather than by a whole turn.
	// That is the difference the axis is for — one call is a quantity an
	// operator can reason about; one turn is not.
	//
	// An impl whose substrate accepts a spend limit MUST pass it through and
	// report the stop as AgentFailure{Kind: FailureCostCap}, keeping the
	// turn's cost in AgentResponse.CostUSD so the meter still bills it. A
	// caller that sets this field itself is asking for a tighter ceiling
	// than the step's; a metered wrapper must narrow it, never widen it. An
	// impl that cannot enforce it may ignore the field; the caller's
	// pre-dispatch and pre-prompt gates still apply.
	MaxCostUSD float64
}

// AgentResponse is the aggregated result of one Agent.Run call. Failure==nil
// indicates success; if non-nil, Failure.Kind describes the failure category.
type AgentResponse struct {
	// LastText is the last text the turn produced — not every text block
	// joined. A turn that ends on a tool call emits a preamble before each one
	// ("Let me check the tests first"), and concatenating those produced an
	// artifact made entirely of narration that no emptiness check could catch.
	//
	// Usually that is the last assistant text block. It is a delegated
	// subagent's output when the turn handed the work to one and said nothing
	// of its own afterwards: the deliverable is produced inside the subagent
	// and never becomes a message of the parent's, so the last thing the parent
	// SAID is the sentence announcing the delegation.
	LastText string

	// PlanText is what the agent submitted through the harness's plan-
	// submission tool, when it has one — the deliverable of a PermissionMode
	// "plan" turn, which ends AT that tool call rather than in assistant text.
	// Empty when the turn did not end that way.
	//
	// Separate from LastText because they answer different questions: LastText
	// is what the agent said, PlanText is what it submitted. A caller that
	// wanted a plan and got prose needs to be able to tell the difference —
	// see PlanSubmitted.
	PlanText string

	// PlanSubmitted reports that the agent CALLED the plan-submission tool,
	// independent of whether anything was captured from it. The pair
	// (PlanSubmitted true, PlanText empty) is the case worth failing on: the
	// agent produced a plan and the transport lost it, which is not the same
	// fact as an agent that never planned.
	PlanSubmitted bool

	ToolsUsed       []string
	CostUSD         float64
	DurationSeconds float64
	// SessionID is the conversation this turn ran in, reported whether the
	// turn finished or died mid-way — a substrate that named its session
	// opened one, and the conversation holds what the dead turn paid for. The
	// SDK records it as the RESOLUTION's and offers it back as
	// Request.ResumeSessionID at the chokepoint; no handler chains it itself
	// (docs/resolution.md § The agent session). Empty where the substrate has
	// no such notion.
	SessionID string
	Failure   *AgentFailure
}

// FailureCostCap is the AgentFailure.Kind for a turn the substrate stopped
// because it reached AgentRequest.MaxCostUSD. One constant because the
// producer (flow/claude) and the consumer (flow/cli's metered agent, which
// translates it into a cost park) must agree on the string.
const FailureCostCap = "cost-cap"

// FailureAccountExhausted is the AgentFailure.Kind for a turn the substrate
// REFUSED because the agent account's allowance is spent — not a failure of the
// agent, the step or the machine, and the one failure kind that knows when it
// ends (docs/environment.md § The agent account).
//
// The condition is established from the substrate's own statement that it
// refused, never inferred from how the turn died: a refused turn also ends
// badly — no result, or an error whose subtype has been observed to read
// `success` — and that ending is not the evidence. An impl that can read the
// statement sets this kind together with Transient, Window and ClearsAt; the
// orchestrator turns it into a ParkAccountExhausted, bills nothing and counts
// no dispatch.
const FailureAccountExhausted = "account-exhausted"

// AgentFailure carries structured failure info inside AgentResponse.
type AgentFailure struct {
	Kind string // no-result | killed | cancelled | exit-error | start-error | cost-cap | account-exhausted
	// Transient signals an infrastructure failure (remote runner died,
	// network blip, transient 5xx) rather than a real claude-side
	// failure. When true, the orchestrator parks the step with
	// ParkInfraTransient and DOES NOT COUNT THE DISPATCH — a flapping
	// runner must not burn the step's invocation budget. Agent impls
	// (typically a backend's runner-HTTP wrapper) set this from
	// substrate-specific signals; cli.RunOne is backend-agnostic.
	//
	// Kind FailureAccountExhausted sets it too, and parks
	// ParkAccountExhausted rather than ParkInfraTransient: the two share
	// exactly the treatment this field names — nothing billed, no dispatch
	// counted — and differ in what an operator is told clears them. Reuse,
	// not a second mechanism: one gate decides what is charged.
	Transient bool
	Message   string

	// Window names which of the account's allowance windows refused, in the
	// substrate's own vocabulary (the reference impl reports `five_hour` /
	// `seven_day` verbatim). Empty on every other kind, and empty when the
	// substrate refused without naming one.
	Window string
	// ClearsAt is the instant the refusing window resets, as the SUBSTRATE
	// PUBLISHED IT — never re-derived afterwards, which can disagree with it
	// and is unavailable exactly when the account is in no state to be
	// queried.
	//
	// A pointer for the reason InvocationResult.CostUSD is one: a zero
	// time.Time is a value, and absent must read as "no instant", never as
	// "soon". nil on every other kind, and nil when the substrate refused
	// without naming a reset.
	ClearsAt *time.Time
}

// Agent is the SDK's abstraction over an LLM CLI (the reference impl is
// flow/claude). Concrete impls live in subpackages so the root pulls in zero
// transitive deps.
type Agent interface {
	Name() string
	Run(ctx context.Context, req AgentRequest) (*AgentResponse, error)
}

// AgentDoctor is an optional Agent capability: report whether the agent can be
// invoked, WITHOUT starting a turn.
//
// `doctor` is mechanical. It runs before every item, in CI, and on every
// machine an operator touches, and nobody asked for work when it runs — so it
// must not spend. A preflight check that bills the account is one an operator
// turns off, and a check that is turned off prevents nothing. That is why the
// agent check is this capability and not a probe turn: a turn is the one thing
// `doctor` may never buy.
//
// What an impl can establish for free is the difference between "the agent is
// there and this SDK can start it" and "it is not" — the reference impl spawns
// the binary and asks its version, which catches an absent, unexecutable,
// wrong-architecture or too-old install. What it CANNOT establish is that a
// full turn would succeed: credentials, quota and model availability are only
// answered by spending. An impl must not paper over that by running a turn.
//
// Doctor must not mutate anything: `doctor` runs on machines that are mid-item.
type AgentDoctor interface {
	// Doctor reports why this agent could not be invoked, or nil when it can.
	// It spends nothing and starts no turn.
	Doctor(ctx context.Context) error
}
