package common

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
		// fit is reached the same way every gate is: nothing about a gate whose
		// subject is the machine changes how it is asked for, and a bare
		// `bin/gate fit` is refused an envelope by this same rule.
		{"the machine gate is asked for like any other", []string{"fit", "--envelope"}, "fit", true, false},
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

// fit is the one gate here whose subject is not the code. It reports the space
// on both filesystems this project's work writes to, every run — the worktree
// and the build cache are two requirements and are often not one device, and an
// envelope whose shape varied by host is one no threshold can be written
// against.
//
// Shape, not magnitude: free space is not a constant a test may assert.
func TestFitIsAGateAndReportsTwoFilesystems(t *testing.T) {
	env, err := MeasureGate(t.TempDir(), "fit")
	if err != nil {
		t.Fatalf("MeasureGate(fit): %v", err)
	}
	if env.Gate != "fit" {
		t.Errorf("gate = %q, want fit", env.Gate)
	}
	if env.Incomplete != "" {
		t.Fatalf("a machine with a working toolchain reported an incomplete run: %s", env.Incomplete)
	}
	byName := map[string]Metric{}
	for _, m := range env.Metrics {
		byName[m.Name] = m
	}
	for _, want := range []string{"worktree_free_bytes", "build_cache_free_bytes"} {
		m, ok := byName[want]
		if !ok {
			t.Errorf("no %s metric; the envelope is %+v", want, env.Metrics)
			continue
		}
		// A count of bytes is a whole number with a unit beside it. Reported as
		// a float it would be a different kind of measurement wearing the same
		// name, and the envelope decoder is entitled to refuse it.
		if m.Type != MetricInt {
			t.Errorf("%s is %s, want %s", want, m.Type, MetricInt)
		}
		if m.Unit != "bytes" {
			t.Errorf("%s has unit %q, want bytes", want, m.Unit)
		}
		if m.Int <= 0 {
			t.Errorf("%s = %d; the machine running this test has space", want, m.Int)
		}
	}
}

// A machine whose toolchain cannot say where it writes is not one this
// project's work runs on — and saying so is what makes fit non-vacuous before
// any floor exists, because judge() refuses every incomplete run.
//
// Named rather than omitted: one filesystem of two IS a run that measured less
// than a full one, and a short envelope that did not say so would be read as a
// complete measurement of a machine with less to check.
func TestFitReportsIncompleteWhenTheBuildCacheIsUnknown(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // `go` is now unresolvable
	metrics, incomplete, err := measureFit(t.TempDir())
	if err != nil {
		t.Fatalf("measureFit gave up entirely; the worktree was still measurable: %v", err)
	}
	if incomplete == "" {
		t.Fatal("one filesystem of two was measured and the run did not say so")
	}
	if len(metrics) != 1 || metrics[0].Name != "worktree_free_bytes" {
		t.Errorf("metrics = %+v, want only the worktree measurement", metrics)
	}
	// The reason names what established it. `go` was never started, so there is
	// no child output to quote — and a reason that trailed off after the colon
	// would leave an operator to reproduce the condition on the machine that is
	// already the problem.
	if strings.HasSuffix(incomplete, ": ") {
		t.Errorf("incomplete = %q, and names nothing after the colon", incomplete)
	}
	// An incomplete run is never a pass, so this is already a refusal today.
	if acceptable, _, _ := judge(Envelope{Gate: "fit", Metrics: metrics, Incomplete: incomplete}); acceptable {
		t.Error("a machine whose toolchain cannot be reached was judged fit")
	}
}

// A toolchain that says something on stderr and names no cache on stdout has
// not answered, however it exited. What it said reaches the incomplete, and
// nothing else is reported.
//
// The exit-0 case is the one that pins the two streams apart. Read together,
// the diagnostic IS the answer: it becomes the cache path, freeBytesNear walks
// it up to the process's own directory, and the run reports a real number about
// a filesystem nobody asked about — at full size, complete, and wrong.
func TestFitDoesNotReadTheToolchainsDiagnosticsAsAPath(t *testing.T) {
	for _, c := range []struct {
		name   string
		script string
	}{
		{"it objected and gave up", `echo 'go: parsing GOFLAGS: non-flag "x"' >&2; exit 2`},
		{"it warned and answered nothing", `echo 'go: parsing GOFLAGS: non-flag "x"' >&2; echo ""`},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeGo(t, c.script)
			metrics, incomplete, err := measureFit(t.TempDir())
			if err != nil {
				t.Fatalf("measureFit gave up entirely; the worktree was still measurable: %v", err)
			}
			if len(metrics) != 1 || metrics[0].Name != "worktree_free_bytes" {
				t.Errorf("metrics = %+v, want only the worktree measurement — the toolchain named no cache", metrics)
			}
			if !strings.Contains(incomplete, `non-flag "x"`) {
				t.Errorf("incomplete = %q, want it to carry what the toolchain objected to", incomplete)
			}
		})
	}
}

// What the toolchain says AROUND its answer is not part of the answer. A
// machine switching Go versions prints `go: downloading go1.x` to stderr before
// it prints the path to stdout, and the two read together make a path that
// names nothing.
func TestBuildCacheIsTheAnswerAndNotWhatWasSaidAroundIt(t *testing.T) {
	cache := t.TempDir()
	fakeGo(t, "echo 'go: downloading go1.99.0 (linux/amd64)' >&2\necho "+cache)
	path, why := buildCache(t.TempDir())
	if why != "" {
		t.Fatalf("the toolchain answered and the answer was refused: %s", why)
	}
	if path != cache {
		t.Errorf("build cache = %q, want %q", path, cache)
	}
}

// An answer that is not an absolute path is not a location. freeBytesNear walks
// up until it finds a filesystem, so a relative answer resolves against the
// process's own directory — which would be reported as the build cache, and
// reported completely.
func TestBuildCacheRefusesAnAnswerThatIsNotAnAbsolutePath(t *testing.T) {
	fakeGo(t, "echo relative/go-build")
	path, why := buildCache(t.TempDir())
	if path != "" {
		t.Errorf("build cache = %q, want none: it names no filesystem", path)
	}
	if !strings.Contains(why, "relative/go-build") || !strings.Contains(why, "absolute") {
		t.Errorf("why = %q, want it to quote the answer and say what is wrong with it", why)
	}
}

// fakeGo puts a `go` on PATH that behaves as body says.
//
// Two streams and an exit code are all these tests need of a toolchain, and a
// real one cannot be asked to warn on demand — which is exactly the condition
// under test.
func fakeGo(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake toolchain is a POSIX shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "go")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil { // WriteFile respects umask; Chmod does not
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// Every way `go env GOCACHE` can fail to answer produces a reason, because a
// condition reported without what established it is one an operator has to
// reproduce before believing it.
func TestGocacheReasonIsNeverEmpty(t *testing.T) {
	for _, c := range []struct {
		name   string
		stderr string
		err    error
		want   string
	}{
		// The toolchain ran and objected: its own words say the most.
		{"the child said why", "go: no such env var\nsecond line", errors.New("exit status 1"), "go: no such env var"},
		// It never ran, so there is nothing to quote and the error is all there is.
		{"the child never ran", "", errors.New(`exec: "go": executable file not found in $PATH`), "not found"},
		// It ran, exited 0, and said nothing. Rare, and still not silence.
		{"the child answered nothing", "", nil, "printed nothing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := gocacheReason(c.stderr, c.err)
			if got == "" {
				t.Fatal("the reason is empty; the incomplete message would trail off after its colon")
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("gocacheReason = %q, want it to carry %q", got, c.want)
			}
		})
	}
}

// A gate measures and never modifies what it measures. fit is the easiest one
// to get this wrong in — a free-space probe that wrote a file to find out would
// be reporting on a tree it had just changed — and measureCovered is the
// standing example of how much care the rule takes.
func TestFitModifiesNothing(t *testing.T) {
	dir := t.TempDir()
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := measureFit(dir); err != nil {
		t.Fatalf("measureFit: %v", err)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("the directory held %d entries before and %d after", len(before), len(after))
	}
	for i := range after {
		if after[i].Name() != before[i].Name() {
			t.Errorf("entry %d is %q, was %q", i, after[i].Name(), before[i].Name())
		}
	}
}

// A build cache that has never been written has no directory yet, and fit runs
// on exactly that machine — the fresh one, before work is given. A path that is
// not there yet is the ordinary case, and the filesystem that would hold it is
// the honest answer about it.
func TestFreeBytesNearWalksUpToAnExistingAncestor(t *testing.T) {
	n, err := freeBytesNear(filepath.Join(t.TempDir(), "no", "such", "dir"))
	if err != nil {
		t.Fatalf("freeBytesNear refused a path that does not exist yet: %v", err)
	}
	if n <= 0 {
		t.Errorf("freeBytesNear = %d, want the space on the filesystem that would hold it", n)
	}
}

// fit is deliberately not part of integration. A machine that cannot build is
// not a change that may not land, and folding the two together would report an
// unfit host as a defective change — on every machine the item then reaches.
//
// This is the mistake a later reader is most likely to "fix", so it is pinned.
func TestFitIsNotPartOfIntegration(t *testing.T) {
	for _, part := range gates["integration"].parts {
		if part == "fit" {
			t.Fatal("integration composes fit; an unfit machine would be reported as a change that may not land")
		}
	}
}
