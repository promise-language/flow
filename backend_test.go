package flow

import (
	"context"
	"errors"
	"testing"
)

// ---------------------------------------------------------------------------
// FindPR convenience function — three dispatch paths.
// ---------------------------------------------------------------------------

// stubWorktreeNilRequest returns nil from Request(), modelling a backend that
// does not support pull request operations at all.
type stubWorktreeNilRequest struct{ stubWorktreeBase }

func (w *stubWorktreeNilRequest) Request() RequestManager { return nil }

// stubWorktreeNoPRFinder has a non-nil RequestManager that does NOT implement
// PRFinder — i.e. a backend that supports Open/Merge but not FindPR.
type stubWorktreeNoPRFinder struct{ stubWorktreeBase }

type plainRequestManager struct{}

func (plainRequestManager) Open(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (plainRequestManager) Merge(context.Context, string) error { return nil }

func (w *stubWorktreeNoPRFinder) Request() RequestManager { return plainRequestManager{} }

// stubWorktreeWithPRFinder has a RequestManager that implements PRFinder.
type stubWorktreeWithPRFinder struct {
	stubWorktreeBase
	prInfo PRInfo
	prErr  error
}

type prFinderRM struct {
	info PRInfo
	err  error
}

func (r prFinderRM) Open(context.Context, string, string, string) (string, error) { return "", nil }
func (r prFinderRM) Merge(context.Context, string) error                          { return nil }
func (r prFinderRM) FindPR(context.Context) (PRInfo, error)                       { return r.info, r.err }

func (w *stubWorktreeWithPRFinder) Request() RequestManager {
	return prFinderRM{info: w.prInfo, err: w.prErr}
}

// stubWorktreeBase satisfies the Worktree interface methods that FindPR does
// not call but the compiler requires.
type stubWorktreeBase struct{}

func (stubWorktreeBase) Branch(context.Context, string, string) (bool, error) { return false, nil }
func (stubWorktreeBase) CurrentBranch(context.Context) (string, error)        { return "", nil }
func (stubWorktreeBase) Commit(context.Context, string) error                 { return nil }
func (stubWorktreeBase) Stage(context.Context) error                          { return nil }
func (stubWorktreeBase) Push(context.Context) error                           { return nil }
func (stubWorktreeBase) Verify(context.Context) error                         { return nil }
func (stubWorktreeBase) RevParse(context.Context, string) (string, error)     { return "", nil }
func (stubWorktreeBase) RunGate(context.Context, GateName) (GateRun, error)   { return GateRun{}, nil }
func (stubWorktreeBase) Judge(context.Context, GateRun) (GateVerdict, error) {
	return GateVerdict{}, nil
}
func (stubWorktreeBase) IsDirty(context.Context) (bool, error)        { return false, nil }
func (stubWorktreeBase) CapturePatch(context.Context) ([]byte, error) { return nil, nil }
func (stubWorktreeBase) Request() RequestManager                      { return nil }

func TestFindPR_NilRequest(t *testing.T) {
	wt := &stubWorktreeNilRequest{}
	_, err := FindPR(context.Background(), wt)
	if !errors.Is(err, ErrRequestNotSupported) {
		t.Errorf("FindPR with nil Request: got %v, want ErrRequestNotSupported", err)
	}
}

func TestFindPR_NotAPRFinder(t *testing.T) {
	wt := &stubWorktreeNoPRFinder{}
	_, err := FindPR(context.Background(), wt)
	if !errors.Is(err, ErrRequestNotSupported) {
		t.Errorf("FindPR with non-PRFinder: got %v, want ErrRequestNotSupported", err)
	}
}

func TestFindPR_Delegates(t *testing.T) {
	wt := &stubWorktreeWithPRFinder{
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
	wt := &stubWorktreeWithPRFinder{
		prErr: errors.New("GitHub API rate limit"),
	}
	_, err := FindPR(context.Background(), wt)
	if err == nil {
		t.Fatal("expected error from PRFinder to propagate")
	}
	if errors.Is(err, ErrRequestNotSupported) {
		t.Error("error should be the PRFinder's error, not ErrRequestNotSupported")
	}
}
