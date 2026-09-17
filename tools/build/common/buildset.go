package common

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// What this project builds, and what it answers — the two halves of the one
// question `bin/run --list` exists to answer (the workspace's
// docs/generic-projects.md § 4).
//
// The build set is ASKED of the entry point rather than read out of a directory
// by whoever wants to know. A reader that inspected tools/build/cmd/ would be
// assuming Go and one directory per command, and a project that builds its
// tools any other way could not declare anything at all. Here, inside the
// project, reading that listing is exactly right — it is this project's own
// layout — and it is read ONCE, by CommandNames, so the set ./make builds and
// the set `run --list` reports cannot differ.

// MetaBuilderName is the one command under cmd/ that is never compiled into
// bin/: it runs via `go run` from the ./make trampoline, which is what breaks
// the bootstrap cycle.
const MetaBuilderName = "make"

// commandDir is where this project keeps one main per command, relative to the
// repo root.
var commandDir = filepath.Join("tools", "build", "cmd")

// CommandNames returns the commands this project builds, sorted and unsuffixed.
//
// The listing IS the registry — there is no list anywhere to keep in step with
// it, so adding a command is adding a directory and retiring one is deleting it
// (#199 retired `guard` that way); the next ./make removes the binary it had
// built for the retired name (see pruneRetired). A file under cmd/ is not a
// command: `go build ./cmd/<name>` wants a package.
//
// Unsuffixed because the caller applies the host's executable suffix. The
// answer travels to readers asking a different question than the one
// BinaryName was written for.
//
// It reports what the project BUILDS, not what is built: bin/ is empty in a
// fresh clone and the claim holds there too, which is the whole reason a
// missing binary is diagnosable rather than a contradiction.
func CommandNames(repoRoot string) ([]string, error) {
	dir := filepath.Join(repoRoot, commandDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A directory that cannot be read is NOT an empty set, and an absent
		// one is not either: this directory IS the registry, so a checkout
		// without it is one that cannot say what it builds. Answering "none"
		// would take ./make's up-to-date short circuit — nothing to build,
		// every expected binary present, "Tools up to date" — over a bin/ that
		// a fresh clone leaves empty, and would pass every collision the set
		// exists to catch.
		return nil, fmt.Errorf("reading the build set at %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != MetaBuilderName {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// BuildList is what `run --list` answers.
//
// The two kinds stay separable and must not be flattened: a twin check is about
// binaries and a gate is not one, so a merged list would compare a tool name
// against a gate name and report a collision that cannot exist.
type BuildList struct {
	Commands []string `json:"commands"`
	Gates    []string `json:"gates"`
}

// CollectBuildList answers both kinds, and refuses a name that is both.
//
// One namespace, because `run <name>` addresses both. A collision is an error
// rather than a precedence rule: precedence would make the shadowed name
// silently unreachable while both still appeared in the list, and no reader
// could see which one it had reached.
//
// The gates come from GateNames — the gate entry point's own enumeration, which
// this program is built from — rather than from a spawn. `--list` spawns
// nothing and judges nothing, so a listing that shelled out to bin/gate would
// answer nothing in a checkout where the gate binary is not built yet, which is
// exactly the checkout a build set is asked about.
func CollectBuildList(repoRoot string) (BuildList, error) {
	commands, err := CommandNames(repoRoot)
	if err != nil {
		return BuildList{}, err
	}
	gates := GateNames()
	var both []string
	for _, c := range commands {
		if slices.Contains(gates, c) {
			both = append(both, c)
		}
	}
	if len(both) > 0 {
		return BuildList{}, fmt.Errorf(
			"%s is both a command this project builds and a gate it answers, so `run %s` means one of two things — rename one of them",
			strings.Join(both, ", "), both[0])
	}
	// Never nil: an empty array at exit 0 is this project stating it builds
	// none, which is definitive, where JSON null would read as the unknown that
	// only an unanswerable entry point is.
	if commands == nil {
		commands = []string{}
	}
	if gates == nil {
		gates = []string{}
	}
	return BuildList{Commands: commands, Gates: gates}, nil
}

// WriteBuildList renders the answer. Stdout carries the result and nothing else
// (docs/org/cli-guide.md § 6); this writes no narration at all.
func WriteBuildList(w io.Writer, mode OutputMode, list BuildList) error {
	if mode == OutputJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(list)
	}
	// Two labelled groups, one name per line under each, so a reader tells the
	// kinds apart without consulting the JSON.
	for _, group := range []struct {
		label string
		names []string
	}{
		{"commands", list.Commands},
		{"gates", list.Gates},
	} {
		if _, err := fmt.Fprintf(w, "%s:\n", group.label); err != nil {
			return err
		}
		for _, n := range group.names {
			if _, err := fmt.Fprintf(w, "  %s\n", n); err != nil {
				return err
			}
		}
	}
	return nil
}

// ParseListArgs reads a listing invocation: the -list flag, the output flags,
// and nothing else.
//
// The query TAKES NO ARGUMENT. One that accepted a name would invite being read
// as a filter, and a caller reading a filtered list as the build set would see
// a collision that is not there. An unknown flag is refused rather than
// ignored, for the reason ParseRunArgs refuses one.
//
// It reports whether a listing was asked for at all, so a caller can fall
// through to its other modes without scanning the arguments a second time.
func ParseListArgs(args []string) (asked bool, of OutputFlags, err error) {
	if !slices.Contains(args, "-list") {
		return false, OutputFlags{}, nil
	}
	rest, of := TakeOutputFlags(args)
	rest = slices.DeleteFunc(rest, func(a string) bool { return a == "-list" })
	if len(rest) > 0 {
		return true, of, fmt.Errorf("--list takes no argument, got %q", rest[0])
	}
	return true, of, nil
}
