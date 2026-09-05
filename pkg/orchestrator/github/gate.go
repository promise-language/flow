package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/promise-language/flow"
)

// envelopeFlag is how the runner asks a gate for its measurements. Protocol,
// not configuration: a gate prints an envelope only when it was given the
// flag, and any other invocation is a person at a terminal. Appended last, so
// the gate name reaches the entry point in the position it would have had
// without it.
const envelopeFlag = "--envelope"

// maxEnvelopeOutput bounds what the runner reads from a gate's stdout. An
// envelope is one small JSON object; anything approaching 1 MiB is already
// broke-the-contract. The bound prevents a chatty gate from exhausting the
// runner.
const maxEnvelopeOutput = 1 << 20 // 1 MiB

// gateStderr is where a gate's progress goes. Typed *os.File deliberately:
// exec.Cmd hands a real descriptor straight to the child and spawns no copying
// goroutine, which is the passthrough the contract requires. An io.Writer here
// would be a pipe the runner copies, and most runtimes switch to block
// buffering the moment their output is not a terminal — so a ten-minute gate's
// progress would arrive in one block at exit, which is the silence the rule
// exists to prevent.
//
// A variable so a test can point it somewhere it can read; nothing else
// reassigns it.
var gateStderr *os.File = os.Stderr

// gateWaitDelay bounds how long Wait may block on the stdout pipe AFTER the
// process itself is gone. Without it, a gate that leaves a background child
// holding that pipe makes Wait block forever after the timeout kill — the
// declared timeout would fire and the runner would still hang, which is the
// failure a declared timeout exists to close.
var gateWaitDelay = 5 * time.Second

// gateDeadline builds the runner's own deadline from the caller's context. It
// is context.WithTimeout and nothing else; a variable only so a test can hand
// the runner a deadline it fires from a SIGNAL instead of from the clock.
//
// The cases that need it are the ones where a gate must be killed at the
// deadline AFTER it has printed something. Those two events cannot be ordered
// by choosing a duration: the write is a process waiting to be scheduled, so a
// window that holds on an idle machine collapses on a loaded one and no amount
// of lengthening closes it. See docs/org/engineering-guide.md § "Time is not a
// coordinate".
var gateDeadline = context.WithTimeout

// runGate spawns one gate and reports what it observed. It is the runner: it
// never consults the gate's exit code to decide, and it never looks inside the
// envelope — the SDK does not read a project's measurements, it runs the gate
// and reports what became of the run.
//
// A non-nil error means no gate ran and no outcome exists. Every way a gate
// itself can fail is one of the five outcomes.
func runGate(ctx context.Context, dir string, name flow.GateName, argv []string, timeout time.Duration) (flow.GateRun, error) {
	run := flow.GateRun{Gate: name, ExitCode: -1}

	if err := ctx.Err(); err != nil {
		return flow.GateRun{}, fmt.Errorf("gate %s: caller went away before the gate was spawned: %w", name, err)
	}

	gateCtx, cancel := gateDeadline(ctx, timeout)
	defer cancel()

	// argv[0] contains a separator, so os/exec performs no PATH lookup and the
	// OS resolves it against Dir after the child chdirs. No shell stands
	// between here and the gate: a line that is interpreted gives different
	// answers on different hosts for reasons that have nothing to do with the
	// subject.
	out := newBoundedWriter(maxEnvelopeOutput)
	cmd := exec.CommandContext(gateCtx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin = nil // /dev/null — a gate cannot block on a terminal that is not there
	// Captured and bounded, so it can be parsed without a chatty gate
	// exhausting the runner.
	cmd.Stdout = out
	cmd.Stderr = gateStderr
	cmd.WaitDelay = gateWaitDelay

	if err := cmd.Start(); err != nil {
		// Start fails only when no process was created. That is exactly "could
		// not start": absent, not executable, a directory, a bad interpreter.
		// No error-string sniffing is needed to reach it — except for the
		// caller's own context, which exec reports through the same return.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return flow.GateRun{}, fmt.Errorf("gate %s: caller went away before the gate was spawned: %w", name, ctxErr)
		}
		if errors.Is(gateCtx.Err(), context.DeadlineExceeded) {
			// Our own deadline expired before the process existed, and exec
			// reports that through the same return as a missing program. The
			// wait is the problem, not the tree: could-not-start would name
			// whoever declared the gate or delivered the tree and send them
			// hunting for a bin/gate that is sitting right there. An outcome
			// that absorbs another attributes a failure to the wrong
			// repository, which is the whole reason the set has five names.
			run.Outcome = flow.OutcomeTimedOut
			run.Detail = fmt.Sprintf("the declared timeout of %s expired before the gate was spawned", timeout)
			return run, nil
		}
		run.Outcome = flow.OutcomeCouldNotStart
		run.Detail = fmt.Sprintf("spawning %s in %s: %v", argv[0], dir, err)
		return run, nil
	}

	waitErr := cmd.Wait()
	run.Stdout = out.Bytes()
	if cmd.ProcessState != nil {
		run.ExitCode = cmd.ProcessState.ExitCode()
	}

	// The caller going away is not an outcome. Checked before the deadline,
	// because a cancelled parent cancels the derived context too and only OUR
	// deadline is the one a retry can resolve.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return flow.GateRun{}, fmt.Errorf("gate %s: caller went away while the gate ran: %w", name, ctxErr)
	}
	if errors.Is(gateCtx.Err(), context.DeadlineExceeded) {
		run.Outcome = flow.OutcomeTimedOut
		run.Detail = fmt.Sprintf("killed at the declared timeout of %s", timeout)
		return run, nil
	}
	// A signal wins over anything on stdout. A gate the host killed has not
	// reported, whatever it managed to print first.
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		run.Outcome = flow.OutcomeDied
		run.Detail = fmt.Sprintf("did not exit on its own: %v", waitErr)
		return run, nil
	}

	// The envelope is written whole, so what is on stdout tells the rest apart.
	// The runner checks that ONE JSON object is there and nothing else; what is
	// inside it is between the project and whoever judges it.
	//
	// "An envelope that contradicts what the manifest declared" is also
	// broke-the-contract, and is unreachable until a manifest exists (#38).
	if err := parseEnvelope(run.Stdout); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// Silence is absence, not a malformed envelope — and a truncated
			// envelope is not an envelope at all, which is what makes it safe
			// to read a complete one as a complete run.
			run.Outcome = flow.OutcomeDied
			run.Detail = fmt.Sprintf("exited %d without printing a readable envelope: %v", run.ExitCode, err)
			return run, nil
		}
		run.Outcome = flow.OutcomeBrokeContract
		run.Detail = fmt.Sprintf("printed something that is not an envelope: %v", err)
		return run, nil
	}

	// Exit code recorded, decided on by nothing: a gate that measured a failure
	// and said so exited non-zero and has measured.
	run.Outcome = flow.OutcomeMeasured
	return run, nil
}

// parseEnvelope reports whether stdout carries exactly one JSON object — not
// null, not an array, not a scalar — and nothing else. It returns io.EOF or
// io.ErrUnexpectedEOF for absence and
// truncation — the two the caller must read as "died" rather than as a defect
// in what the gate printed.
func parseEnvelope(stdout []byte) error {
	dec := json.NewDecoder(bytes.NewReader(stdout))
	var envelope map[string]any // a map, so an array or a scalar is refused
	if err := dec.Decode(&envelope); err != nil {
		return err
	}
	if envelope == nil {
		// JSON `null` is the one scalar a map absorbs: it decodes without
		// error and leaves the map nil. It is PRESENT, so it is not absence,
		// and it is not an object either — it is a defect in what the gate
		// printed. Reading it as a measurement is the safe-looking direction
		// the contract warns about: a complete parse taken for a complete run,
		// carrying no measurements and no stated reason for their absence,
		// which is exactly what makes deriving completeness from a silent
		// envelope safe everywhere else.
		return errors.New("the envelope is null")
	}
	if dec.More() {
		return errors.New("trailing content after the envelope")
	}
	return nil
}

// runCommand spawns one COMMAND and reports what it observed.
//
// It is the same shape as runGate and deliberately so: a command returns a run
// carrying an Outcome, not a bare error, because a caller has to tell a failing
// check from one that never executed. A lock that timed out is worth retrying
// unchanged and costs no invocation; a missing binary is not and does not; a
// check that ran and reported failures is a real result and costs a round.
// Collapsing the three into `err != nil` charged all of them the same.
//
// Where it differs from runGate is the whole difference between a command and a
// gate: there is NO ENVELOPE. A command reports by exiting, so a non-zero exit
// is a measurement — "it ran and what remains is not sound" — and never
// broke_contract. Nothing decides on what a command reports either way.
//
// A non-nil error is impossible here by construction; the signature matches
// runGate's caller shape and every failure is an outcome.
func runCommand(ctx context.Context, dir string, name flow.CommandName, argv []string, timeout time.Duration) flow.CommandRun {
	run := flow.CommandRun{Command: name, ExitCode: -1}

	if err := ctx.Err(); err != nil {
		run.Outcome = flow.OutcomeCouldNotStart
		run.Detail = fmt.Sprintf("caller went away before the command was spawned: %v", err)
		return run
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := newBoundedWriter(maxEnvelopeOutput)
	cmd := exec.CommandContext(cmdCtx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Stdout = out
	// Straight through to the reader's own stream, not accumulated: a command
	// that repairs and then measures can run for minutes, and someone watching
	// it needs to see it work.
	cmd.Stderr = gateStderr
	cmd.WaitDelay = gateWaitDelay

	if err := cmd.Start(); err != nil {
		if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
			run.Outcome = flow.OutcomeTimedOut
			run.Detail = fmt.Sprintf("the timeout of %s expired before the command was spawned", timeout)
			return run
		}
		run.Outcome = flow.OutcomeCouldNotStart
		run.Detail = fmt.Sprintf("spawning %s in %s: %v", argv[0], dir, err)
		return run
	}

	waitErr := cmd.Wait()
	run.Stdout = out.Bytes()
	if cmd.ProcessState != nil {
		run.ExitCode = cmd.ProcessState.ExitCode()
	}

	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		run.Outcome = flow.OutcomeTimedOut
		run.Detail = fmt.Sprintf("killed at the timeout of %s", timeout)
		return run
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		run.Outcome = flow.OutcomeDied
		run.Detail = fmt.Sprintf("did not exit on its own: %v", waitErr)
		return run
	}

	// It ran and reported. Whether the exit code is zero is the measurement,
	// not a classification of the run.
	run.Outcome = flow.OutcomeMeasured
	run.Detail = fmt.Sprintf("command %q exited %d", name, run.ExitCode)
	return run
}
