package github

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

func TestLabels_Vocabulary(t *testing.T) {
	l := newLabels("flow:")
	cases := []struct {
		name, got, want string
	}{
		{"Blocked", l.Blocked(), "flow:blocked"},
		{"NeedsAnswer", l.NeedsAnswer(), "flow:needs-answer"},
		{"Disabled", l.Disabled(), "flow:disabled"},
		{"Owner", l.Owner("alice"), "flow:owner:alice"},
		{"Binary", l.Binary("implement"), "flow:implement"},
		{"ClaimToken", l.ClaimToken("deadbeef"), "flow:claim:deadbeef"},
		{"TreasurerRefused", l.TreasurerRefused("plan"), "flow:treasurer-refused:plan"},
		{"Awaits role", l.Awaits("contributor"), "flow:awaits:contributor"},
		// The signal spelling FALLS OUT of awaitsString's prefix rather than
		// being composed a second time here.
		{"Awaits signal", l.Awaits(awaitsString(flow.Awaits{Signal: "pr-merged"})), "flow:awaits:signal:pr-merged"},
		{"AwaitsPrefix", l.AwaitsPrefix(), "flow:awaits:"},
		{"RequiresPrefix", l.RequiresPrefix(), "flow:requires:"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestLabels_NormalizesMissingColon(t *testing.T) {
	l := newLabels("custom") // no trailing colon
	if got := l.Blocked(); got != "custom:blocked" {
		t.Errorf("Blocked = %q, want custom:blocked (colon added)", got)
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

// labelSuffixConsts parses label.go and returns every package-level constant
// named labelSuffix*, name → value. The constants are untyped string literals,
// so there is no type to select on the way constNamesOfType (wire_enum_test.go,
// package flow) does; the NAME is the declaration, and the value is read off
// the literal.
func labelSuffixConsts(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "label.go", nil, 0)
	if err != nil {
		t.Fatalf("parse label.go: %v", err)
	}
	consts := map[string]string{}
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
			for i, n := range vs.Names {
				if !strings.HasPrefix(n.Name, "labelSuffix") {
					continue
				}
				if i >= len(vs.Values) {
					t.Fatalf("%s: a labelSuffix constant with no literal value", n.Name)
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s: a labelSuffix constant must be a string literal, got %T", n.Name, vs.Values[i])
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: unquote %s: %v", n.Name, lit.Value, err)
				}
				consts[n.Name] = value
			}
		}
	}
	return consts
}

// EVERY DECLARED SUFFIX HAS EXACTLY ONE ROW, and every row names a declared
// suffix. This is the assertion that would have caught #210, #217 and #256:
// each was a labelSuffix* constant reaching one of two hand-kept switches and
// not the other, and because otherBinaryLabel reads by exclusion each omission
// made every claim on an item carrying the label refuse other-binary.
func TestLabels_EveryDeclaredSuffixHasAVocabularyRow(t *testing.T) {
	declared := labelSuffixConsts(t)
	if len(declared) == 0 {
		t.Fatal("found no labelSuffix* constants in label.go — the walker is broken")
	}

	rows := map[string]structuralLabel{}
	for _, s := range structuralLabels {
		if _, dup := rows[s.suffix]; dup {
			t.Errorf("structuralLabels lists %q twice", s.suffix)
		}
		rows[s.suffix] = s
		// The valued bit is the spelling: a suffix that carries a value ends
		// in the separator, and one that is the whole label does not.
		if s.valued != strings.HasSuffix(s.suffix, ":") {
			t.Errorf("row %q: valued=%v disagrees with its spelling", s.suffix, s.valued)
		}
	}

	values := map[string]string{}
	for name, value := range declared {
		values[value] = name
		if _, ok := rows[value]; !ok {
			t.Errorf("%s = %q is declared but has no structuralLabels row — "+
				"otherBinaryLabel would read it as a binary name", name, value)
		}
	}
	for _, s := range structuralLabels {
		if _, ok := values[s.suffix]; !ok {
			t.Errorf("structuralLabels row %q names no labelSuffix* constant", s.suffix)
		}
	}
}

// sampleStructuralLabel spells one label under row `s`: the suffix alone, or
// the suffix carrying a value.
func sampleStructuralLabel(l labels, s structuralLabel) string {
	if s.valued {
		return l.named(s.suffix + "sample")
	}
	return l.named(s.suffix)
}

// Maintained IS structuralLabels' maintained bit and nothing else: the binary
// marker and an operator's own classification are outside the table, and a
// prefix other than flow: reads its own vocabulary.
func TestLabels_Maintained(t *testing.T) {
	l := newLabels("flow:")
	for _, s := range structuralLabels {
		label := sampleStructuralLabel(l, s)
		if got := l.Maintained(label); got != s.maintained {
			t.Errorf("Maintained(%q) = %v, want %v", label, got, s.maintained)
		}
	}
	for _, label := range []string{"flow:implement", "area:api", "custom:blocked"} {
		if l.Maintained(label) {
			t.Errorf("Maintained(%q) = true, want false", label)
		}
	}

	custom := newLabels("custom:")
	if !custom.Maintained("custom:blocked") {
		t.Error("Maintained(custom:blocked) under the custom: prefix = false, want true")
	}
	if custom.Maintained("flow:blocked") {
		t.Error("Maintained(flow:blocked) under the custom: prefix = true, want false")
	}
}

// A RETIRED spelling is structural — so otherBinaryLabel never reads it as
// another binary's name — and NOT maintained, so RemoveTag can clear one by
// hand. Live issues in this repository carry flow:seeded right now; the row
// missing would be a standing claim refusal on every one of them.
func TestLabels_RetiredSuffixesAreStructuralAndNotMaintained(t *testing.T) {
	l := newLabels("flow:")
	for _, s := range retiredSuffixes {
		label := sampleStructuralLabel(l, s)
		row, ok := l.structural(label)
		if !ok {
			t.Errorf("structural(%q) = false — otherBinaryLabel would read it as a binary name", label)
			continue
		}
		if row.maintained {
			t.Errorf("Maintained(%q) = true, want false — nothing writes a retired spelling", label)
		}
	}
	// The exact spellings that are out there, not just the row shapes.
	for _, label := range []string{"flow:seeded", "flow:stale:plan", "flow:budget-exhausted:push"} {
		if _, ok := l.structural(label); !ok {
			t.Errorf("structural(%q) = false, want true", label)
		}
		if l.Maintained(label) {
			t.Errorf("Maintained(%q) = true, want false", label)
		}
	}
}

// A retired suffix is deliberately NOT a labelSuffix* constant: it is no longer
// vocabulary, and a constant would put it back in the table the completeness
// test enforces.
func TestLabels_RetiredSuffixesAreNotVocabulary(t *testing.T) {
	declared := labelSuffixConsts(t)
	for _, s := range retiredSuffixes {
		for name, value := range declared {
			if value == s.suffix {
				t.Errorf("%s = %q is a retired spelling and must not be a labelSuffix* constant", name, value)
			}
		}
		for _, row := range structuralLabels {
			if row.suffix == s.suffix {
				t.Errorf("structuralLabels still lists the retired spelling %q", s.suffix)
			}
		}
	}
}

// A result the disclosure guard refuses at capture, whose refusal could not be
// kept with the step, parks step-did-not-complete rather than blocked (#325;
// one whose refusal was kept is revised inside the dispatch, and parks blocked
// only when that round is spent), and the move was made on one premise: the
// tracker's visible state does not change, because the kind is advertised by
// the same flow:blocked label a blocked park carries (docs/github-schema.md §
// Labels). This pins that premise. A label of the kind's own would move every
// such park off the one label discover reads as "parked, do not offer", and
// nothing else in the move would notice.
func TestParkLabel_StepDidNotCompleteIsAdvertisedAsBlocked(t *testing.T) {
	l := newLabels("flow:")
	req := &flow.ParkRequest{
		Kind:   flow.ParkStepDidNotComplete,
		Step:   "plan",
		Reason: "the disclosure guard refused this step's result (artifact-comment)",
	}
	if got := parkLabel(l, req); got != l.Blocked() {
		t.Errorf("parkLabel(step-did-not-complete) = %q, want %q — the label a refused capture was advertised under before it left the blocked kind", got, l.Blocked())
	}
}

// An exhausted agent account gets its own label rather than infra-transient's.
// The two parks share their treatment — nothing billed, no dispatch counted —
// and differ in what a human scanning the issue list should do: go look at the
// infrastructure for one, and nothing at all for the other until the window
// resets.
func TestParkLabel_AccountExhaustedIsItsOwnLabel(t *testing.T) {
	l := newLabels("flow:")
	req := &flow.ParkRequest{Kind: flow.ParkAccountExhausted, Step: "plan"}
	got := parkLabel(l, req)
	if got != l.AccountExhausted() {
		t.Errorf("parkLabel(account-exhausted) = %q, want %q", got, l.AccountExhausted())
	}
	if got == l.InfraTransient() {
		t.Error("an exhausted account is advertised as an infrastructure failure: the infrastructure is healthy")
	}
	if got == l.Blocked() {
		t.Error("an exhausted account fell through to the generic blocked label")
	}
	if want := "flow:account-exhausted"; got != want {
		t.Errorf("the wire spelling is %q, want %q — it matches the park kind", got, want)
	}
}

// hasParkLabel is parkLabel's inverse over the whole vocabulary: every kind's
// label must be one it recognises as advertising a park.
//
// The crosswalk walks AllParkKinds() rather than listing the five spellings,
// because the two functions are what decide whether the state comment is read
// for an item's park kind at all. A kind given a label of its own, with
// hasParkLabel left alone, would leave every item parked that way reporting no
// kind — silently, and only on the backend where it matters.
func TestParkLabels_AreAllRecognisedAsParks(t *testing.T) {
	l := newLabels("flow:")
	for _, kind := range flow.AllParkKinds() {
		req := &flow.ParkRequest{Kind: kind, Step: "plan"}
		label := parkLabel(l, req)
		if label == "" {
			t.Errorf("park kind %q advertises no label", kind)
			continue
		}
		if !hasParkLabel(l, []string{label}) {
			t.Errorf("hasParkLabel does not recognise %q, the label parkLabel gives park kind %q", label, kind)
		}
	}
	// Labels that are not park labels do not make an item read as parked —
	// otherwise the state comment would be fetched for every item in the
	// listing, which is the cost the index exists to avoid.
	for _, name := range []string{l.Manual(), l.Binary("issue"), "flow:awaits:contributor", "cli", ""} {
		if hasParkLabel(l, []string{name}) {
			t.Errorf("hasParkLabel(%q) = true, but that label advertises no park", name)
		}
	}
}

// Every member of the park vocabulary has a label written down, including the
// three that take parkLabel's fall-through — `blocked`, `step-did-not-complete`
// and `remote-unreachable`. Five kinds carry the generic label in all; the other
// two reach it through arms of their own, and the entries below say which is
// which. The crosswalk walks flow.AllParkKinds(), so a kind added to the
// vocabulary lands here rather than in `default: l.Blocked()`, where a kind that
// belongs under the generic label and one that does not read exactly alike
// (#324).
//
// TestParkLabels_AreAllRecognisedAsParks above proves every kind's label is
// recognised as advertising a park; this one proves WHICH label each kind gets.
func TestParkLabel_LabelsEveryKindDeliberately(t *testing.T) {
	l := newLabels("flow:")
	want := map[flow.ParkKind]string{
		// The fall-through, and the kind the generic label is named for.
		flow.ParkBlocked: l.Blocked(),
		// Arms of their own, both returning the generic label: a deterministic
		// refusal and a write-contract violation are blocked until a person
		// acts, and no budget grant clears either.
		flow.ParkRefused:       l.Blocked(),
		flow.ParkWriteContract: l.Blocked(),
		// The fall-through, and right: advertised as blocked since before it
		// left the blocked kind (#325); docs/github-schema.md § Labels and
		// TestParkLabel_StepDidNotCompleteIsAdvertisedAsBlocked hold why.
		flow.ParkStepDidNotComplete: l.Blocked(),
		// The fall-through, and #322 holds that it is the WRONG answer for this
		// kind: an unreachable remote is a condition that clears on its own, and
		// the generic label reads back as waits-on-person. Written down here so
		// the fall-through is visible; #322 is the item that changes this line.
		flow.ParkRemoteUnreachable: l.Blocked(),
		flow.ParkQuestion:          l.NeedsAnswer(),
		flow.ParkTreasurerRefused:  l.TreasurerRefused("plan"),
		flow.ParkInfraTransient:    l.InfraTransient(),
		flow.ParkAccountExhausted:  l.AccountExhausted(),
	}
	kinds := flow.AllParkKinds()
	if len(kinds) != len(want) {
		t.Fatalf("AllParkKinds() has %d members, but %d labels are written down: %v", len(kinds), len(want), kinds)
	}
	for _, kind := range kinds {
		label, ok := want[kind]
		if !ok {
			t.Errorf("park kind %q has no label written down here", kind)
			continue
		}
		if got := parkLabel(l, &flow.ParkRequest{Kind: kind, Step: "plan"}); got != label {
			t.Errorf("parkLabel(%q) = %q, want %q", kind, got, label)
		}
	}
	// A kind this binary does not know — a park written by a newer one — still
	// gets a label. The fall-through is load-bearing rather than tidy: Park
	// passes this result straight to AddLabels, so an empty answer would post a
	// park that no listing reads as parked.
	if got := parkLabel(l, &flow.ParkRequest{Kind: "from-the-future", Step: "plan"}); got != l.Blocked() {
		t.Errorf("parkLabel(unknown kind) = %q, want %q — an unrecognised kind is still advertised as parked", got, l.Blocked())
	}
	// No park, no label: the add and the remove go through this one function, so
	// an item that is not parked must name nothing rather than name the generic
	// label.
	if got := parkLabel(l, nil); got != "" {
		t.Errorf("parkLabel(nil) = %q, want the empty string", got)
	}
}
