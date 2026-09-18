package verifiedtree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/forge/primitives"
)

// THE CONTRACT IS WITH A PROGRAM THAT IS NOT IN THIS REPOSITORY. The commit
// guard that reads the record is a workspace tool, and what it compares the
// record against is `git write-tree` over the real index after `git add -A`.
// So the central assertion below is spelled out as those git commands rather
// than as another call into this package: a test that asked this package both
// questions would agree with itself no matter what it computed.

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), dir, "", args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newRepo is a fresh checkout with an identity and the .gitignore entry every
// project carrying this contract must have. ignoreRecord is false for the
// tests about a checkout that does not ignore the record.
func newRepo(t *testing.T, ignoreRecord bool) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", ".")
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "user.name", "test")
	ignore := "ignored/\n*.log\n"
	if ignoreRecord {
		ignore = "/.workspace/\n" + ignore
	}
	writeFile(t, filepath.Join(dir, ".gitignore"), ignore)
	writeFile(t, filepath.Join(dir, "kept.txt"), "one\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	return dir
}

// stagedByGit is what the guard computes: a real `git add -A` into the real
// index, then `git write-tree`. It MUTATES the checkout's index, so every
// caller runs it last.
func stagedByGit(t *testing.T, dir string) string {
	t.Helper()
	gitIn(t, dir, "add", "-A")
	return gitIn(t, dir, "write-tree")
}

func TestTreeIDEqualsWhatTheGuardWouldCompute(t *testing.T) {
	dir := newRepo(t, true)
	writeFile(t, filepath.Join(dir, "kept.txt"), "changed\n")
	writeFile(t, filepath.Join(dir, "fresh.txt"), "new\n")
	writeFile(t, filepath.Join(dir, "ignored", "junk"), "junk\n")

	got, err := TreeID(context.Background(), dir)
	if err != nil {
		t.Fatalf("TreeID: %v", err)
	}
	if want := stagedByGit(t, dir); got != want {
		t.Errorf("TreeID = %s, git add -A + write-tree = %s", got, want)
	}
}

func TestTreeIDCoversUntrackedAndNotIgnored(t *testing.T) {
	dir := newRepo(t, true)
	before, err := TreeID(context.Background(), dir)
	if err != nil {
		t.Fatalf("TreeID: %v", err)
	}

	writeFile(t, filepath.Join(dir, "ignored", "junk"), "junk\n")
	writeFile(t, filepath.Join(dir, "build.log"), "noise\n")
	ignoredSame, err := TreeID(context.Background(), dir)
	if err != nil {
		t.Fatalf("TreeID: %v", err)
	}
	if ignoredSame != before {
		t.Errorf("ignored content changed the tree: %s then %s", before, ignoredSame)
	}

	writeFile(t, filepath.Join(dir, "fresh.txt"), "new\n")
	after, err := TreeID(context.Background(), dir)
	if err != nil {
		t.Fatalf("TreeID: %v", err)
	}
	if after == before {
		t.Errorf("an untracked, unignored file left the tree unchanged at %s", before)
	}
}

// THE THREE CASES THE SEED GETS RIGHT AND NO OTHER SEED DOES. Ignore rules
// apply only to untracked paths, so the tracked set has to come from the
// index: a file that is tracked AND ignored belongs in the tree, one
// force-added but never committed belongs in it, and one just `git rm
// --cached`ed does not — and HEAD disagrees with the index about the last two.
func TestTreeIDTrackedSetComesFromTheIndexNotHEAD(t *testing.T) {
	t.Run("tracked but ignored stays in", func(t *testing.T) {
		dir := newRepo(t, true)
		writeFile(t, filepath.Join(dir, "ignored", "kept"), "tracked\n")
		gitIn(t, dir, "add", "-f", "ignored/kept")
		gitIn(t, dir, "commit", "-q", "-m", "track an ignored file")

		got, err := TreeID(context.Background(), dir)
		if err != nil {
			t.Fatalf("TreeID: %v", err)
		}
		if want := stagedByGit(t, dir); got != want {
			t.Errorf("TreeID = %s, git = %s — a tracked-but-ignored file was dropped", got, want)
		}
	})

	t.Run("force-added but not committed stays in", func(t *testing.T) {
		dir := newRepo(t, true)
		writeFile(t, filepath.Join(dir, "ignored", "staged"), "staged\n")
		gitIn(t, dir, "add", "-f", "ignored/staged")

		got, err := TreeID(context.Background(), dir)
		if err != nil {
			t.Fatalf("TreeID: %v", err)
		}
		if want := stagedByGit(t, dir); got != want {
			t.Errorf("TreeID = %s, git = %s — a force-added uncommitted file was dropped", got, want)
		}
	})

	t.Run("git rm --cached drops out", func(t *testing.T) {
		dir := newRepo(t, true)
		gitIn(t, dir, "rm", "-q", "--cached", "kept.txt")
		writeFile(t, filepath.Join(dir, ".gitignore"), "/.workspace/\nignored/\n*.log\nkept.txt\n")

		got, err := TreeID(context.Background(), dir)
		if err != nil {
			t.Fatalf("TreeID: %v", err)
		}
		if want := stagedByGit(t, dir); got != want {
			t.Errorf("TreeID = %s, git = %s — an uncached path was kept", got, want)
		}
	})
}

func TestTreeIDLeavesTheIndexFileUntouched(t *testing.T) {
	dir := newRepo(t, true)
	writeFile(t, filepath.Join(dir, "fresh.txt"), "new\n")
	index := filepath.Join(dir, ".git", "index")

	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if _, err := TreeID(context.Background(), dir); err != nil {
		t.Fatalf("TreeID: %v", err)
	}
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if string(before) != string(after) {
		t.Error("TreeID rewrote the real index")
	}
}

func TestTreeIDLeavesTheWorktreeStatusUnchanged(t *testing.T) {
	dir := newRepo(t, true)
	writeFile(t, filepath.Join(dir, "kept.txt"), "changed\n")
	writeFile(t, filepath.Join(dir, "fresh.txt"), "new\n")

	before := gitIn(t, dir, "status", "--porcelain")
	if _, err := TreeID(context.Background(), dir); err != nil {
		t.Fatalf("TreeID: %v", err)
	}
	if after := gitIn(t, dir, "status", "--porcelain"); after != before {
		t.Errorf("status changed across TreeID:\nbefore %q\nafter  %q", before, after)
	}
}

// A ZERO-LENGTH INDEX IS NOT AN ABSENT ONE. git refuses every command against
// a zero-byte index ("index file smaller than expected"), so seeding a copy
// from one turns a state git itself can recover from into a hard failure. The
// seed is skipped instead.
func TestTreeIDWithAZeroLengthIndex(t *testing.T) {
	dir := newRepo(t, true)
	writeFile(t, filepath.Join(dir, "fresh.txt"), "new\n")
	if err := os.WriteFile(filepath.Join(dir, ".git", "index"), nil, 0o644); err != nil {
		t.Fatalf("truncate index: %v", err)
	}

	got, err := TreeID(context.Background(), dir)
	if err != nil {
		t.Fatalf("TreeID over a zero-length index: %v", err)
	}
	if !isObjectID(got) {
		t.Errorf("TreeID = %q, want a tree id", got)
	}
}

func TestTreeIDOutsideACheckout(t *testing.T) {
	if _, err := TreeID(context.Background(), t.TempDir()); !errors.Is(err, ErrNotACheckout) {
		t.Errorf("TreeID outside a checkout: %v, want ErrNotACheckout", err)
	}
}

func TestStagedTreeIDIsTheRealIndex(t *testing.T) {
	dir := newRepo(t, true)
	writeFile(t, filepath.Join(dir, "fresh.txt"), "new\n")

	head := gitIn(t, dir, "rev-parse", "HEAD^{tree}")
	staged, err := StagedTreeID(context.Background(), dir)
	if err != nil {
		t.Fatalf("StagedTreeID: %v", err)
	}
	if staged != head {
		t.Errorf("StagedTreeID = %s with nothing staged, want the HEAD tree %s", staged, head)
	}

	gitIn(t, dir, "add", "-A")
	staged, err = StagedTreeID(context.Background(), dir)
	if err != nil {
		t.Fatalf("StagedTreeID: %v", err)
	}
	if want := gitIn(t, dir, "write-tree"); staged != want {
		t.Errorf("StagedTreeID = %s, git write-tree = %s", staged, want)
	}
}

func TestStagedTreeIDOutsideACheckout(t *testing.T) {
	if _, err := StagedTreeID(context.Background(), t.TempDir()); !errors.Is(err, ErrNotACheckout) {
		t.Errorf("StagedTreeID outside a checkout: %v, want ErrNotACheckout", err)
	}
}

// A CHECK THAT CANNOT RUN, FAILS. An unmerged index has no tree, and answering
// "" would be read by a caller as "nothing is staged" — a commit gate passing
// mid-conflict.
func TestStagedTreeIDFailsOnAnUnmergedIndex(t *testing.T) {
	dir := newRepo(t, true)
	base := gitIn(t, dir, "rev-parse", "--abbrev-ref", "HEAD")

	gitIn(t, dir, "checkout", "-q", "-b", "other")
	writeFile(t, filepath.Join(dir, "kept.txt"), "theirs\n")
	gitIn(t, dir, "commit", "-q", "-am", "theirs")

	gitIn(t, dir, "checkout", "-q", base)
	writeFile(t, filepath.Join(dir, "kept.txt"), "ours\n")
	gitIn(t, dir, "commit", "-q", "-am", "ours")

	// Expected to fail: it is what leaves the index unmerged.
	if _, err := git(context.Background(), dir, "", "merge", "--no-edit", "other"); err == nil {
		t.Fatal("the merge did not conflict, so the index is not unmerged")
	}

	if _, err := StagedTreeID(context.Background(), dir); err == nil {
		t.Error("StagedTreeID answered over an unmerged index")
	}
}

// AN UNIGNORED RECORD IS INSIDE ITS OWN SUBJECT. Writing it would change the
// tree it names, so the blessing would be for content that stopped existing
// the moment it was recorded and every commit would be refused with nothing
// the caller could do. The refusal names the path, at the one moment the cause
// is still visible.
func TestBlessRefusesWhenTheRecordIsNotIgnored(t *testing.T) {
	dir := newRepo(t, false)

	_, err := Bless(context.Background(), dir)
	if !errors.Is(err, ErrRecordNotIgnored) {
		t.Fatalf("Bless = %v, want ErrRecordNotIgnored", err)
	}
	if !strings.Contains(err.Error(), primitives.VerifiedTreeRecord) {
		t.Errorf("the refusal does not name the path: %v", err)
	}
	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Error("a refused bless wrote a record anyway")
	}
}

// A TRACKED RECORD IS THE SAME DEFECT WEARING AN IGNORE RULE. `git
// check-ignore` consults the index, so a force-added record reports as not
// ignored — which is the answer wanted.
func TestBlessRefusesWhenTheRecordIsTracked(t *testing.T) {
	dir := newRepo(t, true)
	writeFile(t, Path(dir), "deadbeef\n")
	gitIn(t, dir, "add", "-f", primitives.VerifiedTreeRecord)

	if _, err := Bless(context.Background(), dir); !errors.Is(err, ErrRecordNotIgnored) {
		t.Fatalf("Bless = %v, want ErrRecordNotIgnored", err)
	}
	body, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if string(body) != "deadbeef\n" {
		t.Errorf("a refused bless overwrote the record: %q", body)
	}
}

func TestBlessOutsideACheckout(t *testing.T) {
	if _, err := Bless(context.Background(), t.TempDir()); !errors.Is(err, ErrNotACheckout) {
		t.Errorf("Bless outside a checkout: %v, want ErrNotACheckout", err)
	}
}

// THE FORMAT IS HALF THE CONTRACT. The reader is a program in another
// repository that cannot be imported here, so nothing but agreement on the
// bytes connects the two ends: one tree id, newline terminated, and nothing
// else. Every other test reads the record through a trim, so a record written
// without its newline — or with a second line of commentary — passes all of
// them and is refused by a reader this repository cannot run.
func TestBlessWritesOneTreeIdNewlineTerminated(t *testing.T) {
	dir := newRepo(t, true)

	tree, err := Bless(context.Background(), dir)
	if err != nil {
		t.Fatalf("Bless: %v", err)
	}
	body, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if string(body) != tree+"\n" {
		t.Errorf("record = %q, want exactly %q", body, tree+"\n")
	}
}

func TestBlessCreatesTheDirectoryAndLeavesNoTemporary(t *testing.T) {
	dir := newRepo(t, true)
	recordDir := filepath.Dir(Path(dir))
	if _, err := os.Stat(recordDir); !os.IsNotExist(err) {
		t.Fatalf("the record's directory already exists, so this proves nothing")
	}

	if _, err := Bless(context.Background(), dir); err != nil {
		t.Fatalf("Bless: %v", err)
	}
	if _, err := Bless(context.Background(), dir); err != nil {
		t.Fatalf("second Bless: %v", err)
	}

	entries, err := os.ReadDir(recordDir)
	if err != nil {
		t.Fatalf("read %s: %v", recordDir, err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(Path(dir)) {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s holds %v, want only the record — an atomic write left its temporary behind", recordDir, names)
	}
}

func TestCheckStates(t *testing.T) {
	valid := strings.Repeat("a1", 20)
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"blank", "", ""},
		{"a newline only", "\n", ""},
		{"whitespace only", "   \n\t\n", ""},
		{"a second line", valid + "\nand a note\n", ""},
		{"not hex", strings.Repeat("z", 40) + "\n", ""},
		{"uppercase", strings.ToUpper(valid) + "\n", ""},
		{"too short", valid[:39] + "\n", ""},
		{"a tree id", valid + "\n", valid},
		{"a tree id with no newline", valid, valid},
		{"a sha-256 tree id", strings.Repeat("b2", 32) + "\n", strings.Repeat("b2", 32)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, Path(dir), tc.body)
			got, err := Check(dir)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if got != tc.want {
				t.Errorf("Check = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckAbsentRecord(t *testing.T) {
	got, err := Check(t.TempDir())
	if err != nil {
		t.Fatalf("Check on an absent record: %v", err)
	}
	if got != "" {
		t.Errorf("Check = %q, want \"\"", got)
	}
}

// AN UNREADABLE RECORD IS NOT AN UNBLESSED TREE. Absent is an answer; a record
// that exists and cannot be read is a question nobody answered, and returning
// "" would let it pass for one.
func TestCheckReportsAnUnreadableRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(Path(dir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := Check(dir); err == nil {
		t.Error("Check read a directory as a record and reported no error")
	}
}

// BOTH SIDES ARE "" FOR A REASON, AND THEY MUST NOT MEET. The record is "" when
// nothing has blessed the tree; a caller's tree id is "" when computing it
// failed. Under `==` those two failures agree and an unverified checkout reads
// as blessed.
func TestBlesses(t *testing.T) {
	id := strings.Repeat("a1", 20)
	other := strings.Repeat("b2", 20)
	for _, tc := range []struct {
		name         string
		record, tree string
		want         bool
	}{
		{"the same tree", id, id, true},
		{"a different tree", id, other, false},
		{"nothing blessed", "", id, false},
		{"no tree to compare", id, "", false},
		{"neither", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Blesses(tc.record, tc.tree); got != tc.want {
				t.Errorf("Blesses(%q, %q) = %v, want %v", tc.record, tc.tree, got, tc.want)
			}
		})
	}
}

func TestBlessIfEqualRecordsOnlyTheNamedTree(t *testing.T) {
	dir := newRepo(t, true)
	tree, err := TreeID(context.Background(), dir)
	if err != nil {
		t.Fatalf("TreeID: %v", err)
	}

	ok, err := BlessIfEqual(context.Background(), dir, tree)
	if err != nil {
		t.Fatalf("BlessIfEqual: %v", err)
	}
	if !ok {
		t.Fatal("BlessIfEqual over the tree it names did not record it")
	}
	if got, _ := Check(dir); got != tree {
		t.Errorf("record = %q, want %q", got, tree)
	}

	// The checkout moves on: a green result for the earlier tree must not be
	// recorded against this one, and must not disturb what is already there.
	writeFile(t, filepath.Join(dir, "fresh.txt"), "new\n")
	ok, err = BlessIfEqual(context.Background(), dir, tree)
	if err != nil {
		t.Fatalf("BlessIfEqual after the tree moved: %v", err)
	}
	if ok {
		t.Error("BlessIfEqual recorded a tree the checkout no longer holds")
	}
	if got, _ := Check(dir); got != tree {
		t.Errorf("a mismatched BlessIfEqual disturbed the record: %q, want %q", got, tree)
	}
}

func TestBlessIfEqualRefusesAnEmptyTree(t *testing.T) {
	dir := newRepo(t, true)
	ok, err := BlessIfEqual(context.Background(), dir, "")
	if err == nil {
		t.Error("BlessIfEqual accepted a tree id its caller failed to compute")
	}
	if ok {
		t.Error("BlessIfEqual reported a blessing it did not make")
	}
	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Error("BlessIfEqual wrote a record for an empty tree id")
	}
}

func TestClear(t *testing.T) {
	dir := newRepo(t, true)
	if _, err := Bless(context.Background(), dir); err != nil {
		t.Fatalf("Bless: %v", err)
	}

	if err := Clear(dir); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got, _ := Check(dir); got != "" {
		t.Errorf("Check after Clear = %q, want \"\"", got)
	}
	if err := Clear(dir); err != nil {
		t.Errorf("Clear on an absent record: %v, want success", err)
	}
}

func TestClearReportsWhatItCouldNotRemove(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(Path(dir), "in-the-way"), "x\n")

	if err := Clear(dir); err == nil {
		t.Error("Clear reported success over a record it did not remove — a stale blessing would survive a red run")
	}
}

// ONE HOME FOR THE PATH. The contract has two ends in two repositories and
// neither module can import the other, so the location is one constant they
// both import rather than a spelling each keeps. A local re-spelling here
// would be a silent, permanent refusal: verify writes one path and the guard
// reads another, always finding it absent.
func TestPathIsTheOneConstant(t *testing.T) {
	want := filepath.Join("root", filepath.FromSlash(primitives.VerifiedTreeRecord))
	if got := Path("root"); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestBlessThenCheckAgreeAndStopAgreeingWhenTheTreeMoves(t *testing.T) {
	dir := newRepo(t, true)
	if _, err := Bless(context.Background(), dir); err != nil {
		t.Fatalf("Bless: %v", err)
	}

	record, err := Check(dir)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	tree, err := TreeID(context.Background(), dir)
	if err != nil {
		t.Fatalf("TreeID: %v", err)
	}
	if !Blesses(record, tree) {
		t.Fatalf("a blessed tree does not read as blessed: record %q, tree %q", record, tree)
	}

	writeFile(t, filepath.Join(dir, "fresh.txt"), "new\n")
	tree, err = TreeID(context.Background(), dir)
	if err != nil {
		t.Fatalf("TreeID: %v", err)
	}
	if Blesses(record, tree) {
		t.Error("the record still blesses the tree after the content changed")
	}
}
