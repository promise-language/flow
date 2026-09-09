package flow

import (
	"encoding/json"
	"testing"
)

// ArtifactBody.Empty is ONE definition applied by every orchestrator that
// stores the bytes itself, so it is tested here rather than only through the
// two AppendEntry implementations that call it: a third orchestrator has to get
// the same answer, or the same entry is legal against one store and refused by
// another.
//
// The rows that matter are the ones an implementation could plausibly get
// wrong: a FLAG carries no payload and is still never empty, a value that is
// only whitespace carries nothing, and a body naming no type at all has nothing
// it could be carrying.
func TestArtifactBody_Empty(t *testing.T) {
	tests := []struct {
		name string
		body ArtifactBody
		want bool
	}{
		{"a flag is never empty", ArtifactBody{Type: ArtifactFlag}, false},

		{"commit hash absent", ArtifactBody{Type: ArtifactCommitHash}, true},
		{"commit hash of whitespace", ArtifactBody{Type: ArtifactCommitHash, CommitHash: " \t\n"}, true},
		{"commit hash", ArtifactBody{Type: ArtifactCommitHash, CommitHash: "0123456789abcdef"}, false},

		{"markdown absent", ArtifactBody{Type: ArtifactMarkdown}, true},
		{"markdown of whitespace", ArtifactBody{Type: ArtifactMarkdown, Markdown: "  \n\t "}, true},
		{"markdown", ArtifactBody{Type: ArtifactMarkdown, Markdown: "the plan"}, false},

		{"json absent", ArtifactBody{Type: ArtifactJSON}, true},
		{"json", ArtifactBody{Type: ArtifactJSON, JSON: json.RawMessage(`{}`)}, false},

		// A named file with no bytes is empty: the name is the label for the
		// payload, not the payload.
		{"file named but empty", ArtifactBody{Type: ArtifactFile, File: FileBody{Name: "notes.txt"}}, true},
		{"file", ArtifactBody{Type: ArtifactFile, File: FileBody{Name: "notes.txt", Content: []byte("x")}}, false},

		// Likewise a patch: the base and branch describe where a diff applies,
		// so they cannot stand in for one.
		{"patch with context but no diff", ArtifactBody{Type: ArtifactPatch, Patch: PatchBody{BaseSHA: "abc", BaseBranch: "main"}}, true},
		{"patch", ArtifactBody{Type: ArtifactPatch, Patch: PatchBody{Diff: []byte("--- a\n+++ b\n")}}, false},

		{"no type at all", ArtifactBody{}, true},
		{"a type nothing declares", ArtifactBody{Type: ArtifactType(99)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.body.Empty(); got != tt.want {
				t.Errorf("Empty() = %v, want %v", got, tt.want)
			}
		})
	}
}
