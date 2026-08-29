package main

import (
	"strings"
	"testing"
	"text/template"

	"github.com/promise-language/flow/issue"
)

// This project's bodies REPLACE the library defaults rather than extending
// them, so the library's own test that every default renders the stashed work
// says nothing about the prompts this binary actually sends. A body here that
// drops the block re-derives, silently and expensively, exactly what an
// earlier invocation already paid for — and for the plan step, which changes
// no files, that is the whole step.
func TestProjectPromptsRenderTheWorkInProgressBlock(t *testing.T) {
	const notes = "what I worked out before I stopped"
	pc := issue.PromptContext{WorkInProgress: notes}
	pc.VerifyCmd = "bin/verify"
	if err := pc.Context.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for id, body := range prompts {
		// The fix re-prompt is the one exclusion, for the same reason as in the
		// library: it runs inside a single implement invocation, resuming the
		// session that has the working-out in context already.
		if id == issue.PromptImplementFix {
			continue
		}
		t.Run(string(id), func(t *testing.T) {
			tmpl, err := template.New(string(id)).Parse(body)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var b strings.Builder
			if err := tmpl.Execute(&b, pc); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if !strings.Contains(b.String(), notes) {
				t.Errorf("project prompt %q does not render the stashed work — a resumed step would re-derive it", id)
			}
			// The framing travels with the notes: read as a finished result,
			// they would be defended instead of finished.
			if !strings.Contains(b.String(), "not a result") {
				t.Errorf("project prompt %q renders the notes without saying they are scaffolding", id)
			}
		})
	}
}
