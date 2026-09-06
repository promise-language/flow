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
	b.WriteString(formatRefusal(prefix, e.Reason, e.Check, e.Detail))
	if e.Override != "" {
		b.WriteString("\n  override with --")
		b.WriteString(e.Override)
		b.WriteString(" (audited)")
	}
	return b.String()
}

// formatRefusal is the shape itself: what was refused, why, which check
// answered, and the detail indented under it. Shared by the claim refusal above
// and by the boundary refusal in requireRunnable, which is not a claim refusal
// and so does not borrow its type — only the layout a reader has learned.
func formatRefusal(prefix, reason, check, detail string) string {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString(": refused — ")
	b.WriteString(reason)
	if check != "" {
		b.WriteString(" (check \"")
		b.WriteString(check)
		b.WriteString("\")")
	}
	if detail != "" {
		for _, line := range strings.Split(detail, "\n") {
			b.WriteString("\n  ")
			b.WriteString(line)
		}
	}
	return b.String()
}
