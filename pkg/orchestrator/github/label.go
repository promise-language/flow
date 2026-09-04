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
// markers from Park, the manual marker from the editor.
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
		strings.HasPrefix(rest, labelSuffixBudgetExhPref):
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
