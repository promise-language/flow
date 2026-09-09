package flow

import "testing"

// ladder is the seven-rung listing ladder, in order: each Availability is
// exactly the boundary between two adjacent scopes (docs/cli.md § Availability).
// The table is written once and read by every test below, because the point of
// the ladder is that the two vocabularies are ONE list read from its two ends —
// a rung stated twice could disagree with itself.
var ladder = []struct {
	avail Availability
	scope ItemScope
}{
	{AvailClosed, ScopeAll},
	{AvailOutsideRemit, ScopeOpen},
	{AvailAwaits, ScopeProcessable},
	{AvailBlocked, ScopeActionable},
	{AvailHeld, ScopeWorkable},
	{AvailAvailable, ScopeFree},
	{AvailAuto, ScopeAuto},
}

// TestAvailability_InScope verifies the ladder across ALL 7×7 pairs: each
// availability level is included in its own scope and every wider one, and
// excluded from every narrower one. Exhaustive rather than sampled — a
// misplaced rung shows up as a wrong answer on exactly one pair.
func TestAvailability_InScope(t *testing.T) {
	if len(ladder) != 7 {
		t.Fatalf("ladder has %d rungs, want 7", len(ladder))
	}
	for i, l := range ladder {
		for j, other := range ladder {
			want := j <= i // wider scopes (lower index) include this level
			if got := l.avail.InScope(other.scope); got != want {
				t.Errorf("%s.InScope(%s) = %v, want %v", l.avail, other.scope, got, want)
			}
		}
	}
}

// Each Availability names exactly one boundary: no two share a rung, and every
// declared value appears. A duplicated rung would sort two states identically
// while the vocabulary claims they differ.
func TestAvailability_EachNamesOneRung(t *testing.T) {
	seenAvail := map[Availability]bool{}
	seenScope := map[ItemScope]bool{}
	for _, l := range ladder {
		if seenAvail[l.avail] {
			t.Errorf("availability %s appears twice in the ladder", l.avail)
		}
		if seenScope[l.scope] {
			t.Errorf("scope %s appears twice in the ladder", l.scope)
		}
		seenAvail[l.avail] = true
		seenScope[l.scope] = true
	}

	// The zero value is on neither ladder: an unset availability is not
	// "everything", and an unrecognized scope is not "all".
	if Availability("").InScope(ScopeAll) {
		t.Errorf(`Availability("").InScope(ScopeAll) = true, want false`)
	}
	if AvailAuto.InScope(ItemScope("galaxy")) != true {
		// scopeLevel("galaxy") == 0, so every level is "in" it. Documented
		// here so the asymmetry is deliberate rather than discovered: callers
		// validate the scope with ValidScope before asking.
		t.Errorf("unknown scope should not be narrower than auto")
	}
}

// TestValidScope covers the closed set — all seven, and nothing else.
func TestValidScope(t *testing.T) {
	for _, l := range ladder {
		if !ValidScope(l.scope) {
			t.Errorf("ValidScope(%q) = false, want true", l.scope)
		}
	}
	invalid := []ItemScope{"galaxy", "", "ALL", "unhandled", "remit"}
	for _, s := range invalid {
		if ValidScope(s) {
			t.Errorf("ValidScope(%q) = true, want false", s)
		}
	}
}
