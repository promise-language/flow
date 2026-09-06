# Proposal: multi-project support across flow, gate, and engagement

**Status:** draft, not yet implemented
**Author:** initial sketch
**Related:** [docs/archive/design.md](../archive/design.md),
[docs/gates-and-commands.md](../gates-and-commands.md)

## Goal

A **single Reactor** must serve items, flows, gates, and the engagement feed
for **many projects** at once. Every flow run, gate run, backend/runner call,
MCP tool call, and feed article must be unambiguously attributed to exactly one
project — and that attribution must be **reliable even when an LLM agent is in
the loop**, which means it can never depend on the agent supplying it.

A **project is identified by its repo URL** (normalized to a stable *project
key*). Flow and gate binaries run from `./bin/<bin>` with their project root
baked in at build time, so the binary always knows which repo it is. The hard
part is the *smart* path: a flow step (or an agentic gate) runs an agent
prompt, the agent calls an MCP tool, and the MCP server must deduce the project
on its own. Agents are unreliable narrators; "please pass `project=…` on every
call" will not hold.

### The one principle

> **Project is bound at the boundaries the SDK controls, never asserted by the
> agent.**

There are exactly three such boundaries, and all attribution flows from them:

1. **Build time** — the flow/gate binary has its project root baked in
   (`projectRoot` ldflag, already used by gates). One binary is built for one
   project; the project key is that root's repo.
2. **The backend/runner call** — every `flow.Backend` method the binary invokes
   carries the project key as an ambient scope; the runner partitions by it.
   The authoritative project for an item is the one the backend stamps on it
   (`Item.Project`), fixed at claim time.

**One carrier per interface point — no fallbacks.** At each boundary there is
**exactly one** way project is conveyed, never a primary-plus-fallback chain: a
backend↔runner HTTP call carries it one way, an MCP call one way, a binary
invocation one way. "Try the header, else the path, else derive from the
worktree" is explicitly disallowed — multiple resolution paths are how two
callers disagree about project. Pick one mechanism per point and reject a call
that lacks it.
3. **Agent spawn** — when a flow step *or an agentic gate* launches an agent,
   the SDK mints a **session-scoped capability** for that agent's MCP access and
   injects it into the agent's MCP configuration *out of band*. The MCP server
   resolves the capability to a project server-side. The agent never sees,
   types, or can forge a project parameter.

Everything below is the mechanical consequence of that principle.

## What "project" is

```go
// A project is a repo URL, normalized to a stable, comparable key.
// "git@github.com:promise-language/flow.git"  ─┐
// "https://github.com/promise-language/flow"   ├─► "github.com/promise-language/flow"
// "https://github.com/Promise-Language/Flow.git"┘   (host + path, lowercased,
//                                                    no scheme, no creds, no .git, no trailing /)
type ProjectKey string
```

Normalization rules (host + path, scheme/credentials/`.git`/trailing-slash
stripped, lowercased) live in the SDK as a single `flow.NormalizeProject(url)
ProjectKey` so the binary, the backend, the runner, and the Reactor all derive
the *same* key from the *same* repo URL. The repo URL is the natural project
identity: it is globally unique and needs no separate registry.

### Forks and PRs — two single rules, no chain

Project has exactly one source at each point, so forks and PRs need no
resolution ladder:

- **The binary's project = its baked `projectRoot` repo.** A fork that ships
  **its own** binary (built with `projectRoot` = the fork) is, by that, its own
  project. Nothing derives project from the worktree's live `origin`.
- **An item's project = `Item.Project`, stamped by the backend.** A PR is part
  of the project it **merges into**: the backend (which manages the PR and
  knows its base repo — `gh pr view` makes the base explicit) stamps the
  upstream as the item's project, even while a contributor's commits sit on a
  fork branch. The SDK receives one value; it does not re-derive it.

So "a fork is a separate project" and "a PR belongs to the upstream it merges
into" are the *same* one-source rule seen from the two ends: a separate binary
means a separate project; a PR processed by the upstream's binary carries the
upstream project. The two values (binary `projectRoot`, item `Item.Project`)
must agree for a claim — that agreement is a check, not a fallback (see
"Cross-project mismatch").

### Non-goal: one repo per project (no monorepos)

A project **must live in its own repo**. Multiple projects in one repo (a
monorepo) is explicitly **out of scope** — a deliberate, material
simplification: it keeps `ProjectKey` a bare repo URL, keeps commits /
versioning / PRs atomic per project, and sidesteps routing items, gates, and
feeds within a shared history.

If it ever had to be extended, the key could grow to `repoURL:projectPath` —
but that pushes real pain downstream (non-atomic commits, per-path versioning,
PR routing), so it is not planned. The one composition that *can* express
"several pieces under one project" is **submodules** — a supermodule is the
project, with one or more submodules — but that has proven a nightmare to
manage (commits stop being atomic; submodule bumps must be staged in the right
order), so it is **neither recommended nor supported** at the moment. One
project, one repo.

## How the flow binary knows and propagates project

The binary has `projectRoot` baked in, giving the project for the pre-claim
paths (`ListEligible`, `claim`): `NormalizeProject(gitRemote(projectRoot))`. At
claim, the backend stamps `Item.Project`, and from then on that is the project:

> **Project is bound once, at claim, and is stable for the claim's life.** A
> project effectively never changes for an item, so the SDK resolves
> `claim.Project` when the claim opens and treats it as fixed for the duration
> — no per-call re-derivation, no mid-claim re-check. `RefreshItem()` re-pulls
> item state but `Item.Project` is invariant across the claim.

Project then rides as an **ambient scope on every backend call** via the
`Claim` — one carrier, no per-method argument. `Claim` already carries
`BackendName`, `ItemRef`, `Owner`, and a backend-internal `Token`; add
`Project ProjectKey`. Every `Backend` method already takes the `claim`, so
project reaches `LoadState`, `ResolveArtifact`, `Bump*`, `Park`,
`AskQuestions`, `Worktree`, … with **zero signature churn**. The runner backend
reads `claim.Project` and puts it on each runner/Reactor request by the single
agreed carrier (§ contract). The single-project github backend ignores it (its
scope *is* the one repo); the Reactor backend keys everything on it.

## How the gate binary knows and propagates project

Identical root: the gate binary's `projectRoot` is baked in, so `gate.Main`
resolves `project = NormalizeProject(gitRemote(projectRoot))` the same way.
Two propagation points:

1. **The run-output envelope** (`flow:gate-output-v1`) gains an optional
   `project` field, stamped by `gate.Main` from the baked root — so a gate's
   stdout is self-attributing even when an orchestrator collects runs from many
   projects into one pipe.
2. **The invocation context.** When the Reactor *itself* invokes
   `bin/gate <name>` it already knows which project's binary it launched (it
   holds the per-project binary), so it tags the result with that project; the
   envelope's `project` is a cross-check, not the sole source.

### Agentic gates propagate project too

Gates are deterministic today, but a gate may run an **agent** (a flake
triager, a semantic diff reviewer, an LLM-judged metric). The "no agent in the
gate path" assumption does **not** hold, and gate→agent→MCP must carry project
exactly as reliably as the flow path does. The mechanism is the same: the gate
binary knows its project from the baked `projectRoot`, so when `gate.Main`
spawns an agent it fills the same model-invisible `MCPScope` (next section) from
that project — the gate just *is* the SDK-controlled boundary here, instead of a
`StepCtx`. The only difference is scope: a gate isn't bound to a claimed item,
so its capability is **project-scoped** (project + gate-run/session), not
item-bound. The `flow/gate` `RunCtx` gains an `Agent()` accessor mirroring
`StepCtx.Agent()`, with the same chokepoint filling `MCPScope`.

## Items carry their project

A single Reactor holds items for many projects at once — e.g. Promise (the
language), the legacy orchestrator, and the new Reactor itself. So **the item
is project-scoped, and the backend must say which project every item belongs
to.** How that association arises depends on where items live:

- **GitHub issues** — the association is *implicit in storage*: an issue lives
  in exactly one repo, and that repo URL is the project. The github backend
  derives `Item.Project` from the issue's repo, for free.
- **Custom durable storage** (a bespoke tracking system, a database, …) — the
  project is **either set explicitly on the item** (a stored field) **or
  deduced from the item's location** (which space/table/partition it came
  from). The backend picks whichever its storage supports.

Either way the rule at the SDK seam is uniform:

> **`Item.Project` is a Backend invariant.** Every `Item` a Backend returns —
> from `ListEligible`, `LookupClaim`, `LoadState`, `RefreshItem` — MUST carry a
> non-empty `Project ProjectKey`. (Same standing as the existing "every `Item`
> carries a non-empty `Type`" invariant.)

```go
type Item struct {
    // … existing: Type, Title, Body, ID, … …
    Project ProjectKey // REQUIRED — the project this item belongs to
}
```

### Cross-project: writes never cross, reads cross only on explicit request

Projects are **self-contained by default** — a binary built for project A
operates on A's items. But total isolation is too strict: an item in one
project routinely *depends on* or references an item in another (a Promise issue
blocked on a Reactor issue) and needs to **consult** it. So the rule splits by
operation, not by a blanket reject:

- **Writes never cross — no exception.** Any mutation (`claim`, `run-step`,
  `resolve`, `dismiss`, `post`, an MCP write tool, …) targeting an item whose
  project ≠ the caller's project is **hard-rejected**. A project-A binary or
  agent modifying project-B items is a recipe for catastrophe; the boundary is
  absolute and not relaxable by any flag.
- **Reads cross only when explicitly asked, and stay read-only.** A foreign
  read is **never automatic** — it happens only when the caller deliberately
  opts in to a specific foreign item (a foreign-read flag/endpoint, an
  explicit cross-project tool), so an agent or a user can't *accidentally* latch
  onto items they don't need to see. An ordinary same-project read is never
  silently widened to include another project. When the caller does opt in, the
  Reactor returns the item **read-only** and the surface **flags it**, and that
  read never enables a later write. `bin/flow status <foreign-item>` prints the
  item with a banner:

  ```
  WARNING: T1407 belongs to github.com/promise-language/reactor (read-only here).
  ```

  An agent prompt can likewise *consult* a referenced cross-project item, but
  only through a **distinct, explicit** cross-project read tool — never by
  naming a foreign id to an ordinary item-read tool — so a confused or
  hallucinating agent can't latch onto another project by accident. Every write
  tool refuses a foreign target regardless.

What counts as the **explicit** signal differs by surface: a human operator
naming a specific id (`status <id>`) or passing `--all-projects` to `list` is
itself the deliberate act; an agent needs the dedicated cross-project tool. In
no case does a foreign item appear in an *ordinary* same-project read or list.

Per layer:

- **Flow (client).** `cli.Run` compares `Item.Project` against the binary's
  baked `projectRoot`. Mutating verbs (`claim`/`run-step`/…) on a foreign item
  are **refused**. Read verbs do the opposite of failing closed on a *named*
  item: `status <id>` for a foreign id **returns the item with the warning
  banner** (not a confusing "no such item"), and `list` defaults to this
  project but, with **`--all-projects`**, spans every project the operator can
  access (each row tagged with its project; foreign rows read-only).
- **Runner / Reactor (server).** The authoritative check. A write whose target
  item's project ≠ the call's project is rejected outright; a read is served
  read-only and tagged foreign when the principal may see that project. The
  server never trusts the client to have enforced this.
- **Agent / MCP.** The session capability is project-scoped (§ MCP); write
  tools reject foreign-project targets, read tools may return foreign items
  read-only when explicitly referenced.

For the **home** project, the binary's `projectRoot`, `Claim.Project`, and
`Item.Project` are views of one value — always equal for a live claim; any
disagreement on a *write* is an error, never a silent merge. A foreign item is
only ever reachable read-only and only when explicitly named.

## The hard part: MCP project-deduction

Setup: a flow step — or an agentic gate — calls its `Agent().Run(...)`. The
agent (Claude) runs with the `Worktree` as cwd and, during the prompt, calls an
MCP tool
(`ask_user_question`, `post_article`, item reads/writes, …). The MCP server is
a **single shared server inside the Reactor** handling many projects. It must
know which project this call is for. The agent must not be the source of truth.

### Why "ask the agent" fails

Passing `project` as an MCP tool argument means the model has to (a) know the
value and (b) reproduce it correctly on every call. Both fail in practice:
prompt drift, hallucinated values, copy-paste across sessions, and a
straightforward path to cross-project writes if a prompt is confused or
adversarial. Project is a **security and correctness boundary**; it cannot live
in model-generated text.

### The mechanism: a session-scoped capability, injected out of band

When the SDK spawns the agent, the runner mints a **per-session capability
token** bound to `(project, item, claim, session)` and injects it into the
agent's MCP *configuration* — not its prompt. Every MCP call the agent makes
carries that capability automatically (transport-level), and the MCP server
resolves it **server-side** to the project. The agent cannot read it from the
conversation, cannot vary it per call, and cannot forge one for another
project.

The capability is the **single** project carrier for the MCP interface point —
there is no second way to convey project to an MCP call. The chosen transport
just decides how that one capability rides:

- **stdio MCP server (spawned per agent session):** the SDK launches the MCP
  server process with the capability in its **environment / argv**, inheriting
  the agent's `Worktree` cwd. The agent talks to *its* server; that server was
  born knowing the project. Nothing crosses the model.
- **HTTP/SSE MCP server (shared):** the SDK writes the capability as a
  **bearer header** in the agent's MCP client config for that session. The
  Reactor validates the header and maps it to the project on every request.

These are not fallbacks for each other — the reference transport is **HTTP**
(the Reactor already serves an HTTP MCP endpoint), and that transport's delivery
is the one and only carrier.

A new SDK seam carries this without the flow author touching it:

```go
// AgentRequest gains a backend-populated, model-invisible scope.
type AgentRequest struct {
    // … existing fields (Prompt, Worktree, Model, …) …
    MCPScope MCPScope // populated by the SDK/backend at spawn, NOT by the handler
}

// MCPScope is the per-session capability the backend mints and the agent
// runner injects into MCP config (env for stdio, header for http).
type MCPScope struct {
    Project   ProjectKey
    Item      ItemRef
    Token     json.RawMessage // opaque capability, validated server-side
}
```

`ctx.Agent()` (the metered chokepoint) fills `MCPScope` from the active claim
before calling the underlying `flow.Agent.Run`. The reference `flow/claude`
runner translates `MCPScope` into the claude MCP config (env or header). A
handler **cannot** override it — there is no field on the prompt for it.

### Defense in depth — checks, not alternative carriers

The capability is the sole carrier; the items below only ever *validate* it and
can reject, never *supply* a project (so they don't reintroduce a fallback):

1. **Capability → project (authoritative).** The token *is* the project; a
   forged/absent token is rejected. This alone is sufficient.
2. **Worktree origin (corroborating).** The agent's cwd is the project's
   checkout; a co-located stdio server can read `git remote get-url origin` and
   assert it matches the token's project — a cross-check that only flags
   misconfiguration, never resolves project on its own.
3. **Item ownership (authorization).** The capability is bound to the claimed
   item; an MCP write to an item the session doesn't own is refused even within
   the right project.

If a tool call ever arrives without a resolvable capability, it is **rejected**,
never guessed — the same posture the engagement feed takes for unregistered
identifiers (fail-closed on a security boundary, not best-effort).

### Beyond flows: standalone MCP use from a worktree

The MCP server must also work **with no flow or gate running** — a developer in
a project worktree firing ad-hoc agent prompts that call Reactor tools. There
is no claim and no SDK-minted session capability then, so the project binding is
established **at MCP-config setup time** instead of at agent spawn: a different
*when*, the **same one carrier**.

Tooling lives outside the checkout (a naked `git clone` has none): the shared
`workspace` repo provides `bin/workspace`, and the developer runs `workspace
setup` **from the project root**. Running in the project's CWD, it reads the git
remote, derives the `ProjectKey`, and writes a project-scoped `.mcp.json`. That
is the human-boundary analog of the build-time `projectRoot` — the project is
captured by the setup step the developer controls, never typed by the agent.

The reference transport is **HTTP** (the Reactor already serves an HTTP MCP
endpoint; stdio remains possible for a per-session isolated server but is not
the default). Today a project's `.mcp.json` carries only the URL and no project:

```jsonc
// .mcp.json today — points at the shared HTTP MCP server, but says nothing
// about WHICH project the calls are for. Ambiguous on a multi-project Reactor.
{
  "mcpServers": {
    "reactor": { "type": "http", "url": "http://198.51.100.7:9121/mcp" }
  }
}
```

The fix is to carry the `ProjectKey` as the **single carrier** in the MCP client
config — a project-scoped bearer token the setup step obtained for this repo:

```jsonc
// .mcp.json — written by `workspace setup`, run from the project root.
{
  "mcpServers": {
    "reactor": {
      "type": "http",
      "url": "http://198.51.100.7:9121/mcp",
      "headers": {
        // The one project carrier: a token scoped to this repo. The Reactor
        // resolves token -> (project, principal) server-side; the agent never
        // sees, sets, or can change which project a call targets.
        "Authorization": "Bearer rkt_pf_9f3c…"   // scoped to github.com/promise-language/promise
      }
    }
  }
}
```

The bearer token *is* both the auth and the project scope, so a prompt cannot
reach another project by editing a plaintext field, and there is no second
project source to disagree with it. (A plain `"X-Reactor-Project":
"github.com/promise-language/promise"` header, or a `/mcp/<project>` URL path,
is a simpler interim on a trusted LAN with no auth — but pick exactly **one**,
and a token that *is* the scope is the safer single carrier.)

This unifies the two entry points behind one carrier (an `Authorization` bearer
the Reactor maps to a project):

| Entry point | Who writes the MCP config | Token scope |
|---|---|---|
| Flow-step agent | the SDK, at agent spawn (`StepCtx.Agent` → `MCPScope`) | claim-bound (project + item + session) |
| Agentic-gate agent | the SDK, at agent spawn (`RunCtx.Agent` → `MCPScope`) | project-bound (project + gate-run) |
| Manual prompt in a worktree | `workspace setup`, from the project root | project-wide |

## Engagement feed: project as an ambient scope

The feed already insists identifiers are **stamped at the boundary, not typed
by the emitter** (see the engagement-feed "Identifiers" rules). Project is the
strongest case of exactly that:

- An `Article` carries **no author-supplied project field**. Project is stamped
  by the ingress the same way `CreatedAt` is server-stamped:
  - `StepCtx.Post` → from `claim.Project`.
  - gate `articles[]` → from the gate envelope's `project`.
  - MCP `post_article` → from the session capability (§ MCP above).
  - `POST /api/feed` → from the project-scoped auth token / path.
- Storage and ranking are **per project**. `GET /api/feed` returns one
  project's feed; the Reactor never mixes two projects' articles into one
  ranked list by accident.
- The operator UI gains a **project selector** (the Feed tab scopes to the
  chosen project), and optionally a cross-project "all my projects" view that
  *groups by* project. This stays compatible with the "one linear list, no
  tabs *inside* the feed" rule — project selection is outside the feed, like
  choosing which inbox to read, not a tab within it.

So the engagement-feed schema needs **no new article field**; it needs the
ingress paths to stamp `project` server-side, and the store/query to be
project-keyed. (Internally the stored `FeedArticle` gains a server-owned
`Project ProjectKey`, alongside `CreatedAt`/`State`.)

## The runner / Reactor contract

- **Every runner/Reactor API call is project-scoped, by one carrier.** Each
  interface point fixes a single mechanism (no header-or-path choice, no
  fallback): the HTTP API uses a `/api/projects/{project}/…` path prefix; the
  MCP path uses the session capability; a binary invocation uses the envelope
  field. The carrier is immaterial *as long as it's the only one* — what
  matters is that there is exactly one, so two callers can never disagree. The
  Reactor stores items, flows, gates, leases, and articles partitioned by
  `ProjectKey`.
- **The project must match the credential.** A backend token (or the agent
  capability) is itself scoped to a project; the Reactor rejects a call whose
  path/header project disagrees with the credential's project. Project is not a
  free routing hint — it is checked.
- **The project must match the target item — for writes.** A write naming an
  item whose stored project differs from the call's project is rejected, even
  with a valid credential for the call's project (§ "Cross-project"). A read of
  a foreign item is served read-only **only when the caller explicitly opts in**
  (an explicit foreign-read flag/endpoint), never as an automatic widening of an
  ordinary same-project read.
- **Registration.** A project is registered with the Reactor once (its repo
  URL → `ProjectKey`, plus which binaries/gates it exposes), mirroring the
  engagement-feed component registry: data-level onboarding, no Reactor code
  change to add a project.

## Where these contracts live

Multi-project touches two different boundaries, owned by two different repos —
and the split is a hard rule, not a preference:

- **The SDK owns every worktree↔orchestrator boundary.** The flow SDK is a
  versioned Go module that Reactor (and any other orchestrator/backend) imports.
  Every structure that crosses from a worktree-side binary to the
  runner / Reactor / HTTP / MCP — and every binary input/output — is **defined
  once in the SDK** so both sides compile against the same definition:
  `ProjectKey` + `NormalizeProject`, the `Backend` interface, `Claim`, `Item`,
  `MCPScope`, the gate run-output envelope (`flow:gate-output-v1`), the flow
  `InvocationResult`, the feed wire article (`flow:feed-article-v1`). The
  normalizer in particular is **imported, never re-implemented or mirrored** —
  one function, so a scheme/case/`.git` difference can never split one project
  into two keys.

- **Reactor owns its own internal boundaries.** Anything that does *not* cross
  the worktree boundary — Reactor ↔ its durable storage (GitHub issues, a custom
  tracking DB) and Reactor ↔ its UI primitives — is **defined in Reactor**, not
  the SDK. The SDK never names a storage schema or a UI type. This is why
  `Item.Project` is an SDK field but *how* a backend derives it from its store
  is the backend's private business, and why the feed's wire article is SDK
  while the stored `FeedArticle` and the Feed tab are Reactor's.

Rule of thumb: **if a worktree binary can observe it, the SDK defines it; if
only Reactor and its storage/UI see it, Reactor defines it.**

## What changes where

| Layer | Change |
|---|---|
| SDK root | `ProjectKey`, `NormalizeProject(url)`, `Claim.Project`, `Item.Project` (Backend invariant), `AgentRequest.MCPScope` + `MCPScope` type |
| `cli.Run` | resolve project from `projectRoot` once; set `claim.Project` on claim/load; **reject writes to foreign-project items**; `status <foreign-id>` returns read-only with a warning; `list --all-projects` spans accessible projects; fill `MCPScope` in `ctx.Agent()` |
| `flow/claude` | translate `MCPScope` → MCP config (bearer header on the HTTP reference transport; env/argv if stdio) |
| `workspace setup` | derive `ProjectKey` from project-root remote; write project-scoped `.mcp.json` for manual/standalone use |
| gates | `project` field on `flow:gate-output-v1`, stamped by `gate.Main`; `RunCtx.Agent()` fills `MCPScope` (project-scoped) for agentic gates |
| engagement feed | ingress stamps `project`; store/query keyed by project; UI project selector (no schema field added) |
| github backend | ignores project (single-repo scope) — multi-project is a no-op there |
| Reactor (closed) | project-partitioned storage; `/api/projects/{project}/…`; capability minting + resolution; project registry |

The OSS SDK changes are small and additive; the heavy lifting (partitioning,
capability service, registry) is Reactor-side — the same SDK/orchestrator split
as gates and the feed.

## Open questions

1. **Capability lifetime & rotation.** Project is stable for the claim, so the
   capability's *project binding* never needs to change mid-claim — it can be
   minted at claim and re-used. Open is only the *security* TTL: does the token
   expire on park/release and get re-minted on resume (a parked step may resume
   days later), independent of the unchanging project? Define the refresh
   without re-deriving project.

## Why this shape

- **Reliable without trusting the agent.** Project lives only on
  SDK-controlled boundaries (build-time root, backend call, capability at
  spawn). The model has no project parameter to get wrong — the failure mode of
  "the agent named the wrong project" is structurally impossible.
- **No new article/handler surface.** Project is stamped, not authored —
  exactly the engagement-feed identifier discipline, extended to the strongest
  boundary. Flow authors write project-agnostic handlers; the SDK scopes them.
- **Same SDK/Reactor split as gates & feed.** The SDK defines the contract
  (project key derivation, the MCP-scope seam, the envelope field); the Reactor
  owns partitioning, the capability service, and the registry.
- **Fail-closed.** An unresolvable project (no capability, mismatched
  credential) is rejected, never guessed — consistent with treating project as
  a security boundary, not a routing hint.
