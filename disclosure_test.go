package flow

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// An origin that cannot be stated is a refusal, so Valid has to say no to
// everything outside the set — including the two spellings a caller reaches for
// when it does not know: the zero value, and a word that means "I did not
// find out".
func TestOrigin_Valid(t *testing.T) {
	for _, c := range []struct {
		origin Origin
		valid  bool
	}{
		{OriginWorktree, true},
		{OriginItem, true},
		{OriginFlow, true},
		{OriginOperator, true},
		{OriginAgent, true},
		{OriginElsewhere, true},

		// The zero value. A struct field nobody filled in is the commonest way
		// an origin goes unstated, and it must not be the one that passes.
		{"", false},
		// There is deliberately no `unknown` member: "a value that can be
		// passed is a value someone defaults to."
		{"unknown", false},
		{"none", false},
		{"probably-fine", false},
		// Case and spacing are not forgiven. A near miss is a caller that
		// invented a name, which is what a closed set exists to stop.
		{"Agent", false},
		{"agent ", false},
	} {
		t.Run(string(c.origin), func(t *testing.T) {
			if got := c.origin.Valid(); got != c.valid {
				t.Errorf("Origin(%q).Valid() = %v, want %v", c.origin, got, c.valid)
			}
		})
	}
}

func TestDisclosureAct_Valid(t *testing.T) {
	for _, c := range []struct {
		act   DisclosureAct
		valid bool
	}{
		{ActArtifactComment, true},
		{ActPush, true},
		{ActArtifactFile, true},

		{"", false},
		{"unknown", false},
		// A plausible near-name for a declared act. The guard switches on these
		// strings, so a name it does not know is a write it cannot answer about.
		{"comment", false},
		{"pull-request-open", false},
	} {
		t.Run(string(c.act), func(t *testing.T) {
			if got := c.act.Valid(); got != c.valid {
				t.Errorf("DisclosureAct(%q).Valid() = %v, want %v", c.act, got, c.valid)
			}
		})
	}
}

// AllOrigins and AllDisclosureActs are hand-maintained, and every consumer that
// enumerates one — a guard deciding it has an answer for each member, Valid
// itself — trusts it to be the whole set. A constant added without being listed
// would be invisible to all of them, and Valid would refuse a write that names
// it: the drift this parses the source to prevent.
//
// Asserting a count would not do: it passes the moment someone adds one
// constant and removes another.
func TestAllOriginsAndActs_MatchTheDeclaredConstants(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "disclosure.go", nil, 0)
	if err != nil {
		t.Fatalf("parse disclosure.go: %v", err)
	}

	declared := map[string]map[string]bool{"Origin": {}, "DisclosureAct": {}}
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		id, ok := spec.Type.(*ast.Ident)
		if !ok || declared[id.Name] == nil {
			return true
		}
		for _, v := range spec.Values {
			lit, ok := v.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			declared[id.Name][strings.Trim(lit.Value, `"`)] = true
		}
		return true
	})

	listed := map[string]map[string]bool{
		"Origin":        {},
		"DisclosureAct": {},
	}
	for _, o := range AllOrigins() {
		listed["Origin"][string(o)] = true
	}
	for _, a := range AllDisclosureActs() {
		listed["DisclosureAct"][string(a)] = true
	}

	for typeName, want := range declared {
		if len(want) == 0 {
			t.Fatalf("parsed no %s constants — the probe is broken, not the code", typeName)
		}
		for name := range want {
			if !listed[typeName][name] {
				t.Errorf("%s %q is declared but missing from All%ss — every consumer that "+
					"enumerates the set would not see it, and Valid would refuse it", typeName, name, typeName)
			}
		}
		for name := range listed[typeName] {
			if !want[name] {
				t.Errorf("All%ss lists %q, which is not a declared %s constant", typeName, name, typeName)
			}
		}
	}
}

// Two names for one string are one act: the guard cannot tell the writes apart,
// and a refusal cannot say which of them it refused. The same argument holds
// for origins, where a duplicate would mean two parties sharing one name.
func TestAllOriginsAndActs_HaveNoDuplicates(t *testing.T) {
	seenOrigin := map[Origin]bool{}
	for _, o := range AllOrigins() {
		if seenOrigin[o] {
			t.Errorf("the origin %q is listed twice", o)
		}
		seenOrigin[o] = true
	}
	seenAct := map[DisclosureAct]bool{}
	for _, a := range AllDisclosureActs() {
		if seenAct[a] {
			t.Errorf("the act %q is listed twice", a)
		}
		seenAct[a] = true
	}
}

// The returned slices are fresh each call. A package-level var would be mutable
// by any importer, and a guard that reordered or truncated one would change the
// set for everyone else in the process — including for Valid.
func TestAllOriginsAndActs_CallerCannotMutateTheSet(t *testing.T) {
	AllOrigins()[0] = "clobbered"
	if AllOrigins()[0] == "clobbered" {
		t.Error("mutating the returned slice changed the origin set for the next caller")
	}
	AllDisclosureActs()[0] = "clobbered"
	if AllDisclosureActs()[0] == "clobbered" {
		t.Error("mutating the returned slice changed the act set for the next caller")
	}
}

// A refusal has to be recognisable without matching on a message, and must
// never be mistaken for ErrTransient — which the orchestrator retries, and
// retrying a refusal re-proposes the very text that was refused.
func TestErrDisclosureRefused_CarriesTheActAndTheReason(t *testing.T) {
	reason := ErrNoDisclosureGuard
	err := error(ErrDisclosureRefused{Act: ActPush, Reason: reason})

	if !errors.Is(err, ErrNoDisclosureGuard) {
		t.Errorf("errors.Is(%v, ErrNoDisclosureGuard) = false, want true", err)
	}
	if errors.Is(err, ErrTransient) {
		t.Error("a refusal is indistinguishable from a transient failure, so it will be retried")
	}
	var refused ErrDisclosureRefused
	if !errors.As(err, &refused) {
		t.Fatalf("errors.As did not reach ErrDisclosureRefused from %v", err)
	}
	if refused.Act != ActPush {
		t.Errorf("refusal names the act %q, want %q", refused.Act, ActPush)
	}
	for _, want := range []string{string(ActPush), reason.Error()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
}
