package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

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

// A handle the substrate no longer has is DROPPED, and the same prompt is sent
// again without it.
//
// `claude --resume <gone>` fails before the turn starts and fails identically
// every time it is asked, so a dead handle re-offered is a step that can never
// run: the failure is transient, the re-dispatch does not count, and the handle
// stored with the resolution outlives the process that minted it. The handle is
// offered and never depended on (docs/resolution.md § The agent session) — this
// is that rule at the substrate.
func TestRun_DeadHandleIsDroppedAndThePromptResent(t *testing.T) {
	var argv [][]string
	c := &Client{
		Binary: "claude",
		spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
			argv = append(argv, args)
			if len(argv) == 1 {
				// What a declined resume looks like: no stream at all, and a
				// non-zero exit.
				return &fakeCmd{stderrStream: "No conversation found with session ID: gone", waitErr: errors.New("exit 1")}
			}
			return &fakeCmd{stdoutStream: successStream}
		},
	}

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go", ResumeSessionID: "gone"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Failure != nil {
		t.Fatalf("Failure = %+v, want the second turn's success — a declined handle must not fail the turn", resp.Failure)
	}
	if resp.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want the session the retry opened", resp.SessionID)
	}
	if len(argv) != 2 {
		t.Fatalf("spawned %d times, want 2 — one offering the handle, one without it", len(argv))
	}
	if !strings.Contains(strings.Join(argv[0], " "), "--resume gone") {
		t.Errorf("first spawn did not offer the handle: %v", argv[0])
	}
	if strings.Contains(strings.Join(argv[1], " "), "--resume") {
		t.Errorf("the retry offered the handle again: %v", argv[1])
	}
}

// The fallback is for a turn that NEVER STARTED, and the sharp case is the one
// next to it: a turn killed in the middle produces no result event either, and
// it must not be re-sent. It resumed fine — it said which conversation it was in
// before it died — so re-sending it would run the prompt a second time and
// abandon a conversation that is still there, which is § Nothing is bought twice
// paying twice.
func TestRun_ATurnKilledInTheMiddleIsNotResent(t *testing.T) {
	// An init event names the session; nothing after it. This is a killed
	// process, not a declined handle.
	const killedMidTurn = `{"type":"system","subtype":"init","session_id":"sess-1"}
{"type":"assistant","message":{"id":"msg_1","content":[{"type":"text","text":"working"}]},"session_id":"sess-1"}
`
	spawns := 0
	c := &Client{
		Binary: "claude",
		spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
			spawns++
			return &fakeCmd{stdoutStream: killedMidTurn, waitErr: errors.New("signal: killed")}
		},
	}

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go", ResumeSessionID: "sess-1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if spawns != 1 {
		t.Errorf("spawned %d times, want 1 — the turn started, so the handle was honoured", spawns)
	}
	if resp.Failure == nil || !resp.Failure.Transient {
		t.Errorf("Failure = %+v, want the transient no-result it has always been", resp.Failure)
	}
	if resp.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want the session the dead turn named — that is what says it started", resp.SessionID)
	}
}

// And a turn that carried NO handle is never resent either: there is nothing to
// drop, so a second spawn would only re-send a prompt at full price.
func TestRun_NoHandleMeansNoRetry(t *testing.T) {
	spawns := 0
	c := &Client{
		Binary: "claude",
		spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
			spawns++
			return &fakeCmd{waitErr: errors.New("exit 1")}
		},
	}

	resp, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if spawns != 1 {
		t.Errorf("spawned %d times, want 1", spawns)
	}
	if resp.Failure == nil {
		t.Fatal("Failure = nil, want the failure reported as before")
	}
}

// The retry drops THE HANDLE AND NOTHING ELSE. It is the same turn, sent again,
// so every other term the caller set still binds — and the cost ceiling is the
// one that matters: a retry that lost `--max-budget-usd` would spend unbounded
// on a step that had been granted a fixed amount, and nothing downstream would
// notice, because the ceiling is enforced by the process that no longer has it.
func TestRun_TheRetryDropsTheHandleAndNothingElse(t *testing.T) {
	var argv [][]string
	c := &Client{
		Binary: "claude",
		spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
			argv = append(argv, args)
			if len(argv) == 1 {
				return &fakeCmd{waitErr: errors.New("exit 1")}
			}
			return &fakeCmd{stdoutStream: successStream}
		},
	}

	if _, err := c.Run(context.Background(), flow.AgentRequest{
		Prompt:          "go",
		ResumeSessionID: "gone",
		Model:           "claude-opus-4-7",
		PermissionMode:  "acceptEdits",
		MaxCostUSD:      1.5,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(argv) != 2 {
		t.Fatalf("spawned %d times, want 2", len(argv))
	}
	retry := strings.Join(argv[1], " ")
	for _, want := range []string{"--model claude-opus-4-7", "--permission-mode acceptEdits", "--max-budget-usd 1.5"} {
		if !strings.Contains(retry, want) {
			t.Errorf("the retry does not carry %q: %v", want, argv[1])
		}
	}
}

// A turn the CALLER cancelled is not retried. The failure is the context's, not
// the substrate's: the handle was never judged, a second spawn would be refused
// the same way, and a run stopping on a deadline must not spend twice on its way
// out.
func TestRun_ACancelledTurnIsNotRetried(t *testing.T) {
	spawns := 0
	c := &Client{
		Binary: "claude",
		spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
			spawns++
			return &fakeCmd{waitErr: errors.New("signal: killed")}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := c.Run(ctx, flow.AgentRequest{Prompt: "go", ResumeSessionID: "sess-1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if spawns != 1 {
		t.Errorf("spawned %d times, want 1 — a cancelled turn says nothing about the handle", spawns)
	}
	if resp.Failure == nil || resp.Failure.Kind != "cancelled" {
		t.Errorf("Failure = %+v, want the cancellation reported as itself", resp.Failure)
	}
}

// WHICH ENDINGS COUNT AS "the handle was declined", one row per kind the
// substrate can end a turn with. The retry exists for a turn that produced no
// evidence of ever starting, and everything either side of that line is a turn
// that DID: re-sending one of those runs the prompt a second time and bills it a
// second time, against a step that has already been charged for the first
// (docs/resolution.md § Nothing is bought twice).
//
// The cost-cap row is the expensive one. A turn the substrate stopped AT THE CAP
// reports the stop and nothing else — no text, and no session id when the result
// event carried none — so it is one predicate clause away from being re-sent
// with the handle dropped, which spends the cap's worth again on a step the
// caller is about to park for exceeding it.
func TestRun_OnlyATurnThatNeverStartedDropsTheHandle(t *testing.T) {
	cases := []struct {
		name       string
		stream     string
		wantSpawns int
	}{
		{
			// is_error with no budget subtype: the substrate reported a fault and
			// named no conversation, which is what a refused `--resume` looks like
			// when the CLI still emits a result event for it.
			name:       "an error with nothing to show for it is resent without the handle",
			stream:     `{"type":"result","subtype":"error_during_execution","is_error":true,"duration_ms":10,"total_cost_usd":0}` + "\n",
			wantSpawns: 2,
		},
		{
			name:       "a turn stopped at the cost cap is not resent",
			stream:     `{"type":"result","subtype":"error_max_budget_usd","is_error":true,"duration_ms":10,"total_cost_usd":1.5}` + "\n",
			wantSpawns: 1,
		},
		{
			// A turn that ran, cost money and simply answered with nothing. There
			// is no failure to read and no fault to attribute to the handle.
			name:       "a clean turn that said nothing is not resent",
			stream:     `{"type":"result","subtype":"success","is_error":false,"result":"","duration_ms":10,"total_cost_usd":0.2}` + "\n",
			wantSpawns: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var argv [][]string
			c := &Client{
				Binary: "claude",
				spawn: func(ctx context.Context, name string, args ...string) cmdHandle {
					argv = append(argv, args)
					if len(argv) == 1 {
						return &fakeCmd{stdoutStream: tc.stream}
					}
					return &fakeCmd{stdoutStream: successStream}
				},
			}

			if _, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "go", ResumeSessionID: "held"}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(argv) != tc.wantSpawns {
				t.Fatalf("spawned %d times, want %d", len(argv), tc.wantSpawns)
			}
			if !strings.Contains(strings.Join(argv[0], " "), "--resume held") {
				t.Errorf("the first spawn did not offer the handle: %v", argv[0])
			}
			if tc.wantSpawns == 2 && strings.Contains(strings.Join(argv[1], " "), "--resume") {
				t.Errorf("the retry offered the handle again: %v", argv[1])
			}
		})
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

// A CANCELLED turn keeps the session it named, and this is the path where that
// matters most: a step killed on its deadline parks, and the dispatch that picks
// it up after a `grant --timeout` has to resume the conversation the dead turn
// opened rather than buy it again (docs/resolution.md § Nothing is bought twice).
//
// The turn below never reaches a result event — the process was killed mid-turn —
// but the init event already said which conversation it was in, which is the
// whole of what the next dispatch needs.
func TestRun_ACancelledTurnKeepsTheSessionItNamed(t *testing.T) {
	const killedMidTurn = `{"type":"system","subtype":"init","session_id":"sess-9"}
{"type":"assistant","message":{"id":"msg_1","content":[{"type":"text","text":"working"}]},"session_id":"sess-9"}
`
	c := clientWith(&fakeCmd{stdoutStream: killedMidTurn, waitErr: errors.New("signal: killed")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := c.Run(ctx, flow.AgentRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Failure == nil || resp.Failure.Kind != "cancelled" {
		t.Fatalf("Failure = %+v, want kind=cancelled", resp.Failure)
	}
	if resp.SessionID != "sess-9" {
		t.Errorf("SessionID = %q, want sess-9 — a turn the clock killed still opened a conversation, and dropping the handle makes the resume pay for it twice", resp.SessionID)
	}
}

// errAfter reads r to exhaustion and then fails, which is what a broken pipe
// looks like to the scanner: some events arrived, the read did not end cleanly.
type errAfter struct {
	r   io.Reader
	err error
}

func (e *errAfter) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		return n, e.err
	}
	return n, err
}

// A scan failure keeps the session the stream already named, for the same reason
// the missing-result path does: the caller tells a declined resume from a turn
// that broke by whether the turn ever said which conversation it was in, and a
// read that failed after the init event is the second of those.
func TestParseStream_AScanFailureKeepsTheSessionItNamed(t *testing.T) {
	stream := &errAfter{
		r:   strings.NewReader(`{"type":"system","subtype":"init","session_id":"sess-scan"}` + "\n"),
		err: errors.New("broken pipe"),
	}

	resp, err := parseStream(stream)
	if err == nil {
		t.Fatal("parseStream returned no error; a failed read is not a usable turn")
	}
	if resp == nil {
		t.Fatal("parseStream returned no response; the session the stream named is what the caller reads to know the turn started")
	}
	if resp.SessionID != "sess-scan" {
		t.Errorf("SessionID = %q, want sess-scan", resp.SessionID)
	}
}

// ---------------------------------------------------------------------------
// The exhausted agent account
//
// The substrate emits a dedicated rate_limit_event before the turn dies: that
// it refused, which window refused, and when that window resets. It is the
// only evidence this condition may be classified from — the turn's own ending
// carries none of the three, and docs/environment.md forbids keying on an
// error taxonomy that varies between versions.
// ---------------------------------------------------------------------------

// rejectedEvent is the substrate's refusal, in the shape it arrives in.
func rejectedEvent(window string, resetsAt int64) string {
	return fmt.Sprintf(
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":%q,"resetsAt":%d}}`,
		window, resetsAt)
}

// The exact shape a classifier keyed on the result's subtype gets wrong: the
// turn ends is_error with subtype "success", which names nothing and is the
// reason the outcome is not the evidence.
func TestRun_AnExhaustedAccountIsClassifiedFromTheRefusalNotTheSubtype(t *testing.T) {
	const resets = 1789153635
	stream := `{"type":"system","subtype":"init","session_id":"s"}
` + rejectedEvent("seven_day", resets) + `
{"type":"result","subtype":"success","is_error":true,"session_id":"s","duration_ms":100}
`
	resp, err := clientWith(&fakeCmd{stdoutStream: stream}).Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Failure == nil || resp.Failure.Kind != flow.FailureAccountExhausted {
		t.Fatalf("Failure = %+v, want kind=%s", resp.Failure, flow.FailureAccountExhausted)
	}
	if !resp.Failure.Transient {
		t.Error("Transient = false: a refusal that never reached an agent must not be billed or counted")
	}
	if resp.Failure.Window != "seven_day" {
		t.Errorf("Window = %q, want seven_day — the window the substrate named", resp.Failure.Window)
	}
	if resp.Failure.ClearsAt == nil {
		t.Fatal("ClearsAt is absent: the instant is the one fact a driver needs and the substrate published it")
	}
	if got := resp.Failure.ClearsAt.Unix(); got != resets {
		t.Errorf("ClearsAt = %d, want %d", got, resets)
	}
	if !strings.Contains(resp.Failure.Message, "seven_day") ||
		!strings.Contains(resp.Failure.Message, resp.Failure.ClearsAt.Format(time.RFC3339)) {
		t.Errorf("Message = %q, want the window and the instant: %q is not actionable",
			resp.Failure.Message, "agent failure")
	}
}

// A refused turn ends WITHOUT a result event — the case reported as `no-result`
// today, which loses the window and the instant and bills the refusal. The
// classification must therefore beat the no-result path as well as the
// is_error one.
func TestRun_AnExhaustedAccountIsClassifiedWithNoResultEventAtAll(t *testing.T) {
	stream := `{"type":"system","subtype":"init","session_id":"s"}
` + rejectedEvent("five_hour", 1789153635) + `
`
	resp, err := clientWith(&fakeCmd{stdoutStream: stream}).Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Failure == nil {
		t.Fatal("Failure = nil, want an account-exhausted refusal")
	}
	if resp.Failure.Kind != flow.FailureAccountExhausted {
		t.Fatalf("Kind = %q, want %q — a turn that died without a result is not a no-result when the substrate said why",
			resp.Failure.Kind, flow.FailureAccountExhausted)
	}
	if resp.Failure.Window != "five_hour" {
		t.Errorf("Window = %q, want five_hour", resp.Failure.Window)
	}
}

// `status` is a field reporting a standing, not an occurrence: a turn that was
// throttled and then let through was not refused, so the LAST event wins.
func TestRun_ALaterAllowedSupersedesAnEarlierRejection(t *testing.T) {
	stream := `{"type":"system","subtype":"init","session_id":"s"}
` + rejectedEvent("five_hour", 1789153635) + `
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}
{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s","total_cost_usd":0.5}
`
	resp, err := clientWith(&fakeCmd{stdoutStream: stream}).Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Failure != nil {
		t.Fatalf("Failure = %+v, want nil: the substrate let the turn through", resp.Failure)
	}
	if resp.LastText != "done" {
		t.Errorf("LastText = %q, want the turn's answer", resp.LastText)
	}
}

// An allowed standing on its own changes nothing about how the turn reads.
func TestRun_AnAllowedRateLimitEventLeavesTheTurnUntouched(t *testing.T) {
	stream := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour","resetsAt":1789153635}}
` + successStream
	resp, err := clientWith(&fakeCmd{stdoutStream: stream}).Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Failure != nil {
		t.Fatalf("Failure = %+v, want nil", resp.Failure)
	}
	if resp.LastText != "Hello World" || resp.CostUSD != 0.42 {
		t.Errorf("resp = %+v, want the ordinary successful turn", resp)
	}
}

// A refusal that names no reset still classifies: the condition is the status,
// and the instant is what the park carries WHEN there is one. Absent reads as
// "no instant" — never as the epoch, which would read as an instant long past
// and send a driver straight back into the same refusal. An undecodable event
// is skipped like any other malformed line and leaves the standing alone.
func TestRun_AnExhaustedAccountWithNoResetInstantCarriesNone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream string
	}{
		{
			name:   "no resetsAt field",
			stream: `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour"}}`,
		},
		{
			name: "an undecodable event does not clear the refusal",
			stream: `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour"}}
{"type":"rate_limit_event","rate_limit_info":["not","an","object"]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := tc.stream + "\n{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":true,\"session_id\":\"s\"}\n"
			resp, err := clientWith(&fakeCmd{stdoutStream: stream}).Run(context.Background(), flow.AgentRequest{Prompt: "go"})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if resp.Failure == nil || resp.Failure.Kind != flow.FailureAccountExhausted {
				t.Fatalf("Failure = %+v, want kind=%s", resp.Failure, flow.FailureAccountExhausted)
			}
			if resp.Failure.ClearsAt != nil {
				t.Errorf("ClearsAt = %v, want absent: the substrate named no reset", resp.Failure.ClearsAt)
			}
			if strings.Contains(resp.Failure.Message, "resets") {
				t.Errorf("Message = %q, want no reset claimed when none was published", resp.Failure.Message)
			}
		})
	}
}

// An unknown status is not a refusal. A later release adding one must not park
// every step on the machine.
func TestRun_AnUnknownRateLimitStatusIsNotARefusal(t *testing.T) {
	stream := `{"type":"rate_limit_event","rate_limit_info":{"status":"warning","rateLimitType":"five_hour"}}
` + successStream
	resp, err := clientWith(&fakeCmd{stdoutStream: stream}).Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Failure != nil {
		t.Fatalf("Failure = %+v, want nil: only %q is the refusal", resp.Failure, "rejected")
	}
}

// A refused turn also ENDS BADLY — no result, a non-zero exit, nothing on
// stdout but the refusal itself. That ending is not the evidence: read as an
// exit-error it parks as infrastructure and loses the window and the instant
// the substrate had already handed over.
func TestRun_ARefusalOutranksHowTheTurnDied(t *testing.T) {
	const resets = 1789153635
	f := &fakeCmd{
		stdoutStream: rejectedEvent("seven_day", resets) + "\n",
		stderrStream: "error: exiting\n",
		waitErr:      errors.New("exit status 1"),
	}
	resp, err := clientWith(f).Run(context.Background(), flow.AgentRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Failure == nil {
		t.Fatal("Failure = nil, want the refusal the substrate stated")
	}
	if resp.Failure.Kind != flow.FailureAccountExhausted {
		t.Fatalf("Kind = %q, want %q — the non-zero exit is the wreckage, not the evidence",
			resp.Failure.Kind, flow.FailureAccountExhausted)
	}
	if resp.Failure.Window != "seven_day" {
		t.Errorf("Window = %q, want seven_day", resp.Failure.Window)
	}
	if resp.Failure.ClearsAt == nil || resp.Failure.ClearsAt.Unix() != resets {
		t.Errorf("ClearsAt = %v, want the published instant", resp.Failure.ClearsAt)
	}
}
