package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// The route half of `status`: the journal, whose move it is, the treasurer's
// spend, a step executing elsewhere, and a run that is deliberately idle.

// recordRoute appends journal entries to the item the env holds, as a run that
// had completed those executions would have left behind.
func recordRoute(t *testing.T, env *parkGrantEnv, entries ...flow.JournalEntry) {
	t.Helper()
	for _, e := range entries {
		if err := env.be.AppendEntry(context.Background(), env.claim.ItemRef, e); err != nil {
			t.Fatalf("AppendEntry: %v", err)
		}
	}
}

// `status` reports the item's ROUTE, not a checklist: each completed execution
// with who ran it, in which role, electing what, and why.
//
// docs/cli.md § Status has required this since the step-model amendment and
// `status` rendered none of it — the journal was loaded on every invocation and
// discarded.
func TestStatusHuman_RendersTheJournalInOrder(t *testing.T) {
	env := newParkGrantEnv(t)
	recordRoute(t, env,
		flow.JournalEntry{
			Step: "plan", Execution: 1,
			Route: flow.Route{Next: "commit"},
			By:    "djabi", Role: "contributor",
			Message: "the plan is written",
			At:      time.Date(2025, 5, 1, 9, 0, 0, 0, time.UTC),
			Spend:   flow.Spend{CostUSD: 1.25, Duration: 90 * time.Second},
		},
		flow.JournalEntry{
			Step: "commit", Execution: 1,
			Route: flow.Route{Next: "pr-open"},
			By:    "djabi", Role: "contributor",
			Message: "the change is committed",
			At:      time.Date(2025, 5, 1, 9, 5, 0, 0, time.UTC),
		},
	)

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d; stderr=%q", code, env.err.String())
	}
	out := env.out.String()
	if !strings.Contains(out, "route:") {
		t.Fatalf("no route block:\n%s", out)
	}
	// Who ran it, in which role, electing what, and why — all four.
	if !strings.Contains(out, "plan — djabi as contributor → commit") {
		t.Errorf("the first execution is not reported whole:\n%s", out)
	}
	if !strings.Contains(out, "the plan is written") {
		t.Errorf("the election's reason is missing:\n%s", out)
	}
	if !strings.Contains(out, "$1.25") {
		t.Errorf("the execution's cost is missing:\n%s", out)
	}
	// IN ORDER: the journal is the route, and a route out of order is a
	// different route.
	if strings.Index(out, "plan — djabi") > strings.Index(out, "commit — djabi") {
		t.Errorf("the journal is out of order:\n%s", out)
	}
}

// A finalizing election names the disposition it ended on, rather than a
// successor there is none of.
func TestStatusHuman_AFinalizingElectionNamesItsDisposition(t *testing.T) {
	env := newParkGrantEnv(t)
	recordRoute(t, env, flow.JournalEntry{
		Step: "plan", Execution: 1,
		Route:   flow.Route{Finalize: flow.DispositionResolved},
		By:      "djabi",
		Role:    "contributor",
		Message: "nothing to do",
		At:      time.Now(),
	})

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d", code)
	}
	if !strings.Contains(env.out.String(), "finalize:resolved") {
		t.Errorf("a finalizing election does not name its disposition:\n%s", env.out.String())
	}
}

// An item with NO ENTRIES renders no route block at all. An unstarted item has
// an empty route, and a header over nothing says something happened.
func TestStatusHuman_AnEmptyJournalRendersNoRouteBlock(t *testing.T) {
	env := newParkGrantEnv(t)

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d; stderr=%q", code, env.err.String())
	}
	if strings.Contains(env.out.String(), "route:") {
		t.Errorf("an unstarted item printed a route block:\n%s", env.out.String())
	}
}

// The journal reaches --json too, with the field names the CLI owns: the SDK's
// JournalEntry carries no tags of its own, so this key set is the contract.
func TestStatusJSON_JournalKeySet(t *testing.T) {
	env := newParkGrantEnv(t)
	recordRoute(t, env, flow.JournalEntry{
		Step: "plan", Execution: 2,
		Route: flow.Route{Next: "commit"},
		By:    "djabi", Role: "contributor",
		Message: "the plan is written",
		Note:    "the base branch moved",
		At:      time.Date(2025, 5, 1, 9, 0, 0, 0, time.UTC),
		Spend:   flow.Spend{CostUSD: 1.25, Duration: 90 * time.Second},
	})

	if code := env.app.cmdStatus(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("cmdStatus = %d", code)
	}
	var payload statusPayload
	if err := json.Unmarshal(env.out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Journal) != 1 {
		t.Fatalf("journal = %d entries, want 1", len(payload.Journal))
	}
	e := payload.Journal[0]
	if e.Step != "plan" || e.Execution != 2 || e.By != "djabi" || e.Role != "contributor" {
		t.Errorf("entry identity wrong: %+v", e)
	}
	if e.Elected != "commit" || e.Reason != "the plan is written" || e.Note != "the base branch moved" {
		t.Errorf("entry election wrong: %+v", e)
	}
	if e.CostUSD != 1.25 || e.DurationSeconds != 90 {
		t.Errorf("entry spend wrong: %+v", e)
	}
}

// WHOSE MOVE IT IS, for a role — with the account of record beside it, which is
// the half a bare role name cannot give.
func TestStatusHuman_WhoseMoveForARole(t *testing.T) {
	env := newParkGrantEnv(t)
	recordRoute(t, env, flow.JournalEntry{
		Step: "plan", Execution: 1,
		Route:  flow.Route{Next: "commit"},
		Awaits: flow.Awaits{Role: "contributor"},
		By:     "djabi", Role: "contributor",
		At: time.Now(),
	})

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d", code)
	}
	out := env.out.String()
	if !strings.Contains(out, "whose move: contributor") {
		t.Errorf("status does not say whose move it is:\n%s", out)
	}
	if !strings.Contains(out, "djabi") {
		t.Errorf("the awaited role's account of record is missing:\n%s", out)
	}
}

// An awaited SIGNAL is NOBODY's move, and says so. "awaiting maintainer" and
// "awaiting an observation" call for different acts, and reporting the second
// as a role would send an operator looking for a person.
func TestStatusHuman_WhoseMoveForASignalIsNobody(t *testing.T) {
	env := newParkGrantEnv(t)
	recordRoute(t, env, flow.JournalEntry{
		Step: "commit", Execution: 1,
		Route:  flow.Route{Next: "pr-open"},
		Awaits: flow.Awaits{Signal: "pr-merged"},
		By:     "djabi", Role: "contributor",
		At: time.Now(),
	})

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d", code)
	}
	out := env.out.String()
	if !strings.Contains(out, "whose move: nobody — awaiting signal pr-merged") {
		t.Errorf("an awaited signal is not reported as nobody's move:\n%s", out)
	}
}

// An item that awaits nothing — unstarted, or finalized — reports no move at
// all rather than an empty one.
func TestStatusHuman_AnItemAwaitingNothingReportsNoMove(t *testing.T) {
	env := newParkGrantEnv(t)

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d", code)
	}
	if strings.Contains(env.out.String(), "whose move:") {
		t.Errorf("an unstarted item claimed a move was owed:\n%s", env.out.String())
	}
}

// The treasurer's spend, WITH WAITING REPORTED APART. A total that folded the
// two together would read as a step that took hours to do a minute's work.
func TestStatusHuman_SpendReportsWaitingApart(t *testing.T) {
	env := newParkGrantEnv(t)
	ctx := context.Background()
	if err := env.be.AddCost(ctx, env.claim.ItemRef, "plan", 2.50); err != nil {
		t.Fatalf("AddCost: %v", err)
	}
	if err := env.be.AddDuration(ctx, env.claim.ItemRef, "plan", 3*time.Minute); err != nil {
		t.Fatalf("AddDuration: %v", err)
	}
	// Waiting is recorded through its own method, and reported apart from the
	// active time — the distinction this assertion exists for.
	if err := env.be.AddWaiting(ctx, env.claim.ItemRef, "plan", 20*time.Minute); err != nil {
		t.Fatalf("AddWaiting: %v", err)
	}

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d; stderr=%q", code, env.err.String())
	}
	out := env.out.String()
	if !strings.Contains(out, "spend: $2.50") {
		t.Errorf("the spend is missing:\n%s", out)
	}
	if !strings.Contains(out, "3m active") {
		t.Errorf("the active time is missing or not labelled as active:\n%s", out)
	}
	// Apart, and visibly so: twenty minutes of waiting folded into three
	// minutes of work would read as a step that took ages to do very little.
	if !strings.Contains(out, "20m waiting") {
		t.Errorf("waiting is not reported apart from active time:\n%s", out)
	}
}

// An item nothing has been spent on reports NO spend block. A zeroed one reads
// as a measurement.
func TestStatusHuman_AnUnspentItemReportsNoSpend(t *testing.T) {
	env := newParkGrantEnv(t)

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d", code)
	}
	if strings.Contains(env.out.String(), "spend:") {
		t.Errorf("an unspent item printed a spend block:\n%s", env.out.String())
	}
}

// A park says WHAT WOULD CLEAR IT, from the one table `resolve` narrates a park
// with — so the two commands cannot tell an operator to do different things
// about one park.
func TestStatusHuman_AParkNamesWhatWouldClearIt(t *testing.T) {
	env := newParkGrantEnv(t)
	if err := env.be.Park(context.Background(), env.claim.ItemRef, flow.ParkRequest{
		Kind: flow.ParkQuestion, Step: "plan", Reason: "a question is pending",
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d; stderr=%q", code, env.err.String())
	}
	if !strings.Contains(env.out.String(), "to resume:") {
		t.Errorf("the park does not say what would clear it:\n%s", env.out.String())
	}
	if !strings.Contains(env.out.String(), "answer") {
		t.Errorf("a question park should send the operator to `answer`:\n%s", env.out.String())
	}
}

// A step executing under ANOTHER PARTY'S CLAIM is its own state, not different
// prose for `pending`. The two call for opposite responses: pending invites the
// operator to go start it, and an item leased on another host is one to leave
// alone.
//
// The evidence is the CLAIM — who holds it, and where — never a process: a
// process identity on another machine is not knowable from here in principle,
// which is why `running` could never be honestly said of it.
func TestStatusHuman_APendingStepHeldElsewhereExecutesElsewhere(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "held away"})
	// Held by an arena that is not this one — the account and a foreign arena,
	// which is what LookupClaim returns for a lease on another host.
	be := &claimedBackend{Orchestrator: inner, info: &flow.ClaimInfo{
		Account: "someone-else",
		Arena:   flow.Arena{Host: "another-host", Id: "/elsewhere/checkout"},
	}}
	app, out := selectionApp(t, be)
	ref, err := inner.ResolveRef(t.Context(), "1")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	t.Setenv("FLOW_DIR", t.TempDir())

	if code := app.cmdStatus(context.Background(), []string{"--human", ref.Display}); code != 0 {
		t.Fatalf("cmdStatus = %d", code)
	}
	if !strings.Contains(out.String(), stateElsewhere) {
		t.Errorf("a step held by another arena is not reported as executing elsewhere:\n%s", out.String())
	}
}

// …and NOT when the claim names THIS arena. An item this arena holds is
// pending, running, or resolved like any other — the state exists for the case
// the process is unreachable, and claiming it here would hide a stalled run of
// our own.
func TestStatusHuman_APendingStepHeldHereIsNotExecutingElsewhere(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "held here"})
	be := &claimedBackend{Orchestrator: inner, info: &flow.ClaimInfo{
		Account: "djabi", Arena: flow.ArenaAt(inner.ArenaRoot()),
	}}
	app, out := selectionApp(t, be)
	ref, err := inner.ResolveRef(t.Context(), "1")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	t.Setenv("FLOW_DIR", t.TempDir())

	if code := app.cmdStatus(context.Background(), []string{"--human", ref.Display}); code != 0 {
		t.Fatalf("cmdStatus = %d", code)
	}
	if strings.Contains(out.String(), stateElsewhere) {
		t.Errorf("an item this arena holds was reported as executing elsewhere:\n%s", out.String())
	}
}

// A run that is ALIVE AND DELIBERATELY IDLE — pacing, or between steps — is
// reported with what it waits on and until when. Without it, "claim held,
// nothing running" is indistinguishable from a stalled run, and an operator who
// reads it that way goes looking for a process to kill.
func TestStatusHuman_AWaitingRunIsReportedWithItsReason(t *testing.T) {
	env := newParkGrantEnv(t)
	until := time.Now().Add(95 * time.Minute)
	withRunningRecord(t, clistate.RunningRecord{
		Item:      env.claim.ItemRef.Display,
		Waiting:   "quota headroom",
		WaitUntil: until,
	})

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d; stderr=%q", code, env.err.String())
	}
	out := env.out.String()
	if !strings.Contains(out, "waiting: quota headroom") {
		t.Errorf("a deliberately idle run is not reported:\n%s", out)
	}
	if !strings.Contains(out, until.Local().Format("15:04")) {
		t.Errorf("the wait does not say until when:\n%s", out)
	}
}

// OBSERVED, NEVER ASSUMED, exactly as a running step is. A record left behind
// by a process that died would report a run waiting for something when it
// stopped hours ago — worse than the same mistake about a running step,
// because "waiting" invites the operator to keep waiting too.
func TestStatusHuman_ADeadWaitingRecordIsNotReported(t *testing.T) {
	env := newParkGrantEnv(t)
	withRunningRecord(t, clistate.RunningRecord{
		Item:    env.claim.ItemRef.Display,
		Waiting: "quota headroom",
		PID:     1,
		Exe:     "/nonexistent/flow-binary",
	})

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d", code)
	}
	if strings.Contains(env.out.String(), "waiting:") {
		t.Errorf("a dead registration was reported as a live wait:\n%s", env.out.String())
	}
}

// A record naming a STEP is a dispatch, reported as the running step — never
// also as a wait. The two states are exclusive, and a record that claimed both
// would have `status` report a step running and the run idle at once.
func TestStatusHuman_ADispatchIsNotAlsoAWait(t *testing.T) {
	env := newParkGrantEnv(t)
	withRunningRecord(t, clistate.RunningRecord{
		Item:    env.claim.ItemRef.Display,
		Step:    "plan",
		Waiting: "quota headroom",
	})

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d", code)
	}
	if strings.Contains(env.out.String(), "waiting:") {
		t.Errorf("a record naming a step was also reported as a wait:\n%s", env.out.String())
	}
}

// A wait that ends on a RE-MEASUREMENT rather than a clock reports no instant.
// Inventing one would be a prediction, and an operator reading it would wait
// for a moment nothing promised.
func TestStatusHuman_AWaitWithNoKnownEndNamesNoInstant(t *testing.T) {
	env := newParkGrantEnv(t)
	withRunningRecord(t, clistate.RunningRecord{
		Item:    env.claim.ItemRef.Display,
		Waiting: "the machine to become fit",
	})

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d", code)
	}
	out := env.out.String()
	if !strings.Contains(out, "waiting: the machine to become fit") {
		t.Errorf("the wait is missing:\n%s", out)
	}
	if strings.Contains(out, "until") {
		t.Errorf("a wait with no known end invented an instant:\n%s", out)
	}
}

// claimedBackend reports a claim of the test's choosing, so a lease held by
// another arena can be set up without a machine to hold it.
type claimedBackend struct {
	*fake.Orchestrator
	info *flow.ClaimInfo
}

func (b *claimedBackend) LookupClaim(ctx context.Context, ref flow.ItemRef) (*flow.ClaimInfo, error) {
	return b.info, nil
}
