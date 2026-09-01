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
		"--verbose",
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

// ---------------------------------------------------------------------------
// Tool-policy fields: AllowedTools / DisallowedTools → CLI flags.
// ---------------------------------------------------------------------------

func TestRun_AllowedToolsArgs(t *testing.T) {
	var capturedArgs []string
	c := &Client{
		Binary:       "claude",
		AllowedTools: []string{"Read", "Bash(git *)"},
		spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
			capturedArgs = args
			return &fakeCmd{stdoutStream: successStream}
		},
	}
	_, _ = c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})

	joined := strings.Join(capturedArgs, " ")
	for _, want := range []string{
		"--allowed-tools Read",
		"--allowed-tools Bash(git *)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q; full: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--disallowed-tools") {
		t.Errorf("unexpected --disallowed-tools in args: %s", joined)
	}
}

func TestRun_DisallowedToolsArgs(t *testing.T) {
	var capturedArgs []string
	c := &Client{
		Binary:          "claude",
		DisallowedTools: []string{"Write", "Edit"},
		spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
			capturedArgs = args
			return &fakeCmd{stdoutStream: successStream}
		},
	}
	_, _ = c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})

	joined := strings.Join(capturedArgs, " ")
	for _, want := range []string{
		"--disallowed-tools Write",
		"--disallowed-tools Edit",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q; full: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--allowed-tools") {
		t.Errorf("unexpected --allowed-tools in args: %s", joined)
	}
}

func TestRun_BothToolPolicies(t *testing.T) {
	var capturedArgs []string
	c := &Client{
		Binary:          "claude",
		AllowedTools:    []string{"Read"},
		DisallowedTools: []string{"Write"},
		spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
			capturedArgs = args
			return &fakeCmd{stdoutStream: successStream}
		},
	}
	_, _ = c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})

	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "--allowed-tools Read") {
		t.Errorf("args missing --allowed-tools Read; full: %s", joined)
	}
	if !strings.Contains(joined, "--disallowed-tools Write") {
		t.Errorf("args missing --disallowed-tools Write; full: %s", joined)
	}

	// Allowed flags must appear before disallowed flags.
	allowedIdx := strings.Index(joined, "--allowed-tools")
	disallowedIdx := strings.Index(joined, "--disallowed-tools")
	if allowedIdx >= disallowedIdx {
		t.Errorf("--allowed-tools (at %d) should precede --disallowed-tools (at %d)", allowedIdx, disallowedIdx)
	}
}

func TestRun_EmptyPoliciesNoFlags(t *testing.T) {
	var capturedArgs []string
	c := &Client{
		Binary: "claude",
		spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
			capturedArgs = args
			return &fakeCmd{stdoutStream: successStream}
		},
	}
	_, _ = c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})

	joined := strings.Join(capturedArgs, " ")
	if strings.Contains(joined, "--allowed-tools") {
		t.Errorf("unexpected --allowed-tools in args: %s", joined)
	}
	if strings.Contains(joined, "--disallowed-tools") {
		t.Errorf("unexpected --disallowed-tools in args: %s", joined)
	}
}

func TestRun_ToolPoliciesPrecedeExtraArgs(t *testing.T) {
	var capturedArgs []string
	c := &Client{
		Binary:          "claude",
		AllowedTools:    []string{"Read"},
		DisallowedTools: []string{"Write"},
		ExtraArgs:       []string{"--extra", "val"},
		spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
			capturedArgs = args
			return &fakeCmd{stdoutStream: successStream}
		},
	}
	_, _ = c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})

	joined := strings.Join(capturedArgs, " ")
	disallowedIdx := strings.Index(joined, "--disallowed-tools")
	extraIdx := strings.Index(joined, "--extra")
	if disallowedIdx < 0 || extraIdx < 0 {
		t.Fatalf("expected both --disallowed-tools and --extra in args: %s", joined)
	}
	if disallowedIdx >= extraIdx {
		t.Errorf("tool policy flags (at %d) must precede ExtraArgs (at %d); full: %s",
			disallowedIdx, extraIdx, joined)
	}
}

// ---------------------------------------------------------------------------
// Plan-mode turns: the deliverable is a tool call's input, not assistant text.
// ---------------------------------------------------------------------------

// planStream is the shape a headless plan-mode turn actually has: a preamble
// before each tool call, then ExitPlanMode carrying the plan, then a result
// event with an EMPTY result because the turn did not end in assistant text.
const planStream = `{"type":"system","session_id":"sess-p"}
{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Let me read the key files first."},{"type":"tool_use","name":"Read","input":{"file_path":"/x"}}]}}
{"type":"assistant","message":{"id":"m2","content":[{"type":"text","text":"Now let me write the plan."},{"type":"tool_use","name":"ExitPlanMode","input":{"plan":"## Plan\n\n1. Capture the tool input.\n2. Stop joining every text block."}}]}}
{"type":"result","session_id":"sess-p","result":"","total_cost_usd":0.5,"duration_ms":1000}
`

// The reported bug, as a test: the plan reached the parser inside the
// ExitPlanMode call and was discarded, leaving the preambles as the answer.
func TestRun_PlanModeCapturesTheSubmittedPlan(t *testing.T) {
	c := clientWith(&fakeCmd{stdoutStream: planStream})

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "plan it", PermissionMode: "plan"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !resp.PlanSubmitted {
		t.Error("PlanSubmitted = false, want true — the turn called ExitPlanMode")
	}
	if !strings.Contains(resp.PlanText, "Capture the tool input") {
		t.Errorf("PlanText = %q, want the submitted plan", resp.PlanText)
	}
	// The whole point: LastText must be the plan, never the narration that
	// preceded the tool calls.
	if !strings.Contains(resp.LastText, "Capture the tool input") {
		t.Errorf("LastText = %q, want the plan", resp.LastText)
	}
	if strings.Contains(resp.LastText, "Let me read the key files") {
		t.Errorf("LastText carries tool-call narration: %q", resp.LastText)
	}
}

// PlanSubmitted must be true even when the input does not decode, because
// "planned and we lost it" is the case the plan step refuses on. Reporting it
// as "never planned" would let the narration resolve as the artifact.
func TestRun_PlanSubmittedEvenWhenInputUndecodable(t *testing.T) {
	stream := `{"type":"assistant","message":{"id":"m","content":[{"type":"text","text":"Now let me write the plan."},{"type":"tool_use","name":"ExitPlanMode","input":"not-an-object"}]}}
{"type":"result","session_id":"s","result":"","total_cost_usd":0.1,"duration_ms":10}
`
	c := clientWith(&fakeCmd{stdoutStream: stream})

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "plan it"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !resp.PlanSubmitted {
		t.Error("PlanSubmitted = false, want true — the call happened regardless of its payload")
	}
	if resp.PlanText != "" {
		t.Errorf("PlanText = %q, want empty — nothing decodable was carried", resp.PlanText)
	}
}

// LastText is the LAST text block, not every block joined. The old behaviour
// concatenated them, which is what turned a series of preambles into something
// that looked like content and passed every emptiness check.
func TestRun_LastTextIsTheLastBlockNotTheConcatenation(t *testing.T) {
	stream := `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"First preamble."},{"type":"tool_use","name":"Read","input":{}}]}}
{"type":"assistant","message":{"id":"m2","content":[{"type":"text","text":"Second preamble."},{"type":"tool_use","name":"Grep","input":{}}]}}
{"type":"result","session_id":"s","result":"","total_cost_usd":0.1,"duration_ms":10}
`
	c := clientWith(&fakeCmd{stdoutStream: stream})

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.LastText != "Second preamble." {
		t.Errorf("LastText = %q, want only the last block", resp.LastText)
	}
	if resp.PlanSubmitted {
		t.Error("PlanSubmitted = true with no ExitPlanMode call")
	}
}

// --verbose may introduce event types the parser does not know about (e.g.
// tool_result echoes, progress events). They must be silently ignored.
func TestParseStream_UnknownEventTypesIgnored(t *testing.T) {
	stream := strings.NewReader(
		`{"type":"system","session_id":"sess-u"}` + "\n" +
			`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Hello"}]}}` + "\n" +
			`{"type":"tool_result","content":"some echoed tool output"}` + "\n" +
			`{"type":"progress","percent":50}` + "\n" +
			`{"type":"result","session_id":"sess-u","result":"done","total_cost_usd":0.1,"duration_ms":10}` + "\n",
	)

	resp, err := parseStream(stream)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if resp.SessionID != "sess-u" {
		t.Errorf("SessionID = %q, want sess-u", resp.SessionID)
	}
	if resp.LastText != "done" {
		t.Errorf("LastText = %q, want done", resp.LastText)
	}
}

// --verbose can emit non-JSON diagnostic lines (timestamps, debug info).
// The parser must skip them without error rather than aborting the stream.
func TestParseStream_NonJSONLinesSkipped(t *testing.T) {
	stream := strings.NewReader(
		"[2026-08-31 12:00:00] claude: loading session\n" +
			`{"type":"system","session_id":"sess-n"}` + "\n" +
			"VERBOSE: model turn started\n" +
			`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Hi"}]}}` + "\n" +
			`{"type":"result","session_id":"sess-n","result":"done","total_cost_usd":0.1,"duration_ms":10}` + "\n",
	)

	resp, err := parseStream(stream)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if resp.SessionID != "sess-n" {
		t.Errorf("SessionID = %q, want sess-n", resp.SessionID)
	}
	if resp.LastText != "done" {
		t.Errorf("LastText = %q, want done", resp.LastText)
	}
}

// Blank lines can appear between events (e.g. verbose separators). The parser
// must tolerate them without treating them as errors or truncating the stream.
func TestParseStream_BlankLinesSkipped(t *testing.T) {
	stream := strings.NewReader(
		"\n" +
			`{"type":"system","session_id":"sess-b"}` + "\n" +
			"\n" +
			"\n" +
			`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Hi"}]}}` + "\n" +
			"\n" +
			`{"type":"result","session_id":"sess-b","result":"ok","total_cost_usd":0.1,"duration_ms":10}` + "\n",
	)

	resp, err := parseStream(stream)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if resp.SessionID != "sess-b" {
		t.Errorf("SessionID = %q, want sess-b", resp.SessionID)
	}
	if resp.LastText != "ok" {
		t.Errorf("LastText = %q, want ok", resp.LastText)
	}
}

// A turn that ends in assistant text is unaffected: the result event still
// wins, so ordinary (non-plan) steps keep the behaviour they had.
func TestRun_ResultEventStillWinsOverAPlan(t *testing.T) {
	stream := `{"type":"assistant","message":{"id":"m","content":[{"type":"tool_use","name":"ExitPlanMode","input":{"plan":"the plan"}}]}}
{"type":"result","session_id":"s","result":"the final answer","total_cost_usd":0.1,"duration_ms":10}
`
	c := clientWith(&fakeCmd{stdoutStream: stream})

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.LastText != "the final answer" {
		t.Errorf("LastText = %q, want the result event's text", resp.LastText)
	}
	if resp.PlanText != "the plan" {
		t.Errorf("PlanText = %q, want it captured alongside", resp.PlanText)
	}
}
