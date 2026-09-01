package cli

import (
	"strings"

	"github.com/promise-language/flow"
)

// formatClaimRefusal renders an ErrClaimRefused for the operator. Output shape:
//
//	claim: refused — arena not admitted (check "git-identity")
//	  author email "djabi@kmac" is not a @users.noreply.github.com address
//	  override with --force-unadmitted (audited)
func formatClaimRefusal(prefix string, e flow.ErrClaimRefused) string {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString(": refused — ")
	b.WriteString(e.Reason)
	if e.Check != "" {
		b.WriteString(" (check \"")
		b.WriteString(e.Check)
		b.WriteString("\")")
	}
	if e.Detail != "" {
		for _, line := range strings.Split(e.Detail, "\n") {
			b.WriteString("\n  ")
			b.WriteString(line)
		}
	}
	if e.Override != "" {
		b.WriteString("\n  override with --")
		b.WriteString(e.Override)
		b.WriteString(" (audited)")
	}
	return b.String()
}
