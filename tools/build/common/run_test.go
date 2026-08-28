package common

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The judging layer holds the caps, so this is where a measurement becomes an
// answer — the only place in this project that compares a number to a
// threshold. Both modes come through judge(), so a test of judge() is a test of
// what a person sees and of what the SDK is told.

// A verdict says what was compared as well as what it concluded. Terms that
// were not applied are not in it: a verdict has to travel with what it was
// reached from, and caps a measurement never mentioned had no part in it.
func TestJudge_WithinEveryCapIsAcceptableAndSaysWhatItApplied(t *testing.T) {
	env := Envelope{Gate: "tested", Metrics: []Metric{
		Count("failed_tests", 0),
		Count("failed_packages", 0),
		Quantity("statement_coverage", 61.5, "percent"),
	}}
	acceptable, thresholds, detail := judge(env)
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
	if out := renderVerdict(env); !strings.Contains(out, "not judged") {
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
	acceptable, thresholds, detail := judge(env)
	if acceptable {
		t.Fatal("a measurement over its cap passed")
	}
	for _, want := range []string{"failed_tests", "3", "cap 0"} {
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
	acceptable, thresholds, detail := judge(env)
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
// rather than implying a comparison nobody made. The caps table is a
// placeholder (#38); a metric with no entry is reported and not judged.
func TestJudge_NothingCappedIsAcceptableAndSaysNothingWasJudged(t *testing.T) {
	env := Envelope{Gate: "covered", Metrics: []Metric{Quantity("statement_coverage", 12.5, "percent")}}
	acceptable, thresholds, detail := judge(env)
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

// The wire form: one JSON object on stdout, whole, with both required fields.
// The SDK refuses anything else, and a missing "acceptable" would be read as a
// refusal nobody made.
func TestJudgeStdin_WritesExactlyOneVerdictObject(t *testing.T) {
	env := Envelope{Gate: "tested", Metrics: []Metric{Count("failed_tests", 3), Count("failed_packages", 1)}}
	var out bytes.Buffer
	if err := JudgeStdin("tested", bytes.NewReader(marshal(t, env)), &out); err != nil {
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
	env := Envelope{Gate: "covered", Metrics: []Metric{Quantity("statement_coverage", 12.5, "percent")}}
	var out bytes.Buffer
	if err := JudgeStdin("covered", bytes.NewReader(marshal(t, env)), &out); err != nil {
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
			err := JudgeStdin(c.gate, bytes.NewReader(c.envelope), &out)
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
	var out bytes.Buffer
	err := JudgeStdin("tested", failingReader{}, &out)
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
	env := Envelope{Gate: "tested", Metrics: []Metric{Count("failed_tests", 2), Count("failed_packages", 1)}}
	acceptable, _, detail := judge(env)

	var out bytes.Buffer
	if err := JudgeStdin("tested", bytes.NewReader(marshal(t, env)), &out); err != nil {
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
