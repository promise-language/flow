package common

import (
	"fmt"
	"os"
)

// Two output modes, one rule (docs/org/cli-guide.md § 6): a result is rendered
// for a person when stdout is a terminal and as JSON when it is not, and -json
// and -human force either regardless of what stdout is.
//
// This is a second spelling of what cli/output.go does for the SDK's commands,
// and deliberately so: tools/build is a module that depends on nothing of
// flow's, which is what lets ./make build a tree whose SDK does not compile.
// Importing the SDK here to save a file would give that property up.
//
// It is not a copy of that one. The mode here is decided by stdout ALONE, with
// no environment override, because that is what § 6 says decides it: "never
// stderr, never an environment variable". A mode a caller did not type is one
// it cannot see in the command line it is reading back.

// OutputMode is how a result is rendered.
type OutputMode int

const (
	// OutputHuman is the rendering for a person at a terminal. Its labels and
	// its layout are free to improve whenever they read better.
	OutputHuman OutputMode = iota
	// OutputJSON is the interface anything reading the result parses. It grows
	// by adding fields.
	OutputJSON
)

// OutputFlags is what -json and -human were given as, before the default is
// applied. Both false is "not asked", which is the ordinary case.
type OutputFlags struct {
	JSON  bool
	Human bool
}

// TakeOutputFlags strips -json and -human from args and returns the rest.
//
// Stripping rather than parsing with the flag package: these tools take their
// one positional argument in any position and a FlagSet stops at the first
// non-flag, so `run --list -json` and `run -json --list` would not mean the
// same thing. primitives.NormalizeArgs has already made --json and -json one
// name, so callers pass normalized arguments.
func TakeOutputFlags(args []string) ([]string, OutputFlags) {
	var of OutputFlags
	rest := make([]string, 0, len(args))
	for _, a := range args {
		switch a {
		case "-json":
			of.JSON = true
		case "-human":
			of.Human = true
		default:
			rest = append(rest, a)
		}
	}
	return rest, of
}

// Mode resolves the flags against the default.
//
// Passing both is a contradiction rather than a precedence puzzle, so it is the
// usage error § 8 makes it: named, and reported before anything is done.
//
// The default asks stdout whether it is a character device — a terminal is, a
// pipe or a redirect is not. Asking stdout rather than a flag is what makes
// `tool > out.json` produce JSON without anyone having to remember to say so.
func (of OutputFlags) Mode() (OutputMode, error) {
	if of.JSON && of.Human {
		return OutputHuman, fmt.Errorf("-json and -human are mutually exclusive: pass one, or neither to let stdout decide")
	}
	switch {
	case of.JSON:
		return OutputJSON, nil
	case of.Human:
		return OutputHuman, nil
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		// A stdout that cannot be described is not a terminal anyone is
		// reading. JSON survives being piped somewhere unexamined; a human
		// rendering is the one that would silently lose structure.
		return OutputJSON, nil
	}
	if fi.Mode()&os.ModeCharDevice != 0 {
		return OutputHuman, nil
	}
	return OutputJSON, nil
}
