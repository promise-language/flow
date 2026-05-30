package common

import (
	"fmt"
	"os"
)

// HasHelpFlag reports whether args request usage. It accepts both flag prefixes
// (-help and --help) and the short form (-h / --h), normalizing long to short
// via NormalizeArgs first.
func HasHelpFlag(args []string) bool {
	for _, a := range NormalizeArgs(args) {
		if a == "-h" || a == "-help" {
			return true
		}
	}
	return false
}

// MaybeHelp prints usage and exits 0 when args request help; otherwise it
// returns and the caller proceeds. Call it first thing in a tool's main — help
// must work regardless of staleness, so it runs before CheckStale.
func MaybeHelp(args []string, usage string) {
	if HasHelpFlag(args) {
		fmt.Println(usage)
		os.Exit(0)
	}
}
