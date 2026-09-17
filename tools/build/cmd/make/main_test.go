package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/promise-language/flow/tools/build/common"
	"github.com/promise-language/forge/primitives"
)

// writeBin creates a binary file with the given content and returns its SHA-256.
func writeBin(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, primitives.BinaryName(name))
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

// Raising the pinned forge version, and nothing else, must make every tool
// report itself stale until the next ./make (#401, forge docs/primitives.md
// §4). That is what buys this project out of adding any mechanism for
// staleness when the shared helpers stop being local files.
//
// The half that is forge's is already tested there: SourceHash reads go.mod
// and go.sum (primitives/hash_test.go, TestSourceHashReadsOnlySourceAndManifests,
// which fails with "raising a pinned version would not report a binary stale").
// Restating it here would be a second copy of an upstream test, which is the
// shape this item exists to remove.
//
// What is this repository's to check is the composition — that the pin it
// actually ships lands inside the tree the hash walks.
//
// THE `replace` CHECK IS THE ONE NOTHING ELSE MAKES. ./make stamps
// primitives.ToolsSourceHash(repoRoot), which names no directory and therefore
// covers tools/build and nothing else. A `replace` pointing at a forge working
// tree puts every helper outside that walk, and an edit there leaves every
// binary in bin/ claiming to be current — the one failure the hash exists to
// prevent. §4 says a project in that case names the replaced directories; this
// one is not in that case, and the no-directory call is correct only while that
// holds. The toolchain has no opinion here: a resolving `replace` builds, vets
// and tests clean, and staleness goes quietly wrong underneath.
//
// The require and the go.sum digest are named rather than relied on: `go test`
// already refuses a tools module that requires no forge or carries no digest for
// it, so those two lines cannot be the first to fail. They state the invariant,
// and they are what makes the last leg's failure legible — it rewrites the pin
// inside the real go.mod and go.sum bytes, and a pin that appeared in neither
// would fail there with nothing pointing at why.
func TestThePinnedForgeVersionIsInsideTheHashedTree(t *testing.T) {
	toolsDir := filepath.Join(repoRoot(t), "tools", "build")
	goMod, err := os.ReadFile(filepath.Join(toolsDir, "go.mod"))
	if err != nil {
		t.Fatalf("reading this repository's tools/build/go.mod: %v", err)
	}
	goSum, err := os.ReadFile(filepath.Join(toolsDir, "go.sum"))
	if err != nil {
		t.Fatalf("reading this repository's tools/build/go.sum: %v — an unverified pin is not a pin", err)
	}

	pin := pinnedForgeVersion(t, string(goMod))
	if want := forgeModule + " " + pin + " h1:"; !strings.Contains(string(goSum), want) {
		t.Errorf("go.sum carries no %q line — go.mod's pin is not verified by anything", want)
	}
	for _, line := range strings.Split(string(goMod), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == "replace" {
			t.Fatalf("tools/build/go.mod replaces a module (%q), but ./make hashes tools/build and nothing else — "+
				"an edit inside the replaced tree would leave every binary claiming to be current "+
				"(forge docs/primitives.md §4)", strings.TrimSpace(line))
		}
	}

	// The consequence, over the bytes this repository ships: raising the pin is
	// a source change even though no .go file moved.
	repo := t.TempDir()
	dir := filepath.Join(repo, "tools", "build")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, body []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", goMod)
	write("go.sum", goSum)

	// The hash ./make bakes into every binary it builds from this tree.
	stamped, err := primitives.ToolsSourceHash(repo)
	if err != nil {
		t.Fatalf("hashing the tools source: %v", err)
	}
	if reason := primitives.StaleReason(repo, stamped); reason != "" {
		t.Fatalf("a tree nobody touched reported stale: %q", reason)
	}

	const raised = "v0.0.0-29990101000000-ffffffffffff"
	write("go.mod", []byte(strings.Replace(string(goMod), pin, raised, 1)))
	if reason := primitives.StaleReason(repo, stamped); !strings.Contains(reason, "tools source has changed") {
		t.Errorf("raising the pin in go.mod reported %q — every tool in bin/ would go on running the forge it was built against", reason)
	}

	// And go.sum alone, which is where the version's digest lives: go.mod
	// restored to its old text over a go.sum that moved is a tree whose
	// dependency cannot be what the binaries were built against.
	write("go.mod", goMod)
	write("go.sum", []byte(strings.ReplaceAll(string(goSum), pin, raised)))
	if reason := primitives.StaleReason(repo, stamped); !strings.Contains(reason, "tools source has changed") {
		t.Errorf("rewriting go.sum reported %q — a changed dependency digest left every binary claiming to be current", reason)
	}
}

const forgeModule = "github.com/promise-language/forge"

// forge owns these helpers, and this module must not own them again.
//
// #401 deleted seven files from tools/build/common — platform, args, help,
// setup, hash, stale, exec — and replaced them with an import of
// github.com/promise-language/forge/primitives at the version pinned above.
// Nothing in the toolchain keeps them deleted. A file declaring HasHelpFlag
// again would compile, vet, and pass every other test in this module, and the
// call sites would quietly resolve to it.
//
// The pressure to bring one back is real and already named: #345 asks this
// repository to fix `-h` and flag normalisation, and the shortest way to do
// that is a local help.go. That makes flow the one copy in the org that
// differs, and leaves forge and every project importing it with the bug
// (forge docs/primitives.md §1, "A second copy of it is a future
// disagreement"). A change to a primitive is made in forge and reaches here by
// raising the pin — never by a local patch.
//
// The scan matches on the NAME, and that is not a shortcut. §5 says a helper
// published in primitives keeps the name it had in the copies, so that adopting
// the library is a deletion and an import rather than a rewrite. A copy brought
// back therefore arrives under the name it left with.
func TestNoHelperForgePublishesIsDeclaredHere(t *testing.T) {
	published := publishedByForge(t)
	found, err := redeclaredPrimitives(filepath.Join(repoRoot(t), "tools", "build"), published)
	if err != nil {
		t.Fatalf("scanning this module: %v", err)
	}
	for _, r := range found {
		t.Errorf("tools/build/%s declares %s, which forge publishes in primitives/%s — that is a second copy "+
			"of a helper forge owns. Fix it in forge and raise the pin in tools/build/go.mod "+
			"(#401, forge docs/primitives.md §1)", r.file, r.name, published[r.name])
	}
}

// Against a clean module the scan above reports nothing whether it works or
// not, so these cases are what say whether it still does anything.
//
// The copy is forge's own help.go put back where it was, which is what the #345
// shortcut would produce. The lookalikes are the other half and matter just as
// much: tools/build really does want a method named after a helper and a local
// unexported one, and a check that refused those would be deleted the week it
// first fired — the same outcome as not having one.
func TestRedeclaredPrimitives_FindsACopyAndLeavesALookalike(t *testing.T) {
	published := publishedByForge(t)
	write := func(t *testing.T, name, body string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("a copy under its own name is found", func(t *testing.T) {
		dir := write(t, "help.go", `package common

import (
	"fmt"
	"os"
)

func HasHelpFlag(args []string) bool {
	for _, a := range NormalizeArgs(args) {
		if a == "-h" || a == "-help" {
			return true
		}
	}
	return false
}

func MaybeHelp(args []string, usage string) {
	if HasHelpFlag(args) {
		fmt.Println(usage)
		os.Exit(0)
	}
}
`)
		if got, want := namesOf(t, dir, published), []string{"HasHelpFlag", "MaybeHelp"}; !sameNames(got, want) {
			t.Errorf("scanning a re-copied help.go found %v, want %v — the check no longer detects what it exists for", got, want)
		}
	})

	// A copy behind a build tag is still a copy, and it is the one the compiler
	// on this machine says least about.
	t.Run("a copy behind a build tag is found", func(t *testing.T) {
		dir := write(t, "exec_plan9.go", `//go:build plan9

package common

import "os/exec"

func RunSilent(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}
`)
		if got, want := namesOf(t, dir, published), []string{"RunSilent"}; !sameNames(got, want) {
			t.Errorf("scanning a tagged-out copy found %v, want %v", got, want)
		}
	})

	t.Run("a lookalike is left alone", func(t *testing.T) {
		dir := write(t, "lookalike.go", `package common

type cache struct {
	// A field, not a declaration.
	Exists bool
}

// A method on a type is not a package-level helper.
func (c cache) Which(path string) string { return "" }

// An unexported name is a different name.
func exists(path string) bool { return false }

func measure() {
	// A local binding is not a declaration either.
	RunIn := "go build"
	_ = RunIn
}
`)
		if got := namesOf(t, dir, published); len(got) != 0 {
			t.Errorf("the scan flagged %v, none of which is a copy of a forge helper — a check this broad gets turned off", got)
		}
	})

	// The cases above are worth nothing if the published set came back empty,
	// which is how a broken probe reads: every scan comes back clean.
	t.Run("the published set is the real one", func(t *testing.T) {
		for _, name := range []string{"HasHelpFlag", "NormalizeArgs", "ToolsSourceHash", "StaleReason", "Exists", "RunIn"} {
			if published[name] == "" {
				t.Errorf("primitives publishes %s, but the set read from the pinned module does not name it", name)
			}
		}
	})
}

// publishedByForge maps each name the pinned primitives package exports to the
// file it is published in.
//
// It is read from the module the build actually resolves rather than written
// down here: a list in this file would go stale exactly when it mattered, since
// the helper forge adds after it was written is the one nobody would weigh a
// copy against. Subpackages are not walked — primitives/containment and
// primitives/disclosure are other packages, and this module imports neither.
func publishedByForge(t *testing.T) map[string]string {
	t.Helper()
	toolsDir := filepath.Join(repoRoot(t), "tools", "build")
	cmd := exec.Command("go", "list", "-f", "{{.Dir}}", forgeModule+"/primitives")
	cmd.Dir = toolsDir
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		t.Fatalf("locating the pinned primitives package from %s: %v\n%s", toolsDir, err, stderr)
	}
	decls, err := topLevelDecls(strings.TrimSpace(string(out)), false)
	if err != nil {
		t.Fatalf("reading the pinned primitives package: %v", err)
	}
	published := map[string]string{}
	for name, file := range decls {
		if ast.IsExported(name) {
			published[name] = file
		}
	}
	// A floor, not a count: the exact number is forge's to change, but a
	// handful would mean the package moved or the parse failed, and every scan
	// below would come back clean for the wrong reason.
	if len(published) < 10 {
		t.Fatalf("read %d exported names from the pinned primitives package — the probe is broken, not the code", len(published))
	}
	return published
}

// redeclaration is one name this module declares that forge also publishes.
type redeclaration struct{ name, file string }

// redeclaredPrimitives reports every top-level declaration under dir whose name
// forge publishes. Sorted, so a failure names them in a stable order.
func redeclaredPrimitives(dir string, published map[string]string) ([]redeclaration, error) {
	here, err := topLevelDecls(dir, true)
	if err != nil {
		return nil, err
	}
	var found []redeclaration
	for name, file := range here {
		if published[name] != "" {
			found = append(found, redeclaration{name: name, file: file})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].name < found[j].name })
	return found, nil
}

// topLevelDecls maps every package-level declaration under dir to the file it
// is in, relative to dir and slash-separated. Test files are skipped: forge's
// own test names are not helpers, and this module's are not copies.
//
// Build tags are ignored on purpose. Only one platform file compiles on any
// given machine, so a copy parked behind a tag is the one the toolchain here
// says nothing about at all.
//
// Methods are excluded. A method shares a namespace with nothing, so one named
// Which is not a second Which — it is a name on a type.
func topLevelDecls(dir string, recurse bool) (map[string]string, error) {
	decls := map[string]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch {
			case path == dir:
				return nil
			case !recurse, strings.HasPrefix(entry.Name(), "."):
				return fs.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					decls[d.Name.Name] = rel
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						decls[s.Name.Name] = rel
					case *ast.ValueSpec:
						for _, n := range s.Names {
							decls[n.Name] = rel
						}
					}
				}
			}
		}
		return nil
	})
	return decls, err
}

// namesOf is redeclaredPrimitives reduced to the names it found.
func namesOf(t *testing.T, dir string, published map[string]string) []string {
	t.Helper()
	found, err := redeclaredPrimitives(dir, published)
	if err != nil {
		t.Fatalf("scanning %s: %v", dir, err)
	}
	names := make([]string, 0, len(found))
	for _, r := range found {
		names = append(names, r.name)
	}
	return names
}

func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pinnedForgeVersion is the version tools/build/go.mod requires forge at, in
// either spelling — a bare `require` line or a line inside a `require (` block.
// It fails the test when there is none: a tools module that requires nothing is
// the state this item replaced, and a version that is not exact is a build
// whose helpers can change without anyone raising a line.
func pinnedForgeVersion(t *testing.T, goMod string) string {
	t.Helper()
	for _, line := range strings.Split(goMod, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "require" && fields[1] == forgeModule && strings.HasPrefix(fields[2], "v") {
			return fields[2]
		}
		if len(fields) == 2 && fields[0] == forgeModule && strings.HasPrefix(fields[1], "v") {
			return fields[1]
		}
	}
	t.Fatalf("tools/build/go.mod requires no exact version of %s — the helpers it is built from are unpinned:\n%s", forgeModule, goMod)
	return ""
}

// Retiring a tool must not leave the binary ./make built for it in bin/: #199
// deleted cmd/guard, and every clone that had built it kept a bin/guard — the
// very "binary under a guard name" guardNames defends against. The sidecar the
// previous build wrote says what make built; a recorded name no longer in the
// tool set, whose file is still what was recorded, is make's to remove.
func TestPruneRetired_RemovesWhatMakeBuiltAndNoLongerBuilds(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	verifyHash := writeBin(t, binDir, "verify", "verify-binary-v1")
	retiredHash := writeBin(t, binDir, "precommit", "precommit-binary-v1")

	hashFile := filepath.Join(binDir, ".tools.hash")
	writeSidecar(t, hashFile, "abc123", map[string]string{
		"verify":    verifyHash,
		"precommit": retiredHash,
	})

	if err := pruneRetired(hashFile, binDir, []string{"verify"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(binDir, primitives.BinaryName("precommit"))); !os.IsNotExist(err) {
		t.Errorf("bin/precommit is still there after its tool was retired (stat: %v)", err)
	}
	if _, err := os.Stat(filepath.Join(binDir, primitives.BinaryName("verify"))); err != nil {
		t.Errorf("bin/verify, still a tool, was removed: %v", err)
	}
}

// A binary the sidecar never recorded is not make's to remove. The workspace's
// guards are hard-linked into bin/ by provisioning and appear on no list this
// program holds, so "not built here and not on the allowlist" would delete a
// provisioned guard on its first run — the hazard guardNames describes. The
// only safe rule is "what make wrote".
func TestPruneRetired_LeavesABinaryItNeverRecorded(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	verifyHash := writeBin(t, binDir, "verify", "verify-binary-v1")
	writeBin(t, binDir, "tool-guard", "provisioned-by-the-workspace")

	hashFile := filepath.Join(binDir, ".tools.hash")
	writeSidecar(t, hashFile, "abc123", map[string]string{
		"verify": verifyHash,
	})

	if err := pruneRetired(hashFile, binDir, []string{"verify"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(binDir, primitives.BinaryName("tool-guard"))); err != nil {
		t.Errorf("bin/tool-guard, which make never recorded, was removed: %v", err)
	}
}

// A recorded binary whose content no longer matches was replaced by somebody
// since make wrote it — copied from another clone, or hard-linked over by
// provisioning. It is not make's leftover any more, so it stays, and the
// warning says so: a file silently kept looks exactly like one nobody looked
// at.
func TestPruneRetired_LeavesAReplacedBinary(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	verifyHash := writeBin(t, binDir, "verify", "verify-binary-v1")
	builtHash := writeBin(t, binDir, "precommit", "precommit-as-make-built-it")

	hashFile := filepath.Join(binDir, ".tools.hash")
	writeSidecar(t, hashFile, "abc123", map[string]string{
		"verify":    verifyHash,
		"precommit": builtHash,
	})
	// Replaced after the build: same name, different content.
	writeBin(t, binDir, "precommit", "precommit-from-somewhere-else")

	stderr := captureStderr(t)
	if err := pruneRetired(hashFile, binDir, []string{"verify"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(binDir, primitives.BinaryName("precommit"))); err != nil {
		t.Errorf("bin/precommit, replaced since make built it, was removed: %v", err)
	}
	if got := stderr(); !strings.Contains(got, "precommit") || !strings.Contains(got, "not the file ./make built") {
		t.Errorf("stderr = %q, want a warning naming the replaced binary and why it stays", got)
	}
}

// Nothing recorded, or nothing left to remove, is nothing to do — not an
// error. A first build has no sidecar; a sidecar an older make wrote may not
// parse; a retired binary may already be gone. Each is the state make is about
// to overwrite anyway, and failing the build on it would block every clone
// that reaches one.
func TestPruneRetired_MissingSidecarOrMissingBinaryIsNotAnError(t *testing.T) {
	t.Run("no sidecar", func(t *testing.T) {
		dir := t.TempDir()
		binDir := filepath.Join(dir, "bin")
		os.MkdirAll(binDir, 0o755)
		writeBin(t, binDir, "precommit", "precommit-binary-v1")

		if err := pruneRetired(filepath.Join(binDir, ".tools.hash"), binDir, []string{"verify"}); err != nil {
			t.Fatalf("pruneRetired with no sidecar: %v", err)
		}
		if _, err := os.Stat(filepath.Join(binDir, primitives.BinaryName("precommit"))); err != nil {
			t.Errorf("with no sidecar nothing is recorded, yet bin/precommit was removed: %v", err)
		}
	})
	t.Run("malformed sidecar", func(t *testing.T) {
		dir := t.TempDir()
		binDir := filepath.Join(dir, "bin")
		os.MkdirAll(binDir, 0o755)
		writeBin(t, binDir, "precommit", "precommit-binary-v1")
		hashFile := filepath.Join(binDir, ".tools.hash")
		os.WriteFile(hashFile, []byte("abc123\nprecommit-no-colon\n"), 0o644)

		if err := pruneRetired(hashFile, binDir, []string{"verify"}); err != nil {
			t.Fatalf("pruneRetired with a malformed sidecar: %v", err)
		}
		if _, err := os.Stat(filepath.Join(binDir, primitives.BinaryName("precommit"))); err != nil {
			t.Errorf("a sidecar that does not parse records nothing, yet bin/precommit was removed: %v", err)
		}
	})
	t.Run("recorded binary already gone", func(t *testing.T) {
		dir := t.TempDir()
		binDir := filepath.Join(dir, "bin")
		os.MkdirAll(binDir, 0o755)
		hashFile := filepath.Join(binDir, ".tools.hash")
		writeSidecar(t, hashFile, "abc123", map[string]string{
			"precommit": "0000000000000000000000000000000000000000000000000000000000000000",
		})

		if err := pruneRetired(hashFile, binDir, []string{"verify"}); err != nil {
			t.Fatalf("pruneRetired with the recorded binary already gone: %v", err)
		}
	})
}

// A retired binary that cannot be read cannot be shown to be what make built,
// and the rule is "recorded AND unchanged": unverifiable is not unchanged. It
// stays, the warning says why, and the build goes on — the same treatment a
// replaced binary gets, since both are files make can no longer vouch for.
// Removing it would delete something on the strength of its name alone.
func TestPruneRetired_LeavesABinaryItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	retiredHash := writeBin(t, binDir, "precommit", "precommit-binary-v1")
	hashFile := filepath.Join(binDir, ".tools.hash")
	writeSidecar(t, hashFile, "abc123", map[string]string{
		"precommit": retiredHash,
	})
	path := filepath.Join(binDir, primitives.BinaryName("precommit"))
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o755) })

	stderr := captureStderr(t)
	if err := pruneRetired(hashFile, binDir, []string{"verify"}); err != nil {
		t.Fatalf("an unreadable retired binary must not fail the build: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("bin/precommit, which make could not read and so could not vouch for, was removed: %v", err)
	}
	if got := stderr(); !strings.Contains(got, "precommit") || !strings.Contains(got, "cannot be read") {
		t.Errorf("stderr = %q, want a warning naming the unreadable binary and why it stays", got)
	}
}

// A retired binary that IS make's and cannot be removed is a failed build, not
// a warning. This is the one exit that fails: the file is provably make's
// leftover, and a leftover under a guard name is the hazard guardNames
// describes, so leaving it behind with a line on stderr would let "removed"
// and "could not remove" look alike to the next ./make, which finds the name
// gone from the sidecar it is about to write and never looks again.
func TestPruneRetired_CannotRemoveIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	retiredHash := writeBin(t, binDir, "precommit", "precommit-binary-v1")
	hashFile := filepath.Join(binDir, ".tools.hash")
	writeSidecar(t, hashFile, "abc123", map[string]string{
		"precommit": retiredHash,
	})
	// Readable, so the hash check passes; not writable, so unlink fails.
	if err := os.Chmod(binDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(binDir, 0o755) })

	err := pruneRetired(hashFile, binDir, []string{"verify"})
	if err == nil {
		t.Fatal("pruneRetired reported success with make's own retired binary still in bin/")
	}
	if !strings.Contains(err.Error(), "precommit") {
		t.Errorf("err = %v, want it to name the binary it could not remove", err)
	}
	if _, statErr := os.Stat(filepath.Join(binDir, primitives.BinaryName("precommit"))); statErr != nil {
		t.Errorf("bin/precommit should still be there after a failed remove: %v", statErr)
	}
}

// captureStderr redirects os.Stderr for the rest of the test and returns a
// function that yields what was written so far.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = old
		f.Close()
	})
	return func() string {
		data, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
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
	tools, err := common.CommandNames(repoRoot(t))
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
