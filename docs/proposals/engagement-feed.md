# Proposal: a user-engagement feed in the flow SDK

**Status:** draft, not yet implemented
**Author:** initial sketch
**Related:** [docs/design.md](../design.md), [docs/proposals/gates.md](gates.md)

## Goal

Define one **abstract, well-defined engagement surface** that any component in
the system — a flow step, a gate, an item question, an item concern, the
orchestrator itself, or a future component nobody has written yet — can post to
without the orchestrator needing to know that component exists.

The core unit is a **feed article**: a durably-identified, self-describing,
*decaying* call to attention. The user reads a single ranked **feed** (the
"Feed" tab of the Reactor UI), where articles are ordered by a score derived
from their priority and how long they've been aging against their half-life.
The user can **dismiss** an article, **take one of its calls to action**, or
**navigate** to whatever it references.

**A river, not a ledger.** The feed is ephemeral by design, like a social
timeline: an article is either engaged with *now* or it flows away. There is no
archive and no permanent read-history — you cannot scroll back to what you read
two years ago. Articles leave by decaying out (then, after a bounded tail,
deleted), or by being dismissed/resolved/expired (removed at once). **Nothing is
retained forever.** This shapes every storage and UI decision below: the system
optimizes for "what deserves attention now," not for recall.

The design rests on two properties. First, the article *kind* is open-ended:
the component identifier is a registry-backed value, not a closed SDK enum, so
an unforeseen component can post after a data-level registration — no
orchestrator code change. Second, priority, decay, audience, and
calls-to-action are first-class, creator-controlled fields rather than
orchestrator-side conditionals. Identifiers that would otherwise drift into
free-text soup (component, audience, tags) are kept to controlled vocabularies
— stamped at the boundary and canonicalized on ingest (see "Identifiers").

### What this proposal adds

Mirroring the gates proposal's split of concerns:

1. A **stable wire format** for a feed article (`flow:feed-article-v1`) — a
   single JSON envelope any language can emit.
2. A **`flow/feed` Go subpackage** — types + a tiny constructor/emit helper
   so Go-authored components are typed end-to-end.
3. **Emission channels** that route an article from a component to the
   orchestrator: a `StepCtx.Post(...)` method for flow handlers, an
   `articles` array on the gate output envelope, an MCP `post_article` tool,
   and the REST sink `POST /api/feed`.
4. A **ranking contract** (priority × half-life decay) defined here so every
   orchestrator ranks identically, even though the SDK ships no ranker.

The SDK ships **no store, no ranker, no decay timer, no UI**. Recording
articles, computing scores, decaying them out, serving the feed, and
rendering the tab all stay in the orchestrator — exactly as gates keep
scheduling/ratcheting/baselines on the orchestrator side. The SDK owns only
the article *definition* and the *emission boundary*.

## Naming

The core unit needs a name. "Notification" is overloaded and push-y;
"article" is descriptive but generic. Candidates, with the verb they imply:

| Name | "A flow ___s a ___" | Notes |
|---|---|---|
| **Brief** (recommended) | "a flow files a Brief" | short, actionable, reads as a noun and matches the "skim a feed of briefs" mental model |
| Post | "a flow posts a Post" | closest to the Facebook-newsfeed framing; verb/noun collision is awkward |
| Bulletin | "a flow posts a Bulletin" | evokes a board of notices that age out; slightly heavy |
| Dispatch | "a flow sends a Dispatch" | good for failures/alerts, less so for informational |
| Notice | "a flow raises a Notice" | clean but close to "notification" |

This document uses **article** generically and **`flow:feed-article-v1`** as
the wire tag so the schema name survives whatever the unit is ultimately
called; the Go type is written as `feed.Article` below. **Open question 1**
settles the user-facing noun. The collection the user reads is the **feed**;
the UI surface is the **Feed tab**.

## The article

### Schema (the durable unit)

```go
package feed

// Article is one unit in the engagement feed: a durably-identified,
// self-describing, decaying call to attention. Matches flow:feed-article-v1.
type Article struct {
    Schema int `json:"schema"` // 1

    // Identity — chosen by the creator, stable across re-posts.
    Key    string `json:"key"`              // durable id, creator-namespaced (e.g. "gate:build-time", "item:T0481:concern")
    Source Source `json:"source"`           // who created this (component + optional item/agent/gate ref)

    // Content.
    Title       string       `json:"title"`
    Description string       `json:"description,omitempty"` // markdown body
    Media       []Attachment `json:"media,omitempty"`       // ordered; [0] is primary (loud), rest subdued

    // Calls to action — one primary + ordered alternatives; or a choice set.
    Actions []Action `json:"actions,omitempty"`

    // Ranking inputs.
    Priority float64  `json:"priority"`              // raw weight; higher = more important. Named anchors: Low=12.5 … Critical=100
    HalfLife Duration `json:"half_life,omitempty"`   // how fast priority decays; "" = orchestrator default half-life
    Pin      bool     `json:"pin,omitempty"`         // float above unpinned & never decay-out; still decays for ordering & still expires

    // Audience & grouping — structured, not free text (see "Identifiers").
    Audience *Audience `json:"audience,omitempty"` // nil = everyone
    Tags     []string  `json:"tags,omitempty"`     // "namespace:value", canonicalized; advisory only

    // Timestamps are stamped by the orchestrator on ingest, NOT by the
    // emitter (emitters have no trustworthy clock and no monotonicity
    // guarantee across hosts). Carried here for the read model only.
    CreatedAt string `json:"created_at,omitempty"` // RFC3339, server-stamped
    ExpiresAt string `json:"expires_at,omitempty"` // optional hard expiry, server-stamped from TTL
}
```

Every field the user enumerated maps onto exactly one schema field:

| Requested | Field |
|---|---|
| durable identity (chosen by creator) | `Key` + `Source` |
| title | `Title` |
| description | `Description` |
| who created it (flow, item) | `Source` (stamped at the boundary) |
| when created | `CreatedAt` (server-stamped) |
| attached media / links (first is primary) | `Media []Attachment` |
| call to action (primary + alternatives, multiple choice) | `Actions []Action` |
| priority (drives rank) | `Priority` |
| half-life / decay speed | `HalfLife` |
| intended reader (optional) | `Audience{Role, Reference}` |
| article tags (grouping) | `Tags` (`namespace:value`) |

### Identifiers: controlled vocabularies, not free text

`Source`, `Audience`, and `Tags` carry the identifiers the orchestrator
attributes, groups, and filters by. Left as raw free strings they rot into
synonym soup (`perf` / `performance` / `perf-regression`; `me` / `operator` /
`george`) and every filter silently misses. Three rules keep them clean
*without* a closed enum (which would defeat the open-extension goal):

1. **Stamp at the boundary, don't type by hand.** The emission channel fills
   identifiers from what the runtime already knows. `StepCtx.Post` stamps
   `Source{component:"flow", name:<flow>, step:<step>, item_id:<item>}`; gate
   ingest stamps `Source{component:"gate", name:<gate>}`. A flow/gate author
   never spells its own `Source`. Only the MCP and REST sinks accept an
   author-supplied `Source` — and only there does rule 2 bite.

2. **Canonicalize on ingest.** Author-supplied identifiers are lowercased,
   kebab-cased, and trimmed before storage, so `Perf`, `perf`, and `PERF`
   collapse to one value. Tags additionally take a `namespace:value` shape
   (both `[a-z0-9-]+`) so related labels cluster under a known facet.

3. **Registry, not enum; advisory, not behavioral.** The legal set of
   `component` values, audience `role`s, and tag `namespace`s lives in an
   **orchestrator-owned registry** — seeded with the built-ins and extended by
   *config/registration*, not an SDK code change (so an unforeseen component
   still onboards without recompiling anyone). The orchestrator **never
   branches behavior** on any of these; they drive attribution, grouping, and
   filtering only. A typo or not-yet-registered value therefore degrades a
   chip or a filter — never logic — and is surfaced as "unregistered" rather
   than silently trusted.

### Source — who created it

```go
type Source struct {
    Component string `json:"component"`        // registered id: flow | gate | item-question | item-concern | orchestrator | <registered>
    Name      string `json:"name,omitempty"`  // flow/gate/breaker name — stamped by the channel, canonicalized
    ItemID    string `json:"item_id,omitempty"`
    Agent     string `json:"agent,omitempty"`
    Step      string `json:"step,omitempty"`   // flow step id, when component=="flow"
}
```

`Component` is a **registered identifier** — neither an arbitrary string nor a
closed SDK enum. On the flow and gate paths the channel stamps it (rule 1), so
authors never spell it; on the MCP/REST paths it must resolve against the
orchestrator's component registry, where an unforeseen component registers once
(config/data, no code change) and is thereafter a known value. The orchestrator
uses `Source` only for attribution display and to resolve navigate-to-source
actions; it never branches behavior on it — so even an unregistered value still
renders (flagged) rather than corrupting logic.

### Media — an ordered list of attachments

`Media` is a plain ordered list. **The first attachment is the primary** — the
one the Feed card presents loudly (inline thumbnail/link); the rest are subdued
(behind a disclosure). No `primary`/`secondary` fields and no fixed cap on the
count: ordering carries the emphasis, so an article can attach one thing, three,
or none.

```go
type Attachment struct {
    Kind      string `json:"kind"`                // "link" | "image" | "file" | "item" | "patch"
    Label     string `json:"label,omitempty"`
    URL       string `json:"url,omitempty"`       // external link or orchestrator-served blob
    Reference string `json:"reference,omitempty"` // internal reference (item id, patch hash, …) when Kind in {item,patch}
}
```

Bytes are **not** embedded in the article (same rule as gate output and as
the patch-capture flow, which is bytes-over-API but never inline). An
`image`/`file` attachment either points at an external `URL` or at an
orchestrator-served blob the creator uploaded separately; the article
carries the reference only.

### Action — the call(s) to action

This is the generalization of hardcoded "Create Bug / Create Task / Open
{ItemID}" buttons, plus the multiple-choice shape of `ask_user_question`.

```go
type Action struct {
    ID      string     `json:"id"`               // stable within the article
    Label   string     `json:"label"`            // button text
    Kind    ActionKind `json:"kind"`
    Primary bool       `json:"primary,omitempty"` // at most one; rendered as the prominent button

    // Presentation / safety — Destructive and Confirm are independent.
    Destructive bool        `json:"destructive,omitempty"` // irreversible/harmful: caution (red) styling
    Confirm     bool        `json:"confirm,omitempty"`     // gate behind a confirm dialog before firing (cost/latency/side-effects)
    Explain     string      `json:"explain,omitempty"`     // markdown: what taking this does — tooltip, and the confirm-dialog body
    After       Disposition `json:"after,omitempty"`       // article fate once taken; "" = keep (see below)

    // Per-kind payload (only the field matching Kind is read):
    Navigate  *NavigateTo `json:"navigate,omitempty"`  // Kind==navigate
    Choice    *Choice     `json:"choice,omitempty"`    // Kind==choice
    Operation *Operation  `json:"operation,omitempty"` // Kind==operation
    URL       string      `json:"url,omitempty"`       // Kind==external
}

// Disposition — what happens to the article after an action is taken.
type Disposition string

const (
    AfterKeep    Disposition = "keep"    // default: stays active (e.g. navigate — you only looked)
    AfterDismiss Disposition = "dismiss" // acknowledge & remove the article once taken
    AfterResolve Disposition = "resolve" // underlying condition handled; retract like creator resolve
)

type ActionKind string

const (
    // Open something in the UI (an item, the source gate/agent, a URL panel).
    ActionNavigate ActionKind = "navigate"
    // Present a (single- or multi-) choice; the selection is recorded and
    // delivered back to the creator (see "callback" below). Generalizes
    // ask_user_question.
    ActionChoice ActionKind = "choice"
    // Invoke a named, allow-listed orchestrator operation (create-task,
    // create-bug, run-gate, release-lease, …). Generalizes the inbox's
    // create-bug / create-task buttons.
    ActionOperation ActionKind = "operation"
    // Open an external URL.
    ActionExternal ActionKind = "external"
    // Dismiss is ALWAYS implicitly available — it never needs to be listed.
)

type NavigateTo struct {
    Target    string `json:"target"`    // "item" | "gate" | "agent" | "url"
    Reference string `json:"reference"` // item id, gate name, agent name, or url
}

type Choice struct {
    Options     []string `json:"options"`
    MultiSelect bool     `json:"multi_select,omitempty"`
}

type Operation struct {
    Name       string            `json:"name"`                 // allow-listed operation name
    Parameters map[string]string `json:"parameters,omitempty"` // operation-specific
}
```

**Taking an action does two things:** (1) the orchestrator performs the
local effect (navigate, create the item, run the gate, record the choice),
and (2) — for `choice` and any action the creator marks `callback: true` —
the orchestrator **delivers the outcome back to the creating component** so
the flow/gate/question that posted the article can react. This is the
general escape hatch that lets `ask_user_question` be *just another article*:
the item-question component posts an article with a `choice` action; the
user's selection flows back and unblocks the item exactly as the orchestrator's
`ask_user_question` does today.

**`Explain`, `Destructive`, `Confirm` — make the consequence legible before
the click. The two flags are independent:**

- **`Explain`** is a short markdown sentence describing the *effect* ("Files a
  `perf`-tagged bug linked to this gate and assigns it to you"). `Label` stays
  terse for the button; `Explain` carries the detail.
- **`Destructive`** is pure presentation: an irreversible or harmful action
  (release a lease, delete a draft) rendered in caution/red styling so it
  reads as dangerous at a glance. It says nothing about confirmation.
- **`Confirm`** gates the action behind a confirmation dialog before it fires.
  This is for anything the user should not trigger by a stray click —
  including actions that are **expensive or slow but perfectly safe** (run the
  full CI suite, kick off a paid build). An action can require confirmation
  without being destructive, and vice-versa.

The two compose: `{Destructive}` alone = a scary-looking one-click button;
`{Confirm}` alone = a calm button that asks "are you sure?"; `{Destructive,
Confirm}` = the dangerous *and* gated case. As a safety floor the orchestrator
**also confirms `Destructive` actions even if `Confirm` is unset** — a
destructive action is never one-click — so in practice `Confirm` is "gate this
*even though* it isn't destructive".

When a confirmation is required, the dialog shows `Explain` as its body and two
buttons: the affirmative button **echoes the action's `Label`** (e.g. *"Run
full CI"* / *"File bug"*) rather than a generic "OK", paired with a *"Cancel"*.
Restating the verb keeps the user oriented on exactly what they're about to do.

**What happens to the article after an action — `After`.** Taking an action
does **not** remove the article by default (`After == "" / keep`): you might
"Open gate" just to look, and the condition is still live. An action declares
its own fate:

- **`keep`** (default) — navigate/external "go look" actions; the article
  stays until it's separately dismissed, resolved, decayed, or expired.
- **`dismiss`** — the action *is* the acknowledgement (e.g. "Got it"); the
  article is removed.
- **`resolve`** — the action handles the underlying condition, so the article
  retracts exactly like a creator `resolve` (e.g. "Create task" on a
  `suggestion`, or answering a `choice` question — the prompt is done). Choice
  actions default to `resolve` once a selection is recorded; everything else
  defaults to `keep`.

So "dismiss vs keep vs dismiss-later" is the creator's choice per action, not a
global rule — and the always-available implicit **Dismiss** (the user's own
acknowledgement) is independent of any action's `After`. A timed
auto-dismiss-after-action ("snooze for 24h then resurface") is left to a future
`snooze` disposition rather than baked in now.

### Priority and Duration

`Priority` is a raw numeric weight (`float64` on the wire), not an enum:
higher means more important, and the value *is* the base score before decay
(see Ranking). Any positive number is valid; the SDK ships named anchors on a
0–100 scale so a human emitter rarely types a bare number:

```go
// Priority is a raw weight; higher ranks higher. The named levels double each
// step — handy because each level then buys exactly one extra half-life of
// staying power (see Ranking). An emitter can pass any float (e.g. 60, or
// High+5) to sit between anchors.
const (
    Low      float64 = 12.5
    Medium   float64 = 25
    High     float64 = 50
    Critical float64 = 100
)

// Duration is a flow:duration string ("4h", "36h", "7d") to keep the wire
// format language-neutral (Go's time.Duration int-nanoseconds is unfriendly
// to non-Go emitters). The orchestrator parses it.
type Duration string
```

The absolute scale is arbitrary — only the ratios between levels and the
ratio of a level to the decay floor affect ordering — so 0–100 is purely a
readability choice. The numeric form drops the enum→weight lookup the ranker
would otherwise need (the priority *is* the weight) and lets a creator place
an article *between* the anchors when "higher than a normal high, below
critical" is the honest intent.

### Audience and Tags

Both follow the "Identifiers" rules above — structured and registry-backed,
never free text.

```go
// Audience targets a reader. nil/empty Audience means everyone.
type Audience struct {
    Role      string `json:"role,omitempty"`      // registered role: operator | reviewer | agent | "" (everyone)
    Reference string `json:"reference,omitempty"` // optional specific identity within the role (agent name, operator id)
}
```

The mess in "who is this for" comes from per-person free strings. Targeting by
**role** keeps the vocabulary tiny and stable (the role set is a registry,
seeded with `operator`/`reviewer`/`agent`); an optional `Reference` names a specific
identity *within* a role, validated against the orchestrator's known agents/
operators. "For me" is the filter `role == operator` matching the configured
operator identity — there is no place to type a raw username.

**Tags** are advisory display/filter facets, each shaped `namespace:value`
(e.g. `topic:perf`, `area:build`, `severity:regression`). The `namespace` is
drawn from the registry; the `value` is free *within* a namespace but
canonicalized on ingest, so labels cluster instead of scattering. Two hard
rules keep the tag set from rotting:

- **Tags never drive behavior** — only chips and filters. A typo costs a
  filter hit, nothing more.
- **Don't duplicate what `Source`/`Key` already say.** The component, item,
  gate, and agent are derivable facets the orchestrator generates for free;
  tags carry only the *cross-cutting* themes (`topic:`, `area:`) that `Source`
  can't express.

## Ranking — priority × half-life decay

The feed is sorted by a **decayed score** the orchestrator recomputes at read
time. This is the one piece of math the SDK pins (so every orchestrator
ranks identically) while shipping no implementation:

```
halfLife = a.HalfLife if set, else the orchestrator default
age      = now - CreatedAt
score(a) = a.Priority * 2^(-age / halfLife)        // one formula, pinned or not

order:   pinned articles first, then unpinned; within each group, score desc
```

**Scoring is identical for every article** — pinned articles decay with the
same formula (an unset `HalfLife` falls back to the orchestrator default for
both). Pinning affects only the **final ordering and removal**, never the
score: pinned articles are floated into a group *above* all unpinned ones, and
**within the pinned group they sort by the same decayed score** (so two pinned
articles order against each other exactly as two unpinned ones would). The
priority *is* the base weight — no enum→weight table. The named anchors
(`Low=12.5` … `Critical=100`) double each step, so a one-level bump buys
exactly one extra half-life of staying power.

- An article with `HalfLife = 24h` loses half its weight every day: a
  `Critical` (100) posted 24h ago (→50) ties a fresh `High` (50) and still
  outranks a fresh `Medium` (25); a fresh `Critical` (100) outranks them all.
  Tune the half-life to encode "how long should this keep shouting".
- **`Pin == true` means pinned** — the article is **floated above all unpinned
  articles** and the decay floor never decays it out. It still decays
  normally (the score formula is unchanged, `HalfLife` falls back to the
  orchestrator default if unset); decay only orders it *within* the pinned
  group, where the same score logic applies as anywhere else. Use for standing
  conditions that must stay up top and not silently age out (e.g. the
  `data-git-failure` health alarm: it sits above the feed until resolved). Pin
  is an explicit flag, not an overload of `HalfLife`.
- **Decay floor → drops below the fold.** When an *unpinned* `score(a)` drops
  below a configurable floor (`REACTOR_FEED_DECAY_FLOOR`, default e.g. `0.5`),
  the article is marked `decayed` and **collapses below the fold** — out of the
  main linear feed but reachable via the end-of-feed "show decayed" toggle (see
  UI) until it falls past the fixed decayed cap (lowest-ranked dropped first),
  then deleted. It is *not* moved to a separate tab; the feed stays one list. Decay-out differs from a user
  **dismiss** and a creator **resolve**, which remove the article at once:
  decay is the only exit that leaves a short, bounded tail. It keeps the feed
  from accumulating stale informational posts without anyone touching them.
- **Hard expiry — applies to pinned articles too.** Optional `ExpiresAt` (set
  from a TTL on ingest) **deletes** an article regardless of score *or* pin
  state. Pinning exempts an article from decay-out, **not** from a wall-clock
  deadline — a pinned article expires on the same terms as any other.

The floor and the priority weights are orchestrator config, not SDK
constants — the SDK defines the *formula*; the orchestrator owns the
*thresholds* (same division of labor as gate ratchet caps).

## Durable identity, supersede, and resolve

`Key` is creator-chosen and namespaced (`"gate:build-time"`,
`"item:T0481:concern"`, `"breaker:work-stalled"`). It replaces an inferred
`(type, item/agent/entity)` dedup tuple with an explicit, single field the
creator controls. Semantics:

- **First post of a `Key`** creates the article; the orchestrator stamps
  `CreatedAt`.
- **Re-post of an existing `Key`** *supersedes in place*: content/actions/
  priority are replaced. A `freshen` flag on the emit call chooses whether
  `CreatedAt` resets (restart the decay clock — "this got worse again") or is
  preserved (keep aging — "same condition, updated details"). An implicit
  "archive prior, create new" always resets; making it a choice is strictly
  more expressive.
- **Resolve** is the creator's explicit retraction — the **one** creator-side
  removal verb: `POST /api/feed/{key}/resolve` (or `feed.Resolve(key)` from a
  handler). It covers both "the underlying condition is gone" (how
  gate-recovery clears a `gate-failure` article, generalized to any keyed
  article) *and* "this article is simply no longer relevant" (a mis-post or an
  informational item the creator wants gone early) — there is no separate
  `delete`, because in an ephemeral feed both just remove the record. Resolve ≠
  dismiss: resolve is the **creator** retracting; dismiss is the **user**
  acknowledging. Resolve also fires any pending callback on the article.

## Wire format

A single JSON object, `flow:feed-article-v1`:

```jsonc
// flow:feed-article-v1
{
  "schema": 1,
  "key": "gate:build-time",
  "source": { "component": "gate", "name": "build-time" },
  "title": "Build time regressed 18% (42.7s → 50.4s)",
  "description": "`go build ./...` crossed the ratchet baseline …",
  "media": [
    { "kind": "link", "label": "Trend chart", "url": "https://…/gates/build-time" },
    { "kind": "item", "label": "Suspect commit", "reference": "T0481" }
  ],
  "actions": [
    { "id": "open-gate", "label": "Open gate", "kind": "navigate", "primary": true,
      "navigate": { "target": "gate", "reference": "build-time" } },
    { "id": "file-bug", "label": "File bug", "kind": "operation", "after": "resolve",
      "explain": "Files a `perf`-tagged bug linked to this gate and assigns it to you.",
      "operation": { "name": "create-bug", "parameters": { "tag": "perf" } } },
    { "id": "rerun", "label": "Re-run full CI", "kind": "operation", "confirm": true,
      "explain": "Triggers the full paid CI suite (~12 min, billed). Safe but not free.",
      "operation": { "name": "run-gate", "parameters": { "gate": "build-time", "full": "true" } } }
  ],
  "priority": 50,
  "half_life": "48h",
  "tags": ["topic:perf", "area:gates"]
}
```

The gate-output envelope from the gates proposal gains an optional
`articles` array so a gate can post in the same stdout it already emits:

```jsonc
// flow:gate-output-v1 (extended)
{
  "schema": 1,
  "metrics": { "build_seconds": 50.4 },
  "articles": [ { /* flow:feed-article-v1, schema field optional when nested */ } ]
}
```

## The `flow/feed` Go subpackage

A small package — types (above) plus a constructor and emit helpers. Mirrors
`flow/gate`: zero transitive deps pulled into the root.

```go
package feed

// New builds an Article with required fields and sane defaults
// (Schema=1, Priority=Medium). Chainable setters fill the rest.
func New(key, title string) *Article { … }

func (a *Article) Describe(markdown string) *Article   { … }
func (a *Article) Prioritize(weight float64) *Article   { … } // e.g. feed.High, or 60
func (a *Article) Decay(halfLife Duration) *Article     { … } // "" => orchestrator default
func (a *Article) Pin() *Article                        { … } // float above unpinned; exempt from decay-out
func (a *Article) For(role string, reference ...string) *Article { … } // e.g. For("agent", "verifier-1")
func (a *Article) Tag(tags ...string) *Article          { … } // each "namespace:value"; canonicalized on ingest
func (a *Article) Attach(attachment Attachment) *Article { … } // append; the first attachment is the primary
func (a *Article) AddAction(action Action) *Article      { … } // append a call to action

// Emit writes the article as a single flow:feed-article-v1 JSON line to w
// (stdout for the gate/CLI path). The orchestrator-integrated paths
// (StepCtx.Post, MCP, REST) marshal the same struct.
func Emit(w io.Writer, a *Article) error { … }
```

Two things stay deliberately OUT of the SDK (same boundary as gates):

- **The store + ranker + decay timer.** No persistence, no score
  computation, no background sweep in the SDK. The orchestrator owns all
  stateful machinery; the SDK is stateless per emission.
- **The feed UI.** Rendering, grouping, dismiss/CTA wiring are
  orchestrator-side.

## Emission channels

How an `Article` reaches the orchestrator — four routes, one struct:

1. **Flow handler — `StepCtx.Post`.** The natural path for flows. A new
   method on `flow.StepCtx`:
   ```go
   // Post publishes a feed article through the backend. Idempotent on
   // a.Key (re-post supersedes). The backend routes it to the orchestrator;
   // the github backend can no-op or render it as an issue comment.
   func (ctx *StepCtx) Post(a *feed.Article) error
   ```
   Goes through `flow.Backend`, so each backend decides where articles land
   (Reactor → the feed store; github → a marker comment or a no-op). This is
   the one new `Backend` method (`PostArticle(ctx, item, article) error`);
   backends that don't support a feed return `ErrUnsupported` and the SDK
   degrades gracefully.

2. **Gate output.** The `articles[]` field on `flow:gate-output-v1`. The
   orchestrator ingests them when it parses gate stdout — no extra round
   trip. A gate's `Key` defaults to `"gate:<name>"` so recovery/supersede
   "just works" like an entity-keyed gate-failure notification.

3. **MCP `post_article` tool.** For agents/components that talk MCP rather
   than the SDK: `post_article(key, title, description?, priority?,
   half_life?, audience?, tags?, actions?, media?, source?)`. The
   item-question and item-concern paths can use this directly, or the
   orchestrator can keep posting those server-side (see migration).

4. **REST sink `POST /api/feed`.** The single authoritative write path the
   other three converge on, auth-gated by `REACTOR_AUTH_TOKEN` (same posture
   as the patch-upload sink). Body is one `flow:feed-article-v1` object; the
   server stamps `CreatedAt`/`ExpiresAt` and returns the stored article.

## Orchestrator (Reactor) side

This is where Reactor's notification machinery lives. Sketch:

### Store

- `FeedArticle` struct = `feed.Article` + server-owned fields:
  `ID` (internal), `CreatedAt`, `ExpiresAt`, `State` — **just `active` |
  `decayed`** — `ReadAt`, and `TakenActions []string` (which CTAs the user
  invoked).
- **No "archived" state.** There are only two live states. `active` fills the
  main list; `decayed` is the auto-aged-out remainder behind the end-of-feed
  "show decayed" toggle. Every other exit — user **dismiss**, creator
  **resolve**, hard **expire** — simply **removes the record** (the exit reason
  is logged, not stored). There is no archive tab, archive state, or archive
  endpoint; "archived" carried no real weight and is dropped on purpose.
- Keyed by `Key`; supersede = overwrite-in-place with the freshen rule.
- Persisted one-file-per-article (mirrors item storage) or a single
  `_feed.json` — TBD by volume; an in-memory mirror applies if it's hot.
- **Nothing sticks forever — one fixed cap, enforced on decay-in.** At most
  `REACTOR_FEED_DECAYED_MAX` articles (default **1000**) may live under the
  fold. The cap is enforced *only when a new article decays in*: append it to
  the decayed set, and if that pushes the count over the cap, **drop the
  lowest-scoring article(s) beyond the limit**. No periodic tail-scan — the set
  only grows at a decay event, so that's the only moment trimming is needed.
  One global count, lowest-ranked evicted first.
- **No decay TTL on disk.** Decay is computed at read time from
  `CreatedAt` + `HalfLife`; the only background job is a periodic sweep that
  (a) flips below-floor articles to `decayed` — **enforcing the cap inline on
  each flip** (evict the lowest-scoring decayed article when over
  `REACTOR_FEED_DECAYED_MAX`) — and (b) **deletes** past-`ExpiresAt` articles.
  That sweep is a single configurable interval following the no-hidden-timeouts
  contract: `REACTOR_FEED_SWEEP_SECONDS` (default e.g. 300s), value+source
  logged at startup, every flip/delete logged with the article key and reason.

### Feed API

| Method | Path | Purpose |
|---|---|---|
| `GET`   | `/api/feed` | active articles, **server-ranked** (pinned first, then by decayed score desc within each group); optional `?role=` and `?tag=namespace:value` filters |
| `GET`   | `/api/feed/decayed` | the decayed tail backing the "show decayed" toggle (≤ `REACTOR_FEED_DECAYED_MAX`, ranked) |
| `POST`  | `/api/feed` | ingest one article — first post creates, re-post of a `Key` **supersedes** (full update) |
| `PATCH` | `/api/feed/{key}` | partial **update** of an existing article (change title/priority/actions/… without a full re-post) |
| `POST`  | `/api/feed/{id}/read` | mark read |
| `POST`  | `/api/feed/{id}/dismiss` | user dismiss → **removed** (reason logged) |
| `POST`  | `/api/feed/{id}/action` | take a CTA: `{action_id, choice?: []string, confirmed?: bool}`; rejects an unconfirmed action that needs confirmation (`Confirm` or `Destructive`), else performs the local effect, applies the action's `After` disposition (keep/dismiss/resolve), AND delivers the outcome back to the source (callback/choice) |
| `POST`  | `/api/feed/{key}/resolve` | creator retraction → **removed**: the condition cleared *or* the article is simply no longer relevant. The one creator-side removal verb; fires any pending callback |

Ranking happens server-side so every client (web, future CLI `feed` command)
sees one ordering. The SSE hub pushes feed deltas the same way the inbox is
live today.

### Feed tab (UI)

**One linear list — no tabs inside the feed.** The Feed is itself a tab in the
Reactor UI, so nesting tabs (by state, by type, …) inside it is out;
sub-grouping is done by *ordering* and an end-of-feed disclosure, never by tabs.
The list is: pinned articles first (with a **pin marker**), then the
score-ranked active remainder, scrolling as far as there are material articles.

- **Decayed below the fold.** Articles that fell under the decay floor don't
  vanish and don't move to a tab — they collapse beneath a single end-of-feed
  toggle: *"Show N decayed"*. This is the **only** below-the-fold bucket:
  dismissed/resolved/expired articles are gone, not tucked away. The decayed
  tail is hard-capped (default 1000, lowest-ranked dropped first), so it's a
  bounded tail, not an archive.
- **Empty (nothing material).** When no article is above the floor, show a
  calm resting state rather than a blank pane — e.g. **"All caught up —
  nothing needs your attention"** — with the "show decayed" toggle still
  available beneath it if any decayed remain.
- **Filtering, not tabs.** The single list can be narrowed by a text search
  and by facet filters (audience role: "for me" = `operator` vs everyone;
  namespaced tag chips — group by `namespace`, filter by `namespace:value`).
  Filters refine the one list in place; they never split it into tabs.
- **Selecting an article opens a detail view** beside the list (a side panel)
  or as a modal — TBD — showing the full `Description`, all media, every
  action with its `Explain`, and the source/lifecycle metadata. The list stays
  put behind/around it.

Per-article card (in the list):

- Priority/decay indicator (the live score, or a freshness bar), a pin marker
  when pinned, source attribution, title, time-ago, tags.
- Primary CTA rendered prominently; alternatives in an overflow; dismiss
  always present (the implicit action). Multiple-choice articles render the
  option set inline (same shape as the `ask_user_question` answer UI).
  `Destructive` actions take caution/red styling; `Confirm` actions (and, as a
  safety floor, destructive ones) pop a confirm dialog whose body is `Explain`
  and whose proceed button echoes the action `Label` (vs *Cancel*). `Explain`
  otherwise shows as a hover/focus tooltip.
- Media: the first attachment inline (thumbnail/link); any remaining
  attachments subdued behind a disclosure.

Must follow the existing design language (theme tokens, card/badge/button
patterns) and ship tests per the mandatory-coverage policy (card render
empty+populated, the "all caught up" resting state, the decayed-toggle reveal,
dismiss, take-action, destructive styling, confirm-gate (proceed + cancel), SSE
delta, multiple-choice submit, and each `After` disposition keeping/removing the
card).

## Mapping a notification inbox onto the feed

Every notification creator/remover becomes an article post — no behavior
lost, dedup made explicit:

| Notification | Becomes (`feed.Article`) |
|---|---|
| `gate-failure` (entity=gate) | `Key="gate:<name>"`, `Source{gate}`, `Priority=High`, `Pin`, actions: open-gate / create-bug |
| gate recovery resolve | `POST /api/feed/gate:<name>/resolve` |
| `inspection-concern` (itemID) | `Key="item:<id>:concern"`, `Source{item-concern,itemID}`, actions: open-item / create-task |
| `closure-review` (itemID) | `Key="item:<id>:closure"`, `Source{orchestrator,itemID}`, `Priority=Low`, decays |
| `suggestion` (itemID) | `Key="item:<id>:suggestion"`, actions: open-item / create-task |
| `verify-result` / `gate-result` | `Key="agent:<name>:verify"`, pass→`Low`+short half-life (ages out fast), fail→`High` |
| `work-summary` | `Source{orchestrator}`, `Priority=Low`, short half-life — informational, decays out on its own (fixes the "3-week-old summary still in the list" problem) |
| `work-stalled` / `data-git-failure` | `Pin` + `Critical` — floats on top, never decays out; removed only on resolve |
| `ask_user_question` (item question) | article with a `choice` action; selection callback unblocks the item |
| auto-archive-prior dedup | explicit `Key` supersede |
| user "Dismiss" button | `POST /api/feed/{id}/dismiss` |

The item-question case is the interesting unification: the `ask_user_question`
MCP tool is otherwise a bespoke path (appends to the item, flips status to
`needs_answer`, frees the agent). Under the feed model it *also* posts a
`choice` article so the question is visible in the feed, and the answer
callback drives the same item transition. The MCP tool keeps its item-side
contract; it gains a feed projection for free.

## Open questions

1. **The user-facing noun.** `Brief` / `Post` / `Bulletin` / `Dispatch` /
   `Notice` (see Naming). Settle before the `feed.Article` type name is
   public.

2. **Priority anchors (numeric chosen).** Priority is a raw `float64`
   (higher = more important, the value is the base score). Resolved in favor
   of numeric over an enum: the value *is* the weight, so there's no
   enum→weight table, and a creator can sit between anchors. Remaining
   sub-question: should the wire *also* accept the anchor names as string
   sugar (`"high"` → 50) for hand-authored JSON, or stay strictly numeric?

3. **Audience & identifier registry seeding.** Audience is resolved to a
   structured `{role, ref}` (not a free string) — see Identifiers/Audience.
   Open: the exact seed sets for the three registries (component values,
   audience roles, tag namespaces) and whether a not-yet-registered value is
   *rejected* at the write sink or *accepted-but-flagged* (this proposal leans
   accepted-but-flagged so a new component is never blocked, with the typo cost
   bounded to a mislabeled chip). Also: is the registry global, or per-flow/
   per-source-scoped?

4. **Callback delivery to flows/gates.** A gate is a one-shot process — by
   the time the user takes a CTA, the gate process is long gone. Choice/
   callback outcomes for gate-sourced articles must be *durably queued* (like
   the runner action queue) and delivered on the gate's next run, or routed
   to a flow that owns the follow-up. For flow-sourced articles the
   `needs_answer`-style re-dispatch already exists. Define the callback
   envelope and the at-least-once delivery contract.

5. **Decay floor & curve as config vs per-article.** The decay floor is
   global orchestrator config (priority is now per-article and carries its own
   weight). Some articles might want a custom curve (e.g. linear, or a hard
   cliff). Keep v1 to the single exponential half-life; revisit if a real case
   needs another shape.

6. **Does the SDK ship `feed` at all, or only the wire format?** Like gates,
   the contract is the JSON, not the Go API. The `flow/feed` package is
   convenience for Go authors; a shell/Python component emits the JSON
   directly. Confirm the Go package is worth the surface area vs
   wire-format-only.

## Why this shape

- **Open extension point, not a closed enum.** Any component posts via one
  struct; the orchestrator never branches on component type. A new surface
  needs zero orchestrator *code* changes — it registers as data — yet its
  identifiers stay in a controlled vocabulary, so the feed doesn't decay into
  free-text soup the way a closed inbox's ad-hoc strings would.
- **Priority + decay replace manual triage.** The feed self-curates:
  important things rank up, stale things age out, pinned conditions stay
  loud. No human has to clear a `work-summary` backlog.
- **Calls to action are data, not frontend conditionals.** The creator
  declares what the user can do; the UI renders it generically. The
  hardcoded `switch (n.type)` action buttons disappear.
- **Same SDK/orchestrator boundary as gates.** Stable wire format = the
  contract; the Go package is convenience; all stateful machinery
  (store/rank/decay/UI) stays orchestrator-side.
- **`ask_user_question` becomes a special case of the general thing** — the
  feed subsumes the one bespoke user-engagement path the system already has.

## Migration path

1. Land `flow/feed` types + helpers (no `Backend`/`StepCtx` changes yet).
2. Add the `articles[]` field to `flow:gate-output-v1` (additive).
3. Add `flow.Backend.PostArticle` + `StepCtx.Post`; github backend returns
   `ErrUnsupported` (or renders a comment) for now.
4. **In the Reactor repo** (separate change): build the `FeedArticle` store,
   the `/api/feed` endpoints, the ranker + sweep, the MCP `post_article`
   tool, and the `Feed` tab — then **route every existing notification call
   site through `PostArticle`** with the keys from the mapping table. The old
   `Notification` type and inbox endpoints stay as a read-only compatibility
   shim during cutover, then are deleted.
5. Update `docs/design.md` with a top-level "Engagement feed (optional)"
   section linking back here.

No changes required to existing artifacts/signals/flows — the feed is
additive, exactly like gates.
