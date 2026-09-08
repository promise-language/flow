package github

import (
	"strings"

	"github.com/promise-language/flow"
)

// label vocabulary — keep all string constants in one place so other
// modules don't sprinkle "flow:" literals around.
const (
	labelSuffixSeeded         = "seeded"
	labelSuffixOwnerPrefix    = "owner:"
	labelSuffixBlocked        = "blocked"
	labelSuffixNeedsAnswer    = "needs-answer"
	labelSuffixDisabled       = "disabled"
	labelSuffixInfraTransient = "infra-transient"
	labelSuffixStalePrefix    = "stale:"
	labelSuffixClaimPrefix    = "claim:"
	labelSuffixBudgetExhPref  = "budget-exhausted:"
	labelSuffixTypePrefix     = "type:"
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
func (l labels) Seeded() string         { return l.named(labelSuffixSeeded) }
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

// Artifact lifecycle labels.
func (l labels) StaleArtifact(id string) string {
	return l.named(labelSuffixStalePrefix + id)
}
func (l labels) BudgetExhausted(id string) string {
	return l.named(labelSuffixBudgetExhPref + id)
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

// Maintained reports whether `name` is a marker this orchestrator maintains
// itself as a consequence of a contract operation — the owner and claim
// markers from Claim, the seeded and binary markers from seeding, the park
// markers from Park, the manual marker from the editor, and the two selection
// axes, which the typed setters own the way SetManual owns flow:manual.
//
// ItemEditor.RemoveTag refuses these: a caller able to delete one directly
// could make an item report a state no operation put it in.
func (l labels) Maintained(name string) bool {
	if !strings.HasPrefix(name, l.prefix) {
		return false
	}
	rest := strings.TrimPrefix(name, l.prefix)
	switch {
	case rest == labelSuffixSeeded,
		rest == labelSuffixBlocked,
		rest == labelSuffixNeedsAnswer,
		rest == labelSuffixInfraTransient,
		rest == labelSuffixManual,
		strings.HasPrefix(rest, labelSuffixOwnerPrefix),
		strings.HasPrefix(rest, labelSuffixClaimPrefix),
		strings.HasPrefix(rest, labelSuffixStalePrefix),
		strings.HasPrefix(rest, labelSuffixBudgetExhPref),
		strings.HasPrefix(rest, labelSuffixPriorityPrefix),
		strings.HasPrefix(rest, labelSuffixUrgencyPrefix):
		return true
	}
	return false
}

// ClaimTokenFromLabel returns the random hex if `name` has the flow:claim: prefix.
func (l labels) ClaimTokenFromLabel(name string) (hex string, ok bool) {
	want := l.ClaimPrefix()
	if !strings.HasPrefix(name, want) {
		return "", false
	}
	return name[len(want):], true
}
