package flow

import "time"

// ParkKind discriminates the reason an item was parked.
type ParkKind string

const (
	ParkBlocked           ParkKind = "blocked"
	ParkQuestion          ParkKind = "question"
	ParkBudgetExhausted   ParkKind = "budget-exhausted"
	ParkStepDidNotResolve ParkKind = "step-did-not-resolve"
	// ParkInfraTransient — the step's failure was observed-infra (remote
	// runner offline, transient 5xx, network timeout). The orchestrator
	// parks the step WITHOUT consuming an invocation, so the
	// re-dispatch path retries the same item against a healthy runner
	// without burning budget.
	ParkInfraTransient ParkKind = "infra-transient"
)

// BudgetAxis identifies which budget axis was exhausted (when
// ParkKind==ParkBudgetExhausted).
type BudgetAxis string

const (
	AxisInvocations BudgetAxis = "invocations"
	AxisPrompts     BudgetAxis = "prompts"
	AxisCost        BudgetAxis = "cost"
	AxisTimeout     BudgetAxis = "timeout"
)

// ParkRequest is what handlers (via ctx.Park) or the SDK pass to
// Backend.Park to mark a step blocked / waiting on input / exhausted.
type ParkRequest struct {
	Kind    ParkKind   `json:"kind"`
	Step    string     `json:"step,omitempty"` // lifecycle item name
	Axis    BudgetAxis `json:"axis,omitempty"` // set when Kind==ParkBudgetExhausted
	Reason  string     `json:"reason,omitempty"`
	Details string     `json:"details,omitempty"`
}

// QuestionFormat is the presentation hint an agent attaches to a question:
// the UI may render text input, a yes/no toggle, or a choice list, but the
// user can always reply with free text ("Other") regardless of format.
type QuestionFormat string

const (
	FormatText   QuestionFormat = "text"
	FormatYesNo  QuestionFormat = "yes_no"
	FormatChoice QuestionFormat = "choice"
)

// AgentQuestion is the question payload an agent or step handler emits via
// ctx.AskQuestions. Header is a short chip/label; Text is the full prompt.
// Options + MultiSelect apply when Format == FormatChoice; they are
// presentation hints only and never constrain the answer.
type AgentQuestion struct {
	Text        string         `json:"text"`
	Header      string         `json:"header,omitempty"`
	Format      QuestionFormat `json:"format,omitempty"`
	Options     []string       `json:"options,omitempty"`
	MultiSelect bool           `json:"multi_select,omitempty"`
}

// UserAnswer is the user's response to an AgentQuestion. Answer is free-form
// — a chosen option string or "Other" free text, even for choice questions.
type UserAnswer struct {
	Answer     string     `json:"answer,omitempty"`
	AnsweredAt *time.Time `json:"answered_at,omitempty"`
}

// Question is a recorded ask_user_question entry on an item: a backend-
// assigned id plus the AgentQuestion that was asked and, once answered, the
// UserAnswer. The embedded structs flatten into one JSON object.
type Question struct {
	ID string `json:"id"`
	AgentQuestion
	UserAnswer
}

// AskText is a convenience constructor for a free-text question.
func AskText(header, text string) AgentQuestion {
	return AgentQuestion{Header: header, Text: text, Format: FormatText}
}

// AskYesNo is a convenience constructor for a yes/no question.
func AskYesNo(header, text string) AgentQuestion {
	return AgentQuestion{Header: header, Text: text, Format: FormatYesNo}
}

// AskChoice is a convenience constructor for a single-select choice question.
func AskChoice(header, text string, options ...string) AgentQuestion {
	return AgentQuestion{Header: header, Text: text, Format: FormatChoice, Options: options}
}

// AskMultiChoice is a convenience constructor for a multi-select choice question.
func AskMultiChoice(header, text string, options ...string) AgentQuestion {
	return AgentQuestion{Header: header, Text: text, Format: FormatChoice, Options: options, MultiSelect: true}
}

// Grant adds budget to an artifact's existing Granted* caps. Zero values mean
// "no change on this axis" — Grant is additive, not a replacement.
type Grant struct {
	Invocations          int
	PromptsPerInvocation int
	CostUSD              float64
	TimeoutAdd           int64 // additional seconds; stored as int64 for JSON friendliness
}

// InvocationResult is the stdout summary one ./binary run invocation prints.
type InvocationResult struct {
	Flow         string       `json:"flow"`
	InvocationID string       `json:"invocation_id"`
	Item         string       `json:"item"`
	Step         string       `json:"step"`
	Status       string       `json:"status"` // done | skipped | failed | parked
	Reason       string       `json:"reason,omitempty"`
	Park         *ParkRequest `json:"park,omitempty"`
}
