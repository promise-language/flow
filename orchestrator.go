package flow

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"time"
)

// ItemRef is the item identity, and the only one. The SDK carries it whole and
// interprets none of it; only the originating orchestrator reads Ref.
//
// Its parts are NOT identities: the orchestrator-internal address, the
// human-readable Display, and the orchestrator's own store key are all
// PROJECTIONS of the ref. Nothing may be reconstructed into a ref from a
// projection — that is what ResolveRef exists to do.
type ItemRef struct {
	OrchestratorName OrchestratorName `json:"orchestrator"`
	Display          string           `json:"display"` // for logs / UI; e.g. "owner/repo#123"
	Ref              json.RawMessage  `json:"ref"`     // orchestrator-internal addressing
}

// Item is what Load returns: the item, and what the flow recorded on it.
//
// Nothing in it is a state value — an item's states are ItemStatus,
// Availability and the two flags below. It is FLAT: an item's own fields and
// the flow's record of working on it are read together, by one call, and
// nothing needs half of it.
//
// It carries everything ItemInfo does except Availability, and the exception is
// the whole reason there are two types. Every other field is true of the item
// whoever asks; availability is computed FOR A BINARY, and Load is not told
// which one. A caller holding an Item can therefore answer "is this blocked,
// who holds it, what is it tagged" without a second call — it cannot answer
// "would this binary pick it up".
//
// It carries NO STORE ID. The orchestrator's own key for the item is a
// projection of ItemRef, and a caller that has the item has the ref it was
// loaded by. Callers that want something to print use Ref.Display.
type Item struct {
	Ref   ItemRef
	Type  ItemType // routes flow selection; MUST be non-empty
	Title string
	Body  string
	URL   string // where a person reads the request
	Flow  string // last selected flow name, when known

	// Status is the orchestrator's own lifecycle position; Disposition is its
	// own name for it ("done", "won't fix", "duplicate"), carried alongside for
	// display and interpreted by nothing here.
	Status      ItemStatus
	Disposition string

	// Holder is the arena holding the item and the account credited, or the
	// zero Holder when unclaimed.
	Holder Holder

	// Tags is every TagId the item carries — the operator's and the
	// orchestrator's own markers alike, never filtered to what a flow
	// recognises.
	Tags []TagId

	// BlockedBy is every blocker DECLARED on the item, each with its own
	// ItemStatus, and BlockReason is one line for a person. A blocker that has
	// since finished stays listed until someone retracts it: a set that quietly
	// dropped satisfied entries could not be edited, because nothing could see
	// what was there to remove.
	BlockedBy   []Blocker
	BlockReason string

	// Finalized marks the item's flow run as complete — the sole terminal "no
	// more work" record, set only by Finalize. Load MUST report it truthfully:
	// a write nothing can observe is not a record.
	Finalized bool

	// Manual reports that an operator has taken hand control. It stops anything
	// dispatching the item underneath the person now driving it. Load MUST
	// report it truthfully.
	Manual bool

	Artifacts map[ArtifactId]ArtifactRecord
	Signals   map[SignalId]SignalState

	// Questions is every question asked, answered or not, each with its
	// QuestionId. Questions are never removed — one leaves the pending set by
	// being answered, not by being deleted.
	Questions []Question

	// Park is the item's current park record, or nil when it is not parked. It
	// says WHY an item stopped (which step, which budget axis) instead of
	// leaving a caller to infer it; `grant` with no arguments reads it to top up
	// exactly the axis that parked the step.
	Park *ParkRequest
}

// Parked reports whether the item currently carries a park record.
func (i *Item) Parked() bool { return i != nil && i.Park != nil }

// PendingQuestions returns the subset of Questions that have not yet been
// answered (UserAnswer.Answer is empty). The flow parks while this is
// non-empty, and the outstanding-question marker clears only when it is not.
func (i *Item) PendingQuestions() []Question {
	if i == nil {
		return nil
	}
	out := make([]Question, 0, len(i.Questions))
	for _, q := range i.Questions {
		if q.Answer == "" {
			out = append(out, q)
		}
	}
	return out
}

// Artifact looks up an artifact record by id; returns the zero record if
// absent. Convenience for derivation paths that don't care about the ok-value.
func (i *Item) Artifact(id ArtifactId) ArtifactRecord {
	if i == nil {
		return ArtifactRecord{}
	}
	return i.Artifacts[id]
}

// HasRequiredArtifacts reports whether the item has a seeded finalization
// checklist — i.e. at least one artifact record marked Required. It is the
// "is this item seeded?" predicate used by cli.RunOne's mandatory-seed gate: an
// item with no required artifact has not been seeded and the flow must not run
// any step against it.
func (i *Item) HasRequiredArtifacts() bool {
	if i == nil {
		return false
	}
	for _, rec := range i.Artifacts {
		if rec.Required {
			return true
		}
	}
	return false
}

// SignalSet returns true iff the named signal is set on the item.
func (i *Item) SignalSet(id SignalId) bool {
	if i == nil {
		return false
	}
	return i.Signals[id].Set
}

// ItemInfo is the orchestrator's standing on an item, returned by List and Get.
//
// It is the listing projection: cheap enough to return hundreds of, so it omits
// artifacts, signals, questions and park. Where it and Item overlap they mean
// the same thing and MUST agree — a field that read one way through List and
// another through Load would make the two calls into two answers about one
// item.
type ItemInfo struct {
	Ref   ItemRef
	Type  ItemType
	Title string
	Body  string
	URL   string

	Status      ItemStatus
	Disposition string

	// Availability is where the item sits on the listing ladder, FOR THE ASKING
	// BinaryName. It is the one field Item does not carry.
	Availability Availability

	Holder Holder
	Tags   []TagId

	// BlockedBy is every declared blocker with its own status; Blocked is the
	// answer to "is this blocked right now?", item-level and the same whoever
	// asks. BlockKind says who must act and on what. BlockReason is prose for a
	// person and nothing else: nothing parses it, branches on it, or infers a
	// state from it — not from its wording and not from whether it is empty.
	BlockedBy   []Blocker
	Blocked     bool
	BlockKind   BlockKind
	BlockReason string

	Manual bool
}

// ItemScope names how far up the listing ladder to go. The set is closed.
type ItemScope string

const (
	// ScopeAll: every item the orchestrator holds, open and closed.
	ScopeAll ItemScope = "all"
	// ScopeOpen: every open item.
	ScopeOpen ItemScope = "open"
	// ScopeProcessable: open items this binary could process (default).
	ScopeProcessable ItemScope = "processable"
	// ScopeWorkable: processable items not blocked — someone could work them.
	ScopeWorkable ItemScope = "workable"
	// ScopeFree: workable items this operator could claim now.
	ScopeFree ItemScope = "free"
	// ScopeAuto: free items that are opted in — an unattended `resolve` would pick one.
	ScopeAuto ItemScope = "auto"
)

// ValidScope reports whether s is one of the six recognized scope values.
func ValidScope(s ItemScope) bool {
	switch s {
	case ScopeAll, ScopeOpen, ScopeProcessable, ScopeWorkable, ScopeFree, ScopeAuto:
		return true
	}
	return false
}

// Availability is the per-item state, from a closed set. Each state is the
// boundary between two adjacent scope levels.
type Availability string

const (
	// AvailAuto: in scope level 6 — opted in for unattended selection.
	AvailAuto Availability = "auto"
	// AvailAvailable: in 5 (free) but not 6 (auto).
	AvailAvailable Availability = "available"
	// AvailHeld: in 4 (workable) but not 5 (free) — someone else holds it.
	AvailHeld Availability = "held"
	// AvailBlocked: in 3 (processable) but not 4 (workable).
	AvailBlocked Availability = "blocked"
	// AvailUnhandled: in 2 (open) but not 3 — no flow here accepts the type.
	AvailUnhandled Availability = "unhandled"
	// AvailClosed: in 1 (all) but not 2.
	AvailClosed Availability = "closed"
)

// InScope reports whether an item at this availability level is included in
// the given scope.
func (a Availability) InScope(s ItemScope) bool {
	return availLevel(a) >= scopeLevel(s)
}

func scopeLevel(s ItemScope) int {
	switch s {
	case ScopeAll:
		return 1
	case ScopeOpen:
		return 2
	case ScopeProcessable:
		return 3
	case ScopeWorkable:
		return 4
	case ScopeFree:
		return 5
	case ScopeAuto:
		return 6
	}
	return 0
}

func availLevel(a Availability) int {
	switch a {
	case AvailClosed:
		return 1
	case AvailUnhandled:
		return 2
	case AvailBlocked:
		return 3
	case AvailHeld:
		return 4
	case AvailAvailable:
		return 5
	case AvailAuto:
		return 6
	}
	return 0
}

// Claim is the credentialed handle returned by Orchestrator.Claim. It carries
// the ItemRef, the arena the lease binds to, the AccountId credited, and the
// orchestrator-internal token.
//
// IT IS NEVER A PARAMETER. Every method addresses the item by ItemRef; holding
// the lease is a precondition the orchestrator checks against the claim it
// currently has, not a value a caller supplies. A supplied claim is a value,
// and values go stale: the already-held override lets another arena take an
// item over, leaving the first holding a struct that still looks valid, so a
// write carrying its own proof would be writing under revoked authority.
//
// It carries the arena because a handle to a lease that could not name what the
// lease binds would leave every holder unidentifiable wherever one account runs
// more than one arena.
type Claim struct {
	OrchestratorName OrchestratorName `json:"orchestrator"`
	ItemRef          ItemRef          `json:"item"`
	Arena            Arena            `json:"arena"`
	Account          AccountId        `json:"account"`
	ClaimedAt        time.Time        `json:"claimed_at"`
	Token            json.RawMessage  `json:"token"`               // orchestrator-internal credential
	Overrides        []string         `json:"overrides,omitempty"` // opaque strings echoed from the lease response
}

// ClaimInfo is the read-only view returned by Orchestrator.LookupClaim. It is
// not a Claim: it carries no token and authorises nothing.
//
// It names the ARENA holding the item alongside the account credited, because
// the account alone does not answer the question: one person may hold this item
// in one arena and twenty others elsewhere, and "held by that person" tells an
// operator nothing about where the work is.
type ClaimInfo struct {
	Arena     Arena
	Account   AccountId
	ClaimedAt time.Time
}

// GateDef — one entry in an orchestrator's SupportedGates(). Required marks a
// gate that must exist for the contract to be satisfied; `integration` and
// `fit` are both required and must appear.
type GateDef struct {
	Name     GateName
	Required bool
}

// Gate returns a GateDef. Convenience constructor so call sites read as
// flow.Gate("tested", false).
func Gate(name GateName, required bool) GateDef { return GateDef{Name: name, Required: required} }

// CommandDef — one entry in an orchestrator's SupportedCommands(). Only the
// three CommandNames exist, and `verify` is required.
type CommandDef struct {
	Name CommandName
}

// Command returns a CommandDef. Convenience constructor.
func Command(name CommandName) CommandDef { return CommandDef{Name: name} }

// CommandRun is what Worktree.Run observed of one command process — the same
// shape as GateRun, and for the same reason: the Outcome separates "ran and
// reported" from "could not start", "timed out" or "died", so a caller can tell
// a failing check from a command that never executed.
//
// NO DECISION RESTS ON WHAT A COMMAND REPORTS. A command may modify the
// worktree, so a measurement taken by it is not reproducible by whoever asks
// next — the tree it measured no longer exists.
type CommandRun struct {
	// Command is the name that was asked for.
	Command CommandName

	// Outcome is what the runner observed. Always set when the runner returned
	// no error.
	Outcome Outcome

	// ExitCode is the command's own exit status, kept as a raw diagnostic. It
	// is -1 exactly where the kernel has no number to give.
	ExitCode int

	// Stdout is what the command printed on stdout.
	Stdout []byte

	// Detail is the runner's account for a person: which signal, which program
	// was absent. It is prose and nothing keys on it.
	Detail string
}

// Orchestrator is what the SDK talks to. It leases items to arenas, holds their
// state, runs gates and commands in a worktree, and lands what those produce.
// WHERE IT STORES ANY OF THAT IS ITS OWN BUSINESS — the GitHub orchestrator
// (pkg/orchestrator/github) keeps state on issues, the tracker orchestrator in
// its own service — and a store is something an orchestrator uses, not
// something it is.
//
// It may be LOCAL OR REMOTE. A remote one is a service that dispatches to many
// arenas; a local one is the binary orchestrating itself against a store it
// reaches directly, which is what the GitHub orchestrator is. Both satisfy this
// interface, and nothing above the boundary knows which it is talking to.
//
// THERE ARE NO OPTIONAL CAPABILITIES. Every method is required, and an
// orchestrator that cannot do one refuses it — ErrUnsupported for "never here",
// ErrUnavailable for "not right now". An interface an orchestrator may omit
// leaves a caller with nothing to call and no way to ask why; a method that
// refuses gives an answer.
//
// NO METHOD TAKES A Claim. See the Claim docstring.
type Orchestrator interface {
	// ---- Declaration ----

	// Name returns the orchestrator's name, carried on every ItemRef it mints.
	Name() OrchestratorName

	// SupportedSignals returns the set of SignalDefs this orchestrator knows
	// how to observe. cli.Run validates every signal reference against this
	// list at startup.
	SupportedSignals() []SignalDef

	// SupportedGates returns every gate that can be run here. `integration` and
	// `fit` are both required and MUST appear: nothing lands without
	// integration passing, and fit must run before a claim is taken.
	//
	// Listing them is also what makes integration's parts addressable — it is
	// assembled from smaller gates, and each is separately runnable only if a
	// caller can discover what they are.
	//
	// IT REPORTS WHAT CAN ACTUALLY RUN, NOT WHAT SHOULD BE THERE. An
	// orchestrator reads this off the machine — the github one asks its gate
	// entry point which gates it has — so a checkout that never built its gates
	// declares none, and every caller learns that at once instead of at the
	// first measurement. A hardcoded list is a claim about intentions: it says
	// `fit` on a machine with no gate entry point at all, and `doctor` then
	// reports a machine fit on the strength of a list it printed back to
	// itself.
	SupportedGates() []GateDef

	// SupportedCommands returns which of the three CommandNames this
	// orchestrator can run. `verify` is required — a step should not fail over
	// something verify would have fixed.
	//
	// Read off the machine for the same reason SupportedGates is: the github
	// orchestrator lists the command binaries the project actually has. A
	// declared command that is not there fails at the point of use, which is
	// after a step has done the work it was about to check.
	SupportedCommands() []CommandDef

	// SupportedArtifacts returns this orchestrator's canonical artifact schema:
	// a closed, curated set of ArtifactDefs it knows how to record. cli.Run
	// validates every declared App.Artifact against this set at startup — by id
	// AND type — so a flow that declares an artifact this orchestrator cannot
	// store is refused at startup rather than failing at resolve-time after the
	// producing step has already run and burned a turn.
	//
	// It is a closed list, not an open "supports anything" predicate: the (id,
	// type) pair is a stable schema multiple flows — even across projects — must
	// agree on.
	SupportedArtifacts() []ArtifactDef

	// ---- Discovery ----

	// ResolveRef turns a user-supplied string into an ItemRef. THE ONE PLACE A
	// VALUE ENTERS THIS CONTRACT BEFORE IT IS AN IDENTITY, and the only
	// supported way to reach an item by a name a person typed — the alternative
	// is matching on Display, which is a projection: it resolves by substring
	// and first-match, so it answers with AN item rather than THE item.
	ResolveRef(ctx context.Context, input string) (ItemRef, error)

	// List returns items at the given ItemScope, with per-item availability,
	// tags, holder and blockers. The BinaryName names the label that marks an
	// item opted in, which is what separates auto from available.
	//
	// The predicate is NOT A VALUE: it is the SDK lending the orchestrator its
	// own knowledge of which flows are registered, without which the
	// orchestrator could not tell unhandled from processable.
	//
	// Feeds `list`. THE AUTO-SELECT PATH MUST NEVER CALL IT — List reports
	// blocked items, and widening auto-select would let a bare `resolve` pick an
	// arbitrary open item and begin work on it.
	List(ctx context.Context, scope ItemScope, binary BinaryName, acceptsType func(ItemType) bool) ([]ItemInfo, error)

	// Get returns one of exactly what List returns, addressed by ref instead of
	// enumerated. IT MUST ANSWER IDENTICALLY TO List for the same item at the
	// same moment — one derivation serving both, never two. An item that reads
	// blocked in `list` and available in `status` is a contradiction an operator
	// cannot resolve, and nothing in the item caused it.
	Get(ctx context.Context, ref ItemRef, binary BinaryName, acceptsType func(ItemType) bool) (*ItemInfo, error)

	// ListAutoSelectable returns the items an unattended `resolve` may start
	// on, carrying every TagId given. An empty list means no filter.
	//
	// MUST NOT return a blocked item. The orchestrator that knows about the
	// dependency is the one that keeps it out of the selectable set; the SDK
	// does not filter afterwards, because a rule enforced in two places is a
	// rule with two owners and one of them wrong.
	//
	// Filtering belongs here because tags live in the orchestrator and ItemRef
	// does not carry them — a caller has nothing to filter on. An orchestrator
	// with no tag vocabulary returns nothing when tags are given, which is an
	// honest answer.
	ListAutoSelectable(ctx context.Context, tags []TagId) ([]ItemRef, error)

	// ---- Claiming ----

	// Claim acquires an exclusive lease binding item ↔ arena, one-to-one in
	// both directions.
	//
	// NEITHER THE ARENA NOR THE ACCOUNT IS A PARAMETER. Both are ambient, fixed
	// by where the call runs: the arena is the worktree the process sits in, and
	// the account is whoever that arena's credentials act as. An arena must
	// commit, push and merge, so it always has exactly one account, and the
	// orchestrator can read it — a caller-supplied one could only agree or be
	// wrong.
	Claim(ctx context.Context, ref ItemRef, overrides []ClaimOverride) (Claim, error)

	// Release relinquishes the lease.
	Release(ctx context.Context, ref ItemRef) error

	// LookupClaim reports who holds this item, or (nil, nil) if unclaimed.
	LookupClaim(ctx context.Context, ref ItemRef) (*ClaimInfo, error)

	// LookupActiveClaim returns the claim THIS ARENA holds right now, or (nil,
	// nil). Single source of truth for "what am I currently working on?".
	//
	// IT TAKES NO KEY: the arena is ambient, the same way it is for Claim, and
	// one arena holds at most one claim — so the question has exactly one
	// answer. Keying it by AccountId would not: a person running many arenas
	// holds many claims at once, and no single return value is the right one.
	LookupActiveClaim(ctx context.Context) (*Claim, error)

	// ---- State ----

	// Load returns the item and everything the flow has recorded on it —
	// artifacts, signals, questions and park — in one round. Signals are
	// refreshed by orchestrator-internal polling.
	//
	// ADDRESSED BY REF, NOT BY CLAIM: reading an item is not a privileged act,
	// and the item is what is being loaded. An orchestrator wanting the
	// shortcut a held claim affords looks up its own.
	Load(ctx context.Context, ref ItemRef) (*Item, error)

	// SeedState pre-loads the artifact set and budget caps. It MUST refuse a
	// second seed for the same item — mid-flight items are frozen against later
	// flow-source changes — and MUST refuse an unclaimed item, or one claimed by
	// another arena.
	SeedState(ctx context.Context, ref ItemRef, artifacts []ArtifactSpec) error

	// ResetSeed clears the existing seed so the next SeedState succeeds. This is
	// the ONLY escape hatch from SeedState's "frozen after first write"
	// contract. Operator-initiated only; the SDK never calls it automatically.
	//
	// An orchestrator with no separable seed concept refuses with ErrUnsupported.
	ResetSeed(ctx context.Context, ref ItemRef) error

	// ---- Editing ----

	// Edit opens an edit on the item. NOTHING CHANGES UNTIL Commit. Opening one
	// is not a lock: another writer may land first, and Commit is where that is
	// discovered.
	Edit(ctx context.Context, ref ItemRef) (ItemEditor, error)

	// ---- Artifacts ----

	// ResolveArtifact writes a handler-produced artifact value. There is no
	// orchestrator method for writing signals — signals are written by
	// orchestrator-internal side effects or the Load poll path.
	//
	// An EMPTY body is a legal call, not a client-side error: it is the
	// side-effect-artifact pattern, where the content was already attached
	// out-of-band and the handler is saying "I'm done — record me as resolved".
	// An orchestrator that stores such content elsewhere MUST decide emptiness
	// itself: verify the side effect happened and fail naming what is missing.
	ResolveArtifact(ctx context.Context, ref ItemRef, id ArtifactId, body ArtifactBody) error

	// MarkStale flips the stale bit on an artifact, causing its step to re-run.
	MarkStale(ctx context.Context, ref ItemRef, id ArtifactId) error

	// ---- Budget ----
	//
	// Every counter is keyed by the ArtifactId of the step's artifact: the
	// artifact a step produces is that step's identity, and the budget record
	// hangs off it. Signal steps produce no artifact, so they own no budget
	// record — they are never counted and never grantable. The counters are
	// transactional with the artifact record.

	BumpInvocations(ctx context.Context, ref ItemRef, id ArtifactId) error
	BumpPrompts(ctx context.Context, ref ItemRef, id ArtifactId) error
	AddCost(ctx context.Context, ref ItemRef, id ArtifactId, usd float64) error
	AddDuration(ctx context.Context, ref ItemRef, id ArtifactId, d time.Duration) error

	// Grant adds budget to the artifact record.
	//
	// It MUST clear a ParkBudgetExhausted park when the grant raises the parked
	// step's offending axis above its consumption — use GrantClearsPark so every
	// orchestrator applies the same rule. Parks of any other kind, and grants
	// too small to clear the cap, MUST be left in place: reporting an item as
	// unparked when the next dispatch would re-park it immediately is the
	// failure this contract exists to prevent.
	Grant(ctx context.Context, ref ItemRef, id ArtifactId, g Grant) error

	// ---- Parking and questions ----

	// Park records that a step stopped without completing, and why.
	Park(ctx context.Context, ref ItemRef, req ParkRequest) error

	// SaveWorkInProgress stores what a step worked out when it stopped without
	// completing, so the next attempt does not start from nothing.
	//
	// A record is keyed by the ITEM and by `step`, and a stored record naming a
	// different item or step is not this step's: LoadWorkInProgress returns ""
	// for it. Keying is the correctness property, not clearing — every path that
	// skips the cleanup would otherwise feed one item's reasoning to another
	// item's agent, arriving with origin OriginAgent and indistinguishable from
	// that agent's own thinking.
	//
	// NOTHING HERE IS EVER PUBLISHED. The record is the step's own scratch
	// state, and for a refused write the text to store IS the text a disclosure
	// guard refused.
	SaveWorkInProgress(ctx context.Context, ref ItemRef, step StepId, body string) error

	// LoadWorkInProgress returns what was stored, or empty. Absence is ("",
	// nil), not an error.
	LoadWorkInProgress(ctx context.Context, ref ItemRef, step StepId) (string, error)

	// ClearWorkInProgress discards it. Clearing what is not there is not an
	// error.
	ClearWorkInProgress(ctx context.Context, ref ItemRef, step StepId) error

	// PostAnswer records a person's answer AGAINST THE QUESTION IT ANSWERS. The
	// orchestrator stores the text on that Question, so the question stops being
	// pending — an answer that lands nowhere leaves the question still listed as
	// unanswered.
	//
	// THE OUTSTANDING-QUESTION MARKER CLEARS ONLY WHEN NO PENDING QUESTION
	// REMAINS: answering one of three is not answering the item, and clearing on
	// the first resumes a flow still waiting on two.
	//
	// No claim — the person answering does not hold the item.
	PostAnswer(ctx context.Context, ref ItemRef, id QuestionId, text string) error

	// AskQuestion records ONE agent-asked question on the item. The
	// orchestrator assigns it a QuestionId and persists the payload; THE RETURN
	// IS WHERE A QuestionId COMES FROM.
	//
	// EACH CALL ADDS ONE — THERE IS NO REPLACE. A step with several questions
	// calls this several times: nothing needs them recorded together, one call
	// per question tells the caller exactly which were recorded where a
	// partly-failed batch would not, and taking one question rather than a list
	// leaves no way to ask for none — a state a parked item cannot be answered
	// out of.
	AskQuestion(ctx context.Context, ref ItemRef, q AgentQuestion) (Question, error)

	// ---- Completion ----

	// Finalize marks the item's flow run complete and releases its claim.
	//
	// MUST REFUSE an item whose ItemStatus is not terminal. Finalizing does not
	// MAKE an item terminal; there is no method here that closes one. So
	// Finalize records that the flow is finished with an item already finished,
	// and refusing an open one is what keeps the two facts from drifting: a
	// finalized item still open claims the work is over while the orchestrator
	// says it is not.
	//
	// THIS IS THE ONLY WAY COMPLETION IS EVER RECORDED — nothing infers it.
	Finalize(ctx context.Context, ref ItemRef) error

	// ---- Worktree ----

	// Worktree returns the local-git surface for the item's arena.
	Worktree(ctx context.Context, ref ItemRef) (Worktree, error)
}

// ItemEditor is the transaction Edit opens. Fields are staged on it and land
// together, so a caller never has to ask which half of an edit succeeded —
// before Commit nothing has happened, and after it either everything has or
// nothing has.
//
// An orchestrator that cannot write some combination of these together REFUSES
// AT Commit rather than applying the part it can: refusing is an answer, a
// partial success is not.
type ItemEditor interface {
	// SetTitle replaces the title.
	SetTitle(title string)

	// SetBody replaces the body.
	SetBody(body string)

	// AddTag adds one tag. Adding one already present changes nothing.
	//
	// Tags are added and removed rather than assigned as a set, because an
	// item's tags have more than one author — the operator's classification, and
	// the markers an orchestrator maintains as consequences of contract
	// operations. Assigning the set entire would silently delete whichever half
	// the caller did not know about.
	AddTag(tag TagId)

	// RemoveTag removes one tag. Removing one absent changes nothing.
	//
	// AN ORCHESTRATOR MUST REFUSE TO REMOVE A MARKER IT MAINTAINS ITSELF: the
	// owner, binary and blocked markers follow from Claim, seeding and a blocker
	// declaration, and a caller able to delete one directly could make an item
	// report a state no operation put it in.
	RemoveTag(tag TagId)

	// AddBlocker records that this item waits on that one. Adding one already
	// recorded changes nothing.
	//
	// An orchestrator MUST refuse a blocker it cannot resolve — an identifier
	// naming nothing is a typo, and accepting it blocks the item forever on
	// something that does not exist — and MUST refuse a cycle, including an item
	// named as its own blocker. Both refusals land at Commit.
	AddBlocker(ref ItemRef)

	// RemoveBlocker retracts one dependency. Removing one not recorded changes
	// nothing.
	RemoveBlocker(ref ItemRef)

	// SetManual sets or clears manual control of the item.
	//
	// Setting it stops anything dispatching the item underneath the person now
	// driving it, and RESOLVES ANY UNRESOLVED PARK — the operator's `run-step`
	// IS the resume. Clearing it returns the item to automatic dispatch; an item
	// that could be taken over and never handed back would be stranded by the
	// act of helping it.
	SetManual(manual bool)

	// Commit applies every change made on this editor, OR NONE OF THEM.
	//
	// Committing publishes: the result is visible to everyone who can see the
	// item and is not undone by forgetting it happened, so it is an outward
	// write and subject to whatever guards those.
	Commit(ctx context.Context) error
}

// Worktree is the local-git surface handlers use via ctx.Worktree().
type Worktree interface {
	// Branch ensures `branch` is checked out, creating it off `base` (or HEAD
	// when base is empty). THE BOOL REPORTS WHETHER IT CREATED THE BRANCH OR
	// FOUND ONE ALREADY THERE, which is how a caller tells a fresh start from a
	// resumption. Idempotent. Errors on dirty tree.
	Branch(ctx context.Context, branch, base BranchName) (created bool, err error)

	// CurrentBranch returns the current branch name.
	CurrentBranch(ctx context.Context) (BranchName, error)

	// IsDirty reports whether tracked files have uncommitted changes (staged or
	// unstaged). Untracked files do NOT count as dirty.
	IsDirty(ctx context.Context) (bool, error)

	// Stage makes every change in the tree, untracked files included, visible
	// to the next CapturePatch — without committing it.
	//
	// The contract is that OUTCOME, not a particular mechanism. It exists
	// because a git-shaped CapturePatch diffs against HEAD, which cannot see
	// untracked files, while Commit stages everything: capturing before the
	// commit silently omits every file the change added, and capturing after it
	// sees a clean tree and returns nothing.
	//
	// An orchestrator whose CapturePatch already accounts for untracked content
	// legitimately implements this as a no-op. That is not a stub: the guarantee
	// callers depend on already holds.
	Stage(ctx context.Context) error

	// Commit commits the tree whole. Every file not ignored is staged.
	Commit(ctx context.Context, msg string) error

	// Push publishes the branch.
	//
	// It MAY WAIT. Landing is rebase → measure the merge result → push, and a
	// push that lands first invalidates every merge result measured against the
	// old mainline. With two workers landing at once and nothing serializing
	// them, each one's push sends the other back to rebase and measure again,
	// indefinitely — a livelock in which the work is sound, the gate passes
	// every time, and nothing ever lands.
	//
	// So an orchestrator serializes landing across everything sharing the
	// mainline. Waiting is not failing.
	Push(ctx context.Context) error

	// RevParse resolves a revision to a commit SHA.
	//
	// Every implementation MUST answer "HEAD" and the item's base branch.
	// Anything beyond those two is best-effort, and an orchestrator that cannot
	// resolve arbitrary revisions MUST return an error naming the limitation
	// rather than fall back to HEAD — a caller comparing a branch against its
	// base would otherwise be handed the same SHA twice and conclude the branch
	// is empty.
	//
	// The guaranteed pair is what tells a branch carrying work from an empty
	// one. Commit is a deliberate no-op when nothing is staged, so its nil
	// return is not evidence anything was recorded.
	RevParse(ctx context.Context, rev Revision) (CommitSha, error)

	// Run runs one of the three commands SupportedCommands() declares.
	//
	// It MAY MODIFY THE WORKTREE OR THE ARENA ENVIRONMENT, which is what makes
	// it a command, and why NO LANDING DECISION RESTS ON WHAT IT REPORTS. A
	// caller re-reads worktree state afterwards rather than assuming the tree is
	// unchanged.
	//
	// Like RunGate it returns a run rather than a bare error: the Outcome
	// separates "ran and reported" from "could not start", "timed out" or
	// "died", so a caller can tell a failing check from a command that never
	// executed — and the two have different budget consequences. A NON-NIL ERROR
	// MEANS NO COMMAND WAS RUN AND NO OUTCOME EXISTS.
	Run(ctx context.Context, name CommandName) (CommandRun, error)

	// RunGate runs the named GATE in the worktree and reports what the RUNNER
	// observed of the process.
	//
	// The SDK is the party that spawns gate processes, so the SDK is the runner.
	// A gate is never invoked directly and its own exit code is not consulted:
	// the states that matter most — killed for memory, exited 0 having printed
	// nothing — are the ones the gate is not alive to report, and only the
	// process that spawned it can tell them apart.
	//
	// The runner reports an OUTCOME, never a verdict. "measured" says a
	// measurement exists, not that it is acceptable; that needs the thresholds,
	// which are a separate artefact out of the subject's reach.
	//
	// A NON-NIL ERROR MEANS NO GATE WAS RUN AND NO OUTCOME EXISTS. It is
	// returned only for a request the runner could not attempt — an undeclared
	// gate name, or a caller whose context went away — and a caller must never
	// read it as a gate failure. Every way a gate can fail is an outcome:
	// OutcomeCouldNotStart in particular is an outcome, not an error, because a
	// caller that cannot tell it from OutcomeDied retries a missing binary
	// forever.
	//
	// A gate MODIFIES NOTHING IT MEASURES — tree or environment — which is what
	// makes its answer reproducible by whoever runs it. That is the entire
	// reason a decision may rest on a gate and not on the verify command. The
	// rule covers AFTER the measurement as well as during it: an implementation
	// that measures faithfully and then tidies the worktree is not a gate,
	// because a producing step asks a gate mid-work and cleaning up behind the
	// answer would discard the very work the step is in the middle of. A gate
	// that modifies lands on OutcomeBrokeContract.
	//
	// A gate MAY WAIT before it runs — some are too heavy to run beside another
	// — and waiting is not failing: a gate that queued and then ran is exactly as
	// authoritative as one that ran at once, while a gate that gave up waiting
	// has not measured anything.
	RunGate(ctx context.Context, name GateName) (GateRun, error)

	// Judge asks the PROJECT whether a measurement is acceptable.
	//
	// THE SDK NEVER COMPUTES THIS ITSELF. The thresholds are the project's, and
	// an SDK that computed a verdict would have to hold them for every project
	// it is ever pointed at.
	//
	// THE SDK KEEPS THE SPAWN. The judge is handed an envelope it did not
	// produce and could not have: a project entry point that ran the gate itself
	// and returned a verdict would be the runner, and the runner is the one
	// party that may not come from the tree — it is the sole witness to a
	// process that no longer exists, so a runner that lied is undetectable in
	// principle. The judge is checkable, which is what lets it be a tree
	// artifact.
	//
	// ONLY OutcomeMeasured MAY BE JUDGED, and a run carrying any other outcome
	// is refused with an error before anything is spawned: a gate that could not
	// start, timed out, died or broke its contract has not reported that the
	// tree is bad, and turning that into a refusal blames a change for a failure
	// that was never about it.
	//
	// A NON-NIL ERROR MEANS NO VERDICT EXISTS, AND IS NEVER A REFUSAL. A
	// refusal, by contrast, is a perfectly good verdict and arrives with a nil
	// error: Acceptable is false and Detail says which metric, its value, and
	// the term it was judged against.
	Judge(ctx context.Context, run GateRun) (GateVerdict, error)

	// CapturePatch produces a unified diff of the current working tree.
	//
	// Returning no bytes is legal and does not mean failure: an orchestrator
	// whose patches live server-side attaches the diff out-of-band and has
	// nothing client-side to hand back, and so does a tree whose work is already
	// committed.
	CapturePatch(ctx context.Context) (patch []byte, err error)

	// Request returns the RequestManager for pull-request operations, or nil
	// when this orchestrator does not land changes through pull requests.
	Request() RequestManager
}

// RequestManager is the pull-request surface exposed via Worktree.Request().
//
// It is ONE CAPABILITY, NOT SIX: proposing a change, finding it, measuring the
// merge result, and landing it are all the same statement — this orchestrator
// lands changes through pull requests. An orchestrator that does not returns nil
// from Request() and implements none of it.
type RequestManager interface {
	// Open opens a pull request and returns its URL. THIS IS THE ONLY PLACE A
	// PULL-REQUEST URL ORIGINATES — Merge and FindPR consume what this produced.
	// May trigger orchestrator signals (e.g. pr-open).
	Open(ctx context.Context, base BranchName, title, body string) (RequestUrl, error)

	// Merge merges the pull request named by that URL.
	Merge(ctx context.Context, url RequestUrl) error

	// FindPR returns the pull request for the current claim branch.
	FindPR(ctx context.Context) (PRInfo, error)

	// PrepareMergeResult sets the tree to reflect the merge result, so a gate
	// measures what will actually land rather than the branch in isolation.
	PrepareMergeResult(ctx context.Context, base BranchName) error

	// RevertMergePrep undoes that preparation.
	RevertMergePrep(ctx context.Context) error

	// RebuildTools rebuilds project tools so they match the current tree.
	// Needed after PrepareMergeResult changes it — compiled tools go stale when
	// a merge brings newer tool source from the base branch.
	RebuildTools(ctx context.Context) error
}

// PRInfo is the read-only snapshot FindPR returns.
type PRInfo struct {
	URL            RequestUrl
	MergeCommitSHA CommitSha // the merge commit; reliable only when the PR is merged
}

// Open is a nil-safe convenience: if wt.Request() returns nil (including a
// typed-nil interface value), returns ErrUnsupported instead of panicking on a
// nil-receiver call. Handlers that want a clean typed-error path use this;
// handlers that know their orchestrator lands through pull requests can call
// wt.Request().Open(...) directly.
func Open(ctx context.Context, wt Worktree, base BranchName, title, body string) (RequestUrl, error) {
	rq := wt.Request()
	if isNilRequest(rq) {
		return "", ErrUnsupported
	}
	return rq.Open(ctx, base, title, body)
}

// Merge is the nil-safe counterpart to Open. See Open for usage.
func Merge(ctx context.Context, wt Worktree, url RequestUrl) error {
	rq := wt.Request()
	if isNilRequest(rq) {
		return ErrUnsupported
	}
	return rq.Merge(ctx, url)
}

// FindPR is the nil-safe counterpart to Open, parallel to Merge.
func FindPR(ctx context.Context, wt Worktree) (PRInfo, error) {
	rq := wt.Request()
	if isNilRequest(rq) {
		return PRInfo{}, ErrUnsupported
	}
	return rq.FindPR(ctx)
}

// isNilRequest catches both untyped-nil interfaces and the typed-nil pitfall
// (a non-nil interface header pointing at a nil concrete pointer), which a
// plain `rq == nil` check misses and which would otherwise panic on the
// method call.
func isNilRequest(rq RequestManager) bool {
	if rq == nil {
		return true
	}
	v := reflect.ValueOf(rq)
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		return v.IsNil()
	}
	return false
}

// RequiredGates returns the gates every orchestrator must declare. `integration`
// is the composition nothing lands without; `fit` asks whether a machine may be
// given work at all, and must run before a claim is taken.
//
// Enumerated rather than restated at each check, for the same reason
// AllOutcomes is: a hand-written second copy is what goes stale when the set
// changes, and it goes stale silently.
func RequiredGates() []GateName { return []GateName{GateIntegration, GateFit} }

// RequiredCommands returns the commands every orchestrator must declare. Only
// `verify` is required; `setup` and `cleanup` are the orchestrator's choice.
func RequiredCommands() []CommandName { return []CommandName{CommandVerify} }

// HasGate reports whether defs declares name. Used by startup validation and by
// any caller deciding whether to ask for a gate at all.
func HasGate(defs []GateDef, name GateName) bool {
	return slices.ContainsFunc(defs, func(d GateDef) bool { return d.Name == name })
}

// HasCommand reports whether defs declares name.
func HasCommand(defs []CommandDef, name CommandName) bool {
	return slices.ContainsFunc(defs, func(d CommandDef) bool { return d.Name == name })
}

// Answer is one human reply to a question a flow asked.
//
// It lives in the flow package rather than in a consumer because orchestrators
// produce it and recipe libraries consume it: a type declared in either one
// would force the other to import it, and Go's method sets are nominal, so a
// look-alike struct would not satisfy the reader interface.
type Answer struct {
	QuestionID QuestionId
	Text       string // the question that was asked
	Answer     string
	Author     AccountId
	AnsweredAt time.Time
}

// RepoPermissions is what the authenticated principal may do on the item's
// repository. Orchestrators that have no permission model at all (a tracker
// where the runner holds full rights by construction) do not implement the probe
// that returns it; consumers requiring a role must be configured explicitly.
//
// The flags are cumulative as GitHub reports them — an admin also carries
// maintain/push/triage/pull — so decide a role by testing from the most
// privileged flag down, not by expecting exactly one to be set.
type RepoPermissions struct {
	Admin    bool
	Maintain bool
	Push     bool
	Triage   bool
	Pull     bool
}
