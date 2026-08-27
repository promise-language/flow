# Proposal: step identity, validated grants, and park-driven top-up

**Status:** implemented
**Author:** initial sketch
**Related:** [docs/design.md](../design.md),
[docs/proposals/gates.md](gates.md) (the TTY→JSON output convention this
proposal adopts for the flow CLI)

## Goal

Make `grant` safe and self-driving for the agent that invokes it:

1. **One identity per step.** The result id (`plan`, `implementation`) is the
   only name a step answers to. Every other spelling — the human label, a
   signal id, a typo — is refused with a loud error naming the legal ids.
2. **Every grant input validated** before any write reaches the backend.
   Today the CLI validates nothing.
3. **`flow grant` with no arguments does the obvious thing** — it reads why
   the item parked and tops up exactly the axis that parked it. `grant` is
   almost always a reaction to a park; that should be the default path, not
   the one requiring the most typing.
4. **Machine output when piped.** No tool driving a flow should be parsing
   `[~] implementation` out of human text.

## The problems, concretely

### 1. `grant` accepts anything

[`cmdGrant`](../../cli/cmd_grant.go) takes `fs.Arg(0)` and hands it straight
to `Backend.Grant`. There is no check that the string names a step of the
item's flow, that the step carries a budget, or that a budget record exists.

The bundled backends do refuse an unknown key — `fake.bumpField` returns
`artifact %q not seeded on item %q`, and the github backend's
`mutateArtifact` returns `artifact %q not found in state doc` — but a backend
that upserts silently accepts the write, and the operator gets a success
message for a grant that went nowhere. The check belongs in the CLI, which is
the only layer that has the flow definition in hand and can say *what the
legal ids are*.

### 2. Two names per step, and the display teaches the wrong one

```go
contributor.AddStep("write plan", "plan", stepPlan, flow.StepConfig{})
//                   ^^^^^^^^^^^  ^^^^^^
//                   label        id  ← the only thing grant accepts
```

`printFlowChecklist` prints the label first and the id in parentheses:

```
  [ ] write plan (plan)
```

Both usage strings then have to apologize for it — `<artifact-id> is the id
passed to AddStep, NOT the human step name` appears in
[cli/app.go](../../cli/app.go) *and* [cli/help.go](../../cli/help.go) *and*
README's command table. Three warnings against one display is a sign the
display is wrong, not the reader.

### 3. Only artifact steps are grantable, and nothing says so

`Flow.SeedSpec` emits `ArtifactSpec` entries for `stepArtifact` steps only.
`AddSignalStep` and `AwaitSignal` steps own no budget record, so granting to
`pr-open` can only ever produce an obscure backend error. Neither `status`
nor `--help` distinguishes the two kinds.

### 4. Park is write-only

The github backend's `Park` writes a `flow:budget-exhausted:<step>` label and
a `<!-- flow:park -->` timeline comment; its own comment says the state
comment's `park` field is *"not yet in the schema"*. `LoadState` reads
neither. The fake keeps the park in memory behind a test-only accessor.
`unpark` appears in zero `.go` and zero `.md` files in the repo.

So the CLI cannot see why an item parked — which is precisely the
information `grant` needs.

### 5. `status` and `list` emit human text only

`resolve` already gets the stream split right: stdout carries nothing but
`InvocationResult` JSON, human progress goes to stderr (see the comment above
the loop in [cli/cmd_resolve.go](../../cli/cmd_resolve.go)). It took no
`--json` / `--human` at the time of writing — that was fixed separately, and
the JSON is now gated on the mode while the stderr narration stays. `status`,
`list`, and `grant` print for humans unconditionally, so a tool has to parse
`[x]` / `[~]` / `[ ]` markers to learn anything.

## Principle

> **The result id is the step's identity. The label is decoration.**

Everything below follows from that, plus one corollary for park:

> **A park record that is readable must also be clearable.** Exposing park
> state without clearing it on grant is worse than today's write-only park.

## 1. Vocabulary and display

Rename throughout code comments, help, and docs:

- **step id** — the `ArtifactId` / `SignalId` (`plan`, `pr-open`). The
  grant target. The `status` key.
- **step label** — the `AddStep` first argument (`"write plan"`). Display
  only; never accepted as input anywhere.

No SDK signature changes. `AddStep(label, id, handler, cfg)` keeps its shape.

`printFlowChecklist` leads with the id in an aligned column, and shows budget
consumption on artifact steps that have any:

```
contributor
  [x] plan             write plan                  1/3 inv · $2.40/$10.00
  [~] implementation   implement the change        3/3 inv · $9.80/$10.00
  [ ] review           review the work
  [ ] pr-open          create pull request         (signal — no budget)
  [ ] pr-merged        await pr-merged             (signal — no budget)
```

The `(signal — no budget)` tag is the human-readable form of "not a grant
target". It replaces the three separate warnings in the usage strings: the
listing itself now teaches which ids exist and which can take a grant.

Add one SDK helper so the CLI resolves ids without re-walking `Items()`:

```go
// ItemByResult returns the LifecycleItem whose Result() is key.
func (f *Flow) ItemByResult(key string) (LifecycleItem, bool)
```

## 2. Validated grant input

A new `resolveGrantTarget` runs **before any backend write**. Order:

1. Require an active claim, then `LoadState` (needed for the seeded records
   regardless).
2. Resolve the flow with `flowForType(app, state.Item.Type)` — the same
   selector `status` uses, so it also works on parked and finalized items,
   which is when `grant` is actually typed.
3. **Exact match** of the argument against the flow's artifact-step ids →
   accept. No prefix matching, no case folding, no fuzzy acceptance.
4. Argument matches a step **label**:
   `grant: "write plan" is a step label, not a step id — did you mean "plan"?`
5. Argument matches a **signal** id:
   `grant: step "pr-open" is a signal step and carries no budget (nothing to grant)`
6. No match at all:
   ```
   grant: unknown step id "push"
          valid ids for flow "contributor": plan, implementation, review, coverage, verify-impl
   ```
   plus a `did you mean "plan"?` line on a near match (case-insensitive
   equality or edit distance 1).
7. Id is in the flow but **absent from `state.Artifacts`** — the item has not
   been seeded:
   `grant: item not seeded — no budget record for "plan" (run \`run-step\` once first)`
   This is the case that silently created junk records on an upserting
   backend.
8. Id is in the seeded state but **no longer in the flow** (flow source
   changed after the item was seeded): **accept**, with a warning on stderr.
   The budget record is real and the operator's intent is unambiguous;
   refusing would strand mid-flight items whose flow definition has moved on.
9. Existing checks retained: negative flag values, "at least one flag set" in
   manual mode. Added: empty / whitespace-only argument.

**Exit codes.** Validation failures exit **2** (usage error, consistent with
the rest of the CLI); backend failures exit 1. No validation path writes.

The success message loses the `%+v` struct dump:

```
granted implementation: invocations 3 → 4
```

## 3. Park becomes readable, and grant clears it

### Read path

Add to `ItemState`:

```go
// Park is the item's current park record, or nil if it is not parked.
// Populated by Backend.LoadState. Backends with no park concept leave it nil.
Park *ParkRequest
```

- **github** — move park into the state doc, the field its own code comment
  already anticipates. `LoadState` fetches that comment already, so this
  costs zero extra API calls, and a single mutable field supersedes itself
  instead of accumulating timeline comments. The label and the timeline
  comment stay: they are for humans and for history.
- **fake** — surface the `parkRequest` it already holds.
- **tracker** (closed) — leaves the field nil until it populates it, and
  degrades to the fallback in §4. No compile break.

### Write path — clearing is part of `Backend.Grant`'s contract

Amend the documented contract on the existing `Backend.Grant`:

> `Grant` MUST clear a `ParkBudgetExhausted` park when the grant raises the
> parked step's offending axis above its consumption. Parks of any other
> kind, and grants that do not actually clear the cap, are left untouched.

In the backend rather than the CLI, because grant is not reached only through
this CLI — a tracker-UI grant must drop the label too.

- **github** — remove the `flow:budget-exhausted:<step>` label, clear the
  state-doc `park` field.
- **fake** — nil `rec.parkRequest`.
- **tracker** — contract change on a method it already implements; needs
  flagging to its owner. Until done, its `status` shows a stale park.

**The condition matters.** `grant implementation --cost 0.01` against a step
that is $2.40 over does not unpark anything and must not claim to:

```
granted implementation: cost $10.00 → $10.01
still parked: $12.40 spent exceeds the $10.01 cap — grant at least $2.40 more to resume
```

with `"unparked": false` in the JSON payload. Park-driven mode (§4) satisfies
the condition by construction.

### Implementation note: `ParkRequest.Step` carries the id

Found while implementing, and worth stating because it is a wire-format
change: `ParkRequest.Step` was documented as the "lifecycle item name" and the
orchestrator filled it with the **label**. A backend cannot map a label back to
the artifact record whose budget caused the park — it has no flow definition —
so `Grant` could not have decided whether a park was satisfied.

`ParkRequest.Step` is now the step **id** (`LifecycleItem.Result()`), which is
the same principle this proposal applies everywhere else. Consequences:

- the github park label is now `flow:budget-exhausted:plan` rather than
  `flow:budget-exhausted:write plan` (no spaces — an improvement on its own);
- a park written by an older version recorded the label, so the CLI accepts a
  label in `park.Step` too and maps it to the id, rather than stranding items
  parked across the upgrade;
- backends that render `park.Step` in a UI (the closed tracker) will show the
  id — which is also what an operator has to type into `grant`.

`InvocationResult.Step` is unchanged: it is a progress/result field, not an
identity used to look anything up.

### Implementation note: state-doc writes must edit in place

Also found while implementing. The github backend had a `docFromState` helper
that rebuilt the whole state document from a `flow.ItemState` snapshot, and
`markSignalSetOnState` (the pr-open / pr-merged side-effect writer) used it.
A rebuild carries only what `ItemState` models — so the moment `park` joined
the document, opening a PR would have silently erased the park of a
budget-exhausted item.

Every write path now edits the loaded document in place through
`mutateStateDoc`, and the rebuild helpers are deleted rather than left as a
trap for the next writer. Two consequences worth noting: signal writes now
resolve the state comment by scanning when the claim token carries no comment
id (as every other mutator already did, instead of failing), and park-label
removal is best-effort — `Grant` is not idempotent, so failing it over a
leftover label would invite a retry that grants the budget twice.

### Note on what "unpark" does and does not mean

No dispatch path gates on park. `RunOne`'s budget gate re-reads
`art.Invocations >= art.GrantedInvocations` live, so raising the cap is
already sufficient for the next `run-step` to proceed. Clearing the park
record is about the *reported* state not lying — which starts mattering the
moment `ItemState.Park` exists and `status` prints it.

## 4. `grant` modes — park-driven by default

| invocation | behavior |
|---|---|
| `flow grant` | read the park; top up **exactly** the parked step's parked axis |
| `flow grant --all` | blanket sweep: every pending step gets headroom over consumption |
| `flow grant <id> --flags` | manual additive grant (today's behavior, now validated) |
| `flow grant --all <id>` | rejected, exit 2 |
| `--dry-run` | valid in all modes: print the plan, write nothing |

### Park-driven (default)

Read `state.Park`, act on `Park.Axis` alone:

| `Axis` | action | default increment |
|---|---|---|
| `AxisInvocations` | `granted = used + N` | 1 |
| `AxisCost` | `granted = spent + N` | the step's configured `MaxCostUSD`, floor $1 |
| `AxisPrompts` | `granted += N` — a per-invocation cap, not a cumulative meter (`PromptsThisInvocation` resets on every `BumpInvocations`) | 1 |
| `AxisTimeout` | `granted += N` — a duration, not a meter | the step's configured `Timeout`, i.e. one more full run |

Flags override the increment (`flow grant --invocations 5` = "give the parked
step 5, not 1") and are **rejected when they name an axis other than the
park's** — no silently granting an axis nobody asked for.

**Refusals** (exit 2, nothing written):

- **Park kind is not `ParkBudgetExhausted`:**
  `grant: item is parked on a question (q1: "which base branch?"), not a budget cap — answering it is what unparks this; grant would do nothing`.
  Same shape for `ParkBlocked`, `ParkStepDidNotResolve`, and
  `ParkInfraTransient` (which just needs a re-run). This is the most valuable
  refusal in the proposal: a tool that grants at a question-park burns a
  write and stays stuck.
- **No park recorded, or the backend does not populate `Park`:**
  ```
  grant: no park recorded on owner/repo#123 — nothing to top up.
         Use `grant <step-id> --invocations N` to grant explicitly,
         or `grant --all` to sweep every pending step.
  ```
  Deliberately *not* a silent fallback to `--all`: quietly touching every
  step when we do not know what blocked is the same class of surprise as the
  unvalidated id this proposal exists to remove.

**Stale park** (the parked step has since resolved, or its cap is already
above consumption) → `grant: park on "implementation" is stale (step resolved
at 14:02) — nothing to grant`, exit **0**, no write.

### `--all` — blanket sweep

For every *pending* artifact step (seeded, unresolved, not operator-opted-out),
raise each axis to at least `consumed + headroom`:

| axis | target | default headroom |
|---|---|---|
| invocations | `rec.Invocations + h` | 1 |
| cost | `rec.CostUSDSpent + h` | the step's configured `MaxCostUSD`, floor $1 |
| prompts/invocation | `max(granted, seeded default, 1)` | — |
| timeout | untouched unless `--timeout` is given | — |

Because the target is a `max`, a step that already has headroom yields a zero
delta and **no backend call** — which matters on github, where every `Grant`
rewrites the state comment. Resolved steps are skipped entirely.

```
plan             unchanged (has headroom)
implementation   invocations 3 → 4, cost $10.00 → $19.80
review           invocations 3 → 4
topped up 2 steps, 1 unchanged
```

Nothing to do is exit 0 with `all pending steps already have headroom`.

### One documented wart

`--invocations N` means "add N" in manual mode and "ensure N above
consumption" under `--all`. The two modes are visibly different at the call
site, so this is documented rather than split into a second flag set
(`--headroom-invocations`). Open to reversing if the ambiguity bothers you.

## 5. Output modes: human on a TTY, JSON when piped

Same convention [gates.md](gates.md) already specifies for `bin/gate --list`.

**Detection.** `go.mod` carries only `go-github` and `yaml.v3`, and
`golang.org/x/term` is not worth adding for this: type-assert `app.Out` to
`*os.File` and test `fi.Mode()&os.ModeCharDevice != 0`. Not a char device
(pipe, file, redirect) → JSON.

Resolved once in `RunWithArgs` into a new `App.Output` field
(`OutputAuto | OutputHuman | OutputJSON`), so every command consults one
field instead of reaching for `os.Stdout`:

- `--json` / `--human` flags override per invocation — explicit beats
  inference, needed when a human pipes to `less`.
- `FLOW_OUTPUT=json|human` overrides for a harness that cannot thread a flag
  through every call site.
- Help and usage text stay human in both modes; they are for a person by
  definition.

**Test impact — the part that bites.** Every existing test assigns
`app.Out = &bytes.Buffer{}`, which is not a char device, so naive
auto-detection flips the whole suite to JSON and breaks assertions like the
doctor glyph check and the `"usage:"` checks in `help_test.go`. Therefore:
`App.Output` defaults to `OutputHuman` when `Out` is not an `*os.File`, and
auto-detection is triggered by the binary entry point (`Run`). Tests keep
asserting human text unless they opt in; a real piped invocation still gets
JSON.

**Errors stay plain text on stderr in both modes**, with the exit code as the
real signal. Emitting error JSON on stdout would force a tool to
disambiguate success from failure payloads on the same stream; the two-stream
split is what `resolve` already does.

### `status` — one object

```json
{
  "item": "owner/repo#123",
  "owner": "george",
  "flow": "contributor",
  "flow_state": "eligible",
  "finalized": false,
  "park": {
    "kind": "budget-exhausted",
    "step": "implementation",
    "axis": "invocations",
    "reason": "ran 3 times without resolving \"implementation\""
  },
  "steps": [
    {
      "id": "plan", "label": "write plan", "kind": "artifact",
      "state": "resolved", "required": true,
      "budget": {
        "invocations": {"used": 1, "granted": 3},
        "cost_usd": {"used": 2.4, "granted": 10},
        "prompts_per_invocation": {"granted": 1},
        "timeout_seconds": {"granted": 1800}
      }
    },
    {
      "id": "pr-open", "label": "create pull request", "kind": "signal",
      "state": "pending", "required": true, "budget": null
    }
  ],
  "questions": [{"id": "q1", "text": "which base branch?", "answered": false}]
}
```

- `kind`: `artifact | signal | await`.
- `state`: `pending | resolved | stale | skipped` (closed enum).
- `budget: null` is the machine-readable form of "not a grant target" — the
  same fact the human listing shows as `(signal — no budget)`.
- `park` is what makes the loop closeable: a tool reads it, calls bare
  `flow grant`, and never guesses.

### `list`

```json
{"items": [{"id": "123", "display": "owner/repo#123", "owner": "george"}]}
```

An empty eligible set is `{"items": []}`, not `(no eligible items)`.

### `grant`

```json
{
  "mode": "park",
  "park": {"kind": "budget-exhausted", "step": "implementation", "axis": "invocations"},
  "granted": [{"id": "implementation", "invocations": {"from": 3, "to": 4}}],
  "unchanged": [],
  "unparked": true,
  "dry_run": false
}
```

`mode` is `park | all | manual`. `unparked` reports the §3 condition
honestly.

## 6. Sequencing

Each step is independently shippable; stopping after any of them leaves the
CLI better than it is now.

1. **Park read + clear, together.** `ItemState.Park`, github state-doc field,
   fake surfacing, and park-clearing in `Grant`. Shipping the reader without
   the clearer regresses `status`, so they are one unit.
2. **Identity and validation.** `ItemByResult`, the id-first checklist, the
   `resolveGrantTarget` refusals, usage/README rewording.
3. **Park-driven grant**, with `--all` and manual as the other two modes.
4. **Output modes** for `status`, `list`, `grant`.

## Files touched

| file | change |
|---|---|
| [backend.go](../../backend.go) | `ItemState.Park`; `Backend.Grant` contract amendment |
| [flow.go](../../flow.go) | `Flow.ItemByResult` |
| [pkg/backend/github/artifact.go](../../pkg/backend/github/artifact.go) | park in the state doc; `Grant` clears park + label |
| [pkg/backend/github/state_comment.go](../../pkg/backend/github/state_comment.go) | `park` field in the state doc schema; lossy rebuild helpers removed |
| [pkg/backend/github/signal.go](../../pkg/backend/github/signal.go) | signal writes edit the doc in place so they cannot erase the park |
| [pkg/backend/fake/fake.go](../../pkg/backend/fake/fake.go) | surface park in `LoadState`; clear on `Grant` |
| [cli/cmd_grant.go](../../cli/cmd_grant.go) | validation, three modes, `--dry-run` |
| [cli/cmd_status.go](../../cli/cmd_status.go) | id-first checklist, budget column, park line |
| [cli/cmd_list.go](../../cli/cmd_list.go) | payload instead of `Fprintf` |
| `cli/output.go` *(new)* | mode resolution + payload structs + renderer |
| [cli/app.go](../../cli/app.go), [cli/help.go](../../cli/help.go) | `App.Output`, usage rewording |
| [README.md](../../README.md) | command table, output-modes section |

## Tests

- **Grant validation** — unknown id, label-instead-of-id, signal id, unseeded
  id, id-dropped-from-flow (accepted + warns); each asserting **no backend
  write occurred**.
- **Park-driven** — each axis maps to the right increment; non-budget park
  kinds refuse; missing park refuses with the fallback hint; stale park exits
  0 without writing; a flag naming the wrong axis is rejected.
- **Unpark** — park cleared when the grant clears the cap; park *retained*
  and `unparked: false` when it does not.
- **`--all`** — tops up only pending steps, no-ops when headroom exists, and
  `--dry-run` writes nothing.
- **Status/list/grant JSON** — golden-payload tests pinning field names and
  the `kind` / `state` / `mode` enums against accidental renames.
- **Existing suite** — must pass unchanged, which is the check on the
  `OutputHuman`-for-non-`*os.File` default.

## Open decisions

1. **Flag-semantics wart** (§4) — same flag name meaning "add" vs "ensure
   above consumption". Documented, or split into `--headroom-*`?
2. **Grant as a state transition** — folding park-clearing into
   `Backend.Grant` means grant is no longer a pure budget bump. Confirmed as
   wanted; noted here because it is a contract change every backend
   implementor must act on, including the closed tracker backend.
3. **Id in the seed but no longer in the flow** (§2, case 8) — currently
   accept-with-warning rather than refuse.
4. **`--all` fallback** — bare `grant` with no park refuses rather than
   silently sweeping. Refusal is deliberate; reversible if it proves annoying
   in practice.
