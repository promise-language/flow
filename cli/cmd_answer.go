package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/promise-language/flow"
)

type answerPayload struct {
	Item       string `json:"item"`
	QuestionID string `json:"question_id"`
	Answered   bool   `json:"answered"`
	// Questions is what the item was asked, for the forms that READ rather than
	// answer: bare `answer`, and `--answered`. Empty on an invocation that
	// posted an answer, whose three fields above say what happened.
	Questions []questionPayload `json:"questions,omitempty"`
}

// cmdAnswer reads and answers the questions a step parked on.
//
// THE BARE FORM IS THE ONE AN OPERATOR REACHES FOR, and until now it did not
// exist: both positionals were required, so answering meant already knowing the
// question and having composed the reply. On a real parked item that meant
// `status` for a clipped header, then `gh api` against the raw comment to read
// what was actually asked, then `answer <id> "<text>"` — three commands and two
// tools to answer one question. Bare `answer` prints the question in full and,
// on a terminal, takes the reply.
//
// The item id is OPTIONAL, not gone: it is required when no lease is held, and
// it still names a different item when one is. An explicit id wins.
func (app *App) cmdAnswer(ctx context.Context, args []string) int {
	fs := app.newFlagSet("answer")
	question := fs.String("question", "", "which question to answer (required when multiple are pending)")
	answered := fs.Bool("answered", false, "print every question and its answer, in order")
	of := addOutputFlags(fs)
	if !app.parseArgs(fs, args) {
		return 2
	}
	mode, ok := of.mode(app, "answer")
	if !ok {
		return 2
	}
	if fs.NArg() > 2 {
		return app.usageError("answer: unexpected argument %q (answer takes at most <item-id> and <text>)", fs.Arg(2))
	}

	ref, text, ok := app.answerTarget(ctx, fs.Args())
	if !ok {
		return 1
	}
	item, err := app.Orchestrator.Load(ctx, ref)
	if err != nil {
		fmt.Fprintln(app.Err, "answer:", err)
		return 1
	}

	// WHAT THIS INVOCATION CAN ANSWER, derived ONCE and read by every form
	// below — the reading forms, the prompt, and the pick — so no two of them
	// can disagree about what is answerable.
	//
	// Ordinarily the item's own questions. But a park whose kind says a human
	// must answer is a park a human must be able to answer, and whether the
	// asking step managed to register its question is not something the
	// answerer can see or fix; when nothing was registered, the park's own
	// question stands in (parkQuestion).
	questions := item.Questions
	pending := item.PendingQuestions()
	if q, ok := parkQuestion(item); ok {
		// It is unanswered by construction — the park is what says a human
		// must answer — and parkQuestion fires only on an item carrying no
		// question at all, so it is both the whole list and the whole pending
		// set.
		questions = []flow.Question{q}
		pending = questions
	}

	// --answered prints the WHOLE HISTORY: every question and its answer, in
	// order, not only the outstanding one. A question already answered once, in
	// different words, is invisible otherwise — to the operator and to the
	// asking step alike.
	if *answered {
		return app.emitQuestions(mode, ref, questions)
	}

	if len(pending) == 0 {
		fmt.Fprintf(app.Err, "answer: no outstanding questions on %s\n", ref.Display)
		return 1
	}

	target, ok := app.pickQuestion(ref, pending, *question)
	if !ok {
		return 1
	}

	// No text given: READ rather than answer. On a terminal the operator is
	// then asked for the reply, which is what makes this one command instead of
	// a read followed by a second invocation carrying an id copied by hand.
	if text == "" {
		if mode != OutputHuman || !app.interactive() {
			// NON-INTERACTIVE BARE ANSWER NEVER BLOCKS. A prompt on a piped
			// stdin waits for input that is not coming, which is a hang rather
			// than a report.
			//
			// A JSON RENDERING IS NON-INTERACTIVE WHATEVER STDIN IS. The
			// payload is the whole of stdout in that mode, and the question and
			// the prompt are prose: written there they would be prepended to
			// the JSON, so `answer --json` and `answer | tee` — the ordinary
			// ways to ask for the machine form from a terminal — would emit
			// something no decoder can read. The reading form is what the mode
			// has to offer, and it carries the same questions.
			return app.emitQuestions(mode, ref, questions)
		}
		// The question first: an operator cannot answer what they have not been
		// shown, and showing it is what the bare form is FOR.
		app.printQuestionInFull(target)
		reply, rerr := app.readAnswer()
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			fmt.Fprintln(app.Err, "answer:", rerr)
			return 1
		}
		if strings.TrimSpace(reply) == "" {
			// Nothing was said. Recording an empty answer would clear the park
			// on a question nobody settled, so the question stays pending and
			// the exit code says the command did not do what it was asked.
			fmt.Fprintln(app.Err, "answer: no answer given — nothing was recorded")
			return 1
		}
		text = reply
	}

	id, err := app.recordAnswer(ctx, ref, target, text)
	if err != nil {
		fmt.Fprintln(app.Err, "answer:", err)
		return 1
	}

	payload := answerPayload{
		Item:       ref.Display,
		QuestionID: string(id),
		Answered:   true,
	}
	return app.emit(mode, payload, func() {
		fmt.Fprintf(app.Out, "answered %s on %s\n", id, ref.Display)
	})
}

// parkQuestion is the question a question-park is waiting on when NOTHING WAS
// REGISTERED — the state #165 stranded in, where the park says a human must
// answer and the item carries no question for an answer to be recorded
// against.
//
// It is DERIVED, never stored, and only from a park of kind `question` on an
// item with no question at all: an item that has one — answered or not — has
// its own, and a park of any other kind is not waiting on an answer.
//
// It carries NO ID, which is what tells recordAnswer the question has still to
// be registered, and its text is the park's REASON, because the reason is all
// the park kept — the one-line form questionReason wrote. The ask itself was
// published where the step asked it and never copied onto the item's state, so
// a restatement is the most the park can offer.
func parkQuestion(item *flow.Item) (flow.Question, bool) {
	if item == nil || item.Park == nil || item.Park.Kind != flow.ParkQuestion {
		return flow.Question{}, false
	}
	if len(item.Questions) > 0 {
		return flow.Question{}, false
	}
	return flow.Question{
		AgentQuestion: flow.AgentQuestion{Header: item.Park.Reason},
		// The park's own ask time, read back through the one parser that
		// knows the marker's spelling rather than a second reading of
		// Details here.
		AskedAt: flow.QuestionAskedAt(item.Park),
	}, true
}

// recordAnswer records the answer AGAINST THE QUESTION IT ANSWERS, registering
// that question first when the park has been standing in for one.
//
// There is no other way to record an answer and no other way to deliver one:
// PostAnswer names the question it answers, and what a resumed step reads is
// Item.Questions. So a park that registered nothing is answered by registering
// its question and answering that — and AskQuestion is where a QuestionId
// comes from.
//
// THE REGISTRATION HAPPENS HERE AND NOWHERE EARLIER, because it is a write:
// every form that only reads — including the operator who is prompted and says
// nothing — must leave the item exactly as it found it.
func (app *App) recordAnswer(ctx context.Context, ref flow.ItemRef, target flow.Question, text string) (flow.QuestionId, error) {
	if target.ID == "" {
		rec, err := app.Orchestrator.AskQuestion(ctx, ref, target.AgentQuestion)
		if err != nil {
			return "", fmt.Errorf("register the question %s parked on: %w", ref.Display, err)
		}
		target = rec
	}
	// The id is passed THROUGH, not merely selected for the output line. It is
	// what the answer is recorded against, which is what makes the question stop
	// being pending — and what lets the outstanding-question marker clear only
	// when no pending question remains, rather than on the first of several.
	if err := app.Orchestrator.PostAnswer(ctx, ref, target.ID, text); err != nil {
		return "", err
	}
	return target.ID, nil
}

// answerTarget resolves the positionals into an item and the answer text.
//
// THE AMBIGUITY IS ONE POSITIONAL, and it is settled by what this arena holds
// rather than by guessing at the string's shape:
//
//   - no positionals — the active claim, and nothing to post yet.
//   - one, with NO active claim — it can only be the item id, because there is
//     no item for an answer to be about.
//   - one, WITH an active claim — the answer text, unless it names an item that
//     EXISTS, which is how a different item is still nameable.
//   - two — the item id and the text, as before. An explicit id always wins.
//
// The claim comes from the orchestrator, which owns claim state — never from
// clistate directly, whose own package doc says so.
func (app *App) answerTarget(ctx context.Context, args []string) (flow.ItemRef, string, bool) {
	if len(args) == 2 {
		ref, err := app.resolveClaimRef(ctx, args[0])
		if err != nil {
			fmt.Fprintln(app.Err, "answer:", err)
			return flow.ItemRef{}, "", false
		}
		return ref, args[1], true
	}

	claim, err := app.Orchestrator.LookupActiveClaim(ctx)
	if err != nil {
		fmt.Fprintln(app.Err, "answer:", err)
		return flow.ItemRef{}, "", false
	}

	if len(args) == 1 {
		if claim == nil {
			// Nothing held, so the one argument names the item. A bare answer
			// text with no item to attach it to is not an invocation that could
			// mean anything else.
			ref, rerr := app.resolveClaimRef(ctx, args[0])
			if rerr != nil {
				fmt.Fprintln(app.Err, "answer:", rerr)
				return flow.ItemRef{}, "", false
			}
			return ref, "", true
		}
		// A claim is held, so the argument is the answer — unless it names an
		// item that EXISTS, which is how `answer <other-item>` still reaches
		// one.
		//
		// EXISTENCE, not resolvability. A backend's resolver is not required to
		// be a syntax check — the fake mints a ref for any non-empty string —
		// so "does this resolve" can answer yes for prose, and the answer text
		// would be read as an item id. A load is the question actually being
		// asked — is there an item here — and it costs one read on this branch
		// alone.
		if ref, rerr := app.resolveClaimRef(ctx, args[0]); rerr == nil {
			if _, lerr := app.Orchestrator.Load(ctx, ref); lerr == nil {
				return ref, "", true
			}
		}
		return claim.ItemRef, args[0], true
	}

	if claim == nil {
		fmt.Fprintln(app.Err, "answer: no active claim — name the item: `answer <item-id>`")
		return flow.ItemRef{}, "", false
	}
	return claim.ItemRef, "", true
}

// pickQuestion chooses which pending question is being answered.
//
// Answering is NEVER APPLIED TO AN UNSPECIFIED QUESTION: with more than one
// pending and no --question, it refuses and names the ids rather than picking.
func (app *App) pickQuestion(ref flow.ItemRef, pending []flow.Question, named string) (flow.Question, bool) {
	if named != "" {
		for _, q := range pending {
			if q.ID == flow.QuestionId(named) {
				return q, true
			}
		}
		fmt.Fprintf(app.Err, "answer: question %q not found among pending questions on %s\n", named, ref.Display)
		return flow.Question{}, false
	}
	if len(pending) == 1 {
		return pending[0], true
	}
	ids := make([]string, 0, len(pending))
	for _, q := range pending {
		ids = append(ids, string(q.ID))
	}
	fmt.Fprintf(app.Err, "answer: %d outstanding questions on %s — use --question to name one: %s\n",
		len(pending), ref.Display, strings.Join(ids, ", "))
	return flow.Question{}, false
}

// emitQuestions prints every question and its answer, in order. The unanswered
// one shows an empty answer rather than being omitted: what an operator is
// looking for is the question still waiting, and a list that dropped it would
// answer a different question.
func (app *App) emitQuestions(mode OutputMode, ref flow.ItemRef, questions []flow.Question) int {
	if len(questions) == 0 {
		fmt.Fprintf(app.Err, "answer: no questions on %s\n", ref.Display)
		return 1
	}
	payload := answerPayload{Item: ref.Display, Questions: make([]questionPayload, 0, len(questions))}
	for _, q := range questions {
		payload.Questions = append(payload.Questions, questionPayload{
			ID: string(q.ID), Header: q.Header, Text: q.Text, Answered: q.Answer != "",
		})
	}
	return app.emit(mode, payload, func() {
		for i, q := range questions {
			if i > 0 {
				fmt.Fprintln(app.Out)
			}
			app.printQuestionInFull(q)
			if q.Answer != "" {
				fmt.Fprintf(app.Out, "answer: %s\n", q.Answer)
			} else {
				fmt.Fprintln(app.Out, "answer:")
			}
		}
	})
}

// printQuestionInFull writes a question the way `status` writes an unanswered
// one: header and text, UNCLIPPED, on the lines the asker wrote them on.
//
// Nothing goes through titleLine here, and that is deliberate: the ask
// convention puts the options, the evidence and the recommendation in the text,
// and those are what the operator is being asked to decide on.
//
// NO IDENTIFIER IS SHOWN FOR ONE NOTHING REGISTERED. The park's own question
// has no id until it is answered (parkQuestion, recordAnswer), and printing an
// empty one would offer `--question` a value it can never match.
func (app *App) printQuestionInFull(q flow.Question) {
	if q.ID == "" {
		fmt.Fprintln(app.Out, "question")
	} else {
		fmt.Fprintf(app.Out, "question %s\n", q.ID)
	}
	for _, line := range questionBlock(questionPayload{Header: q.Header, Text: q.Text}) {
		fmt.Fprintf(app.Out, "  %s\n", line)
	}
}

// isTerminal reports whether a reader is an operator's terminal. It is a var
// for the reason quotaFetch and quotaCacheDir are: the real answer depends on
// the process's actual stdin, and a test that cannot substitute one can only
// exercise whichever branch the test runner happens to hand it.
var isTerminal = func(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// interactive reports whether there is an operator at the other end of stdin.
//
// STDIN decides, not stdout: the question is whether a REPLY CAN BE READ, and
// `answer > file` on a terminal is still an operator who can type one. This is
// the same character-device test the output mode is selected by, which is the
// strongest evidence available here without a build-tagged syscall.
func (app *App) interactive() bool { return isTerminal(app.in()) }

// readAnswer reads one answer from the operator. One line: an answer is a
// decision, and the evidence for it belongs on the item rather than in the
// reply.
func (app *App) readAnswer() (string, error) {
	fmt.Fprint(app.Out, "\nyour answer: ")
	line, err := bufio.NewReader(app.in()).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// in is App.In with the default applied, the way Out and Err are defaulted.
func (app *App) in() io.Reader {
	if app.In == nil {
		return os.Stdin
	}
	return app.In
}
