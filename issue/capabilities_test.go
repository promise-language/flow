package issue

import (
	"testing"

	ghbackend "github.com/promise-language/flow/pkg/orchestrator/github"
)

// The github backend must satisfy every optional capability this package
// probes for. A missed method degrades silently at runtime (role undetectable,
// answers unreadable), so assert it at compile time.
func TestGithubBackendSatisfiesCapabilities(t *testing.T) {
	var b *ghbackend.Orchestrator
	var _ RoleProber = b
	var _ BranchDetector = b
	var _ AnswerReader = b
	var _ Principal = b
}
