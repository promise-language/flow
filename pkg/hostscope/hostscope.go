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

// maxHolderRecord bounds a read of the holder record. The record is one arena,
// a pid and an instant, and the arena's larger half is a filesystem path — so
// this is generous by two orders of magnitude and still refuses to size a
// buffer from bytes on disk.
const maxHolderRecord = 64 << 10

// holderRecord is written into the locked file while the exclusion is held: it
// is how an operator looking at a stalled machine sees which arena has it, and
// it is what a nested party checks to find that the exclusion is its own
// arena's already.
//
// THE SECOND USE IS WHY THE WRITE IS PART OF THE ACQUIRE. It is read only by a
// party the kernel has just refused, so whoever it names is whoever holds the
// lock — and a holder that could not write its name would be one its own tools
// could not recognise, which is a deadlock rather than a missing diagnostic.
// The PID and the instant are diagnostic; the arena is not.
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
// queue is asking. It is also what the exclusion is re-entered on, below, so it
// is load-bearing rather than only diagnostic.
//
// IT IS RE-ENTRANT TO THE ARENA THAT HOLDS IT. A caller whose own arena is
// already inside the exclusion is granted it at once, with no wait, and gets a
// release that does nothing — the acquisition it is nested inside is what owns
// the machine. Every party in one arena takes it, and the nested ones do not
// queue behind each other; see the block at the contended branch for why the
// alternative is a deadlock rather than a slow run.
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

	// ZERO MEANS UNCONTENDED, NOT "TOO FAST TO SEE". The lock is asked for
	// without blocking first, so a run that queued for nothing reports nothing —
	// rather than the microseconds a clock around the syscall would report,
	// which the ledger would then carry as contention and pay an orchestrator
	// write for on every single run.
	granted, err := tryLockExclusive(f)
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	if granted {
		release, err := held(f, holder)
		return release, 0, err
	}

	// CONTENDED — BUT BY WHOM? An exclusion this arena already holds is one
	// this caller is already inside, and a lock is re-entrant to the party that
	// holds it. Without this the participant set would be a deadlock rather
	// than a queue: the runner takes the exclusion and then spawns the project's
	// gate entry point, which is bound by the same rule and would queue behind
	// its own parent until the gate's timeout killed it — so every gate a
	// project declared host-scoped would report a timeout, and only those.
	//
	// THE PARTY IS THE ARENA, which is the granularity the holder is recorded
	// at, and it is the one the norm can state: the parties inside one arena
	// are one measurement by construction, because a claim is item ↔ arena.
	// What it gives up is a person running a gate by hand inside a checkout a
	// flow is currently measuring in — which is an operator reaching into a live
	// arena, a thing no exclusion was going to make safe. An operator with their
	// own checkout is their own arena and queues like anybody else.
	//
	// An empty holder can never match: Arena.Empty() is not an identity, and
	// comparing one would let a caller that could not name itself re-enter
	// anything that also could not.
	//
	// THE ONE READING THIS CANNOT RULE OUT is a name left behind by a holder the
	// kernel reaped: the lock is free at that instant, so the next acquirer is
	// granted it and blanks the record — but between its grant and that syscall
	// the file still names the arena that died. A third party from THAT arena,
	// asking inside that window, would read its own name and walk in. It is a
	// crash, an immediate peer acquire and a same-arena acquire aligning inside
	// one ftruncate, and closing it would take a lock-and-truncate the kernel
	// does not offer. Writing the name is therefore the first thing a holder
	// does, so the window is as narrow as the syscall.
	if ours, ok := readHolder(f); ok && !holder.Empty() && ours == holder {
		f.Close()
		// A no-op release. The acquisition this call is nested inside owns the
		// exclusion, and releasing it here would hand the machine away while
		// the outer measurement is still running.
		return func() {}, 0, nil
	}

	started := time.Now()
	if err := waitLockExclusive(ctx, f); err != nil {
		// waitLockExclusive owns f from here: the blocking lock may still be
		// outstanding and only it can know when to close.
		return nil, time.Since(started), err
	}
	release, err = held(f, holder)
	if err != nil {
		return nil, time.Since(started), err
	}
	return release, time.Since(started), nil
}

// held names the holder in the now-locked f and returns the release.
//
// THE NAME IS PART OF THE ACQUIRE, NOT A DIAGNOSTIC BESIDE IT, which is why a
// failure here gives the exclusion back rather than proceeding without it. The
// re-entrancy check above decides on this record: a holder that took the lock
// and could not say whose it is leaves its own tools unable to recognise it,
// and they would queue behind their own arena until their timeout killed them.
// Refusing costs one run and says why; writing nothing costs every nested run
// on the machine and says nothing.
func held(f *os.File, holder flow.Arena) (func(), error) {
	if err := writeHolder(f, holder); err != nil {
		_ = unlock(f)
		_ = f.Close()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// Clear the holder before unlocking, so the next acquirer never
			// reads a name that has moved on — which at this point is not
			// cosmetic: a stale name is a name another party could mistake for
			// its own and re-enter on.
			_ = f.Truncate(0)
			_ = unlock(f)
			_ = f.Close()
		})
	}, nil
}

// writeHolder records who holds it.
func writeHolder(f *os.File, holder flow.Arena) error {
	b, err := json.Marshal(holderRecord{Arena: holder, PID: os.Getpid(), Since: time.Now()})
	if err != nil {
		return fmt.Errorf("hostscope: cannot render the holder of %s: %w", f.Name(), err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("hostscope: cannot clear the previous holder of %s: %w", f.Name(), err)
	}
	if _, err := f.WriteAt(b, 0); err != nil {
		return fmt.Errorf("hostscope: cannot record the holder of %s: %w", f.Name(), err)
	}
	return nil
}

// readHolder reads the record out of an already-open exclusion file.
//
// A record that is absent, torn or unparseable reads as NOBODY, and nobody is
// never equal to a caller's own arena — so every way of failing to read this
// lands on "queue", which is the answer that is wrong at worst by a wait.
func readHolder(f *os.File) (flow.Arena, bool) {
	b := make([]byte, maxHolderRecord)
	n, err := f.ReadAt(b, 0)
	if n == 0 && err != nil {
		return flow.Arena{}, false
	}
	return parseHolder(b[:n])
}

func parseHolder(b []byte) (flow.Arena, bool) {
	var rec holderRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return flow.Arena{}, false
	}
	if rec.Arena.Empty() {
		return flow.Arena{}, false
	}
	return rec.Arena, true
}

// Holder reports the arena currently named in the exclusion's file, and false
// when nothing is named there.
//
// A READING, NOT A CHECK. It is what an operator asks to find out who has the
// machine; it establishes nothing about whether the exclusion is held now, and
// no caller may act on it — only taking the lock establishes that. That is what
// separates it from the read inside Acquire, which is made by a caller the
// kernel has just refused and so is already standing on the fact this one
// cannot supply. The file is
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
	return parseHolder(b)
}
