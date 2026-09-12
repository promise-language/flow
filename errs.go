package flow

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsupported and ErrUnavailable are the two refusals an orchestrator has.
//
// Required does not mean always possible. Every method on the Orchestrator
// interface must exist and must answer; none of them must succeed. What being
// required forbids is SILENCE — an absent method leaves a caller nothing to
// call and no way to ask why, and a method that quietly succeeds while doing
// nothing gives a false answer, which is worse than either.
//
// The pair is two answers and not one because a caller acts differently on
// each: the first is permanent and the second is worth retrying. Collapsing
// them turns a missing capability into a retry loop that never terminates, and
// a rate limit into a permanent verdict.
var (
	// ErrUnsupported — this orchestrator cannot do this AT ALL. Never here, no
	// configuration changes it, and retrying is pointless. Returned by
	// flow.Open / flow.Merge / flow.FindPR when Worktree.Request() is nil, and
	// by any method whose capability the orchestrator simply lacks.
	ErrUnsupported = errors.New("flow: orchestrator does not support this operation")

	// ErrUnavailable — this orchestrator cannot do it RIGHT NOW. A service is
	// down, a lease is held elsewhere, a rate limit is in force. Retrying is
	// what a caller should do.
	ErrUnavailable = errors.New("flow: orchestrator cannot do this right now")
)

// ErrTransient — handler-returned sentinel for infrastructure failures
// observed by the handler (e.g. a Worktree HTTP call against a remote
// runner that's offline). The orchestrator translates this to:
//
//	InvocationResult{Status: "parked", Park: {Kind: ParkInfraTransient, ...}}
//
// and does not count the dispatch, so a flapping runner does not burn
// the step's invocation budget. Wrap a concrete cause with fmt.Errorf
// using %w so callers can errors.Is against ErrTransient.
//
// Agent-side transient failures (the dominant case — runner died mid-turn)
// are surfaced differently: the Agent impl sets AgentResponse.Failure.Transient
// = true, and the orchestrator applies the same skip-bump + park policy
// without needing a handler-level sentinel.
var ErrTransient = errors.New("flow: transient infrastructure failure")

// ErrRefused — handler-returned sentinel for deterministic failures that
// provably cannot change on re-run: a repository guard refused a staged file,
// a required tool reports itself out of date, or a precondition on the
// environment is unmet. The orchestrator translates this to:
//
//	InvocationResult{Status: "parked", Park: {Kind: ParkRefused, ...}}
//
// and does not count the dispatch, so a deterministic refusal does not
// burn the step's invocation budget. The park reason is the refusal's own
// message, so the operator sees what was refused rather than a generic
// "budget exhausted" after the budget drains on identical no-op retries.
//
// Symmetric with ErrTransient: one sentinel for "retry is free" (transient),
// one for "retry is pointless" (refused), and the default in between stays
// as it is (retry and pay).
var ErrRefused = errors.New("flow: deterministic refusal")

// ErrUnfit — handler-returned sentinel for a machine that is not fit to
// perform work. The orchestrator translates this to:
//
//	InvocationResult{Status: "blocked", ...}
//
// and does not count the dispatch: a full disk is a condition, not a
// failure, and a condition that ends on its own must not consume budget.
// No park is written — a park names a step and persists until cleared, but
// a machine condition travels with nobody and ends the moment the machine
// recovers. The claim is kept (the machine is unfit, not the item).
var ErrUnfit = errors.New("flow: machine unfit")

// ErrNoDisclosureGuard — no DisclosureGuard was injected, so nothing is
// published. docs/disclosure.md: "an interface that defaults to allow is an
// interface whose whole purpose is optional." Reads are unaffected; the first
// write refuses.
//
// It is the Reason inside an ErrDisclosureRefused, so callers reach it with
// errors.Is.
var ErrNoDisclosureGuard = errors.New("flow: no disclosure guard is installed; nothing is published")

// ErrDisclosureRefused — a proposed outward write did not happen. Act names
// what was refused; Reason is the guard's own answer, carried unchanged so the
// author can see what was caught.
//
// Typed rather than prose because a refusal has to be recognisable without
// matching on a message, and because it must never be mistaken for
// ErrTransient: a transient failure is retried, and retrying a refusal would
// re-propose exactly the text that was refused.
type ErrDisclosureRefused struct {
	Act    DisclosureAct
	Reason error
}

func (e ErrDisclosureRefused) Error() string {
	return fmt.Sprintf("disclosure refused (%s): %v", e.Act, e.Reason)
}

func (e ErrDisclosureRefused) Unwrap() error { return e.Reason }

// ErrPark — handler raised a structured park request. The SDK forwards
// req to Backend.Park — with one exception: Kind ParkQuestion fails the step
// instead of parking. This route registers no question, so the park it would
// write is one `answer` has no id to name. A decision needed asks
// (StepCtx.AskQuestions), which records the question and parks on it.
//
// The rule is stated here, not only on StepCtx.Park, because this type is
// exported: a handler can return it directly without going through ctx.Park,
// and the SDK guards both doors alike.
type ErrPark struct {
	Req ParkRequest
}

func (e ErrPark) Error() string {
	if e.Req.Reason != "" {
		return fmt.Sprintf("park[%s]: %s", e.Req.Kind, e.Req.Reason)
	}
	return fmt.Sprintf("park[%s]", e.Req.Kind)
}

// ErrQuestion — handler emitted one or more questions for the user; flow
// parks until at least one is answered. The call to Backend.AskQuestions
// happens inside stepCtx.AskQuestions, so by the time the orchestrator
// sees this sentinel the questions have already been persisted. Recorded
// carries the backend's response (ids and timestamps).
type ErrQuestion struct {
	Questions []AgentQuestion
	Recorded  []Question
}

func (e ErrQuestion) Error() string {
	if len(e.Questions) == 0 {
		return "question: (empty)"
	}
	if len(e.Questions) == 1 {
		return "question: " + e.Questions[0].Text
	}
	return fmt.Sprintf("questions: %d pending (first: %s)", len(e.Questions), e.Questions[0].Text)
}

// ErrWaitsOnItems — handler-returned sentinel from ctx.WaitOnItems: the step's
// work waits on the items named, which the call has already recorded as
// blockers on the item. The orchestrator reloads the item and translates this
// to:
//
//	InvocationResult{Status: "blocked", BlockKind: WaitsOnItems, BlockedBy: ...}
//
// and does not count the dispatch: the work exists elsewhere and will land,
// and nobody touches this item until it does — a dispatch charged for finding
// that out would spend the budget on a wait.
//
// It is neither a park nor a refusal. A park names a step and a person clears
// it; a refusal says no change will help. This stop clears itself when the last
// blocker goes terminal, with nobody acting on this item at all — the same stop
// the advance makes before dispatch when it finds the item blocked, reported
// the same way (docs/resolution.md § Blocked on items).
type ErrWaitsOnItems struct {
	Refs []ItemRef
}

func (e ErrWaitsOnItems) Error() string {
	if len(e.Refs) == 0 {
		return "waits on items: (none)"
	}
	names := make([]string, 0, len(e.Refs))
	for _, r := range e.Refs {
		names = append(names, r.Display)
	}
	return "waits on items: " + strings.Join(names, ", ")
}

// ErrTypeMismatch — the payload a handler returned is not of the step's
// declared artifact type (e.g. .Markdown(…) on a step declared ArtifactPatch).
// Refused at capture: nothing is journaled and nothing is published.
type ErrTypeMismatch struct {
	Step     string
	Expected ArtifactType
	Got      ArtifactType
}

func (e ErrTypeMismatch) Error() string {
	return fmt.Sprintf("step %q: expected %s artifact, got %s", e.Step, e.Expected, e.Got)
}

// ErrSignalNotWritable — the StepResult of an AddSignalStep or AwaitSignal
// lifecycle item carried a payload. Signals are never handler-writable: the
// orchestrator observes them, so a signal step's election carries no result.
type ErrSignalNotWritable struct {
	Step   string
	Signal SignalId
}

func (e ErrSignalNotWritable) Error() string {
	return fmt.Sprintf("step %q produces signal %q; signals are not handler-writable", e.Step, e.Signal)
}

// ErrStepDidNotComplete — the handler returned without completing: it elected
// no route at all, or it elected one and produced no payload for the artifact
// it owes. Both are the same failure — the step said it was done and did not do
// its job (docs/step-handler.md § Error semantics).
//
// Step is the lifecycle item's description, Result its result id: a description
// alone is display text, and an id alone is not what the reader wrote down.
type ErrStepDidNotComplete struct {
	Step   string
	Result string
}

func (e ErrStepDidNotComplete) Error() string {
	return fmt.Sprintf("step %q completed without producing %q", e.Step, e.Result)
}

// ErrRouteNotDeclared — the handler elected a route the step does not declare:
// a successor outside StepConfig.Next, or a finalization outside
// StepConfig.MayFinalize.
//
// It names both sides. What was elected alone leaves the reader to go and find
// the registration, and what was declared alone does not say what the handler
// tried to do — and the fix is always one or the other: either the handler
// elects a declared route, or the registration declares the route the handler
// needs.
type ErrRouteNotDeclared struct {
	Step        string
	Elected     Route
	Next        []StepId
	MayFinalize []Disposition
}

func (e ErrRouteNotDeclared) Error() string {
	if e.Elected.Finalizes() {
		return fmt.Sprintf("step %q elected finalization %q, which it does not declare; it may finalize as %v",
			e.Step, e.Elected.Finalize, e.MayFinalize)
	}
	return fmt.Sprintf("step %q elected successor %q, which it does not declare; its declared successors are %v",
		e.Step, e.Elected.Next, e.Next)
}

// ErrUnknownRole — a lookup named a role the flow does not declare.
//
// Refused loudly rather than answered empty, because the two readings are
// indistinguishable to the caller and only one of them ever clears: empty means
// "declared, and has not acted yet", which a handler waits on. A typo read that
// way waits forever (docs/resolution.md § Whose move it is: "A reference that
// names no declaration is refused loudly, never left to match nothing").
//
// Declared names the alternatives when the raiser can enumerate them, and is
// empty when it cannot — the message says which.
type ErrUnknownRole struct {
	Role     RoleName
	Declared []RoleName
}

func (e ErrUnknownRole) Error() string {
	if len(e.Declared) == 0 {
		return fmt.Sprintf("role %q is not declared by this flow", e.Role)
	}
	return fmt.Sprintf("role %q is not declared by this flow; declared roles are %v", e.Role, e.Declared)
}

// ErrBudgetExhausted — pre-dispatch or in-handler budget check refused the
// call.
type ErrBudgetExhausted struct {
	Step string
	Axis BudgetAxis
	Cap  string // human-readable cap value
}

func (e ErrBudgetExhausted) Error() string {
	return fmt.Sprintf("step %q: budget exhausted on axis %s (cap %s)", e.Step, e.Axis, e.Cap)
}

// ClaimOverride names a safety check the operator chose to bypass. The values
// are flow's own vocabulary — CLI flag names — not the backend's.
type ClaimOverride string

const (
	OverrideDirtyTree   ClaimOverride = "dirty-tree"
	OverrideAlreadyHeld ClaimOverride = "already-held"
	OverrideUnadmitted  ClaimOverride = "unadmitted"
	OverrideStaleBase   ClaimOverride = "stale-base"
)

// ClaimRefusalCode is the refusal vocabulary, carried through verbatim. flow
// deliberately defines NO constants for it: the vocabulary belongs to the
// backend's protocol (for the tracker backend, workspace/wire), which flow
// cannot import. Mirroring it here would be a second enum nothing keeps in
// sync — including for the one code the SDK itself raises, which is named in
// ErrClaimRefused below and spelled at the single place that raises it.
type ClaimRefusalCode string

// ErrClaimRefused — a claim cannot proceed. Raised by the orchestrator's own
// Claim for its own preconditions, and by the SDK for the one refusal it owns:
// an item awaiting a role the runner cannot assume, code `awaits-other-role`
// (docs/resolution.md § Claiming). Which roles exist and who may assume one are
// the SDK's derivations, and no orchestrator is told about them
// (docs/orchestrator.md § Identities).
//
// Every other code is the refusing backend's own vocabulary (opaque to flow);
// ItemScoped reports
// whether a DIFFERENT item might succeed (true → retry the next ref in an
// auto-select loop; false → stop). ItemScoped MUST default to false for any
// code the backend does not recognize: false means "stop", and stopping on an
// unknown refusal is the safe direction.
type ErrClaimRefused struct {
	Code       ClaimRefusalCode // opaque: display, logs, --json passthrough
	ItemScoped bool             // true → a different item might succeed
	Reason     string           // one-line, human
	Detail     string           // verbatim check stderr, printed unmodified
	Check      string           // failing check name, when the backend has one
	Override   string           // flag that bypasses it; empty = not overridable
}

func (e ErrClaimRefused) Error() string {
	msg := "claim refused: " + e.Reason
	if e.Check != "" {
		msg += fmt.Sprintf(" (check %q)", e.Check)
	}
	return msg
}
