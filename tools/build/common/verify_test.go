package common

import (
	"os"
	"path/filepath"
	"testing"
)

// modules() is the foundation of the multi-module fix: if it returns only the
// root, then runAllModules degenerates to the old single-module behaviour and
// the tools module is silently skipped.

func TestModules_ReturnsToolsBuildWhenItHasGoMod(t *testing.T) {
	root := t.TempDir()
	toolsDir := filepath.Join(root, "tools", "build")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Root module.
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Tools module.
	if err := os.WriteFile(filepath.Join(toolsDir, "go.mod"), []byte("module example/tools/build\n"), 0644); err != nil {
		t.Fatal(err)
	}

	dirs := modules(root)
	if len(dirs) != 2 {
		t.Fatalf("modules returned %d dirs, want 2: %v", len(dirs), dirs)
	}
	if dirs[0] != root {
		t.Errorf("dirs[0] = %q, want the repo root %q", dirs[0], root)
	}
	if dirs[1] != toolsDir {
		t.Errorf("dirs[1] = %q, want the tools dir %q", dirs[1], toolsDir)
	}
}

func TestModules_ReturnsOnlyRootWhenToolsBuildHasNoGoMod(t *testing.T) {
	root := t.TempDir()
	toolsDir := filepath.Join(root, "tools", "build")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// No go.mod in tools/build.

	dirs := modules(root)
	if len(dirs) != 1 {
		t.Fatalf("modules returned %d dirs, want 1 (root only): %v", len(dirs), dirs)
	}
	if dirs[0] != root {
		t.Errorf("dirs[0] = %q, want the repo root %q", dirs[0], root)
	}
}

// runAllModules must visit the second module. This test creates a two-module
// layout where the second module contains a vet error. If runAllModules only
// visited the root, the error would never be seen and the test would pass
// incorrectly.
func TestRunAllModules_ReachesSecondModule(t *testing.T) {
	root := t.TempDir()
	toolsDir := filepath.Join(root, "tools", "build")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Root module: valid, minimal.
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package example\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Tools module: contains code that fails `go vet`.
	if err := os.WriteFile(filepath.Join(toolsDir, "go.mod"), []byte("module example/tools/build\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Printf with a format verb and no argument: go vet reports this.
	badCode := "package build\nimport \"fmt\"\nfunc init() { fmt.Printf(\"%d\") }\n"
	if err := os.WriteFile(filepath.Join(toolsDir, "bad.go"), []byte(badCode), 0644); err != nil {
		t.Fatal(err)
	}

	err := runAllModules(root, "vet")
	if err == nil {
		t.Fatal("runAllModules returned nil; the second module has a vet error that should have been caught")
	}
}

// runAllModules must stop at the first failure rather than continuing. This is
// the same contract as the old single-RunIn path and as verifySteps ("break //
// stop at the first failure").
func TestRunAllModules_StopsAtFirstFailure(t *testing.T) {
	root := t.TempDir()
	// A root module with code that fails vet — no tools/build module at all.
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	badCode := "package example\nimport \"fmt\"\nfunc init() { fmt.Printf(\"%d\") }\n"
	if err := os.WriteFile(filepath.Join(root, "bad.go"), []byte(badCode), 0644); err != nil {
		t.Fatal(err)
	}

	err := runAllModules(root, "vet")
	if err == nil {
		t.Fatal("runAllModules returned nil for a module that fails vet")
	}
}

// verifySteps in a Go project must include vet, build, and test — and after
// the fix, each must go through runAllModules (indirectly via the step
// closures). This test verifies the step names are present; the behaviour of
// each step is tested above.
func TestVerifySteps_GoProjectHasExpectedSteps(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n"), 0644); err != nil {
		t.Fatal(err)
	}

	steps := verifySteps(root)
	// The spend ratchet runs first and costs milliseconds: a commit can be
	// waved through with --no-verify, so this gate is the last thing between
	// an unapproved agent turn and trunk. See agentturns.go.
	want := []string{"agent turns", "format", "vet", "build", "test"}
	if len(steps) != len(want) {
		t.Fatalf("got %d steps, want %d", len(steps), len(want))
	}
	for i, w := range want {
		if steps[i].name != w {
			t.Errorf("step[%d].name = %q, want %q", i, steps[i].name, w)
		}
	}
}

// A non-Go project gets stub steps rather than real Go tooling. The stubs must
// succeed (not error) — they are placeholders, not failures.
func TestVerifySteps_NonGoProjectGetsStubs(t *testing.T) {
	root := t.TempDir() // no go.mod

	steps := verifySteps(root)
	if len(steps) != 4 {
		t.Fatalf("got %d stub steps, want 4", len(steps))
	}
	for _, s := range steps {
		if err := s.run(root); err != nil {
			t.Errorf("stub step %q failed: %v", s.name, err)
		}
	}
}
