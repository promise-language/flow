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
// one is deleting it (#199 retired `guard` that way).
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

func upToDate(hashFile, hash, binDir string, tools []string) bool {
	f, err := os.Open(hashFile)
	if err != nil {
		return false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)

	// Line 1: source hash.
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != hash {
		return false
	}

	// Lines 2+: name:sha256 per binary. Build a lookup.
	recorded := make(map[string]string)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return false // malformed entry
		}
		recorded[parts[0]] = parts[1]
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
