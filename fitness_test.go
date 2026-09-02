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
func (s *stubWorktree) Branch(context.Context, string, string) (bool, error) {
	panic("not implemented")
}
func (s *stubWorktree) CurrentBranch(context.Context) (string, error) {
	panic("not implemented")
}
func (s *stubWorktree) IsDirty(context.Context) (bool, error) { panic("not implemented") }
func (s *stubWorktree) Stage(context.Context) error           { panic("not implemented") }
func (s *stubWorktree) Commit(context.Context, string) error  { panic("not implemented") }
func (s *stubWorktree) Push(context.Context) error            { panic("not implemented") }
func (s *stubWorktree) RevParse(context.Context, string) (string, error) {
	panic("not implemented")
}
func (s *stubWorktree) Verify(context.Context) error                 { panic("not implemented") }
func (s *stubWorktree) CapturePatch(context.Context) ([]byte, error) { panic("not implemented") }
func (s *stubWorktree) Request() RequestManager                      { return nil }

func TestCheckFit_AcceptableReturnsNil(t *testing.T) {
	wt := &stubWorktree{
		runGate: func(_ context.Context, name GateName) (GateRun, error) {
			return GateRun{Gate: name, Outcome: OutcomeMeasured}, nil
		},
		judge: func(_ context.Context, _ GateRun) (GateVerdict, error) {
			return GateVerdict{Acceptable: true}, nil
		},
	}
	if err := CheckFit(context.Background(), wt); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckFit_UnacceptableReturnsErrUnfit(t *testing.T) {
	detail := "coverage 42% < 80%"
	wt := &stubWorktree{
		runGate: func(_ context.Context, name GateName) (GateRun, error) {
			return GateRun{Gate: name, Outcome: OutcomeMeasured}, nil
		},
		judge: func(_ context.Context, _ GateRun) (GateVerdict, error) {
			return GateVerdict{Acceptable: false, Detail: detail}, nil
		},
	}
	err := CheckFit(context.Background(), wt)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrUnfit) {
		t.Fatalf("expected ErrUnfit, got %v", err)
	}
	want := fmt.Sprintf("%s: %s", detail, ErrUnfit)
	if err.Error() != want {
		t.Fatalf("error message = %q, want %q", err.Error(), want)
	}
}

func TestCheckFit_RunGateErrorIsFailOpen(t *testing.T) {
	wt := &stubWorktree{
		runGate: func(context.Context, GateName) (GateRun, error) {
			return GateRun{}, errors.New("boom")
		},
		judge: func(context.Context, GateRun) (GateVerdict, error) {
			t.Fatal("Judge must not be called when RunGate errors")
			return GateVerdict{}, nil
		},
	}
	if err := CheckFit(context.Background(), wt); err != nil {
		t.Fatalf("expected nil (fail-open), got %v", err)
	}
}

func TestCheckFit_NonMeasuredOutcomeIsFailOpen(t *testing.T) {
	wt := &stubWorktree{
		runGate: func(_ context.Context, name GateName) (GateRun, error) {
			return GateRun{Gate: name, Outcome: OutcomeDied}, nil
		},
		judge: func(context.Context, GateRun) (GateVerdict, error) {
			t.Fatal("Judge must not be called for non-measured outcome")
			return GateVerdict{}, nil
		},
	}
	if err := CheckFit(context.Background(), wt); err != nil {
		t.Fatalf("expected nil (fail-open), got %v", err)
	}
}

func TestCheckFit_JudgeErrorIsFailOpen(t *testing.T) {
	wt := &stubWorktree{
		runGate: func(_ context.Context, name GateName) (GateRun, error) {
			return GateRun{Gate: name, Outcome: OutcomeMeasured}, nil
		},
		judge: func(context.Context, GateRun) (GateVerdict, error) {
			return GateVerdict{}, errors.New("judge broke")
		},
	}
	if err := CheckFit(context.Background(), wt); err != nil {
		t.Fatalf("expected nil (fail-open), got %v", err)
	}
}

// Ensure stubWorktree satisfies the Worktree interface at compile time.
var _ Worktree = (*stubWorktree)(nil)
