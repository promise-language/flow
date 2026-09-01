package cli

import (
	"testing"

	"github.com/promise-language/flow"
)

func TestFormatClaimRefusal(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		err    flow.ErrClaimRefused
		want   string
	}{
		{
			name:   "reason only",
			prefix: "claim",
			err:    flow.ErrClaimRefused{Code: "item-already-leased", Reason: "item already leased"},
			want:   `claim: refused — item already leased`,
		},
		{
			name:   "with check name",
			prefix: "resolve",
			err:    flow.ErrClaimRefused{Code: "not-admitted", Reason: "arena not admitted", Check: "git-identity"},
			want:   `resolve: refused — arena not admitted (check "git-identity")`,
		},
		{
			name:   "with detail",
			prefix: "claim",
			err: flow.ErrClaimRefused{
				Code:   "not-admitted",
				Reason: "arena not admitted",
				Check:  "git-identity",
				Detail: `author email "djabi@kmac" is not a @users.noreply.github.com address`,
			},
			want: "claim: refused \xe2\x80\x94 arena not admitted (check \"git-identity\")\n  author email \"djabi@kmac\" is not a @users.noreply.github.com address",
		},
		{
			name:   "with override",
			prefix: "claim",
			err: flow.ErrClaimRefused{
				Code:     "not-admitted",
				Reason:   "arena not admitted",
				Override: "force-unadmitted",
			},
			want: "claim: refused \xe2\x80\x94 arena not admitted\n  override with --force-unadmitted (audited)",
		},
		{
			name:   "all fields",
			prefix: "claim",
			err: flow.ErrClaimRefused{
				Code:     "not-admitted",
				Reason:   "arena not admitted",
				Check:    "git-identity",
				Detail:   "line 1\nline 2",
				Override: "force-unadmitted",
			},
			want: "claim: refused \xe2\x80\x94 arena not admitted (check \"git-identity\")\n  line 1\n  line 2\n  override with --force-unadmitted (audited)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatClaimRefusal(c.prefix, c.err)
			if got != c.want {
				t.Errorf("formatClaimRefusal(%q, ...):\n got: %q\nwant: %q", c.prefix, got, c.want)
			}
		})
	}
}
