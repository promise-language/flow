package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/backend/fake"
)

// answerTestSetup builds an App + claim with one pending question on the item.
// Returns the app (with captured stdout/stderr), the fake backend, and the
// item ID.
func answerTestSetup(t *testing.T) (*App, *bytes.Buffer, *bytes.Buffer, *fake.Backend, string) {
	t.Helper()
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, a)

	// Ask a question so there is something to answer.
	if _, err := be.AskQuestions(context.Background(), claim, []flow.AgentQuestion{
		{Text: "should we re-plan?"},
	}); err != nil {
		t.Fatalf("AskQuestions: %v", err)
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
	ref := flow.ItemRef{BackendName: "fake", Display: itemID, Ref: json.RawMessage(`"` + itemID + `"`)}
	st, err := be.LoadStateByRef(context.Background(), ref)
	if err != nil {
		t.Fatalf("LoadStateByRef: %v", err)
	}
	pending := st.PendingQuestions()
	if len(pending) != 0 {
		t.Errorf("PendingQuestions = %d, want 0", len(pending))
	}
}

func TestCmdAnswer_MultipleQuestions_NoFlag(t *testing.T) {
	app, _, errBuf, be, itemID := answerTestSetup(t)

	// Add a second question.
	claim, err := be.LookupActiveClaim(context.Background(), "alice")
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if _, err := be.AskQuestions(context.Background(), *claim, []flow.AgentQuestion{
		{Text: "what approach?"},
	}); err != nil {
		t.Fatalf("AskQuestions: %v", err)
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
	claim, err := be.LookupActiveClaim(context.Background(), "alice")
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	qs, err := be.AskQuestions(context.Background(), *claim, []flow.AgentQuestion{
		{Text: "what approach?"},
	})
	if err != nil {
		t.Fatalf("AskQuestions: %v", err)
	}
	targetID := qs[0].ID

	code := app.cmdAnswer(context.Background(), []string{itemID, "option B", "--question", targetID})
	if code != 0 {
		t.Fatalf("cmdAnswer = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), targetID) {
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

// TestCmdAnswer_BackendLacksQuestionAnswerer verifies the error path when the
// backend does not implement QuestionAnswerer.
func TestCmdAnswer_BackendLacksQuestionAnswerer(t *testing.T) {
	app, _, _, _, itemID := answerTestSetup(t)

	// Replace backend with one that lacks QuestionAnswerer.
	app.Backend = noAnswerBackend{app.Backend}

	var errBuf bytes.Buffer
	app.Err = &errBuf

	code := app.cmdAnswer(context.Background(), []string{itemID, "yes"})
	if code != 1 {
		t.Fatalf("cmdAnswer = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "does not support") {
		t.Errorf("stderr = %q, want 'does not support'", errBuf.String())
	}
}

// noAnswerBackend wraps a Backend but hides QuestionAnswerer.
type noAnswerBackend struct {
	flow.Backend
}

// TestCmdAnswer_BackendLacksStateInspector verifies the error path when the
// backend does not implement StateInspector.
func TestCmdAnswer_BackendLacksStateInspector(t *testing.T) {
	app, _, _, _, itemID := answerTestSetup(t)

	// Replace backend with one that lacks StateInspector.
	app.Backend = noInspectorBackend{app.Backend}

	var errBuf bytes.Buffer
	app.Err = &errBuf

	code := app.cmdAnswer(context.Background(), []string{itemID, "yes"})
	if code != 1 {
		t.Fatalf("cmdAnswer = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "cannot inspect") {
		t.Errorf("stderr = %q, want 'cannot inspect'", errBuf.String())
	}
}

// noInspectorBackend wraps a Backend and provides QuestionAnswerer but hides
// StateInspector.
type noInspectorBackend struct {
	flow.Backend
}

func (b noInspectorBackend) PostAnswer(ctx context.Context, ref flow.ItemRef, text string) error {
	return nil
}

func (b noInspectorBackend) ClearQuestionMarker(ctx context.Context, ref flow.ItemRef) {}
