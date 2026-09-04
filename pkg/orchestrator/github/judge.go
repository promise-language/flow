package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/promise-language/flow"
)

// verdictFlag is how the SDK asks the judging entry point for a verdict on a
// measurement it is being handed. Protocol, not configuration: the entry point
// answers on stdout only when it was given the flag, and any other invocation
// is the project's own measuring mode, which spawns a gate and is not this.
// Appended last, for the same reason --envelope is.
const verdictFlag = "--verdict"

const (
	// maxVerdictOutput bounds what the SDK reads from a judge's stdout. A
	// verdict is one small JSON object; anything approaching 1 MiB is already
	// malformed.
	maxVerdictOutput = 1 << 20 // 1 MiB

	// maxJudgeStderr bounds the diagnostic output kept for error messages.
	maxJudgeStderr = 64 << 10 // 64 KiB
)

// askJudge hands one measurement to the project's judging entry point and
// returns what it answered.
//
// The SDK spawns this, as it spawns the gate, but it is NOT a second runner:
// nothing here classifies what became of the process, because nothing needs
// to. A runner is the sole witness to a gate that no longer exists; a judge
// that did not answer can simply be asked again by anyone holding the
// envelope. So there is no outcome vocabulary here and no five-way split — the
// verdict is what arrived on stdout, and everything else is an error.
//
// A NON-NIL ERROR MEANS NO VERDICT EXISTS. It is never a refusal: a judge that
// could not be reached has said nothing about the tree, and reading its
// silence as "not acceptable" would refuse a sound change because the
// project's tooling is broken.
func askJudge(ctx context.Context, dir string, run flow.GateRun, argv []string, timeout time.Duration) (flow.GateVerdict, error) {
	if err := ctx.Err(); err != nil {
		return flow.GateVerdict{}, fmt.Errorf("judging %s: caller went away before the judge was spawned: %w", run.Gate, err)
	}

	judgeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := newBoundedWriter(maxVerdictOutput)
	errs := newBoundedWriter(maxJudgeStderr)
	// argv[0] contains a separator, so os/exec performs no PATH lookup and no
	// shell stands between here and the judge — the same rule the gate's exec
	// line follows, and for the same reason.
	cmd := exec.CommandContext(judgeCtx, argv[0], argv[1:]...)
	cmd.Dir = dir
	// The envelope the runner observed, byte for byte. The judge is handed a
	// measurement it did not produce; that is the whole property this method
	// exists to preserve.
	cmd.Stdin = bytes.NewReader(run.Stdout)
	// Captured and bounded, so they can be parsed and reported without a
	// chatty judge exhausting the runner.
	cmd.Stdout = out
	// The judge's stderr is CAPTURED and not passed through, which is the
	// opposite of a gate's. A gate's stderr is the minutes of a long run, and
	// a person watching needs it as it happens; a judge compares numbers it
	// was given and prints an answer, so anything it writes there is the
	// reason it could not, and it belongs in the error.
	cmd.Stderr = errs
	cmd.WaitDelay = gateWaitDelay

	runErr := cmd.Run()

	// The caller going away is not an answer. Checked before anything else,
	// because a cancelled parent cancels the derived context too.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return flow.GateVerdict{}, fmt.Errorf("judging %s: caller went away while the judge ran: %w", run.Gate, ctxErr)
	}

	// The verdict is what is on stdout, and the exit code is not consulted —
	// for the same reason a gate's is not, plus one of its own: a project that
	// made its judging mode exit non-zero on a refusal is doing something
	// reasonable, and reading that as "the judge failed" would turn a
	// legitimate refusal into an unanswerable judge.
	verdict, parseErr := parseVerdict(out.Bytes())
	if parseErr == nil {
		verdict.Run = run
		return verdict, nil
	}

	// No verdict. Say why in the terms of whoever has to fix it: the deadline
	// first, because our own kill is what stopped the answer; then a process
	// that never ran to completion; then what it printed instead.
	if errors.Is(judgeCtx.Err(), context.DeadlineExceeded) {
		return flow.GateVerdict{}, fmt.Errorf("judging %s: killed at the declared timeout of %s%s", run.Gate, timeout, stderrDetail(errs.Bytes()))
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return flow.GateVerdict{}, fmt.Errorf("judging %s: %s did not answer: %w%s", run.Gate, argv[0], runErr, stderrDetail(errs.Bytes()))
	}
	// Silence and truncation are absence — the judge printed no verdict at all
	// — while anything else is a defect in what it printed. Neither is a
	// refusal, and the difference is what tells the person reading this
	// whether to look at the judge's output or at why it stopped.
	if errors.Is(parseErr, io.EOF) || errors.Is(parseErr, io.ErrUnexpectedEOF) {
		return flow.GateVerdict{}, fmt.Errorf("judging %s: %s exited %d without printing a verdict: %w%s",
			run.Gate, argv[0], exitCode(cmd), parseErr, stderrDetail(errs.Bytes()))
	}
	return flow.GateVerdict{}, fmt.Errorf("judging %s: %s printed something that is not a verdict: %w%s", run.Gate, argv[0], parseErr, stderrDetail(errs.Bytes()))
}

// exitCode is the judge's exit status where the kernel has one, and -1 where
// it does not. A raw diagnostic in an error message and nothing else: no
// verdict is read from it, which is the whole reason the judging mode may exit
// non-zero on a refusal without that refusal becoming unanswerable.
func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}

// stderrDetail renders what the judge wrote to stderr for inclusion in an
// error, or nothing if it wrote nothing. It is the only account of why an
// unanswerable judge could not answer.
func stderrDetail(stderr []byte) string {
	s := strings.TrimSpace(string(stderr))
	if s == "" {
		return ""
	}
	return "\nstderr:\n" + s
}

// verdictWire is the judge's answer as it arrives on stdout.
//
// Acceptable is a POINTER and Thresholds a raw message so that an absent field
// is refused rather than defaulted, and so that a field present but null is
// refused too. A missing "acceptable" decoding to false is the SDK inventing a
// refusal out of a judge that did not give one, and a verdict whose thresholds
// were discarded is as unfalsifiable as a lying runner — whether they were
// omitted or written as null.
type verdictWire struct {
	Acceptable *bool           `json:"acceptable"`
	Thresholds json.RawMessage `json:"thresholds"`
	Detail     string          `json:"detail"`
}

// parseVerdict reads exactly one JSON object from stdout — not null, not an
// array, not a scalar — and nothing else, and requires the two fields a
// verdict cannot be without.
//
// What is inside "thresholds" is between the project and its judge: the SDK
// carries it so the verdict can be recomputed, and does not read it.
func parseVerdict(stdout []byte) (flow.GateVerdict, error) {
	dec := json.NewDecoder(bytes.NewReader(stdout))
	// A pointer, so JSON null — the one scalar a struct absorbs without
	// complaint — is caught rather than read as a verdict with every field at
	// its zero value, which would be a refusal nobody made.
	var wire *verdictWire
	if err := dec.Decode(&wire); err != nil {
		return flow.GateVerdict{}, err
	}
	if wire == nil {
		return flow.GateVerdict{}, errors.New("the verdict is null")
	}
	if dec.More() {
		return flow.GateVerdict{}, errors.New("trailing content after the verdict")
	}
	if wire.Acceptable == nil {
		return flow.GateVerdict{}, errors.New(`the verdict has no "acceptable" field, and a verdict that did not answer is not a refusal`)
	}
	// JSON null is refused here for the same reason it is refused one level up:
	// it is the value a raw message absorbs without complaint, and a null
	// threshold set is discarded terms wearing a present field — nothing can
	// tell it from the judge that forgot, and neither can be re-checked.
	if len(wire.Thresholds) == 0 || bytes.Equal(bytes.TrimSpace(wire.Thresholds), []byte("null")) {
		return flow.GateVerdict{}, errors.New(`the verdict states no "thresholds", so nothing could re-check it`)
	}
	return flow.GateVerdict{
		Acceptable: *wire.Acceptable,
		Thresholds: wire.Thresholds,
		Detail:     wire.Detail,
	}, nil
}
