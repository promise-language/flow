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

// takeoverBackend wraps fake.Orchestrator to count committed manual edits, so
// a test can assert that `run-step` makes none: running a single step is not
// an act of takeover, and an unattended caller must not flag every item it
// touches for a person who is not there.
type takeoverBackend struct {
	*fake.Orchestrator
	calls int
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
	return e.ItemEditor.Commit(ctx)
}

// Compile-time assert: takeoverBackend is a whole orchestrator.
var _ flow.Orchestrator = (*takeoverBackend)(nil)

// failingLoadBackend forces Load to error once a claim is held. Load is
// RunOne's first call and its first error return (cli/orchestrator.go), so it
// is how a test reaches cmdRun's RunOne error branch.
type failingLoadBackend struct {
	*fake.Orchestrator
	err error
}

func (b *failingLoadBackend) Load(ctx context.Context, ref flow.ItemRef) (*flow.Item, error) {
	return nil, b.err
}

// Compile-time assert: failingLoadBackend is a whole orchestrator.
var _ flow.Orchestrator = (*failingLoadBackend)(nil)

// runStepStubFlow is the one-step flow every run-step test in this file drives:
// a plan step that finalizes. One definition, so a test about a refusal differs
// from a test about a success only in what it does to the arena.
func runStepStubFlow(f *flow.Flow) {
	f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
	}, flow.StepConfig{Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
}

// A run-step that never reached a step still reports on the machine channel:
// the refusal is the same InvocationResult object resolve streams, carrying
// the scope a caller branches on. Without a claim this arena does not know
// which item to run against, and every item would meet the same answer — so
// the scope is the arena, not the item.
func TestCmdRun_NoActiveClaimJSONCarriesTheRefusal(t *testing.T) {
	app, be, claim := testApp(t, runStepStubFlow, &stubAgent{name: "stub"})
	// testApp pre-claims; drop it so the arena holds nothing.
	if err := be.Release(context.Background(), claim.ItemRef); err != nil {
		t.Fatalf("Release: %v", err)
	}
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	app.Out, app.Err = out, errBuf

	if code := app.cmdRun(context.Background(), []string{"--json"}); code != 1 {
		t.Fatalf("cmdRun = %d, want 1", code)
	}
	got := decodeResultStream(t, out.String())
	if len(got) != 1 {
		t.Fatalf("stdout carried %d results, want exactly 1; got %q", len(got), out.String())
	}
	res := got[0]
	if res.Status != string(flow.StatusFailed) {
		t.Errorf("status = %q, want %q — a refusal that exits 1 is a stop the command could not complete", res.Status, flow.StatusFailed)
	}
	if !strings.Contains(res.Reason, "no active claim") {
		t.Errorf("reason = %q, want it to name the missing claim", res.Reason)
	}
	if res.ItemScoped == nil {
		t.Fatal("item_scoped absent — a caller sequencing steps has nothing to branch on")
	}
	if *res.ItemScoped {
		t.Error("item_scoped = true, want false — no claim is this arena's condition, and a different item would meet the same answer")
	}
}

// Human mode is unchanged by the refusal joining the machine channel: the
// prose is the line it always was, on stderr, and stdout stays empty.
func TestCmdRun_NoActiveClaimHumanIsUnchanged(t *testing.T) {
	app, be, claim := testApp(t, runStepStubFlow, &stubAgent{name: "stub"})
	if err := be.Release(context.Background(), claim.ItemRef); err != nil {
		t.Fatalf("Release: %v", err)
	}
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	app.Out, app.Err = out, errBuf

	if code := app.cmdRun(context.Background(), []string{"--human"}); code != 1 {
		t.Fatalf("cmdRun = %d, want 1", code)
	}
	if out.Len() != 0 {
		t.Errorf("human-mode stdout = %q, want empty", out.String())
	}
	if want := "run-step: no active claim (run `claim <id>` first)\n"; errBuf.String() != want {
		t.Errorf("stderr = %q, want %q", errBuf.String(), want)
	}
}

// A LookupActiveClaim failure is the arena's: the lease store this checkout
// reads is unreachable, so every item would meet the same answer.
func TestCmdRun_LookupActiveClaimErrorIsArenaScoped(t *testing.T) {
	app, be, _ := testApp(t, runStepStubFlow, &stubAgent{name: "stub"})
	app.Orchestrator = &failingLookupActiveClaimBackend{Orchestrator: be, err: errors.New("lookup boom")}
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	app.Out, app.Err = out, errBuf

	if code := app.cmdRun(context.Background(), []string{"--json"}); code != 1 {
		t.Fatalf("cmdRun = %d, want 1", code)
	}
	got := decodeResultStream(t, out.String())
	if len(got) != 1 {
		t.Fatalf("stdout carried %d results, want exactly 1; got %q", len(got), out.String())
	}
	if !strings.Contains(got[0].Reason, "lookup boom") {
		t.Errorf("reason = %q, want the backend's error text", got[0].Reason)
	}
	if got[0].ItemScoped == nil || *got[0].ItemScoped {
		t.Errorf("item_scoped = %v, want a present false", got[0].ItemScoped)
	}
	if !strings.Contains(errBuf.String(), "run-step: lookup boom") {
		t.Errorf("stderr = %q, want the prose line it has always carried", errBuf.String())
	}
}

// A RunOne failure is reported through the same object, and — unlike the
// refusals before it — names the item, because by then the claim said which
// one. The scope is still the arena: RunOne returns an error only when the
// orchestrator itself failed, not when the work did.
func TestCmdRun_RunOneErrorIsArenaScoped(t *testing.T) {
	app, be, claim := testApp(t, runStepStubFlow, &stubAgent{name: "stub"})
	app.Orchestrator = &failingLoadBackend{Orchestrator: be, err: errors.New("load boom")}
	out := &bytes.Buffer{}
	app.Out = out

	if code := app.cmdRun(context.Background(), []string{"--json"}); code != 1 {
		t.Fatalf("cmdRun = %d, want 1", code)
	}
	got := decodeResultStream(t, out.String())
	if len(got) != 1 {
		t.Fatalf("stdout carried %d results, want exactly 1; got %q", len(got), out.String())
	}
	if got[0].Item != claim.ItemRef.Display {
		t.Errorf("item = %q, want %q — the claim named the item before the step failed", got[0].Item, claim.ItemRef.Display)
	}
	if !strings.Contains(got[0].Reason, "load boom") {
		t.Errorf("reason = %q, want the orchestrator's error text", got[0].Reason)
	}
	if got[0].ItemScoped == nil || *got[0].ItemScoped {
		t.Errorf("item_scoped = %v, want a present false", got[0].ItemScoped)
	}
}

// Running a single step is not an act of takeover. `run-step` is the primitive
// an external scheduler calls in a loop, and marking the item manual there
// flags every item it touches for a person who is not there — and silently
// clears the parks that exist to stop the item advancing.
func TestCmdRun_DoesNotMarkTheItemManual(t *testing.T) {
	app, be, claim := testApp(t, runStepStubFlow, &stubAgent{name: "stub"})
	wrapped := &takeoverBackend{Orchestrator: be}
	app.Orchestrator = wrapped

	if code := app.cmdRun(context.Background(), nil); code != 0 {
		t.Fatalf("cmdRun = %d, want 0", code)
	}
	if wrapped.calls != 0 {
		t.Errorf("committed manual edits = %d, want 0 — run-step marks nothing", wrapped.calls)
	}
	item, err := be.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if item.Manual {
		t.Error("the item is under manual control after a run-step — nothing in the CLI sets it")
	}
}

// The park is the other half of what the takeover took: setting manual
// resolves any unresolved park, so a `run-step` that asserted it cleared the
// stop a person put on the item — silently, once per call, for a caller that
// only meant to advance a step. The park here names a step this run does not
// resolve (resolving a step legitimately clears its own park), so run-step is
// the only thing that could have cleared it.
func TestCmdRun_LeavesAnUnresolvedParkStanding(t *testing.T) {
	app, be, claim := testApp(t, runStepStubFlow, &stubAgent{name: "stub"})
	park := flow.ParkRequest{Kind: flow.ParkQuestion, Step: "commit", Reason: "waiting for an answer"}
	if err := be.Park(context.Background(), claim.ItemRef, park); err != nil {
		t.Fatalf("Park: %v", err)
	}

	if code := app.cmdRun(context.Background(), nil); code != 0 {
		t.Fatalf("cmdRun = %d, want 0", code)
	}
	got := be.ParkRequest(claim.ItemRef.Display)
	if got == nil {
		t.Fatal("the park is gone after a run-step — a command that only advances the item cleared the stop on it")
	}
	if got.Step != park.Step || got.Reason != park.Reason {
		t.Errorf("park = %+v, want %+v untouched", *got, park)
	}
}

// The regression guard against reintroducing the switch: the retired name is
// spelled as a string literal, because the code must no longer know it. Set or
// unset, the two runs behave identically and mark nothing — an environment
// variable selects no behaviour here (docs/org/cli-guide.md § 2).
func TestCmdRun_NoEnvironmentVariableSelectsManualControl(t *testing.T) {
	for _, value := range []string{"1", ""} {
		t.Run("env="+value, func(t *testing.T) {
			t.Setenv("FLOW_DISPATCHED_BY_RUNNER", value)
			app, be, claim := testApp(t, runStepStubFlow, &stubAgent{name: "stub"})
			wrapped := &takeoverBackend{Orchestrator: be}
			app.Orchestrator = wrapped

			if code := app.cmdRun(context.Background(), nil); code != 0 {
				t.Fatalf("cmdRun = %d, want 0", code)
			}
			if wrapped.calls != 0 {
				t.Errorf("committed manual edits = %d, want 0 whatever the environment holds", wrapped.calls)
			}
			item, err := be.Load(context.Background(), claim.ItemRef)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if item.Manual {
				t.Error("the environment flipped manual control — the switch is back")
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
				f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
					return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
				}, flow.StepConfig{Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		f.AddStep("flaky", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return flow.StepResult{}, errors.New("boom")
		}, flow.StepConfig{Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			t.Fatal("the step must not dispatch on a blocked item")
			return flow.StepResult{}, nil
		}, flow.StepConfig{Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			t.Fatal("the step must not dispatch on a blocked item")
			return flow.StepResult{}, nil
		}, flow.StepConfig{Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
		f.AddStep("silent", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return flow.StepResult{}, nil // returns without resolving
		}, flow.StepConfig{Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
