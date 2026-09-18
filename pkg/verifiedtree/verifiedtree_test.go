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

// AN UNREADABLE INDEX IS NOT AN EMPTY ONE. Only an ABSENT index means the
// tracked set is empty; any other read failure means it is UNKNOWN, and
// carrying on without it silently drops exactly the tracked-but-ignored and
// force-added paths the seed exists to keep. The answer would still look like a
// tree id, and it would name content no `git add -A` in this checkout can
// stage — the silent disagreement with the guard that this package exists to
// end.
func TestTreeIDFailsWhenTheIndexCannotBeRead(t *testing.T) {
	dir := newRepo(t, true)
	writeFile(t, filepath.Join(dir, "ignored", "kept"), "tracked\n")
	gitIn(t, dir, "add", "-f", "ignored/kept")
	gitIn(t, dir, "commit", "-q", "-m", "track an ignored file")

	// A directory where the index file belongs: `git rev-parse --git-path`
	// still names it, and reading it fails for a reason that is not absence.
	index := filepath.Join(dir, ".git", "index")
	if err := os.Remove(index); err != nil {
		t.Fatalf("remove the index: %v", err)
	}
	if err := os.Mkdir(index, 0o755); err != nil {
		t.Fatalf("put a directory where the index belongs: %v", err)
	}

	if got, err := TreeID(context.Background(), dir); err == nil {
		t.Errorf("TreeID = %s over an index it could not read, want an error — the tracked set was guessed", got)
	}
}

// A LINKED WORKTREE HAS ITS OWN INDEX, AND IT IS THE ONE TO SEED FROM. This is
// where a resolution works, so it is where the record is written and read.
// `<root>/.git` there is a FILE, not a directory, so a guess at
// `<root>/.git/index` reads nothing at all, and the superproject's index
// describes a tree nobody is working on — both answer about content the guard
// in this checkout will never be shown.
func TestTreeIDInALinkedWorktree(t *testing.T) {
	main := newRepo(t, true)
	linked := filepath.Join(t.TempDir(), "linked")
	gitIn(t, main, "worktree", "add", "-q", "-b", "side", linked)

	writeFile(t, filepath.Join(linked, "kept.txt"), "changed here\n")
	writeFile(t, filepath.Join(linked, "fresh.txt"), "new\n")
	// Staged in the linked worktree's own index and nowhere else: the
	// superproject's index has never seen it.
	gitIn(t, linked, "add", "fresh.txt")

	got, err := TreeID(context.Background(), linked)
	if err != nil {
		t.Fatalf("TreeID in a linked worktree: %v", err)
	}
	mainTree, err := TreeID(context.Background(), main)
	if err != nil {
		t.Fatalf("TreeID in the superproject: %v", err)
	}
	if got == mainTree {
		t.Errorf("TreeID answered the superproject's tree %s for the linked worktree", got)
	}
	if want := stagedByGit(t, linked); got != want {
		t.Errorf("TreeID = %s, git add -A + write-tree in the linked worktree = %s", got, want)
	}
}

// The blessing has to work where the flow works. The linked worktree shares
// the committed .gitignore, so the record is ignored there too.
func TestBlessAndCheckInALinkedWorktree(t *testing.T) {
	main := newRepo(t, true)
	linked := filepath.Join(t.TempDir(), "linked")
	gitIn(t, main, "worktree", "add", "-q", "-b", "side", linked)
	writeFile(t, filepath.Join(linked, "fresh.txt"), "new\n")

	tree, err := Bless(context.Background(), linked)
	if err != nil {
		t.Fatalf("Bless in a linked worktree: %v", err)
	}
	record, err := Check(linked)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !Blesses(record, tree) {
		t.Fatalf("the linked worktree's record %q does not bless the tree it just blessed %q", record, tree)
	}
	// The superproject is a different checkout and nothing blessed it.
	if got, err := Check(main); err != nil || got != "" {
		t.Errorf("Check(superproject) = %q, %v — a worktree's blessing leaked", got, err)
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

// THE VERIFY LOOP RE-BLESSES, AND THE SECOND RUN IS ABOUT DIFFERENT CONTENT.
// A record naming the earlier tree must be replaced — not kept, not appended
// to. Kept, and the guard refuses the very commit the green run just cleared;
// appended to, and the record stops being one tree id and blesses nothing at
// all. The tests above only ever bless the same tree twice, so a write that
// declines to overwrite passes all of them.
func TestBlessReplacesTheRecordWhenTheTreeMoves(t *testing.T) {
	dir := newRepo(t, true)

	first, err := Bless(context.Background(), dir)
	if err != nil {
		t.Fatalf("Bless: %v", err)
	}
	writeFile(t, filepath.Join(dir, "fresh.txt"), "new\n")
	second, err := Bless(context.Background(), dir)
	if err != nil {
		t.Fatalf("second Bless: %v", err)
	}
	if second == first {
		t.Fatalf("the tree did not move across the two blessings (%s), so this proves nothing", first)
	}

	body, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if string(body) != second+"\n" {
		t.Errorf("record = %q, want exactly %q — the second blessing did not replace the first", body, second+"\n")
	}
}

// A BLESSING THAT WAS NOT RECORDED MUST NOT BE REPORTED AS ONE. Bless hands
// back the tree it recorded and its caller prints it and moves on; a swallowed
// write failure is a run that reports green and blesses nothing, which is the
// whole shape of the failure this package exists to end. The temporary goes
// with it: a write that could not finish must not leave litter in the
// directory the next reader lists.
func TestBlessReportsARecordItCouldNotWrite(t *testing.T) {
	dir := newRepo(t, true)
	// A non-empty directory where the record belongs: the rename over it
	// cannot succeed on any platform.
	if err := os.MkdirAll(filepath.Join(Path(dir), "in-the-way"), 0o755); err != nil {
		t.Fatalf("put a directory where the record belongs: %v", err)
	}

	tree, err := Bless(context.Background(), dir)
	if err == nil {
		t.Error("Bless reported a blessing it did not record")
	}
	if tree != "" {
		t.Errorf("Bless = %q after a failed write, want \"\" — a caller cannot tell this from a recorded tree", tree)
	}

	recordDir := filepath.Dir(Path(dir))
	entries, err := os.ReadDir(recordDir)
	if err != nil {
		t.Fatalf("read %s: %v", recordDir, err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(Path(dir)) {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s holds %v, want only the path that was in the way — the failed write left its temporary behind", recordDir, names)
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
		// A record that reached a Windows checkout through a translating
		// filter, or a hand that edited it there. The carriage return must not
		// turn a blessed tree into "nothing blessed this", which would refuse
		// every commit on that machine with a record that looks correct to
		// anyone reading it.
		{"a tree id with a carriage return", valid + "\r\n", valid},
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

// THE JOIN IS THE CASE THE PACKAGE WAS BUILT FOR, AND IT SPANS TWO CHECKOUTS.
// A runner recognises that the same work over the same tree has already been
// done, and the green result it hands back was measured somewhere else
// entirely — a different clone, on a different host, with its own object
// database and its own history. The blessing has to be recorded HERE, from an
// id computed THERE, so the id may carry nothing checkout-local: not a path,
// not a git directory, not a commit. Every test above asks one checkout both
// questions, and a tree id that quietly depended on where it was computed
// would agree with itself in all of them and make every real join a no-op.
func TestATreeIDFromAnotherCheckoutBlessesThisOne(t *testing.T) {
	measured := newRepo(t, true) // where the green result was produced
	here := newRepo(t, true)     // the checkout asking to be blessed

	// Identical content, reached differently: committed there, staged and
	// unstaged here. The tree is about content, so none of that may show.
	for _, dir := range []string{measured, here} {
		writeFile(t, filepath.Join(dir, "kept.txt"), "changed\n")
		writeFile(t, filepath.Join(dir, "fresh.txt"), "new\n")
		writeFile(t, filepath.Join(dir, "ignored", "junk"), "junk\n")
	}
	gitIn(t, measured, "add", "-A")
	gitIn(t, measured, "commit", "-q", "-m", "the content the result was measured over")
	gitIn(t, here, "add", "fresh.txt")

	result, err := TreeID(context.Background(), measured)
	if err != nil {
		t.Fatalf("TreeID where the result was measured: %v", err)
	}

	ok, err := BlessIfEqual(context.Background(), here, result)
	if err != nil {
		t.Fatalf("BlessIfEqual from a joined result: %v", err)
	}
	if !ok {
		mine, err := TreeID(context.Background(), here)
		if err != nil {
			t.Fatalf("TreeID here: %v", err)
		}
		t.Fatalf("a result measured over identical content did not bless this checkout: there %s, here %s", result, mine)
	}

	// The join must leave the checkout in the state a run would have: what a
	// commit here is about to record is what the record blesses.
	staged := stagedByGit(t, here)
	record, err := Check(here)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !Blesses(record, staged) {
		t.Errorf("the record %q does not bless the tree a commit here would record %q", record, staged)
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
