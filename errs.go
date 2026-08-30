package flow

import (
	"errors"
	"fmt"
)

// ErrRequestNotSupported — returned by flow.Open / flow.Merge when the
// backend's Worktree.Request() returns nil. Handlers that prefer a typed
// error over a panic on misuse call those nil-safe helpers; this is the
// sentinel they should errors.Is against.
var ErrRequestNotSupported = errors.New("flow: backend does not support pull request operations")

// ErrResetSeedUnsupported — returned by Backend.ResetSeed when the backend
// has no separable seed concept (e.g. a read-only backend). Callers
// invoking ResetSeed should errors.Is against this to distinguish
// "intentionally not supported" from a transient failure.
var ErrResetSeedUnsupported = errors.New("flow: backend does not support ResetSeed")

// ErrWorkInProgressUnsupported — the backend has no work-in-progress store, so
// there is nowhere for a step to leave what it worked out. Reads report absence
// instead; only a WRITE says so, because a caller that believed it stashed
// something and did not is the case worth naming.
var ErrWorkInProgressUnsupported = errors.New("flow: backend does not support work in progress")

// ErrTransient — handler-returned sentinel for infrastructure failures
// observed by the handler (e.g. a Worktree HTTP call against a remote
// runner that's offline). The orchestrator translates this to:
//
//	InvocationResult{Status: "parked", Park: {Kind: ParkInfraTransient, ...}}
//
// and SKIPS the BumpInvocations call, so a flapping runner does not burn
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
// and SKIPS the BumpInvocations call, so a deterministic refusal does not
// burn the step's invocation budget. The park reason is the refusal's own
// message, so the operator sees what was refused rather than a generic
// "budget exhausted" after the budget drains on identical no-op retries.
//
// Symmetric with ErrTransient: one sentinel for "retry is free" (transient),
// one for "retry is pointless" (refused), and the default in between stays
// as it is (retry and pay).
var ErrRefused = errors.New("flow: deterministic refusal")

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

// ErrSkip — handler decided no progress is possible right now. The SDK marks
// the invocation skipped (no budget consumed beyond the invocation count).
type ErrSkip struct {
	Reason string
}

func (e ErrSkip) Error() string { return "skip: " + e.Reason }

// ErrPark — handler raised a structured park request. The SDK forwards
// req to Backend.Park.
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
// parks until at least one is answered. The SDK forwards Questions to
// Backend.AskQuestions, which assigns ids and persists them on the item.
type ErrQuestion struct {
	Questions []AgentQuestion
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

// ErrTypeMismatch — handler called the wrong Resolve* for its declared
// artifact type (e.g. ResolveMarkdown on an ArtifactPatch step).
type ErrTypeMismatch struct {
	Step     string
	Expected ArtifactType
	Got      ArtifactType
}

func (e ErrTypeMismatch) Error() string {
	return fmt.Sprintf("step %q: expected %s artifact, got %s", e.Step, e.Expected, e.Got)
}

// ErrSignalNotWritable — handler called a Resolve* on an AddSignalStep
// lifecycle item. Signals are never handler-writable.
type ErrSignalNotWritable struct {
	Step   string
	Signal SignalId
}

func (e ErrSignalNotWritable) Error() string {
	return fmt.Sprintf("step %q produces signal %q; signals are not handler-writable", e.Step, e.Signal)
}

// ErrStepDidNotResolve — handler returned nil but never called Resolve*
// (artifact step) or the matching signal never set (signal step).
type ErrStepDidNotResolve struct {
	Step   string
	Result string
}

func (e ErrStepDidNotResolve) Error() string {
	return fmt.Sprintf("step %q returned without resolving %q", e.Step, e.Result)
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
