package issue

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// Coverage is what keeps one graph honest. Every step exists on every build, so
// the only thing that says which of them a binary may perform is the coverage
// it declares — and without it, collapsing the three old graphs would turn every
// maintainer-capable operator into a runner carrying every item through with no
// way to decline.

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

// gateFor builds the flow and the gate a binary declaring this coverage
// installs.
func gateFor(t *testing.T, covered ...flow.RoleName) flow.PreflightFunc {
	t.Helper()
	b := &builder{cfg: Config{}}
	return coverageGate(b.resolveFlow(Config{}), covered)
}

var (
	contributorOnly = []flow.RoleName{contributorRole}
	maintainerOnly  = []flow.RoleName{maintainerRole}
	bothRoles       = []flow.RoleName{contributorRole, maintainerRole}
)

func TestCoverageGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		covered []flow.RoleName
		item    *flow.Item
		blocked bool
	}{
		// A contributor binary ends at the proposal, exactly as it did when the
		// maintainer's steps were a different graph.
		{"a contributor does not judge the proposal", contributorOnly,
			atStep(flow.StepId(StepReviewProposal)), true},
		{"a contributor performs its own steps", contributorOnly,
			atStep(flow.StepId(StepImplement)), false},
		// And a maintainer binary does not do the contributor's work: an empty
		// journal stands at the entry step, which is the contributor's.
		{"a maintainer does not implement", maintainerOnly,
			atStep(flow.StepId(StepImplement)), true},
		{"a maintainer judges the proposal", maintainerOnly,
			atStep(flow.StepId(StepReviewProposal)), false},
		{"an empty journal is gated on the entry step's role", maintainerOnly,
			&flow.Item{}, true},
		// Covering both roles is one principal covering both sides, so neither
		// half is refused — and that is the whole of what carrying through is.
		{"covering both roles covers the contributor's steps", bothRoles,
			atStep(flow.StepId(StepImplement)), false},
		{"covering both roles covers the maintainer's steps", bothRoles,
			atStep(flow.StepId(StepReviewProposal)), false},
		{"covering both roles starts at the entry step", bothRoles,
			&flow.Item{}, false},
		// The rework edge lands back on a step whose result is already recorded.
		// Coverage judges the ROLE the route names, never whether the step has
		// run before.
		{"a rework handback is the contributor's move like any other", contributorOnly,
			atStep(flow.StepId(StepImplement)), false},
		// Coverage is read as declared: the order the roles are named in
		// changes nothing.
		{"coverage order is not a ranking", []flow.RoleName{maintainerRole, contributorRole},
			atStep(flow.StepId(StepImplement)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := gateFor(t, tc.covered...)(context.Background(), tc.item)
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
	err := gateFor(t, contributorOnly...)(context.Background(), atStep(flow.StepId(StepReviewProposal)))
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

// A binary covering both roles performs two, and the refusal it would raise has
// to render both — a coverage set printed as a bare slice reads as a bug in the
// message rather than as the binary's standing.
func TestPerformedList(t *testing.T) {
	for _, tc := range []struct {
		name    string
		covered []flow.RoleName
		want    string
	}{
		{"none", nil, "no role this lifecycle declares"},
		{"one", contributorOnly, `the "contributor" role`},
		{"both", bothRoles, `the "contributor" and "maintainer" roles`},
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
			if err := gateFor(t, contributorOnly...)(context.Background(), item); err != nil {
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
	if err := gateFor(t, contributorOnly...)(context.Background(), item); err != nil {
		t.Errorf("gate = %v, want the refusal left to the branch that owns it", err)
	}
}

// ONE declared set, read by both the gate and the app. Config.Coverage is
// converted once in BuildApp and handed to both, so what the gate refuses at
// dispatch and what the app announces, selects and hands off on cannot
// disagree. Each half is asserted from the other side: the App's field is what
// `resolve` and `doctor` read, and the gate's refusal is what a dispatch meets.
func TestBuildApp_TheAppAndTheGateReadTheSameCoverage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		covered []Role
		want    []flow.RoleName
		// steps the gate lets through and refuses, at the item the route
		// stands on
		performs, refuses *flow.Item
	}{
		{"contributor", []Role{RoleContributor}, contributorOnly,
			atStep(flow.StepId(StepImplement)), atStep(flow.StepId(StepReviewProposal))},
		{"maintainer", []Role{RoleMaintainer}, maintainerOnly,
			atStep(flow.StepId(StepReviewProposal)), atStep(flow.StepId(StepImplement))},
		{"both", []Role{RoleContributor, RoleMaintainer}, bothRoles,
			atStep(flow.StepId(StepReviewProposal)), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, err := BuildApp(context.Background(), Config{
				BinaryName: "issue", VerifyCmd: []string{"bin/verify"}, BaseBranch: "main",
				Coverage: tc.covered,
			}, Deps{Orchestrator: &stubBackend{}, Agent: stubAgent{}})
			if err != nil {
				t.Fatalf("BuildApp: %v", err)
			}
			if !slices.Equal(app.Coverage, tc.want) {
				t.Errorf("App.Coverage = %v, want %v — the declaration as configured", app.Coverage, tc.want)
			}
			if err := app.Preflight(context.Background(), tc.performs); err != nil {
				t.Errorf("Preflight = %v on a covered role's step, want it performed", err)
			}
			if tc.refuses == nil {
				return
			}
			err = app.Preflight(context.Background(), tc.refuses)
			if err == nil || !errors.Is(err, flow.ErrBlocked) {
				t.Fatalf("Preflight = %v on an uncovered role's step, want it blocked", err)
			}
			if !strings.Contains(err.Error(), performedList(tc.want)) {
				t.Errorf("err = %q, want it to name the coverage the app carries (%s)", err, performedList(tc.want))
			}
		})
	}
}

// Nothing is probed to build the app. Capability is the ceiling and coverage
// is the choice within it, and a covered role the account cannot back is a
// handoff rather than a misconfiguration — so BuildApp has no reason to ask
// what the account can do, and a binary starts, and `doctor` runs, on a token
// the backend would refuse.
func TestBuildApp_DetectsNothing(t *testing.T) {
	be := &stubBackend{capsErr: errors.New("the token is expired")}
	if _, err := BuildApp(context.Background(), Config{
		BinaryName: "issue", VerifyCmd: []string{"bin/verify"}, BaseBranch: "main",
		Coverage: []Role{RoleContributor, RoleMaintainer},
	}, Deps{Orchestrator: be, Agent: stubAgent{}}); err != nil {
		t.Fatalf("BuildApp = %v, want an app built without asking the backend anything", err)
	}
	if be.capsCalls != 0 {
		t.Errorf("DetectCapabilities called %d time(s) at build, want none", be.capsCalls)
	}
}
