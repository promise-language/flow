package github

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
)

// A gate's timeout is the only thing standing between a wedged gate and a
// runner that never returns, and a zero duration is not "no deadline" here —
// it is a deadline that has already expired. Left unfilled, every gate in
// every project reports OutcomeTimedOut without a process ever being spawned,
// and the outcome that means "retry this unchanged" is handed out for a
// configuration that will produce it again forever.
func TestConfigGateTimeoutIsFilledIn(t *testing.T) {
	got := Config{}.withDefaults()
	if got.GateTimeout <= 0 {
		t.Fatalf("GateTimeout = %v — an unset timeout is one that has already expired", got.GateTimeout)
	}

	// The declared value is a project's to set, and withDefaults must not
	// overwrite one that was.
	declared := Config{GateTimeout: 3 * time.Second}.withDefaults()
	if declared.GateTimeout != 3*time.Second {
		t.Errorf("GateTimeout = %v, want the declared 3s", declared.GateTimeout)
	}
}

// The default VerifyCmd must point at the compiled binary, not a shell
// script that no longer exists. A consumer that sets no VerifyCmd gets this
// default, so it must be a command that can actually run.
func TestConfigVerifyCmdDefaultIsTheBinary(t *testing.T) {
	got := Config{}.withDefaults()
	if len(got.VerifyCmd) != 1 || got.VerifyCmd[0] != "bin/verify" {
		t.Errorf("default VerifyCmd = %v, want [bin/verify]", got.VerifyCmd)
	}

	// A declared value must not be overwritten.
	declared := Config{VerifyCmd: []string{"make", "check"}}.withDefaults()
	if len(declared.VerifyCmd) != 2 || declared.VerifyCmd[0] != "make" {
		t.Errorf("VerifyCmd = %v, want the declared [make check]", declared.VerifyCmd)
	}
}

// The default sits UNDER the step's own budget, which is what makes a wedged
// gate get caught by its own deadline and reported as OutcomeTimedOut. Raise
// it past the step timeout and the step dies first: the gate then has no
// outcome at all, and the failure is attributed to the step rather than to the
// wait it was actually waiting on.
func TestConfigGateTimeoutFitsInsideAStep(t *testing.T) {
	gate := Config{}.withDefaults().GateTimeout
	step := flow.DefaultStepBudget().Timeout
	if gate >= step {
		t.Errorf("default GateTimeout %v >= default step timeout %v — the step dies before the gate's own deadline fires", gate, step)
	}
}

// WorktreeDir is the one field withDefaults must NOT fill in. `"."` is not a
// location — it is wherever the process happened to be started — and the moment
// it is stored here every consumer of the field inherits it: the arena
// identity, the claim state, gate discovery and the verify command all
// silently became the operator's working directory (#286). New resolves the
// field instead, deriving or refusing.
func TestConfigWorktreeDirIsNotDefaulted(t *testing.T) {
	if got := (Config{}).withDefaults().WorktreeDir; got != "" {
		t.Errorf("WorktreeDir = %q, want it left empty for New to resolve", got)
	}
	// A declared value must not be overwritten either.
	declared := Config{WorktreeDir: "/w/checkout"}.withDefaults()
	if declared.WorktreeDir != "/w/checkout" {
		t.Errorf("WorktreeDir = %q, want the declared /w/checkout", declared.WorktreeDir)
	}
}

// A relative WorktreeDir is REFUSED, not resolved. Resolving one would re-import
// the process working directory through the back door — the caller would have
// configured a path and got wherever the operator stood.
func TestNewRefusesARelativeWorktreeDir(t *testing.T) {
	_, err := New(Config{
		WorktreeDir: "some/checkout",
		Owner:       "acme",
		Repo:        "widget",
		BinaryName:  "issue",
		Token:       "fake-token",
	})
	if err == nil {
		t.Fatal("New accepted a relative WorktreeDir")
	}
	if !strings.Contains(err.Error(), "WorktreeDir") {
		t.Errorf("err = %v, want it to name the field the caller has to fix", err)
	}
	// Refused BEFORE git is spawned: the anchor is resolved first, so a bad one
	// costs no subprocess and no network round trip.
	if strings.Contains(err.Error(), "git ") || strings.Contains(err.Error(), "resolve repo") {
		t.Errorf("err = %v, want the refusal to precede any git invocation", err)
	}

	// And refused even when the relative path EXISTS where the process happens
	// to be standing, which is what the ordering guards. The canonicalization
	// resolves symlinks, and resolving a relative path resolves it against the
	// process working directory — so a refusal that came second would answer
	// "wherever the operator stood/bin" for a caller who configured "bin", the
	// exact ambient dependency the refusal exists to remove.
	t.Chdir(t.TempDir())
	if err := os.Mkdir("bin", 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveWorktreeDir("bin"); err == nil {
		t.Errorf("resolveWorktreeDir(\"bin\") = %q, want a refusal — an existing relative path is still the operator's cwd", got)
	}
}

// An absolute WorktreeDir is the caller's answer and is kept exactly: New
// resolves the repository in it, and every later reader — arena(), the state
// dir, gate discovery — inherits that one value.
func TestNewKeepsAnAbsoluteWorktreeDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	// The configured spelling goes in raw; the canonical one is what every
	// reader gets back. On macOS a t.TempDir sits under /var, a symlink to
	// /private/var, so these are two different strings for one directory —
	// and the ArenaId must be the same one either way.
	dir := t.TempDir()
	want := flow.CanonicalPath(dir)
	for _, args := range [][]string{
		{"init"},
		{"remote", "add", "origin", "https://github.com/acme/widget.git"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	b, err := New(Config{WorktreeDir: dir, BinaryName: "issue", Token: "fake-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.cfg.WorktreeDir != want {
		t.Errorf("WorktreeDir = %q, want the configured %q canonicalized to %q", b.cfg.WorktreeDir, dir, want)
	}
	if b.cfg.Owner != "acme" || b.cfg.Repo != "widget" {
		t.Errorf("resolved %s/%s, want acme/widget from the worktree's own origin", b.cfg.Owner, b.cfg.Repo)
	}
	// The arena identity is that same path, not a second derivation of it.
	if got := string(b.arena().Id); got != want {
		t.Errorf("ArenaId = %q, want the worktree %q", got, want)
	}
}

// ONE WORKTREE IS ONE ARENA, however it was spelled. The ArenaId is the
// worktree path compared by string equality — the fingerprint on the item is a
// digest of it — so "/w/repo/", "/w/./repo" and "/w/repo" must not be three
// arenas. That is the same "one checkout, two arenas" error #286 reports,
// arriving through a spelling rather than through a cwd: the exclusion that
// keeps one item to one arena would not fire, and two runs would proceed on the
// same item.
//
// filepath.Abs down in arena() used to normalize this incidentally. Removing it
// (rightly — it was also what turned "." into the operator's cwd) took the
// normalization with it, so it belongs here, where the location is decided.
func TestResolveWorktreeDirCanonicalizesOneWorktreeToOneArena(t *testing.T) {
	const want = "/w/repo"
	// None of these exists, so this half also pins the fallback: a worktree
	// with nothing resolvable above it is CLEANED, not refused — a caller may
	// name one it is about to create.
	for _, spelling := range []string{"/w/repo", "/w/repo/", "/w/./repo", "/w/sibling/../repo", "/w//repo"} {
		got, err := resolveWorktreeDir(spelling)
		if err != nil {
			t.Fatalf("resolveWorktreeDir(%q): %v", spelling, err)
		}
		if got != want {
			t.Errorf("resolveWorktreeDir(%q) = %q, want %q — one worktree spelled two ways is two arenas",
				spelling, got, want)
		}
	}

	// And the identity that reaches the item follows: one fingerprint, not one
	// per spelling.
	trailing := (&Orchestrator{cfg: Config{WorktreeDir: mustResolveWorktree(t, "/w/repo/")}}).arenaFingerprint()
	bare := (&Orchestrator{cfg: Config{WorktreeDir: mustResolveWorktree(t, "/w/repo")}}).arenaFingerprint()
	if trailing != bare {
		t.Errorf("flow:arena fingerprints differ (%s vs %s) for one worktree spelled two ways", trailing, bare)
	}

	// The spelling a lexical table cannot reach: a SYMLINK. One worktree
	// reached through a symlinked parent and reached directly must be one
	// arena — the derived route resolves symlinks, so a configured route that
	// only cleaned would hand the same checkout two ArenaIds depending on which
	// way in the caller took.
	t.Run("through a symlinked parent", func(t *testing.T) {
		tmp := t.TempDir()
		direct := filepath.Join(tmp, "real", "repo")
		if err := os.MkdirAll(direct, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(tmp, "real"), filepath.Join(tmp, "link")); err != nil {
			t.Skipf("symlinks are unavailable here: %v", err)
		}
		viaLink := filepath.Join(tmp, "link", "repo")

		if got, want := mustResolveWorktree(t, viaLink), mustResolveWorktree(t, direct); got != want {
			t.Errorf("resolveWorktreeDir(%q) = %q, want %q — one worktree reached two ways is two arenas",
				viaLink, got, want)
		}
		linked := (&Orchestrator{cfg: Config{WorktreeDir: mustResolveWorktree(t, viaLink)}}).arenaFingerprint()
		plain := (&Orchestrator{cfg: Config{WorktreeDir: mustResolveWorktree(t, direct)}}).arenaFingerprint()
		if linked != plain {
			t.Errorf("flow:arena fingerprints differ (%s vs %s) for one worktree reached through a symlink",
				linked, plain)
		}
	})
}

// An absolute worktree that DOES NOT EXIST YET is accepted, not refused — a
// caller may name one it is about to create — and it answers THE SAME STRING
// once it does exist.
//
// That second half is the load-bearing one, and nothing downstream supplies it:
// with Owner and Repo both configured New never touches the filesystem, so this
// path reaches arena() as the ArenaId the item is claimed under, and Claim
// publishes flow:arena:<fingerprint> without going near the worktree either. An
// answer that changed when the directory appeared would be one worktree and two
// arenas — the whole error — arriving by the clock instead of by the route.
func TestResolveWorktreeDirAcceptsAnAbsoluteWorktreeThatDoesNotExistYet(t *testing.T) {
	// Under a symlinked ancestor, which is where the difference shows: on macOS
	// a t.TempDir sits under /var, a symlink to /private/var.
	tmp := t.TempDir()
	absent := filepath.Join(tmp, "not", "created", "yet")
	before, err := resolveWorktreeDir(absent + string(filepath.Separator))
	if err != nil {
		t.Fatalf("resolveWorktreeDir(%q): %v — a worktree the caller is about to create is not a bad path", absent, err)
	}
	if !filepath.IsAbs(before) || filepath.Clean(before) != before {
		t.Errorf("resolveWorktreeDir(%q) = %q, want an absolute, cleaned path", absent, before)
	}

	if err := os.MkdirAll(absent, 0o755); err != nil {
		t.Fatal(err)
	}
	after := mustResolveWorktree(t, absent)
	if after != before {
		t.Errorf("resolveWorktreeDir(%q) = %q before the worktree existed and %q after — one worktree, two arenas",
			absent, before, after)
	}
	if pre, post := (&Orchestrator{cfg: Config{WorktreeDir: before}}).arenaFingerprint(),
		(&Orchestrator{cfg: Config{WorktreeDir: after}}).arenaFingerprint(); pre != post {
		t.Errorf("flow:arena fingerprints differ (%s vs %s) for one worktree before and after it was created", pre, post)
	}
}

func mustResolveWorktree(t *testing.T, dir string) string {
	t.Helper()
	got, err := resolveWorktreeDir(dir)
	if err != nil {
		t.Fatalf("resolveWorktreeDir(%q): %v", dir, err)
	}
	return got
}

// FAIL CLOSED when there is nothing to derive from. An empty WorktreeDir that
// survived New would BE the ArenaId, since arena() uses the field as it stands
// — one empty identity shared by every arena on every host, which is the "two
// checkouts are one arena" half of #286 at its widest. The old default made
// this unreachable by answering "." to every question; there is no default now,
// so the refusal is the whole of the answer.
func TestNewRefusesAWorktreeItCannotDerive(t *testing.T) {
	if _, err := flow.DeriveArenaRoot(); err == nil {
		t.Skip("this test binary lives inside a checkout, so the underivable case cannot be exercised here")
	}
	_, err := New(Config{Owner: "acme", Repo: "widget", BinaryName: "issue", Token: "fake-token"})
	if err == nil {
		t.Fatal("New accepted a worktree it could not derive — every arena would share one empty ArenaId")
	}
	if !strings.Contains(err.Error(), "worktree") {
		t.Errorf("err = %v, want it to name the worktree it could not locate", err)
	}
}
