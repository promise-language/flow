package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/promise-language/flow"
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
					Status: "failed",
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
					Status: "failed",
					Reason: "finalize: " + err.Error(),
				}, nil
			}
			reason = "no eligible flow — finalized + released"
		}
		return flow.InvocationResult{
			Flow:   "",
			Item:   claim.ItemRef.Display,
			Status: "done",
			Reason: reason,
		}, nil
	}

	// Cross-flow preflight gate. Runs AFTER LoadState (fresh state) and
	// AFTER the terminal-done check (so completed items finalize) but
	// BEFORE seed / handler dispatch. Non-nil error → skipped, no handler
	// runs, no budget consumed.
	if app.Preflight != nil {
		if perr := app.Preflight(ctx, state); perr != nil {
			return flow.InvocationResult{
				Item:   claim.ItemRef.Display,
				Status: "skipped",
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
		Step:         nextName,
	}

	// AwaitSignal items have no handler; signal is checked by DeriveNext.
	// Reaching here means the signal isn't set yet — skip without
	// consuming budget.
	if li.Kind == flow.LifecycleAwait {
		result.Status = "skipped"
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
				Reason: fmt.Sprintf("ran %d times without resolving %q", art.Invocations, li.ArtifactId),
			})
		}
		if art.GrantedCostUSD > 0 && art.CostUSDSpent >= art.GrantedCostUSD {
			return parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
				Kind:   flow.ParkBudgetExhausted,
				Step:   li.Result(),
				Axis:   flow.AxisCost,
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
	timeout := li.Budget.Timeout
	if art.GrantedTimeout > 0 {
		timeout = art.GrantedTimeout
	}
	if timeout <= 0 {
		timeout = flow.DefaultStepBudget().Timeout
	}
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build the per-invocation StepCtx.
	budgetKey := li.Result()
	sctx := newStepCtx(stepCtx, app, claim, f, li, state)

	// Auto-emit step entry so every step transition reaches the tracker
	// without each handler having to call ctx.Notify. Handlers that DO call
	// ctx.Notify with richer detail will override this baseline.
	if app.Telemetry != nil {
		app.Telemetry.StepProgress(stepCtx, claim, li.Name, "")
	}

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
			if err := app.Backend.BumpInvocations(ctx, claim, budgetKey); err != nil {
				return flow.InvocationResult{}, fmt.Errorf("bump invocations: %w", err)
			}
		}
		return parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
			Kind:   flow.ParkBudgetExhausted,
			Step:   li.Result(),
			Axis:   flow.AxisTimeout,
			Reason: fmt.Sprintf("step %q exceeded %s", nextName, timeout),
		})
	}

	// Transient infra failure (handler returned flow.ErrTransient OR the
	// metered agent observed AgentResponse.Failure.Transient and surfaced
	// it through the wrapped error). Park with ParkInfraTransient and
	// SKIP the BumpInvocations call — a flapping runner must not burn the
	// step's invocation budget.
	if handlerErr != nil && errors.Is(handlerErr, flow.ErrTransient) {
		return parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
			Kind:   flow.ParkInfraTransient,
			Step:   li.Result(),
			Reason: handlerErr.Error(),
		})
	}

	// Non-transient: the invocation produced a result (success, skip,
	// park, or a real failure). Count it.
	if li.Kind == flow.LifecycleArtifact {
		if err := app.Backend.BumpInvocations(ctx, claim, budgetKey); err != nil {
			return flow.InvocationResult{}, fmt.Errorf("bump invocations: %w", err)
		}
	}

	return translateHandlerError(ctx, app, claim, result, li, sctx, handlerErr)
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
		result.Status = "done"
		return result, nil
	}

	// Sentinel translations.
	var skip flow.ErrSkip
	if errors.As(handlerErr, &skip) {
		result.Status = "skipped"
		result.Reason = skip.Reason
		return result, nil
	}
	var park flow.ErrPark
	if errors.As(handlerErr, &park) {
		req := park.Req
		if req.Step == "" {
			req.Step = li.Result()
		}
		return parkAndReturn(ctx, app, claim, result, req)
	}
	var question flow.ErrQuestion
	if errors.As(handlerErr, &question) {
		if _, err := app.Backend.AskQuestions(ctx, claim, question.Questions); err != nil {
			return flow.InvocationResult{}, fmt.Errorf("backend.AskQuestions: %w", err)
		}
		req := flow.ParkRequest{Kind: flow.ParkQuestion, Step: li.Result(), Reason: questionReason(question.Questions)}
		return parkAndReturn(ctx, app, claim, result, req)
	}
	var budget flow.ErrBudgetExhausted
	if errors.As(handlerErr, &budget) {
		return parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
			Kind:   flow.ParkBudgetExhausted,
			Step:   li.Result(),
			Axis:   budget.Axis,
			Reason: budget.Error(),
		})
	}

	result.Status = "failed"
	result.Reason = handlerErr.Error()
	return result, nil
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
	result.Status = "parked"
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
		return "question: " + qs[0].Text
	}
	return fmt.Sprintf("%d questions pending (first: %s)", len(qs), qs[0].Text)
}

// invocationID returns a coarse-grained id for the current invocation.
// Format: <unix-ns>; collision-resistant within one worktree.
func invocationID() string {
	return fmt.Sprintf("inv-%d", time.Now().UnixNano())
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
}

func newStepCtx(ctx context.Context, app *App, claim flow.Claim, f *flow.Flow, li flow.LifecycleItem, state *flow.ItemState) *stepCtx {
	sc := &stepCtx{
		ctx:   ctx,
		app:   app,
		claim: claim,
		flow:  f,
		li:    li,
		state: state,
	}
	sc.agent = &meteredAgent{
		inner:   app.Agent,
		backend: app.Backend,
		claim:   claim,
		stepCtx: sc,
	}
	return sc
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
	// A zero-byte diff is never worth uploading: the record carries no
	// content, and on a rerun it would replace whatever the previous
	// invocation attached. A handler reaching here with nothing to show has
	// a bug — surface it instead of writing an empty patch.
	if len(body.Diff) == 0 && len(body.Untracked) == 0 {
		return fmt.Errorf("step %q resolved %q with an empty patch (no diff, no untracked files)", s.li.Name, s.li.ArtifactId)
	}
	return s.writeResolve(flow.ArtifactBody{Type: flow.ArtifactPatch, Patch: body})
}

func (s *stepCtx) writeResolve(body flow.ArtifactBody) error {
	if err := s.app.Backend.ResolveArtifact(s.ctx, s.claim, s.li.ArtifactId, body); err != nil {
		return err
	}
	s.resolved = true
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

func (s *stepCtx) Notify(step, detail string) {
	if s.app.Telemetry == nil {
		return
	}
	if step == "" {
		step = s.li.Name
	}
	s.app.Telemetry.StepProgress(s.ctx, s.claim, step, detail)
}

func (s *stepCtx) Agent() flow.Agent { return s.agent }

func (s *stepCtx) Worktree() (flow.Worktree, error) {
	if s.worktree != nil || s.wtErr != nil {
		return s.worktree, s.wtErr
	}
	s.worktree, s.wtErr = s.app.Backend.Worktree(s.ctx, s.claim)
	return s.worktree, s.wtErr
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
