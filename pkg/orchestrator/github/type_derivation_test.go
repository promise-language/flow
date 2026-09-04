package github

import (
	"testing"

	"github.com/promise-language/flow"
)

func TestItemTypeFromLabels(t *testing.T) {
	l := newLabels("flow:")
	cases := []struct {
		name        string
		labels      []string
		defaultType string
		want        flow.ItemType
	}{
		{"type:task wins", []string{"type:task", "bug"}, "", "task"},
		{"first type:* wins", []string{"foo", "type:bug", "type:task"}, "", "bug"},
		{"no type: → empty", []string{"random", "labels"}, "", ""},
		{"no type: → default", []string{"random"}, "task", "task"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := itemTypeFromLabels(l, c.labels, c.defaultType)
			if got != c.want {
				t.Errorf("itemTypeFromLabels = %q, want %q", got, c.want)
			}
		})
	}
}
