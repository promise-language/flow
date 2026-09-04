package github

import (
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// newQuestionEnv claims and seeds issue 42 — the state the ask half of
// park-for-answer always runs in, since the mandatory-seed gate precedes any
// handler that could ask.
func newQuestionEnv(t *testing.T) (*ghMock, *Orchestrator, flow.Claim) {
	t.Helper()
	return newQuestionEnvSeeded(t, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true,
			Budget: flow.StepBudget{MaxInvocations: 3, MaxCostUSD: 10}},
	})
}

// newQuestionEnvSeeded is newQuestionEnv with the checklist named, for a test
// that needs a step other than the one the park is against.
func newQuestionEnvSeeded(t *testing.T, specs []flow.ArtifactSpec) (*ghMock, *Orchestrator, flow.Claim) {
	t.Helper()
	mock := newGHMock(t)
	srv := mock.server()
	t.Cleanup(srv.Close)
	b := newMockedOrchestrator(t, mock, srv)

	ctx := t.Context()
	claim, err := b.Claim(ctx, b.refFromIssue(42), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SeedState(ctx, claim.ItemRef, specs); err != nil {
		t.Fatalf("SeedState: %v", err)
	}
	return mock, b, claim
}

func askOne(t *testing.T, b *Orchestrator, claim flow.Claim, q flow.AgentQuestion) flow.Question {
	t.Helper()
	recorded, err := b.AskQuestion(t.Context(), claim.ItemRef, q)
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}
	return recorded
}

func parkOnQuestion(t *testing.T, b *Orchestrator, claim flow.Claim) {
	t.Helper()
	if err := b.Park(t.Context(), claim.ItemRef, flow.ParkRequest{
		Kind: flow.ParkQuestion, Step: "plan", Reason: "question: should the doc be amended?",
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}
}

// The reported defect: a question park Load returns no question for is a
// park `answer` cannot clear, because it has no id to name. The whole payload
// must survive the round trip through the state comment.
func TestBackend_QuestionRoundTripsThroughStateComment(t *testing.T) {
	_, b, claim := newQuestionEnv(t)

	asked := askOne(t, b, claim, flow.AskChoice("amend the doc?",
		"Issue #111 asks to remove `--yes`, but docs/release.md §11 says it skips.\n\nWhich wins?",
		"amend the doc", "keep the flag"))
	parkOnQuestion(t, b, claim)

	state, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Questions) != 1 {
		t.Fatalf("Questions = %d, want 1", len(state.Questions))
	}
	got := state.Questions[0]
	if got.ID != asked.ID {
		t.Errorf("ID = %q, want %q", got.ID, asked.ID)
	}
	if got.Header != asked.Header || got.Text != asked.Text {
		t.Errorf("payload = %q / %q, want %q / %q", got.Header, got.Text, asked.Header, asked.Text)
	}
	if got.Format != flow.FormatChoice {
		t.Errorf("Format = %q, want %q", got.Format, flow.FormatChoice)
	}
	if strings.Join(got.Options, ",") != "amend the doc,keep the flag" {
		t.Errorf("Options = %v, want the two asked", got.Options)
	}
	// The backend's own clock — the one the replies are stamped by, and the
	// only one an answer scan can be compared against.
	if asked.AskedAt.IsZero() {
		t.Fatal("AskQuestions returned a zero AskedAt; the question comment's CreatedAt was not used")
	}
	if !got.AskedAt.Equal(asked.AskedAt) {
		t.Errorf("AskedAt = %v, want %v (the question comment's CreatedAt)", got.AskedAt, asked.AskedAt)
	}
	// The exact predicate `issue answer` refuses on.
	if len(state.PendingQuestions()) != 1 {
		t.Errorf("PendingQuestions = %d, want 1 — `answer` refuses on this", len(state.PendingQuestions()))
	}
}

// A multi-select choice question's presentation hints must survive too — they
// are what `answer` shows the operator.
func TestBackend_QuestionRoundTripsMultiSelect(t *testing.T) {
	_, b, claim := newQuestionEnv(t)

	askOne(t, b, claim, flow.AskMultiChoice("which gates?", "Pick the gates to re-run.", "build", "test"))
	parkOnQuestion(t, b, claim)

	state, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Questions) != 1 {
		t.Fatalf("Questions = %d, want 1", len(state.Questions))
	}
	if !state.Questions[0].MultiSelect {
		t.Error("MultiSelect = false, want true")
	}
}

// Questions are every question asked, answered or not, and the park is not
// what makes one visible. Gating them on a question park hid the record from
// the one command that acts on it: `answer --question <id>` needs the id, and
// an id it cannot read is an id it cannot pass.
func TestBackend_QuestionsAreNotGatedOnAPark(t *testing.T) {
	_, b, claim := newQuestionEnv(t)
	asked := askOne(t, b, claim, flow.AskText("base", "which base branch?"))

	state, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Questions) != 1 || state.Questions[0].ID != asked.ID {
		t.Fatalf("Questions = %+v with no park, want the one that was asked", state.Questions)
	}

	if err := b.Park(t.Context(), claim.ItemRef, flow.ParkRequest{
		Kind: flow.ParkBlocked, Step: "plan", Reason: "waiting on infra",
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	state, err = b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Questions) != 1 {
		t.Errorf("Questions = %d under a %s park, want the one that was asked",
			len(state.Questions), flow.ParkBlocked)
	}
}

// storedQuestions reads the questions out of the state comment itself, which is
// where a caller checking that something was PERSISTED has to look: Load
// answers about the item, and a test that only ever asked Load could not tell a
// record written from one reconstructed on the way out.
func storedQuestions(t *testing.T, mock *ghMock) []stateQuestionDoc {
	t.Helper()
	mock.mu.Lock()
	defer mock.mu.Unlock()
	for _, c := range mock.comments {
		doc, _, found, err := extractStateDoc(c.Body)
		if err != nil {
			t.Fatalf("extractStateDoc: %v", err)
		}
		if found && doc != nil {
			return doc.Questions
		}
	}
	t.Fatal("no state comment on the issue")
	return nil
}

// Once the wait ends the questions go with the park: the record is dropped
// from the document, not merely hidden by the read gate, so nothing can
// inherit it later.
func TestBackend_QuestionsSurviveTheStepResolving(t *testing.T) {
	mock, b, claim := newQuestionEnv(t)
	askOne(t, b, claim, flow.AskText("base", "which base branch?"))
	parkOnQuestion(t, b, claim)

	if err := b.ResolveArtifact(t.Context(), claim.ItemRef, "plan", flow.ArtifactBody{
		Type: flow.ArtifactMarkdown, Markdown: "the plan",
	}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}
	state, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Parked() {
		t.Fatalf("park = %+v, want cleared by the resolve", state.Park)
	}
	if len(state.Questions) != 1 {
		t.Errorf("Questions = %d after the step resolved, want the one that was asked", len(state.Questions))
	}
	if q := storedQuestions(t, mock); len(q) != 1 {
		t.Errorf("state comment carries %d question(s) after the resolve, want 1: %+v", len(q), q)
	}
}

// ResetSeed clears the checklist and the park that named a step in it. The
// questions are neither: they record what was asked of a person, and a reseed
// does not answer one.
func TestBackend_QuestionsSurviveResetSeed(t *testing.T) {
	mock, b, claim := newQuestionEnv(t)
	askOne(t, b, claim, flow.AskText("base", "which base branch?"))
	parkOnQuestion(t, b, claim)

	if err := b.ResetSeed(t.Context(), claim.ItemRef); err != nil {
		t.Fatalf("ResetSeed: %v", err)
	}
	state, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Questions) != 1 {
		t.Errorf("Questions = %d after ResetSeed, want the one that was asked", len(state.Questions))
	}
	if q := storedQuestions(t, mock); len(q) != 1 {
		t.Errorf("state comment carries %d question(s) after the reset, want 1: %+v", len(q), q)
	}
}

// A park of another kind supersedes the park, not the question. The question
// was asked of a person and remains unanswered; deleting it is what leaves
// `answer --question <id>` with an id that names nothing.
func TestBackend_QuestionsSurviveASupersedingPark(t *testing.T) {
	mock, b, claim := newQuestionEnv(t)
	askOne(t, b, claim, flow.AskText("base", "which base branch?"))
	parkOnQuestion(t, b, claim)

	if err := b.Park(t.Context(), claim.ItemRef, flow.ParkRequest{
		Kind: flow.ParkBudgetExhausted, Step: "plan", Axis: flow.AxisInvocations,
		Reason: "ran 3 times without resolving \"plan\"",
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if q := storedQuestions(t, mock); len(q) != 1 {
		t.Errorf("state comment carries %d question(s) under a budget park, want 1: %+v", len(q), q)
	}
}

// A question leaves the OUTSTANDING set by being answered, and by nothing
// else. That is what stops a later question park inheriting it: the record is
// still there — it is the history of what was asked — but it is no longer
// pending, so nothing presents it as the thing being waited on.
func TestBackend_AnAnsweredQuestionStopsBeingPending(t *testing.T) {
	_, b, claim := newQuestionEnv(t)
	answered := askOne(t, b, claim, flow.AskText("base", "which base branch?"))
	parkOnQuestion(t, b, claim)

	if err := b.PostAnswer(t.Context(), claim.ItemRef, answered.ID, "main"); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}

	// A later question park that registered no question of its own.
	parkOnQuestion(t, b, claim)

	state, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Questions) != 1 {
		t.Fatalf("Questions = %+v, want the one that was asked — questions are never removed", state.Questions)
	}
	if state.Questions[0].Answer != "main" {
		t.Errorf("Answer = %q, want the text that was posted", state.Questions[0].Answer)
	}
	for _, q := range state.PendingQuestions() {
		if q.ID == answered.ID {
			t.Errorf("the answered question %q is reported as outstanding again", q.ID)
		}
	}
}

// The record is dropped by the step the park names, and only by that step. A
// resolve elsewhere on the checklist leaves the question park standing, so
// dropping the questions with it would reproduce the reported defect exactly:
// an item parked on a question `answer` has no id to name.
func TestBackend_QuestionsSurviveAnUnrelatedStepResolving(t *testing.T) {
	mock, b, claim := newQuestionEnvSeeded(t, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true,
			Budget: flow.StepBudget{MaxInvocations: 3, MaxCostUSD: 10}},
		{Id: "impl", Type: flow.ArtifactMarkdown, Required: true,
			Budget: flow.StepBudget{MaxInvocations: 3, MaxCostUSD: 10}},
	})
	asked := askOne(t, b, claim, flow.AskText("base", "which base branch?"))
	parkOnQuestion(t, b, claim) // against "plan"

	if err := b.ResolveArtifact(t.Context(), claim.ItemRef, "impl", flow.ArtifactBody{
		Type: flow.ArtifactMarkdown, Markdown: "the code",
	}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}
	state, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !state.Parked() {
		t.Fatal("park cleared by a resolve of another step")
	}
	if len(state.PendingQuestions()) != 1 {
		t.Fatalf("PendingQuestions = %d, want 1 — the park it belongs to is still standing",
			len(state.PendingQuestions()))
	}
	if got := state.Questions[0].ID; got != asked.ID {
		t.Errorf("question id = %q, want %q", got, asked.ID)
	}
	if q := storedQuestions(t, mock); len(q) != 1 {
		t.Errorf("state comment carries %d question(s), want the one the park is waiting on", len(q))
	}
}

// A step may ask several questions — `answer --question` exists for exactly
// that case, naming one of them by id. Each call ADDS one: a record that
// replaced what was there would drop every earlier unanswered question, which
// is the reported defect.
func TestBackend_EveryQuestionAskedIsRecorded(t *testing.T) {
	_, b, claim := newQuestionEnv(t)

	asked := []flow.AgentQuestion{
		flow.AskYesNo("drop `--yes`?", "Issue #111 asks to remove it."),
		flow.AskChoice("amend the doc?", "§11 step 1 says it skips.", "amend", "leave"),
	}
	var recorded []flow.Question
	for _, q := range asked {
		rec, err := b.AskQuestion(t.Context(), claim.ItemRef, q)
		if err != nil {
			t.Fatalf("AskQuestion: %v", err)
		}
		recorded = append(recorded, rec)
	}
	if len(recorded) != 2 {
		t.Fatalf("recorded %d questions, want 2", len(recorded))
	}
	parkOnQuestion(t, b, claim)

	state, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Questions) != 2 {
		t.Fatalf("Questions = %d, want both of the ask", len(state.Questions))
	}
	// Order matters as much as presence: `answer` prints the ids for the
	// operator to choose between, and a payload paired with the wrong id sends
	// the answer to the other question.
	for i, want := range recorded {
		got := state.Questions[i]
		if got.ID != want.ID {
			t.Errorf("Questions[%d].ID = %q, want %q", i, got.ID, want.ID)
		}
		if got.Header != want.Header || got.Text != want.Text {
			t.Errorf("Questions[%d] payload = %q / %q, want %q / %q",
				i, got.Header, got.Text, want.Header, want.Text)
		}
		if got.Format != want.Format {
			t.Errorf("Questions[%d].Format = %q, want %q", i, got.Format, want.Format)
		}
	}
	if strings.Join(state.Questions[1].Options, ",") != "amend,leave" {
		t.Errorf("Questions[1].Options = %v, want the two asked", state.Questions[1].Options)
	}
	if len(state.PendingQuestions()) != 2 {
		t.Errorf("PendingQuestions = %d, want 2 — `answer --question` names one of these",
			len(state.PendingQuestions()))
	}
}

// EACH CALL ADDS ONE — THERE IS NO REPLACE. A second ask that overwrote the
// first left the earlier question recorded nowhere: still unanswered, no longer
// listed, and unreachable by `answer --question <id>` for the rest of the
// item's life.
func TestBackend_AskQuestionAppends(t *testing.T) {
	_, b, claim := newQuestionEnv(t)
	first := askOne(t, b, claim, flow.AskText("first", "the first question?"))
	second := askOne(t, b, claim, flow.AskText("second", "the second question?"))
	parkOnQuestion(t, b, claim)

	state, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Questions) != 2 {
		t.Fatalf("Questions = %d, want 2 — each ask adds one", len(state.Questions))
	}
	if state.Questions[0].ID != first.ID || state.Questions[1].ID != second.ID {
		t.Errorf("Questions = %q/%q, want %q then %q in the order asked",
			state.Questions[0].ID, state.Questions[1].ID, first.ID, second.ID)
	}
	if first.ID == second.ID {
		t.Error("the two asks were assigned the same QuestionId")
	}
	if len(state.PendingQuestions()) != 2 {
		t.Errorf("PendingQuestions = %d, want 2 — neither has been answered",
			len(state.PendingQuestions()))
	}
}

// A question cannot be asked before the item is seeded — the mandatory-seed
// gate runs first — so a missing state comment here means the record that
// makes the question answerable was not written. That is an error, not the
// tolerated case Park has.
func TestBackend_AskQuestionsWithoutStateCommentFails(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)

	ctx := t.Context()
	claim, err := b.Claim(ctx, b.refFromIssue(42), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// Deliberately not seeded.
	if _, err := b.AskQuestion(ctx, claim.ItemRef, flow.AskText("base", "which base branch?")); err == nil {
		t.Fatal("AskQuestion succeeded with no state comment, want an error")
	}
	// Nothing may advertise "a human must answer this" when nothing recorded
	// the question.
	if hasLabel(mock.labelNames(), b.labels.NeedsAnswer()) {
		t.Errorf("labels = %v, want %q absent", mock.labelNames(), b.labels.NeedsAnswer())
	}
}

// The needs-answer label is the advertisement; the state-comment entry is what
// makes answering possible. The label must come second, so no window exists in
// which the item asks for an answer nothing can accept.
func TestBackend_AskQuestionsLabelsOnlyAfterRecording(t *testing.T) {
	mock, b, claim := newQuestionEnv(t)

	mock.mu.Lock()
	before := len(mock.mutations)
	mock.mu.Unlock()

	askOne(t, b, claim, flow.AskText("base", "which base branch?"))

	mock.mu.Lock()
	tape := append([]string(nil), mock.mutations[before:]...)
	mock.mu.Unlock()

	recordedAt, labelledAt := -1, -1
	for i, m := range tape {
		if recordedAt < 0 && strings.HasPrefix(m, "PATCH ") && strings.Contains(m, "/issues/comments/") {
			recordedAt = i
		}
		if labelledAt < 0 && strings.HasPrefix(m, "POST ") && strings.HasSuffix(m, "/labels") {
			labelledAt = i
		}
	}
	if recordedAt < 0 {
		t.Fatalf("no state-comment write on the tape: %v", tape)
	}
	if labelledAt < 0 {
		t.Fatalf("no needs-answer label write on the tape: %v", tape)
	}
	if recordedAt > labelledAt {
		t.Errorf("label written before the question was recorded: %v", tape)
	}
}

// Items parked before this field existed must still load: their state comment
// has no `questions:` key, and an absent key is an empty list, not an error.
func TestExtractStateDoc_MissingQuestionsKeyLoads(t *testing.T) {
	body := "<!-- flow:state-v1 begin owner=alice -->\n" +
		"```yaml\n" +
		"flow: implement\n" +
		"schema: 1\n" +
		"park:\n" +
		"    kind: question\n" +
		"    step: plan\n" +
		"    reason: 'question: should the doc be amended?'\n" +
		"```\n" +
		"<!-- flow:state-v1 end -->\n"

	doc, _, found, err := extractStateDoc(body)
	if err != nil {
		t.Fatalf("extractStateDoc: %v", err)
	}
	if !found || doc == nil {
		t.Fatal("extractStateDoc found=false on a well-formed body")
	}
	if len(doc.Questions) != 0 {
		t.Errorf("Questions = %v, want empty", doc.Questions)
	}
	if len(questionsFromDocs(doc.Questions)) != 0 {
		t.Error("questionsFromDocs on an absent array returned questions")
	}
}
