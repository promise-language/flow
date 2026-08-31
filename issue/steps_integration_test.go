package issue

import (
	"context"
	"errors"
	"testing"

	"github.com/promise-language/flow"
)

// ---------------------------------------------------------------------------
// Test doubles for integration steps.
// ---------------------------------------------------------------------------

// integrationWorktree extends fakeWorktree with PRFinder and
// MergeResultPreparer capabilities.
type integrationWorktree struct {
	fakeWorktree

	// PR lookup
	prURL            string
	prMergeCommitSHA string
	findPRErr        error

	// Merge simulation
	mergePrepped    bool
	mergePrepErr    error
	revertPrepErr   error
	revertPrepCalls int

	// Merge execution
	merged   bool
	mergeErr error
}

func newIntegrationWorktree() *integrationWorktree {
	return &integrationWorktree{
		fakeWorktree: *resumedWorktree(),
		prURL:        "https://example.invalid/pr/1",
	}
}

// Request returns the worktree itself, which implements both RequestManager
// and PRFinder.
func (w *integrationWorktree) Request() flow.RequestManager { return w }

func (w *integrationWorktree) Open(_ context.Context, _, _, body string) (string, error) {
	w.fakeWorktree.calls = append(w.fakeWorktree.calls, "open")
	w.fakeWorktree.opened, w.fakeWorktree.openBody = true, body
	return "https://example.invalid/pr/1", nil
}

func (w *integrationWorktree) Merge(_ context.Context, _ string) error {
	w.fakeWorktree.calls = append(w.fakeWorktree.calls, "merge")
	if w.mergeErr != nil {
		return w.mergeErr
	}
	w.merged = true
	return nil
}

func (w *integrationWorktree) FindPR(_ context.Context) (flow.PRInfo, error) {
	if w.findPRErr != nil {
		return flow.PRInfo{}, w.findPRErr
	}
	return flow.PRInfo{
		URL:            w.prURL,
		MergeCommitSHA: w.prMergeCommitSHA,
	}, nil
}

func (w *integrationWorktree) PrepareMergeResult(_ context.Context, _ string) error {
	w.fakeWorktree.calls = append(w.fakeWorktree.calls, "merge-prep")
	if w.mergePrepErr != nil {
		return w.mergePrepErr
	}
	w.mergePrepped = true
	return nil
}

func (w *integrationWorktree) RevertMergePrep(_ context.Context) error {
	w.revertPrepCalls++
	return w.revertPrepErr
}

// Worktree returns the integrationWorktree itself for steps that call
// ctx.Worktree().
func (w *integrationWorktree) asWorktree() flow.Worktree { return w }

// integrationCtx returns a fakeCtx wired to an integrationWorktree, with the
// contributor artifacts already resolved (the integration phase comes after
// the contributor phase).
func integrationCtx(wt *integrationWorktree) *fakeCtx {
	return &fakeCtx{
		item: flow.Item{ID: "42", Type: "task", Title: "widget is broken"},
		wt:   &wt.fakeWorktree,
		arts: map[flow.ArtifactId]flow.ArtifactRecord{
			"plan":           {Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "the plan"},
			"branch":         {Resolved: true, Type: flow.ArtifactCommitHash, CommitHash: "base"},
			"implementation": {Resolved: true, Type: flow.ArtifactCommitHash, CommitHash: "sha-1"},
			"review":         {Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "looks good"},
			"coverage":       {Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "95%"},
		},
	}
}

// integrationCtxWithWorktree returns a fakeCtx that delegates Worktree() to
// the integrationWorktree directly (so the PRFinder and MergeResultPreparer
// interfaces are visible to type assertions).
type integrationFakeCtx struct {
	*fakeCtx
	iwt *integrationWorktree
}

func (c *integrationFakeCtx) Worktree() (flow.Worktree, error) { return c.iwt, nil }

func newIntegrationCtx(wt *integrationWorktree) *integrationFakeCtx {
	return &integrationFakeCtx{fakeCtx: integrationCtx(wt), iwt: wt}
}

// ---------------------------------------------------------------------------
// stepVerifyMerge.
// ---------------------------------------------------------------------------

func TestStepVerifyMerge_GatePassesMergeResultAccepted(t *testing.T) {
	wt := newIntegrationWorktree()
	wt.envelope = []byte(`{"coverage": 95}`)
	wt.thresholds = []byte(`{"coverage": 80}`)
	ctx := newIntegrationCtx(wt)

	if err := testBuilder(t).stepVerifyMerge(ctx); err != nil {
		t.Fatalf("stepVerifyMerge: %v", err)
	}
	if !ctx.didResolve {
		t.Fatal("step did not resolve")
	}
	if ctx.resolved.Type != flow.ArtifactMarkdown {
		t.Errorf("resolved a %v, want markdown", ctx.resolved.Type)
	}
	// The merge simulation should have been prepared and reverted.
	if !wt.mergePrepped {
		t.Error("merge result was not prepared")
	}
	if wt.revertPrepCalls != 1 {
		t.Errorf("RevertMergePrep called %d times, want 1", wt.revertPrepCalls)
	}
	// The carry-through caveat should have been notified.
	found := false
	for _, n := range ctx.notices {
		if n == "this binary carries through to merge — this is not independent review" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("carry-through caveat not notified; notices = %v", ctx.notices)
	}
}

func TestStepVerifyMerge_GateRefuses(t *testing.T) {
	wt := newIntegrationWorktree()
	wt.judgeRefuses = true
	wt.judgeDetail = "coverage below threshold"
	wt.thresholds = []byte(`{"coverage": 80}`)
	ctx := newIntegrationCtx(wt)

	err := testBuilder(t).stepVerifyMerge(ctx)
	if err == nil {
		t.Fatal("expected error when gate refuses")
	}
	if ctx.didResolve {
		t.Error("step resolved despite gate refusal")
	}
}

func TestStepVerifyMerge_GateError(t *testing.T) {
	wt := newIntegrationWorktree()
	wt.gateErr = errors.New("gate binary not found")
	ctx := newIntegrationCtx(wt)

	err := testBuilder(t).stepVerifyMerge(ctx)
	if err == nil {
		t.Fatal("expected error when gate cannot run")
	}
	if ctx.didResolve {
		t.Error("step resolved despite gate error")
	}
}

func TestStepVerifyMerge_GateNotMeasured(t *testing.T) {
	wt := newIntegrationWorktree()
	wt.gateOutcome = map[flow.GateName]flow.GateOutcome{
		flow.GateIntegration: flow.OutcomeDied,
	}
	ctx := newIntegrationCtx(wt)

	err := testBuilder(t).stepVerifyMerge(ctx)
	if err == nil {
		t.Fatal("expected error when gate outcome is not measured")
	}
	if ctx.didResolve {
		t.Error("step resolved despite non-measured outcome")
	}
}

func TestStepVerifyMerge_JudgeError(t *testing.T) {
	wt := newIntegrationWorktree()
	wt.judgeErr = errors.New("judge binary broken")
	ctx := newIntegrationCtx(wt)

	err := testBuilder(t).stepVerifyMerge(ctx)
	if err == nil {
		t.Fatal("expected error when judge cannot answer")
	}
	if ctx.didResolve {
		t.Error("step resolved despite judge error")
	}
}

func TestStepVerifyMerge_MergeConflicts(t *testing.T) {
	wt := newIntegrationWorktree()
	wt.mergePrepErr = errors.New("CONFLICT (content): merge conflict in main.go")
	ctx := newIntegrationCtx(wt)

	err := testBuilder(t).stepVerifyMerge(ctx)
	if err == nil {
		t.Fatal("expected error when merge simulation has conflicts")
	}
	if ctx.didResolve {
		t.Error("step resolved despite merge conflicts")
	}
}

func TestStepVerifyMerge_RevertCalledEvenOnGateFailure(t *testing.T) {
	wt := newIntegrationWorktree()
	wt.gateErr = errors.New("gate failed")
	ctx := newIntegrationCtx(wt)

	_ = testBuilder(t).stepVerifyMerge(ctx)
	// RevertMergePrep should still be called (via defer) even when the gate errors.
	if wt.revertPrepCalls != 1 {
		t.Errorf("RevertMergePrep called %d times, want 1 (should be called on error path too)", wt.revertPrepCalls)
	}
}

// stepVerifyMerge on a worktree that does NOT implement MergeResultPreparer —
// the gate runs on the branch directly, with no merge simulation.
func TestStepVerifyMerge_WithoutMergeResultPreparer(t *testing.T) {
	wt := resumedWorktree()
	wt.envelope = []byte(`{"coverage": 95}`)
	wt.thresholds = []byte(`{"coverage": 80}`)
	// Use fakeCtx directly — fakeWorktree does NOT implement
	// MergeResultPreparer, so the if-prep branch is skipped.
	ctx := &fakeCtx{
		item: flow.Item{ID: "42", Type: "task", Title: "widget is broken"},
		wt:   wt,
		arts: map[flow.ArtifactId]flow.ArtifactRecord{
			"plan":           {Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "the plan"},
			"branch":         {Resolved: true, Type: flow.ArtifactCommitHash, CommitHash: "base"},
			"implementation": {Resolved: true, Type: flow.ArtifactCommitHash, CommitHash: "sha-1"},
			"review":         {Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "looks good"},
			"coverage":       {Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "95%"},
		},
	}

	if err := testBuilder(t).stepVerifyMerge(ctx); err != nil {
		t.Fatalf("stepVerifyMerge: %v", err)
	}
	if !ctx.didResolve {
		t.Fatal("step did not resolve")
	}
	// No merge prep calls should have happened — fakeWorktree is not a
	// MergeResultPreparer.
	for _, c := range wt.calls {
		if c == "merge-prep" {
			t.Error("merge-prep should not have been called on a non-MergeResultPreparer worktree")
		}
	}
}

// ---------------------------------------------------------------------------
// stepMerge.
// ---------------------------------------------------------------------------

func TestStepMerge_HappyPath(t *testing.T) {
	wt := newIntegrationWorktree()
	ctx := newIntegrationCtx(wt)

	if err := testBuilder(t).stepMerge(ctx); err != nil {
		t.Fatalf("stepMerge: %v", err)
	}
	if !wt.merged {
		t.Error("pull request was not merged")
	}
}

func TestStepMerge_PRNotFound(t *testing.T) {
	wt := newIntegrationWorktree()
	wt.findPRErr = errors.New("no pull request found")
	ctx := newIntegrationCtx(wt)

	err := testBuilder(t).stepMerge(ctx)
	if err == nil {
		t.Fatal("expected error when PR not found")
	}
}

func TestStepMerge_MergeFails(t *testing.T) {
	wt := newIntegrationWorktree()
	wt.mergeErr = errors.New("merge failed: required status check is failing")
	ctx := newIntegrationCtx(wt)

	err := testBuilder(t).stepMerge(ctx)
	if err == nil {
		t.Fatal("expected error when merge fails")
	}
	if wt.merged {
		t.Error("merged flag should not be set when merge fails")
	}
}

// ---------------------------------------------------------------------------
// stepRecordMerge.
// ---------------------------------------------------------------------------

func TestStepRecordMerge_PRMerged(t *testing.T) {
	wt := newIntegrationWorktree()
	wt.prMergeCommitSHA = "abc123"
	ctx := newIntegrationCtx(wt)

	if err := testBuilder(t).stepRecordMerge(ctx); err != nil {
		t.Fatalf("stepRecordMerge: %v", err)
	}
	if !ctx.didResolve {
		t.Fatal("step did not resolve")
	}
	if ctx.resolved.Type != flow.ArtifactCommitHash {
		t.Errorf("resolved a %v, want a commit hash", ctx.resolved.Type)
	}
	if ctx.resolved.CommitHash != "abc123" {
		t.Errorf("resolved commit %q, want %q", ctx.resolved.CommitHash, "abc123")
	}
}

func TestStepRecordMerge_PRNotYetMerged(t *testing.T) {
	wt := newIntegrationWorktree()
	wt.prMergeCommitSHA = "" // not merged yet
	ctx := newIntegrationCtx(wt)

	err := testBuilder(t).stepRecordMerge(ctx)
	if err == nil {
		t.Fatal("expected error when PR has not merged yet")
	}
	if ctx.didResolve {
		t.Error("step resolved despite PR not being merged")
	}
}

func TestStepRecordMerge_PRNotFound(t *testing.T) {
	wt := newIntegrationWorktree()
	wt.findPRErr = errors.New("no pull request found")
	ctx := newIntegrationCtx(wt)

	err := testBuilder(t).stepRecordMerge(ctx)
	if err == nil {
		t.Fatal("expected error when PR not found")
	}
}
