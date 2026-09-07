package github

import (
	"strings"
	"testing"
	"time"

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
		// The park is not the marker and does not go with it. It records which
		// step stopped and carries the asked-at window the resumed step reads
		// its answers through, so it stands through every answer — including
		// the last, the one that would otherwise delete its own delivery.
		if !item.Parked() {
			t.Errorf("the park cleared after answering %d of %d — the resume has no answer window left",
				i+1, len(asked))
		}
	}
}

// An unknown id is REFUSED, and nothing is published. Validating before the
// comment is posted is the point: an answer comment posted against a question
// that does not exist is a disclosure that cannot be taken back.
func TestBackend_PostAnswer_RefusesAnUnknownQuestionWithoutPublishing(t *testing.T) {
	mock, b, claim := newQuestionEnv(t)
	asked := askOne(t, b, claim, flow.AskText("base", "which base branch?"))
	parkOnQuestionAsked(t, b, claim, asked)
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
	// So does the park, with the window it carries — a refusal moves nothing.
	item, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !item.Parked() {
		t.Fatal("the park cleared on a refused answer")
	}
	if got := flow.QuestionAskedAt(item.Park); !got.Equal(asked.AskedAt) {
		t.Errorf("asked-at = %v, want %v — a refusal rewrote the answer window", got, asked.AskedAt)
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

// THE ANSWER MUST NOT DELETE ITS OWN DELIVERY.
//
// PostAnswer used to clear the question park along with the marker. The park
// carries `asked-at`, and that stamp is the ONLY window a resumed step has for
// finding replies: answersFor returns nil without a question park, so the
// resumed step got an empty answers block, re-derived the same question, minted
// a fresh id and parked again — one full agent turn per resume, forever.
func TestBackend_PostAnswerLeavesTheQuestionParkForTheResume(t *testing.T) {
	_, b, claim := newQuestionEnv(t)
	asked := askOne(t, b, claim, flow.AskText("base", "which base branch?"))
	parkOnQuestionAsked(t, b, claim, asked)

	if err := b.PostAnswer(t.Context(), claim.ItemRef, asked.ID, "main"); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}
	item, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if item.Park == nil {
		t.Fatal("the park cleared with the answer — the resume cannot tell which step stopped, or when")
	}
	if item.Park.Kind != flow.ParkQuestion {
		t.Errorf("park kind = %q, want %q", item.Park.Kind, flow.ParkQuestion)
	}
	// The assertion that matters: the window, not merely the park object. A
	// park that survived with a rewritten or missing stamp reads every comment
	// on the item, or none.
	if got := flow.QuestionAskedAt(item.Park); !got.Equal(asked.AskedAt) {
		t.Errorf("asked-at = %v, want %v (the ask time the answers are compared against)", got, asked.AskedAt)
	}
}

// The two halves in one test, which is the sequence a resume actually performs:
// ask → park → answer → read the answers through the park's window. Nothing
// covered it end to end, which is why the loop was invisible here.
func TestBackend_AnsweringThenReadingComposesTheResume(t *testing.T) {
	_, b, claim := newQuestionEnv(t)
	asked := askOne(t, b, claim, flow.AskText("base", "which base branch?"))
	parkOnQuestionAsked(t, b, claim, asked)

	if err := b.PostAnswer(t.Context(), claim.ItemRef, asked.ID, "main — the release branch is cut from it"); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}
	item, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if item.Park == nil {
		t.Fatal("no park after the answer, so the resume has no window to read through")
	}

	// `self` is the login the mock posts every comment under, deliberately:
	// exclusion is by marker, not by author, because the common case is a human
	// running the flow under their own token.
	answers, err := b.ReadAnswers(t.Context(), *item, flow.QuestionAskedAt(item.Park), "alice")
	if err != nil {
		t.Fatalf("ReadAnswers: %v", err)
	}
	if len(answers) != 1 {
		t.Fatalf("ReadAnswers = %d answer(s), want exactly the one that was posted: %+v", len(answers), answers)
	}
	if !strings.Contains(answers[0].Answer, "main — the release branch is cut from it") {
		t.Errorf("answer = %q, want the text PostAnswer published", answers[0].Answer)
	}
}

// A PARK THAT SURVIVES THE ANSWER MUST NOT KEEP THE ITEM BLOCKED.
//
// The item has to become workable again, or the fix trades one stall for
// another: blocked items are skipped by selection, so the step that asked would
// never resume, so nothing would ever drop the park it is waiting behind. Here
// that holds because blockedness is derived from the LABELS and PostAnswer drops
// the needs-answer one — a derivation that also consulted `park`, the obvious
// reading of a park that is still there, would reintroduce the stall with every
// existing test still green.
func TestBackend_AnsweringUnblocksTheItemThoughTheParkStands(t *testing.T) {
	_, b, claim := newQuestionEnv(t)
	asked := askOne(t, b, claim, flow.AskText("base", "which base branch?"))
	parkOnQuestionAsked(t, b, claim, asked)

	item, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if item.BlockReason == "" {
		t.Fatal("an item with an unanswered question reports no block reason")
	}

	if err := b.PostAnswer(t.Context(), claim.ItemRef, asked.ID, "main"); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}
	item, err = b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if item.BlockReason != "" {
		t.Errorf("BlockReason = %q after the answer, want none — the human has acted", item.BlockReason)
	}
	if !item.Parked() {
		t.Error("the park cleared with the answer — the resume has no answer window left")
	}
}

// seedComment puts a comment on the issue with a CHOSEN creation time, which
// posting through the API cannot do: the mock stamps a POST with its own clock.
// The window ReadAnswers applies is a comparison against that stamp, so a test
// about the window has to be able to place a comment on either side of it.
func seedComment(t *testing.T, mock *ghMock, body string, at time.Time) {
	t.Helper()
	mock.mu.Lock()
	defer mock.mu.Unlock()
	mock.nextCommentID++
	mock.comments = append(mock.comments, ghMockComment{
		ID: mock.nextCommentID, Body: body, User: "carol", CreatedAt: at,
	})
}

// THE WINDOW IS WHAT MAKES THE PARK WORTH KEEPING, so it has to actually
// exclude.
//
// The park is kept through the answer because it carries `asked-at`; that stamp
// earns its keep only if replies at or before it are dropped. A thread carries
// prose from long before the question ("any update?", the reporter's original
// comment), and GitHub's own `since` filter is inclusive to the second, so the
// question's own second leaks through unless the backend drops it itself — the
// mock, like the API, hands back everything and lets the caller filter. Counting
// either as an answer clears the gate with nobody having decided, and the step
// resumes only to ask again: the reported loop, reached from the other side.
func TestBackend_ReadAnswersTakesOnlyRepliesAfterTheAsk(t *testing.T) {
	mock, b, claim := newQuestionEnv(t)
	asked := askOne(t, b, claim, flow.AskText("base", "which base branch?"))
	parkOnQuestionAsked(t, b, claim, asked)

	seedComment(t, mock, "any update on this?", asked.AskedAt.Add(-time.Hour))
	seedComment(t, mock, "posted in the very second of the ask", asked.AskedAt)

	if err := b.PostAnswer(t.Context(), claim.ItemRef, asked.ID, "main"); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}
	item, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	answers, err := b.ReadAnswers(t.Context(), *item, flow.QuestionAskedAt(item.Park), "alice")
	if err != nil {
		t.Fatalf("ReadAnswers: %v", err)
	}
	var got []string
	for _, a := range answers {
		got = append(got, a.Answer)
	}
	// Matched loosely rather than exactly: what is under test is which comments
	// pass the window, not how a comment body is framed.
	if len(got) != 1 || !strings.Contains(got[0], "main") {
		t.Errorf("ReadAnswers = %q, want only the reply posted after the ask", got)
	}
}

// The boundary on the other side: the park now outlives the answer, but it is
// still dropped when the step it belongs to completes — one of the three
// triggers docs/github-schema.md:113 names. Without this the change would read
// as "the question park never clears".
func TestBackend_TheQuestionParkClearsWhenTheAskingStepResolves(t *testing.T) {
	mock, b, claim := newQuestionEnv(t)
	first := askOne(t, b, claim, flow.AskText("base", "which base branch?"))
	askOne(t, b, claim, flow.AskText("naming", "what should the flag be called?"))
	parkOnQuestionAsked(t, b, claim, first)

	// One of two answered: the park and the marker both stand.
	if err := b.PostAnswer(t.Context(), claim.ItemRef, first.ID, "main"); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}
	item, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !item.Parked() {
		t.Fatal("the park cleared on the answer")
	}
	if !hasLabel(mock.labelNames(), b.labels.NeedsAnswer()) {
		t.Fatal("the needs-answer marker cleared with a question still pending")
	}

	if err := b.ResolveArtifact(t.Context(), claim.ItemRef, "plan", flow.ArtifactBody{
		Type: flow.ArtifactMarkdown, Markdown: "the plan",
	}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}
	item, err = b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if item.Parked() {
		t.Errorf("park = %+v, want dropped by the step it was recorded against", item.Park)
	}
	if hasLabel(mock.labelNames(), b.labels.NeedsAnswer()) {
		t.Error("the park label outlived the park it advertises")
	}
}
