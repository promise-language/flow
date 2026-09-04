package flow

import (
	"os"
	"slices"
	"strings"
)

// This file holds every name that crosses the orchestrator boundary, and the
// two vocabularies that classify an item without naming one.
//
// The rule the set exists to enforce is docs/orchestrator.md's: **no identity
// is ever a bare `string`**. A parameter, field or return that identifies
// something is one of the types below. A `string` standing in for one is a
// defect, not a shorthand — it type-checks against every other string in the
// program, so an ArtifactId reaches a method expecting a StepId and nothing
// says so until the wrong record is written.
//
// ItemRef, ArtifactId, SignalId, ItemType and GateName are identities too and
// live beside what they address (orchestrator.go, artifact.go, signal.go).

// AccountId is a person's account in the ORCHESTRATOR's own namespace — for
// GitHub, the authenticated login.
//
// It is never passed in and always derived from the credentials the arena acts
// as. An arena that can commit, push and merge has exactly one account by
// construction, so the orchestrator reads it rather than being told, and there
// is no way for a caller's idea of the account to differ from the one the work
// is actually done as.
//
// Attribution only. It is not what a lease binds to: a claim is item ↔ arena,
// never item ↔ user.
type AccountId string

// Valid reports whether the account id is storable. It becomes a label
// (flow:owner:<account>) and an assignee, so it carries the same floor as
// TagId: non-empty, single-line, no edge whitespace.
func (a AccountId) Valid() bool { return validName(string(a)) }

// HostId is the machine an arena lives on: the host's SHORT NAME — its first
// dotted segment — normalized.
//
// It MUST be unique across every host the orchestrator can see. A duplicate is
// not a cosmetic collision: arenas are addressed by (HostId, ArenaId), so two
// machines sharing an id merge into one identity and the one-to-one claim
// invariant stops holding. Uniqueness cannot be inherited from the FQDN,
// because the short form drops the domain — build01.us-east and build01.eu-west
// both normalize to build01 — so a fleet spanning domains must assign unique
// short names rather than relying on the domain to separate them.
type HostId string

// ArenaId names a worktree plus a stable identity, where work physically
// happens. It is UNIQUE ONLY WITHIN ITS HOST, so it is a component and not an
// identity: it names an arena no more than a house number names a house. Pair
// it with a HostId — see Arena.
type ArenaId string

// StepId is one step within a flow: its ArtifactId when it produces an
// artifact, its SignalId when it completes on a signal.
//
// Inside a flow the two are a single namespace — registering a second step
// under an id already taken is refused whichever kind it is — so a StepId is
// unambiguous without carrying a discriminator. The two vocabularies stay
// independent everywhere else; it is the flow that merges them.
//
// A StepId that is an ArtifactId also keys a budget record; one that is a
// SignalId keys none, which is why the budget methods take an ArtifactId and
// not this.
type StepId string

// QuestionId names one asked question. Assigned by the orchestrator in
// AskQuestion and returned on the persisted Question; opaque to the SDK.
//
// Unique WITHIN ITS ITEM, which is the scope every consumer needs: `answer
// --question <id>` matches among one item's pending questions. It must be
// stable for the life of the question — the id is read from one command and
// passed to another.
type QuestionId string

// TagId is an area of work in the orchestrator's own vocabulary: a label
// carried by an item.
//
// Free-form, but not every string is one — see Valid. The floor is
// load-bearing rather than decorative: a TagId is interpolated into the
// orchestrator's own query, where a value containing a space does not fail, it
// silently becomes a different query.
//
// Compared by EXACT equality, by TagsMatch and by nothing else.
type TagId string

// Valid reports whether the tag is above the floor every orchestrator must
// accept: non-empty, single-line, and carrying no leading or trailing
// whitespace. Below it a value is not a tag and is refused rather than stored.
// Above it, validity is the orchestrator's own.
func (t TagId) Valid() bool { return validName(string(t)) }

// validName is the shared floor for TagId and AccountId — both become labels,
// both are interpolated into a query, and both are refused rather than mangled
// below it. One implementation because it is one rule: docs/orchestrator.md
// gives AccountId "the same floor as TagId" in those words.
func validName(s string) bool {
	if s == "" {
		return false
	}
	if strings.ContainsAny(s, "\n\r") {
		return false
	}
	return strings.TrimSpace(s) == s
}

// TagsMatch reports whether `have` carries every tag in `want`. The comparison
// is EXACT and the filter is CONJUNCTIVE.
//
// It is the only tag comparison in the SDK, and an orchestrator filtering
// server-side must post-filter through it. Without that, one --tag value means
// two different things across `list` and `resolve`, which are meant to read as
// symmetrical: a search index is case-insensitive and lagged, and exact
// equality is the contract.
//
// An empty `want` matches everything — "no filter", which is what an empty
// --tag set means.
func TagsMatch(have, want []TagId) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// CommandName names one command the arena can run. A command MAY MODIFY the
// worktree or the arena environment, which is what separates it from a gate and
// why no decision may rest on what one reports.
//
// The set is CLOSED AT THREE. A project does not invent a fourth, because the
// flow decides when each runs and would have no place to run one it did not
// know about — unlike gates, which a project composes into `integration`
// freely.
type CommandName string

const (
	// CommandSetup prepares the arena before a claim is taken. Optional.
	CommandSetup CommandName = "setup"
	// CommandVerify repairs what is mechanically repairable and reports on the
	// rest. Required: a step should not fail over something verify would have
	// fixed.
	CommandVerify CommandName = "verify"
	// CommandCleanup tidies after a resolution. Optional, and the one thing
	// here NOT guaranteed to run at all — the arena it would clean can be gone
	// by the time it would run. Nothing may depend on it having run.
	CommandCleanup CommandName = "cleanup"
)

// AllCommandNames returns every declared command, in declaration order.
// Consumers enumerate it rather than mirroring the set, which is how two copies
// of one vocabulary drift.
func AllCommandNames() []CommandName {
	return []CommandName{CommandSetup, CommandVerify, CommandCleanup}
}

// Valid reports whether c is one of the three. The empty name is not one.
func (c CommandName) Valid() bool { return slices.Contains(AllCommandNames(), c) }

// BinaryName is which binary is asking: the BARE executable name — `issue`,
// never `bin/issue`.
//
// The bareness is a requirement, not a convention: the value becomes a label
// (flow:issue) and is interpolated into the query that finds it, so a path
// separator would name a label the orchestrator cannot hold and silently
// corrupt the search.
//
// It decides what counts as processable and auto — the same item is unhandled
// to one binary and workable to another — so it is an input to availability,
// not a property of the item.
type BinaryName string

// OrchestratorName is which orchestrator owns an item. Returned by Name() and
// carried on every ItemRef; a ref whose OrchestratorName is not this
// orchestrator's is not this orchestrator's to interpret.
type OrchestratorName string

// BranchName is a line of work in the worktree: a git branch.
type BranchName string

// Revision is something resolvable to a commit — whatever RevParse is asked
// for. Only "HEAD" and the item's base branch must resolve.
type Revision string

// HeadRevision is the one revision every orchestrator must resolve, alongside
// the item's base branch.
const HeadRevision Revision = "HEAD"

// CommitSha is one commit: what RevParse returns.
type CommitSha string

// RequestUrl is an opened pull request. Produced only by Open; consumed by
// Merge and FindPR. Orchestrator-specific in form and opaque to the SDK, but
// never a bare string at the boundary.
type RequestUrl string

// Arena is the pair that identifies an arena, and the only form unique beyond a
// single machine. An orchestrator enforcing one-to-one claims compares this,
// never the ArenaId alone.
type Arena struct {
	Host HostId  `json:"host"`
	Id   ArenaId `json:"id"`
}

// Empty reports whether the arena names nothing. Both halves are the identity,
// so a pair missing either one identifies no arena.
func (a Arena) Empty() bool { return a.Host == "" || a.Id == "" }

// Holder is who holds an item: the arena the lease binds to, and the account
// credited for it. The account is attribution; the arena is the lease.
type Holder struct {
	Arena   Arena     `json:"arena"`
	Account AccountId `json:"account"`
}

// Empty reports whether the item is unclaimed.
func (h Holder) Empty() bool { return h.Arena.Empty() && h.Account == "" }

// Blocker is one declared dependency: the item waited on, and its own
// ItemStatus.
//
// The status comes with the reference because the list alone answers the wrong
// question. "These items were declared as blockers" is not "this is what you
// are waiting on" — an operator wants the one still open, and without its
// status would have to look up every entry to find it. The orchestrator is not
// doing extra work to say so: it CANNOT derive whether the item is blocked
// without already knowing which blockers are unfinished.
type Blocker struct {
	Ref    ItemRef    `json:"ref"`
	Status ItemStatus `json:"status"`
}

// ItemStatus is where an item stands in the ORCHESTRATOR's own lifecycle.
// Closed at two.
//
// Distinct from Availability, which says whether THIS BINARY could work the
// item now, and from Item.Finalized, which says whether the FLOW is done with
// it — an item can be terminal and not finalized (closed by hand mid-run) or
// finalized only because it reached terminal first.
type ItemStatus string

const (
	// StatusOpen — work may still happen.
	StatusOpen ItemStatus = "open"
	// StatusTerminal — it will not. Finalize refuses anything that is not this.
	StatusTerminal ItemStatus = "terminal"
)

// AllItemStatuses returns every declared status, in declaration order.
func AllItemStatuses() []ItemStatus { return []ItemStatus{StatusOpen, StatusTerminal} }

// Valid reports whether s is one of the two. The empty status is not one.
func (s ItemStatus) Valid() bool { return slices.Contains(AllItemStatuses(), s) }

// BlockKind says WHO MUST ACT, AND ON WHAT, for the item to become workable.
// Closed at three. Each names an actor and a subject, which together are the
// only thing a caller does differently.
//
// The causes within each are open-ended — disabled, unmet dependency, rate
// limit, whatever an orchestrator adds next — but the responses are not, which
// is why this is a field and the reason is prose.
type BlockKind string

const (
	// WaitsOnItems — actor: whoever can work the blockers. Subject: those
	// items, named in BlockedBy. It clears when they finish and nobody touches
	// this item at all.
	WaitsOnItems BlockKind = "waits-on-items"
	// WaitsOnCondition — actor: nobody. Subject: nothing addressable — a
	// network away, a service down, a rate limit. It clears on its own and
	// there is nothing to go work; the only response is to come back.
	WaitsOnCondition BlockKind = "waits-on-condition"
	// WaitsOnPerson — actor: a person. Subject: this item — answer its
	// question, grant its budget, re-enable it. It will not clear on its own.
	WaitsOnPerson BlockKind = "waits-on-person"
)

// AllBlockKinds returns every declared kind, in declaration order.
func AllBlockKinds() []BlockKind {
	return []BlockKind{WaitsOnItems, WaitsOnCondition, WaitsOnPerson}
}

// Valid reports whether k is one of the three. The empty kind is not one: an
// item that is blocked has a kind, and an item that is not carries no block at
// all.
func (k BlockKind) Valid() bool { return slices.Contains(AllBlockKinds(), k) }

// NormalizeHostId reduces a machine name to its HostId: the first dotted
// segment, lowercased.
//
// The normalization is a requirement rather than a courtesy — these are
// compared by string equality across systems, so two spellings of one host are
// two hosts.
func NormalizeHostId(name string) HostId {
	name = strings.TrimSpace(name)
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	return HostId(strings.ToLower(name))
}

// DeriveHostId returns this machine's HostId, normalized from its own name.
//
// Derivation is what makes the ordinary case take no configuration; an explicit
// setting replaces it WITHOUT changing the FQDN, which is what lets "the same
// machine again" be told apart from "a second machine with the same short
// name". This returns the derived half only — where an override lives is the
// registry's business.
//
// A machine that cannot name itself yields the empty HostId rather than a
// guess: an invented name would be a second machine's identity waiting to
// collide.
func DeriveHostId() HostId {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return NormalizeHostId(name)
}
