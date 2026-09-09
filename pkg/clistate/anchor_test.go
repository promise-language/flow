package clistate_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
)

// ---------------------------------------------------------------------------
// Where the state directory lands, measured on a real process.
//
// Every other test in this package sets FLOW_DIR, so none of them exercises the
// derivation at all — and the derivation is the defect. `.flow` was a bare
// relative name resolved against the process working directory, so a `resolve`
// started from a parent directory wrote its claim state THERE, and the next
// invocation from inside the checkout found no active claim and left the lease
// on the item orphaned. Nothing about that is visible in-process: the answer
// depends on where the binary lives and where it was started, which only a
// subprocess can vary.
//
// The helper below is this test binary, COPIED into a fake checkout and run
// from a decoy one. A copy rather than a symlink because os.Executable resolves
// /proc/self/exe on Linux — a symlinked test binary would report the original
// path, and the fake checkout would never be reached.
// ---------------------------------------------------------------------------

// TestAnchorHelperProcess is the child. It reports where clistate believes the
// state directory is and then writes a claim there, so the tests can assert
// against a file that actually exists rather than against a string.
func TestAnchorHelperProcess(t *testing.T) {
	if os.Getenv("FLOW_ANCHOR_HELPER") != "1" {
		t.Skip("not the helper process")
	}
	dir, err := clistate.Dir()
	if err != nil {
		fmt.Println("ERR " + err.Error())
		return
	}
	if err := clistate.Save(flow.Claim{OrchestratorName: "fake"}); err != nil {
		fmt.Println("ERR " + err.Error())
		return
	}
	fmt.Println("DIR " + dir)
}

// installExe puts a runnable copy of this test binary at dst.
func installExe(t *testing.T, dst string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate this test binary: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	// A hard link where the filesystem allows it: the binary is tens of
	// megabytes and these tests install it more than once.
	if err := os.Link(exe, dst); err == nil {
		return
	}
	src, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// runHelper runs exe from the working directory cwd, with HOME pointed at home
// and FLOW_DIR unset, and returns what the child said the state directory was —
// or the error it refused with, prefixed "ERR ".
func runHelper(t *testing.T, exe, cwd, home string) string {
	t.Helper()
	cmd := exec.Command(exe, "-test.run=^TestAnchorHelperProcess$")
	cmd.Dir = cwd
	env := []string{}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "FLOW_DIR=") || strings.HasPrefix(kv, "HOME=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "FLOW_ANCHOR_HELPER=1", "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper run from %s: %v\n%s", cwd, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "DIR ") || strings.HasPrefix(line, "ERR ") {
			return line
		}
	}
	t.Fatalf("helper run from %s said nothing about the state directory:\n%s", cwd, out)
	return ""
}

// anchorTree lays out a checkout to install the binary in, a decoy checkout to
// run it from, and a home directory that is neither.
func anchorTree(t *testing.T) (root, decoy, home string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX layout")
	}
	// CanonicalPath, because the derived answer the child reports is canonical
	// by contract — one location has one spelling — so the path these tests
	// build their expectation from has to be too. On macOS /var is a symlink to
	// /private/var, and an unresolved tempdir would describe a different tree
	// than the one the child names. The production helper is what says this is
	// the same canonicalization rather than a test's own idea of one.
	tmp := flow.CanonicalPath(t.TempDir())
	root, decoy, home = filepath.Join(tmp, "checkout"), filepath.Join(tmp, "decoy"), filepath.Join(tmp, "home")
	// The decoy is a checkout too, and it is where the process is started. A
	// state dir read off the working directory would resolve HERE and look
	// entirely reasonable doing it — which is what made the defect quiet.
	for _, d := range []string{filepath.Join(root, ".git"), filepath.Join(decoy, ".git"), home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, decoy, home
}

// The claim state belongs to the checkout the binary lives in, wherever the
// operator was standing when they ran it.
func TestStateDirIsTheBinarysCheckoutNotTheWorkingDirectory(t *testing.T) {
	root, decoy, home := anchorTree(t)
	exe := filepath.Join(root, "bin", "prog")
	installExe(t, exe)

	if got, want := runHelper(t, exe, decoy, home), "DIR "+filepath.Join(root, ".flow"); got != want {
		t.Fatalf("run from the decoy checkout said %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(root, ".flow", "active.json")); err != nil {
		t.Errorf("no claim state in the binary's own checkout: %v", err)
	}
	// And none in the directory the operator happened to be standing in: a
	// claim written there is a lease nothing local points at.
	if _, err := os.Stat(filepath.Join(decoy, ".flow")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("claim state was written into the working directory %s; stat err = %v", decoy, err)
	}
}

// A binary reached through a PATH symlink and the same binary reached directly
// answer ONE state directory. os.Executable resolves symlinks on some platforms
// and not others, so without the explicit resolution the answer would depend on
// which platform ran it — and on the ones that do not resolve, an
// ~/bin/issue → ~/prog/flow/bin/issue install would anchor on ~/bin, which is
// refused outright.
func TestStateDirIsTheSameThroughAPathSymlink(t *testing.T) {
	root, decoy, home := anchorTree(t)
	exe := filepath.Join(root, "bin", "prog")
	installExe(t, exe)

	link := filepath.Join(filepath.Dir(root), "pathbin", "prog")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, link); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}

	direct := runHelper(t, exe, decoy, home)
	viaLink := runHelper(t, link, decoy, home)
	if viaLink != direct {
		t.Errorf("through the symlink %s the state dir was %q, want %q — one binary, one state directory",
			link, viaLink, direct)
	}
}

// A binary installed under the home directory has no checkout to anchor to, and
// says so. Taking $HOME would give every binary installed that way ONE state
// directory to share, and a home that happens to be a dotfiles repo would make
// it look deliberate.
func TestStateDirRefusesToAnchorOnTheHomeDirectory(t *testing.T) {
	_, decoy, home := anchorTree(t)
	// The tempting case: home is itself a checkout, and the binary sits in the
	// conventional install location under it.
	if err := os.MkdirAll(filepath.Join(home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(home, "go", "bin", "prog")
	installExe(t, exe)

	got := runHelper(t, exe, decoy, home)
	if !strings.HasPrefix(got, "ERR ") {
		t.Fatalf("a binary installed at %s answered %q, want a refusal", exe, got)
	}
	if _, err := os.Stat(filepath.Join(home, ".flow")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("claim state was written into the home directory; stat err = %v", err)
	}
}

// And it refuses that home however $HOME is SPELLED. The guard is a string
// comparison against a walk that descends from the resolved executable, while
// $HOME arrives verbatim: a home reached through a symlinked component —
// /home → /mnt/home, or any macOS home under /var → /private/var — is spelled
// one way on each side, the comparison never matches, and the guard silently
// does not fire. What follows is not a refusal but the walk continuing into the
// home directory and adopting it: the ArenaId shared by every binary installed
// that way, which is precisely what the guard exists to refuse.
//
// The fixture's temp root is deliberately NOT resolved here. Handing the child
// an already-canonical $HOME is what hides this.
func TestStateDirRefusesAHomeDirectoryReachedThroughASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX layout")
	}
	tmp := t.TempDir()
	// A real subtree, and a sibling symlink pointing at it. The binary is
	// installed at its real path, the way an install puts it there; only the
	// $HOME the child is handed goes through the link.
	realTree := filepath.Join(tmp, "real")
	home := filepath.Join(realTree, "home")
	decoy := filepath.Join(realTree, "decoy")
	for _, d := range []string{filepath.Join(home, ".git"), filepath.Join(decoy, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(realTree, link); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}
	exe := filepath.Join(home, "go", "bin", "prog")
	installExe(t, exe)

	got := runHelper(t, exe, decoy, filepath.Join(link, "home"))
	if !strings.HasPrefix(got, "ERR ") {
		t.Fatalf("a binary installed at %s, with $HOME spelled %s, answered %q, want a refusal",
			exe, filepath.Join(link, "home"), got)
	}
	if _, err := os.Stat(filepath.Join(home, ".flow")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("claim state was written into the home directory; stat err = %v", err)
	}
}
