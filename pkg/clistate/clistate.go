// Package clistate exposes the worktree-local active-claim file
// (`.flow/active.json`) as a reusable helper for backends that choose to
// store their lease state on local disk (e.g. the github backend).
//
// The flow.Backend is the authoritative owner of active-claim state — the
// cli commands always go through Backend.LookupActiveClaim, never directly
// through this package. Backends whose lease ledger lives off-host (e.g.
// a tracker server) ignore these helpers entirely; backends whose lease
// IS the local file (github) call them from Claim / LookupActiveClaim /
// Release.
package clistate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/promise-language/flow"
)

const (
	flowDirName   = ".flow"
	activeJSONRel = "active.json"
)

// Dir returns the worktree-local state directory. Tests can override via
// the FLOW_DIR env var.
func Dir() string {
	if d := os.Getenv("FLOW_DIR"); d != "" {
		return d
	}
	return flowDirName
}

// ActiveJSONPath returns the resolved path to active.json.
func ActiveJSONPath() string {
	return filepath.Join(Dir(), activeJSONRel)
}

// Load reads the serialized Claim from `.flow/active.json`. Returns
// (nil, nil) when no active claim exists on disk.
func Load() (*flow.Claim, error) {
	b, err := os.ReadFile(ActiveJSONPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", ActiveJSONPath(), err)
	}
	var c flow.Claim
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ActiveJSONPath(), err)
	}
	return &c, nil
}

// Save writes the Claim to `.flow/active.json`, creating the directory
// if needed.
func Save(c flow.Claim) error {
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", Dir(), err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal claim: %w", err)
	}
	if err := os.WriteFile(ActiveJSONPath(), b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", ActiveJSONPath(), err)
	}
	return nil
}

// Clear removes `.flow/active.json` (and the directory if empty).
// Idempotent — no error if already absent.
func Clear() error {
	if err := os.Remove(ActiveJSONPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", ActiveJSONPath(), err)
	}
	_ = os.Remove(Dir())
	return nil
}
