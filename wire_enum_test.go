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

// The AST crosswalk above proves the SET is complete; it cannot recover the
// string VALUES, which are what a stored park and a `--json` payload carry. So
// the wire spelling of every kind is written down once, here.
//
// The two renames this vocabulary took are the reason it is worth pinning:
// `budget-exhausted` became `treasurer-refused` — the treasurer refused to fund
// the dispatch, which is a decision, not a clock reading — and
// `step-did-not-resolve` became `step-did-not-complete`, since a step completes
// by electing a route and "resolve" named the artifact it no longer must
// produce.
func TestParkKind_WireSpellings(t *testing.T) {
	want := map[flow.ParkKind]string{
		flow.ParkBlocked:            "blocked",
		flow.ParkQuestion:           "question",
		flow.ParkTreasurerRefused:   "treasurer-refused",
		flow.ParkStepDidNotComplete: "step-did-not-complete",
		flow.ParkInfraTransient:     "infra-transient",
		flow.ParkRemoteUnreachable:  "remote-unreachable",
		flow.ParkRefused:            "refused",
		flow.ParkWriteContract:      "write-contract",
	}
	got := flow.AllParkKinds()
	if len(got) != len(want) {
		t.Fatalf("AllParkKinds() has %d members, but %d spellings are written down: %v", len(got), len(want), got)
	}
	for _, k := range got {
		spelling, ok := want[k]
		if !ok {
			t.Errorf("park kind %q has no spelling written down here", k)
			continue
		}
		if string(k) != spelling {
			t.Errorf("park kind spelled %q, want %q", string(k), spelling)
		}
	}
	// The old spellings are gone, not aliased: a reader that still writes them
	// is writing a value nothing in the vocabulary answers to.
	for _, gone := range []string{"budget-exhausted", "step-did-not-resolve"} {
		for _, k := range got {
			if string(k) == gone {
				t.Errorf("the retired spelling %q is still a declared park kind", gone)
			}
		}
	}
}

// RedispatchMayClear is the retry classification the vocabulary carries: whether
// dispatching a parked item again could possibly do anything. It is written
// down once, here, per kind, so a kind that joins the vocabulary without a
// classification fails this test rather than silently reading as "no" through
// the fail-closed default.
func TestParkKind_RedispatchMayClear_ClassifiesEveryKind(t *testing.T) {
	want := map[flow.ParkKind]bool{
		flow.ParkBlocked:            false, // a person decides on the refusal
		flow.ParkQuestion:           false, // an answer must arrive
		flow.ParkTreasurerRefused:   false, // only a grant clears it
		flow.ParkStepDidNotComplete: true,  // a re-dispatch does the job it left
		flow.ParkInfraTransient:     true,  // against a healthy runner
		flow.ParkRemoteUnreachable:  true,  // once the remote returns
		flow.ParkRefused:            false, // deterministic
		flow.ParkWriteContract:      false, // same prompt, same result
	}
	got := flow.AllParkKinds()
	if len(got) != len(want) {
		t.Fatalf("AllParkKinds() has %d members, but %d classifications are written down: %v", len(got), len(want), got)
	}
	for _, k := range got {
		mayClear, ok := want[k]
		if !ok {
			t.Errorf("park kind %q has no classification written down here", k)
			continue
		}
		if k.RedispatchMayClear() != mayClear {
			t.Errorf("%q.RedispatchMayClear() = %v, want %v", k, k.RedispatchMayClear(), mayClear)
		}
	}
}

// The one member where "can re-dispatch help" and "who must act" disagree. A
// step that did not complete waits on nobody and clears on nothing — it is not
// a `waits-on-condition` park that heals itself while a scheduler backs off —
// yet an active re-dispatch is exactly its cure. This is why the classification
// is its own table over ParkKind and not a reading of BlockKind: deriving one
// from the other would define this member's answer away.
func TestParkKind_RedispatchMayClear_StepDidNotCompleteIsRetryable(t *testing.T) {
	if !flow.ParkStepDidNotComplete.RedispatchMayClear() {
		t.Error("step-did-not-complete must classify as clearable by re-dispatch: a re-dispatch is what does the job the step left undone")
	}
}

// A kind this binary does not know — a park written by a newer one — fails
// closed. A scheduler reading false stops; one reading true would loop on a
// park it cannot reason about.
func TestParkKind_RedispatchMayClear_UnknownKindFailsClosed(t *testing.T) {
	for _, k := range []flow.ParkKind{"not-a-kind", ""} {
		if k.RedispatchMayClear() {
			t.Errorf("ParkKind(%q).RedispatchMayClear() = true, want false: an unrecognised kind must stop a scheduler, not loop it", k)
		}
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

// The vocabularies this file guards for the step-model amendment. Same AST
// check as the ones above, and load-bearing for the same reason the selection
// axes are: a member declared without joining its enumerator would not fail to
// compile. It would simply never be Valid(), and every declaration naming it
// would panic as though it were a typo — for the first four because
// StepConfig.normalized maps the empty string onto the loosest member, so the
// hole is invisible until something spells the member out.
func TestStepModelEnums_ExhaustiveAgainstAST(t *testing.T) {
	cases := []struct {
		file    string
		typ     string
		members []string // the enumerator's values, as strings
	}{
		{"wire.go", "Disposition", stringsOf(flow.AllDispositions())},
		{"step.go", "CaptureSource", stringsOf(flow.AllCaptureSources())},
		{"step.go", "NeedsState", stringsOf(flow.AllNeedsStates())},
		{"step.go", "LeavesState", stringsOf(flow.AllLeavesStates())},
		// Capability has no defined zero, but the enumerator is still the only
		// thing standing between a declared member and a role that can never be
		// assumed: Flow.Role validates a declaration against AllCapabilities,
		// so a constant left out of it is refused at the registration that
		// names it.
		{"role.go", "Capability", stringsOf(flow.AllCapabilities())},
	}
	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			declared := constNamesOfType(t, tc.file, tc.typ)
			if len(declared) == 0 {
				t.Fatalf("found no %s constants in %s; the parse is wrong, not the code", tc.typ, tc.file)
			}
			if len(tc.members) != len(declared) {
				t.Fatalf("All%ss() returns %d members, but %s declares %d %s constants: %v",
					tc.typ, len(tc.members), tc.file, len(declared), tc.typ, declared)
			}
			// A duplicate keeps the count right while pushing another member
			// out — the same silent hole the count is meant to catch.
			seen := map[string]bool{}
			for _, m := range tc.members {
				if seen[m] {
					t.Errorf("All%ss() contains %q twice: %v", tc.typ, m, tc.members)
				}
				seen[m] = true
			}
		})
	}
}

// stringsOf renders any enumerator of a string-kinded vocabulary as plain
// strings, so one table can hold four of them.
func stringsOf[T ~string](members []T) []string {
	out := make([]string, len(members))
	for i, m := range members {
		out[i] = string(m)
	}
	return out
}
