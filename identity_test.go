package flow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	if got := len(AllPriorities()); got != 4 {
		t.Errorf("Priority is closed at four, got %d", got)
	}
	if got := len(AllUrgencies()); got != 3 {
		t.Errorf("Urgency is closed at three, got %d", got)
	}
	// The empty value is never a member: an item that is blocked has a kind,
	// and a run with no outcome is one the runner never classified. The two
	// selection axes have a neutral member each, but the empty value is not it
	// — that is what OrNeutral is for.
	if ItemStatus("").Valid() || BlockKind("").Valid() || CommandName("").Valid() ||
		Priority("").Valid() || Urgency("").Valid() {
		t.Error("the empty value passed a closed-set check")
	}
	// A misspelling is not a member. A closed vocabulary that accepted one
	// would store a value naming nothing and sort as though nobody had set it.
	if Priority("hihg").Valid() || Urgency("soon").Valid() {
		t.Error("a value naming no member passed a closed-set check")
	}
	for _, p := range AllPriorities() {
		if !p.Valid() {
			t.Errorf("AllPriorities returned %q, which Valid rejects", p)
		}
	}
	for _, u := range AllUrgencies() {
		if !u.Valid() {
			t.Errorf("AllUrgencies returned %q, which Valid rejects", u)
		}
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

// ---------------------------------------------------------------------------
// The arena anchor.
//
// checkoutRoot is the walk DeriveArenaRoot performs over the directory the
// running binary lives in, with the executable and the real home directory
// factored out so the walk itself is testable. Every fixture below is a
// t.TempDir tree: an absolute path written into a test would be one operator's
// machine baked into the suite, which is the class of thing this file exists to
// remove.
// ---------------------------------------------------------------------------

// mkTree creates dir and returns it.
func mkTree(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(parts...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	return p
}

// gitDir marks dir as an ordinary checkout.
func gitDir(t *testing.T, dir string) string {
	t.Helper()
	mkTree(t, dir, ".git")
	return dir
}

// gitFile marks dir as a LINKED worktree, whose `.git` is a file naming the
// gitdir rather than a directory holding it.
func gitFile(t *testing.T, dir string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: ../main/.git/worktrees/w\n"), 0o644); err != nil {
		t.Fatalf("write .git file: %v", err)
	}
	return dir
}

func TestCheckoutRootFindsTheCheckoutTheBinaryLivesIn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// build lays out a tree under tmp and returns the directory the
		// executable sits in, plus the checkout that must be found from it.
		build func(t *testing.T, tmp string) (start, want string)
	}{
		{
			"the executable's own directory is the checkout",
			func(t *testing.T, tmp string) (string, string) {
				root := gitDir(t, mkTree(t, tmp, "checkout"))
				return root, root
			},
		},
		{
			"a binary in <root>/bin — where every flow binary is built",
			func(t *testing.T, tmp string) (string, string) {
				root := gitDir(t, mkTree(t, tmp, "checkout"))
				return mkTree(t, root, "bin"), root
			},
		},
		{
			"nested three deep",
			func(t *testing.T, tmp string) (string, string) {
				root := gitDir(t, mkTree(t, tmp, "checkout"))
				return mkTree(t, root, "tools", "build", "bin"), root
			},
		},
		{
			// A linked `git worktree` records its gitdir in a FILE. A linked
			// worktree is exactly the arena this project is about, so accepting
			// only the directory form would refuse the case.
			"a linked worktree, whose .git is a file",
			func(t *testing.T, tmp string) (string, string) {
				root := gitFile(t, mkTree(t, tmp, "linked"))
				return mkTree(t, root, "bin"), root
			},
		},
		{
			// The NEAREST ancestor wins: a checkout inside a checkout is where
			// the binary lives, and the outer one is somebody else's arena.
			"nested checkouts — the nearest ancestor wins",
			func(t *testing.T, tmp string) (string, string) {
				outer := gitDir(t, mkTree(t, tmp, "outer"))
				inner := gitDir(t, mkTree(t, outer, "vendor", "inner"))
				return mkTree(t, inner, "bin"), inner
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, want := tc.build(t, t.TempDir())
			got, err := checkoutRoot(start, "")
			if err != nil {
				t.Fatalf("checkoutRoot(%s) = %v, want %s", start, err, want)
			}
			if got != want {
				t.Errorf("checkoutRoot(%s) = %s, want %s", start, got, want)
			}
		})
	}
}

// A binary that is inside no checkout gets an ERROR, never a guess. The guess
// is what the defect was: `"."` is always an answer, and always the wrong one.
func TestCheckoutRootRefusesWhatIsNotACheckout(t *testing.T) {
	t.Run("no .git anywhere above the binary", func(t *testing.T) {
		start := mkTree(t, t.TempDir(), "not", "a", "checkout")
		got, err := checkoutRoot(start, "")
		if err == nil {
			t.Fatalf("checkoutRoot(%s) = %s, want an error — nothing there is a checkout", start, got)
		}
		if !strings.Contains(err.Error(), start) {
			t.Errorf("error = %v, want it to name %s, the directory the operator has to look at", err, start)
		}
	})

	// The home directory is refused as a candidate rather than adopted: a home
	// that happens to be a dotfiles repo is not the checkout a binary works on,
	// and without this a binary installed at ~/go/bin would take $HOME as its
	// arena — one ArenaId shared by every binary installed that way.
	t.Run("the home directory is not a checkout, even when it is a repo", func(t *testing.T) {
		home := gitDir(t, mkTree(t, t.TempDir(), "home"))
		start := mkTree(t, home, "go", "bin")
		if got, err := checkoutRoot(start, home); err == nil {
			t.Fatalf("checkoutRoot(%s, home=%s) = %s, want a refusal", start, home, got)
		}
	})

	// The walk terminates rather than looping at the filesystem root.
	t.Run("the walk reaches the filesystem root", func(t *testing.T) {
		root := t.TempDir()
		for parent := filepath.Dir(root); parent != root; parent = filepath.Dir(root) {
			root = parent
		}
		if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
			t.Skipf("%s is itself a checkout on this machine", root)
		}
		if got, err := checkoutRoot(root, ""); err == nil {
			t.Fatalf("checkoutRoot(%s) = %s, want an error rather than a walk that never ends", root, got)
		}
	})
}
