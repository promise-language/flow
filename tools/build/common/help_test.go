package common

import "testing"

func TestHasHelpFlag(t *testing.T) {
	help := [][]string{
		{"-h"}, {"--h"}, {"-help"}, {"--help"},
		{"build", "--help"}, {"-force", "-h"},
	}
	for _, args := range help {
		if !HasHelpFlag(args) {
			t.Errorf("expected help for %v", args)
		}
	}
	notHelp := [][]string{
		nil, {}, {"-force"}, {"--force"}, {"-helper"}, {"--helpme"}, {"help"},
	}
	for _, args := range notHelp {
		if HasHelpFlag(args) {
			t.Errorf("expected no help for %v", args)
		}
	}
}
