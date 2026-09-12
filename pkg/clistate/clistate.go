// Package clistate exposes the worktree-local active-claim file
// (`.flow/active.json`) as a reusable helper for orchestrators that choose to
// store their lease state on local disk (e.g. the github orchestrator).
//
// The flow.Orchestrator is the authoritative owner of active-claim state — the
// cli commands always go through Orchestrator.LookupActiveClaim, never directly
// through this package. An orchestrator whose lease ledger lives off-host (e.g.
// a tracker service) ignores these helpers entirely; one whose lease IS the
// local file (github) calls them from Claim / LookupActiveClaim / Release.
// One file per checkout is what does the arena scoping a fleet-serving
// orchestrator has to do explicitly.
//
// The same directory holds the other per-clone thing a claim owns: the
// work-in-progress records a step leaves for its own next invocation
// (`.flow/draft/<item>/<step>.json`). See SaveWork and flow.WorkInProgress.
package clistate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promise-language/flow"
)

const (
	flowDirName   = ".flow"
	activeJSONRel = "active.json"
	workDirRel    = "draft"
)

// Dir returns the state directory: `.flow` inside the checkout the running
// binary belongs to (flow.DeriveArenaRoot). Tests override it with the FLOW_DIR
// env var, which must be an ABSOLUTE path.
//
// The error return is the whole shape of this. `.flow` was a bare relative name
// resolved against the process working directory, so a `resolve` started from a
// parent directory wrote its claim state THERE — the next invocation from
// inside the checkout found no active claim, and the lease on the item was
// orphaned with nothing local pointing at it. A Dir() that cannot say "I do not
// know where the checkout is" has to invent an answer, and that invented answer
// was the defect. It fails closed instead.
func Dir() (string, error) {
	if d := os.Getenv("FLOW_DIR"); d != "" {
		if !filepath.IsAbs(d) {
			return "", fmt.Errorf("FLOW_DIR %q is relative — it must be an absolute path", d)
		}
		return d, nil
	}
	// The ONE derivation, shared with the orchestrator's WorktreeDir rather
	// than copied: a second copy is a second thing to get wrong, and the two
	// disagreeing would put the claim state outside the arena that holds it.
	root, err := flow.DeriveArenaRoot()
	if err != nil {
		return "", fmt.Errorf("locate the state dir: %w", err)
	}
	return filepath.Join(root, flowDirName), nil
}

// ActiveJSONPath returns the resolved path to active.json.
func ActiveJSONPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, activeJSONRel), nil
}

// Load reads the serialized Claim from `.flow/active.json`. Returns
// (nil, nil) when no active claim exists on disk.
func Load() (*flow.Claim, error) {
	path, err := ActiveJSONPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c flow.Claim
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// Save writes the Claim to `.flow/active.json`, creating the directory
// if needed.
func Save(c flow.Claim) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal claim: %w", err)
	}
	path := filepath.Join(dir, activeJSONRel)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Clear removes the worktree-local claim state: `active.json`, the running
// record, and every work-in-progress record (and the directory if empty).
// Idempotent — no error if already absent.
//
// The work tree goes first, and not only so `os.Remove(Dir())` can succeed
// again: releasing a claim ends that reasoning's life. Prose left on disk after
// the work is over is a disclosure sitting around for no benefit.
func Clear() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	workDir := filepath.Join(dir, workDirRel)
	if err := os.RemoveAll(workDir); err != nil {
		return fmt.Errorf("remove %s: %w", workDir, err)
	}
	if err := ClearRunning(); err != nil {
		return fmt.Errorf("clear running: %w", err)
	}
	active := filepath.Join(dir, activeJSONRel)
	if err := os.Remove(active); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", active, err)
	}
	_ = os.Remove(dir)
	return nil
}

// ---------------------------------------------------------------------------
// Work in progress.
//
// A step that stops without completing — parked on a question, or refused on
// the way out — leaves what it worked out here, and the next dispatch of that
// same step continues from it rather than re-deriving it. See
// flow.WorkInProgress for the contract these implement.
//
// Records live under `.flow/draft/<item>/<step>.json`, beside `active.json`:
// per-clone and gitignored, which is where a local backend's claim state
// already is. They are never published.
// ---------------------------------------------------------------------------

// workRecord is one step's stashed work. Item and Step are stored IN the file
// as well as being in its path: the in-file pair is the authority, so
// sanitising two different ids onto one path can only lose a record, never
// hand one step another's reasoning.
type workRecord struct {
	Item       string    `json:"item"`
	Step       string    `json:"step"`
	RecordedAt time.Time `json:"recorded_at"`
	Body       string    `json:"body"`
}

// WorkDir returns the directory work-in-progress records live under.
func WorkDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, workDirRel), nil
}

// workPath is the file one (item, step) record lives at. Both segments are
// sanitised because they are backend-supplied ids being used as path
// components; the in-file check in LoadWork is what makes a collision safe.
func workPath(item, step string) (string, error) {
	dir, err := WorkDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sanitizeSegment(item), sanitizeSegment(step)+".json"), nil
}

// sanitizeSegment reduces an id to characters that are safe in a path
// component. Anything else becomes "_", so "../escape" cannot walk out of the
// work directory.
func sanitizeSegment(s string) string {
	if s == "" {
		return "_"
	}
	out := []rune(s)
	for i, r := range out {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
		default:
			out[i] = '_'
		}
	}
	// A segment of dots alone would name a directory rather than a record.
	if strings.Trim(string(out), ".") == "" {
		return "_"
	}
	return string(out)
}

// SaveWork stores body as the work in progress for (item, step), replacing
// whatever was there. The file is 0o600 because it may carry text a disclosure
// guard refused to publish.
func SaveWork(item, step, body string) error {
	path, err := workPath(item, step)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	b, err := json.MarshalIndent(workRecord{
		Item:       item,
		Step:       step,
		RecordedAt: time.Now().UTC(),
		Body:       body,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal work record: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// LoadWork returns the work in progress stored for (item, step), or "" when
// there is none.
//
// A record naming a different item or step is not this step's, and reads as
// absence. That check — not the clearing — is what keeps one item's reasoning
// out of another item's agent when a crash, a kill, or an abandoned run leaves
// `.flow/` behind.
func LoadWork(item, step string) (string, error) {
	path, err := workPath(item, step)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	var rec workRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	if rec.Item != item || rec.Step != step {
		return "", nil
	}
	return rec.Body, nil
}

// ClearItemWork removes EVERY work-in-progress record for one item. Idempotent.
//
// What Reset needs: a reset clears the flow's whole record, and a draft kept
// past the journal it belonged to is scratch prose with nothing left to resume.
// Per-item rather than wholesale, because one arena's other items are not part
// of the record being cleared.
func ClearItemWork(item string) error {
	dir, err := WorkDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, sanitizeSegment(item))
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// ClearWork removes the record for (item, step). Idempotent — no error if
// already absent.
func ClearWork(item, step string) error {
	path, err := workPath(item, step)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	// The per-item directory is worth nothing empty; failing to drop it is not
	// worth failing a clear over.
	_ = os.Remove(filepath.Dir(path))
	return nil
}

// ---------------------------------------------------------------------------
// Running-step record.
//
// One step executes at a time in a given worktree (enforced by the claim).
// The record lives at `.flow/running.json`, alongside `active.json`, and
// carries enough to verify liveness: the PID and the executable path. status
// reads it back and confirms the process is alive before reporting "running".
// ---------------------------------------------------------------------------

const runningJSONRel = "running.json"

// RunningJSONPath returns the resolved path to running.json.
func RunningJSONPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, runningJSONRel), nil
}

// RunningRecord identifies the process executing a step.
type RunningRecord struct {
	Item string `json:"item"`
	Step string `json:"step"`
	PID  int    `json:"pid"`
	Exe  string `json:"exe"`
}

// SaveRunning writes the running record to `.flow/running.json`, creating the
// directory if needed.
func SaveRunning(rec RunningRecord) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal running record: %w", err)
	}
	path := filepath.Join(dir, runningJSONRel)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// LoadRunning reads the running record from `.flow/running.json`. Returns
// (nil, nil) when no record exists on disk.
func LoadRunning() (*RunningRecord, error) {
	path, err := RunningJSONPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var rec RunningRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &rec, nil
}

// ClearRunning removes the running record. Idempotent — no error if already
// absent.
func ClearRunning() error {
	path, err := RunningJSONPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
