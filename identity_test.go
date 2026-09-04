package flow

import "testing"

// The TagId floor is load-bearing rather than decorative: a tag is
// interpolated into the orchestrator's own query, where a value containing a
// space does not fail — it silently becomes a different query. Below the floor
// a value is not a tag and is refused rather than stored.
func TestTagId_Valid(t *testing.T) {
	for _, tc := range []struct {
		name string
		tag  TagId
		want bool
	}{
		{"ordinary", "area/parser", true},
		{"single word", "bug", true},
		{"inner space", "needs triage", true}, // a space inside is the orchestrator's business
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"leading space", " bug", false},
		{"trailing space", "bug ", false},
		{"leading tab", "\tbug", false},
		{"trailing newline", "bug\n", false},
		{"embedded newline", "bug\nfix", false},
		{"embedded carriage return", "bug\rfix", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tag.Valid(); got != tc.want {
				t.Errorf("TagId(%q).Valid() = %v, want %v", string(tc.tag), got, tc.want)
			}
		})
	}
}

// AccountId becomes a label (flow:owner:<account>) and an assignee, so
// docs/orchestrator.md gives it "the same floor as TagId" — one rule, one
// implementation, and this pins that they have not drifted apart.
func TestAccountId_CarriesTheSameFloorAsTagId(t *testing.T) {
	for _, s := range []string{"octocat", "", "  ", " octocat", "octocat ", "octo\ncat", "octo\rcat"} {
		if got, want := AccountId(s).Valid(), TagId(s).Valid(); got != want {
			t.Errorf("AccountId(%q).Valid() = %v but TagId(%q).Valid() = %v — the two floors have drifted",
				s, got, s, want)
		}
	}
}

// The comparison is EXACT and the filter is CONJUNCTIVE. Anything looser and
// one --tag value means two different things across `list` and `resolve`,
// which are meant to read as symmetrical.
func TestTagsMatch(t *testing.T) {
	have := []TagId{"area/parser", "bug", "P1"}
	for _, tc := range []struct {
		name string
		want []TagId
		ok   bool
	}{
		{"no filter matches everything", nil, true},
		{"empty filter matches everything", []TagId{}, true},
		{"one present", []TagId{"bug"}, true},
		{"all present", []TagId{"bug", "P1", "area/parser"}, true},
		{"one absent", []TagId{"bug", "missing"}, false},
		{"case differs", []TagId{"Bug"}, false},
		{"case differs on the other side", []TagId{"p1"}, false},
		{"prefix is not a match", []TagId{"area"}, false},
		{"substring is not a match", []TagId{"par"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TagsMatch(have, tc.want); got != tc.ok {
				t.Errorf("TagsMatch(%v, %v) = %v, want %v", have, tc.want, got, tc.ok)
			}
		})
	}
	if TagsMatch(nil, []TagId{"bug"}) {
		t.Error("an item with no tags matched a filter naming one")
	}
}

// These are compared by string equality across systems, so two spellings of one
// host are two hosts. Normalization is a requirement, not a courtesy.
func TestNormalizeHostId(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want HostId
	}{
		{"Build01.US-East", "build01"},
		{"build01.us-east.example.com", "build01"},
		{"BUILD01", "build01"},
		{"build01", "build01"},
		{"  build01.local  ", "build01"},
		{"", ""},
	} {
		if got := NormalizeHostId(tc.in); got != tc.want {
			t.Errorf("NormalizeHostId(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Two machines in different domains normalize to one HostId. That is not a bug
// in the normalization — the short form drops the domain by contract — and it
// is exactly why a fleet spanning domains must assign unique short names rather
// than relying on the domain to separate them.
func TestNormalizeHostId_ShortFormDropsTheDomain(t *testing.T) {
	east := NormalizeHostId("build01.us-east")
	west := NormalizeHostId("build01.eu-west")
	if east != west {
		t.Fatalf("expected both to normalize to one id, got %q and %q", east, west)
	}
}

func TestArenaAndHolder_Empty(t *testing.T) {
	if !(Arena{}).Empty() {
		t.Error("the zero Arena names an arena")
	}
	// Both halves are the identity, so a pair missing either one names nothing.
	if !(Arena{Host: "build01"}).Empty() {
		t.Error("an Arena with no ArenaId reads as an identity")
	}
	if !(Arena{Id: "/w/repo"}).Empty() {
		t.Error("an Arena with no HostId reads as an identity — an ArenaId alone is a component, not a name")
	}
	if (Arena{Host: "build01", Id: "/w/repo"}).Empty() {
		t.Error("a complete Arena reads as empty")
	}
	if !(Holder{}).Empty() {
		t.Error("the zero Holder reads as claimed")
	}
	if (Holder{Account: "octocat"}).Empty() {
		t.Error("a Holder carrying an account reads as unclaimed")
	}
}

func TestVocabularies_AreClosed(t *testing.T) {
	if got := len(AllItemStatuses()); got != 2 {
		t.Errorf("ItemStatus is closed at two, got %d", got)
	}
	if got := len(AllBlockKinds()); got != 3 {
		t.Errorf("BlockKind is closed at three, got %d", got)
	}
	if got := len(AllCommandNames()); got != 3 {
		t.Errorf("CommandName is closed at three, got %d", got)
	}
	// The empty value is never a member: an item that is blocked has a kind,
	// and a run with no outcome is one the runner never classified.
	if ItemStatus("").Valid() || BlockKind("").Valid() || CommandName("").Valid() {
		t.Error("the empty value passed a closed-set check")
	}
	for _, s := range AllItemStatuses() {
		if !s.Valid() {
			t.Errorf("AllItemStatuses returned %q, which Valid rejects", s)
		}
	}
	for _, k := range AllBlockKinds() {
		if !k.Valid() {
			t.Errorf("AllBlockKinds returned %q, which Valid rejects", k)
		}
	}
	for _, c := range AllCommandNames() {
		if !c.Valid() {
			t.Errorf("AllCommandNames returned %q, which Valid rejects", c)
		}
	}
	// A fourth command is not a project's to invent: the flow decides when each
	// runs and would have no place to run one it did not know about.
	if CommandName("deploy").Valid() {
		t.Error("an invented command name passed Valid")
	}
}
