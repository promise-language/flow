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

// Claim is the credentialed handle returned by Backend.Claim. Holds the
// backend-internal token used by subsequent write ops; the SDK serializes
// this to .flow/active.json.
type Claim struct {
	BackendName string          `json:"backend"`
	ItemRef     ItemRef         `json:"item"`
	Owner       string          `json:"owner"`
	ClaimedAt   time.Time       `json:"claimed_at"`
	Token       json.RawMessage `json:"token"` // backend-internal credential
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

	// Claim acquires an exclusive lease on the item. force overrides backend-side
	// safety refusals (e.g. the tracker backend refuses a claim onto an arena that
	// still holds unsaved work); backends without such a check ignore it.
	Claim(ctx context.Context, ref ItemRef, owner string, force bool) (Claim, error)

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
	ResolveArtifact(ctx context.Context, claim Claim, id ArtifactId, body ArtifactBody) error
	MarkStale(ctx context.Context, claim Claim, id ArtifactId) error

	// Budget counters — transactional with the artifact record.
	BumpInvocations(ctx context.Context, claim Claim, key string) error
	BumpPrompts(ctx context.Context, claim Claim, key string) error
	AddCost(ctx context.Context, claim Claim, key string, usd float64) error

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

	Commit(ctx context.Context, msg string) error
	Push(ctx context.Context) error

	// Validate runs the project's verify command in the worktree. Returns
	// nil iff verify exits 0.
	Validate(ctx context.Context) error

	// CapturePatch produces a unified diff of the current working tree.
	// Handlers call it to attach work they have already verified; the
	// orchestrator does NOT call it when a step's deadline fires — a timeout
	// park carries no verify-green signal, and a step that commits before a
	// long verify has an empty diff to capture anyway.
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
