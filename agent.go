package flow

import "context"

// AgentRequest is the spawn payload for one Agent.Run call. ResumeSessionID
// empty means "don't actively resume a specific session id" (the Agent
// impl may still attach to whatever session the substrate has cached);
// non-empty resumes that exact session id. FreshSession is the stronger
// "discard any inherited session state" signal — Agent impls should
// honor it by spawning the underlying tool from a clean slate. Useful at
// flow-boundary turns (e.g. the plan step opens a new piece of work and
// must never inherit the previous flow's chat history).
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
	// LastText is the LAST assistant text block of the turn — not every text
	// block joined. A turn that ends on a tool call emits a preamble before
	// each one ("Let me check the tests first"), and concatenating those
	// produced an artifact made entirely of narration that no emptiness check
	// could catch.
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
	SessionID       string // for chaining via Request.ResumeSessionID
	Failure         *AgentFailure
}

// FailureCostCap is the AgentFailure.Kind for a turn the substrate stopped
// because it reached AgentRequest.MaxCostUSD. One constant because the
// producer (flow/claude) and the consumer (flow/cli's metered agent, which
// translates it into a cost park) must agree on the string.
const FailureCostCap = "cost-cap"

// AgentFailure carries structured failure info inside AgentResponse.
type AgentFailure struct {
	Kind string // no-result | killed | cancelled | exit-error | start-error | cost-cap
	// Transient signals an infrastructure failure (remote runner died,
	// network blip, transient 5xx) rather than a real claude-side
	// failure. When true, the orchestrator parks the step with
	// ParkInfraTransient and SKIPS the BumpInvocations call — a flapping
	// runner must not burn the step's invocation budget. Agent impls
	// (typically a backend's runner-HTTP wrapper) set this from
	// substrate-specific signals; cli.RunOne is backend-agnostic.
	Transient bool
	Message   string
}

// Agent is the SDK's abstraction over an LLM CLI (the reference impl is
// flow/claude). Concrete impls live in subpackages so the root pulls in zero
// transitive deps.
type Agent interface {
	Name() string
	Run(ctx context.Context, req AgentRequest) (*AgentResponse, error)
}
