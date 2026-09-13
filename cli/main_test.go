package cli

import (
	"fmt"
	"os"
	"sync"
	"testing"
)

// TestMain points the machine-wide quota cache (cli/quota_cache.go) at a
// throwaway directory for the whole package, and its credential discovery at a
// fixed token, before any test runs.
//
// This is the same doctrine App.Quota's own comment carries, one layer down: a
// test must never reach live account state, and now that a reading is SHARED
// between every tool on the machine, a test that wrote one would not merely
// mislead itself — it would pace the operator's next real run against numbers a
// test made up, and a test that read one would take its result from whatever
// the developer's account happened to be doing. Redirecting it here rather than
// per test is what makes it unforgettable: a test added later cannot omit it.
//
// The credential seam is redirected for the same reason and one more: the
// record is named after a digest of the credential, so discovery now runs on
// the path every test takes, and the real one reads the developer's
// .credentials.json — or forks `security` to interrogate the macOS Keychain,
// which on a locked machine prompts.
//
// Individual tests that care about cache CONTENT take a fresh directory of
// their own with useTempQuotaCache; tests that care about WHICH account is
// reading take a credential of their own with installCredential.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "flow-cli-quota-cache-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cli tests: cannot redirect the quota cache:", err)
		os.Exit(1)
	}
	quotaCacheDir = func() (string, bool) { return dir, true }
	quotaCredential = func() (string, string) { return "cli-test-credential", "" }
	// The agent account (cli/quota.go) is redirected here for the same reason
	// and one step further: its discovery reads $HOME, which no CLAUDE_CONFIG_DIR
	// a test sets can shadow, so an unstubbed read would print the developer's
	// own account into a test's expected output. A test about discovery itself
	// restores it with useStubAgentAccount.
	agentAccount = func() string { return "" }
	code := m.Run()
	// os.Exit skips deferred calls, so the cleanup is explicit.
	os.RemoveAll(dir)
	os.Exit(code)
}

// useStubAgentAccount points the agent-account seam at a fixed answer for one
// test, restoring TestMain's empty one afterwards. Tests that assert on the
// account line take it; everything else keeps the package-wide silence.
func useStubAgentAccount(t *testing.T, account string) {
	t.Helper()
	prev := agentAccount
	agentAccount = func() string { return account }
	t.Cleanup(func() { agentAccount = prev })
}

// useTempQuotaCache gives one test its own quota cache directory, so a record
// it writes is not read by the next test. Returns the directory.
func useTempQuotaCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := quotaCacheDir
	quotaCacheDir = func() (string, bool) { return dir, true }
	t.Cleanup(func() { quotaCacheDir = prev })
	return dir
}

// resetQuotaAccountKey clears the once-per-process memo of the account key, so
// the next read discovers the credential again. Only a test has any business
// calling it: within a process the key is a constant, and re-discovering it is
// the per-call subprocess the memo exists to avoid.
func resetQuotaAccountKey() {
	quotaAccountOnce = sync.Once{}
	quotaAccountKeyed = ""
}
