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

// Both message forms. The raiser can enumerate the alternatives or it cannot,
// and the message has to say which: a refusal that named neither would leave
// the reader unable to tell "you typed a name nothing declares" from "this flow
// declares nothing at all".
func TestErrUnknownRole_Error(t *testing.T) {
	cases := []struct {
		name string
		err  flow.ErrUnknownRole
		want string
	}{
		{
			name: "no alternatives to name",
			err:  flow.ErrUnknownRole{Role: "reviewer"},
			want: `role "reviewer" is not declared by this flow`,
		},
		{
			name: "the declared set travels with it",
			err:  flow.ErrUnknownRole{Role: "reviewer", Declared: []flow.RoleName{"contributor", "maintainer"}},
			want: `role "reviewer" is not declared by this flow; declared roles are [contributor maintainer]`,
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

// It survives wrapping with both fields intact. ValidateGraph wraps it into a
// message naming the step, and a caller reading the graph's refusal has to be
// able to recover which role named nothing.
func TestErrUnknownRole_ErrorsAs(t *testing.T) {
	original := flow.ErrUnknownRole{Role: "reviewer", Declared: []flow.RoleName{"contributor"}}
	wrapped := fmt.Errorf(`flow "resolve": step "review the work" (review): %w`, original)

	var target flow.ErrUnknownRole
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed on wrapped ErrUnknownRole")
	}
	if target.Role != "reviewer" {
		t.Errorf("Role = %q, want reviewer", target.Role)
	}
	if len(target.Declared) != 1 || target.Declared[0] != "contributor" {
		t.Errorf("Declared = %v, want the declared set carried through", target.Declared)
	}
}

func TestErrWaitsOnItems_Error(t *testing.T) {
	refs := []flow.ItemRef{{Display: "o/r#7"}, {Display: "o/r#8"}}
	if got, want := (flow.ErrWaitsOnItems{Refs: refs}).Error(), "waits on items: o/r#7, o/r#8"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if got, want := (flow.ErrWaitsOnItems{}).Error(), "waits on items: (none)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// The sentinel survives wrapping, carries its refs through, and is none of the
// other sentinels: a branch that read it as a refusal would park what should
// clear on its own.
func TestErrWaitsOnItems_ErrorsAs(t *testing.T) {
	original := flow.ErrWaitsOnItems{Refs: []flow.ItemRef{{Display: "o/r#7"}}}
	wrapped := fmt.Errorf("plan: %w", original)

	var target flow.ErrWaitsOnItems
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed on wrapped ErrWaitsOnItems")
	}
	if len(target.Refs) != 1 || target.Refs[0].Display != "o/r#7" {
		t.Errorf("Refs = %+v, want the one ref carried through", target.Refs)
	}
	for _, other := range []error{flow.ErrRefused, flow.ErrTransient, flow.ErrUnfit} {
		if errors.Is(wrapped, other) {
			t.Errorf("errors.Is(%v, %v) = true — the stop would take that branch instead", wrapped, other)
		}
	}
}
