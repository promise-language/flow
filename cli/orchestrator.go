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

// SelectFlow returns the binary's one flow and the pending step's description
// when it is eligible for the given item — every RequireSignal is set, and the
// journal's route has not finalized — and (nil, "") when it is not.
//
// The pending step comes from Flow.Position: the route the journal's last entry
// elected, or the declared entry step on an empty journal, and nothing else
// (docs/resolution.md § Deriving the next step). A step whose result is already
// recorded is dispatched again when the route names it — "reaching a step a
// second time is not an anomaly but a route" — which is what makes the rework
// handback an ordinary election rather than an edge nothing could take.
//
// Position's two refusals are RETURNED, never swallowed. An empty answer reads
// as "nothing left to do", and RunOne finalizes on it: reporting a flow that
// declares no entry, or a route naming no registered step, as a completed item
// would finish an item the flow never ran.
//
// It does NOT consult the remit. The remit gates listing and selection and is
// consulted before the journal's first entry, never after
// (docs/flow-registration.md § Item types), so it is read once, ahead of this,
// by inRemit — never per dispatch, where an item retyped mid-resolution would
// be re-routed by the edit.
//
// MIGRATION: position derives from the journal and nothing else, so an item
// left mid-resolution by a binary from before the journal was the record reads
// as an item with no journal — and restarts at the entry step. `reset` is the
// remedy. There is deliberately no fallback that reads the artifact records
// when the journal is empty: a second derivation of position is exactly what
// this retires, and one kept "for migration" is one that decides differently
// from the first on some item nobody has thought about yet.
func SelectFlow(app *App, item *flow.Item) (*flow.Flow, string, error) {
	f := app.Flow
	if !f.IsReady(item) {
		return nil, "", nil
	}
	pos, err := f.Position(item)
	if err != nil {
		return nil, "", err
	}
	if pos.Finalized {
		return nil, "", nil
	}
	return f, pos.Step.Description, nil
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
		// A Position refusal is the flow's own defect — no entry declared, or a
		// route naming nothing registered — so it comes back as the
		// catastrophic error it is rather than as an item state. Reporting it
		// as `done` would finalize an item on the strength of a graph that
		// could not say where it stood.
		var perr error
		if f, nextName, perr = SelectFlow(app, state); perr != nil {
			return flow.InvocationResult{}, perr
		}
	}
	if f == nil {
		// No step remains — the flow is complete (or the item is terminal).
		// Finalize + release the claim if the backend supports it, so a manual
		// run closes the item and frees the arena the same way the orchestrator
		// does on completion (instead of leaving it un-finalized + leased).
		reason := "no eligible flow — finalized + released"
		finalized := true
		if err := app.Orchestrator.Finalize(ctx, ref, finalDisposition(app, state)); err != nil {
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
	//
	// The pending step is resolved once, here, and feeds every report from this
	// point on: the two stops below and the base result the dispatch builds on.
	// Each names the step itself as what the route still points at, because
	// nothing a stop does moves the route.
	li, err := lifecycleItemOf(f, nextName)
	if err != nil {
		return flow.InvocationResult{}, err
	}
	if blockedFromAdvancing(state) {
		return blockedOnItems(state, stampNext(flow.InvocationResult{
			Flow: f.Name(),
			Item: claim.ItemRef.Display,
			Step: string(li.Result()),
		}, li)), nil
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
			return stampNext(flow.InvocationResult{
				Item:   claim.ItemRef.Display,
				Step:   string(li.Result()),
				Status: status,
				Reason: "preflight: " + perr.Error(),
			}, li), nil
		}
	}

	// Every later path builds on this by value, so a park, a failure, an await
	// skip and a blocked stop all report the step itself as still pending. Only
	// a completion overwrites it, with the successor the route elected.
	result := stampNext(flow.InvocationResult{
		Flow:         f.Name(),
		InvocationID: invocationID(),
		Item:         claim.ItemRef.Display,
		Step:         string(li.Result()),
	}, li)

	// AwaitSignal items have no handler: the item sits here, nobody's move,
	// until the orchestrator observes the signal. Skip without consuming
	// budget. Appending the wait's own entry once the signal IS observed —
	// which is what would let the route move past it — is #238's; no shipped
	// flow registers a wait, so nothing reaches this today.
	if li.Kind == flow.LifecycleAwait {
		result.Status = string(flow.StatusSkipped)
		result.Reason = fmt.Sprintf("awaiting signal %q", li.SignalId)
		return result, nil
	}

	// Pre-dispatch budget gate. Parks name the step by its RESULT ID (not the
	// label) so `grant` can act on the ledger row whose caps caused the park —
	// see ParkRequest.Step.
	//
	// The caps come from flow.EffectiveBudget: the binary's policy plus the
	// extensions recorded on the step's ledger row. That is the ONE arithmetic,
	// shared with `grant`, so the gate that refuses a dispatch and the top-up
	// meant to clear it cannot disagree about what the cap was.
	//
	// Artifact steps only, as before. A signal step owns a ledger row now, but
	// what may fund one is the treasurer's question (#235), not this gate's.
	budget := app.effectiveBudget(state, li.Result())
	row := state.Ledger.Row(li.Result())
	if li.Kind == flow.LifecycleArtifact {
		if budget.MaxInvocations > 0 && row.Dispatches >= budget.MaxInvocations {
			return parkAndReturn(ctx, app, ref, result, flow.ParkRequest{
				Kind: flow.ParkTreasurerRefused,
				Step: li.Result(),
				Axis: flow.AxisInvocations,
				// Prompts are per-invocation and this dispatch has not begun, so
				// nothing has been spent on that axis yet.
				Axes:   axisReports(row, budget, 0, 0),
				Reason: fmt.Sprintf("ran %d times without completing %q", row.Dispatches, li.Result()),
			})
		}
		if budget.MaxCostUSD > 0 && row.CostUSD >= budget.MaxCostUSD {
			return parkAndReturn(ctx, app, ref, result, flow.ParkRequest{
				Kind:   flow.ParkTreasurerRefused,
				Step:   li.Result(),
				Axis:   flow.AxisCost,
				Axes:   axisReports(row, budget, 0, 0),
				Reason: fmt.Sprintf("spent $%.2f without completing %q", row.CostUSD, li.Result()),
			})
		}
	}

	// Wrap a context for this invocation on the same effective timeout the gate
	// above judged by, so `grant --timeout` lands on the deadline as well as on
	// the check.
	timeout := budget.Timeout
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build the per-invocation StepCtx.
	sctx := newStepCtx(stepCtx, app, claim, f, li, state, budget)

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

	// A dispatch that picks the item up from a park on this very step is a
	// RESUMPTION, and the treasurer counts those apart from dispatches: one
	// number says how often the step was attempted, the other how often
	// something had to unstick it. Recorded once, before the dispatch, because
	// after it there is no longer a park to have resumed from.
	if state.Park != nil && state.Park.Step == li.Result() {
		if err := app.Orchestrator.RecordResumption(ctx, ref, li.Result()); err != nil {
			return flow.InvocationResult{}, fmt.Errorf("record resumption: %w", err)
		}
	}

	// Dispatch. The handler completes by RETURNING its election; res is read
	// only on the completion path (translateHandlerError's nil-error branch),
	// because every other way a dispatch ends is one where nothing was elected.
	res, handlerErr := li.Handler(sctx)

	// A prompt refused because the step declared Prompts: none. First among the
	// post-handler branches, and read off the chokepoint rather than off
	// handlerErr: a handler that swallowed the refusal, re-wrapped it without
	// %w, or turned it into ErrTransient would otherwise complete, fail as
	// charged, or park as infra-transient and be re-dispatched forever. The
	// declaration holds whatever the handler did with the error.
	//
	// The step PARKS rather than fails, so journal position and the claim
	// survive for whoever corrects it, and it parks ParkRefused — the kind that
	// classifies itself as not clearing by re-dispatch, because a mis-declared
	// step answers identically every time. No chargeDispatch: nothing was sent,
	// so nothing is billed for the violation, which is the guarantee the
	// declaration exists to give (docs/flow-registration.md § Step
	// configuration).
	if err := sctx.agent.refusedPrompt; err != nil {
		return sctx.stampResult(parkAndReturn(ctx, app, ref, result, flow.ParkRequest{
			Kind:   flow.ParkRefused,
			Step:   li.Result(),
			Reason: err.Error(),
		}))
	}

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
			Kind: flow.ParkTreasurerRefused,
			Step: li.Result(),
			Axis: flow.AxisTimeout,
			// The charge above is already counted here: a timeout park that
			// under-reported dispatches is exactly what sent the operator back
			// for a second grant.
			Axes:   sctx.axisReports(),
			Reason: fmt.Sprintf("step %q exceeded %s", li.Result(), timeout),
		}))
	}

	// Machine unfit (handler returned flow.ErrUnfit). The machine is not
	// fit to perform work — e.g. disk full. No park (a machine condition
	// has no step and ends on its own), no dispatch counted (a condition is
	// not a failure), status blocked. The claim is kept.
	if handlerErr != nil && errors.Is(handlerErr, flow.ErrUnfit) {
		result.Status = string(flow.StatusBlocked)
		result.Reason = handlerErr.Error()
		return sctx.stampResult(result, nil)
	}

	// Transient infra failure (handler returned flow.ErrTransient OR the
	// metered agent observed AgentResponse.Failure.Transient and surfaced
	// it through the wrapped error). Park with ParkInfraTransient and
	// SKIP the dispatch count — a flapping runner must not burn the
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
	// ParkRefused and SKIP the dispatch count — symmetric with the
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
	// statuses included. No dispatch counted: the work exists elsewhere and will
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
			Kind:   flow.ParkTreasurerRefused,
			Step:   li.Result(),
			Axis:   budget.Axis,
			Axes:   sctx.axisReports(),
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
				Kind:   flow.ParkStepDidNotComplete,
				Step:   li.Result(),
				Reason: incomplete.Error(),
			})
		}
		result.Status = string(flow.StatusFailed)
		result.Reason = err.Error()
		return result, nil
	}
	// ONE APPEND. The captured result and the elected route are the same write
	// — "result and route land together or not at all" — and the entry also
	// carries what the item now awaits, who ran the step and what it spent. The
	// separate ClearWorkInProgress is gone with it: the draft ends where the
	// result begins, inside the one write.
	entry := flow.JournalEntry{
		Step:      li.Result(),
		Execution: executionOf(state, li.Result()),
		Result:    body,
		Route:     res.Route,
		Message:   res.Message,
		Note:      res.Note,
		// The SDK computes the awaited marker: the orchestrator has no flow, so
		// the step-to-role mapping cannot be derived on the far side.
		Awaits: sctx.flow.AwaitsAfter(res.Route),
		By:     sctx.claim.Account,
		Role:   li.Role,
		At:     time.Now().UTC(),
		Spend: flow.Spend{
			CostUSD:  sctx.agent.costThisInvocation,
			Duration: time.Since(sctx.startedAt),
		},
	}
	if err := app.Orchestrator.AppendEntry(ctx, ref, entry); err != nil {
		var refused flow.ErrDisclosureRefused
		if errors.As(err, &refused) {
			// The ONE outcome that is not charged — the correction round
			// chargeDispatch names. AppendEntry publishes, so it can still
			// refuse, and nothing is journaled when it does.
			return refusedCapture(ctx, app, ref, result, li, sctx, refused, body)
		}
		if cerr := chargeDispatch(ctx, app, ref, state, li); cerr != nil {
			return flow.InvocationResult{}, cerr
		}
		result.Status = string(flow.StatusFailed)
		result.Reason = err.Error()
		return result, nil
	}
	if cerr := chargeDispatch(ctx, app, ref, state, li); cerr != nil {
		return flow.InvocationResult{}, cerr
	}
	result.Status = string(flow.StatusDone)
	// The route moved: what is pending now is the elected successor, or nothing
	// on a finalizing route.
	return stampNextAfter(result, sctx.flow, res.Route), nil
}

// stampNext reports li as the lifecycle item the route now points at: its
// name, and whether dispatching it invokes no agent. Both fields come from the
// item's own declaration (LifecycleItem.Mechanical), so the envelope and the
// chokepoint that enforces the declaration cannot disagree.
func stampNext(r flow.InvocationResult, li flow.LifecycleItem) flow.InvocationResult {
	mechanical := li.Mechanical()
	r.NextStep = string(li.Result())
	r.NextMechanical = &mechanical
	return r
}

// stampNextAfter is stampNext for a route just journaled: the successor it
// elected, looked up on the flow the way AwaitsAfter looks it up, or nothing
// when the route finalizes — absent means nothing is pending, which is what a
// finalized item reports (docs/cli.md § Output).
//
// The lookup cannot miss on a validated flow: Elect refused any route outside
// the step's declared successors, and ValidateGraph refused any declared
// successor naming nothing registered.
func stampNextAfter(r flow.InvocationResult, f *flow.Flow, route flow.Route) flow.InvocationResult {
	r.NextStep, r.NextMechanical = "", nil
	if route.Finalizes() {
		return r
	}
	if succ, ok := f.ItemByResult(route.Next); ok {
		r = stampNext(r, succ)
	}
	return r
}

// executionOf is which completed execution of this step the entry about to be
// appended is, 1-based: the prior entries for it, plus this one.
//
// Counted off the journal rather than off a stored number, because the journal
// is the record — a counter beside it would be a second answer to a question
// the entries already settle.
func executionOf(state *flow.Item, step flow.StepId) int {
	n := 1
	for _, e := range state.Journal {
		if e.Step == step {
			n++
		}
	}
	return n
}

// finalDisposition is what a Finalize records: the disposition the finalizing
// election carried, read from the journal, and DispositionResolved on the
// legacy no-flow branch — where nothing elected anything and the flow reaching
// its end is the whole record there is.
func finalDisposition(app *App, state *flow.Item) flow.Disposition {
	if pos, err := app.Flow.Position(state); err == nil && pos.Finalized {
		return pos.Disposition
	}
	return flow.DispositionResolved
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
// It counts a SIGNAL step's dispatch too. A ledger row is keyed by StepId, not
// by an artifact, so a step that completes on an observation has a row like any
// other — and a step dispatched a hundred times without its signal arriving is
// exactly the thing a treasurer needs to be able to see.
//
// The count is mirrored into the in-memory ledger as well as written to the
// orchestrator, because the park paths downstream snapshot their axes from it:
// a timeout park reporting the pre-charge count would under-report the
// invocations axis, which is precisely the axis that re-parks the step once the
// operator grants the time.
func chargeDispatch(ctx context.Context, app *App, ref flow.ItemRef, state *flow.Item, li flow.LifecycleItem) error {
	step := li.Result()
	if err := app.Orchestrator.RecordDispatch(ctx, ref, step); err != nil {
		return fmt.Errorf("record dispatch: %w", err)
	}
	mirrorLedger(state, step, func(row *flow.LedgerRow) { row.Dispatches++ })
	return nil
}

// mirrorLedger applies a ledger write to the loaded item, so a park snapshot
// taken later in the same dispatch reads what the orchestrator now holds rather
// than what it held when the item was loaded.
func mirrorLedger(state *flow.Item, step flow.StepId, mutate func(*flow.LedgerRow)) flow.LedgerRow {
	row := state.Ledger.Row(step)
	row.Step = step
	mutate(&row)
	if state.Ledger.Steps == nil {
		state.Ledger.Steps = map[flow.StepId]flow.LedgerRow{}
	}
	state.Ledger.Steps[step] = row
	return row
}

// refusedCapture is what a disclosure refusal at capture leaves behind.
//
// AppendEntry publishes, so it can refuse — and with capture after the
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

// effectiveBudget resolves a step's caps: the binary's policy for it, plus the
// extensions recorded on its ledger row. Shared by the pre-dispatch gate, the
// dispatch deadline, the metered agent and the park-time axis snapshot, so none
// of them can disagree about what the cap actually was.
func (app *App) effectiveBudget(state *flow.Item, step flow.StepId) flow.StepBudget {
	return flow.EffectiveBudget(app.stepBudget(step), state.Ledger.Row(step))
}

// axisReports snapshots all four budget axes for a treasurer-refused park.
//
// Every axis is reported, never just the one that tripped. The axes go flat
// together — a run that times out has usually burned its invocations too, and
// a step parked on prompts is often already over on cost — so a park naming
// one axis sent the operator back for another grant as soon as the next
// dispatch re-parked on the next axis. One park, one report, one grant.
//
// prompts is passed in rather than read off the ledger: the prompt cap is
// per-invocation and the ledger keeps no counter for it, so what the operator
// needs to judge the cap by is what the run that just parked actually spent.
// elapsed is likewise unrecorded — it is wall time against the deadline,
// meaningful only for a park that happens after dispatch, and zero for the
// pre-dispatch gates.
func axisReports(row flow.LedgerRow, budget flow.StepBudget, prompts int, elapsed time.Duration) []flow.AxisReport {
	return []flow.AxisReport{
		flow.NewAxisReport(flow.AxisInvocations, float64(row.Dispatches), float64(budget.MaxInvocations)),
		flow.NewAxisReport(flow.AxisPrompts, float64(prompts), float64(budget.MaxPromptsPerInvocation)),
		flow.NewAxisReport(flow.AxisCost, row.CostUSD, budget.MaxCostUSD),
		flow.NewAxisReport(flow.AxisTimeout, elapsed.Seconds(), budget.Timeout.Seconds()),
	}
}

// axisReports is the post-dispatch snapshot: same four axes, read from the live
// view of the invocation rather than the loaded row alone. Cost and dispatches
// come off the in-memory mirror (kept current by meteredAgent and
// chargeDispatch), prompts off the metered agent's own counter, and elapsed off
// the step's start.
func (sc *stepCtx) axisReports() []flow.AxisReport {
	if sc.li.Kind != flow.LifecycleArtifact {
		return nil
	}
	var prompts int
	if sc.agent != nil {
		prompts = sc.agent.promptsThisInvocation
	}
	return axisReports(sc.state.Ledger.Row(sc.li.Result()), sc.budget, prompts, time.Since(sc.startedAt))
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
	// Every park site routes through here, so the classification is carried
	// outward once and no site can forget it.
	mayClear := req.Kind.RedispatchMayClear()
	result.RedispatchMayClear = &mayClear
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
	// startedAt and budget back the park-time axis snapshot: elapsed-vs-cap is
	// the one axis with no counter in the ledger, and the caps are the ones the
	// pre-dispatch gate judged by — read once, so nothing downstream can resolve
	// them a second way.
	startedAt time.Time
	budget    flow.StepBudget
	// writeSnap is the worktree state captured when the handler first acquires
	// the worktree. nil when no worktree was acquired (no check will run).
	writeSnap *writeSnapshot
}

func newStepCtx(ctx context.Context, app *App, claim flow.Claim, f *flow.Flow, li flow.LifecycleItem, state *flow.Item, budget flow.StepBudget) *stepCtx {
	sc := &stepCtx{
		ctx:       ctx,
		app:       app,
		claim:     claim,
		flow:      f,
		li:        li,
		state:     state,
		startedAt: time.Now(),
		budget:    budget,
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
// and persists the step's ACTIVE duration to its ledger row. When err is non-nil the
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
	// ACTIVE time. A wait on a declared exclusion is reported by the party that
	// held it, through AddWaiting, and never lands here.
	_ = s.app.Orchestrator.AddDuration(s.ctx, s.claim.ItemRef, s.li.Result(), elapsed)
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
// Read from the ledger row's dispatch count, which chargeDispatch maintains at
// the end of every dispatch: this dispatch is the one after those. A signal step
// has a row like any other, so it reads truthfully too.
//
// The count is the treasurer's, so it does not move for the one dispatch that is
// not charged as one — a result the disclosure guard refused (chargeDispatch) —
// and a dispatch resuming from that refusal reads the same number as the one
// that was refused. That is the ledger having one carve-out, not two counters.
func (s *stepCtx) RunNumber() int {
	return s.state.Ledger.Row(s.li.Result()).Dispatches + 1
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

	// refusedPrompt is the first prompt refused because the step declared
	// Prompts: none — kept so RunOne parks on it whatever the handler did with
	// the error it was handed. nil until a mechanical step asks.
	refusedPrompt error
}

func (m *meteredAgent) Name() string { return m.inner.Name() }

func (m *meteredAgent) Run(ctx context.Context, req flow.AgentRequest) (*flow.AgentResponse, error) {
	li := m.stepCtx.li
	// A mechanical step is refused FIRST — before the unmetered pass-through a
	// signal step takes, before the cap checks, and before the prompt counter
	// moves. Any later and a refused prompt would be counted as one, and a
	// mechanical step whose prompt cap was exhausted would park
	// treasurer-refused instead of naming the real defect. Nothing is sent, so
	// nothing is billed for the violation: that is the whole point of refusing
	// before the request goes out rather than after an answer comes back
	// (docs/flow-registration.md § Step configuration).
	//
	// The site is the frame that called Run — the handler, or the helper it
	// prompts through — so the park can name what asked.
	if li.Mechanical() {
		if m.refusedPrompt == nil {
			m.refusedPrompt = mechanicalPromptRefusal(li.Result(), callSite(0))
		}
		return nil, m.refusedPrompt
	}
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
	// Signal/await steps carry no cap policy worth metering. Allow the call to
	// pass through unmetered — those steps shouldn't normally call the agent,
	// but if they do the spend is not gated here.
	if li.Kind != flow.LifecycleArtifact {
		return m.inner.Run(ctx, req)
	}
	step := li.Result()
	row := m.stepCtx.state.Ledger.Row(step)
	budget := m.stepCtx.budget
	// Both caps name the step by result id: the message tells the operator
	// exactly what to pass to `grant`.
	if budget.MaxPromptsPerInvocation > 0 && m.promptsThisInvocation >= budget.MaxPromptsPerInvocation {
		return nil, flow.ErrBudgetExhausted{
			Step: string(step),
			Axis: flow.AxisPrompts,
			Cap:  fmt.Sprintf("%d", budget.MaxPromptsPerInvocation),
		}
	}
	if budget.MaxCostUSD > 0 && row.CostUSD >= budget.MaxCostUSD {
		return nil, flow.ErrBudgetExhausted{
			Step: string(step),
			Axis: flow.AxisCost,
			Cap:  fmt.Sprintf("$%.2f", budget.MaxCostUSD),
		}
	}
	// Hand the turn the headroom left in the budget, so the substrate can stop
	// it at the cap. Without this the cap only bounds when a step stops being
	// dispatched: a turn that starts inside it can spend whatever it spends, and
	// the overrun is discovered one whole turn late. A handler that set its own
	// ceiling asked for a TIGHTER one than the step's, so narrow to it —
	// overwriting would silently widen the very bound the handler wrote down.
	if headroom := budget.MaxCostUSD - row.CostUSD; budget.MaxCostUSD > 0 &&
		(req.MaxCostUSD <= 0 || headroom < req.MaxCostUSD) {
		req.MaxCostUSD = headroom
	}
	// The prompt counter lives HERE and not in the ledger. The cap is
	// per-invocation and every dispatch resets it, so it was never durable
	// across one: counting in memory is the same number by a shorter route, and
	// the axis survives as a park and grant axis whose granted extensions do
	// live on the row.
	m.promptsThisInvocation++

	resp, err := m.inner.Run(ctx, req)
	// Skip cost accounting on transient infra failures — symmetric with the
	// orchestrator's skip-the-dispatch-count policy for ParkInfraTransient. A
	// flapping runner must not burn the cost axis any more than it burns the
	// invocations axis.
	transient := resp != nil && resp.Failure != nil && resp.Failure.Transient
	if err == nil && resp != nil && resp.CostUSD > 0 && !transient {
		_ = m.orch.AddCost(ctx, m.claim.ItemRef, step, resp.CostUSD)
		// Update the local mirror so subsequent calls, and the park snapshot,
		// see fresh cost.
		row = mirrorLedger(m.stepCtx.state, step, func(r *flow.LedgerRow) { r.CostUSD += resp.CostUSD })
		m.costThisInvocation += resp.CostUSD
	}
	// A turn the substrate stopped at the cap we set IS this step reaching
	// its cost cap, so it parks on cost through the same sentinel the
	// pre-prompt gate returns — one park path, one axis snapshot, and the
	// AddCost above has already put the true spend on the mirror the
	// snapshot reads. Without a cost cap the stop was never ours to claim:
	// fall through to the ordinary agent failure.
	if err == nil && resp != nil && resp.Failure != nil &&
		resp.Failure.Kind == flow.FailureCostCap && budget.MaxCostUSD > 0 {
		return resp, flow.ErrBudgetExhausted{
			Step: string(step),
			Axis: flow.AxisCost,
			Cap:  fmt.Sprintf("$%.2f", budget.MaxCostUSD),
		}
	}
	// Surface AgentResponse.Failure through the error return so the
	// canonical "if err != nil { return err }" pattern in handlers picks
	// it up without forcing every handler to interrogate resp.Failure
	// separately. If Failure.Transient is set, the returned error wraps
	// flow.ErrTransient — the orchestrator's transient check will park
	// the step and skip the dispatch count.
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
