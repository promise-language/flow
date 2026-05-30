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
	LastText        string
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
