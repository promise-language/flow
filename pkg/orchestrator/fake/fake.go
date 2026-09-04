// Package fake is an in-memory flow.Orchestrator implementation used by SDK
// tests and by flow authors who want to exercise their flow logic without
// touching GitHub. NOT suitable for production.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/promise-language/flow"
)

// Orchestrator is the in-memory orchestrator.
//
// It is one arena — the process it runs in — so the account and the arena are
// ambient here exactly as the contract requires: Claim takes neither, and
// LookupActiveClaim takes no key at all.
type Orchestrator struct {
	mu      sync.Mutex
	items   map[string]*itemRecord // keyed by the fake's own item id
	signals []flow.SignalDef
	clock   func() time.Time

	// arena and account are what this orchestrator's one arena acts as.
	arena   flow.Arena
	account flow.AccountId

	// active is the claim this arena holds, or nil. One arena holds at most
	// one, which is why LookupActiveClaim needs no key.
	active *flow.Claim

	verifyOK        bool         // controls the exit code of the verify command
	commandOutcome  flow.Outcome // controls what Worktree.Run observes
	gateOutcome     flow.Outcome // controls what Worktree.RunGate observes
	verdict         *bool        // controls what Worktree.Judge answers; nil = acceptable
	supportsRequest bool         // controls whether Worktree.Request() returns non-nil

	// supportedArtifacts is the orchestrator's canonical artifact schema
	// returned by SupportedArtifacts. nil (the default) means "use the standard
	// vocabulary" (defaultSupportedArtifacts); SetSupportedArtifacts overrides
	// it so tests can exercise cli.App's startup rejection of an unrecordable
	// artifact.
	supportedArtifacts []flow.ArtifactDef
	// supportedGates / supportedCommands work the same way.
	supportedGates    []flow.GateDef
	supportedCommands []flow.CommandDef

	// worktrees is one checkout per item; see Worktree.
	worktrees       map[string]*fakeWorktree
	nothingToCommit bool
	branchHeads     map[string]string
	initialBranch   flow.BranchName
}

// defaultSupportedArtifacts is the schema an unconfigured fake reports — the
// standard flow vocabulary, enough for SDK tests that build an App without
// caring about the artifact schema. Override per-test with
// Orchestrator.SetSupportedArtifacts.
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

// defaultSupportedGates is what an unconfigured fake declares. `integration`
// and `fit` are required of every orchestrator; the rest are the ordinary
// project concerns a flow may compose.
var defaultSupportedGates = []flow.GateDef{
	flow.Gate(flow.GateIntegration, true),
	flow.Gate(flow.GateFit, true),
	flow.Gate(flow.GateFormatted, false),
	flow.Gate(flow.GateBuilds, false),
	flow.Gate(flow.GateChecked, false),
	flow.Gate(flow.GateTested, false),
	flow.Gate(flow.GateCovered, false),
}

// defaultSupportedCommands declares all three. `verify` is required; the fake
// answers for setup and cleanup too, since modelling the protocol is what it is
// for.
var defaultSupportedCommands = []flow.CommandDef{
	flow.Command(flow.CommandVerify),
	flow.Command(flow.CommandSetup),
	flow.Command(flow.CommandCleanup),
}

type itemRecord struct {
	id   string
	item flow.Item // the item's own fields; the flow's record lives beside it

	claim     *flow.Claim
	claimedAt time.Time
	holder    flow.Holder

	artifacts   map[flow.ArtifactId]*flow.ArtifactRecord
	signals     map[flow.SignalId]flow.SignalState
	questions   []flow.Question
	nextQID     int
	seeded      bool
	parkRequest *flow.ParkRequest

	// blockedBy holds the fake ids of declared blockers. Blockedness is DERIVED
	// from their statuses on every read, never stored — a stored bit beside a
	// stored edge reads as well-formed, selection honours it, and nothing ever
	// lifts it.
	blockedBy []string

	// work is this item's work-in-progress records, keyed by step. Per-item
	// rather than orchestrator-wide, which is the keying the contract turns on:
	// one item's reasoning must never be readable as another's.
	work map[flow.StepId]string
}

// New constructs an empty fake orchestrator. Signals lists the SignalIds this
// orchestrator will report as supported.
func New(signals ...flow.SignalDef) *Orchestrator {
	return &Orchestrator{
		items:           map[string]*itemRecord{},
		signals:         signals,
		clock:           time.Now,
		verifyOK:        true,
		commandOutcome:  flow.OutcomeMeasured,
		gateOutcome:     flow.OutcomeMeasured,
		supportsRequest: true,
		arena:           flow.Arena{Host: "fakehost", Id: "/fake/arena"},
		account:         "fake-account",
	}
}

// SetClock overrides the orchestrator's time source. For deterministic tests.
func (b *Orchestrator) SetClock(c func() time.Time) { b.clock = c }

// SetAccount overrides the account this arena acts as. The account is AMBIENT —
// no caller passes one — so a test that needs a particular one sets it here.
func (b *Orchestrator) SetAccount(a flow.AccountId) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.account = a
}

// Account reports the account this arena acts as.
func (b *Orchestrator) Account() flow.AccountId {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.account
}

// SetArena overrides the arena this orchestrator is. Both halves are the
// identity, so tests that model two arenas give them different pairs.
func (b *Orchestrator) SetArena(a flow.Arena) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.arena = a
}

// SetVerifyOK controls whether the `verify` command exits 0. Default true.
//
// It is an EXIT CODE, not a success flag: a failing verify still measured, and
// a caller that cannot tell "ran and reported failures" from "could not start"
// spends an agent turn on a missing binary. Use SetCommandOutcome for the
// outcomes that are not measurements.
func (b *Orchestrator) SetVerifyOK(ok bool) { b.verifyOK = ok }

// SetCommandOutcome controls what the runner observes of every command.
// Default flow.OutcomeMeasured.
func (b *Orchestrator) SetCommandOutcome(o flow.Outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.commandOutcome = o
	for _, wt := range b.worktrees {
		wt.commandOutcome = o
	}
}

// SetGateOutcome controls what the runner observes of every gate. Default
// flow.OutcomeMeasured.
//
// Separate from SetVerifyOK because verify and gates are different things: a
// tree can be repairable by verify and still fail the gate that decides. An
// outcome rather than a boolean because the set is what the fake exists to
// model — a caller that must tell "could not start" from "died" cannot be
// exercised against a fake that only knows pass and fail.
func (b *Orchestrator) SetGateOutcome(o flow.Outcome) { b.gateOutcome = o }

// SetGateVerdict controls what Worktree.Judge answers about a measured run.
// Default: acceptable.
//
// A boolean rather than an outcome, because a judging layer IS entitled to a
// binary answer — that is its whole job, and the reason a gate has no verdict
// to give does not apply to it. A refusal is an ANSWER: it arrives with a nil
// error, and a fake that returned one as an error would let a caller be
// written that cannot tell a project saying no from a project whose judge is
// broken.
func (b *Orchestrator) SetGateVerdict(acceptable bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.verdict = &acceptable
	for _, wt := range b.worktrees {
		wt.verdict = b.verdict
	}
}

// SetSupportsRequest controls whether Worktree.Request() returns a non-nil
// RequestManager. Default true; set to false to exercise the "this
// orchestrator does not land through pull requests" path.
func (b *Orchestrator) SetSupportsRequest(ok bool) { b.supportsRequest = ok }

// AddItem registers an item under the given id so List / Claim see it. The id
// is the fake's own store key — a projection of the ItemRef it mints, which is
// why the item itself carries no such field.
func (b *Orchestrator) AddItem(id string, item flow.Item) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if item.Status == "" {
		item.Status = flow.StatusOpen
	}
	item.Ref = b.refFor(id)
	b.items[id] = &itemRecord{
		id:        id,
		item:      item,
		artifacts: map[flow.ArtifactId]*flow.ArtifactRecord{},
		signals:   map[flow.SignalId]flow.SignalState{},
	}
}

// Ref returns the ItemRef the fake mints for an id. Tests addressing an item
// directly use it rather than assembling a ref by hand — reconstructing one
// from a projection is exactly what ResolveRef exists to prevent.
func (b *Orchestrator) Ref(id string) flow.ItemRef { return b.refFor(id) }

func (b *Orchestrator) refFor(id string) flow.ItemRef {
	return flow.ItemRef{
		OrchestratorName: b.Name(),
		Display:          id,
		Ref:              json.RawMessage(fmt.Sprintf("%q", id)),
	}
}

// SetStatus moves an item's lifecycle position. Finalize refuses anything that
// is not terminal, so a test driving a run to completion closes the item first
// — which is what the orchestrator's own means would have done.
func (b *Orchestrator) SetStatus(id string, status flow.ItemStatus, disposition string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if rec := b.items[id]; rec != nil {
		rec.item.Status = status
		rec.item.Disposition = disposition
	}
}

// SetTags replaces an item's tags. A test helper, not part of the contract
// surface: the contract's way to change tags is ItemEditor.
func (b *Orchestrator) SetTags(id string, tags ...flow.TagId) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if rec := b.items[id]; rec != nil {
		rec.item.Tags = tags
	}
}

// SetSignal flips a signal on an item (simulates an external observation).
func (b *Orchestrator) SetSignal(itemID string, sig flow.SignalId, set bool) {
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
func (b *Orchestrator) ParkRequest(itemID string) *flow.ParkRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return nil
	}
	return rec.parkRequest
}

// ---------------------------------------------------------------------------
// Declaration
// ---------------------------------------------------------------------------

func (b *Orchestrator) Name() flow.OrchestratorName { return "fake" }

func (b *Orchestrator) SupportedSignals() []flow.SignalDef { return b.signals }

// SetSupportedArtifacts overrides the canonical artifact schema with exactly
// the given defs. With no call, the fake reports the standard vocabulary. Use
// it to exercise cli.App's startup refusal of an artifact it cannot record.
func (b *Orchestrator) SetSupportedArtifacts(defs ...flow.ArtifactDef) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.supportedArtifacts = defs
}

// SupportedArtifacts returns the canonical artifact schema — the configured
// set, or the standard vocabulary when unconfigured.
func (b *Orchestrator) SupportedArtifacts() []flow.ArtifactDef {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.supportedArtifacts == nil {
		return defaultSupportedArtifacts
	}
	return b.supportedArtifacts
}

// SetSupportedGates overrides the declared gate set. Use it to exercise
// cli.App's startup refusal of a flow naming a gate nothing can run, and of an
// orchestrator that fails to declare `integration` or `fit`.
func (b *Orchestrator) SetSupportedGates(defs ...flow.GateDef) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.supportedGates = defs
	if defs == nil {
		// A nil slice means "unset" to the reader below, so an explicit empty
		// declaration is kept distinguishable from no call at all.
		b.supportedGates = []flow.GateDef{}
	}
}

func (b *Orchestrator) SupportedGates() []flow.GateDef {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.supportedGates == nil {
		return defaultSupportedGates
	}
	return b.supportedGates
}

// SetSupportedCommands overrides the declared command set.
func (b *Orchestrator) SetSupportedCommands(defs ...flow.CommandDef) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.supportedCommands = defs
	if defs == nil {
		b.supportedCommands = []flow.CommandDef{}
	}
}

func (b *Orchestrator) SupportedCommands() []flow.CommandDef {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.supportedCommands == nil {
		return defaultSupportedCommands
	}
	return b.supportedCommands
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

// ResolveRef turns a user-typed id into a ref. The fake's ids ARE its store
// keys, so this is direct — but it is still the only supported route in, and a
// display string is not accepted in its place.
func (b *Orchestrator) ResolveRef(ctx context.Context, input string) (flow.ItemRef, error) {
	if input == "" {
		return flow.ItemRef{}, errors.New("fake: empty item id")
	}
	return b.refFor(input), nil
}

// List returns the items at the given scope. Get and ListAutoSelectable are
// derived from the same per-item computation, so the three cannot disagree.
func (b *Orchestrator) List(ctx context.Context, scope flow.ItemScope, binary flow.BinaryName, acceptsType func(flow.ItemType) bool) ([]flow.ItemInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]flow.ItemInfo, 0, len(b.items))
	for _, rec := range b.items {
		info := b.itemInfoFor(rec, acceptsType)
		if !info.Availability.InScope(scope) {
			continue
		}
		out = append(out, info)
	}
	// Deterministic order: the map iteration is not, and a listing that
	// reordered between calls would make every diff of it noise.
	slices.SortFunc(out, func(x, y flow.ItemInfo) int {
		switch {
		case x.Ref.Display < y.Ref.Display:
			return -1
		case x.Ref.Display > y.Ref.Display:
			return 1
		}
		return 0
	})
	return out, nil
}

// Get answers about one item, through the same derivation List uses. That is
// what makes "must answer identically to List" structural rather than a
// promise: an item that read blocked in `list` and available in `status` is a
// contradiction an operator cannot resolve.
func (b *Orchestrator) Get(ctx context.Context, ref flow.ItemRef, binary flow.BinaryName, acceptsType func(flow.ItemType) bool) (*flow.ItemInfo, error) {
	id, err := refID(ref)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[id]
	if rec == nil {
		return nil, fmt.Errorf("fake: item %q not registered", id)
	}
	info := b.itemInfoFor(rec, acceptsType)
	return &info, nil
}

// ListAutoSelectable returns the refs an unattended resolve may start on. It
// runs the same derivation and drops anything blocked, so the rule has one
// owner.
func (b *Orchestrator) ListAutoSelectable(ctx context.Context, tags []flow.TagId) ([]flow.ItemRef, error) {
	for _, t := range tags {
		if !t.Valid() {
			return nil, fmt.Errorf("fake: %q is not a valid tag", string(t))
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]string, 0, len(b.items))
	for id := range b.items {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]flow.ItemRef, 0, len(ids))
	for _, id := range ids {
		rec := b.items[id]
		// acceptsType is not available here and availability is not the
		// question: selection asks whether the item is workable, and the
		// caller's own flow selection decides the rest.
		if rec.item.Status == flow.StatusTerminal {
			continue
		}
		if blocked, _, _ := b.blockednessOf(rec); blocked {
			continue
		}
		if !holderIsFree(rec.holder, b.arena) {
			continue
		}
		if !flow.TagsMatch(rec.item.Tags, tags) {
			continue
		}
		out = append(out, b.refFor(id))
	}
	return out, nil
}

// itemInfoFor is the ONE derivation behind List, Get and selection. Caller
// holds b.mu.
func (b *Orchestrator) itemInfoFor(rec *itemRecord, acceptsType func(flow.ItemType) bool) flow.ItemInfo {
	blocked, kind, reason := b.blockednessOf(rec)
	info := flow.ItemInfo{
		Ref:         b.refFor(rec.id),
		Type:        rec.item.Type,
		Title:       rec.item.Title,
		Body:        rec.item.Body,
		URL:         rec.item.URL,
		Status:      rec.item.Status,
		Disposition: rec.item.Disposition,
		Holder:      rec.holder,
		Tags:        slices.Clone(rec.item.Tags),
		BlockedBy:   b.blockersOf(rec),
		Blocked:     blocked,
		BlockKind:   kind,
		BlockReason: reason,
		Manual:      rec.item.Manual,
	}
	info.Availability = b.availabilityOf(rec, blocked, acceptsType)
	return info
}

func (b *Orchestrator) availabilityOf(rec *itemRecord, blocked bool, acceptsType func(flow.ItemType) bool) flow.Availability {
	if rec.item.Status == flow.StatusTerminal {
		return flow.AvailClosed
	}
	if acceptsType != nil && !acceptsType(rec.item.Type) {
		return flow.AvailUnhandled
	}
	if blocked {
		return flow.AvailBlocked
	}
	if !holderIsFree(rec.holder, b.arena) {
		return flow.AvailHeld
	}
	// The fake opts every workable item in: its whole selectable set is what it
	// holds, which is what its callers' tests are written against.
	return flow.AvailAuto
}

// holderIsFree reports whether this arena could take the item — either nobody
// holds it, or this arena already does. The comparison is on the ARENA, never
// the account: a claim is item ↔ arena.
func holderIsFree(h flow.Holder, self flow.Arena) bool {
	return h.Arena.Empty() || h.Arena == self
}

// blockersOf reports every DECLARED blocker with its own status. A blocker
// that has since finished stays listed until someone retracts it — a set that
// quietly dropped satisfied entries could not be edited.
func (b *Orchestrator) blockersOf(rec *itemRecord) []flow.Blocker {
	if len(rec.blockedBy) == 0 {
		return nil
	}
	out := make([]flow.Blocker, 0, len(rec.blockedBy))
	for _, id := range rec.blockedBy {
		status := flow.StatusOpen
		if other := b.items[id]; other != nil {
			status = other.item.Status
		}
		out = append(out, flow.Blocker{Ref: b.refFor(id), Status: status})
	}
	return out
}

// blockednessOf DERIVES whether the item is blocked. Nothing is stored: the
// item whose last blocker finishes is workable at the next read, with nobody
// having acted.
//
// The reason names the KIND of block and never an item — the refs are the
// reference, and prose repeating them is a second copy nothing can act on and
// nothing updates when a blocker lands.
func (b *Orchestrator) blockednessOf(rec *itemRecord) (bool, flow.BlockKind, string) {
	for _, id := range rec.blockedBy {
		other := b.items[id]
		if other == nil || other.item.Status != flow.StatusTerminal {
			return true, flow.WaitsOnItems, "waiting on unfinished dependencies"
		}
	}
	if rec.parkRequest != nil {
		switch rec.parkRequest.Kind {
		case flow.ParkQuestion:
			return true, flow.WaitsOnPerson, "waiting for an answer"
		case flow.ParkBudgetExhausted:
			return true, flow.WaitsOnPerson, "budget exhausted"
		}
	}
	return false, "", ""
}

// ---------------------------------------------------------------------------
// Claiming
// ---------------------------------------------------------------------------

// Claim takes the lease. Neither the arena nor the account is a parameter:
// both are ambient, fixed by where the call runs.
func (b *Orchestrator) Claim(ctx context.Context, ref flow.ItemRef, overrides []flow.ClaimOverride) (flow.Claim, error) {
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
	if rec.claim != nil && rec.holder.Arena != b.arena && !slices.Contains(overrides, flow.OverrideAlreadyHeld) {
		return flow.Claim{}, flow.ErrClaimRefused{
			Code: "already-claimed", ItemScoped: true,
			Reason: fmt.Sprintf("fake: item %q already claimed by arena %s/%s", id, rec.holder.Arena.Host, rec.holder.Arena.Id),
		}
	}
	now := b.clock()
	c := flow.Claim{
		OrchestratorName: b.Name(),
		ItemRef:          b.refFor(id),
		Arena:            b.arena,
		Account:          b.account,
		ClaimedAt:        now,
		Token:            json.RawMessage(`{}`),
	}
	rec.claim = &c
	rec.claimedAt = now
	rec.holder = flow.Holder{Arena: b.arena, Account: b.account}
	active := c
	b.active = &active
	return c, nil
}

func (b *Orchestrator) Release(ctx context.Context, ref flow.ItemRef) error {
	id, err := refID(ref)
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
	rec.holder = flow.Holder{}
	// Releasing ends that reasoning's life: work in progress kept past the
	// claim it belonged to is scratch prose with nothing left to resume.
	rec.work = nil
	if b.active != nil {
		if activeID, aerr := refID(b.active.ItemRef); aerr == nil && activeID == id {
			b.active = nil
		}
	}
	return nil
}

// LookupActiveClaim takes no key: the arena is ambient, and one arena holds at
// most one claim, so the question has exactly one answer.
func (b *Orchestrator) LookupActiveClaim(ctx context.Context) (*flow.Claim, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active == nil {
		return nil, nil
	}
	cp := *b.active
	return &cp, nil
}

func (b *Orchestrator) LookupClaim(ctx context.Context, ref flow.ItemRef) (*flow.ClaimInfo, error) {
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
	return &flow.ClaimInfo{
		Arena:     rec.holder.Arena,
		Account:   rec.holder.Account,
		ClaimedAt: rec.claimedAt,
	}, nil
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

// Load reads the item and everything the flow recorded on it. Addressed by
// ref, not by claim: reading an item is not a privileged act.
func (b *Orchestrator) Load(ctx context.Context, ref flow.ItemRef) (*flow.Item, error) {
	id, err := refID(ref)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[id]
	if rec == nil {
		return nil, fmt.Errorf("fake: item %q not registered", id)
	}
	return b.loadLocked(rec), nil
}

func (b *Orchestrator) loadLocked(rec *itemRecord) *flow.Item {
	it := rec.item
	it.Ref = b.refFor(rec.id)
	it.Holder = rec.holder
	it.Tags = slices.Clone(rec.item.Tags)
	it.BlockedBy = b.blockersOf(rec)
	_, _, it.BlockReason = b.blockednessOf(rec)
	it.Artifacts = make(map[flow.ArtifactId]flow.ArtifactRecord, len(rec.artifacts))
	for k, v := range rec.artifacts {
		it.Artifacts[k] = *v
	}
	it.Signals = make(map[flow.SignalId]flow.SignalState, len(rec.signals))
	maps.Copy(it.Signals, rec.signals)
	it.Questions = slices.Clone(rec.questions)
	if rec.parkRequest != nil {
		cp := *rec.parkRequest
		it.Park = &cp
	}
	return &it
}

func (b *Orchestrator) SeedState(ctx context.Context, ref flow.ItemRef, specs []flow.ArtifactSpec) error {
	id, err := refID(ref)
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

func (b *Orchestrator) ResetSeed(ctx context.Context, ref flow.ItemRef) error {
	id, err := refID(ref)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[id]
	if rec == nil {
		return fmt.Errorf("fake: item %q not registered", id)
	}
	rec.seeded = false
	rec.artifacts = map[flow.ArtifactId]*flow.ArtifactRecord{}
	rec.parkRequest = nil
	return nil
}

// ---------------------------------------------------------------------------
// Editing
// ---------------------------------------------------------------------------

// Edit opens a staged edit. Nothing changes until Commit.
func (b *Orchestrator) Edit(ctx context.Context, ref flow.ItemRef) (flow.ItemEditor, error) {
	id, err := refID(ref)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.items[id] == nil {
		return nil, fmt.Errorf("fake: item %q not registered", id)
	}
	return &editor{b: b, id: id}, nil
}

type editor struct {
	b  *Orchestrator
	id string

	title, body      *string
	addTags, delTags []flow.TagId
	addBlk, delBlk   []string
	manual           *bool
}

func (e *editor) SetTitle(t string)      { e.title = &t }
func (e *editor) SetBody(b string)       { e.body = &b }
func (e *editor) SetManual(m bool)       { e.manual = &m }
func (e *editor) AddTag(t flow.TagId)    { e.addTags = append(e.addTags, t) }
func (e *editor) RemoveTag(t flow.TagId) { e.delTags = append(e.delTags, t) }

func (e *editor) AddBlocker(ref flow.ItemRef) {
	if id, err := refID(ref); err == nil {
		e.addBlk = append(e.addBlk, id)
	} else {
		// A malformed ref cannot be staged as anything else; record it so
		// Commit refuses rather than silently dropping the caller's intent.
		e.addBlk = append(e.addBlk, "\x00malformed")
	}
}

func (e *editor) RemoveBlocker(ref flow.ItemRef) {
	if id, err := refID(ref); err == nil {
		e.delBlk = append(e.delBlk, id)
	}
}

// Commit applies every staged change or none of them.
func (e *editor) Commit(ctx context.Context) error {
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	rec := e.b.items[e.id]
	if rec == nil {
		return fmt.Errorf("fake: item %q not registered", e.id)
	}

	for _, t := range append(slices.Clone(e.addTags), e.delTags...) {
		if !t.Valid() {
			return fmt.Errorf("fake: %q is not a valid tag", string(t))
		}
	}

	// Blockers are validated before anything is written: an identifier naming
	// nothing is a typo, and accepting it blocks the item forever on something
	// that does not exist.
	next := slices.Clone(rec.blockedBy)
	for _, id := range e.addBlk {
		if e.b.items[id] == nil {
			return fmt.Errorf("fake: blocker %q names no item — refusing to block on something that does not exist", id)
		}
		if id == e.id {
			return fmt.Errorf("fake: item %q named as its own blocker — a cycle of length one", e.id)
		}
		if !slices.Contains(next, id) {
			next = append(next, id)
		}
	}
	for _, id := range e.delBlk {
		next = slices.DeleteFunc(next, func(x string) bool { return x == id })
	}
	if cycle := e.b.findCycle(e.id, next); cycle != "" {
		return fmt.Errorf("fake: blocker %q closes a cycle back to %q — every item in the ring would be blocked and none would ever clear", cycle, e.id)
	}

	if e.title != nil {
		rec.item.Title = *e.title
	}
	if e.body != nil {
		rec.item.Body = *e.body
	}
	for _, t := range e.addTags {
		if !slices.Contains(rec.item.Tags, t) {
			rec.item.Tags = append(rec.item.Tags, t)
		}
	}
	for _, t := range e.delTags {
		rec.item.Tags = slices.DeleteFunc(rec.item.Tags, func(x flow.TagId) bool { return x == t })
	}
	rec.blockedBy = next
	if e.manual != nil {
		rec.item.Manual = *e.manual
		// Setting manual resolves any unresolved park: the operator's run-step
		// IS the resume.
		if *e.manual {
			rec.parkRequest = nil
		}
	}
	return nil
}

// findCycle walks the dependency graph from the item's proposed blocker set and
// reports the blocker that leads back to `from`, or "" when none does.
//
// Detection costs a traversal, and it is a traversal already being made: the
// derivation reads each blocker's status to answer "is this blocked" at all.
func (b *Orchestrator) findCycle(from string, blockers []string) string {
	for _, start := range blockers {
		seen := map[string]bool{}
		stack := []string{start}
		for len(stack) > 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if id == from {
				return start
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			if rec := b.items[id]; rec != nil {
				stack = append(stack, rec.blockedBy...)
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Artifacts and budget
// ---------------------------------------------------------------------------

func (b *Orchestrator) ResolveArtifact(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId, body flow.ArtifactBody) error {
	itemID, err := refID(ref)
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
	art.ResolvedBy = string(rec.holder.Account)
	art.PromptsThisInvocation = 0 // resets at successful resolve
	// A park recorded against this step is obsolete the moment the step
	// resolves — keeping it would make Load report a reason that no longer
	// holds.
	if rec.parkRequest != nil && rec.parkRequest.Step == flow.StepId(id) {
		rec.parkRequest = nil
	}
	return nil
}

func (b *Orchestrator) MarkStale(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId) error {
	return b.withArtifact(ref, id, func(a *flow.ArtifactRecord) { a.Stale = true })
}

func (b *Orchestrator) BumpInvocations(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId) error {
	return b.withArtifact(ref, id, func(a *flow.ArtifactRecord) {
		a.Invocations++
		a.PromptsThisInvocation = 0
		a.LastRunAt = b.clock()
	})
}

func (b *Orchestrator) BumpPrompts(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId) error {
	return b.withArtifact(ref, id, func(a *flow.ArtifactRecord) { a.PromptsThisInvocation++ })
}

func (b *Orchestrator) AddCost(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId, usd float64) error {
	return b.withArtifact(ref, id, func(a *flow.ArtifactRecord) { a.CostUSDSpent += usd })
}

func (b *Orchestrator) AddDuration(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId, d time.Duration) error {
	return b.withArtifact(ref, id, func(a *flow.ArtifactRecord) { a.DurationWorked += d })
}

// Grant adds budget to the artifact record and clears a ParkBudgetExhausted
// park that the grant actually satisfies (see the Orchestrator.Grant contract).
func (b *Orchestrator) Grant(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId, g flow.Grant) error {
	itemID, err := refID(ref)
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
	art.GrantedInvocations += g.Invocations
	art.GrantedPromptsPerInvocation += g.PromptsPerInvocation
	art.GrantedCostUSD += g.CostUSD
	art.GrantedTimeout += time.Duration(g.TimeoutAdd) * time.Second
	if flow.GrantClearsPark(rec.parkRequest, id, *art, g) {
		rec.parkRequest = nil
	}
	return nil
}

func (b *Orchestrator) withArtifact(ref flow.ItemRef, id flow.ArtifactId, mutate func(*flow.ArtifactRecord)) error {
	itemID, err := refID(ref)
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
	mutate(art)
	return nil
}

// ---------------------------------------------------------------------------
// Parking, work in progress and questions
// ---------------------------------------------------------------------------

func (b *Orchestrator) Park(ctx context.Context, ref flow.ItemRef, req flow.ParkRequest) error {
	itemID, err := refID(ref)
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

// SaveWorkInProgress stores what a step worked out against that step on this
// item. Keyed by both, which is the property the contract turns on.
func (b *Orchestrator) SaveWorkInProgress(ctx context.Context, ref flow.ItemRef, step flow.StepId, body string) error {
	itemID, err := refID(ref)
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
		rec.work = map[flow.StepId]string{}
	}
	rec.work[step] = body
	return nil
}

// LoadWorkInProgress returns what this step stashed against this item, or ""
// when there is none. Absence is ("", nil), not an error.
func (b *Orchestrator) LoadWorkInProgress(ctx context.Context, ref flow.ItemRef, step flow.StepId) (string, error) {
	itemID, err := refID(ref)
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
func (b *Orchestrator) ClearWorkInProgress(ctx context.Context, ref flow.ItemRef, step flow.StepId) error {
	itemID, err := refID(ref)
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

// AskQuestion records ONE question and APPENDS it. There is no replace: a step
// with several questions calls this several times, and each call says exactly
// which was recorded.
func (b *Orchestrator) AskQuestion(ctx context.Context, ref flow.ItemRef, aq flow.AgentQuestion) (flow.Question, error) {
	itemID, err := refID(ref)
	if err != nil {
		return flow.Question{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return flow.Question{}, fmt.Errorf("fake: item %q not registered", itemID)
	}
	rec.nextQID++
	q := flow.Question{
		ID:            flow.QuestionId(fmt.Sprintf("q%d", rec.nextQID)),
		AgentQuestion: aq,
		AskedAt:       b.clock(),
	}
	rec.questions = append(rec.questions, q)
	return q, nil
}

// AnswerQuestion is a test helper — fills in UserAnswer.Answer on a recorded
// question, addressed by the fake's own item id.
func (b *Orchestrator) AnswerQuestion(itemID string, qID flow.QuestionId, answer string) error {
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

// PostAnswer records the answer AGAINST THE QUESTION IT ANSWERS, and clears the
// park only when no pending question remains — answering one of three is not
// answering the item.
func (b *Orchestrator) PostAnswer(ctx context.Context, ref flow.ItemRef, id flow.QuestionId, text string) error {
	itemID, err := refID(ref)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.items[itemID]
	if rec == nil {
		return fmt.Errorf("fake: item %q not registered", itemID)
	}
	idx := -1
	for i := range rec.questions {
		if rec.questions[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("fake: question %q not found on item %q", id, itemID)
	}
	if rec.questions[idx].Answer != "" {
		return fmt.Errorf("fake: question %q on item %q is already answered", id, itemID)
	}
	now := b.clock()
	rec.questions[idx].UserAnswer = flow.UserAnswer{Answer: text, AnsweredAt: &now}
	if !anyPending(rec.questions) && rec.parkRequest != nil && rec.parkRequest.Kind == flow.ParkQuestion {
		rec.parkRequest = nil
	}
	return nil
}

func anyPending(qs []flow.Question) bool {
	for _, q := range qs {
		if q.Answer == "" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Completion
// ---------------------------------------------------------------------------

// Finalize records that the flow is done with an item the orchestrator already
// considers finished, and releases the claim.
//
// It REFUSES an item whose status is not terminal. Finalizing does not make an
// item terminal; a finalized item still open would claim the work is over while
// the orchestrator says it is not. The refusal is ErrUnavailable rather than
// ErrUnsupported: the item may reach terminal later, so asking again is exactly
// what a caller should do.
func (b *Orchestrator) Finalize(ctx context.Context, ref flow.ItemRef) error {
	id, err := refID(ref)
	if err != nil {
		return err
	}
	b.mu.Lock()
	rec := b.items[id]
	if rec == nil {
		b.mu.Unlock()
		return fmt.Errorf("fake: item %q not registered", id)
	}
	if rec.item.Status != flow.StatusTerminal {
		b.mu.Unlock()
		return fmt.Errorf("fake: item %q is %s, and only a %s item's flow run may be recorded complete: %w",
			id, rec.item.Status, flow.StatusTerminal, flow.ErrUnavailable)
	}
	rec.item.Finalized = true
	b.mu.Unlock()
	return b.Release(ctx, ref)
}

// ---------------------------------------------------------------------------
// Worktree
// ---------------------------------------------------------------------------

func (b *Orchestrator) Worktree(ctx context.Context, ref flow.ItemRef) (flow.Worktree, error) {
	id, err := refID(ref)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// One worktree PER ITEM, kept across calls. A real checkout keeps its
	// branch and its commits between invocations, which callers comparing a
	// branch against its base depend on — and it does NOT share them with
	// another item, which a single orchestrator-wide worktree would.
	if b.worktrees == nil {
		b.worktrees = map[string]*fakeWorktree{}
	}
	wt := b.worktrees[id]
	if wt == nil {
		wt = &fakeWorktree{branches: map[flow.BranchName]bool{}}
		b.worktrees[id] = wt
	}
	wt.verifyOK = b.verifyOK
	wt.commandOutcome = b.commandOutcome
	wt.gateOutcome = b.gateOutcome
	wt.verdict = b.verdict
	wt.supportsRequest = b.supportsRequest
	wt.nothingToCommit = b.nothingToCommit
	wt.branchHeads = b.branchHeads
	if b.initialBranch != "" && wt.branch == "" {
		wt.branch = b.initialBranch
	}
	return wt, nil
}

// SetNothingToCommit makes Worktree.Commit a no-op, as git is when nothing is
// staged. Use it to exercise a caller's "did anything actually land?" guard.
func (b *Orchestrator) SetNothingToCommit(v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nothingToCommit = v
	for _, wt := range b.worktrees {
		wt.nothingToCommit = v
	}
}

// SetBranchHeads populates a per-branch SHA map on every worktree (existing
// and future). When set, RevParse("HEAD") looks up the current branch in
// this map before falling back to the commit-counter formula. This models
// the real-world fact that switching branches changes the SHA HEAD resolves to.
func (b *Orchestrator) SetBranchHeads(m map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.branchHeads = m
	for _, wt := range b.worktrees {
		wt.branchHeads = m
	}
}

// SetInitialBranch sets the branch that new worktrees start on. Existing
// worktrees are unaffected — call this before any step runs. An empty string
// means "main" (the default). This models a checkout that is already on a
// claim branch when the step handler acquires the worktree.
func (b *Orchestrator) SetInitialBranch(name flow.BranchName) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.initialBranch = name
}

// SetDirty makes Worktree.IsDirty report true for all existing worktrees and
// any subsequently created ones. Use it to simulate a handler that leaves
// tracked files modified.
func (b *Orchestrator) SetDirty(v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, wt := range b.worktrees {
		wt.dirty = v
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
	branches        map[flow.BranchName]bool
	gateOutcome     flow.Outcome
	commandOutcome  flow.Outcome
	verdict         *bool
	verifyOK        bool
	supportsRequest bool
	branch          flow.BranchName
	dirty           bool
	// branchHeads maps branch names to SHAs so RevParse("HEAD") returns a
	// branch-specific value when set. Models the real-world fact that
	// switching branches changes the SHA HEAD resolves to.
	branchHeads map[string]string
}

func (w *fakeWorktree) Branch(ctx context.Context, name, base flow.BranchName) (bool, error) {
	w.branch = name
	// created means the branch did not EXIST — not merely that we were on a
	// different one. Callers use it to tell "there is no work here" from
	// "switching back to work that is already here", and conflating the two
	// makes every re-checkout look like a fresh start.
	if w.branches == nil {
		w.branches = map[flow.BranchName]bool{}
	}
	if w.branches[name] {
		return false, nil
	}
	w.branches[name] = true
	return true, nil
}

func (w *fakeWorktree) CurrentBranch(ctx context.Context) (flow.BranchName, error) {
	if w.branch == "" {
		return "main", nil
	}
	return w.branch, nil
}

func (w *fakeWorktree) IsDirty(ctx context.Context) (bool, error) {
	return w.dirty, nil
}

func (w *fakeWorktree) Stage(ctx context.Context) error { return nil }

// Commit is a no-op when SetNothingToCommit(true) — the real git behavior when
// nothing is staged, and the case a caller's "did anything land?" guard exists
// for.
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
//
// When branchHeads is set, HEAD resolves to the entry for the current branch
// (using "main" when branch is ""), modelling the real-world fact that
// switching branches changes the SHA HEAD resolves to.
func (w *fakeWorktree) RevParse(ctx context.Context, rev flow.Revision) (flow.CommitSha, error) {
	if rev != flow.HeadRevision {
		return "sha-0", nil
	}
	if w.branchHeads != nil {
		name := w.branch
		if name == "" {
			name = "main"
		}
		if sha, ok := w.branchHeads[string(name)]; ok {
			return flow.CommitSha(sha), nil
		}
	}
	return flow.CommitSha(fmt.Sprintf("sha-%d", w.commits)), nil
}

// Run answers for the three declared commands. A command returns a RUN, not a
// bare error: the outcome separates "ran and reported" from "could not start",
// "timed out" or "died", and the three have different budget consequences.
//
// The error is for a request no runner could attempt — a name that is not one
// of the three. Every way a command can fail is an outcome.
func (w *fakeWorktree) Run(ctx context.Context, name flow.CommandName) (flow.CommandRun, error) {
	if !name.Valid() {
		return flow.CommandRun{}, fmt.Errorf("fake: %q is not one of the three command names", name)
	}
	outcome := w.commandOutcome
	if outcome == "" {
		outcome = flow.OutcomeMeasured
	}
	run := flow.CommandRun{Command: name, Outcome: outcome, ExitCode: -1}
	if outcome == flow.OutcomeMeasured {
		run.ExitCode = 0
		if name == flow.CommandVerify && !w.verifyOK {
			run.ExitCode = 1
			run.Stdout = []byte("fake: verify failed\n")
		}
	}
	run.Detail = fmt.Sprintf("fake: command %q %s", name, outcome)
	return run, nil
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
// tests can exercise the pull-request paths. Tests that want the "this
// orchestrator does not land through pull requests" path call
// SetSupportsRequest(false).
func (w *fakeWorktree) Request() flow.RequestManager {
	if !w.supportsRequest {
		return nil
	}
	return w
}

func (w *fakeWorktree) Open(ctx context.Context, base flow.BranchName, title, body string) (flow.RequestUrl, error) {
	return "https://example.invalid/pr/1", nil
}
func (w *fakeWorktree) Merge(ctx context.Context, url flow.RequestUrl) error { return nil }
func (w *fakeWorktree) FindPR(ctx context.Context) (flow.PRInfo, error) {
	return flow.PRInfo{URL: "https://example.invalid/pr/1", MergeCommitSHA: "sha-merge"}, nil
}
func (w *fakeWorktree) PrepareMergeResult(ctx context.Context, base flow.BranchName) error {
	return nil
}
func (w *fakeWorktree) RevertMergePrep(ctx context.Context) error { return nil }
func (w *fakeWorktree) RebuildTools(ctx context.Context) error    { return nil }

// One assertion covers the whole surface: with no optional capabilities there
// is nothing left for a second one to check.
var _ flow.Orchestrator = (*Orchestrator)(nil)
var _ flow.Worktree = (*fakeWorktree)(nil)
var _ flow.RequestManager = (*fakeWorktree)(nil)
var _ flow.ItemEditor = (*editor)(nil)
