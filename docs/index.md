# Documentation Index

This is the map of `docs/`. It is the one file in the root that is not a specification —
everything else there is.

**The rules are defined once, org-wide, in [org/normative.md](org/normative.md)**: which
locations bind and which do not, the tag header, where status lives and where it never does,
one fact one home, the lifecycle of a specification, and what is enforced mechanically. This
index does not restate them.

**This project's status query.** Each root document's tag is a GitHub label, spelled as the
file's basename minus `.md`, and the remaining work for a document is:

> `gh issue list --label <tag> --state open --limit 200`

`--state open` and `--limit 200` are both written out deliberately: `gh issue list` defaults to
a limit of 30, so a specification with more remaining work than that would silently
under-report and read as nearly done.

## Specifications

- [resolution.md](resolution.md) — How an item is resolved, independent of what drives it; the
  two drive models.
- [resolution-standalone.md](resolution-standalone.md) — The model where the binary is the
  whole system, no server.
- [resolution-orchestrated.md](resolution-orchestrated.md) — The model where a central server
  schedules and the binary runs one step.
- [cli.md](cli.md) — The complete, closed command surface every `cli.Run` binary exposes.
- [gates-and-commands.md](gates-and-commands.md) — What a project must supply for a flow to run
  against it, and the names for those things.
- [environment.md](environment.md) — What makes a machine fit to be given an item, and what
  follows from unfitness.
- [disclosure.md](disclosure.md) — What the flow sends outward and the guard every outward byte
  passes.
- [agent.md](agent.md) — The `Agent` interface: the metered chokepoint, permission modes,
  failure kinds.
- [orchestrator.md](orchestrator.md) — The SDK–orchestrator boundary: required methods, what
  may be refused.
- [step-handler.md](step-handler.md) — What a handler receives, must do, may do, and may
  return.
- [flow-registration.md](flow-registration.md) — How a flow declares steps, item types, signal
  preconditions.
- [artifacts-and-signals.md](artifacts-and-signals.md) — The two result kinds and the
  vocabulary depending on them.
- [issue-flow.md](issue-flow.md) — The concrete step set this repository ships for resolving
  issues.
- [github-schema.md](github-schema.md) — The on-issue wire format: state comment, artifact
  comments, orphan branch.

## Organization-wide corpus — binding

Vendored from [promise-language/org](https://github.com/promise-language/org) at the release
named in [org/stamp.json](org/stamp.json). Never edited here: an issue about one of these
documents is filed against `org` (org/normative.md §7); what this project files locally under
their tags is its own compliance gaps.

- [org/normative.md](org/normative.md) — What makes a document binding, and the one docs
  structure every project holds.
- [org/engineering-guide.md](org/engineering-guide.md) — How code in this organization is
  written, in any language.
- [org/engineering-guide-promise.md](org/engineering-guide-promise.md) — The engineering guide
  applied to Promise source.
- [org/engineering-guide-go.md](org/engineering-guide-go.md) — The engineering guide applied to
  Go source.
- [org/cli-guide.md](org/cli-guide.md) — How every command-line tool behaves at its invocation
  surface.
- [org/stamp.json](org/stamp.json) — The version stamp: the org release these copies came from,
  with per-file hashes.

## Proposals — not binding

- [proposals/anchoring.md](proposals/anchoring.md) — Aspects whose modification requires a
  person to approve it. Superseded in direction by org's anchoring proposal, which widened it
  to the fleet.
- [proposals/grant-and-step-identity.md](proposals/grant-and-step-identity.md) — Grants and how
  a step proves it is the step it claims.
- [proposals/multi-project.md](proposals/multi-project.md) — One binary resolving items across
  projects.
- [proposals/reusable-flows-and-leases.md](proposals/reusable-flows-and-leases.md) — Flows as
  reusable definitions, and leases.

## Archive — superseded or delivered

- [archive/design.md](archive/design.md) — The original storage-schema design; archived, and
  the gap of a normative successor is carried as an issue.
