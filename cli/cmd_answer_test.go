package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// askAll records each question in turn. AskQuestion takes ONE and appends, so a
// test wanting several asks several times — which is exactly what a handler
// with several questions does.
func askAll(o flow.Orchestrator, ctx context.Context, c flow.Claim, qs []flow.AgentQuestion) ([]flow.Question, error) {
	var out []flow.Question
	for _, q := range qs {
		rec, err := o.AskQuestion(ctx, c.ItemRef, q)
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// answerTestSetup builds an App + claim with one pending question on the item.
// Returns the app (with captured stdout/stderr), the fake backend, and the
// item ID.
func answerTestSetup(t *testing.T) (*App, *bytes.Buffer, *bytes.Buffer, *fake.Orchestrator, string) {
	t.Helper()
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, a)

	// Ask a question so there is something to answer.
	if _, err := askAll(be, context.Background(), claim, []flow.AgentQuestion{
		{Text: "should we re-plan?"},
	}); err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	var out, errBuf bytes.Buffer
	app.Out = &out
	app.Err = &errBuf
	return app, &out, &errBuf, be, claim.ItemRef.Display
}

func TestCmdAnswer_HappyPath_OneQuestion(t *testing.T) {
	app, out, errBuf, be, itemID := answerTestSetup(t)

	code := app.cmdAnswer(context.Background(), []string{itemID, "yes, re-plan"})
	if code != 0 {
		t.Fatalf("cmdAnswer = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "answered") {
		t.Errorf("stdout = %q, want 'answered' in output", out.String())
	}

	// Verify the answer was recorded in the backend.
	ref := flow.ItemRef{OrchestratorName: "fake", Display: itemID, Ref: json.RawMessage(`"` + itemID + `"`)}
	st, err := be.Load(context.Background(), ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pending := st.PendingQuestions()
	if len(pending) != 0 {
		t.Errorf("PendingQuestions = %d, want 0", len(pending))
	}
}

func TestCmdAnswer_MultipleQuestions_NoFlag(t *testing.T) {
	app, _, errBuf, be, itemID := answerTestSetup(t)

	// Add a second question.
	claim, err := be.LookupActiveClaim(context.Background())
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if _, err := askAll(be, context.Background(), *claim, []flow.AgentQuestion{
		{Text: "what approach?"},
	}); err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	code := app.cmdAnswer(context.Background(), []string{itemID, "answer"})
	if code != 1 {
		t.Fatalf("cmdAnswer = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "--question") {
		t.Errorf("stderr = %q, want mention of --question", errBuf.String())
	}
}

func TestCmdAnswer_MultipleQuestions_WithFlag(t *testing.T) {
	app, out, errBuf, be, itemID := answerTestSetup(t)

	// Add a second question.
	claim, err := be.LookupActiveClaim(context.Background())
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	qs, err := askAll(be, context.Background(), *claim, []flow.AgentQuestion{
		{Text: "what approach?"},
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}
	targetID := qs[0].ID

	code := app.cmdAnswer(context.Background(), []string{itemID, "option B", "--question", string(targetID)})
	if code != 0 {
		t.Fatalf("cmdAnswer = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), string(targetID)) {
		t.Errorf("stdout = %q, want question id %q", out.String(), targetID)
	}
}

func TestCmdAnswer_NoPendingQuestions(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, a)
	_ = be

	var errBuf bytes.Buffer
	app.Out = newDiscardWriter()
	app.Err = &errBuf

	code := app.cmdAnswer(context.Background(), []string{"1", "some answer"})
	if code != 1 {
		t.Fatalf("cmdAnswer = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "no outstanding questions") {
		t.Errorf("stderr = %q, want 'no outstanding questions'", errBuf.String())
	}
}

func TestCmdAnswer_WrongArity_Zero(t *testing.T) {
	app, _, errBuf, _, _ := answerTestSetup(t)

	code := app.cmdAnswer(context.Background(), nil)
	if code != 2 {
		t.Fatalf("cmdAnswer = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "need") {
		t.Errorf("stderr = %q, want usage error", errBuf.String())
	}
}

func TestCmdAnswer_WrongArity_One(t *testing.T) {
	app, _, errBuf, _, _ := answerTestSetup(t)

	code := app.cmdAnswer(context.Background(), []string{"1"})
	if code != 2 {
		t.Fatalf("cmdAnswer = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "need") {
		t.Errorf("stderr = %q, want usage error", errBuf.String())
	}
}

func TestCmdAnswer_WrongArity_Three(t *testing.T) {
	app, _, errBuf, _, _ := answerTestSetup(t)

	code := app.cmdAnswer(context.Background(), []string{"1", "text", "extra"})
	if code != 2 {
		t.Fatalf("cmdAnswer = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unexpected") {
		t.Errorf("stderr = %q, want usage error mentioning unexpected", errBuf.String())
	}
}

func TestCmdAnswer_JSONOutput(t *testing.T) {
	app, out, errBuf, _, itemID := answerTestSetup(t)

	code := app.cmdAnswer(context.Background(), []string{"--json", itemID, "yes"})
	if code != 0 {
		t.Fatalf("cmdAnswer = %d, want 0; stderr=%q", code, errBuf.String())
	}

	var payload answerPayload
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v; raw=%q", err, out.String())
	}
	if !payload.Answered {
		t.Error("Answered = false, want true")
	}
	if payload.Item == "" {
		t.Error("Item is empty")
	}
	if payload.QuestionID == "" {
		t.Error("QuestionID is empty")
	}
}

func TestCmdAnswer_BadQuestionFlag(t *testing.T) {
	app, _, errBuf, _, itemID := answerTestSetup(t)

	code := app.cmdAnswer(context.Background(), []string{itemID, "yes", "--question", "nonexistent"})
	if code != 1 {
		t.Fatalf("cmdAnswer = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "not found") {
		t.Errorf("stderr = %q, want 'not found'", errBuf.String())
	}
}

// There are no optional capabilities: an orchestrator that cannot record an
// answer implements PostAnswer and REFUSES. `answer` must report the refusal
// and exit non-zero — an operator who typed an answer needs to know it landed
// nowhere.
func TestCmdAnswer_OrchestratorRefusesToRecordAnswers(t *testing.T) {
	app, _, _, _, itemID := answerTestSetup(t)
	app.Orchestrator = refusingAnswerBackend{app.Orchestrator}

	var errBuf bytes.Buffer
	app.Err = &errBuf

	code := app.cmdAnswer(context.Background(), []string{itemID, "yes"})
	if code != 1 {
		t.Fatalf("cmdAnswer = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "answer") {
		t.Errorf("stderr = %q, want the refusal reported", errBuf.String())
	}
}

// refusingAnswerBackend answers the question the contract asks — "can you do
// this?" — with a typed no.
type refusingAnswerBackend struct {
	flow.Orchestrator
}

func (b refusingAnswerBackend) PostAnswer(context.Context, flow.ItemRef, flow.QuestionId, string) error {
	return fmt.Errorf("this orchestrator records no answers: %w", flow.ErrUnsupported)
}

// An item that cannot be loaded cannot be answered: the question ids live on
// it, so there is nothing to match --question against and nothing to record.
func TestCmdAnswer_LoadFailureIsReported(t *testing.T) {
	app, _, _, _, itemID := answerTestSetup(t)
	app.Orchestrator = unloadableBackend{app.Orchestrator}

	var errBuf bytes.Buffer
	app.Err = &errBuf

	code := app.cmdAnswer(context.Background(), []string{itemID, "yes"})
	if code != 1 {
		t.Fatalf("cmdAnswer = %d, want 1", code)
	}
	if errBuf.Len() == 0 {
		t.Error("stderr is empty, want the load failure reported")
	}
}

type unloadableBackend struct {
	flow.Orchestrator
}

func (b unloadableBackend) Load(context.Context, flow.ItemRef) (*flow.Item, error) {
	return nil, errors.New("orchestrator unreachable")
}

// TestCmdAnswer_PostAnswerError verifies that when PostAnswer returns an error,
// cmdAnswer exits 1 and reports the error on stderr.
func TestCmdAnswer_PostAnswerError(t *testing.T) {
	app, _, _, _, itemID := answerTestSetup(t)

	// Replace backend with one whose PostAnswer always fails.
	app.Orchestrator = failingAnswerBackend{app.Orchestrator}

	var errBuf bytes.Buffer
	app.Out = newDiscardWriter()
	app.Err = &errBuf

	code := app.cmdAnswer(context.Background(), []string{itemID, "yes"})
	if code != 1 {
		t.Fatalf("cmdAnswer = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "backend broke") {
		t.Errorf("stderr = %q, want mention of 'backend broke'", errBuf.String())
	}
}

// failingAnswerBackend provides a PostAnswer that always errors, so the
// command's own reporting of a failed write is what is under test.
type failingAnswerBackend struct {
	flow.Orchestrator
}

func (b failingAnswerBackend) PostAnswer(_ context.Context, _ flow.ItemRef, _ flow.QuestionId, _ string) error {
	return fmt.Errorf("backend broke")
}
