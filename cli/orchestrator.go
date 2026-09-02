package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
)

// SelectFlow picks the first flow in app.Flows whose constraints match the
// given state: types contains item.Type (or is empty/universal), every
// RequireSignal is set, and at least one lifecycle item is pending. Returns
// (nil, "") when no flow is eligible (terminal state).
func SelectFlow(app *App, state *flow.ItemState) (*flow.Flow, string) {
	for _, f := range app.Flows {
		if !f.AcceptsType(state.Item.Type) {
			continue
		}
		if !f.IsReady(state) {
			continue
		}
		next, ok := f.DeriveNext(state)
		if !ok {
			continue
		}
		return f, next
	}
	return nil, ""
}

// RunOne advances at most one lifecycle item on the given claim. Returns an
// InvocationResult describing what happened plus an error if the orchestrator
// itself (not the handler) failed catastrophically — failures inside
// handlers, sentinel returns, and budget exhaustion all manifest as a
// populated InvocationResult and nil err.
func RunOne(ctx context.Context, app *App, claim flow.Claim) (flow.InvocationResult, error) {
	state, err := app.Backend.LoadState(ctx, claim)
	if err != nil {
		return flow.InvocationResult{}, fmt.Errorf("load state: %w", err)
	}

	// Select the flow first; we need it for seeding too. The terminal-done
	// short-circuit also has to beat Preflight so a completed item retires
	// cleanly even when a generic preflight (e.g. "item still open on
	// tracker") would otherwise refuse it.
	f, nextName := SelectFlow(app, state)
	if f == nil {
		// SelectFlow returns nil for two conditions that mean opposite things:
		// the flow has no step left (the work is done), and no flow accepts the
		// item's type (no work was ever attempted). Only the first is success.
		// Finalizing means the work was done, so an item no flow will act on is
		// not finalized on that basis — reporting success for work never
		// attempted hides the misconfiguration, and finalizing makes it
		// terminal. This is checked BEFORE the pending-artifact guard below so a
		// seeded-then-unmatched item reports the root cause rather than a
		// symptom of it; the guard itself is blind here anyway, because seeding
		// happens after flow selection and an unmatched item has no records to
		// iterate.
		//
		// "blocked", not "failed" or "skipped": nothing failed and no next cycle
		// will pass — a person has to register a flow for the type or correct
		// the item's type, and the reason names both so the operator does not
		// have to read the flow registration to find out what happened.
		//
		// An already-finalized item is exempt: its run really is over, and
		// blocking one that this very defect finalized would strand it.
		if !state.Item.Finalized && flowForType(app, state.Item.Type) == nil {
			return flow.InvocationResult{
				Item:   claim.ItemRef.Display,
				Status: "blocked",
				Reason: fmt.Sprintf(
					"no flow accepts item type %q (registered: %s) — register a flow for this type, or correct the item's type",
					state.Item.Type, registeredTypes(app)),
			}, nil
		}
		// T0481: refuse to Finalize+release when any required artifact in the
		// loaded state is still unresolved. status=done ≠ finalized — a missing
		// summary/inspection means finalization work is owed on this arena, and
		// a release here would strand the operator's next hand-run `run-step`
		// with "no active claim". This is a defensive guard against a
		// misseeded flow / a future regression that lets SelectFlow return nil
		// over an unfinalized item; the happy-path producer-flow already
		// derives the next step (review/coverage/commit/push/...) for it.
		for _, rec := range state.Artifacts {
			if rec.Required && !rec.Resolved {
				return flow.InvocationResult{
					Item:   claim.ItemRef.Display,
					Status: string(flow.StatusFailed),
					Reason: fmt.Sprintf("no eligible flow but required artifact %q still pending — refusing premature finalize/release", rec.Id),
				}, nil
			}
		}
		// No step remains — the flow is complete (or the item is terminal).
		// Finalize + release the claim if the backend supports it, so a manual
		// run closes the item and frees the arena the same way the orchestrator
		// does on completion (instead of leaving it un-finalized + leased).
		reason := "no eligible flow"
		if fz, ok := app.Backend.(flow.Finalizer); ok {
			if err := fz.Finalize(ctx, claim); err != nil {
				return flow.InvocationResult{
					Item:   claim.ItemRef.Display,
					Status: string(flow.StatusFailed),
					Reason: "finalize: " + err.Error(),
				}, nil
			}
			reason = "no eligible flow — finalized + released"
		}
		return flow.InvocationResult{
			Flow:   "",
			Item:   claim.ItemRef.Display,
			Status: string(flow.StatusDone),
			Reason: reason,
		}, nil
	}

	// Cross-flow preflight gate. Runs AFTER LoadState (fresh state) and
	// AFTER the terminal-done check (so completed items finalize) but
	// BEFORE seed / handler dispatch. Non-nil error → skipped, no handler
	// runs, no budget consumed.
	if app.Preflight != nil {
		if perr := app.Preflight(ctx, state); perr != nil {
			// A gate that a human has to clear is reported as "blocked", not
			// "skipped": a skip claims the next cycle might pass, and this one
			// will not until somebody acts. See flow.ErrBlocked.
			status := string(flow.StatusSkipped)
			if errors.Is(perr, flow.ErrBlocked) {
				status = string(flow.StatusBlocked)
			}
			return flow.InvocationResult{
				Item:   claim.ItemRef.Display,
				Status: status,
				Reason: "preflight: " + perr.Error(),
			}, nil
		}
	}

	// The answer gate passed for an item parked on a question — an answer
	// exists (otherwise the gate would have returned ErrBlocked). Clear the
	// needs-answer marker now rather than waiting for step resolution, since
	// the condition it advertises is already false. Best-effort: a stale
	// label is cosmetic, and failing the dispatch over it would block a step
	// somebody already answered.
	if state.Park != nil && state.Park.Kind == flow.ParkQuestion {
		if qa, ok := app.Backend.(flow.QuestionAnswerer); ok {
			qa.ClearQuestionMarker(ctx, claim.ItemRef)
		}
	}

	// Mandatory seed gate. A flow that declares required artifacts runs steps
	// ONLY against an item whose finalization checklist has been seeded (the
	// required-artifact set). An item with no required artifact has not been
	// seeded; seed it now. If seeding fails, OR it produces no checklist, OR
	// no flow is derivable afterwards, the invocation errors out — the flow
	// NEVER runs a step against an unseeded item, and there is no fallback.
	// This binds step-selection to the seed instead of the compiled-in step
	// list. (Signal-only flows declare no required artifacts and are exempt.)
	seedSpecs := f.SeedSpec(app.artifactById)
	requiresSeed := false
	for _, s := range seedSpecs {
		if s.Required {
			requiresSeed = true
			break
		}
	}
	if requiresSeed && !state.HasRequiredArtifacts() {
		if err := app.Backend.SeedState(ctx, claim, seedSpecs); err != nil {
			return flow.InvocationResult{}, fmt.Errorf("seed state: %w", err)
		}
		state, err = app.Backend.LoadState(ctx, claim)
		if err != nil {
			return flow.InvocationResult{}, fmt.Errorf("reload after seed: %w", err)
		}
		if !state.HasRequiredArtifacts() {
			return flow.InvocationResult{}, fmt.Errorf(
				"item %s has no required-artifact checklist after seed — refusing to run any step (seeding is mandatory)",
				claim.ItemRef.Display)
		}
		// Re-derive against the now-seeded state.
		f, nextName = SelectFlow(app, state)
		if f == nil {
			return flow.InvocationResult{}, fmt.Errorf(
				"no eligible flow for item %s after seed — refusing to run", claim.ItemRef.Display)
		}
	}

	li, ok := f.Item(nextName)
	if !ok {
		return flow.InvocationResult{}, fmt.Errorf("flow %q has no step %q", f.Name(), nextName)
	}

	result := flow.InvocationResult{
		Flow:         f.Name(),
		InvocationID: invocationID(),
		Item:         claim.ItemRef.Display,
		Step:         li.Result(),
	}

	// AwaitSignal items have no handler; signal is checked by DeriveNext.
	// Reaching here means the signal isn't set yet — skip without
	// consuming budget.
	if li.Kind == flow.LifecycleAwait {
		result.Status = string(flow.StatusSkipped)
		result.Reason = fmt.Sprintf("awaiting signal %q", li.SignalId)
		return result, nil
	}

	// Pre-dispatch budget gate (artifact steps only — signal steps don't
	// own an artifact record yet on the first invocation). Parks name the
	// step by its RESULT ID (not the label) so `grant` can act on the record
	// whose budget caused the park — see ParkRequest.Step.
	art := state.Artifact(li.ArtifactId)
	if li.Kind == flow.LifecycleArtifact {
		if art.GrantedInvocations > 0 && art.Invocations >= art.GrantedInvocations {
			return parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
				Kind:   flow.ParkBudgetExhausted,
				Step:   li.Result(),
				Axis:   flow.AxisInvocations,
				Axes:   axisReports(art, effectiveTimeout(li, art), art.PromptsThisInvocation, 0),
				Reason: fmt.Sprintf("ran %d times without resolving %q", art.Invocations, li.ArtifactId),
			})
		}
		if art.GrantedCostUSD > 0 && art.CostUSDSpent >= art.GrantedCostUSD {
			return parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
				Kind:   flow.ParkBudgetExhausted,
				Step:   li.Result(),
				Axis:   flow.AxisCost,
				Axes:   axisReports(art, effectiveTimeout(li, art), art.PromptsThisInvocation, 0),
				Reason: fmt.Sprintf("spent $%.2f without resolving %q", art.CostUSDSpent, li.ArtifactId),
			})
		}
	}

	// Wrap a context for this invocation. All three budget axes resolve from
	// the persisted record so that grants actually land: the record's granted
	// timeout wins when set (`grant --timeout` raises it), otherwise fall back
	// to the step's compiled-in budget and then the package default. A signal
	// step owns no record yet, so Artifact returns the zero record and the
	// flow definition applies.
	timeout := effectiveTimeout(li, art)
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build the per-invocation StepCtx.
	budgetKey := li.Result()
	sctx := newStepCtx(stepCtx, app, claim, f, li, state, timeout)

	// Auto-emit step entry so every step transition reaches the tracker
	// without each handler having to call ctx.Notify. Handlers that DO call
	// ctx.Notify with richer detail will override this baseline.
	if app.Telemetry != nil {
		app.Telemetry.StepProgress(stepCtx, claim, li.Result(), "")
	}

	// Record the running step so `status` can report it. The record is
	// advisory: a failure to write means status will not show "running",
	// which is a degradation, not a breakage.
	exe, _ := os.Executable()
	absExe, _ := filepath.Abs(exe)
	_ = clistate.SaveRunning(clistate.RunningRecord{
		Item: claim.ItemRef.Display,
		Step: li.Result(),
		PID:  os.Getpid(),
		Exe:  absExe,
	})
	defer clistate.ClearRunning()

	// Dispatch.
	handlerErr := li.Handler(sctx)

	// Timeout (deadline reached during handler). Counts as an invocation —
	// the handler ran, it just didn't finish in time.
	if errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
		// No patch is captured here. A deadline kill says nothing about the
		// state of the worktree: verify never went green (that is what the
		// step ran out of time doing), so an attached diff is unverified
		// work that a resume would apply on top of a broken tree. And the
		// common shape — a step that commits and then runs a long verify —
		// leaves `git diff HEAD` empty, so the capture uploaded a zero-byte
		// patch carrying no diagnostic value at all. Park only; the work
		// stays in the worktree where the rerun picks it up.
		if li.Kind == flow.LifecycleArtifact {
			if err := bumpInvocations(ctx, app, claim, state, li.ArtifactId, budgetKey); err != nil {
				return flow.InvocationResult{}, fmt.Errorf("bump invocations: %w", err)
			}
		}
		return sctx.stampResult(parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
			Kind: flow.ParkBudgetExhausted,
			Step: li.Result(),
			Axis: flow.AxisTimeout,
			// The invocations bump above is already counted here: a timeout
			// park that under-reported invocations is exactly what sent the
			// operator back for a second grant.
			Axes:   sctx.axisReports(timeout),
			Reason: fmt.Sprintf("step %q exceeded %s", li.Result(), timeout),
		}))
	}

	// Transient infra failure (handler returned flow.ErrTransient OR the
	// metered agent observed AgentResponse.Failure.Transient and surfaced
	// it through the wrapped error). Park with ParkInfraTransient and
	// SKIP the BumpInvocations call — a flapping runner must not burn the
	// step's invocation budget.
	if handlerErr != nil && errors.Is(handlerErr, flow.ErrTransient) {
		return sctx.stampResult(parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
			Kind:   flow.ParkInfraTransient,
			Step:   li.Result(),
			Reason: handlerErr.Error(),
		}))
	}

	// Deterministic refusal (handler returned flow.ErrRefused): the failure
	// provably cannot change on re-run, so retrying is pointless. Park with
	// ParkRefused and SKIP the BumpInvocations call — symmetric with the
	// ErrTransient branch above. The park reason is the refusal's own
	// message so the operator sees what was refused.
	if handlerErr != nil && errors.Is(handlerErr, flow.ErrRefused) {
		return sctx.stampResult(parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
			Kind:   flow.ParkRefused,
			Step:   li.Result(),
			Reason: handlerErr.Error(),
		}))
	}

	// Write-contract check. Runs after the transient/refused early returns
	// (which skip budget) but BEFORE the normal bumpInvocations. Only when
	// the handler acquired a worktree (writeSnap != nil). On violation:
	// charge the invocation (the handler ran), park with ParkWriteContract,
	// do NOT revert changes.
	if sctx.writeSnap != nil {
		if reason := checkWriteContract(ctx, sctx.worktree, sctx.writeSnap, li.Writes); reason != "" {
			if li.Kind == flow.LifecycleArtifact {
				if err := bumpInvocations(ctx, app, claim, state, li.ArtifactId, budgetKey); err != nil {
					return flow.InvocationResult{}, fmt.Errorf("bump invocations: %w", err)
				}
			}
			return sctx.stampResult(parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
				Kind:   flow.ParkWriteContract,
				Step:   li.Result(),
				Reason: reason,
			}))
		}
	}

	// Non-transient: the invocation produced a result (success, skip,
	// park, or a real failure). Count it.
	if li.Kind == flow.LifecycleArtifact {
		if err := bumpInvocations(ctx, app, claim, state, li.ArtifactId, budgetKey); err != nil {
			return flow.InvocationResult{}, fmt.Errorf("bump invocations: %w", err)
		}
	}

	return sctx.stampResult(translateHandlerError(ctx, app, claim, result, li, sctx, handlerErr))
}

// translateHandlerError converts the handler's return into an
// InvocationResult, applying the appropriate Backend.Park / Backend.AskQuestions
// as a side effect.
func translateHandlerError(
	ctx context.Context,
	app *App,
	claim flow.Claim,
	result flow.InvocationResult,
	li flow.LifecycleItem,
	sctx *stepCtx,
	handlerErr error,
) (flow.InvocationResult, error) {
	if handlerErr == nil {
		// For artifact steps, the handler must have called Resolve* — the
		// metered StepCtx tracked it.
		if li.Kind == flow.LifecycleArtifact && !sctx.resolved {
			return parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
				Kind:   flow.ParkStepDidNotResolve,
				Step:   li.Result(),
				Reason: fmt.Sprintf("handler returned nil without calling ctx.Resolve* on %q", li.ArtifactId),
			})
		}
		result.Status = string(flow.StatusDone)
		return result, nil
	}

	// Sentinel translations.
	var skip flow.ErrSkip
	if errors.As(handlerErr, &skip) {
		result.Status = string(flow.StatusSkipped)
		result.Reason = skip.Reason
		return result, nil
	}
	var park flow.ErrPark
	if errors.As(handlerErr, &park) {
		req := park.Req
		if req.Step == "" {
			req.Step = li.Result()
		}
		// A question park needs the ask time whichever route produced it.
		// Without it, a reader scanning for answers has no boundary and takes
		// every comment already on the item — including ones written long
		// before the question — for a reply.
		//
		// This route has no backend timestamp to use (the handler parked
		// directly rather than going through Backend.AskQuestions), so the mark
		// is backed off the local clock. A local time recorded as-is would be
		// compared against the backend's, and a runner running even slightly
		// fast would discard every answer permanently.
		if req.Kind == flow.ParkQuestion && flow.QuestionAskedAt(&req).IsZero() {
			req.Details = strings.TrimPrefix(
				strings.TrimSpace(req.Details+";"+flow.MarkQuestionAskedLocal(time.Now())), ";")
		}
		return parkAndReturn(ctx, app, claim, result, req)
	}
	var question flow.ErrQuestion
	if errors.As(handlerErr, &question) {
		recorded, err := app.Backend.AskQuestions(ctx, claim, question.Questions)
		if err != nil {
			return flow.InvocationResult{}, fmt.Errorf("backend.AskQuestions: %w", err)
		}
		// Stamp WHEN the question was asked — a reader scanning the item for
		// answers has no other way to tell a reply from the question itself.
		// Prefer the backend's own clock: the replies it later reports are
		// stamped by that same clock, and mixing in the local one means a
		// runner running fast discards answers it can never get back.
		marker := flow.MarkQuestionAskedLocal(time.Now())
		if len(recorded) > 0 && !recorded[0].AskedAt.IsZero() {
			// The backend's own clock — the one the answers will be stamped by.
			marker = flow.MarkQuestionAsked(recorded[0].AskedAt)
		}
		req := flow.ParkRequest{
			Kind:    flow.ParkQuestion,
			Step:    li.Result(),
			Reason:  questionReason(question.Questions),
			Details: marker,
		}
		return parkAndReturn(ctx, app, claim, result, req)
	}
	var budget flow.ErrBudgetExhausted
	if errors.As(handlerErr, &budget) {
		return parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
			Kind:   flow.ParkBudgetExhausted,
			Step:   li.Result(),
			Axis:   budget.Axis,
			Axes:   sctx.axisReports(sctx.timeout),
			Reason: budget.Error(),
		})
	}

	result.Status = string(flow.StatusFailed)
	result.Reason = handlerErr.Error()
	return result, nil
}

// effectiveTimeout resolves a step's per-run deadline: the granted timeout
// from the record wins when set (`grant --timeout` raises it), then the step's
// compiled-in budget, then the package default. Shared by the dispatch
// deadline and the park-time axis snapshot so the two can never disagree about
// what the cap actually was.
func effectiveTimeout(li flow.LifecycleItem, rec flow.ArtifactRecord) time.Duration {
	if rec.GrantedTimeout > 0 {
		return rec.GrantedTimeout
	}
	if li.Budget.Timeout > 0 {
		return li.Budget.Timeout
	}
	return flow.DefaultStepBudget().Timeout
}

// axisReports snapshots all four budget axes for a budget park.
//
// Every axis is reported, never just the one that tripped. The axes go flat
// together — a run that times out has usually burned its invocations too, and
// a step parked on prompts is often already over on cost — so a park naming
// one axis sent the operator back for another grant as soon as the next
// dispatch re-parked on the next axis. One park, one report, one grant.
//
// prompts is passed in rather than read off the record: the record's counter
// resets on the invocation bump, and what the operator needs to judge the cap
// by is what the run that just parked actually spent. elapsed is likewise
// unrecorded — it is wall time against the deadline, meaningful only for a
// park that happens after dispatch, and zero for the pre-dispatch gates.
func axisReports(rec flow.ArtifactRecord, timeout time.Duration, prompts int, elapsed time.Duration) []flow.AxisReport {
	return []flow.AxisReport{
		flow.NewAxisReport(flow.AxisInvocations, float64(rec.Invocations), float64(rec.GrantedInvocations)),
		flow.NewAxisReport(flow.AxisPrompts, float64(prompts), float64(rec.GrantedPromptsPerInvocation)),
		flow.NewAxisReport(flow.AxisCost, rec.CostUSDSpent, rec.GrantedCostUSD),
		flow.NewAxisReport(flow.AxisTimeout, elapsed.Seconds(), timeout.Seconds()),
	}
}

// axisReports is the post-dispatch snapshot: same four axes, read from the
// live view of the invocation rather than the record alone. Cost and
// invocations come off the in-memory mirror (kept current by meteredAgent and
// bumpInvocations), prompts off the metered agent's own counter, and elapsed
// off the step's start.
func (sc *stepCtx) axisReports(timeout time.Duration) []flow.AxisReport {
	if sc.li.Kind != flow.LifecycleArtifact {
		return nil
	}
	var prompts int
	if sc.agent != nil {
		prompts = sc.agent.promptsThisInvocation
	}
	return axisReports(sc.state.Artifact(sc.li.ArtifactId), timeout, prompts, time.Since(sc.startedAt))
}

// bumpInvocations counts the invocation on the backend and mirrors it into the
// in-memory record. The mirror matters because the park paths downstream
// snapshot their axes from it: a timeout park reporting the pre-bump count
// would under-report the invocations axis, which is precisely the axis that
// re-parks the step once the operator grants the time.
func bumpInvocations(ctx context.Context, app *App, claim flow.Claim, state *flow.ItemState, id flow.ArtifactId, key string) error {
	if err := app.Backend.BumpInvocations(ctx, claim, key); err != nil {
		return err
	}
	rec := state.Artifact(id)
	rec.Invocations++
	rec.PromptsThisInvocation = 0 // mirror the backend exactly — it resets here too
	state.Artifacts[id] = rec
	return nil
}

// checkWriteContract compares the current worktree state against the
// pre-handler snapshot and the step's WriteContract. Returns an empty string
// when the contract holds, or a violation reason string.
//
// Uses the parent ctx (not stepCtx) for the post-handler reads, since the
// step's deadline may have been consumed.
func checkWriteContract(ctx context.Context, wt flow.Worktree, snap *writeSnapshot, wc flow.WriteContract) string {
	if !wc.MayBranch {
		branch, err := wt.CurrentBranch(ctx)
		if err == nil && branch != snap.branch {
			return fmt.Sprintf("branch moved: was %q, now %q", snap.branch, branch)
		}
	}
	if !wc.MayCommit {
		sha, err := wt.RevParse(ctx, "HEAD")
		if err == nil && sha != snap.commitSHA {
			return fmt.Sprintf("commit moved: was %.12s, now %.12s", snap.commitSHA, sha)
		}
	}
	if !wc.MayEditTree {
		dirty, err := wt.IsDirty(ctx)
		if err == nil && dirty {
			return "tree has uncommitted changes to tracked files"
		}
	}
	return ""
}

func parkAndReturn(
	ctx context.Context,
	app *App,
	claim flow.Claim,
	result flow.InvocationResult,
	req flow.ParkRequest,
) (flow.InvocationResult, error) {
	if err := app.Backend.Park(ctx, claim, req); err != nil {
		return flow.InvocationResult{}, fmt.Errorf("backend.Park: %w", err)
	}
	result.Status = string(flow.StatusParked)
	result.Reason = req.Reason
	cp := req
	result.Park = &cp
	return result, nil
}

func questionReason(qs []flow.AgentQuestion) string {
	if len(qs) == 0 {
		return "question pending"
	}
	if len(qs) == 1 {
		return "question: " + questionSummary(qs[0])
	}
	return fmt.Sprintf("%d questions pending (first: %s)", len(qs), questionSummary(qs[0]))
}

// questionSummary is the one-line form of a question, for a park reason, a
// status line, or a blocked message.
//
// Header first: it is the short scannable form, and Text may be a multi-line
// block of supporting evidence. A reason built from Text would splice that
// whole block into a field every reader expects to be one line.
func questionSummary(q flow.AgentQuestion) string {
	if s := strings.TrimSpace(q.Header); s != "" {
		return firstLine(s)
	}
	// Header is optional, so this path is reachable from any handler calling
	// ctx.AskQuestions directly — and Text is exactly where a multi-line body
	// belongs. Taking the first line keeps the one-line invariant that made
	// this helper necessary.
	return firstLine(strings.TrimSpace(q.Text))
}

// firstLine reduces a possibly-multi-line string to its first line.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// invocationID returns a coarse-grained id for the current invocation.
// Format: <unix-ns>; collision-resistant within one worktree.
func invocationID() string {
	return fmt.Sprintf("inv-%d", time.Now().UnixNano())
}

// writeSnapshot records the worktree state at the moment the handler first
// acquires it, before the handler runs. checkWriteContract compares against
// it afterwards.
type writeSnapshot struct {
	branch    string
	commitSHA string
}

// stepCtx is the concrete StepCtx the orchestrator hands to handlers. Tracks
// whether the handler called the matching Resolve* (artifact steps) and
// memoises the lazily-acquired Worktree.
type stepCtx struct {
	ctx      context.Context
	app      *App
	claim    flow.Claim
	flow     *flow.Flow
	li       flow.LifecycleItem
	state    *flow.ItemState
	worktree flow.Worktree
	wtErr    error
	agent    *meteredAgent
	resolved bool
	// wip memoises the step's work-in-progress record for this invocation. A
	// step that never asks never loads, which is what makes the record cost
	// nothing to not use.
	wip       string
	wipLoaded bool
	wipErr    error
	// startedAt and timeout back the timeout axis of a park-time snapshot:
	// elapsed-vs-cap is the one axis with no counter on the record.
	startedAt time.Time
	timeout   time.Duration
	// writeSnap is the worktree state captured when the handler first acquires
	// the worktree. nil when no worktree was acquired (no check will run).
	writeSnap *writeSnapshot
}

func newStepCtx(ctx context.Context, app *App, claim flow.Claim, f *flow.Flow, li flow.LifecycleItem, state *flow.ItemState, timeout time.Duration) *stepCtx {
	sc := &stepCtx{
		ctx:       ctx,
		app:       app,
		claim:     claim,
		flow:      f,
		li:        li,
		state:     state,
		startedAt: time.Now(),
		timeout:   timeout,
	}
	sc.agent = &meteredAgent{
		inner:   app.Agent,
		backend: app.Backend,
		claim:   claim,
		stepCtx: sc,
	}
	return sc
}

// stampResult fills in duration and cost on a post-dispatch InvocationResult
// and persists duration to the artifact record. When err is non-nil the
// orchestrator itself failed catastrophically — there is no meaningful result
// to stamp.
func (s *stepCtx) stampResult(r flow.InvocationResult, err error) (flow.InvocationResult, error) {
	if err != nil {
		return r, err
	}
	elapsed := time.Since(s.startedAt)
	r.DurationSeconds = elapsed.Seconds()
	cost := s.agent.costThisInvocation
	r.CostUSD = &cost
	if s.li.Kind == flow.LifecycleArtifact {
		_ = s.app.Backend.AddDuration(s.ctx, s.claim, string(s.li.ArtifactId), elapsed)
	}
	return r, nil
}

func (s *stepCtx) Context() context.Context { return s.ctx }
func (s *stepCtx) Flow() string             { return s.flow.Name() }
func (s *stepCtx) StepName() string         { return s.li.Name }
func (s *stepCtx) Result() flow.ArtifactId  { return s.li.ArtifactId }
func (s *stepCtx) Item() flow.Item          { return s.state.Item }
func (s *stepCtx) Claim() flow.Claim        { return s.claim }
func (s *stepCtx) VerifyCmd() string        { return s.app.VerifyCmd }

func (s *stepCtx) Artifact(id flow.ArtifactId) (flow.ArtifactRecord, bool) {
	rec, ok := s.state.Artifacts[id]
	if !ok || !rec.Resolved {
		return flow.ArtifactRecord{}, false
	}
	return rec, true
}

func (s *stepCtx) Flag(id flow.ArtifactId) (bool, bool) {
	rec, ok := s.Artifact(id)
	if !ok || rec.Type != flow.ArtifactFlag {
		return false, false
	}
	return true, true
}

func (s *stepCtx) CommitHash(id flow.ArtifactId) (string, bool) {
	rec, ok := s.Artifact(id)
	if !ok || rec.Type != flow.ArtifactCommitHash {
		return "", false
	}
	return rec.CommitHash, true
}

func (s *stepCtx) Markdown(id flow.ArtifactId) (string, bool) {
	rec, ok := s.Artifact(id)
	if !ok || rec.Type != flow.ArtifactMarkdown {
		return "", false
	}
	return rec.Markdown, true
}

func (s *stepCtx) JSON(id flow.ArtifactId) (json.RawMessage, bool) {
	rec, ok := s.Artifact(id)
	if !ok || rec.Type != flow.ArtifactJSON {
		return nil, false
	}
	return rec.JSON, true
}

func (s *stepCtx) File(id flow.ArtifactId) (string, []byte, bool) {
	rec, ok := s.Artifact(id)
	if !ok || rec.Type != flow.ArtifactFile {
		return "", nil, false
	}
	return rec.File.Name, rec.File.Content, true
}

func (s *stepCtx) Patch(id flow.ArtifactId) (flow.PatchBody, bool) {
	rec, ok := s.Artifact(id)
	if !ok || rec.Type != flow.ArtifactPatch {
		return flow.PatchBody{}, false
	}
	return rec.Patch, true
}

func (s *stepCtx) Signal(id flow.SignalId) bool {
	return s.state.SignalSet(id)
}

// resolveGuard centralises the kind/type check shared by all Resolve* methods.
// Returns the artifact's declared type on success; an error otherwise.
func (s *stepCtx) resolveGuard(want flow.ArtifactType) error {
	switch s.li.Kind {
	case flow.LifecycleSignal, flow.LifecycleAwait:
		return flow.ErrSignalNotWritable{Step: s.li.Name, Signal: s.li.SignalId}
	}
	def := s.app.artifactById[s.li.ArtifactId]
	if def.Type != want {
		return flow.ErrTypeMismatch{Step: s.li.Name, Expected: def.Type, Got: want}
	}
	if s.resolved {
		return fmt.Errorf("step %q already resolved %q", s.li.Name, s.li.ArtifactId)
	}
	return nil
}

func (s *stepCtx) ResolveFlag() error {
	if err := s.resolveGuard(flow.ArtifactFlag); err != nil {
		return err
	}
	return s.writeResolve(flow.ArtifactBody{Type: flow.ArtifactFlag})
}

func (s *stepCtx) ResolveCommitHash(sha string) error {
	if err := s.resolveGuard(flow.ArtifactCommitHash); err != nil {
		return err
	}
	return s.writeResolve(flow.ArtifactBody{Type: flow.ArtifactCommitHash, CommitHash: sha})
}

func (s *stepCtx) ResolveMarkdown(body string) error {
	if err := s.resolveGuard(flow.ArtifactMarkdown); err != nil {
		return err
	}
	return s.writeResolve(flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: body})
}

func (s *stepCtx) ResolveJSON(body json.RawMessage) error {
	if err := s.resolveGuard(flow.ArtifactJSON); err != nil {
		return err
	}
	return s.writeResolve(flow.ArtifactBody{Type: flow.ArtifactJSON, JSON: body})
}

func (s *stepCtx) ResolveFile(name string, content []byte) error {
	if err := s.resolveGuard(flow.ArtifactFile); err != nil {
		return err
	}
	return s.writeResolve(flow.ArtifactBody{Type: flow.ArtifactFile, File: flow.FileBody{Name: name, Content: content}})
}

func (s *stepCtx) ResolvePatch(body flow.PatchBody) error {
	if err := s.resolveGuard(flow.ArtifactPatch); err != nil {
		return err
	}
	// An EMPTY PatchBody is legal and must reach the backend. Backends whose
	// patches live server-side attach the diff out-of-band (their
	// Worktree.CapturePatch returns no bytes by design) and the handler calls
	// ResolvePatch with a zero body purely to say "I'm done — verify the side
	// effect"; the same is true when the work is already committed, where
	// `git diff HEAD` is empty by definition. Only the backend knows where
	// the evidence lives, so emptiness is ITS call: ResolveArtifact either
	// confirms the attachment or fails with a message that names what is
	// missing. Rejecting a zero body here made both shapes unrepresentable.
	return s.writeResolve(flow.ArtifactBody{Type: flow.ArtifactPatch, Patch: body})
}

func (s *stepCtx) writeResolve(body flow.ArtifactBody) error {
	if err := s.app.Backend.ResolveArtifact(s.ctx, s.claim, s.li.ArtifactId, body); err != nil {
		return err
	}
	s.resolved = true
	// The step has a result now, so its scaffolding is done. Clearing lives
	// HERE and nowhere else: one place that runs whichever Resolve* the step
	// called, so no handler can complete while leaving stale prose behind for a
	// later reader to mistake for a record.
	//
	// Best-effort, and deliberately so. The artifact has already landed, so
	// failing the step now would report a failure for work that is recorded —
	// and a record that outlives its step is harmless anyway, because keying by
	// (item, step) means the next dispatch of a resolved step never reads it.
	// Keying is the correctness property; clearing is hygiene.
	if store, ok := s.app.Backend.(flow.WorkInProgress); ok {
		if err := store.ClearWorkInProgress(s.ctx, s.claim, s.li.Result()); err != nil {
			s.Notify("", "could not clear work in progress: "+err.Error())
		}
	}
	return nil
}

func (s *stepCtx) Skip(reason string) error { return flow.ErrSkip{Reason: reason} }
func (s *stepCtx) MarkStale(id flow.ArtifactId) error {
	return s.app.Backend.MarkStale(s.ctx, s.claim, id)
}

func (s *stepCtx) Park(req flow.ParkRequest) error { return flow.ErrPark{Req: req} }

func (s *stepCtx) AskQuestions(qs ...flow.AgentQuestion) error {
	if len(qs) == 0 {
		return errors.New("ctx.AskQuestions called with no questions")
	}
	return flow.ErrQuestion{Questions: qs}
}

// ParkedOn reports the park this dispatch is resuming from, from the state
// already loaded for it.
func (s *stepCtx) ParkedOn() *flow.ParkRequest {
	if s.state == nil {
		return nil
	}
	return s.state.Park
}

// WorkInProgress returns what this step stashed on an earlier invocation.
//
// A backend with no store reads as absence rather than as an error: the record
// is optional, and a step that has to distinguish "nothing stashed" from "no
// store" to build its prompt would be a step no backend without one could run.
func (s *stepCtx) WorkInProgress() (string, error) {
	if s.wipLoaded {
		return s.wip, s.wipErr
	}
	s.wipLoaded = true
	store, ok := s.app.Backend.(flow.WorkInProgress)
	if !ok {
		return "", nil
	}
	s.wip, s.wipErr = store.LoadWorkInProgress(s.ctx, s.claim, s.li.Result())
	return s.wip, s.wipErr
}

// RecordWorkInProgress stashes work for this step's next invocation, keyed by
// the same result id its budget is metered against.
//
// A missing store is named here and only here. A caller that believed it
// stashed something and did not would park expecting to resume from a draft
// that was never written — which is the failure this whole surface exists to
// stop, silently reintroduced.
func (s *stepCtx) RecordWorkInProgress(body string) error {
	store, ok := s.app.Backend.(flow.WorkInProgress)
	if !ok {
		return flow.ErrWorkInProgressUnsupported
	}
	if err := store.SaveWorkInProgress(s.ctx, s.claim, s.li.Result(), body); err != nil {
		return err
	}
	// Keep the memo honest: a later read in this same invocation must see what
	// was just written, not a load from before it.
	s.wip, s.wipLoaded, s.wipErr = body, true, nil
	return nil
}

func (s *stepCtx) Notify(step, detail string) {
	if s.app.Telemetry == nil {
		return
	}
	if step == "" {
		step = s.li.Result()
	}
	s.app.Telemetry.StepProgress(s.ctx, s.claim, step, detail)
}

func (s *stepCtx) Agent() flow.Agent { return s.agent }

func (s *stepCtx) Worktree() (flow.Worktree, error) {
	if s.worktree != nil || s.wtErr != nil {
		return s.worktree, s.wtErr
	}
	s.worktree, s.wtErr = s.app.Backend.Worktree(s.ctx, s.claim)
	if s.wtErr != nil {
		return nil, s.wtErr
	}
	// Capture a snapshot for the write-contract check. If either read
	// fails, leave writeSnap nil — fail-open on infrastructure error,
	// since the handler hasn't run yet.
	branch, berr := s.worktree.CurrentBranch(s.ctx)
	sha, serr := s.worktree.RevParse(s.ctx, "HEAD")
	if berr == nil && serr == nil {
		s.writeSnap = &writeSnapshot{branch: branch, commitSHA: sha}
	}
	return s.worktree, nil
}

func (s *stepCtx) RefreshItem() error {
	state, err := s.app.Backend.LoadState(s.ctx, s.claim)
	if err != nil {
		return err
	}
	s.state = state
	return nil
}

// meteredAgent wraps app.Agent with prompts-per-invocation + cost caps.
// Failure paths return an ErrBudgetExhausted sentinel the orchestrator
// translates to a parked InvocationResult.
type meteredAgent struct {
	inner   flow.Agent
	backend flow.Backend
	claim   flow.Claim
	stepCtx *stepCtx

	promptsThisInvocation int
	costThisInvocation    float64
}

func (m *meteredAgent) Name() string { return m.inner.Name() }

func (m *meteredAgent) Run(ctx context.Context, req flow.AgentRequest) (*flow.AgentResponse, error) {
	li := m.stepCtx.li
	// Signal/await steps don't own an artifact budget. Allow the call to
	// pass through unmetered — those steps shouldn't normally call the
	// agent, but if they do the spend is not gated here.
	if li.Kind != flow.LifecycleArtifact {
		return m.inner.Run(ctx, req)
	}
	art := m.stepCtx.state.Artifact(li.ArtifactId)
	// Both caps name the step by result id: the message tells the operator
	// exactly what to pass to `grant`.
	if art.GrantedPromptsPerInvocation > 0 && m.promptsThisInvocation >= art.GrantedPromptsPerInvocation {
		return nil, flow.ErrBudgetExhausted{
			Step: li.Result(),
			Axis: flow.AxisPrompts,
			Cap:  fmt.Sprintf("%d", art.GrantedPromptsPerInvocation),
		}
	}
	if art.GrantedCostUSD > 0 && art.CostUSDSpent >= art.GrantedCostUSD {
		return nil, flow.ErrBudgetExhausted{
			Step: li.Result(),
			Axis: flow.AxisCost,
			Cap:  fmt.Sprintf("$%.2f", art.GrantedCostUSD),
		}
	}
	// Hand the turn the headroom left in the grant, so the substrate can stop
	// it at the cap. Without this the grant only bounds when a step stops
	// being dispatched: a turn that starts inside the grant can spend
	// whatever it spends, and the overrun is discovered one whole turn late.
	// A handler that set its own ceiling asked for a TIGHTER one than the
	// step's, so narrow to it — overwriting would silently widen the very
	// bound the handler wrote down.
	if headroom := art.GrantedCostUSD - art.CostUSDSpent; art.GrantedCostUSD > 0 &&
		(req.MaxCostUSD <= 0 || headroom < req.MaxCostUSD) {
		req.MaxCostUSD = headroom
	}
	if err := m.backend.BumpPrompts(ctx, m.claim, string(li.ArtifactId)); err != nil {
		return nil, fmt.Errorf("bump prompts: %w", err)
	}
	m.promptsThisInvocation++

	resp, err := m.inner.Run(ctx, req)
	// Skip cost accounting on transient infra failures — symmetric with
	// the orchestrator's skip-BumpInvocations policy for ParkInfraTransient.
	// A flapping runner must not burn the cost axis any more than it burns
	// the invocations axis.
	transient := resp != nil && resp.Failure != nil && resp.Failure.Transient
	if err == nil && resp != nil && resp.CostUSD > 0 && !transient {
		_ = m.backend.AddCost(ctx, m.claim, string(li.ArtifactId), resp.CostUSD)
		// Update local mirror so subsequent calls see fresh cost.
		art.CostUSDSpent += resp.CostUSD
		m.stepCtx.state.Artifacts[li.ArtifactId] = art
		m.costThisInvocation += resp.CostUSD
	}
	// A turn the substrate stopped at the cap we set IS this step reaching
	// its cost cap, so it parks on cost through the same sentinel the
	// pre-prompt gate returns — one park path, one axis snapshot, and the
	// AddCost above has already put the true spend on the mirror the
	// snapshot reads. Without a cost grant the cap was never ours to claim:
	// fall through to the ordinary agent failure.
	if err == nil && resp != nil && resp.Failure != nil &&
		resp.Failure.Kind == flow.FailureCostCap && art.GrantedCostUSD > 0 {
		return resp, flow.ErrBudgetExhausted{
			Step: li.Result(),
			Axis: flow.AxisCost,
			Cap:  fmt.Sprintf("$%.2f", art.GrantedCostUSD),
		}
	}
	// Surface AgentResponse.Failure through the error return so the
	// canonical "if err != nil { return err }" pattern in handlers picks
	// it up without forcing every handler to interrogate resp.Failure
	// separately. If Failure.Transient is set, the returned error wraps
	// flow.ErrTransient — the orchestrator's transient check will park
	// the step and skip the BumpInvocations call.
	if err == nil && resp != nil && resp.Failure != nil {
		err = agentFailureError(resp.Failure)
	}
	return resp, err
}

// agentFailureError converts an AgentFailure into an error. When
// Failure.Transient is true the error chain includes flow.ErrTransient so
// errors.Is(err, flow.ErrTransient) returns true regardless of whether the
// agent or the handler surfaced the transient.
func agentFailureError(f *flow.AgentFailure) error {
	msg := fmt.Sprintf("agent failure (%s): %s", f.Kind, f.Message)
	if f.Transient {
		return fmt.Errorf("%s: %w", msg, flow.ErrTransient)
	}
	return errors.New(msg)
}
