// Package verifiedtree is the tree blessing: how the tree hash a green
// `verify` records is computed, how the record is written, and how a party
// reading it decides whether a tree is blessed.
// docs/gates-and-commands.md, "A green `verify` blesses the tree it verified",
// states the rules; this package is the one implementation of them.
//
// IT EXISTS BECAUSE THE FACT HAS MORE THAN ONE READER AND NO READER MAY TRUST
// ANOTHER'S ARITHMETIC. A project's `bin/verify` writes the record, a commit
// guard refuses any commit whose staged tree differs from it, and a party
// deciding whether the gate needs to run at all compares it against the
// worktree. Those are three programs in three repositories answering one
// question — was this tree verified? — and while each computed its own tree id
// they could disagree with nothing able to reconcile them. A runner that had
// genuinely verified a tree could not record the fact, because nothing
// guaranteed its tree id was the guard's tree id.
//
// THE POLICY STAYS WITH THE CALLER. This package knows nothing about verify
// pipelines, commits, rounds or executions: it takes a checkout root and
// answers about trees and the record. Whether a missing checkout is a failure
// or a reported no-op, what a refusal says to a human, and when to bless are
// the caller's, and each caller has a different answer.
package verifiedtree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/promise-language/forge/primitives"
)

// ErrNotACheckout is returned for a root that is not inside a git checkout.
// There is no tree to hash there and no commit to gate, so a caller decides
// what that means — a verify outside a checkout reports it and carries on,
// while a guard has nothing to guard.
var ErrNotACheckout = errors.New("not a git checkout")

// ErrRecordNotIgnored refuses a bless in a checkout that does not ignore the
// record's path. An unignored record is inside its own subject: writing it
// changes the tree it names, so the tree it blesses is one that stops existing
// the moment it is blessed, and every commit is then refused with nothing the
// caller can do about it. Refusing by name at the bless is the only moment the
// cause is still visible.
var ErrRecordNotIgnored = errors.New("the verified-tree record's path is not ignored by this checkout")

// Path is the record's absolute path in the checkout rooted at root. The
// record's location is one constant, held in forge's primitives and imported
// by every end of the contract rather than spelled at each — see
// primitives.VerifiedTreeRecord.
func Path(root string) string {
	return filepath.Join(root, filepath.FromSlash(primitives.VerifiedTreeRecord))
}

// TreeID is the git tree object id of what `git add -A` would stage in the
// checkout rooted at root: tracked changes plus untracked-but-not-ignored
// files. It is the content a blessing is about, and it is also what a party
// holding a gate result keys that result to — content rather than the commit,
// because a step that amends as it works produces a new commit per attempt
// over an identical tree.
//
// The real index is never touched: the id is computed over a COPY of it, so
// this is safe to run beside a live worktree and beside a person at a
// terminal. The copy is the seed rather than an empty index or HEAD, because
// that is the tracked set the real `git add -A` starts from and ignore rules
// apply only to untracked paths. Any other seed gets the cases where the
// tracking state and the ignore rules disagree wrong — an empty seed drops a
// tracked-but-ignored file, and a HEAD seed both drops one force-added but not
// yet committed and keeps one just `git rm --cached`ed.
//
// A zero-length index file is not copied. It is not the same as an absent one:
// git creates the index it needs, but seeded with zero bytes it refuses every
// command with "index file smaller than expected", so copying one turns a
// recoverable state into a hard failure.
//
// `git add -A` writes blobs for changed and untracked content into the object
// database. They are unreferenced, so they cost a little disk until the next
// gc — the same trade `git stash create` makes.
func TreeID(ctx context.Context, root string) (string, error) {
	if err := requireCheckout(ctx, root); err != nil {
		return "", err
	}
	return treeID(ctx, root)
}

// StagedTreeID is `git write-tree` over root's REAL index: the tree id the
// commit about to be made would record. It is what a commit-time check
// compares the record against, where TreeID is what a party deciding whether
// the gate needs to run at all compares it against.
//
// An index that cannot be written as a tree — an unmerged one, mid-conflict —
// is an error rather than an empty answer. A check that cannot run must fail
// rather than pass.
func StagedTreeID(ctx context.Context, root string) (string, error) {
	if err := requireCheckout(ctx, root); err != nil {
		return "", err
	}
	return git(ctx, root, "", "write-tree")
}

// Check reports the tree the record blesses, or "" when nothing does.
//
// Absent, blank and malformed are one answer deliberately. They are different
// histories — no verify has ever run, one is in flight having cleared the
// record before its first step, a write was interrupted — and they mean the
// same thing to every reader: nothing has blessed this tree. A caller that
// could tell them apart would have to decide what to do about each, and the
// only safe answer for all three is the one answer.
func Check(root string) (string, error) {
	raw, err := os.ReadFile(Path(root))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return readRecord(raw), nil
}

// Blesses reports whether a record read by Check blesses tree.
//
// IT IS NOT `==`, AND THAT IS THE WHOLE POINT. Both sides are "" for a reason:
// the record is "" when nothing has blessed the tree, and a caller's tree id is
// "" when computing it failed. Compared with `==` those two failures agree, and
// a checkout nothing has verified reads as blessed by a caller that could not
// even ask what its tree was. The empty-means-nothing rule lives here, once.
func Blesses(record, tree string) bool {
	return record != "" && record == tree
}

// Bless records root's current tree id and returns it, so a green result is
// recorded as the fact "a green verify completed on exactly this content".
//
// The write is atomic — a temp file in the record's own directory, renamed
// over — so no reader ever sees a half-written record. The record's directory
// is created if it is missing.
func Bless(ctx context.Context, root string) (string, error) {
	return bless(ctx, root, "")
}

// BlessIfEqual records root's current tree id only when it equals want, and
// reports whether it did.
//
// IT IS WHAT LETS A PARTY HOLDING A GREEN RESULT RECORD IT WITHOUT RUNNING
// ANYTHING. A runner that joins an earlier execution over tree X has a genuine
// green result for X and nothing else; blessing unconditionally would record
// whatever the checkout happens to hold now, which may have moved on. Naming
// the tree the result is about makes the join leave the checkout in the state a
// run would have, and makes it impossible for a join to bless content nobody
// verified.
//
// A mismatch is not an error — the checkout simply moved — and it leaves any
// existing record exactly as it was. An empty want is refused: it is a caller
// passing on a tree id it failed to compute, and the unconditional form is
// Bless.
func BlessIfEqual(ctx context.Context, root, want string) (bool, error) {
	if want == "" {
		return false, errors.New("verifiedtree: BlessIfEqual needs the tree the result is about; the unconditional form is Bless")
	}
	blessed, err := bless(ctx, root, want)
	return blessed != "", err
}

// Clear removes the record. A verify calls it before its first step, so a run
// that dies part way leaves nothing blessed and a verify in flight blesses
// nothing.
//
// An absent record is success: the state Clear exists to produce is the state
// it is already in.
func Clear(root string) error {
	if err := os.Remove(Path(root)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// bless is the one implementation behind both blessing entry points. An empty
// want is the unconditional form; a non-empty one records only a tree equal to
// it. The returned tree id is "" exactly when want was given and did not match.
//
// The ignore check comes before the tree is computed, so a checkout that cannot
// hold a record is refused before anything is spent on one.
func bless(ctx context.Context, root, want string) (string, error) {
	if err := requireCheckout(ctx, root); err != nil {
		return "", err
	}
	if err := requireRecordIgnored(ctx, root); err != nil {
		return "", err
	}
	tree, err := treeID(ctx, root)
	if err != nil {
		return "", err
	}
	if want != "" && tree != want {
		return "", nil
	}
	if err := writeRecord(root, tree); err != nil {
		return "", err
	}
	return tree, nil
}

// treeID is TreeID with the checkout already established.
func treeID(ctx context.Context, root string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "verified-tree-")
	if err != nil {
		return "", fmt.Errorf("creating temp index dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	index := filepath.Join(tmpDir, "index")

	// --git-path rather than a guess at <git-dir>/index: a linked worktree
	// keeps its own index under .git/worktrees/<name>/, and hashing the
	// superproject's would describe a tree nobody is working on.
	realIndex, err := git(ctx, root, "", "rev-parse", "--git-path", "index")
	if err != nil {
		return "", fmt.Errorf("locating the index: %w", err)
	}
	if !filepath.IsAbs(realIndex) {
		realIndex = filepath.Join(root, realIndex)
	}
	data, err := os.ReadFile(realIndex)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("seeding a copy of the index: %w", err)
	}
	if len(data) > 0 {
		if err := os.WriteFile(index, data, 0o600); err != nil {
			return "", fmt.Errorf("seeding a copy of the index: %w", err)
		}
	}

	if _, err := git(ctx, root, index, "add", "-A"); err != nil {
		return "", fmt.Errorf("staging into a copy of the index: %w", err)
	}
	tree, err := git(ctx, root, index, "write-tree")
	if err != nil {
		return "", fmt.Errorf("computing the tree id: %w", err)
	}
	return tree, nil
}

// readRecord reads the bytes of the record file as a tree id, or "" when they
// are not one. The format is one git tree object id, newline terminated, and
// nothing else; anything else is a record no verify wrote whole.
func readRecord(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if !isObjectID(s) {
		return ""
	}
	return s
}

// isObjectID reports whether s is a lowercase hex git object id, at either
// hash length. Checking the shape is what keeps a torn or hand-edited record
// from being compared as though it were a tree: a reader that trimmed and
// returned whatever it found would hand its caller a string that means
// nothing and let it be compared against one that means something.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// writeRecord writes tree to the record atomically: a temp file in the
// record's own directory, then a rename. Same directory so the rename cannot
// cross a filesystem, which is the one way it stops being atomic.
func writeRecord(root, tree string) error {
	path := Path(root)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".verified-tree-*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(tree + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// requireCheckout turns "root is not in a git checkout" into ErrNotACheckout,
// so a caller can tell it apart from a git that failed for any other reason.
func requireCheckout(ctx context.Context, root string) error {
	if _, err := git(ctx, root, "", "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("%s: %w", root, ErrNotACheckout)
	}
	return nil
}

// requireRecordIgnored refuses when the checkout does not ignore the record's
// path. `git check-ignore` consults the index as well as the ignore rules, so
// a record that is TRACKED is reported as not ignored — which is the answer
// wanted: a tracked record is inside the tree it would describe just as surely
// as an unignored untracked one.
func requireRecordIgnored(ctx context.Context, root string) error {
	cmd := exec.CommandContext(ctx, "git", "check-ignore", "-q", "--", primitives.VerifiedTreeRecord)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return fmt.Errorf("%s: %w", primitives.VerifiedTreeRecord, ErrRecordNotIgnored)
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return fmt.Errorf("git check-ignore: %s", msg)
	}
	return fmt.Errorf("git check-ignore: %w", err)
}

// git runs one git command in dir and returns its trimmed stdout, with
// GIT_INDEX_FILE pointed at indexFile when one is given. Stderr is captured
// into the error rather than passed through: this is a library, and a caller
// deciding how to report a failure cannot do that for output that already
// reached the terminal.
func git(ctx context.Context, dir, indexFile string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if indexFile != "" {
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+indexFile)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %s", args[0], msg)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}
