package github

import (
	"testing"
	"time"

	"github.com/promise-language/flow"
)

// A gate's timeout is the only thing standing between a wedged gate and a
// runner that never returns, and a zero duration is not "no deadline" here —
// it is a deadline that has already expired. Left unfilled, every gate in
// every project reports OutcomeTimedOut without a process ever being spawned,
// and the outcome that means "retry this unchanged" is handed out for a
// configuration that will produce it again forever.
func TestConfigGateTimeoutIsFilledIn(t *testing.T) {
	got := Config{}.withDefaults()
	if got.GateTimeout <= 0 {
		t.Fatalf("GateTimeout = %v — an unset timeout is one that has already expired", got.GateTimeout)
	}

	// The declared value is a project's to set, and withDefaults must not
	// overwrite one that was.
	declared := Config{GateTimeout: 3 * time.Second}.withDefaults()
	if declared.GateTimeout != 3*time.Second {
		t.Errorf("GateTimeout = %v, want the declared 3s", declared.GateTimeout)
	}
}

// The default sits UNDER the step's own budget, which is what makes a wedged
// gate get caught by its own deadline and reported as OutcomeTimedOut. Raise
// it past the step timeout and the step dies first: the gate then has no
// outcome at all, and the failure is attributed to the step rather than to the
// wait it was actually waiting on.
func TestConfigGateTimeoutFitsInsideAStep(t *testing.T) {
	gate := Config{}.withDefaults().GateTimeout
	step := flow.DefaultStepBudget().Timeout
	if gate >= step {
		t.Errorf("default GateTimeout %v >= default step timeout %v — the step dies before the gate's own deadline fires", gate, step)
	}
}
