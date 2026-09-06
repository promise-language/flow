package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/promise-language/flow/tools/build/common"
)

// writeBin creates a binary file with the given content and returns its SHA-256.
func writeBin(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, common.BinaryName(name))
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// writeSidecar writes the extended-format sidecar file.
func writeSidecar(t *testing.T, path, sourceHash string, entries map[string]string) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(sourceHash)
	sb.WriteByte('\n')
	for name, hash := range entries {
		sb.WriteString(name)
		sb.WriteByte(':')
		sb.WriteString(hash)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUpToDate_ValidExtendedSidecar(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	hash1 := writeBin(t, binDir, "guard", "guard-binary-v1")
	hash2 := writeBin(t, binDir, "verify", "verify-binary-v1")

	hashFile := filepath.Join(binDir, ".tools.hash")
	sourceHash := "abc123"
	writeSidecar(t, hashFile, sourceHash, map[string]string{
		"guard":  hash1,
		"verify": hash2,
	})

	if !upToDate(hashFile, sourceHash, binDir, []string{"guard", "verify"}) {
		t.Error("expected up-to-date with matching sidecar and binaries")
	}
}

func TestUpToDate_WrongBinaryHash(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	hash1 := writeBin(t, binDir, "guard", "guard-binary-v1")
	writeBin(t, binDir, "verify", "verify-binary-v1")

	// Record correct hash for guard but wrong hash for verify (simulates replaced binary).
	hashFile := filepath.Join(binDir, ".tools.hash")
	sourceHash := "abc123"
	writeSidecar(t, hashFile, sourceHash, map[string]string{
		"guard":  hash1,
		"verify": "0000000000000000000000000000000000000000000000000000000000000000",
	})

	if upToDate(hashFile, sourceHash, binDir, []string{"guard", "verify"}) {
		t.Error("expected not up-to-date when binary hash does not match sidecar")
	}
}

func TestUpToDate_MissingBinary(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	hash1 := writeBin(t, binDir, "guard", "guard-binary-v1")

	hashFile := filepath.Join(binDir, ".tools.hash")
	sourceHash := "abc123"
	writeSidecar(t, hashFile, sourceHash, map[string]string{
		"guard":  hash1,
		"verify": "does-not-matter",
	})

	// verify binary does not exist on disk.
	if upToDate(hashFile, sourceHash, binDir, []string{"guard", "verify"}) {
		t.Error("expected not up-to-date when binary is missing")
	}
}

func TestUpToDate_OldFormatSidecar(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	writeBin(t, binDir, "guard", "guard-binary-v1")

	// Old format: just the source hash, no per-binary entries.
	hashFile := filepath.Join(binDir, ".tools.hash")
	sourceHash := "abc123"
	os.WriteFile(hashFile, []byte(sourceHash+"\n"), 0o644)

	if upToDate(hashFile, sourceHash, binDir, []string{"guard"}) {
		t.Error("expected not up-to-date with old-format sidecar (no per-binary entries)")
	}
}

func TestUpToDate_MissingToolEntry(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	hash1 := writeBin(t, binDir, "guard", "guard-binary-v1")
	writeBin(t, binDir, "verify", "verify-binary-v1")

	// Sidecar only records guard, not verify.
	hashFile := filepath.Join(binDir, ".tools.hash")
	sourceHash := "abc123"
	writeSidecar(t, hashFile, sourceHash, map[string]string{
		"guard": hash1,
	})

	if upToDate(hashFile, sourceHash, binDir, []string{"guard", "verify"}) {
		t.Error("expected not up-to-date when tool entry is missing from sidecar")
	}
}

func TestUpToDate_WrongSourceHash(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	hash1 := writeBin(t, binDir, "guard", "guard-binary-v1")

	hashFile := filepath.Join(binDir, ".tools.hash")
	writeSidecar(t, hashFile, "old-source-hash", map[string]string{
		"guard": hash1,
	})

	if upToDate(hashFile, "new-source-hash", binDir, []string{"guard"}) {
		t.Error("expected not up-to-date when source hash differs")
	}
}

func TestUpToDate_MissingSidecar(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	hashFile := filepath.Join(binDir, ".tools.hash") // does not exist

	if upToDate(hashFile, "any", binDir, []string{"guard"}) {
		t.Error("expected not up-to-date when sidecar is missing")
	}
}

func TestUpToDate_MalformedSidecarEntry(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	writeBin(t, binDir, "guard", "guard-binary-v1")

	// Sidecar has correct source hash but a malformed binary entry (no colon).
	hashFile := filepath.Join(binDir, ".tools.hash")
	sourceHash := "abc123"
	os.WriteFile(hashFile, []byte(sourceHash+"\nguard-no-colon\n"), 0o644)

	if upToDate(hashFile, sourceHash, binDir, []string{"guard"}) {
		t.Error("expected not up-to-date with malformed sidecar entry (no colon)")
	}
}

// Retiring a tool (issue #199 removed cmd/guard) leaves every existing clone
// with a binary and a sidecar entry for a name that is no longer built. Neither
// surplus may flip upToDate false: a false here rebuilds every tool on every
// invocation, forever, in every clone that ever ran the old build.
func TestUpToDate_SurplusBinaryAndSidecarEntry(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	verifyHash := writeBin(t, binDir, "verify", "verify-binary-v1")
	// The retired tool: still on disk, still recorded, no longer expected.
	retiredHash := writeBin(t, binDir, "guard", "retired-binary")

	hashFile := filepath.Join(binDir, ".tools.hash")
	sourceHash := "abc123"
	writeSidecar(t, hashFile, sourceHash, map[string]string{
		"verify": verifyHash,
		"guard":  retiredHash,
	})

	if !upToDate(hashFile, sourceHash, binDir, []string{"verify"}) {
		t.Error("expected up-to-date: a retired tool's leftover binary and sidecar entry must be ignored")
	}
}

// The exact deadlock from issue #93: sidecar written correctly at build time,
// then the binary is replaced (copied from another clone, restored from cache,
// etc.). The old upToDate only checked existence and would report "up to date".
func TestUpToDate_BinaryReplacedAfterBuild(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	// Build: write binary, record its hash in the sidecar.
	origHash := writeBin(t, binDir, "guard", "guard-binary-original")
	hashFile := filepath.Join(binDir, ".tools.hash")
	sourceHash := "abc123"
	writeSidecar(t, hashFile, sourceHash, map[string]string{
		"guard": origHash,
	})

	// Simulate replacement: overwrite the binary with different content.
	// The file still exists, but its SHA-256 no longer matches.
	writeBin(t, binDir, "guard", "guard-binary-from-another-clone")

	if upToDate(hashFile, sourceHash, binDir, []string{"guard"}) {
		t.Error("expected not up-to-date when binary was replaced after build")
	}
}

// guardNames are the names this repository must never build into bin/.
// `guard` is the retired one — #199 deleted tools/build/cmd/guard. The other
// two are the workspace artifact's, hard-linked into bin/ by provisioning; a
// tool built here under either name would overwrite the link on the first
// ./make and split the installed set behind a version marker still claiming
// the artifact's.
var guardNames = []string{"guard", "tool-guard", "precommit-guard"}

// provisionedBinaries are the names in bin/ that come from the workspace
// artifact rather than from ./make, so a hook may name one even though no
// directory under cmd/ builds it.
var provisionedBinaries = []string{"tool-guard", "precommit-guard", "workspace"}

func TestDiscoverTools_ListsDirectoriesExceptMake(t *testing.T) {
	cmdDir := t.TempDir()
	for _, name := range []string{"verify", "make", "gate"} {
		if err := os.Mkdir(filepath.Join(cmdDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A file is not a tool: `go build ./cmd/notes.md` is not a build.
	if err := os.WriteFile(filepath.Join(cmdDir, "notes.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := discoverTools(cmdDir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "gate,verify" {
		t.Errorf("discoverTools() = %v, want [gate verify] — make runs from source and a file is not a package", got)
	}
}

// A cmd directory that cannot be read is not an empty tool set. Reporting no
// tools with no error would take the up-to-date short circuit — nothing to
// build, every expected binary present, "Tools up to date" — and leave bin/
// however it was found, which for a fresh clone is empty.
func TestDiscoverTools_UnreadableDirectoryIsAnError(t *testing.T) {
	if _, err := discoverTools(filepath.Join(t.TempDir(), "cmd")); err == nil {
		t.Error("discoverTools() succeeded on a missing cmd directory; make would report every tool up to date having built none")
	}
}

// The #199 regression, and the reason the deletion was the whole change: the
// tool set is the directory listing, so re-creating tools/build/cmd/guard is
// by itself enough to bring the twin back. On an artifact-provisioned clone
// the first ./make then overwrites the hard-linked bin/guard.
func TestToolSet_BuildsNoGuard(t *testing.T) {
	for _, name := range repoTools(t) {
		for _, guard := range guardNames {
			if name == guard {
				t.Errorf("./make builds bin/%s: guards are provisioned with the workspace artifact, not built here — delete tools/build/cmd/%s", name, name)
			}
		}
	}
}

// binRef finds every bin/<name> a hook definition runs or names.
var binRef = regexp.MustCompile(`bin/([A-Za-z0-9_.-]+)`)

// The other half of #199, and what made its order load-bearing: the hooks were
// repointed at bin/tool-guard BEFORE cmd/guard was deleted, because a hook
// naming a binary nothing supplies is worse than one naming a stale binary.
// These hooks are tracked files, live in a fresh clone before anything has
// been built or provisioned, and the PreToolUse one ends `|| exit 2` — pointed
// at a name no build produces, it blocks every tool call in the arena.
func TestCommittedHooks_NameOnlySuppliedBinaries(t *testing.T) {
	root := repoRoot(t)
	tools := repoTools(t)
	supplied := map[string]bool{}
	for _, name := range tools {
		supplied[name] = true
	}
	for _, name := range provisionedBinaries {
		supplied[name] = true
	}

	for _, rel := range []string{".claude/settings.json", ".githooks/pre-commit"} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		seen := 0
		for _, line := range strings.Split(string(data), "\n") {
			// The interpreter line names a binary too — /usr/bin/env — and it
			// is not one anything here supplies.
			if strings.HasPrefix(line, "#!") {
				continue
			}
			for _, m := range binRef.FindAllStringSubmatch(line, -1) {
				seen++
				if !supplied[m[1]] {
					t.Errorf("%s runs bin/%s, which nothing supplies: ./make builds %v and provisioning installs %v",
						rel, m[1], tools, provisionedBinaries)
				}
			}
		}
		if seen == 0 {
			// Not a pass: either the hook stopped naming a binary, or the scan
			// stopped recognising one. Either way nothing above was checked.
			t.Errorf("%s names no bin/ binary — this test is no longer looking at anything", rel)
		}
	}
}

// repoTools is the tool set of THIS repository, read the way make reads it.
func repoTools(t *testing.T) []string {
	t.Helper()
	tools, err := discoverTools(filepath.Join(repoRoot(t), "tools", "build", "cmd"))
	if err != nil {
		t.Fatalf("reading this repository's cmd directory: %v", err)
	}
	return tools
}

// repoRoot is four levels up from tools/build/cmd/make.
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	return abs
}
