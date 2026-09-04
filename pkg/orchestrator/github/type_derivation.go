package github

import (
	"strings"

	"github.com/promise-language/flow"
)

// itemTypeFromLabels scans the label set for the first `type:<x>` label and
// returns the suffix as a flow.ItemType. The `type:` prefix is NOT
// flow-managed (it lives in the user's own label vocabulary), so it is
// always the bare string "type:" regardless of cfg.LabelPrefix. When no
// type label is present, returns defaultType (which may be empty — empty
// means "exclude from ListEligible").
func itemTypeFromLabels(_ labels, names []string, defaultType string) flow.ItemType {
	const want = "type:"
	for _, n := range names {
		if strings.HasPrefix(n, want) {
			return flow.ItemType(n[len(want):])
		}
	}
	return flow.ItemType(defaultType)
}
