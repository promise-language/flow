// Package machinecache is the file mechanics every machine-wide cache in flow
// shares: one directory under the OS user cache, records written whole or not
// at all, a single-flight lock beside each record, and a sweep that retires the
// names nothing reads any more.
//
// It exists because the shape was needed twice. cli/quota_cache.go built it
// first, for the agent subscription endpoint: "25 checkouts of one account share
// one reading" rather than each earning its own 429. The GitHub seam
// (pkg/orchestrator/github/cache.go) needs every one of those properties and
// none of that file's policy — different key, different freshness per method, a
// different endpoint. Copying the mechanics would leave two implementations of
// atomic-store-plus-single-flight that must be kept in sync, and the one that
// drifts is the one nobody is reading when it does.
//
// So the POLICY stays with each caller and only the mechanics live here. This
// package knows nothing about quotas, repositories, accounts or freshness: it
// takes a path, and what goes in the file is the caller's.
//
// NOTHING HERE MAY EVER FAIL A RUN. Every filesystem error degrades to the
// behaviour the caller has without a cache: a load that cannot read reports a
// miss, a store that cannot write is silent, and a lock that cannot be created
// for any reason other than already existing is not treated as held.
package machinecache

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Dir is where the shared records live: flow's directory under the OS user
// cache directory ($XDG_CACHE_HOME or ~/.cache on Linux, ~/Library/Caches on
// macOS). User-scoped rather than per-clone on purpose — the whole point is
// that every checkout on the machine shares one record.
//
// Reports false when the machine has no such directory, which is a machine
// without a cache, not an error.
func Dir() (string, bool) {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return "", false
	}
	return filepath.Join(dir, "flow"), true
}

// Load reads a JSON record. Absent, torn, or unparseable all read as a miss —
// nil, no error — so a corrupt cache costs one refresh rather than a run.
func Load[T any](path string) *T {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rec T
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil
	}
	return &rec
}

// Store writes the record via temp file + rename, so a concurrent reader sees
// either the whole previous record or the whole new one and never half of
// either. Every failure is silent: not being able to cache something is not a
// reason to fail the run that produced it.
//
// The sweep runs after the write rather than on a timer of its own, because a
// store is exactly when a new name can have appeared. A zero Sweep skips it.
func Store[T any](path string, rec T, sweep Sweep) {
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	// 0o700: what these records hold is the caller's business and nothing
	// else on the machine has a reason to read it.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	// CreateTemp makes the file 0o600, which the rename carries over. The
	// prefix is deliberately one no caller's sweep glob matches: a temp file
	// belongs to a store in flight, and removing one under the process writing
	// it is how a store lands on nothing.
	tmp, err := os.CreateTemp(dir, tempPrefix)
	if err != nil {
		return
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return
	}
	Sweeper(dir, sweep, time.Now())
}

// tempPrefix names a store in flight. Callers choose sweep globs that do not
// match it — see Store.
const tempPrefix = ".machinecache-"

// LockSuffix is the name a record's single-flight lock takes: the record's own
// path plus this. Exported because a caller's sweep glob has to match it.
const LockSuffix = ".lock"

// Sweep says which names in a cache directory are retired, and when. A record
// keyed by a credential or a repository orphans its file the moment that key
// stops being used, so something has to retire them; nothing else ever will.
//
// The zero value sweeps nothing.
type Sweep struct {
	Globs   []string
	KeepFor time.Duration
}

// Sweeper removes the records — and the locks beside them — that nothing is
// reading any more.
//
// Silent, like everything else here: a sweep that cannot run is not a reason to
// fail the run whose record was just stored.
//
// The LOCK is swept with its record, because keying orphans it the same way and
// nothing else ever will: the TTL breaker in AcquireLock clears a stale lock
// only when some process contends for that exact name, and a retired key's name
// is one no process asks for again. A sweep cannot reach a LIVE lock as long as
// KeepFor is well above the lock TTL — anything that old is a leak by
// definition.
func Sweeper(dir string, sweep Sweep, now time.Time) {
	if sweep.KeepFor <= 0 {
		return
	}
	for _, glob := range sweep.Globs {
		matches, err := filepath.Glob(filepath.Join(dir, glob))
		if err != nil {
			continue
		}
		for _, path := range matches {
			st, err := os.Stat(path)
			if err != nil || st.IsDir() {
				continue
			}
			if now.Sub(st.ModTime()) > sweep.KeepFor {
				os.Remove(path)
			}
		}
	}
}

// AcquireLock takes the machine-wide slot named by lock, so that of several
// processes wanting it at the same moment one proceeds and the rest do
// something else.
//
// Returns (release, true) when the caller holds it, and (nil, false) when
// somebody else does. A lock that cannot be created for any reason OTHER than
// already existing — an unwritable cache directory — is not treated as held:
// exclusion here is an optimisation over a correctness property the caller
// still has, and one that could stop every process on the machine from ever
// proceeding would be worse than the duplication it saves.
//
// A lock older than ttl is broken. A process killed while holding one leaves
// the file behind, and a lock nothing can clear would disable the path it
// guards on this machine permanently.
func AcquireLock(lock string, ttl time.Duration, now time.Time) (func(), bool) {
	take := func() (func(), bool) {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(lock) }, true
		}
		if !errors.Is(err, fs.ErrExist) {
			return func() {}, true
		}
		return nil, false
	}
	if release, ok := take(); ok {
		return release, true
	}
	st, err := os.Stat(lock)
	if err != nil || now.Sub(st.ModTime()) <= ttl {
		return nil, false
	}
	os.Remove(lock)
	// Re-take rather than assume: if another process broke the same stale lock
	// first, it holds the slot and this one does not.
	return take()
}
