package flow

import "testing"

// TestAvailability_InScope verifies the ladder: each availability level is
// included in its own scope and all wider scopes, but excluded from narrower.
func TestAvailability_InScope(t *testing.T) {
	levels := []struct {
		avail Availability
		scope DiscoveryScope
	}{
		{AvailClosed, ScopeAll},
		{AvailUnhandled, ScopeOpen},
		{AvailBlocked, ScopeProcessable},
		{AvailHeld, ScopeWorkable},
		{AvailAvailable, ScopeFree},
		{AvailAuto, ScopeAuto},
	}

	for i, l := range levels {
		// Must be IN its own scope.
		if !l.avail.InScope(l.scope) {
			t.Errorf("%s should be in scope %s", l.avail, l.scope)
		}
		// Must be IN all wider scopes (lower indices).
		for _, wider := range levels[:i] {
			if !l.avail.InScope(wider.scope) {
				t.Errorf("%s should be in wider scope %s", l.avail, wider.scope)
			}
		}
		// Must be OUT of all narrower scopes (higher indices).
		for _, narrower := range levels[i+1:] {
			if l.avail.InScope(narrower.scope) {
				t.Errorf("%s should NOT be in narrower scope %s", l.avail, narrower.scope)
			}
		}
	}
}

// TestValidScope covers the closed set.
func TestValidScope(t *testing.T) {
	valid := []DiscoveryScope{ScopeAll, ScopeOpen, ScopeProcessable, ScopeWorkable, ScopeFree, ScopeAuto}
	for _, s := range valid {
		if !ValidScope(s) {
			t.Errorf("ValidScope(%q) = false, want true", s)
		}
	}
	invalid := []DiscoveryScope{"galaxy", "", "ALL"}
	for _, s := range invalid {
		if ValidScope(s) {
			t.Errorf("ValidScope(%q) = true, want false", s)
		}
	}
}
