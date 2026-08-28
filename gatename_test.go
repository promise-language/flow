package flow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestGateName_ConceptAndInstance(t *testing.T) {
	for _, c := range []struct {
		name     GateName
		concept  GateName
		instance string
		valid    bool
	}{
		{"tested", "tested", "", true},
		{"tested:wasm", "tested", "wasm", true},
		{"integration", "integration", "", true},
		// The instance is the project's and is never validated: only the
		// project knows how its work divides.
		{"tested:anything-at-all", "tested", "anything-at-all", true},
		// A colon is not special to the instance, only to the first split.
		{"tested:wasm:web", "tested", "wasm:web", true},

		// An undeclared concept is refused however it is dressed.
		{"lint", "lint", "", false},
		{"lint:go", "lint", "go", false},
		{"", "", "", false},
		{":wasm", "", "wasm", false},

		// A colon promising an instance and naming none is a typo. Treating it
		// as the bare concept would run every suite for someone who meant one.
		{"tested:", "tested", "", false},
	} {
		t.Run(string(c.name), func(t *testing.T) {
			if got := c.name.Concept(); got != c.concept {
				t.Errorf("Concept() = %q, want %q", got, c.concept)
			}
			if got := c.name.Instance(); got != c.instance {
				t.Errorf("Instance() = %q, want %q", got, c.instance)
			}
			if got := c.name.Valid(); got != c.valid {
				t.Errorf("Valid() = %v, want %v", got, c.valid)
			}
		})
	}
}

// AllGateConcepts is hand-maintained, and every consumer that enumerates it
// trusts it to be the whole set. A constant added without being listed would
// be invisible to all of them — the drift this parses the source to prevent.
//
// Asserting a count would not do: it passes the moment someone adds one
// constant and removes another.
func TestAllGateConcepts_MatchesTheDeclaredConstants(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "artifact.go", nil, 0)
	if err != nil {
		t.Fatalf("parse artifact.go: %v", err)
	}

	declared := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		id, ok := spec.Type.(*ast.Ident)
		if !ok || id.Name != "GateName" {
			return true
		}
		for _, v := range spec.Values {
			lit, ok := v.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			declared[strings.Trim(lit.Value, `"`)] = true
		}
		return true
	})
	if len(declared) == 0 {
		t.Fatal("parsed no GateName constants — the probe is broken, not the code")
	}

	listed := map[string]bool{}
	for _, n := range AllGateConcepts() {
		listed[string(n)] = true
	}
	for name := range declared {
		if !listed[name] {
			t.Errorf("GateName %q is declared but missing from AllGateConcepts — "+
				"every consumer that enumerates the set would not see it", name)
		}
	}
	for name := range listed {
		if !declared[name] {
			t.Errorf("AllGateConcepts lists %q, which is not a declared GateName constant", name)
		}
	}
}

// The returned slice is fresh each call. A package-level var would be mutable
// by any importer, and a consumer that reordered or truncated it would change
// the set for everyone else in the process.
func TestAllGateConcepts_CallerCannotMutateTheSet(t *testing.T) {
	got := AllGateConcepts()
	got[0] = "clobbered"
	if AllGateConcepts()[0] == "clobbered" {
		t.Error("mutating the returned slice changed the set for the next caller")
	}
}
