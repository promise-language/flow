// Package claude is the reference flow.Agent implementation: it spawns the
// `claude` CLI with stream-json I/O and aggregates the per-turn events into
// a flow.AgentResponse.
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/promise-language/flow"
)

// Client is the flow.Agent backed by the claude CLI. Binary defaults to
// "claude" if empty (looked up on PATH). ExtraArgs is appended after every
// invocation's argv — useful for testing or for forwarding custom flags.
//
// Requires claude CLI v2.1.217 or later: that is the first release whose
// --max-budget-usd actually stops the run at the cap, which is what
// AgentRequest.MaxCostUSD relies on.
type Client struct {
	Binary    string
	ExtraArgs []string

	// AllowedTools restricts the agent to exactly these tools. Each entry
	// is a tool name or pattern the CLI accepts (e.g. "Read", "Bash(git *)").
	// Empty means no restriction (all tools permitted). This is the SDK's
	// expression of the "run tool" gate (docs/gates-and-commands.md §Gates
	// on what an agent does).
	AllowedTools []string

	// DisallowedTools forbids specific tools. Each entry is a tool name or
	// pattern. Applied after AllowedTools when both are set.
	// Empty means nothing explicitly forbidden.
	DisallowedTools []string

	// spawn is the seam tests use to substitute a fake process. nil means
	// use os/exec.CommandContext.
	spawn spawnFunc
}

// New returns a Client wired to the real claude binary.
func New() *Client { return &Client{Binary: "claude"} }

func (c *Client) Name() string {
	if c.Binary == "" {
		return "claude"
	}
	return c.Binary
}

func (c *Client) Run(ctx context.Context, req flow.AgentRequest) (*flow.AgentResponse, error) {
	args := []string{
		"--print",
		"--verbose",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.PermissionMode != "" {
		args = append(args, "--permission-mode", req.PermissionMode)
	}
	if req.ResumeSessionID != "" {
		args = append(args, "--resume", req.ResumeSessionID)
	}
	if req.MaxCostUSD > 0 {
		// Exact, not rounded: %.2f would round 20.005 UP and hand the turn
		// half a cent more than the step was granted.
		args = append(args, "--max-budget-usd", strconv.FormatFloat(req.MaxCostUSD, 'f', -1, 64))
	}
	for _, t := range c.AllowedTools {
		args = append(args, "--allowed-tools", t)
	}
	for _, t := range c.DisallowedTools {
		args = append(args, "--disallowed-tools", t)
	}
	args = append(args, c.ExtraArgs...)

	binary := c.Binary
	if binary == "" {
		binary = "claude"
	}

	cmd := c.spawnCmd(ctx, binary, args...)
	if req.Worktree != "" {
		cmd.SetDir(req.Worktree)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, &startError{wrapped: fmt.Errorf("stdin pipe: %w", err)}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &startError{wrapped: fmt.Errorf("stdout pipe: %w", err)}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, &startError{wrapped: fmt.Errorf("stderr pipe: %w", err)}
	}

	if err := cmd.Start(); err != nil {
		return nil, &startError{wrapped: err}
	}

	// Write the prompt as a single user event, then close stdin so claude
	// finishes the turn.
	writeErr := make(chan error, 1)
	go func() {
		defer close(writeErr)
		enc := json.NewEncoder(stdin)
		ue := userEvent{
			Type: "user",
			Message: userMessage{
				Role:    "user",
				Content: []contentBlock{{Type: "text", Text: req.Prompt}},
			},
		}
		if err := enc.Encode(ue); err != nil {
			writeErr <- err
		}
		_ = stdin.Close()
	}()

	// Drain stderr in the background so the child doesn't block on a full
	// pipe; capture it for failure diagnostics.
	stderrBuf := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(stderr)
		stderrBuf <- b
	}()

	resp, parseErr := parseStream(stdout)

	waitErr := cmd.Wait()
	stderrBytes := <-stderrBuf

	// Stdin write failure is rare but worth surfacing.
	if err := <-writeErr; err != nil && parseErr == nil {
		parseErr = fmt.Errorf("stdin write: %w", err)
	}

	// Context cancellation overrides everything else.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &flow.AgentResponse{
			Failure: &flow.AgentFailure{
				Kind:    "cancelled",
				Message: ctxErr.Error(),
			},
		}, nil
	}

	if parseErr != nil {
		return &flow.AgentResponse{
			Failure: &flow.AgentFailure{
				Kind:      "no-result",
				Message:   combineDiagnostic(parseErr, waitErr, stderrBytes),
				Transient: true, // the agent is not broken; something on the host interfered
			},
		}, nil
	}
	if waitErr != nil && resp.SessionID == "" && resp.LastText == "" {
		// No usable output and the process errored — treat as exit-error.
		return &flow.AgentResponse{
			Failure: &flow.AgentFailure{
				Kind:      "exit-error",
				Message:   combineDiagnostic(nil, waitErr, stderrBytes),
				Transient: true, // the process crashed before producing output — infrastructure failure
			},
		}, nil
	}
	return resp, nil
}

// parseStream consumes stream-json events from r and aggregates them into an
// AgentResponse. A successful parse REQUIRES a `result` event; absent one,
// returns an error so Run can map it to a no-result failure.
func parseStream(r io.Reader) (*flow.AgentResponse, error) {
	scanner := bufio.NewScanner(r)
	// claude emits long JSON lines; raise the scanner buffer.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	resp := &flow.AgentResponse{}
	toolSeen := map[string]struct{}{}
	// delegated holds the tool_use ids of the delegation calls this turn made,
	// so a later tool_result can be recognised as a subagent's output rather
	// than some other tool's. Local to the parse: nothing outside needs it, and
	// ToolsUsed already reports that a delegation happened.
	delegated := map[string]struct{}{}
	var (
		lastText     string
		resultEvent  bool
		isError      bool
		errorSubtype string
	)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			// Skip non-JSON noise lines defensively.
			continue
		}
		switch probe.Type {
		case "system":
			var ev systemEvent
			if err := json.Unmarshal(line, &ev); err == nil && ev.SessionID != "" {
				resp.SessionID = ev.SessionID
			}
		case "assistant":
			var ev assistantEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				continue
			}
			for _, block := range ev.Message.Content {
				switch block.Type {
				case "text":
					// The LAST text the turn produced, not the accumulation.
					// Every tool call is preceded by a one-line preamble;
					// joining them yields narration that reads like content and
					// passes any emptiness check. A delegated subagent's output
					// is written to the same variable by the "user" arm below,
					// because it too is text this turn produced — the stream is
					// already in order, so "last" falls out of assignment.
					if strings.TrimSpace(block.Text) != "" {
						lastText = block.Text
					}
				case "tool_use":
					if _, dup := toolSeen[block.Name]; !dup && block.Name != "" {
						toolSeen[block.Name] = struct{}{}
						resp.ToolsUsed = append(resp.ToolsUsed, block.Name)
					}
					if delegationTools[block.Name] && block.ID != "" {
						delegated[block.ID] = struct{}{}
					}
					if block.Name == exitPlanTool {
						// Recorded even when the input will not decode, so a
						// caller can tell "never planned" from "planned and we
						// lost it".
						resp.PlanSubmitted = true
						var in struct {
							Plan string `json:"plan"`
						}
						if err := json.Unmarshal(block.Input, &in); err == nil {
							resp.PlanText = in.Plan
						}
					}
				}
			}
		case "user":
			// The only user events worth reading are the tool_results of this
			// turn's own delegations: a subagent's plan, review or answer is
			// text the turn produced, and it arrives here or nowhere. Every
			// other tool_result is the tool's output, not the turn's — a Read's
			// file contents must never become the answer — and the prompt echo
			// is not a result at all.
			var ev userEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				continue
			}
			for _, block := range ev.Message.Content {
				if block.Type != "tool_result" || block.IsError {
					continue
				}
				if _, ok := delegated[block.ToolUseID]; !ok {
					continue
				}
				if text := toolResultText(block.Content); strings.TrimSpace(text) != "" {
					lastText = text
				}
			}
		case "result":
			var ev resultEvent_
			if err := json.Unmarshal(line, &ev); err != nil {
				continue
			}
			resultEvent = true
			if ev.SessionID != "" {
				resp.SessionID = ev.SessionID
			}
			// ev.Result is the harness's own view of the turn's answer and
			// wins when present. It is EMPTY for a turn that ended on a tool
			// call — which is exactly how a plan-mode turn ends — and the plan
			// is the deliverable there, so it comes before the preamble.
			switch {
			case ev.Result != "":
				resp.LastText = ev.Result
			case resp.PlanText != "":
				resp.LastText = resp.PlanText
			default:
				resp.LastText = lastText
			}
			resp.CostUSD = ev.TotalCostUSD
			resp.DurationSeconds = float64(ev.DurationMs) / 1000.0
			isError = ev.IsError
			errorSubtype = ev.Subtype
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	if !resultEvent {
		return nil, errors.New("stream ended without a result event")
	}
	if isError {
		// A budget stop is a clean end-of-run, not a broken one: the result
		// event carries total_cost_usd for everything spent including the
		// response that crossed the cap, so the caller can bill the turn and
		// park on cost instead of reporting an opaque exit-error.
		kind := "exit-error"
		if errorSubtype == subtypeMaxBudget {
			kind = flow.FailureCostCap
		}
		resp.Failure = &flow.AgentFailure{
			Kind:    kind,
			Message: "claude reported is_error (subtype=" + errorSubtype + ")",
		}
	}
	// Same precedence as the result branch, for a stream that carried text or a
	// plan but no result event to hang them on.
	if resp.LastText == "" {
		if resp.PlanText != "" {
			resp.LastText = resp.PlanText
		} else {
			resp.LastText = lastText
		}
	}
	return resp, nil
}

// toolResultText renders a tool_result's content as the text it carries. The
// CLI writes that content two ways — a bare JSON string for short results, an
// array of text blocks for structured ones — and both shapes appear in real
// transcripts, so both are read. Anything else yields "" and is ignored: a
// shape this parser does not recognise is not the turn's deliverable, and
// guessing at one would put something arbitrary under the artifact's name.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func combineDiagnostic(parseErr, waitErr error, stderr []byte) string {
	parts := []string{}
	if parseErr != nil {
		parts = append(parts, parseErr.Error())
	}
	if waitErr != nil {
		parts = append(parts, "wait: "+waitErr.Error())
	}
	if len(stderr) > 0 {
		parts = append(parts, "stderr: "+truncate(string(stderr), 4096))
	}
	if len(parts) == 0 {
		return "unknown failure"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "; " + p
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}

// startError is the typed wrapper Run returns for spawn-time problems before
// the child has produced any output.
type startError struct{ wrapped error }

func (e *startError) Error() string { return "claude: start: " + e.wrapped.Error() }
func (e *startError) Unwrap() error { return e.wrapped }

// stream-json event shapes (subset).

type userEvent struct {
	Type    string      `json:"type"`
	Message userMessage `json:"message"`
}

type userMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Name string `json:"name,omitempty"` // for tool_use
	ID   string `json:"id,omitempty"`   // for tool_use
	// Input is a tool_use call's arguments. Kept as RawMessage because only
	// the tools whose input IS the deliverable are decoded (see exitPlanTool);
	// every other tool's arguments are none of this parser's business.
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result fields. ToolUseID keys the result back to the call that
	// produced it, which is the only way to tell a delegated subagent's output
	// from any other tool's. Content is RawMessage because the CLI writes it
	// two ways — a bare JSON string, or an array of text blocks — and both
	// shapes appear in real transcripts (see toolResultText).
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// exitPlanTool is the tool a plan-mode turn ends on. Its input carries the
// plan, and the turn produces no assistant text after it — so a parser that
// reads only block.Name discards the entire deliverable and leaves the
// tool-call preambles behind as if they were the answer.
const exitPlanTool = "ExitPlanMode"

// delegationTools are the names the CLI gives the tool that runs a subagent.
// A turn that delegates emits one line of narration announcing the delegation
// and then nothing: the deliverable is produced inside the subagent and comes
// back as a tool_result, never as a parent assistant message. Reading only
// assistant events therefore ends the turn holding the preamble.
//
// Two names, because it differs across releases and both are in the field. An
// unrecognised name is not silently wrong: stepPlan's structural floor still
// refuses the narration that a missed delegation leaves behind.
var delegationTools = map[string]bool{"Task": true, "Agent": true}

// subtypeMaxBudget is the result-event subtype the CLI reports when
// --max-budget-usd stopped the run.
const subtypeMaxBudget = "error_max_budget_usd"

type systemEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
}

type assistantEvent struct {
	Type    string `json:"type"`
	Message struct {
		ID      string         `json:"id"`
		Content []contentBlock `json:"content"`
	} `json:"message"`
	SessionID string `json:"session_id,omitempty"`
}

// resultEvent_ — trailing underscore avoids shadowing the package-level
// `result` symbol typo. Only used for unmarshaling.
type resultEvent_ struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype,omitempty"`
	IsError      bool    `json:"is_error,omitempty"`
	DurationMs   int64   `json:"duration_ms,omitempty"`
	NumTurns     int     `json:"num_turns,omitempty"`
	Result       string  `json:"result,omitempty"`
	SessionID    string  `json:"session_id,omitempty"`
	TotalCostUSD float64 `json:"total_cost_usd,omitempty"`
}

// spawnFunc / cmdHandle let tests stub out exec without an interface heap.

type spawnFunc func(ctx context.Context, name string, args ...string) cmdHandle

type cmdHandle interface {
	SetDir(dir string)
	StdinPipe() (io.WriteCloser, error)
	StdoutPipe() (io.ReadCloser, error)
	StderrPipe() (io.ReadCloser, error)
	Start() error
	Wait() error
}

func (c *Client) spawnCmd(ctx context.Context, name string, args ...string) cmdHandle {
	if c.spawn != nil {
		return c.spawn(ctx, name, args...)
	}
	return &execCmd{cmd: exec.CommandContext(ctx, name, args...)}
}

// execCmd adapts *exec.Cmd to cmdHandle.
type execCmd struct{ cmd *exec.Cmd }

func (e *execCmd) SetDir(dir string)                  { e.cmd.Dir = dir }
func (e *execCmd) StdinPipe() (io.WriteCloser, error) { return e.cmd.StdinPipe() }
func (e *execCmd) StdoutPipe() (io.ReadCloser, error) { return e.cmd.StdoutPipe() }
func (e *execCmd) StderrPipe() (io.ReadCloser, error) { return e.cmd.StderrPipe() }
func (e *execCmd) Start() error                       { return e.cmd.Start() }
func (e *execCmd) Wait() error                        { return e.cmd.Wait() }
