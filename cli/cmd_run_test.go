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
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// takeoverBackend wraps fake.Orchestrator to count and steer MarkManualTakeover
// calls — the test asserts on whether/when the cli's manual-takeover hook
// fires depending on the FLOW_DISPATCHED_BY_RUNNER env. T0481.
type takeoverBackend struct {
	*fake.Orchestrator
	calls    int
	failWith error
}

// Edit hands back an editor that counts a committed SetManual(true). Manual
// control is set through the editor now — there is no separate capability for
// it — so what a test can observe is the edit landing, not a method being
// called.
func (b *takeoverBackend) Edit(ctx context.Context, ref flow.ItemRef) (flow.ItemEditor, error) {
	inner, err := b.Orchestrator.Edit(ctx, ref)
	if err != nil {
		return nil, err
	}
	return &takeoverEditor{ItemEditor: inner, be: b}, nil
}

type takeoverEditor struct {
	flow.ItemEditor
	be     *takeoverBackend
	manual bool
}

func (e *takeoverEditor) SetManual(m bool) {
	e.manual = m
	e.ItemEditor.SetManual(m)
}

func (e *takeoverEditor) Commit(ctx context.Context) error {
	if e.manual {
		e.be.calls++
	}
	if e.be.failWith != nil {
		return e.be.failWith
	}
	return e.ItemEditor.Commit(ctx)
}

// Compile-time assert: takeoverBackend is a whole orchestrator.
var _ flow.Orchestrator = (*takeoverBackend)(nil)

// run-step NEVER asserts manual control, whoever invoked it.
//
// It used to: the absence of FLOW_DISPATCHED_BY_RUNNER was read as "an operator
// typed this", and the item was flagged hand-driven with any unresolved park
// resolved. Both halves were wrong for the caller that most needs this command
// — an external scheduler is unattended like the runner and external like the
// operator, so it took the operator branch and marked every item it touched for
// a person who was not there, clearing the parks that existed to stop the item.
//
// Running one step is not an act of takeover. `resolve` is what a person types
// to drive an item, so manual control is asserted there or deliberately through
// the editor, never inferred from which command was reached for
// (docs/orchestrator.md § ItemEditor: "set by a deliberate act and never as a
// side effect of advancing the item").
//
// The env var is a table row rather than a separate test, because the point is
// that it no longer selects anything: both values must produce no edit. A test
// covering only the unset case would pass against code that still branched.
func TestCmdRun_NeverAssertsManualControl(t *testing.T) {
	for _, tc := range []struct{ name, env string }{
		{"env unset — would have been read as an operator takeover", ""},
		{"env set — the runner's old signal, now meaning nothing here", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &stubAgent{name: "stub"}
			app, be, _ := testApp(t, func(f *flow.Flow) {
				f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
					return ctx.ResolveMarkdown("the plan")
				}, flow.StepConfig{})
			}, a)
			wrapped := &takeoverBackend{Orchestrator: be}
			app.Orchestrator = wrapped

			t.Setenv(dispatchedByRunnerEnv, tc.env)

			if code := app.cmdRun(context.Background(), nil); code != 0 {
				t.Fatalf("cmdRun = %d, want 0", code)
			}
			if wrapped.calls != 0 {
				t.Errorf("manual edits = %d, want 0 — run-step must never assert manual control", wrapped.calls)
			}
		})
	}
}

// noClaimBackend answers "this arena holds nothing".
type noClaimBackend struct{ *fake.Orchestrator }

func (b *noClaimBackend) LookupActiveClaim(context.Context) (*flow.Claim, error) {
	return nil, nil
}

// unreadableLeaseBackend answers with the failure a truncated lease file
// produces — the store exists and cannot say what it holds.
type unreadableLeaseBackend struct{ *fake.Orchestrator }

func (b *unreadableLeaseBackend) LookupActiveClaim(context.Context) (*flow.Claim, error) {
	return nil, errors.New("active.json: unexpected end of JSON input")
}

// A refusal is REPORTED, on stdout, in the same shape as any other outcome —
// and it names its scope.
//
// Before this, every failure path printed prose to stderr and exited 1, so a
// caller sequencing steps itself had the branch `resolve` makes internally
// (ErrClaimRefused.ItemScoped: try the next item, or stop) and nothing to make
// it on but the text. Both cases here are arena-scoped: neither names an item,
// so "try a different one" is not a move that exists.
//
// Asserted on the decoded JSON rather than on the string, because a caller
// decodes — a substring check would pass on prose that merely mentioned the
// word.
func TestCmdRun_RefusalIsReportedWithItsScope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		backend  func(*fake.Orchestrator) flow.Orchestrator
		wantCode string
	}{
		{
			name:     "no claim — nothing to advance, and no item to name",
			backend:  func(f *fake.Orchestrator) flow.Orchestrator { return &noClaimBackend{f} },
			wantCode: string(refusalNoClaim),
		},
		{
			name:     "lease unreadable — the store cannot answer at all",
			backend:  func(f *fake.Orchestrator) flow.Orchestrator { return &unreadableLeaseBackend{f} },
			wantCode: string(refusalLeaseUnreadable),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &stubAgent{name: "stub"}
			app, be, _ := testApp(t, func(f *flow.Flow) {
				f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
					return ctx.ResolveMarkdown("the plan")
				}, flow.StepConfig{})
			}, a)
			app.Orchestrator = tc.backend(be)
			var out bytes.Buffer
			app.Out = &out

			code := app.cmdRun(context.Background(), []string{"--json"})
			if code != 1 {
				t.Fatalf("cmdRun = %d, want 1 (a refusal is not success)", code)
			}

			var res flow.InvocationResult
			if err := json.Unmarshal(out.Bytes(), &res); err != nil {
				t.Fatalf("stdout is not an InvocationResult: %v\n%s", err, out.String())
			}
			if res.Refusal == nil {
				t.Fatalf("result carries no refusal:\n%s", out.String())
			}
			if res.Refusal.Code != tc.wantCode {
				t.Errorf("refusal code = %q, want %q", res.Refusal.Code, tc.wantCode)
			}
			if res.Refusal.ItemScoped {
				t.Error("refusal is item-scoped; neither of these conditions names an item, " +
					"so trying the next item is not a move a caller could make")
			}
			if res.Refusal.Reason == "" {
				t.Error("refusal carries no human reason")
			}
		})
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
			if res.Step != "plan" || res.Status != "done" {
				t.Errorf("result = %+v, want step %q status done", res, "plan")
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
	if !strings.Contains(got, "plan") || !strings.Contains(got, "done") {
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
	app.Preflight = func(ctx context.Context, state *flow.Item) error {
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
	if res.Step != "plan" || res.Status != "done" {
		t.Errorf("result = %+v, want step %q status done", res, "plan")
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

// ---------------------------------------------------------------------------
// Budget park narration includes axes (#3)
// ---------------------------------------------------------------------------

func TestCmdRun_BudgetParkNarratesAxes(t *testing.T) {
	t.Setenv(outputEnv, "")
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("flaky", "plan", func(ctx flow.StepCtx) error {
			return errors.New("boom")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	app.StepBudgets = map[flow.StepId]flow.StepBudget{"plan": {
		MaxInvocations:          1,
		MaxPromptsPerInvocation: 2,
		MaxCostUSD:              10,
		Timeout:                 30 * time.Minute,
	}}

	// Burn the only invocation.
	res1, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res1.Status != "failed" {
		t.Fatalf("first run = %+v, want failed", res1)
	}

	// Now run-step: next dispatch parks on budget.
	out := &bytes.Buffer{}
	app.Out = out
	app.Orchestrator = be

	code := app.cmdRun(context.Background(), []string{"--human"})
	if code != 0 {
		t.Fatalf("cmdRun = %d, want 0", code)
	}
	got := out.String()
	if !strings.Contains(got, "axes:") {
		t.Errorf("expected axes line in park narration; got %q", got)
	}
	if !strings.Contains(got, "inv (flat)") {
		t.Errorf("expected invocations flagged flat; got %q", got)
	}
}

// A blocked stop on items prints the reason and the open blockers on their own
// line, exits 1, and keeps the claim.
func TestCmdRun_BlockedOnItemsNarratesTheBlockers(t *testing.T) {
	t.Setenv(outputEnv, "")
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			t.Fatal("the step must not dispatch on a blocked item")
			return nil
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	be.AddItem("2", flow.Item{Type: "task", Title: "landed"})
	be.AddItem("3", flow.Item{Type: "task", Title: "still open"})
	be.SetStatus("2", flow.StatusTerminal, "done")
	blockOn(t, be, claim.ItemRef, be.Ref("2"))
	blockOn(t, be, claim.ItemRef, be.Ref("3"))
	out := &bytes.Buffer{}
	app.Out = out

	code := app.cmdRun(context.Background(), []string{"--human"})
	if code != 1 {
		t.Fatalf("cmdRun = %d, want 1 — a blocked item must not read as nothing to do", code)
	}
	got := out.String()
	if !strings.Contains(got, "plan → blocked — waiting on unfinished dependencies") {
		t.Errorf("expected the step, status and reason; got %q", got)
	}
	if !strings.Contains(got, "\n  blocked by: 3\n") {
		t.Errorf("expected a `blocked by:` line naming only the open blocker; got %q", got)
	}
	if held, _ := be.LookupActiveClaim(context.Background()); held == nil {
		t.Error("the claim was released — the stop keeps it")
	}
}

func TestCmdRun_BlockedOnItemsJSONCarriesTheKindAndBlockers(t *testing.T) {
	t.Setenv(outputEnv, "")
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			t.Fatal("the step must not dispatch on a blocked item")
			return nil
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	be.AddItem("2", flow.Item{Type: "task", Title: "still open"})
	blockOn(t, be, claim.ItemRef, be.Ref("2"))
	out := &bytes.Buffer{}
	app.Out = out

	if code := app.cmdRun(context.Background(), []string{"--json"}); code != 1 {
		t.Fatalf("cmdRun = %d, want 1", code)
	}
	var res flow.InvocationResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("stdout is not one JSON result: %v\n%s", err, out.String())
	}
	if res.Status != "blocked" || res.BlockKind != flow.WaitsOnItems {
		t.Errorf("result = %+v, want blocked, kind waits-on-items", res)
	}
	if len(res.BlockedBy) != 1 || res.BlockedBy[0].Ref.Display != "2" || res.BlockedBy[0].Status != flow.StatusOpen {
		t.Errorf("BlockedBy = %+v, want the open blocker with its status", res.BlockedBy)
	}
}

func TestCmdRun_NonBudgetParkOmitsAxes(t *testing.T) {
	t.Setenv(outputEnv, "")
	app, _, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("silent", "plan", func(ctx flow.StepCtx) error {
			return nil // returns without resolving
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	out := &bytes.Buffer{}
	app.Out = out

	code := app.cmdRun(context.Background(), []string{"--human"})
	if code != 0 {
		t.Fatalf("cmdRun = %d, want 0", code)
	}
	if strings.Contains(out.String(), "axes:") {
		t.Errorf("non-budget park must not emit axes line; got %q", out.String())
	}
}
