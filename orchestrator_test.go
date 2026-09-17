package flow

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The nil-safe RequestManager helpers.
//
// Request() may still return nil — an orchestrator that does not land changes
// through pull requests implements none of the six methods — so the helpers
// have two paths and not three. There is no longer an "implements Open but not
// FindPR" case to test: RequestManager is ONE capability, and an orchestrator
// either has the surface or returns nil from Request().
// ---------------------------------------------------------------------------

// stubWorktreeNilRequest models an orchestrator with no pull-request surface.
type stubWorktreeNilRequest struct{ stubWorktreeBase }

func (w *stubWorktreeNilRequest) Request() RequestManager { return nil }

// stubWorktreeTypedNilRequest returns a non-nil interface header pointing at a
// nil concrete pointer — the typed-nil pitfall a plain `rq == nil` misses, and
// which would otherwise panic on the method call.
type stubWorktreeTypedNilRequest struct{ stubWorktreeBase }

func (w *stubWorktreeTypedNilRequest) Request() RequestManager {
	var rm *stubRequestManager
	return rm
}

type stubRequestManager struct {
	info PRInfo
	err  error
}

func (r *stubRequestManager) Open(context.Context, BranchName, string, string) (RequestUrl, error) {
	return "", nil
}
func (r *stubRequestManager) Merge(context.Context, RequestUrl) error              { return nil }
func (r *stubRequestManager) FindPR(context.Context) (PRInfo, error)               { return r.info, r.err }
func (r *stubRequestManager) PrepareMergeResult(context.Context, BranchName) error { return nil }
func (r *stubRequestManager) RevertMergePrep(context.Context) error                { return nil }
func (r *stubRequestManager) RebuildTools(context.Context) error                   { return nil }

// stubWorktreeWithRequest has a working pull-request surface.
type stubWorktreeWithRequest struct {
	stubWorktreeBase
	prInfo PRInfo
	prErr  error
}

func (w *stubWorktreeWithRequest) Request() RequestManager {
	return &stubRequestManager{info: w.prInfo, err: w.prErr}
}

// stubWorktreeBase satisfies the Worktree methods these tests do not call but
// the compiler requires.
type stubWorktreeBase struct{}

func (stubWorktreeBase) Branch(context.Context, BranchName, BranchName) (bool, error) {
	return false, nil
}
func (stubWorktreeBase) CurrentBranch(context.Context) (BranchName, error) { return "", nil }
func (stubWorktreeBase) Commit(context.Context, string) error              { return nil }
func (stubWorktreeBase) Stage(context.Context) error                       { return nil }
func (stubWorktreeBase) Push(context.Context) error                        { return nil }
func (stubWorktreeBase) Drift(context.Context) (Drift, error)              { return Drift{}, nil }
func (stubWorktreeBase) RevParse(context.Context, Revision) (CommitSha, error) {
	return "", nil
}
func (stubWorktreeBase) CutPoint(context.Context, BranchName) (CommitSha, error) {
	return "", nil
}
func (stubWorktreeBase) Run(context.Context, CommandName) (CommandRun, error) {
	return CommandRun{}, nil
}
func (stubWorktreeBase) RunGate(context.Context, GateName) (GateRun, error) { return GateRun{}, nil }
func (stubWorktreeBase) Judge(context.Context, GateRun) (GateVerdict, error) {
	return GateVerdict{}, nil
}
func (stubWorktreeBase) IsDirty(context.Context) (bool, error)        { return false, nil }
func (stubWorktreeBase) CapturePatch(context.Context) ([]byte, error) { return nil, nil }
func (stubWorktreeBase) Request() RequestManager                      { return nil }

func TestFindPR_NilRequest(t *testing.T) {
	wt := &stubWorktreeNilRequest{}
	_, err := FindPR(context.Background(), wt)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("FindPR with nil Request: got %v, want ErrUnsupported", err)
	}
	// "never here" and "not right now" must stay distinguishable: a caller
	// retries the second and not the first.
	if errors.Is(err, ErrUnavailable) {
		t.Error("an absent pull-request surface reported as ErrUnavailable — a caller would retry it forever")
	}
}

func TestFindPR_TypedNilRequest(t *testing.T) {
	wt := &stubWorktreeTypedNilRequest{}
	_, err := FindPR(context.Background(), wt)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("FindPR with typed-nil Request: got %v, want ErrUnsupported", err)
	}
}

func TestOpenAndMerge_NilRequestRefuseTyped(t *testing.T) {
	wt := &stubWorktreeNilRequest{}
	if _, err := Open(context.Background(), wt, "main", "t", "b"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Open with nil Request: got %v, want ErrUnsupported", err)
	}
	if err := Merge(context.Background(), wt, "https://example.invalid/pr/1"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Merge with nil Request: got %v, want ErrUnsupported", err)
	}
}

func TestFindPR_Delegates(t *testing.T) {
	wt := &stubWorktreeWithRequest{
		prInfo: PRInfo{URL: "https://example.invalid/pr/99", MergeCommitSHA: "deadbeef"},
	}
	info, err := FindPR(context.Background(), wt)
	if err != nil {
		t.Fatalf("FindPR: %v", err)
	}
	if info.URL != "https://example.invalid/pr/99" {
		t.Errorf("URL = %q, want %q", info.URL, "https://example.invalid/pr/99")
	}
	if info.MergeCommitSHA != "deadbeef" {
		t.Errorf("MergeCommitSHA = %q, want %q", info.MergeCommitSHA, "deadbeef")
	}
}

func TestFindPR_PropagatesError(t *testing.T) {
	wt := &stubWorktreeWithRequest{prErr: errors.New("GitHub API rate limit")}
	_, err := FindPR(context.Background(), wt)
	if err == nil {
		t.Fatal("expected error from FindPR to propagate")
	}
	if errors.Is(err, ErrUnsupported) {
		t.Error("error should be the RequestManager's error, not ErrUnsupported")
	}
}

// ---------------------------------------------------------------------------
// ExaminePush — the optional PushExaminer capability.
// ---------------------------------------------------------------------------

// stubExaminingWorktree can answer about a push; stubWorktreeBase deliberately
// cannot, which is what the degradation case rests on.
type stubExaminingWorktree struct {
	stubWorktreeBase
	err    error
	asks   int
	pushes int
}

func (w *stubExaminingWorktree) ExaminePush(context.Context) error {
	w.asks++
	return w.err
}
func (w *stubExaminingWorktree) Push(context.Context) error { w.pushes++; return nil }

// A worktree that cannot answer says so TYPED. The two answers lead opposite
// ways — a refusal is work for a repair step, an unsupported examine is a
// capability the arena lacks — so a caller that could not tell them apart
// would either prompt blind or read a refused push as permitted.
func TestExaminePush_UnsupportedIsTyped(t *testing.T) {
	err := ExaminePush(context.Background(), &stubWorktreeNilRequest{})
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("ExaminePush on a worktree that cannot answer: got %v, want ErrUnsupported", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Error("an absent examine reported as ErrUnavailable — a caller would retry it forever")
	}
	var refused ErrDisclosureRefused
	if errors.As(err, &refused) {
		t.Error("an absent examine reported as a disclosure refusal — a repair step would prompt with nothing to repair from")
	}
}

// The guard's answer reaches the caller unchanged, and asking pushes nothing.
func TestExaminePush_DelegatesAndPublishesNothing(t *testing.T) {
	refusal := ErrDisclosureRefused{Act: ActPush, Reason: errors.New("line 3 names a home path")}
	wt := &stubExaminingWorktree{err: refusal}

	err := ExaminePush(context.Background(), wt)
	var got ErrDisclosureRefused
	if !errors.As(err, &got) || got.Act != ActPush {
		t.Fatalf("err = %v, want the worktree's own ActPush refusal", err)
	}
	if !errors.Is(err, refusal.Reason) {
		t.Errorf("err = %v, want the guard's reason reachable through it", err)
	}
	if wt.asks != 1 {
		t.Errorf("the worktree was asked %d times, want 1", wt.asks)
	}
	if wt.pushes != 0 {
		t.Errorf("ExaminePush pushed %d times — it asks, it does not act", wt.pushes)
	}
}

func TestExaminePush_PermittedIsNil(t *testing.T) {
	wt := &stubExaminingWorktree{}
	if err := ExaminePush(context.Background(), wt); err != nil {
		t.Errorf("ExaminePush = %v, want nil: permission carries nothing", err)
	}
	if wt.asks != 1 {
		t.Errorf("the worktree was asked %d times, want 1", wt.asks)
	}
}

// ---------------------------------------------------------------------------
// Declaration helpers.
// ---------------------------------------------------------------------------

func TestRequiredGatesAndCommands(t *testing.T) {
	gates := RequiredGates()
	if !HasGate([]GateDef{{Name: GateIntegration}, {Name: GateFit}}, GateIntegration) {
		t.Error("HasGate did not find integration")
	}
	for _, want := range gates {
		if !HasGate([]GateDef{{Name: GateIntegration}, {Name: GateFit}}, want) {
			t.Errorf("required gate %q missing from a set declaring both", want)
		}
	}
	if HasGate([]GateDef{{Name: GateFit}}, GateIntegration) {
		t.Error("HasGate found integration in a set that does not declare it")
	}
	if !HasCommand([]CommandDef{{Name: CommandVerify}}, CommandVerify) {
		t.Error("HasCommand did not find verify")
	}
	if HasCommand([]CommandDef{{Name: CommandSetup}}, CommandVerify) {
		t.Error("HasCommand found verify in a set that does not declare it")
	}
	if got := RequiredCommands(); len(got) != 1 || got[0] != CommandVerify {
		t.Errorf("RequiredCommands() = %v, want [verify]", got)
	}
}

// declares builds a declaration list from names, the way a discovery read
// returns one.
func declares(names ...GateName) []GateDef {
	defs := make([]GateDef, 0, len(names))
	for _, n := range names {
		defs = append(defs, Gate(n, false))
	}
	return defs
}

func TestMissingGates(t *testing.T) {
	for _, tt := range []struct {
		name     string
		declared []GateDef
		want     string
	}{
		{"both required gates declared", declares(GateIntegration, GateFit), ""},
		{"the project's own gates do not substitute", declares(GateIntegration, GateFit, GateTested, GateCovered), ""},
		{"fit dropped", declares(GateIntegration, GateTested), "fit"},
		{"integration dropped", declares(GateFit, GateTested), "integration"},
		// The order is RequiredGates()'s, which is the order the boundary
		// refusal and `doctor` already name them in.
		{"nothing declared at all", nil, "integration, fit"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := joinGateNames(MissingGates(tt.declared)); got != tt.want {
				t.Errorf("MissingGates(%v) = %q, want %q", tt.declared, got, tt.want)
			}
		})
	}
}

// Nothing disappeared is not a finding. A change that adds gates, reorders the
// listing, or repeats a name has removed nothing, and a check that parked on
// any of those would stop work it has no business stopping.
func TestCheckGatesHeld_NothingDisappeared(t *testing.T) {
	for _, tt := range []struct {
		name          string
		before, after []GateDef
	}{
		{"identical", declares(GateIntegration, GateFit), declares(GateIntegration, GateFit)},
		{"same names in another order", declares(GateIntegration, GateFit), declares(GateFit, GateIntegration)},
		{"names added", declares(GateIntegration, GateFit), declares(GateIntegration, GateFit, GateCovered)},
		{"a name repeated in the listing", declares(GateFit, GateFit, GateIntegration), declares(GateIntegration, GateFit)},
		{"nothing before and nothing now", nil, nil},
		// The before-side is the whole of what this check has over `doctor`.
		// With none, it has nothing to say: a machine that declared nothing when
		// the resolution started is the BOUNDARY refusal's condition, already
		// met before any step ran, and reporting it here would blame this change
		// for a checkout whose tools were never built.
		{"nothing before, gates now", nil, declares(GateIntegration, GateFit)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := CheckGatesHeld(tt.before, tt.after); got != nil {
				t.Errorf("CheckGatesHeld() reported %v, want no regression", got.Gone)
			}
		})
	}
}

func TestCheckGatesHeld_ReportsWhatDisappeared(t *testing.T) {
	// The observed case: an entry point that answers a form the reader cannot
	// parse declares nothing, exactly as an unbuilt checkout does.
	everything := declares(GateBuilds, GateChecked, GateCovered, GateFit, GateFormatted, GateIntegration, GateTested)

	for _, tt := range []struct {
		name           string
		before, after  []GateDef
		gone, required string
	}{
		{
			name:   "a project gate the change retired",
			before: declares(GateIntegration, GateFit, GateCovered),
			after:  declares(GateIntegration, GateFit),
			gone:   "covered",
			// Not required, so the checkout is still driveable. It is still a
			// name that disappeared, and still the finding.
			required: "",
		},
		{
			name:     "a required gate",
			before:   declares(GateIntegration, GateFit, GateTested),
			after:    declares(GateIntegration, GateTested),
			gone:     "fit",
			required: "fit",
		},
		{
			name:     "the listing stopped being readable",
			before:   everything,
			after:    nil,
			gone:     "builds, checked, covered, fit, formatted, integration, tested",
			required: "integration, fit",
		},
		{
			// Names are compared whole: the full name is what a caller
			// addresses a gate by, so an instance that stopped being declared
			// is a name that disappeared, however alive its concept is.
			name:     "an instance renamed under a concept that stayed",
			before:   declares(GateIntegration, GateFit, "tested:wasm"),
			after:    declares(GateIntegration, GateFit, "tested:wasi"),
			gone:     "tested:wasm",
			required: "",
		},
		{
			name:     "a name repeated in the before-listing is reported once",
			before:   declares(GateIntegration, GateFit, GateCovered, GateCovered),
			after:    declares(GateIntegration, GateFit),
			gone:     "covered",
			required: "",
		},
		{
			name:     "gates added while a required one went",
			before:   declares(GateIntegration, GateFit),
			after:    declares(GateIntegration, GateCovered, GateTested),
			gone:     "fit",
			required: "fit",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckGatesHeld(tt.before, tt.after)
			if got == nil {
				t.Fatalf("CheckGatesHeld() found no regression, want gone: %s", tt.gone)
			}
			if g := joinGateNames(got.Gone); g != tt.gone {
				t.Errorf("Gone = %q, want %q", g, tt.gone)
			}
			if r := joinGateNames(got.Required); r != tt.required {
				t.Errorf("Required = %q, want %q", r, tt.required)
			}
			if b := joinGateNames(got.Before); b != joinGateNames(gateNames(tt.before)) {
				t.Errorf("Before = %q, want the before-listing whole", b)
			}
			if a := joinGateNames(got.After); a != joinGateNames(gateNames(tt.after)) {
				t.Errorf("After = %q, want the after-listing whole", a)
			}
		})
	}
}

// The report carries BOTH lists. One of them empty is the whole finding in the
// observed case, and an operator who sees only "integration, fit are missing"
// has been handed the same sentence a never-built checkout produces.
func TestGateRegression_ErrorNamesBothLists(t *testing.T) {
	reg := CheckGatesHeld(declares(GateIntegration, GateFit, GateTested), nil)
	if reg == nil {
		t.Fatal("CheckGatesHeld() found no regression")
	}
	msg := reg.Error()
	for _, want := range []string{
		"fit, integration, tested",   // what was declared when the resolution started
		"required: integration, fit", // which of the losses leave no arena able to start
		"(none)",                     // what the tree as it will land declares
		"declared by the tree as it will land",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to carry %q", msg, want)
		}
	}
}

// A caller that wants an error gets one through Err(), and a change that
// removed nothing gets a NIL one.
//
// This is the case the method exists for: CheckGatesHeld answers with a typed
// pointer, and a typed nil moved into an error interface is not nil. An
// installer returning the result directly would report a regression on every
// checkout it did not break — and panic rendering it — so the conversion has to
// be somewhere that knows the receiver may be nil.
func TestGateRegression_ErrIsNilWhenNothingDisappeared(t *testing.T) {
	held := CheckGatesHeld(declares(GateIntegration, GateFit), declares(GateIntegration, GateFit, GateTested))
	if err := held.Err(); err != nil {
		t.Errorf("Err() = %v, want a nil error — the change added a gate and removed none", err)
	}

	reg := CheckGatesHeld(declares(GateIntegration, GateFit), declares(GateIntegration))
	err := reg.Err()
	if err == nil {
		t.Fatal("Err() = nil, want the regression reported as an error")
	}
	if !strings.Contains(err.Error(), "fit") {
		t.Errorf("Err().Error() = %q, want it to name the gate that went", err.Error())
	}
	// And the lists survive the conversion: a caller that wrapped it still
	// reaches both sides of the comparison.
	var recovered *GateRegression
	if !errors.As(err, &recovered) || recovered != reg {
		t.Errorf("errors.As() did not recover the regression itself, got %v", recovered)
	}
}

// The park is deterministic and carries its evidence. Re-dispatching it is a
// loop, not a retry: the same tree re-read answers the same way.
func TestGateRegression_ParkRequest(t *testing.T) {
	reg := CheckGatesHeld(declares(GateIntegration, GateFit), declares(GateIntegration))
	if reg == nil {
		t.Fatal("CheckGatesHeld() found no regression")
	}
	req := reg.ParkRequest("implementation", "--- a/bin/gate\n+++ b/bin/gate\n")

	if req.Kind != ParkRefused {
		t.Errorf("Kind = %q, want %q", req.Kind, ParkRefused)
	}
	if req.Kind.RedispatchMayClear() {
		t.Error("a declaration regression must not be re-dispatchable: the same tree answers identically")
	}
	if req.Step != "implementation" {
		t.Errorf("Step = %q, want the step that was about to commit", req.Step)
	}
	if !strings.Contains(req.Reason, "no arena can start without") || !strings.Contains(req.Reason, "fit") {
		t.Errorf("Reason = %q, want it to name the required gate that went and what that costs", req.Reason)
	}
	for _, want := range []string{"fit, integration", "--- a/bin/gate"} {
		if !strings.Contains(req.Details, want) {
			t.Errorf("Details = %q, want it to carry %q — both lists and the diff that caused them to differ", req.Details, want)
		}
	}
}

// A regression with no required gate in it still parks, and says so without
// claiming the arena cannot start.
func TestGateRegression_ParkRequestWithoutARequiredGate(t *testing.T) {
	reg := CheckGatesHeld(declares(GateIntegration, GateFit, GateCovered), declares(GateIntegration, GateFit))
	if reg == nil {
		t.Fatal("CheckGatesHeld() found no regression")
	}
	req := reg.ParkRequest("implementation", "")

	if req.Kind != ParkRefused {
		t.Errorf("Kind = %q, want %q", req.Kind, ParkRefused)
	}
	if strings.Contains(req.Reason, "no arena can start without") {
		t.Errorf("Reason = %q, want no claim that the arena cannot start — `covered` is the project's own", req.Reason)
	}
	if !strings.Contains(req.Reason, "covered") {
		t.Errorf("Reason = %q, want it to name the gate that went", req.Reason)
	}
	// No diff offered, so none is fabricated — the lists are still there.
	if strings.Contains(req.Details, "the diff that caused it") {
		t.Errorf("Details = %q, want no diff section when the caller had none", req.Details)
	}
	if !strings.Contains(req.Details, "covered, fit, integration") {
		t.Errorf("Details = %q, want the before-listing", req.Details)
	}
}
