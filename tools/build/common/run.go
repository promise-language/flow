package common

// Running one gate by hand, and reaching a verdict from what it measured.
//
// This is the judging layer, and it is a DIFFERENT PROGRAM from the gates on
// purpose. A gate that held its own thresholds could be made to pass by
// editing the gate — and when the thing being measured is a change written by
// an agent, the agent can edit it. The party under judgement must not hold
// what judges it.
//
// It is also the only layer that can render a result for a person: it holds
// the caps, so it can print a number beside the terms it was judged on. A gate
// could only ever print the left-hand column.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// caps are the thresholds this project's measurements are judged against: the
// most of each that may exist for a change to be sound. A metric with no entry
// is reported and not judged.
//
// THIS TABLE IS A PLACEHOLDER, deliberately kept small. The real thing is a
// manifest declaring each metric's type, its direction, and the baseline it
// ratchets against — https://github.com/promise-language/flow/issues/38. It
// lives here rather than in a file of its own because inventing a manifest
// format now would be inventing one that has to be migrated; what matters
// today is that the thresholds are not inside any gate.
var caps = map[string]float64{
	"unformatted_files":    0,
	"unbuildable_packages": 0,
	"vet_findings":         0,
	"failed_tests":         0,
	"failed_packages":      0,
}

// RunOneGate measures one gate the way a runner would — by executing the gate
// program, not by calling into it — then judges what came back and prints it.
//
// Going through the process boundary is the point. This is the same path the
// SDK takes, so a gate that is broken in a way only visible across that
// boundary (prints to stdout, exits without an envelope, hangs) is broken here
// too, where a person can see it.
func RunOneGate(repoRoot, gateBin, name string) error {
	if !KnownGate(name) {
		return fmt.Errorf("no gate named %q in this project; known gates: %s",
			name, strings.Join(GateNames(), ", "))
	}

	cmd := exec.Command(gateBin, name, "--envelope")
	cmd.Dir = repoRoot
	// Stderr is the gate's progress, and it goes straight to ours — not into a
	// buffer we print afterwards. Gates run for minutes, and a gate that is
	// working and a gate that is wedged produce the same thing (nothing) for
	// as long as the output is held.
	cmd.Stderr = os.Stderr
	out, runErr := cmd.Output()

	var env Envelope
	if jsonErr := json.Unmarshal(out, &env); jsonErr != nil {
		// No readable envelope. Whether the process died or printed something
		// that is not an envelope, nothing was measured — and either way this
		// is not a report that the tree is bad.
		if runErr != nil {
			return fmt.Errorf("%s did not measure anything: %w", name, runErr)
		}
		return fmt.Errorf("%s printed something that is not an envelope: %s",
			name, firstLine(string(out)))
	}

	fmt.Print(renderVerdict(env))
	if !passes(env) {
		return fmt.Errorf("%s: measurements exceed what this project allows", name)
	}
	return nil
}

// passes reports whether every capped metric is within its cap. A metric
// nothing caps cannot fail, and an incomplete run cannot pass: honest numbers
// that understate what was checked must not be read as a good result.
func passes(env Envelope) bool {
	if env.Incomplete != "" {
		return false
	}
	for _, m := range env.Metrics {
		if capValue, ok := caps[m.Name]; ok && m.Number() > capValue {
			return false
		}
	}
	return true
}

// renderVerdict prints each measurement beside the term it was judged on.
func renderVerdict(env Envelope) string {
	var sb strings.Builder
	width := 0
	for _, m := range env.Metrics {
		if len(m.Name) > width {
			width = len(m.Name)
		}
	}
	for _, m := range env.Metrics {
		capValue, capped := caps[m.Name]
		judged, mark := "not judged", " "
		if capped {
			judged = "cap " + number(capValue)
			mark = "✗"
			if m.Number() <= capValue {
				mark = "✓"
			}
		}
		fmt.Fprintf(&sb, "  %-*s  %8s  %-12s %s\n",
			width, m.Name, m.String()+unitSuffix(m.Unit), judged, mark)
	}
	if env.Incomplete != "" {
		fmt.Fprintf(&sb, "\n  incomplete: %s\n", env.Incomplete)
		fmt.Fprintf(&sb, "  an incomplete run is never a pass, and never moves a baseline\n")
	}
	return sb.String()
}

func number(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func unitSuffix(unit string) string {
	if unit == "percent" {
		return "%"
	}
	return ""
}

// GateBinary is where a project's gate program lives, relative to the repo
// root. Not configurable: the names are fixed, so the way to reach them is.
func GateBinary(repoRoot string) string {
	return filepath.Join(repoRoot, "bin", "gate")
}

// CappedMetrics lists the metrics this layer judges, for usage text.
func CappedMetrics() []string {
	names := make([]string, 0, len(caps))
	for n := range caps {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
