package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
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
// human writes. Errors never travel through here — they stay plain text on
// stderr with the exit code as the signal, so a caller never has to tell a
// success payload from a failure payload on the same stream.
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

// Step states, as reported in JSON.
const (
	stateResolved = "resolved"
	stateStale    = "stale"
	statePending  = "pending"
	stateSkipped  = "skipped"
)

// Flow states, as reported in the status payload's flow_state.
const (
	flowStateEligible       = "eligible"
	flowStateFinalized      = "finalized"
	flowStateNotSeeded      = "not-seeded"
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
	Title     string            `json:"title"`
	Owner     string            `json:"owner"`
	Overrides []string          `json:"overrides,omitempty"`
	Flow      string            `json:"flow"`
	FlowState string            `json:"flow_state"`
	Finalized bool              `json:"finalized"`
	Park      *parkPayload      `json:"park"`
	Steps     []stepPayload     `json:"steps"`
	Questions []questionPayload `json:"questions"`
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
	ID       string `json:"id"`
	Text     string `json:"text"`
	Answered bool   `json:"answered"`
}

type listPayload struct {
	Scope string            `json:"scope"`
	Items []listItemPayload `json:"items"`
}

type listItemPayload struct {
	Display      string   `json:"display"`
	Title        string   `json:"title,omitempty"`
	Owner        string   `json:"owner"`
	Backend      string   `json:"backend"`
	Availability string   `json:"availability,omitempty"`
	Tags         []string `json:"tags,omitempty"`
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
