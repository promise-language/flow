package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

func TestCmdRemark_Records(t *testing.T) {
	app, be, out, errBuf := editTestSetup(t)

	code := app.cmdRemark(context.Background(), []string{"1", "released: the arena was needed elsewhere"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "remarked on 1") {
		t.Errorf("stdout = %q, want 'remarked on 1'", out.String())
	}
	got := be.Remarks("1")
	if len(got) != 1 || got[0] != "released: the arena was needed elsewhere" {
		t.Errorf("remarks = %v, want the one text recorded", got)
	}
}

// --body-file is what the observed workaround used (`gh issue comment
// --body-file`), and it must carry the file verbatim.
func TestCmdRemark_BodyFile(t *testing.T) {
	app, be, _, errBuf := editTestSetup(t)
	path := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(path, []byte("what was found\n\nand why"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := app.cmdRemark(context.Background(), []string{"1", "--body-file", path}); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if got := be.Remarks("1"); len(got) != 1 || got[0] != "what was found\n\nand why" {
		t.Errorf("remarks = %v, want the file's contents", got)
	}
}

func TestCmdRemark_BodyFileUnreadable(t *testing.T) {
	app, be, out, _ := editTestSetup(t)

	code := app.cmdRemark(context.Background(),
		[]string{"1", "--body-file", filepath.Join(t.TempDir(), "absent.md")})
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty on a refusal in human mode", out.String())
	}
	if len(be.Remarks("1")) != 0 {
		t.Error("a remark was recorded from a file that could not be read")
	}
}

// Nothing was said, so nothing is published: an empty comment on the item
// cannot be taken back, and it says nothing when it arrives.
func TestCmdRemark_EmptyTextRecordsNothing(t *testing.T) {
	for _, text := range []string{"", "   \n\t "} {
		app, be, out, errBuf := editTestSetup(t)
		code := app.cmdRemark(context.Background(), []string{"1", text})
		if code != 1 {
			t.Fatalf("%q: exit = %d, want 1", text, code)
		}
		if out.String() != "" {
			t.Errorf("%q: stdout = %q, want empty", text, out.String())
		}
		if !strings.Contains(errBuf.String(), "nothing was recorded") {
			t.Errorf("%q: stderr = %q, want 'nothing was recorded'", text, errBuf.String())
		}
		if len(be.Remarks("1")) != 0 {
			t.Errorf("%q: a remark was recorded", text)
		}
	}
}

// The id is first and always required. A bare-number remark is the case that
// breaks an overloaded positional, and it must land as TEXT on the named item.
func TestCmdRemark_IdIsNeverOverloaded(t *testing.T) {
	app, be, _, errBuf := editTestSetup(t)

	if code := app.cmdRemark(context.Background(), []string{"1", "2"}); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if got := be.Remarks("1"); len(got) != 1 || got[0] != "2" {
		t.Errorf("remarks on 1 = %v, want the text \"2\"", got)
	}
	if got := be.Remarks("2"); len(got) != 0 {
		t.Errorf("remarks on 2 = %v, want none — \"2\" was the text, not the item", got)
	}
}

func TestCmdRemark_UsageErrors(t *testing.T) {
	cases := []struct {
		name, reason string
		args         []string
	}{
		{"no args", "remark: no item given (remark takes <item-id> and the text)", nil},
		{"no text", "remark: no text given (remark takes <item-id> and the text)", []string{"1"}},
		{"too many", `remark: unexpected argument "third" (remark takes <item-id> and one text)`,
			[]string{"1", "text", "third"}},
		{"text twice", "remark: the text was given twice — pass <text> or --body-file, not both",
			[]string{"1", "text", "--body-file", "f"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, be, out, errBuf := editTestSetup(t)
			code := app.cmdRemark(context.Background(), tc.args)
			checkUsageError(t, "remark "+tc.name, out.String(), errBuf.String(), code, tc.reason)
			if len(be.Remarks("1")) != 0 {
				t.Error("a remark was recorded for a malformed invocation")
			}
		})
	}
}

// An orchestrator with nowhere to keep prose has no combination to re-shape
// and no way to be asked again, so the named line IS the whole answer here —
// unlike `edit`, where the sentinel carries an instruction.
func TestCmdRemark_UnsupportedBackend(t *testing.T) {
	app, be, _, errBuf := editTestSetup(t)
	app.Orchestrator = &unsupportedRemarkBackend{Orchestrator: be}

	if code := app.cmdRemark(context.Background(), []string{"1", "text"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "does not support remark") {
		t.Errorf("stderr = %q, want 'does not support remark'", errBuf.String())
	}
}

// A disclosure refusal names what it found, and that is what makes it
// actionable — it must reach the operator rather than being summarised away.
func TestCmdRemark_DisclosureRefusalReachesTheOperator(t *testing.T) {
	app, be, out, errBuf := editTestSetup(t)
	app.Orchestrator = &refusingRemarkBackend{
		Orchestrator: be,
		err: flow.ErrDisclosureRefused{
			Act:    flow.ActRemark,
			Reason: errors.New("local filesystem path: /home/someone/prog/flow"),
		},
	}

	if code := app.cmdRemark(context.Background(), []string{"1", "see /home/someone/prog/flow"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	if !strings.Contains(errBuf.String(), "/home/someone/prog/flow") {
		t.Errorf("stderr = %q, want what the guard caught", errBuf.String())
	}
}

func TestCmdRemark_ItemNotResolvable(t *testing.T) {
	app, be, _, errBuf := editTestSetup(t)
	app.Orchestrator = &unresolvableBackend{Orchestrator: be}

	if code := app.cmdRemark(context.Background(), []string{"nope", "text"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "no such item") {
		t.Errorf("stderr = %q, want the resolver's message", errBuf.String())
	}
}

// Recording why an item was released is done by whoever released it, from
// wherever they are: no claim, on this item or any other.
func TestCmdRemark_NeedsNoClaim(t *testing.T) {
	app, be, _, errBuf := editTestSetup(t)
	ctx := context.Background()

	if claim, err := be.LookupActiveClaim(ctx); err != nil || claim != nil {
		t.Fatalf("LookupActiveClaim = %v, %v; want no claim held", claim, err)
	}
	if code := app.cmdRemark(ctx, []string{"1", "unheld"}); code != 0 {
		t.Fatalf("unheld: exit = %d, stderr=%q", code, errBuf.String())
	}
	if _, err := be.Claim(ctx, be.Ref("2"), nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if code := app.cmdRemark(ctx, []string{"1", "other item held"}); code != 0 {
		t.Fatalf("other item held: exit = %d, stderr=%q", code, errBuf.String())
	}
	if got := be.Remarks("1"); len(got) != 2 {
		t.Errorf("remarks = %v, want both recorded", got)
	}
}

func TestCmdRemarkJSON_Schema(t *testing.T) {
	app, _, out, errBuf := editTestSetup(t)
	if code := app.cmdRemark(context.Background(), []string{"--json", "1", "text"}); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errBuf.String())
	}
	m := decode(t, out)
	for _, key := range []string{"item", "posted"} {
		if _, ok := m[key]; !ok {
			t.Errorf("payload %v missing %q", m, key)
		}
	}
}

// The fake refuses an empty remark itself — the CLI's own check is the earlier
// of two, and the contract's is what holds for every other caller.
func TestFakeRemark_RefusesEmptyAndUnknownItem(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "t"})
	ctx := context.Background()

	if err := be.Remark(ctx, be.Ref("1"), "  "); err == nil {
		t.Error("Remark accepted empty text, want a refusal")
	}
	if err := be.Remark(ctx, be.Ref("99"), "text"); err == nil {
		t.Error("Remark accepted an unregistered item, want a refusal")
	}
	if got := be.Remarks("1"); len(got) != 0 {
		t.Errorf("remarks = %v, want none", got)
	}
}

// Remarks accumulate in order: it is an append, and nothing replaces one.
func TestFakeRemark_AccumulatesInOrder(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "t"})
	ctx := context.Background()

	for _, text := range []string{"first", "second"} {
		if err := be.Remark(ctx, be.Ref("1"), text); err != nil {
			t.Fatalf("Remark %q: %v", text, err)
		}
	}
	got := be.Remarks("1")
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("remarks = %v, want [first second]", got)
	}
}

// ---------------------------------------------------------------------------
// Doubles
// ---------------------------------------------------------------------------

type unsupportedRemarkBackend struct {
	*fake.Orchestrator
}

func (b *unsupportedRemarkBackend) Remark(ctx context.Context, ref flow.ItemRef, text string) error {
	return flow.ErrUnsupported
}

type refusingRemarkBackend struct {
	*fake.Orchestrator
	err error
}

func (b *refusingRemarkBackend) Remark(ctx context.Context, ref flow.ItemRef, text string) error {
	return b.err
}
