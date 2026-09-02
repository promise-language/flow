package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/promise-language/flow"
)

type answerPayload struct {
	Item       string `json:"item"`
	QuestionID string `json:"question_id"`
	Answered   bool   `json:"answered"`
}

func (app *App) cmdAnswer(ctx context.Context, args []string) int {
	fs := app.newFlagSet("answer")
	question := fs.String("question", "", "which question to answer (required when multiple are pending)")
	of := addOutputFlags(fs)
	if !app.parseArgs(fs, args) {
		return 2
	}
	mode, ok := of.mode(app, "answer")
	if !ok {
		return 2
	}
	if fs.NArg() < 2 {
		return app.usageError("answer: need <item-id> and <text>")
	}
	if fs.NArg() > 2 {
		return app.usageError("answer: unexpected argument %q (answer takes <item-id> <text>)", fs.Arg(2))
	}

	answerer, ok := app.Backend.(flow.QuestionAnswerer)
	if !ok {
		fmt.Fprintln(app.Err, "answer: this backend does not support answering questions")
		return 1
	}
	inspector, ok := app.Backend.(flow.StateInspector)
	if !ok {
		fmt.Fprintln(app.Err, "answer: this backend cannot inspect an item by id without a claim")
		return 1
	}

	ref, err := app.resolveClaimRef(ctx, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(app.Err, "answer:", err)
		return 1
	}
	state, err := inspector.LoadStateByRef(ctx, ref)
	if err != nil {
		fmt.Fprintln(app.Err, "answer:", err)
		return 1
	}

	pending := state.PendingQuestions()
	if len(pending) == 0 {
		fmt.Fprintf(app.Err, "answer: no outstanding questions on %s\n", ref.Display)
		return 1
	}

	var target flow.Question
	switch {
	case *question != "":
		found := false
		for _, q := range pending {
			if q.ID == *question {
				target = q
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(app.Err, "answer: question %q not found among pending questions on %s\n", *question, ref.Display)
			return 1
		}
	case len(pending) == 1:
		target = pending[0]
	default:
		var ids []string
		for _, q := range pending {
			ids = append(ids, q.ID)
		}
		fmt.Fprintf(app.Err, "answer: %d outstanding questions on %s — use --question to name one: %s\n",
			len(pending), ref.Display, strings.Join(ids, ", "))
		return 1
	}

	if err := answerer.PostAnswer(ctx, ref, fs.Arg(1)); err != nil {
		fmt.Fprintln(app.Err, "answer:", err)
		return 1
	}

	payload := answerPayload{
		Item:       ref.Display,
		QuestionID: target.ID,
		Answered:   true,
	}
	return app.emit(mode, payload, func() {
		fmt.Fprintf(app.Out, "answered %s on %s\n", target.ID, ref.Display)
	})
}
