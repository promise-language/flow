package common

// The gates this project provides.
//
// A gate MEASURES and never modifies what it measures. That is the whole
// difference between this file and verify.go: `verify` formats the tree with
// `gofmt -w` on its way to an answer, which is correct for a command and
// disqualifying for a gate — an answer about a tree that was repaired first is
// not an answer about the tree anyone proposed.
//
// A gate also does not JUDGE. Nothing here compares a number to a threshold or
// returns a verdict; it reports what it found and stops. Whether
// `unformatted_files: 3` is acceptable needs thresholds the gate deliberately
// does not hold — see run.go, and docs/gates-and-commands.md.
//
// Nothing in here writes to stdout. Stdout carries the envelope and nothing
// else, so every child process has its output captured, and progress goes to
// stderr where a person watching a long run can see it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// MetricType is what kind of number a measurement is. The set is closed.
type MetricType string

const (
	// MetricInt counts things. A count is a whole number of them.
	MetricInt MetricType = "int"
	// MetricFloat measures a quantity that is not a count.
	MetricFloat MetricType = "float"
)

// Metric is one number a gate measured. It carries no opinion about whether the
// number is good: that comparison needs a threshold, and a gate holds none.
//
// The type travels with the value, and a count is held as an integer rather
// than as a float that happens to be whole. Reporting a float where a count was
// declared is a mismatch to name rather than a widening to absorb: a metric
// whose type changed measured something else, and absorbed silently it would
// move a ratchet that by construction never moves back.
type Metric struct {
	Name string
	Type MetricType
	// Exactly one of these carries the value, chosen by Type.
	Int   int64
	Float float64
	Unit  string
}

// Count is a measurement of how many. Whole by construction.
func Count(name string, n int) Metric {
	return Metric{Name: name, Type: MetricInt, Int: int64(n)}
}

// Quantity is a measurement that is not a count.
func Quantity(name string, v float64, unit string) Metric {
	return Metric{Name: name, Type: MetricFloat, Float: v, Unit: unit}
}

// Size is a measurement of how much, in whole units of it. Neither of the two
// above fits: Count takes an int and carries no unit, and Quantity is a float.
// Bytes are whole, carry a unit, and outrun what an int holds on a 32-bit host.
func Size(name string, n int64, unit string) Metric {
	return Metric{Name: name, Type: MetricInt, Int: n, Unit: unit}
}

// Number is the value as a float, for comparison against a threshold. Widening
// is safe HERE and nowhere else: the judging layer compares, it does not store,
// so nothing downstream can mistake the widened form for what was measured.
func (m Metric) Number() float64 {
	if m.Type == MetricInt {
		return float64(m.Int)
	}
	return m.Float
}

// String renders the value in its own type — a count never grows a decimal
// point, and a quantity never loses one.
func (m Metric) String() string {
	if m.Type == MetricInt {
		return strconv.FormatInt(m.Int, 10)
	}
	return strconv.FormatFloat(m.Float, 'f', 1, 64)
}

// metricWire is the envelope form of a Metric: one "value" field, and the type
// beside it so a reader knows which kind of number it is looking at.
type metricWire struct {
	Name  string          `json:"name"`
	Type  MetricType      `json:"type"`
	Value json.RawMessage `json:"value"`
	Unit  string          `json:"unit,omitempty"`
}

func (m Metric) MarshalJSON() ([]byte, error) {
	w := metricWire{Name: m.Name, Type: m.Type, Unit: m.Unit}
	switch m.Type {
	case MetricInt:
		w.Value = json.RawMessage(strconv.FormatInt(m.Int, 10))
	case MetricFloat:
		w.Value = json.RawMessage(strconv.FormatFloat(m.Float, 'f', -1, 64))
	default:
		return nil, fmt.Errorf("metric %q has no type", m.Name)
	}
	return json.Marshal(w)
}

func (m *Metric) UnmarshalJSON(b []byte) error {
	var w metricWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	m.Name, m.Type, m.Unit = w.Name, w.Type, w.Unit
	switch w.Type {
	case MetricInt:
		// A count arriving with a fractional part is not a count. Refusing it
		// is the point: absorbed, it would be a type change nothing recorded.
		if err := json.Unmarshal(w.Value, &m.Int); err != nil {
			return fmt.Errorf("metric %q is declared %s but its value is not: %w", w.Name, w.Type, err)
		}
	case MetricFloat:
		if err := json.Unmarshal(w.Value, &m.Float); err != nil {
			return fmt.Errorf("metric %q is declared %s but its value is not: %w", w.Name, w.Type, err)
		}
	default:
		return fmt.Errorf("metric %q has an unknown type %q", w.Name, w.Type)
	}
	return nil
}

// Envelope is what a gate prints on stdout: one JSON object, written whole.
//
// It is written whole deliberately. A run killed part-way leaves output that
// does not parse, which is how a reader tells "measured nothing" from "measured
// and reported" without asking the gate — a gate that died is not alive to say
// so.
type Envelope struct {
	Gate    string   `json:"gate"`
	Metrics []Metric `json:"metrics"`
	// Incomplete names the reason this run measured less than a full one, and
	// is empty when it did not. A run that skipped part of its work reports
	// honest numbers that UNDERSTATE what was checked, which is
	// indistinguishable from an improvement unless the run says so — and a
	// baseline moved by such a run sets a floor no complete run can meet.
	Incomplete string `json:"incomplete,omitempty"`
}

// gateDef is either a leaf that measures, or a composition of other gates.
type gateDef struct {
	summary string
	measure func(repoRoot string) ([]Metric, string, error)
	parts   []string
}

// gates is CLOSED. A name absent from this map is refused rather than guessed
// at, because a runner asking for a gate this project does not have must learn
// that, not receive an empty measurement that reads like a clean result.
//
// The names come from the SDK's declared concepts (see artifact.go): a project
// may leave a concept unprovided, but may not invent a spelling for one it has.
var gates = map[string]gateDef{
	"formatted": {
		summary: "source files that gofmt would rewrite",
		measure: measureFormatted,
	},
	"builds": {
		summary: "packages that fail to compile",
		measure: measureBuilds,
	},
	"checked": {
		summary: "go vet diagnostics",
		measure: measureChecked,
	},
	"tested": {
		summary: "failing tests and failing packages",
		measure: measureTested,
	},
	"covered": {
		summary: "statement coverage over the module",
		measure: measureCovered,
	},
	// integration is what a decision rests on: the whole, measured at once.
	// Its parts stay separately runnable, and that is a requirement rather
	// than a convenience — a step fixing one failing suite should re-run that
	// suite, not pay for the formatter and every other target each round.
	"integration": {
		summary: "everything that must hold before a change may land",
		parts:   []string{"formatted", "builds", "checked", "tested"},
	},
	// fit is the one gate here whose subject is not the code: it measures the
	// machine, before work is given to it. It is deliberately NOT a part of
	// integration — a machine that cannot build is not a change that may not
	// land. See docs/environment.md.
	"fit": {
		summary: "space where this project's work writes",
		measure: measureFit,
	},
}

// GateNames returns every gate this project provides, sorted, for usage text.
func GateNames() []string {
	names := make([]string, 0, len(gates))
	for n := range gates {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// GateSummary returns the one-line description of a gate, or "" if unknown.
func GateSummary(name string) string { return gates[name].summary }

// KnownGate reports whether this project provides a gate by that name.
func KnownGate(name string) bool { _, ok := gates[name]; return ok }

// MeasureGate runs one gate and returns what it measured.
//
// The error return means the measurement could not be OBTAINED — a tool is
// missing, or the environment refused. It never means "the numbers are bad":
// three failing tests is a successful run of the `tested` gate, and the
// envelope says so.
func MeasureGate(repoRoot, name string) (Envelope, error) {
	def, ok := gates[name]
	if !ok {
		return Envelope{}, fmt.Errorf("no gate named %q in this project", name)
	}
	env := Envelope{Gate: name, Metrics: []Metric{}}
	if def.measure != nil {
		metrics, incomplete, err := def.measure(repoRoot)
		if err != nil {
			return Envelope{}, err
		}
		env.Metrics = metrics
		env.Incomplete = incomplete
		return env, nil
	}
	// A composition. Each part is measured by the same path a caller asking
	// for that part alone would take, so the whole cannot disagree with its
	// parts about how anything is measured.
	var reasons []string
	for _, part := range def.parts {
		sub, err := MeasureGate(repoRoot, part)
		if err != nil {
			return Envelope{}, fmt.Errorf("%s: %w", part, err)
		}
		env.Metrics = append(env.Metrics, sub.Metrics...)
		if sub.Incomplete != "" {
			reasons = append(reasons, part+": "+sub.Incomplete)
		}
	}
	env.Incomplete = strings.Join(reasons, "; ")
	return env, nil
}

// ParseGateArgs reads a gate invocation: exactly one name, and whether the
// caller asked for an envelope.
//
// The rules it enforces are the protocol, not this program's preferences. A
// gate is asked for one way, because two callers asking the same thing must
// not be able to get different answers and both be right — so an unknown flag
// is refused rather than ignored, and a second name is refused rather than
// silently dropped.
func ParseGateArgs(args []string) (name string, envelope bool, err error) {
	for _, a := range NormalizeArgs(args) {
		switch {
		case a == "-envelope":
			envelope = true
		case strings.HasPrefix(a, "-"):
			return "", false, fmt.Errorf("use of unknown flag %q", a)
		case name != "":
			return "", false, fmt.Errorf("unexpected argument %q; a gate is asked for by name, once", a)
		default:
			name = a
		}
	}
	if name == "" {
		return "", false, fmt.Errorf("no gate named; known gates: %s", strings.Join(GateNames(), ", "))
	}
	if !KnownGate(name) {
		return "", false, unknownGate(name)
	}
	return name, envelope, nil
}

// unknownGate is the refusal every entry point gives for a name this project
// does not have. One wording, because a caller that mistyped a gate name is
// told the same thing whichever program it typed it at.
func unknownGate(name string) error {
	return fmt.Errorf("no gate named %q in this project; known gates: %s",
		name, strings.Join(GateNames(), ", "))
}

// measureFormatted counts files gofmt would rewrite. `-l` lists them; `-w`
// would repair them, which is verify's job and not a gate's.
func measureFormatted(repoRoot string) ([]Metric, string, error) {
	out, err := gateOutput(repoRoot, "gofmt", "-l", ".")
	if err != nil && out == "" {
		return nil, "", fmt.Errorf("gofmt: %w", err)
	}
	return []Metric{Count("unformatted_files", countLines(out))}, "", nil
}

// modules lists every Go module in this repository.
//
// `./...` is module-scoped, so one invocation at the root measures the SDK and
// silently skips the tools that build it — including the source of the gates
// themselves. A gate that skipped a module would report honest numbers about
// part of the subject, which is the shape of an incomplete run that does not
// know it is incomplete.
func modules(repoRoot string) []string {
	dirs := []string{repoRoot}
	tools := filepath.Join(repoRoot, "tools", "build")
	if Exists(filepath.Join(tools, "go.mod")) {
		dirs = append(dirs, tools)
	}
	return dirs
}

// measureBuilds counts packages that fail to compile. `go build` prefixes each
// failing package with a "# " header line, so the headers are the count.
func measureBuilds(repoRoot string) ([]Metric, string, error) {
	n := 0
	for _, dir := range modules(repoRoot) {
		out, err := gateOutput(dir, "go", "build", "./...")
		found := countPrefixed(out, "# ")
		if err != nil && found == 0 {
			// It failed and named no package: the failure is about the
			// toolchain or the module, not about a package in this tree.
			return nil, "", fmt.Errorf("go build in %s: %w: %s", dir, err, firstLine(out))
		}
		n += found
	}
	return []Metric{Count("unbuildable_packages", n)}, "", nil
}

// measureChecked counts go vet diagnostics — the lines naming a file and a
// position, as distinct from the "# package" headers that group them.
func measureChecked(repoRoot string) ([]Metric, string, error) {
	n := 0
	for _, dir := range modules(repoRoot) {
		out, err := gateOutput(dir, "go", "vet", "./...")
		found := countDiagnostics(out)
		if err != nil && found == 0 {
			return nil, "", fmt.Errorf("go vet in %s: %w: %s", dir, err, firstLine(out))
		}
		n += found
	}
	return []Metric{Count("vet_findings", n)}, "", nil
}

// measureTested counts failing tests and failing packages. Both are worth
// having: one failing test in one package and forty in forty are different
// situations, and a single number cannot tell them apart.
func measureTested(repoRoot string) ([]Metric, string, error) {
	tests, pkgs := 0, 0
	for _, dir := range modules(repoRoot) {
		out, err := gateOutput(dir, "go", "test", "./...")
		t := countPrefixed(out, "--- FAIL:")
		p := countPrefixed(out, "FAIL\t")
		if err != nil && t == 0 && p == 0 {
			// The run itself did not happen — a build failure in a test
			// package, most often. That is not "zero failing tests".
			return nil, "", fmt.Errorf("go test in %s: %w: %s", dir, err, firstLine(out))
		}
		tests += t
		pkgs += p
	}
	return []Metric{
		Count("failed_tests", tests),
		Count("failed_packages", pkgs),
	}, "", nil
}

// measureCovered reports statement coverage over the module.
//
// The profile is written to a temporary directory OUTSIDE the repository. A
// gate that dropped it in the worktree would have modified the subject it was
// measuring — untracked, but a difference all the same, and the runner's
// non-modification check cannot be asked to guess which stray files were meant.
func measureCovered(repoRoot string) ([]Metric, string, error) {
	dir, err := os.MkdirTemp("", "flow-gate-cover-")
	if err != nil {
		return nil, "", fmt.Errorf("coverage: %w", err)
	}
	defer os.RemoveAll(dir)
	profile := filepath.Join(dir, "coverage.out")

	_, testErr := gateOutput(repoRoot, "go", "test", "-coverprofile="+profile, "./...")
	if _, err := os.Stat(profile); err != nil {
		return nil, "", fmt.Errorf("coverage: no profile was produced: %w", testErr)
	}
	out, err := gateOutput(repoRoot, "go", "tool", "cover", "-func="+profile)
	if err != nil {
		return nil, "", fmt.Errorf("go tool cover: %w: %s", err, firstLine(out))
	}
	pct, ok := totalCoverage(out)
	if !ok {
		return nil, "", fmt.Errorf("coverage: could not read a total from `go tool cover -func`")
	}
	// Failing tests do not stop coverage from being reported, but they DO mean
	// this run measured less than a green one: a package whose tests failed
	// contributes whatever ran before the failure. Saying so is what keeps the
	// number from being read as a drop in coverage.
	incomplete := ""
	if testErr != nil {
		incomplete = "some packages failed their tests, so their statements were only partly exercised"
	}
	return []Metric{Quantity("statement_coverage", pct, "percent")}, incomplete, nil
}

// measureFit reports the space available where this project's work writes.
//
// It reports bytes and stops. How much is enough is a property of this
// project's build, held by the judging layer: "is there enough disk" reads like
// a yes/no question and is not one, and a gate that answered it would be the
// threshold sitting inside the party under measurement.
//
// Two filesystems, because they are two requirements and are often not one
// device: the worktree is where the change is written, and the build cache is
// where the toolchain writes what it reuses. Both are reported every run even
// when they resolve to the same filesystem — an envelope whose shape varied by
// host is one no threshold can be written against.
func measureFit(repoRoot string) ([]Metric, string, error) {
	free, err := freeBytesNear(repoRoot)
	if err != nil {
		return nil, "", fmt.Errorf("free space at %s: %w", repoRoot, err)
	}
	metrics := []Metric{Size("worktree_free_bytes", free, "bytes")}

	// `go env GOCACHE` is the only authoritative answer to where the build
	// cache is. Recomputing it here would be a second copy of the toolchain's
	// own resolution, wrong on every machine that configured it.
	out, err := gateOutput(repoRoot, "go", "env", "GOCACHE")
	cache := strings.TrimSpace(out)
	if err != nil || cache == "" {
		// Named rather than omitted. One filesystem of two IS a run that
		// measured less than a full one, and an incomplete run is never a
		// pass — which is the right answer here: a machine whose toolchain
		// cannot say where it writes is not one this project's work runs on.
		return metrics, "the build cache location is unknown, so only the " +
			"worktree filesystem was measured: " + gocacheReason(out, err), nil
	}
	free, err = freeBytesNear(cache)
	if err != nil {
		return nil, "", fmt.Errorf("free space at %s: %w", cache, err)
	}
	return append(metrics, Size("build_cache_free_bytes", free, "bytes")), "", nil
}

// gocacheReason accounts for a `go env GOCACHE` that did not answer.
//
// Never empty, which is the whole of it. An unfit machine is reported with the
// measurement that established it, and a reason that trails off after a colon
// leaves an operator to reproduce the condition on the machine that is already
// the problem. The child's own output is preferred because it says what the
// toolchain objected to; a child that could not be started produced none, and
// there the error is the entire account.
func gocacheReason(out string, err error) string {
	if line := firstLine(out); line != "" {
		return line
	}
	if err != nil {
		return err.Error()
	}
	return "`go env GOCACHE` printed nothing"
}

// freeBytesNear reports free space for path, or for the nearest ancestor that
// exists. A build cache that has never been written has no directory yet, and
// `fit` runs on exactly that machine — the fresh one, before work is given — so
// a path that is not there yet is the ordinary case rather than an error.
func freeBytesNear(path string) (int64, error) {
	for p := filepath.Clean(path); ; {
		if Exists(p) {
			return freeBytes(p)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return 0, fmt.Errorf("nothing on the path %s exists", path)
		}
		p = parent
	}
}

// gateOutput runs a child and returns its combined output as a string.
//
// Combined, and captured: the child's stdout must not reach ours, which
// carries the envelope and nothing else. Progress goes to stderr so a person
// watching a long gate can see it is working — silence and a hang look the
// same from outside.
func gateOutput(dir, name string, args ...string) (string, error) {
	fmt.Fprintf(os.Stderr, "==> %s %s\n", name, strings.Join(args, " "))
	var buf bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func countLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// countPrefixed counts lines starting with prefix, ignoring leading
// whitespace — `go test` indents a failing subtest under its parent, and a
// subtest that failed is a failing test.
func countPrefixed(s, prefix string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), prefix) {
			n++
		}
	}
	return n
}

// countDiagnostics counts vet findings: lines of the form path:line:col: msg.
// The "# package" headers that group them are not findings.
func countDiagnostics(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		if isDiagnostic(line) {
			n++
		}
	}
	return n
}

// isDiagnostic reports whether a line names a source position: at least two
// colon-separated numeric fields after a path.
func isDiagnostic(line string) bool {
	parts := strings.Split(line, ":")
	if len(parts) < 3 {
		return false
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return false
	}
	_, err := strconv.Atoi(parts[2])
	return err == nil
}

// totalCoverage reads the percentage from `go tool cover -func`'s last line,
// which is of the form "total:\t(statements)\t62.5%".
func totalCoverage(s string) (float64, bool) {
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "total:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		last := strings.TrimSuffix(fields[len(fields)-1], "%")
		pct, err := strconv.ParseFloat(last, 64)
		if err != nil {
			return 0, false
		}
		return pct, true
	}
	return 0, false
}

func firstLine(s string) string {
	first, _, _ := strings.Cut(s, "\n")
	return first
}
