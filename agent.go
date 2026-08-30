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

// AgentFailure carries structured failure info inside AgentResponse.
type AgentFailure struct {
	Kind string // no-result | killed | cancelled | exit-error | start-error
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
