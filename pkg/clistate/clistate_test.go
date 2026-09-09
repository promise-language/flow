package clistate_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
)

func TestActiveClaimRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_DIR", filepath.Join(dir, ".flow"))

	if got, err := clistate.Load(); got != nil || err != nil {
		t.Errorf("Load on empty dir = (%v, %v), want (nil, nil)", got, err)
	}

	claim := flow.Claim{
		OrchestratorName: "fake",
		Arena:            flow.Arena{Host: "build01", Id: "/w/one"},
		Account:          "alice",
		ItemRef: flow.ItemRef{
			OrchestratorName: "fake",
			Display:          "test#1",
			Ref:              json.RawMessage(`"1"`),
		},
	}
	if err := clistate.Save(claim); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := clistate.Load()
	if err != nil || got == nil {
		t.Fatalf("Load: (%v, %v)", got, err)
	}
	if got.Account != "alice" {
		t.Errorf("Account = %q, want alice", got.Account)
	}
	if got.Arena != (flow.Arena{Host: "build01", Id: "/w/one"}) {
		t.Errorf("Arena = %+v, want build01 /w/one", got.Arena)
	}
	if err := clistate.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".flow", "active.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("active.json should be gone, stat err = %v", err)
	}
}

func TestWorkRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_DIR", filepath.Join(dir, ".flow"))

	if got, err := clistate.LoadWork("42", "plan"); got != "" || err != nil {
		t.Errorf("LoadWork with nothing stored = (%q, %v), want (\"\", nil)", got, err)
	}
	if err := clistate.SaveWork("42", "plan", "half a plan"); err != nil {
		t.Fatalf("SaveWork: %v", err)
	}
	got, err := clistate.LoadWork("42", "plan")
	if err != nil || got != "half a plan" {
		t.Fatalf("LoadWork = (%q, %v), want the stored body", got, err)
	}
	// A second save replaces rather than accumulates: the record is the step's
	// current working-out, not a log of every one it has had.
	if err := clistate.SaveWork("42", "plan", "a better half"); err != nil {
		t.Fatalf("SaveWork (again): %v", err)
	}
	if got, _ := clistate.LoadWork("42", "plan"); got != "a better half" {
		t.Errorf("LoadWork after re-save = %q, want the newer body", got)
	}
	if err := clistate.ClearWork("42", "plan"); err != nil {
		t.Fatalf("ClearWork: %v", err)
	}
	if got, err := clistate.LoadWork("42", "plan"); got != "" || err != nil {
		t.Errorf("LoadWork after ClearWork = (%q, %v), want (\"\", nil)", got, err)
	}
}

// The keying is the correctness property, not the clearing: every path that
// skips a cleanup — a crash, a kill, an abandoned run — leaves a record on
// disk, and reading one that belongs to another item feeds that item's
// reasoning to this item's agent.
func TestWorkIsNotReadableUnderAnotherKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_DIR", filepath.Join(dir, ".flow"))

	if err := clistate.SaveWork("42", "plan", "item 42's reasoning"); err != nil {
		t.Fatalf("SaveWork: %v", err)
	}
	if got, err := clistate.LoadWork("43", "plan"); got != "" || err != nil {
		t.Errorf("LoadWork for another item = (%q, %v), want (\"\", nil)", got, err)
	}
	if got, err := clistate.LoadWork("42", "review"); got != "" || err != nil {
		t.Errorf("LoadWork for another step = (%q, %v), want (\"\", nil)", got, err)
	}
	if got, _ := clistate.LoadWork("42", "plan"); got != "item 42's reasoning" {
		t.Errorf("LoadWork under its own key = %q, want the stored body", got)
	}
}

// The in-file (item, step) pair is the authority, so two ids that sanitise onto
// one path can only LOSE a record, never hand one step another's reasoning.
func TestWorkRefusesARecordWhosePathItShares(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_DIR", filepath.Join(dir, ".flow"))

	if err := clistate.SaveWork("a/b", "plan", "one item's reasoning"); err != nil {
		t.Fatalf("SaveWork: %v", err)
	}
	// "a/b" and "a:b" both sanitise to "a_b".
	if got, err := clistate.LoadWork("a:b", "plan"); got != "" || err != nil {
		t.Errorf("LoadWork for a colliding id = (%q, %v), want (\"\", nil)", got, err)
	}
}

// A traversing id must not write outside the work tree — the ids are
// backend-supplied and used as path components.
func TestWorkStaysUnderTheWorkDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_DIR", filepath.Join(dir, ".flow"))

	if err := clistate.SaveWork("../../escape", "../plan", "nowhere"); err != nil {
		t.Fatalf("SaveWork: %v", err)
	}
	if err := filepath.WalkDir(filepath.Join(dir, ".flow"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if rel, rerr := filepath.Rel(workDir(t), path); rerr != nil || !filepath.IsLocal(rel) {
			t.Errorf("record landed at %s, outside %s", path, workDir(t))
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a record escaped the flow directory, stat err = %v", err)
	}
}

func TestClearWorkIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_DIR", filepath.Join(dir, ".flow"))

	if err := clistate.ClearWork("42", "plan"); err != nil {
		t.Errorf("ClearWork with nothing stored = %v, want nil", err)
	}
	if err := clistate.SaveWork("42", "plan", "body"); err != nil {
		t.Fatalf("SaveWork: %v", err)
	}
	if err := clistate.ClearWork("42", "plan"); err != nil {
		t.Fatalf("ClearWork: %v", err)
	}
	if err := clistate.ClearWork("42", "plan"); err != nil {
		t.Errorf("second ClearWork = %v, want nil", err)
	}
}

// Releasing a claim ends that reasoning's life: prose left on disk after the
// work is over is a disclosure sitting around for no benefit. It is also what
// lets the .flow directory itself go.
func TestClearRemovesEveryWorkRecord(t *testing.T) {
	dir := t.TempDir()
	flowDir := filepath.Join(dir, ".flow")
	t.Setenv("FLOW_DIR", flowDir)

	if err := clistate.Save(flow.Claim{OrchestratorName: "fake", Account: "alice"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	for _, w := range []struct{ item, step string }{{"42", "plan"}, {"42", "review"}, {"43", "plan"}} {
		if err := clistate.SaveWork(w.item, w.step, "reasoning"); err != nil {
			t.Fatalf("SaveWork(%s, %s): %v", w.item, w.step, err)
		}
	}
	if err := clistate.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got, _ := clistate.LoadWork("42", "plan"); got != "" {
		t.Errorf("LoadWork after Clear = %q, want nothing left", got)
	}
	if _, err := os.Stat(flowDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s should be gone once it holds nothing, stat err = %v", flowDir, err)
	}
}

// ---------------------------------------------------------------------------
// Running-step record.
// ---------------------------------------------------------------------------

func TestRunningRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_DIR", filepath.Join(dir, ".flow"))

	rec := clistate.RunningRecord{
		Item: "test#1",
		Step: "write plan",
		PID:  12345,
		Exe:  "/usr/bin/flow",
	}
	if err := clistate.SaveRunning(rec); err != nil {
		t.Fatalf("SaveRunning: %v", err)
	}
	got, err := clistate.LoadRunning()
	if err != nil || got == nil {
		t.Fatalf("LoadRunning: (%v, %v)", got, err)
	}
	if got.Item != rec.Item || got.Step != rec.Step || got.PID != rec.PID || got.Exe != rec.Exe {
		t.Errorf("LoadRunning = %+v, want %+v", *got, rec)
	}
}

func TestLoadRunning_NoFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_DIR", filepath.Join(dir, ".flow"))

	got, err := clistate.LoadRunning()
	if got != nil || err != nil {
		t.Errorf("LoadRunning on empty dir = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestClearRunning_Idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_DIR", filepath.Join(dir, ".flow"))

	// No file exists — must not error.
	if err := clistate.ClearRunning(); err != nil {
		t.Errorf("ClearRunning with nothing stored = %v, want nil", err)
	}
	// Write and clear — must not error.
	if err := clistate.SaveRunning(clistate.RunningRecord{Item: "1", Step: "plan", PID: 1, Exe: "/x"}); err != nil {
		t.Fatalf("SaveRunning: %v", err)
	}
	if err := clistate.ClearRunning(); err != nil {
		t.Fatalf("ClearRunning: %v", err)
	}
	// Second clear — must not error.
	if err := clistate.ClearRunning(); err != nil {
		t.Errorf("second ClearRunning = %v, want nil", err)
	}
	// Verify the file is gone.
	got, err := clistate.LoadRunning()
	if got != nil || err != nil {
		t.Errorf("LoadRunning after ClearRunning = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestClear_RemovesRunning(t *testing.T) {
	dir := t.TempDir()
	flowDir := filepath.Join(dir, ".flow")
	t.Setenv("FLOW_DIR", flowDir)

	if err := clistate.Save(flow.Claim{OrchestratorName: "fake", Account: "alice"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := clistate.SaveRunning(clistate.RunningRecord{Item: "1", Step: "plan", PID: 99, Exe: "/bin/flow"}); err != nil {
		t.Fatalf("SaveRunning: %v", err)
	}
	if err := clistate.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, err := os.Stat(runningJSONPath(t)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("running.json should be gone after Clear, stat err = %v", err)
	}
	if got, err := clistate.LoadRunning(); got != nil || err != nil {
		t.Errorf("LoadRunning after Clear = (%v, %v), want (nil, nil)", got, err)
	}
}

// A corrupt running.json reads as an error, not as a partial record.
func TestLoadRunning_Corrupt(t *testing.T) {
	dir := t.TempDir()
	flowDir := filepath.Join(dir, ".flow")
	t.Setenv("FLOW_DIR", flowDir)

	// Write a valid record first so the directory exists.
	if err := clistate.SaveRunning(clistate.RunningRecord{Item: "1", Step: "plan", PID: 1, Exe: "/x"}); err != nil {
		t.Fatalf("SaveRunning: %v", err)
	}
	// Overwrite with truncated JSON.
	if err := os.WriteFile(runningJSONPath(t), []byte(`{"item":"1","step":"pl`), 0o644); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	got, err := clistate.LoadRunning()
	if err == nil {
		t.Errorf("LoadRunning on corrupt file = (%+v, nil), want an error", got)
	}
	if got != nil {
		t.Errorf("LoadRunning on corrupt file returned non-nil record: %+v", got)
	}
}

// A record whose bytes are not a record — a write cut off by the crash that
// ended the run that was writing it — reads as an error, never as a body. The
// caller is told; what it must never get is a fragment of one, or of something
// that was never a record at all.
func TestLoadWorkRefusesACorruptRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_DIR", filepath.Join(dir, ".flow"))

	if err := clistate.SaveWork("42", "plan", "half a plan"); err != nil {
		t.Fatalf("SaveWork: %v", err)
	}
	path := filepath.Join(workDir(t), "42", "plan.json")
	if err := os.WriteFile(path, []byte(`{"item":"42","step":"pl`), 0o600); err != nil {
		t.Fatalf("truncate the record: %v", err)
	}
	got, err := clistate.LoadWork("42", "plan")
	if err == nil {
		t.Errorf("LoadWork on a truncated record = (%q, nil), want an error", got)
	}
	if got != "" {
		t.Errorf("LoadWork on a truncated record = %q, want nothing", got)
	}
}

// A record with a well-formed but empty body names neither this item nor this
// step, so it reads as absence — the same answer as no file at all.
func TestLoadWorkTreatsARecordNamingNothingAsAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_DIR", filepath.Join(dir, ".flow"))

	if err := clistate.SaveWork("42", "plan", "half a plan"); err != nil {
		t.Fatalf("SaveWork: %v", err)
	}
	path := filepath.Join(workDir(t), "42", "plan.json")
	if err := os.WriteFile(path, []byte(`{"body":"whose is this?"}`), 0o600); err != nil {
		t.Fatalf("rewrite the record: %v", err)
	}
	if got, err := clistate.LoadWork("42", "plan"); got != "" || err != nil {
		t.Errorf("LoadWork = (%q, %v), want (\"\", nil) for a record that names no item", got, err)
	}
}

// The ids are backend-supplied and used as path components, so an id that
// names a directory rather than a name has to be neutralised. `..` is the one
// that costs something: `.flow/work/../plan.json` is `.flow/plan.json`, which
// sits beside active.json and survives the work tree's removal — a record that
// releasing the claim does not take with it.
func TestWorkKeepsDegenerateIdsInsideTheWorkTree(t *testing.T) {
	dir := t.TempDir()
	flowDir := filepath.Join(dir, ".flow")
	t.Setenv("FLOW_DIR", flowDir)

	for _, ids := range []struct{ item, step string }{
		{"..", "plan"}, {".", "plan"}, {"42", ".."}, {"", ""},
	} {
		if err := clistate.SaveWork(ids.item, ids.step, "reasoning"); err != nil {
			t.Fatalf("SaveWork(%q, %q): %v", ids.item, ids.step, err)
		}
	}
	if err := filepath.WalkDir(flowDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if rel, rerr := filepath.Rel(workDir(t), path); rerr != nil || !filepath.IsLocal(rel) {
			t.Errorf("record landed at %s, outside %s", path, workDir(t))
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	// Which is what makes releasing the claim take every one of them.
	if err := clistate.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, err := os.Stat(flowDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s survived Clear, so a record escaped the work tree; stat err = %v", flowDir, err)
	}
}

// ---------------------------------------------------------------------------
// Where the state directory is.
// ---------------------------------------------------------------------------

// workDir and runningJSONPath are the package's path helpers with their error
// turned into a test failure. Every test above sets FLOW_DIR, so locating the
// directory is not what any of them is about.
func workDir(t *testing.T) string {
	t.Helper()
	d, err := clistate.WorkDir()
	if err != nil {
		t.Fatalf("clistate.WorkDir: %v", err)
	}
	return d
}

func runningJSONPath(t *testing.T) string {
	t.Helper()
	p, err := clistate.RunningJSONPath()
	if err != nil {
		t.Fatalf("clistate.RunningJSONPath: %v", err)
	}
	return p
}

// A relative FLOW_DIR is refused rather than resolved. Resolving one against
// the process working directory is the defect this override would otherwise
// re-introduce by hand: the state would follow the operator around instead of
// staying with the checkout.
func TestDirRefusesARelativeOverride(t *testing.T) {
	t.Setenv("FLOW_DIR", ".flow")
	got, err := clistate.Dir()
	if err == nil {
		t.Fatalf("Dir() = %q, want a refusal — a relative state dir is the process working directory", got)
	}
	if !strings.Contains(err.Error(), "FLOW_DIR") {
		t.Errorf("err = %v, want it to name FLOW_DIR, the thing the caller has to fix", err)
	}
	// And every operation refuses with it, rather than one of them inventing a
	// location the others do not share.
	if err := clistate.Save(flow.Claim{OrchestratorName: "fake"}); err == nil {
		t.Error("Save wrote claim state under a relative FLOW_DIR")
	}
	if err := clistate.SaveWork("42", "plan", "reasoning"); err == nil {
		t.Error("SaveWork wrote a record under a relative FLOW_DIR")
	}
	if err := clistate.SaveRunning(clistate.RunningRecord{Item: "1", Step: "plan"}); err == nil {
		t.Error("SaveRunning wrote a record under a relative FLOW_DIR")
	}
}

// FAIL CLOSED. With no override and no checkout to anchor to, every operation
// reports that it does not know where the state directory is — and writes
// nothing. The old Dir() could not say that: it answered `.flow`, resolved
// against wherever the process stood, so a `resolve` started from a parent
// directory wrote its claim state there and orphaned its own lease.
func TestOperationsRefuseWhenTheStateDirCannotBeLocated(t *testing.T) {
	if _, err := flow.DeriveArenaRoot(); err == nil {
		t.Skip("this test binary lives inside a checkout, so the unanchored case cannot be exercised here")
	}
	t.Setenv("FLOW_DIR", "")

	if _, err := clistate.Dir(); err == nil {
		t.Fatal("Dir() answered with no checkout to anchor to")
	}
	for _, op := range []struct {
		name string
		run  func() error
	}{
		{"Save", func() error { return clistate.Save(flow.Claim{OrchestratorName: "fake"}) }},
		{"Clear", clistate.Clear},
		{"SaveWork", func() error { return clistate.SaveWork("42", "plan", "reasoning") }},
		{"LoadWork", func() error { _, err := clistate.LoadWork("42", "plan"); return err }},
		{"ClearWork", func() error { return clistate.ClearWork("42", "plan") }},
		{"ClearItemWork", func() error { return clistate.ClearItemWork("42") }},
		{"SaveRunning", func() error { return clistate.SaveRunning(clistate.RunningRecord{Item: "1"}) }},
		{"LoadRunning", func() error { _, err := clistate.LoadRunning(); return err }},
		{"ClearRunning", clistate.ClearRunning},
		{"Load", func() error { _, err := clistate.Load(); return err }},
	} {
		if err := op.run(); err == nil {
			t.Errorf("%s succeeded with no state directory to write to", op.name)
		}
	}
	// And nothing was created where the old relative name would have put it.
	if _, err := os.Stat(".flow"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".flow was created in the process working directory; stat err = %v", err)
	}
}

// ClearItemWork is what a Reset needs: the flow's whole record on ONE item
// goes, and a draft kept past the journal it belonged to is scratch prose with
// nothing left to resume. Per-item, because one arena's other items are not
// part of the record being cleared — the wholesale Clear above is a different
// operation with a different trigger.
func TestClearItemWorkTakesOneItemsRecordsAndNoOthers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_DIR", filepath.Join(dir, ".flow"))

	for _, w := range []struct{ item, step string }{{"42", "plan"}, {"42", "review"}, {"43", "plan"}} {
		if err := clistate.SaveWork(w.item, w.step, w.item+"/"+w.step); err != nil {
			t.Fatalf("SaveWork(%s, %s): %v", w.item, w.step, err)
		}
	}

	if err := clistate.ClearItemWork("42"); err != nil {
		t.Fatalf("ClearItemWork: %v", err)
	}
	for _, step := range []string{"plan", "review"} {
		if got, err := clistate.LoadWork("42", step); got != "" || err != nil {
			t.Errorf("LoadWork(42, %s) after ClearItemWork = (%q, %v), want (\"\", nil)", step, got, err)
		}
	}
	if got, err := clistate.LoadWork("43", "plan"); got != "43/plan" || err != nil {
		t.Errorf("LoadWork(43, plan) = (%q, %v), want another item's draft untouched", got, err)
	}
}

// Idempotent: an item with no drafts is already in the state ClearItemWork is
// trying to reach, and a reset that failed because there was nothing to clear
// would fail the whole reset.
func TestClearItemWorkIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOW_DIR", filepath.Join(dir, ".flow"))

	if err := clistate.ClearItemWork("42"); err != nil {
		t.Errorf("ClearItemWork with nothing stored = %v, want nil", err)
	}
	if err := clistate.SaveWork("42", "plan", "half a plan"); err != nil {
		t.Fatalf("SaveWork: %v", err)
	}
	if err := clistate.ClearItemWork("42"); err != nil {
		t.Fatalf("ClearItemWork: %v", err)
	}
	if err := clistate.ClearItemWork("42"); err != nil {
		t.Errorf("second ClearItemWork = %v, want nil", err)
	}
}
