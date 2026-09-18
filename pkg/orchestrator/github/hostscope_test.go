package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promise-language/flow"
)

// fakeHostScope stands in for the real exclusion so these cases assert what the
// RUNNER does with it — that it takes one when the project declared one, holds
// it across the whole measurement, releases it on every exit, and refuses when
// it cannot be taken. pkg/hostscope's own tests assert that the exclusion
// excludes; repeating that here would test the kernel twice and the runner
// once.
type fakeHostScope struct {
	mu sync.Mutex

	// slot IS the exclusion: it excludes, because a stand-in that only counted
	// would let two measurements through and then report that two got through,
	// which measures the stand-in rather than the runner.
	slot chan struct{}

	held     int
	takes    int
	arenas   []flow.Arena
	waitFor  time.Duration
	failWith error
}

func (f *fakeHostScope) acquire(ctx context.Context, holder flow.Arena) (func(), time.Duration, error) {
	f.mu.Lock()
	f.takes++
	f.arenas = append(f.arenas, holder)
	fail, wait := f.failWith, f.waitFor
	f.mu.Unlock()

	if fail != nil {
		return nil, 0, fail
	}

	// Uncontended reports EXACTLY zero, as the real one does: it asks without
	// blocking first, so a run that queued for nothing has nothing to report.
	started := time.Now()
	contended := false
	select {
	case f.slot <- struct{}{}:
	default:
		contended = true
		select {
		case f.slot <- struct{}{}:
		case <-ctx.Done():
			return nil, time.Since(started), ctx.Err()
		}
	}

	if wait > 0 {
		contended = true
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			<-f.slot
			return nil, time.Since(started), ctx.Err()
		}
	}

	f.mu.Lock()
	f.held++
	f.mu.Unlock()

	waited := time.Duration(0)
	if contended {
		waited = time.Since(started)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.held--
			f.mu.Unlock()
			<-f.slot
		})
	}, waited, nil
}

func (f *fakeHostScope) snapshot() (takes, stillHeld int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.takes, f.held
}

// useFakeHostScope installs the stand-in for one test. Every case here takes
// it: reaching the real exclusion would block every other arena on the
// developer's machine for as long as the test ran.
func useFakeHostScope(t *testing.T) *fakeHostScope {
	t.Helper()
	f := &fakeHostScope{slot: make(chan struct{}, 1)}
	prev := acquireHostScope
	acquireHostScope = f.acquire
	t.Cleanup(func() { acquireHostScope = prev })
	return f
}

// hostScopeWorktree builds a worktree whose entry point answers the listing
// with the given gates and runs body for a measurement.
//
// It goes through Orchestrator.Worktree rather than &worktree{}, because that
// is where the declarations are read and a worktree built any other way carries
// none.
func hostScopeWorktree(t *testing.T, listing, body string) (*worktree, string) {
	t.Helper()
	requireRealProcesses(t)

	dir := filepath.Join(t.TempDir(), "work tree")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGateEntryPoint(t, dir, `case "$*" in
"--list --json") printf '`+listing+`\n' ;;
*) `+body+` ;;
esac`)

	gitInit := func(args ...string) {
		t.Helper()
		if out, err := runGit(t, dir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitInit("init")
	gitInit("config", "user.email", "t@example.com")
	gitInit("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "tracked"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInit("add", ".")
	gitInit("commit", "-m", "init")

	b := &Orchestrator{cfg: Config{WorktreeDir: dir, GateTimeout: 30 * time.Second, Repo: "o/r"}}
	b.git = newGitOps(dir)
	wt, err := b.Worktree(context.Background(), testItemRef(b))
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	return wt.(*worktree), dir
}

// testItemRef is any ref this orchestrator can resolve an issue number from.
// Which item it is does not matter here — no gate requires a claim, and the
// exclusion is the machine's rather than the item's.
func testItemRef(b *Orchestrator) flow.ItemRef {
	return flow.ItemRef{OrchestratorName: b.Name(), Display: "o/r#1", Ref: json.RawMessage(`{"issue":1}`)}
}

func runGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	g := newGitOps(dir)
	out, errOut, err := g.runner(context.Background(), dir, "git", args...)
	return string(out) + string(errOut), err
}

// A gate the project declared host-scoped holds the exclusion while it runs,
// and the arena it holds it as is the (HostId, ArenaId) pair — not a checkout
// path, which could not say which of a host's arenas has the machine.
func TestRunGate_DeclaredGateTakesTheExclusion(t *testing.T) {
	f := useFakeHostScope(t)
	w, dir := hostScopeWorktree(t,
		`{"gates":[{"name":"tested","host_scope":true}]}`,
		`echo '{"gate":"tested"}'`)

	run, err := w.RunGate(context.Background(), "tested")
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if run.Outcome != flow.OutcomeMeasured {
		t.Fatalf("outcome = %q, want measured (detail: %s)", run.Outcome, run.Detail)
	}

	takes, stillHeld := f.snapshot()
	if takes != 1 {
		t.Errorf("the exclusion was taken %d times, want once", takes)
	}
	if stillHeld != 0 {
		t.Error("the exclusion is still held after RunGate returned — it is released around the measurement, not kept")
	}
	if len(f.arenas) != 1 || f.arenas[0] != flow.ArenaAt(dir) {
		t.Errorf("holder = %+v, want the arena %+v", f.arenas, flow.ArenaAt(dir))
	}
}

// A gate that declared nothing never queues. This is the property a
// machine-wide mutex would destroy: a formatting check has nothing in common
// with a test suite in what it costs, and serializing it would pay the heaviest
// lock every time.
func TestRunGate_UndeclaredGateTakesNothing(t *testing.T) {
	f := useFakeHostScope(t)
	w, _ := hostScopeWorktree(t,
		`{"gates":[{"name":"tested","host_scope":true},{"name":"formatted"}]}`,
		`echo '{"gate":"formatted"}'`)

	run, err := w.RunGate(context.Background(), "formatted")
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if run.Outcome != flow.OutcomeMeasured {
		t.Fatalf("outcome = %q, want measured (detail: %s)", run.Outcome, run.Detail)
	}
	if takes, _ := f.snapshot(); takes != 0 {
		t.Errorf("the exclusion was taken %d times for a gate that declared none", takes)
	}
	if run.Waited != 0 {
		t.Errorf("Waited = %s for a gate that never queued", run.Waited)
	}
}

// A gate this machine does not declare at all takes nothing, and that is not a
// hole: a name absent from the listing is a name the entry point could not have
// been asked about, and refusing here would report "the tools are not built" as
// "the machine is wedged".
func TestRunGate_AGateOutsideTheListingTakesNothing(t *testing.T) {
	f := useFakeHostScope(t)
	w, _ := hostScopeWorktree(t,
		`{"gates":[{"name":"tested","host_scope":true}]}`,
		`echo '{"gate":"builds"}'`)

	if _, err := w.RunGate(context.Background(), "builds"); err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if takes, _ := f.snapshot(); takes != 0 {
		t.Errorf("the exclusion was taken %d times for a gate this machine never declared", takes)
	}
}

// THE HOLD SPANS THE SPAWN, which is the runner's half of the property and the
// one a stand-in cannot fake. A runner that took the exclusion and released it
// before exec'ing the gate would satisfy every counter and still let four
// suites saturate the machine together.
//
// So the overlap is detected where it would actually hurt: the GATE PROCESS
// appends "in" on entry and "out" on exit, and a log in which an "in" follows
// an "in" is two measurements running at once.
func TestRunGate_TheExclusionIsHeldAcrossTheGateProcess(t *testing.T) {
	useFakeHostScope(t)

	log := filepath.Join(t.TempDir(), "overlap.log")
	w, _ := hostScopeWorktree(t,
		`{"gates":[{"name":"tested","host_scope":true}]}`,
		`printf 'in\n' >> `+log+`; sleep 0.1; printf 'out\n' >> `+log+`; echo '{"gate":"tested"}'`)

	const runs = 4
	var wg sync.WaitGroup
	for range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run, err := w.RunGate(context.Background(), "tested")
			if err != nil {
				t.Errorf("RunGate: %v", err)
				return
			}
			if run.Outcome != flow.OutcomeMeasured {
				t.Errorf("outcome = %q, want measured (detail: %s)", run.Outcome, run.Detail)
			}
		}()
	}
	wg.Wait()

	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read the gate's own log: %v", err)
	}
	lines := strings.Fields(string(b))
	if len(lines) != 2*runs {
		t.Fatalf("the gate logged %d lines, want %d — it did not run once per call:\n%s", len(lines), 2*runs, b)
	}
	inside := 0
	for i, l := range lines {
		if l == "in" {
			inside++
		} else {
			inside--
		}
		if inside > 1 {
			t.Fatalf("two gate processes were inside the exclusion at once (line %d):\n%s", i+1, b)
		}
	}
}

// The queue time is carried out on the run, so the party that can name the step
// files it as waiting. It is reported for the run that waited and for no other.
func TestRunGate_ReportsWhatItQueuedFor(t *testing.T) {
	f := useFakeHostScope(t)
	f.waitFor = 40 * time.Millisecond
	w, _ := hostScopeWorktree(t,
		`{"gates":[{"name":"tested","host_scope":true},{"name":"formatted"}]}`,
		`echo '{"gate":"g"}'`)

	run, err := w.RunGate(context.Background(), "tested")
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if run.Waited < 40*time.Millisecond {
		t.Errorf("Waited = %s, want the time actually spent queued", run.Waited)
	}

	free, err := w.RunGate(context.Background(), "formatted")
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if free.Waited != 0 {
		t.Errorf("Waited = %s on a gate that declared no host scope", free.Waited)
	}
}

// THE REGRESSION THE WHOLE ITEM EXISTS FOR. An exclusion that cannot be taken
// is an error and NO MEASUREMENT: proceeding would produce exactly the failure
// the declaration was made to prevent, and produce it with nothing in the
// report to say the exclusion was skipped.
//
// The proof is that the gate was not spawned, read from the tree rather than
// from the error text — an error naming the exclusion would be just as easy to
// write above a gate that ran anyway.
func TestRunGate_AnExclusionThatCannotBeTakenRunsNothing(t *testing.T) {
	f := useFakeHostScope(t)
	f.failWith = errors.New("injected: nowhere to put the lock")
	// The gate runs with the worktree as its working directory, so the marker
	// lands there — and its absence is the proof, read from the tree rather
	// than from the error text.
	w, dir := hostScopeWorktree(t,
		`{"gates":[{"name":"tested","host_scope":true}]}`,
		`touch ran; echo '{"gate":"tested"}'`)

	run, err := w.RunGate(context.Background(), "tested")
	if err == nil {
		t.Fatal("RunGate ran a declared gate without the exclusion")
	}
	if !strings.Contains(err.Error(), "host scope") {
		t.Errorf("err = %v, want it to name the exclusion it could not take", err)
	}
	if !errors.Is(err, f.failWith) {
		t.Errorf("err = %v, want it to carry why the exclusion could not be taken", err)
	}
	if run.Outcome != "" {
		t.Errorf("outcome = %q, want none — nothing ran, so there is nothing to judge", run.Outcome)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ran")); statErr == nil {
		t.Error("the gate was spawned despite the exclusion being unavailable")
	}
}

// A caller that gives up while queued runs no gate and still reports the queue
// it sat in. This is the path a step deadline firing mid-wait takes (#435), and
// the one where losing the figure would hide the contention that caused it.
func TestRunGate_AWaitGivenUpOnIsStillReported(t *testing.T) {
	f := useFakeHostScope(t)
	f.waitFor = time.Minute
	// The gate runs with the worktree as its working directory, so the marker
	// is written there and its absence is the proof nothing was spawned.
	w, dir := hostScopeWorktree(t,
		`{"gates":[{"name":"tested","host_scope":true}]}`,
		`touch ran; echo '{"gate":"tested"}'`)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	run, err := w.RunGate(ctx, "tested")
	if err == nil {
		t.Fatal("RunGate ran the gate for a caller that gave up queueing")
	}
	if run.Outcome != "" {
		t.Errorf("outcome = %q, want none — nothing ran", run.Outcome)
	}
	if run.Waited < 60*time.Millisecond {
		t.Errorf("Waited = %s, want the queue this run sat in before giving up", run.Waited)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ran")); statErr == nil {
		t.Error("the gate was spawned for a caller that never got the exclusion")
	}
	if _, stillHeld := f.snapshot(); stillHeld != 0 {
		t.Error("the exclusion is held after an acquire that failed")
	}
}

// Every exit releases it. A hold that survived one of these would starve every
// other measurement on the machine until the process ended — the failure the
// "never held across a stop" rule exists to close, reached by an ordinary
// return rather than by a crash.
func TestRunGate_ReleasesTheExclusionOnEveryExit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantOut flow.Outcome
	}{
		{
			name:    "a measured run",
			body:    `echo '{"gate":"tested"}'`,
			wantOut: flow.OutcomeMeasured,
		},
		{
			// The gate modified what it measured, so the runner refuses to call
			// the result a measurement. It must not also keep the machine.
			name:    "a gate that broke its contract",
			body:    `echo modified >> tracked; echo '{"gate":"tested"}'`,
			wantOut: flow.OutcomeBrokeContract,
		},
		{
			name:    "a gate that printed nothing",
			body:    `exit 3`,
			wantOut: flow.OutcomeDied,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := useFakeHostScope(t)
			w, _ := hostScopeWorktree(t, `{"gates":[{"name":"tested","host_scope":true}]}`, tc.body)

			run, err := w.RunGate(context.Background(), "tested")
			if err != nil {
				t.Fatalf("RunGate: %v", err)
			}
			if run.Outcome != tc.wantOut {
				t.Fatalf("outcome = %q, want %q (detail: %s)", run.Outcome, tc.wantOut, run.Detail)
			}
			if _, stillHeld := f.snapshot(); stillHeld != 0 {
				t.Errorf("%d holds outstanding after a %s — the exclusion is released around the measurement", stillHeld, tc.wantOut)
			}
		})
	}
}

// The exclusion is released when the runner fails before the spawn, too. The
// pre-snapshot is inside it because both snapshots are part of the one
// measurement, which makes this the exit that is easiest to leak.
func TestRunGate_ReleasesTheExclusionWhenThePreSnapshotFails(t *testing.T) {
	requireRealProcesses(t)
	f := useFakeHostScope(t)
	f.waitFor = 30 * time.Millisecond

	dir := filepath.Join(t.TempDir(), "work tree")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGateEntryPoint(t, dir, `case "$*" in
"--list --json") printf '{"gates":[{"name":"tested","host_scope":true}]}\n' ;;
*) echo '{"gate":"tested"}' ;;
esac`)

	b := &Orchestrator{cfg: Config{WorktreeDir: dir, GateTimeout: 30 * time.Second, Repo: "o/r"}}
	b.git = &gitOps{dir: dir, runner: func(_ context.Context, _, _ string, args ...string) ([]byte, []byte, error) {
		for _, a := range args {
			if a == "status" {
				return nil, nil, fmt.Errorf("injected: git status failed")
			}
		}
		return nil, nil, nil
	}}
	wt, err := b.Worktree(context.Background(), testItemRef(b))
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	run, err := wt.RunGate(context.Background(), "tested")
	if err == nil || !strings.Contains(err.Error(), "cannot snapshot worktree before gate") {
		t.Fatalf("err = %v, want the pre-snapshot failure", err)
	}
	if _, stillHeld := f.snapshot(); stillHeld != 0 {
		t.Error("the exclusion is still held after the runner failed before the spawn")
	}
	// The wait is reported even though no gate ran. A queue paid for and then
	// dropped because the runner failed is still this arena's wall clock spent
	// on contention, and a figure that counted it only on the paths that
	// succeeded would understate contention exactly where it is worst.
	if run.Waited < 30*time.Millisecond {
		t.Errorf("Waited = %s, want the queue this run paid for before it failed", run.Waited)
	}
}

// `fit` never queues, whatever a project declared, because it is asked BEFORE
// an item is taken and precisely when the machine may be busy. CheckFit is the
// path that asks it.
func TestCheckFit_NeverQueuesBehindTheExclusion(t *testing.T) {
	f := useFakeHostScope(t)
	w, _ := hostScopeWorktree(t,
		`{"gates":[{"name":"fit","host_scope":true}]}`,
		`echo '{"gate":"fit"}'`)

	run, err := w.RunGate(context.Background(), flow.GateFit)
	if err != nil {
		t.Fatalf("RunGate(fit): %v", err)
	}
	if run.Outcome != flow.OutcomeMeasured {
		t.Fatalf("outcome = %q, want measured (detail: %s)", run.Outcome, run.Detail)
	}
	if takes, _ := f.snapshot(); takes != 0 {
		t.Errorf("fit queued for the exclusion %d times — it is asked because the machine may be busy", takes)
	}
}
