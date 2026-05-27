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

	// Select the flow first; we need it for seeding too.
	f, nextName := SelectFlow(app, state)
	if f == nil {
		return flow.InvocationResult{
			Flow:   "",
			Item:   claim.ItemRef.Display,
			Status: "done",
			Reason: "no eligible flow",
		}, nil
	}

	// One-shot seed: zero artifacts means the backend hasn't been seeded yet.
	if len(state.Artifacts) == 0 {
		if err := app.Backend.SeedState(ctx, claim, f.SeedSpec(app.artifactById)); err != nil {
			return flow.InvocationResult{}, fmt.Errorf("seed state: %w", err)
		}
		state, err = app.Backend.LoadState(ctx, claim)
		if err != nil {
			return flow.InvocationResult{}, fmt.Errorf("reload after seed: %w", err)
		}
		// Re-derive after seed (rare: seed could change derivation).
		f, nextName = SelectFlow(app, state)
		if f == nil {
			return flow.InvocationResult{
				Item:   claim.ItemRef.Display,
				Status: "done",
				Reason: "no eligible flow after seed",
			}, nil
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
	// own an artifact record yet on the first invocation).
	if li.Kind == flow.LifecycleArtifact {
		art := state.Artifact(li.ArtifactId)
		if art.GrantedInvocations > 0 && art.Invocations >= art.GrantedInvocations {
			return parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
				Kind:   flow.ParkBudgetExhausted,
				Step:   nextName,
				Axis:   flow.AxisInvocations,
				Reason: fmt.Sprintf("ran %d times without resolving %q", art.Invocations, li.ArtifactId),
			})
		}
		if art.GrantedCostUSD > 0 && art.CostUSDSpent >= art.GrantedCostUSD {
			return parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
				Kind:   flow.ParkBudgetExhausted,
				Step:   nextName,
				Axis:   flow.AxisCost,
				Reason: fmt.Sprintf("spent $%.2f without resolving %q", art.CostUSDSpent, li.ArtifactId),
			})
		}
	}

	// Wrap a context for this invocation. Use the step's resolved timeout.
	timeout := li.Budget.Timeout
	if timeout <= 0 {
		timeout = flow.DefaultStepBudget().Timeout
	}
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Bump invocations BEFORE dispatch so a crash still counts.
	budgetKey := li.Result()
	if li.Kind == flow.LifecycleArtifact {
		if err := app.Backend.BumpInvocations(stepCtx, claim, budgetKey); err != nil {
			return flow.InvocationResult{}, fmt.Errorf("bump invocations: %w", err)
		}
	}

	// Build the per-invocation StepCtx.
	sctx := newStepCtx(stepCtx, app, claim, f, li, state)

	// Dispatch.
	handlerErr := li.Handler(sctx)

	// Timeout (deadline reached during handler).
	if errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
		// Best-effort capture — ignore errors, the patch is opportunistic.
		if wt, e := app.Backend.Worktree(ctx, claim); e == nil {
			_, _ = wt.CapturePatch(ctx)
		}
		return parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
			Kind:   flow.ParkBudgetExhausted,
			Step:   nextName,
			Axis:   flow.AxisTimeout,
			Reason: fmt.Sprintf("step %q exceeded %s", nextName, timeout),
		})
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
				Step:   li.Name,
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
			req.Step = li.Name
		}
		return parkAndReturn(ctx, app, claim, result, req)
	}
	var question flow.ErrQuestion
	if errors.As(handlerErr, &question) {
		if _, err := app.Backend.AskQuestions(ctx, claim, question.Questions); err != nil {
			return flow.InvocationResult{}, fmt.Errorf("backend.AskQuestions: %w", err)
		}
		req := flow.ParkRequest{Kind: flow.ParkQuestion, Step: li.Name, Reason: questionReason(question.Questions)}
		return parkAndReturn(ctx, app, claim, result, req)
	}
	var budget flow.ErrBudgetExhausted
	if errors.As(handlerErr, &budget) {
		return parkAndReturn(ctx, app, claim, result, flow.ParkRequest{
			Kind:   flow.ParkBudgetExhausted,
			Step:   li.Name,
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
	if art.GrantedPromptsPerInvocation > 0 && m.promptsThisInvocation >= art.GrantedPromptsPerInvocation {
		return nil, flow.ErrBudgetExhausted{
			Step: li.Name,
			Axis: flow.AxisPrompts,
			Cap:  fmt.Sprintf("%d", art.GrantedPromptsPerInvocation),
		}
	}
	if art.GrantedCostUSD > 0 && art.CostUSDSpent >= art.GrantedCostUSD {
		return nil, flow.ErrBudgetExhausted{
			Step: li.Name,
			Axis: flow.AxisCost,
			Cap:  fmt.Sprintf("$%.2f", art.GrantedCostUSD),
		}
	}
	if err := m.backend.BumpPrompts(ctx, m.claim, string(li.ArtifactId)); err != nil {
		return nil, fmt.Errorf("bump prompts: %w", err)
	}
	m.promptsThisInvocation++

	resp, err := m.inner.Run(ctx, req)
	if err == nil && resp != nil && resp.CostUSD > 0 {
		_ = m.backend.AddCost(ctx, m.claim, string(li.ArtifactId), resp.CostUSD)
		// Update local mirror so subsequent calls see fresh cost.
		art.CostUSDSpent += resp.CostUSD
		m.stepCtx.state.Artifacts[li.ArtifactId] = art
	}
	return resp, err
}
