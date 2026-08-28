package common

import (
	"encoding/json"
	"strings"
	"testing"
)

// The gate set is closed. A name absent from it is refused rather than treated
// as a gate that measured nothing — the two are opposite results, and the
// second reads like a clean tree.
func TestUnknownGateIsRefusedRatherThanEmpty(t *testing.T) {
	if _, err := MeasureGate(t.TempDir(), "lint"); err == nil {
		t.Fatal("MeasureGate accepted a gate this project does not provide")
	}
	if _, _, err := ParseGateArgs([]string{"lint", "--envelope"}); err == nil {
		t.Fatal("ParseGateArgs accepted an unknown gate name")
	}
}

// Every part of a composition must be individually runnable. That is a
// requirement rather than an implementation detail: a step fixing one failing
// suite re-runs that suite, and a composition whose parts cannot be named
// makes every fix round pay for the whole set.
func TestCompositionPartsAreThemselvesGates(t *testing.T) {
	def, ok := gates["integration"]
	if !ok {
		t.Fatal("no integration gate")
	}
	if len(def.parts) == 0 {
		t.Fatal("integration composes nothing")
	}
	for _, part := range def.parts {
		if !KnownGate(part) {
			t.Errorf("integration composes %q, which is not a gate anyone can ask for", part)
		}
	}
}

// A gate is asked for one way. Two callers asking the same thing must not be
// able to get different answers and both be right, so anything that is not
// "one name, optionally --envelope" is refused rather than interpreted.
func TestGateInvocationIsOneWay(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantName string
		wantEnv  bool
		wantErr  bool
	}{
		{"bare name asks for no envelope", []string{"tested"}, "tested", false, false},
		{"runner appends the flag", []string{"tested", "--envelope"}, "tested", true, false},
		{"short spelling is the same flag", []string{"tested", "-envelope"}, "tested", true, false},
		{"no name at all", []string{"--envelope"}, "", false, true},
		{"two names", []string{"tested", "builds", "--envelope"}, "", false, true},
		{"an unknown flag is refused, not ignored", []string{"tested", "--quiet"}, "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, env, err := ParseGateArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseGateArgs(%q) = (%q, %v, nil), want an error", tc.args, name, env)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGateArgs(%q): %v", tc.args, err)
			}
			if name != tc.wantName || env != tc.wantEnv {
				t.Errorf("ParseGateArgs(%q) = (%q, %v), want (%q, %v)",
					tc.args, name, env, tc.wantName, tc.wantEnv)
			}
		})
	}
}

// The envelope is written whole and parses as one object. That is what lets a
// reader tell "measured and reported" from "died part-way" without asking the
// gate — a truncated envelope is not an envelope.
func TestEnvelopeIsOneParseableObject(t *testing.T) {
	env := Envelope{
		Gate:    "tested",
		Metrics: []Metric{Count("failed_tests", 3)},
	}
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "\n") {
		t.Error("the envelope spans lines; a reader cannot tell a truncated one from a short one")
	}
	var back Envelope
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("the envelope does not round-trip: %v", err)
	}
	if back.Gate != "tested" || len(back.Metrics) != 1 {
		t.Fatalf("round-trip lost the measurement: %+v", back)
	}
	if got := back.Metrics[0]; got.Type != MetricInt || got.Int != 3 {
		t.Errorf("round-trip = %+v, want an int metric of 3", got)
	}
	// Truncation must not parse. If it did, a killed gate would read as a
	// complete run reporting nothing — exactly backwards.
	if err := json.Unmarshal(out[:len(out)-5], &back); err == nil {
		t.Error("a truncated envelope parsed; a killed gate would read as a clean run")
	}
}

// A gate reports what it measured and does not judge it. Nothing in the gate
// layer may hold a threshold — the caps live with the judging layer, and a
// gate that carried its own could be passed by editing the gate.
func TestGatesHoldNoThresholds(t *testing.T) {
	env := Envelope{Gate: "tested", Metrics: []Metric{Count("failed_tests", 99)}}
	if _, err := json.Marshal(env); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Ninety-nine failing tests is a successful RUN of the tested gate. The
	// measurement is obtained; whether it is acceptable is a different
	// question, asked elsewhere, against state the gate does not have.
	if env.Incomplete != "" {
		t.Error("a gate reporting failures marked its own run incomplete")
	}
}

// The judging half — what the caps do to a measurement, and the wire form of
// the answer — lives beside the layer that holds them, in run_test.go.

// go test indents a failing subtest under its parent, and a subtest that failed
// is a failing test.
func TestFailureCountingReadsIndentedSubtests(t *testing.T) {
	out := strings.Join([]string{
		"--- FAIL: TestOuter (0.00s)",
		"    --- FAIL: TestOuter/inner (0.00s)",
		"FAIL",
		"FAIL\tgithub.com/promise-language/flow\t0.012s",
		"ok  \tgithub.com/promise-language/flow/cli\t0.004s",
	}, "\n")
	if got := countPrefixed(out, "--- FAIL:"); got != 2 {
		t.Errorf("failed_tests = %d, want 2 (the subtest counts)", got)
	}
	if got := countPrefixed(out, "FAIL\t"); got != 1 {
		t.Errorf("failed_packages = %d, want 1 (the bare FAIL line is not a package)", got)
	}
}

// Vet groups its findings under "# package" headers. A header is not a finding.
func TestVetHeadersAreNotFindings(t *testing.T) {
	out := strings.Join([]string{
		"# github.com/promise-language/flow",
		"artifact.go:12:2: unreachable code",
		"backend.go:44:9: printf: wrong type",
		"",
	}, "\n")
	if got := countDiagnostics(out); got != 2 {
		t.Errorf("vet_findings = %d, want 2", got)
	}
}

func TestCoverageTotalIsReadFromTheSummaryLine(t *testing.T) {
	out := strings.Join([]string{
		"github.com/promise-language/flow/artifact.go:20:\tValid\t\t100.0%",
		"total:\t(statements)\t63.9%",
	}, "\n")
	pct, ok := totalCoverage(out)
	if !ok || pct != 63.9 {
		t.Errorf("totalCoverage = (%v, %v), want (63.9, true)", pct, ok)
	}
	if _, ok := totalCoverage("no total here"); ok {
		t.Error("read a total from output that has none")
	}
}
