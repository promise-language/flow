package claude

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// fakeCmd implements cmdHandle for unit tests — emits canned stdout, captures
// stdin, and returns a configurable Wait error.
type fakeCmd struct {
	stdoutStream string
	stderrStream string
	waitErr      error

	stdinBuf strings.Builder
	dir      string
	stdinW   *closableBuf
	stdoutR  io.ReadCloser
	stderrR  io.ReadCloser
}

type closableBuf struct {
	*strings.Builder
	target *strings.Builder
	closed bool
}

func (c *closableBuf) Write(p []byte) (int, error) {
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	return c.target.Write(p)
}

func (c *closableBuf) Close() error {
	c.closed = true
	return nil
}

func (f *fakeCmd) SetDir(dir string) { f.dir = dir }

func (f *fakeCmd) StdinPipe() (io.WriteCloser, error) {
	f.stdinW = &closableBuf{target: &f.stdinBuf}
	return f.stdinW, nil
}

func (f *fakeCmd) StdoutPipe() (io.ReadCloser, error) {
	f.stdoutR = io.NopCloser(strings.NewReader(f.stdoutStream))
	return f.stdoutR, nil
}

func (f *fakeCmd) StderrPipe() (io.ReadCloser, error) {
	f.stderrR = io.NopCloser(strings.NewReader(f.stderrStream))
	return f.stderrR, nil
}

func (f *fakeCmd) Start() error { return nil }
func (f *fakeCmd) Wait() error  { return f.waitErr }

const successStream = `{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-opus-4-7"}
{"type":"assistant","message":{"id":"msg_1","content":[{"type":"text","text":"Hello"}]},"session_id":"sess-1"}
{"type":"assistant","message":{"id":"msg_2","content":[{"type":"tool_use","id":"tu1","name":"Read"}]},"session_id":"sess-1"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}}
{"type":"assistant","message":{"id":"msg_3","content":[{"type":"text","text":"World"}]}}
{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"result":"Hello World","session_id":"sess-1","total_cost_usd":0.42}
`

func clientWith(cmd *fakeCmd) *Client {
	return &Client{
		Binary: "claude",
		spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
			return cmd
		},
	}
}

func TestRun_Success(t *testing.T) {
	fc := &fakeCmd{stdoutStream: successStream}
	c := clientWith(fc)

	resp, err := c.Run(context.Background(), flow.AgentRequest{
		Prompt: "hi",
		Model:  "claude-opus-4-7",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Failure != nil {
		t.Fatalf("Failure = %+v, want nil", resp.Failure)
	}
	if resp.LastText != "Hello World" {
		t.Errorf("LastText = %q, want \"Hello World\"", resp.LastText)
	}
	if resp.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", resp.SessionID)
	}
	if resp.CostUSD != 0.42 {
		t.Errorf("CostUSD = %v, want 0.42", resp.CostUSD)
	}
	if resp.DurationSeconds != 1.234 {
		t.Errorf("DurationSeconds = %v, want 1.234", resp.DurationSeconds)
	}
	if len(resp.ToolsUsed) != 1 || resp.ToolsUsed[0] != "Read" {
		t.Errorf("ToolsUsed = %v, want [Read]", resp.ToolsUsed)
	}

	// Stdin payload should be a single user event with our prompt.
	if !strings.Contains(fc.stdinBuf.String(), `"text":"hi"`) {
		t.Errorf("stdin missing prompt; got %q", fc.stdinBuf.String())
	}
}

func TestRun_NoResultEventIsFailure(t *testing.T) {
	stream := `{"type":"assistant","message":{"id":"m","content":[{"type":"text","text":"partial"}]}}
`
	fc := &fakeCmd{stdoutStream: stream, stderrStream: "boom"}
	c := clientWith(fc)

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run err = %v, want nil (failure surfaced via Response)", err)
	}
	if resp.Failure == nil || resp.Failure.Kind != "no-result" {
		t.Errorf("Failure = %+v, want kind=no-result", resp.Failure)
	}
	if !strings.Contains(resp.Failure.Message, "result event") && !strings.Contains(resp.Failure.Message, "stderr") {
		t.Errorf("Failure.Message should explain; got %q", resp.Failure.Message)
	}
}

func TestRun_IsErrorReportedAsExitError(t *testing.T) {
	stream := `{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"s","total_cost_usd":0.01,"duration_ms":100}
`
	fc := &fakeCmd{stdoutStream: stream}
	c := clientWith(fc)

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Failure == nil || resp.Failure.Kind != "exit-error" {
		t.Errorf("Failure = %+v, want kind=exit-error", resp.Failure)
	}
	if resp.SessionID != "s" {
		t.Errorf("SessionID = %q, want s (populated even on is_error)", resp.SessionID)
	}
}

func TestRun_CancelledContext(t *testing.T) {
	fc := &fakeCmd{stdoutStream: successStream}
	c := clientWith(fc)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run

	resp, err := c.Run(ctx, flow.AgentRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Failure == nil || resp.Failure.Kind != "cancelled" {
		t.Errorf("Failure = %+v, want kind=cancelled", resp.Failure)
	}
}

func TestRun_WaitErrorWithUsableOutputIsStillSuccess(t *testing.T) {
	// claude exited non-zero but emitted a complete result event; we trust
	// the result event and return the resp without overriding to exit-error.
	fc := &fakeCmd{stdoutStream: successStream, waitErr: errors.New("exit 1")}
	c := clientWith(fc)

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Failure != nil {
		t.Errorf("Failure = %+v, want nil (result event arrived)", resp.Failure)
	}
	if resp.LastText != "Hello World" {
		t.Errorf("LastText = %q, want Hello World", resp.LastText)
	}
}

func TestRun_ArgsIncludeStreamFlags(t *testing.T) {
	var capturedArgs []string
	c := &Client{
		Binary: "claude",
		spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
			capturedArgs = args
			return &fakeCmd{stdoutStream: successStream}
		},
	}
	_, _ = c.Run(context.Background(), flow.AgentRequest{
		Prompt:          "go",
		Model:           "claude-opus-4-7",
		PermissionMode:  "acceptEdits",
		ResumeSessionID: "prev-sess",
	})

	joined := strings.Join(capturedArgs, " ")
	for _, want := range []string{
		"--print",
		"--input-format stream-json",
		"--output-format stream-json",
		"--model claude-opus-4-7",
		"--permission-mode acceptEdits",
		"--resume prev-sess",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q; full: %s", want, joined)
		}
	}
}

func TestRun_WorktreeSetsDir(t *testing.T) {
	var captured *fakeCmd
	c := &Client{
		Binary: "claude",
		spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
			captured = &fakeCmd{stdoutStream: successStream}
			return captured
		},
	}
	_, _ = c.Run(context.Background(), flow.AgentRequest{Prompt: "x", Worktree: "/work/here"})
	if captured.dir != "/work/here" {
		t.Errorf("dir = %q, want /work/here", captured.dir)
	}
}
