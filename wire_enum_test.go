package flow_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"

	"github.com/promise-language/flow"
)

// constNamesOfType parses `file` and returns the names of all package-level
// constants declared with the given type name, in source order.
//
// The file is a parameter because every closed vocabulary in this package needs
// the same check and they are not all in one file — the wire enums are in
// wire.go, the selection axes in orchestrator.go. One walker, because a second
// copy would drift from this one and each would guard only what it was written
// for.
func constNamesOfType(t *testing.T, file, typeName string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var names []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// A const spec carries a type when it is explicitly typed
			// (e.g. `ParkBlocked ParkKind = "blocked"`). Iota-style groups
			// carry it on the first spec only; subsequent specs inherit.
			// Both shapes resolve to an *ast.Ident whose Name is the type.
			if vs.Type == nil {
				continue
			}
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != typeName {
				continue
			}
			for _, n := range vs.Names {
				names = append(names, n.Name)
			}
		}
	}
	return names
}

func TestAllParkKinds_ExhaustiveAgainstAST(t *testing.T) {
	declared := constNamesOfType(t, "wire.go", "ParkKind")
	if len(declared) == 0 {
		t.Fatal("found no ParkKind constants in wire.go")
	}
	got := flow.AllParkKinds()
	if len(got) != len(declared) {
		t.Fatalf("AllParkKinds() returns %d members, but wire.go declares %d ParkKind constants: %v", len(got), len(declared), declared)
	}
	// Build a set from the returned values to check completeness.
	seen := map[flow.ParkKind]bool{}
	for _, pk := range got {
		seen[pk] = true
	}
	// Every declared constant must map to a string value that appears in the
	// returned slice. We cannot recover the string from the AST (the value is
	// a string literal, not a name), so we check the count matches and that
	// there are no duplicates.
	if len(seen) != len(got) {
		t.Errorf("AllParkKinds() contains duplicates: %v", got)
	}
}

func TestAllInvocationStatuses_ExhaustiveAgainstAST(t *testing.T) {
	declared := constNamesOfType(t, "wire.go", "InvocationStatus")
	if len(declared) == 0 {
		t.Fatal("found no InvocationStatus constants in wire.go")
	}
	got := flow.AllInvocationStatuses()
	if len(got) != len(declared) {
		t.Fatalf("AllInvocationStatuses() returns %d members, but wire.go declares %d InvocationStatus constants: %v", len(got), len(declared), declared)
	}
	seen := map[flow.InvocationStatus]bool{}
	for _, s := range got {
		seen[s] = true
	}
	if len(seen) != len(got) {
		t.Errorf("AllInvocationStatuses() contains duplicates: %v", got)
	}
	// Verify by value: each declared constant name maps to a known value.
	// Build expected from the constant names → values:
	nameToValue := map[string]flow.InvocationStatus{
		"StatusDone":    flow.StatusDone,
		"StatusSkipped": flow.StatusSkipped,
		"StatusFailed":  flow.StatusFailed,
		"StatusParked":  flow.StatusParked,
		"StatusBlocked": flow.StatusBlocked,
	}
	for _, name := range declared {
		val, ok := nameToValue[name]
		if !ok {
			t.Errorf("InvocationStatus constant %q declared in wire.go but not in nameToValue map — update the test", name)
			continue
		}
		if !slices.Contains(got, val) {
			t.Errorf("AllInvocationStatuses() is missing %q (from constant %s)", val, name)
		}
	}
}

// The two selection axes, checked the same way — they are in orchestrator.go
// rather than wire.go, which is the only difference.
//
// For these the check is load-bearing rather than hygienic: THE ENUMERATOR
// DOUBLES AS THE RANK. A priority's position in AllPriorities is its rank, so a
// member declared without joining the list would not fail to compile — it would
// read as the neutral value and sort where an unassessed item sorts, which is
// the one failure a closed vocabulary exists to prevent.
func TestAllPrioritiesAndUrgencies_ExhaustiveAgainstAST(t *testing.T) {
	priorities := constNamesOfType(t, "orchestrator.go", "Priority")
	urgencies := constNamesOfType(t, "orchestrator.go", "Urgency")
	if len(priorities) == 0 || len(urgencies) == 0 {
		t.Fatal("found no Priority/Urgency constants in orchestrator.go; the parse is wrong, not the code")
	}
	if got := flow.AllPriorities(); len(got) != len(priorities) {
		t.Errorf("AllPriorities() returns %d members, but orchestrator.go declares %d Priority constants: %v",
			len(got), len(priorities), priorities)
	}
	if got := flow.AllUrgencies(); len(got) != len(urgencies) {
		t.Errorf("AllUrgencies() returns %d members, but orchestrator.go declares %d Urgency constants: %v",
			len(got), len(urgencies), urgencies)
	}
	// And neither list repeats a member. A duplicate keeps the count right
	// while pushing another member out, where its rank is -1 and it sorts ahead
	// of everything — the same silent misordering the count guards against.
	seenP := map[flow.Priority]bool{}
	for _, p := range flow.AllPriorities() {
		seenP[p] = true
	}
	if len(seenP) != len(flow.AllPriorities()) {
		t.Errorf("AllPriorities() contains duplicates: %v", flow.AllPriorities())
	}
	seenU := map[flow.Urgency]bool{}
	for _, u := range flow.AllUrgencies() {
		seenU[u] = true
	}
	if len(seenU) != len(flow.AllUrgencies()) {
		t.Errorf("AllUrgencies() contains duplicates: %v", flow.AllUrgencies())
	}
}
