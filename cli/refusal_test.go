package cli

import (
	"errors"
	"fmt"
	"strings"
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

// A rate limit used to reach an operator as `resolve: add claim label: POST
// …/labels: 403 API rate limit exceeded` — a status code, with no name for what
// happened and no instant for when it clears. The seam names both now; this is
// what stops the naming being thrown away one layer up.
func TestConditionOrError(t *testing.T) {
	limited := fmt.Errorf("get issue 42: GitHub secondary rate limit in force; clears at 10:20Z: %w", flow.ErrUnavailable)
	got := conditionOrError("resolve", limited)
	if !strings.Contains(got, "waiting on a condition") {
		t.Errorf("%q does not name the stop as a condition", got)
	}
	for _, want := range []string{"resolve:", "rate limit", "clears at 10:20Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q does not carry %q", got, want)
		}
	}

	// Everything else is unchanged: saying what a stop was must not restate
	// stops that were something else.
	plain := errors.New("dial tcp: connection refused")
	if got := conditionOrError("resolve", plain); got != "resolve: dial tcp: connection refused" {
		t.Errorf("an ordinary error was reworded: %q", got)
	}
}
