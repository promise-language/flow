package flow

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// ParkKind discriminates the reason an item was parked.
type ParkKind string

const (
	ParkBlocked  ParkKind = "blocked"
	ParkQuestion ParkKind = "question"
	// ParkTreasurerRefused — the treasurer would not fund the next dispatch on
	// some BudgetAxis. The only kind a Grant can clear, and only when the
	// treasurer accepts the extension as clearing what it refused on
	// (docs/orchestrator.md § Vocabularies).
	ParkTreasurerRefused ParkKind = "treasurer-refused"
	// ParkStepDidNotComplete — the step returned without completing: it elected
	// no route, or elected one and produced no result. A re-dispatch can still
	// do the job, which is why it parks rather than fails.
	ParkStepDidNotComplete ParkKind = "step-did-not-complete"
	// ParkInfraTransient — the step's failure was observed-infra (remote
	// runner offline, transient 5xx, network timeout). The orchestrator
	// parks the step WITHOUT consuming an invocation, so the
	// re-dispatch path retries the same item against a healthy runner
	// without burning budget.
	ParkInfraTransient ParkKind = "infra-transient"
	// ParkRemoteUnreachable — a git remote was unreachable (SSH refused,
	// "Could not read from remote repository", DNS, "No route to host",
	// connection timeout). Distinct from ParkInfraTransient because the
	// tracker's orchestrator pauses dispatch GLOBALLY and runs a probe
	// ticker until the remote is reachable again. Like ParkInfraTransient
	// it consumes no invocation budget; the tracker translates this kind
	// into the typed FlowFailureRemoteUnreachable on the ledger row.
	ParkRemoteUnreachable ParkKind = "remote-unreachable"
	// ParkRefused — the handler determined that the failure is deterministic:
	// re-invoking the identical handler against the identical worktree would
	// return the identical answer. A repository guard refused a staged file,
	// a required tool is out of date, or an environment precondition is unmet.
	//
	// Like ParkInfraTransient the orchestrator records no dispatch — the
	// handler never got a real chance to do its work, so charging it would
	// burn the invocation budget on identical no-op failures and eventually
	// park with ParkTreasurerRefused, which describes the clock rather than
	// the refusal.
	ParkRefused ParkKind = "refused"
	// ParkWriteContract — the step modified the worktree outside its declared
	// contract: it switched branches, committed, or dirtied tracked files
	// when its WriteContract forbade it. The changes are NOT reverted — they
	// may be wanted — and the step is NOT retried (same prompt, same result).
	// A human inspects the evidence and either widens the contract or reverts.
	ParkWriteContract ParkKind = "write-contract"
)

// AllParkKinds returns every ParkKind, in declaration order. Downstream
// crosswalks enumerate it to prove they handle each one.
func AllParkKinds() []ParkKind {
	return []ParkKind{
		ParkBlocked, ParkQuestion, ParkTreasurerRefused,
		ParkStepDidNotComplete, ParkInfraTransient,
		ParkRemoteUnreachable, ParkRefused, ParkWriteContract,
	}
}

// Disposition is how a step ENDS THE FLOW when it elects finalization —
// docs/resolution.md § Finalizing. Closed at two.
//
// Not to be confused with Item.Disposition (orchestrator.go), which is the
// ORCHESTRATOR's own display name for its own status ("done", "won't fix",
// "duplicate"), carried for a person to read and interpreted by nothing. This
// one is a decision the flow makes, declared per step in StepConfig.MayFinalize
// and elected at runtime; that one is a label the orchestrator already had.
type Disposition string

const (
	// DispositionResolved — the work was done.
	DispositionResolved Disposition = "resolved"
	// DispositionRejected — the item was reviewed and declined, with the
	// reasons in the finalizing entry's message. Terminal in a way a handback
	// is not: a route that returns work for rework expects the resolution to
	// continue, and this ends it.
	DispositionRejected Disposition = "rejected"
)

// AllDispositions returns every declared disposition, in declaration order.
// Consumers enumerate it rather than mirroring the set, which is how two copies
// of one vocabulary drift.
func AllDispositions() []Disposition {
	return []Disposition{DispositionResolved, DispositionRejected}
}

// Valid reports whether d is one of the two. The empty disposition is not one:
// a step that finalizes says how, and a step that does not declares no
// disposition at all.
func (d Disposition) Valid() bool { return slices.Contains(AllDispositions(), d) }

// BudgetAxis identifies which budget axis was exhausted (when
// ParkKind==ParkTreasurerRefused).
type BudgetAxis string

const (
	AxisInvocations BudgetAxis = "invocations"
	AxisPrompts     BudgetAxis = "prompts"
	AxisCost        BudgetAxis = "cost"
	AxisTimeout     BudgetAxis = "timeout"
)

// ParkRequest is what handlers (via ctx.Park) or the SDK pass to
// Orchestrator.Park to mark a step blocked / waiting on input / exhausted.
type ParkRequest struct {
	Kind ParkKind `json:"kind"`
	// Step is the StepId — its ArtifactId (or SignalId), NOT the human
	// label passed as AddStep's first argument. The id is what identifies a
	// step everywhere else (it keys the budget record, and it is the only
	// thing `grant` accepts), so a park that named the label could not be
	// matched to the record whose budget caused it. The SDK fills this in
	// from LifecycleItem.Result() when a handler leaves it empty.
	Step StepId     `json:"step,omitempty"`
	Axis BudgetAxis `json:"axis,omitempty"` // set when Kind==ParkTreasurerRefused
	// Axes is the state of EVERY budget axis at park time, not just the one
	// in Axis. Reporting only the tripping axis cost the operator a round-trip
	// per axis: the axes go flat together, so granting the named one bought a
	// dispatch that re-parked on the next. With the full set an operator reads
	// one park and grants once. Set alongside Axis when
	// Kind==ParkTreasurerRefused; empty for every other park kind.
	Axes    []AxisReport `json:"axes,omitempty"`
	Reason  string       `json:"reason,omitempty"`
	Details string       `json:"details,omitempty"`
}

// AxisReport is one budget axis's meter and cap at the moment a step parked.
//
// Used and Granted are carried in the axis's own unit — whole counts for
// invocations and prompts, dollars for cost, seconds for timeout — as float64
// so one type covers all four. Render them with Format, which knows the units.
type AxisReport struct {
	Axis    BudgetAxis `json:"axis"`
	Used    float64    `json:"used"`
	Granted float64    `json:"granted"`
	// Exhausted marks an axis that is at or past its cap. These are the axes
	// a grant must cover: any one of them re-parks the step. An axis with no
	// cap set (Granted == 0) is unmetered, never exhausted.
	Exhausted bool `json:"exhausted"`
}

// Format renders one axis as "used/granted" in its own unit, e.g. "3/3 inv",
// "$11.18/$10.00", "1h30m0s/3h0m0s". Exhausted axes are tagged so the operator
// can see at a glance which ones a grant has to cover.
func (a AxisReport) Format() string {
	var s string
	switch a.Axis {
	case AxisCost:
		s = fmt.Sprintf("$%.2f/$%.2f", a.Used, a.Granted)
	case AxisTimeout:
		// Scale as float, not via time.Duration(seconds)*time.Second: the
		// integer conversion truncates, and a step on a sub-second budget
		// would report the meaningless "0s/0s".
		s = fmt.Sprintf("%s/%s", durationSeconds(a.Used), durationSeconds(a.Granted))
	case AxisPrompts:
		s = fmt.Sprintf("%d/%d prompts", int(a.Used), int(a.Granted))
	default:
		s = fmt.Sprintf("%d/%d inv", int(a.Used), int(a.Granted))
	}
	if a.Exhausted {
		s += " (flat)"
	}
	return s
}

// FormatAxes joins all axis reports with " · " separator. Returns "" when axes
// is empty.
func FormatAxes(axes []AxisReport) string {
	if len(axes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(axes))
	for _, a := range axes {
		parts = append(parts, a.Format())
	}
	return strings.Join(parts, " · ")
}

// durationSeconds renders a float count of seconds as a duration, rounded to
// the millisecond so wall-clock jitter does not print as noise.
func durationSeconds(secs float64) time.Duration {
	return (time.Duration(secs * float64(time.Second))).Round(time.Millisecond)
}

// NewAxisReport builds one report, deriving Exhausted from the pair. A zero
// Granted means the axis carries no cap, so it cannot be exhausted.
func NewAxisReport(axis BudgetAxis, used, granted float64) AxisReport {
	return AxisReport{
		Axis:      axis,
		Used:      used,
		Granted:   granted,
		Exhausted: granted > 0 && used >= granted,
	}
}

// questionMarkerLayout is the timestamp format carried in a question park's
// Details. RFC3339 so it round-trips through the state document as plain text.
const questionMarkerLayout = time.RFC3339

const questionMarkerPrefix = "asked-at="

// LocalClockSkewAllowance is subtracted when an ask time can only be taken
// from the local clock.
//
// The two failure directions are not symmetric. An ask time slightly EARLY
// sweeps in a few minutes of comments that predate the question — the agent
// sees they do not answer it and asks again, costing one invocation. An ask
// time slightly LATE discards the real answer, and since a comment's timestamp
// never changes, no amount of answering can ever clear it. So a clock that
// might be wrong is pushed backwards, never forwards.
const LocalClockSkewAllowance = 5 * time.Minute

// MarkQuestionAskedLocal is MarkQuestionAsked for a caller with no backend
// timestamp available, backing the mark off by LocalClockSkewAllowance.
func MarkQuestionAskedLocal(now time.Time) string {
	return MarkQuestionAsked(now.Add(-LocalClockSkewAllowance))
}

// MarkQuestionAsked builds the ParkRequest.Details value recording WHEN a
// question was asked.
//
// Without it, a reader scanning for answers cannot tell a reply from the
// question itself, or from any comment that predates the question — so it must
// either treat everything as an answer (resuming a step that just asks again)
// or nothing (stranding the step forever). The timestamp is what makes "later
// than the question" expressible.
func MarkQuestionAsked(at time.Time) string {
	return questionMarkerPrefix + at.UTC().Format(questionMarkerLayout)
}

// QuestionAskedAt recovers the timestamp MarkQuestionAsked recorded, or the
// zero time when the park carries no usable marker.
//
// Falling back to the zero time reads every comment on the item, which can only
// over-report answers. That is the recoverable direction: an over-report
// resumes a step that asks again, while an under-report strands it with nobody
// watching.
func QuestionAskedAt(park *ParkRequest) time.Time {
	if park == nil {
		return time.Time{}
	}
	for _, field := range strings.Split(park.Details, ";") {
		field = strings.TrimSpace(field)
		if !strings.HasPrefix(field, questionMarkerPrefix) {
			continue
		}
		ts, err := time.Parse(questionMarkerLayout, strings.TrimPrefix(field, questionMarkerPrefix))
		if err != nil {
			return time.Time{}
		}
		return ts
	}
	return time.Time{}
}

// GrantClearsPark reports whether a grant against `step` — leaving the ledger
// row `post` — satisfies the park in `park` and so must clear it. Orchestrators
// call this from Grant so the rule is identical everywhere (see the
// Orchestrator.Grant contract).
//
// Only a ParkTreasurerRefused park on this very step can be cleared, and only
// when the offending axis now has headroom: granting $0.01 against a step that
// is $2.40 over clears nothing, and saying otherwise would report an item as
// resumable when the next dispatch would re-park it.
//
// THE CAP COMES FROM THE PARK, NOT FROM THE ROW. A step's cap is the binary's
// policy plus the row's extensions (EffectiveBudget), and the policy is the
// binary's — an orchestrator does not hold it and could not recompute the cap
// if it wanted to. What it does hold is the run's own snapshot of every axis at
// park time (ParkRequest.Axes), which records the cap that was refused on; the
// new cap is that plus what this grant adds. Consumption comes from the row,
// which is where the meter lives.
//
// A park carrying no snapshot for its axis is left in place rather than guessed
// away, for the same reason a park with no axis at all is: there is nothing to
// reason about.
func GrantClearsPark(park *ParkRequest, step StepId, post LedgerRow, g Grant) bool {
	if park == nil || park.Kind != ParkTreasurerRefused || park.Step != step {
		return false
	}
	if park.Axis == AxisTimeout {
		// Timeout is a per-run duration, not a meter that fills up: there is no
		// "consumed" value to clear. Any added time is what lets the step run
		// again, so a positive TimeoutAdd — and nothing else — clears it.
		return g.TimeoutAdd > 0
	}
	snap, ok := park.axisSnapshot(park.Axis)
	if !ok {
		return false
	}
	switch park.Axis {
	case AxisInvocations:
		return snap.Granted+float64(g.Invocations) > float64(post.Dispatches)
	case AxisCost:
		return snap.Granted+g.CostUSD > post.CostUSD
	case AxisPrompts:
		// Prompts are per-invocation and the ledger keeps no counter for them —
		// the cap resets at every dispatch, so the run's own snapshot is the
		// only record of what it burned.
		return snap.Granted+float64(g.PromptsPerInvocation) > snap.Used
	}
	// Refused with no axis recorded: nothing to reason about, so leave the park
	// in place rather than guess it away.
	return false
}

// axisSnapshot returns the park's recorded meter for one axis. ok is false when
// the park carries no snapshot for it.
func (p *ParkRequest) axisSnapshot(axis BudgetAxis) (AxisReport, bool) {
	if p == nil {
		return AxisReport{}, false
	}
	for _, a := range p.Axes {
		if a.Axis == axis {
			return a, true
		}
	}
	return AxisReport{}, false
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

// Question is a recorded ask_user_question entry on an item: an
// orchestrator-assigned QuestionId plus the AgentQuestion that was asked and,
// once answered, the UserAnswer. The embedded structs flatten into one JSON
// object.
type Question struct {
	ID QuestionId `json:"id"`
	AgentQuestion
	UserAnswer

	// AskedAt is when the orchestrator recorded the question, on the
	// ORCHESTRATOR's clock. Optional; zero when one has no server-side
	// timestamp.
	//
	// It exists because "answered after the question" is decided by comparing
	// this against reply timestamps that also come from the orchestrator.
	// Stamping it locally instead compares two different clocks, and a runner
	// running even slightly fast would silently discard every answer — a stall
	// with no diagnostic, since the reply's own timestamp never changes.
	AskedAt time.Time `json:"asked_at,omitempty"`
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

// Grant is one operator extension, in the treasurer's own axes. Zero values
// mean "no change on this axis" — a grant is ADDITIVE, recorded against the
// step's ledger row and read back through EffectiveBudget, never a replacement
// for the binary's policy.
type Grant struct {
	Invocations          int
	PromptsPerInvocation int
	CostUSD              float64
	TimeoutAdd           int64 // additional seconds; stored as int64 for JSON friendliness
}

// InvocationStatus discriminates the outcome of a single invocation.
type InvocationStatus string

const (
	StatusDone    InvocationStatus = "done"
	StatusSkipped InvocationStatus = "skipped"
	StatusFailed  InvocationStatus = "failed"
	StatusParked  InvocationStatus = "parked"
	StatusBlocked InvocationStatus = "blocked"
)

// AllInvocationStatuses returns every InvocationStatus, in declaration order.
// Downstream crosswalks enumerate it to prove they handle each one.
func AllInvocationStatuses() []InvocationStatus {
	return []InvocationStatus{StatusDone, StatusSkipped, StatusFailed, StatusParked, StatusBlocked}
}

// InvocationResult is the stdout summary one ./binary run invocation prints.
type InvocationResult struct {
	Flow         string       `json:"flow"`
	InvocationID string       `json:"invocation_id"`
	Item         string       `json:"item"`
	Step         string       `json:"step"`
	Status       string       `json:"status"` // done | skipped | failed | parked | blocked
	Reason       string       `json:"reason,omitempty"`
	Park         *ParkRequest `json:"park,omitempty"`

	// BlockKind and BlockedBy are set on a `blocked` result that stopped on the
	// item's own blockedness — the check before dispatch, or a step declaring
	// the blockers it found (docs/resolution.md § Blocked on items) — and on
	// nothing else. BlockedBy is the declared set with each blocker's status,
	// exactly as on Item, so a reader can tell the one still open from the ones
	// that have landed. Both omitempty: every other result serialises
	// byte-for-byte as it did without them.
	BlockKind BlockKind `json:"block_kind,omitempty"`
	BlockedBy []Blocker `json:"blocked_by,omitempty"`

	// Finalized reports whether the flow run was RECORDED complete, which is
	// not the same as reaching the end of the flow: Finalize refuses an item
	// the orchestrator does not yet consider finished, so a run can legitimately
	// end with every step done and nothing finalized. A caller printing
	// "finalized ✓" needs to know which happened.
	Finalized bool `json:"finalized,omitempty"`

	// DurationSeconds is the step's wall-clock time, measured by the
	// orchestrator around handler dispatch. Zero-valued with omitempty means
	// the step never dispatched (pre-dispatch exit). A measured wall-clock of
	// exactly 0.0s is effectively impossible, so omitempty suffices.
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	// CostUSD is what this invocation spent (sum of agent turns), not the
	// step's running total. nil means unknown (step never dispatched);
	// pointer-to-zero means the step ran but spent nothing. With omitempty a
	// nil pointer is omitted while &0.0 serialises as "cost_usd":0.
	CostUSD *float64 `json:"cost_usd,omitempty"`

	// ItemScoped is the scope a caller branches on when a result reports a
	// stop: true → this ITEM is the problem and a different one might succeed;
	// false → this ARENA is, and every item would meet the same answer. nil
	// means the result carries no scope classification, and a caller reads nil
	// as false — the fail-closed direction ErrClaimRefused.ItemScoped already
	// fixes, where stopping on an unclassified refusal is the safe one.
	//
	// A pointer for the reason CostUSD is one: absent means unknown, not zero
	// (docs/org/cli-guide.md § 6). With omitempty every result that classifies
	// nothing serialises byte-for-byte as it did.
	ItemScoped *bool `json:"item_scoped,omitempty"`
}
