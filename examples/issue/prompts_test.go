package main

import (
	"strings"
	"testing"
	"text/template"

	"github.com/promise-language/flow/issue"
)

// The library now appends required fragments — including WorkInProgressBlock
// and AnswersBlock — to project overrides automatically (#47). That guarantee
// is tested in the library's own TestWorkInProgressReachesEveryResumableDefaultPrompt
// and TestAnswersReachEveryResumableDefaultPrompt (both with override subtests).
//
// This test verifies what the project controls: that its prompt bodies parse
// and render cleanly against a populated PromptContext.
func TestProjectPromptsRender(t *testing.T) {
	pc := issue.PromptContext{WorkInProgress: "notes from earlier"}
	pc.VerifyCmd = "bin/verify"
	pc.VerifyOutput = "FAIL"
	if err := pc.Context.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for id, body := range prompts {
		t.Run(string(id), func(t *testing.T) {
			tmpl, err := template.New(string(id)).Parse(body)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var b strings.Builder
			if err := tmpl.Execute(&b, pc); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if strings.TrimSpace(b.String()) == "" {
				t.Errorf("project prompt %q rendered empty", id)
			}
		})
	}
}
