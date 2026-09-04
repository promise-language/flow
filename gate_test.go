package flow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// The five outcome strings are a WIRE CONTRACT shared with base, not flow's own
// spelling. A silent change to one of them is a break that only shows up in
// another repository, so they are pinned here as literals rather than derived
// from the constants they name.
//
// Pinning the set as well as the values: a sixth outcome is not an addition, it
// is a change to a vocabulary something else reads.
func TestOutcomes_AreTheDeclaredWireSpelling(t *testing.T) {
	for _, c := range []struct {
		outcome Outcome
		wire    string
	}{
		{OutcomeMeasured, "measured"},
		{OutcomeTimedOut, "timed_out"},
		{OutcomeCouldNotStart, "could_not_start"},
		{OutcomeDied, "died"},
		{OutcomeBrokeContract, "broke_contract"},
	} {
		if string(c.outcome) != c.wire {
			t.Errorf("outcome = %q, want %q", c.outcome, c.wire)
		}
	}
}

// The zero GateRun says nothing was observed. Callers get one back beside an
// error — the request the runner could not attempt — and must not be able to
// read a measurement out of it.
func TestGateRun_ZeroValueIsNotAnOutcome(t *testing.T) {
	var run GateRun
	if run.Outcome != "" {
		t.Errorf("zero GateRun.Outcome = %q, want the empty outcome", run.Outcome)
	}
	if run.Outcome == OutcomeMeasured {
		t.Error("a zero GateRun reads as a measurement")
	}
}

// The zero GateVerdict says nothing was judged, and above all it does not say
// a measured run was refused. Callers get one back beside an error — the
// request no judge could answer — and a caller that read `Acceptable == false`
// out of it would refuse a sound change because the project's judging layer
// could not be reached.
func TestGateVerdict_ZeroValueIsNotAVerdictAboutAMeasuredRun(t *testing.T) {
	var v GateVerdict
	if v.Acceptable {
		t.Error("a zero GateVerdict reads as acceptable")
	}
	// The refusal it appears to be is unbacked: there is no run behind it, no
	// terms it was reached against, and nothing a person could re-check.
	if v.Run.Outcome == OutcomeMeasured {
		t.Error("a zero GateVerdict carries a measured run")
	}
	if v.Thresholds != nil {
		t.Errorf("Thresholds = %q, want none — nothing was compared", v.Thresholds)
	}
	if v.Detail != "" {
		t.Errorf("Detail = %q, want none", v.Detail)
	}
}

// The set is CLOSED and it is not flow's to extend — base reads the same five
// names. The test above pins the five VALUES; it does not notice a sixth
// constant, which would compile, satisfy every switch in this repository, and
// reach base as a name it has never heard of. So the declared constants are
// parsed out of the source and matched against the register above.
//
// A count would not do: it passes the moment someone adds one and removes
// another. Adding a name here is meant to be the deliberate act of changing a
// vocabulary another repository reads, not a side effect of adding a constant.
func TestOutcomes_TheSetIsClosed(t *testing.T) {
	register := map[string]bool{
		"OutcomeMeasured":      true,
		"OutcomeTimedOut":      true,
		"OutcomeCouldNotStart": true,
		"OutcomeDied":          true,
		"OutcomeBrokeContract": true,
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "gate.go", nil, 0)
	if err != nil {
		t.Fatalf("parse gate.go: %v", err)
	}

	declared := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		id, ok := spec.Type.(*ast.Ident)
		if !ok || id.Name != "Outcome" {
			return true
		}
		for _, name := range spec.Names {
			declared[name.Name] = true
		}
		return true
	})
	if len(declared) == 0 {
		t.Fatal("parsed no Outcome constants — the probe is broken, not the code")
	}

	for name := range declared {
		if !register[name] {
			t.Errorf("%s is an Outcome the wire contract does not name — "+
				"base reads this vocabulary and has never heard of it", name)
		}
	}
	for name := range register {
		if !declared[name] {
			t.Errorf("%s is named by the wire contract and is no longer declared — "+
				"whatever base sends carrying it now has no meaning here", name)
		}
	}
}

// A truncated envelope is not an envelope, so both OutcomeDied and
// OutcomeBrokeContract can be read as covering it — and a caller who reads only
// the second sends a dead host to the gate's author. The runner classifies it as
// died; what this pins is that BOTH comments say so, because a caller reads
// whichever it reaches first and there is no third place to look.
//
// It asserts the pairing, not the prose: each of the two names the truncated
// case and names the other constant, so neither reads as its sole home. Anyone
// is free to rewrite either comment; what fails here is deleting the handoff
// from one of them and leaving the rule in the other.
func TestOutcomes_TruncationIsAttributedFromBothEnds(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "gate.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse gate.go: %v", err)
	}

	docs := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		id, ok := spec.Type.(*ast.Ident)
		if !ok || id.Name != "Outcome" {
			return true
		}
		for _, name := range spec.Names {
			docs[name.Name] = spec.Doc.Text()
		}
		return true
	})
	if len(docs) == 0 {
		t.Fatal("parsed no Outcome constants — the probe is broken, not the code")
	}

	for _, c := range []struct{ self, other string }{
		{"OutcomeDied", "OutcomeBrokeContract"},
		{"OutcomeBrokeContract", "OutcomeDied"},
	} {
		doc, ok := docs[c.self]
		if !ok {
			t.Fatalf("%s is not declared — the probe is broken, not the code", c.self)
		}
		if doc == "" {
			t.Fatalf("%s has no doc comment; the two are the only place the rule lives", c.self)
		}
		if !strings.Contains(doc, "truncated") {
			t.Errorf("%s does not say where a truncated envelope lands, so a caller "+
				"reading it first cannot tell", c.self)
		}
		if !strings.Contains(doc, c.other) {
			t.Errorf("%s does not name %s, so it reads as the sole home of the "+
				"truncated case", c.self, c.other)
		}
	}
}

// The enumerator and the constants are one vocabulary, and a hand-written list
// is what goes stale when a member is added — silently, because a stale list
// still compiles and still passes every test that uses it. This parses the
// declarations and compares, so adding a sixth outcome without adding it to
// AllOutcomes fails here rather than somewhere downstream months later.
func TestAllOutcomesMatchesTheDeclaredConstants(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "gate.go", nil, 0)
	if err != nil {
		t.Fatalf("parse gate.go: %v", err)
	}
	var declared []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 {
			return true
		}
		id, ok := vs.Type.(*ast.Ident)
		if !ok || id.Name != "Outcome" {
			return true
		}
		declared = append(declared, vs.Names[0].Name)
		return true
	})
	if len(declared) == 0 {
		t.Fatal("found no Outcome constants; the parse is wrong, not the code")
	}
	if len(declared) != len(AllOutcomes()) {
		t.Errorf("gate.go declares %d outcomes (%v) but AllOutcomes returns %d",
			len(declared), declared, len(AllOutcomes()))
	}
	for _, o := range AllOutcomes() {
		if !o.Valid() {
			t.Errorf("AllOutcomes returned %q, which Valid rejects", o)
		}
	}
	if Outcome("").Valid() {
		t.Error("the empty outcome is valid; a run the runner never classified would pass")
	}
}
