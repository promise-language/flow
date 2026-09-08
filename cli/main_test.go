package cli

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points the machine-wide quota cache (cli/quota_cache.go) at a
// throwaway directory for the whole package, before any test runs.
//
// This is the same doctrine App.Quota's own comment carries, one layer down: a
// test must never reach live account state, and now that a reading is SHARED
// between every tool on the machine, a test that wrote one would not merely
// mislead itself — it would pace the operator's next real run against numbers a
// test made up, and a test that read one would take its result from whatever
// the developer's account happened to be doing. Redirecting it here rather than
// per test is what makes it unforgettable: a test added later cannot omit it.
//
// Individual tests that care about cache CONTENT take a fresh directory of
// their own with useTempQuotaCache.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "flow-cli-quota-cache-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cli tests: cannot redirect the quota cache:", err)
		os.Exit(1)
	}
	quotaCacheDir = func() (string, bool) { return dir, true }
	code := m.Run()
	// os.Exit skips deferred calls, so the cleanup is explicit.
	os.RemoveAll(dir)
	os.Exit(code)
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
