package claude

import (
	"context"
	"encoding/json"
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

func TestRun_MaxCostUSDBecomesMaxBudgetFlag(t *testing.T) {
	var capturedArgs []string
	c := &Client{
		Binary: "claude",
		spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
			capturedArgs = args
			return &fakeCmd{stdoutStream: successStream}
		},
	}
	_, _ = c.Run(context.Background(), flow.AgentRequest{Prompt: "go", MaxCostUSD: 2.5})

	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "--max-budget-usd 2.5") {
		t.Errorf("args missing --max-budget-usd 2.5; full: %s", joined)
	}
}

// A cap of 20.005 must reach the CLI intact: %.2f would round it up to 20.01
// and hand the turn more than the step was granted.
func TestRun_MaxCostUSDIsNotRounded(t *testing.T) {
	var capturedArgs []string
	c := &Client{
		Binary: "claude",
		spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
			capturedArgs = args
			return &fakeCmd{stdoutStream: successStream}
		},
	}
	_, _ = c.Run(context.Background(), flow.AgentRequest{Prompt: "go", MaxCostUSD: 20.005})

	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "--max-budget-usd 20.005") {
		t.Errorf("args missing --max-budget-usd 20.005; full: %s", joined)
	}
}

func TestRun_NoMaxCostUSDOmitsMaxBudgetFlag(t *testing.T) {
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
	if strings.Contains(joined, "--max-budget-usd") {
		t.Errorf("unexpected --max-budget-usd in args: %s", joined)
	}
}

// The budget stop is a clean end-of-run: it reports its own failure kind AND
// the cost of the turn. Losing the cost is the regression that matters — the
// next dispatch would re-run the same turn against a meter that never moved.
func TestRun_MaxBudgetStopIsCostCapAndStillBills(t *testing.T) {
	stream := `{"type":"system","subtype":"init","session_id":"s"}
{"type":"result","subtype":"error_max_budget_usd","is_error":true,"session_id":"s","total_cost_usd":21.868663,"duration_ms":100}
`
	fc := &fakeCmd{stdoutStream: stream}
	c := clientWith(fc)

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go", MaxCostUSD: 20})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Failure == nil || resp.Failure.Kind != flow.FailureCostCap {
		t.Fatalf("Failure = %+v, want kind=%s", resp.Failure, flow.FailureCostCap)
	}
	if resp.Failure.Kind != "cost-cap" {
		t.Errorf("FailureCostCap = %q, want the wire string cost-cap", resp.Failure.Kind)
	}
	if resp.CostUSD != 21.868663 {
		t.Errorf("CostUSD = %v, want 21.868663 (the stopped turn still bills)", resp.CostUSD)
	}
	if resp.SessionID != "s" {
		t.Errorf("SessionID = %q, want s", resp.SessionID)
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

// delegationStream is the shape a turn that hands its work to a subagent
// actually has: one line of narration announcing the delegation, the Task
// tool_use, the subagent's deliverable coming back as a top-level user event
// carrying a tool_result, and a result event with an EMPTY result because the
// parent never spoke again.
const delegationStream = `{"type":"system","session_id":"sess-d"}
{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Now let me write the final plan."},{"type":"tool_use","id":"toolu_01","name":"Task","input":{"prompt":"design it"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"text","text":"## Plan\n\nCapture the subagent's deliverable, not the preamble."}]}]}}
{"type":"result","session_id":"sess-d","result":"","total_cost_usd":2.32,"duration_ms":9000}
`

// The reported bug, as a test: the plan was produced inside the subagent, was
// charged for, and was discarded — leaving the delegation preamble as the
// turn's answer.
func TestRun_DelegatedDeliverableIsTheTurnsText(t *testing.T) {
	c := clientWith(&fakeCmd{stdoutStream: delegationStream})

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "plan it"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(resp.LastText, "Capture the subagent's deliverable") {
		t.Errorf("LastText = %q, want the subagent's plan", resp.LastText)
	}
	if strings.Contains(resp.LastText, "Now let me write the final plan") {
		t.Errorf("LastText carries the delegation preamble: %q", resp.LastText)
	}
	if resp.PlanSubmitted {
		t.Error("PlanSubmitted = true with no ExitPlanMode call")
	}
}

// Both names the CLI gives the delegation tool are in the field, and content
// arrives as a bare string as well as an array of blocks. Every combination
// must reach the same place.
func TestRun_DelegationToolNamesAndContentShapes(t *testing.T) {
	blocks := `[{"type":"text","text":"## Plan\n\nThe deliverable."}]`
	bare := `"## Plan\n\nThe deliverable."`
	for _, tc := range []struct {
		name    string
		tool    string
		content string
	}{
		{"Task/blocks", "Task", blocks},
		{"Task/bare-string", "Task", bare},
		{"Agent/blocks", "Agent", blocks},
		{"Agent/bare-string", "Agent", bare},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Delegating now."},{"type":"tool_use","id":"tu9","name":"` + tc.tool + `","input":{}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu9","content":` + tc.content + `}]}}
{"type":"result","session_id":"s","result":"","total_cost_usd":0.1,"duration_ms":10}
`
			c := clientWith(&fakeCmd{stdoutStream: stream})
			resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !strings.Contains(resp.LastText, "The deliverable.") {
				t.Errorf("LastText = %q, want the subagent's output", resp.LastText)
			}
		})
	}
}

// An errored subagent produced no deliverable. Its tool_result is diagnostic
// text, and publishing that under the plan's name is the same defect wearing a
// different coat — so it is ignored, and the narration it leaves behind is
// what stepPlan's structural floor then refuses.
func TestRun_ErroredDelegationResultIsIgnored(t *testing.T) {
	stream := `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Delegating now."},{"type":"tool_use","id":"tu9","name":"Task","input":{}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu9","is_error":true,"content":"Error: subagent exceeded its turn limit"}]}}
{"type":"result","session_id":"s","result":"","total_cost_usd":0.1,"duration_ms":10}
`
	c := clientWith(&fakeCmd{stdoutStream: stream})

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.LastText != "Delegating now." {
		t.Errorf("LastText = %q, want the narration — an errored delegation carries no deliverable", resp.LastText)
	}
}

// A tool_result belongs to the turn only when the turn delegated. Every other
// tool's output is the tool's, not the agent's: a Read's file contents must
// never become the answer.
func TestRun_NonDelegationToolResultIsIgnored(t *testing.T) {
	stream := `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Reading the file."},{"type":"tool_use","id":"tu1","name":"Read","input":{}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"the entire contents of some file"}]}}
{"type":"result","session_id":"s","result":"","total_cost_usd":0.1,"duration_ms":10}
`
	c := clientWith(&fakeCmd{stdoutStream: stream})

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.LastText != "Reading the file." {
		t.Errorf("LastText = %q, want the assistant text — a Read result is not the turn's answer", resp.LastText)
	}
}

// "Last text the turn produced" is exactly that: a parent that speaks after
// its subagent returns has the final word, unchanged from before.
func TestRun_ParentTextAfterDelegationWins(t *testing.T) {
	stream := `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Delegating now."},{"type":"tool_use","id":"tu9","name":"Task","input":{}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu9","content":"## Draft\n\nthe subagent's draft"}]}}
{"type":"assistant","message":{"id":"m2","content":[{"type":"text","text":"## Plan\n\nthe parent's own final answer"}]}}
{"type":"result","session_id":"s","result":"","total_cost_usd":0.1,"duration_ms":10}
`
	c := clientWith(&fakeCmd{stdoutStream: stream})

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(resp.LastText, "the parent's own final answer") {
		t.Errorf("LastText = %q, want the parent's later text", resp.LastText)
	}
}

// Precedence is unchanged: a real final message still outranks anything the
// stream carried on the way there.
func TestRun_ResultEventStillWinsOverADelegatedResult(t *testing.T) {
	stream := `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Delegating now."},{"type":"tool_use","id":"tu9","name":"Task","input":{}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu9","content":"## Draft\n\nthe subagent's draft"}]}}
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
}

// A submitted plan still outranks a delegated result: a turn that delegated
// and THEN submitted through the plan tool has said which one is the plan.
func TestRun_SubmittedPlanStillWinsOverADelegatedResult(t *testing.T) {
	stream := `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Delegating now."},{"type":"tool_use","id":"tu9","name":"Task","input":{}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu9","content":"## Draft\n\nthe subagent's draft"}]}}
{"type":"assistant","message":{"id":"m2","content":[{"type":"tool_use","id":"tu10","name":"ExitPlanMode","input":{"plan":"## Plan\n\nthe submitted plan"}}]}}
{"type":"result","session_id":"s","result":"","total_cost_usd":0.1,"duration_ms":10}
`
	c := clientWith(&fakeCmd{stdoutStream: stream})

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(resp.LastText, "the submitted plan") {
		t.Errorf("LastText = %q, want the submitted plan", resp.LastText)
	}
}

// Two user events carry nothing for this parser: a tool_result keyed to a call
// this turn never made, and the prompt echo, whose content is a bare string
// where a block array would be. Neither may error, and neither may become the
// turn's text.
func TestParseStream_UnrelatedUserEventsIgnored(t *testing.T) {
	stream := strings.NewReader(
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Working."}]}}` + "\n" +
			`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"unknown-id","content":"stray output"}]}}` + "\n" +
			`{"type":"user","message":{"role":"user","content":"the echoed prompt"}}` + "\n" +
			`{"type":"result","session_id":"s","result":"","total_cost_usd":0.1,"duration_ms":10}` + "\n",
	)

	resp, err := parseStream(stream)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if resp.LastText != "Working." {
		t.Errorf("LastText = %q, want the assistant text", resp.LastText)
	}
}

// A turn that fans out to several subagents keeps the LAST one's output, by the
// same rule that governs assistant text. Nothing else is available to choose
// between them: the parser cannot know which subagent was asked for the
// deliverable, and the turn's own ordering is the only evidence there is.
func TestRun_LastDelegationWinsAmongSeveral(t *testing.T) {
	stream := `{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"tu1","name":"Task","input":{}},{"type":"tool_use","id":"tu2","name":"Task","input":{}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"## Research\n\nwhat the first subagent found"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu2","content":"## Plan\n\nwhat the second subagent wrote"}]}}
{"type":"result","session_id":"s","result":"","total_cost_usd":0.1,"duration_ms":10}
`
	c := clientWith(&fakeCmd{stdoutStream: stream})

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(resp.LastText, "what the second subagent wrote") {
		t.Errorf("LastText = %q, want the last delegation's output", resp.LastText)
	}
}

// A subagent that returned nothing said nothing, and blanking the turn's text
// on its way past would be worse than ignoring it: the text it would overwrite
// is the parent's own, which is at least something the turn produced. The same
// guard is what keeps an empty result from turning a real answer into the
// "agent returned an empty plan" refusal one layer up.
func TestRun_EmptyDelegationResultDoesNotClobberTheTurnsText(t *testing.T) {
	stream := `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"## Plan\n\nthe parent wrote this, then checked it."},{"type":"tool_use","id":"tu1","name":"Task","input":{}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"   \n  "}]}}
{"type":"result","session_id":"s","result":"","total_cost_usd":0.1,"duration_ms":10}
`
	c := clientWith(&fakeCmd{stdoutStream: stream})

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(resp.LastText, "the parent wrote this") {
		t.Errorf("LastText = %q, want the parent's text — an empty delegation result carries nothing", resp.LastText)
	}
}

// The shapes a tool_result's content arrives in. Both recognised ones are read;
// everything else yields "" and is dropped, which is the whole point — the
// alternative to recognising a shape is guessing at it, and a guess here puts
// something arbitrary under the artifact's name.
func TestToolResultText_ContentShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"bare string", `"the deliverable"`, "the deliverable"},
		{"one text block", `[{"type":"text","text":"the deliverable"}]`, "the deliverable"},
		{"text blocks joined", `[{"type":"text","text":"first"},{"type":"text","text":"second"}]`, "first\nsecond"},
		{"non-text blocks skipped", `[{"type":"image","source":"..."},{"type":"text","text":"the deliverable"}]`, "the deliverable"},
		{"empty block array", `[]`, ""},
		{"absent", ``, ""},
		{"null", `null`, ""},
		// An object and a number are shapes this parser does not know. Rendering
		// them as their raw JSON would be a deliverable made of punctuation.
		{"object", `{"result":"the deliverable"}`, ""},
		{"number", `17`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolResultText(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("toolResultText(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
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

// ---------------------------------------------------------------------------
// Transient field on AgentFailure
// ---------------------------------------------------------------------------

// TestRun_NoResultFailureIsTransient verifies that when the process emits no
// result event, the failure is marked transient (infrastructure, not agent).
func TestRun_NoResultFailureIsTransient(t *testing.T) {
	stream := `{"type":"assistant","message":{"id":"m","content":[{"type":"text","text":"garbage"}]}}
`
	fc := &fakeCmd{stdoutStream: stream, stderrStream: "oops", waitErr: errors.New("exit 1")}
	c := clientWith(fc)

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run err = %v, want nil", err)
	}
	if resp.Failure == nil || resp.Failure.Kind != "no-result" {
		t.Fatalf("Failure = %+v, want kind=no-result", resp.Failure)
	}
	if !resp.Failure.Transient {
		t.Errorf("Failure.Transient = false, want true for no-result failures")
	}
}

// TestRun_ExitErrorFailureIsTransient verifies that when the process exits
// with an error and produces no usable output (no SessionID, no LastText),
// the failure is marked transient.
func TestRun_ExitErrorFailureIsTransient(t *testing.T) {
	// The stream must parse without error (a result event is present) but
	// carry no usable output: empty session_id and empty result text.
	stream := `{"type":"result","subtype":"error","is_error":true,"session_id":"","result":"","total_cost_usd":0,"duration_ms":0}
`
	fc := &fakeCmd{stdoutStream: stream, stderrStream: "segfault", waitErr: errors.New("exit 139")}
	c := clientWith(fc)

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run err = %v, want nil", err)
	}
	if resp.Failure == nil || resp.Failure.Kind != "exit-error" {
		t.Fatalf("Failure = %+v, want kind=exit-error", resp.Failure)
	}
	if !resp.Failure.Transient {
		t.Errorf("Failure.Transient = false, want true for exit-error failures")
	}
}

// TestRun_CancelledFailureIsNotTransient verifies that a context cancellation
// produces a non-transient failure (the caller cancelled, not infrastructure).
func TestRun_CancelledFailureIsNotTransient(t *testing.T) {
	fc := &fakeCmd{stdoutStream: successStream}
	c := clientWith(fc)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run

	resp, err := c.Run(ctx, flow.AgentRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Failure == nil || resp.Failure.Kind != "cancelled" {
		t.Fatalf("Failure = %+v, want kind=cancelled", resp.Failure)
	}
	if resp.Failure.Transient {
		t.Errorf("Failure.Transient = true, want false for cancelled failures")
	}
}
