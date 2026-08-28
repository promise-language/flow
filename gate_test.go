package flow

import "testing"

// The five outcome strings are a WIRE CONTRACT shared with base, not flow's own
// spelling. A silent change to one of them is a break that only shows up in
// another repository, so they are pinned here as literals rather than derived
// from the constants they name.
//
// Pinning the set as well as the values: a sixth outcome is not an addition, it
// is a change to a vocabulary something else reads.
func TestGateOutcomes_AreTheDeclaredWireSpelling(t *testing.T) {
	for _, c := range []struct {
		outcome GateOutcome
		wire    string
	}{
		{OutcomeMeasured, "measured"},
		{OutcomeTimedOut, "timed_out"},
		{OutcomeCouldNotStart, "could_not_start"},
		{OutcomeDied, "died"},
		{OutcomeBrokeContract, "broke_contract"},
	} {
		if string(c.outcome) != c.wire {
			t.Errorf("outcome = %q, want %q", c.outcome, c.wire)
		}
	}
}

// The zero GateRun says nothing was observed. Callers get one back beside an
// error — the request the runner could not attempt — and must not be able to
// read a measurement out of it.
func TestGateRun_ZeroValueIsNotAnOutcome(t *testing.T) {
	var run GateRun
	if run.Outcome != "" {
		t.Errorf("zero GateRun.Outcome = %q, want the empty outcome", run.Outcome)
	}
	if run.Outcome == OutcomeMeasured {
		t.Error("a zero GateRun reads as a measurement")
	}
}
