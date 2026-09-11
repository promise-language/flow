// Command make is the meta-builder. It compiles every other tool under cmd/
// into <repoRoot>/bin, stamping each binary with the tools-source hash and the
// absolute repo root via -ldflags. It is the one tool that runs via 'go run'
// (from the ./make trampoline), so it is never compiled into bin/ and never
// stale — which is what breaks the bootstrap cycle.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/promise-language/flow/tools/build/common"
)

const usage = `make — the meta-builder.

Usage:
  ./make [-force | --force] [-h | -help]

Compiles every tool under tools/build/cmd into bin/ (stamping each with the
tools-source hash and repo root) and wires git hooks. Skips the build when
bin/ is already up to date; -force rebuilds regardless.`

func main() {
	common.MaybeHelp(os.Args[1:], usage)
	force := false
	for _, a := range os.Args[1:] {
		if a == "-force" || a == "--force" {
			force = true
		}
	}

	// 1. Resolve the repo root. The ./make trampoline cd'd go run into
	//    <root>/tools/build, so our cwd is exactly that. Two levels up is root.
	cwd, err := os.Getwd()
	must(err)
	repoRoot := filepath.Dir(filepath.Dir(cwd))
	if !filepath.IsAbs(repoRoot) {
		fail("resolved repo root is not absolute: %s", repoRoot)
	}

	// 2. Hash the tools source — baked into every binary below.
	hash, err := common.ToolsSourceHash(repoRoot)
	must(err)

	// 3. Enable git hooks unconditionally (idempotent, fast).
	if err := common.RunSetup(repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not configure git hooks: %v\n", err)
	}

	tools, err := discoverTools(filepath.Join(repoRoot, "tools", "build", "cmd"))
	must(err)

	binDir := filepath.Join(repoRoot, "bin")
	hashFile := filepath.Join(binDir, ".tools.hash")

	// 4. Up-to-date short circuit.
	if !force && upToDate(hashFile, hash, binDir, tools) {
		fmt.Println("Tools up to date")
		return
	}

	must(os.MkdirAll(binDir, 0o755))

	// 5. Build each tool, injecting repoRoot and sourceHash via ldflags.
	ldflags := fmt.Sprintf("-s -w -X main.sourceHash=%s -X main.repoRoot=%s", hash, repoRoot)
	toolsModDir := filepath.Join(repoRoot, "tools", "build")
	for _, name := range tools {
		out := filepath.Join(binDir, common.BinaryName(name))
		fmt.Printf("building %s\n", name)
		if err := common.RunIn(toolsModDir, "go", "build",
			"-trimpath",
			"-ldflags", ldflags,
			"-o", out,
			"./cmd/"+name,
		); err != nil {
			fail("building %s: %v", name, err)
		}
	}

	// 5b. Remove what an earlier build left for tools retired since. This
	//     reads the sidecar the previous build wrote, so it runs before that
	//     record is replaced below.
	must(pruneRetired(hashFile, binDir, tools))

	// 6. Write the hash sidecar — the staleness contract.
	//    Line 1: source hash. Lines 2+: name:sha256 per binary.
	var sb strings.Builder
	sb.WriteString(hash)
	sb.WriteByte('\n')
	for _, name := range tools {
		h, err := fileHash(filepath.Join(binDir, common.BinaryName(name)))
		if err != nil {
			fail("hashing %s: %v", name, err)
		}
		sb.WriteString(name)
		sb.WriteByte(':')
		sb.WriteString(h)
		sb.WriteByte('\n')
	}
	must(os.WriteFile(hashFile, []byte(sb.String()), 0o644))
	fmt.Printf("built %d tool(s) into bin/\n", len(tools))
}

// discoverTools is the tool set: one tool per directory under
// tools/build/cmd, except make itself, which runs from source and is never
// compiled into bin/. The listing IS the registry — there is no list anywhere
// to keep in step with it, so adding a tool is adding a directory and retiring
// one is deleting it (#199 retired `guard` that way); the next ./make removes
// the binary it had built for the retired name (see pruneRetired).
//
// A file under cmd/ is not a tool: `go build ./cmd/<name>` wants a package.
func discoverTools(cmdDir string) ([]string, error) {
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		return nil, err
	}
	var tools []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != "make" {
			tools = append(tools, e.Name())
		}
	}
	sort.Strings(tools)
	return tools, nil
}

// readSidecar parses the hash sidecar: line 1 is the tools-source hash, lines
// 2+ are name:sha256 for each binary make built. It is the one parser of that
// format — upToDate reads it to decide whether to build, and pruneRetired reads
// it to learn what an earlier build left in bin/. A sidecar that cannot be
// read, is empty, or carries a malformed entry is an error; the callers decide
// what that means for them.
func readSidecar(hashFile string) (sourceHash string, recorded map[string]string, err error) {
	f, err := os.Open(hashFile)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return "", nil, fmt.Errorf("%s: no source hash on line 1", hashFile)
	}
	sourceHash = strings.TrimSpace(sc.Text())

	recorded = make(map[string]string)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return "", nil, fmt.Errorf("%s: malformed entry %q", hashFile, line)
		}
		recorded[parts[0]] = parts[1]
	}
	if err := sc.Err(); err != nil {
		return "", nil, err
	}
	return sourceHash, recorded, nil
}

func upToDate(hashFile, hash, binDir string, tools []string) bool {
	sourceHash, recorded, err := readSidecar(hashFile)
	if err != nil || sourceHash != hash {
		return false
	}

	// Every expected tool must have a recorded hash that matches the binary on disk.
	for _, name := range tools {
		want, ok := recorded[name]
		if !ok {
			return false // tool not recorded in sidecar
		}
		got, err := fileHash(filepath.Join(binDir, common.BinaryName(name)))
		if err != nil {
			return false // binary missing or unreadable
		}
		if got != want {
			return false // binary replaced since last build
		}
	}
	return true
}

// pruneRetired removes the binaries an earlier ./make built for tools that no
// longer exist. Retiring a tool is deleting its directory under cmd/ (see
// discoverTools); without this, the binary built for it stays in bin/ in every
// clone that ever built it. #199 retired `guard` and left a bin/guard behind —
// the exact "binary under a guard name" that main_test.go's guardNames
// defends against.
//
// The rule is "recorded by make and unchanged since", read from the sidecar
// the PREVIOUS build wrote: a name that is recorded but not in tools, whose
// file still hashes to what was recorded, is make's own leftover and goes.
// Nothing else is touched. A binary make never recorded is not make's to
// remove — the workspace's guards are hard-linked into bin/ by provisioning
// and appear on no list this program holds, so an allowlist prune would delete
// one on its first run. A recorded binary whose hash differs was replaced by
// somebody since make wrote it and is left in place, with a warning. No
// sidecar, or one that does not parse, is nothing recorded: make is about to
// rewrite it anyway.
func pruneRetired(hashFile, binDir string, tools []string) error {
	_, recorded, err := readSidecar(hashFile)
	if err != nil {
		return nil
	}
	current := make(map[string]bool, len(tools))
	for _, name := range tools {
		current[name] = true
	}
	var retired []string
	for name := range recorded {
		if !current[name] {
			retired = append(retired, name)
		}
	}
	sort.Strings(retired)

	for _, name := range retired {
		path := filepath.Join(binDir, common.BinaryName(name))
		got, err := fileHash(path)
		switch {
		case os.IsNotExist(err):
			continue // already gone; nothing to remove
		case err != nil:
			fmt.Fprintf(os.Stderr, "warning: %s is retired but cannot be read (%v); left in place\n", path, err)
			continue
		case got != recorded[name]:
			fmt.Fprintf(os.Stderr, "warning: %s is retired but not the file ./make built; left in place\n", path)
			continue
		}
		fmt.Printf("removing retired %s\n", name)
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("removing retired %s: %w", name, err)
		}
	}
	return nil
}

// fileHash returns the hex-encoded SHA-256 of the file at path.
func fileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "make: "+format+"\n", args...)
	os.Exit(1)
}
