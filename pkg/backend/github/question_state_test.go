package github

import (
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// newQuestionEnv claims and seeds issue 42 — the state the ask half of
// park-for-answer always runs in, since the mandatory-seed gate precedes any
// handler that could ask.
func newQuestionEnv(t *testing.T) (*ghMock, *Backend, flow.Claim) {
	t.Helper()
	mock := newGHMock(t)
	srv := mock.server()
	t.Cleanup(srv.Close)
	b := newMockedBackend(t, mock, srv)

	ctx := t.Context()
	claim, err := b.Claim(ctx, b.refFromIssue(42), "alice", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true,
			Budget: flow.StepBudget{MaxInvocations: 3, MaxCostUSD: 10}},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}
	return mock, b, claim
}

func askOne(t *testing.T, b *Backend, claim flow.Claim, q flow.AgentQuestion) flow.Question {
	t.Helper()
	recorded, err := b.AskQuestions(t.Context(), claim, []flow.AgentQuestion{q})
	if err != nil {
		t.Fatalf("AskQuestions: %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("AskQuestions recorded %d questions, want 1", len(recorded))
	}
	return recorded[0]
}

func parkOnQuestion(t *testing.T, b *Backend, claim flow.Claim) {
	t.Helper()
	if err := b.Park(t.Context(), claim, flow.ParkRequest{
		Kind: flow.ParkQuestion, Step: "plan", Reason: "question: should the doc be amended?",
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}
}

// The reported defect: a question park LoadState returns no question for is a
// park `answer` cannot clear, because it has no id to name. The whole payload
// must survive the round trip through the state comment.
func TestBackend_QuestionRoundTripsThroughStateComment(t *testing.T) {
	_, b, claim := newQuestionEnv(t)

	asked := askOne(t, b, claim, flow.AskChoice("amend the doc?",
		"Issue #111 asks to remove `--yes`, but docs/release.md §11 says it skips.\n\nWhich wins?",
		"amend the doc", "keep the flag"))
	parkOnQuestion(t, b, claim)

	state, err := b.LoadState(t.Context(), claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
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

	state, err := b.LoadState(t.Context(), claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Questions) != 1 {
		t.Fatalf("Questions = %d, want 1", len(state.Questions))
	}
	if !state.Questions[0].MultiSelect {
		t.Error("MultiSelect = false, want true")
	}
}

// Questions are returned exactly while the item is parked on one: an ask that
// did not park (the handler returned a question error the orchestrator has yet
// to act on, say) and a park of any other kind both read as no questions.
func TestBackend_QuestionsGatedOnQuestionPark(t *testing.T) {
	_, b, claim := newQuestionEnv(t)
	askOne(t, b, claim, flow.AskText("base", "which base branch?"))

	state, err := b.LoadState(t.Context(), claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Questions) != 0 {
		t.Errorf("Questions = %d with no park, want 0", len(state.Questions))
	}

	if err := b.Park(t.Context(), claim, flow.ParkRequest{
		Kind: flow.ParkBlocked, Step: "plan", Reason: "waiting on infra",
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	state, err = b.LoadState(t.Context(), claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Questions) != 0 {
		t.Errorf("Questions = %d under a %s park, want 0", len(state.Questions), flow.ParkBlocked)
	}
}

// Once the wait ends the questions go with the park — no separate clearing
// site, so "pending questions with no question park" (the state issue/answers
// treats as a permanent block) cannot arise here.
func TestBackend_QuestionsGoneWhenStepResolves(t *testing.T) {
	_, b, claim := newQuestionEnv(t)
	askOne(t, b, claim, flow.AskText("base", "which base branch?"))
	parkOnQuestion(t, b, claim)

	if err := b.ResolveArtifact(t.Context(), claim, "plan", flow.ArtifactBody{
		Type: flow.ArtifactMarkdown, Markdown: "the plan",
	}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}
	state, err := b.LoadState(t.Context(), claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.Parked() {
		t.Fatalf("park = %+v, want cleared by the resolve", state.Park)
	}
	if len(state.Questions) != 0 {
		t.Errorf("Questions = %d after the step resolved, want 0", len(state.Questions))
	}
}

func TestBackend_QuestionsGoneAfterResetSeed(t *testing.T) {
	_, b, claim := newQuestionEnv(t)
	askOne(t, b, claim, flow.AskText("base", "which base branch?"))
	parkOnQuestion(t, b, claim)

	if err := b.ResetSeed(t.Context(), claim); err != nil {
		t.Fatalf("ResetSeed: %v", err)
	}
	state, err := b.LoadState(t.Context(), claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Questions) != 0 {
		t.Errorf("Questions = %d after ResetSeed, want 0", len(state.Questions))
	}
}

// The field carries what is outstanding, not the history: a second ask
// replaces the first rather than leaving the operator to answer a question
// nothing is waiting on.
func TestBackend_AskQuestionsReplacesThePreviousSet(t *testing.T) {
	_, b, claim := newQuestionEnv(t)
	first := askOne(t, b, claim, flow.AskText("first", "the first question?"))
	second := askOne(t, b, claim, flow.AskText("second", "the second question?"))
	parkOnQuestion(t, b, claim)

	state, err := b.LoadState(t.Context(), claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Questions) != 1 {
		t.Fatalf("Questions = %d, want 1 (the second ask replaces the first)", len(state.Questions))
	}
	if state.Questions[0].ID != second.ID {
		t.Errorf("Questions[0].ID = %q, want %q", state.Questions[0].ID, second.ID)
	}
	if state.Questions[0].ID == first.ID {
		t.Error("the superseded question is still outstanding")
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
	b := newMockedBackend(t, mock, srv)

	ctx := t.Context()
	claim, err := b.Claim(ctx, b.refFromIssue(42), "alice", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// Deliberately not seeded.
	if _, err := b.AskQuestions(ctx, claim, []flow.AgentQuestion{
		flow.AskText("base", "which base branch?"),
	}); err == nil {
		t.Fatal("AskQuestions succeeded with no state comment, want an error")
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
