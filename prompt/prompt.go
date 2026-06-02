// Package prompt provides reusable, domain-agnostic prompt fragments
// ("partials") for flow binaries built on github.com/promise-language/flow.
//
// Every project's per-step prompts share a skeleton — identify the item under
// work, tell the agent how to ask the user for a decision, explain how to
// resolve a plan that is already-done / blocked / infeasible, how to finish a
// rebase that stopped on conflicts — and differ only in the project-specific
// body (which build/test commands, which pipeline stages, which conventions).
// This package owns the skeleton so it can be reused across flow binaries
// (e.g. both the tracker and Promise "do" flows) instead of copy-pasted.
//
// Composition is two-phase. The caller fills the raw inputs on a Context, calls
// Render to populate the partial fields, then executes its own per-step
// template against a struct that embeds the Context:
//
//	c := prompt.Context{ItemID: "T1", ItemType: "task", ItemTitle: "…", VerifyCmd: "make check"}
//	_ = c.Render() // fills c.ItemHeader, c.AskGuidance, c.PlanStepResolution, …
//	// the project's step template then references {{.ItemHeader}} {{.PlanStepResolution}} {{.AskGuidance}} …
//
// Partials are rendered in a fixed, documented order (see the partials table);
// each may reference any raw input and any partial rendered before it. A caller
// overrides a partial by setting its field to a non-empty value BEFORE calling
// Render — Render leaves a pre-set field untouched, so partials rendered later
// interpolate the override too.
package prompt

import (
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed partials/*.tmpl
var partialFS embed.FS

// partialTmpl holds every shared partial, keyed by its base filename
// (e.g. "item_header.tmpl"). Parsed once at init; a malformed partial panics
// the program at startup rather than at first render.
var partialTmpl = template.Must(template.New("prompt").ParseFS(partialFS, "partials/*.tmpl"))

// Context is the data for one rendered prompt. The first group is raw input the
// caller sets; the second group is the shared partials Render fills (and that
// the caller may override by pre-setting). A project typically embeds Context in
// a larger struct that adds its own per-step fields, and executes its step
// templates against that struct.
type Context struct {
	// ── Raw inputs (set before Render) ──────────────────────────────────
	ItemID          string // tracker item id, e.g. "T0042"
	ItemType        string // item type, e.g. "task" / "bug" / "plan"
	ItemTitle       string // item title
	ItemDescription string // item description (optional; omitted from the header when empty)
	VerifyCmd       string // the project verify command, e.g. "bin/verify --wasm"

	// ── Shared partials (filled by Render; override by pre-setting) ─────
	// Rendered in the order declared in the partials table below; each may
	// reference any raw input and any partial rendered before it.

	// ItemHeader identifies the item under work. Uses ItemID/ItemType/ItemTitle/ItemDescription.
	ItemHeader string
	// AskGuidance: how to ask the user for a decision (via the MCP question tool, never plain text).
	AskGuidance string
	// PlanStepResolution: the plan-step feasibility decision tree — ALREADY
	// IMPLEMENTED / DUPLICATE (close the item, do not emit a plan), BLOCKED, and
	// NOT FEASIBLE. The already-implemented branch is what prevents the
	// empty-diff implement stall.
	PlanStepResolution string
	// RebaseResolution: how to finish a rebase that stopped on conflicts —
	// resolve integrating both sides, continue, then re-verify. Uses VerifyCmd.
	RebaseResolution string
	// DeferCommit: the reminder that committing/pushing is a later step's job.
	DeferCommit string
}

// partials is the fixed render order and the field wiring for each shared
// partial: the embedded template name, a getter (to detect a caller override),
// and a setter (to write the rendered result back).
var partials = []struct {
	tmpl string
	get  func(*Context) string
	set  func(*Context, string)
}{
	{"item_header.tmpl", func(c *Context) string { return c.ItemHeader }, func(c *Context, s string) { c.ItemHeader = s }},
	{"ask_guidance.tmpl", func(c *Context) string { return c.AskGuidance }, func(c *Context, s string) { c.AskGuidance = s }},
	{"plan_resolution.tmpl", func(c *Context) string { return c.PlanStepResolution }, func(c *Context, s string) { c.PlanStepResolution = s }},
	{"rebase_resolution.tmpl", func(c *Context) string { return c.RebaseResolution }, func(c *Context, s string) { c.RebaseResolution = s }},
	{"defer_commit.tmpl", func(c *Context) string { return c.DeferCommit }, func(c *Context, s string) { c.DeferCommit = s }},
}

// Render fills every partial field the caller has not already set, in the fixed
// order above. A non-empty field is treated as a caller override and left
// untouched, so partials rendered later interpolate the override. The rendered
// text is whitespace-trimmed; the project's step template controls spacing
// between partials.
func (c *Context) Render() error {
	for _, p := range partials {
		if strings.TrimSpace(p.get(c)) != "" {
			continue // caller-supplied override — keep it, and let later partials see it
		}
		var b strings.Builder
		if err := partialTmpl.ExecuteTemplate(&b, p.tmpl, c); err != nil {
			return fmt.Errorf("prompt: render %s: %w", p.tmpl, err)
		}
		p.set(c, strings.TrimSpace(b.String()))
	}
	return nil
}
