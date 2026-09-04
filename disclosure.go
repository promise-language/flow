package flow

import (
	"context"
	"slices"
)

// Origin names the party standing behind a string that is about to be
// published. It is not a category of text — two of the six name nobody, and
// that is what the set is for.
//
// The caller supplies it and the guard never infers it. Provenance is not a
// property of text: a paragraph about another project's architecture reads
// exactly like a paragraph about this one, and provenance is precisely what is
// lost when text is copied. But it IS known to whoever is about to publish, so
// the party that has the fact states it and the guard decides from it.
//
// The set is closed, because an open one is a set a caller extends by
// inventing a name that means "fine, probably". Text that fits none of these
// is not a new origin; it is OriginElsewhere, which exists to have somewhere
// honest to put it.
//
// There is deliberately no `unknown`. A caller that cannot state an origin
// cannot make the call, and a guard that is not called permits nothing — the
// write does not happen. An `unknown` member would turn the one case this
// exists for into a value that can be passed, and a value that can be passed
// is a value someone defaults to.
//
// docs/disclosure.md § "The set of origins is closed" is the definition; this
// is its declaration.
type Origin string

const (
	// OriginWorktree — a file read from the tree under resolution. Vouched for
	// by the tree, which is what is being published about.
	OriginWorktree Origin = "worktree"

	// OriginItem — the item being resolved: its title, body, comments. Vouched
	// for by the destination itself, which has already published it.
	OriginItem Origin = "item"

	// OriginFlow — the SDK: templates, headings, its own error strings.
	// Vouched for by the flow, which is delivered from outside the tree.
	OriginFlow Origin = "flow"

	// OriginOperator — a person, typed.
	OriginOperator Origin = "operator"

	// OriginAgent — an agent composed it, and what went into it is not known
	// to the caller. Vouched for by NOBODY, which is the true thing to say:
	// the agent may have had several repositories in context. It earns the
	// strictest examination precisely because there is nothing behind it, and
	// it is the most common origin in practice — almost everything a
	// resolution publishes was composed by an agent.
	OriginAgent Origin = "agent"

	// OriginElsewhere — anywhere else: another repository, another session,
	// another machine. Vouched for by nobody, and known not to belong.
	OriginElsewhere Origin = "elsewhere"
)

// AllOrigins returns every declared origin, in declaration order. A guard
// enumerates it rather than mirroring the set, which is how two copies of one
// vocabulary drift.
func AllOrigins() []Origin {
	return []Origin{
		OriginWorktree, OriginItem, OriginFlow, OriginOperator, OriginAgent, OriginElsewhere,
	}
}

// Valid reports whether o is a declared origin. The empty string is not one:
// an origin that cannot be stated is a refusal, not a default.
func (o Origin) Valid() bool { return slices.Contains(AllOrigins(), o) }

// Text is one string as it will be sent, with the party standing behind it.
//
// One string rather than a whole write's body, because a single write commonly
// publishes several strings with different origins: a push carries a branch
// name the flow chose, commit messages an agent wrote, and a diff read out of
// the worktree. Collapsing them would mean stating one origin for all three,
// which means stating a false one for two.
//
// Body is the final bytes. Not the template, not the artifact before assembly,
// not the prose before the SDK wrapped it in a heading — anything examined
// before its last transformation can have content introduced after the
// examination.
type Text struct {
	Origin Origin
	Body   string
}

// DisclosureAct names what is being published. The set is closed: a write
// whose act is not named here does not exist, and adding one means adding it
// here.
//
// It lives in the SDK rather than in a backend so that a guard supplied from
// outside can switch on the whole vocabulary without importing a backend and
// its client library.
type DisclosureAct string

const (
	ActArtifactComment DisclosureAct = "artifact-comment"
	ActStateComment    DisclosureAct = "state-comment"
	ActParkRecord      DisclosureAct = "park-record"
	ActQuestion        DisclosureAct = "question"
	ActAnswer          DisclosureAct = "answer"
	ActLabel           DisclosureAct = "label"
	ActPullRequest     DisclosureAct = "pull-request"
	ActMerge           DisclosureAct = "pull-request-merge"
	ActPush            DisclosureAct = "push"

	// ActAssignee maps to the Assignees row. ActArtifactFile maps to
	// Issue comments — same artifact, different route when it exceeds
	// the comment size limit.
	ActAssignee     DisclosureAct = "assignee"
	ActArtifactFile DisclosureAct = "artifact-file"

	// ActItemEdit is a change to the item's own request — its title and body —
	// through ItemEditor. It is a disclosure like any other: the result is
	// visible to everyone who can see the item and is not undone by forgetting
	// it happened.
	//
	// It is distinct from ActLabel because the two carry different things and a
	// guard decides differently about each: a label is a name the flow
	// constructed, while a title and a body are prose that can carry anything a
	// caller put in them. An editor staging both proposes both, each with its
	// own origin.
	ActItemEdit DisclosureAct = "item-edit"

	// ActBlocker records that one item waits on another. The reference is
	// published on the item and visible to everyone who can see it.
	ActBlocker DisclosureAct = "blocker"
)

// AllDisclosureActs returns every declared act, in declaration order. A guard
// enumerates it to be sure it has an answer for each; a test enumerates it to
// be sure each has a call site behind it.
func AllDisclosureActs() []DisclosureAct {
	return []DisclosureAct{
		ActArtifactComment, ActStateComment, ActParkRecord, ActQuestion, ActAnswer, ActLabel,
		ActPullRequest, ActMerge, ActPush, ActAssignee, ActArtifactFile,
		ActItemEdit, ActBlocker,
	}
}

// Valid reports whether a is a declared act.
func (a DisclosureAct) Valid() bool { return slices.Contains(AllDisclosureActs(), a) }

// Disclosure is one proposed outward write: the final bytes, each with the
// party standing behind it, plus where they were going.
type Disclosure struct {
	// Act is what is being published. Always one of AllDisclosureActs.
	Act DisclosureAct

	// Owner and Repo are the repository the write would reach.
	Owner string
	Repo  string

	// Item is the backend's id for the item being written to, or "" when the
	// write is not item-scoped. A string rather than a number because the SDK's
	// item ids are opaque; only the backend knows their shape.
	Item string

	// Ref is the git ref or branch being written to, or "".
	Ref string

	// Text is every string this write publishes, as it will be sent.
	Text []Text
}

// DisclosureGuard examines a proposed write and answers whether it may happen.
//
// The flow declares this seam and cannot fill it. A concrete implementation
// living here would be code inside a tree that agents edit, rebuildable by the
// party it refuses; a shape the flow declares and something else fills means
// the flow can state THAT every write is checked without owning WHAT the check
// permits. So an implementation is supplied from outside and injected, the
// same way a backend and an agent are — and a flow with nothing injected does
// not publish.
//
// The answer is definitive. A nil error allows; any non-nil error refuses and
// IS the reason, returned to whoever wrote the text. No confidence, no maybe,
// no allowed-with-a-warning: a guard that returns a degree of concern has
// moved the decision to the caller, and the caller is the party trying to
// publish. A refusal must name what it found and where, and quote enough to
// act on — "this may contain private information" is indistinguishable from a
// guard that gave up.
//
// It never modifies what it examines. A guard that redacted and forwarded
// would be worse than none: the author would never learn what was caught, and
// nobody could tell text that was clean from text that was quietly rewritten.
//
// There is no override parameter, deliberately. A person may override a
// refusal and nothing else may — the party proposing a disclosure is exactly
// the party that must not be able to authorise it — so an override is arranged
// between a person and the guard, and recorded there with the text it
// permitted. A caller cannot ask for one.
//
// It must answer fast enough to sit on every path it is on, which rules out a
// model call and a network round trip: a guard that adds a visible pause is a
// guard someone turns off, and a guard that is off is indistinguishable from
// one that was never written.
type DisclosureGuard interface {
	Examine(ctx context.Context, d Disclosure) error
}
