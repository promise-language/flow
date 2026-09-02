package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
)

// These spawn real processes, for the same reason the runner's tests do: the
// subject is a process boundary. The judge is a program the SDK hands an
// envelope on stdin and reads one object back from, and a mocked call would
// exercise none of that — not the stdin plumbing, not an entry point that is
// not there, not a judge that answers and exits non-zero anyway.

// aVerdict is what a well-behaved judge prints: one object, both required
// fields.
const aVerdict = `{"acceptable":true,"thresholds":{"failed_tests":0},"detail":"failed_tests is 0, cap 0"}`

// judgeWorktree returns a worktree whose judging entry point is the given
// script, in the runner's fixture: a directory whose name contains a SPACE (so
// a shell between the SDK and the entry point would word-split and fail), and
// a command-runner seam that errors if anything goes through it.
//
// It writes NO bin/gate. Judge is handed a measurement it did not produce, so
// it must not need a gate in the tree to answer.
//
// script == "" writes no bin/run at all.
func judgeWorktree(t *testing.T, script string, timeout time.Duration) (*worktree, string) {
	t.Helper()
	w, dir := gateWorktree(t, "", timeout)
	if script != "" {
		writeScript(t, dir, "run", script, 0o755)
	}
	return w, dir
}

// measured is a run the SDK observed as a measurement: the only kind that may
// be judged.
func measured(gate flow.GateName, envelope string) flow.GateRun {
	return flow.GateRun{
		Gate:    gate,
		Outcome: flow.OutcomeMeasured,
		Stdout:  []byte(envelope),
	}
}

// assertNoVerdict states the rule an error return carries: no verdict exists,
// and above all it is NOT a refusal. A caller that read the zero value as
// "unacceptable" would refuse a sound change because the project's judging
// layer is broken, which is a fact for a person and not a result.
func assertNoVerdict(t *testing.T, v flow.GateVerdict, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("no error: a judge that did not answer must not read as one that did")
	}
	if v.Acceptable {
		t.Error("Acceptable is true beside an error")
	}
	if v.Thresholds != nil {
		t.Errorf("Thresholds = %q, want none — nothing was compared", v.Thresholds)
	}
	if v.Run.Outcome != "" {
		t.Errorf("Run.Outcome = %q, want none — there is no verdict to carry a run", v.Run.Outcome)
	}
}

// A verdict is what the judge printed, and it comes back carrying the
// measurement it was reached from.
func TestJudge_ReturnsWhatTheProjectAnswered(t *testing.T) {
	requireRealProcesses(t)

	for _, c := range []struct {
		name       string
		script     string
		acceptable bool
		thresholds string
		detail     string
	}{{
		name:       "acceptable",
		script:     `echo '{"acceptable":true,"thresholds":{"failed_tests":0},"detail":"failed_tests is 0, cap 0"}'`,
		acceptable: true,
		thresholds: `{"failed_tests":0}`,
		detail:     "failed_tests is 0, cap 0",
	}, {
		// A REFUSAL IS A VERDICT, not an error. The judging layer's whole job
		// is to have a binary answer, and a caller that could not tell "the
		// project says no" from "the judge could not be reached" would treat a
		// broken judge as a failing tree.
		name:       "a refusal",
		script:     `echo '{"acceptable":false,"thresholds":{"failed_tests":0},"detail":"failed_tests is 3, cap 0"}'`,
		acceptable: false,
		thresholds: `{"failed_tests":0}`,
		detail:     "failed_tests is 3, cap 0",
	}, {
		// Terms that parse and say nothing. A gate whose metrics nothing caps
		// was judged against an empty set, which is a real answer and a
		// different one from having no terms at all.
		name:       "no term applied",
		script:     `echo '{"acceptable":true,"thresholds":{}}'`,
		acceptable: true,
		thresholds: `{}`,
	}, {
		// The exit code is not consulted — a project that exits non-zero on a
		// refusal is doing something reasonable, and reading that as failure
		// would turn a legitimate refusal into an unanswerable judge.
		name:       "printed a verdict and exited 1",
		script:     `echo '{"acceptable":false,"thresholds":{"failed_tests":0},"detail":"failed_tests is 3, cap 0"}'; exit 1`,
		acceptable: false,
		thresholds: `{"failed_tests":0}`,
		detail:     "failed_tests is 3, cap 0",
	}} {
		t.Run(c.name, func(t *testing.T) {
			w, _ := judgeWorktree(t, c.script, 30*time.Second)
			run := measured(flow.GateTested, `{"gate":"tested"}`)
			v, err := w.Judge(context.Background(), run)
			if err != nil {
				t.Fatalf("Judge: %v — the judge answered, so there is a verdict", err)
			}
			if v.Acceptable != c.acceptable {
				t.Errorf("Acceptable = %t, want %t", v.Acceptable, c.acceptable)
			}
			if string(v.Thresholds) != c.thresholds {
				t.Errorf("Thresholds = %q, want %q", v.Thresholds, c.thresholds)
			}
			if v.Detail != c.detail {
				t.Errorf("Detail = %q, want %q", v.Detail, c.detail)
			}
			// The measurement travels with the verdict. Without it nobody can
			// re-run the judge against the same inputs, and a verdict nothing
			// can re-check is as unfalsifiable as a lying runner — which is
			// the property that lets a judge live in the tree it judges.
			if v.Run.Gate != run.Gate || v.Run.Outcome != run.Outcome || string(v.Run.Stdout) != string(run.Stdout) {
				t.Errorf("Run = %+v, want the measurement that was judged (%+v)", v.Run, run)
			}
		})
	}
}

// The envelope reaches the judge on stdin, byte for byte, and it is the one
// the runner observed. The judge does not produce it and could not: an entry
// point that measured its own subject would be the runner, and the runner may
// not come from the tree.
func TestJudge_HandsTheEnvelopeToStdinUnchanged(t *testing.T) {
	requireRealProcesses(t)

	w, dir := judgeWorktree(t, `cat > stdin.seen; echo '`+aVerdict+`'`, 30*time.Second)
	envelope := `{"gate":"tested","metrics":[{"name":"failed_tests","type":"int","value":3}],"note":"  spaces  and \"quotes\" "}`
	if _, err := w.Judge(context.Background(), measured(flow.GateTested, envelope)); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	seen, err := os.ReadFile(filepath.Join(dir, "stdin.seen"))
	if err != nil {
		t.Fatal(err)
	}
	if string(seen) != envelope {
		t.Errorf("the judge read %q on stdin, want %q", seen, envelope)
	}
}

// The exec line is exec'd, never interpreted, and --verdict is appended last.
// A project answers on stdout only when it was given the flag; without it the
// same entry point is in its measuring mode, which spawns a gate.
func TestJudge_ExecsTheEntryPointWithTheNameThenTheFlag(t *testing.T) {
	requireRealProcesses(t)

	for _, gate := range []flow.GateName{flow.GateIntegration, flow.GateTested, "tested:wasm"} {
		t.Run(string(gate), func(t *testing.T) {
			w, dir := judgeWorktree(t,
				": > argv\necho \"$#\" >> argv\nfor a in \"$@\"; do echo \"$a\" >> argv; done\necho '"+aVerdict+"'",
				30*time.Second)
			if _, err := w.Judge(context.Background(), measured(gate, `{}`)); err != nil {
				t.Fatalf("Judge: %v", err)
			}
			recorded, err := os.ReadFile(filepath.Join(dir, "argv"))
			if err != nil {
				t.Fatal(err)
			}
			// The instance travels whole, and the count pins it: splitting
			// "tested:wasm" would ask the judge about the concept and get the
			// terms for every suite under it.
			want := "2\n" + string(gate) + "\n--verdict\n"
			if string(recorded) != want {
				t.Errorf("the judge saw argv %q, want %q", recorded, want)
			}
		})
	}
}

// One JSON object with both required fields, and nothing else. Everything here
// is a judge that did not answer — never a refusal.
func TestJudge_RefusesAnythingThatIsNotAVerdict(t *testing.T) {
	requireRealProcesses(t)

	for _, c := range []struct {
		name   string
		script string
		says   string
	}{{
		name:   "nothing at all",
		script: `exit 0`,
	}, {
		name:   "not JSON",
		script: `echo 'looks fine to me'`,
	}, {
		name:   "an array",
		script: `echo '[{"acceptable":true,"thresholds":{}}]'`,
	}, {
		name:   "a scalar",
		script: `echo 7`,
	}, {
		// The one scalar a struct decodes from without complaint: null leaves
		// every field at its zero value, and Acceptable's zero value is a
		// refusal nobody made.
		name:   "null",
		script: `echo null`,
	}, {
		name:   "two verdicts are not one verdict",
		script: `printf '{"acceptable":true,"thresholds":{}}{"acceptable":false,"thresholds":{}}\n'`,
	}, {
		// Stdout carries the verdict and nothing else, for the same reason it
		// carries the envelope and nothing else.
		name:   "a verdict with something after it",
		script: `printf '{"acceptable":true,"thresholds":{}}\ndone\n'`,
	}, {
		// The load-bearing one. Absent, "acceptable" would decode to false and
		// the SDK would have invented a refusal out of a judge that gave none.
		name:   "no acceptable field",
		script: `echo '{"thresholds":{"failed_tests":0},"detail":"looks fine"}'`,
		says:   "acceptable",
	}, {
		name:   "acceptable is not a boolean",
		script: `echo '{"acceptable":"yes","thresholds":{}}'`,
		says:   "acceptable",
	}, {
		// A verdict whose terms were discarded cannot be re-checked by anyone
		// who was not there, which is exactly what a judge is allowed to be a
		// tree artifact in exchange for.
		name:   "no thresholds field",
		script: `echo '{"acceptable":true,"detail":"looks fine"}'`,
		says:   "thresholds",
	}, {
		// Null terms are discarded terms wearing a present field. Nothing
		// tells them from the judge that forgot, and a verdict carrying
		// "null" is exactly the unrecomputable one the required field exists
		// to prevent — so it is refused where an absent field is.
		name:   "thresholds is null",
		script: `echo '{"acceptable":true,"thresholds":null,"detail":"looks fine"}'`,
		says:   "thresholds",
	}} {
		t.Run(c.name, func(t *testing.T) {
			w, _ := judgeWorktree(t, c.script, 30*time.Second)
			v, err := w.Judge(context.Background(), measured(flow.GateTested, `{"gate":"tested"}`))
			assertNoVerdict(t, v, err)
			if c.says != "" && !strings.Contains(err.Error(), c.says) {
				t.Errorf("err = %v, want it to name %q — the person fixing the judge needs the field", err, c.says)
			}
		})
	}
}

// Only a measured run may be judged. The other four outcomes are not verdicts
// and must never be passed off as one: a gate that could not start, timed out,
// died or broke its contract has not reported that the tree is bad, and
// handing that to a judge would ask it to invent an answer about a measurement
// that does not exist.
func TestJudge_RefusesARunThatMeasuredNothingWithoutSpawning(t *testing.T) {
	requireRealProcesses(t)

	for _, outcome := range []flow.GateOutcome{
		flow.OutcomeTimedOut,
		flow.OutcomeCouldNotStart,
		flow.OutcomeDied,
		flow.OutcomeBrokeContract,
		"", // the zero GateRun, which says nothing was observed at all
	} {
		t.Run(string("outcome "+outcome), func(t *testing.T) {
			w, dir := judgeWorktree(t, "touch asked\necho '"+aVerdict+"'", 30*time.Second)
			run := flow.GateRun{Gate: flow.GateTested, Outcome: outcome, ExitCode: -1}
			v, err := w.Judge(context.Background(), run)
			assertNoVerdict(t, v, err)
			// The refusal names what WAS observed, because that is the fact
			// the person reading it has to act on — a timeout is the host's
			// problem and a broken contract is the gate author's. A zero run
			// observed nothing at all, and saying so beats a sentence with a
			// hole where the outcome should be.
			says := string(outcome)
			if says == "" {
				says = "no outcome at all"
			}
			if !strings.Contains(err.Error(), says) {
				t.Errorf("err = %v, want it to name what was observed (%q)", err, says)
			}
			if _, err := os.Stat(filepath.Join(dir, "asked")); err == nil {
				t.Error("the judge was asked about a run that measured nothing")
			}
		})
	}
}

// An undeclared concept is refused before anything is spawned, exactly as the
// runner refuses it: the project cannot answer for a name it does not have,
// and the failure would look like the judge refusing rather than the caller
// asking about something that does not exist.
func TestJudge_RefusesAnUndeclaredNameWithoutSpawning(t *testing.T) {
	requireRealProcesses(t)

	w, dir := judgeWorktree(t, "touch asked\necho '"+aVerdict+"'", 30*time.Second)
	v, err := w.Judge(context.Background(), measured("lint", `{"gate":"lint"}`))
	assertNoVerdict(t, v, err)
	if !strings.Contains(err.Error(), "not a declared gate name") {
		t.Errorf("err = %v, want a refusal naming the problem", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "asked")); err == nil {
		t.Error("asked the judge about an undeclared gate name")
	}
}

// THE PROPERTY THE WHOLE SPLIT RESTS ON. Judge does not run the gate: the SDK
// spawned it, and the judge is handed the envelope that came back. A judging
// entry point that measured its own subject would be the runner — and the
// runner is the one party that may not come from the tree, because it is the
// sole witness to a process that no longer exists.
func TestJudge_DoesNotRunTheGate(t *testing.T) {
	requireRealProcesses(t)

	w, dir := judgeWorktree(t, "echo '"+aVerdict+"'", 30*time.Second)
	// A gate that is right there, executable, and would be found by anything
	// that went looking for it.
	writeScript(t, dir, "gate", "touch measured\necho '{\"gate\":\"tested\"}'", 0o755)

	v, err := w.Judge(context.Background(), measured(flow.GateTested, `{"gate":"tested"}`))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if !v.Acceptable {
		t.Error("Acceptable = false, want the verdict the judge printed")
	}
	if _, err := os.Stat(filepath.Join(dir, "measured")); err == nil {
		t.Error("the gate ran: the judging entry point took the runner's seat")
	}
}

// A judge that is not there, or not executable, has not refused anything. This
// is the case an error return exists for, and the one most expensive to read
// as a verdict: every change would be refused for a reason that is nowhere in
// the change.
func TestJudge_AnUnreachableEntryPointIsNotARefusal(t *testing.T) {
	requireRealProcesses(t)

	for _, c := range []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{{
		name:  "no judging entry point in the tree",
		setup: func(*testing.T, string) {},
	}, {
		name: "present but not executable",
		setup: func(t *testing.T, dir string) {
			writeScript(t, dir, "run", "echo '"+aVerdict+"'", 0o644)
		},
	}} {
		t.Run(c.name, func(t *testing.T) {
			w, dir := judgeWorktree(t, "", 30*time.Second)
			c.setup(t, dir)
			v, err := w.Judge(context.Background(), measured(flow.GateTested, `{"gate":"tested"}`))
			assertNoVerdict(t, v, err)
			if !strings.Contains(err.Error(), "bin/run") {
				t.Errorf("err = %v, want it to name the program that was absent", err)
			}
		})
	}
}

// A wedged judge must not hang the SDK, and it must not answer either.
func TestJudge_EnforcesTheDeclaredTimeout(t *testing.T) {
	requireRealProcesses(t)

	// exec, so the shell does not linger as the sleep's parent.
	w, _ := judgeWorktree(t, "exec sleep 60", 100*time.Millisecond)
	start := time.Now()
	v, err := w.Judge(context.Background(), measured(flow.GateTested, `{"gate":"tested"}`))
	elapsed := time.Since(start)
	assertNoVerdict(t, v, err)
	if elapsed > 30*time.Second {
		t.Errorf("returned after %s — the declared timeout did not bound the wait", elapsed)
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("err = %v, want it to name the wait as the problem", err)
	}
}

// The caller going away is not an answer: nobody is waiting for the verdict,
// and there is none.
func TestJudge_ACancelledCallerIsAnError(t *testing.T) {
	requireRealProcesses(t)

	w, _ := judgeWorktree(t, "exec sleep 60", 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	v, err := w.Judge(ctx, measured(flow.GateTested, `{"gate":"tested"}`))
	assertNoVerdict(t, v, err)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}

func TestJudge_RefusesACallerWhoseContextIsAlreadyGone(t *testing.T) {
	requireRealProcesses(t)

	w, dir := judgeWorktree(t, "touch asked\necho '"+aVerdict+"'", 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, err := w.Judge(ctx, measured(flow.GateTested, `{"gate":"tested"}`))
	assertNoVerdict(t, v, err)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "asked")); err == nil {
		t.Error("the judge was spawned for a caller that had already gone away")
	}
}

// A judge is free to answer without reading the envelope — it may know the
// gate's terms make the numbers irrelevant, or it may simply be sloppy. Either
// way the SDK is left holding a pipe nobody is draining, and an envelope large
// enough to fill it must not wedge the caller.
//
// 128 KiB is past every common pipe buffer, so the write genuinely blocks and
// the process genuinely exits underneath it.
func TestJudge_AnswersEvenIfTheJudgeNeverReadsStdin(t *testing.T) {
	requireRealProcesses(t)

	w, _ := judgeWorktree(t, "echo '"+aVerdict+"'", 30*time.Second)
	envelope := `{"gate":"tested","padding":"` + strings.Repeat("x", 128*1024) + `"}`

	start := time.Now()
	v, err := w.Judge(context.Background(), measured(flow.GateTested, envelope))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Judge: %v — the judge answered; that it ignored stdin is its business", err)
	}
	if !v.Acceptable {
		t.Error("Acceptable = false, want the verdict the judge printed")
	}
	if elapsed > 20*time.Second {
		t.Errorf("returned after %s — the SDK waited on a pipe the judge had stopped reading", elapsed)
	}
}

// The judge's stderr is CAPTURED and reaches the error, which is the opposite
// of a gate's. A gate's stderr is the minutes of a long run and a person needs
// it as it happens; a judge that could not answer wrote the reason there, and
// the reason belongs with the error or it is lost.
func TestJudge_TheJudgesOwnComplaintReachesTheError(t *testing.T) {
	requireRealProcesses(t)

	w, _ := judgeWorktree(t, "echo 'bin/run: the thresholds manifest is missing' >&2\nexit 3", 30*time.Second)
	v, err := w.Judge(context.Background(), measured(flow.GateTested, `{"gate":"tested"}`))
	assertNoVerdict(t, v, err)
	if !strings.Contains(err.Error(), "the thresholds manifest is missing") {
		t.Errorf("err = %v, want it to carry what the judge said on stderr", err)
	}
}

// A judge that printed NOTHING and a judge that printed the WRONG THING are
// different problems, and the error says which. The first sends a person to
// find out why it stopped — so the exit status it stopped with is in the
// message — and the second sends them to look at what it printed. One wording
// for both costs the reader the only clue they get, and neither is a refusal.
func TestJudge_SaysWhetherTheVerdictWasAbsentOrMalformed(t *testing.T) {
	requireRealProcesses(t)

	for _, c := range []struct {
		name   string
		script string
		says   string
		notSay string
	}{{
		// Absence. The exit status is the whole account of what became of the
		// judge, so it is reported as the number it was rather than as
		// "non-zero" — the person debugging their judging entry point is the
		// one who knows what 7 means in it.
		name:   "silent, and exited 7",
		script: `exit 7`,
		says:   "exited 7 without printing a verdict",
	}, {
		// Still absence: an object that stops mid-field was never finished, so
		// there is nothing to look at in what it printed.
		name:   "cut off mid-verdict",
		script: `printf '{"acceptable":tr'`,
		says:   "without printing a verdict",
	}, {
		// A defect in what it printed, not in what became of it.
		name:   "printed something else entirely",
		script: `echo 'looks fine to me'`,
		says:   "printed something that is not a verdict",
		notSay: "without printing a verdict",
	}} {
		t.Run(c.name, func(t *testing.T) {
			w, _ := judgeWorktree(t, c.script, 30*time.Second)
			v, err := w.Judge(context.Background(), measured(flow.GateTested, `{"gate":"tested"}`))
			assertNoVerdict(t, v, err)
			if !strings.Contains(err.Error(), c.says) {
				t.Errorf("err = %v, want it to say %q", err, c.says)
			}
			if c.notSay != "" && strings.Contains(err.Error(), c.notSay) {
				t.Errorf("err = %v, want it NOT to say %q — the judge printed something, and that is where to look", err, c.notSay)
			}
		})
	}
}

// A judge may write to stderr and still answer: entry points log, and a project
// that traced where it read its thresholds from has not failed to judge. The
// stderr is held for the error path only, so a verdict that arrived is returned
// whole and the noise reaches neither the answer nor the caller.
func TestJudge_AJudgeThatAlsoWritesToStderrStillAnswers(t *testing.T) {
	requireRealProcesses(t)

	w, _ := judgeWorktree(t, "echo 'run: reading thresholds from the manifest' >&2\necho '"+aVerdict+"'", 30*time.Second)
	v, err := w.Judge(context.Background(), measured(flow.GateTested, `{"gate":"tested"}`))
	if err != nil {
		t.Fatalf("Judge: %v — the judge printed a verdict; what it logged is its business", err)
	}
	if !v.Acceptable {
		t.Error("Acceptable = false, want the verdict the judge printed")
	}
	if strings.Contains(v.Detail, "reading thresholds") {
		t.Errorf("Detail = %q, want only what the judge put in the verdict", v.Detail)
	}
}

// A judge that prints far more than a verdict should not exhaust the SDK.
// The captured stdout is truncated to the bound, and the verdict still
// arrives if it fits within the prefix.
func TestAskJudge_BoundsOutputCapture(t *testing.T) {
	requireRealProcesses(t)

	// A valid verdict followed by enough padding to exceed the bound. The
	// verdict itself is small; the padding is what a defective judge might
	// produce. The trailing content makes this not-a-verdict (trailing content
	// after the object), which is the correct refusal — the point is that the
	// SDK survives to report it.
	blocks := (maxVerdictOutput / 1024) + 10
	script := fmt.Sprintf(
		`echo '%s'; dd if=/dev/zero bs=1024 count=%d 2>/dev/null`,
		aVerdict, blocks,
	)
	w, _ := judgeWorktree(t, script, 30*time.Second)
	v, err := w.Judge(context.Background(), measured(flow.GateTested, `{"gate":"tested"}`))
	assertNoVerdict(t, v, err)
	if !strings.Contains(err.Error(), "not a verdict") {
		t.Errorf("err = %v, want it to name the problem (trailing content)", err)
	}
}
