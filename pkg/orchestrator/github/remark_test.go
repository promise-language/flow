package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// A remark is an ORDINARY, UNMARKED COMMENT. Every machine-readable write on an
// issue carries an HTML marker and every reader of them selects on one, so a
// remark without a marker is invisible to all of them — which is what keeps it
// from being mistaken for state, and what puts it in the thread where a person
// writing the same sentence by hand would have put it.
func TestBackend_RemarkIsAnUnmarkedComment(t *testing.T) {
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
		if isFlowMachineComment(c.Body) {
			t.Errorf("the remark comment carries a flow marker: %q", c.Body)
		}
		if c.Body != "released: the arena was needed elsewhere" {
			t.Errorf("comment body = %q, want the remark's text and nothing else", c.Body)
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
	if got := seen[0].Text[0]; got.Body != "what was found" || got.Origin != flow.OriginOperator {
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

// refusingGuard refuses every disclosure, naming the act it was asked about.
type refusingGuard struct{}

func (refusingGuard) Examine(_ context.Context, d flow.Disclosure) error {
	return flow.ErrDisclosureRefused{Act: d.Act, Reason: errors.New("local filesystem path")}
}
