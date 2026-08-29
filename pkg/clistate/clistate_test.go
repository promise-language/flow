package clistate_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
		BackendName: "fake",
		Owner:       "alice",
		ItemRef: flow.ItemRef{
			BackendName: "fake",
			Display:     "test#1",
			Ref:         json.RawMessage(`"1"`),
		},
	}
	if err := clistate.Save(claim); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := clistate.Load()
	if err != nil || got == nil {
		t.Fatalf("Load: (%v, %v)", got, err)
	}
	if got.Owner != "alice" {
		t.Errorf("Owner = %q, want alice", got.Owner)
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
		if rel, rerr := filepath.Rel(clistate.WorkDir(), path); rerr != nil || !filepath.IsLocal(rel) {
			t.Errorf("record landed at %s, outside %s", path, clistate.WorkDir())
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

	if err := clistate.Save(flow.Claim{BackendName: "fake", Owner: "alice"}); err != nil {
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
