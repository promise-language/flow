package github

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The failure #286 reports, at the level an operator meets it. Running the
// reference binary by path from outside its checkout answered
//
//	resolve repo: git remote get-url origin: exit status 128
//	(stderr=fatal: not a git repository ...)
//
// while the same binary, same arguments, one `cd` later worked. Nothing about
// the request changed: the binary knew where it lived and declined to use it.
//
// Every other test in this change is about a function. This one is about the
// binary, because that is where the four defects compose — a unit test of
// resolveWorktreeDir cannot catch a consumer that resolves a path some other
// way, and one that did is exactly how this class came back after #21 settled
// it.
//
// The DECOY is what gives the test teeth. It is a second checkout, with an
// origin the real one lacks, and it is the working directory of the first run:
// a binary reading its cwd resolves decoy/decoy there and gets further,
// producing different output from the second run. A binary anchored to itself
// answers identically from both, about its own checkout.
func TestIssueBinaryAnswersTheSameFromAnyWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX layout")
	}
	for _, bin := range []string{"go", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not on PATH", bin)
		}
	}

	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	decoy := filepath.Join(tmp, "decoy")
	exe := filepath.Join(root, "bin", "issue")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}

	// Build the reference binary INTO the checkout it is meant to belong to:
	// <root>/bin/issue, which is where every flow binary is built.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source, so cannot find the module to build")
	}
	build := exec.Command("go", "build", "-o", exe, "github.com/promise-language/flow/examples/issue")
	build.Dir = filepath.Dir(thisFile)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./examples/issue: %v\n%s", err, out)
	}

	// The binary's own checkout has NO origin — so resolving the repository
	// there fails, loudly and identically, wherever the process was started.
	// The decoy has one, so a cwd-reading binary would resolve it and diverge.
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatal(err)
	}
	git(root, "init")
	git(decoy, "init")
	git(decoy, "remote", "add", "origin", "https://github.com/decoy/decoy.git")

	run := func(cwd string) string {
		t.Helper()
		cmd := exec.Command(exe, "status")
		cmd.Dir = cwd
		// A state dir and a home of its own: the run must not touch the
		// operator's, and neither is what the binary anchors on.
		cmd.Env = append(os.Environ(),
			"FLOW_DIR="+filepath.Join(tmp, "state"),
			"HOME="+filepath.Join(tmp, "home"),
			"GITHUB_TOKEN=",
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("issue status run from %s succeeded; its own checkout has no origin, "+
				"so it must fail:\n%s", cwd, out)
		}
		return string(out)
	}

	fromOutside := run(decoy)
	fromInside := run(root)

	if fromOutside != fromInside {
		t.Errorf("the same binary answered differently depending on where it was run from.\n"+
			"from the decoy checkout:\n%s\nfrom its own checkout:\n%s", fromOutside, fromInside)
	}
	if strings.Contains(fromOutside, "decoy") {
		t.Errorf("the run from the decoy checkout resolved the DECOY's repository — "+
			"it read the process working directory:\n%s", fromOutside)
	}
	if strings.Contains(fromOutside, "not a git repository") {
		t.Errorf("the run from the decoy checkout looked for a repository in the working "+
			"directory rather than in its own checkout:\n%s", fromOutside)
	}
	// What it must fail on: its own checkout's missing origin.
	if !strings.Contains(fromOutside, "origin") {
		t.Errorf("the failure does not name the missing origin of the binary's own "+
			"checkout:\n%s", fromOutside)
	}
}
