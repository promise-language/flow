package flow

// GateOutcome is what a RUNNER observed of one gate process. It is not a
// verdict and not a number the gate chose: the gate measures, the runner
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
type GateOutcome string

const (
	// OutcomeMeasured — the process completed and printed a valid envelope.
	// Nobody's problem yet: whether the numbers are acceptable is a question
	// for whoever holds the thresholds, and neither the gate nor the runner
	// holds them.
	OutcomeMeasured GateOutcome = "measured"

	// OutcomeTimedOut — killed at the declared timeout. The problem is the
	// wait, or the host. It is not the change, and it is the one outcome a
	// retry can resolve.
	OutcomeTimedOut GateOutcome = "timed_out"

	// OutcomeCouldNotStart — the program the exec line names is absent or not
	// executable, so nothing ran. The problem belongs to whoever declared the
	// gate, or whoever delivered the tree.
	//
	// This must never fold into OutcomeDied, which carries "retry is correct":
	// a retry loop pointed at a missing binary never terminates and reads as a
	// flaky host for as long as anyone lets it run.
	OutcomeCouldNotStart GateOutcome = "could_not_start"

	// OutcomeDied — killed by a signal, or exited without printing a readable
	// envelope. The problem is the host. Silence is absence, not a malformed
	// envelope, so a truncated envelope lands here and not on
	// OutcomeBrokeContract.
	OutcomeDied GateOutcome = "died"

	// OutcomeBrokeContract — the gate printed something that is not an
	// envelope. The problem is in the gate's own code, which is a different
	// repository from an absent program and a different one again from the
	// change under measurement.
	OutcomeBrokeContract GateOutcome = "broke_contract"
)

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
	Outcome GateOutcome

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
