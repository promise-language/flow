package github

import (
	"strings"

	"github.com/promise-language/flow"
)

// label vocabulary — keep all string constants in one place so other
// modules don't sprinkle "flow:" literals around.
const (
	// labelSuffixAwaitsPrefix is whose move it is: the role the journal's last
	// entry elected, or `awaits:signal:<id>` for a wait that is nobody's. It is a
	// cheap index over the journal, maintained at every append — the journal is
	// still the one source, and the label is what a listing can read without
	// fetching a state comment per item.
	labelSuffixAwaitsPrefix = "awaits:"
	// labelSuffixRequiresPrefix is a placement restriction: only arenas whose
	// `<axis>` fact is `<value>` qualify (docs/environment.md § Placement).
	//
	// NOTHING WRITES IT HERE, and the row exists anyway. The behaviour — the
	// axes, the arena facts, the selection filter and the claim refusal — is
	// #251's. What this reserves is the SPELLING, because otherBinaryLabel reads
	// by exclusion: without the row, `flow:requires:os:windows` reads as a binary
	// named `requires:os:windows` and every claim on such an item is refused.
	labelSuffixRequiresPrefix = "requires:"
	labelSuffixOwnerPrefix    = "owner:"
	labelSuffixBlocked        = "blocked"
	labelSuffixNeedsAnswer    = "needs-answer"
	labelSuffixDisabled       = "disabled"
	labelSuffixInfraTransient = "infra-transient"
	labelSuffixClaimPrefix    = "claim:"
	// labelSuffixArenaPrefix marks WHICH ARENA holds the claim, as an opaque
	// digest of its (HostId, ArenaId). It is the other half of
	// flow:owner:<login>: the owner label records the account credited, and on
	// the ordinary single-operator fleet — one login, several worktrees — every
	// arena writes the same one, so it cannot separate them. The lease binds
	// item ↔ arena, so the arena is what the exclusion has to compare.
	//
	// A DIGEST and not the pair, because docs/disclosure.md closes the
	// categories of thing a flow must not publish and two of them are exactly
	// what an arena is made of — local filesystem paths and host identifiers.
	// Equality is the only operation the exclusion needs, and a digest supports
	// exactly that and nothing else.
	labelSuffixArenaPrefix      = "arena:"
	labelSuffixTreasurerRefPref = "treasurer-refused:"
	labelSuffixTypePrefix       = "type:"
	// The two selection axes. Their NEUTRAL VALUES ARE UNSPELLABLE: `medium`
	// and `default` have no label, so no state is reachable both by a label and
	// by that label's absence — an item demoted from `high` to `medium` and an
	// item nobody ever assessed are the same item to selection, and must read
	// as the same item.
	labelSuffixPriorityPrefix = "priority:"
	labelSuffixUrgencyPrefix  = "urgency:"
	// labelSuffixManual marks an item an operator has taken hand control of.
	// Item.Manual has to live somewhere on the issue for Load to report it
	// truthfully, and docs/github-schema.md's label set is closed — so it is a
	// label, declared here with the rest.
	labelSuffixManual = "manual"
)

// structuralLabel is one row of the structural vocabulary: a suffix under the
// label prefix that is THIS ORCHESTRATOR'S OWN MARKER rather than a binary's
// name, and what the two readers of that distinction may do with it.
type structuralLabel struct {
	// suffix is a labelSuffix* constant in structuralLabels — never a literal,
	// so a live row cannot spell a suffix the vocabulary above does not declare.
	// The retiredSuffixes rows below are the one exception, and spell literals
	// precisely because they are no longer vocabulary.
	suffix string
	// valued marks a suffix that carries a value after it (owner:<login>,
	// type:<type>) and so matches a label by prefix; an unvalued suffix is the
	// whole label and matches exactly.
	valued bool
	// maintained marks a label a contract operation writes, and so one
	// ItemEditor.RemoveTag refuses to delete: a caller able to remove it
	// directly could make an item report a state no operation put it in.
	maintained bool
}

// matches reports whether `rest` — a label with the prefix already stripped —
// falls under this row. THE one matching rule, so the live vocabulary and the
// retired spellings are recognised by the same test.
func (s structuralLabel) matches(rest string) bool {
	if s.valued {
		return strings.HasPrefix(rest, s.suffix)
	}
	return rest == s.suffix
}

// structuralLabels is THE vocabulary of structural suffixes. Both readers of
// the distinction derive from it — otherBinaryLabel (claim.go), which reads
// every label under the prefix it does not find here as another binary's name,
// and Maintained below — so a suffix cannot be known to one and not the other.
// Two hand-kept switches drifted three times (#210, #217, #256), and each
// omission was a standing other-binary refusal on every item carrying the
// label, because otherBinaryLabel reads by exclusion. The rule, enforced by
// TestLabels_EveryDeclaredSuffixHasAVocabularyRow: EVERY labelSuffix* constant
// has exactly one row here, and every row names a constant.
//
// The three rows one switch had and the other did not, decided:
//
//   - manual: structural, maintained (SetManual owns it), and it REFUSES NO
//     CLAIM. The lease is not dispatch. No command asserts manual control
//     (docs/cli.md § Advancing one step), so the flag arrives through the
//     editor while the person driving the item holds the lease — and their
//     later `claim` or `resolve` is the holder's idempotent re-claim
//     (docs/resolution.md § Claiming), which the other-binary preflight sits
//     above: the driver's own next claim was the one refused. Another arena is
//     kept off by the already-held refusal while the driver holds the lease; a
//     manual item nobody holds is claimable, and what keeps it from being
//     DISPATCHED is the manual hold at dispatch (docs/resolution.md § skipped;
//     #170). Neither docs/github-schema.md § Claim protocol nor
//     docs/orchestrator.md § What an orchestrator may refuse lists manual among
//     the refusals.
//   - disabled: structural, NOT maintained. No operation writes it — it is
//     the operator's stop switch — and RemoveTag is the contract's only route
//     to "re-enable it", which is waits-on-person's own wording. Its absence
//     from Maintained was the intent, now stated.
//   - type:: structural, NOT maintained. No operation writes it, and a
//     flow:type:bug label must never read as a binary named "type:bug". The
//     schema spells the label flow:type: and this code reads a bare type:
//     prefix; that mismatch is #266 and does not change the row.
//
// Recognising a binary label POSITIVELY — it equals a known binary's name —
// would remove the deny-by-default that makes each omission a hard block, but
// nothing declares the fleet's names: cfg.BinaryName is this binary only, and
// the schema spells the label flow:<binary-name> with no distinguishing shape.
// Until a registry exists, this table plus the completeness test is the
// containment.
var structuralLabels = []structuralLabel{
	{suffix: labelSuffixAwaitsPrefix, valued: true, maintained: true},
	{suffix: labelSuffixRequiresPrefix, valued: true, maintained: true},
	{suffix: labelSuffixOwnerPrefix, valued: true, maintained: true},
	{suffix: labelSuffixBlocked, maintained: true},
	{suffix: labelSuffixNeedsAnswer, maintained: true},
	{suffix: labelSuffixDisabled},
	{suffix: labelSuffixInfraTransient, maintained: true},
	{suffix: labelSuffixClaimPrefix, valued: true, maintained: true},
	{suffix: labelSuffixArenaPrefix, valued: true, maintained: true},
	{suffix: labelSuffixTreasurerRefPref, valued: true, maintained: true},
	{suffix: labelSuffixTypePrefix, valued: true},
	{suffix: labelSuffixPriorityPrefix, valued: true, maintained: true},
	{suffix: labelSuffixUrgencyPrefix, valued: true, maintained: true},
	{suffix: labelSuffixManual, maintained: true},
}

// retiredSuffixes are spellings this vocabulary USED to write and no longer
// does: `flow:seeded` (superseded by the binary label as the "begun" marker),
// `flow:stale:<id>` (the checklist's stale bit, which schema v2 drops), and
// `flow:budget-exhausted:<id>` (renamed `flow:treasurer-refused:<id>`).
//
// THEY CANNOT SIMPLY VANISH. Live issues carry them right now, and
// otherBinaryLabel reads by exclusion — a label under the prefix that nothing
// recognises is another binary's name. Dropping these rows would turn every item
// still carrying one into a standing, permanent claim refusal naming a binary
// nobody wrote, which is exactly the failure the completeness rule above records
// happening three times (#210, #217, #256).
//
// Recognised as STRUCTURAL and NOT MAINTAINED: never read as a binary, never
// written, and ItemEditor.RemoveTag will clear one by hand.
//
// Plain literals rather than labelSuffix* constants, deliberately: they are no
// longer vocabulary, and a constant for each would put them back in the table
// the completeness test enforces.
var retiredSuffixes = []structuralLabel{
	{suffix: "seeded"},
	{suffix: "stale:", valued: true},
	{suffix: "budget-exhausted:", valued: true},
}

// labels collects the prefixed label names an orchestrator instance uses.
// Built from cfg.LabelPrefix at New time.
type labels struct {
	prefix string
}

func newLabels(prefix string) labels {
	if prefix == "" {
		prefix = "flow:"
	}
	if !strings.HasSuffix(prefix, ":") {
		prefix += ":"
	}
	return labels{prefix: prefix}
}

// Generic builder.
func (l labels) named(suffix string) string { return l.prefix + suffix }

// Static labels.
func (l labels) Blocked() string        { return l.named(labelSuffixBlocked) }
func (l labels) NeedsAnswer() string    { return l.named(labelSuffixNeedsAnswer) }
func (l labels) Disabled() string       { return l.named(labelSuffixDisabled) }
func (l labels) InfraTransient() string { return l.named(labelSuffixInfraTransient) }
func (l labels) Manual() string         { return l.named(labelSuffixManual) }

// Per-binary owner labels.
func (l labels) Binary(name string) string { return l.named(name) }
func (l labels) Owner(login string) string { return l.named(labelSuffixOwnerPrefix + login) }
func (l labels) OwnerPrefix() string       { return l.prefix + labelSuffixOwnerPrefix }
func (l labels) ClaimToken(hex string) string {
	return l.named(labelSuffixClaimPrefix + hex)
}
func (l labels) ClaimPrefix() string { return l.prefix + labelSuffixClaimPrefix }

// Per-arena claim labels. The value is a fingerprint, never the pair itself —
// see labelSuffixArenaPrefix and fingerprintArena.
func (l labels) Arena(fingerprint string) string {
	return l.named(labelSuffixArenaPrefix + fingerprint)
}
func (l labels) ArenaPrefix() string { return l.prefix + labelSuffixArenaPrefix }

// Awaited-marker labels. `value` is what awaitsString renders — a role name, or
// `signal:<id>` — so `flow:awaits:signal:<id>` falls out of the wire's own
// spelling rather than being composed a second time here.
func (l labels) Awaits(value string) string { return l.named(labelSuffixAwaitsPrefix + value) }
func (l labels) AwaitsPrefix() string       { return l.prefix + labelSuffixAwaitsPrefix }

// Placement labels. The formatter exists so #251's writer has one spelling to
// use; nothing in this package writes one yet.
func (l labels) RequiresPrefix() string { return l.prefix + labelSuffixRequiresPrefix }

// Step lifecycle labels.
func (l labels) TreasurerRefused(id string) string {
	return l.named(labelSuffixTreasurerRefPref + id)
}

// Type-derivation labels.
func (l labels) TypePrefix() string { return l.prefix + labelSuffixTypePrefix }

// Selection-axis labels. Priority and Urgency are THE formatters — the writer
// spells a label through them and the reader matches what they produce, so the
// schema has one spelling and not two that have to agree.
//
// A neutral value formats as the EMPTY STRING rather than a label, because it
// has none: SetPriority(medium) and SetUrgency(default) remove the axis's label
// instead of writing one.
func (l labels) Priority(p flow.Priority) string {
	if p.OrNeutral() == flow.PriorityMedium {
		return ""
	}
	return l.named(labelSuffixPriorityPrefix + string(p))
}

func (l labels) Urgency(u flow.Urgency) string {
	if u.OrNeutral() == flow.UrgencyDefault {
		return ""
	}
	return l.named(labelSuffixUrgencyPrefix + string(u))
}

func (l labels) PriorityPrefix() string { return l.prefix + labelSuffixPriorityPrefix }
func (l labels) UrgencyPrefix() string  { return l.prefix + labelSuffixUrgencyPrefix }

// PriorityOf and UrgencyOf are THE readers of the two axes — Get, List, Load
// and selection all go through them, so no two reads of one item can disagree.
//
// A suffix naming no member — `flow:priority:hihg` — is no label at all, and so
// is one spelling a neutral value: both fall to the neutral, which is the one
// state an unlabelled item is in.
func (l labels) PriorityOf(names []string) flow.Priority {
	want := l.PriorityPrefix()
	for _, n := range names {
		if !strings.HasPrefix(n, want) {
			continue
		}
		if p := flow.Priority(n[len(want):]); l.Priority(p) == n {
			return p
		}
	}
	return flow.Priority("").OrNeutral()
}

func (l labels) UrgencyOf(names []string) flow.Urgency {
	want := l.UrgencyPrefix()
	for _, n := range names {
		if !strings.HasPrefix(n, want) {
			continue
		}
		if u := flow.Urgency(n[len(want):]); l.Urgency(u) == n {
			return u
		}
	}
	return flow.Urgency("").OrNeutral()
}

// OwnerFromLabel returns the account if `name` has the flow:owner: prefix.
func (l labels) OwnerFromLabel(name string) (account flow.AccountId, ok bool) {
	want := l.OwnerPrefix()
	if !strings.HasPrefix(name, want) {
		return "", false
	}
	return flow.AccountId(name[len(want):]), true
}

// structural reports the structuralLabels row `name` falls under, if any: the
// prefix stripped, then an exact match for an unvalued suffix or a prefix match
// for a valued one. THE one reader of the table — otherBinaryLabel and
// Maintained both go through here, so the two cannot disagree about what is
// structural.
func (l labels) structural(name string) (structuralLabel, bool) {
	rest, ok := strings.CutPrefix(name, l.prefix)
	if !ok {
		return structuralLabel{}, false
	}
	for _, s := range structuralLabels {
		if s.matches(rest) {
			return s, true
		}
	}
	// A spelling this vocabulary has retired is still structural — it is this
	// orchestrator's own marker, just an old one — and never maintained. See
	// retiredSuffixes for why recognising them is required rather than tidy.
	for _, s := range retiredSuffixes {
		if s.matches(rest) {
			return s, true
		}
	}
	return structuralLabel{}, false
}

// Maintained reports whether `name` is a marker this orchestrator maintains
// itself as a consequence of a contract operation — the owner, arena and claim
// markers from Claim, the binary marker from the first journal entry, the
// awaited marker from every append, the park markers from Park, the manual
// marker from the editor, and the two selection axes, which the typed setters
// own the way SetManual owns flow:manual. Which rows those are is
// structuralLabels' maintained bit, decided beside the vocabulary rather than
// restated here.
//
// A retired spelling is NOT maintained: nothing writes it, so RemoveTag is the
// only way one leaves an item that still carries it.
//
// ItemEditor.RemoveTag refuses these: a caller able to delete one directly
// could make an item report a state no operation put it in.
func (l labels) Maintained(name string) bool {
	s, ok := l.structural(name)
	return ok && s.maintained
}

// ClaimTokenFromLabel returns the random hex if `name` has the flow:claim: prefix.
func (l labels) ClaimTokenFromLabel(name string) (hex string, ok bool) {
	want := l.ClaimPrefix()
	if !strings.HasPrefix(name, want) {
		return "", false
	}
	return name[len(want):], true
}

// ArenaFromLabel returns the arena fingerprint if `name` has the flow:arena:
// prefix. The value is opaque: it compares for equality against this arena's
// own fingerprint and cannot be turned back into a (HostId, ArenaId).
func (l labels) ArenaFromLabel(name string) (fingerprint string, ok bool) {
	want := l.ArenaPrefix()
	if !strings.HasPrefix(name, want) {
		return "", false
	}
	return name[len(want):], true
}
