package github

import (
	"context"
	"errors"
	"fmt"

	"github.com/promise-language/flow"
)

// Worktree returns the local-git surface for the item's arena. It is stateless
// beyond the underlying local git + the orchestrator's GH client — fine to call
// repeatedly within an invocation; cli/StepCtx caches the return.
//
// Addressed by ref and not by claim: NO GATE REQUIRES A CLAIM. Running one is a
// measurement, and measuring is not a privileged act — which is what lets the
// pre-claim `fit` check ask for a worktree before an item is taken.
func (b *Orchestrator) Worktree(ctx context.Context, ref flow.ItemRef) (flow.Worktree, error) {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return nil, err
	}
	return &worktree{
		b:        b,
		ref:      ref,
		issueNum: issueNum,
	}, nil
}

type worktree struct {
	b        *Orchestrator
	ref      flow.ItemRef
	issueNum int

	// mergeRestorePoint is the HEAD SHA saved by PrepareMergeResult, used by
	// RevertMergePrep to undo the local merge simulation.
	mergeRestorePoint string
}

func (w *worktree) Branch(ctx context.Context, name, base flow.BranchName) (bool, error) {
	cur, err := w.b.git.CurrentBranch(ctx)
	if err != nil {
		return false, err
	}
	if flow.BranchName(cur) == name {
		return false, nil
	}
	dirty, err := w.b.git.IsDirty(ctx)
	if err != nil {
		return false, err
	}
	if dirty {
		return false, errors.New("worktree is dirty; cannot switch branches")
	}
	exists, err := w.b.git.BranchExists(ctx, string(name))
	if err != nil {
		return false, err
	}
	if exists {
		return false, w.b.git.Checkout(ctx, string(name), "", false)
	}
	if err := w.b.git.Checkout(ctx, string(name), string(base), true); err != nil {
		return false, err
	}
	return true, nil
}

func (w *worktree) CurrentBranch(ctx context.Context) (flow.BranchName, error) {
	br, err := w.b.git.CurrentBranch(ctx)
	return flow.BranchName(br), err
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

// Request exposes the pull-request surface. This orchestrator lands changes
// through pull requests, so it returns the worktree itself — one capability,
// not six.
func (w *worktree) Request() flow.RequestManager { return w }

func (w *worktree) Open(ctx context.Context, base flow.BranchName, title, body string) (flow.RequestUrl, error) {
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

	url, err := w.b.out.OpenPullRequest(ctx, string(base), expected, title, body)
	if err != nil {
		return "", err
	}

	// Side-effect: the orchestrator marks pr-open in the state comment.
	if err := w.b.markSignalSetOnState(ctx, w.ref, "pr-open"); err != nil {
		// Non-fatal — the Load poll will pick up the signal on the next run.
		_ = err
	}
	return flow.RequestUrl(url), nil
}

// FindPR looks up the pull request for the current claim branch.
func (w *worktree) FindPR(ctx context.Context) (flow.PRInfo, error) {
	pr, err := w.b.findPRForBranch(ctx, w.b.claimBranch(w.issueNum))
	if err != nil {
		return flow.PRInfo{}, err
	}
	if pr == nil {
		return flow.PRInfo{}, fmt.Errorf("no pull request found for branch %q", w.b.claimBranch(w.issueNum))
	}
	return flow.PRInfo{
		URL:            flow.RequestUrl(pr.GetHTMLURL()),
		MergeCommitSHA: flow.CommitSha(pr.GetMergeCommitSHA()),
	}, nil
}

func (w *worktree) Merge(ctx context.Context, url flow.RequestUrl) error {
	if err := w.b.out.MergePullRequest(ctx, string(url)); err != nil {
		return err
	}
	if err := w.b.markSignalSetOnState(ctx, w.ref, "pr-merged"); err != nil {
		_ = err
	}
	return nil
}

// PrepareMergeResult creates a local merge of origin/<base> into the current
// branch so the integration gate measures the merge result, not just the
// branch.
func (w *worktree) PrepareMergeResult(ctx context.Context, base flow.BranchName) error {
	head, err := w.b.git.HeadSHA(ctx)
	if err != nil {
		return fmt.Errorf("save restore point: %w", err)
	}
	w.mergeRestorePoint = head
	if err := w.b.git.Fetch(ctx, "origin"); err != nil {
		return err
	}
	if err := w.b.git.MergeLocal(ctx, "origin/"+string(base)); err != nil {
		return fmt.Errorf("merge simulation with origin/%s: %w", base, err)
	}
	return nil
}

// RevertMergePrep restores the branch to the state it was in before the merge
// simulation.
func (w *worktree) RevertMergePrep(ctx context.Context) error {
	if w.mergeRestorePoint == "" {
		return nil
	}
	return w.b.git.ResetHardTo(ctx, w.mergeRestorePoint)
}

// RebuildTools runs ./make in the worktree to rebuild dev tools against the
// current tree. The meta-builder runs via 'go run' and is never stale itself;
// it short-circuits when tools are already up to date.
func (w *worktree) RebuildTools(ctx context.Context) error {
	return w.run(ctx, "rebuild tools", []string{"./make"})
}

// Run runs one of the declared commands and reports what the RUNNER observed.
//
// It returns a RUN, not a bare error, for the same reason RunGate does: the
// Outcome separates "ran and reported" from "could not start", "timed out" or
// "died", and the three have different budget consequences. A step that failed
// because a lock timed out used to be indistinguishable from one that failed
// because the code is broken — both were a non-nil error — so a retry that was
// free got charged like one that was pointless.
//
// A NON-NIL ERROR MEANS NO COMMAND WAS RUN AND NO OUTCOME EXISTS: an undeclared
// name, or one this orchestrator has no configured command for.
func (w *worktree) Run(ctx context.Context, name flow.CommandName) (flow.CommandRun, error) {
	if !name.Valid() {
		return flow.CommandRun{}, fmt.Errorf("worktree.Run: %q is not one of the three command names", name)
	}
	if !flow.HasCommand(w.b.SupportedCommands(), name) {
		return flow.CommandRun{}, fmt.Errorf("worktree.Run: this orchestrator declares no %q command: %w", name, flow.ErrUnsupported)
	}
	if name != flow.CommandVerify {
		// Declared but unreachable would be worse than undeclared: a caller
		// would read a missing outcome as a measurement.
		return flow.CommandRun{}, fmt.Errorf("worktree.Run: %q has no configured command: %w", name, flow.ErrUnsupported)
	}
	if len(w.b.cfg.VerifyCmd) == 0 {
		return flow.CommandRun{}, fmt.Errorf("worktree.Run: cfg.VerifyCmd is empty: %w", flow.ErrUnsupported)
	}
	// The gate timeout bounds it too: a command that never returns is worse
	// than one that reports timed_out, and it is the only declared bound
	// there is.
	return runCommand(ctx, w.b.cfg.WorktreeDir, name, w.b.cfg.VerifyCmd, w.b.cfg.GateTimeout), nil
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

// run executes one ad-hoc command in the worktree. Used by RebuildTools, which
// is a pull-request operation rather than a declared CommandName.
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

func (w *worktree) RevParse(ctx context.Context, rev flow.Revision) (flow.CommitSha, error) {
	sha, err := w.b.git.RevParse(ctx, string(rev))
	return flow.CommitSha(sha), err
}

func (w *worktree) Stage(ctx context.Context) error { return w.b.git.StageAll(ctx) }
