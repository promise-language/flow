package flow

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestResolveBudget_OverlaysOnDefaults(t *testing.T) {
	got := ResolveStepBudget(StepBudget{MaxInvocations: 7})
	want := DefaultStepBudget()
	want.MaxInvocations = 7
	if got != want {
		t.Errorf("ResolveStepBudget(MaxInvocations=7) = %+v, want %+v", got, want)
	}
}

func TestResolveBudget_PartialOverlay(t *testing.T) {
	got := ResolveStepBudget(StepBudget{
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
	if ResolveStepBudget(StepBudget{}) != DefaultStepBudget() {
		t.Errorf("ResolveStepBudget(zero) should equal DefaultStepBudget()")
	}
}

func TestDefaultStepBudget_HasExpectedValues(t *testing.T) {
	d := DefaultStepBudget()
	if d.MaxInvocations != 3 || d.MaxPromptsPerInvocation != 50 || d.MaxCostUSD != 20 || d.Timeout != 30*time.Minute {
		t.Errorf("defaults = %+v, want {3,50,20,30m}", d)
	}
}

// TestREADME_BudgetDefaultsMatchCode reads the step-budget table and the
// inline summary in README.md and checks every stated default against
// DefaultStepBudget(). This is the test that would have caught #96.
func TestREADME_BudgetDefaultsMatchCode(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Skipf("README.md not readable: %v", err)
	}
	body := string(readme)
	d := DefaultStepBudget()

	// --- Table rows ---
	// | invocations | `3` | ...
	// | prompts / invocation | `50` | ...
	// | cost (USD) | `$20` | ...
	// | timeout | `30m` | ...
	tableRow := regexp.MustCompile(`\|\s*([^|]+?)\s*\|\s*` + "`" + `([^` + "`" + `]+)` + "`" + `\s*\|`)
	matches := tableRow.FindAllStringSubmatch(body, -1)

	found := map[string]string{}
	for _, m := range matches {
		axis := strings.TrimSpace(m[1])
		val := strings.TrimSpace(m[2])
		switch {
		case axis == "invocations":
			found["invocations"] = val
		case strings.Contains(axis, "prompts"):
			found["prompts"] = val
		case strings.Contains(axis, "cost"):
			found["cost"] = val
		case axis == "timeout":
			found["timeout"] = val
		}
	}

	check := func(label, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("README table %s = %q, budget.go = %q", label, got, want)
		}
	}
	check("invocations", found["invocations"], strconv.Itoa(d.MaxInvocations))
	check("prompts/invocation", found["prompts"], strconv.Itoa(d.MaxPromptsPerInvocation))
	check("cost", found["cost"], fmt.Sprintf("$%g", d.MaxCostUSD))
	check("timeout", found["timeout"], "30m")

	// --- Inline summaries: `{3, 50, $20, 30m}` (appears twice) ---
	summaryRe := regexp.MustCompile("`" + `\{(\d+),\s*(\d+),\s*\$(\d+),\s*(\d+m)\}` + "`")
	allSm := summaryRe.FindAllStringSubmatch(body, -1)
	if len(allSm) == 0 {
		t.Fatal("inline budget summary `{N, N, $N, Nm}` not found in README.md")
	}
	for i, sm := range allSm {
		prefix := fmt.Sprintf("inline[%d]", i)
		check(prefix+" invocations", sm[1], strconv.Itoa(d.MaxInvocations))
		check(prefix+" prompts", sm[2], strconv.Itoa(d.MaxPromptsPerInvocation))
		check(prefix+" cost", sm[3], strconv.FormatFloat(d.MaxCostUSD, 'f', -1, 64))
		check(prefix+" timeout", sm[4], "30m")
	}
}

// --- EffectiveBudget ---
//
// The ONE place a cap is computed now that nothing seeds caps onto an item: the
// binary's policy, resolved against the package defaults, plus the extensions
// the ledger row records. The gate that refuses a dispatch and the `grant` that
// tops it up read this and nothing else, so they cannot disagree.

func TestEffectiveBudget_NoGrantsIsThePolicyResolved(t *testing.T) {
	base := StepBudget{MaxInvocations: 5}
	got := EffectiveBudget(base, LedgerRow{Step: "plan"})
	want := ResolveStepBudget(base)
	if got != want {
		t.Errorf("EffectiveBudget with no grants = %+v, want the resolved policy %+v", got, want)
	}
}

// An unfunded step — no policy of its own and no grants — is the package
// defaults whole, not a set of zero caps.
func TestEffectiveBudget_UnfundedStepIsTheDefaults(t *testing.T) {
	got := EffectiveBudget(StepBudget{}, LedgerRow{})
	if got != DefaultStepBudget() {
		t.Errorf("EffectiveBudget of an unfunded step = %+v, want the defaults %+v", got, DefaultStepBudget())
	}
}

func TestEffectiveBudget_OneAxisExtended(t *testing.T) {
	base := StepBudget{MaxInvocations: 3, MaxPromptsPerInvocation: 50, MaxCostUSD: 20, Timeout: 30 * time.Minute}
	row := LedgerRow{Step: "plan", Granted: []GrantRecord{{Axis: AxisInvocations, Amount: 2}}}
	got := EffectiveBudget(base, row)
	if got.MaxInvocations != 5 {
		t.Errorf("MaxInvocations = %d, want 5 (3 policy + 2 granted)", got.MaxInvocations)
	}
	// Every other axis is untouched: a grant raises what it names.
	if got.MaxCostUSD != 20 || got.MaxPromptsPerInvocation != 50 || got.Timeout != 30*time.Minute {
		t.Errorf("a grant on invocations moved another axis: %+v", got)
	}
}

func TestEffectiveBudget_EveryAxisExtended(t *testing.T) {
	base := StepBudget{MaxInvocations: 3, MaxPromptsPerInvocation: 50, MaxCostUSD: 20, Timeout: 30 * time.Minute}
	row := LedgerRow{Step: "plan", Granted: []GrantRecord{
		{Axis: AxisInvocations, Amount: 2},
		{Axis: AxisPrompts, Amount: 10},
		{Axis: AxisCost, Amount: 5.50},
		// Timeout grants are recorded in SECONDS, per GrantRecord; the
		// conversion to a Duration happens here and nowhere else.
		{Axis: AxisTimeout, Amount: 900},
	}}
	want := StepBudget{
		MaxInvocations:          5,
		MaxPromptsPerInvocation: 60,
		MaxCostUSD:              25.50,
		Timeout:                 45 * time.Minute,
	}
	if got := EffectiveBudget(base, row); got != want {
		t.Errorf("EffectiveBudget = %+v, want %+v", got, want)
	}
}

// Repeated grants on one axis accumulate — GrantedOn sums them, so two $5
// grants are $10 of headroom and not the larger of the two.
func TestEffectiveBudget_RepeatedGrantsAccumulate(t *testing.T) {
	row := LedgerRow{Step: "plan", Granted: []GrantRecord{
		{Axis: AxisCost, Amount: 5},
		{Axis: AxisCost, Amount: 5},
	}}
	got := EffectiveBudget(StepBudget{MaxCostUSD: 20}, row)
	if got.MaxCostUSD != 30 {
		t.Errorf("MaxCostUSD = %v, want 30 (20 policy + 5 + 5)", got.MaxCostUSD)
	}
}

// A sub-second timeout grant is not silently truncated to nothing: the amount
// is scaled as a float, the way AxisReport.Format does for the same reason.
func TestEffectiveBudget_FractionalTimeoutGrant(t *testing.T) {
	row := LedgerRow{Step: "plan", Granted: []GrantRecord{{Axis: AxisTimeout, Amount: 0.5}}}
	got := EffectiveBudget(StepBudget{Timeout: time.Second}, row)
	if got.Timeout != 1500*time.Millisecond {
		t.Errorf("Timeout = %v, want 1.5s", got.Timeout)
	}
}
