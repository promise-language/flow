package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/promise-language/flow"
)

type remarkPayload struct {
	Item   string `json:"item"`
	Posted bool   `json:"posted"`
}

// cmdRemark records prose on the item — what was decided, what was found, why
// something was released.
//
// IT IS NOT `edit`. A remark is an append and the editor's fields are fields:
// folding it into `edit` would either break "all of them or none of them" —
// the one property the editor exists to give — or make a remark unusable in
// combination with anything, since a comment is a third endpoint beside the
// two the github editor already refuses to write together.
//
// THE ITEM ID IS A REQUIRED POSITIONAL and is never overloaded. `answer`
// shares one positional between the id and the text, and decides which is
// which by reading the item — #408 is open against exactly that, because a
// bare-number remark would be read as an item reference and a backend read
// would decide what the invocation meant.
//
// No claim, for the reason `answer` needs none: the party recording a remark
// is not the party holding the item.
func (app *App) cmdRemark(ctx context.Context, args []string) int {
	fs := app.newFlagSet("remark")
	of := addOutputFlags(fs)
	bodyFile := fs.String("body-file", "", "take the remark's text from a file")
	if !app.parseArgs(fs, args) {
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	mode, ok := of.mode(app, "remark")
	if !ok {
		return 2
	}
	if fs.NArg() == 0 {
		return app.usageError("remark: no item given (remark takes <item-id> and the text)")
	}
	if fs.NArg() > 2 {
		return app.usageError("remark: unexpected argument %q (remark takes <item-id> and one text)", fs.Arg(2))
	}
	if fs.NArg() == 2 && set["body-file"] {
		return app.usageError("remark: the text was given twice — pass <text> or --body-file, not both")
	}
	if fs.NArg() == 1 && !set["body-file"] {
		return app.usageError("remark: no text given (remark takes <item-id> and the text)")
	}

	text := ""
	if set["body-file"] {
		content, err := os.ReadFile(*bodyFile)
		if err != nil {
			fmt.Fprintln(app.Err, "remark:", err)
			return 1
		}
		text = string(content)
	} else {
		text = fs.Arg(1)
	}
	if strings.TrimSpace(text) == "" {
		// Nothing was said. Publishing it would put an empty comment on the
		// item that nothing can take back — the same judgement `answer` makes
		// about an operator who is asked and says nothing.
		fmt.Fprintln(app.Err, "remark: the text is empty — nothing was recorded")
		return 1
	}

	ref, err := app.resolveClaimRef(ctx, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(app.Err, "remark:", err)
		return 1
	}

	if err := app.Orchestrator.Remark(ctx, ref, text); err != nil {
		if errors.Is(err, flow.ErrUnsupported) {
			// Unlike `edit`, the generic line is the whole of what there is to
			// say: an orchestrator with nowhere to keep prose has no
			// combination to re-shape and no way to be asked again.
			fmt.Fprintf(app.Err, "remark: backend %q does not support remark\n", app.Orchestrator.Name())
			return 1
		}
		fmt.Fprintln(app.Err, conditionOrError("remark", err))
		return 1
	}

	return app.emit(mode, remarkPayload{Item: ref.Display, Posted: true}, func() {
		fmt.Fprintf(app.Out, "remarked on %s\n", ref.Display)
	})
}
