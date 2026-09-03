package prompt

import (
	"strings"
	"testing"
	"text/template"
)

func TestRender_FillsPartials(t *testing.T) {
	c := Context{ItemID: "T1", ItemType: "task", ItemTitle: "do it", VerifyCmd: "make check"}
	if err := c.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(c.ItemHeader, "T1") || !strings.Contains(c.ItemHeader, "do it") {
		t.Errorf("ItemHeader missing id/title: %q", c.ItemHeader)
	}
	if !strings.Contains(c.AskGuidance, "mcp__tracker__ask_user_question") {
		t.Errorf("AskGuidance missing MCP question tool: %q", c.AskGuidance)
	}
	// The plan-resolution partial must carry the proof-gated already-implemented
	// branch (the stall fix) with the precise terminal statuses, the ask-if-unsure
	// path, and the not-feasible path. (BLOCKED moved to the implement step.)
	for _, want := range []string{"ALREADY IMPLEMENTED", "PROOF", "duplicate", "cant_reproduce", "works_as_intended", "ask_user_question", "wontfix", "needs-attention"} {
		if !strings.Contains(c.PlanStepResolution, want) {
			t.Errorf("PlanStepResolution missing %q", want)
		}
	}
	if !strings.Contains(c.RebaseResolution, "make check") {
		t.Errorf("RebaseResolution should interpolate VerifyCmd: %q", c.RebaseResolution)
	}
	if !strings.Contains(c.RebaseResolution, "git rebase --continue") {
		t.Error("RebaseResolution missing the continue step")
	}
	// "--ours" is asserted because the carve-out is unusable without it: git swaps
	// ours/theirs during a rebase, so the obvious reach for "the mainline's version"
	// — --theirs — keeps OUR side instead. That resolves the file silently and brings
	// the same conflict back forever, which is the exact failure the carve-out exists
	// to prevent, so naming the correct flag must not be droppable.
	for _, want := range []string{"duplicate fix", "git log --oneline", "DUPLICATE-WORK CANDIDATES", "mainline", "Absent", "--ours"} {
		if !strings.Contains(c.RebaseResolution, want) {
			t.Errorf("RebaseResolution missing %q", want)
		}
	}
	// The main rule ("integrate BOTH sides") must survive alongside the exception.
	if !strings.Contains(c.RebaseResolution, "BOTH sides") {
		t.Error("RebaseResolution lost the main integrate-both-sides rule")
	}
	// The exception must appear after the main rule it qualifies — an agent
	// reading the exception before the rule has no context for it.
	mainIdx := strings.Index(c.RebaseResolution, "BOTH sides")
	exIdx := strings.Index(c.RebaseResolution, "duplicate fix")
	if mainIdx >= exIdx {
		t.Errorf("duplicate-fix exception (at %d) must appear after the BOTH-sides rule (at %d)", exIdx, mainIdx)
	}
	if !strings.Contains(c.DeferCommit, "later step") {
		t.Errorf("DeferCommit missing later-step note: %q", c.DeferCommit)
	}
}

func TestRender_OmitsEmptyDescription(t *testing.T) {
	with := Context{ItemID: "T1", ItemTitle: "t", ItemDescription: "the details"}
	if err := with.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(with.ItemHeader, "Description:") || !strings.Contains(with.ItemHeader, "the details") {
		t.Errorf("expected the description section when present: %q", with.ItemHeader)
	}
	without := Context{ItemID: "T1", ItemTitle: "t"}
	if err := without.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(without.ItemHeader, "Description:") {
		t.Errorf("empty description should be omitted from the header: %q", without.ItemHeader)
	}
}

func TestRender_HonorsOverride(t *testing.T) {
	c := Context{ItemID: "T1", ItemTitle: "t", AskGuidance: "CUSTOM ASK"}
	if err := c.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if c.AskGuidance != "CUSTOM ASK" {
		t.Errorf("Render overwrote a caller-set partial: %q", c.AskGuidance)
	}
	// Non-overridden partials still render from their defaults.
	if !strings.Contains(c.ItemHeader, "T1") {
		t.Error("non-overridden ItemHeader should still render")
	}
}

// rebaseResolution renders the shared partials and hands back the
// rebase-conflict text the duplicate-work assertions below all read.
func rebaseResolution(t *testing.T) string {
	t.Helper()
	c := Context{ItemID: "T1", ItemType: "task", ItemTitle: "do it", VerifyCmd: "make check"}
	if err := c.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return c.RebaseResolution
}

// bulletWith returns the wrapped bullet of s containing needle, so a failure
// quotes the offending sentence instead of dumping the whole partial.
func bulletWith(s, needle string) string {
	at := strings.Index(s, needle)
	if at < 0 {
		return ""
	}
	start := strings.LastIndex(s[:at], "\n- ") + 1
	end := strings.Index(s[at:], "\n- ")
	if end < 0 {
		return s[start:]
	}
	return s[start : at+end]
}

// The carve-out tells the agent to take the mainline's side. Naming the outcome
// is not enough: git SWAPS ours/theirs during a rebase, so `--theirs` — the
// obvious reach for "the mainline's version" — keeps OUR side instead. That
// resolves the file, clears the markers, lets `git rebase --continue` succeed,
// and brings the identical conflict back on every later rebase. It is the exact
// failure the carve-out exists to prevent, reached by obeying it, and nothing
// reports that the wrong side was taken.
//
// TestRender_FillsPartials asserts `--ours` appears, which is NOT sufficient:
// the bullet names both flags (one as the trap), so inverting it wholesale —
// "take `git checkout --theirs <file>`, NOT `--ours`" — leaves both literals
// present and passes. Pin the flag to the command instead.
func TestRender_RebaseCarveOutFlagIsNotInverted(t *testing.T) {
	got := rebaseResolution(t)
	if !strings.Contains(got, "checkout --ours") {
		t.Error("carve-out must spell taking the mainline's side as `git checkout --ours`")
	}
	if strings.Contains(got, "checkout --theirs") {
		t.Errorf("carve-out must never spell the checkout with --theirs — mid-rebase that keeps OUR side:\n%s", bulletWith(got, "checkout --theirs"))
	}
}

// The exception is gated on evidence, and the gate is what keeps it from
// reading as licence to discard work: absent the evidence, the default —
// integrate both sides — still stands. Two ways that gate can silently rot:
//
//   - The closing "keep both sides" is trimmed as redundant, and the exception
//     becomes unconditional.
//   - The self-serve evidence command is dropped in favour of "read the
//     DUPLICATE-WORK CANDIDATES block", which only exists when a tracker runner
//     computed it. The SDK also drives standalone flows where nothing appends
//     that block, so the agent would be left with a rule it cannot apply.
func TestRender_RebaseCarveOutIsGatedOnEvidenceItCanObtainAlone(t *testing.T) {
	got := rebaseResolution(t)
	if !strings.Contains(got, "keep both sides") {
		t.Error("carve-out must re-arm the default (\"keep both sides\") when the evidence is absent")
	}
	if !strings.Contains(got, "git log") {
		t.Error("carve-out must name a way to obtain the evidence without the DUPLICATE-WORK CANDIDATES block")
	}
}

// An exception has to arrive after the rule it qualifies and before the step
// that ends the resolution, or it reads as a standalone instruction. Reordered
// above "integrate BOTH sides" it is an unconditional "take the mainline's
// version"; pushed below "git rebase --continue" the agent has already been told
// to finish before learning the case exists.
func TestRender_RebaseCarveOutFollowsTheRuleItQualifies(t *testing.T) {
	got := rebaseResolution(t)
	order := []string{"integrate BOTH sides", "Exception", "checkout --ours", "git rebase --continue"}
	prev, prevWant := -1, ""
	for _, want := range order {
		at := strings.Index(got, want)
		if at < 0 {
			t.Fatalf("RebaseResolution missing %q (expected order: %v)", want, order)
		}
		if at < prev {
			t.Errorf("%q must come after %q (expected order: %v)", want, prevWant, order)
		}
		prev, prevWant = at, want
	}
}

// A prose edit to a partial can reference a field that does not exist. That
// parses fine, so template.Must's init-time panic does not catch it — the
// failure surfaces only at ExecuteTemplate, and Render's error is the one thing
// between the typo and a prompt quietly missing a whole section. Verify it
// fires, names the partial that broke, keeps the cause, and writes nothing.
func TestRender_ReportsTheFailingPartialByName(t *testing.T) {
	restoreTmpl, restorePartials := partialTmpl, partials
	t.Cleanup(func() { partialTmpl, partials = restoreTmpl, restorePartials })

	partialTmpl = template.Must(template.New("rebase_resolution.tmpl").Parse(`{{.NoSuchField}}`))
	partials = []struct {
		tmpl string
		get  func(*Context) string
		set  func(*Context, string)
	}{
		{"rebase_resolution.tmpl", func(c *Context) string { return c.RebaseResolution }, func(c *Context, s string) { c.RebaseResolution = s }},
	}

	c := Context{ItemID: "T1", ItemTitle: "t"}
	err := c.Render()
	if err == nil {
		t.Fatal("Render must fail when a partial cannot execute, not render it away silently")
	}
	if !strings.Contains(err.Error(), "rebase_resolution.tmpl") {
		t.Errorf("error must name the partial that broke: %v", err)
	}
	if !strings.Contains(err.Error(), "NoSuchField") {
		t.Errorf("error must keep the underlying cause: %v", err)
	}
	if c.RebaseResolution != "" {
		t.Errorf("a failed partial must be left unwritten, not half-rendered: %q", c.RebaseResolution)
	}
}
