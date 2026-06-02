package flow

import "testing"

// TestIsNonFastForward covers the push-rejection classifier the do-flows
// use to decide "origin moved → fetch + rebase + retry" vs. every other
// rejection. It must match git's several non-fast-forward phrasings, be
// case-insensitive, and NOT fire on unrelated rejections / up-to-date.
func TestIsNonFastForward(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"empty", "", false},
		{"non-fast-forward", "! [rejected]        master -> master (non-fast-forward)", true},
		{"fetch first", "! [rejected] master -> master (fetch first)", true},
		{"tip behind long form", "Updates were rejected because the tip of your current branch is behind its remote counterpart.", true},
		{"remote contains work", "Updates were rejected because the remote contains work that you do not have locally.", true},
		{"case-insensitive", "Non-Fast-Forward", true},
		{"up-to-date is not NFF", "Everything up-to-date", false},
		{"protected branch is not NFF", "remote: error: GH006: Protected branch update failed for refs/heads/master.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNonFastForward(tc.out); got != tc.want {
				t.Errorf("IsNonFastForward(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}
