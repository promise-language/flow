package flow

import (
	"context"
	"encoding/json"
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
}

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

	// ListEligible returns candidate items in the backend's scope.
	ListEligible(ctx context.Context) ([]ItemRef, error)

	// Claim acquires an exclusive lease on the item.
	Claim(ctx context.Context, ref ItemRef, owner string) (Claim, error)

	// Release relinquishes the lease.
	Release(ctx context.Context, claim Claim) error

	// LookupClaim returns read-only info about the current claim, or
	// (nil, nil) if unclaimed.
	LookupClaim(ctx context.Context, ref ItemRef) (*ClaimInfo, error)

	// LoadState returns artifacts + signals in one snapshot, with signals
	// refreshed by backend-internal polling.
	LoadState(ctx context.Context, claim Claim) (*ItemState, error)

	// SeedState pre-loads the artifact set + budget caps. The backend MUST
	// refuse a second seed for the same item; mid-flight items are frozen
	// against later flow-source changes.
	SeedState(ctx context.Context, claim Claim, artifacts []ArtifactSpec) error

	// ResolveArtifact writes a handler-produced artifact value. No backend
	// method for writing signals; signals are written by backend-internal
	// side effects (worktree actions) or the LoadState poll path.
	ResolveArtifact(ctx context.Context, claim Claim, id ArtifactId, body ArtifactBody) error
	MarkStale(ctx context.Context, claim Claim, id ArtifactId) error

	// Budget counters — transactional with the artifact record.
	BumpInvocations(ctx context.Context, claim Claim, key string) error
	BumpPrompts(ctx context.Context, claim Claim, key string) error
	AddCost(ctx context.Context, claim Claim, key string, usd float64) error
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

// Worktree is the local-git + remote-PR surface the SDK exposes to handlers
// via ctx.Worktree(). Branch/Commit/Push/OpenPR/MergePR/Validate are all
// idempotent where they reasonably can be.
type Worktree interface {
	// Branch ensures `name` is checked out. Creates off base (or HEAD if
	// base==""). Idempotent: no-op if already current. Errors on dirty
	// tree. Returns true iff newly created.
	Branch(ctx context.Context, name string, base string) (created bool, err error)
	CurrentBranch(ctx context.Context) (string, error)

	Commit(ctx context.Context, msg string) error
	Push(ctx context.Context) error

	// OpenPR / MergePR may trigger backend-internal side effects that set
	// signals (pr-open / pr-merged on github).
	OpenPR(ctx context.Context, base, title, body string) (url string, err error)
	MergePR(ctx context.Context, url string) error

	// Validate runs the project's verify command in the worktree. Returns
	// nil iff verify exits 0.
	Validate(ctx context.Context) error

	// CapturePatch produces a unified diff of the current working tree for
	// timeout / park retention.
	CapturePatch(ctx context.Context) (patch []byte, err error)
}
