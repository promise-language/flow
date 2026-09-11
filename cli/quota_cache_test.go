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

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
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
// Every test here drives quotaNow through the three seams quota_cache.go
// declares — the cache directory (redirected package-wide by TestMain, and per
// test by useTempQuotaCache), quotaFetch, and quotaCredential (redirected
// package-wide by TestMain, and per test by installCredential). Nothing in this
// file reaches the network, the developer's real cache, or the developer's real
// credentials.
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

// credStub is a quotaCredential that counts its calls. The count is what the
// keying tests assert: keying may not make a cache HIT pay for a credential
// discovery, which on a Keychain machine forks a subprocess.
type credStub struct {
	mu     sync.Mutex
	calls  int
	token  string
	reason string
}

func (s *credStub) discover() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.token, s.reason
}

func (s *credStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// installCredential points quotaCredential at a fixed token for one test.
//
// The process-wide memo is cleared on the way in and on the way out: the key is
// computed once per PROCESS in production, and a test that switches accounts is
// standing in for a second process, not asking the same one to change its mind.
func installCredential(t *testing.T, token string) *credStub {
	t.Helper()
	return installCredentialResult(t, token, "")
}

// installCredentialFailure stands in for a machine whose credential cannot be
// read at all — no credentials.json, no Keychain entry.
func installCredentialFailure(t *testing.T, reason string) *credStub {
	t.Helper()
	return installCredentialResult(t, "", reason)
}

func installCredentialResult(t *testing.T, token, reason string) *credStub {
	t.Helper()
	s := &credStub{token: token, reason: reason}
	prev := quotaCredential
	quotaCredential = s.discover
	resetQuotaAccountKey()
	t.Cleanup(func() {
		quotaCredential = prev
		resetQuotaAccountKey()
	})
	return s
}

// recordPath is the record this process's account reads and writes.
func recordPath(t *testing.T) string {
	t.Helper()
	path, ok := quotaCachePath()
	if !ok {
		t.Fatalf("no cache path available")
	}
	return path
}

// lockPath is the single-flight lock beside that record.
func lockPath(t *testing.T) string {
	t.Helper()
	return recordPath(t) + quotaLockSuffix
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
func seedRecord(t *testing.T, rec quotaRecord) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if err := os.WriteFile(recordPath(t), b, 0o600); err != nil {
		t.Fatalf("seed record: %v", err)
	}
}

func readRecord(t *testing.T) quotaRecord {
	t.Helper()
	b, err := os.ReadFile(recordPath(t))
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
	useTempQuotaCache(t)
	seedRecord(t, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-1 * time.Minute)})
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
	useTempQuotaCache(t)
	seedRecord(t, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-10 * time.Minute)})
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
	rec := readRecord(t)
	if len(rec.Usage) == 0 || rec.Usage[0].Used != 0.55 {
		t.Errorf("record was not updated: %+v", rec)
	}
	if time.Since(rec.ReadAt) > time.Minute {
		t.Errorf("stored ReadAt = %v, want ~now", rec.ReadAt)
	}
}

func TestQuotaNow_SuccessClearsAPriorFailure(t *testing.T) {
	useTempQuotaCache(t)
	seedRecord(t, quotaRecord{
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
	rec := readRecord(t)
	if rec.Failure != "" || !rec.RetryAt.IsZero() {
		t.Errorf("stored record still carries the old failure: %+v", rec)
	}
}

func TestQuotaNow_StaleFailureServesStaleAndBacksOff(t *testing.T) {
	// The 429 case, and the whole point of the change: the run keeps pacing on
	// the last known-good reading instead of switching pacing off, and stops
	// asking the endpoint that just refused.
	useTempQuotaCache(t)
	seedRecord(t, quotaRecord{Usage: usageAt(0.91), ReadAt: time.Now().Add(-10 * time.Minute)})
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
	if rec := readRecord(t); len(rec.Usage) == 0 || rec.Usage[0].Used != 0.91 {
		t.Errorf("a failed refresh must preserve the reading on disk; got %+v", rec)
	}
}

func TestQuotaNow_ColdFailureErrorsAndCachesTheFailure(t *testing.T) {
	// Warn once, proceed unpaced: with nothing cached and a refusing endpoint,
	// the behaviour flow has today is the behaviour that must survive.
	useTempQuotaCache(t)
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
	if rec := readRecord(t); rec.Failure == "" || rec.RetryAt.IsZero() {
		t.Errorf("failure was not cached: %+v", rec)
	}
}

func TestQuotaNow_RetryAfterSetsTheBackoff(t *testing.T) {
	useTempQuotaCache(t)
	installFetch(t, refuseFetch(90*time.Second))

	if _, err := quotaNow(); err == nil {
		t.Fatal("expected an error")
	}
	rec := readRecord(t)
	wait := time.Until(rec.RetryAt)
	if wait < 80*time.Second || wait > 100*time.Second {
		t.Errorf("RetryAt is %v away, want ~90s (the endpoint's own Retry-After)", wait)
	}
}

func TestQuotaNow_DefaultBackoffWhenNoRetryAfter(t *testing.T) {
	useTempQuotaCache(t)
	installFetch(t, func() ([]windowUsage, error) { return nil, errors.New("transport — no route to host") })

	if _, err := quotaNow(); err == nil {
		t.Fatal("expected an error")
	}
	rec := readRecord(t)
	wait := time.Until(rec.RetryAt)
	if wait < quotaFailureBackoff-time.Minute || wait > quotaFailureBackoff {
		t.Errorf("RetryAt is %v away, want ~%v", wait, quotaFailureBackoff)
	}
}

func TestQuotaNow_RepeatedRefusalRenewsTheBackoff(t *testing.T) {
	// The backoff elapsed, the retry was refused again. If the second refusal
	// did not push RetryAt forward, the machine would be back to asking on every
	// call — the amplifier, restored one backoff after it was removed.
	useTempQuotaCache(t)
	seedRecord(t, quotaRecord{
		Usage:   usageAt(0.91),
		ReadAt:  time.Now().Add(-10 * time.Minute),
		Failure: "rejected — HTTP 429",
		RetryAt: time.Now().Add(-time.Second), // the first backoff just elapsed
	})
	s := installFetch(t, func() ([]windowUsage, error) {
		return nil, &quotaRefusal{msg: "rejected — HTTP 503"}
	})

	r, err := quotaNow()
	if err != nil {
		t.Fatalf("a second refusal over a good reading must not error: %v", err)
	}
	if got := firstUsed(t, r); got != 0.91 {
		t.Errorf("used = %v, want the preserved 0.91", got)
	}
	if !strings.Contains(r.Failure, "503") {
		t.Errorf("failure = %q, want the refusal just earned", r.Failure)
	}
	rec := readRecord(t)
	if wait := time.Until(rec.RetryAt); wait < quotaFailureBackoff-time.Minute || wait > quotaFailureBackoff {
		t.Errorf("RetryAt is %v away, want a renewed ~%v", wait, quotaFailureBackoff)
	}
	if s.count() != 1 {
		t.Fatalf("fetches = %d, want 1", s.count())
	}
	if _, err := quotaNow(); err != nil {
		t.Fatalf("quotaNow inside the renewed backoff: %v", err)
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d, want the renewed backoff to hold at 1", s.count())
	}
}

func TestQuotaNow_AncientReadingIsDropped(t *testing.T) {
	useTempQuotaCache(t)
	seedRecord(t, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-quotaStaleBound - time.Minute)})
	installFetch(t, refuseFetch(0))

	_, err := quotaNow()
	if err == nil {
		t.Fatal("a reading past the outer bound is not a reading — expected an error")
	}
}

func TestQuotaNow_FutureDatedReadingIsAMiss(t *testing.T) {
	// A clock that jumped backwards must not pin the cache forever.
	useTempQuotaCache(t)
	seedRecord(t, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(2 * time.Hour)})
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

func TestQuotaNow_FirstRunCreatesTheCacheDirectory(t *testing.T) {
	// Flow has no machine-wide state directory today, so on every machine the
	// first read is this one: the directory does not exist yet. It has to be
	// created, or the reading is taken and thrown away and the second process
	// pays for it again.
	dir := filepath.Join(t.TempDir(), "flow")
	prev := quotaCacheDir
	quotaCacheDir = func() (string, bool) { return dir, true }
	t.Cleanup(func() { quotaCacheDir = prev })
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })

	if _, err := quotaNow(); err != nil {
		t.Fatalf("quotaNow: %v", err)
	}
	if rec := readRecord(t); len(rec.Usage) == 0 || rec.Usage[0].Used != 0.42 {
		t.Errorf("record = %+v, want the first reading cached", rec)
	}
	if _, err := quotaNow(); err != nil {
		t.Fatalf("quotaNow: %v", err)
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d, want 1 — the second read is served from what the first stored", s.count())
	}
}

// ---------------------------------------------------------------------------
// Degrading to today's behaviour. Nothing about the cache may fail a run.
// ---------------------------------------------------------------------------

func TestQuotaNow_TornFileReadsAsAMiss(t *testing.T) {
	useTempQuotaCache(t)
	if err := os.WriteFile(recordPath(t), []byte(`{"usage":[{"lab`), 0o600); err != nil {
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
	if rec := readRecord(t); len(rec.Usage) == 0 || rec.Usage[0].Used != 0.55 {
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
	useTempQuotaCache(t)
	seedRecord(t, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-10 * time.Minute)})
	if err := os.WriteFile(lockPath(t), nil, 0o600); err != nil {
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
	useTempQuotaCache(t)
	if err := os.WriteFile(lockPath(t), nil, 0o600); err != nil {
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
	useTempQuotaCache(t)
	lock := lockPath(t)
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

func TestQuotaNow_ConcurrentReadersMakeOneRequest(t *testing.T) {
	// Run under -race. Every goroutine gets a coherent reading, and the group
	// makes exactly one request between them: the winner asks, the losers serve
	// the stale reading, and a loser that reaches the slot after the winner has
	// released it finds the answer already there rather than asking again.
	useTempQuotaCache(t)
	seedRecord(t, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-10 * time.Minute)})
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
	if c := s.count(); c != 1 {
		t.Errorf("fetches = %d over %d concurrent readers, want 1", c, n)
	}
}

func TestRefreshQuota_ServesASiblingsReadingRatherThanAsking(t *testing.T) {
	// The record this process loaded was stale, but by the time it won the
	// single-flight slot a sibling had already refreshed. Asking again is the
	// duplicate request single-flight exists to save.
	useTempQuotaCache(t)
	seedRecord(t, quotaRecord{Usage: usageAt(0.55), ReadAt: time.Now()})
	stale := &quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-10 * time.Minute)}
	s := installFetch(t, func() ([]windowUsage, error) {
		t.Error("a sibling refreshed while this process waited for the slot — it must not ask")
		return nil, nil
	})

	rec := refreshQuota(recordPath(t), stale)
	if len(rec.Usage) == 0 || rec.Usage[0].Used != 0.55 {
		t.Errorf("record = %+v, want the sibling's 0.55", rec)
	}
	if s.count() != 0 {
		t.Errorf("fetches = %d, want 0", s.count())
	}
}

func TestRefreshQuota_HonoursARefusalRecordedWhileItWaited(t *testing.T) {
	// The backoff is shared, or it is only shared with whoever reads late
	// enough: a refusal recorded between this process loading the record and
	// winning the slot binds it too.
	useTempQuotaCache(t)
	seedRecord(t, quotaRecord{
		Usage:   usageAt(0.42),
		ReadAt:  time.Now().Add(-10 * time.Minute),
		Failure: "rejected — HTTP 429",
		RetryAt: time.Now().Add(2 * time.Minute),
	})
	stale := &quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-10 * time.Minute)}
	s := installFetch(t, func() ([]windowUsage, error) {
		t.Error("a refusal recorded while this process waited for the slot binds it too")
		return nil, nil
	})

	rec := refreshQuota(recordPath(t), stale)
	if !strings.Contains(rec.Failure, "429") {
		t.Errorf("record = %+v, want the sibling's refusal", rec)
	}
	if s.count() != 0 {
		t.Errorf("fetches = %d, want 0", s.count())
	}
}

func TestQuotaNow_FailedRefreshKeepsAReadingThatLandedMidRequest(t *testing.T) {
	// Two refreshes overlapped — a lock broken at its TTL is enough — and this
	// one lost: a sibling's reading landed while this process was asking, and
	// this process's own request was refused. The refusal is recorded AGAINST
	// the sibling's numbers. Writing the pre-request view back over them would
	// take the whole machine unpaced for a refresh interval, which is the
	// failure the cache exists to prevent.
	useTempQuotaCache(t)
	seedRecord(t, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-10 * time.Minute)})
	installFetch(t, func() ([]windowUsage, error) {
		seedRecord(t, quotaRecord{Usage: usageAt(0.77), ReadAt: time.Now()})
		return nil, &quotaRefusal{msg: "rejected — HTTP 429"}
	})

	r, err := quotaNow()
	if err != nil {
		t.Fatalf("a failed refresh over a good reading must not error: %v", err)
	}
	if got := firstUsed(t, r); got != 0.77 {
		t.Errorf("used = %v, want the sibling's 0.77 — a failure may not drop a reading", got)
	}
	rec := readRecord(t)
	if len(rec.Usage) == 0 || rec.Usage[0].Used != 0.77 {
		t.Errorf("record = %+v, want the sibling's reading preserved", rec)
	}
	if rec.Failure == "" || rec.RetryAt.IsZero() {
		t.Errorf("record = %+v, want the refusal recorded against it", rec)
	}
}

func TestQuotaNow_LeavesNoStrayFiles(t *testing.T) {
	dir := useTempQuotaCache(t)
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })
	if _, err := quotaNow(); err != nil {
		t.Fatalf("quotaNow: %v", err)
	}
	// Force a second, failing refresh past the interval.
	seedRecord(t, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-10 * time.Minute)})
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
		if e.Name() != filepath.Base(recordPath(t)) {
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

func TestQuotaCache_TestsNeverUseTheRealLocation(t *testing.T) {
	// The doctrine App.Quota's comment carries, asserted rather than trusted.
	// TestMain redirects the cache for the whole package; if that redirect is
	// ever dropped, every test in cli starts reading and writing the operator's
	// real record — pacing the next real run against numbers a test made up —
	// and nothing else here would notice.
	operators, ok := userQuotaCacheDir()
	if !ok {
		t.Skip("machine has no user cache directory")
	}
	dir, ok := quotaCacheDir()
	if !ok {
		t.Fatal("the cli tests must have a cache directory of their own")
	}
	if dir == operators {
		t.Fatalf("the cli tests are pointed at the real quota cache %q", dir)
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
	path := filepath.Join(dir, quotaCacheFilePrefix+"acct"+quotaCacheFileExt)
	storeQuotaRecord(path, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now()})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("nothing should have been written; stat err = %v", err)
	}
}

func TestQuotaNow_UnstorableRecordStillReads(t *testing.T) {
	// Something else owns the record's name — here a directory. The store
	// cannot land, and the run must neither fail nor leave a temp file behind.
	dir := useTempQuotaCache(t)
	if err := os.MkdirAll(filepath.Join(recordPath(t), "occupied"), 0o700); err != nil {
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
		if e.Name() != filepath.Base(recordPath(t)) {
			t.Errorf("stray file left behind by the failed store: %q", e.Name())
		}
	}
}

func TestStoreQuotaRecord_Is0600(t *testing.T) {
	useTempQuotaCache(t)
	path := recordPath(t)
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
// Keyed by account. One OS user may drive two of them.
// ---------------------------------------------------------------------------

func TestQuotaNow_AccountBDoesNotInheritAccountAsHeadroom(t *testing.T) {
	// The defect, in the direction that matters: A is at 5%, B is at 91%. An
	// unkeyed record hands B the 5% and B runs unpaced — which is the failure
	// the cache exists to close, reached through the cache itself.
	useTempQuotaCache(t)
	installCredential(t, "token-account-A")
	seedRecord(t, quotaRecord{Usage: usageAt(0.05), ReadAt: time.Now()})

	installCredential(t, "token-account-B")
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.91), nil })

	r, err := quotaNow()
	if err != nil {
		t.Fatalf("quotaNow: %v", err)
	}
	if got := firstUsed(t, r); got != 0.91 {
		t.Errorf("used = %v, want B's own 0.91 — a reading of A's account is not a reading of B's", got)
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d, want 1 — another account's record is a miss", s.count())
	}
}

func TestQuotaNow_AccountAsRecordSurvivesAccountBsRun(t *testing.T) {
	// The other direction, and the reason keying is a separate FILE rather than
	// a check on one: B's run may not cost A its reading. If it did, the two
	// accounts would evict each other and a machine driving both would fetch on
	// every call — the amplifier, restored by the fix for the amplifier.
	useTempQuotaCache(t)
	installCredential(t, "token-account-A")
	seedRecord(t, quotaRecord{Usage: usageAt(0.05), ReadAt: time.Now()})
	aPath := recordPath(t)

	installCredential(t, "token-account-B")
	if bPath := recordPath(t); bPath == aPath {
		t.Fatalf("both accounts resolved to %q — two accounts must not share a record", bPath)
	}
	installFetch(t, func() ([]windowUsage, error) { return usageAt(0.91), nil })
	if _, err := quotaNow(); err != nil {
		t.Fatalf("quotaNow for B: %v", err)
	}

	installCredential(t, "token-account-A")
	s := installFetch(t, func() ([]windowUsage, error) {
		t.Error("A's own reading is still fresh — B's run must not have cost it")
		return nil, nil
	})
	r, err := quotaNow()
	if err != nil {
		t.Fatalf("quotaNow for A: %v", err)
	}
	if got := firstUsed(t, r); got != 0.05 {
		t.Errorf("used = %v, want A's own 0.05", got)
	}
	if s.count() != 0 {
		t.Errorf("fetches = %d, want 0", s.count())
	}
}

func TestQuotaNow_AHitCostsOneCredentialDiscovery(t *testing.T) {
	// What keying may NOT cost. Discovery forks `security` on a Keychain
	// machine, and paying it per call is the cost the cache was built to remove:
	// a resolve's reads must between them discover the credential once.
	useTempQuotaCache(t)
	c := installCredential(t, "token-account-A")
	installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })

	for range maxResolveSteps {
		if _, err := quotaNow(); err != nil {
			t.Fatalf("quotaNow: %v", err)
		}
	}
	if c.count() != 1 {
		t.Errorf("credential discoveries = %d over %d reads, want 1", c.count(), maxResolveSteps)
	}
}

func TestQuotaNow_BackoffDoesNotCrossAccounts(t *testing.T) {
	// A's credential has expired and the endpoint refuses it. Backing B off on
	// A's 401 would take pacing off a run whose credential is perfectly good,
	// for as long as the refusal asked for.
	useTempQuotaCache(t)
	installCredential(t, "token-account-A")
	seedRecord(t, quotaRecord{
		Failure: "rejected — HTTP 401 (credentials may be expired; re-run claude to refresh)",
		RetryAt: time.Now().Add(30 * time.Minute),
	})

	installCredential(t, "token-account-B")
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })

	r, err := quotaNow()
	if err != nil {
		t.Fatalf("B must not be held by a refusal earned with A's credential: %v", err)
	}
	if got := firstUsed(t, r); got != 0.42 {
		t.Errorf("used = %v, want B's own 0.42", got)
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d, want 1", s.count())
	}
}

func TestQuotaNow_LockDoesNotCrossAccounts(t *testing.T) {
	// A is mid-refresh. A shared slot would make B wait for an answer about an
	// account that is not B's — and with nothing of its own on disk, B would
	// serve nothing and run unpaced.
	useTempQuotaCache(t)
	installCredential(t, "token-account-A")
	if err := os.WriteFile(lockPath(t), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	installCredential(t, "token-account-B")
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })

	r, err := quotaNow()
	if err != nil {
		t.Fatalf("B must not wait on a refresh of A's account: %v", err)
	}
	if got := firstUsed(t, r); got != 0.42 {
		t.Errorf("used = %v, want B's own 0.42", got)
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d, want 1", s.count())
	}
}

func TestQuotaNow_UnreadableCredentialStillBacksOff(t *testing.T) {
	// A machine whose credential cannot be read cannot fetch either. It gets a
	// record all the same, under one shared name: without one it could record no
	// backoff, and would ask — and fail — once per step. The discovery itself is
	// made once too; retrying it is the subprocess per call the cache exists to
	// avoid, on the machine where every one of them also fails.
	useTempQuotaCache(t)
	c := installCredentialFailure(t, "no Claude credentials found")
	s := installFetch(t, func() ([]windowUsage, error) {
		return nil, errors.New("no Claude credentials found")
	})

	for range maxResolveSteps {
		if _, err := quotaNow(); err == nil {
			t.Fatal("a machine with no credential has nothing to serve — expected an error")
		}
	}
	want := quotaCacheFilePrefix + quotaKeyUnknown + quotaCacheFileExt
	if got := filepath.Base(recordPath(t)); got != want {
		t.Errorf("record = %q, want %q", got, want)
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d over %d reads, want 1 — the backoff binds this machine too", s.count(), maxResolveSteps)
	}
	if c.count() != 1 {
		t.Errorf("credential discoveries = %d, want 1 — the failure is memoised with the success", c.count())
	}
}

func TestQuotaAccountKey_NamesTheCredentialWithoutCarryingIt(t *testing.T) {
	// The key is a digest, so the file name distinguishes two accounts without
	// disclosing either: a cache directory is not a place to leave a bearer
	// token, and equality is the only thing a key is asked for.
	useTempQuotaCache(t)
	// A distinctive marker rather than a credential-shaped literal: what the
	// test needs is a string it can look for in the path, and a file carrying
	// a token's own shape is the thing this test says not to leave lying around.
	const token = "quota-test-account-token-MARKER"
	installCredential(t, token)

	path := recordPath(t)
	if strings.Contains(path, token) || strings.Contains(path, "MARKER") {
		t.Errorf("record path %q carries the credential", path)
	}
	key := quotaAccountKey()
	if len(key) != 16 {
		t.Errorf("key = %q, want 16 hex digits", key)
	}
	if strings.Trim(key, "0123456789abcdef") != "" {
		t.Errorf("key = %q, want hex", key)
	}
	if again := quotaAccountKey(); again != key {
		t.Errorf("key moved within one process: %q then %q", key, again)
	}

	installCredential(t, token+"-other")
	if other := quotaAccountKey(); other == key {
		t.Errorf("a different credential produced the same key %q", other)
	}
}

func TestStoreQuotaRecord_PrunesRetiredRecords(t *testing.T) {
	// Keying the record by credential is what makes records accumulate: a token
	// rotation leaves the file it was reading behind, and so does every account
	// the machine has stopped driving. A record past quotaRecordKeepFor holds
	// neither a servable reading nor a live backoff, so nothing is taken from
	// anyone by removing it.
	dir := useTempQuotaCache(t)
	installCredential(t, "token-account-A")

	write := func(name string, age time.Duration) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		return path
	}
	old := quotaRecordKeepFor + time.Minute
	orphan := write("quota-0123456789abcdef"+quotaCacheFileExt, old)
	legacy := write("quota"+quotaCacheFileExt, old) // what the cache's first version left
	sibling := write("quota-fedcba9876543210"+quotaCacheFileExt, time.Minute)
	// The locks orphan with the records, and nothing else ever clears them: the
	// TTL breaker fires only when a process contends for that exact name, and a
	// retired key's name is one nothing asks for again.
	orphanLock := write("quota-0123456789abcdef"+quotaCacheFileExt+quotaLockSuffix, old)
	legacyLock := write("quota"+quotaCacheFileExt+quotaLockSuffix, old)
	// A lock in use is younger than quotaRefreshLockTTL, two orders of magnitude
	// below the bound; a temp file belongs to a store in flight, and removing one
	// under the process writing it is how a store lands on nothing.
	liveLock := write("quota-fedcba9876543210"+quotaCacheFileExt+quotaLockSuffix, time.Second)
	tmp := write(".quota-inflight", old)

	path := recordPath(t)
	storeQuotaRecord(path, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now()})

	for _, gone := range []string{orphan, legacy, orphanLock, legacyLock} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should have been pruned; stat err = %v", filepath.Base(gone), err)
		}
	}
	for _, kept := range []string{sibling, liveLock, tmp, path} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s should have been kept: %v", filepath.Base(kept), err)
		}
	}
}

func TestPruneQuotaRecords_KeepsARecordStillHoldingABackoff(t *testing.T) {
	// The bound is the retry cap rather than quotaStaleBound: at 40 minutes a
	// record's reading is already unservable, but the refusal recorded against
	// it can still be in force, and dropping it would put the machine straight
	// back on an endpoint that asked to be left alone.
	dir := useTempQuotaCache(t)
	path := filepath.Join(dir, "quota-0123456789abcdef"+quotaCacheFileExt)
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-40 * time.Minute)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}

	pruneQuotaRecords(dir, time.Now())

	if _, err := os.Stat(path); err != nil {
		t.Errorf("a record past the stale bound but inside the retry cap must be kept: %v", err)
	}
}

func TestPruneQuotaRecords_MissingDirectoryIsSilent(t *testing.T) {
	// Nothing on this path may fail a run, and a prune has even less business
	// doing so than a store: it runs after the reading is already safe.
	pruneQuotaRecords(filepath.Join(t.TempDir(), "nonexistent"), time.Now())
}

// ---------------------------------------------------------------------------
// The installed reader, and what the display sites print.
// ---------------------------------------------------------------------------

func TestCachedQuota_ServesTheCachedReading(t *testing.T) {
	useTempQuotaCache(t)
	seedRecord(t, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-1 * time.Minute)})
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

func TestRunWithArgs_InstallsTheCacheBackedReader(t *testing.T) {
	// The wiring, from the binary's entry point: with App.Quota nil, the reader
	// Run installs is the one that goes through the cache. Every read in the
	// run — the display sites and the pacing read in resolve's loop — goes
	// through it, so one fetch serves them all. A reader that went to the
	// network instead would make more calls, or none at all and print "quota
	// unreadable" for a machine with a perfectly good reading available.
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, _, errBuf := resolveTestApp(t, be)
	app.Quota = nil
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })

	if code := RunWithArgs(*app, []string{"resolve", "1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d, want 1 — the installed reader must read through the cache", s.count())
	}
	if out := errBuf.String(); strings.Contains(out, "quota unreadable") {
		t.Errorf("pacing was disabled on a run that had a reading; got:\n%s", out)
	}
}

func TestRunWithArgs_OneResolveMakesOneRequest(t *testing.T) {
	// The defect, end to end: one resolve made up to 52 requests — a display
	// print at startup, one per step, another on the terminal outcome. It makes
	// one now, and the display sites still print the figures they printed
	// before.
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	app, _, errBuf := resolveTestApp(t, be)
	app.Quota = nil
	s := installFetch(t, func() ([]windowUsage, error) { return usageAt(0.42), nil })

	if code := RunWithArgs(*app, []string{"resolve", "1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; err=%q", code, errBuf.String())
	}
	if s.count() != 1 {
		t.Errorf("fetches = %d over one whole resolve, want 1", s.count())
	}
	out := errBuf.String()
	if n := strings.Count(out, "42% used"); n < 2 {
		t.Errorf("want the quota block at startup and on the outcome (≥2); got %d in:\n%s", n, out)
	}
	if strings.Contains(out, "quota unreadable") {
		t.Errorf("pacing was disabled on a run that had a reading; got:\n%s", out)
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
		useTempQuotaCache(t)
		seedRecord(t, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-1 * time.Minute)})
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
		useTempQuotaCache(t)
		seedRecord(t, quotaRecord{Usage: usageAt(0.42), ReadAt: time.Now().Add(-10 * time.Minute)})
		installFetch(t, refuseFetch(0))
		out := captureReportQuota(t)
		if !strings.Contains(out, "42% used") {
			t.Errorf("stale figures are still the best available — print them; got %q", out)
		}
		if !strings.Contains(out, "refresh failing: rejected — HTTP 429") {
			t.Errorf("the diagnostic operators read today must survive; got %q", out)
		}
		// The age is the READING's, not the moment it was printed: "0s old"
		// beside a ten-minute-old number is the silent staleness this line
		// exists to prevent.
		if !strings.Contains(out, "figures are 10m old") {
			t.Errorf("the age of the figures must be stated, and be theirs; got %q", out)
		}
	})
}
