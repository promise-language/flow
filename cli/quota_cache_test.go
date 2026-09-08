package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func captureReportQuota(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	reportQuota(&buf)
	return buf.String()
}

// ---------------------------------------------------------------------------
// Test scaffolding.
//
// Every test here drives quotaNow through the two seams quota_cache.go
// declares — the cache directory (redirected package-wide by TestMain, and per
// test by useTempQuotaCache) and quotaFetch. Nothing in this file reaches the
// network or the developer's real cache.
// ---------------------------------------------------------------------------

// fetchStub is a quotaFetch that counts its calls. The count is what most of
// these tests actually assert: the defect was call volume, so "did not ask" is
// the behaviour under test.
type fetchStub struct {
	mu    sync.Mutex
	calls int
	fn    func() ([]windowUsage, error)
}

func (s *fetchStub) fetch() ([]windowUsage, error) {
	s.mu.Lock()
	s.calls++
	fn := s.fn
	s.mu.Unlock()
	return fn()
}

func (s *fetchStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// installFetch points quotaFetch at fn for the duration of one test.
func installFetch(t *testing.T, fn func() ([]windowUsage, error)) *fetchStub {
	t.Helper()
	s := &fetchStub{fn: fn}
	prev := quotaFetch
	quotaFetch = s.fetch
	t.Cleanup(func() { quotaFetch = prev })
	return s
}

// refuseFetch is a quotaFetch that always refuses, as the endpoint does when a
// machine full of resolves has earned a 429.
func refuseFetch(retryAfter time.Duration) func() ([]windowUsage, error) {
	return func() ([]windowUsage, error) {
		return nil, &quotaRefusal{msg: "rejected — HTTP 429", retryAfter: retryAfter}
	}
}

// usageAt builds a one-window reading with the given utilisation.
func usageAt(used float64) []windowUsage {
	return []windowUsage{{
		Label:    "5h",
		Length:   5 * time.Hour,
		Used:     used,
		ResetsAt: time.Now().Add(2 * time.Hour),
	}}
}

// seedRecord writes a cache record directly, standing in for whatever another
// process on the machine left behind.
func seedRecord(t *testing.T, dir string, rec quotaRecord) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, quotaCacheFile), b, 0o600); err != nil {
		t.Fatalf("seed record: %v", err)
	}
}

func readRecord(t *testing.T, dir string) quotaRecord {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, quotaCacheFile))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var rec quotaRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("parse record: %v", err)
	}
	return rec
}

func firstUsed(t *testing.T, r quotaReading) float64 {
	t.Helper()
	if len(r.Usage) == 0 {
		t.Fatalf("reading carried no windows")
	}
	return r.Usage[0].Used
}

// ---------------------------------------------------------------------------
// The policy.
// ---------------------------------------------------------------------------

func TestQuotaNow_FreshHitMakesNoRequest(t *testing.T) {
	dir := useTempQuotaCache(t)
	seedRecord(t, dir, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-1 * time.Minute)})
	s := installFetch(t, func() ([]windowUsage, error) {
		t.Error("a reading inside the refresh interval must not touch the endpoint")
		return usageAt(0.99), nil
	})

	r, err := quotaNow()
	if err != nil {
		t.Fatalf("quotaNow: %v", err)
	}
	if got := firstUsed(t, r); got != 0.42 {
		t.Errorf("used = %v, want the cached 0.42", got)
	}
	if r.Failure != "" {
		t.Errorf("a healthy fresh reading carries no failure; got %q", r.Failure)
	}
	if s.count() != 0 {
		t.Errorf("fetches = %d, want 0", s.count())
	}
}

func TestQuotaNow_FiftyReadsMakeOneRequest(t *testing.T) {
	// The defect, stated as a test: one resolve's fifty in-loop reads.
	useTempQuotaCache(t)
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })

	for range maxResolveSteps {
		if _, err := quotaNow(); err != nil {
			t.Fatalf("quotaNow: %v", err)
		}
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d over %d reads, want 1", s.count(), maxResolveSteps)
	}
}

func TestQuotaNow_StaleRefreshesAndStores(t *testing.T) {
	dir := useTempQuotaCache(t)
	seedRecord(t, dir, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-10 * time.Minute)})
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.55), nil })

	r, err := quotaNow()
	if err != nil {
		t.Fatalf("quotaNow: %v", err)
	}
	if got := firstUsed(t, r); got != 0.55 {
		t.Errorf("used = %v, want the refreshed 0.55", got)
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d, want 1", s.count())
	}
	rec := readRecord(t, dir)
	if len(rec.Usage) == 0 || rec.Usage[0].Used != 0.55 {
		t.Errorf("record was not updated: %+v", rec)
	}
	if time.Since(rec.ReadAt) > time.Minute {
		t.Errorf("stored ReadAt = %v, want ~now", rec.ReadAt)
	}
}

func TestQuotaNow_SuccessClearsAPriorFailure(t *testing.T) {
	dir := useTempQuotaCache(t)
	seedRecord(t, dir, quotaRecord{
		Usage:   usageAt(0.42),
		ReadAt:  time.Now().Add(-10 * time.Minute),
		Failure: "rejected — HTTP 429",
		RetryAt: time.Now().Add(-time.Second), // backoff just elapsed
	})
	installFetch(t, func() ([]windowUsage, error) { return usageAt(0.55), nil })

	r, err := quotaNow()
	if err != nil {
		t.Fatalf("quotaNow: %v", err)
	}
	if r.Failure != "" {
		t.Errorf("a successful refresh must clear the failure; got %q", r.Failure)
	}
	rec := readRecord(t, dir)
	if rec.Failure != "" || !rec.RetryAt.IsZero() {
		t.Errorf("stored record still carries the old failure: %+v", rec)
	}
}

func TestQuotaNow_StaleFailureServesStaleAndBacksOff(t *testing.T) {
	// The 429 case, and the whole point of the change: the run keeps pacing on
	// the last known-good reading instead of switching pacing off, and stops
	// asking the endpoint that just refused.
	dir := useTempQuotaCache(t)
	seedRecord(t, dir, quotaRecord{Usage: usageAt(0.91), ReadAt: time.Now().Add(-10 * time.Minute)})
	s := installFetch(t, refuseFetch(0))

	r, err := quotaNow()
	if err != nil {
		t.Fatalf("a failed refresh over a good reading must not error: %v", err)
	}
	if got := firstUsed(t, r); got != 0.91 {
		t.Errorf("used = %v, want the preserved 0.91", got)
	}
	if !strings.Contains(r.Failure, "429") {
		t.Errorf("reading should name the refusal in force; got %q", r.Failure)
	}
	if s.count() != 1 {
		t.Fatalf("fetches = %d, want 1", s.count())
	}

	// Every subsequent read inside the backoff serves the same reading and asks
	// nothing. This is the amplifier the defect described: retry every
	// iteration, warn once.
	for range 10 {
		again, err := quotaNow()
		if err != nil {
			t.Fatalf("quotaNow inside backoff: %v", err)
		}
		if got := firstUsed(t, again); got != 0.91 {
			t.Errorf("used = %v, want the preserved 0.91", got)
		}
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d after 11 reads inside the backoff, want 1", s.count())
	}
	if rec := readRecord(t, dir); len(rec.Usage) == 0 || rec.Usage[0].Used != 0.91 {
		t.Errorf("a failed refresh must preserve the reading on disk; got %+v", rec)
	}
}

func TestQuotaNow_ColdFailureErrorsAndCachesTheFailure(t *testing.T) {
	// Warn once, proceed unpaced: with nothing cached and a refusing endpoint,
	// the behaviour flow has today is the behaviour that must survive.
	dir := useTempQuotaCache(t)
	s := installFetch(t, refuseFetch(0))

	_, err := quotaNow()
	if err == nil {
		t.Fatal("expected an error with no reading and a failed refresh")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should carry the refusal; got %q", err)
	}
	if s.count() != 1 {
		t.Fatalf("fetches = %d, want 1", s.count())
	}

	// The failure is shared, so the next process reports it without asking.
	_, err = quotaNow()
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Errorf("second read should report the cached refusal; got %v", err)
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d, want the backoff to hold at 1", s.count())
	}
	if rec := readRecord(t, dir); rec.Failure == "" || rec.RetryAt.IsZero() {
		t.Errorf("failure was not cached: %+v", rec)
	}
}

func TestQuotaNow_RetryAfterSetsTheBackoff(t *testing.T) {
	dir := useTempQuotaCache(t)
	installFetch(t, refuseFetch(90*time.Second))

	if _, err := quotaNow(); err == nil {
		t.Fatal("expected an error")
	}
	rec := readRecord(t, dir)
	wait := time.Until(rec.RetryAt)
	if wait < 80*time.Second || wait > 100*time.Second {
		t.Errorf("RetryAt is %v away, want ~90s (the endpoint's own Retry-After)", wait)
	}
}

func TestQuotaNow_DefaultBackoffWhenNoRetryAfter(t *testing.T) {
	dir := useTempQuotaCache(t)
	installFetch(t, func() ([]windowUsage, error) { return nil, errors.New("transport — no route to host") })

	if _, err := quotaNow(); err == nil {
		t.Fatal("expected an error")
	}
	rec := readRecord(t, dir)
	wait := time.Until(rec.RetryAt)
	if wait < quotaFailureBackoff-time.Minute || wait > quotaFailureBackoff {
		t.Errorf("RetryAt is %v away, want ~%v", wait, quotaFailureBackoff)
	}
}

func TestQuotaNow_AncientReadingIsDropped(t *testing.T) {
	dir := useTempQuotaCache(t)
	seedRecord(t, dir, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-quotaStaleBound - time.Minute)})
	installFetch(t, refuseFetch(0))

	_, err := quotaNow()
	if err == nil {
		t.Fatal("a reading past the outer bound is not a reading — expected an error")
	}
}

func TestQuotaNow_FutureDatedReadingIsAMiss(t *testing.T) {
	// A clock that jumped backwards must not pin the cache forever.
	dir := useTempQuotaCache(t)
	seedRecord(t, dir, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(2 * time.Hour)})
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.55), nil })

	r, err := quotaNow()
	if err != nil {
		t.Fatalf("quotaNow: %v", err)
	}
	if got := firstUsed(t, r); got != 0.55 {
		t.Errorf("used = %v, want the refreshed 0.55", got)
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d, want 1", s.count())
	}
}

// ---------------------------------------------------------------------------
// Degrading to today's behaviour. Nothing about the cache may fail a run.
// ---------------------------------------------------------------------------

func TestQuotaNow_TornFileReadsAsAMiss(t *testing.T) {
	dir := useTempQuotaCache(t)
	if err := os.WriteFile(filepath.Join(dir, quotaCacheFile), []byte(`{"usage":[{"lab`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.55), nil })

	r, err := quotaNow()
	if err != nil {
		t.Fatalf("a torn cache must cost one refresh, not a run: %v", err)
	}
	if got := firstUsed(t, r); got != 0.55 {
		t.Errorf("used = %v, want 0.55", got)
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d, want 1", s.count())
	}
	if rec := readRecord(t, dir); len(rec.Usage) == 0 || rec.Usage[0].Used != 0.55 {
		t.Errorf("the torn file should have been replaced; got %+v", rec)
	}
}

func TestQuotaNow_NoCacheLocationFetchesEveryTime(t *testing.T) {
	prev := quotaCacheDir
	quotaCacheDir = func() (string, bool) { return "", false }
	t.Cleanup(func() { quotaCacheDir = prev })
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })

	for range 3 {
		r, err := quotaNow()
		if err != nil {
			t.Fatalf("quotaNow: %v", err)
		}
		if got := firstUsed(t, r); got != 0.42 {
			t.Errorf("used = %v, want 0.42", got)
		}
	}
	if s.count() != 3 {
		t.Errorf("fetches = %d, want 3 — with nowhere to cache, this is today's behaviour", s.count())
	}
}

func TestQuotaNow_NoCacheLocationSurfacesTheFetchError(t *testing.T) {
	prev := quotaCacheDir
	quotaCacheDir = func() (string, bool) { return "", false }
	t.Cleanup(func() { quotaCacheDir = prev })
	installFetch(t, refuseFetch(0))

	if _, err := quotaNow(); err == nil || !strings.Contains(err.Error(), "429") {
		t.Errorf("error = %v, want the fetch's own refusal", err)
	}
}

func TestQuotaNow_UnwritableCacheDirDegrades(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o700) })
	prev := quotaCacheDir
	quotaCacheDir = func() (string, bool) { return filepath.Join(parent, "flow"), true }
	t.Cleanup(func() { quotaCacheDir = prev })
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })

	for range 2 {
		if _, err := quotaNow(); err != nil {
			t.Fatalf("an uncacheable machine must still read quota: %v", err)
		}
	}
	if s.count() != 2 {
		t.Errorf("fetches = %d, want 2 — uncached is today's behaviour", s.count())
	}
}

func TestQuotaNow_ReadOnlyCacheDirDegrades(t *testing.T) {
	// The directory exists but nothing can be written into it: no record, no
	// lock. Every call fetches, and none of it fails.
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := useTempQuotaCache(t)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })

	for range 2 {
		r, err := quotaNow()
		if err != nil {
			t.Fatalf("quotaNow: %v", err)
		}
		if got := firstUsed(t, r); got != 0.42 {
			t.Errorf("used = %v, want 0.42", got)
		}
	}
	if s.count() != 2 {
		t.Errorf("fetches = %d, want 2 — a lock that cannot be created is not a held lock", s.count())
	}
}

// ---------------------------------------------------------------------------
// Single-flight.
// ---------------------------------------------------------------------------

func TestQuotaNow_LockHeldServesStaleWithoutFetching(t *testing.T) {
	dir := useTempQuotaCache(t)
	seedRecord(t, dir, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-10 * time.Minute)})
	if err := os.WriteFile(filepath.Join(dir, quotaLockFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := installFetch(t, func() ([]windowUsage, error) {
		t.Error("another process holds the refresh — this one must serve what it has")
		return nil, nil
	})

	r, err := quotaNow()
	if err != nil {
		t.Fatalf("quotaNow: %v", err)
	}
	if got := firstUsed(t, r); got != 0.42 {
		t.Errorf("used = %v, want the stale 0.42", got)
	}
	if s.count() != 0 {
		t.Errorf("fetches = %d, want 0", s.count())
	}
}

func TestQuotaNow_LockHeldWithNothingCachedReports(t *testing.T) {
	dir := useTempQuotaCache(t)
	if err := os.WriteFile(filepath.Join(dir, quotaLockFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := installFetch(t, func() ([]windowUsage, error) {
		t.Error("must not fetch while another process holds the refresh")
		return nil, nil
	})

	_, err := quotaNow()
	if err == nil {
		t.Fatal("expected an error with nothing cached and the refresh held")
	}
	if !strings.Contains(err.Error(), "refreshing") {
		t.Errorf("error should say somebody else is asking; got %q", err)
	}
	if s.count() != 0 {
		t.Errorf("fetches = %d, want 0", s.count())
	}
}

func TestQuotaNow_StaleLockIsBroken(t *testing.T) {
	// A process killed mid-refresh leaves the lock behind. A lock nothing can
	// clear would disable refreshing on the machine for good.
	dir := useTempQuotaCache(t)
	lock := filepath.Join(dir, quotaLockFile)
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-quotaRefreshLockTTL - time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.55), nil })

	r, err := quotaNow()
	if err != nil {
		t.Fatalf("quotaNow: %v", err)
	}
	if got := firstUsed(t, r); got != 0.55 {
		t.Errorf("used = %v, want 0.55", got)
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d, want 1", s.count())
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("the lock should have been released; stat err = %v", err)
	}
}

func TestQuotaNow_ConcurrentReadersAreBounded(t *testing.T) {
	// Run under -race. Every goroutine gets a coherent reading, and the whole
	// group makes at most one request each — never more.
	dir := useTempQuotaCache(t)
	seedRecord(t, dir, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-10 * time.Minute)})
	s := installFetch(t, func() ([]windowUsage, error) {
		time.Sleep(10 * time.Millisecond)
		return usageAt(0.55), nil
	})

	const n = 8
	var wg sync.WaitGroup
	used := make([]float64, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := quotaNow()
			errs[i] = err
			if len(r.Usage) > 0 {
				used[i] = r.Usage[0].Used
			}
		}()
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Errorf("goroutine %d: %v", i, errs[i])
			continue
		}
		if used[i] != 0.42 && used[i] != 0.55 {
			t.Errorf("goroutine %d read %v, want the stale 0.42 or the refreshed 0.55", i, used[i])
		}
	}
	if c := s.count(); c < 1 || c > n {
		t.Errorf("fetches = %d, want between 1 and %d", c, n)
	}
}

func TestQuotaNow_LeavesNoStrayFiles(t *testing.T) {
	dir := useTempQuotaCache(t)
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })
	if _, err := quotaNow(); err != nil {
		t.Fatalf("quotaNow: %v", err)
	}
	// Force a second, failing refresh past the interval.
	seedRecord(t, dir, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-10 * time.Minute)})
	s.mu.Lock()
	s.fn = refuseFetch(0)
	s.mu.Unlock()
	if _, err := quotaNow(); err != nil {
		t.Fatalf("quotaNow: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != quotaCacheFile {
			t.Errorf("stray file left in the cache directory: %q", e.Name())
		}
	}
}

func TestUserQuotaCacheDir(t *testing.T) {
	// The production location: one directory for the OS user, so 25 checkouts
	// of one account share one reading.
	ucd, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("machine has no user cache directory: %v", err)
	}
	dir, ok := userQuotaCacheDir()
	if !ok {
		t.Fatal("expected a cache directory on a machine that has one")
	}
	if want := filepath.Join(ucd, "flow"); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
}

func TestUserQuotaCacheDir_NoneOnThisMachine(t *testing.T) {
	// No HOME and no XDG_CACHE_HOME: nowhere to cache is a machine without a
	// cache, not an error.
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	if dir, ok := userQuotaCacheDir(); ok {
		t.Errorf("expected no cache directory; got %q", dir)
	}
}

func TestStoreQuotaRecord_UnwritableDirIsSilent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	// Not being able to cache a reading is not a reason to fail the run that
	// took it — this returns nothing and must not panic.
	storeQuotaRecord(filepath.Join(dir, quotaCacheFile), quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now()})
	if _, err := os.Stat(filepath.Join(dir, quotaCacheFile)); !os.IsNotExist(err) {
		t.Errorf("nothing should have been written; stat err = %v", err)
	}
}

func TestQuotaNow_UnstorableRecordStillReads(t *testing.T) {
	// Something else owns the record's name — here a directory. The store
	// cannot land, and the run must neither fail nor leave a temp file behind.
	dir := useTempQuotaCache(t)
	if err := os.MkdirAll(filepath.Join(dir, quotaCacheFile, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })

	r, err := quotaNow()
	if err != nil {
		t.Fatalf("an unstorable record must not fail the read: %v", err)
	}
	if got := firstUsed(t, r); got != 0.42 {
		t.Errorf("used = %v, want 0.42", got)
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d, want 1", s.count())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != quotaCacheFile {
			t.Errorf("stray file left behind by the failed store: %q", e.Name())
		}
	}
}

func TestStoreQuotaRecord_Is0600(t *testing.T) {
	dir := useTempQuotaCache(t)
	path := filepath.Join(dir, quotaCacheFile)
	storeQuotaRecord(path, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now()})

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("record mode = %o, want 600", perm)
	}
}

// ---------------------------------------------------------------------------
// The installed reader, and what the display sites print.
// ---------------------------------------------------------------------------

func TestCachedQuota_ServesTheCachedReading(t *testing.T) {
	dir := useTempQuotaCache(t)
	seedRecord(t, dir, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-1 * time.Minute)})
	s := installFetch(t, func() ([]windowUsage, error) {
		t.Error("must not fetch on a fresh cache")
		return nil, nil
	})

	usage, err := cachedQuota()
	if err != nil {
		t.Fatalf("cachedQuota: %v", err)
	}
	if len(usage) != 1 || usage[0].Used != 0.42 {
		t.Errorf("usage = %+v, want the cached 0.42", usage)
	}
	if s.count() != 0 {
		t.Errorf("fetches = %d, want 0", s.count())
	}
}

func TestCachedQuota_PassesTheFailureThrough(t *testing.T) {
	useTempQuotaCache(t)
	installFetch(t, refuseFetch(0))

	usage, err := cachedQuota()
	if err == nil {
		t.Fatal("expected an error")
	}
	if usage != nil {
		t.Errorf("usage = %+v, want nil on error — App.Quota's contract", usage)
	}
}

func TestReportQuota_ThreeShapes(t *testing.T) {
	t.Run("failure only", func(t *testing.T) {
		useTempQuotaCache(t)
		installFetch(t, refuseFetch(0))
		out := captureReportQuota(t)
		if !strings.Contains(out, "quota: rejected — HTTP 429") {
			t.Errorf("want the refusal; got %q", out)
		}
		if strings.Contains(out, "used") {
			t.Errorf("nothing to show — no window lines; got %q", out)
		}
	})

	t.Run("current figures", func(t *testing.T) {
		dir := useTempQuotaCache(t)
		seedRecord(t, dir, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-1 * time.Minute)})
		installFetch(t, func() ([]windowUsage, error) {
			t.Error("must not fetch on a fresh cache")
			return nil, nil
		})
		out := captureReportQuota(t)
		if !strings.Contains(out, "42% used") {
			t.Errorf("want the window line; got %q", out)
		}
		if strings.Contains(out, "refresh failing") {
			t.Errorf("a current reading says nothing about staleness; got %q", out)
		}
	})

	t.Run("stale figures and a failing refresh", func(t *testing.T) {
		dir := useTempQuotaCache(t)
		seedRecord(t, dir, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-10 * time.Minute)})
		installFetch(t, refuseFetch(0))
		out := captureReportQuota(t)
		if !strings.Contains(out, "42% used") {
			t.Errorf("stale figures are still the best available — print them; got %q", out)
		}
		if !strings.Contains(out, "refresh failing: rejected — HTTP 429") {
			t.Errorf("the diagnostic operators read today must survive; got %q", out)
		}
		if !strings.Contains(out, "old") {
			t.Errorf("the age of the figures must be stated; got %q", out)
		}
	})
}
