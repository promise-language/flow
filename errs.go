package flow

import "fmt"

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
