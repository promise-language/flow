package common

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type step struct {
	name string
	run  func(repoRoot string) error
}

// RunVerify is the commit gate: format → vet → build → test. It always prints a
// summary block (even on failure) so an agent tailing the output sees the
// result without re-running, and the process exit code is the only contract.
//
// This is an EXAMPLE pipeline. For a Go project it runs real go tooling; for
// anything else it runs harmless stubs. Replace verifySteps with your project's
// real commands.
func RunVerify(repoRoot string, args []string) error {
	steps := verifySteps(repoRoot)
	start := time.Now()

	type result struct {
		name string
		ok   bool
	}
	var results []result
	failed := false

	for _, s := range steps {
		fmt.Printf("==> %s\n", s.name)
		err := s.run(repoRoot)
		results = append(results, result{s.name, err == nil})
		if err != nil {
			failed = true
			fmt.Fprintf(os.Stderr, "    %s failed: %v\n", s.name, err)
			break // stop at the first failure
		}
	}

	fmt.Println("\n──────── verify summary ────────")
	for _, r := range results {
		status := "ok"
		if !r.ok {
			status = "FAIL"
		}
		fmt.Printf("  %-4s  %s\n", status, r.name)
	}
	fmt.Printf("  elapsed %s\n", time.Since(start).Round(time.Millisecond))
	fmt.Println("────────────────────────────────")

	if failed {
		fmt.Println("❌ Verify FAILED: not safe to commit")
		return fmt.Errorf("verify failed")
	}
	fmt.Println("✅ OK to Commit")
	return nil
}

func verifySteps(repoRoot string) []step {
	if Exists(filepath.Join(repoRoot, "go.mod")) {
		return []step{
			{"format", checkFormatted},
			{"vet", func(r string) error { return runAllModules(r, "vet") }},
			{"build", func(r string) error { return runAllModules(r, "build") }},
			{"test", func(r string) error { return runAllModules(r, "test") }},
		}
	}
	stub := func(label string) step {
		return step{label, func(r string) error {
			fmt.Printf("    (stub) wire up your %s command in tools/build/common/verify.go\n", label)
			return nil
		}}
	}
	return []step{stub("format"), stub("vet"), stub("build"), stub("test")}
}

// runAllModules runs `go <verb> ./...` in every module of the repository.
func runAllModules(repoRoot, verb string) error {
	for _, dir := range modules(repoRoot) {
		if err := RunIn(dir, "go", verb, "./..."); err != nil {
			return err
		}
	}
	return nil
}

// checkFormatted reports unformatted files instead of rewriting them.
//
// `gofmt -w` made this step incapable of failing: it repaired the tree and
// exited 0, so the gate reported a clean run over a change it had silently
// altered. A gate states what is true about the tree it was handed; one that
// edits first is answering about a different tree — and under CI, about one
// nobody will ever see, since the checkout is discarded. Unformatted code would
// merge with the gate green.
//
// `gofmt -l` exits 0 whether or not it lists anything, so the OUTPUT is the
// signal and the exit code carries nothing. The names are printed because
// "run gofmt" without them leaves the reader to find the files themselves.
func checkFormatted(repoRoot string) error {
	out, err := RunOutputIn(repoRoot, "gofmt", "-l", ".")
	if err != nil {
		return fmt.Errorf("gofmt -l: %w", err)
	}
	if out == "" {
		return nil
	}
	files := strings.Split(out, "\n")
	for _, f := range files {
		fmt.Printf("    unformatted: %s\n", f)
	}
	return fmt.Errorf("%d file(s) need gofmt -w", len(files))
}
