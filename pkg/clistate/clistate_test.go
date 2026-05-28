package clistate_test

import (
	"encoding/json"
	"errors"
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
