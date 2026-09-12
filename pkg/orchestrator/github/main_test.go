package github

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points the machine-wide GitHub cache (cache.go) at a throwaway
// directory for the whole package, before any test runs.
//
// The same doctrine cli/main_test.go carries for the quota cache, and for the
// same reason: the record is SHARED by every flow process on the machine, so a
// test that wrote one would not merely mislead itself — it would answer the
// operator's next real `bin/issue status` from bytes a test made up, including
// a rate-limit refusal that would make the next real run fail fast against a
// limit that never existed. Redirecting it here rather than per test is what
// makes it unforgettable: a test added later cannot omit it.
//
// Individual tests that care about cache CONTENT take a fresh directory of
// their own with useTempSeamCache.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "flow-github-cache-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "github tests: cannot redirect the seam cache:", err)
		os.Exit(1)
	}
	cacheDir = func() (string, bool) { return dir, true }
	code := m.Run()
	// os.Exit skips deferred calls, so the cleanup is explicit.
	os.RemoveAll(dir)
	os.Exit(code)
}

// useTempSeamCache gives one test its own cache directory, so a record it
// writes is not read by the next test. Returns the directory.
//
// Taken by every test that drives the mock server more than once: without it
// the package-wide directory would serve one test's issue read to another's,
// and the tests would pass or fail on their order.
func useTempSeamCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := cacheDir
	cacheDir = func() (string, bool) { return dir, true }
	t.Cleanup(func() { cacheDir = prev })
	return dir
}
