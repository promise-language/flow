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
	verifyOK        bool // controls Worktree.Validate result
	supportsRequest bool // controls whether Worktree.Request() returns non-nil
	// supportedArtifacts is the backend's canonical artifact schema returned by
	// SupportedArtifacts. nil (the default) means "use the standard vocabulary"
	// (defaultSupportedArtifacts); SetSupportedArtifacts overrides it so tests
	// can exercise cli.App's startup rejection of an unrecordable artifact.
	supportedArtifacts []flow.ArtifactDef
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
}

// New constructs an empty fake backend. Signals lists the SignalIds this
// backend will report as supported.
func New(signals ...flow.SignalDef) *Backend {
	return &Backend{
		items:           map[string]*itemRecord{},
		signals:         signals,
		clock:           time.Now,
		verifyOK:        true,
		supportsRequest: true,
	}
}

// SetClock overrides the backend's time source. For deterministic tests.
func (b *Backend) SetClock(c func() time.Time) { b.clock = c }

// SetVerifyOK controls what Worktree.Validate returns. Default true.
func (b *Backend) SetVerifyOK(ok bool) { b.verifyOK = ok }

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
	return &fakeWorktree{verifyOK: b.verifyOK, supportsRequest: b.supportsRequest}, nil
}

func refID(ref flow.ItemRef) (string, error) {
	var id string
	if err := json.Unmarshal(ref.Ref, &id); err != nil {
		return "", fmt.Errorf("fake: malformed ItemRef.Ref: %w", err)
	}
	return id, nil
}

type fakeWorktree struct {
	verifyOK        bool
	supportsRequest bool
	branch          string
}

func (w *fakeWorktree) Branch(ctx context.Context, name, base string) (bool, error) {
	created := w.branch != name
	w.branch = name
	return created, nil
}

func (w *fakeWorktree) CurrentBranch(ctx context.Context) (string, error) {
	if w.branch == "" {
		return "main", nil
	}
	return w.branch, nil
}

func (w *fakeWorktree) Commit(ctx context.Context, msg string) error { return nil }
func (w *fakeWorktree) Push(ctx context.Context) error               { return nil }
func (w *fakeWorktree) Validate(ctx context.Context) error {
	if !w.verifyOK {
		return errors.New("fake: validate failed")
	}
	return nil
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
