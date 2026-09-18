package hostscope

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promise-language/flow"
)

// useTempDir points the exclusion at a directory of this test's own.
//
// Every test takes it. Taking the machine's real exclusion would block every
// arena on the developer's machine for as long as the test ran, and would
// serialize the test against whatever real gate happened to be measuring.
func useTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := machineDir
	machineDir = func() (string, bool) { return dir, true }
	t.Cleanup(func() { machineDir = prev })
	return dir
}

func requireFlock(t *testing.T) {
	t.Helper()
	switch runtime.GOOS {
	case "darwin", "dragonfly", "freebsd", "illumos", "linux", "netbsd", "openbsd":
	default:
		t.Skip("no advisory file lock is reachable from the standard library here")
	}
}

func testArena() flow.Arena {
	return flow.Arena{Host: "build01", Id: flow.ArenaId("/srv/work/promise")}
}

// An uncontended acquire reports EXACTLY no wait, not a small one.
//
// The figure is filed to the ledger as contention, so "a few microseconds" is
// not a harmless rounding: it would report queueing on a machine where nothing
// queued, and pay an orchestrator write for it on every run. Zero is available
// because the exclusion is asked for without blocking first — a clock around
// the syscall could never produce it.
func TestAcquire_UncontendedReportsExactlyNoWait(t *testing.T) {
	requireFlock(t)
	useTempDir(t)

	release, waited, err := Acquire(context.Background(), testArena())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	if waited != 0 {
		t.Errorf("waited = %s on a free exclusion, want exactly 0", waited)
	}
}

// The property the whole mechanism rests on: while one party holds it, no other
// party may. A second acquire returns only after the first releases, and says
// how long it was held up.
func TestAcquire_SerializesTwoProcesses(t *testing.T) {
	requireFlock(t)
	dir := useTempDir(t)

	first, _, err := Acquire(context.Background(), testArena())
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	// A second PROCESS, not a second goroutine: flock is held per open file
	// description, so two descriptors in one process would be the only case
	// this mechanism does not have to survive and the one a same-process test
	// would accidentally measure.
	held := 300 * time.Millisecond
	second := waiterProcess(t, dir, held+2*time.Second)
	waitUntilWaiting(t, second)

	time.Sleep(held)
	start := time.Now()
	first()

	out, err := second.wait()
	if err != nil {
		t.Fatalf("the waiting process failed: %v\n%s", err, out)
	}
	if grantedAfter := time.Since(start); grantedAfter > 2*time.Second {
		t.Errorf("the second party was granted %s after the release, want promptly", grantedAfter)
	}
	if !strings.Contains(out, "waited=") {
		t.Fatalf("the waiting process did not report a wait:\n%s", out)
	}
	if strings.Contains(out, "waited=0s") {
		t.Errorf("the second party reported no wait, but it was held up:\n%s", out)
	}
}

// THE INVARIANT WITH NOTHING ELSE BEHIND IT. docs/gates-and-commands.md § Two
// scopes requires that a process which dies releases the exclusion, and the
// holder is exactly the party that cannot keep that promise. Nothing in this
// package unlocks here: the kernel does, when it reaps the process.
//
// A lockfile mechanism fails this test, which is why this is not one.
func TestAcquire_AProcessThatDiesReleasesIt(t *testing.T) {
	requireFlock(t)
	dir := useTempDir(t)

	holder := holderProcess(t, dir, time.Minute)
	waitUntilHeld(t, holder)

	// Establish that the child really has it, or the acquire below proves
	// nothing: a child that silently failed to take the exclusion would make
	// this case pass while testing the opposite of what it names.
	busy, cancelBusy := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelBusy()
	if stolen, _, err := Acquire(busy, testArena()); err == nil {
		stolen()
		t.Fatal("the exclusion was free while the child claimed to hold it")
	}

	if err := holder.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the holder: %v", err)
	}
	_, _ = holder.cmd.Process.Wait()

	// Bounded so a failure is a failure rather than a hung suite.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	release, _, err := Acquire(ctx, testArena())
	if err != nil {
		t.Fatalf("Acquire after the holder was killed: %v — a dead process is still holding it", err)
	}
	release()
}

// A caller that gives up gets its context's error, holds nothing, and leaves
// nothing held. The last clause is the one worth testing: the flock syscall has
// no deadline, so the request the caller abandoned is still outstanding and
// will be granted later — a grant nobody unlocks would wedge the machine.
func TestAcquire_ContextCancelledWhileQueued(t *testing.T) {
	requireFlock(t)
	useTempDir(t)

	first, _, err := Acquire(context.Background(), testArena())
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	release, waited, err := Acquire(ctx, testArena())
	if err == nil {
		release()
		t.Fatal("Acquire returned a held exclusion for a caller that had given up")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to carry the context's own error", err)
	}
	if release != nil {
		t.Error("a failed Acquire returned a release — a caller that defers it would unlock something it never held")
	}
	if waited < 100*time.Millisecond {
		t.Errorf("waited = %s, want the time actually spent queued", waited)
	}

	first()

	// The abandoned request is granted and cleaned up by the goroutine that
	// owns it. Nothing else may be holding the exclusion afterwards.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	release2, _, err := Acquire(ctx2, testArena())
	if err != nil {
		t.Fatalf("Acquire after an abandoned wait: %v — the abandoned request kept the exclusion", err)
	}
	release2()
}

// THE REGRESSION THIS EXISTS FOR. A machine with nowhere to put the lock must
// refuse, not hand back a free pass: the private lock this replaced returned
// "no lock, silently" when os.UserHomeDir failed, and a machine in that state
// ran every gate at once and said nothing about it.
func TestAcquire_NoMachineDirectoryRefuses(t *testing.T) {
	prev := machineDir
	machineDir = func() (string, bool) { return "", false }
	t.Cleanup(func() { machineDir = prev })

	release, _, err := Acquire(context.Background(), testArena())
	if err == nil {
		if release != nil {
			release()
		}
		t.Fatal("Acquire succeeded with nowhere to put the lock — that is the silent free pass this replaces")
	}
	if release != nil {
		t.Error("a refused Acquire returned a release")
	}
	if _, ok := Path(); ok {
		t.Error("Path reported a location on a machine that has none")
	}
}

// A directory that cannot be created is the same case as no directory at all:
// an error, never an exclusion nobody holds.
func TestAcquire_UnusableDirectoryRefuses(t *testing.T) {
	// A regular file where the directory should be: MkdirAll fails on every
	// platform, without depending on permissions a root-run suite would ignore.
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := machineDir
	machineDir = func() (string, bool) { return blocked, true }
	t.Cleanup(func() { machineDir = prev })

	release, _, err := Acquire(context.Background(), testArena())
	if err == nil {
		if release != nil {
			release()
		}
		t.Fatal("Acquire succeeded against an unusable directory")
	}
}

// Release is deferred, and a deferred call that ran twice must not unlock an
// exclusion a later party has since been granted.
func TestRelease_IsIdempotent(t *testing.T) {
	requireFlock(t)
	dir := useTempDir(t)

	release, _, err := Acquire(context.Background(), testArena())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()
	release()

	// A second party takes it, and the stale release must not reach it.
	holder := holderProcess(t, dir, 3*time.Second)
	waitUntilHeld(t, holder)
	release()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	stolen, _, err := Acquire(ctx, testArena())
	if err == nil {
		stolen()
		t.Fatal("a repeated release unlocked an exclusion another process held")
	}
	_ = holder.cmd.Process.Kill()
	_, _ = holder.cmd.Process.Wait()
}

// The holder is an arena, and an operator can read which one. A checkout path
// alone cannot say which of a host's arenas has the machine, which is the whole
// reason the holder is the pair.
func TestHolder_NamesTheArenaAndIsClearedOnRelease(t *testing.T) {
	requireFlock(t)
	useTempDir(t)

	if _, ok := Holder(); ok {
		t.Error("Holder named an arena before anything took the exclusion")
	}

	release, _, err := Acquire(context.Background(), testArena())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	got, ok := Holder()
	if !ok {
		t.Fatal("Holder named nobody while the exclusion was held")
	}
	if got != testArena() {
		t.Errorf("Holder = %+v, want %+v", got, testArena())
	}

	release()
	if _, ok := Holder(); ok {
		t.Error("Holder still names an arena after the release — a later reader would blame a run that has finished")
	}
}

// A file holding something that is not a holder record reads as nobody, rather
// than as a panic or a half-parsed arena. Whatever wrote it, no caller decides
// on this value.
func TestHolder_UnreadableRecordNamesNobody(t *testing.T) {
	dir := useTempDir(t)
	path := filepath.Join(dir, lockName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := Holder(); ok {
		t.Error("Holder named an arena from an unparseable record")
	}

	if err := os.WriteFile(path, []byte(`{"arena":{"host":"","id":""}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := Holder(); ok {
		t.Error("Holder named an arena from a record naming neither half of the pair — an ArenaId alone is not an identity")
	}
}

// The exclusion must not sit inside anything a gate touches, and must not be
// named where a cache sweep would reach it: machinecache.Sweeper removes a name
// it matches, and removing this file under a live holder leaves the next
// acquirer locking a fresh inode while the first still runs.
func TestPath_IsNotSweptWithTheCacheRecords(t *testing.T) {
	dir := useTempDir(t)
	path, ok := Path()
	if !ok {
		t.Fatal("Path reported no location")
	}
	if filepath.Dir(path) != dir {
		t.Errorf("Path = %s, want it directly under the machine directory %s", path, dir)
	}
	for _, glob := range []string{"quota*.json", "quota*.json.lock", "github-*.json", "github-*.lock"} {
		matched, err := filepath.Match(glob, filepath.Base(path))
		if err != nil {
			t.Fatal(err)
		}
		if matched {
			t.Errorf("%s matches the cache sweep glob %q, so a sweep could remove it under a live holder", filepath.Base(path), glob)
		}
	}
}

// --- child processes -------------------------------------------------------
//
// Two processes are required rather than convenient: flock is held per open
// file description, so a same-process test would measure a case this mechanism
// never has to survive, and could not test the one that matters — a holder the
// kernel reaps.

type child struct {
	t   *testing.T
	cmd *exec.Cmd
	out *syncBuffer
}

// syncBuffer collects a child's output. Synchronised because exec's copier
// writes it from a goroutine of its own while the parent polls it for a
// progress line, which is a genuine race and not a formality.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// holderProcess starts a child that takes the exclusion, prints "held", and
// then sleeps until killed or until hold elapses.
func holderProcess(t *testing.T, dir string, hold time.Duration) *child {
	return spawn(t, dir, "hold", hold)
}

// waiterProcess starts a child that queues for the exclusion and prints how
// long it waited once granted.
func waiterProcess(t *testing.T, dir string, patience time.Duration) *child {
	return spawn(t, dir, "wait", patience)
}

func spawn(t *testing.T, dir, mode string, d time.Duration) *child {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("cannot find the test binary to re-exec: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestHostScopeChild")
	cmd.Env = append(os.Environ(),
		"FLOW_HOSTSCOPE_CHILD="+mode,
		"FLOW_HOSTSCOPE_DIR="+dir,
		"FLOW_HOSTSCOPE_FOR="+d.String(),
	)
	out := &syncBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	c := &child{t: t, cmd: cmd, out: out}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	return c
}

func (c *child) wait() (string, error) {
	err := c.cmd.Wait()
	return c.out.String(), err
}

func waitUntilHeld(t *testing.T, c *child)    { t.Helper(); waitForOutput(t, c, "held") }
func waitUntilWaiting(t *testing.T, c *child) { t.Helper(); waitForOutput(t, c, "waiting") }

func waitForOutput(t *testing.T, c *child, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(c.out.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("child never printed %q:\n%s", want, c.out.String())
}

// TestHostScopeChild is the child process's entry point, not a test. It runs
// only when spawn re-execs this binary with FLOW_HOSTSCOPE_CHILD set, and is an
// ordinary no-op otherwise.
func TestHostScopeChild(t *testing.T) {
	mode := os.Getenv("FLOW_HOSTSCOPE_CHILD")
	if mode == "" {
		t.Skip("not the child process")
	}
	machineDir = func() (string, bool) { return os.Getenv("FLOW_HOSTSCOPE_DIR"), true }
	d, err := time.ParseDuration(os.Getenv("FLOW_HOSTSCOPE_FOR"))
	if err != nil {
		t.Fatalf("child: %v", err)
	}

	switch mode {
	case "hold":
		release, _, err := Acquire(context.Background(), testArena())
		if err != nil {
			t.Fatalf("child: Acquire: %v", err)
		}
		// Printed AFTER the exclusion is held, so a parent that saw it knows
		// the state it is about to act on. Flushed by the unbuffered pipe.
		os.Stdout.WriteString("held\n")
		os.Stdout.Sync()
		time.Sleep(d)
		release()
	case "wait":
		os.Stdout.WriteString("waiting\n")
		os.Stdout.Sync()
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		release, waited, err := Acquire(ctx, testArena())
		if err != nil {
			t.Fatalf("child: Acquire: %v", err)
		}
		os.Stdout.WriteString("waited=" + waited.Round(time.Millisecond).String() + "\n")
		release()
	default:
		t.Fatalf("child: unknown mode %q", mode)
	}
}
