package flow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"
	"time"
)

// The enumerator DOUBLES AS THE RANK — priorityRank and urgencyRank are a
// position in AllPriorities / AllUrgencies — so a member declared without
// joining the list would not fail to compile. It would silently read as the
// neutral value and sort where an unassessed item sorts, which is the one
// failure a closed vocabulary exists to prevent. Hence a parse of the source.
func TestAllPrioritiesAndUrgenciesMatchTheDeclaredConstants(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "orchestrator.go", nil, 0)
	if err != nil {
		t.Fatalf("parse orchestrator.go: %v", err)
	}
	declared := map[string][]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 {
			return true
		}
		id, ok := vs.Type.(*ast.Ident)
		if !ok {
			return true
		}
		if id.Name == "Priority" || id.Name == "Urgency" {
			declared[id.Name] = append(declared[id.Name], vs.Names[0].Name)
		}
		return true
	})

	if len(declared["Priority"]) == 0 || len(declared["Urgency"]) == 0 {
		t.Fatal("found no Priority/Urgency constants; the parse is wrong, not the code")
	}
	if got, want := len(declared["Priority"]), len(AllPriorities()); got != want {
		t.Errorf("orchestrator.go declares %d priorities (%v) but AllPriorities returns %d",
			got, declared["Priority"], want)
	}
	if got, want := len(declared["Urgency"]), len(AllUrgencies()); got != want {
		t.Errorf("orchestrator.go declares %d urgencies (%v) but AllUrgencies returns %d",
			got, declared["Urgency"], want)
	}
}

// OrNeutral is "what an item has when nothing has said otherwise". Anything not
// naming a member reads as the neutral one — a misspelling included, so a
// stored value naming nothing sorts exactly where an unassessed item does
// rather than somewhere of its own.
func TestOrNeutral_ReadsAnythingUnrecognizedAsTheNeutralValue(t *testing.T) {
	for _, p := range []Priority{"", "hihg", "HIGH", "medium", "urgent"} {
		if got := p.OrNeutral(); got != PriorityMedium {
			t.Errorf("Priority(%q).OrNeutral() = %q, want %q", string(p), got, PriorityMedium)
		}
	}
	for _, u := range []Urgency{"", "soon", "NEXT", "default"} {
		if got := u.OrNeutral(); got != UrgencyDefault {
			t.Errorf("Urgency(%q).OrNeutral() = %q, want %q", string(u), got, UrgencyDefault)
		}
	}
	// A member reads as itself, neutral or not.
	for _, p := range AllPriorities() {
		if got := p.OrNeutral(); got != p {
			t.Errorf("Priority(%q).OrNeutral() = %q, want itself", p, got)
		}
	}
	for _, u := range AllUrgencies() {
		if got := u.OrNeutral(); got != u {
			t.Errorf("Urgency(%q).OrNeutral() = %q, want itself", u, got)
		}
	}
}

var (
	older = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newer = time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
)

// Urgency sorts first, and that is the entire content of "an operator overrides
// a machine": a `next` item at `low` priority starts before a `default` item at
// `critical`.
func TestCompareSelection_UrgencyOutranksPriority(t *testing.T) {
	instruction := SelectionKey{Urgency: UrgencyNext, Priority: PriorityLow, Age: newer}
	assessment := SelectionKey{Urgency: UrgencyDefault, Priority: PriorityCritical, Age: older}
	if CompareSelection(instruction, assessment) >= 0 {
		t.Error("a next+low item did not sort before a default+critical one")
	}
	if CompareSelection(assessment, instruction) <= 0 {
		t.Error("the comparison is not antisymmetric across the two")
	}
}

// Within one urgency the four priorities take their declared order, highest
// first.
func TestCompareSelection_PriorityOrderIsTheDeclaredOrder(t *testing.T) {
	keys := []SelectionKey{
		{Urgency: UrgencyDefault, Priority: PriorityLow, Age: older},
		{Urgency: UrgencyDefault, Priority: PriorityMedium, Age: older},
		{Urgency: UrgencyDefault, Priority: PriorityCritical, Age: older},
		{Urgency: UrgencyDefault, Priority: PriorityHigh, Age: older},
	}
	slices.SortFunc(keys, CompareSelection)
	want := []Priority{PriorityCritical, PriorityHigh, PriorityMedium, PriorityLow}
	for i, k := range keys {
		if k.Priority != want[i] {
			t.Errorf("position %d = %q, want %q (got %v)", i, k.Priority, want[i], keys)
		}
	}
}

// Age sorts last and oldest first. It is what makes this an order at all: two
// callers reading the same set at the same moment must start on the same item.
func TestCompareSelection_AgeBreaksTheRemainingTie(t *testing.T) {
	old := SelectionKey{Urgency: UrgencyDefault, Priority: PriorityHigh, Age: older}
	recent := SelectionKey{Urgency: UrgencyDefault, Priority: PriorityHigh, Age: newer}
	if CompareSelection(old, recent) >= 0 {
		t.Error("the older item did not sort first")
	}
	// But only after priority: a newer critical still beats an older high.
	crit := SelectionKey{Urgency: UrgencyDefault, Priority: PriorityCritical, Age: newer}
	if CompareSelection(crit, old) >= 0 {
		t.Error("age outranked priority")
	}
	// Identical keys compare equal — the orchestrator's own tiebreak decides,
	// because ItemRef is orchestrator-specific and there is nothing here to
	// break the tie on.
	if got := CompareSelection(old, old); got != 0 {
		t.Errorf("two identical keys compared %d, want 0", got)
	}
}

// The zero Priority is not a fifth level below `low`: silence is not an
// assessment, so an item nobody ranked sorts exactly where `medium` does.
func TestCompareSelection_TheZeroValuesSortWhereTheNeutralOnesDo(t *testing.T) {
	unset := SelectionKey{Age: older}
	neutral := SelectionKey{Urgency: UrgencyDefault, Priority: PriorityMedium, Age: older}
	if got := CompareSelection(unset, neutral); got != 0 {
		t.Errorf("the zero key compared %d against medium/default, want 0", got)
	}
	high := SelectionKey{Urgency: UrgencyDefault, Priority: PriorityHigh, Age: older}
	low := SelectionKey{Urgency: UrgencyDefault, Priority: PriorityLow, Age: older}
	if CompareSelection(unset, low) >= 0 {
		t.Error("an unranked item sorted below one deliberately marked low")
	}
	if CompareSelection(unset, high) <= 0 {
		t.Error("an unranked item sorted above one marked high")
	}
	// And a value naming nothing sorts as the neutral one, not last.
	misspelled := SelectionKey{Urgency: "soon", Priority: "hihg", Age: older}
	if got := CompareSelection(misspelled, neutral); got != 0 {
		t.Errorf("a key naming no member compared %d against medium/default, want 0", got)
	}
}

// Deferred sorts last on its axis. Nothing selects one — ListAutoSelectable
// must leave it out of the set entirely — but the rank exists because the
// enumerator is the rank, and a member outside it would read as `default`.
func TestCompareSelection_DeferredSortsLastOnItsAxis(t *testing.T) {
	deferred := SelectionKey{Urgency: UrgencyDeferred, Priority: PriorityCritical, Age: older}
	ordinary := SelectionKey{Urgency: UrgencyDefault, Priority: PriorityLow, Age: newer}
	if CompareSelection(deferred, ordinary) <= 0 {
		t.Error("a deferred item did not sort after an ordinary one")
	}
}
