package flow

import (
	"encoding/json"
	"strings"
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

// ArtifactDef — one entry in a backend's canonical artifact schema
// (Backend.SupportedArtifacts) and in an App's declared set (App.Artifacts).
// The (Id, Type) pair is the stable identity multiple flows — even from
// different projects — coordinate on; Doc explains the artifact's purpose so a
// reader (human or agent) understands it beyond the name and type.
type ArtifactDef struct {
	Id   ArtifactId
	Type ArtifactType
	// Doc is a one-line, AI-understandable description of what this artifact
	// holds and when it is produced. Authoritative on the backend's
	// SupportedArtifacts entries; App.Artifacts references may leave it empty.
	Doc string
}

// Artifact returns an ArtifactDef. Convenience constructor so call sites read
// as flow.Artifact("plan", flow.ArtifactMarkdown). Attach a description with
// .WithDoc(...) when declaring a backend's canonical schema.
func Artifact(id ArtifactId, t ArtifactType) ArtifactDef {
	return ArtifactDef{Id: id, Type: t}
}

// WithDoc returns a copy of the def with its Doc set — a one-line description
// of the artifact's purpose. Backends document their canonical schema with it:
// flow.Artifact("plan", flow.ArtifactMarkdown).WithDoc("Implementation plan.").
func (d ArtifactDef) WithDoc(doc string) ArtifactDef {
	d.Doc = doc
	return d
}

// ArtifactSpec — what the seed phase records for each artifact: cap values
// pre-loaded from StepConfig (or defaults), required flag, type.
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

	// Budget caps — pre-loaded at SeedState from the flow's StepConfig
	// values (or package defaults). User grants ADD to these directly.
	GrantedInvocations          int
	GrantedPromptsPerInvocation int
	GrantedCostUSD              float64
	GrantedTimeout              time.Duration

	// Usage counters.
	Invocations           int
	PromptsThisInvocation int
	CostUSDSpent          float64
	DurationWorked        time.Duration
	LastRunAt             time.Time
}

// GateName identifies a gate, as a declared concept and an optional instance:
// "tested", or "tested:wasm".
//
// The CONCEPT is closed. The names below are the whole vocabulary, and a
// project must not call one of them something else or use one for something it
// does not name — a "formatted" that quietly reformats is worse than no gate,
// because a decision gets made on its answer and the answer describes a state
// that no longer exists.
//
// The INSTANCE is the project's. One concept commonly has several separately
// runnable gates under it: a project with a host suite, a wasm suite and a
// stress suite has three things that are all obviously tests and all worth
// asking for individually. Collapsing them into one name would destroy the
// property naming exists to provide — that a step fixing one failing suite
// asks for that suite instead of paying for the whole set — and it would do so
// on exactly the projects whose suites are most expensive.
//
// So the vocabulary is closed where it must be understood by everyone, and
// open where only the project knows how its work divides.
//
// These are the names the FLOW asks for, not the names a project may have. A
// project's gate set is usually larger — a size measurement watched for a
// trend, a release invariant checked on a schedule — and those answer questions
// about the project over time rather than about a change about to land. A gate
// this vocabulary does not name is not a gap in it.
//
// Names describe the concern, not the tool that historically served it: `lint`
// is a C utility from 1978 and `vet` is one language's spelling of the same
// idea, and a reader who knows neither learns nothing from either.
type GateName string

const (
	// GateFormatted — it is written the agreed way.
	GateFormatted GateName = "formatted"
	// GateBuilds — it compiles.
	GateBuilds GateName = "builds"
	// GateChecked — it compiles and is still probably wrong: an unused result,
	// a shadowed name, a conversion that cannot be meant.
	GateChecked GateName = "checked"
	// GateTested — it behaves correctly when run. Usually several instances.
	GateTested GateName = "tested"
	// GateCovered — enough of it is exercised. Produces a measurement compared
	// against a floor.
	GateCovered GateName = "covered"
	// GateIntegration — will the mainline still be green. The composition every
	// decision to propose or to land rests on.
	GateIntegration GateName = "integration"
	// GateFit — the machine is fit to be given work. The only concept whose
	// subject is not the change: it measures the host, before work is given, and
	// a machine that cannot build is not a change that may not land. It is
	// therefore never part of an `integration`. See docs/environment.md.
	GateFit GateName = "fit"
)

// AllGateConcepts returns every declared concept, in declaration order.
// Downstream consumers enumerate it rather than mirroring the set, which is how
// two copies of one vocabulary drift.
func AllGateConcepts() []GateName {
	return []GateName{
		GateFormatted, GateBuilds, GateChecked, GateTested, GateCovered, GateIntegration,
		GateFit,
	}
}

// Concept returns the declared part, without any instance.
func (n GateName) Concept() GateName {
	if i := strings.IndexByte(string(n), ':'); i >= 0 {
		return GateName(n[:i])
	}
	return n
}

// Instance returns the project-supplied part, or "" when the name has none.
func (n GateName) Instance() string {
	if i := strings.IndexByte(string(n), ':'); i >= 0 {
		return string(n[i+1:])
	}
	return ""
}

// Valid reports whether the concept is declared. The instance is the project's
// and is not checked against anything: only the project knows how its work
// divides, which is the entire reason instances exist.
//
// A trailing colon is NOT valid. "tested:" promises an instance and names
// none, which is a typo rather than a request for the whole concept — and
// silently treating it as "tested" would run every suite for someone who meant
// to run one.
func (n GateName) Valid() bool {
	if strings.HasSuffix(string(n), ":") {
		return false
	}
	c := n.Concept()
	for _, k := range AllGateConcepts() {
		if c == k {
			return true
		}
	}
	return false
}
