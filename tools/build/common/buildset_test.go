package common

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// cmdTree lays out a repo root holding one directory per command.
func cmdTree(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "tools", "build", "cmd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCommandNames_ListsDirectoriesExceptTheMetaBuilder(t *testing.T) {
	root := cmdTree(t, "verify", "make", "gate")
	// A file is not a command: `go build ./cmd/notes.md` is not a build.
	notes := filepath.Join(root, "tools", "build", "cmd", "notes.md")
	if err := os.WriteFile(notes, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := CommandNames(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "gate,verify" {
		t.Errorf("CommandNames() = %v, want [gate verify] — make runs from source and a file is not a package", got)
	}
}

// A cmd directory that cannot be read is not an empty command set. Reporting
// none with no error would take ./make's up-to-date short circuit — nothing to
// build, every expected binary present, "Tools up to date" — and leave bin/
// however it was found, which for a fresh clone is empty.
func TestCommandNames_UnreadableDirectoryIsAnError(t *testing.T) {
	if _, err := CommandNames(t.TempDir()); err == nil {
		t.Error("CommandNames() succeeded on a missing cmd directory; make would report every tool up to date having built none")
	}
}

// The listing is what the project BUILDS, not what is built: nothing here has
// ever run ./make, and the claim is still the answer.
func TestCollectBuildList_AnswersInAnUnbuiltCheckout(t *testing.T) {
	root := cmdTree(t, "gate", "make", "run", "verify")

	list, err := CollectBuildList(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(list.Commands, ",") != "gate,run,verify" {
		t.Errorf("Commands = %v, want [gate run verify]", list.Commands)
	}
	// The gates are the gate entry point's own enumeration, which is what a
	// runner asking this program will go on to ask bin/gate for.
	if !slices.Equal(list.Gates, GateNames()) {
		t.Errorf("Gates = %v, want %v — run takes its gates from the gate entry point", list.Gates, GateNames())
	}
}

// One namespace, because `run <name>` addresses both kinds. Precedence would
// make the shadowed name silently unreachable while both still appeared in the
// list.
func TestCollectBuildList_RefusesANameThatIsBoth(t *testing.T) {
	root := cmdTree(t, "run", GateNames()[0])

	_, err := CollectBuildList(root)
	if err == nil {
		t.Fatalf("CollectBuildList() accepted %q as a command and a gate; `run %[1]s` would mean one of two things", GateNames()[0])
	}
	if !strings.Contains(err.Error(), GateNames()[0]) {
		t.Errorf("the refusal does not name the colliding name: %v", err)
	}
}

// Both kinds are always arrays. JSON null would read as the unknown that only
// an entry point nobody could ask is; an empty array is this project stating it
// builds none.
func TestWriteBuildList_JSONIsAnObjectOfTwoArrays(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteBuildList(&buf, OutputJSON, BuildList{Commands: []string{}, Gates: []string{}}); err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("the listing is not one JSON object: %v (%s)", err, buf.String())
	}
	for _, key := range []string{"commands", "gates"} {
		raw, ok := got[key]
		if !ok {
			t.Fatalf("the listing carries no %q: %s", key, buf.String())
		}
		if string(raw) != "[]" {
			t.Errorf("%q = %s, want [] — an empty list is an answer and null is not", key, raw)
		}
	}
}

// A reader tells the kinds apart without consulting the JSON.
func TestWriteBuildList_HumanIsTwoLabelledGroups(t *testing.T) {
	var buf bytes.Buffer
	list := BuildList{Commands: []string{"verify"}, Gates: []string{"tested"}}
	if err := WriteBuildList(&buf, OutputHuman, list); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "commands:\n  verify\ngates:\n  tested\n"; got != want {
		t.Errorf("human listing =\n%q\nwant\n%q", got, want)
	}
}

func TestParseListArgs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantAsked bool
		wantJSON  bool
		wantErr   bool
	}{
		{name: "no listing asked", args: []string{"tested"}},
		{name: "bare", args: []string{"-list"}, wantAsked: true},
		{name: "json after", args: []string{"-list", "-json"}, wantAsked: true, wantJSON: true},
		// The flags are stripped wherever they sit: these tools take their one
		// positional argument in any position, so the two orders must mean the
		// same thing.
		{name: "json before", args: []string{"-json", "-list"}, wantAsked: true, wantJSON: true},
		// A query that accepted a name would invite being read as a filter.
		{name: "with a name", args: []string{"-list", "tested"}, wantAsked: true, wantErr: true},
		{name: "with an unknown flag", args: []string{"-list", "-envelope"}, wantAsked: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked, of, err := ParseListArgs(tc.args)
			if asked != tc.wantAsked {
				t.Errorf("asked = %v, want %v", asked, tc.wantAsked)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error: %v", err, tc.wantErr)
			}
			if err == nil && of.JSON != tc.wantJSON {
				t.Errorf("JSON flag = %v, want %v", of.JSON, tc.wantJSON)
			}
		})
	}
}

// Passing both is a contradiction rather than a precedence puzzle, and it is
// reported before anything is rendered.
func TestOutputFlags_BothIsAUsageError(t *testing.T) {
	if _, err := (OutputFlags{JSON: true, Human: true}).Mode(); err == nil {
		t.Error("-json -human was accepted; one of the two would have been silently dropped")
	}
	for _, tc := range []struct {
		of   OutputFlags
		want OutputMode
	}{
		{OutputFlags{JSON: true}, OutputJSON},
		{OutputFlags{Human: true}, OutputHuman},
	} {
		got, err := tc.of.Mode()
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("Mode(%+v) = %v, want %v", tc.of, got, tc.want)
		}
	}
}

// This repository's own answer, which is what a consumer reads: the tools
// contract names verify, gate and run, and a set missing one of them is a
// checkout nothing can drive.
func TestCommandNames_ThisRepositoryDeclaresTheContractTools(t *testing.T) {
	root := repoRootFromTestFile(t)
	names, err := CommandNames(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gate", "run", "verify"} {
		if !slices.Contains(names, want) {
			t.Errorf("CommandNames() = %v, which does not declare %q", names, want)
		}
	}
	if slices.Contains(names, MetaBuilderName) {
		t.Errorf("CommandNames() declares %q, which runs from source and is never in bin/", MetaBuilderName)
	}
}

// repoRootFromTestFile is two levels up from tools/build/common.
func repoRootFromTestFile(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(wd)))
}
