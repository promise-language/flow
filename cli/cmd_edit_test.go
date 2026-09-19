package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// editTestSetup builds an App holding no claim at all. THAT IS THE POINT: an
// operator correcting a title or retracting a blocker does not hold the item,
// and docs/orchestrator.md § Editing makes editing claim-free for exactly that
// case. Every test here runs against an unheld item unless it says otherwise.
func editTestSetup(t *testing.T) (*App, *fake.Orchestrator, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "as filed", Body: "the original body"})
	be.AddItem("2", flow.Item{Type: "task", Title: "the other one"})
	app := &App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flow:         newDummyFlow("x"),
		Coverage:     []flow.RoleName{"contributor"},
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	app.Out, app.Err = out, errBuf
	return app, be, out, errBuf
}

func loadItem(t *testing.T, be *fake.Orchestrator, id string) *flow.Item {
	t.Helper()
	item, err := be.Load(context.Background(), be.Ref(id))
	if err != nil {
		t.Fatalf("Load %s: %v", id, err)
	}
	return item
}

// Every field the command offers reaches the editor, and the write is visible
// on a subsequent Load. The re-read is the assertion: a command that printed
// "edited" and staged nothing would pass a stdout check.
func TestCmdEdit_EachFieldLands(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want func(*testing.T, *flow.Item)
	}{
		{"title", []string{"1", "--title", "corrected"}, func(t *testing.T, it *flow.Item) {
			if it.Title != "corrected" {
				t.Errorf("title = %q, want %q", it.Title, "corrected")
			}
		}},
		{"body", []string{"1", "--body", "rewritten"}, func(t *testing.T, it *flow.Item) {
			if it.Body != "rewritten" {
				t.Errorf("body = %q, want %q", it.Body, "rewritten")
			}
		}},
		{"add-tag", []string{"1", "--add-tag", "cli", "--add-tag", "orchestrator"}, func(t *testing.T, it *flow.Item) {
			for _, want := range []flow.TagId{"cli", "orchestrator"} {
				if !slices.Contains(it.Tags, want) {
					t.Errorf("tags = %v, want %q among them", it.Tags, want)
				}
			}
		}},
		{"priority", []string{"1", "--priority", "critical"}, func(t *testing.T, it *flow.Item) {
			if it.Priority != flow.PriorityCritical {
				t.Errorf("priority = %q, want critical", it.Priority)
			}
		}},
		{"urgency", []string{"1", "--urgency", "deferred"}, func(t *testing.T, it *flow.Item) {
			if it.Urgency != flow.UrgencyDeferred {
				t.Errorf("urgency = %q, want deferred", it.Urgency)
			}
		}},
		{"block-on", []string{"1", "--block-on", "2"}, func(t *testing.T, it *flow.Item) {
			if len(it.BlockedBy) != 1 || it.BlockedBy[0].Ref.Display != "2" {
				t.Errorf("blockedBy = %v, want the one blocker 2", it.BlockedBy)
			}
			if !it.Blocked {
				t.Error("item is not blocked, want blocked by an open blocker")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, be, out, errBuf := editTestSetup(t)
			if code := app.cmdEdit(context.Background(), tc.args); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr=%q", code, errBuf.String())
			}
			if !strings.Contains(out.String(), "edited 1") {
				t.Errorf("stdout = %q, want 'edited 1'", out.String())
			}
			tc.want(t, loadItem(t, be, "1"))
		})
	}
}

// Removing is the other half of each pair, and it has to be reachable: a
// vocabulary that could only be added to would leave a tag or a dependency
// permanent once recorded.
func TestCmdEdit_RemovesTagAndBlocker(t *testing.T) {
	app, be, _, errBuf := editTestSetup(t)
	ctx := context.Background()

	if code := app.cmdEdit(ctx, []string{"1", "--add-tag", "cli"}); code != 0 {
		t.Fatalf("add: exit = %d, stderr=%q", code, errBuf.String())
	}
	if code := app.cmdEdit(ctx, []string{"1", "--block-on", "2"}); code != 0 {
		t.Fatalf("block: exit = %d, stderr=%q", code, errBuf.String())
	}
	if code := app.cmdEdit(ctx, []string{"1", "--remove-tag", "cli"}); code != 0 {
		t.Fatalf("remove-tag: exit = %d, stderr=%q", code, errBuf.String())
	}
	if code := app.cmdEdit(ctx, []string{"1", "--unblock", "2"}); code != 0 {
		t.Fatalf("unblock: exit = %d, stderr=%q", code, errBuf.String())
	}

	it := loadItem(t, be, "1")
	if slices.Contains(it.Tags, flow.TagId("cli")) {
		t.Errorf("tags = %v, want 'cli' gone", it.Tags)
	}
	if len(it.BlockedBy) != 0 {
		t.Errorf("blockedBy = %v, want empty after --unblock", it.BlockedBy)
	}
}

// Several changes in one invocation are ONE transaction: one Edit, one Commit.
// A command per field would be a transaction per field, which is the property
// the single command exists to keep.
func TestCmdEdit_SeveralChangesAreOneTransaction(t *testing.T) {
	app, be, _, errBuf := editTestSetup(t)
	counting := &countingEditBackend{Orchestrator: be}
	app.Orchestrator = counting

	code := app.cmdEdit(context.Background(),
		[]string{"1", "--title", "corrected", "--add-tag", "cli", "--priority", "high"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if counting.edits != 1 || counting.commits != 1 {
		t.Errorf("Edit/Commit = %d/%d, want 1/1", counting.edits, counting.commits)
	}
	it := loadItem(t, be, "1")
	if it.Title != "corrected" || it.Priority != flow.PriorityHigh || !slices.Contains(it.Tags, flow.TagId("cli")) {
		t.Errorf("item = %+v, want all three changes landed", it)
	}
}

// --body-file is what an operator revising a long description actually reaches
// for; it must produce exactly what --body would.
func TestCmdEdit_BodyFileIsTheSameAsBody(t *testing.T) {
	app, be, _, errBuf := editTestSetup(t)
	path := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(path, []byte("from a file\n\nwith paragraphs"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := app.cmdEdit(context.Background(), []string{"1", "--body-file", path}); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if got := loadItem(t, be, "1").Body; got != "from a file\n\nwith paragraphs" {
		t.Errorf("body = %q, want the file's contents", got)
	}
}

// `changed` reports the FIELD, so the two ways of giving the body report the
// same one. Reporting the flag would put two names on one effect, and anything
// acting on the report would have to know both to learn the body moved.
func TestCmdEdit_BodyFileReportsTheBodyField(t *testing.T) {
	app, _, out, errBuf := editTestSetup(t)
	path := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(path, []byte("from a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := app.cmdEdit(context.Background(), []string{"--json", "1", "--body-file", path}); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errBuf.String())
	}
	changed, _ := decode(t, out)["changed"].([]any)
	if len(changed) != 1 || changed[0] != "body" {
		t.Errorf("changed = %v, want [body] — the field, not the flag that carried it", changed)
	}
}

// An unreadable --body-file is an environment condition, not a malformed
// invocation: the command line is well-formed and the file is not there.
func TestCmdEdit_BodyFileUnreadable(t *testing.T) {
	app, be, out, errBuf := editTestSetup(t)
	before := loadItem(t, be, "1").Body

	code := app.cmdEdit(context.Background(),
		[]string{"1", "--body-file", filepath.Join(t.TempDir(), "absent.md")})
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty on a refusal in human mode", out.String())
	}
	if !strings.Contains(errBuf.String(), "edit:") {
		t.Errorf("stderr = %q, want the command's prefix", errBuf.String())
	}
	if got := loadItem(t, be, "1").Body; got != before {
		t.Errorf("body = %q, want it unchanged at %q", got, before)
	}
}

// Clearing a field is `--body ""`, so the flag being GIVEN is what stages the
// change — a check on the value would silently drop it.
func TestCmdEdit_EmptyBodyClearsIt(t *testing.T) {
	app, be, _, errBuf := editTestSetup(t)
	if code := app.cmdEdit(context.Background(), []string{"1", "--body", ""}); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if got := loadItem(t, be, "1").Body; got != "" {
		t.Errorf("body = %q, want it cleared", got)
	}
}

// Usage errors: every one is decided before anything is written, and each is
// checked for the two-line shape every malformed invocation takes.
func TestCmdEdit_UsageErrors(t *testing.T) {
	cases := []struct {
		name, reason string
		args         []string
	}{
		{"no item", "edit: no item given (edit takes <item-id> and at least one change)",
			[]string{"--title", "x"}},
		{"two items", `edit: unexpected argument "2" (edit takes one <item-id>)`,
			[]string{"1", "2", "--title", "x"}},
		{"body twice", "edit: --body and --body-file both give the body — pass one",
			[]string{"1", "--body", "x", "--body-file", "y"}},
		{"unknown priority", `edit: unknown priority "hihg" (valid: critical, high, medium, low)`,
			[]string{"1", "--priority", "hihg"}},
		{"unknown urgency", `edit: unknown urgency "soon" (valid: next, default, deferred)`,
			[]string{"1", "--urgency", "soon"}},
		{"no change", "edit: no change given (pass at least one of --title, --body, --body-file, --add-tag, --remove-tag, --block-on, --unblock, --priority, --urgency)",
			[]string{"1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, be, out, errBuf := editTestSetup(t)
			guard := &refusingEditBackend{Orchestrator: be}
			app.Orchestrator = guard

			code := app.cmdEdit(context.Background(), tc.args)
			checkUsageError(t, "edit "+tc.name, out.String(), errBuf.String(), code, tc.reason)
			// Nothing was written, and nothing was even opened: a malformed
			// invocation is rejected before the command takes any action.
			if guard.opened {
				t.Error("an editor was opened for a malformed invocation")
			}
		})
	}
}

// A value outside a closed vocabulary is rejected BY NAME and BEFORE the
// editor opens. The orchestrator refuses it too — it must, being reachable
// without this command — but reaching the orchestrator for it would report a
// backend refusal where the invocation was the problem.
func TestCmdEdit_UnknownPriorityWritesNothing(t *testing.T) {
	app, be, _, _ := editTestSetup(t)
	before := loadItem(t, be, "1").Priority

	if code := app.cmdEdit(context.Background(), []string{"1", "--title", "x", "--priority", "hihg"}); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	it := loadItem(t, be, "1")
	if it.Priority != before {
		t.Errorf("priority = %q, want it unchanged at %q", it.Priority, before)
	}
	if it.Title == "x" {
		t.Error("the title landed; a rejected invocation must write nothing at all")
	}
}

// A blocker reference that does not resolve is named, and costs no write: the
// refs are resolved before the editor opens.
func TestCmdEdit_UnresolvableBlockerRef(t *testing.T) {
	app, be, out, errBuf := editTestSetup(t)
	guard := &refusingEditBackend{Orchestrator: be}
	app.Orchestrator = guard

	code := app.cmdEdit(context.Background(), []string{"1", "--block-on", ""})
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, errBuf.String())
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	if !strings.Contains(errBuf.String(), "edit:") {
		t.Errorf("stderr = %q, want the command's prefix", errBuf.String())
	}
	if guard.opened {
		t.Error("an editor was opened for a blocker that does not resolve")
	}
}

// A blocker the orchestrator refuses — one naming nothing, and a self-blocker
// — comes back at Commit, and the orchestrator's own words reach the operator.
func TestCmdEdit_BlockerRefusedAtCommit(t *testing.T) {
	cases := []struct {
		name, arg, want string
	}{
		{"names no item", "99", "names no item"},
		{"itself", "1", "its own blocker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, be, out, errBuf := editTestSetup(t)
			code := app.cmdEdit(context.Background(), []string{"1", "--block-on", tc.arg})
			if code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			if out.String() != "" {
				t.Errorf("stdout = %q, want empty on a refusal in human mode", out.String())
			}
			if !strings.Contains(errBuf.String(), tc.want) {
				t.Errorf("stderr = %q, want %q", errBuf.String(), tc.want)
			}
			if bl := loadItem(t, be, "1").BlockedBy; len(bl) != 0 {
				t.Errorf("blockedBy = %v, want nothing recorded", bl)
			}
		})
	}
}

// ErrUnsupported carries the way out, and `edit` must not replace it. The
// github editor refuses a stage mixing item fields with blockers, and one
// staging several dependency changes, with that sentinel and an instruction —
// a generic "does not support edit" would throw the instruction away and
// report a permanent limitation where there is a combination to re-shape.
func TestCmdEdit_UnsupportedCombinationKeepsItsMessage(t *testing.T) {
	app, be, _, errBuf := editTestSetup(t)
	app.Orchestrator = &commitFailsBackend{
		Orchestrator: be,
		err:          fmt.Errorf("github: split it into two edits: %w", flow.ErrUnsupported),
	}

	if code := app.cmdEdit(context.Background(), []string{"1", "--title", "x"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "split it into two edits") {
		t.Errorf("stderr = %q, want the orchestrator's own instruction", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "does not support edit") {
		t.Errorf("stderr = %q, want no substituted generic line", errBuf.String())
	}
}

// ErrUnavailable is "not right now", and reads as the condition it is — the
// same rendering resolve gives it, through the one helper.
func TestCmdEdit_UnavailableReadsAsACondition(t *testing.T) {
	app, be, _, errBuf := editTestSetup(t)
	app.Orchestrator = &commitFailsBackend{
		Orchestrator: be,
		err:          fmt.Errorf("github: the title moved since this edit opened: %w", flow.ErrUnavailable),
	}

	if code := app.cmdEdit(context.Background(), []string{"1", "--title", "x"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "waiting on a condition") {
		t.Errorf("stderr = %q, want the condition rendering", errBuf.String())
	}
}

// An item nobody holds, and an item another arena holds, are both editable.
// This is the whole reason editing takes no claim: a title is corrected, and a
// backlog is re-prioritised, by somebody who holds none of it.
func TestCmdEdit_NeedsNoClaim(t *testing.T) {
	app, be, _, errBuf := editTestSetup(t)
	ctx := context.Background()

	if claim, err := be.LookupActiveClaim(ctx); err != nil || claim != nil {
		t.Fatalf("LookupActiveClaim = %v, %v; want no claim held", claim, err)
	}
	if code := app.cmdEdit(ctx, []string{"1", "--title", "unheld"}); code != 0 {
		t.Fatalf("unheld: exit = %d, stderr=%q", code, errBuf.String())
	}

	// And with a claim on a DIFFERENT item: the arena holds one thing and edits
	// another, which nothing about editing objects to.
	if _, err := be.Claim(ctx, be.Ref("2"), nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if code := app.cmdEdit(ctx, []string{"1", "--title", "still editable"}); code != 0 {
		t.Fatalf("other item held: exit = %d, stderr=%q", code, errBuf.String())
	}
	if got := loadItem(t, be, "1").Title; got != "still editable" {
		t.Errorf("title = %q, want %q", got, "still editable")
	}
}

func TestCmdEdit_ItemNotResolvable(t *testing.T) {
	app, be, out, errBuf := editTestSetup(t)
	app.Orchestrator = &unresolvableBackend{Orchestrator: be}

	if code := app.cmdEdit(context.Background(), []string{"nope", "--title", "x"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	if !strings.Contains(errBuf.String(), "no such item") {
		t.Errorf("stderr = %q, want the resolver's message", errBuf.String())
	}
}

// The payload's field names are the machine contract.
func TestCmdEditJSON_Schema(t *testing.T) {
	app, _, out, errBuf := editTestSetup(t)
	code := app.cmdEdit(context.Background(), []string{"--json", "1", "--title", "x", "--add-tag", "cli"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errBuf.String())
	}
	m := decode(t, out)
	for _, key := range []string{"item", "changed"} {
		if _, ok := m[key]; !ok {
			t.Errorf("payload %v missing %q", m, key)
		}
	}
	changed, _ := m["changed"].([]any)
	if len(changed) != 2 || changed[0] != "title" || changed[1] != "add-tag" {
		t.Errorf("changed = %v, want [title add-tag] in declaration order", changed)
	}
}

// Both commands are reachable through the dispatch, not only by calling the
// method: registration in the switch and in the usage registry are separate
// facts, and a command in one and not the other is the failure this guards.
func TestEditAndRemark_DispatchEndToEnd(t *testing.T) {
	app, be, out, errBuf := editTestSetup(t)

	if code := RunWithArgs(*app, []string{"edit", "1", "--title", "through dispatch"}); code != 0 {
		t.Fatalf("edit: exit = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if code := RunWithArgs(*app, []string{"remark", "1", "through dispatch"}); code != 0 {
		t.Fatalf("remark: exit = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if got := loadItem(t, be, "1").Title; got != "through dispatch" {
		t.Errorf("title = %q, want the edit to have landed", got)
	}
	if got := be.Remarks("1"); len(got) != 1 || got[0] != "through dispatch" {
		t.Errorf("remarks = %v, want the remark to have landed", got)
	}
	if !strings.Contains(out.String(), "edited 1") || !strings.Contains(out.String(), "remarked on 1") {
		t.Errorf("stdout = %q, want both reports", out.String())
	}
}

// ---------------------------------------------------------------------------
// Doubles
// ---------------------------------------------------------------------------

// countingEditBackend counts editors opened and commits taken, so a test can
// prove one invocation is one transaction.
type countingEditBackend struct {
	*fake.Orchestrator
	edits, commits int
}

func (b *countingEditBackend) Edit(ctx context.Context, ref flow.ItemRef) (flow.ItemEditor, error) {
	inner, err := b.Orchestrator.Edit(ctx, ref)
	if err != nil {
		return nil, err
	}
	b.edits++
	return &countingEditor{ItemEditor: inner, be: b}, nil
}

type countingEditor struct {
	flow.ItemEditor
	be *countingEditBackend
}

func (e *countingEditor) Commit(ctx context.Context) error {
	e.be.commits++
	return e.ItemEditor.Commit(ctx)
}

// refusingEditBackend records that Edit was reached at all — how a test proves
// a rejection happened before the command took any action.
type refusingEditBackend struct {
	*fake.Orchestrator
	opened bool
}

func (b *refusingEditBackend) Edit(ctx context.Context, ref flow.ItemRef) (flow.ItemEditor, error) {
	b.opened = true
	return b.Orchestrator.Edit(ctx, ref)
}

// commitFailsBackend fails the commit with a caller-supplied error, which is
// how the sentinel renderings are exercised without a live GitHub.
type commitFailsBackend struct {
	*fake.Orchestrator
	err error
}

func (b *commitFailsBackend) Edit(ctx context.Context, ref flow.ItemRef) (flow.ItemEditor, error) {
	inner, err := b.Orchestrator.Edit(ctx, ref)
	if err != nil {
		return nil, err
	}
	return &commitFailsEditor{ItemEditor: inner, err: b.err}, nil
}

type commitFailsEditor struct {
	flow.ItemEditor
	err error
}

func (e *commitFailsEditor) Commit(ctx context.Context) error { return e.err }

// unresolvableBackend refuses to resolve any id — the fake's own resolver mints
// a ref for every non-empty string, so a "no such item" path needs this.
type unresolvableBackend struct {
	*fake.Orchestrator
}

func (b *unresolvableBackend) ResolveRef(ctx context.Context, input string) (flow.ItemRef, error) {
	return flow.ItemRef{}, errors.New("no such item")
}
