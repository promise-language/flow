package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
)

// SelectFlow returns the binary's one flow when it is eligible for the given
// item — every RequireSignal is set, and at least one lifecycle item is
// pending — and (nil, "") when it is not (terminal state).
//
// It does NOT consult the remit. The remit gates listing and selection and is
// consulted before the journal's first entry, never after
// (docs/flow-registration.md § Item types), so it is read once, ahead of this,
// by inRemit — never per dispatch, where an item retyped mid-resolution would
// be re-routed by the edit.
func SelectFlow(app *App, item *flow.Item) (*flow.Flow, string) {
	f := app.Flow
	if !f.IsReady(item) {
		return nil, ""
	}
	next, ok := f.DeriveNext(item)
	if !ok {
		return nil, ""
	}
	return f, next
}

// inRemit reports whether the item is this binary's work: its type is in the
// flow's remit, asked once — before the journal's first entry, and never after
// (docs/flow-registration.md § Item types). Once an item has a journal the
// route is the authority, and retyping it mid-resolution redirects nothing.
//
// One definition, read by the advance and by the narration that precedes it,
// so the two cannot disagree about what is about to happen.
func inRemit(app *App, state *flow.Item) bool {
	return len(state.Journal) > 0 || app.Flow.InRemit(state.Type)
}

// RunOne advances at most one lifecycle item on the given claim. Returns an
// InvocationResult describing what happened plus an error if the orchestrator
// itself (not the handler) failed catastrophically — failures inside
// handlers, sentinel returns, and budget exhaustion all manifest as a
// populated InvocationResult and nil err.
func RunOne(ctx context.Context, app *App, claim flow.Claim) (flow.InvocationResult, error) {
	ref := claim.ItemRef
	state, err := app.Orchestrator.Load(ctx, ref)
	if err != nil {
		return flow.InvocationResult{}, fmt.Errorf("load item: %w", err)
	}

	// The remit, ahead of selection and of everything it gates. An item outside
	// it is not this binary's work, so no step of this flow is ever derived for
	// it: the item's type is the whole question, and it is answered statically,
	// before anything is dispatched.
	//
	// "blocked", not "failed" or "skipped": nothing failed and no next cycle
	// will pass — a person has to register a flow for the type or correct the
	// item's type, and the reason names both so the operator does not have to
	// read the flow registration to find out what happened. Finalizing means
	// the work was done, so an item this binary will not act on is not
	// finalized on that basis — reporting success for work never attempted
	// hides the misconfiguration, and finalizing makes it terminal
	// (docs/resolution.md § Finalizing).
	//
	// An already-finalized item is exempt: its run really is over, and blocking
	// one that this very defect finalized would strand it. It falls through
	// with no flow selected, to the finalize-and-release path below.
	acts := inRemit(app, state)
	if !acts && !state.Finalized {
		return flow.InvocationResult{
			Item:   claim.ItemRef.Display,
			Status: "blocked",
			Reason: fmt.Sprintf(
				"no flow accepts item type %q (registered: %s) — register a flow for this type, or correct the item's type",
				state.Type, registeredTypes(app)),
		}, nil
	}

	// Select the flow first; we need it for seeding too. The terminal-done
	// short-circuit also has to beat Preflight so a completed item retires
	// cleanly even when a generic preflight (e.g. "item still open on
	// tracker") would otherwise refuse it.
	var (
		f        *flow.Flow
		nextName string
	)
	if acts {
		f, nextName = SelectFlow(app, state)
	}
	if f == nil {
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
		reason := "no eligible flow — finalized + released"
		finalized := true
		if err := app.Orchestrator.Finalize(ctx, ref); err != nil {
			// Finalize REFUSES an item the orchestrator does not yet consider
			// finished, and that refusal is not a failure of this run: the flow
			// has done everything it can, and the item reaches terminal by the
			// orchestrator's own means (a merge that closes it, a person). It is
			// typed ErrUnavailable precisely because asking again later is the
			// right response, so reporting it as `failed` would send an operator
			// hunting for a defect that is not there.
			if !errors.Is(err, flow.ErrUnavailable) {
				return flow.InvocationResult{
					Item:   claim.ItemRef.Display,
					Status: string(flow.StatusFailed),
					Reason: "finalize: " + err.Error(),
				}, nil
			}
			reason = "no eligible flow, and the item is not yet terminal — nothing finalized, claim kept"
			// The flag is set here, beside the branch that decides it. Reading it
			// back out of the reason would make prose the record of a state, and
			// the reason is for a person: rewording it would silently flip the
			// field.
			finalized = false
		}
		return flow.InvocationResult{
			Flow:      "",
			Item:      claim.ItemRef.Display,
			Status:    string(flow.StatusDone),
			Reason:    reason,
			Finalized: finalized,
		}, nil
	}

	// Blocked on items. Derived by the orchestrator on this very load from the
	// item's declared blockers and read here, before anything is dispatched to
	// the pending step (docs/resolution.md § Blocked on items). Nothing stores
	// it: the item whose last blocker finishes is workable at the next read,
	// and a blocker reopened blocks it again at the next read, with nobody
	// having touched the item either time.
	//
	// Before the preflight, so an item both waiting on items and awaiting an
	// answer reports waits-on-items — the precedence the derivation itself
	// gives it, so this report cannot disagree with `status`. After the
	// no-flow block, so the finalize path is untouched: an item with no
	// pending step has nothing to be blocked from.
	//
	// The stop is clean. No seed, no budget gate, no step context, no
	// invocation bump, no park, no running record. The claim is kept — an
	// arena reservation, not work — and the pending step stays pending, so
	// when the last blocker lands the next advance runs it from here.
	if blockedFromAdvancing(state) {
		li, err := lifecycleItemOf(f, nextName)
		if err != nil {
			return flow.InvocationResult{}, err
		}
		return blockedOnItems(state, flow.InvocationResult{
			Flow: f.Name(),
			Item: claim.ItemRef.Display,
			Step: string(li.Result()),
		}), nil
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

	// Mandatory seed gate. A flow that declares required artifacts runs steps
	// ONLY against an item whose finalization checklist has been seeded (the
	// required-artifact set). An item with no required artifact has not been
	// seeded; seed it now. If seeding fails, OR it produces no checklist, OR
	// no flow is derivable afterwards, the invocation errors out — the flow
	// NEVER runs a step against an unseeded item, and there is no fallback.
	// This binds step-selection to the seed instead of the compiled-in step
	// list. (Signal-only flows declare no required artifacts and are exempt.)
	seedSpecs := f.SeedSpec(app.artifactById, app.StepBudgets)
	requiresSeed := false
	for _, s := range seedSpecs {
		if s.Required {
			requiresSeed = true
			break
		}
	}
	if requiresSeed && !state.HasRequiredArtifacts() {
		if err := app.Orchestrator.SeedState(ctx, ref, seedSpecs); err != nil {
			return flow.InvocationResult{}, fmt.Errorf("seed state: %w", err)
		}
		state, err = app.Orchestrator.Load(ctx, ref)
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

	li, err := lifecycleItemOf(f, nextName)
	if err != nil {
		return flow.InvocationResult{}, err
	}

	result := flow.InvocationResult{
		Flow:         f.Name(),
		InvocationID: invocationID(),
		Item:         claim.ItemRef.Display,
		Step:         string(li.Result()),
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
			return parkAndReturn(ctx, app, ref, result, flow.ParkRequest{
				Kind:   flow.ParkBudgetExhausted,
				Step:   li.Result(),
				Axis:   flow.AxisInvocations,
				Axes:   axisReports(art, app.effectiveTimeout(li, art), art.PromptsThisInvocation, 0),
				Reason: fmt.Sprintf("ran %d times without resolving %q", art.Invocations, li.ArtifactId),
			})
		}
		if art.GrantedCostUSD > 0 && art.CostUSDSpent >= art.GrantedCostUSD {
			return parkAndReturn(ctx, app, ref, result, flow.ParkRequest{
				Kind:   flow.ParkBudgetExhausted,
				Step:   li.Result(),
				Axis:   flow.AxisCost,
				Axes:   axisReports(art, app.effectiveTimeout(li, art), art.PromptsThisInvocation, 0),
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
	timeout := app.effectiveTimeout(li, art)
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build the per-invocation StepCtx.
	sctx := newStepCtx(stepCtx, app, claim, f, li, state, timeout)

	// Auto-emit step entry so every step transition reaches the tracker
	// without each handler having to call ctx.Notify. Handlers that DO call
	// ctx.Notify with richer detail will override this baseline.
	if app.Telemetry != nil {
		app.Telemetry.StepProgress(stepCtx, claim, string(li.Result()), "")
	}

	// Record the running step so `status` can report it. The record is
	// advisory: a failure to write means status will not show "running",
	// which is a degradation, not a breakage.
	exe, _ := os.Executable()
	absExe, _ := filepath.Abs(exe)
	_ = clistate.SaveRunning(clistate.RunningRecord{
		Item: claim.ItemRef.Display,
		Step: string(li.Result()),
		PID:  os.Getpid(),
		Exe:  absExe,
	})
	defer clistate.ClearRunning()

	// Dispatch. The handler completes by RETURNING its election; res is read
	// only on the completion path (translateHandlerError's nil-error branch),
	// because every other way a dispatch ends is one where nothing was elected.
	res, handlerErr := li.Handler(sctx)

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
		if err := chargeDispatch(ctx, app, ref, state, li); err != nil {
			return flow.InvocationResult{}, err
		}
		return sctx.stampResult(parkAndReturn(ctx, app, ref, result, flow.ParkRequest{
			Kind: flow.ParkBudgetExhausted,
			Step: li.Result(),
			Axis: flow.AxisTimeout,
			// The charge above is already counted here: a timeout park that
			// under-reported invocations is exactly what sent the operator back
			// for a second grant.
			Axes:   sctx.axisReports(timeout),
			Reason: fmt.Sprintf("step %q exceeded %s", li.Result(), timeout),
		}))
	}

	// Machine unfit (handler returned flow.ErrUnfit). The machine is not
	// fit to perform work — e.g. disk full. No park (a machine condition
	// has no step and ends on its own), no BumpInvocations (a condition is
	// not a failure), status blocked. The claim is kept.
	if handlerErr != nil && errors.Is(handlerErr, flow.ErrUnfit) {
		result.Status = string(flow.StatusBlocked)
		result.Reason = handlerErr.Error()
		return sctx.stampResult(result, nil)
	}

	// Transient infra failure (handler returned flow.ErrTransient OR the
	// metered agent observed AgentResponse.Failure.Transient and surfaced
	// it through the wrapped error). Park with ParkInfraTransient and
	// SKIP the BumpInvocations call — a flapping runner must not burn the
	// step's invocation budget.
	if handlerErr != nil && errors.Is(handlerErr, flow.ErrTransient) {
		return sctx.stampResult(parkAndReturn(ctx, app, ref, result, flow.ParkRequest{
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
		return sctx.stampResult(parkAndReturn(ctx, app, ref, result, flow.ParkRequest{
			Kind:   flow.ParkRefused,
			Step:   li.Result(),
			Reason: handlerErr.Error(),
		}))
	}

	// The step declared the blockers it found (handler returned
	// flow.ErrWaitsOnItems through ctx.WaitOnItems) and stopped on them. Not a
	// park, not a failure, not a refusal: the same clean stop the check before
	// dispatch makes, reported from the same derivation — the item is reloaded
	// so the report carries what the orchestrator now says, blockers and
	// statuses included. No BumpInvocations: the work exists elsewhere and will
	// land, and charging the wait would spend the budget on nothing. The
	// artifact stays unresolved, and work in progress is kept for the resume,
	// as with a question.
	//
	// The reload decides, under the same condition as the check before
	// dispatch. A step can declare items that have all finished already — the
	// orchestrator accepts those, since naming an item that has landed is not
	// an error — and the item then reads unblocked. That is not a stop on
	// anything: nothing waits, the next advance would run the step, and a
	// `blocked` report on an item nothing blocks would tell the operator to
	// wait for nothing (docs/resolution.md § Reporting names the kind, and
	// there is none). It is the step not doing its job — a turn spent to
	// declare a wait that does not hold — and it falls through as the failure
	// it is, charged as one, the reason naming what was declared.
	var waits flow.ErrWaitsOnItems
	if errors.As(handlerErr, &waits) {
		state, err = app.Orchestrator.Load(ctx, ref)
		if err != nil {
			return flow.InvocationResult{}, fmt.Errorf("reload after declaring blockers: %w", err)
		}
		if blockedFromAdvancing(state) {
			return sctx.stampResult(blockedOnItems(state, result), nil)
		}
		handlerErr = fmt.Errorf(
			"step declared it %s, but every item it named has already finished and nothing blocks the item — the step stopped on no wait",
			waits.Error())
	}

	// Post-handler fitness catch-all: any unclassified handler failure on an
	// unfit machine is reported as blocked, not charged. This catches
	// environment failures (ENOSPC, etc.) from ANY handler in ANY flow,
	// without each handler having to classify them. Runs after the sentinel
	// branches (already classified) and before write-contract / the charge.
	if handlerErr != nil && sctx.worktree != nil {
		if fitErr := flow.CheckFit(ctx, sctx.worktree); fitErr != nil {
			result.Status = string(flow.StatusBlocked)
			// Both, and the handler's first. CheckFit fails CLOSED — a fit gate
			// that could not run, timed out or died reports unfit — so this
			// branch is also reached when the fit gate is the broken thing.
			// Reporting the fitness verdict alone would then throw away the only
			// account of what actually failed, and blame the machine for it.
			result.Reason = fmt.Sprintf("%s (and the machine is unfit: %s)", handlerErr, fitErr)
			return sctx.stampResult(result, nil)
		}
	}

	// Write-contract check. Runs after the transient/refused early returns
	// (which skip budget) but BEFORE the normal charge. Only when the handler
	// acquired a worktree (writeSnap != nil). On violation: charge the
	// invocation (the handler ran), park with ParkWriteContract, do NOT revert
	// changes.
	if sctx.writeSnap != nil {
		if reason := checkWriteContract(ctx, sctx.worktree, sctx.writeSnap, li.Writes); reason != "" {
			if err := chargeDispatch(ctx, app, ref, state, li); err != nil {
				return flow.InvocationResult{}, err
			}
			return sctx.stampResult(parkAndReturn(ctx, app, ref, result, flow.ParkRequest{
				Kind:   flow.ParkWriteContract,
				Step:   li.Result(),
				Reason: reason,
			}))
		}
	}

	// The completion path counts its own dispatch, at each of its outcomes,
	// because one of them must not be counted at all — see chargeDispatch.
	if handlerErr == nil {
		return sctx.stampResult(completeStep(ctx, app, ref, result, li, sctx, res, state))
	}

	// Non-transient: the invocation produced a result (a park, or a real
	// failure). Count it.
	if err := chargeDispatch(ctx, app, ref, state, li); err != nil {
		return flow.InvocationResult{}, err
	}

	return sctx.stampResult(translateHandlerError(ctx, app, ref, result, li, sctx, handlerErr))
}

// translateHandlerError converts a handler's non-nil error into an
// InvocationResult, applying the appropriate Orchestrator.Park
// as a side effect. A handler that returned no error completed, and that path
// is completeStep's.
func translateHandlerError(
	ctx context.Context,
	app *App,
	ref flow.ItemRef,
	result flow.InvocationResult,
	li flow.LifecycleItem,
	sctx *stepCtx,
	handlerErr error,
) (flow.InvocationResult, error) {
	// Sentinel translations.
	var park flow.ErrPark
	if errors.As(handlerErr, &park) {
		req := park.Req
		// A question park is answerable only through a question something
		// REGISTERED, and this route registers nothing — it takes the handler's
		// kind as given. Writing one here leaves an item parked on a question
		// `answer` has no id to name (cli/cmd_answer.go: no outstanding
		// questions) and nothing else clears. Fail the step here, where the kind
		// is still only a request, and name the route that works — the same
		// answer the ask route gives when the orchestrator registers nothing
		// (stepCtx.AskQuestions). docs/resolution.md § Questions: a step that
		// needs a human decision asks a question and parks.
		//
		// The guard sits here rather than in stepCtx.Park because flow.ErrPark
		// is exported: a handler can return the sentinel without going through
		// ctx.Park, and errors.As catches it either way. This is the write site,
		// so it is the only place the invariant holds for both doors.
		if req.Kind == flow.ParkQuestion {
			result.Status = string(flow.StatusFailed)
			result.Reason = "ctx.Park cannot raise a question park: it registers no question, " +
				"so `answer` would have none to name — ask through ctx.AskQuestions, which " +
				"records the question and parks on it"
			return result, nil
		}
		if req.Step == "" {
			req.Step = li.Result()
		}
		return parkAndReturn(ctx, app, ref, result, req)
	}
	var question flow.ErrQuestion
	if errors.As(handlerErr, &question) {
		// The backend call already happened inside stepCtx.AskQuestions;
		// question.Recorded carries the persisted questions with their ids
		// and timestamps.
		//
		// Stamp WHEN the question was asked — a reader scanning the item for
		// answers has no other way to tell a reply from the question itself.
		// Prefer the backend's own clock: the replies it later reports are
		// stamped by that same clock, and mixing in the local one means a
		// runner running fast discards answers it can never get back.
		marker := flow.MarkQuestionAskedLocal(time.Now())
		if len(question.Recorded) > 0 && !question.Recorded[0].AskedAt.IsZero() {
			// The backend's own clock — the one the answers will be stamped by.
			marker = flow.MarkQuestionAsked(question.Recorded[0].AskedAt)
		}
		req := flow.ParkRequest{
			Kind:    flow.ParkQuestion,
			Step:    li.Result(),
			Reason:  questionReason(question.Questions),
			Details: marker,
		}
		return parkAndReturn(ctx, app, ref, result, req)
	}
	var budget flow.ErrBudgetExhausted
	if errors.As(handlerErr, &budget) {
		return parkAndReturn(ctx, app, ref, result, flow.ParkRequest{
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

// completeStep is the completion path: the handler returned an election, so it
// is checked against the step's declaration and, on an artifact step, the
// payload is captured.
//
// Capture happens HERE — after the handler returned — rather than mid-handler,
// which puts it after RunOne's fitness catch-all and write-contract check. A
// step that violated its WriteContract therefore no longer leaves a captured
// artifact behind: the result is "verified before capture"
// (docs/flow-registration.md § Step configuration).
//
// One election, one check (flow.StepResult.Elect), and two ways it can end
// without a capture: a step that decided nothing PARKS, because a re-dispatch
// can still do the job; a step that decided something it may not decide FAILS,
// because only a change to the handler or the registration will help. Nothing
// is journaled and nothing is published in either case.
//
// It counts the dispatch itself, at each outcome, because one outcome does not
// count it: see chargeDispatch.
func completeStep(
	ctx context.Context,
	app *App,
	ref flow.ItemRef,
	result flow.InvocationResult,
	li flow.LifecycleItem,
	sctx *stepCtx,
	res flow.StepResult,
	state *flow.Item,
) (flow.InvocationResult, error) {
	body, err := res.Elect(li, app.artifactById[li.ArtifactId].Type)
	if err != nil {
		if cerr := chargeDispatch(ctx, app, ref, state, li); cerr != nil {
			return flow.InvocationResult{}, cerr
		}
		var incomplete flow.ErrStepDidNotComplete
		if errors.As(err, &incomplete) {
			return parkAndReturn(ctx, app, ref, result, flow.ParkRequest{
				Kind:   flow.ParkStepDidNotResolve,
				Step:   li.Result(),
				Reason: incomplete.Error(),
			})
		}
		result.Status = string(flow.StatusFailed)
		result.Reason = err.Error()
		return result, nil
	}
	if li.Kind == flow.LifecycleArtifact {
		if err := app.Orchestrator.ResolveArtifact(ctx, ref, li.ArtifactId, body); err != nil {
			var refused flow.ErrDisclosureRefused
			if errors.As(err, &refused) {
				// The ONE outcome that is not charged — the correction round
				// chargeDispatch names.
				return refusedCapture(ctx, app, ref, result, li, sctx, refused, body)
			}
			if cerr := chargeDispatch(ctx, app, ref, state, li); cerr != nil {
				return flow.InvocationResult{}, cerr
			}
			result.Status = string(flow.StatusFailed)
			result.Reason = err.Error()
			return result, nil
		}
		// The step has a result now, so its scaffolding is done. Clearing lives
		// HERE and nowhere else — the one place a step completes — so no handler
		// can complete while leaving stale prose behind for a later reader to
		// mistake for a record.
		//
		// Best-effort, and deliberately so. The artifact has already landed, so
		// failing the step now would report a failure for work that is recorded
		// — and a record that outlives its step is harmless anyway, because
		// keying by (item, step) means the next dispatch of a resolved step
		// never reads it. Keying is the correctness property; clearing is
		// hygiene.
		if err := app.Orchestrator.ClearWorkInProgress(ctx, ref, li.Result()); err != nil {
			sctx.Notify("", "could not clear work in progress: "+err.Error())
		}
	}
	if cerr := chargeDispatch(ctx, app, ref, state, li); cerr != nil {
		return flow.InvocationResult{}, cerr
	}
	result.Status = string(flow.StatusDone)
	return result, nil
}

// chargeDispatch counts this dispatch against the step's record — the same
// count RunOne makes for every other way a dispatch ends, made here so the
// completion path can skip it for the one outcome that must not be charged.
//
// That outcome is a capture the disclosure guard refused. "A correction round
// is priced as a round, not as a dispatch. A dispatch is an attempt at the
// step; a refused expression of finished work is not a failed attempt, and a
// treasurer that charged it as one would report exhaustion after three refused
// sentences — naming the wrong problem" (docs/resolution.md § The treasurer),
// and three is the default invocation budget. The round is not free: the turn
// that produced the refused text was metered on the cost axis when it ran, and
// the next dispatch pays for its own.
//
// A signal step owns no record, so there is nothing to count against it.
//
// The count is mirrored into the in-memory record as well as written to the
// orchestrator, because the park paths downstream snapshot their axes from it:
// a timeout park reporting the pre-charge count would under-report the
// invocations axis, which is precisely the axis that re-parks the step once the
// operator grants the time.
func chargeDispatch(ctx context.Context, app *App, ref flow.ItemRef, state *flow.Item, li flow.LifecycleItem) error {
	if li.Kind != flow.LifecycleArtifact {
		return nil
	}
	if err := app.Orchestrator.BumpInvocations(ctx, ref, li.ArtifactId); err != nil {
		return fmt.Errorf("bump invocations: %w", err)
	}
	rec := state.Artifact(li.ArtifactId)
	rec.Invocations++
	rec.PromptsThisInvocation = 0 // mirror the backend exactly — it resets here too
	state.Artifacts[li.ArtifactId] = rec
	return nil
}

// refusedCapture is what a disclosure refusal at capture leaves behind.
//
// ResolveArtifact publishes, so it can refuse — and with capture after the
// handler returns there is no in-invocation revision loop to catch it any more.
// So the capture path does what that loop did on its last round: stash the
// refusal and the text it refused in the step's work-in-progress record, which
// is local and never published, and park. The next dispatch renders both into
// its prompt, so the author is answering something this one did not know — and
// that dispatch, not this refusal, is what the treasurer counts (chargeDispatch).
//
// The park reason carries NOTHING the guard said. A park IS published — the
// orchestrator posts the request through the same guard — and a refusal names
// what it found and quotes it (docs/disclosure.md § What a refusal carries), so
// a reason repeating the guard's answer carries the refused fragment into the
// park and gets the park itself refused: the run would die with nobody told
// anything. The act is the SDK's own closed vocabulary and is safe to publish;
// the guard's answer stays in the stash, which the next dispatch's prompt reads.
func refusedCapture(
	ctx context.Context,
	app *App,
	ref flow.ItemRef,
	result flow.InvocationResult,
	li flow.LifecycleItem,
	sctx *stepCtx,
	refused flow.ErrDisclosureRefused,
	body flow.ArtifactBody,
) (flow.InvocationResult, error) {
	// Best-effort: a stash that failed costs the next run a re-derivation, and
	// turning it into a failure would lose the park as well as the work.
	if err := sctx.RecordWorkInProgress(flow.RefusedRecord(refused, refusedPayload(body))); err != nil {
		sctx.Notify("", "could not record refused text: "+err.Error())
	}
	return parkAndReturn(ctx, app, ref, result, flow.ParkRequest{
		Kind: flow.ParkBlocked,
		Step: li.Result(),
		Reason: fmt.Sprintf(
			"the disclosure guard refused this step's result (%s); "+
				"what it refused and why are kept with the step for the next run",
			refused.Act),
	})
}

// refusedPayload renders a refused payload as the text to stash beside the
// refusal. The next dispatch reads it as prose, so each type is rendered as
// what a reader would need to see in order to revise it.
func refusedPayload(body flow.ArtifactBody) string {
	switch body.Type {
	case flow.ArtifactCommitHash:
		return body.CommitHash
	case flow.ArtifactMarkdown:
		return body.Markdown
	case flow.ArtifactJSON:
		return string(body.JSON)
	case flow.ArtifactFile:
		return fmt.Sprintf("%s\n\n%s", body.File.Name, body.File.Content)
	case flow.ArtifactPatch:
		return string(body.Patch.Diff)
	}
	// A flag carries no payload, so what was refused is the fact of the write.
	return fmt.Sprintf("(the %s artifact carries no payload)", body.Type)
}

// effectiveTimeout resolves a step's per-run deadline: the granted timeout
// from the record wins when set (`grant --timeout` raises it), then the
// binary's policy for the step, which already falls back to the package
// default. Shared by the dispatch deadline and the park-time axis snapshot so
// the two can never disagree about what the cap actually was.
func (app *App) effectiveTimeout(li flow.LifecycleItem, rec flow.ArtifactRecord) time.Duration {
	if rec.GrantedTimeout > 0 {
		return rec.GrantedTimeout
	}
	return app.stepBudget(li.Result()).Timeout
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
// chargeDispatch), prompts off the metered agent's own counter, and elapsed
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

// checkWriteContract compares the current worktree state against the
// pre-handler snapshot and the step's WriteContract. Returns an empty string
// when the contract holds, or a violation reason string.
//
// Uses the parent ctx (not stepCtx) for the post-handler reads, since the
// step's deadline may have been consumed.
func checkWriteContract(ctx context.Context, wt flow.Worktree, snap *writeSnapshot, wc flow.WriteContract) string {
	branch, berr := wt.CurrentBranch(ctx)
	branchChanged := berr == nil && branch != snap.branch

	if !wc.MayBranch && branchChanged {
		return fmt.Sprintf("branch moved: was %q, now %q", snap.branch, branch)
	}
	// A permitted branch switch necessarily moves HEAD to the target
	// branch's tip.  That is the branch permission's consequence, not an
	// independent commit, so the commit check is skipped when the branch
	// legitimately changed.
	if !wc.MayCommit && !(wc.MayBranch && branchChanged) {
		sha, err := wt.RevParse(ctx, flow.HeadRevision)
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

// lifecycleItemOf looks a step up on its flow by name. The name came from
// SelectFlow over this same flow, so a miss is a defect in the flow, not a
// state of the item.
func lifecycleItemOf(f *flow.Flow, name string) (flow.LifecycleItem, error) {
	li, ok := f.Item(name)
	if !ok {
		return flow.LifecycleItem{}, fmt.Errorf("flow %q has no step %q", f.Name(), name)
	}
	return li, nil
}

// blockedFromAdvancing answers the one question a block puts to an advance:
// does the item wait on items that are still open? It is the condition both of
// RunOne's clean stops turn on — the check before dispatch, and the reload
// after a step declared the blockers it found — and the one `status` reports
// through, so the advance and the report cannot disagree about whether the
// next run will run a step.
//
// Only waits-on-items. The person and condition kinds are park-derived, and
// the answer preflight, the budget gate and the park machinery already own
// them: an advance runs straight through those, so calling them blocked here
// would stop a step nothing is waiting on.
func blockedFromAdvancing(state *flow.Item) bool {
	return state.Blocked && state.BlockKind == flow.WaitsOnItems
}

// blockedOnItems is the report for a stop on the item's own blockedness, built
// from the loaded item and from nothing else: status blocked, the
// orchestrator's reason, the kind, and the declared blockers with their
// statuses. Both stops — the check before dispatch, and a step declaring the
// blockers it found — report through this one function, so the report and
// `status` come from one derivation. RunOne never inspects blocker statuses
// itself. Only ever called on an item the orchestrator reports blocked on
// items: both callers check that first.
func blockedOnItems(state *flow.Item, result flow.InvocationResult) flow.InvocationResult {
	result.Status = string(flow.StatusBlocked)
	result.Reason = state.BlockReason
	result.BlockKind = state.BlockKind
	result.BlockedBy = state.BlockedBy
	return result
}

func parkAndReturn(
	ctx context.Context,
	app *App,
	ref flow.ItemRef,
	result flow.InvocationResult,
	req flow.ParkRequest,
) (flow.InvocationResult, error) {
	if err := app.Orchestrator.Park(ctx, ref, req); err != nil {
		return flow.InvocationResult{}, fmt.Errorf("orchestrator.Park: %w", err)
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
	branch    flow.BranchName
	commitSHA flow.CommitSha
}

// stepCtx is the concrete StepCtx the orchestrator hands to handlers. It
// memoises the lazily-acquired Worktree and the step's work-in-progress record.
type stepCtx struct {
	ctx      context.Context
	app      *App
	claim    flow.Claim
	flow     *flow.Flow
	li       flow.LifecycleItem
	state    *flow.Item
	worktree flow.Worktree
	wtErr    error
	agent    *meteredAgent
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

func newStepCtx(ctx context.Context, app *App, claim flow.Claim, f *flow.Flow, li flow.LifecycleItem, state *flow.Item, timeout time.Duration) *stepCtx {
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
		// agentImpl, not App.Agent: the field refuses Run() so nothing but a
		// step dispatch can spend (see outsideStepAgent). This is that
		// dispatch.
		inner:   app.agentImpl(),
		orch:    app.Orchestrator,
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
		_ = s.app.Orchestrator.AddDuration(s.ctx, s.claim.ItemRef, s.li.ArtifactId, elapsed)
	}
	return r, nil
}

func (s *stepCtx) Context() context.Context { return s.ctx }
func (s *stepCtx) Flow() string             { return s.flow.Name() }
func (s *stepCtx) Description() string      { return s.li.Description }
func (s *stepCtx) Result() flow.ArtifactId  { return s.li.ArtifactId }
func (s *stepCtx) Item() flow.Item          { return *s.state }
func (s *stepCtx) Claim() flow.Claim        { return s.claim }
func (s *stepCtx) VerifyCmd() string        { return s.app.VerifyCmd }

// Runner is the account this resolution acts as: the claim's, which is the
// account every write of this dispatch is made by.
func (s *stepCtx) Runner() flow.AccountId { return s.claim.Account }

// Role is the declared role this dispatch runs under — the step's own tag, and
// nothing derived at runtime.
func (s *stepCtx) Role() flow.RoleName { return s.li.Role }

// RoleAccount is the account of record for a role on this item.
//
// The declaration decides whether the question can be asked, and the journal
// answers it: an undeclared name is refused rather than answered empty, because
// empty means "declared and has not acted yet" and a handler waits on that
// (docs/resolution.md § Whose move it is).
func (s *stepCtx) RoleAccount(role flow.RoleName) (flow.AccountId, error) {
	if !s.flow.DeclaresRole(role) {
		// The declared set travels with the refusal. The raiser can enumerate
		// it here, and a handler told only that its name is unknown has to go
		// and read the registration to find out what it should have asked for.
		return "", flow.ErrUnknownRole{Role: role, Declared: s.flow.RoleNames()}
	}
	return s.state.AccountForRole(role), nil
}

// Journal returns the item's journal whole — a COPY, so a handler ranging it
// cannot rewrite the route through the slice it was handed. Symmetric with
// Flow.Items, and for the same reason.
func (s *stepCtx) Journal() []flow.JournalEntry {
	if s.state == nil {
		return nil
	}
	return slices.Clone(s.state.Journal)
}

// Transfer is the entry that routed here: the last one appended, carrying the
// predecessor and its message.
//
// The same entry the pending step is derived from (Flow.Position), so "the
// entry that routed here" has one definition. Nil on an empty journal, where
// nothing routed anywhere and the item is at its entry step.
func (s *stepCtx) Transfer() *flow.JournalEntry {
	entry, ok := s.state.LastEntry()
	if !ok {
		return nil
	}
	return &entry
}

// Notes returns every entry carrying a standing note, in journal order.
func (s *stepCtx) Notes() []flow.JournalEntry {
	if s.state == nil {
		return nil
	}
	var out []flow.JournalEntry
	for _, e := range s.state.Journal {
		if e.Note != "" {
			out = append(out, e)
		}
	}
	return out
}

// RunNumber is which dispatch of the pending step this is, 1-based.
//
// Read from the record's invocation counter, which the bump at the end of every
// dispatch maintains: this dispatch is the one after those. A signal step owns
// no record, so it reads 1 — accurate for the only counter it has.
//
// The counter is the treasurer's, so it does not move for the one dispatch that
// is not charged as one — a result the disclosure guard refused (chargeDispatch)
// — and a dispatch resuming from that refusal reads the same number as the one
// that was refused. That is the ledger this reads having one carve-out, not two
// counters: the step's own count lands with the ledger itself (#240).
func (s *stepCtx) RunNumber() int {
	if s.li.Kind != flow.LifecycleArtifact {
		return 1
	}
	return s.state.Artifact(s.li.ArtifactId).Invocations + 1
}

// Next and Finalize build the two elections. They are on the context rather
// than free functions because an election is made BY a step: the constructor a
// handler reaches for is the one on the handle it was given.
func (s *stepCtx) Next(step flow.StepId, message string) flow.StepResult {
	return flow.StepResult{Route: flow.Route{Next: step}, Message: message}
}

func (s *stepCtx) Finalize(d flow.Disposition, message string) flow.StepResult {
	return flow.StepResult{Route: flow.Route{Finalize: d}, Message: message}
}

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

func (s *stepCtx) Park(req flow.ParkRequest) error { return flow.ErrPark{Req: req} }

func (s *stepCtx) AskQuestions(qs ...flow.AgentQuestion) error {
	if len(qs) == 0 {
		return errors.New("ctx.AskQuestions called with no questions")
	}
	// One call per question. AskQuestion APPENDS — there is no replace — so a
	// step asking three records three, and a partly-failed batch is not a state
	// this loop can reach: it stops at the first failure and reports exactly
	// what was recorded before it.
	recorded := make([]flow.Question, 0, len(qs))
	for _, q := range qs {
		rec, err := s.app.Orchestrator.AskQuestion(s.ctx, s.claim.ItemRef, q)
		if err != nil {
			// A disclosure refusal is returned as-is so the handler-level
			// revision loop can catch it and re-prompt the agent.
			var refused flow.ErrDisclosureRefused
			if errors.As(err, &refused) {
				return err
			}
			return fmt.Errorf("orchestrator.AskQuestion: %w", err)
		}
		// A question park's entire recovery path is `answer --question <id>`,
		// and THE RETURN IS WHERE A QuestionId COMES FROM. One that comes back
		// without an id registered nothing an operator can name, so fail the
		// step here — where the ask route can still see what was recorded —
		// rather than let the park be written and discovered later, on an item
		// nothing can move forward.
		if rec.ID == "" {
			return fmt.Errorf("orchestrator.AskQuestion recorded %q without a question id: "+
				"parking on a question nothing registered leaves an item `answer` cannot clear", q.Header)
		}
		recorded = append(recorded, rec)
	}
	return flow.ErrQuestion{Questions: qs, Recorded: recorded}
}

// WaitOnItems records each ref as a blocker on the item and returns the
// sentinel RunOne reads as a clean stop on the item's own blockedness.
//
// One edit per ref, not one edit carrying all of them: the GitHub editor
// refuses more than one dependency change per commit — each is its own
// request, so two cannot land atomically (pkg/orchestrator/github/edit.go) —
// and an editor that could take them together gains nothing from it. A commit
// refusal (an unresolvable ref, a cycle, the item named as its own blocker)
// returns as an ordinary error naming the ref, so the step fails visibly.
// Blockers already committed stay: adding one is idempotent, and retracting
// them would be a second write that could fail the same way.
func (s *stepCtx) WaitOnItems(refs ...flow.ItemRef) error {
	if len(refs) == 0 {
		return errors.New("ctx.WaitOnItems called with no items")
	}
	for _, ref := range refs {
		ed, err := s.app.Orchestrator.Edit(s.ctx, s.claim.ItemRef)
		if err != nil {
			return fmt.Errorf("record %s as a blocker: %w", ref.Display, err)
		}
		ed.AddBlocker(ref)
		if err := ed.Commit(s.ctx); err != nil {
			return fmt.Errorf("record %s as a blocker: %w", ref.Display, err)
		}
	}
	return flow.ErrWaitsOnItems{Refs: refs}
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
	s.wip, s.wipErr = s.app.Orchestrator.LoadWorkInProgress(s.ctx, s.claim.ItemRef, s.li.Result())
	if s.wipErr != nil && errors.Is(s.wipErr, flow.ErrUnsupported) {
		// An orchestrator with no store reads as ABSENCE rather than as an
		// error: the record is optional, and a step that had to distinguish
		// "nothing stashed" from "no store" to build its prompt would be a step
		// no such orchestrator could run.
		s.wip, s.wipErr = "", nil
	}
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
	if err := s.app.Orchestrator.SaveWorkInProgress(s.ctx, s.claim.ItemRef, s.li.Result(), body); err != nil {
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
		step = string(s.li.Result())
	}
	s.app.Telemetry.StepProgress(s.ctx, s.claim, step, detail)
}

func (s *stepCtx) Agent() flow.Agent { return s.agent }

func (s *stepCtx) Worktree() (flow.Worktree, error) {
	if s.worktree != nil || s.wtErr != nil {
		return s.worktree, s.wtErr
	}
	s.worktree, s.wtErr = s.app.Orchestrator.Worktree(s.ctx, s.claim.ItemRef)
	if s.wtErr != nil {
		return nil, s.wtErr
	}
	// Capture a snapshot for the write-contract check. If either read
	// fails, leave writeSnap nil — fail-open on infrastructure error,
	// since the handler hasn't run yet.
	branch, berr := s.worktree.CurrentBranch(s.ctx)
	sha, serr := s.worktree.RevParse(s.ctx, flow.HeadRevision)
	if berr == nil && serr == nil {
		s.writeSnap = &writeSnapshot{branch: branch, commitSHA: sha}
	}
	return s.worktree, nil
}

func (s *stepCtx) RefreshItem() error {
	state, err := s.app.Orchestrator.Load(s.ctx, s.claim.ItemRef)
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
	orch    flow.Orchestrator
	claim   flow.Claim
	stepCtx *stepCtx

	promptsThisInvocation int
	costThisInvocation    float64
}

func (m *meteredAgent) Name() string { return m.inner.Name() }

func (m *meteredAgent) Run(ctx context.Context, req flow.AgentRequest) (*flow.AgentResponse, error) {
	li := m.stepCtx.li
	// Where the agent edits is not the handler's to choose: it is the arena the
	// orchestrator was constructed against — the same tree the commit is taken
	// in and the gates measure. Stamped here rather than at each construction
	// site because every construction site forgot it (#303), and a request with
	// no directory inherits the process working directory.
	//
	// Assigned unconditionally, the empty answer included. An orchestrator with
	// no local checkout has no directory to offer, and a handler's own path is
	// not a stand-in for one: keeping it would let a step choose the tree by the
	// back door of the orchestrator having nothing to say, which is the one
	// thing docs/agent.md says this field is never set by.
	req.Worktree = m.orch.ArenaRoot()
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
			Step: string(li.Result()),
			Axis: flow.AxisPrompts,
			Cap:  fmt.Sprintf("%d", art.GrantedPromptsPerInvocation),
		}
	}
	if art.GrantedCostUSD > 0 && art.CostUSDSpent >= art.GrantedCostUSD {
		return nil, flow.ErrBudgetExhausted{
			Step: string(li.Result()),
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
	if err := m.orch.BumpPrompts(ctx, m.claim.ItemRef, li.ArtifactId); err != nil {
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
		_ = m.orch.AddCost(ctx, m.claim.ItemRef, li.ArtifactId, resp.CostUSD)
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
			Step: string(li.Result()),
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
