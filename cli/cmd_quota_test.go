package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// quotaApp builds the smallest App `quota` needs. The command reads the agent
// substrate, not the orchestrator, so the wiring is only what cli.App demands.
func quotaApp(t *testing.T) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	// A cache of this test's own. The cache is machine-wide by design, so
	// without this one test's reading is served to the next and the fetch
	// counts and failures below would be measuring the previous test.
	dir := filepath.Join(t.TempDir(), "flow")
	prev := quotaCacheDir
	quotaCacheDir = func() (string, bool) { return dir, true }
	t.Cleanup(func() { quotaCacheDir = prev })
	return newArgparseApp(t)
}

// The human rendering IS reportQuota — the function `resolve` narrates with —
// so the two cannot come to describe the same windows differently. The
// assertion is that the same figures appear, not that a second renderer
// produces similar ones.
func TestCmdQuota_HumanRenderingIsTheOneResolveUses(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })
	app, out, errBuf := quotaApp(t)

	if code := app.cmdQuota(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdQuota = %d; err=%q", code, errBuf.String())
	}
	got := out.String()

	var want bytes.Buffer
	reportQuota(&want)
	if got != want.String() {
		t.Errorf("quota rendered\n%q\nbut reportQuota renders\n%q", got, want.String())
	}
	if !strings.Contains(got, "42% used") {
		t.Errorf("the figures are missing; got:\n%s", got)
	}
}

// The report IS the output, so it goes to stdout — docs/cli.md § One-shot
// reports. resolve's copy goes to stderr because there it is narration beside a
// machine stream; here there is no machine stream to keep clear of.
func TestCmdQuota_ReportGoesToStdout(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })
	app, out, errBuf := quotaApp(t)

	if code := app.cmdQuota(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdQuota = %d", code)
	}
	if out.Len() == 0 {
		t.Error("nothing on stdout")
	}
	if errBuf.Len() != 0 {
		t.Errorf("the report belongs on one stream; stderr carried %q", errBuf.String())
	}
}

// JSON carries the actual values: the account whose allowance is being spent,
// each window's fraction and reset instant, and when the reading was taken.
func TestCmdQuota_JSONKeySet(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })
	app, out, _ := quotaApp(t)

	if code := app.cmdQuota(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("cmdQuota = %d", code)
	}
	var payload quotaPayload
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal %s: %v", out.String(), err)
	}
	if len(payload.Windows) == 0 {
		t.Fatal("no windows reported")
	}
	w := payload.Windows[0]
	if w.Used == nil || *w.Used != 0.42 {
		t.Errorf("used = %v, want the actual 0.42 — never the rendered percentage", w.Used)
	}
	if w.Label == "" || w.ResetsAt.IsZero() || w.LengthSeconds <= 0 {
		t.Errorf("window is missing a field: %+v", w)
	}
	if payload.ReadAt.IsZero() {
		t.Error("read_at is unset; the figures are cached, so when they were taken is part of the report")
	}
}

// A window the substrate published no figure for reports NULL, never zero: an
// allowance nobody measured and an allowance nothing has been spent from are
// opposite facts, and zero is the one that reads as headroom.
func TestCmdQuota_AnUnpublishedWindowIsNullNotZero(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	installFetch(t, func() ([]windowUsage, error) {
		u := usageAt(0.42)
		u[0].Used = -1 // the sentinel the reader uses for "absent"
		return u, nil
	})
	app, out, _ := quotaApp(t)

	if code := app.cmdQuota(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("cmdQuota = %d", code)
	}
	var raw struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, ok := raw.Windows[0]["used"]; !ok || got != nil {
		t.Errorf("used = %v, want null for a window the substrate did not publish", got)
	}
}

// WHOSE ALLOWANCE the figures belong to reaches --json, as the identifier the
// human rendering names above the same figures.
//
// A machine may drive more than one agent account, so figures reported without
// one say what is being spent without saying whose it is — and a park written
// against an account is scoped to it. The two renderings are one report, so the
// field carries the account rather than the line the human form prints it on.
func TestCmdQuota_JSONNamesTheAccountTheFiguresBelongTo(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })
	useStubAgentAccount(t, agentAccountRecord{Id: "uuid-1", Email: "pat@example.com"}, nil)
	app, out, _ := quotaApp(t)

	if code := app.cmdQuota(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("cmdQuota = %d", code)
	}
	var payload quotaPayload
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal %s: %v", out.String(), err)
	}
	if payload.Account != "pat@example.com (uuid-1)" {
		t.Errorf("account = %q, want the account the figures belong to", payload.Account)
	}
}

// A reading that could not be taken is a command that could not complete. The
// report IS the command here, unlike the narration beside a run that carries on
// regardless, so the exit code has to say so (docs/cli.md § Exit codes: 1 is
// "the command could not complete").
func TestCmdQuota_AnUnreadableQuotaExitsOne(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	installFetch(t, func() ([]windowUsage, error) { return nil, errors.New("no credentials (injected)") })

	for _, mode := range []string{"--human", "--json"} {
		t.Run(mode, func(t *testing.T) {
			app, _, errBuf := quotaApp(t)
			if code := app.cmdQuota(context.Background(), []string{mode}); code != 1 {
				t.Errorf("cmdQuota %s = %d, want 1; err=%q", mode, code, errBuf.String())
			}
		})
	}
}

// It takes no arguments, and says so by name rather than by printing usage.
func TestCmdQuota_RejectsAnArgument(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })
	app, out, errBuf := quotaApp(t)

	if code := app.cmdQuota(context.Background(), []string{"5h"}); code != 2 {
		t.Fatalf("cmdQuota = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), `"5h"`) {
		t.Errorf("refusal does not name the argument: %q", errBuf.String())
	}
	if out.Len() != 0 {
		t.Errorf("a rejected invocation reported anyway: %q", out.String())
	}
}

// It spends nothing and changes nothing: ONE read, in either mode.
//
// The cache is deliberately taken away here. With one in place a second ask is
// served from disk and a command that reads twice looks identical to one that
// reads once — which is the whole shape of the defect this pins, because the
// machine with no usable cache location is exactly the one where the second ask
// is a second request.
func TestCmdQuota_ReadsOnceInEitherMode(t *testing.T) {
	for _, mode := range []string{"--human", "--json"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })
			prev := quotaCacheDir
			quotaCacheDir = func() (string, bool) { return "", false }
			t.Cleanup(func() { quotaCacheDir = prev })
			app, _, errBuf := newArgparseApp(t)

			if code := app.cmdQuota(context.Background(), []string{mode}); code != 0 {
				t.Fatalf("cmdQuota = %d; err=%q", code, errBuf.String())
			}
			if s.count() != 1 {
				t.Errorf("fetches = %d, want 1 — the reading is rendered and judged from one ask", s.count())
			}
		})
	}
}

// …and through the machine-wide cache every other display site goes through,
// so a reading already taken costs no request at all.
func TestCmdQuota_ReadsThroughTheCache(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })
	app, _, _ := quotaApp(t)

	if code := app.cmdQuota(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("cmdQuota = %d", code)
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d, want 1", s.count())
	}
}
