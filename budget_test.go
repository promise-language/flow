package flow

import (
	"testing"
	"time"
)

func TestResolveBudget_OverlaysOnDefaults(t *testing.T) {
	got := resolveBudget(StepBudget{MaxInvocations: 7})
	want := DefaultStepBudget()
	want.MaxInvocations = 7
	if got != want {
		t.Errorf("resolveBudget(MaxInvocations=7) = %+v, want %+v", got, want)
	}
}

func TestResolveBudget_PartialOverlay(t *testing.T) {
	got := resolveBudget(StepBudget{
		MaxPromptsPerInvocation: 5,
		Timeout:                 10 * time.Minute,
	})
	if got.MaxPromptsPerInvocation != 5 {
		t.Errorf("MaxPromptsPerInvocation = %d, want 5", got.MaxPromptsPerInvocation)
	}
	if got.Timeout != 10*time.Minute {
		t.Errorf("Timeout = %v, want 10m", got.Timeout)
	}
	// untouched axes inherit defaults
	if got.MaxInvocations != DefaultStepBudget().MaxInvocations {
		t.Errorf("MaxInvocations = %d, want default %d", got.MaxInvocations, DefaultStepBudget().MaxInvocations)
	}
	if got.MaxCostUSD != DefaultStepBudget().MaxCostUSD {
		t.Errorf("MaxCostUSD = %v, want default %v", got.MaxCostUSD, DefaultStepBudget().MaxCostUSD)
	}
}

func TestResolveBudget_EmptyMatchesDefault(t *testing.T) {
	if resolveBudget(StepBudget{}) != DefaultStepBudget() {
		t.Errorf("resolveBudget(zero) should equal DefaultStepBudget()")
	}
}

func TestDefaultStepBudget_HasExpectedValues(t *testing.T) {
	d := DefaultStepBudget()
	if d.MaxInvocations != 3 || d.MaxPromptsPerInvocation != 1 || d.MaxCostUSD != 10 || d.Timeout != 30*time.Minute {
		t.Errorf("defaults = %+v, want {3,1,10,30m}", d)
	}
}
