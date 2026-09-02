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
