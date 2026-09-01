package flow_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"

	"github.com/promise-language/flow"
)

// constNamesOfType parses wire.go and returns the names of all package-level
// constants declared with the given type name, in source order.
func constNamesOfType(t *testing.T, typeName string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "wire.go", nil, 0)
	if err != nil {
		t.Fatalf("parse wire.go: %v", err)
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
	declared := constNamesOfType(t, "ParkKind")
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
	declared := constNamesOfType(t, "InvocationStatus")
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
