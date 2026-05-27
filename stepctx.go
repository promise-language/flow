package flow

import (
	"context"
	"encoding/json"
)

// StepCtx is the per-invocation handle a step handler receives. The same
// concrete type backs all kinds of steps; calling the wrong Resolve* for a
// step's declared result returns ErrTypeMismatch or ErrSignalNotWritable.
type StepCtx interface {
	Context() context.Context
	Flow() string
	StepName() string
	Result() ArtifactId // result id (artifact OR signal as string); see kind via Item
	Item() Item

	// Artifact read surface — full record + typed accessors. ok=false if
	// missing, unresolved, or wrong type.
	Artifact(id ArtifactId) (ArtifactRecord, bool)
	Flag(id ArtifactId) (set bool, ok bool)
	CommitHash(id ArtifactId) (sha string, ok bool)
	Markdown(id ArtifactId) (body string, ok bool)
	JSON(id ArtifactId) (body json.RawMessage, ok bool)
	File(id ArtifactId) (name string, content []byte, ok bool)
	Patch(id ArtifactId) (body PatchBody, ok bool)

	// Signal read surface — handlers cannot write signals.
	Signal(id SignalId) bool

	// Artifact write surface — one Resolve* per ArtifactType.
	ResolveFlag() error
	ResolveCommitHash(sha string) error
	ResolveMarkdown(body string) error
	ResolveJSON(body json.RawMessage) error
	ResolveFile(name string, content []byte) error
	ResolvePatch(body PatchBody) error

	// Sentinel returns — wrap typed errors the SDK translates to InvocationResult.
	Skip(reason string) error
	MarkStale(id ArtifactId) error
	Park(req ParkRequest) error

	// AskQuestions surfaces one or more questions for the user. The handler
	// returns the sentinel error AskQuestions emits; the SDK forwards the
	// questions to Backend.AskQuestions, which assigns ids and persists them
	// on the item, then parks the flow until at least one is answered.
	// Variadic so single-question and multi-question call sites both read
	// naturally: ctx.AskQuestions(q1) vs ctx.AskQuestions(q1, q2, q3).
	AskQuestions(qs ...AgentQuestion) error

	// Agent returns the SDK-metered agent. The ONLY spend chokepoint.
	Agent() Agent

	// Worktree lazily acquires (and caches for the invocation) the Backend's
	// Worktree for this claim. Handlers call methods directly on the
	// returned Worktree.
	Worktree() (Worktree, error)

	RefreshItem() error
}
