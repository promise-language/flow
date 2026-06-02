package flow

import "strings"

// IsNonFastForward reports whether git push output indicates a
// "remote moved" (non-fast-forward) rejection — origin holds commits the
// local branch does not, so the only fix is to fetch + rebase onto the
// tip and retry the push (NOT to re-run an earlier step). Pass the
// combined stdout+stderr of the push.
//
// Distinct from an "already up to date" result (an idempotent re-push)
// and from a transport / remote-unreachable failure (infrastructure):
// only a non-fast-forward is resolved by rebasing. Lives in flow so both
// the tracker and promise do-flows classify push rejections the same way.
func IsNonFastForward(pushOutput string) bool {
	s := strings.ToLower(pushOutput)
	return strings.Contains(s, "non-fast-forward") ||
		strings.Contains(s, "fetch first") ||
		strings.Contains(s, "tip of your current branch is behind") ||
		strings.Contains(s, "remote contains work that you do not have")
}
