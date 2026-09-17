package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/promise-language/flow"
)

// OutputMode selects how a command renders its result.
type OutputMode int

const (
	// OutputAuto (the zero value) picks per invocation: JSON when stdout is
	// piped or redirected, human text when it is a terminal.
	OutputAuto OutputMode = iota
	OutputHuman
	OutputJSON
)

// outputEnv is the environment override for the output mode. Set it when a
// harness cannot thread --json through every call site.
const outputEnv = "FLOW_OUTPUT"

// resolveOutput decides the mode for this invocation. Precedence: an explicit
// App.Output, then $FLOW_OUTPUT, then the nature of app.Out.
//
// The last step is why tests keep reading human text: a *os.File that is a
// character device is a terminal (human), any other *os.File is a pipe or a
// redirect (JSON), and anything that is not a file at all — the bytes.Buffer
// every test injects — is human. Auto-detection is a property of the real
// stdout, so a buffer never silently flips a test's expectations to JSON.
func (app *App) resolveOutput() OutputMode {
	if app.Output != OutputAuto {
		return app.Output
	}
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(outputEnv))); v {
	case "":
		// unset — fall through to detection
	case "json":
		return OutputJSON
	case "human":
		return OutputHuman
	default:
		// Don't silently ignore a typo: the operator asked for a mode and
		// would otherwise get the opposite one with no hint why.
		fmt.Fprintf(app.Err, "warning: %s=%q is not json|human; ignoring\n", outputEnv, v)
	}
	f, ok := app.Out.(*os.File)
	if !ok {
		return OutputHuman
	}
	fi, err := f.Stat()
	if err != nil {
		return OutputHuman
	}
	if fi.Mode()&os.ModeCharDevice != 0 {
		return OutputHuman
	}
	return OutputJSON
}

// outputFlags registers --json / --human on a command's FlagSet.
type outputFlags struct {
	asJSON  *bool
	asHuman *bool
}

func addOutputFlags(fs *flag.FlagSet) outputFlags {
	return outputFlags{
		asJSON:  fs.Bool("json", false, "force machine-readable JSON output"),
		asHuman: fs.Bool("human", false, "force human-readable output"),
	}
}

// mode applies the flags on top of the auto-detected mode. Passing both is a
// contradiction, not a precedence puzzle — it is rejected.
func (of outputFlags) mode(app *App, cmd string) (OutputMode, bool) {
	if *of.asJSON && *of.asHuman {
		app.usageError("%s: --json and --human are mutually exclusive", cmd)
		return OutputAuto, false
	}
	switch {
	case *of.asJSON:
		return OutputJSON, true
	case *of.asHuman:
		return OutputHuman, true
	}
	return app.resolveOutput(), true
}

// emit renders one command result: the payload as indented JSON, or whatever
// human writes.
//
// A STOP IS STILL A RESULT, and in JSON mode it is reported here like any
// other — the shape `run-step` already takes (refuseArena) and `claim` takes
// now, because docs/cli.md § One-shot reports asks for a report on EVERY
// outcome, including the ones that did no work. What never changes is the
// human rendering: a refusal's PROSE is on stderr with the exit code as the
// signal, and human-mode stdout carries nothing at all for it — so a reader of
// human output still never has to tell a success from a failure on one stream.
func (app *App) emit(mode OutputMode, payload any, human func()) int {
	if mode == OutputJSON {
		enc := json.NewEncoder(app.Out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(payload); err != nil {
			fmt.Fprintln(app.Err, "encode output:", err)
			return 1
		}
		return 0
	}
	human()
	return 0
}

// ---------------------------------------------------------------------------
// Payloads. Field names and the kind/state/mode enums are the machine
// contract; golden tests pin them.
// ---------------------------------------------------------------------------

// Step kinds, as reported in JSON.
const (
	kindArtifact = "artifact"
	kindSignal   = "signal"
	kindAwait    = "await"
)

// Step states, as reported in JSON. The set is CLOSED: a step is in exactly
// one of these, and a new situation needs a member rather than different prose
// (docs/cli.md § Status).
const (
	stateResolved = "resolved"
	statePending  = "pending"
	stateRunning  = "running"
	// stateElsewhere: the step is executing under ANOTHER PARTY'S CLAIM. It is
	// its own state and not a wording of `running`, because the two call for
	// opposite responses — a running step here can be watched or ended, while
	// an item leased on another host is one to leave alone.
	//
	// The evidence is the CLAIM, never a process: a process identity on another
	// machine is not knowable from here in principle, so `running` — which is
	// reported only on an observed process — could never be honestly said of it.
	stateElsewhere = "executing-elsewhere"
)

// Flow states, as reported in the status payload's flow_state.
const (
	flowStateEligible       = "eligible"
	flowStateBlocked        = "blocked"
	flowStateFinalized      = "finalized"
	flowStateNoEligibleStep = "no-eligible-step"
	flowStateNoMatchingFlow = "no-matching-flow"
)

// Grant modes, as reported in the grant payload's mode.
const (
	grantModePark   = "park"
	grantModeAll    = "all"
	grantModeManual = "manual"
)

type statusPayload struct {
	Item string `json:"item"`
	// Title is Item.Title VERBATIM — free backend prose, unclipped and
	// possibly multi-line. Only the human rendering bounds it (titleLine);
	// a tool reading JSON gets the whole string. Always present (no
	// omitempty): a stable key set is the machine contract, so an item with
	// no title reports "" rather than dropping the field.
	Title string `json:"title"`
	Owner string `json:"owner"`
	// Priority and Urgency are the two selection axes. Always present, for the
	// reason Title is: an item nothing has said anything about reports
	// "medium"/"default" rather than dropping the field.
	Priority  string   `json:"priority"`
	Urgency   string   `json:"urgency"`
	Overrides []string `json:"overrides,omitempty"`
	Flow      string   `json:"flow"`
	FlowState string   `json:"flow_state"`
	// The block, in the same keys `list` reports it under — the fact `list` and
	// `status` are required to answer identically for the same item at the same
	// moment (docs/orchestrator.md § Dependencies).
	blockPayload
	Finalized bool         `json:"finalized"`
	Park      *parkPayload `json:"park"`
	// Journal is the route so far: every completed execution, in order, with
	// who ran it, in which role, electing what and why. docs/cli.md § Status
	// asks for a ROUTE, not a checklist, and this is the half that was missing.
	Journal []journalEntryPayload `json:"journal"`
	// Awaits is whose move it is — the awaited role with its account of record,
	// or the signal a pending wait is held on. Null when the item awaits
	// nothing: unstarted, or finalized.
	Awaits *awaitsPayload `json:"awaits"`
	// Spend is the treasurer's record for the item as a whole. Null when
	// nothing has been spent on it yet.
	Spend     *spendPayload     `json:"spend"`
	Steps     []stepPayload     `json:"steps"`
	Questions []questionPayload `json:"questions"`
	// Waiting is a run that holds this arena's claim, is alive, and has no step
	// executing — pacing, or between steps. Null when nothing is waiting.
	Waiting *waitingPayload `json:"waiting"`
}

// journalEntryPayload is one completed step execution, as `status` reports it.
//
// flow.JournalEntry carries no JSON tags of its own — its wire form is the
// orchestrator's, and docs/github-schema.md owns that one — so these names are
// the CLI's and are pinned by TestStatusPayload_JournalKeySet.
type journalEntryPayload struct {
	Step string `json:"step"`
	// Execution is which completed execution of this step this is, 1-based. A
	// step the route reaches again appends again.
	Execution int `json:"execution"`
	// By and Role are who ran it and the declared role they acted in. Empty
	// when the orchestrator records no account.
	By   string `json:"by,omitempty"`
	Role string `json:"role,omitempty"`
	// Elected is the route the execution chose: the successor's step id, or
	// "finalize:<disposition>" on the entry that ended the flow.
	Elected string `json:"elected"`
	// Reason is why the successor is being run, from the step that decided —
	// or, on a finalizing entry, the closing reasons.
	Reason string `json:"reason,omitempty"`
	// Note is a standing note addressed to every subsequent step rather than
	// only the next one. It informs; it never binds.
	Note string    `json:"note,omitempty"`
	At   time.Time `json:"at"`
	// CostUSD and DurationSeconds are what the execution cost. Active time
	// only — waiting on a declared exclusion is the ledger's, and the item-level
	// spend below reports it apart.
	CostUSD         float64 `json:"cost_usd"`
	DurationSeconds float64 `json:"duration_seconds"`
}

type awaitsPayload struct {
	Role string `json:"role,omitempty"`
	// Account is the awaited role's account of record — the last entry appended
	// in that role. Empty when the role has not acted yet.
	Account string `json:"account,omitempty"`
	// Signal is set instead of Role when the pending step is a signal wait.
	// An awaited signal is NOBODY's move, which is why it is reported here and
	// as a block rather than as `awaits`.
	Signal string `json:"signal,omitempty"`
}

// spendPayload is the treasurer's item-level record.
//
// ACTIVE AND WAITING ARE SEPARATE, and reported separately: time blocked on a
// declared exclusion is not work, and a total that folded the two together
// would read as a step that took hours to do a minute's work.
type spendPayload struct {
	CostUSD        float64 `json:"cost_usd"`
	ActiveSeconds  float64 `json:"active_seconds"`
	WaitingSeconds float64 `json:"waiting_seconds"`
	// Sessions is the treasurer's count of the agent sessions this resolution
	// opened, with what the route it travelled accounts for. Null when it has
	// opened none and refused none.
	Sessions *sessionsPayload `json:"sessions"`
}

// sessionsPayload is how many conversations a resolution bought, and how many
// the route asked for.
//
// NO `excess` FIELD. An excess is `opened > expected`, and a stored answer to a
// question these two numbers already answer is a second copy that can disagree
// with them.
type sessionsPayload struct {
	// Opened is Declared + HandleGone. Refused opened nothing, so it is not in
	// it.
	Opened     int `json:"opened"`
	Declared   int `json:"declared"`
	HandleGone int `json:"handle_gone"`
	Refused    int `json:"refused"`
	// Expected is what the journal accounts for. Omitted when no flow handles
	// this item, because the count the route asks for is read off a graph this
	// binary would then not have — and a zero there would read as "the route
	// asked for none", which is an accusation rather than an absence.
	Expected *int `json:"expected,omitempty"`
}

// waitingPayload is a run that is deliberately idle: alive, holding the claim,
// with no step executing. Without it that state is indistinguishable from a
// stalled run, which is the signature an operator reads it as.
type waitingPayload struct {
	// Reason is what the run is waiting on, in words.
	Reason string `json:"reason"`
	// Until is when the hold ends, when the run knows. Zero when it does not.
	Until time.Time `json:"until,omitempty"`
	PID   int       `json:"pid,omitempty"`
}

type parkPayload struct {
	Kind string `json:"kind"`
	Step string `json:"step,omitempty"`
	Axis string `json:"axis,omitempty"`
	// Axes is every budget axis at park time, not just the tripping one, so a
	// tool reading this can compute a single grant that covers all of them.
	// Empty for park kinds that aren't budget-exhausted.
	Axes   []axisReportPayload `json:"axes,omitempty"`
	Reason string              `json:"reason,omitempty"`
}

type axisReportPayload struct {
	Axis      string  `json:"axis"`
	Used      float64 `json:"used"`
	Granted   float64 `json:"granted"`
	Exhausted bool    `json:"exhausted"`
}

type stepPayload struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
	Required bool   `json:"required"`
	// Budget is null on signal and await steps: they own no budget record, so
	// null is the machine-readable "not a grant target".
	Budget *budgetPayload `json:"budget"`
	// Next and MayFinalize are the step's DECLARED WAYS FORWARD — the
	// successors its handler may elect, and the dispositions it may end the
	// flow with. docs/cli.md § Status asks for the route, and half a route is
	// where the work has been: these are where it can go.
	//
	// They are the flow's own declaration, not a prediction of what will
	// happen, and they cost no read at all — the registration is in hand. Empty
	// slices rather than null on a step declaring neither, which is what a wait
	// is: it goes where the signal takes it and elects nothing.
	Next        []string `json:"next"`
	MayFinalize []string `json:"may_finalize"`
	RunningPID  int      `json:"running_pid,omitempty"`
	RunningExe  string   `json:"running_exe,omitempty"`
}

type budgetPayload struct {
	Invocations          intAxis     `json:"invocations"`
	CostUSD              costAxis    `json:"cost_usd"`
	PromptsPerInvocation intAxis     `json:"prompts_per_invocation"`
	TimeoutSeconds       grantedOnly `json:"timeout_seconds"`
}

type intAxis struct {
	Used    int `json:"used"`
	Granted int `json:"granted"`
}

type costAxis struct {
	Used    float64 `json:"used"`
	Granted float64 `json:"granted"`
}

type grantedOnly struct {
	Granted int `json:"granted"`
}

type questionPayload struct {
	ID string `json:"id"`
	// Header is the question's short scannable form and Text is the whole
	// prompt — a fenced evidence block, commonly, since that is what the ask
	// convention puts there. Both go out VERBATIM, unclipped and possibly
	// multi-line; only the human rendering bounds them (questionLine). Both
	// carry no omitempty for the same reason Title does: a stable key set is
	// the machine contract, and a header-less question reports "".
	Header   string `json:"header"`
	Text     string `json:"text"`
	Answered bool   `json:"answered"`
}

type listPayload struct {
	Scope string `json:"scope"`
	// Sort is the order the items are in, so a reader of the payload alone
	// knows what it is looking at. Both renderings come out in this order.
	Sort string `json:"sort"`
	// Matched is how many items the scope and tag filters selected, BEFORE
	// --limit cut. Always present: a consumer comparing it against len(items)
	// is how a cut listing is told from a complete one, which is the same fact
	// the human rendering's closing line carries.
	Matched int               `json:"matched"`
	Items   []listItemPayload `json:"items"`
}

type listItemPayload struct {
	Display string `json:"display"`
	Title   string `json:"title,omitempty"`
	// FiledAt is when the item was filed, as the ACTUAL timestamp — what
	// `--sort newest` orders by and what the resolution order breaks ties on.
	FiledAt time.Time `json:"filed_at"`
	Owner   string    `json:"owner"`
	// Arena is the holding arena as the orchestrator reports it, empty when the
	// orchestrator cannot name it (an item held by another arena on a backend
	// that publishes only a fingerprint) or when nothing holds the item.
	Arena string `json:"arena,omitempty"`
	// InProgress is whether a run was OBSERVED advancing this item — a live
	// registration naming it — never whether one is presumed to be.
	//
	// No omitempty, for the reason Priority and Urgency below carry none: the
	// value is always known, and `false` is the actual answer rather than an
	// absent one. Omitted, "no run was observed" and "this report does not
	// carry the fact" would be the same absent key.
	InProgress bool `json:"in_progress"`
	// ParkKind is the kind of the item's current park, empty when it is not
	// parked. The kind, not the rendering of it: the human row's words come
	// from this value and nothing parses them back.
	ParkKind     string   `json:"park_kind,omitempty"`
	Backend      string   `json:"orchestrator"`
	Availability string   `json:"availability,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	// Priority and Urgency are the two selection axes — what decides which of
	// these items an unattended `resolve` takes, and in what order. No
	// omitempty: both are always populated, and a stable key set is the machine
	// contract. Urgency carries the ACTUAL value, "default" included, whatever
	// the human rendering does with it.
	Priority string `json:"priority"`
	Urgency  string `json:"urgency"`
	blockPayload
}

// blockPayload is the item's block, as `list` and `status` both report it.
// Embedded anonymously, so encoding/json flattens it into the payload that
// carries it: one set of keys with one meaning, and neither command can grow a
// field the other lacks.
type blockPayload struct {
	// Blocked answers "is this blocked right now?" — item-level, and the same
	// whoever asks, unlike Availability which reports `closed` or `outside-remit`
	// instead when those come first on the ladder.
	Blocked   bool   `json:"blocked,omitempty"`
	BlockKind string `json:"block_kind,omitempty"`
	// BlockReason is prose FOR A PERSON. Nothing parses it, branches on it, or
	// infers a state from it — the fields beside it carry every machine-readable
	// fact, which is why the reason never names an item.
	BlockReason string `json:"block_reason,omitempty"`
	// BlockedBy carries the blockers still open, as references — so something
	// can act on them, which is the whole point of reporting them as data
	// rather than copied into the reason's prose.
	BlockedBy []string `json:"blocked_by,omitempty"`
}

type grantPayload struct {
	Mode string       `json:"mode"`
	Park *parkPayload `json:"park,omitempty"`
	// Note explains a successful invocation that granted nothing — a stale
	// park, an item with no pending steps. Present so a caller reading JSON
	// never has to infer "why is granted empty?" from an empty array.
	Note      string       `json:"note,omitempty"`
	Granted   []grantDelta `json:"granted"`
	Unchanged []string     `json:"unchanged"`
	Unparked  bool         `json:"unparked"`
	DryRun    bool         `json:"dry_run"`
}

type grantDelta struct {
	ID                   string     `json:"id"`
	Invocations          *intDelta  `json:"invocations,omitempty"`
	PromptsPerInvocation *intDelta  `json:"prompts_per_invocation,omitempty"`
	CostUSD              *costDelta `json:"cost_usd,omitempty"`
	TimeoutSeconds       *intDelta  `json:"timeout_seconds,omitempty"`
}

type intDelta struct {
	From int `json:"from"`
	To   int `json:"to"`
}

type costDelta struct {
	From float64 `json:"from"`
	To   float64 `json:"to"`
}

// claimPayload is `claim`'s report, and it is ONE object for every outcome: a
// claim taken, a typed refusal, and a stop that never reached the backend at
// all. A caller driving arenas reads one shape rather than switching on
// whether stdout carried anything (docs/cli.md § One-shot reports).
//
// The refusal's fields are FLAT rather than nested under a `refusal` object.
// docs/cli.md § Output defines `item_scoped` as a field of a result and says a
// refused claim carries "the same distinction, and the same name" — nested, the
// one consumer that reads both this and an InvocationResult would have two
// spellings of one fact.
type claimPayload struct {
	Item string `json:"item"`
	// Claimed carries no omitempty for the reason listItemPayload.InProgress
	// carries none: the value is always known, and `false` is the actual answer
	// rather than an absent one.
	Claimed bool `json:"claimed"`
	// Account is the account the lease was minted as — ambient, read by the
	// orchestrator rather than given to it. Empty on every outcome but a claim.
	Account string `json:"account,omitempty"`

	// Code is the refusal's own code, VERBATIM. flow defines no constants for
	// the vocabulary (flow.ClaimRefusalCode) because it belongs to the refusing
	// backend, and this passes it through without interpreting it. Empty on a
	// stop that carries no typed refusal.
	Code string `json:"code,omitempty"`
	// ItemScoped is the scope a caller branches on: true → this ITEM is the
	// problem and a different one might succeed; false → this ARENA is, and
	// every item would meet the same answer. nil means the stop classified
	// nothing, and a caller reads nil as false — the fail-closed direction
	// docs/cli.md § Output fixes.
	//
	// A pointer for the reason flow.InvocationResult.ItemScoped is one: a
	// present false is the classification that matters most, and a plain bool
	// with omitempty would put it on the wire identically to absent — while a
	// plain bool WITHOUT omitempty would report a successful claim as
	// `item_scoped: false`, which reads as an arena-scoped stop.
	ItemScoped *bool `json:"item_scoped,omitempty"`
	// Reason is the one-line human reason. Nothing parses it: every
	// machine-readable fact about the stop is a field beside it.
	Reason string `json:"reason,omitempty"`
	// Detail is the failing check's own output, reproduced verbatim and
	// unmodified, and Check names the check that produced it.
	Detail string `json:"detail,omitempty"`
	Check  string `json:"check,omitempty"`
	// Override is the flag that would bypass this refusal, as its NAME —
	// `force-unadmitted`, not `--force-unadmitted`. The value, never the
	// rendering: the human line adds the dashes and nothing parses them back.
	// Empty when nothing overrides it.
	Override string `json:"override,omitempty"`
}

// refusedClaimPayload projects a typed refusal onto the wire. THE ONE
// projection of flow.ErrClaimRefused: a second copy of this mapping is how the
// four things docs/cli.md § Claiming says a refusal carries come to be three at
// one command and four at another.
func refusedClaimPayload(item string, e flow.ErrClaimRefused) claimPayload {
	scoped := e.ItemScoped
	return claimPayload{
		Item:       item,
		Claimed:    false,
		Code:       string(e.Code),
		ItemScoped: &scoped,
		Reason:     e.Reason,
		Detail:     e.Detail,
		Check:      e.Check,
		Override:   e.Override,
	}
}
