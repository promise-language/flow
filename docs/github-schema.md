# GitHub storage schema

> **Tag:** `github-schema` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** This document defines how state, artifacts, and history are stored on a GitHub issue by the GitHub backend. It is written for readers who have no SDK — the wire format is the contract.

## State comment

Each issue carries at most one **state comment** — the machine-readable record `LoadState` returns. It is identified by HTML-comment markers and wrapped in a `<details>` element:

```
<!-- flow:state-v1 begin owner=<login> -->
<details><summary>📋 Flow state — <binary> (machine-managed, do not edit)</summary>

```yaml
<YAML body>
```

</details>
<!-- flow:state-v1 end -->
```

The `owner=<login>` attribute on the `begin` marker records who authored the state comment. When a different user claims the item, a fresh state comment is posted under their identity.

### Schema version

The YAML body carries a `schema` field. The current version is **1**. The version is bumped only on incompatible schema changes.

### YAML fields

| Field | Type | Meaning |
|---|---|---|
| `flow` | string | The binary name that seeded this item. |
| `schema` | int | Schema version (currently 1). |
| `seeded_at` | timestamp | When the artifact checklist was written. |
| `artifacts` | array | Per-artifact state entries. See below. |
| `signals` | array | Per-signal state entries. See below. |
| `park` | object or null | Current park record, or absent when not parked. |
| `questions` | array | Questions the item is parked on. Written by an ask and dropped with the park that was waiting on it; read back only while `park.kind` is `question`. See below. |
| `finalized` | bool | Whether the item's flow run is complete. |

### Artifact entries

Each entry in the `artifacts` array:

| Field | Type | Meaning |
|---|---|---|
| `id` | string | The artifact identifier. |
| `type` | string | One of: `flag`, `commit_hash`, `markdown`, `json`, `file`, `patch`. |
| `required` | bool | Whether this artifact blocks completion. |
| `stale` | bool | Whether the artifact has been marked stale. |
| `resolved` | bool | Whether the artifact has been produced. |
| `resolved_by` | string | URL of the artifact comment (when resolved). |
| `resolved_by_principal` | string | Login of who resolved it. |
| `produced_at` | timestamp | When the artifact was resolved. |
| `version` | int | Monotonically increasing version counter. |
| `commit_hash` | string | Inline value for `commit_hash` type. |
| `json` | string | Inline value for `json` type. |
| `granted_invocations` | int | Budget cap: invocations. |
| `granted_prompts_per_invocation` | int | Budget cap: prompts per invocation. |
| `granted_cost_usd` | float | Budget cap: cost in USD. |
| `granted_timeout` | duration | Budget cap: wall-clock time. |
| `invocations` | int | Usage counter: invocations consumed. |
| `prompts_this_invocation` | int | Usage counter: prompts in current invocation. |
| `cost_usd_spent` | float | Usage counter: cost spent. |
| `last_run_at` | timestamp | When the last invocation started. |

### Signal entries

Each entry in the `signals` array:

| Field | Type | Meaning |
|---|---|---|
| `id` | string | The signal identifier. |
| `set` | bool | Whether the signal is currently set. |
| `observed_at` | timestamp | When the signal was last observed. |
| `observed_via` | string | `side-effect` or `poll`. |

### Question entries

Each entry in the `questions` array:

| Field | Type | Meaning |
|---|---|---|
| `id` | string | The backend-assigned question identifier, named by `answer --question`. |
| `header` | string | Short scannable label. |
| `text` | string | The full prompt. |
| `format` | string | One of: `text`, `yes_no`, `choice`. |
| `options` | array | Presentation hints for `choice`; never constrain the answer. |
| `multi_select` | bool | Whether a `choice` question accepts several options. |
| `asked_at` | timestamp | When the question comment was created, on **GitHub's** clock — the clock the replies it is compared against are stamped by. |

A new ask replaces the array rather than appending to it: the field carries the questions currently outstanding, and the question comments carry the history. Answers are not recorded here — the issue thread is the answer store.

The array belongs to the park that is waiting on it, and goes wherever that park does: dropped when the asking step resolves, when a park of another kind supersedes it, and on a re-seed. A record left behind would be inherited by the next question park and presented as its outstanding ask, which `answer` would then accept — an answer to a question nothing is waiting on.

### Park record

When present, the `park` object:

| Field | Type | Meaning |
|---|---|---|
| `kind` | string | One of: `blocked`, `question`, `budget-exhausted`, `step-did-not-resolve`, `infra-transient`, `remote-unreachable`, `refused`. |
| `step` | string | The step's result id (artifact or signal id). |
| `axis` | string | Budget axis when `kind=budget-exhausted`. |
| `axes` | array | Full budget snapshot at park time (each with `axis`, `used`, `granted`, `exhausted`). |
| `reason` | string | Human-readable reason. |
| `details` | string | Additional detail (e.g. question timestamp marker). |
| `parked_at` | timestamp | When the park was recorded. |

## Artifact comments

Each resolved artifact (except `flag`, which has no payload) gets its own issue comment, identified by an HTML-comment marker:

```
<!-- flow:artifact id=<id> type=<type> v=<version> by=<login> ts=<RFC3339> -->
<body>
```

The body format depends on the type:

| Type | Body format |
|---|---|
| `markdown` | Raw markdown text. When spilled, a truncated preview followed by a spill notice. |
| `commit_hash` | `` commit: `<sha>` `` |
| `json` | Fenced `json` code block. |
| `file` | Link to the orphan-branch file with byte count. |
| `patch` | Link to the orphan-branch diff with byte count and base SHA. |

Multiple versions of the same artifact produce multiple comments (append-only). The state comment's `version` pointer identifies the current one.

## Large artifact storage

Artifacts too large for an issue comment (file and patch types always; markdown when exceeding the configured `MaxCommentBytes`) are stored on the **orphan branch** `flow-artifacts`:

```
flow/artifacts/issue-<N>/<id>/<filename>
```

Where:
- `<N>` is the issue number.
- `<id>` is the artifact identifier.
- `<filename>` is the sanitized original filename (file type), `patch.diff` (patch type), or `body.md` (spilled markdown).

The orphan branch is created on first use with a parentless commit. Subsequent writes use the Contents API. The artifact comment on the issue links to the `raw.githubusercontent.com` URL for the file.

### Spill notice

A markdown artifact whose comment body was truncated carries a trailing notice:

```
[truncated preview; full body at <URL>]
```

Readers detecting this trailer must fetch the full body from the orphan branch rather than using the inline preview.

## Labels

All labels use a configurable prefix (default `flow:`). The label set is closed:

| Label | Meaning |
|---|---|
| `flow:seeded` | The item has been seeded with an artifact checklist. |
| `flow:owner:<login>` | The item is claimed by `<login>`. |
| `flow:claim:<token>` | Transient claim-race token, self-limiting (see "Claim protocol" below). |
| `flow:blocked` | The item is parked (generic block or deterministic refusal). |
| `flow:needs-answer` | The item is parked waiting for a human answer. |
| `flow:disabled` | The item is excluded from processing. Claim is refused. |
| `flow:manual` | An operator has taken hand control (`ItemEditor.SetManual`). Nothing dispatches the item underneath the person driving it. |
| `flow:infra-transient` | The item is parked due to infrastructure failure. |
| `flow:stale:<id>` | The artifact `<id>` has been marked stale. |
| `flow:budget-exhausted:<id>` | The step producing `<id>` exhausted its budget. |
| `flow:type:<type>` | Item type derivation label. |
| `flow:<binary-name>` | The binary that owns this item. |

Park labels are added when a park is recorded and removed when the park is cleared (by a grant, a resolve, or a reset). A park label that outlives its condition is worse than no label — it is read as current.

## Signals from GitHub state

The backend derives four signals by polling the pull request on the claim branch:

| Signal | Set when |
|---|---|
| `pr-open` | A PR for the claim branch has been opened. Latched: once set, not unset by merge or close. |
| `pr-merged` | The PR is merged. |
| `pr-closed` | The PR state is `closed`. |
| `pr-approved` | At least one reviewer's latest review state is `APPROVED`. |

These are refreshed on every `LoadState` call. The PR must be on the branch `flow/issue-<N>` to be found.

## Branch naming

| Branch | Purpose |
|---|---|
| `flow/issue-<N>` | Work branch for issue `<N>`. One per item. |
| `flow-artifacts` | Orphan branch for large artifact storage. |

## Claim protocol

Claiming uses a label-based race to achieve exclusivity without server-side locking:

1. **Post** a claim label `flow:claim:<token>`. The token is exactly 24 lowercase hex digits: 8 of creation time (unix seconds) followed by 16 of randomness.
2. **Re-fetch** the issue's labels.
3. **Collect** every abandoned token: one older than **10 minutes**, or one carrying no readable creation time (a different width, a non-hex character, or the untimestamped format that predates this rule). Its label is removed and it takes no part in the race. A claimer never tests its own token, so a clock adjustment mid-attempt cannot make it collect itself.
4. **Settle** among the tokens that remain: the **lexicographically smallest** wins. Every token is the same fixed width and lowercase, so the comparison is well-defined; because the creation time leads, lexicographic order is chronological order and the earliest attempt still in flight wins.
5. **Losers** remove their own claim label and return `ErrClaimRefused`. A token that wins is a live attempt, so the refusal names the winner and how long ago it started.
6. **Winner**: adds self as assignee, posts `flow:owner:<login>`, removes the transient `flow:claim:<token>` label.

The token exists only *inside* one claim attempt — every exit from the attempt removes it. A process that dies between step 1 and its removal therefore strands a token that no process holds and nothing expires, and because the smallest token wins, that one token blocks the item for every later claimer, permanently. Collection in step 3 is what makes the token self-limiting, so recovery is an ordinary `claim` rather than an operator deleting the label through the backend.

**The window is sized for the clocks, not for the attempt.** Age is read against the collecting claimer's own clock — there is no shared one — so ten minutes covers not the attempt itself, which is two API calls, but the disagreement between the clocks of two claimers racing from different machines. The rule holds while those agree to within a window: a claimer running more than a window behind the others mints tokens they read as already abandoned, and they settle the race without it.

**Collection is not lease recovery.** It touches the claim-race token and nothing else: `flow:owner:<login>`, the assignee, and the worktree-local active claim are never removed on a timer. Those record ownership by a person, and recovering a claim held by something no longer running is a separate problem — governed by [resolution-orchestrated.md](resolution-orchestrated.md) under "Interruption", and requiring that the holder be observed to be gone rather than inferred from elapsed time. A settled race token has no holder to observe.

Preflight checks before posting the claim label refuse items that are disabled, owned by another binary, or held by another user (unless `OverrideAlreadyHeld` is passed).

## Work in progress

The GitHub backend's work-in-progress store is the worktree-local `.flow/work/` directory. Records are keyed by issue number and step result id. Nothing in this directory touches the GitHub API — the structural separation from the outward-facing code **is** the "never published" guarantee. Records are cleared when the claim is released (via `clistate.Clear`).

## Cross-references

- [artifacts-and-signals.md](artifacts-and-signals.md) — the result kinds stored here.
- [orchestrator.md](orchestrator.md) — the SDK ↔ orchestrator boundary this schema implements.
- [resolution.md](resolution.md) — the lifecycle whose state this schema persists.
