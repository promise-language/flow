package github

import "testing"

func TestLabels_Vocabulary(t *testing.T) {
	l := newLabels("flow:")
	cases := []struct {
		name, got, want string
	}{
		{"Seeded", l.Seeded(), "flow:seeded"},
		{"Blocked", l.Blocked(), "flow:blocked"},
		{"NeedsAnswer", l.NeedsAnswer(), "flow:needs-answer"},
		{"Disabled", l.Disabled(), "flow:disabled"},
		{"Owner", l.Owner("alice"), "flow:owner:alice"},
		{"Binary", l.Binary("implement"), "flow:implement"},
		{"ClaimToken", l.ClaimToken("deadbeef"), "flow:claim:deadbeef"},
		{"StaleArtifact", l.StaleArtifact("plan"), "flow:stale:plan"},
		{"BudgetExhausted", l.BudgetExhausted("plan"), "flow:budget-exhausted:plan"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestLabels_NormalizesMissingColon(t *testing.T) {
	l := newLabels("custom") // no trailing colon
	if got := l.Seeded(); got != "custom:seeded" {
		t.Errorf("Seeded = %q, want custom:seeded (colon added)", got)
	}
}

func TestLabels_OwnerFromLabel(t *testing.T) {
	l := newLabels("flow:")
	if login, ok := l.OwnerFromLabel("flow:owner:bob"); !ok || login != "bob" {
		t.Errorf("OwnerFromLabel = (%q, %v), want (bob, true)", login, ok)
	}
	if _, ok := l.OwnerFromLabel("flow:seeded"); ok {
		t.Errorf("OwnerFromLabel should reject non-owner labels")
	}
}

func TestLabels_ClaimTokenFromLabel(t *testing.T) {
	l := newLabels("flow:")
	if h, ok := l.ClaimTokenFromLabel("flow:claim:abcdef"); !ok || h != "abcdef" {
		t.Errorf("ClaimTokenFromLabel = (%q, %v), want (abcdef, true)", h, ok)
	}
	if _, ok := l.ClaimTokenFromLabel("flow:seeded"); ok {
		t.Errorf("ClaimTokenFromLabel should reject non-claim labels")
	}
}
