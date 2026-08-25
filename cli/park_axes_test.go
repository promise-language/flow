package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
)

// axisOf pulls one axis out of a park's report.
func axisOf(t *testing.T, park *flow.ParkRequest, axis flow.BudgetAxis) flow.AxisReport {
	t.Helper()
	if park == nil {
		t.Fatalf("no park recorded")
	}
	for _, a := range park.Axes {
		if a.Axis == axis {
			return a
		}
	}
	t.Fatalf("park reported no %s axis; got %+v", axis, park.Axes)
	return flow.AxisReport{}
}

// ---------------------------------------------------------------------------
// A budget park reports every axis, not just the one that tripped.
// ---------------------------------------------------------------------------

func TestParkReportsEveryAxis(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("flaky", "plan", func(ctx flow.StepCtx) error {
			return errors.New("boom")
		}, flow.StepConfig{Budget: flow.StepBudget{
			MaxInvocations:          1,
			MaxPromptsPerInvocation: 2,
			MaxCostUSD:              10,
			Timeout:                 30 * time.Minute,
		}})
	}, &stubAgent{name: "stub"})

	// Burn the only invocation, then park on it.
	if _, err := RunOne(context.Background(), app, claim); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Status != "parked" || res.Park == nil {
		t.Fatalf("res = %+v, want parked", res)
	}
	// The headline axis is unchanged...
	if res.Park.Axis != flow.AxisInvocations {
		t.Errorf("Axis = %q, want invocations", res.Park.Axis)
	}
	// ...but all four are now reported.
	if len(res.Park.Axes) != 4 {
		t.Fatalf("Axes = %+v, want all four axes", res.Park.Axes)
	}
	for _, want := range []flow.BudgetAxis{
		flow.AxisInvocations, flow.AxisPrompts, flow.AxisCost, flow.AxisTimeout,
	} {
		axisOf(t, res.Park, want)
	}

	inv := axisOf(t, res.Park, flow.AxisInvocations)
	if inv.Used != 1 || inv.Granted != 1 || !inv.Exhausted {
		t.Errorf("invocations = %+v, want 1/1 exhausted", inv)
	}
	// An axis with headroom is reported too — and is NOT flagged flat.
	cost := axisOf(t, res.Park, flow.AxisCost)
	if cost.Granted != 10 || cost.Exhausted {
		t.Errorf("cost = %+v, want cap 10 and not exhausted", cost)
	}
	to := axisOf(t, res.Park, flow.AxisTimeout)
	if to.Granted != (30 * time.Minute).Seconds() {
		t.Errorf("timeout granted = %v, want 1800s", to.Granted)
	}
}

// The regression the issue is really about: a timed-out run burns an
// invocation on its way out, so by the time the operator reads the timeout
// park the invocations axis is flat too. The report has to say so, or granting
// time alone buys a dispatch that re-parks on invocations.
func TestTimeoutParkReportsInvocationsAsFlat(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("slow", "plan", func(ctx flow.StepCtx) error {
			<-ctx.Context().Done()
			return ctx.Context().Err()
		}, flow.StepConfig{Budget: flow.StepBudget{
			MaxInvocations: 1,
			Timeout:        50 * time.Millisecond,
		}})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Park == nil || res.Park.Axis != flow.AxisTimeout {
		t.Fatalf("res = %+v, want a timeout park", res)
	}
	inv := axisOf(t, res.Park, flow.AxisInvocations)
	if inv.Used != 1 || !inv.Exhausted {
		t.Errorf("invocations = %+v, want 1/1 flagged flat — the bump on the way "+
			"out is exactly what re-parks the step after a timeout-only grant", inv)
	}
	to := axisOf(t, res.Park, flow.AxisTimeout)
	if !to.Exhausted {
		t.Errorf("timeout = %+v, want exhausted", to)
	}
}

// Prompts is per-invocation and the record's counter resets on the bump, so
// the park report is the only place the run's prompt usage is preserved.
func TestParkReportsPromptsSpentByTheRun(t *testing.T) {
	agent := &stubAgent{name: "stub", responses: []flow.AgentResponse{
		{LastText: "one"}, {LastText: "two"},
	}}
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("chatty", "plan", func(ctx flow.StepCtx) error {
			for i := 0; i < 3; i++ {
				if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{}); err != nil {
					return err
				}
			}
			return nil
		}, flow.StepConfig{Budget: flow.StepBudget{
			MaxInvocations:          3,
			MaxPromptsPerInvocation: 2,
			MaxCostUSD:              10,
			Timeout:                 time.Minute,
		}})
	}, agent)

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Park == nil || res.Park.Axis != flow.AxisPrompts {
		t.Fatalf("res = %+v, want a prompts park", res)
	}
	p := axisOf(t, res.Park, flow.AxisPrompts)
	if p.Used != 2 || p.Granted != 2 || !p.Exhausted {
		t.Errorf("prompts = %+v, want 2/2 flat (what the run actually spent)", p)
	}
}

// Non-budget parks carry no axis report: there is no cap to grant against, and
// an empty report is what tells a reader that.
func TestNonBudgetParkReportsNoAxes(t *testing.T) {
	app, _, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("silent", "plan", func(ctx flow.StepCtx) error {
			return nil // returns without resolving
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})

	res, err := RunOne(context.Background(), app, claim)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Park == nil || res.Park.Kind != flow.ParkStepDidNotResolve {
		t.Fatalf("res = %+v, want a did-not-resolve park", res)
	}
	if len(res.Park.Axes) != 0 {
		t.Errorf("Axes = %+v, want none on a non-budget park", res.Park.Axes)
	}
}

// ---------------------------------------------------------------------------
// Rendering.
// ---------------------------------------------------------------------------

func TestAxisReportFormat(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   flow.AxisReport
		want string
	}{
		{"invocations", flow.NewAxisReport(flow.AxisInvocations, 3, 3), "3/3 inv (flat)"},
		{"prompts", flow.NewAxisReport(flow.AxisPrompts, 1, 2), "1/2 prompts"},
		{"cost", flow.NewAxisReport(flow.AxisCost, 11.18, 10), "$11.18/$10.00 (flat)"},
		{"timeout", flow.NewAxisReport(flow.AxisTimeout, 5400, 10800), "1h30m0s/3h0m0s"},
		// Sub-second budgets must not truncate to a meaningless "0s/0s".
		{"sub-second timeout", flow.NewAxisReport(flow.AxisTimeout, 0.051, 0.05), "51ms/50ms (flat)"},
		// No cap set means the axis is unmetered — never "flat".
		{"uncapped", flow.NewAxisReport(flow.AxisCost, 4, 0), "$4.00/$0.00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Format(); got != tc.want {
				t.Errorf("Format() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParkLineListsEveryAxis(t *testing.T) {
	line := parkLine(&parkPayload{
		Kind:   "budget-exhausted",
		Step:   "push",
		Axis:   "cost",
		Reason: `spent $11.18 without resolving "push"`,
		Axes: []axisReportPayload{
			{Axis: "invocations", Used: 3, Granted: 3, Exhausted: true},
			{Axis: "prompts", Used: 2, Granted: 2, Exhausted: true},
			{Axis: "cost", Used: 11.18, Granted: 10, Exhausted: true},
			{Axis: "timeout", Used: 0, Granted: 10800},
		},
	})
	// Headline first, axes on their own indented line.
	if !strings.HasPrefix(line, `budget-exhausted on "push" (cost) — spent $11.18`) {
		t.Errorf("line = %q, want the headline unchanged", line)
	}
	for _, want := range []string{
		"\n  axes: ", "3/3 inv (flat)", "2/2 prompts (flat)", "$11.18/$10.00 (flat)", "0s/3h0m0s",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("line = %q, want it to contain %q", line, want)
		}
	}
}

func TestParkLineOmitsAxesWhenAbsent(t *testing.T) {
	line := parkLine(&parkPayload{Kind: "question", Step: "plan", Reason: "question pending"})
	if strings.Contains(line, "axes:") || strings.Contains(line, "\n") {
		t.Errorf("line = %q, want a single line with no axes section", line)
	}
}

// ---------------------------------------------------------------------------
// The report has to be actionable: an axis printed as "(flat)" must be
// grantable by flag, or the operator reads a report they cannot act on.
// ---------------------------------------------------------------------------

func TestGrantAcceptsFlagForParkReportedFlatAxis(t *testing.T) {
	env := newParkGrantEnv(t)
	// Actually spend the cost cap, so the park below describes a real record.
	if err := env.be.AddCost(context.Background(), env.claim, "plan", 10); err != nil {
		t.Fatalf("AddCost: %v", err)
	}
	// Parked on cost; prompts is flat too, and only the park knows it — the
	// record's per-invocation counter has already reset.
	env.park(t, flow.ParkRequest{
		Kind: flow.ParkBudgetExhausted, Step: "plan", Axis: flow.AxisCost,
		Reason: "test park",
		Axes: []flow.AxisReport{
			flow.NewAxisReport(flow.AxisPrompts, 1, 1),
			flow.NewAxisReport(flow.AxisCost, 10, 10),
		},
	})

	if code := env.grant("--prompts", "4"); code != 0 {
		t.Fatalf("exit = %d, want 0. stderr=%s", code, env.err.String())
	}
	rec := env.rec(t, "plan")
	if rec.GrantedPromptsPerInvocation != 5 {
		t.Errorf("GrantedPromptsPerInvocation = %d, want 5 (1 + 4)", rec.GrantedPromptsPerInvocation)
	}
	// The parked axis is still topped up in the same grant — one write, one
	// unpark.
	if rec.GrantedCostUSD <= 10 {
		t.Errorf("GrantedCostUSD = %v, want > 10", rec.GrantedCostUSD)
	}
}

func TestGrantStillRefusesFlagForAxisWithHeadroom(t *testing.T) {
	env := newParkGrantEnv(t)
	// Parked on cost, and the report says prompts has room to spare.
	env.park(t, flow.ParkRequest{
		Kind: flow.ParkBudgetExhausted, Step: "plan", Axis: flow.AxisCost,
		Reason: "test park",
		Axes: []flow.AxisReport{
			flow.NewAxisReport(flow.AxisPrompts, 0, 1),
			flow.NewAxisReport(flow.AxisCost, 10, 10),
		},
	})

	if code := env.grant("--prompts", "4"); code != 2 {
		t.Fatalf("exit = %d, want 2. stderr=%s", code, env.err.String())
	}
	if !strings.Contains(env.err.String(), "does not apply") {
		t.Errorf("stderr = %q, want a refusal", env.err.String())
	}
	if got := env.rec(t, "plan").GrantedPromptsPerInvocation; got != 1 {
		t.Errorf("GrantedPromptsPerInvocation = %d, want 1 (a refusal must not write)", got)
	}
}

// A park with no report at all is the pre-existing shape; it must keep
// behaving exactly as before.
func TestGrantUnchangedForParkWithoutAxisReport(t *testing.T) {
	env := newParkGrantEnv(t)
	env.park(t, budgetExhausted("plan", flow.AxisCost))

	if code := env.grant("--prompts", "4"); code != 2 {
		t.Fatalf("exit = %d, want 2. stderr=%s", code, env.err.String())
	}
	if got := env.rec(t, "plan").GrantedPromptsPerInvocation; got != 1 {
		t.Errorf("GrantedPromptsPerInvocation = %d, want 1 (unchanged)", got)
	}
}
