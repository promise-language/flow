package main

// This repository's CI is committed configuration that nothing here compiles,
// so before GitHub reads it a test is the only thing that does. These live
// beside TestCommittedHooks_NameOnlySuppliedBinaries — the other test of
// committed configuration — and share its repoRoot.
//
// What they are about (#277): an action that declares `runs.using: node20`
// produces a deprecation warning and no failure. The workflow stays green, so
// a pin that slides back to a Node 20 major is invisible to the very run that
// would report it, and nothing else in this tree would notice.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// noNode24 marks an action with no release declaring a Node 24 runtime at all.
// Such an action can only be carried by the opt-in below; it cannot be bumped
// onto one.
const noNode24 = 0

// firstNode24Major is the first major of each action pinned in this
// repository's workflows that declares a native Node 24 runtime (`runs.using:
// node24`). #277 read these off each action's tags rather than assuming them,
// and that is the only way to get one: it is a fact about a release on
// GitHub, not about anything in this tree.
//
// Hand-maintained on purpose: an action missing from here fails the test
// rather than being waved through, because the one thing worth knowing about
// a new action is the runtime it declares, and nothing offline can look it up.
var firstNode24Major = map[string]int{
	"actions/checkout": 5,
	"actions/setup-go": 6,
	// v2.6.1 and the action's main both declare node20; no newer release
	// exists. cla.yml carries it with the opt-in and states the dates.
	"contributor-assistant/github-action": noNode24,
}

// node24OptIns are the environment variables that run a JavaScript action on
// Node 24 regardless of the runtime it declares. They are how a workflow keeps
// an action that has nowhere to be bumped to — not an alternative to bumping
// one that does.
var node24OptIns = []string{
	"FORCE_JAVASCRIPT_ACTIONS_TO_NODE24",
	"ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION",
}

// pin is a step's action reference: `owner/repo` and whatever followed the `@`.
type pin struct {
	action string
	ref    string
}

// workflow is the part of a workflow file these tests are about.
type workflow struct {
	rel    string                       // path relative to the repository root
	pins   []pin                        // every action it uses, in file order
	inputs map[string]map[string]string // action -> its `with:` key -> value
	optIns []string                     // Node-runtime opt-ins it sets
}

var (
	// A step's pin, written either as the first key of a list item or after it.
	usesLine = regexp.MustCompile(`^\s*(?:-\s+)?uses:\s*(\S+)`)
	// A list item that is not a pin — it ends the step above it.
	itemLine = regexp.MustCompile(`^\s*-\s`)
	// A mapping key and its value — how `with:` inputs and `env:` names read.
	mappingLine = regexp.MustCompile(`^\s*([A-Za-z0-9_.-]+):\s*(.*)$`)
	// A ref that names a major: v5, v7.0.1. A SHA names none.
	majorRef = regexp.MustCompile(`^v(\d+)(?:[.\-+].*)?$`)
)

// parseWorkflows reads every workflow file in this repository.
//
// Line-scanned rather than parsed: this module has no dependencies, and these
// are forty lines of two-space mappings. What that costs is precision about
// which step a key belongs to, so the attribution is the narrow one — a
// `with:` key belongs to the pin above it within the same list item.
func parseWorkflows(t *testing.T) []workflow {
	t.Helper()
	rel := filepath.Join(".github", "workflows")
	dir := filepath.Join(repoRoot(t), rel)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}

	var out []workflow
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", filepath.Join(rel, e.Name()), err)
		}

		w := workflow{
			rel:    filepath.ToSlash(filepath.Join(rel, e.Name())),
			inputs: map[string]map[string]string{},
		}
		current := "" // the action whose list item we are inside
		for _, raw := range strings.Split(string(data), "\n") {
			// A commented-out pin is not a pin, and a comment naming an
			// environment variable does not set one.
			if trimmed := strings.TrimSpace(raw); trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if m := usesLine.FindStringSubmatch(raw); m != nil {
				action, ref, _ := strings.Cut(unquote(m[1]), "@")
				w.pins = append(w.pins, pin{action: action, ref: ref})
				current = action
				continue
			}
			// A list item that is not a pin ends the step above it, so its
			// keys are not attributed to that step's action.
			if itemLine.MatchString(raw) {
				current = ""
			}
			m := mappingLine.FindStringSubmatch(raw)
			if m == nil {
				continue
			}
			key, value := m[1], unquote(stripComment(m[2]))
			for _, name := range node24OptIns {
				if key == name {
					w.optIns = append(w.optIns, name)
				}
			}
			if current != "" && value != "" {
				if w.inputs[current] == nil {
					w.inputs[current] = map[string]string{}
				}
				w.inputs[current][key] = value
			}
		}
		out = append(out, w)
	}
	return out
}

// stripComment drops a trailing `# ...` from a scalar value.
func stripComment(v string) string {
	if before, _, found := strings.Cut(v, " #"); found {
		return strings.TrimSpace(before)
	}
	return v
}

func unquote(v string) string {
	return strings.Trim(strings.TrimSpace(v), `'"`)
}

// An action with a native Node 24 release is pinned at one. This is the whole
// of #277: `actions/checkout@v4` and `actions/setup-go@v5` declare node20,
// both have majors that declare node24, and a run on the old pins is green.
func TestWorkflowActions_PinnedWhereANativeNode24ReleaseExists(t *testing.T) {
	seen := 0
	for _, w := range parseWorkflows(t) {
		for _, p := range w.pins {
			seen++
			first, known := firstNode24Major[p.action]
			if !known {
				t.Errorf("%s pins %s, which firstNode24Major does not list — read its action.yml `runs.using` and add it, because nothing offline can look that up",
					w.rel, p.action)
				continue
			}
			if first == noNode24 {
				continue // has no major to be pinned at; the opt-in test below covers it
			}
			m := majorRef.FindStringSubmatch(p.ref)
			if m == nil {
				t.Errorf("%s pins %s@%s, whose major cannot be read — pin a major, because a node24 release cannot be told from a node20 one by the ref alone",
					w.rel, p.action, p.ref)
				continue
			}
			major, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("%s: major of %s: %v", w.rel, p.ref, err)
			}
			if major < first {
				t.Errorf("%s pins %s@%s: v%d is the first major declaring a native Node 24 runtime, and a node20 action warns rather than failing, so the run that would report this stays green",
					w.rel, p.action, p.ref, first)
			}
		}
	}
	if seen == 0 {
		// Not a pass: either the workflows stopped pinning actions, or the
		// scan stopped recognising a pin. Either way nothing above was checked.
		t.Error("no action pin found in any workflow — this test is no longer looking at anything")
	}
}

// The opt-in is set exactly where an action has nowhere to be bumped to.
//
// Both directions matter. Missing, a node20 action loses the runtime under it
// once Node 20 is removed. Present where every action pinned has a native
// Node 24 release, it holds the workaround in place of the upgrade — the fix
// #277 rejects for integration.yml, and the one that would otherwise let the
// test above pass over a reverted pin.
func TestWorkflowNode24OptIn_SetOnlyWhereNoNativeReleaseExists(t *testing.T) {
	for _, w := range parseWorkflows(t) {
		var stranded []string
		for _, p := range w.pins {
			if first, known := firstNode24Major[p.action]; known && first == noNode24 {
				stranded = append(stranded, p.action)
			}
		}
		switch {
		case len(stranded) > 0 && len(w.optIns) == 0:
			t.Errorf("%s pins %s, which has no release declaring a Node 24 runtime, and sets none of %v — the action loses the runtime under it",
				w.rel, strings.Join(stranded, ", "), node24OptIns)
		case len(stranded) == 0 && len(w.optIns) > 0:
			t.Errorf("%s sets %v while every action it pins has a native Node 24 release — bump the pin instead: an opt-in here keeps the workaround and hides the pin that needed moving",
				w.rel, w.optIns)
		}
	}
}

// go.mod is the single source of the toolchain version, so bumping it moves CI
// too. The regression this guards is silent: with no version input, setup-go
// installs its own default and the Go toolchain fetches what go.mod asks for
// anyway, so CI is green while its Go version is no longer this repository's.
func TestWorkflows_TakeTheGoToolchainFromGoMod(t *testing.T) {
	seen := 0
	for _, w := range parseWorkflows(t) {
		with, ok := w.inputs["actions/setup-go"]
		if !ok {
			// A workflow that does not set up Go has nothing to say here; one
			// that does so with no inputs at all takes the runner's default.
			for _, p := range w.pins {
				if p.action == "actions/setup-go" {
					t.Errorf("%s pins actions/setup-go and gives it no inputs — its Go version is the runner's default, not this repository's", w.rel)
				}
			}
			continue
		}
		seen++
		if v, ok := with["go-version"]; ok {
			t.Errorf("%s gives actions/setup-go go-version %q — a second copy of the toolchain version, which go.mod cannot move", w.rel, v)
		}
		file := with["go-version-file"]
		if file != "go.mod" {
			t.Errorf("%s reads the Go version from go-version-file %q, want \"go.mod\"", w.rel, file)
			continue
		}
		if _, err := os.Stat(filepath.Join(repoRoot(t), file)); err != nil {
			t.Errorf("%s reads the Go version from %s, which is not there: %v", w.rel, file, err)
		}
	}
	if seen == 0 {
		t.Error("no workflow gives actions/setup-go any input — this test is no longer looking at anything")
	}
}
