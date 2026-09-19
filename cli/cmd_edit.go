package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/promise-language/flow"
)

type editPayload struct {
	Item string `json:"item"`
	// Changed names each field the edit staged, in the order editFlagNames
	// declares them. It is what landed, not what was typed in what order: an
	// operator reading the report, and anything acting on it, wants the effect.
	Changed []string `json:"changed"`
}

// editFlagNames is the set of flags that stage a change, in the order they are
// reported. ONE LIST: the staged set is derived from it and the usage error
// names it, so a flag added to the command is added to both by being added
// here.
func editFlagNames() []string {
	return []string{
		"title", "body", "body-file",
		"add-tag", "remove-tag", "block-on", "unblock",
		"priority", "urgency",
	}
}

// cmdEdit changes the item itself — the one thing the command set could not do.
//
// ONE COMMAND, NOT ONE PER FIELD, because ItemEditor is a transaction and a
// command per field would be a transaction per field: `edit --title X
// --add-tag y` is one Commit, so an operator never has to ask which half took.
// The flags are one per ItemEditor method and carry no rules of their own —
// every refusal about what can be written together is the orchestrator's, and
// arrives at Commit.
//
// IT TAKES NO CLAIM (docs/orchestrator.md § Editing). An operator recording a
// dependency or correcting a title does not hold the item, and usually that
// nobody holds it is the point — so, like `answer`, this addresses the item by
// id and works from any machine.
func (app *App) cmdEdit(ctx context.Context, args []string) int {
	fs := app.newFlagSet("edit")
	of := addOutputFlags(fs)
	title := fs.String("title", "", "replace the title")
	body := fs.String("body", "", "replace the body")
	bodyFile := fs.String("body-file", "", "replace the body with a file's contents")
	var addTags, removeTags, blockOn, unblock stringSliceFlag
	fs.Var(&addTags, "add-tag", "add a tag (repeatable)")
	fs.Var(&removeTags, "remove-tag", "remove a tag (repeatable)")
	fs.Var(&blockOn, "block-on", "record that this item waits on another (repeatable)")
	fs.Var(&unblock, "unblock", "retract a dependency (repeatable)")
	priority := fs.String("priority", "", "assessed priority: "+joinNames(flow.AllPriorities()))
	urgency := fs.String("urgency", "", "what to do about it now: "+joinNames(flow.AllUrgencies()))
	if !app.parseArgs(fs, args) {
		return 2
	}
	// Which flags the operator actually TYPED, not what they hold: clearing a
	// body is `--body ""`, and a zero-value check would silently drop it.
	// Same reason `grant` collects the set it was given.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	// Before arity, as `claim` decides it: a contradiction about how to render
	// the report is settled before anything about what the report is of.
	mode, ok := of.mode(app, "edit")
	if !ok {
		return 2
	}
	if fs.NArg() == 0 {
		return app.usageError("edit: no item given (edit takes <item-id> and at least one change)")
	}
	if fs.NArg() > 1 {
		return app.usageError("edit: unexpected argument %q (edit takes one <item-id>)", fs.Arg(1))
	}
	if set["body"] && set["body-file"] {
		return app.usageError("edit: --body and --body-file both give the body — pass one")
	}

	// The closed vocabularies are checked HERE, before the editor opens, and
	// rejected by name. The orchestrator refuses them too — it must, being
	// reachable without this command — but an invocation naming a value that
	// exists nowhere is malformed rather than refused, and exit 2 is what says
	// so. Nothing is written on this path.
	if set["priority"] && !flow.Priority(*priority).Valid() {
		return app.usageError("edit: unknown priority %q (valid: %s)", *priority, joinNames(flow.AllPriorities()))
	}
	if set["urgency"] && !flow.Urgency(*urgency).Valid() {
		return app.usageError("edit: unknown urgency %q (valid: %s)", *urgency, joinNames(flow.AllUrgencies()))
	}

	var staged []string
	for _, name := range editFlagNames() {
		if set[name] {
			staged = append(staged, name)
		}
	}
	if len(staged) == 0 {
		// An edit that stages nothing is not "there was nothing to do" — it is
		// an invocation that asked for nothing. Reporting success for it would
		// tell an operator who mistyped a flag that their change landed.
		return app.usageError("edit: no change given (pass at least one of --%s)",
			strings.Join(editFlagNames(), ", --"))
	}

	bodyText := *body
	if set["body-file"] {
		content, err := os.ReadFile(*bodyFile)
		if err != nil {
			// A well-formed invocation naming a file that is not there: the
			// condition is the environment's, not the command line's.
			fmt.Fprintln(app.Err, "edit:", err)
			return 1
		}
		bodyText = string(content)
	}

	ref, err := app.resolveClaimRef(ctx, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(app.Err, "edit:", err)
		return 1
	}
	// Every blocker reference goes through the same resolver the item did, and
	// BEFORE the editor opens — so a typo among them costs no write at all.
	blockOnRefs, ok := app.resolveRefs(ctx, "edit", blockOn)
	if !ok {
		return 1
	}
	unblockRefs, ok := app.resolveRefs(ctx, "edit", unblock)
	if !ok {
		return 1
	}

	ed, err := app.Orchestrator.Edit(ctx, ref)
	if err != nil {
		fmt.Fprintln(app.Err, "edit:", err)
		return 1
	}
	if set["title"] {
		ed.SetTitle(*title)
	}
	if set["body"] || set["body-file"] {
		ed.SetBody(bodyText)
	}
	for _, t := range addTags {
		ed.AddTag(flow.TagId(t))
	}
	for _, t := range removeTags {
		ed.RemoveTag(flow.TagId(t))
	}
	for _, r := range blockOnRefs {
		ed.AddBlocker(r)
	}
	for _, r := range unblockRefs {
		ed.RemoveBlocker(r)
	}
	if set["priority"] {
		ed.SetPriority(flow.Priority(*priority))
	}
	if set["urgency"] {
		ed.SetUrgency(flow.Urgency(*urgency))
	}

	if err := ed.Commit(ctx); err != nil {
		// The orchestrator's own message, carried through unmodified — it is
		// the actionable part. The github editor refuses a stage mixing item
		// fields with blockers, and one staging several dependency changes,
		// both with ErrUnsupported and both naming the way out ("split it into
		// two edits"). Substituting a generic "does not support edit" would
		// throw that instruction away and report a permanent limitation where
		// what there is is a combination to re-shape.
		fmt.Fprintln(app.Err, conditionOrError("edit", err))
		return 1
	}

	return app.emit(mode, editPayload{Item: ref.Display, Changed: staged}, func() {
		fmt.Fprintf(app.Out, "edited %s — %s\n", ref.Display, strings.Join(staged, ", "))
	})
}

// resolveRefs turns typed item ids into refs through the one resolver, naming
// the first that does not resolve. It reports before any write, so a typo in a
// blocker costs nothing.
func (app *App) resolveRefs(ctx context.Context, cmd string, ids []string) ([]flow.ItemRef, bool) {
	refs := make([]flow.ItemRef, 0, len(ids))
	for _, id := range ids {
		ref, err := app.resolveClaimRef(ctx, id)
		if err != nil {
			fmt.Fprintf(app.Err, "%s: %s: %v\n", cmd, id, err)
			return nil, false
		}
		refs = append(refs, ref)
	}
	return refs, true
}
