# Proposal: structured gates in the flow SDK

**Status:** draft, not yet implemented
**Author:** initial sketch
**Related:** [docs/design.md](../design.md). The contracts here are
self-contained — any orchestrator (a tracker, GitHub Actions, a homegrown
scheduler) can consume them; none is assumed.

## Goal

Let a standalone **gate binary** host one or more **gates** — structured,
repeatable checks whose stdout JSON feeds an orchestrator and powers
ratcheting metrics + health dashboards. Gates are independent of the
per-item claim/run-step lifecycle: they run on a schedule against the
project's repo, not against a single item — they need no items and produce
no artifacts. They do still need somewhere to run: when the system dispatches
a gate automatically it runs inside an **arena** under a **temporary arena
lease** held only for that run (a developer may also run a gate by hand in
their own checkout). That lease binds an arena to a gate run — there is no
item to bind — which is exactly what makes it different from the item↔arena
**claim**. So a gate lives in its **own binary**, separate from any flow
binary (see [Why a separate binary](#why-a-separate-binary)).

Concretely, this proposal adds:

1. A strict **command protocol** for a gate binary: `bin/gate <name>` runs
   one gate, `bin/gate --list` enumerates definitions, bare `bin/gate`
   prints usage.
2. A self-contained **run-output wire format** (the JSON a gate writes to
   stdout).
3. A fully-specified **gate-definition JSON** (what `bin/gate --list --json`
   emits).
4. A **`flow/gate` Go subpackage**: type defs, an `Emit` helper, and a
   `Main` entry point so a gate binary is a few lines and typed end-to-end.

The SDK ships **no scheduler**. Scheduling, ratcheting, baseline storage,
and dashboarding stay in the orchestrator. The SDK is concerned only with
the gate definition / invocation / output boundary.

## The gate command protocol

A gate binary exposes exactly one command surface with three invocation
forms — **and nothing else is accepted**. Throughout this doc `bin/gate`
denotes that binary; the first positional argument is always the gate name
(there is no `run` subcommand and no other subcommands).

```
bin/gate                      # usage (human-readable) → stdout, exit 0
bin/gate --list [--json]      # enumerate gate definitions
bin/gate <gate-name> [flags]  # run one gate → JSON envelope on stdout
```

`--list`, `--json`, and `--help`/`-h` are list/usage flags; `--target` and
`--report-failures-only` are **built-in run flags** — the same across every gate,
honored only when running one, and never declared in `flags[]` (so a gate may
not declare a flag named `target` or `report-failures-only`). Everything else —
unknown gate, unknown flag, stray positional — is a hard error (see [Strict
input handling](#strict-input-handling)).

### Form 1 — `bin/gate` (no arguments): usage

Prints human-readable usage to stdout and exits 0. Usage covers the three
forms and points the reader at `bin/gate --list` to see what gates exist.
`bin/gate --help` / `-h` is identical. Usage is for humans only — it is
never JSON.

### Form 2 — `bin/gate --list`: gate definitions

Enumerates every gate the binary hosts. The output format auto-detects the
destination:

- **stdout is not a TTY (piped/redirected) → JSON** (the gate-definition
  array below). This is the default for machine consumers; no `--json`
  needed when piped.
- **stdout is a TTY → human-readable** — one row per gate (name,
  description, schedule, declared metrics and flags).
- **`--json` forces JSON** regardless of TTY. `bin/gate --list --json`
  always emits the definition array, byte-for-byte identical to the piped
  form.

Exit 0 on success. `--list` MUST NOT be combined with a gate name (→ usage
error, exit 2).

#### The gate-definition JSON

`bin/gate --list --json` emits a single JSON **array**; each element fully
describes one gate. This array is the complete, self-describing manifest —
an orchestrator needs nothing beyond it to discover, schedule, and
correctly invoke every gate.

```jsonc
[
  {
    "name": "build-time",                         // REQUIRED. unique id; the positional arg to run it. /^[a-z0-9][a-z0-9-]*$/
    "description": "wall-clock of `go build ./...`", // optional. human summary, **Markdown**.
    "schedule": { "kind": "after-commit" },        // optional, ADVISORY, STRUCTURED (see field rules). the binary never schedules itself.
    "timeout_seconds": 600,                        // optional. wall-clock budget hint for the orchestrator; integer seconds. absent/0 = no hint.
    "targets": [                                   // optional. the exact targets this gate can produce; each run selects ONE via --target. OMIT for a target-agnostic gate (output then has no `target`).
      { "target": "linux-amd64", "host_os": "linux" } // host_os: "linux"|"darwin"|"windows"|"any" — where the orchestrator may run this target.
    ],
    "metrics": {                                   // optional. declares each metric this gate emits: its value type + how to ratchet it.
      "build_seconds": {
        "type": "float",                           // REQUIRED. "bool"|"int"|"float" — the metric's value type.
        "direction": "lower-is-better",            // REQUIRED. "lower-is-better"|"higher-is-better".
        "mode": "enforced",                        // REQUIRED. "enforced"|"pending"|"informational".
        "ratchet_cap": 30.0                        // optional. baseline stops auto-advancing past this value.
      }
    },
    "flags": [                                     // optional. tuning knobs for manual/dev runs; EVERY flag is optional (see field rules).
      {
        "name": "fresh",                           // REQUIRED. long flag name, no leading dashes → invoked as --fresh.
        "type": "bool",                            // REQUIRED. "bool"|"string"|"int"|"duration".
        "description": "ignore the build cache",   // optional. Markdown.
        "default": "false"                         // optional. string-encoded default, used when the flag is omitted.
      }
    ]
  }
]
```

Field rules:

- **`name` is the only required field** and MUST be unique across the
  array. It is the exact token passed to run the gate.
- **`description` is Markdown** — a consumer rendering it (a dashboard, a
  web UI) should treat it as such; the human `--list` output prints it
  verbatim.
- **`schedule` is a struct, never a string** — the orchestrator switches on
  `kind` and reads a number; it never parses prose. Shape:
  `{ "kind": <enum>, "interval_seconds"?: <int> }`.
  - `"interval"` — run every `interval_seconds` (integer seconds; **required**
    for this kind, must be > 0). "every 4h" is `14400`; "daily" is `86400`.
  - `"after-commit"` — run after each commit lands. No number.
  - `"manual"` — never run automatically. No number.

  An absent `schedule`, an empty object, or an unrecognized `kind` all mean
  **manual**. The field is **advisory**: the orchestrator owns actual timing;
  the gate binary never schedules itself.
- **`targets` makes a gate target-bound.** It lists the **exact** targets the
  gate can produce (e.g. `linux-amd64`, `darwin-arm64`, `wasm32-wasi`), each
  with the `host_os` (`"linux"`/`"darwin"`/`"windows"`/`"any"`) on which the
  orchestrator may run it. A target-bound gate MUST be run with `--target <t>`
  naming one listed target, and it emits `"target":"<t>"` **exactly** — there
  is no `os`→triple inference. A gate with **no** `targets` is
  **target-agnostic**: it takes no `--target`, emits no `target`, and has a
  single baseline (build-time, lint). Fan-out across targets is the
  **orchestrator's** job: it runs one invocation per listed target on a
  `host_os`-capable machine and keys a baseline per **(gate, target)**. The
  run enforces `host_os` defensively — it refuses a target whose `host_os` the
  current host can't satisfy (see strict input handling), so a misdispatch
  fails loud instead of producing a bogus result. For a manual run `--target`
  may be omitted, defaulting to the host-native target when exactly one
  declared target matches the host's OS. Two runs are comparable iff
  they share a target — which is why the target is an explicit, exact,
  per-invocation value and never a coarse OS hint.
- Absent optional scalars carry their zero value (`""`, `0`, `false`);
  absent objects/arrays are empty (`{}`, `[]`). A consumer treats absent and
  zero identically.
- **`flags[]` is authoritative**: it is the complete set of flags the run
  form will accept for that gate. Anything not listed is rejected.
- **Every flag is optional — there are no required flags.** A gate MUST run to
  completion with none supplied, so `bin/gate <name>` (which on a target-bound
  gate defaults `--target` to the host-native one) is always a complete, valid
  invocation. This is load-
  bearing: the orchestrator drives gates from their **definitions alone** and
  has no way to know how to set a gate-specific flag, so it supplies none. Flags
  exist only as tuning knobs for manual/dev runs; an omitted flag falls back to
  its `default` (or the zero value for its `type` if `default` is unset).
- `metrics` keys are shared with the run output: the **definition** gives
  each metric its `type` + ratchet config, and the **run output** reports the
  same key's value. Reporting an undeclared metric is a gate bug the
  orchestrator may reject. On the wire a value is a JSON number encoded per
  its declared `type` — `bool` ⇒ `0`/`1`, `int` ⇒ integral, `float` ⇒ real —
  so ratcheting always compares numbers.

### Form 3 — `bin/gate <gate-name> [flags]`: run a gate

Runs exactly one gate.

- **stdout carries the run-output JSON envelope and nothing else** — always
  JSON, one object, never human-readable, regardless of TTY.
- **stderr MAY carry human-readable progress/logs** while the gate runs
  (build output, a spinner, diagnostics). Orchestrators never parse stderr
  for **results** — those come only from the stdout envelope and the exit
  code — but they MAY capture and retain it for diagnostics, especially to
  debug a failed run. It is also there for humans tailing a run live.
- **`[flags]` are the gate's declared flags** (from its `flags[]`), and are
  **all optional** — the orchestrator runs gates with none, so `bin/gate
  <name>` (plus `--target` where required) is always a complete run. Flag
  access inside the handler is typed.
- **`--target <t>` selects the target** for a target-bound gate (a built-in
  selector, not a gate-declared flag). `<t>` MUST be one of the declared
  targets; the run echoes it as the envelope's `target`, exactly. It may be
  **omitted**: the run then defaults to the **host-native target** — the unique
  declared target whose `host_os` equals the current host's OS — so a manual
  `bin/gate <name>` runs without ceremony on a capable host. `--target` is
  required only when there is no single native target to default to (zero, or
  several, declared targets match the host's OS). `host_os: "any"` targets
  (wasm) are never auto-selected — request them explicitly. A target-agnostic
  gate accepts no `--target`. The run also **refuses a target this host can't
  run**: if `<t>`'s `host_os` is a concrete OS that isn't the current host's
  (and isn't `any`), it errors instead of running — e.g. `--target windows-x86`
  on a Linux host. Wasm is just another target — `--target wasm32-wasi` — never
  a `--wasm` flag.
- **`--report-failures-only` is a built-in diagnostic flag** any gate honors
  (not gate-declared). It projects the output to **failures only** — a record
  is kept iff it actually failed: `fail`, `timeout`, `leak`, `memory`, or
  `not-run`. `pass` is dropped, and so is `excluded` (a deliberate "don't run
  this test for this target" — not a failure) — drops now-empty `FileEntry`
  objects, and reports **`complete: false`** (the result is no longer the full
  set). `metrics` are
  left untouched. It is a human/dev view of "what isn't passing"; the
  orchestrator's ratcheting path always runs **without** it, and a
  `--report-failures-only` output MUST NOT be counted for baselines (its `files[]` is
  partial). A gate with no `files` emits an unchanged, file-less envelope.
- **Exit code is the result signal:**

| exit | meaning | stdout |
|---|---|---|
| `0` | gate ran and **passed** | valid envelope |
| `1` | gate ran and **failed** (≥1 failing record and/or `error` set) | valid envelope, best-effort |
| `2` | **usage / strict-input error** (unknown gate or flag, bad value, extra args) | **no envelope**; clear message on stderr |
| `3` | **infra error** — the gate could not really run (transient) | envelope with `"infra_error": true`, best-effort |

The contract on failure: whenever a gate is selected and starts (exit 1 or
3), stdout SHOULD still be a **valid JSON envelope** that captures the
failure — `error` (short reason) and `error_context` (bounded detail: the
failing command, a stderr tail, a stack) — so a run can be debugged from its
captured stdout alone. Only exit 2 (no gate ran) is allowed to skip the
envelope, because there is no gate context to attribute the error to.

#### The run-output JSON envelope

A single JSON object on stdout:

```jsonc
{
  "schema": 2,                          // REQUIRED. envelope version.
  "gate": "build-time",                 // RECOMMENDED. the gate that produced this output (aids debugging collected runs).
  "target": "linux-amd64",              // present iff the gate is target-bound; MUST equal the requested --target exactly. omitted by target-agnostic gates.
  "metrics": { "build_seconds": 42.7 }, // optional. measured values, keyed by name (a subset of the gate's declared `metrics`; encoded per each metric's `type`).
  "files": [ /* FileEntry */ ],         // optional. test records grouped by source file.
  "complete": true,                     // optional bool. true iff files[] is the COMPLETE record set for this (gate, target) — every test incl. pass & excluded. false/absent on a --report-failures-only or otherwise partial run.
  "infra_error": false,                 // optional. true ⇒ "didn't really run; ignore this result" (paired with exit 3).
  "error": "build failed",              // optional. short human reason the gate (as a whole) failed.
  "error_context": "exit status 2\n…"   // optional. bounded detail for debugging (failing cmd, stderr tail, stack).
}
```

```jsonc
// FileEntry — test records for one source file
{
  "file": "tests/std/bool_test.pr",     // repo-relative, forward slashes. stable identity.
  "tests": [
    { "test": "test_and", "status": "pass", "elapsed": 0.001 },
    { "test": "test_or",  "status": "fail", "elapsed": 0.002,
      "context": "panic: assertion failed: ..." }   // bounded detail when status != pass
  ]
}
```

A target-agnostic gate (build time, lint count) emits just `metrics`; a
target-bound test gate emits `target` + `files` and lets the consumer derive
`metrics` by counting records. Four properties define the test shape:

- **Single-target invariant.** One gate invocation reports exactly one
  `target` — the one named by `--target` — stamped once at the top, never per
  record. A gate produces its other targets only via *other* invocations; the
  orchestrator fans out across the gate's declared `targets`. wasm is just
  another target entry (`wasm32-wasi`), selected like any other — never a
  flag, never multiplexed into one run.
- **Tests grouped by file.** Each `FileEntry` carries a repo-relative `file`
  and a `tests[]` of `{test, status, elapsed, context?}`. A test's identity
  is the pair **(`file`, `test`)** and is **stable across runs** — it never
  varies with outcome, so a test that flips pass↔fail keeps its identity.
  `test` is the function name for batch tests or the literal `"main"` for
  e2e/snapshot files; for Go tests `file` is the repo-relative package dir
  and `test` is the Go test-function name (subtests as `TestFoo/sub`).
- **Status vocabulary** (lower-case): `pass`, `fail`, `timeout`, `leak`,
  `memory` (per-test memory limit tripped), `excluded` (compiled but not run
  for this target), and `not-run` (never ran because an earlier test aborted
  the process). Tests excluded from compilation produce **no record**. The
  `not-run` status is how the format **captures the tests that did not run**:
  an aborted-process tail is still enumerated from the file's compile-time
  roster — a `memory` abort marks the first un-resulted roster test `memory`
  and every later one `not-run`; a hard crash marks the first unseen `fail`
  (with crash context) and the rest `not-run`.
- **Metrics derived by counting records.** When a gate reports `files`, each
  metric counts records of one status (`test_count`=pass, `test_failures`=fail,
  `leak_count`, `timeout_count`, `memory_count`, `excluded_count`,
  `not_run_count`). Names are **not** target-prefixed — the envelope's
  `target` already keys the result, so the baseline tuple is **(gate, target,
  metric)**. (This is what the old `host_`/`wasm_` prefixes were faking before
  `target` was a first-class field.) A gate MAY emit `metrics` directly
  instead; if it emits both, they MUST agree with `files`. Deriving by counting
  is valid only on a **full** output — a `--report-failures-only` run has dropped its
  `pass` records, so its `files[]` MUST NOT be counted for baselines.

`metrics` drives ratcheting baselines; `files` feeds a per-test health
ledger; `complete` (a bool) is true iff `files[]` holds the **whole** record
set for this `(gate, target)` — every test, including `pass` and `excluded`.
It is the ledger's signal that a run is authoritative coverage rather than a
partial view; a `--report-failures-only` (or sharded/sampled) run reports
`complete: false`. There is no implicit-pass backfilling — `complete` asserts
nothing about tests absent from `files[]`. The value is never a set *name*:
the set is always "(gate, target)'s tests", so a string id would just
duplicate fields already in the envelope.

### Strict input handling

The command treats every malformed invocation as an error — it **never**
silently ignores or guesses, because a swallowed flag or misspelled gate
name reads downstream as "the gate passed":

- **Unknown gate name** → exit 2, stderr: `unknown gate "X"; run 'bin/gate
  --list' for the available gates`.
- **Unknown flag** (not in the selected gate's `flags[]`, not a recognized
  global flag) → exit 2; stderr names the offending flag and lists the
  gate's accepted flags.
- **Wrong flag value** (non-integer for `int`, unparseable `duration`,
  missing value for a value flag) → exit 2.
- **Bad `--target`** — a target-bound gate run without `--target` **when there
  is no single host-native target to default to** (zero or several declared
  targets match the host's OS), or with a `--target` not in its declared
  `targets`, or a `--target` passed to a target-agnostic gate → exit 2.
- **Host cannot run the target** — `--target` names a declared target whose
  `host_os` is a concrete OS the current host isn't (and isn't `any`) → exit 2,
  e.g. stderr `target "windows-x86" needs host_os "windows"; this host is
  "linux"`. This catches an orchestrator dispatching a target to an incapable
  host; the gate refuses rather than producing a bogus result.
- **Extra positional arguments** (a second token after the gate name; any
  positional alongside `--list`) → exit 2.
- **Conflicting forms** (`--list` with a gate name; `--json` without
  `--list`) → exit 2.

Every exit-2 error prints a single clear line to stderr and produces **no
stdout envelope**. `--json` is meaningful only with `--list`; in run mode
the output is always JSON, so a bare `--json` there is just an unknown flag
(rejected unless the gate happens to declare one named `json`).

## The `flow/gate` Go subpackage

A small new package — types, an `Emit` helper, and a `Main` entry point.
Mirrors the way `flow/claude/` sits next to `flow.Agent` in its own
subpackage, so the root pulls in zero transitive deps. The wire format is
language-independent; this package is convenience for Go authors, not the
contract — a gate can be written in shell, Python, anything that honors the
protocol above.

```go
package gate

// Output is the stdout JSON envelope a gate writes on exit.
type Output struct {
    Schema       int                `json:"schema"`                  // 2
    Gate         string             `json:"gate,omitempty"`          // the gate that produced this output
    Target       string             `json:"target,omitempty"`        // the requested --target, echoed exactly; "" if target-agnostic
    Metrics      map[string]float64 `json:"metrics,omitempty"`       // measured values
    Files        []FileEntry        `json:"files,omitempty"`         // test records grouped by file
    Complete     bool               `json:"complete,omitempty"`      // true iff Files is the full (gate, target) roster (incl. pass & excluded)
    InfraError   bool               `json:"infra_error,omitempty"`   // paired with exit 3
    Error        string             `json:"error,omitempty"`         // short reason
    ErrorContext string             `json:"error_context,omitempty"` // bounded debug detail
}

// FileEntry groups the test records for one source file. A test's identity is
// the pair (File, TestEntry.Test) and is stable across runs.
type FileEntry struct {
    File  string      `json:"file"`   // repo-relative path, forward slashes
    Tests []TestEntry `json:"tests"`
}

type TestEntry struct {
    Test    string  `json:"test"`               // function name, or "main" for e2e/snapshot
    Status  Status  `json:"status"`
    Elapsed float64 `json:"elapsed,omitempty"`  // seconds
    Context string  `json:"context,omitempty"`  // bounded failure detail when not pass
}

type Status string

const (
    StatusPass     Status = "pass"
    StatusFail     Status = "fail"
    StatusTimeout  Status = "timeout"
    StatusLeak     Status = "leak"
    StatusMemory   Status = "memory"   // per-test memory limit tripped (process aborted)
    StatusExcluded Status = "excluded" // compiled, not run for this target
    StatusNotRun   Status = "not-run"  // never ran; an earlier test aborted the process
)

// Gate is one gate's declarative descriptor — what `bin/gate --list`
// surfaces, plus the handler that `bin/gate <name>` runs.
type Gate struct {
    Name           string            `json:"name"`                       // unique id; the positional arg to run it
    Description    string            `json:"description,omitempty"`      // human summary, Markdown
    Schedule       *Schedule         `json:"schedule,omitempty"`         // advisory cadence; nil ⇒ manual
    TimeoutSeconds int               `json:"timeout_seconds,omitempty"`  // wall-clock budget hint (integer seconds)
    Targets        []Target                `json:"targets,omitempty"`          // exact targets this gate can produce; empty ⇒ target-agnostic
    Metrics        map[string]MetricConfig `json:"metrics,omitempty"`          // declared metrics: value type + ratchet config
    Flags          []Flag                  `json:"flags,omitempty"`            // gate-specific flags the run form accepts

    // Run is the in-process handler. It receives a minimal RunCtx — NOT a
    // flow.StepCtx: a gate has no item, no artifacts, and no claim. It builds
    // an Output and returns it; Main wraps Emit + the exit code.
    Run func(rc RunCtx) (Output, error) `json:"-"`
}

// RunCtx is a gate handler's whole world. Deliberately a struct (not a bare
// context.Context) so future capabilities — notably the user-attention API
// (TBD) — can be added as fields without breaking handler signatures.
type RunCtx struct {
    Context context.Context // carries the orchestrator-imposed deadline
    Flags   Flags           // this gate's declared, already-validated flags
    Target  string          // the selected target: the --target value, or the host-native default when omitted; "" if target-agnostic
    // Attention Attention   // FUTURE: raise something for a human; nil until defined.
}

// Target is one target a target-bound gate can produce, plus the host OS that
// may run it. Each run selects one via --target and echoes its Triple exactly.
type Target struct {
    Triple string `json:"target"`            // the target triple, e.g. "linux-amd64", "wasm32-wasi"
    HostOS string `json:"host_os,omitempty"` // "linux"|"darwin"|"windows"|"any" — where the orchestrator may run it
}

// Flags gives a handler typed access to its declared flags. Names match the
// gate's Flag entries; an undeclared name panics (programmer error).
type Flags interface {
    Bool(name string) bool
    String(name string) string
    Int(name string) int
    Duration(name string) time.Duration
}

// Flag declares one optional gate flag. There is no "required" field by
// design: the orchestrator runs gates from their definitions alone, so every
// flag must have a usable default and a gate must run with none supplied.
type Flag struct {
    Name        string   `json:"name"`                  // long name, no leading dashes → --name
    Type        FlagType `json:"type"`                  // bool|string|int|duration
    Description string   `json:"description,omitempty"`
    Default     string   `json:"default,omitempty"`     // string-encoded default, used when the flag is omitted
}

type FlagType string

const (
    FlagBool     FlagType = "bool"
    FlagString   FlagType = "string"
    FlagInt      FlagType = "int"
    FlagDuration FlagType = "duration"
)

// Schedule is an advisory cadence hint — structured so an orchestrator
// switches on Kind and reads a number, never parsing prose. A nil *Schedule
// (or an unrecognized Kind) means manual.
type Schedule struct {
    Kind            ScheduleKind `json:"kind"`
    IntervalSeconds int          `json:"interval_seconds,omitempty"` // required (>0) iff Kind == ScheduleInterval
}

type ScheduleKind string

const (
    ScheduleInterval    ScheduleKind = "interval"     // run every IntervalSeconds
    ScheduleAfterCommit ScheduleKind = "after-commit" // run after each commit
    ScheduleManual      ScheduleKind = "manual"       // never run automatically
)

// Schedule constructors — author cadence without touching IntervalSeconds:
//
//	Schedule: gate.Every(4 * time.Hour)   // interval
//	Schedule: gate.AfterCommit()
//	Schedule: gate.Manual()
func Every(d time.Duration) *Schedule { return &Schedule{Kind: ScheduleInterval, IntervalSeconds: int(d.Seconds())} }
func AfterCommit() *Schedule          { return &Schedule{Kind: ScheduleAfterCommit} }
func Manual() *Schedule               { return &Schedule{Kind: ScheduleManual} }

type MetricConfig struct {
    Type       MetricType `json:"type"`                  // bool | int | float — the metric's value type
    Direction  Direction  `json:"direction"`             // lower-is-better | higher-is-better
    Mode       Mode       `json:"mode"`                  // enforced | pending | informational
    RatchetCap *float64   `json:"ratchet_cap,omitempty"`
}

type MetricType string

const (
    MetricBool  MetricType = "bool"
    MetricInt   MetricType = "int"
    MetricFloat MetricType = "float"
)

type Direction string

const (
    LowerIsBetter  Direction = "lower-is-better"
    HigherIsBetter Direction = "higher-is-better"
)

type Mode string

const (
    ModeEnforced      Mode = "enforced"
    ModePending       Mode = "pending"
    ModeInformational Mode = "informational"
)

// Main implements the full command protocol over os.Args[1:] and returns the
// process exit code. A gate binary is just:
//
//	func main() { os.Exit(gate.Main(buildTimeGate, lintGate)) }
//
// Main owns: usage (bare invocation / --help), --list (TTY-aware + --json),
// strict argument parsing (exit 2 on any unknown gate/flag/positional), flag
// validation + typing, --target selection + validation against the gate's
// declared Targets (defaulting an omitted --target to the host-native target,
// refusing one this host's OS can't run), the built-in --report-failures-only
// projection (drop pass records, set complete=false), dispatch of the selected
// Run, and Emit of its Output.
func Main(gates ...Gate) int { /* ... */ }

// Emit writes out as a single-line JSON envelope to stdout and returns the
// exit code: 0 if the gate passed (no failing records, no Error, no
// InfraError); 3 if out.InfraError; otherwise 1. Main calls this; gates that
// build their own entry point can reuse it. (Exit 2 is owned by Main's arg
// parser and never reaches Emit — no gate ran.)
func Emit(out Output) int { /* ... */ }
```

## What stays out of the SDK

Three things stay deliberately **out** of the SDK:

- **Baselines / ratcheting state.** State lives in the orchestrator; the SDK
  never touches disk for gate state. This keeps each `bin/gate <name>`
  invocation stateless.
- **Scheduler.** No goroutine timers, no cron parsing. The orchestrator
  decides when to invoke `bin/gate <name>`; `schedule` in the definition is
  an advisory hint, nothing more.
- **Arena leasing.** When gates run automatically they run inside an arena,
  but acquiring and releasing that **temporary arena lease** is the
  **orchestrator's** responsibility, not the SDK's. A gate resolves its target
  worktree from its **own binary location** — the repo root baked in at build
  time (forge-style ldflags injection) or derived from the binary's directory.
  **CWD is not an input**: running `bin/gate <name>` from any directory
  produces an identical run. So "running a gate in a given arena" means
  invoking *that arena's* gate binary; the orchestrator selects the arena by
  choosing which binary to run, and owns the lease around it. The SDK adds no
  leasing API to `RunCtx`, and a manual run needs no lease at all.

## Why a separate binary

Gates are a separate binary from any flow binary, on purpose:

- **A gate needs almost none of the flow lifecycle.** No item, no artifacts,
  no item↔arena **claim** — it measures the worktree its **own binary**
  belongs to (the repo root is baked in at build time, or derived from the
  binary's location — never from CWD) and emits JSON. It *does* run inside an
  arena when the system dispatches it, but leasing that arena is the
  orchestrator's job, invisible to the gate (see [What stays out of the
  SDK](#what-stays-out-of-the-sdk)). Folding gates into the flow binary's
  command tree would force the confusing `./issue gate <name>` shape and couple
  two things that share little.
- **The gate name wants to be the first positional.** `bin/gate build-time`
  reads cleanly; the gate name is the subject of the command, not a value
  buried under a subcommand. A dedicated binary gives that for free.
- **The only thing a gate may eventually need from the SDK** beyond a context
  and its flags is the **user-attention API** (raise something for a human —
  not yet defined). When it lands it becomes a field on `RunCtx`, not a
  reason to merge binaries.

A project that wants both ships two binaries (`bin/issue` for flows, `bin/gate`
for gates) — both built by the same `./make`, neither aware of the other.

## Discovery flow (orchestrator side)

```
orchestrator (a tracker, CI, custom):
  1. bin/gate --list --json     → gate-definition array (discover gates, their targets + flags)
  2. for each gate, decide WHEN to run it (from `schedule`) and — if target-bound —
     WHICH of its `targets` to run, dispatching each to a `host_os`-capable machine
  3. bin/gate <name> --target <t> [flags]  → one target per run; read the envelope; exit code is the result
  4. parse `metrics`, compare to the (gate, target) baseline, ratchet, alert, dashboard, etc.
```

An orchestrator periodically re-fetches `bin/gate --list --json` to pick up
gates the project added — an `import-gates <binary>` command on the
orchestrator side becomes a few lines: invoke `--list --json`, merge into
whatever local gate registry it keeps.

## Worked example

A dedicated gate binary hosting a build-time and a lint gate:

```go
// cmd/gate/main.go
package main

import (
    "os"
    "os/exec"
    "time"

    "github.com/promise-language/flow/gate"
)

// projectRoot is baked in at build time (forge-style ldflags injection); a
// gate resolves its worktree from it, never from CWD.
var projectRoot = "/path/baked/in/at/build"

func main() {
    os.Exit(gate.Main(
        gate.Gate{
            Name:           "build-time",
            Description:    "wall-clock of `go build ./...`",
            Schedule:       gate.AfterCommit(),
            TimeoutSeconds: 600,
            Metrics: map[string]gate.MetricConfig{
                "build_seconds": {Type: gate.MetricFloat, Direction: gate.LowerIsBetter, Mode: gate.ModeEnforced},
            },
            Flags: []gate.Flag{
                {Name: "fresh", Type: gate.FlagBool, Description: "ignore the build cache"},
            },
            Run: runBuildTimeGate,
        },
        gate.Gate{
            Name:        "lint",
            Description: "`go vet ./...` warning count",
            Schedule:    gate.AfterCommit(),
            Metrics: map[string]gate.MetricConfig{
                "lint_warnings": {Type: gate.MetricInt, Direction: gate.LowerIsBetter, Mode: gate.ModeEnforced},
            },
            Run: runLintGate,
        },
    ))
}

func runBuildTimeGate(rc gate.RunCtx) (gate.Output, error) {
    args := []string{"build", "./..."}
    if rc.Flags.Bool("fresh") {
        args = append([]string{"build", "-a"}, "./...")
    }
    start := time.Now()
    cmd := exec.CommandContext(rc.Context, "go", args...)
    cmd.Dir = projectRoot // resolve the worktree from the binary, not CWD
    if out, err := cmd.CombinedOutput(); err != nil {
        return gate.Output{
            Schema:       2,
            Gate:         "build-time",
            Error:        "build failed: " + err.Error(),
            ErrorContext: tail(out, 2000),
        }, nil
    }
    return gate.Output{
        Schema:  2,
        Gate:    "build-time",
        Metrics: map[string]float64{"build_seconds": time.Since(start).Seconds()},
    }, nil
}
```

`bin/gate --list --json` returns:

```json
[
  {
    "name": "build-time",
    "description": "wall-clock of `go build ./...`",
    "schedule": { "kind": "after-commit" },
    "timeout_seconds": 600,
    "metrics": {
      "build_seconds": {"type": "float", "direction": "lower-is-better", "mode": "enforced"}
    },
    "flags": [
      {"name": "fresh", "type": "bool", "description": "ignore the build cache"}
    ]
  },
  { "name": "lint", "description": "`go vet ./...` warning count", "schedule": { "kind": "after-commit" },
    "metrics": {"lint_warnings": {"type": "int", "direction": "lower-is-better", "mode": "enforced"}} }
]
```

`bin/gate build-time` writes to stdout:

```json
{ "schema": 2, "gate": "build-time", "metrics": {"build_seconds": 42.7} }
```

Exit 0; the orchestrator parses the metric, compares to baseline, ratchets
or flags a regression. On a broken build, `bin/gate build-time` exits 1 and
still emits `{ "schema": 2, "gate": "build-time", "error": "build failed: …",
"error_context": "…" }` so the failure is debuggable from captured stdout.

Both gates above are **target-agnostic**. A test gate is **target-bound** — it
produces a different result per target, so it declares its `Targets` and reads
the selected one from `RunCtx.Target`:

```go
gate.Gate{
    Name: "test",
    Targets: []gate.Target{
        {Triple: "linux-amd64", HostOS: "linux"},
        {Triple: "darwin-arm64", HostOS: "darwin"},
        {Triple: "wasm32-wasi", HostOS: "any"},
    },
    Metrics: map[string]gate.MetricConfig{
        "test_failures": {Type: gate.MetricInt, Direction: gate.LowerIsBetter, Mode: gate.ModeEnforced},
    },
    Run: runTests, // builds/runs for rc.Target; emits files[] + "target": rc.Target
}
```

The orchestrator fans it out — one invocation per target, each on a
`host_os`-capable machine — and keeps a baseline per `(gate, target)`:

```
bin/gate test                         # on a linux host → defaults to linux-amd64 (host-native)
bin/gate test --target linux-amd64    # on a linux host → "target":"linux-amd64"
bin/gate test --target darwin-arm64   # on a mac host   → "target":"darwin-arm64"
bin/gate test --target wasm32-wasi    # any host        → "target":"wasm32-wasi"
bin/gate test --target windows-x86    # on a linux host → exit 2: host can't run host_os "windows"
```

So a manual `bin/gate test` "just works" on a host with one native target. It
exits 2 only when there's no single native target to default to, when
`--target` is outside the declared set, or when the named target can't run on
this host.

## Open questions

1. **The user-attention API.** Gates may need to raise something for a human
   (e.g. "this gate has been red for 3 days"). The shape is undefined; the
   `RunCtx` struct reserves the seam so it can be added without breaking
   handler signatures. What is its surface, and does it write through the
   orchestrator or directly? **TBD — blocks nothing else here.**

2. **Health-ledger semantics for `complete`.** `complete: true` tells the
   ledger a run is authoritative coverage for `(gate, target)`; how it uses
   that (e.g. flagging tests that vanished from the roster between runs) is the
   consumer's design, still TBD. There is **no** implicit-pass backfilling —
   that idea was replaced by the `--report-failures-only` projection.

3. **Concurrency caps.** Per-host + global parallelism belongs in the
   orchestrator, not the SDK. `bin/gate <name>` is single-process,
   single-gate; the orchestrator caps how many it dispatches at once.

4. **Output redaction.** Secrets (AWS keys, private keys, Bearer tokens,
   `*_TOKEN`/`*_SECRET` env values) must not leak into gate stdout. The SDK
   could expose a `gate.Redact` helper as defense-in-depth, but the
   orchestrator should also redact at ingest.

5. **Env injection.** With in-process gates (the `Gate.Run` model), the gate's
   env is simply the binary's env at invocation time — the orchestrator sets
   it before spawning `bin/gate`. Adequate for v1.

## Why this shape

- **Stable wire format ≠ Go API.** Anyone can author a gate in shell,
  Python, etc. The Go subpackage is convenience for Go authors; the protocol
  and the two JSON contracts are the real interface.
- **Strict by construction.** A gate result feeds ratchets and dashboards;
  a silently-swallowed flag or typo would score as a pass/fail nobody chose.
  Rejecting unknown input with exit 2 keeps a misconfigured orchestrator
  loud.
- **Self-contained contract.** The output and definition JSON stand on their
  own — no orchestrator's internal schema is baked in, so a tracker, CI, or a
  homegrown scheduler can each consume the same binary.
- **No scheduler in the SDK.** Schedulers are stateful and opinionated;
  every orchestrator has its own. The SDK provides what an orchestrator needs
  to *discover* and *invoke* a gate, no more.

## Migration path

1. Land `flow/gate` types + `Emit` + `Main` (a self-contained package; no
   changes to `flow`, `cli.App`, `flow.Backend`, or `flow.StepCtx`).
2. Add a `cmd/gate` example binary (a build-time gate is the obvious first
   one) and wire it into `./make`.
3. Update [docs/design.md](../design.md) with a short top-level "Gates
   (optional)" section linking back here, noting they are a **separate
   binary** from any flow binary.
4. **In the orchestrator** (separate change): an `import-gates <binary>`
   command that invokes `bin/gate --list --json` and merges the result into
   its own gate registry.

Gates are entirely additive — they touch no existing flow type and ship in
their own binary.
