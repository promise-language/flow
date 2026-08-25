package github

import "testing"

// The flow's own bookkeeping comments must never read as a human's answer.
//
// This is matched by shared prefix rather than by an enumerated list because an
// enumeration rotted once already: "flow:park" was missing, and since Park
// posts its comment moments after a question is stamped, every question park
// read as its own answer — the gate cleared itself on the next run, the step
// re-asked, and park-for-answer could never hold.
func TestIsFlowMachineComment(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"state", "<!-- flow:state-v1 begin owner=alice -->\nflow: x", true},
		{"artifact", "<!-- flow:artifact id=plan -->\nbody", true},
		{"question", "<!-- flow:question ts=2026-08-25T12:00:00Z -->\n### q", true},
		{"park", "<!-- flow:park -->\n```json\n{}\n```", true},
		// A marker this test does not know about must still be excluded.
		{"marker added later", "<!-- flow:something-new -->\npayload", true},
		{"human answer", "Use postgres — the existing cache is not durable enough.", false},
		{"human quoting the syntax", "why does it say `<!-- flow` in the thread?", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFlowMachineComment(tc.body); got != tc.want {
				t.Errorf("isFlowMachineComment(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// The state comment is an INDEX: it records that a markdown artifact resolved
// but not its bytes, which live in a comment of their own. Without hydration a
// resolved artifact loads with an empty body while still reporting Resolved,
// so a step reading it gets ("", true) and proceeds on nothing — an implement
// step writing code against a blank plan, with no error anywhere.
func TestArtifactCommentRe(t *testing.T) {
	for _, tc := range []struct {
		name          string
		body          string
		wantID, wantV string
		wantMatch     bool
	}{
		{
			"markdown artifact",
			"<!-- flow:artifact id=plan type=markdown v=1 by=alice ts=2026-08-25T12:00:00Z -->\nthe plan body",
			"plan", "1", true,
		},
		{
			"later version",
			"<!-- flow:artifact id=plan type=markdown v=7 by=alice ts=2026-08-25T12:00:00Z -->\nv7",
			"plan", "7", true,
		},
		// Only the artifact marker; the others must not be mistaken for one.
		{"state comment", "<!-- flow:state-v1 begin owner=alice -->\nflow: x", "", "", false},
		{"question comment", "<!-- flow:question ts=2026-08-25T12:00:00Z -->\n### q", "", "", false},
		{"human prose", "here is my answer", "", "", false},
		// Must anchor at the start: a quoted reply is not an artifact comment.
		{"quoted marker", "> <!-- flow:artifact id=plan type=markdown v=1 by=a ts=t -->", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := artifactCommentRe.FindStringSubmatch(tc.body)
			if (m != nil) != tc.wantMatch {
				t.Fatalf("match = %v, want %v", m != nil, tc.wantMatch)
			}
			if !tc.wantMatch {
				return
			}
			if m[1] != tc.wantID || m[3] != tc.wantV {
				t.Errorf("got id=%q v=%q, want id=%q v=%q", m[1], m[3], tc.wantID, tc.wantV)
			}
			// The body is everything after the marker line.
			if got := tc.body[len(m[0]):]; got == "" {
				t.Error("body was consumed by the marker match")
			}
		})
	}
}
