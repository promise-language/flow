package claude

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// MinVersion is the oldest claude CLI this Client will report as usable. It is
// the release whose --max-budget-usd actually stops the run at the cap (see
// Client's docstring): on anything older AgentRequest.MaxCostUSD is accepted
// and ignored, so a step's cost grant stops bounding the turn and only bounds
// whether the NEXT one is dispatched. That failure is invisible until an
// overrun, which is exactly the kind `doctor` exists to catch beforehand.
const MinVersion = "2.1.217"

// Doctor implements flow.AgentDoctor: it reports whether this SDK can invoke
// the claude binary, and it SPENDS NOTHING.
//
// `--version` is the whole probe. It starts the real binary the same way Run
// does — same path resolution, same process — so an absent, unexecutable or
// wrong-architecture install fails here rather than mid-item. It is not a turn:
// no model is called and no account is billed.
//
// What this cannot answer is whether a turn would succeed. Credentials, quota
// and model availability are only settled by spending, and `doctor` may not
// spend — so it reports what it checked and does not imply the rest.
func (c *Client) Doctor(ctx context.Context) error {
	binary := c.Binary
	if binary == "" {
		binary = "claude"
	}
	// Free, but still the real binary: a test must not reach the machine's
	// install any more than Run may. See spawnable.
	if err := c.spawnable(); err != nil {
		return err
	}

	cmd := c.spawnCmd(ctx, binary, "--version")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%s: stdout pipe: %w", binary, err)
	}
	// Drained, not inherited: a version probe that leaks the binary's stderr
	// into doctor's report would corrupt a scanline an operator reads top to
	// bottom. It is only quoted when it explains a failure.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("%s: stderr pipe: %w", binary, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s could not be started: %w", binary, err)
	}
	out, _ := io.ReadAll(stdout)
	errOut, _ := io.ReadAll(stderr)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s --version failed: %w%s", binary, err, quoted(errOut))
	}

	got := versionOf(string(out))
	if got == "" {
		return fmt.Errorf("%s is not the claude CLI: --version printed %q", binary, firstLine(string(out)))
	}
	if olderThan(got, MinVersion) {
		return fmt.Errorf("%s is version %s; this SDK requires %s or later "+
			"(older releases accept --max-budget-usd and ignore it, so a step's cost grant does not stop the turn)",
			binary, got, MinVersion)
	}
	return nil
}

// versionRe matches the dotted version in `claude --version` output, which
// carries more than the number ("2.1.217 (Claude Code)"). Anchored at a word
// boundary so it reads the version and not a digit from the product name.
var versionRe = regexp.MustCompile(`\b(\d+)\.(\d+)\.(\d+)\b`)

// versionOf extracts the dotted version from --version output, or "" when the
// output carries none — which means the binary is not the CLI we think it is.
func versionOf(out string) string {
	m := versionRe.FindString(out)
	return m
}

// olderThan compares two dotted versions numerically. Numerically, because
// "2.1.217" sorts BEFORE "2.1.99" as a string and the whole check would invert
// on exactly the releases it exists to separate.
func olderThan(got, min string) bool {
	g, m := fields3(got), fields3(min)
	for i := range g {
		switch {
		case g[i] < m[i]:
			return true
		case g[i] > m[i]:
			return false
		}
	}
	return false
}

// fields3 splits a dotted version into three numbers, treating a missing or
// unparseable component as zero. versionRe has already established the shape.
func fields3(v string) [3]int {
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n, _ := strconv.Atoi(part)
		out[i] = n
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// quoted renders captured stderr as a trailing clause, or nothing when the
// process said nothing.
func quoted(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	return ": " + s
}
