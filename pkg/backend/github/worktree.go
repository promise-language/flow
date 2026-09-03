package github

import (
	"context"
	"errors"
	"fmt"

	"github.com/promise-language/flow"
)

// Worktree returns the worktree impl for a claim. The Worktree is stateless
// beyond the underlying local git + the backend's GH client — fine to call
// repeatedly within an invocation; cli/StepCtx caches the return.
func (b *Backend) Worktree(ctx context.Context, claim flow.Claim) (flow.Worktree, error) {
	issueNum, err := b.issueNumber(claim.ItemRef)
	if err != nil {
		return nil, err
	}
	return &worktree{
		b:        b,
		claim:    claim,
		issueNum: issueNum,
	}, nil
}

type worktree struct {
	b        *Backend
	claim    flow.Claim
	issueNum int

	// mergeRestorePoint is the HEAD SHA saved by PrepareMergeResult, used by
	// RevertMergePrep to undo the local merge simulation.
	mergeRestorePoint string
}

func (w *worktree) Branch(ctx context.Context, name, base string) (bool, error) {
	cur, err := w.b.git.CurrentBranch(ctx)
	if err != nil {
		return false, err
	}
	if cur == name {
		return false, nil
	}
	dirty, err := w.b.git.IsDirty(ctx)
	if err != nil {
		return false, err
	}
	if dirty {
		return false, errors.New("worktree is dirty; cannot switch branches")
	}
	exists, err := w.b.git.BranchExists(ctx, name)
	if err != nil {
		return false, err
	}
	if exists {
		return false, w.b.git.Checkout(ctx, name, "", false)
	}
	if err := w.b.git.Checkout(ctx, name, base, true); err != nil {
		return false, err
	}
	return true, nil
}

func (w *worktree) CurrentBranch(ctx context.Context) (string, error) {
	return w.b.git.CurrentBranch(ctx)
}

func (w *worktree) IsDirty(ctx context.Context) (bool, error) {
	return w.b.git.IsDirty(ctx)
}

func (w *worktree) Commit(ctx context.Context, msg string) error {
	return w.b.git.Commit(ctx, msg)
}

func (w *worktree) Push(ctx context.Context) error {
	return w.b.out.Push(ctx)
}

// Request exposes the pull-request management surface. The github backend
// supports pull requests, so this returns the worktree itself (its Open/
// Merge methods satisfy flow.RequestManager).
func (w *worktree) Request() flow.RequestManager { return w }

func (w *worktree) Open(ctx context.Context, base, title, body string) (string, error) {
	// Ensure the claim branch is current + pushed.
	expected := w.b.claimBranch(w.issueNum)
	branch, err := w.b.git.CurrentBranch(ctx)
	if err != nil {
		return "", err
	}
	if branch != expected {
		return "", fmt.Errorf("worktree.Open: current branch %q != claim branch %q", branch, expected)
	}
	if err := w.b.out.Push(ctx); err != nil {
		return "", err
	}

	url, err := w.b.out.OpenPullRequest(ctx, base, expected, title, body)
	if err != nil {
		return "", err
	}

	// Side-effect: backend marks pr-open in the state comment.
	if err := w.b.markSignalSetOnState(ctx, w.claim, "pr-open"); err != nil {
		// Non-fatal — the LoadState poll will pick up the signal on the
		// next run.
		_ = err
	}
	return url, nil
}

// FindPR implements flow.PRFinder: look up the pull request for the current
// claim branch.
func (w *worktree) FindPR(ctx context.Context) (flow.PRInfo, error) {
	pr, err := w.b.findPRForBranch(ctx, w.b.claimBranch(w.issueNum))
	if err != nil {
		return flow.PRInfo{}, err
	}
	if pr == nil {
		return flow.PRInfo{}, fmt.Errorf("no pull request found for branch %q", w.b.claimBranch(w.issueNum))
	}
	return flow.PRInfo{
		URL:            pr.GetHTMLURL(),
		MergeCommitSHA: pr.GetMergeCommitSHA(),
	}, nil
}

func (w *worktree) Merge(ctx context.Context, url string) error {
	if err := w.b.out.MergePullRequest(ctx, url); err != nil {
		return err
	}
	if err := w.b.markSignalSetOnState(ctx, w.claim, "pr-merged"); err != nil {
		_ = err
	}
	return nil
}

// PrepareMergeResult implements flow.MergeResultPreparer: create a local merge
// of origin/<base> into the current branch so the integration gate measures the
// merge result, not just the branch.
func (w *worktree) PrepareMergeResult(ctx context.Context, base string) error {
	head, err := w.b.git.HeadSHA(ctx)
	if err != nil {
		return fmt.Errorf("save restore point: %w", err)
	}
	w.mergeRestorePoint = head
	if err := w.b.git.Fetch(ctx, "origin"); err != nil {
		return err
	}
	if err := w.b.git.MergeLocal(ctx, "origin/"+base); err != nil {
		return fmt.Errorf("merge simulation with origin/%s: %w", base, err)
	}
	return nil
}

// RevertMergePrep implements flow.MergeResultPreparer: restore the branch to
// the state it was in before the merge simulation.
func (w *worktree) RevertMergePrep(ctx context.Context) error {
	if w.mergeRestorePoint == "" {
		return nil
	}
	return w.b.git.ResetHardTo(ctx, w.mergeRestorePoint)
}

// RebuildTools implements flow.ToolsRebuilder: run ./make in the worktree
// to rebuild dev tools against the current tree.  The meta-builder runs via
// 'go run' and is never stale itself; it short-circuits when tools are
// already up to date.
func (w *worktree) RebuildTools(ctx context.Context) error {
	return w.run(ctx, "rebuild tools", []string{"./make"})
}

// Verify runs cfg.VerifyCmd in cfg.WorktreeDir. Exit-0 → success.
func (w *worktree) Verify(ctx context.Context) error {
	if len(w.b.cfg.VerifyCmd) == 0 {
		return errors.New("worktree.Verify: cfg.VerifyCmd is empty")
	}
	return w.run(ctx, "verify", w.b.cfg.VerifyCmd)
}

// RunGate runs the named gate in cfg.WorktreeDir and reports what the runner
// observed. Not exit-0 → pass: the gate's exit code is carried as a diagnostic
// and decided on by nothing. See runGate.
//
// Gates are reached through the project's gate entry point by name rather than
// each being configured separately. That is what makes the parts addressable:
// a step fixing one failing suite asks for that suite, without the project
// having had to enumerate every part in advance.
func (w *worktree) RunGate(ctx context.Context, name flow.GateName) (flow.GateRun, error) {
	if !name.Valid() {
		return flow.GateRun{}, fmt.Errorf("worktree.RunGate: %q is not a declared gate name", name)
	}

	// Capture tracked state before spawning so the runner can detect a gate
	// that modified the worktree. See docs/gates-and-commands.md § "The
	// non-modification rule is checked, not assumed".
	before, err := w.b.git.StatusPorcelain(ctx)
	if err != nil {
		return flow.GateRun{}, fmt.Errorf("worktree.RunGate: cannot snapshot worktree before gate: %w", err)
	}

	argv := append(append([]string{}, gateEntryPoint...), string(name), envelopeFlag)
	run, err := runGate(ctx, w.b.cfg.WorktreeDir, name, argv, w.b.cfg.GateTimeout)
	if err != nil {
		return run, err
	}

	// Only override a measured outcome. The four non-measured outcomes are
	// already failures; overriding them would lose attribution (e.g.,
	// timeout → broke-contract sends the wrong person to investigate).
	if run.Outcome != flow.OutcomeMeasured {
		return run, nil
	}

	after, err := w.b.git.StatusPorcelain(ctx)
	if err != nil {
		run.Outcome = flow.OutcomeBrokeContract
		run.Detail = fmt.Sprintf("cannot verify worktree integrity after gate: %v", err)
		return run, nil
	}

	if before != after {
		run.Outcome = flow.OutcomeBrokeContract
		run.Detail = "the gate modified the worktree:\n" + after
		return run, nil
	}

	return run, nil
}

// Judge asks the project whether a measurement is acceptable, by exec'ing its
// judging entry point with the envelope on stdin. See flow.Worktree.Judge: the
// SDK never computes the verdict, and it keeps the spawn.
//
// Only a measured run may be judged, and that is checked here rather than by
// the judge — the four other outcomes mean no measurement exists, so there is
// nothing to hand over, and a judge asked to answer about one would have to
// invent something.
func (w *worktree) Judge(ctx context.Context, run flow.GateRun) (flow.GateVerdict, error) {
	if !run.Gate.Valid() {
		return flow.GateVerdict{}, fmt.Errorf("worktree.Judge: %q is not a declared gate name", run.Gate)
	}
	if run.Outcome != flow.OutcomeMeasured {
		observed := string(run.Outcome)
		if observed == "" {
			observed = "no outcome at all"
		}
		return flow.GateVerdict{}, fmt.Errorf(
			"worktree.Judge: the run of gate %s reports %s, and only a %s run may be judged — "+
				"a run that measured nothing has not reported that the tree is bad",
			run.Gate, observed, flow.OutcomeMeasured)
	}
	argv := append(append([]string{}, judgeEntryPoint...), string(run.Gate), verdictFlag)
	return askJudge(ctx, w.b.cfg.WorktreeDir, run, argv, w.b.cfg.GateTimeout)
}

// gateEntryPoint is how a project exposes its gates. Not configurable: the
// names are fixed, so the way to reach them is too.
var gateEntryPoint = []string{"bin/gate"}

// judgeEntryPoint is how a project exposes the layer that holds its
// thresholds. Not configurable, for the same reason the gate entry point is
// not: the SDK asks one way, so two callers asking about the same measurement
// cannot get different answers and both be right.
var judgeEntryPoint = []string{"bin/run"}

// run executes one configured command in the worktree.
//
// Verify's, and not gates'. The seam it goes through reports (stdout, stderr,
// error), which is all a COMMAND needs — it either exited 0 or it did not. A
// runner has to observe more than that: the seam hides a failure to spawn
// inside the same error as a failure to finish, and discards the ProcessState
// that says whether a signal ended the process. See runGate.
func (w *worktree) run(ctx context.Context, what string, args []string) error {
	stdout, stderr, err := w.b.git.runner(ctx, w.b.cfg.WorktreeDir, args[0], args[1:]...)
	if err != nil {
		return fmt.Errorf("%s failed: %w\noutput:\n%s%s", what, err, string(stdout), string(stderr))
	}
	return nil
}

func (w *worktree) CapturePatch(ctx context.Context) ([]byte, error) {
	return w.b.git.CapturePatch(ctx)
}

func (w *worktree) RevParse(ctx context.Context, rev string) (string, error) {
	return w.b.git.RevParse(ctx, rev)
}

func (w *worktree) Stage(ctx context.Context) error { return w.b.git.StageAll(ctx) }
