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

	"github.com/promise-language/flow"
)

// Client is the flow.Agent backed by the claude CLI. Binary defaults to
// "claude" if empty (looked up on PATH). ExtraArgs is appended after every
// invocation's argv — useful for testing or for forwarding custom flags.
type Client struct {
	Binary    string
	ExtraArgs []string

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
				Kind:    "no-result",
				Message: combineDiagnostic(parseErr, waitErr, stderrBytes),
			},
		}, nil
	}
	if waitErr != nil && resp.SessionID == "" && resp.LastText == "" {
		// No usable output and the process errored — treat as exit-error.
		return &flow.AgentResponse{
			Failure: &flow.AgentFailure{
				Kind:    "exit-error",
				Message: combineDiagnostic(nil, waitErr, stderrBytes),
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
	var (
		textBuf      []byte
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
					if len(textBuf) > 0 {
						textBuf = append(textBuf, '\n')
					}
					textBuf = append(textBuf, block.Text...)
				case "tool_use":
					if _, dup := toolSeen[block.Name]; !dup && block.Name != "" {
						toolSeen[block.Name] = struct{}{}
						resp.ToolsUsed = append(resp.ToolsUsed, block.Name)
					}
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
			if ev.Result != "" {
				resp.LastText = ev.Result
			} else if len(textBuf) > 0 {
				resp.LastText = string(textBuf)
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
		resp.Failure = &flow.AgentFailure{
			Kind:    "exit-error",
			Message: "claude reported is_error (subtype=" + errorSubtype + ")",
		}
	}
	if resp.LastText == "" && len(textBuf) > 0 {
		resp.LastText = string(textBuf)
	}
	return resp, nil
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
}

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

func (e *execCmd) SetDir(dir string)                 { e.cmd.Dir = dir }
func (e *execCmd) StdinPipe() (io.WriteCloser, error) { return e.cmd.StdinPipe() }
func (e *execCmd) StdoutPipe() (io.ReadCloser, error) { return e.cmd.StdoutPipe() }
func (e *execCmd) StderrPipe() (io.ReadCloser, error) { return e.cmd.StderrPipe() }
func (e *execCmd) Start() error                       { return e.cmd.Start() }
func (e *execCmd) Wait() error                        { return e.cmd.Wait() }
