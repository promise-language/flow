package flow

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"time"
)

// Item is the backend-supplied per-item snapshot the SDK gives to handlers
// via ctx.Item(). The shape is universal — both github and tracker backends
// populate the same fields.
type Item struct {
	ID    string   // backend-specific identifier; opaque to SDK
	Type  ItemType // routes flow selection; MUST be non-empty
	Title string
	Body  string
	URL   string // backend-specific display URL
	Flow  string // last selected flow name, when known

	// Finalized marks the item's flow run as complete — the sole terminal
	// "no more work" signal (set by Finalizer.Finalize). Optional/backend-
	// specific: the tracker backend populates it; backends without a finalize
	// concept leave it false. Surfaced so `status` can distinguish "finalized"
	// from "no flow currently eligible".
	Finalized bool
}

// IDStr is a convenience for handlers that just want the id as a string.
func (i Item) IDStr() string { return i.ID }

// IDInt parses ID as an integer; returns 0 + error if not numeric. Convenience
// for github backend handlers (issues are numeric).
func (i Item) IDInt() (int, error) {
	return strconv.Atoi(i.ID)
}

// ItemRef is the backend-internal address for an item. SDK code passes
// ItemRef values around; only the originating backend interprets Ref.
type ItemRef struct {
	BackendName string          `json:"backend"`
	Display     string          `json:"display"` // for logs / UI; e.g. "owner/repo#123"
	Ref         json.RawMessage `json:"ref"`     // backend-internal addressing
}

// RefResolver is an optional Backend capability: turn a user-supplied item id
// string directly into an ItemRef, without enumerating eligible items.
//
// `claim <id>` needs an ItemRef, but the id the user types is just a string.
// The generic CLI path resolves it by listing eligible items and substring-
// matching Display — which is wasteful and, worse, confines `claim` to items
// currently in the eligible set (e.g. status=open). A backend whose ItemRef is
// derivable from the id alone (the tracker, where the ref IS the id) should
// implement RefResolver so the CLI builds the ref directly and can claim any
// named item regardless of its current status. Backends that genuinely need a
// lookup (github: id → owner/repo#n) omit it and keep the list-and-match
// fallback.
type RefResolver interface {
	ResolveRef(ctx context.Context, id string) (ItemRef, error)
}

// Finalizer is an optional Backend capability: mark an item's flow run complete
// (no more steps will run) and release its claim. cli.RunOne calls Finalize when
// SelectFlow finds no eligible step, so a completed MANUAL run finalizes and
// frees the arena the same way the orchestrator's auto path does on flow
// completion — rather than leaving the item un-finalized with the lease held.
// Backends that don't implement it just return the "no eligible flow" result.
type Finalizer interface {
	Finalize(ctx context.Context, claim Claim) error
}

// ManualTakeover is an optional Backend capability: signal that the operator
// has taken hand control of an item (typed `run-step` directly, rather than
// the runner spawning the flow). cli.cmdRun calls this at the top of every
// operator-driven invocation so the backend can apply its "I'm driving now"
// side effects — e.g. the tracker backend sets Manual=true (so the
// orchestrator stops auto-dispatching the item underneath the operator) and
// resolves any unresolved FlowPark (the operator's run-step IS the resume).
// Backends without a manual/park concept simply omit the interface. T0481.
type ManualTakeover interface {
	MarkManualTakeover(ctx context.Context, claim Claim) error
}

// StateInspector is an optional Backend capability: load an item's flow state
// (artifacts + signals + questions) READ-ONLY, addressed by ItemRef, with NO
// claim. `status <id>` uses it to inspect any item's lifecycle checklist
// straight from the backing store without acquiring a lease. Backends whose
// state is resolvable from the ref alone (the tracker, where the ref IS the id)
// implement it; backends that omit it make `status` require an active claim.
type StateInspector interface {
	LoadStateByRef(ctx context.Context, ref ItemRef) (*ItemState, error)
}

// QuestionAnswerer is an optional Backend capability: post a human answer
// on an item and clear the outstanding-question marker, without a claim.
type QuestionAnswerer interface {
	// PostAnswer posts a plain comment carrying the human's answer and clears
	// the outstanding-question marker (best-effort label removal).
	PostAnswer(ctx context.Context, ref ItemRef, text string) error

	// ClearQuestionMarker clears the outstanding-question marker without
	// posting a comment. Used when an out-of-band answer is observed.
	ClearQuestionMarker(ctx context.Context, ref ItemRef)
}

// WorkInProgress is an optional Backend capability: somewhere for a step to
// leave what it worked out when it stops without completing, so the next
// dispatch continues rather than restarts.
//
// `step` is the step's RESULT ID (LifecycleItem.Result()) — the same identity
// that keys the budget record and that `grant` accepts, so nothing has to
// translate between two names for one step.
//
// The contract is on the implementation:
//
//   - A record is keyed by the claim's ITEM and by `step`, and a stored record
//     naming a different item or step is not this step's: LoadWorkInProgress
//     returns "" for it. Keying is the correctness property, not clearing —
//     every path that skips the cleanup (a crash, a kill, a `.flow` left by an
//     abandoned run) would otherwise feed one item's reasoning to another
//     item's agent, arriving with origin OriginAgent and indistinguishable
//     from that agent's own thinking.
//   - Absence is ("", nil), not an error. A step that never stashed anything
//     is the ordinary case, and it is exactly today's behaviour.
//   - NOTHING HERE IS EVER PUBLISHED. The record is the step's own scratch
//     state. For a refused write the text to store IS the text a disclosure
//     guard refused, so a store that could go outward is a store that cannot
//     hold it.
//   - ClearWorkInProgress is idempotent: clearing what is not there is nil.
//
// Where the record physically lives is the backend's business. A local
// backend keeps it beside its claim state; a server-backed one keeps it with
// the claim, where an arena can lose its disk without losing the record.
type WorkInProgress interface {
	SaveWorkInProgress(ctx context.Context, claim Claim, step, body string) error
	LoadWorkInProgress(ctx context.Context, claim Claim, step string) (string, error)
	ClearWorkInProgress(ctx context.Context, claim Claim, step string) error
}

// DiscoveryScope names how far up the listing ladder to go. The set is closed;
// its names are the level names from the issue description.
type DiscoveryScope string

const (
	// ScopeAll: every item the backend holds, open and closed.
	ScopeAll DiscoveryScope = "all"
	// ScopeOpen: every open item.
	ScopeOpen DiscoveryScope = "open"
	// ScopeProcessable: open items this binary could process (default).
	ScopeProcessable DiscoveryScope = "processable"
	// ScopeWorkable: processable items not blocked — someone could work them.
	ScopeWorkable DiscoveryScope = "workable"
	// ScopeFree: workable items this operator could claim now.
	ScopeFree DiscoveryScope = "free"
	// ScopeAuto: free items that are opted in — an unattended `resolve` would pick one.
	ScopeAuto DiscoveryScope = "auto"
)

// ValidScope reports whether s is one of the six recognized scope values.
func ValidScope(s DiscoveryScope) bool {
	switch s {
	case ScopeAll, ScopeOpen, ScopeProcessable, ScopeWorkable, ScopeFree, ScopeAuto:
		return true
	}
	return false
}

// Availability is the per-item state, from a closed set. Each state is the
// boundary between two adjacent scope levels.
type Availability string

const (
	// AvailAuto: in scope level 6 — opted in for unattended selection.
	AvailAuto Availability = "auto"
	// AvailAvailable: in 5 (free) but not 6 (auto).
	AvailAvailable Availability = "available"
	// AvailHeld: in 4 (workable) but not 5 (free) — someone else holds it.
	AvailHeld Availability = "held"
	// AvailBlocked: in 3 (processable) but not 4 (workable).
	AvailBlocked Availability = "blocked"
	// AvailUnhandled: in 2 (open) but not 3 — no flow here accepts the type.
	AvailUnhandled Availability = "unhandled"
	// AvailClosed: in 1 (all) but not 2.
	AvailClosed Availability = "closed"
)

// InScope reports whether an item at this availability level is included in
// the given scope.
func (a Availability) InScope(s DiscoveryScope) bool {
	return availLevel(a) >= scopeLevel(s)
}

func scopeLevel(s DiscoveryScope) int {
	switch s {
	case ScopeAll:
		return 1
	case ScopeOpen:
		return 2
	case ScopeProcessable:
		return 3
	case ScopeWorkable:
		return 4
	case ScopeFree:
		return 5
	case ScopeAuto:
		return 6
	}
	return 0
}

func availLevel(a Availability) int {
	switch a {
	case AvailClosed:
		return 1
	case AvailUnhandled:
		return 2
	case AvailBlocked:
		return 3
	case AvailHeld:
		return 4
	case AvailAvailable:
		return 5
	case AvailAuto:
		return 6
	}
	return 0
}

// DiscoveryItem is the per-item snapshot returned by Discoverer.Discover.
// It is NOT ItemRef: ItemRef is an addressing type embedded in Claim and
// serialized to .flow/active.json, so attaching mutable fields (tags, holder)
// to it would freeze them at claim time. DiscoveryItem carries the same
// addressing fields plus the mutable listing metadata.
type DiscoveryItem struct {
	// Addressing — same fields as ItemRef, usable to construct one.
	BackendName string          `json:"backend"`
	Display     string          `json:"display"`
	Ref         json.RawMessage `json:"ref"`

	// Listing metadata — NOT frozen at claim time.
	Title        string       `json:"title"`
	Availability Availability `json:"availability"`
	Holder       string       `json:"holder,omitempty"`     // current claim owner, if any
	Tags         []string     `json:"tags"`                 // ALL labels, not just flow:*
	BlockedBy    []string     `json:"blocked_by,omitempty"` // blocking item refs, by display id
	BlockReason  string       `json:"block_reason,omitempty"`
}

// ItemRef converts the discovery item into an ItemRef for use by Claim et al.
func (d DiscoveryItem) ItemRef() ItemRef {
	return ItemRef{
		BackendName: d.BackendName,
		Display:     d.Display,
		Ref:         d.Ref,
	}
}

// Discoverer is an optional Backend capability: list items visible to the
// operator beyond the narrow eligible set that ListEligible returns.
//
// ListEligible feeds resolve's auto-select: its narrowness is a safety
// property (it must never pick an item that should not be auto-started).
// Discover feeds `list`: it returns items at any scope the operator asks for,
// with per-item availability, tags, and holder — enough for the operator to
// see the landscape and choose what to claim.
//
// INVARIANT: resolve's auto-select path MUST NEVER call Discover. The two
// draw from different base sets by design, and widening auto-select would
// let a bare `resolve` pick an arbitrary open issue and begin work on it.
// This invariant is stated here and covered by a test.
type Discoverer interface {
	Discover(ctx context.Context, scope DiscoveryScope, binaryName string) ([]DiscoveryItem, error)
}

// TagFilterer is an optional Backend capability: list eligible items
// carrying all of the given tags. Used by `resolve --tag` to narrow the
// auto-selectable set. The result is a subset of what ListEligible returns
// (same base set, fewer items); the tags filter is conjunctive.
//
// Backends that can filter server-side (e.g. the github backend appends
// `label:<tag>` terms to its search query) implement this for efficiency.
// Backends without it make `--tag` unavailable on resolve.
type TagFilterer interface {
	ListEligibleWithTags(ctx context.Context, tags []string) ([]ItemRef, error)
}

// Claim is the credentialed handle returned by Backend.Claim. Holds the
// backend-internal token used by subsequent write ops; the SDK serializes
// this to .flow/active.json.
type Claim struct {
	BackendName string          `json:"backend"`
	ItemRef     ItemRef         `json:"item"`
	Owner       string          `json:"owner"`
	ClaimedAt   time.Time       `json:"claimed_at"`
	Token       json.RawMessage `json:"token"`               // backend-internal credential
	Overrides   []string        `json:"overrides,omitempty"` // opaque strings echoed from the lease response
}

// ClaimInfo is the read-only view returned by Backend.LookupClaim. Cannot be
// used for write ops.
type ClaimInfo struct {
	Owner     string
	ClaimedAt time.Time
}

// ItemState is the snapshot LoadState returns. Artifacts + signals + any
// outstanding questions in one round.
type ItemState struct {
	Item      Item
	Artifacts map[ArtifactId]ArtifactRecord
	Signals   map[SignalId]SignalState
	Questions []Question

	// Park is the item's current park record, or nil when it is not parked.
	// Populated by Backend.LoadState so a caller can see WHY an item stopped
	// (which step, which budget axis) instead of inferring it. `grant` with no
	// arguments reads this to top up exactly the axis that parked the step.
	//
	// Optional/backend-specific: backends with no park store leave it nil, and
	// the CLI degrades to explicit `grant <step-id>` / `grant --all`. A backend
	// that DOES populate it must also clear it per the Backend.Grant contract —
	// a park record that outlives the condition that caused it is worse than no
	// park record at all, because every reader then acts on a stale reason.
	Park *ParkRequest
}

// Parked reports whether the item currently carries a park record.
func (s *ItemState) Parked() bool { return s != nil && s.Park != nil }

// PendingQuestions returns the subset of Questions that have not yet been
// answered (UserAnswer.Answer is empty). The flow parks while this is
// non-empty.
func (s *ItemState) PendingQuestions() []Question {
	if s == nil {
		return nil
	}
	out := make([]Question, 0, len(s.Questions))
	for _, q := range s.Questions {
		if q.Answer == "" {
			out = append(out, q)
		}
	}
	return out
}

// Artifact looks up an artifact record by id; returns the zero record if
// absent. Convenience for derivation paths that don't care about the
// ok-value.
func (s *ItemState) Artifact(id ArtifactId) ArtifactRecord {
	if s == nil {
		return ArtifactRecord{}
	}
	return s.Artifacts[id]
}

// HasRequiredArtifacts reports whether the item has a seeded finalization
// checklist — i.e. at least one artifact record marked Required. It is the
// "is this item seeded?" predicate used by cli.RunOne's mandatory-seed gate:
// an item with no required artifact has not been seeded and the flow must not
// run any step against it.
func (s *ItemState) HasRequiredArtifacts() bool {
	if s == nil {
		return false
	}
	for _, rec := range s.Artifacts {
		if rec.Required {
			return true
		}
	}
	return false
}

// SignalSet returns true iff the named signal is set on the item.
func (s *ItemState) SignalSet(id SignalId) bool {
	if s == nil {
		return false
	}
	return s.Signals[id].Set
}

// Backend is the pluggable storage + worktree boundary the SDK targets. The
// github backend lives in pkg/backend/github; the tracker backend lives in
// the closed tracker repo. Both satisfy the same interface.
type Backend interface {
	Name() string

	// SupportedSignals returns the set of SignalIds this backend knows how
	// to observe. cli.Run validates every signal reference against this list
	// at startup.
	SupportedSignals() []SignalDef

	// SupportedArtifacts returns this backend's canonical artifact schema: the
	// closed, well-defined set of artifacts it knows how to RECORD, each with
	// its stable id, type, and a description (ArtifactDef.Doc). cli.Run
	// validates every declared App.Artifact against this set at startup — by id
	// AND type — so a flow that declares an artifact this backend cannot store
	// (unknown id, or a type that disagrees with the backend's) is refused at
	// startup (every invocation, exit 2) rather than failing at resolve-time
	// after the producing step has already run and burned a turn.
	//
	// It is a closed list (cf. SupportedSignals), not an open "supports
	// anything" predicate: the (id, type) pair is a stable schema multiple
	// flows — even across projects — must agree on. Owning that schema in the
	// backend keeps it coordinated; letting each flow invent artifacts ad hoc
	// would push schema coordination onto the flows. A backend that can
	// technically store any id (e.g. github) still declares a curated set.
	SupportedArtifacts() []ArtifactDef

	// ListEligible returns candidate items in the backend's scope.
	ListEligible(ctx context.Context) ([]ItemRef, error)

	// Claim acquires an exclusive lease on the item. overrides names safety
	// checks the operator chose to bypass; nil means no overrides. The old
	// force=true maps to []ClaimOverride{OverrideDirtyTree}.
	Claim(ctx context.Context, ref ItemRef, owner string, overrides []ClaimOverride) (Claim, error)

	// Release relinquishes the lease.
	Release(ctx context.Context, claim Claim) error

	// LookupClaim returns read-only info about the current claim, or
	// (nil, nil) if unclaimed.
	LookupClaim(ctx context.Context, ref ItemRef) (*ClaimInfo, error)

	// LookupActiveClaim returns the backend-authoritative active claim held
	// by `owner` right now, or (nil, nil) if owner holds nothing. This is
	// the single source of truth for "what am I currently working on?"
	// across cli.cmd_run / cmd_status / cmd_release; the CLI never falls
	// back to a local cache. Backends choose where their lease state lives:
	//   - GitHub backend: stores the lease in .flow/active.json (the file
	//     IS the backend's lease store; see pkg/clistate).
	//   - Tracker backend: queries the tracker server's lease ledger; on
	//     server-offline, returns an error rather than reading a stale
	//     local file.
	LookupActiveClaim(ctx context.Context, owner string) (*Claim, error)

	// LoadState returns artifacts + signals in one snapshot, with signals
	// refreshed by backend-internal polling.
	LoadState(ctx context.Context, claim Claim) (*ItemState, error)

	// SeedState pre-loads the artifact set + budget caps. The backend MUST
	// refuse a second seed for the same item; mid-flight items are frozen
	// against later flow-source changes. Use ResetSeed (an explicit,
	// operator-initiated path) to re-seed an in-flight item against the
	// current flow source.
	SeedState(ctx context.Context, claim Claim, artifacts []ArtifactSpec) error

	// ResetSeed clears the item's existing seed so the next SeedState call
	// succeeds. This is the ONLY escape hatch from SeedState's "frozen
	// after first write" contract — adding a new step to a flow definition
	// must not retroactively re-seed every previously-processed item.
	// ResetSeed is operator-initiated (e.g. a tracker-UI "re-seed with
	// current flow" button); the SDK / cli.RunOne never calls it
	// automatically.
	//
	// After ResetSeed the item's artifact records, budget counters, and
	// any park state SHOULD be cleared so the next SeedState pass starts
	// from a clean slate. Backends that have no separable seed concept
	// (e.g. a future read-only backend) may return ErrResetSeedUnsupported.
	ResetSeed(ctx context.Context, claim Claim) error

	// ResolveArtifact writes a handler-produced artifact value. No backend
	// method for writing signals; signals are written by backend-internal
	// side effects (worktree actions) or the LoadState poll path.
	//
	// An EMPTY body is a legal call, not a client-side error, and the SDK
	// passes it through untouched. It is the side-effect-artifact pattern:
	// the content was already attached out-of-band (a runner-captured patch,
	// a commit) and the handler is saying "I'm done — record me as resolved".
	// A backend that stores such content elsewhere MUST decide emptiness
	// itself: verify the side effect happened and fail with a message naming
	// what is missing. Backends that carry the bytes in the body may likewise
	// reject an empty one — either way the judgment is the backend's, because
	// only it knows where the evidence lives.
	ResolveArtifact(ctx context.Context, claim Claim, id ArtifactId, body ArtifactBody) error
	MarkStale(ctx context.Context, claim Claim, id ArtifactId) error

	// Budget counters — transactional with the artifact record.
	BumpInvocations(ctx context.Context, claim Claim, key string) error
	BumpPrompts(ctx context.Context, claim Claim, key string) error
	AddCost(ctx context.Context, claim Claim, key string, usd float64) error
	AddDuration(ctx context.Context, claim Claim, key string, d time.Duration) error

	// Grant adds budget to the artifact record named by key (an ArtifactId as
	// a string — signal steps own no budget record and are never grantable).
	//
	// Grant MUST clear a ParkBudgetExhausted park when the grant raises the
	// parked step's offending axis above its consumption — use GrantClearsPark
	// so every backend applies the same rule. Parks of any other kind, and
	// grants too small to clear the cap, MUST be left in place: reporting an
	// item as unparked when the next dispatch would re-park it immediately is
	// the failure mode this contract exists to prevent.
	//
	// Clearing belongs here rather than in the CLI because Grant is not reached
	// only through the CLI — an operator granting from a backend's own UI must
	// drop the park (and any park label) the same way.
	Grant(ctx context.Context, claim Claim, key string, g Grant) error

	Park(ctx context.Context, claim Claim, req ParkRequest) error

	// AskQuestions records one or more agent-asked questions on the item.
	// The backend assigns each a unique id and persists the AgentQuestion
	// payload; returns the same questions populated with their assigned ids
	// (also reachable via the next LoadState as ItemState.Questions). The
	// flow parks until at least one is answered.
	AskQuestions(ctx context.Context, claim Claim, qs []AgentQuestion) ([]Question, error)

	Worktree(ctx context.Context, claim Claim) (Worktree, error)
}

// Worktree is the local-git surface the SDK exposes to handlers via
// ctx.Worktree(). Branch/Commit/Push/Validate are idempotent where they
// reasonably can be.
//
// Optional capabilities are exposed via accessor methods that may return
// nil when the backend does not support them. The first such capability is
// Request() — backends that have no pull-request concept (e.g. tracker
// backends that commit direct to master) return nil. Use the top-level
// nil-safe helpers (flow.Open, flow.Merge) for handlers that prefer a typed
// error over a panic.
type Worktree interface {
	// Branch ensures `name` is checked out. Creates off base (or HEAD if
	// base==""). Idempotent: no-op if already current. Errors on dirty
	// tree. Returns true iff newly created.
	Branch(ctx context.Context, name string, base string) (created bool, err error)
	CurrentBranch(ctx context.Context) (string, error)

	// Stage makes every change in the tree, untracked files included, visible
	// to the next CapturePatch — without committing it.
	//
	// The contract is that OUTCOME, not a particular mechanism. It exists
	// because a git-shaped CapturePatch diffs against HEAD, which cannot see
	// untracked files, while Commit stages everything: capturing before the
	// commit silently omits every file the change added, and capturing after it
	// sees a clean tree and returns nothing. Staging between the two is what
	// makes the diff complete.
	//
	// A backend whose CapturePatch already accounts for untracked content —
	// server-side capture, say — legitimately implements this as a no-op. That
	// is not a stub: the guarantee callers depend on already holds.
	Stage(ctx context.Context) error

	Commit(ctx context.Context, msg string) error
	// Push publishes the branch.
	//
	// It MAY WAIT. Landing is rebase → measure the merge result → push, and a
	// push that lands first invalidates every merge result measured against the
	// old mainline. With two workers landing at once and nothing serializing
	// them, each one's push sends the other back to rebase and measure again,
	// indefinitely — a livelock in which the work is sound, the gate passes
	// every time, and nothing ever lands.
	//
	// So a backend serializes landing across everything sharing the mainline,
	// which is a wider scope than the host-level serialization heavy gates
	// need: one protects a machine's resources, the other protects a loop's
	// ability to finish. Both are the backend's, because only it knows what
	// shares a machine and what shares a mainline.
	//
	// Waiting is not failing here either.
	Push(ctx context.Context) error

	// RevParse resolves a revision to a commit SHA.
	//
	// Every implementation MUST answer "HEAD" and the item's base branch.
	// Anything beyond those two is best-effort, and a backend that cannot
	// resolve arbitrary revisions MUST return an error naming the limitation
	// rather than fall back to HEAD — a caller comparing a branch against its
	// base would otherwise be handed the same SHA twice and conclude the branch
	// is empty. Callers should stay inside the guaranteed set; a step that
	// needs more than that is a step that will not run on every backend.
	//
	// The guaranteed pair is what tells a branch carrying work from an empty
	// one. Commit is a deliberate no-op when nothing is staged, so its nil
	// return is not evidence anything was recorded, and comparing HEAD before
	// and after only covers the invocation that made the commit. Comparing the
	// branch against its base covers a fresh branch and a resumed one alike —
	// without it an empty branch travels all the way to "No commits between
	// ...", long past where the real cause could have been named.
	RevParse(ctx context.Context, rev string) (string, error)

	// Verify runs the project's verify COMMAND in the worktree. Returns nil iff
	// it exits 0.
	//
	// It repairs what has one right answer — formatting and the like — and then
	// measures, so it MAY MODIFY THE WORKTREE. A caller re-reads worktree state
	// afterwards rather than assuming the tree is unchanged.
	//
	// A producing step works with this. No decision rests on it: it changed its
	// own subject on the way to an answer, so "it passed" is a claim about a
	// tree that no longer exists.
	Verify(ctx context.Context) error

	// RunGate runs the named GATE in the worktree and reports what the RUNNER
	// observed of the process.
	//
	// The SDK is the party that spawns gate processes, so the SDK is the
	// runner. A gate is never invoked directly and its own exit code is not
	// consulted: the states that matter most — killed for memory, exited 0
	// having printed nothing — are the ones the gate is not alive to report,
	// and only the process that spawned it can tell them apart.
	//
	// The runner reports an OUTCOME, never a verdict. "measured" says a
	// measurement exists, not that it is acceptable; that needs the
	// thresholds, which are a separate artefact out of the subject's reach and
	// held by neither the gate nor the runner.
	//
	// The gate's exit code travels in GateRun.ExitCode as a raw diagnostic and
	// nothing decides on it.
	//
	// A NON-NIL ERROR MEANS NO GATE WAS RUN AND NO OUTCOME EXISTS. It is
	// returned only for a request the runner could not attempt — an undeclared
	// gate name, or a caller whose context went away — and a caller must never
	// read it as a gate failure. Every way a gate can fail is an outcome:
	// OutcomeCouldNotStart in particular is an outcome, not an error, because
	// a caller that cannot tell it from OutcomeDied retries a missing binary
	// forever.
	//
	// A gate MODIFIES NOTHING it measures, which is what makes its answer
	// reproducible by whoever runs it — a reviewer, a later bisect, a rebuild
	// elsewhere. That is the entire reason a decision may rest on a gate and
	// not on Verify.
	//
	// "Modifies nothing" covers AFTER the measurement as well as during it. An
	// implementation that measures faithfully and then tidies the worktree is
	// not a gate: a producing step asks a gate mid-work, and cleaning up behind
	// the answer would discard the very work the step is in the middle of.
	//
	// The verdict run takes no parameters. A gate may accept options for other
	// purposes, but the pass/fail question is asked one way and one way only,
	// or two callers asking "did this pass" can get different answers and
	// neither is wrong.
	//
	// Naming the gate rather than configuring a command is what makes the parts
	// addressable: a step fixing one failing suite runs that suite, instead of
	// paying for the whole set to learn about the part it is working on.
	// The concept is closed; the instance after a colon is the project's, so
	// "tested:wasm" and "tested:go" are both askable and both obviously tests.
	// See GateName.
	//
	// A gate MAY WAIT before it runs. Some are too heavy to run beside another
	// — a full suite saturating a machine measures its own contention as much
	// as the code — so the backend serializes what has to be serialized. Which
	// gates those are is known to whoever runs them on real hardware, not here.
	//
	// Waiting is not failing, and the difference must survive. A gate that
	// queued and then ran is exactly as authoritative as one that ran at once;
	// a gate that gave up waiting has not measured anything, and reporting that
	// as a refusal marks a sound change unsound and sends someone to look for a
	// defect that is not there.
	RunGate(ctx context.Context, name GateName) (GateRun, error)

	// Judge asks the PROJECT whether a measurement is acceptable.
	//
	// THE SDK NEVER COMPUTES THIS ITSELF. The thresholds are the project's —
	// what its coverage floor is, which numbers block a change, what a
	// baseline ratchets to — and an SDK that computed a verdict would have to
	// hold them, which is the same mistake as a gate holding its own
	// thresholds one layer up. It would have to hold them for every project it
	// is ever pointed at.
	//
	// The SDK KEEPS THE SPAWN. The judge is handed an envelope it did not
	// produce and could not have: a project entry point that ran the gate
	// itself and returned a verdict would be the runner, and the runner is the
	// one party that may not come from the tree — it is the sole witness to a
	// process that no longer exists, so a runner that lied is undetectable in
	// principle. The judge is checkable, which is what lets it be a tree
	// artifact: re-run it against the same envelope and the same thresholds
	// and it answers the same, which is why GateVerdict carries both.
	//
	// ONLY OutcomeMeasured MAY BE JUDGED, and a run carrying any other outcome
	// is refused with an error before anything is spawned. The other four are
	// not verdicts and must never be passed off as one: a gate that could not
	// start, timed out, died or broke its contract has not reported that the
	// tree is bad, and turning that into a refusal blames a change for a
	// failure that was never about it.
	//
	// A NON-NIL ERROR MEANS NO VERDICT EXISTS, AND IS NEVER A REFUSAL. An
	// unanswerable judge — absent, wedged, printing something that is not a
	// verdict — says nothing about the measurement, and a caller that read it
	// as "not acceptable" would refuse a sound change because the project's
	// own tooling is broken. That is a fact for a person, not a result.
	//
	// A refusal, by contrast, is a perfectly good verdict and arrives with a
	// nil error: Acceptable is false and Detail says which metric, its value,
	// and the term it was judged against.
	//
	// The judge needs no outcome vocabulary of its own. What became of the
	// judge's own process is nobody's evidence about anything — unlike the
	// runner, it is not the sole witness to a vanished process, and anyone
	// holding the envelope can ask again.
	Judge(ctx context.Context, run GateRun) (GateVerdict, error)

	// CapturePatch produces a unified diff of the current working tree.
	// Handlers call it to attach work they have already verified; the
	// orchestrator does NOT call it when a step's deadline fires — a timeout
	// park carries no verify-green signal, and a step that commits before a
	// long verify has an empty diff to capture anyway.
	//
	// Returning no bytes is legal and does not mean failure. A backend whose
	// patches live server-side attaches the diff out-of-band and has nothing
	// client-side to hand back; so does a tree whose work is already
	// committed. Such a handler still resolves its patch artifact — with an
	// EMPTY PatchBody, which ResolveArtifact validates against wherever the
	// evidence actually lives (see ResolveArtifact).
	CapturePatch(ctx context.Context) (patch []byte, err error)

	// Request returns the backend's pull-request management surface, or nil
	// if the backend does not support pull-request operations.
	// Implementations that DO support this surface may return the worktree
	// itself, a sub-struct, or a thin wrapper — callers should not assume any
	// particular concrete type.
	Request() RequestManager
}

// RequestManager is the optional pull-request management surface exposed
// via Worktree.Request(). Implementations may trigger backend-internal
// side effects that set signals (e.g. pr-open / pr-merged on github).
type RequestManager interface {
	Open(ctx context.Context, base, title, body string) (url string, err error)
	Merge(ctx context.Context, url string) error
}

// Open is a nil-safe convenience: if wt.Request() returns nil (including a
// typed-nil interface value), returns ErrRequestNotSupported instead of
// panicking on a nil-receiver call. Handlers that want a clean typed-error
// path use this; handlers that know their backend supports pull requests
// can call wt.Request().Open(...) directly.
func Open(ctx context.Context, wt Worktree, base, title, body string) (string, error) {
	rq := wt.Request()
	if isNilRequest(rq) {
		return "", ErrRequestNotSupported
	}
	return rq.Open(ctx, base, title, body)
}

// Merge is the nil-safe counterpart to Open. See Open for usage.
func Merge(ctx context.Context, wt Worktree, url string) error {
	rq := wt.Request()
	if isNilRequest(rq) {
		return ErrRequestNotSupported
	}
	return rq.Merge(ctx, url)
}

// PRFinder is an optional RequestManager capability: look up the pull
// request for the current claim branch. Backends whose Worktree knows its
// claim branch implement this; others make the merge step unreachable.
type PRFinder interface {
	FindPR(ctx context.Context) (PRInfo, error)
}

// PRInfo is the read-only snapshot a PRFinder returns.
type PRInfo struct {
	URL            string
	MergeCommitSHA string // the merge commit; reliable only when the PR is merged
}

// FindPR is a nil-safe convenience for PRFinder, parallel to Open/Merge.
// Returns ErrRequestNotSupported when the worktree's RequestManager does not
// implement PRFinder.
func FindPR(ctx context.Context, wt Worktree) (PRInfo, error) {
	rq := wt.Request()
	if isNilRequest(rq) {
		return PRInfo{}, ErrRequestNotSupported
	}
	finder, ok := rq.(PRFinder)
	if !ok {
		return PRInfo{}, ErrRequestNotSupported
	}
	return finder.FindPR(ctx)
}

// MergeResultPreparer is an optional Worktree capability: set up the tree
// to reflect the merge result so the gate measures what will actually land.
type MergeResultPreparer interface {
	PrepareMergeResult(ctx context.Context, base string) error
	RevertMergePrep(ctx context.Context) error
}

// isNilRequest catches both untyped-nil interfaces and the typed-nil pitfall
// (a non-nil interface header pointing at a nil concrete pointer), which a
// plain `rq == nil` check misses and which would otherwise panic on the
// method call.
func isNilRequest(rq RequestManager) bool {
	if rq == nil {
		return true
	}
	v := reflect.ValueOf(rq)
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		return v.IsNil()
	}
	return false
}

// Answer is one human reply to a question a flow asked.
//
// It lives in the flow package rather than in a consumer because backends
// produce it and recipe libraries consume it: a type declared in either one
// would force the other to import it, and Go's method sets are nominal, so a
// look-alike struct would not satisfy the reader interface.
//
// The field set matches the tracker's wire.Question subset, so a step body
// reads the same on either backend. Author has no tracker analogue but is
// kept: "the reporter answered" and "a passer-by answered" are different facts
// to whoever reads the thread next.
type Answer struct {
	QuestionID string
	Text       string // the question that was asked
	Answer     string
	Author     string // e.g. github login
	AnsweredAt time.Time
}

// RepoPermissions is what the authenticated principal may do on the item's
// repository. Backends that have no permission model at all (a tracker where
// the runner holds full rights by construction) do not implement the probe
// that returns it; consumers requiring a role must be configured explicitly.
//
// The flags are cumulative as GitHub reports them — an admin also carries
// maintain/push/triage/pull — so decide a role by testing from the most
// privileged flag down, not by expecting exactly one to be set.
type RepoPermissions struct {
	Admin    bool
	Maintain bool
	Push     bool
	Triage   bool
	Pull     bool
}
