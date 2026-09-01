package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/backend/fake"
)

// takeoverBackend wraps fake.Backend to count and steer MarkManualTakeover
// calls — the test asserts on whether/when the cli's manual-takeover hook
// fires depending on the FLOW_DISPATCHED_BY_RUNNER env. T0481.
type takeoverBackend struct {
	*fake.Backend
	calls    int
	failWith error
}

func (b *takeoverBackend) MarkManualTakeover(ctx context.Context, claim flow.Claim) error {
	b.calls++
	return b.failWith
}

// Compile-time assert: takeoverBackend implements flow.ManualTakeover.
var _ flow.ManualTakeover = (*takeoverBackend)(nil)

// TestCmdRun_ManualSetsManualAndClearsPark (T0481): when the binary is run
// by hand (FLOW_DISPATCHED_BY_RUNNER unset), cli.cmdRun MUST call the
// backend's MarkManualTakeover so the operator's "I'm driving now" signal
// flows through (set Manual, resolve any FlowPark).
func TestCmdRun_ManualSetsManualAndClearsPark(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})

	}, a)
	wrapped := &takeoverBackend{Backend: be}
	app.Backend = wrapped

	// Belt-and-braces: ensure the env var is NOT set for this case (the runner
	// would set it; an operator-typed run-step has no such pre-export).
	t.Setenv(dispatchedByRunnerEnv, "")

	code := app.cmdRun(context.Background(), nil)
	if code != 0 {
		t.Fatalf("cmdRun = %d, want 0", code)
	}
	if wrapped.calls != 1 {
		t.Errorf("MarkManualTakeover calls = %d, want 1 (operator-driven run-step)", wrapped.calls)
	}
}

// TestCmdRun_OrchestratedSkipsTakeover (T0481): when the runner spawned this
// process (FLOW_DISPATCHED_BY_RUNNER=1), the takeover side effects MUST NOT
// fire — the orchestrator owns the lease/manual decisions, and applying the
// operator-takeover signal here would flip Manual=true on an auto-dispatched
// item.
func TestCmdRun_OrchestratedSkipsTakeover(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})

	}, a)
	wrapped := &takeoverBackend{Backend: be}
	app.Backend = wrapped

	t.Setenv(dispatchedByRunnerEnv, "1")

	code := app.cmdRun(context.Background(), nil)
	if code != 0 {
		t.Fatalf("cmdRun = %d, want 0", code)
	}
	if wrapped.calls != 0 {
		t.Errorf("MarkManualTakeover calls = %d, want 0 (orchestrator-spawned run-step)", wrapped.calls)
	}
}

// TestCmdRun_TakeoverFailureDoesNotBlockStep (T0481): a manual-takeover
// failure surfaces as a warning on Err but does NOT abort the step — the
// user's intent is the step, the takeover is bookkeeping.
func TestCmdRun_TakeoverFailureDoesNotBlockStep(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})

	}, a)
	wrapped := &takeoverBackend{Backend: be, failWith: errors.New("tracker unreachable")}
	app.Backend = wrapped

	t.Setenv(dispatchedByRunnerEnv, "")

	code := app.cmdRun(context.Background(), nil)
	if code != 0 {
		t.Fatalf("cmdRun = %d, want 0 (takeover failure must not abort the step)", code)
	}
	if wrapped.calls != 1 {
		t.Errorf("MarkManualTakeover calls = %d, want 1 (failure path still calls)", wrapped.calls)
	}
}

// TestCmdRun_JSONModeCompactOutput: --json and FLOW_OUTPUT=json both produce
// compact single-line JSON — byte-identical to the pre-change unconditional
// output. Verifies the machine contract is preserved.
func TestCmdRun_JSONModeCompactOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  string
	}{
		{"flag", []string{"--json"}, ""},
		{"env", nil, "json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(outputEnv, tc.env)
			app, _, _ := testApp(t, func(f *flow.Flow) {
				f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
					return ctx.ResolveMarkdown("the plan")
				}, flow.StepConfig{})
			}, &stubAgent{name: "stub"})
			out := &bytes.Buffer{}
			app.Out = out

			if code := app.cmdRun(context.Background(), tc.args); code != 0 {
				t.Fatalf("cmdRun = %d, want 0", code)
			}
			// Must be valid JSON on a single line (compact, not indented).
			raw := out.Bytes()
			if bytes.Count(raw, []byte("\n")) != 1 {
				t.Fatalf("expected exactly one line; got %q", raw)
			}
			var res flow.InvocationResult
			if err := json.Unmarshal(raw, &res); err != nil {
				t.Fatalf("stdout %q is not an InvocationResult: %v", raw, err)
			}
			if res.Step != "write plan" || res.Status != "done" {
				t.Errorf("result = %+v, want step %q status done", res, "write plan")
			}
		})
	}
}

// TestCmdRun_HumanModeOneLine: --human renders a one-line summary on stdout,
// not raw JSON.
func TestCmdRun_HumanModeOneLine(t *testing.T) {
	app, _, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	out := &bytes.Buffer{}
	app.Out = out

	if code := app.cmdRun(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdRun = %d, want 0", code)
	}
	got := out.String()
	if !strings.Contains(got, "write plan") || !strings.Contains(got, "done") {
		t.Errorf("human output %q should contain step name and status", got)
	}
	if !strings.Contains(got, "→") {
		t.Errorf("human output %q should contain arrow separator", got)
	}
	// Must NOT be valid JSON — that would mean the mode gate did nothing.
	var probe json.RawMessage
	if json.Unmarshal([]byte(got), &probe) == nil {
		t.Errorf("human output %q is valid JSON — mode gate did not take effect", got)
	}
}

// TestCmdRun_HumanModeWithReason: when a step produces a reason (e.g.
// blocked), the reason appears in the human one-liner.
func TestCmdRun_HumanModeWithReason(t *testing.T) {
	app, _, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	// Use a preflight that returns ErrBlocked to produce a reason.
	app.Preflight = func(ctx context.Context, state *flow.ItemState) error {
		return fmt.Errorf("answer needed: %w", flow.ErrBlocked)
	}

	out := &bytes.Buffer{}
	app.Out = out

	// blocked exits 1, but we care about the output, not the exit code.
	app.cmdRun(context.Background(), []string{"--human"})

	got := out.String()
	if !strings.Contains(got, "answer needed") {
		t.Errorf("human output %q should contain the reason", got)
	}
	if !strings.Contains(got, "→") || !strings.Contains(got, "blocked") {
		t.Errorf("human output %q should contain arrow and status", got)
	}
}

// TestCmdRun_AutoDetectsHuman: with no flags and no FLOW_OUTPUT, a
// bytes.Buffer as app.Out (which resolveOutput treats as human) produces
// human output. Validates rule 3 (terminal detection) applies.
func TestCmdRun_AutoDetectsHuman(t *testing.T) {
	t.Setenv(outputEnv, "")
	app, _, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	out := &bytes.Buffer{}
	app.Out = out

	if code := app.cmdRun(context.Background(), nil); code != 0 {
		t.Fatalf("cmdRun = %d, want 0", code)
	}
	// bytes.Buffer → resolveOutput → human. Must NOT be valid JSON.
	var probe json.RawMessage
	if json.Unmarshal(out.Bytes(), &probe) == nil {
		t.Errorf("auto-detected output %q is valid JSON; expected human (bytes.Buffer is not a terminal)", out.String())
	}
	if !strings.Contains(out.String(), "→") {
		t.Errorf("auto-detected output %q should be human format with arrow", out.String())
	}
}

// TestCmdRun_PipedStdoutSelectsJSON: with an *os.File pipe as stdout
// (not a terminal), auto-detection selects JSON mode.
func TestCmdRun_PipedStdoutSelectsJSON(t *testing.T) {
	t.Setenv(outputEnv, "")
	app, _, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	app.Out = w

	if code := app.cmdRun(context.Background(), nil); code != 0 {
		t.Fatalf("cmdRun = %d, want 0", code)
	}
	w.Close()

	var buf bytes.Buffer
	buf.ReadFrom(r)

	var res flow.InvocationResult
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("piped stdout %q is not valid JSON: %v", buf.String(), err)
	}
	if res.Step != "write plan" || res.Status != "done" {
		t.Errorf("result = %+v, want step %q status done", res, "write plan")
	}
}

// TestCmdRun_EnvHumanProducesHumanOutput: FLOW_OUTPUT=human selects human
// mode — the env var was previously ignored by run-step entirely.
func TestCmdRun_EnvHumanProducesHumanOutput(t *testing.T) {
	t.Setenv(outputEnv, "human")
	app, _, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	// Use an os.Pipe so auto-detect would pick JSON — the env var must override.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	app.Out = w

	if code := app.cmdRun(context.Background(), nil); code != 0 {
		t.Fatalf("cmdRun = %d, want 0", code)
	}
	w.Close()

	var buf bytes.Buffer
	buf.ReadFrom(r)
	got := buf.String()

	// Must be human (arrow format), not JSON.
	if !strings.Contains(got, "→") {
		t.Errorf("FLOW_OUTPUT=human output %q should be human format with arrow", got)
	}
	var probe json.RawMessage
	if json.Unmarshal(buf.Bytes(), &probe) == nil {
		t.Errorf("FLOW_OUTPUT=human output %q is valid JSON — env var did not take effect", got)
	}
}

// TestCmdRun_MutuallyExclusiveFlags: --json --human together is a usage
// error (exit 2).
func TestCmdRun_MutuallyExclusiveFlags(t *testing.T) {
	app, _, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	code := app.cmdRun(context.Background(), []string{"--json", "--human"})
	if code != 2 {
		t.Errorf("cmdRun(--json --human) = %d, want 2", code)
	}
}
