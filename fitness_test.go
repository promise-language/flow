package flow

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// stubWorktree is a minimal test double for the Worktree interface.
// Only RunGate and Judge carry behaviour; every other method panics.
type stubWorktree struct {
	runGate func(ctx context.Context, name GateName) (GateRun, error)
	judge   func(ctx context.Context, run GateRun) (GateVerdict, error)
}

func (s *stubWorktree) RunGate(ctx context.Context, name GateName) (GateRun, error) {
	return s.runGate(ctx, name)
}
func (s *stubWorktree) Judge(ctx context.Context, run GateRun) (GateVerdict, error) {
	return s.judge(ctx, run)
}

// Stubs for the rest of the Worktree interface.
func (s *stubWorktree) Branch(context.Context, BranchName, BranchName) (bool, error) {
	panic("not implemented")
}
func (s *stubWorktree) CurrentBranch(context.Context) (BranchName, error) {
	panic("not implemented")
}
func (s *stubWorktree) IsDirty(context.Context) (bool, error) { panic("not implemented") }
func (s *stubWorktree) Stage(context.Context) error           { panic("not implemented") }
func (s *stubWorktree) Commit(context.Context, string) error  { panic("not implemented") }
func (s *stubWorktree) Push(context.Context) error            { panic("not implemented") }
func (s *stubWorktree) RevParse(context.Context, Revision) (CommitSha, error) {
	panic("not implemented")
}
func (s *stubWorktree) Run(context.Context, CommandName) (CommandRun, error) {
	panic("not implemented")
}
func (s *stubWorktree) CapturePatch(context.Context) ([]byte, error) { panic("not implemented") }
func (s *stubWorktree) Request() RequestManager                      { return nil }

// measuredFit builds a stub whose fit gate measures and whose judge answers.
func measuredFit(t *testing.T, acceptable bool, detail string) *stubWorktree {
	t.Helper()
	return &stubWorktree{
		runGate: func(_ context.Context, name GateName) (GateRun, error) {
			if name != GateFit {
				t.Errorf("RunGate asked for %q, want %q", name, GateFit)
			}
			return GateRun{Gate: name, Outcome: OutcomeMeasured}, nil
		},
		judge: func(_ context.Context, _ GateRun) (GateVerdict, error) {
			return GateVerdict{Acceptable: acceptable, Detail: detail}, nil
		},
	}
}

func TestCheckFit_MeasuredAndAcceptableIsFit(t *testing.T) {
	if err := CheckFit(context.Background(), measuredFit(t, true, "")); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckFit_MeasuredAndUnacceptableIsUnfitWithDetail(t *testing.T) {
	detail := "disk 96% full"
	err := CheckFit(context.Background(), measuredFit(t, false, detail))
	if !errors.Is(err, ErrUnfit) {
		t.Fatalf("expected ErrUnfit, got %v", err)
	}
	want := fmt.Sprintf("%s: %s", detail, ErrUnfit)
	if err.Error() != want {
		t.Fatalf("error message = %q, want %q", err.Error(), want)
	}
}

// The three fail-CLOSED paths. Each of them once returned nil — "cannot check →
// do not refuse" — which treated a broken fit gate as a fit machine, the one
// state `fit` exists to stop anyone proceeding from. No outcome is not a
// passing outcome.

func TestCheckFit_RunGateErrorIsUnfit(t *testing.T) {
	wt := &stubWorktree{
		runGate: func(context.Context, GateName) (GateRun, error) {
			return GateRun{}, errors.New("boom")
		},
		judge: func(context.Context, GateRun) (GateVerdict, error) {
			t.Fatal("Judge must not be called when RunGate errors — there is no run to judge")
			return GateVerdict{}, nil
		},
	}
	err := CheckFit(context.Background(), wt)
	if !errors.Is(err, ErrUnfit) {
		t.Fatalf("RunGate error: got %v, want ErrUnfit — a fit gate that cannot run has not reported the machine fit", err)
	}
}

// Every non-measured outcome, enumerated from AllOutcomes rather than listed,
// so a sixth outcome does not silently acquire the fail-open behaviour this
// test exists to prevent.
func TestCheckFit_EveryNonMeasuredOutcomeIsUnfit(t *testing.T) {
	for _, outcome := range AllOutcomes() {
		if outcome == OutcomeMeasured {
			continue
		}
		t.Run(string(outcome), func(t *testing.T) {
			wt := &stubWorktree{
				runGate: func(_ context.Context, name GateName) (GateRun, error) {
					return GateRun{Gate: name, Outcome: outcome, Detail: "detail for " + string(outcome)}, nil
				},
				judge: func(context.Context, GateRun) (GateVerdict, error) {
					t.Fatalf("Judge must not be called for outcome %s — only a measurement may be judged", outcome)
					return GateVerdict{}, nil
				},
			}
			err := CheckFit(context.Background(), wt)
			if !errors.Is(err, ErrUnfit) {
				t.Fatalf("outcome %s: got %v, want ErrUnfit", outcome, err)
			}
		})
	}
}

func TestCheckFit_JudgeErrorIsUnfit(t *testing.T) {
	wt := &stubWorktree{
		runGate: func(_ context.Context, name GateName) (GateRun, error) {
			return GateRun{Gate: name, Outcome: OutcomeMeasured}, nil
		},
		judge: func(context.Context, GateRun) (GateVerdict, error) {
			return GateVerdict{}, errors.New("judge broke")
		},
	}
	err := CheckFit(context.Background(), wt)
	if !errors.Is(err, ErrUnfit) {
		t.Fatalf("Judge error: got %v, want ErrUnfit — an unanswerable judge has not reported the machine fit", err)
	}
}

// Ensure stubWorktree satisfies the Worktree interface at compile time.
var _ Worktree = (*stubWorktree)(nil)
