package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// A remark is ONE comment: the marker line, and the operator's text below it
// and nothing else. The marker is an HTML comment, so what renders in the
// thread is the sentence a person writing it by hand would have written.
func TestBackend_RemarkIsOneMarkedComment(t *testing.T) {
	mock, b, claim := newQuestionEnv(t)

	if err := b.Remark(t.Context(), claim.ItemRef, "released: the arena was needed elsewhere"); err != nil {
		t.Fatalf("Remark: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	found := 0
	for _, c := range mock.comments {
		if !strings.Contains(c.Body, "released: the arena was needed elsewhere") {
			continue
		}
		found++
		if !isFlowMachineComment(c.Body) {
			t.Errorf("the remark comment carries no flow marker, so ReadAnswers "+
				"would take it for a reply: %q", c.Body)
		}
		if c.Body != remarkCommentMarker+"\nreleased: the arena was needed elsewhere" {
			t.Errorf("comment body = %q, want the marker and the remark's text and nothing else", c.Body)
		}
	}
	if found != 1 {
		t.Errorf("found %d comments carrying the remark, want 1", found)
	}
}

// The remark reaches the guard under its own act, with the final bytes, and as
// the operator's words — a step-authored remark would be OriginAgent, and the
// two are not the same question to a guard.
func TestBackend_RemarkReachesTheGuardAsTheOperatorsWords(t *testing.T) {
	b, _, _ := newSeamBackend(t)
	guard := &recordingGuard{}
	b.out.guard = guard

	if err := b.Remark(t.Context(), b.refFromIssue(42), "what was found"); err != nil {
		t.Fatalf("Remark: %v", err)
	}

	seen := guard.of(flow.ActRemark)
	if len(seen) != 1 {
		t.Fatalf("guard saw %d remark disclosures, want 1", len(seen))
	}
	if len(seen[0].Text) != 1 {
		t.Fatalf("disclosure carries %d texts, want 1", len(seen[0].Text))
	}
	// The FINAL bytes — the marker included, because that is what is posted.
	// A guard shown the prose before the last transformation reports a safety
	// it did not establish (docs/disclosure.md § What it must hold).
	if got := seen[0].Text[0]; got.Body != remarkCommentMarker+"\nwhat was found" || got.Origin != flow.OriginOperator {
		t.Errorf("text = %+v, want the final bytes from the operator", got)
	}
}

// A refusal reaches the caller rather than being swallowed: the guard quotes
// what it caught, which is the part that makes it actionable.
func TestBackend_RemarkCarriesADisclosureRefusalOut(t *testing.T) {
	b, _, _ := newSeamBackend(t)
	b.out.guard = &refusingGuard{}

	err := b.Remark(t.Context(), b.refFromIssue(42), "see /home/someone/prog/flow")
	if err == nil {
		t.Fatal("Remark succeeded past a refusing guard, want the refusal")
	}
	var refused flow.ErrDisclosureRefused
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a flow.ErrDisclosureRefused", err)
	}
	if refused.Act != flow.ActRemark {
		t.Errorf("refusal names act %q, want %q", refused.Act, flow.ActRemark)
	}
}

// Empty text is refused BEFORE publishing. An empty comment says nothing when
// it arrives and cannot be taken back.
func TestBackend_RemarkRefusesEmptyTextWithoutPublishing(t *testing.T) {
	mock, b, claim := newQuestionEnv(t)
	before := len(mock.comments)

	for _, text := range []string{"", "  \n\t "} {
		if err := b.Remark(t.Context(), claim.ItemRef, text); err == nil {
			t.Errorf("Remark(%q) succeeded, want a refusal", text)
		}
	}
	if got := len(mock.comments); got != before {
		t.Errorf("comment count = %d, want it unchanged at %d", got, before)
	}
}

// A REMARK IS NOT AN ANSWER, and the one reader that selects on the ABSENCE of
// a marker is what makes that a property of the bytes rather than an intention.
//
// ReadAnswers is the read half of park-for-answer: the issue thread is the
// answer store, so every unmarked comment posted after the ask IS an answer to
// it. An unmarked remark recorded while a step waits for a human would clear
// that wait and resume the step on prose nobody offered as a reply — the
// failure isFlowMachineComment's own docstring records from the other side.
func TestBackend_RemarkIsNotReadBackAsAnAnswer(t *testing.T) {
	_, b, claim := newQuestionEnv(t)
	asked := askOne(t, b, claim, flow.AskText("base", "which base branch?"))
	parkOnQuestionAsked(t, b, claim, asked)

	if err := b.Remark(t.Context(), claim.ItemRef, "released: the arena was needed elsewhere"); err != nil {
		t.Fatalf("Remark: %v", err)
	}
	item, err := b.Load(t.Context(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	answers, err := b.ReadAnswers(t.Context(), *item, flow.QuestionAskedAt(item.Park), "alice")
	if err != nil {
		t.Fatalf("ReadAnswers: %v", err)
	}
	if len(answers) != 0 {
		t.Errorf("ReadAnswers = %+v, want none — a remark is not a reply to the question", answers)
	}
}

// refusingGuard refuses every disclosure, naming the act it was asked about.
type refusingGuard struct{}

func (refusingGuard) Examine(_ context.Context, d flow.Disclosure) error {
	return flow.ErrDisclosureRefused{Act: d.Act, Reason: errors.New("local filesystem path")}
}
