package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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
