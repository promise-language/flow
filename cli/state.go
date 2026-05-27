package cli

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

// flowDir is the worktree-local state directory. Tests can override via
// FLOW_DIR env var.
func flowDir() string {
	if d := os.Getenv("FLOW_DIR"); d != "" {
		return d
	}
	return flowDirName
}

func activeJSONPath() string {
	return filepath.Join(flowDir(), activeJSONRel)
}

// LoadActiveClaim reads the serialized Claim from .flow/active.json. Returns
// (nil, nil) if no active lease exists.
func LoadActiveClaim() (*flow.Claim, error) {
	b, err := os.ReadFile(activeJSONPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", activeJSONPath(), err)
	}
	var c flow.Claim
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", activeJSONPath(), err)
	}
	return &c, nil
}

// SaveActiveClaim writes the Claim to .flow/active.json, creating the dir
// if needed.
func SaveActiveClaim(c flow.Claim) error {
	if err := os.MkdirAll(flowDir(), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", flowDir(), err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal claim: %w", err)
	}
	if err := os.WriteFile(activeJSONPath(), b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", activeJSONPath(), err)
	}
	return nil
}

// ClearActiveClaim removes .flow/active.json (and the directory if empty).
// Idempotent — no error if already absent.
func ClearActiveClaim() error {
	if err := os.Remove(activeJSONPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", activeJSONPath(), err)
	}
	// Try to remove the dir if empty; ignore non-empty / not-found errors.
	_ = os.Remove(flowDir())
	return nil
}
