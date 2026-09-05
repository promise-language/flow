package github

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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

// These spawn real processes.
//
// This is the whole subject of the runner: what it reports is what it OBSERVED
// of a process — that the exec failed, that a signal ended it, that it exited 0
// having printed nothing. A mocked spawn can observe none of that, so a test
// built on one would pass against a runner that decided on the exit code, which
// is precisely the defect these exist to prevent.
func requireRealProcesses(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the gate scripts are POSIX shell")
	}
}

// gateWorktree returns a worktree whose gate entry point is the given script,
// in a directory whose name contains a SPACE. The space is not decoration: if
// anything ever put a shell between the SDK and bin/gate, the exec line would
// word-split here and the gate would not be found.
//
// script == "" writes no bin/gate at all.
func gateWorktree(t *testing.T, script string, timeout time.Duration) (*worktree, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work tree")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if script != "" {
		writeScript(t, dir, "gate", script, 0o755)
	}
	b := &Orchestrator{cfg: Config{WorktreeDir: dir, GateTimeout: timeout}}
	// A gate does not go through the configured-command seam: that seam reports
	// (stdout, stderr, error), which cannot express what a runner has to see.
	b.git = &gitOps{dir: dir, runner: func(_ context.Context, _, name string, args ...string) ([]byte, []byte, error) {
		for _, a := range args {
			// The two halves of the pre/post snapshot. Both appear empty here:
			// this tree is clean and the gates under test do not touch it.
			if a == "status" || a == "diff" {
				return nil, nil, nil
			}
		}
		t.Errorf("a gate was spawned through the command runner (%s)", name)
		return nil, nil, nil
	}}
	return &worktree{b: b}, dir
}

// gateReady is what a gate says on stderr when it has reached the point a case
// needs it at, and gateAnnounce is the shell that says it. Both halves of the
// handshake are spelled once, so a case cannot wait for a word its gate never
// speaks.
const (
	gateReady    = "ready"
	gateAnnounce = "echo " + gateReady + " >&2"
)

// gateResult is what one in-flight RunGate returned. A case that has to act on
// the runner WHILE it runs cannot read the return value, so it reads this.
type gateResult struct {
	run flow.GateRun
	err error
}

// gateAnnouncesOnStderr points the gate's stderr at a pipe and returns the
// channel the gate's announcement arrives on: nil once the gate has said
// gateReady, an error otherwise.
//
// This is the handshake the order-sensitive cases synchronize on. Stderr is the
// stream the runner hands STRAIGHT to the child with no copying goroutine, so a
// line arriving here is the gate itself reporting where it got to — not a
// duration the case hoped was long enough. A window wide enough on an idle
// machine collapses on a loaded one, and the case then arranges a different
// state than the one it is named for. See docs/org/engineering-guide.md § "Time
// is not a coordinate".
//
// A pipe and not a temp file: the read blocks until the gate writes.
func gateAnnouncesOnStderr(t *testing.T) <-chan error {
	t.Helper()

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	restore := gateStderr
	gateStderr = pw
	t.Cleanup(func() {
		gateStderr = restore
		pw.Close()
		// Also what unblocks the reader below, so it never outlives the case.
		pr.Close()
	})

	announced := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(pr).ReadString('\n')
		switch {
		case err != nil:
			announced <- fmt.Errorf("reading the gate's stderr: %w", err)
		case strings.TrimSpace(line) != gateReady:
			announced <- fmt.Errorf("gate announced %q, want %q", strings.TrimSpace(line), gateReady)
		default:
			announced <- nil
		}
	}()
	return announced
}

// writeScript drops one entry point into the tree's bin/. The name is a
// parameter because a project exposes more than one — the gate that measures,
// and the judge that holds the thresholds — and both are reached the same way.
func writeScript(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(dir, "bin", name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // WriteFile respects umask; Chmod does not
		t.Fatal(err)
	}
}

// The five outcomes, decided from what the runner observed and never from the
// number the gate chose.
func TestRunGate_ClassifiesWhatItObserved(t *testing.T) {
	requireRealProcesses(t)

	for _, c := range []struct {
		name     string
		script   string
		outcome  flow.Outcome
		exitCode int
		stdout   string
	}{{
		name:     "a valid envelope is a measurement",
		script:   `echo '{"gate":"integration","checked":3}'`,
		outcome:  flow.OutcomeMeasured,
		exitCode: 0,
		stdout:   `{"gate":"integration","checked":3}`,
	}, {
		// The load-bearing case. A gate that measured a failure says so IN the
		// envelope and exits non-zero; the runner has still watched a complete
		// measurement, and whether the numbers are acceptable is not its
		// question. A runner that read the exit code would report a failure
		// here and throw away the measurement that explains it.
		name:     "a valid envelope beside a non-zero exit is still a measurement",
		script:   `echo '{"gate":"tested","failures":2}'; exit 1`,
		outcome:  flow.OutcomeMeasured,
		exitCode: 1,
		stdout:   `{"gate":"tested","failures":2}`,
	}, {
		// Exit-0 → pass believes this. A gate that exits 0 having printed
		// nothing has stated something false, and only the runner can say so.
		name:     "exit 0 having printed nothing is not a pass",
		script:   `exit 0`,
		outcome:  flow.OutcomeDied,
		exitCode: 0,
	}, {
		name:     "not an envelope at all",
		script:   `echo 'not json'`,
		outcome:  flow.OutcomeBrokeContract,
		exitCode: 0,
		stdout:   "not json",
	}, {
		// Silence is absence, not a malformed envelope. Truncation must land on
		// died and not on broke-the-contract: the envelope is written whole, so
		// a half-written one means the writer stopped existing, and blaming the
		// gate's code sends someone to the wrong repository.
		name:     "a truncated envelope died, it did not break the contract",
		script:   `printf '{"gate":"tested",'`,
		outcome:  flow.OutcomeDied,
		exitCode: 0,
		stdout:   `{"gate":"tested",`,
	}, {
		name:     "an array is not an envelope",
		script:   `echo '[1,2]'`,
		outcome:  flow.OutcomeBrokeContract,
		exitCode: 0,
	}, {
		name:     "a scalar is not an envelope",
		script:   `echo 7`,
		outcome:  flow.OutcomeBrokeContract,
		exitCode: 0,
	}, {
		// The one scalar a JSON object decodes from without complaint: null
		// leaves the map nil and returns no error. A runner that asks only
		// "did it parse" reports a complete run holding no measurements and
		// stating no reason for their absence — the safe-looking direction,
		// and indistinguishable downstream from a gate that measured nothing
		// on purpose.
		name:     "null parses, and it is still not an envelope",
		script:   `echo null`,
		outcome:  flow.OutcomeBrokeContract,
		exitCode: 0,
		stdout:   "null",
	}, {
		// Stdout carries the envelope and nothing else. A gate that also logs
		// there has broken the contract, even though the first value parses.
		name:     "an envelope with something after it",
		script:   `printf '{"gate":"tested"}\ndone\n'`,
		outcome:  flow.OutcomeBrokeContract,
		exitCode: 0,
	}, {
		name:     "two envelopes are not one envelope",
		script:   `printf '{"a":1}{"b":2}\n'`,
		outcome:  flow.OutcomeBrokeContract,
		exitCode: 0,
	}, {
		name:     "whitespace is silence",
		script:   `printf '   \n\n'`,
		outcome:  flow.OutcomeDied,
		exitCode: 0,
	}} {
		t.Run(c.name, func(t *testing.T) {
			w, _ := gateWorktree(t, c.script, 30*time.Second)
			run, err := w.RunGate(context.Background(), flow.GateIntegration)
			if err != nil {
				t.Fatalf("RunGate: %v — an error means no gate ran, and one did", err)
			}
			if run.Outcome != c.outcome {
				t.Errorf("outcome = %q, want %q (detail: %s)", run.Outcome, c.outcome, run.Detail)
			}
			if run.ExitCode != c.exitCode {
				t.Errorf("ExitCode = %d, want %d", run.ExitCode, c.exitCode)
			}
			if c.stdout != "" && !strings.Contains(string(run.Stdout), c.stdout) {
				t.Errorf("Stdout = %q, want it to carry %q", run.Stdout, c.stdout)
			}
			if run.Gate != flow.GateIntegration {
				t.Errorf("Gate = %q, want the name that was asked for", run.Gate)
			}
		})
	}
}

// "Absence and truncation are one case" is a TOTAL claim, and the table above
// samples it at one point: a stop after a comma. The runner reaches it from a
// single signal — the parse error being io.EOF or io.ErrUnexpectedEOF — so the
// claim holds exactly as far as that signal reaches, and a truncation point it
// misses is reported broke_contract, which hands a dead machine to the gate's
// author.
//
// Every prefix is a place a writer can stop, and the envelope below carries
// each JSON value kind so the prefixes cover stopping mid-key, mid-string,
// mid-number, mid-literal, inside a nested object and inside an array. The
// empty prefix is the absence end of the same case.
//
// Only this direction is checked here. The other one — that the signal does not
// spread to a gate that FINISHED printing something else, which would report a
// gate's own defect as a dead host — is what the broke_contract rows of the
// table above already pin, end to end.
func TestParseEnvelope_TruncationAnywhereIsEOFShaped(t *testing.T) {
	const envelope = `{"gate":"tested","ok":true,"n":12.5,"list":[1,2],"sub":{"k":"v"},"s":"abc"}`

	for i := 0; i < len(envelope); i++ {
		prefix := envelope[:i]
		err := parseEnvelope([]byte(prefix))
		if err == nil {
			t.Errorf("parseEnvelope(%q) = nil — a prefix of an envelope is not an envelope", prefix)
			continue
		}
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("parseEnvelope(%q) = %v, which is not EOF-shaped: the runner reads it as "+
				"broke_contract and sends a dead host to the gate's author", prefix, err)
		}
	}
}

// A gate the host killed has not reported, whatever it managed to print first.
// The signal is the runner's observation; the envelope would be the gate's
// claim, and the gate was not alive to finish making it.
func TestRunGate_ASignalIsDeathEvenWithAnEnvelopeOnStdout(t *testing.T) {
	requireRealProcesses(t)

	// The `sleep` is an external command, so the shell flushes its stdout
	// before forking it — the envelope is on the pipe before the kill.
	w, _ := gateWorktree(t, "echo '{\"gate\":\"integration\"}'\nsleep 0.2\nkill -9 $$", 30*time.Second)
	run, err := w.RunGate(context.Background(), flow.GateIntegration)
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if run.Outcome != flow.OutcomeDied {
		t.Errorf("outcome = %q, want %q (detail: %s)", run.Outcome, flow.OutcomeDied, run.Detail)
	}
	if !strings.Contains(string(run.Stdout), `"gate"`) {
		t.Fatalf("Stdout = %q, want the envelope that was printed before the signal", run.Stdout)
	}
	// -1 is the kernel having no number to give, which is exactly the state a
	// runner exists to report and an exit code cannot.
	if run.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 — a killed process chose no exit code", run.ExitCode)
	}
}

// The declared timeout outranks stdout too, and it lands somewhere else than a
// signal does. A gate killed at the timeout mid-write leaves a truncated
// envelope behind — the runner's signal for died — but the runner knows why
// this one stopped, and only timed_out is worth retrying unchanged. Classifying
// it from the stdout would report a sick host for a gate that needed longer,
// and the retry that would have fixed it never happens.
//
// The timeout tests print nothing, so there the deadline and the parse agree on
// the answer and either order passes. This is the case where they disagree, and
// it is the one a literal reading of "absence and truncation are one case"
// breaks.
func TestRunGate_TheDeclaredTimeoutOutranksTheTruncationItCaused(t *testing.T) {
	requireRealProcesses(t)

	// The two events this case needs — the half-envelope on the pipe, then the
	// deadline firing while the gate is still alive — are ordered by a SIGNAL,
	// not by a duration. A window wide enough on an idle machine collapses on a
	// loaded one, and the gate would be killed before it was scheduled to
	// write: see docs/org/engineering-guide.md § "Time is not a coordinate".
	//
	// The announcement comes from the EXEC'D program, never from a line in the
	// same shell. The shell buffers the half-envelope and flushes it when exec
	// replaces the image, so a `>&2` before the exec would announce a write
	// whose bytes are still in the shell's buffer — and the kill discards
	// those. This keeps the flush the case has always relied on and puts the
	// signal strictly after it.
	//
	// An hour, not a short window: with the deadline fired by hand the declared
	// timeout only reaches the Detail string, but a change that stopped
	// consulting gateDeadline while keeping the var would kill the sleep on the
	// clock and let this pass for the wrong reason. At an hour the sleep ends
	// first and the outcome is died, so that bypass fails loudly.
	w, _ := gateWorktree(t, "printf '{\"gate\":\"tes'\nexec sh -c '"+gateAnnounce+"; exec sleep 60'", time.Hour)

	// The signal rides the stderr the runner already hands straight to the
	// child, so reading it is a real handshake with the gate rather than a poll.
	announced := gateAnnouncesOnStderr(t)

	fired := make(chan struct{})
	var once sync.Once
	fire := func() { once.Do(func() { close(fired) }) }
	restoreDeadline := gateDeadline
	gateDeadline = func(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		return firedDeadline{Context: parent, fired: fired}, func() {}
	}
	t.Cleanup(func() { gateDeadline = restoreDeadline })

	// In flight, because the deadline has to be fired while the call is
	// running.
	results := make(chan gateResult, 1)
	var running sync.WaitGroup
	running.Add(1)
	go func() {
		defer running.Done()
		run, err := w.RunGate(context.Background(), flow.GateIntegration)
		results <- gateResult{run: run, err: err}
	}()
	// However this test ends, no sleep outlives it. Registered last, so it runs
	// before the restores above.
	t.Cleanup(func() {
		fire()
		running.Wait()
	})

	// A select and not a bare read: the runner holds the write end open, so a
	// gate that died before announcing would never produce EOF and the read
	// would hang until go test's own panic. Its outcome is the more useful
	// failure.
	select {
	case err := <-announced:
		if err != nil {
			t.Fatalf("waiting for the gate to announce its write: %v", err)
		}
	case r := <-results:
		t.Fatalf("the gate finished before it announced its write: outcome %q, err %v (detail: %s)",
			r.run.Outcome, r.err, r.run.Detail)
	}

	// The half-envelope is on the pipe. Only now can the deadline fire.
	fire()

	r := <-results
	if r.err != nil {
		t.Fatalf("RunGate: %v", r.err)
	}
	if !strings.Contains(string(r.run.Stdout), `{"gate":"tes`) {
		t.Fatalf("Stdout = %q, want the half-envelope — without it this is not the case under test", r.run.Stdout)
	}
	if r.run.Outcome != flow.OutcomeTimedOut {
		t.Errorf("outcome = %q, want %q (detail: %s)", r.run.Outcome, flow.OutcomeTimedOut, r.run.Detail)
	}
}

// firedDeadline is a deadline the test fires by hand, standing in for the one
// context.WithTimeout builds. Once fired it reports DeadlineExceeded and keeps
// reporting it, exactly as a real derived deadline does when the deadline is
// what fired FIRST — including when the caller goes away afterwards. Before it
// fires it is the parent's answer, which is that same real behaviour in the
// other direction.
//
// It deliberately does not shade the answer toward the parent. Which of the two
// a run is attributed to — an error, or the timed_out outcome — is runGate's
// ordering to get right, and a stand-in that pre-resolved it would make the
// case that pins that ordering pass either way.
//
// Done closes only on the firing: the parent's cancellation is not modelled as
// a kill, because the cases using this one kill their gate from the deadline.
type firedDeadline struct {
	context.Context
	fired <-chan struct{}
}

func (d firedDeadline) Done() <-chan struct{} { return d.fired }

func (d firedDeadline) Err() error {
	select {
	case <-d.fired:
		return context.DeadlineExceeded
	default:
		return d.Context.Err()
	}
}

// goneWhenFired is a caller that goes away at the instant the runner's own
// deadline fires — the one moment both are true, and the only one where the
// order runGate reads them in changes the answer.
//
// Done is never closed: the runner reads a caller's departure from Err, and
// nothing else here needs to wake on it.
type goneWhenFired struct {
	context.Context
	fired <-chan struct{}
}

func (c goneWhenFired) Err() error {
	select {
	case <-c.fired:
		return context.Canceled
	default:
		return c.Context.Err()
	}
}

// The caller going away outranks the runner's own deadline, and the order the
// two are read in is the whole of it. Both are true at once whenever a caller
// gives up in the moment the runner spends killing a gate at its deadline and
// reaping it. Read deadline-first, that run is reported timed_out — an outcome
// whose entire meaning is "retry this unchanged", handed to a retrier for work
// nobody is waiting for any more. Read caller-first, it is an error and no
// outcome exists, which is what a run with no reader is.
//
// The state cannot be arranged on the clock: a real derived deadline keeps
// whichever error fired first, so reaching it by timing means waiting out a
// deadline and then cancelling inside the window before Wait returns. The seam
// makes it an arrangement instead — the deadline fires by hand, and the caller
// is gone from that same instant.
func TestRunGate_ACallerWhoWentAwayOutranksTheRunnersOwnDeadline(t *testing.T) {
	requireRealProcesses(t)

	w, _ := gateWorktree(t, "exec sh -c '"+gateAnnounce+"; exec sleep 60'", time.Hour)
	announced := gateAnnouncesOnStderr(t)

	fired := make(chan struct{})
	var once sync.Once
	fire := func() { once.Do(func() { close(fired) }) }
	ctx := goneWhenFired{Context: context.Background(), fired: fired}
	restoreDeadline := gateDeadline
	gateDeadline = func(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		return firedDeadline{Context: parent, fired: fired}, func() {}
	}
	t.Cleanup(func() { gateDeadline = restoreDeadline })

	results := make(chan gateResult, 1)
	var running sync.WaitGroup
	running.Add(1)
	go func() {
		defer running.Done()
		run, err := w.RunGate(ctx, flow.GateIntegration)
		results <- gateResult{run: run, err: err}
	}()
	// However this test ends, no sleep outlives it. Registered last, so it runs
	// before the restores above.
	t.Cleanup(func() {
		fire()
		running.Wait()
	})

	select {
	case err := <-announced:
		if err != nil {
			t.Fatalf("waiting for the gate to announce itself: %v", err)
		}
	case r := <-results:
		t.Fatalf("the gate finished before it announced itself: outcome %q, err %v (detail: %s)",
			r.run.Outcome, r.err, r.run.Detail)
	}

	// The gate is running. Now the deadline fires and the caller goes away
	// together, which is the contested state.
	fire()

	r := <-results
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled — the caller went away, and only the "+
			"runner's OWN deadline is an outcome (outcome %q, detail: %s)", r.err, r.run.Outcome, r.run.Detail)
	}
	if r.run.Outcome != "" {
		t.Errorf("outcome = %q, want none — a run nobody is waiting for is not one to retry", r.run.Outcome)
	}
}

// The runner releases the deadline it built. Not housekeeping: an uncancelled
// derived deadline holds a timer AND a registration on the caller's context
// until it expires on its own, and one caller's context spans every gate a
// resolve runs — so a dropped cancel accumulates one live timer per gate, each
// for the length of the declared timeout.
//
// go vet's lostcancel check enforced this for as long as the call read
// context.WithTimeout in place. Behind a variable it sees no such call and
// reports nothing, which is what leaves the claim to a test.
func TestRunGate_ReleasesTheDeadlineItBuilt(t *testing.T) {
	requireRealProcesses(t)

	// Written by the deferred cancel inside RunGate, on this goroutine, and
	// read after it returns.
	released := false
	restoreDeadline := gateDeadline
	gateDeadline = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		gateCtx, cancel := context.WithTimeout(parent, d)
		return gateCtx, func() { released = true; cancel() }
	}
	t.Cleanup(func() { gateDeadline = restoreDeadline })

	w, _ := gateWorktree(t, `echo '{"gate":"integration"}'`, 30*time.Second)
	run, err := w.RunGate(context.Background(), flow.GateIntegration)
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	// A gate that never started would release the deadline too. This is the
	// path that has to.
	if run.Outcome != flow.OutcomeMeasured {
		t.Fatalf("outcome = %q, want measured (detail: %s)", run.Outcome, run.Detail)
	}
	if !released {
		t.Error("the deadline's cancel was never called: every gate run leaves a timer on the " +
			"caller's context until the declared timeout expires")
	}
}

// could not start is decided at the spawn, and must never fold into died. died
// carries "retry is correct"; a retry loop pointed at a missing binary never
// terminates and reads as a flaky host for as long as anyone lets it run.
func TestRunGate_CouldNotStartIsNotDied(t *testing.T) {
	requireRealProcesses(t)

	for _, c := range []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{{
		name:  "no gate entry point in the tree",
		setup: func(*testing.T, string) {},
	}, {
		name: "present but not executable",
		setup: func(t *testing.T, dir string) {
			writeScript(t, dir, "gate", `echo '{}'`, 0o644)
		},
	}, {
		name: "a directory where the program should be",
		setup: func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "bin", "gate"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}, {
		name: "a shebang naming an interpreter that is not there",
		setup: func(t *testing.T, dir string) {
			path := filepath.Join(dir, "bin", "gate")
			if err := os.WriteFile(path, []byte("#!/nonexistent/interpreter\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}} {
		t.Run(c.name, func(t *testing.T) {
			w, dir := gateWorktree(t, "", 30*time.Second)
			c.setup(t, dir)
			run, err := w.RunGate(context.Background(), flow.GateIntegration)
			if err != nil {
				t.Fatalf("RunGate: %v — this is an outcome, not an error", err)
			}
			if run.Outcome != flow.OutcomeCouldNotStart {
				t.Errorf("outcome = %q, want %q (detail: %s)", run.Outcome, flow.OutcomeCouldNotStart, run.Detail)
			}
			if run.ExitCode != -1 {
				t.Errorf("ExitCode = %d, want -1 — nothing ran, so nothing exited", run.ExitCode)
			}
			if run.Detail == "" {
				t.Error("no Detail: the person who has to fix this needs to know which program was absent")
			}
		})
	}
}

// A hung gate must not hang its runner, and the outcome must be the one worth
// retrying unchanged rather than one that blames the change.
func TestRunGate_EnforcesTheDeclaredTimeout(t *testing.T) {
	requireRealProcesses(t)

	// exec, so the shell does not linger as the sleep's parent.
	w, _ := gateWorktree(t, "exec sleep 60", 100*time.Millisecond)
	start := time.Now()
	run, err := w.RunGate(context.Background(), flow.GateIntegration)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if run.Outcome != flow.OutcomeTimedOut {
		t.Errorf("outcome = %q, want %q (detail: %s)", run.Outcome, flow.OutcomeTimedOut, run.Detail)
	}
	if elapsed > 30*time.Second {
		t.Errorf("returned after %s — the declared timeout did not bound the wait", elapsed)
	}
}

// A deadline that expired before the process existed is the WAIT being wrong,
// not a program the runner could not find — exec reports both through the same
// return from Start, and only the runner knows which it was looking at.
//
// could-not-start naming this would send whoever declared the gate, or whoever
// delivered the tree, hunting for a bin/gate that is sitting right there. It is
// the outcome that must least be allowed to absorb another, and it absorbs just
// as wrongly in this direction as in the one the retry loop cares about.
//
// A zero timeout is the deterministic way to reach the state; any declared
// timeout short enough to elapse between the spawn being set up and exec
// reaching the kernel lands in the same place, and GateTimeout is a project's
// to set.
func TestRunGate_ADeadlineThatExpiredBeforeTheSpawnIsATimeout(t *testing.T) {
	requireRealProcesses(t)

	w, dir := gateWorktree(t, "touch ran\necho '{}'", 0)
	run, err := w.RunGate(context.Background(), flow.GateIntegration)
	if err != nil {
		t.Fatalf("RunGate: %v — the runner's own deadline is an outcome, not an error", err)
	}
	if run.Outcome != flow.OutcomeTimedOut {
		t.Errorf("outcome = %q, want %q (detail: %s)", run.Outcome, flow.OutcomeTimedOut, run.Detail)
	}
	if run.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 — nothing ran, so nothing exited", run.ExitCode)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Error("the gate ran, so the declared timeout did not bound it")
	}
}

// The caller going away is not an outcome: no gate result exists to report.
// Reporting it as timed out would mark a run retryable that nobody is waiting
// for, and reporting it as died would blame the host for a caller's decision.
//
// The cancellation lands while the gate is KNOWN to be running, because the
// gate says so — not after a sleep the case hoped was long enough. Under load a
// sleep cancels before the spawn, and the case silently becomes the
// already-cancelled one below, leaving the branch it is named for untested at
// exactly the moment it would have mattered.
//
// And the declared timeout is an hour, so the second claim has somewhere to
// fail: the runner's own deadline is DERIVED from the caller's, which is what
// makes cancelling the caller kill the gate. A deadline built from a fresh root
// still returns an error here — the runner re-reads the caller's context on the
// way out — but only after the gate had run out an hour of its own, with the
// caller long gone and the process still holding the machine.
func TestRunGate_ACancelledCallerIsAnErrorAndNotAnOutcome(t *testing.T) {
	requireRealProcesses(t)

	w, _ := gateWorktree(t, "exec sh -c '"+gateAnnounce+"; exec sleep 60'", time.Hour)
	announced := gateAnnouncesOnStderr(t)

	ctx, cancel := context.WithCancel(context.Background())
	// However this test ends, the gate goes with it.
	t.Cleanup(cancel)

	results := make(chan gateResult, 1)
	go func() {
		run, err := w.RunGate(ctx, flow.GateIntegration)
		results <- gateResult{run: run, err: err}
	}()

	select {
	case err := <-announced:
		if err != nil {
			t.Fatalf("waiting for the gate to announce itself: %v", err)
		}
	case r := <-results:
		t.Fatalf("the gate finished before it announced itself: outcome %q, err %v (detail: %s)",
			r.run.Outcome, r.err, r.run.Detail)
	}

	cancel()

	// A bound, not a coordinate: the ordering above is the handshake's, and what
	// is left to happen is a process kill, which does not take seconds. An hour
	// is what a runner whose deadline lost the caller waits instead.
	select {
	case r := <-results:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("err = %v, want it to wrap context.Canceled (outcome %q, detail: %s)",
				r.err, r.run.Outcome, r.run.Detail)
		}
		if r.run.Outcome != "" {
			t.Errorf("outcome = %q, want none — no gate result exists", r.run.Outcome)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RunGate is still running 30s after its caller was cancelled: the runner's own " +
			"deadline is not derived from the caller's, so the gate outlives whoever asked for it")
	}
}

func TestRunGate_RefusesACallerWhoseContextIsAlreadyGone(t *testing.T) {
	requireRealProcesses(t)

	w, dir := gateWorktree(t, "touch ran\necho '{}'", 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run, err := w.RunGate(ctx, flow.GateIntegration)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if run.Outcome != "" {
		t.Errorf("outcome = %q, want none", run.Outcome)
	}
	// Specifically NOT could-not-start: exec reports an already-cancelled
	// context through the same return as a missing program, and confusing them
	// would send someone hunting for a binary that is right there.
	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Error("the gate was spawned for a caller that had already gone away")
	}
}

// The exec line is exec'd, never interpreted, and --envelope is appended last.
// A gate prints an envelope only when it was given the flag, so a runner that
// forgot it would read every gate as silence.
func TestRunGate_ExecsTheEntryPointWithTheNameThenTheFlag(t *testing.T) {
	requireRealProcesses(t)

	for _, gate := range []flow.GateName{flow.GateIntegration, flow.GateTested, "tested:wasm"} {
		t.Run(string(gate), func(t *testing.T) {
			w, dir := gateWorktree(t, ": > argv\necho \"$#\" >> argv\nfor a in \"$@\"; do echo \"$a\" >> argv; done\necho '{}'", 30*time.Second)
			run, err := w.RunGate(context.Background(), gate)
			if err != nil {
				t.Fatalf("RunGate: %v", err)
			}
			if run.Outcome != flow.OutcomeMeasured {
				t.Fatalf("outcome = %q, want measured (detail: %s)", run.Outcome, run.Detail)
			}
			recorded, err := os.ReadFile(filepath.Join(dir, "argv"))
			if err != nil {
				t.Fatal(err)
			}
			// The instance travels whole, and the count pins it: splitting
			// "tested:wasm" would ask for the concept and silently run every
			// suite under it.
			want := "2\n" + string(gate) + "\n--envelope\n"
			if string(recorded) != want {
				t.Errorf("gate saw argv %q, want %q", recorded, want)
			}
		})
	}
}

// Stdout is captured so it can be parsed. Stderr is the gate's progress and
// goes straight to the reader's own stream: someone watching a ten-minute
// suite needs to see it working, and a runner that accumulated it would hand
// them one block at exit.
func TestRunGate_PassesStderrThroughAndKeepsItOutOfTheResult(t *testing.T) {
	requireRealProcesses(t)

	sink, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	restore := gateStderr
	gateStderr = sink
	t.Cleanup(func() { gateStderr = restore })

	w, _ := gateWorktree(t, "echo 'suite 3 of 9' >&2\necho '{\"gate\":\"integration\"}'", 30*time.Second)
	run, err := w.RunGate(context.Background(), flow.GateIntegration)
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if run.Outcome != flow.OutcomeMeasured {
		t.Fatalf("outcome = %q, want measured (detail: %s)", run.Outcome, run.Detail)
	}
	passed, err := os.ReadFile(sink.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(passed), "suite 3 of 9") {
		t.Errorf("stderr sink holds %q, want the gate's progress", passed)
	}
	// Stderr is passed through, not accumulated — mixing it into the captured
	// stream would make the gate's own progress look like a broken envelope.
	if strings.Contains(string(run.Stdout), "suite 3 of 9") {
		t.Errorf("stderr reached Stdout: %q", run.Stdout)
	}
}

// The worktree modification check: a gate that modifies tracked or untracked
// state under measurement must be reported as broke_contract, never measured.
// See docs/gates-and-commands.md § "The non-modification rule is checked, not
// assumed".
func TestRunGate_DetectsWorktreeModification(t *testing.T) {
	requireRealProcesses(t)

	for _, c := range []struct {
		name    string
		script  string
		outcome flow.Outcome
		timeout time.Duration
	}{{
		name:    "clean gate stays measured",
		script:  `echo '{"g":1}'`,
		outcome: flow.OutcomeMeasured,
		timeout: 30 * time.Second,
	}, {
		name:    "modifies tracked file",
		script:  `echo x >> tracked; echo '{"g":1}'`,
		outcome: flow.OutcomeBrokeContract,
		timeout: 30 * time.Second,
	}, {
		name:    "creates untracked file",
		script:  `touch stray; echo '{"g":1}'`,
		outcome: flow.OutcomeBrokeContract,
		timeout: 30 * time.Second,
	}, {
		name:    "modifies and restores",
		script:  `echo x >> tracked; git checkout -- tracked; echo '{"g":1}'`,
		outcome: flow.OutcomeMeasured,
		timeout: 30 * time.Second,
	}, {
		name:    "timed out gate that also modified is still timed out",
		script:  `touch stray; exec sleep 60`,
		outcome: flow.OutcomeTimedOut,
		timeout: 100 * time.Millisecond,
	}, {
		name:    "died gate that also modified is still died",
		script:  `touch stray; exit 0`,
		outcome: flow.OutcomeDied,
		timeout: 30 * time.Second,
	}} {
		t.Run(c.name, func(t *testing.T) {
			w, _ := gateWorktreeGit(t, c.script, c.timeout)
			run, err := w.RunGate(context.Background(), flow.GateIntegration)
			if err != nil {
				t.Fatalf("RunGate: %v", err)
			}
			if run.Outcome != c.outcome {
				t.Errorf("outcome = %q, want %q (detail: %s)", run.Outcome, c.outcome, run.Detail)
			}
		})
	}
}

// A gate that deletes a tracked file has modified the worktree, same as one
// that edits or adds.
func TestRunGate_DetectsTrackedFileDeletion(t *testing.T) {
	requireRealProcesses(t)

	w, _ := gateWorktreeGit(t, `rm tracked; echo '{"g":1}'`, 30*time.Second)
	run, err := w.RunGate(context.Background(), flow.GateIntegration)
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if run.Outcome != flow.OutcomeBrokeContract {
		t.Errorf("outcome = %q, want %q", run.Outcome, flow.OutcomeBrokeContract)
	}
	if !strings.Contains(run.Detail, "tracked") {
		t.Errorf("Detail = %q, want it to name the file that was changed", run.Detail)
	}
}

// A gate pointed at a tree that is ALREADY dirty, which then rewrites the file
// that was already modified.
//
// This is the repairing gate — a format gate that fixes rather than reports —
// and it is the case the check exists for: its numbers get better, feed a
// ratchet, and raise a floor no honest run can meet. A dirty tree is not the
// violation; a gate may be pointed at one and should be, because uncommitted
// work is exactly what an operator often wants measured. The requirement is
// that the tree is the SAME afterwards.
//
// The path list alone cannot see it — `tracked` reads ` M` before and after —
// which is why docs/orchestrator.md names CapturePatch before and after as what
// answers it properly.
func TestRunGate_DetectsAModificationToAnAlreadyDirtyFile(t *testing.T) {
	requireRealProcesses(t)

	w, dir := gateWorktreeGit(t, `echo repaired > tracked; echo '{"g":1}'`, 30*time.Second)
	// The operator's own uncommitted work, present before the gate runs.
	if err := os.WriteFile(filepath.Join(dir, "tracked"), []byte("work in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	run, err := w.RunGate(context.Background(), flow.GateIntegration)
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if run.Outcome != flow.OutcomeBrokeContract {
		t.Fatalf("outcome = %q, want %q — the gate rewrote the file it measured (detail: %s)",
			run.Outcome, flow.OutcomeBrokeContract, run.Detail)
	}
	if !strings.Contains(run.Detail, "tracked") {
		t.Errorf("Detail = %q, want it to name the file that was changed", run.Detail)
	}
}

// When the worktree modification check finds a difference, the Detail must
// carry the porcelain output so a human reading the run knows WHAT changed.
func TestRunGate_ModificationDetailNamesTheChangedFiles(t *testing.T) {
	requireRealProcesses(t)

	w, _ := gateWorktreeGit(t, `echo x >> tracked; touch newfile; echo '{"g":1}'`, 30*time.Second)
	run, err := w.RunGate(context.Background(), flow.GateIntegration)
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if run.Outcome != flow.OutcomeBrokeContract {
		t.Fatalf("outcome = %q, want %q", run.Outcome, flow.OutcomeBrokeContract)
	}
	if !strings.Contains(run.Detail, "tracked") {
		t.Errorf("Detail = %q, want it to mention 'tracked'", run.Detail)
	}
	if !strings.Contains(run.Detail, "newfile") {
		t.Errorf("Detail = %q, want it to mention 'newfile'", run.Detail)
	}
}

// If StatusPorcelain fails BEFORE the gate runs, RunGate must return an error
// (not an outcome) and must not spawn the gate at all. An outcome implies the
// gate ran; an error means nothing happened.
func TestRunGate_PreSnapshotFailureIsAnErrorAndDoesNotSpawn(t *testing.T) {
	requireRealProcesses(t)

	dir := filepath.Join(t.TempDir(), "work tree")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, dir, "gate", "touch ran\necho '{}'", 0o755)

	b := &Orchestrator{cfg: Config{WorktreeDir: dir, GateTimeout: 30 * time.Second}}
	b.git = &gitOps{dir: dir, runner: func(_ context.Context, _, _ string, args ...string) ([]byte, []byte, error) {
		for _, a := range args {
			if a == "status" {
				return nil, nil, fmt.Errorf("injected: git status failed")
			}
		}
		return nil, nil, nil
	}}
	w := &worktree{b: b}

	run, err := w.RunGate(context.Background(), flow.GateIntegration)
	if err == nil || !strings.Contains(err.Error(), "cannot snapshot worktree before gate") {
		t.Fatalf("err = %v, want an error about the pre-snapshot failure", err)
	}
	if run.Outcome != "" {
		t.Errorf("outcome = %q, want none — nothing was run", run.Outcome)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ran")); statErr == nil {
		t.Error("the gate was spawned despite the pre-snapshot failure")
	}
}

// If StatusPorcelain fails AFTER a measured gate, the outcome must be
// broke_contract (not an error and not measured). The gate DID run; the runner
// cannot verify that it left the worktree intact, so it must refuse to call the
// result a measurement.
func TestRunGate_PostSnapshotFailureIsBrokeContract(t *testing.T) {
	requireRealProcesses(t)

	dir := filepath.Join(t.TempDir(), "work tree")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, dir, "gate", `echo '{"gate":"integration"}'`, 0o755)

	calls := 0
	b := &Orchestrator{cfg: Config{WorktreeDir: dir, GateTimeout: 30 * time.Second}}
	b.git = &gitOps{dir: dir, runner: func(_ context.Context, _, _ string, args ...string) ([]byte, []byte, error) {
		for _, a := range args {
			if a == "status" {
				calls++
				if calls == 1 {
					return nil, nil, nil // pre-snapshot succeeds
				}
				return nil, nil, fmt.Errorf("injected: post-snapshot failed")
			}
		}
		return nil, nil, nil
	}}
	w := &worktree{b: b}

	run, err := w.RunGate(context.Background(), flow.GateIntegration)
	if err != nil {
		t.Fatalf("RunGate returned error: %v — a post-snapshot failure is an outcome, not an error", err)
	}
	if run.Outcome != flow.OutcomeBrokeContract {
		t.Errorf("outcome = %q, want %q", run.Outcome, flow.OutcomeBrokeContract)
	}
	if !strings.Contains(run.Detail, "cannot verify worktree integrity") {
		t.Errorf("Detail = %q, want it to explain the post-snapshot failure", run.Detail)
	}
}

// gateWorktreeGit is like gateWorktree but initializes a real git repository,
// so StatusPorcelain works and the worktree modification check runs against
// actual git state.
func gateWorktreeGit(t *testing.T, script string, timeout time.Duration) (*worktree, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work tree")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Initialize a git repo with a tracked file so tests can modify it.
	gitInit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInit("init")
	if script != "" {
		writeScript(t, dir, "gate", script, 0o755)
	}
	gitInit("add", "-A")
	gitInit("commit", "-m", "init")

	b := &Orchestrator{cfg: Config{WorktreeDir: dir, GateTimeout: timeout}}
	b.git = newGitOps(dir)
	return &worktree{b: b}, dir
}

// A gate that leaves a background child holding the stdout pipe must not hold
// the runner after the gate itself is gone. Without a bound on that wait, the
// declared timeout fires and the runner still hangs — the exact failure a
// declared timeout exists to close.
func TestRunGate_DoesNotWaitForeverOnAChildTheGateLeftBehind(t *testing.T) {
	requireRealProcesses(t)

	restore := gateWaitDelay
	gateWaitDelay = 200 * time.Millisecond
	t.Cleanup(func() { gateWaitDelay = restore })

	// The child keeps fd 1 — the pipe the runner is reading, which is the whole
	// point — and drops stderr, which it would otherwise hold open past the end
	// of the run for no reason this test is about.
	w, _ := gateWorktree(t, "sleep 30 2>/dev/null &\necho '{\"gate\":\"integration\"}'", 30*time.Second)
	start := time.Now()
	run, err := w.RunGate(context.Background(), flow.GateIntegration)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("returned after %s — the runner waited on a child the gate had abandoned", elapsed)
	}
	// The gate itself completed and printed an envelope. What its leftover
	// child is doing is not a reason to discard that.
	if run.Outcome != flow.OutcomeMeasured {
		t.Errorf("outcome = %q, want measured (detail: %s)", run.Outcome, run.Detail)
	}
}

// An undeclared concept is refused before anything is spawned. Running it would
// hand the project a name it cannot answer, and the failure would look like the
// gate refusing rather than the caller asking for something that does not exist.
func TestRunGate_RefusesAnUndeclaredNameWithoutSpawning(t *testing.T) {
	requireRealProcesses(t)

	w, dir := gateWorktree(t, "touch ran\necho '{}'", 30*time.Second)
	run, err := w.RunGate(context.Background(), "lint")
	if err == nil || !strings.Contains(err.Error(), "not a declared gate name") {
		t.Fatalf("err = %v, want a refusal naming the problem", err)
	}
	if run.Outcome != "" {
		t.Errorf("outcome = %q, want none — nothing was run", run.Outcome)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Error("spawned a process for an undeclared gate name")
	}
}

// A gate that prints far more than an envelope should not exhaust the runner.
// The captured stdout is truncated to the bound, and the excess is discarded
// without blocking the child.
func TestRunGate_BoundsStdoutCapture(t *testing.T) {
	requireRealProcesses(t)

	// A valid envelope followed by enough padding to exceed maxEnvelopeOutput.
	// The padding goes past the envelope, so the runner sees trailing content
	// and reports broke-the-contract — the point is that it SURVIVES to do so
	// rather than accumulating unboundedly.
	//
	// dd writes binary zeros, which are cheaper than generating text and just
	// as effective at filling a buffer. The count is in 1 KiB blocks.
	blocks := (maxEnvelopeOutput / 1024) + 10
	script := fmt.Sprintf(
		`echo '{"gate":"integration"}'; dd if=/dev/zero bs=1024 count=%d 2>/dev/null`,
		blocks,
	)
	w, _ := gateWorktree(t, script, 30*time.Second)
	run, err := w.RunGate(context.Background(), flow.GateIntegration)
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	// The envelope plus padding exceeds the bound, so broke-the-contract is
	// correct (trailing content). The key assertion is that Stdout is capped.
	if run.Outcome != flow.OutcomeBrokeContract {
		t.Errorf("outcome = %q, want %q (detail: %s)", run.Outcome, flow.OutcomeBrokeContract, run.Detail)
	}
	if len(run.Stdout) > maxEnvelopeOutput {
		t.Errorf("Stdout is %d bytes, want at most %d — the capture is unbounded", len(run.Stdout), maxEnvelopeOutput)
	}
}

// Verify runs the CONFIGURED command; RunGate runs the entry point. Confusing
// them would make a decision rest on the command that modifies the tree, which
// is the distinction the two methods exist to keep.
func TestRunGate_RunsTheEntryPointAndNotTheVerifyCommand(t *testing.T) {
	requireRealProcesses(t)

	w, dir := gateWorktree(t, "echo '{\"gate\":\"integration\"}'", 30*time.Second)
	w.b.cfg.VerifyCmd = []string{filepath.Join(dir, "bin", "verify")}
	// bin/verify does not exist. If RunGate ran it, the outcome would be
	// could-not-start rather than a measurement.
	run, err := w.RunGate(context.Background(), flow.GateIntegration)
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	if run.Outcome != flow.OutcomeMeasured {
		t.Errorf("outcome = %q, want measured — RunGate did not reach bin/gate (detail: %s)", run.Outcome, run.Detail)
	}
}
