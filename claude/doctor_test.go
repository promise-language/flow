package claude

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// versionCmd is a fakeCmd that answers --version and records the argv it was
// asked for, so a test can assert the probe is a version query and not a turn.
func versionClient(out string, waitErr error) (*Client, *[]string) {
	var argv []string
	c := &Client{Binary: "claude"}
	c.spawn = func(ctx context.Context, name string, args ...string) cmdHandle {
		argv = append([]string{name}, args...)
		return &fakeCmd{stdoutStream: out, waitErr: waitErr}
	}
	return c, &argv
}

// The probe is a version query: it starts the real binary — same path
// resolution, same process — and it is not a turn. Nothing is billed, and the
// stdin the turn path writes is never opened.
func TestDoctor_AsksTheVersionAndSpendsNothing(t *testing.T) {
	c, argv := versionClient("2.1.217 (Claude Code)\n", nil)

	if err := c.Doctor(context.Background()); err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if len(*argv) != 2 || (*argv)[0] != "claude" || (*argv)[1] != "--version" {
		t.Errorf("argv = %v, want [claude --version] — the probe must not be a turn", *argv)
	}
}

// A binary that cannot be started is the case the check exists for: absent,
// unexecutable, wrong architecture. The reason travels to the caller.
func TestDoctor_ReportsAFailedProcess(t *testing.T) {
	c, _ := versionClient("", errors.New("exit status 127"))

	err := c.Doctor(context.Background())
	if err == nil {
		t.Fatal("Doctor returned nil for a binary that failed to run")
	}
	if !strings.Contains(err.Error(), "127") {
		t.Errorf("error should carry what the process did; got %v", err)
	}
}

// A binary that prints something that is not a version is not the CLI this SDK
// drives. Saying so beats reporting a machine fit on the strength of a
// successful exit from an unrelated program.
func TestDoctor_ReportsOutputThatIsNotAVersion(t *testing.T) {
	c, _ := versionClient("usage: claude-helper [options]\n", nil)

	err := c.Doctor(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not the claude CLI") {
		t.Fatalf("Doctor error = %v, want it to say the binary is not the CLI", err)
	}
}

// The documented minimum is enforced, and it is enforced NUMERICALLY. This is
// the whole reason the check parses rather than compares strings: "2.1.99"
// sorts after "2.1.217" as text, so a string comparison would reject exactly
// the releases the minimum exists to accept.
func TestDoctor_EnforcesTheMinimumVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		wantErr bool
	}{
		{"2.1.217 (Claude Code)", false},
		{"2.1.218 (Claude Code)", false},
		{"2.2.0 (Claude Code)", false},
		{"3.0.0 (Claude Code)", false},
		{"2.1.216 (Claude Code)", true},
		{"2.1.99 (Claude Code)", true},
		{"2.0.999 (Claude Code)", true},
		{"1.9.9 (Claude Code)", true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			c, _ := versionClient(tc.version+"\n", nil)
			err := c.Doctor(context.Background())
			if tc.wantErr && err == nil {
				t.Fatalf("version %q accepted; MinVersion is %s and older releases ignore --max-budget-usd, "+
					"so a step's cost grant would not stop the turn", tc.version, MinVersion)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("version %q rejected: %v", tc.version, err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), MinVersion) {
				t.Errorf("rejection should name the minimum; got %v", err)
			}
		})
	}
}

// A test must not reach the machine's install — not through Run, and not
// through the free probe either, which still spawns the real binary.
func TestRealBinaryIsUnreachableFromATest(t *testing.T) {
	c := New() // no spawn seam: the production wiring

	_, err := c.Run(context.Background(), flow.AgentRequest{Prompt: "hello"})
	if err == nil {
		t.Fatal("Run spawned the real binary from a test")
	}
	if !strings.Contains(err.Error(), "refusing to spawn the real claude binary") {
		t.Errorf("Run error = %v, want the refusal", err)
	}
	if err := c.Doctor(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "refusing to spawn the real claude binary") {
		t.Errorf("Doctor error = %v, want the refusal", err)
	}
}
