package issue

import (
	"context"
	"fmt"
	"strings"

	"github.com/promise-language/flow"
)

// Park-for-answer contract
//
// A step that needs a human decision asks it and parks. Nothing resumes on its
// own: resumption is always operator-triggered, because somebody has to have
// answered, and the flow has no way to be told other than being re-run.
//
//   - The agent's question is posted where the humans already are (an issue
//     comment) and the step parks with ParkQuestion.
//   - Re-running with no answer stops with "answer needed" and a non-zero exit.
//     It burns no invocation and no agent turn — the gate is a preflight, so it
//     runs before dispatch.
//   - Re-running once somebody has answered resumes, and the answers reach the
//     step through PromptContext.Answers.
//
// Anyone's answer counts, not just the issue author's. A maintainer, a
// passer-by, or the reporter can all unblock a step, which is the point of
// asking in the open; Answer.Author records which, so a prompt body (or a human
// reading the thread) can weigh it.

// answerGate returns the Preflight that enforces the contract above.
//
// It returns nil when the backend cannot read answers, rather than a gate that
// always blocks: a backend without AnswerReader has no way to observe a reply,
// so a gate would strand every question park permanently. Such a backend still
// parks on a question — the step just can't auto-detect the answer, and an
// operator resumes by re-running once they have handled it out of band.
// self is a resolver rather than a value: resolving the principal eagerly would
// put a network call in BuildApp, which runs before every command including the
// one meant to diagnose network failures.
func answerGate(backend flow.Backend, self func(context.Context) string) flow.PreflightFunc {
	reader, ok := backend.(AnswerReader)
	if !ok {
		return nil
	}
	return func(ctx context.Context, state *flow.ItemState) error {
		park := state.Park
		if park == nil || park.Kind != flow.ParkQuestion {
			return nil
		}
		since := flow.QuestionAskedAt(park)
		answers, err := reader.ReadAnswers(ctx, state.Item, since, self(ctx))
		if err != nil {
			// Reading answers is the gate's whole job; if it fails we cannot
			// tell "answered" from "unanswered". Block rather than guess —
			// guessing "unanswered" strands a step somebody already answered,
			// and guessing "answered" dispatches a step that will just ask
			// again and burn an invocation doing it.
			return fmt.Errorf("cannot read answers for %q: %w: %w", park.Step, err, flow.ErrBlocked)
		}
		if len(answers) == 0 {
			return fmt.Errorf("answer needed on %q: %s: %w", park.Step, park.Reason, flow.ErrBlocked)
		}
		return nil
	}
}

// MarkQuestionAsked and QuestionAskedAt live in the flow package, because the
// SDK stamps the marker when a handler's ctx.AskQuestions parks the step. These
// aliases keep them reachable under the name a consumer of this package would
// look for.
var (
	MarkQuestionAsked = flow.MarkQuestionAsked
	QuestionAskedAt   = flow.QuestionAskedAt
)

// AskSentinel is how an agent signals that it needs a human decision.
//
// The canonical steps own the convention rather than each project inventing
// one: the library supplies the handlers and a project supplies only prompt
// bodies, so if the token varied per project the handlers could not detect
// anything. A project's prompt instructs the agent to end its final message
// with this token followed by the question on one line, optionally followed by
// a fenced block carrying the evidence for the decision:
//
//	NEEDS-ANSWER: docs/design.md conflicts with this item — amend, adjust, or reject?
//	```
//	§3 states: "No macros." This item asks for a macro system.
//	Recommendation: reject; the constraint is deliberate.
//	```
//
// The question line becomes the AgentQuestion's Header — short, scannable, what
// a human sees first — and the block becomes its Text. The block is what makes
// the question answerable: a choice between amending a document and rejecting
// an item cannot be made without seeing the statement that conflicts and the
// agent's reading of it, and everything the agent reasoned out otherwise stays
// stranded in a turn nobody will read. With no block, Text repeats Header.
//
// The token must sit at COLUMN ZERO, with no leading whitespace. That is what
// lets a prompt body show the agent an indented example of the convention
// without the agent's echo of that example tripping it — a false positive here
// is systematic rather than occasional, because it comes from the instruction
// every run is given. Prose mentioning the token mid-line is inert too.
//
// It is honored on EVERY agent-driven step, not just planning. A review step
// can discover that the code contradicts a policy and that the PLAN is what is
// wrong — a decision no agent should make unilaterally.
//
// Format and Options are deliberately not parsed. A project's choices are
// project-specific, and a library inferring them from prose would be guessing.
const AskSentinel = "NEEDS-ANSWER:"

// detectQuestion looks for the sentinel in an agent's final message and returns
// the question line and its supporting block.
//
// It scans from the END: an agent that reasons about the decision before making
// it may mention the token in passing, and the operative line is its last word
// on the matter.
func detectQuestion(lastText string) (header, body string, ok bool) {
	lines := strings.Split(lastText, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		// Column zero, deliberately not trimmed first — see AskSentinel.
		if !strings.HasPrefix(lines[i], AskSentinel) {
			continue
		}
		header = strings.TrimSpace(strings.TrimPrefix(lines[i], AskSentinel))
		if header == "" {
			continue // a bare token asks nothing; keep looking
		}
		body = fencedBlockAfter(lines[i+1:])
		if body == "" {
			// No evidence supplied — the question stands on its own.
			body = header
		}
		return header, body, true
	}
	return "", "", false
}

// fencedBlockAfter extracts the fenced block following the sentinel line, or ""
// when there is none.
//
// Blank lines between the sentinel and the fence are tolerated; anything else
// before it means the agent moved on and there is no block. An UNTERMINATED
// fence takes everything to the end rather than being discarded: the failure
// that matters here is losing the evidence, and showing a human a few extra
// lines costs nothing next to handing them an unanswerable question.
func fencedBlockAfter(lines []string) string {
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
		return ""
	}
	i++ // step over the opening fence (and any language tag on it)
	var out []string
	for ; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
			break
		}
		out = append(out, lines[i])
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
