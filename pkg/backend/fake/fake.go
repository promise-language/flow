// Package fake is an in-memory flow.Backend implementation used by SDK
// tests and by flow authors who want to exercise their flow logic without
// touching GitHub. NOT suitable for production.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/promise-language/flow"
)

// Backend is the in-memory backend.
type Backend struct {
	mu              sync.Mutex
	items           map[string]*itemRecord // keyed by item.ID
	signals         []flow.SignalDef
	clock           func() time.Time
	verifyOK        bool             // controls Worktree.Verify result
	gateOutcome     flow.GateOutcome // controls what Worktree.RunGate observes
	verdict         *bool            // controls what Worktree.Judge answers; nil = acceptable
	supportsRequest bool             // controls whether Worktree.Request() returns non-nil
	// supportedArtifacts is the backend's canonical artifact schema returned by
	// SupportedArtifacts. nil (the default) means "use the standard vocabulary"
	// (defaultSupportedArtifacts); SetSupportedArtifacts overrides it so tests
	// can exercise cli.App's startup rejection of an unrecordable artifact.
	supportedArtifacts []flow.ArtifactDef
	// worktrees is one checkout per item; see Worktree.
	worktrees       map[string]*fakeWorktree
	nothingToCommit bool
}

// defaultSupportedArtifacts is the schema an unconfigured fake reports — the
// standard flow vocabulary, enough for SDK tests that build an App without
// caring about the artifact schema. Override per-test with
// Backend.SetSupportedArtifacts.
var defaultSupportedArtifacts = []flow.ArtifactDef{
	flow.Artifact("plan", flow.ArtifactMarkdown).WithDoc("Implementation plan."),
	flow.Artifact("implementation", flow.ArtifactPatch).WithDoc("The code change as a diff."),
	flow.Artifact("review", flow.ArtifactMarkdown).WithDoc("Code review findings."),
	flow.Artifact("coverage", flow.ArtifactMarkdown).WithDoc("Test-coverage analysis."),
	flow.Artifact("commit", flow.ArtifactCommitHash).WithDoc("Local commit hash."),
	flow.Artifact("push", flow.ArtifactCommitHash).WithDoc("Pushed commit hash."),
	flow.Artifact("summary", flow.ArtifactMarkdown).WithDoc("Resolution summary."),
	flow.Artifact("inspection", flow.ArtifactJSON).WithDoc("Inspection verdict."),
	flow.Artifact("phases", flow.ArtifactJSON).WithDoc("Child task ids filed from a plan."),
}

type itemRecord struct {
	item        flow.Item
	claim       *flow.Claim
	claimedAt   time.Time
	owner       string
	artifacts   map[flow.ArtifactId]*flow.ArtifactRecord
	signals     map[flow.SignalId]flow.SignalState
	questions   []flow.Question
	nextQID     int
	seeded      bool
	parkRequest *flow.ParkRequest
	// work is this item's work-in-progress records, keyed by step result id.
	// Per-item rather than backend-wide, which is the keying the contract turns
	// on: one item's reasoning must never be readable as another's.
	work map[string]string
}

// New constructs an empty fake backend. Signals lists the SignalIds this
// backend will report as supported.
func New(signals ...flow.SignalDef) *Backend {
	return &Backend{
		items:           map[string]*itemRecord{},
		signals:         signals,
		clock:           time.Now,
		verifyOK:        true,
		gateOutcome:     flow.OutcomeMeasured,
		supportsRequest: true,
	}
}

// SetClock overrides the backend's time source. For deterministic tests.
func (b *Backend) SetClock(c func() time.Time) { b.clock = c }

// SetVerifyOK controls what Worktree.Verify returns. Default true.
func (b *Backend) SetVerifyOK(ok bool) { b.verifyOK = ok }

// SetGateOutcome controls what the runner observes of every gate. Default
// flow.OutcomeMeasured.
//
// Separate from SetVerifyOK because verify and gates are different things: a
// tree can be repairable by verify and still fail the gate that decides. An
// outcome rather than a boolean because the set is what the fake exists to
// model — a caller that must tell "could not start" from "died" cannot be
// exercised against a fake that only knows pass and fail.
func (b *Backend) SetGateOutcome(o flow.GateOutcome) { b.gateOutcome = o }

// SetGateVerdict controls what Worktree.Judge answers about a measured run.
// Default: acceptable.
//
// A boolean rather than an outcome, because a judging layer IS entitled to a
// binary answer — that is its whole job, and the reason a gate has no verdict
// to give does not apply to it. A refusal is an ANSWER: it arrives with a nil
// error, and a fake that returned one as an error would let a caller be
// written that cannot tell a project saying no from a project whose judge is
// broken.
func (b *Backend) SetGateVerdict(acceptable bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.verdict = &acceptable
	for _, wt := range b.worktrees {
		wt.verdict = b.verdict
	}
}

// SetSupportsRequest controls whether Worktree.Request() returns a non-nil
// RequestManager. Default true; set to false to exercise the "backend
// doesn't support pull requests" path.
func (b *Backend) SetSupportsRequest(ok bool) { b.supportsRequest = ok }

// AddItem registers an item with the backend so ListEligible / Claim see it.
func (b *Backend) AddItem(item flow.Item) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items[item.ID] = &itemRecord{
		item:      item,
		artifacts: map[flow.ArtifactId]*flow.ArtifactRecord{},
		signals:   map[flow.SignalId]flow.SignalState{},
	}
}

// SetSignal flips a signal on an item (simulates an external observation).
func (b *Backend) SetSignal(itemID string, sig flow.SignalId, set bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return
	}
	rec.signals[sig] = flow.SignalState{
		Set:        set,
		ObservedAt: b.clock(),
		By:         "fake",
	}
}

// ParkRequest returns the last park recorded for an item, or nil.
func (b *Backend) ParkRequest(itemID string) *flow.ParkRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return nil
	}
	return rec.parkRequest
}

func (b *Backend) Name() string { return "fake" }

func (b *Backend) SupportedSignals() []flow.SignalDef { return b.signals }

// SetSupportedArtifacts overrides the backend's canonical artifact schema with
// exactly the given defs. With no call, the fake reports the standard
// vocabulary (defaultSupportedArtifacts). Use it to exercise cli.App's startup
// refusal of an artifact the backend cannot record.
func (b *Backend) SetSupportedArtifacts(defs ...flow.ArtifactDef) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.supportedArtifacts = defs
}

// SupportedArtifacts returns the backend's canonical artifact schema — the
// configured set, or the standard vocabulary when unconfigured.
func (b *Backend) SupportedArtifacts() []flow.ArtifactDef {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.supportedArtifacts == nil {
		return defaultSupportedArtifacts
	}
	return b.supportedArtifacts
}

func (b *Backend) ListEligible(ctx context.Context) ([]flow.ItemRef, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]flow.ItemRef, 0, len(b.items))
	for id, rec := range b.items {
		out = append(out, flow.ItemRef{
			BackendName: b.Name(),
			Display:     rec.item.Title,
			Ref:         json.RawMessage(fmt.Sprintf("%q", id)),
		})
	}
	return out, nil
}

func (b *Backend) Claim(ctx context.Context, ref flow.ItemRef, owner string, force bool) (flow.Claim, error) {
	id, err := refID(ref)
	if err != nil {
		return flow.Claim{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[id]
	if rec == nil {
		return flow.Claim{}, fmt.Errorf("fake: item %q not registered", id)
	}
	if rec.claim != nil && rec.owner != owner {
		return flow.Claim{}, fmt.Errorf("fake: item %q already claimed by %q", id, rec.owner)
	}
	now := b.clock()
	c := flow.Claim{
		BackendName: b.Name(),
		ItemRef:     ref,
		Owner:       owner,
		ClaimedAt:   now,
		Token:       json.RawMessage(`{}`),
	}
	rec.claim = &c
	rec.claimedAt = now
	rec.owner = owner
	return c, nil
}

func (b *Backend) Release(ctx context.Context, claim flow.Claim) error {
	id, err := refID(claim.ItemRef)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[id]
	if rec == nil {
		return fmt.Errorf("fake: item %q not registered", id)
	}
	rec.claim = nil
	rec.owner = ""
	// Releasing ends that reasoning's life: work in progress kept past the
	// claim it belonged to is scratch prose with nothing left to resume.
	rec.work = nil
	return nil
}

// ---------------------------------------------------------------------------
// Work in progress (flow.WorkInProgress). In memory, keyed by item and step —
// which is the property the contract turns on, so the fake models it rather
// than a single flat map.
// ---------------------------------------------------------------------------

// SaveWorkInProgress stores what a step worked out against that step's result
// id on this item.
func (b *Backend) SaveWorkInProgress(ctx context.Context, claim flow.Claim, step, body string) error {
	itemID, err := refID(claim.ItemRef)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return fmt.Errorf("fake: item %q not registered", itemID)
	}
	if rec.work == nil {
		rec.work = map[string]string{}
	}
	rec.work[step] = body
	return nil
}

// LoadWorkInProgress returns what this step stashed against this item, or ""
// when there is none.
func (b *Backend) LoadWorkInProgress(ctx context.Context, claim flow.Claim, step string) (string, error) {
	itemID, err := refID(claim.ItemRef)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return "", fmt.Errorf("fake: item %q not registered", itemID)
	}
	return rec.work[step], nil
}

// ClearWorkInProgress drops this step's record. Idempotent.
func (b *Backend) ClearWorkInProgress(ctx context.Context, claim flow.Claim, step string) error {
	itemID, err := refID(claim.ItemRef)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return fmt.Errorf("fake: item %q not registered", itemID)
	}
	delete(rec.work, step)
	return nil
}

// LookupActiveClaim returns the (single) active claim held by owner. The
// fake backend tracks claims in memory keyed by item id, so this scans the
// item map for one owned by `owner`.
func (b *Backend) LookupActiveClaim(ctx context.Context, owner string) (*flow.Claim, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, rec := range b.items {
		if rec.claim != nil && rec.owner == owner {
			cp := *rec.claim
			return &cp, nil
		}
	}
	return nil, nil
}

func (b *Backend) LookupClaim(ctx context.Context, ref flow.ItemRef) (*flow.ClaimInfo, error) {
	id, err := refID(ref)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[id]
	if rec == nil || rec.claim == nil {
		return nil, nil
	}
	return &flow.ClaimInfo{Owner: rec.owner, ClaimedAt: rec.claimedAt}, nil
}

func (b *Backend) LoadState(ctx context.Context, claim flow.Claim) (*flow.ItemState, error) {
	id, err := refID(claim.ItemRef)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[id]
	if rec == nil {
		return nil, fmt.Errorf("fake: item %q not registered", id)
	}
	st := &flow.ItemState{
		Item:      rec.item,
		Artifacts: make(map[flow.ArtifactId]flow.ArtifactRecord, len(rec.artifacts)),
		Signals:   make(map[flow.SignalId]flow.SignalState, len(rec.signals)),
	}
	for k, v := range rec.artifacts {
		st.Artifacts[k] = *v
	}
	maps.Copy(st.Signals, rec.signals)
	st.Questions = append([]flow.Question(nil), rec.questions...)
	if rec.parkRequest != nil {
		cp := *rec.parkRequest
		st.Park = &cp
	}
	return st, nil
}

func (b *Backend) SeedState(ctx context.Context, claim flow.Claim, specs []flow.ArtifactSpec) error {
	id, err := refID(claim.ItemRef)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[id]
	if rec == nil {
		return fmt.Errorf("fake: item %q not registered", id)
	}
	if rec.seeded {
		return errors.New("fake: item already seeded; second SeedState refused")
	}
	for _, sp := range specs {
		rec.artifacts[sp.Id] = &flow.ArtifactRecord{
			Id:                          sp.Id,
			Type:                        sp.Type,
			Required:                    sp.Required,
			GrantedInvocations:          sp.Budget.MaxInvocations,
			GrantedPromptsPerInvocation: sp.Budget.MaxPromptsPerInvocation,
			GrantedCostUSD:              sp.Budget.MaxCostUSD,
			GrantedTimeout:              sp.Budget.Timeout,
		}
	}
	rec.seeded = true
	return nil
}

func (b *Backend) ResetSeed(ctx context.Context, claim flow.Claim) error {
	itemID, err := refID(claim.ItemRef)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return fmt.Errorf("fake: item %q not registered", itemID)
	}
	rec.seeded = false
	rec.artifacts = map[flow.ArtifactId]*flow.ArtifactRecord{}
	rec.parkRequest = nil
	return nil
}

func (b *Backend) ResolveArtifact(ctx context.Context, claim flow.Claim, id flow.ArtifactId, body flow.ArtifactBody) error {
	itemID, err := refID(claim.ItemRef)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return fmt.Errorf("fake: item %q not registered", itemID)
	}
	art := rec.artifacts[id]
	if art == nil {
		return fmt.Errorf("fake: artifact %q not seeded on item %q", id, itemID)
	}
	if body.Type != art.Type {
		return flow.ErrTypeMismatch{Step: string(id), Expected: art.Type, Got: body.Type}
	}
	art.Resolved = true
	art.Stale = false
	art.CommitHash = body.CommitHash
	art.Markdown = body.Markdown
	art.JSON = body.JSON
	art.File = body.File
	art.Patch = body.Patch
	art.ProducedAt = b.clock()
	art.Version++
	art.ResolvedBy = claim.Owner
	art.PromptsThisInvocation = 0 // resets at successful resolve
	// A park recorded against this step is obsolete the moment the step
	// resolves — keeping it would make LoadState report a reason that no
	// longer holds.
	if rec.parkRequest != nil && rec.parkRequest.Step == string(id) {
		rec.parkRequest = nil
	}
	return nil
}

func (b *Backend) MarkStale(ctx context.Context, claim flow.Claim, id flow.ArtifactId) error {
	itemID, err := refID(claim.ItemRef)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return fmt.Errorf("fake: item %q not registered", itemID)
	}
	art := rec.artifacts[id]
	if art == nil {
		return fmt.Errorf("fake: artifact %q not seeded on item %q", id, itemID)
	}
	art.Stale = true
	return nil
}

func (b *Backend) BumpInvocations(ctx context.Context, claim flow.Claim, key string) error {
	return b.bumpField(claim, key, func(a *flow.ArtifactRecord) {
		a.Invocations++
		a.PromptsThisInvocation = 0
		a.LastRunAt = b.clock()
	})
}

func (b *Backend) BumpPrompts(ctx context.Context, claim flow.Claim, key string) error {
	return b.bumpField(claim, key, func(a *flow.ArtifactRecord) { a.PromptsThisInvocation++ })
}

func (b *Backend) AddCost(ctx context.Context, claim flow.Claim, key string, usd float64) error {
	return b.bumpField(claim, key, func(a *flow.ArtifactRecord) { a.CostUSDSpent += usd })
}

// Grant adds budget to the artifact record and clears a ParkBudgetExhausted
// park that the grant actually satisfies (see the Backend.Grant contract).
func (b *Backend) Grant(ctx context.Context, claim flow.Claim, key string, g flow.Grant) error {
	itemID, err := refID(claim.ItemRef)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return fmt.Errorf("fake: item %q not registered", itemID)
	}
	art := rec.artifacts[flow.ArtifactId(key)]
	if art == nil {
		return fmt.Errorf("fake: artifact %q not seeded on item %q", key, itemID)
	}
	art.GrantedInvocations += g.Invocations
	art.GrantedPromptsPerInvocation += g.PromptsPerInvocation
	art.GrantedCostUSD += g.CostUSD
	art.GrantedTimeout += time.Duration(g.TimeoutAdd) * time.Second
	if flow.GrantClearsPark(rec.parkRequest, key, *art, g) {
		rec.parkRequest = nil
	}
	return nil
}

func (b *Backend) bumpField(claim flow.Claim, key string, mutate func(*flow.ArtifactRecord)) error {
	itemID, err := refID(claim.ItemRef)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return fmt.Errorf("fake: item %q not registered", itemID)
	}
	art := rec.artifacts[flow.ArtifactId(key)]
	if art == nil {
		return fmt.Errorf("fake: artifact %q not seeded on item %q", key, itemID)
	}
	mutate(art)
	return nil
}

// AskQuestions appends the given questions to the item with a backend-
// assigned id. Returns the persisted records.
func (b *Backend) AskQuestions(ctx context.Context, claim flow.Claim, qs []flow.AgentQuestion) ([]flow.Question, error) {
	itemID, err := refID(claim.ItemRef)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return nil, fmt.Errorf("fake: item %q not registered", itemID)
	}
	out := make([]flow.Question, 0, len(qs))
	for _, aq := range qs {
		rec.nextQID++
		q := flow.Question{
			ID:            fmt.Sprintf("q%d", rec.nextQID),
			AgentQuestion: aq,
		}
		rec.questions = append(rec.questions, q)
		out = append(out, q)
	}
	return out, nil
}

// AnswerQuestion is a test helper — fills in UserAnswer.Answer on a recorded
// question.
func (b *Backend) AnswerQuestion(itemID, qID, answer string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return fmt.Errorf("fake: item %q not registered", itemID)
	}
	now := b.clock()
	for i := range rec.questions {
		if rec.questions[i].ID == qID {
			rec.questions[i].UserAnswer = flow.UserAnswer{Answer: answer, AnsweredAt: &now}
			return nil
		}
	}
	return fmt.Errorf("fake: question %q not found on item %q", qID, itemID)
}

func (b *Backend) Park(ctx context.Context, claim flow.Claim, req flow.ParkRequest) error {
	itemID, err := refID(claim.ItemRef)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return fmt.Errorf("fake: item %q not registered", itemID)
	}
	cp := req
	rec.parkRequest = &cp
	return nil
}

func (b *Backend) Worktree(ctx context.Context, claim flow.Claim) (flow.Worktree, error) {
	id, err := refID(claim.ItemRef)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// One worktree PER ITEM, kept across calls. A real checkout keeps its
	// branch and its commits between invocations, which callers comparing a
	// branch against its base depend on — and it does NOT share them with
	// another item, which a single backend-wide worktree would.
	if b.worktrees == nil {
		b.worktrees = map[string]*fakeWorktree{}
	}
	wt := b.worktrees[id]
	if wt == nil {
		wt = &fakeWorktree{branches: map[string]bool{}}
		b.worktrees[id] = wt
	}
	wt.verifyOK = b.verifyOK
	wt.gateOutcome = b.gateOutcome
	wt.verdict = b.verdict
	wt.supportsRequest = b.supportsRequest
	wt.nothingToCommit = b.nothingToCommit
	return wt, nil
}

// SetNothingToCommit makes Worktree.Commit a no-op, as git is when nothing is
// staged. Use it to exercise a caller's "did anything actually land?" guard.
func (b *Backend) SetNothingToCommit(v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nothingToCommit = v
	for _, wt := range b.worktrees {
		wt.nothingToCommit = v
	}
}

func refID(ref flow.ItemRef) (string, error) {
	var id string
	if err := json.Unmarshal(ref.Ref, &id); err != nil {
		return "", fmt.Errorf("fake: malformed ItemRef.Ref: %w", err)
	}
	return id, nil
}

type fakeWorktree struct {
	// commits counts landed commits so RevParse("HEAD") advances, letting a
	// caller distinguish a commit that recorded work from one that was a no-op.
	commits int
	// nothingToCommit makes Commit the no-op git performs with nothing staged.
	nothingToCommit bool
	// branches records which branches exist, so Branch can report `created`
	// truthfully across invocations.
	branches        map[string]bool
	gateOutcome     flow.GateOutcome
	verdict         *bool
	verifyOK        bool
	supportsRequest bool
	branch          string
}

func (w *fakeWorktree) Branch(ctx context.Context, name, base string) (bool, error) {
	w.branch = name
	// created means the branch did not EXIST — not merely that we were on a
	// different one. Callers use it to tell "there is no work here" from
	// "switching back to work that is already here", and conflating the two
	// makes every re-checkout look like a fresh start.
	if w.branches == nil {
		w.branches = map[string]bool{}
	}
	if w.branches[name] {
		return false, nil
	}
	w.branches[name] = true
	return true, nil
}

func (w *fakeWorktree) CurrentBranch(ctx context.Context) (string, error) {
	if w.branch == "" {
		return "main", nil
	}
	return w.branch, nil
}

// Commit is a no-op when SetNothingToCommit(true) — the real git behavior when
// nothing is staged, and the case a caller's "did anything land?" guard exists
// for.
func (w *fakeWorktree) Stage(ctx context.Context) error { return nil }

func (w *fakeWorktree) Commit(ctx context.Context, msg string) error {
	if w.nothingToCommit {
		return nil
	}
	w.commits++
	return nil
}
func (w *fakeWorktree) Push(ctx context.Context) error { return nil }

// RevParse models a branch that advances only when a commit lands, so a caller
// can tell recorded work from a no-op commit — the distinction the real
// interface exists for. Any rev other than HEAD resolves to the base.
func (w *fakeWorktree) RevParse(ctx context.Context, rev string) (string, error) {
	if rev != "HEAD" {
		return "sha-0", nil
	}
	return fmt.Sprintf("sha-%d", w.commits), nil
}
func (w *fakeWorktree) Verify(ctx context.Context) error {
	if !w.verifyOK {
		return errors.New("fake: verify failed")
	}
	return nil
}

// RunGate answers for every gate name. The fake models the protocol, not a
// project's gate set, so it does not pretend to know which names exist — and
// it does not pretend to know a project's numbers either: on OutcomeMeasured
// the envelope it reports is one that parses and says nothing else.
//
// The error is for a request no runner could attempt. Every way a gate fails
// is an outcome.
func (w *fakeWorktree) RunGate(ctx context.Context, name flow.GateName) (flow.GateRun, error) {
	if !name.Valid() {
		return flow.GateRun{}, fmt.Errorf("fake: %q is not a declared gate name", name)
	}
	outcome := w.gateOutcome
	if outcome == "" {
		outcome = flow.OutcomeMeasured
	}
	run := flow.GateRun{Gate: name, Outcome: outcome, ExitCode: -1}
	switch outcome {
	case flow.OutcomeMeasured:
		run.ExitCode = 0
		run.Stdout = fmt.Appendf(nil, "{%q:%q}\n", "gate", name)
	case flow.OutcomeBrokeContract:
		run.ExitCode = 0
		run.Stdout = []byte("fake: not an envelope\n")
	}
	run.Detail = fmt.Sprintf("fake: gate %q %s", name, outcome)
	return run, nil
}

// Judge answers about a measured run and refuses everything else. The fake
// models the protocol, not a project's thresholds — so the terms it reports
// are ones that parse and say nothing, the same posture as its envelope.
//
// The error is for a request no judge could answer: a name no gate has, and a
// run that measured nothing. A refusal is NOT one of them — it is a verdict,
// and it comes back with a nil error.
func (w *fakeWorktree) Judge(ctx context.Context, run flow.GateRun) (flow.GateVerdict, error) {
	if !run.Gate.Valid() {
		return flow.GateVerdict{}, fmt.Errorf("fake: %q is not a declared gate name", run.Gate)
	}
	if run.Outcome != flow.OutcomeMeasured {
		return flow.GateVerdict{}, fmt.Errorf(
			"fake: the run of gate %q reports %q, and only a %q run may be judged",
			run.Gate, run.Outcome, flow.OutcomeMeasured)
	}
	acceptable := w.verdict == nil || *w.verdict
	return flow.GateVerdict{
		Run:        run,
		Acceptable: acceptable,
		Thresholds: []byte("{}"),
		Detail:     fmt.Sprintf("fake: gate %q judged acceptable=%t against no terms at all", run.Gate, acceptable),
	}, nil
}

func (w *fakeWorktree) CapturePatch(ctx context.Context) ([]byte, error) { return nil, nil }

// Request returns the worktree itself — fake exposes a request manager so
// tests can exercise Open/Merge paths. Tests that want to exercise the
// "backend doesn't support pull requests" path call SetSupportsRequest(false).
func (w *fakeWorktree) Request() flow.RequestManager {
	if !w.supportsRequest {
		return nil
	}
	return w
}

func (w *fakeWorktree) Open(ctx context.Context, base, title, body string) (string, error) {
	return "https://example.invalid/pr/1", nil
}
func (w *fakeWorktree) Merge(ctx context.Context, url string) error { return nil }
