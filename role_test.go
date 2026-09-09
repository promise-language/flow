package flow

import (
	"slices"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Declaring roles on a flow.
// ---------------------------------------------------------------------------

func TestRole_DeclarationOrderIsKept(t *testing.T) {
	f := NewFlow("resolve", nil)
	f.Role("contributor", CapPush)
	f.Role("maintainer", CapPush, CapMerge)
	f.Role("reviewer", CapApprove)

	want := []RoleName{"contributor", "maintainer", "reviewer"}
	if got := f.RoleNames(); !slices.Equal(got, want) {
		t.Errorf("RoleNames() = %v, want %v in declaration order", got, want)
	}
	decls := f.Roles()
	if len(decls) != 3 {
		t.Fatalf("Roles() returned %d declarations, want 3", len(decls))
	}
	if decls[1].Name != "maintainer" || !slices.Equal(decls[1].Capabilities, []Capability{CapPush, CapMerge}) {
		t.Errorf("Roles()[1] = %+v, want the maintainer with push+merge", decls[1])
	}
}

// The declaration is copied in and cloned out, symmetrically. A caller holding
// either end must not be able to rewrite what startup validation certified.
func TestRole_DeclarationsAreCloned(t *testing.T) {
	caps := []Capability{CapPush, CapMerge}
	f := NewFlow("resolve", nil)
	f.Role("maintainer", caps...)

	caps[1] = CapApprove // the caller still holds the slice it passed
	if got := f.Roles()[0].Capabilities; !slices.Equal(got, []Capability{CapPush, CapMerge}) {
		t.Errorf("the declaration followed the caller's slice: %v", got)
	}

	out := f.Roles()
	out[0].Name = "rewritten"
	out[0].Capabilities[0] = CapApprove
	if got := f.Roles()[0]; got.Name != "maintainer" || got.Capabilities[0] != CapPush {
		t.Errorf("the declaration was rewritten through the view Roles() handed out: %+v", got)
	}
}

func TestRole_PanicsOnEmptyName(t *testing.T) {
	mustPanic(t, "empty role name", func() { NewFlow("resolve", nil).Role("", CapPush) })
}

func TestRole_PanicsOnDuplicateDeclaration(t *testing.T) {
	f := NewFlow("resolve", nil)
	f.Role("contributor", CapPush)
	mustPanic(t, "duplicate role", func() { f.Role("contributor", CapPush, CapMerge) })
}

func TestRole_PanicsOnInvalidCapability(t *testing.T) {
	f := NewFlow("resolve", nil)
	mustPanic(t, "not one of", func() { f.Role("contributor", CapPush, "deploy") })
}

// ---------------------------------------------------------------------------
// DeclaresRole answers from the declarations, not from the step tags.
// ---------------------------------------------------------------------------

// DeclaresRole is the one predicate for "does this flow declare that role" —
// the check every role reference is refused by when it names nothing.
func TestDeclaresRole(t *testing.T) {
	f := NewFlow("resolve", nil)
	f.Role("contributor", CapPush)
	f.Role("maintainer", CapPush, CapMerge)

	if !f.DeclaresRole("contributor") || !f.DeclaresRole("maintainer") {
		t.Error("a declared role must read as declared")
	}
	if f.DeclaresRole("reviewer") {
		t.Error("a role nothing declared must not read as declared")
	}
	// The empty name is never declared: a signal wait carries no role, so a
	// lookup on "" would otherwise match every flow with one.
	if f.DeclaresRole("") {
		t.Error("the empty role name must never be declared")
	}
}

// The regression that proves the re-backing. While DeclaresRole scanned the
// step tags, every typo was its own declaration — so the one check the
// declaration surface exists for could never fail, wherever it was written.
func TestDeclaresRole_ATagIsNotADeclaration(t *testing.T) {
	f := NewFlow("resolve", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{Role: "contributor"})

	if f.DeclaresRole("contributor") {
		t.Error("a role seen only as a step tag must not read as declared — " +
			"a tag scan makes every typo its own declaration")
	}
	if len(f.RoleNames()) != 0 {
		t.Errorf("RoleNames() = %v on a flow that declared none", f.RoleNames())
	}
}

func TestDeclaresRole_UndeclaredFlowDeclaresNone(t *testing.T) {
	f := NewFlow("resolve", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{})
	if f.DeclaresRole("contributor") {
		t.Error("a flow that declares no roles declares no roles")
	}
}

// ---------------------------------------------------------------------------
// The capability vocabulary.
// ---------------------------------------------------------------------------

func TestCapability_Valid(t *testing.T) {
	for _, c := range AllCapabilities() {
		if !c.Valid() {
			t.Errorf("%q is enumerated but not Valid()", c)
		}
	}
	for _, c := range []Capability{"", "deploy", "Push", "push "} {
		if c.Valid() {
			t.Errorf("%q reads as a capability", c)
		}
	}
}

// ---------------------------------------------------------------------------
// AssumableRoles — capability is the ceiling.
// ---------------------------------------------------------------------------

func TestAssumableRoles(t *testing.T) {
	decls := []RoleDecl{
		{Name: "contributor", Capabilities: []Capability{CapPush}},
		{Name: "maintainer", Capabilities: []Capability{CapPush, CapMerge}},
		{Name: "reviewer", Capabilities: []Capability{CapApprove}},
	}
	for _, tc := range []struct {
		name     string
		detected []Capability
		want     []RoleName
	}{
		{
			name:     "exact cover of one role",
			detected: []Capability{CapApprove},
			want:     []RoleName{"reviewer"},
		},
		{
			name:     "a superset covers every role it contains, in declaration order",
			detected: []Capability{CapApprove, CapMerge, CapPush},
			want:     []RoleName{"contributor", "maintainer", "reviewer"},
		},
		{
			name:     "one capability missing drops exactly the roles needing it",
			detected: []Capability{CapPush},
			want:     []RoleName{"contributor"},
		},
		{
			name:     "detecting nothing covers nothing",
			detected: nil,
			want:     nil,
		},
		{
			name:     "a capability no role asks for adds none",
			detected: []Capability{CapApprove, CapPush},
			want:     []RoleName{"contributor", "reviewer"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AssumableRoles(decls, tc.detected); !slices.Equal(got, tc.want) {
				t.Errorf("AssumableRoles(_, %v) = %v, want %v", tc.detected, got, tc.want)
			}
		})
	}
}

// A role requiring nothing is covered by every account, including one with
// nothing detected. That is the honest reading of "holds all of what it
// requires" — and it is exactly why ValidateGraph refuses such a declaration
// rather than leaving this to quietly call it unassumable.
func TestAssumableRoles_ZeroCapabilityRoleIsAssumableByEveryone(t *testing.T) {
	decls := []RoleDecl{{Name: "ghost"}}
	if got := AssumableRoles(decls, nil); !slices.Equal(got, []RoleName{"ghost"}) {
		t.Errorf("AssumableRoles(ghost, nothing) = %v, want the ghost covered", got)
	}

	// And the refusal that makes the reading safe.
	f := NewFlow("x", nil)
	f.Role("ghost")
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Role:        "ghost",
		Entry:       true,
		MayFinalize: []Disposition{DispositionResolved},
	})
	err := f.ValidateGraph()
	if err == nil || !strings.Contains(err.Error(), "names no capability") {
		t.Errorf("ValidateGraph() = %v, want the capability-less role refused", err)
	}
}

func TestAssumableRoles_NoDeclarations(t *testing.T) {
	if got := AssumableRoles(nil, AllCapabilities()); got != nil {
		t.Errorf("AssumableRoles(nothing declared, everything detected) = %v, want nil", got)
	}
}
