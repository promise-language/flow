package flow

import (
	"encoding/json"
	"time"
)

// ItemType discriminates items the backend exposes; flows declare which types
// they handle via NewFlow.
type ItemType string

// ArtifactId names a handler-produced artifact. The string is opaque to the
// SDK; uniqueness is enforced within App.Artifacts.
type ArtifactId string

// ArtifactType is the closed set of value shapes an artifact can carry.
// Ordered from most primitive (no payload) to most structured. Adding a value
// is an SDK-version change.
type ArtifactType int

const (
	ArtifactFlag       ArtifactType = iota + 1 // "happened" marker — no payload
	ArtifactCommitHash                         // 40-char git SHA
	ArtifactMarkdown                           // text/markdown body
	ArtifactJSON                               // arbitrary JSON (json.RawMessage on the wire)
	ArtifactFile                               // named bytes
	ArtifactPatch                              // unified diff with structured metadata
)

func (t ArtifactType) String() string {
	switch t {
	case ArtifactFlag:
		return "flag"
	case ArtifactCommitHash:
		return "commit-hash"
	case ArtifactMarkdown:
		return "markdown"
	case ArtifactJSON:
		return "json"
	case ArtifactFile:
		return "file"
	case ArtifactPatch:
		return "patch"
	}
	return "unknown"
}

// FileBody — named bytes, used when ArtifactType == ArtifactFile.
type FileBody struct {
	Name    string
	Content []byte
}

// PatchBody — used when ArtifactType == ArtifactPatch.
type PatchBody struct {
	Diff       []byte // unified diff bytes
	BaseSHA    string // base commit the diff applies against
	BaseBranch string
	RepoURL    string   // origin URL for restoration context
	Untracked  []string // names of untracked files (content NOT embedded)
}

// ArtifactDef — centralized declaration in App.Artifacts. Referenced by id
// from any number of flows; type is resolved from here at validation time.
type ArtifactDef struct {
	Id   ArtifactId
	Type ArtifactType
}

// Artifact returns an ArtifactDef. Convenience constructor so call sites read
// as flow.Artifact("plan", flow.ArtifactMarkdown).
func Artifact(id ArtifactId, t ArtifactType) ArtifactDef {
	return ArtifactDef{Id: id, Type: t}
}

// ArtifactSpec — what the seed phase records for each artifact: cap values
// pre-loaded from StepOption (or defaults), required flag, type.
type ArtifactSpec struct {
	Id       ArtifactId
	Type     ArtifactType
	Required bool
	Budget   StepBudget
}

// ArtifactBody — the union written by Backend.ResolveArtifact. Exactly one
// field is populated; which one is determined by the matching ArtifactType.
type ArtifactBody struct {
	Type ArtifactType

	CommitHash string
	Markdown   string
	JSON       json.RawMessage
	File       FileBody
	Patch      PatchBody
}

// ArtifactRecord — what LoadState returns per artifact. Carries the resolved
// value (if any) alongside budget caps and usage counters.
type ArtifactRecord struct {
	Id       ArtifactId
	Type     ArtifactType
	Required bool
	Stale    bool
	Resolved bool

	// Value — exactly one populated when Resolved, matching Type.
	// (Flag has no payload — Resolved && Type==ArtifactFlag is the signal.)
	CommitHash string
	Markdown   string
	JSON       json.RawMessage
	File       FileBody
	Patch      PatchBody

	ProducedAt time.Time
	Version    int
	ResolvedBy string

	// Budget caps — pre-loaded at SeedState from the flow's StepOption
	// values (or package defaults). User grants ADD to these directly.
	GrantedInvocations          int
	GrantedPromptsPerInvocation int
	GrantedCostUSD              float64
	GrantedTimeout              time.Duration

	// Usage counters.
	Invocations           int
	PromptsThisInvocation int
	CostUSDSpent          float64
	LastRunAt             time.Time
}
