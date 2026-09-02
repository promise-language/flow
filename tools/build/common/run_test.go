package common

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The judging layer reads the thresholds manifest, so this is where a
// measurement becomes an answer — the only place in this project that compares
// a number to a threshold. Both modes come through judge(), so a test of
// judge() is a test of what a person sees and of what the SDK is told.

// writeManifest writes a thresholds.json into dir and returns the dir.
func writeManifest(t *testing.T, manifest map[string]Threshold) string {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), data, 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// testedManifest is the manifest shape most tests use: the two metrics the
// "tested" gate reports that have thresholds, both at_most 0.
var testedManifest = map[string]Threshold{
	"failed_tests":    {Direction: AtMost, Cap: 0},
	"failed_packages": {Direction: AtMost, Cap: 0},
}

// A verdict says what was compared as well as what it concluded. Terms that
// were not applied are not in it: a verdict has to travel with what it was
// reached from, and caps a measurement never mentioned had no part in it.
func TestJudge_WithinEveryCapIsAcceptableAndSaysWhatItApplied(t *testing.T) {
	env := Envelope{Gate: "tested", Metrics: []Metric{
		Count("failed_tests", 0),
		Count("failed_packages", 0),
		Quantity("statement_coverage", 61.5, "percent"),
	}}
	acceptable, thresholds, detail := judge(env, testedManifest)
	if !acceptable {
		t.Fatalf("a complete run inside every cap was refused: %s", detail)
	}
	if len(thresholds) != 2 || thresholds["failed_tests"] != 0 || thresholds["failed_packages"] != 0 {
		t.Errorf("thresholds = %v, want exactly the two caps that applied", thresholds)
	}
	// A metric nothing caps is reported and not judged. Adding a measurement
	// must not silently start failing changes.
	if _, ok := thresholds["statement_coverage"]; ok {
		t.Error("an uncapped metric appears among the terms it was judged against")
	}
	if out := renderVerdict(env, testedManifest); !strings.Contains(out, "not judged") {
		t.Errorf("an uncapped metric is not shown as unjudged:\n%s", out)
	}
}

// Over a cap is a refusal, and the detail is the whole reason a person is told
// anything: which metric, what it measured, and the term it was judged
// against. "Measurements exceed what this project allows" sends someone back to
// the output to work out which.
func TestJudge_OverACapNamesTheMetricItsValueAndTheTerm(t *testing.T) {
	env := Envelope{Gate: "tested", Metrics: []Metric{
		Count("failed_tests", 3),
		Count("failed_packages", 0),
	}}
	acceptable, thresholds, detail := judge(env, testedManifest)
	if acceptable {
		t.Fatal("a measurement over its cap passed")
	}
	for _, want := range []string{"failed_tests", "3", "at_most 0"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail = %q, want it to carry %q", detail, want)
		}
	}
	// The cap that held is still part of what the verdict was reached from.
	if len(thresholds) != 2 {
		t.Errorf("thresholds = %v, want both applied caps", thresholds)
	}
}

// An incomplete run is never a pass and never moves a baseline — even with
// every number inside its cap. Honest numbers that understate what was checked
// are indistinguishable from an improvement unless the run says it measured
// less.
func TestJudge_AnIncompleteRunIsRefusedEvenInsideEveryCap(t *testing.T) {
	env := Envelope{
		Gate:       "tested",
		Metrics:    []Metric{Count("failed_tests", 0), Count("failed_packages", 0)},
		Incomplete: "some packages failed their tests",
	}
	acceptable, thresholds, detail := judge(env, testedManifest)
	if acceptable {
		t.Fatal("an incomplete run passed; its numbers understate what was checked")
	}
	if !strings.Contains(detail, "some packages failed their tests") {
		t.Errorf("detail = %q, want the reason the run measured less", detail)
	}
	if len(thresholds) != 2 {
		t.Errorf("thresholds = %v, want the caps that applied", thresholds)
	}
}

// A gate whose metrics nothing caps is acceptable, and the verdict says so
// rather than implying a comparison nobody made. A metric with no entry in the
// manifest is reported and not judged.
func TestJudge_NothingCappedIsAcceptableAndSaysNothingWasJudged(t *testing.T) {
	env := Envelope{Gate: "covered", Metrics: []Metric{Quantity("statement_coverage", 12.5, "percent")}}
	acceptable, thresholds, detail := judge(env, map[string]Threshold{})
	if !acceptable {
		t.Fatalf("an uncapped metric failed a verdict nobody declared: %s", detail)
	}
	if len(thresholds) != 0 {
		t.Errorf("thresholds = %v, want none — no term applied", thresholds)
	}
	if !strings.Contains(detail, "judged") {
		t.Errorf("detail = %q, want it to say nothing was judged", detail)
	}
}

// fit's measurements are reported and not judged when the manifest has no
// entry for them. The manifest format can now express floors (at_least), but
// this project's manifest has no entry for fit — populating specific floor
// values is separate follow-up work.
//
// What keeps that from being vacuous is the incomplete path, above: a machine
// whose toolchain cannot be reached is already refused without any floor.
func TestJudge_FitIsReportedAndNotJudgedWhenNoFloorExists(t *testing.T) {
	env := Envelope{Gate: "fit", Metrics: []Metric{
		Size("worktree_free_bytes", 12582912, "bytes"),
		Size("build_cache_free_bytes", 4096, "bytes"),
	}}
	acceptable, thresholds, detail := judge(env, map[string]Threshold{})
	if !acceptable {
		t.Fatalf("a complete fit run was refused by a term nobody declared: %s", detail)
	}
	if len(thresholds) != 0 {
		t.Errorf("thresholds = %v, want none — no floor declared in this manifest", thresholds)
	}
	if !strings.Contains(detail, "judged") {
		t.Errorf("detail = %q, want it to say nothing was judged", detail)
	}
	out := renderVerdict(env, map[string]Threshold{})
	for _, want := range []string{"worktree_free_bytes", "build_cache_free_bytes", "12582912 B", "not judged"} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendered verdict is missing %q:\n%s", want, out)
		}
	}
}

// The at_least direction: a metric below its floor fails; at or above passes.
func TestJudge_AtLeastDirection(t *testing.T) {
	manifest := map[string]Threshold{
		"statement_coverage": {Direction: AtLeast, Cap: 50.0},
	}
	// Below the floor: refused.
	env := Envelope{Gate: "covered", Metrics: []Metric{Quantity("statement_coverage", 30.0, "percent")}}
	acceptable, thresholds, detail := judge(env, manifest)
	if acceptable {
		t.Fatal("a measurement below its floor passed")
	}
	if !strings.Contains(detail, "at_least") {
		t.Errorf("detail = %q, want it to carry the direction", detail)
	}
	if thresholds["statement_coverage"] != 50.0 {
		t.Errorf("thresholds = %v, want the floor that applied", thresholds)
	}

	// At the floor: passes.
	env.Metrics = []Metric{Quantity("statement_coverage", 50.0, "percent")}
	acceptable, _, _ = judge(env, manifest)
	if !acceptable {
		t.Fatal("a measurement exactly at its floor was refused")
	}

	// Above the floor: passes.
	env.Metrics = []Metric{Quantity("statement_coverage", 80.0, "percent")}
	acceptable, _, _ = judge(env, manifest)
	if !acceptable {
		t.Fatal("a measurement above its floor was refused")
	}
}

// The wire form: one JSON object on stdout, whole, with both required fields.
// The SDK refuses anything else, and a missing "acceptable" would be read as a
// refusal nobody made.
func TestJudgeStdin_WritesExactlyOneVerdictObject(t *testing.T) {
	dir := writeManifest(t, testedManifest)
	env := Envelope{Gate: "tested", Metrics: []Metric{Count("failed_tests", 3), Count("failed_packages", 1)}}
	var out bytes.Buffer
	if err := JudgeStdin(dir, "tested", bytes.NewReader(marshal(t, env)), &out); err != nil {
		t.Fatalf("JudgeStdin: %v", err)
	}

	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	var wire struct {
		Acceptable *bool              `json:"acceptable"`
		Thresholds map[string]float64 `json:"thresholds"`
		Detail     string             `json:"detail"`
	}
	if err := dec.Decode(&wire); err != nil {
		t.Fatalf("the verdict does not parse: %v (%q)", err, out.String())
	}
	if dec.More() {
		t.Errorf("trailing content after the verdict: %q", out.String())
	}
	if wire.Acceptable == nil {
		t.Fatalf(`no "acceptable" field: %q`, out.String())
	}
	if *wire.Acceptable {
		t.Error("acceptable = true for three failing tests against a cap of 0")
	}
	// The terms travel with the verdict, or nobody who was not there can
	// re-check it — which is the entire reason a judge may be a tree artifact.
	if wire.Thresholds["failed_tests"] != 0 {
		t.Errorf("thresholds = %v, want the caps that were applied", wire.Thresholds)
	}
	if !strings.Contains(wire.Detail, "failed_tests") {
		t.Errorf("detail = %q, want the metric that failed", wire.Detail)
	}
}

// Acceptable is written even when it is true, and thresholds even when nothing
// applied. Omitting either would leave the SDK reading absence — and absence of
// "acceptable" is a refusal nobody made.
func TestJudgeStdin_WritesBothRequiredFieldsWhenThereIsNothingToSay(t *testing.T) {
	dir := writeManifest(t, map[string]Threshold{})
	env := Envelope{Gate: "covered", Metrics: []Metric{Quantity("statement_coverage", 12.5, "percent")}}
	var out bytes.Buffer
	if err := JudgeStdin(dir, "covered", bytes.NewReader(marshal(t, env)), &out); err != nil {
		t.Fatalf("JudgeStdin: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatalf("the verdict does not parse: %v (%q)", err, out.String())
	}
	if string(fields["acceptable"]) != "true" {
		t.Errorf("acceptable = %q, want true", fields["acceptable"])
	}
	// Not null: a null threshold set is discarded terms wearing a present
	// field, and the SDK cannot tell it from a judge that forgot.
	if string(fields["thresholds"]) != "{}" {
		t.Errorf("thresholds = %q, want an empty object", fields["thresholds"])
	}
}

// Nothing reaches stdout on any error path. A caller reads one verdict or
// none: half an object beside an error message is a second channel, and the
// two could disagree.
func TestJudgeStdin_WritesNothingWhenItCannotAnswer(t *testing.T) {
	dir := writeManifest(t, testedManifest)
	for _, c := range []struct {
		name     string
		gate     string
		envelope []byte
		says     string
	}{{
		// The judge's check, not the SDK's: only this layer knows which gate
		// the caps it is about to apply belong to.
		name:     "an envelope for another gate",
		gate:     "tested",
		envelope: []byte(`{"gate":"formatted","metrics":[]}`),
		says:     "formatted",
	}, {
		name:     "not an envelope at all",
		gate:     "tested",
		envelope: []byte("who knows"),
		says:     "not an envelope",
	}, {
		name:     "a gate this project does not have",
		gate:     "lint",
		envelope: []byte(`{"gate":"lint","metrics":[]}`),
		says:     "no gate named",
	}, {
		// A count arriving with a fractional part is not a count, and the
		// envelope decoder refuses it. Judging it anyway would compare a
		// number whose type changed against a cap for the one it replaced.
		name:     "a metric whose value contradicts its declared type",
		gate:     "tested",
		envelope: []byte(`{"gate":"tested","metrics":[{"name":"failed_tests","type":"int","value":1.5}]}`),
		says:     "failed_tests",
	}} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := JudgeStdin(dir, c.gate, bytes.NewReader(c.envelope), &out)
			if err == nil {
				t.Fatalf("JudgeStdin answered a request it could not answer: %q", out.String())
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Errorf("err = %v, want it to name %q", err, c.says)
			}
			if out.Len() != 0 {
				t.Errorf("wrote %q on an error path; a caller must read one verdict or none", out.String())
			}
		})
	}
}

// An unreadable stdin is not a refusal either, and it must not reach stdout as
// one.
func TestJudgeStdin_AnUnreadableEnvelopeIsAnErrorAndNotAVerdict(t *testing.T) {
	dir := writeManifest(t, testedManifest)
	var out bytes.Buffer
	err := JudgeStdin(dir, "tested", failingReader{}, &out)
	if err == nil {
		t.Fatal("JudgeStdin reached a verdict from an envelope it could not read")
	}
	if out.Len() != 0 {
		t.Errorf("wrote %q on an error path", out.String())
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the pipe broke") }

// The two modes reach the same verdict, because they are the same comparison.
// A project that answered "acceptable" to a runner and printed a failure to a
// person would have two thresholds wearing one name.
func TestJudgeStdin_AgreesWithWhatAPersonIsShown(t *testing.T) {
	dir := writeManifest(t, testedManifest)
	env := Envelope{Gate: "tested", Metrics: []Metric{Count("failed_tests", 2), Count("failed_packages", 1)}}
	acceptable, _, detail := judge(env, testedManifest)

	var out bytes.Buffer
	if err := JudgeStdin(dir, "tested", bytes.NewReader(marshal(t, env)), &out); err != nil {
		t.Fatalf("JudgeStdin: %v", err)
	}
	var wire struct {
		Acceptable bool   `json:"acceptable"`
		Detail     string `json:"detail"`
	}
	if err := json.Unmarshal(out.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Acceptable != acceptable || wire.Detail != detail {
		t.Errorf("the wire says (%t, %q) and the human path says (%t, %q)",
			wire.Acceptable, wire.Detail, acceptable, detail)
	}
}

// The invocation is the protocol, not this program's preferences: an unknown
// flag is refused rather than ignored, and a second name refused rather than
// dropped. A caller that meant --verdict and mistyped it must not silently get
// the measuring mode, which spawns a gate.
func TestParseRunArgs(t *testing.T) {
	for _, c := range []struct {
		args    []string
		name    string
		verdict bool
		wantErr bool
	}{
		{args: []string{"tested"}, name: "tested"},
		{args: []string{"tested", "--verdict"}, name: "tested", verdict: true},
		{args: []string{"--verdict", "tested"}, name: "tested", verdict: true},
		// Both spellings, as every other entry point here accepts.
		{args: []string{"tested", "-verdict"}, name: "tested", verdict: true},
		{args: []string{}, wantErr: true},
		{args: []string{"--verdict"}, wantErr: true},
		{args: []string{"tested", "formatted"}, wantErr: true},
		{args: []string{"tested", "--verdicts"}, wantErr: true},
		{args: []string{"tested", "--envelope"}, wantErr: true},
		{args: []string{"lint"}, wantErr: true},
	} {
		name, verdict, err := ParseRunArgs(c.args)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseRunArgs(%q) = (%q, %t, nil), want an error", c.args, name, verdict)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRunArgs(%q): %v", c.args, err)
			continue
		}
		if name != c.name || verdict != c.verdict {
			t.Errorf("ParseRunArgs(%q) = (%q, %t), want (%q, %t)", c.args, name, verdict, c.name, c.verdict)
		}
	}
}

func marshal(t *testing.T, env Envelope) []byte {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every metric over its cap is named, not just the first one found. A person
// told about one, who fixes it and re-runs, is told about the next — one round
// trip per failure, each costing whatever the gate takes to measure.
func TestJudge_NamesEveryMetricOverItsCapNotJustTheFirst(t *testing.T) {
	env := Envelope{Gate: "tested", Metrics: []Metric{
		Count("failed_tests", 3),
		Count("failed_packages", 2),
	}}
	acceptable, _, detail := judge(env, testedManifest)
	if acceptable {
		t.Fatal("two measurements over their caps passed")
	}
	for _, want := range []string{"failed_tests", "failed_packages"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail = %q, want it to carry %q too", detail, want)
		}
	}
}

// One wording for a name this project does not have, whichever entry point it
// was typed at: `bin/gate`'s parser, `bin/run`'s parser, and both of `bin/run`'s
// modes. One mistake, one sentence, one list of what does exist — a caller who
// mistyped a gate should not have to work out whether the two programs disagree
// about what a gate is.
//
// It is also WHERE each of them refuses: before anything is spawned and before
// stdout is touched. RunOneGate is asked here with no gate binary at all, and
// answers with the refusal rather than with a failure to exec one.
//
// MeasureGate is deliberately not in this set. It is reached only after a
// parser has already accepted the name, so it never answers a person.
func TestUnknownGateIsRefusedTheSameWayByEveryEntryPoint(t *testing.T) {
	dir := writeManifest(t, testedManifest)
	_, _, gateArgsErr := ParseGateArgs([]string{"lint"})
	_, _, runArgsErr := ParseRunArgs([]string{"lint"})
	var out bytes.Buffer
	judgeErr := JudgeStdin(dir, "lint", bytes.NewReader([]byte(`{"gate":"lint","metrics":[]}`)), &out)
	measureErr := RunOneGate("", "", "lint")

	if out.Len() != 0 {
		t.Errorf("the judging mode wrote %q for a gate this project does not have", out.String())
	}
	for _, c := range []struct {
		who string
		err error
	}{
		{"ParseGateArgs", gateArgsErr},
		{"ParseRunArgs", runArgsErr},
		{"JudgeStdin", judgeErr},
		{"RunOneGate", measureErr},
	} {
		if c.err == nil {
			t.Fatalf("%s accepted a gate this project does not have", c.who)
		}
		if c.err.Error() != gateArgsErr.Error() {
			t.Errorf("%s says %q, and ParseGateArgs says %q", c.who, c.err, gateArgsErr)
		}
	}
	// The sentence has to carry both halves, or the reader is told only that
	// they are wrong and not what would have been right.
	for _, want := range []string{`"lint"`, "tested"} {
		if !strings.Contains(gateArgsErr.Error(), want) {
			t.Errorf("the refusal is %q, want it to carry %s", gateArgsErr, want)
		}
	}
}

// CappedMetrics reads the manifest — not the judging path, just the usage text.
// It must not crash on a missing root or a missing file, and must sort the names
// it returns.
func TestCappedMetrics(t *testing.T) {
	// Empty repoRoot: usage text before the repo is known.
	if got := CappedMetrics(""); got != nil {
		t.Errorf("CappedMetrics(\"\") = %v, want nil", got)
	}
	// No manifest file: returns nil rather than crashing.
	if got := CappedMetrics(t.TempDir()); got != nil {
		t.Errorf("CappedMetrics(no manifest) = %v, want nil", got)
	}
	// Valid manifest: returns sorted names.
	dir := writeManifest(t, map[string]Threshold{
		"vet_findings":    {Direction: AtMost, Cap: 0},
		"failed_tests":    {Direction: AtMost, Cap: 0},
		"failed_packages": {Direction: AtMost, Cap: 0},
	})
	got := CappedMetrics(dir)
	want := []string{"failed_packages", "failed_tests", "vet_findings"}
	if len(got) != len(want) {
		t.Fatalf("CappedMetrics = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("CappedMetrics[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// JudgeStdin with a missing manifest must return an error and write nothing to
// stdout. Without this, removing the loadManifest call would silently pass
// everything — the zero-threshold path is "nothing judged: acceptable".
func TestJudgeStdin_MissingManifestIsAnErrorAndNotAVerdict(t *testing.T) {
	dir := t.TempDir() // no thresholds.json
	env := Envelope{Gate: "tested", Metrics: []Metric{Count("failed_tests", 3)}}
	var out bytes.Buffer
	err := JudgeStdin(dir, "tested", bytes.NewReader(marshal(t, env)), &out)
	if err == nil {
		t.Fatal("JudgeStdin answered when the manifest was missing")
	}
	if out.Len() != 0 {
		t.Errorf("wrote %q on an error path; a caller must read one verdict or none", out.String())
	}
}

// renderVerdict must show the direction and the correct mark for at_least
// metrics. A passing floor shows ✓; a failing one shows ✗. Without this,
// the at_least rendering branches in renderVerdict are untested.
func TestRenderVerdict_AtLeastDirectionShowsCorrectMark(t *testing.T) {
	manifest := map[string]Threshold{
		"statement_coverage": {Direction: AtLeast, Cap: 50.0},
	}
	// Above the floor: passing mark.
	env := Envelope{Gate: "covered", Metrics: []Metric{Quantity("statement_coverage", 80.0, "percent")}}
	out := renderVerdict(env, manifest)
	if !strings.Contains(out, "✓") {
		t.Errorf("a metric above its floor should show ✓:\n%s", out)
	}
	if !strings.Contains(out, "at_least") {
		t.Errorf("the rendered verdict should show the direction:\n%s", out)
	}
	// Below the floor: failing mark.
	env.Metrics = []Metric{Quantity("statement_coverage", 30.0, "percent")}
	out = renderVerdict(env, manifest)
	if !strings.Contains(out, "✗") {
		t.Errorf("a metric below its floor should show ✗:\n%s", out)
	}
}

// renderVerdict must show ✓ for a passing at_most metric — the marks are
// direction-specific, and the existing tests never assert them.
func TestRenderVerdict_AtMostDirectionShowsCorrectMark(t *testing.T) {
	out := renderVerdict(
		Envelope{Gate: "tested", Metrics: []Metric{Count("failed_tests", 0)}},
		testedManifest,
	)
	if !strings.Contains(out, "✓") {
		t.Errorf("a metric at its at_most cap should show ✓:\n%s", out)
	}
	if !strings.Contains(out, "at_most") {
		t.Errorf("the rendered verdict should show the direction:\n%s", out)
	}
	// Over the cap: ✗.
	out = renderVerdict(
		Envelope{Gate: "tested", Metrics: []Metric{Count("failed_tests", 5)}},
		testedManifest,
	)
	if !strings.Contains(out, "✗") {
		t.Errorf("a metric over its at_most cap should show ✗:\n%s", out)
	}
}

// --- Manifest loading tests ---

func TestLoadManifest_Valid(t *testing.T) {
	dir := writeManifest(t, map[string]Threshold{
		"failed_tests":       {Direction: AtMost, Cap: 0},
		"statement_coverage": {Direction: AtLeast, Cap: 50.0},
	})
	manifest, err := loadManifest(dir)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if len(manifest) != 2 {
		t.Fatalf("len = %d, want 2", len(manifest))
	}
	if manifest["failed_tests"].Direction != AtMost || manifest["failed_tests"].Cap != 0 {
		t.Errorf("failed_tests = %+v", manifest["failed_tests"])
	}
	if manifest["statement_coverage"].Direction != AtLeast || manifest["statement_coverage"].Cap != 50.0 {
		t.Errorf("statement_coverage = %+v", manifest["statement_coverage"])
	}
}

func TestLoadManifest_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := loadManifest(dir)
	if err == nil {
		t.Fatal("loadManifest succeeded without a manifest file")
	}
}

func TestLoadManifest_UnknownDirection(t *testing.T) {
	dir := t.TempDir()
	data := []byte(`{"x": {"direction": "around", "cap": 5}}`)
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), data, 0644); err != nil {
		t.Fatal(err)
	}
	_, err := loadManifest(dir)
	if err == nil {
		t.Fatal("loadManifest accepted an unknown direction")
	}
	if !strings.Contains(err.Error(), "around") {
		t.Errorf("err = %v, want it to name the bad direction", err)
	}
}

func TestLoadManifest_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := loadManifest(dir)
	if err == nil {
		t.Fatal("loadManifest accepted malformed JSON")
	}
}
