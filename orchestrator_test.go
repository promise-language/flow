package flow

import (
	"context"
	"errors"
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
func (stubWorktreeBase) RevParse(context.Context, Revision) (CommitSha, error) {
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
