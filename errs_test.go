package flow_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/promise-language/flow"
)

func TestErrClaimRefused_Error(t *testing.T) {
	cases := []struct {
		name string
		err  flow.ErrClaimRefused
		want string
	}{
		{
			name: "reason only",
			err:  flow.ErrClaimRefused{Code: "item-already-leased", Reason: "item already leased"},
			want: "claim refused: item already leased",
		},
		{
			name: "with check",
			err:  flow.ErrClaimRefused{Code: "not-admitted", Reason: "arena not admitted", Check: "git-identity"},
			want: `claim refused: arena not admitted (check "git-identity")`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.err.Error(); got != c.want {
				t.Errorf("Error() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestErrClaimRefused_ErrorsAs(t *testing.T) {
	original := flow.ErrClaimRefused{
		Code:       "not-admitted",
		ItemScoped: false,
		Reason:     "arena not admitted",
		Check:      "git-identity",
	}
	wrapped := fmt.Errorf("backend: %w", original)

	var target flow.ErrClaimRefused
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed on wrapped ErrClaimRefused")
	}
	if target.Code != "not-admitted" {
		t.Errorf("Code = %q, want not-admitted", target.Code)
	}
	if target.ItemScoped {
		t.Error("ItemScoped = true, want false")
	}
	if target.Check != "git-identity" {
		t.Errorf("Check = %q, want git-identity", target.Check)
	}
}
