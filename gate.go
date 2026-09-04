package flow

import "slices"

// Outcome is what a RUNNER observed of one gate or command process. It is not
// a verdict and not a number the gate chose: the gate measures, the runner
// spawns it and watches what became of it, and something else judges the
// measurement against thresholds the gate cannot reach.
//
// The set is CLOSED, and it is not flow's. It is a wire contract shared with
// base — the SDK reads it in every project, so it has to mean the same thing
// everywhere. See docs/gates-and-commands.md § "What the runner reports", and
// base's docs/gate-contract.md, which is authoritative where the two differ.
//
// Only OutcomeTimedOut is worth retrying unchanged. The other three failures
// all recur — but they stay distinct anyway, because they are owned by three
// different people and collapsing them attributes a failure to the wrong
// repository.
type Outcome string

const (
	// OutcomeMeasured — the process completed and printed a valid envelope.
	// Nobody's problem yet: whether the numbers are acceptable is a question
	// for whoever holds the thresholds, and neither the gate nor the runner
	// holds them.
	OutcomeMeasured Outcome = "measured"

	// OutcomeTimedOut — killed at the declared timeout. The problem is the
	// wait, or the host. It is not the change, and it is the one outcome a
	// retry can resolve.
	OutcomeTimedOut Outcome = "timed_out"

	// OutcomeCouldNotStart — the program the exec line names is absent or not
	// executable, so nothing ran. The problem belongs to whoever declared the
	// gate, or whoever delivered the tree.
	//
	// This must never fold into OutcomeDied, which carries "retry is correct":
	// a retry loop pointed at a missing binary never terminates and reads as a
	// flaky host for as long as anyone lets it run.
	OutcomeCouldNotStart Outcome = "could_not_start"

	// OutcomeDied — killed by a signal, or exited without printing a readable
	// envelope. The problem is the host, not the gate's own code.
	//
	// The envelope is written whole, so ABSENCE AND TRUNCATION are one case and
	// both land here: silence is absence, not a malformed envelope, and a
	// truncated envelope is a stream that stopped because its writer stopped
	// existing. Neither is OutcomeBrokeContract, which is what a gate that
	// finished printing chose to print.
	OutcomeDied Outcome = "died"

	// OutcomeBrokeContract — the gate broke the protocol it runs under: it
	// finished printing something that is still not an envelope, or it MODIFIED
	// WHAT IT MEASURED. The problem is in the gate's own code, which is a
	// different repository from an absent program and a different one again from
	// the change under measurement.
	//
	// A truncated envelope is NOT this case, though it is also not an envelope:
	// a stream that stops mid-value is a writer that stopped existing, which is
	// the host's failure and lands on OutcomeDied. Attributing it here hands a
	// dead machine to the gate's author.
	//
	// A gate that modifies is a protocol violation, not a gate with a side
	// effect: the state it reported on no longer exists, so nothing can
	// reproduce the answer and no decision may rest on it. The result is not a
	// poor measurement, it is NO measurement — which is why it lands here and
	// is never judged.
	OutcomeBrokeContract Outcome = "broke_contract"
)

// AllOutcomes returns every declared outcome, in declaration order.
//
// A consumer enumerates it rather than mirroring the set, which is how two
// copies of one vocabulary drift — the same reason AllOrigins and
// AllDisclosureActs exist. An orchestrator proving it refuses every unjudgeable
// outcome, or a runner proving it has an answer for each, should be able to
// ask rather than restate: a hand-written list is the thing that goes stale
// when a member is added, and it goes stale silently.
func AllOutcomes() []Outcome {
	return []Outcome{
		OutcomeMeasured, OutcomeTimedOut, OutcomeCouldNotStart, OutcomeDied, OutcomeBrokeContract,
	}
}

// Valid reports whether o is a declared outcome. The empty string is not one:
// a run with no outcome is a run the runner never classified.
func (o Outcome) Valid() bool { return slices.Contains(AllOutcomes(), o) }

// GateRun is a runner's account of one gate process: what it observed, plus
// the raw diagnostics a person debugging the gate wants.
//
// Outcome is the field decisions are made on. ExitCode is NOT — it is carried
// because it is the only place the kernel's number survives, and a gate never
// chose it in the states that matter most.
type GateRun struct {
	// Gate is the name that was asked for.
	Gate GateName

	// Outcome is what the runner observed. Always set when the runner returned
	// no error.
	Outcome Outcome

	// ExitCode is the gate's own exit status, kept as a raw diagnostic and
	// decided on by nothing. It is -1 exactly where the kernel has no number to
	// give — the program never started, or the process was terminated by a
	// signal — which is what (*os.ProcessState).ExitCode() already reports.
	ExitCode int

	// Stdout is what the gate printed on stdout: the envelope when Outcome is
	// OutcomeMeasured, and whatever it printed instead when it is not. Stderr
	// is not here — it is passed straight through to the reader's own stream
	// rather than accumulated, so someone watching a long gate sees it work.
	Stdout []byte

	// Detail is the runner's account for a person: which signal, which program
	// was absent, where the envelope stopped parsing. It is prose and nothing
	// keys on it.
	Detail string
}

// GateVerdict is a JUDGING layer's answer about one measurement: whether the
// numbers a gate reported are acceptable to the project that holds the
// thresholds.
//
// A judging layer IS entitled to a binary answer, unlike a gate and unlike a
// runner. The disjoint-channel rule exists because a gate has no verdict to
// give; the judge's whole job is to have one.
//
// It carries the run it was reached from because a verdict handed over with
// its inputs discarded is exactly as unfalsifiable as a lying runner. What
// lets a judge live in the tree it judges is that anyone can re-run it against
// the same envelope and the same thresholds and get the same answer — and that
// is only true if both travel with the verdict. See
// docs/gates-and-commands.md § "The runner comes from outside the tree".
type GateVerdict struct {
	// Run is the measurement this verdict is about, as the runner reported it.
	// Its Stdout is the envelope the judge was handed; the judge did not
	// produce it, and could not have.
	//
	// Only a run whose Outcome is OutcomeMeasured may be judged. The other
	// four are not verdicts and must never be passed off as one: a run that
	// measured nothing has not reported that the tree is bad.
	Run GateRun

	// Acceptable is the answer. It is the project's, computed from thresholds
	// the SDK never sees.
	Acceptable bool

	// Thresholds are the terms the measurement was judged against, as the
	// judge stated them — opaque to the SDK, which does not read a project's
	// numbers. They are carried so the verdict can be recomputed by whoever
	// was not there, which is the whole reason a judge may be a tree artifact.
	Thresholds []byte

	// Detail is the reason a person needs: which metric, its value, the term
	// it was judged against. It is prose and nothing keys on it.
	Detail string
}
