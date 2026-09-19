package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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
	// A non-file stdin, so every test here is NON-INTERACTIVE unless it says
	// otherwise. Left nil it would be whatever `go test` hands the process —
	// /dev/null on macOS, which is a character device and so reads as a
	// terminal, making the interactive branch fire in a test that never asked
	// for it.
	app.In = strings.NewReader("")
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
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
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

// ZERO POSITIONALS IS THE BARE FORM, and with no claim held it is the one
// invocation that cannot be resolved: there is no item for an answer to be
// about. It refuses by saying what to type, exit 1 — a condition to clear, not
// a malformed invocation.
func TestCmdAnswer_BareWithNoClaimNamesTheMissingItem(t *testing.T) {
	app, _, errBuf, be, itemID := answerTestSetup(t)
	releaseClaim(t, be, itemID)

	code := app.cmdAnswer(context.Background(), nil)
	if code != 1 {
		t.Fatalf("cmdAnswer = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "no active claim") {
		t.Errorf("stderr = %q, want the missing claim named", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "answer <item-id>") {
		t.Errorf("stderr = %q, want it to say what to type instead", errBuf.String())
	}
}

// ONE POSITIONAL WITH NO CLAIM IS THE ITEM ID. There is no held item for an
// answer to be about, so the argument can only be naming one — and what
// follows is the read form, which prints the question rather than posting it.
func TestCmdAnswer_OnePositionalWithNoClaimIsTheItemId(t *testing.T) {
	app, out, errBuf, be, itemID := answerTestSetup(t)
	releaseClaim(t, be, itemID)

	code := app.cmdAnswer(context.Background(), []string{itemID})
	if code != 0 {
		t.Fatalf("cmdAnswer = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "should we re-plan?") {
		t.Errorf("the question was not printed:\n%s", out.String())
	}
	// It READ; it did not answer. Nothing was posted.
	ref, err := be.ResolveRef(context.Background(), itemID)
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	item, err := be.Load(context.Background(), ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(item.PendingQuestions()) != 1 {
		t.Errorf("a read posted an answer: %+v", item.Questions)
	}
}

// releaseClaim drops the arena's lease, for the tests about an operator who
// holds nothing — a maintainer or a passer-by, who `answer` is required to
// serve from any machine.
func releaseClaim(t *testing.T, be *fake.Orchestrator, itemID string) {
	t.Helper()
	ref, err := be.ResolveRef(context.Background(), itemID)
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if err := be.Release(context.Background(), ref, nil); err != nil {
		t.Fatalf("Release: %v", err)
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

// asTerminal makes the app read `reply` from what it treats as an operator's
// terminal.
//
// The interactivity test is substituted rather than faked with a pipe: a pipe
// is not a character device, which is the whole distinction the real check
// makes, so a pipe would silently exercise the NON-interactive branch and the
// test would pass while proving nothing.
func asTerminal(t *testing.T, app *App, reply string) {
	t.Helper()
	app.In = strings.NewReader(reply)
	prev := isTerminal
	isTerminal = func(r io.Reader) bool { return r == app.In }
	t.Cleanup(func() { isTerminal = prev })
}

// BARE ANSWER, NON-INTERACTIVE, PRINTS AND NEVER BLOCKS. A prompt on a piped
// stdin waits for input that is not coming, which is a hang rather than a
// report — so the questions are printed and the command returns.
func TestCmdAnswer_BareNonInteractivePrintsAndDoesNotPost(t *testing.T) {
	app, out, errBuf, be, itemID := answerTestSetup(t)

	if code := app.cmdAnswer(context.Background(), nil); code != 0 {
		t.Fatalf("cmdAnswer = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "should we re-plan?") {
		t.Errorf("the pending question was not printed:\n%s", out.String())
	}
	// The unanswered one shows an EMPTY answer rather than being omitted: what
	// the operator is looking for is the question still waiting.
	if !strings.Contains(out.String(), "answer:") {
		t.Errorf("the empty answer line is missing:\n%s", out.String())
	}
	assertStillPending(t, be, itemID, 1)
}

// BARE ANSWER ON A TERMINAL shows the question and takes the reply: one
// command, with no item id and no question id to copy from anywhere.
func TestCmdAnswer_BareInteractiveReadsAndPosts(t *testing.T) {
	app, out, errBuf, be, itemID := answerTestSetup(t)
	asTerminal(t, app, "yes, re-plan\n")

	if code := app.cmdAnswer(context.Background(), nil); code != 0 {
		t.Fatalf("cmdAnswer = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "should we re-plan?") {
		t.Errorf("the question was not shown before the prompt:\n%s", out.String())
	}
	assertStillPending(t, be, itemID, 0)
	if got := answerOn(t, be, itemID); got != "yes, re-plan" {
		t.Errorf("recorded answer = %q, want the operator's reply", got)
	}
}

// An operator who was asked and said NOTHING has decided not to answer.
// Recording an empty answer would clear the park on a question nobody settled,
// so the question stays pending and the exit code says so.
func TestCmdAnswer_AnEmptyReplyRecordsNothing(t *testing.T) {
	app, _, errBuf, be, itemID := answerTestSetup(t)
	asTerminal(t, app, "\n")

	if code := app.cmdAnswer(context.Background(), nil); code != 1 {
		t.Fatalf("cmdAnswer = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "nothing was recorded") {
		t.Errorf("stderr = %q, want it to say nothing was recorded", errBuf.String())
	}
	assertStillPending(t, be, itemID, 1)
}

// A STDIN THAT CANNOT BE READ IS REPORTED, and nothing is recorded. The
// operator was shown the question and asked for a reply; a read that failed is
// not a reply, and inventing one from the error would clear the park on a
// question nobody settled.
func TestCmdAnswer_AnUnreadableStdinIsReported(t *testing.T) {
	app, _, errBuf, be, itemID := answerTestSetup(t)
	asTerminal(t, app, "")
	app.In = failingReader{}
	prev := isTerminal
	isTerminal = func(r io.Reader) bool { return r == app.In }
	t.Cleanup(func() { isTerminal = prev })

	if code := app.cmdAnswer(context.Background(), nil); code != 1 {
		t.Fatalf("cmdAnswer = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "stdin is gone") {
		t.Errorf("stderr = %q, want the read failure named", errBuf.String())
	}
	assertStillPending(t, be, itemID, 1)
}

// AN ANSWER WITH NO TRAILING NEWLINE IS STILL THE ANSWER. The read ends in EOF
// with the line already in hand, and a reader that returned the error instead
// of the bytes would drop what the operator typed and report that they said
// nothing.
func TestCmdAnswer_AReplyWithoutATrailingNewlineIsKept(t *testing.T) {
	app, _, errBuf, be, itemID := answerTestSetup(t)
	asTerminal(t, app, "yes, re-plan") // no "\n"

	if code := app.cmdAnswer(context.Background(), nil); code != 0 {
		t.Fatalf("cmdAnswer = %d, want 0; stderr=%q", code, errBuf.String())
	}
	assertStillPending(t, be, itemID, 0)
	if got := answerOn(t, be, itemID); got != "yes, re-plan" {
		t.Errorf("recorded answer = %q, want the operator's reply", got)
	}
}

// failingReader is a stdin that has gone away mid-prompt — a closed pipe, a
// terminal that vanished — which is neither an answer nor an empty one.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("stdin is gone (injected)") }

// ONE POSITIONAL WITH A CLAIM IS THE ANSWER TEXT. The arena holds the item, so
// there is nothing for an id to disambiguate — and requiring one is exactly the
// friction the bare form removes.
func TestCmdAnswer_OnePositionalWithAClaimIsTheAnswerText(t *testing.T) {
	app, _, errBuf, be, itemID := answerTestSetup(t)

	if code := app.cmdAnswer(context.Background(), []string{"yes, re-plan"}); code != 0 {
		t.Fatalf("cmdAnswer = %d, want 0; stderr=%q", code, errBuf.String())
	}
	assertStillPending(t, be, itemID, 0)
	if got := answerOn(t, be, itemID); got != "yes, re-plan" {
		t.Errorf("recorded answer = %q, want the positional", got)
	}
}

// …unless it NAMES AN ITEM, which is how a different item stays reachable while
// a claim is held. The explicit id wins, and what follows is the read form.
func TestCmdAnswer_OnePositionalNamingAnItemWinsOverTheClaim(t *testing.T) {
	app, out, errBuf, be, itemID := answerTestSetup(t)

	if code := app.cmdAnswer(context.Background(), []string{itemID}); code != 0 {
		t.Fatalf("cmdAnswer = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "should we re-plan?") {
		t.Errorf("the named item's question was not printed:\n%s", out.String())
	}
	assertStillPending(t, be, itemID, 1)
}

// --answered PRINTS THE WHOLE HISTORY: every question and its answer, in order,
// not only the outstanding one. A question already answered once, in different
// words, is invisible otherwise — to the operator and to the asking step alike.
func TestCmdAnswer_AnsweredPrintsTheWholeHistoryInOrder(t *testing.T) {
	app, out, errBuf, be, itemID := answerTestSetup(t)
	ref := refFor(t, be, itemID)

	// The first question, answered; then a second still waiting.
	item, err := be.Load(context.Background(), ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := be.PostAnswer(context.Background(), ref, item.Questions[0].ID, "yes, re-plan"); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}
	if _, err := be.AskQuestion(context.Background(), ref, flow.AskText("which base?", "main or release?")); err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	if code := app.cmdAnswer(context.Background(), []string{"--answered"}); code != 0 {
		t.Fatalf("cmdAnswer --answered = %d; stderr=%q", code, errBuf.String())
	}
	got := out.String()
	if !strings.Contains(got, "should we re-plan?") || !strings.Contains(got, "yes, re-plan") {
		t.Errorf("the answered question and its answer are missing:\n%s", got)
	}
	if !strings.Contains(got, "which base?") {
		t.Errorf("the outstanding question is missing:\n%s", got)
	}
	// IN ORDER: the history is a sequence, and out of order it answers a
	// different question about what was decided when.
	if strings.Index(got, "should we re-plan?") > strings.Index(got, "which base?") {
		t.Errorf("the history is out of order:\n%s", got)
	}
}

// A JSON RENDERING IS NON-INTERACTIVE WHATEVER STDIN IS, so stdout carries the
// payload and nothing else.
//
// The two ordinary ways to ask for the machine form from a terminal — `answer
// --json`, and `answer` with stdout piped — both leave stdin a terminal, so an
// interactivity test that consults only stdin prompts anyway. The question and
// the "your answer:" prompt are prose written to stdout, which is where the
// payload goes: what comes out is text with an object after it, and no decoder
// can read it.
func TestCmdAnswer_JSONModeNeverPromptsOnStdout(t *testing.T) {
	app, out, errBuf, be, itemID := answerTestSetup(t)
	asTerminal(t, app, "yes, re-plan\n")

	if code := app.cmdAnswer(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("cmdAnswer = %d; stderr=%q", code, errBuf.String())
	}
	var payload answerPayload
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not the payload in JSON mode: %v\n%s", err, out.String())
	}
	if len(payload.Questions) != 1 || payload.Questions[0].Text != "should we re-plan?" {
		t.Errorf("the reading form's payload is missing the question: %+v", payload.Questions)
	}
	// It READ; it did not answer. A mode that cannot prompt cannot have been
	// given a reply, so nothing may be recorded from one.
	assertStillPending(t, be, itemID, 1)
}

// The read forms reach --json too, so a tool can read what the flow is waiting
// on without parsing prose.
func TestCmdAnswer_JSONCarriesTheQuestions(t *testing.T) {
	app, out, _, _, _ := answerTestSetup(t)

	if code := app.cmdAnswer(context.Background(), []string{"--answered", "--json"}); code != 0 {
		t.Fatalf("cmdAnswer = %d", code)
	}
	var payload answerPayload
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal %s: %v", out.String(), err)
	}
	if len(payload.Questions) != 1 {
		t.Fatalf("questions = %d, want 1", len(payload.Questions))
	}
	if payload.Questions[0].Text != "should we re-plan?" || payload.Questions[0].Answered {
		t.Errorf("question payload wrong: %+v", payload.Questions[0])
	}
}

// An item with NO QUESTIONS AT ALL has nothing to show, and says so rather than
// printing an empty list a reader would take for an answered one.
func TestCmdAnswer_AnsweredOnAnItemWithNoQuestions(t *testing.T) {
	app, _, errBuf, be, itemID := answerTestSetup(t)
	ref := refFor(t, be, itemID)
	item, err := be.Load(context.Background(), ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Answer the only question, then ask about an item that has none.
	if err := be.PostAnswer(context.Background(), ref, item.Questions[0].ID, "yes"); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}
	be.AddItem("2", flow.Item{Type: "task", Title: "untouched"})

	if code := app.cmdAnswer(context.Background(), []string{"2", "--answered"}); code != 1 {
		t.Fatalf("cmdAnswer = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "no questions") {
		t.Errorf("stderr = %q, want it to say there are none", errBuf.String())
	}
}

// refFor resolves a display id the fixture handed back.
func refFor(t *testing.T, be *fake.Orchestrator, itemID string) flow.ItemRef {
	t.Helper()
	ref, err := be.ResolveRef(context.Background(), itemID)
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	return ref
}

// assertStillPending checks how many questions the item is still waiting on.
func assertStillPending(t *testing.T, be *fake.Orchestrator, itemID string, want int) {
	t.Helper()
	item, err := be.Load(context.Background(), refFor(t, be, itemID))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(item.PendingQuestions()); got != want {
		t.Errorf("pending questions = %d, want %d", got, want)
	}
}

// answerOn returns the recorded answer to the item's first question.
func answerOn(t *testing.T, be *fake.Orchestrator, itemID string) string {
	t.Helper()
	item, err := be.Load(context.Background(), refFor(t, be, itemID))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(item.Questions) == 0 {
		t.Fatal("no questions on the item")
	}
	return item.Questions[0].Answer
}
