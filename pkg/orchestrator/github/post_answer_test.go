package github

import (
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// PostAnswer records a person's answer AGAINST THE QUESTION IT ANSWERS.
//
// It used to post a comment and drop the needs-answer marker without touching
// the Question at all, so Question.Answer stayed empty, PendingQuestions kept
// returning what had just been answered, and answering one of three cleared the
// marker while two remained. These are the paths that make it a record rather
// than a gesture.

// countAnswerComments returns how many non-machine comments the issue carries —
// the answer comments a human reply scan would pick up.
func countAnswerComments(mock *ghMock) int {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	n := 0
	for _, c := range mock.comments {
		if !isFlowMachineComment(c.Body) {
			n++
		}
	}
	return n
}

// THE OUTSTANDING-QUESTION MARKER CLEARS ONLY WHEN NO PENDING QUESTION REMAINS.
// Answering one of three is not answering the item, and clearing on the first
// resumes a flow still waiting on two.
func TestBackend_TheAnswerMarkerClearsOnlyWithTheLastQuestion(t *testing.T) {
	mock, b, claim := newQuestionEnv(t)
	asked := []flow.Question{
		askOne(t, b, claim, flow.AskText("base", "which base branch?")),
		askOne(t, b, claim, flow.AskYesNo("drop --yes?", "issue #111 asks to remove it")),
		askOne(t, b, claim, flow.AskText("naming", "what should the flag be called?")),
	}
	parkOnQuestion(t, b, claim)

	for i, q := range asked {
		if err := b.PostAnswer(t.Context(), claim.ItemRef, q.ID, "answer "+q.Header); err != nil {
			t.Fatalf("PostAnswer %d: %v", i, err)
		}
		item, err := b.Load(t.Context(), claim.ItemRef)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		wantPending := len(asked) - i - 1
		if got := len(item.PendingQuestions()); got != wantPending {
			t.Fatalf("after answering %d of %d: PendingQuestions = %d, want %d",
				i+1, len(asked), got, wantPending)
		}
		marked := hasLabel(mock.labelNames(), b.labels.NeedsAnswer())
		if wantPending > 0 && !marked {
			t.Errorf("the needs-answer marker cleared with %d question(s) still pending — "+
				"the flow would resume still waiting on them", wantPending)
		}
		if wantPending == 0 && marked {
			t.Error("the needs-answer marker survived the last answer")
		}
		// The park is what the questions were waiting on, so it clears with the
		// last of them and not before.
		if item.Parked() != (wantPending > 0) {
			t.Errorf("parked = %v with %d pending — the park clears with the last question",
				item.Parked(), wantPending)
		}
	}
}

// An unknown id is REFUSED, and nothing is published. Validating before the
// comment is posted is the point: an answer comment posted against a question
// that does not exist is a disclosure that cannot be taken back.
func TestBackend_PostAnswer_RefusesAnUnknownQuestionWithoutPublishing(t *testing.T) {
	mock, b, claim := newQuestionEnv(t)
	askOne(t, b, claim, flow.AskText("base", "which base branch?"))
	before := countAnswerComments(mock)

	err := b.PostAnswer(t.Context(), claim.ItemRef, "no-such-question", "main")
	if err == nil {
		t.Fatal("PostAnswer accepted an id naming no question on the item")
	}
	if !strings.Contains(err.Error(), "no-such-question") {
		t.Errorf("error = %q, want it to name the id that matched nothing", err)
	}
	if got := countAnswerComments(mock); got != before {
		t.Errorf("answer comments = %d, want %d — a refusal published anyway", got, before)
	}
	// And the marker stays up: nothing was answered.
	if !hasLabel(mock.labelNames(), b.labels.NeedsAnswer()) {
		t.Error("the needs-answer marker cleared although no question was answered")
	}
}

// Answering an already-answered question is refused rather than silently
// accepted: accepting it would report an answer that moved nothing, and would
// overwrite what the first person said.
func TestBackend_PostAnswer_RefusesAQuestionAlreadyAnswered(t *testing.T) {
	mock, b, claim := newQuestionEnv(t)
	q := askOne(t, b, claim, flow.AskText("base", "which base branch?"))

	if err := b.PostAnswer(t.Context(), claim.ItemRef, q.ID, "main"); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}
	before := countAnswerComments(mock)

	err := b.PostAnswer(t.Context(), claim.ItemRef, q.ID, "actually, the release branch")
	if err == nil {
		t.Fatal("PostAnswer accepted a second answer to one question")
	}
	if !strings.Contains(err.Error(), "already answered") {
		t.Errorf("error = %q, want it to say the question is already answered", err)
	}
	if got := countAnswerComments(mock); got != before {
		t.Errorf("answer comments = %d, want %d — the refused second answer was published", got, before)
	}
	item, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if item.Questions[0].Answer != "main" {
		t.Errorf("Answer = %q, want the first answer kept", item.Questions[0].Answer)
	}
}
