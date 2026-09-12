package machinecache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type rec struct {
	N    int       `json:"n"`
	When time.Time `json:"when"`
}

// A record written whole survives a read, and a missing, torn or unreadable one
// is a MISS rather than an error: nothing here may ever fail the run that was
// trying to cache something.
func TestLoadStore_RoundTripsAndDegrades(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thing.json")

	if got := Load[rec](path); got != nil {
		t.Errorf("an absent record loaded as %+v, want a miss", got)
	}

	Store(path, rec{N: 7}, Sweep{})
	got := Load[rec](path)
	if got == nil || got.N != 7 {
		t.Fatalf("loaded %+v, want the record just stored", got)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Load[rec](path); got != nil {
		t.Errorf("a torn record loaded as %+v, want a miss — a corrupt cache costs one refresh, not a run", got)
	}

	// A path under a directory that cannot be created is silent. The caller has
	// already done its work; failing here would throw it away.
	blocked := filepath.Join(path, "under-a-file", "x.json")
	Store(blocked, rec{N: 1}, Sweep{})
	if got := Load[rec](blocked); got != nil {
		t.Errorf("a store that could not run produced %+v", got)
	}
}

// A store writes through a temp file whose name no caller's sweep glob matches:
// a temp file belongs to a store in flight, and removing one under the process
// writing it is how a store lands on nothing.
func TestStore_LeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	Store(filepath.Join(dir, "thing.json"), rec{N: 1}, Sweep{})
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "thing.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want just the record", names)
	}
}

// Keying a record by a credential or a repository orphans its file the moment
// that key stops being used, and nothing else ever retires them — the lock
// beside it included, because the TTL breaker only fires when some process
// contends for that exact name.
func TestSweeper_RetiresOrphansAndKeepsTheLiving(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, age time.Duration) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	sweep := Sweep{Globs: []string{"thing-*.json", "thing-*" + LockSuffix}, KeepFor: time.Hour}

	orphan := write("thing-aaaa.json", 2*time.Hour)
	orphanLock := write("thing-aaaa.json"+LockSuffix, 2*time.Hour)
	live := write("thing-bbbb.json", time.Minute)
	liveLock := write("thing-bbbb.json"+LockSuffix, time.Second)
	unrelated := write("something-else.json", 2*time.Hour)

	Sweeper(dir, sweep, time.Now())

	for _, gone := range []string{orphan, orphanLock} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should have been swept; stat err = %v", filepath.Base(gone), err)
		}
	}
	for _, kept := range []string{live, liveLock, unrelated} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s should have been kept: %v", filepath.Base(kept), err)
		}
	}

	// The zero Sweep sweeps nothing, which is what a caller with no retirement
	// policy means.
	stale := write("thing-cccc.json", 2*time.Hour)
	Sweeper(dir, Sweep{}, time.Now())
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("the zero Sweep removed %s: %v", filepath.Base(stale), err)
	}
}

// The slot is exclusive while it is held, broken once it is older than the TTL,
// and never wedges the machine when it cannot be created at all.
func TestAcquireLock(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "a.lock")
	now := time.Now()

	release, held := AcquireLock(lock, time.Minute, now)
	if !held {
		t.Fatal("a free slot was not taken")
	}
	if _, second := AcquireLock(lock, time.Minute, now); second {
		t.Error("the slot was handed out twice")
	}
	release()
	if _, after := AcquireLock(lock, time.Minute, now); !after {
		t.Error("the slot was not free after its release")
	}

	// A process killed while holding one leaves the file behind, and a lock
	// nothing can clear would disable the path it guards permanently.
	if _, broken := AcquireLock(lock, time.Minute, now.Add(2*time.Minute)); !broken {
		t.Error("a lock older than its TTL was not broken")
	}

	// Unwritable is NOT held: exclusion here is an optimisation over a
	// correctness property the caller still has, and one that stopped every
	// process from ever proceeding would be worse than the duplication.
	if _, ok := AcquireLock(filepath.Join(lock, "nested.lock"), time.Minute, now); !ok {
		t.Error("a lock that could not be created for any reason other than already existing was treated as held")
	}
}

// Dir is user-scoped and named after flow, which is the whole point: every
// checkout on the machine shares one record.
func TestDir_IsFlowsUnderTheUserCache(t *testing.T) {
	dir, ok := Dir()
	if !ok {
		t.Skip("this machine reports no user cache directory, which is a machine without a cache")
	}
	if filepath.Base(dir) != "flow" {
		t.Errorf("Dir() = %q, want flow's own directory", dir)
	}
}
