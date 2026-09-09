package issue

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// Coverage is what keeps one graph honest. Every step exists on every build, so
// the only thing that says which of them a binary may perform is the narrowing
// it declares — and without it, collapsing the three old graphs would turn every
// maintainer-capable operator into a carry-through runner with no way to decline.

// performedRoles is the ONE derivation of that narrowing, and it is where the
// two config fields acquire their meaning: capability is the ceiling, and
// carry-through is the only choice available inside it.
func TestPerformedRoles(t *testing.T) {
	for _, tc := range []struct {
		name         string
		role         Role
		carryThrough bool
		want         []flow.RoleName
	}{
		{"a contributor performs the contributor's steps", RoleContributor, false,
			[]flow.RoleName{contributorRole}},
		{"a maintainer declining to carry through stops at the proposal", RoleMaintainer, false,
			[]flow.RoleName{maintainerRole}},
		{"carrying through covers both sides of the boundary", RoleMaintainer, true,
			[]flow.RoleName{contributorRole, maintainerRole}},
		// The empty role covers nothing: coverage cannot add a role the account
		// cannot back, so there is nothing for it to narrow.
		{"an account backing no role covers none", "", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := performedRoles(tc.role, tc.carryThrough)
			if len(got) != len(tc.want) {
				t.Fatalf("performedRoles(%q, %t) = %v, want %v", tc.role, tc.carryThrough, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("performedRoles(%q, %t) = %v, want %v", tc.role, tc.carryThrough, got, tc.want)
				}
			}
		})
	}
}

// atStep is an item whose journal routes to step: one completed execution
// electing it. Position reads the journal and nothing else, so this is the only
// way to stand an item anywhere.
func atStep(step flow.StepId) *flow.Item {
	return &flow.Item{Journal: []flow.JournalEntry{{
		Step:      "the-predecessor",
		Execution: 1,
		Route:     flow.Route{Next: step},
		By:        "tester",
	}}}
}

// gateFor builds the flow and the gate a binary of this configuration installs.
func gateFor(t *testing.T, role Role, carryThrough bool) flow.PreflightFunc {
	t.Helper()
	b := &builder{cfg: Config{CarryThrough: carryThrough}, role: role}
	return coverageGate(b.resolveFlow(Config{}), performedRoles(role, carryThrough))
}

func TestCoverageGate(t *testing.T) {
	for _, tc := range []struct {
		name         string
		role         Role
		carryThrough bool
		item         *flow.Item
		blocked      bool
	}{
		// A contributor binary ends at the proposal, exactly as it did when the
		// maintainer's steps were a different graph.
		{"a contributor does not judge the proposal", RoleContributor, false,
			atStep(flow.StepId(StepReviewProposal)), true},
		{"a contributor performs its own steps", RoleContributor, false,
			atStep(flow.StepId(StepImplement)), false},
		// And a maintainer binary does not do the contributor's work: an empty
		// journal stands at the entry step, which is the contributor's.
		{"a maintainer does not implement", RoleMaintainer, false,
			atStep(flow.StepId(StepImplement)), true},
		{"a maintainer judges the proposal", RoleMaintainer, false,
			atStep(flow.StepId(StepReviewProposal)), false},
		{"an empty journal is gated on the entry step's role", RoleMaintainer, false,
			&flow.Item{}, true},
		// Carrying through is one principal covering both sides, so neither
		// half is refused — and that is the whole of what carry-through does.
		{"carrying through covers the contributor's steps", RoleMaintainer, true,
			atStep(flow.StepId(StepImplement)), false},
		{"carrying through covers the maintainer's steps", RoleMaintainer, true,
			atStep(flow.StepId(StepReviewProposal)), false},
		{"carrying through starts at the entry step", RoleMaintainer, true,
			&flow.Item{}, false},
		// The rework edge lands back on a step whose result is already recorded.
		// Coverage judges the ROLE the route names, never whether the step has
		// run before.
		{"a rework handback is the contributor's move like any other", RoleContributor, false,
			atStep(flow.StepId(StepImplement)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := gateFor(t, tc.role, tc.carryThrough)(context.Background(), tc.item)
			if !tc.blocked {
				if err != nil {
					t.Fatalf("gate = %v, want the step performed", err)
				}
				return
			}
			if err == nil {
				t.Fatal("gate = nil, want the step refused")
			}
			// Blocked, not a bare error: a plain preflight error is a skip,
			// which exits 0 and reads as "nothing to do". Nothing failed here —
			// the item awaits somebody else.
			if !errors.Is(err, flow.ErrBlocked) {
				t.Errorf("err = %v, want it to wrap flow.ErrBlocked", err)
			}
		})
	}
}

// The refusal has to say all three things: which step stopped it, whose move
// that step is, and what this binary performs. Two of the three leaves the
// operator unable to choose between re-running elsewhere and reconfiguring.
func TestCoverageGate_RefusalNamesTheStepTheRoleAndTheCoverage(t *testing.T) {
	err := gateFor(t, RoleContributor, false)(context.Background(), atStep(flow.StepId(StepReviewProposal)))
	if err == nil {
		t.Fatal("gate = nil, want the proposal review refused")
	}
	for _, want := range []string{
		string(StepReviewProposal), // the step
		string(RoleMaintainer),     // whose move it is
		string(RoleContributor),    // what this binary performs
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err, want)
		}
	}
}

// A carry-through binary performs two roles, and the refusal it would raise has
// to render both — a coverage set printed as a bare slice reads as a bug in the
// message rather than as the binary's standing.
func TestPerformedList(t *testing.T) {
	for _, tc := range []struct {
		name    string
		covered []flow.RoleName
		want    string
	}{
		{"none", nil, "no role this lifecycle declares"},
		{"one", []flow.RoleName{contributorRole}, `the "contributor" role`},
		{"both", []flow.RoleName{contributorRole, maintainerRole},
			`the "contributor" and "maintainer" roles`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := performedList(tc.covered); got != tc.want {
				t.Errorf("performedList(%v) = %q, want %q", tc.covered, got, tc.want)
			}
		})
	}
}

// A finalized item is never gated. Its run is over, and RunOne's own branch
// finalizes and releases it — refusing here would strand an item this binary
// itself finished, on a role nobody's move belongs to any more.
func TestCoverageGate_LetsAFinalizedItemThrough(t *testing.T) {
	for _, d := range []flow.Disposition{flow.DispositionResolved, flow.DispositionRejected} {
		t.Run(string(d), func(t *testing.T) {
			item := &flow.Item{Journal: []flow.JournalEntry{{
				Step: flow.StepId(StepRecordMerge), Execution: 1,
				Route: flow.Route{Finalize: d}, By: "tester",
			}}}
			if err := gateFor(t, RoleContributor, false)(context.Background(), item); err != nil {
				t.Errorf("gate = %v on a finalized item, want it through to finalize-and-release", err)
			}
		})
	}
}

// A Position refusal passes through too. RunOne returns it as the flow defect
// it is; answering it here would turn a graph that cannot say where the item
// stands into a coverage boundary, which is a different problem with a
// different fix.
func TestCoverageGate_LetsAPositionRefusalThrough(t *testing.T) {
	item := atStep("no-such-step")
	if err := gateFor(t, RoleContributor, false)(context.Background(), item); err != nil {
		t.Errorf("gate = %v, want the refusal left to the branch that owns it", err)
	}
}

// The account that backs nothing gets the sentence about the access it is
// missing, not the one about which role's step the route happens to stand on.
// Both are true; only one is useful, and gate ORDER is what picks it.
func TestBuildApp_NoRoleAnswersAheadOfCoverage(t *testing.T) {
	app, err := BuildApp(context.Background(), Config{
		BinaryName: "issue", VerifyCmd: []string{"bin/verify"}, BaseBranch: "main",
	}, Deps{Orchestrator: &stubBackend{caps: nil}, Agent: stubAgent{}})
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}
	err = app.Preflight(context.Background(), &flow.Item{})
	if err == nil {
		t.Fatal("Preflight = nil, want every dispatch refused")
	}
	if !strings.Contains(err.Error(), noAssumableRole) {
		t.Errorf("err = %q, want the no-role wording", err)
	}
	if strings.Contains(err.Error(), performedList(nil)) {
		t.Errorf("err = %q, want the coverage gate NOT to answer first", err)
	}
}
