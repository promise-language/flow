package github

import "strings"

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
)

// labels collects the prefixed label names a backend instance uses. Built
// from cfg.LabelPrefix at NewBackend time.
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

// OwnerFromLabel returns the gh login if `name` has the flow:owner: prefix.
func (l labels) OwnerFromLabel(name string) (login string, ok bool) {
	want := l.OwnerPrefix()
	if !strings.HasPrefix(name, want) {
		return "", false
	}
	return name[len(want):], true
}

// ClaimTokenFromLabel returns the random hex if `name` has the flow:claim: prefix.
func (l labels) ClaimTokenFromLabel(name string) (hex string, ok bool) {
	want := l.ClaimPrefix()
	if !strings.HasPrefix(name, want) {
		return "", false
	}
	return name[len(want):], true
}
