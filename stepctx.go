package flow

import (
	"context"
	"encoding/json"
)

// StepCtx is the per-invocation handle a step handler receives. The same
// concrete type backs all kinds of steps; an election that does not fit the
// step it was made from is refused at capture (StepResult.Elect).
type StepCtx interface {
	Context() context.Context
	Flow() string
	// Description is the lifecycle item's human description. Display only, and
	// deliberately not called a name: it is never an identity, nothing accepts
	// it where a StepId belongs, and granting, parking and the journal all key
	// on Result() (docs/step-handler.md § Identity).
	Description() string
	Result() ArtifactId // result id (artifact OR signal as string); see kind via Item
	Item() Item

	// Who is here — docs/step-handler.md § Who is here.

	// Runner is the account this resolution acts as.
	Runner() AccountId
	// Role is the declared role this dispatch runs under — always the step's
	// own role tag.
	Role() RoleName
	// RoleAccount is the account of record for a role on this item, derived
	// from the journal — empty when the role has not yet acted.
	//
	// The role must be one the flow declares: an undeclared name is refused
	// with ErrUnknownRole, never answered empty. Empty means "declared and not
	// yet acted", and a typo read as that would wait forever.
	RoleAccount(role RoleName) (AccountId, error)

	// Reading the journal — docs/step-handler.md § Reading the journal.

	// Journal returns the item's journal whole: every completed step execution,
	// in order. A copy — a handler reads the route, it does not edit it.
	Journal() []JournalEntry
	// Transfer is the entry that routed here: the predecessor and its message,
	// which is why this step is being run. Nil on an empty journal.
	Transfer() *JournalEntry
	// Notes returns every entry carrying a standing note, in journal order —
	// the notes addressed past their authors.
	Notes() []JournalEntry
	// RunNumber is which dispatch of the pending step this is — 1 on the first,
	// counting each resume and retry since the route named it. Several
	// dispatches may serve the one execution that eventually completes.
	RunNumber() int

	// Artifact read surface — full record + typed accessors. ok=false if
	// missing, unresolved, or wrong type.
	//
	// These read the ArtifactRecord, not the artifact's latest JOURNAL ENTRY as
	// docs/step-handler.md § Typed accessors has them. They cannot read the
	// journal until something appends to it: completion appends (#239) and the
	// entries persist (#240), and until then every accessor would answer
	// "absent" for an artifact that is recorded. The record is the same value
	// by a different route in the meantime, so this is one derivation early,
	// not two derivations at once.
	Artifact(id ArtifactId) (ArtifactRecord, bool)
	Flag(id ArtifactId) (set bool, ok bool)
	CommitHash(id ArtifactId) (sha string, ok bool)
	Markdown(id ArtifactId) (body string, ok bool)
	JSON(id ArtifactId) (body json.RawMessage, ok bool)
	File(id ArtifactId) (name string, content []byte, ok bool)
	Patch(id ArtifactId) (body PatchBody, ok bool)

	// Signal read surface — handlers cannot write signals.
	Signal(id SignalId) bool

	// ParkedOn returns the park record this invocation is resuming from, or
	// nil when the item was not parked.
	//
	// It reads the state the orchestrator already loaded for this dispatch. A
	// handler that needs it — to pick up the answers to a question it asked
	// last time, say — would otherwise have to re-load the whole item just to
	// see a field that was already in memory.
	ParkedOn() *ParkRequest

	// WorkInProgress returns what THIS step stashed against THIS item on an
	// earlier invocation, or "" when there is none — including when the backend
	// has no store at all.
	//
	// Loaded lazily and memoised: a step that never asks pays nothing, which is
	// what makes the record optional. A step that does ask resumes from its own
	// reasoning instead of re-deriving it, which is the whole point — the work
	// that produced a question is exactly the work a park would otherwise throw
	// away.
	WorkInProgress() (string, error)

	// RecordWorkInProgress stashes work this step wants its next invocation to
	// start from. It does NOT resolve the step — it is scaffolding, not a
	// result — is read by no other step, is never published, and is cleared
	// automatically when the step completes.
	//
	// Returns ErrWorkInProgressUnsupported when the backend has no store.
	RecordWorkInProgress(body string) error

	// Electing the route — docs/step-handler.md § Electing the route. The two
	// constructors are the only way a handler completes: it returns one, and
	// the SDK captures the result and records the route together.
	//
	// Either may be extended with .WithNote and, on an artifact step, with the
	// payload constructor matching the declared type — see StepResult.

	// Next elects a declared successor, with the message telling it why it is
	// being run.
	Next(step StepId, message string) StepResult
	// Finalize ends the item with a disposition the step declares in
	// StepConfig.MayFinalize, with the closing reasons as the message.
	Finalize(d Disposition, message string) StepResult

	// Sentinel returns — wrap typed errors the SDK translates to InvocationResult.

	// Park stops the item on the given request. Never ParkQuestion: a question
	// park is answerable only through a question the orchestrator registered,
	// this route registers none, and `answer` has no QuestionId to record
	// against — so a question kind here fails the step, naming AskQuestions. A
	// decision needed asks.
	Park(req ParkRequest) error

	// AskQuestions surfaces one or more questions for the user. The call
	// persists questions via Backend.AskQuestions and returns the ErrQuestion
	// sentinel carrying the backend's response (ids and timestamps). The
	// orchestrator parks the flow until at least one is answered. A
	// disclosure refusal is returned as ErrDisclosureRefused so the caller
	// can revise and re-offer. Variadic so single-question and
	// multi-question call sites both read naturally:
	// ctx.AskQuestions(q1) vs ctx.AskQuestions(q1, q2, q3).
	AskQuestions(qs ...AgentQuestion) error

	// WaitOnItems records each ref as a blocker on the item — through the
	// orchestrator's editor, one edit per ref — and returns the
	// ErrWaitsOnItems sentinel. The invocation reports blocked, kind
	// waits-on-items, naming the blockers; nothing is parked, nothing is
	// charged, and the step's artifact stays unresolved. It is how a step says
	// "the work exists elsewhere and will land" without spending a refusal on
	// it: a refusal is a park a person must clear, and this clears itself when
	// the last blocker finishes (docs/resolution.md § Blocked on items).
	//
	// At least one ref is required. A ref the orchestrator refuses to record —
	// unresolvable, a cycle, the item itself — returns as an ordinary error
	// naming it, so the step fails visibly instead of stopping on a blocker
	// nothing recorded; refs recorded before it stay recorded.
	WaitOnItems(refs ...ItemRef) error

	// Notify reports a sub-phase progress event ("running verify round 2",
	// "capturing patch"). Forwarded to App.Telemetry.StepProgress when one
	// is configured; otherwise a no-op. step defaults to the current
	// lifecycle item name when empty.
	//
	// NOT a liveness signal — see flow.Telemetry's docstring. The SDK and
	// downstream consumers MUST NOT derive "is this step still alive?"
	// from Notify call density.
	Notify(step, detail string)

	// Agent returns the SDK-metered agent. The ONLY spend chokepoint.
	Agent() Agent

	// Worktree lazily acquires (and caches for the invocation) the Backend's
	// Worktree for this claim. Handlers call methods directly on the
	// returned Worktree.
	Worktree() (Worktree, error)

	// Claim returns the active claim that scoped this StepCtx. Handlers
	// that call backend-specific helpers (typically on a concrete
	// *Backend type captured via a closure in main) pass this through
	// to methods that take a flow.Claim. Treat as read-only — the claim
	// is owned by cli.RunOne for the duration of the invocation.
	Claim() Claim

	// VerifyCmd returns the project verify command configured on the App
	// (App.VerifyCmd), or "" if none was set. Handlers use it both to run the
	// verify gate and to populate prompt context, so the command is defined in
	// exactly one place. See the App.VerifyCmd docstring.
	VerifyCmd() string

	RefreshItem() error
}
