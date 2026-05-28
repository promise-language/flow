# Proposal: structured gates in the flow SDK

**Status:** draft, not yet implemented
**Author:** initial sketch
**Related:** [docs/design.md](../design.md), tracker's `gates.go` /
`store.go` /`health.go`

## Goal

Let a flow binary host one or more **gates** — periodic, structured checks
whose stdout JSON feeds an orchestrator (tracker, GitHub Actions, a
homegrown scheduler) and powers ratcheting metrics + health dashboards.
Gates are independent of the per-issue claim/run-step lifecycle: they run
on a schedule against the project's repo, not against a single item.

Concretely, this proposal adds:

1. A **stable wire format** for gate stdout (mirrors the tracker's
   existing format so tracker tooling Just Works).
2. A **`flow/gate` Go subpackage** with type defs + tiny emit helper so
   Go-authored gates are typed end-to-end.
3. An **optional `cli.App.Gates` slice** so a flow binary can host both
   flows and gates from one entry point.
4. A **discovery contract**: orchestrators call `./binary gate manifest`
   to enumerate gates; `./binary gate run <name>` to execute one.

The SDK ships **no scheduler**. Scheduling, ratcheting, baseline
storage, dashboarding stay in the orchestrator. The SDK is concerned only
with the gate definition / invocation / output boundary.

## What the tracker does today (reference)

For context, here's the shape this proposal mirrors.

### Tracker `Gate` definition (committed to `_gates.json`)

```go
type Gate struct {
    Name           string
    Command        string                  // shell command
    Schedule       string                  // "every 4h" | "daily" | "after-every-commit" | "manual"
    OS             string                  // "darwin"|"linux"|"windows"|""
    AgentMatch     string                  // glob
    HostMatch      string                  // glob list
    HostExclude    string
    Timeout        string                  // "15m"
    Enabled        bool
    OnFailure      string                  // "bug"|"inbox"
    Tags           []string
    AllowDirtyTree bool
    Format         string                  // "json" or "" (exit-code legacy)
    MetricConfigs  map[string]MetricConfig // direction + mode + ratchet_cap
    Arena          *GateArenaConfig        // sandboxed-execution provisioning
    Env            map[string]string       // creds, redacted from output
    Axes           []MetricAxis            // chart axis groupings
    BackfillPasses bool                    // implicit pass on clean runs with empty tests array
}

type MetricConfig struct {
    Direction  string   // "lower-is-better" | "higher-is-better"
    Mode       string   // "enforced" | "pending" | "informational"
    RatchetCap *float64 // baseline stops auto-advancing at this value
}
```

### Tracker stdout JSON contract

```jsonc
{
  "metrics": {
    "build_seconds": 42.7,
    "tests_passed": 87,
    "tests_failed": 0,
    "lint_warnings": 3
  },
  "tests": [
    {
      "test_set": "promise",          // optional ID of the test enumeration
      "target":   "pkg/foo",
      "file":     "pkg/foo/foo_test.go",
      "test":     "TestBar",
      "outcome":  "pass",             // "pass" | "FAIL" | "TIMEOUT" | "LEAK"
      "elapsed":  0.42,
      "context":  ""                  // failure message when not pass
    }
  ],
  "complete": "promise"               // optional: this gate run fully enumerated test set "promise"
}
```

The tracker parses this from stdout. `metrics` drives ratcheting baselines;
`tests` feeds the per-test health ledger; `complete` lets the ledger credit
implicit passes to non-listed tests in the named set (so "the gate enumerates
EVERY promise test" is a recordable fact). Non-zero exit code = gate failed;
metrics still parsed if present.

## Proposed SDK pieces

### 1. The wire format (verbatim, language-independent)

A gate's stdout MUST be a single JSON object matching the schema in
[`docs/state-block-spec.md`][1] (extended with the gate section). The schema
copies the tracker's tested shape so existing tracker tooling works
unchanged. The contract:

```jsonc
// flow:gate-output-v1
{
  "schema": 1,                                  // versioning
  "metrics": { "<name>": <number>, ... },       // optional
  "tests":   [ <TestEntry>, ... ],              // optional
  "complete": "<test-set-id>",                  // optional
  "infra_error": false,                         // optional: "gate didn't really run; ignore"
  "error": "<short reason>"                     // optional: error string
}
```

Test entry shape matches tracker's `GateTestEntry`. Stable across orchestrators.

[1]: ../state-block-spec.md

### 2. The `flow/gate` Go subpackage

A small new package — types + emit helper. Mirrors the way `flow/claude/`
sits next to `flow.Agent` but in its own subpackage so the root pulls in
zero transitive deps.

```go
package gate

// Output is the stdout JSON envelope a gate writes on exit. Matches
// flow:gate-output-v1 (see docs/proposals/gates.md).
type Output struct {
    Schema     int                `json:"schema"`               // 1
    Metrics    map[string]float64 `json:"metrics,omitempty"`
    Tests      []TestEntry        `json:"tests,omitempty"`
    Complete   string             `json:"complete,omitempty"`
    InfraError bool               `json:"infra_error,omitempty"`
    Error      string             `json:"error,omitempty"`
}

type TestEntry struct {
    TestSet string  `json:"test_set,omitempty"`
    Target  string  `json:"target"`
    File    string  `json:"file,omitempty"`
    Test    string  `json:"test,omitempty"`
    Outcome Outcome `json:"outcome"`
    Elapsed float64 `json:"elapsed,omitempty"`  // seconds
    Context string  `json:"context,omitempty"`
}

type Outcome string

const (
    OutcomePass    Outcome = "pass"
    OutcomeFail    Outcome = "FAIL"
    OutcomeTimeout Outcome = "TIMEOUT"
    OutcomeLeak    Outcome = "LEAK"
)

// Emit writes out to stdout as a single JSON line and returns the
// process exit code the caller should use. Convention: non-zero exit on
// any failed test OR explicit Output.Error, zero otherwise.
func Emit(out Output) int { ... }

// Def is a registered gate's declarative descriptor — what `gate manifest`
// surfaces to the orchestrator.
type Def struct {
    Name           string             `json:"name"`
    Description    string             `json:"description,omitempty"`
    Schedule       string             `json:"schedule,omitempty"`       // "every 4h" | "daily" | ...
    Timeout        time.Duration      `json:"timeout,omitempty"`
    OS             string             `json:"os,omitempty"`             // "darwin"|"linux"|"windows"|""
    Tags           []string           `json:"tags,omitempty"`
    MetricConfigs  map[string]Config  `json:"metric_configs,omitempty"`
    AllowDirtyTree bool               `json:"allow_dirty_tree,omitempty"`
    BackfillPasses bool               `json:"backfill_passes,omitempty"`

    // Run is the in-process handler. The handler builds an Output and
    // returns it; the cli wraps Emit + exit code.
    Run func(ctx context.Context) (Output, error) `json:"-"`
}

type Config struct {
    Direction  Direction `json:"direction"`             // lower-is-better | higher-is-better
    Mode       Mode      `json:"mode"`                  // enforced | pending | informational
    RatchetCap *float64  `json:"ratchet_cap,omitempty"`
}

type Direction string
const (
    LowerIsBetter  Direction = "lower-is-better"
    HigherIsBetter Direction = "higher-is-better"
)

type Mode string
const (
    ModeEnforced       Mode = "enforced"
    ModePending        Mode = "pending"
    ModeInformational  Mode = "informational"
)
```

Two things stay deliberately OUT of the SDK:

- **Baselines / ratcheting state.** State lives in the orchestrator
  (tracker stores it in `_gates_state.json`); the SDK never touches disk
  for gate state. This keeps the SDK stateless across `gate run`
  invocations.
- **Scheduler.** No goroutine timers, no cron parsing. The orchestrator
  decides when to invoke `gate run <name>`.

### 3. `cli.App.Gates` integration

Optional new field on `cli.App`:

```go
type App struct {
    // ... existing fields ...
    Gates []gate.Def    // OPTIONAL — gates this binary hosts
}
```

When `Gates` is non-empty, `cli.Run` registers three subcommands:

| command | behavior |
|---|---|
| `gate list` | one line per registered gate, human-readable |
| `gate manifest` | machine-readable JSON: `[]gate.Def` (without the `Run` func) — what orchestrators consume to discover |
| `gate run <name>` | invokes the named gate's `Run`, emits the JSON envelope on stdout, exits non-zero on test failure or `Output.Error` |

A flow binary with no gates omits these commands transparently — no
behavior change for existing binaries.

### 4. Discovery flow (orchestrator side)

```
orchestrator (tracker, CI, custom):
  1. Run `./fix gate manifest`        → []gate.Def JSON
  2. For each gate, decide when to run (cron, post-commit, manual)
  3. Run `./fix gate run <name>`      → reads Output JSON from stdout
  4. Parse, compare metrics to baselines, ratchet, alert, etc.
```

Tracker can keep its existing `Gate` schema in `_gates.json` and treat the
manifest as the source of truth — periodically re-fetch via `gate manifest`
to pick up new gates the project added. A `tracker import-gates ./fix`
command becomes trivial.

## Worked example

A flow binary that runs both the contributor+maintainer flow AND a
build-time + lint gate:

```go
package main

import (
    "context"
    "os"
    "time"

    "github.com/promise-language/flow"
    "github.com/promise-language/flow/cli"
    "github.com/promise-language/flow/claude"
    "github.com/promise-language/flow/gate"
    ghbackend "github.com/promise-language/flow/pkg/backend/github"
)

func main() {
    backend, _ := ghbackend.NewBackend(ghbackend.Config{BinaryName: "fix"})

    os.Exit(cli.Run(cli.App{
        Name:      "fix",
        Backend:   backend,
        Agent:     claude.New(),
        Artifacts: artifacts(),
        Flows:     flows(),
        Gates: []gate.Def{
            {
                Name:        "build-time",
                Description: "wall-clock time of `go build ./...`",
                Schedule:    "after-every-commit",
                Timeout:     10 * time.Minute,
                MetricConfigs: map[string]gate.Config{
                    "build_seconds": {Direction: gate.LowerIsBetter, Mode: gate.ModeEnforced},
                },
                Run: runBuildTimeGate,
            },
            {
                Name:        "lint",
                Description: "`go vet ./...` warning count",
                Schedule:    "after-every-commit",
                MetricConfigs: map[string]gate.Config{
                    "lint_warnings": {Direction: gate.LowerIsBetter, Mode: gate.ModeEnforced},
                },
                Run: runLintGate,
            },
        },
    }))
}

func runBuildTimeGate(ctx context.Context) (gate.Output, error) {
    start := time.Now()
    cmd := exec.CommandContext(ctx, "go", "build", "./...")
    if err := cmd.Run(); err != nil {
        return gate.Output{
            Schema:  1,
            Error:   "build failed: " + err.Error(),
        }, nil
    }
    return gate.Output{
        Schema:  1,
        Metrics: map[string]float64{"build_seconds": time.Since(start).Seconds()},
    }, nil
}
```

`./fix gate manifest` returns:

```json
[
  {
    "name": "build-time",
    "description": "wall-clock time of `go build ./...`",
    "schedule": "after-every-commit",
    "timeout": 600000000000,
    "metric_configs": {
      "build_seconds": {"direction": "lower-is-better", "mode": "enforced"}
    }
  },
  { "name": "lint", ... }
]
```

`./fix gate run build-time` writes to stdout:

```json
{
  "schema": 1,
  "metrics": {"build_seconds": 42.7}
}
```

Exit code 0; the orchestrator parses the metric, compares to baseline,
ratchets or flags regression.

## Open questions

1. **Should gates and flows share an `Agent`?** Probably no — gates are
   side-effect-free measurements; they shouldn't pull on the prompt
   budget. But a gate might want to invoke the agent for, say, "ask Claude
   to rate the readability of this PR". TBD whether to expose `ctx.Agent`
   inside the gate context.

2. **Test-set semantics.** Tracker's `complete` field is subtle — it
   means "this gate enumerated every test in the named set". When the
   tests array is empty AND `complete: "promise"` is set, every test
   previously seen in that set gets credited an implicit pass. The SDK
   should adopt the field verbatim but document it carefully.

3. **Concurrency caps.** Tracker has per-host + global concurrency caps.
   Those belong in the orchestrator, not the SDK. The SDK's `gate run`
   invocation is single-process, single-gate; the orchestrator caps how
   many it dispatches in parallel.

4. **Output redaction.** Tracker redacts AWS keys, private keys, Bearer
   tokens, and env-var values whose keys match `KEY/SECRET/TOKEN/...` from
   gate stdout. The SDK could expose a `gate.Redact` helper but this is
   defense-in-depth; the orchestrator should also redact at ingest.

5. **`Env` injection.** Tracker injects per-gate env vars into the
   subprocess. With in-process gates (the `Def.Run` model above), the env
   is just the binary's env at invocation time — the orchestrator sets it
   before spawning the binary. Adequate for v1.

6. **Should gates be allowed inside `flow.Backend`-style pluggability?**
   I.e., should tracker provide a `flow.Gate` implementation? Likely no —
   gates are emitters, not orchestrated lifecycles. The asymmetry is fine.

## Why this shape

- **Stable wire format ≠ Go API.** Anyone can author a gate in shell,
  Python, etc. The Go subpackage is convenience for Go authors, not the
  contract.
- **No scheduler in the SDK.** Schedulers are stateful and opinionated;
  every orchestrator has its own. The SDK provides what an orchestrator
  needs to *invoke* a gate, no more.
- **One binary can host flows + gates.** Reuses the cli machinery; the
  user doesn't need a second binary just to publish a build-time metric.
- **Tracker compatibility is free.** The wire format IS tracker's
  existing format; the manifest is a faithful subset of tracker's `Gate`
  schema. A `tracker import-gates ./fix` command is a few-line addition
  on the tracker side.

## Migration path

1. Land `flow/gate` types + helpers (no cli changes).
2. Add `cli.App.Gates` + the three subcommands in the cli package.
3. Add a test gate to `examples/verify` (it's the obvious home for a
   build-time gate).
4. Update `docs/design.md` with a top-level "Gates (optional)" section
   linking back here.
5. **In the tracker repo** (separate change): a `tracker import-gates
   <binary>` command that invokes `<binary> gate manifest` and merges
   the result into `_gates.json`.

No changes required to `flow.Backend`, `flow.StepCtx`, or any existing
type — gates are additive.
