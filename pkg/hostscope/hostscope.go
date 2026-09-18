// Package hostscope is the host-scope exclusion: the one slot on a machine that
// a measurement too heavy to run beside another holds while it runs
// (docs/gates-and-commands.md § Two scopes).
//
// It exists as a package, rather than inside the runner that first needed it,
// because the participant set is closed and larger than the runner. The norm
// binds the flow's own gate runner, the project's gate entry point, each of the
// three CommandNames, and a person running the same tools by hand — and parties
// that each derived their own path and their own file format would be several
// exclusions that never meet, which is the failure this replaces. One
// definition, imported by everyone who has to join it.
//
// WHY NOT pkg/machinecache. That package has the file mechanics and none of the
// semantics: its lock is a single-flight optimisation whose contract is that
// nothing in it may ever fail a run, so it fails OPEN, it does not wait, and it
// breaks a lock older than a TTL. Every one of those is wrong here. Failing
// open is the promise defect verbatim — a machine that runs everything at once
// and says nothing. Not waiting makes a queue into a refusal. And a TTL breaker
// would break a legitimate forty-minute suite, handing the machine to a second
// one while the first still holds it. What IS reused is machinecache.Dir(): the
// location of flow's own per-machine directory has one answer, and this is not
// the place to invent a second.
package hostscope

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/machinecache"
)

// lockName is the exclusion's file inside flow's machine directory.
//
// It is deliberately not machinecache.LockSuffix and matches no sweep glob any
// caller of that package registers. A cache lock is swept because keying
// orphans it and nothing else would ever clear the name; this one is a LIVE
// exclusion, and a sweep that removed it while a suite held it would not clear
// anything — the holder keeps the open file, the next acquirer creates a new
// inode, and both run at once believing they are alone.
const lockName = "host-scope.lock"

// holderRecord is written into the locked file while the exclusion is held, so
// that an operator looking at a stalled machine can see which arena has it.
//
// It is diagnostic and nothing reads it to decide. The exclusion is the lock;
// this is the name beside it.
type holderRecord struct {
	Arena flow.Arena `json:"arena"`
	PID   int        `json:"pid"`
	Since time.Time  `json:"since"`
}

// machineDir is the seam this package is tested through: a package var, the
// pattern pkg/orchestrator/github/cache.go:107 already uses, and deliberately
// NOT an environment variable — an environment variable is never an input
// (docs/org/cli-guide.md § 2).
//
// A test that took the real exclusion would not merely mislead itself: it would
// block every other arena on the developer's machine for as long as it ran, and
// serialize against whatever real gate happened to be measuring.
var machineDir = machinecache.Dir

// Path reports where the exclusion lives, and false when this machine has no
// directory for it.
//
// Exported because a party that must join the exclusion may want to name it in
// a diagnostic, and because `doctor`-shaped reporting should not re-derive it.
func Path() (string, bool) {
	dir, ok := machineDir()
	if !ok {
		return "", false
	}
	return filepath.Join(dir, lockName), true
}

// Acquire blocks until this process holds the host's exclusion, and reports how
// long that took. A zero wait is an exclusion that was free.
//
// holder is the arena taking it — (HostId, ArenaId), never a checkout path. A
// path can say which directory is busy and cannot say which of a host's arenas
// holds the machine, which is the question an operator looking at a stalled
// queue is asking.
//
// IT REFUSES RATHER THAN DEGRADING. A machine with nowhere to put the lock, or
// a platform with no way to hold one, gets an error and no exclusion — never a
// silent free pass. That distinction is the whole point: a caller that proceeds
// unserialized produces measurements that are wrong in a way reproducing
// nowhere, and no report mentions it. Its caller's obligation is the other half
// — a party that cannot take the exclusion does not run the measurement.
//
// THE WAIT IS BOUNDED BY ctx AND BY NOTHING ELSE. No TTL, no cap of its own:
// every holder's run is bounded by its own declared timeout and released by its
// own death, so the queue is finite by construction. A cap set below what a
// real run costs would turn every busy period into false failures, which is the
// failure gates-and-commands.md § Gates may have to wait names.
//
// The returned release is idempotent and safe to defer.
func Acquire(ctx context.Context, holder flow.Arena) (release func(), waited time.Duration, err error) {
	path, ok := Path()
	if !ok {
		return nil, 0, fmt.Errorf("hostscope: this machine has no directory for the host-scope exclusion, so it cannot be taken")
	}
	// 0o700 for the directory and 0o600 for the file: what an arena is named
	// here is a machine name and an absolute worktree path, which nothing else
	// on the machine has a reason to read. It never leaves the machine —
	// docs/disclosure.md guards outward bytes, and this is why the claim's
	// published half is a fingerprint while this one is not.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, 0, fmt.Errorf("hostscope: cannot create the directory for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, 0, fmt.Errorf("hostscope: cannot open %s: %w", path, err)
	}

	started := time.Now()
	contended, err := lockExclusive(ctx, f)
	if err != nil {
		// lockExclusive owns f from here: on the ctx path the blocking lock is
		// still outstanding and only it can know when to close.
		if !contended {
			return nil, 0, err
		}
		return nil, time.Since(started), err
	}
	// ZERO MEANS UNCONTENDED, NOT "TOO FAST TO SEE". lockExclusive asks without
	// blocking first and says whether anything was in the way, so a run that
	// queued for nothing reports nothing — rather than the microseconds a clock
	// around the syscall would report, which the ledger would then carry as
	// contention and pay an orchestrator write for on every single run.
	if contended {
		waited = time.Since(started)
	}

	writeHolder(f, holder)

	var once sync.Once
	return func() {
		once.Do(func() {
			// Clear the holder before unlocking, so the next acquirer never
			// reads a name that has moved on. Best-effort: the exclusion is the
			// lock, and failing to blank a diagnostic must not leave it held.
			_ = f.Truncate(0)
			_ = unlock(f)
			_ = f.Close()
		})
	}, waited, nil
}

// writeHolder records who holds it. Best-effort by design: the exclusion has
// already been taken by the time this runs, and refusing to proceed because a
// diagnostic could not be written would trade a working lock for a failed run.
func writeHolder(f *os.File, holder flow.Arena) {
	b, err := json.Marshal(holderRecord{Arena: holder, PID: os.Getpid(), Since: time.Now()})
	if err != nil {
		return
	}
	if err := f.Truncate(0); err != nil {
		return
	}
	_, _ = f.WriteAt(b, 0)
}

// Holder reports the arena currently named in the exclusion's file, and false
// when nothing is named there.
//
// A READING, NOT A CHECK. It is what an operator asks to find out who has the
// machine; it establishes nothing about whether the exclusion is held now, and
// no caller may act on it — only taking the lock establishes that. The file is
// blanked on release, so an empty read is an exclusion nobody holds and a
// non-empty one may be either a live holder or a name the kernel has already
// let go of.
func Holder() (flow.Arena, bool) {
	path, ok := Path()
	if !ok {
		return flow.Arena{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return flow.Arena{}, false
	}
	var rec holderRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return flow.Arena{}, false
	}
	if rec.Arena.Empty() {
		return flow.Arena{}, false
	}
	return rec.Arena, true
}
