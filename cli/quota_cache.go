package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The machine-wide quota cache.
//
// The subscription usage endpoint exists so a run can avoid exhausting the
// account, and an uncached read of it is how a busy machine exhausts the
// endpoint instead: one `resolve` asks up to 52 times (once per step, plus the
// display sites), and a machine carrying two dozen checkouts of the same
// account earns a 429 between them. The run that gets the 429 responds by
// switching pacing OFF — so the busiest machine at the busiest moment runs with
// no ceiling at all, spending headroom its paced siblings are sleeping on.
//
// The fix is not to make a 429 blocking. It is to stop reaching one: every tool
// on the machine reads and writes ONE record, refreshed on a timer rather than
// per call, and a refresh that fails leaves the last good reading in place for
// everyone rather than throwing it away. The 5h and 7d windows move over hours,
// so a reading minutes old is a perfectly good pacing input — paceDelay already
// interpolates against elapsed time rather than assuming its sample is
// instantaneous.
//
// Nothing here may ever fail a run. Every filesystem error degrades to the
// behaviour flow has today: fetch, and if that fails, say so once and proceed
// unpaced.
//
// The record is per credential, and the credential is in its NAME. One OS user
// may drive more than one agent account — switching CLAUDE_CONFIG_DIR, or
// re-authenticating between runs — and a single record shared between them
// serves whichever was read last: the run against an account at 91% reads the
// other's 5% and proceeds unpaced, which is the failure this file exists to
// close, reached by a different route. Naming the file after a digest of the
// credential makes a different account a different FILE — a miss by
// construction, with no record either account can overwrite with the other's
// numbers, and no mismatch to detect on a hit. The lock and the backoff are
// keyed with it for the same reason: A's in-flight refresh must not suppress
// B's, and a 401 for an expired A credential must not silence B.
//
// The digest is read ONCE per process (quotaAccountKey), because reading it is
// the cost the cache exists to remove: discoverOAuthToken forks `security` on a
// Keychain machine, and a resolve reads quota once per step.

const (
	// quotaRefreshInterval is how old a reading may be before the next caller
	// refreshes it. The floor on how often the machine touches the endpoint:
	// with it, 25 checkouts × 50 steps make at most one request per 5 minutes
	// between them, against the ~1250 they make without it.
	quotaRefreshInterval = 5 * time.Minute

	// quotaStaleBound is the outer bound past which a reading is not served at
	// all. Inside it a stale reading is the best estimate available and pacing
	// stays on; past it the account state is genuinely unknown, and pretending
	// otherwise would pace against a number no longer about this window.
	quotaStaleBound = 30 * time.Minute

	// quotaFailureBackoff is how long a refusal that named no Retry-After holds
	// the machine off the endpoint. Retrying every iteration is the amplifier
	// this file exists to remove: a process already being rate-limited must not
	// keep asking.
	quotaFailureBackoff = 5 * time.Minute

	// quotaRetryAfterCap bounds what a Retry-After header can impose. The
	// header is honoured because the endpoint knows better than we do, but a
	// header saying "a week" must not disable pacing for a week — past this
	// cap the reading ages out on its own terms instead.
	quotaRetryAfterCap = 1 * time.Hour

	// quotaRefreshLockTTL is how long the single-flight lock is believed. Long
	// enough to cover a refresh (fetchUsage's client times out at 10s), short
	// enough that a process killed mid-refresh cannot wedge the machine's
	// refreshing for good.
	quotaRefreshLockTTL = 30 * time.Second

	// quotaRecordKeepFor is how long a record no process is writing any more is
	// kept. Keying the record by credential means a rotated token orphans the
	// file it was reading, so something has to retire them. The bound is the
	// retry cap rather than quotaStaleBound: past 30 minutes a record's READING
	// is unservable, but the refusal recorded against it can still be in force,
	// and deleting it would put the machine back on an endpoint that asked to be
	// left alone.
	quotaRecordKeepFor = quotaRetryAfterCap

	// The record is named after the credential it was read with:
	// quota-<key>.json. quotaRecordGlob matches those and the unkeyed quota.json
	// left behind by the cache's first version, and neither the .quota-* temp
	// files nor the .lock beside each record.
	quotaCacheFilePrefix = "quota-"
	quotaCacheFileExt    = ".json"
	quotaRecordGlob      = "quota*" + quotaCacheFileExt

	// quotaKeyUnknown names the record for a machine whose credential cannot be
	// read at all. Such a machine cannot fetch either, so its record holds a
	// failure and a backoff — which is the whole reason it gets one: without a
	// name it could not back off, and would ask once per step.
	quotaKeyUnknown = "none"

	quotaLockSuffix = ".lock"
)

// quotaCacheDir, quotaFetch and quotaCredential are the three seams this file
// is tested through.
//
// They are package vars — the fitnessWaitInterval pattern — and deliberately
// NOT environment variables: an environment variable is never an input
// (docs/org/cli-guide.md § 2). Neither the location nor the credential seam is
// a convenience. A test that read or wrote the developer's real cache would
// pace the next real run against numbers a test made up, and a test that
// discovered the real credential would read the developer's account — and fork
// `security` to do it. cli's TestMain redirects both for the whole package so
// no test in it can.
var (
	quotaCacheDir   = userQuotaCacheDir
	quotaFetch      = readQuota
	quotaCredential = discoverOAuthToken
)

// quotaAccountOnce and quotaAccountKeyed memoise the process's account key.
//
// The FAILURE is memoised with the success. Re-discovering after one would
// restore exactly the per-call subprocess the cache exists to avoid, on the one
// machine where every one of those calls also fails.
var (
	quotaAccountOnce  sync.Once
	quotaAccountKeyed string
)

// quotaAccountKey names the account this process reads quota for: sixteen hex
// digits of a SHA-256 of the credential, the truncated-digest idiom
// fingerprintArena uses, and quotaKeyUnknown when no credential can be read.
//
// Of the CREDENTIAL, not of an account name: it is readable without decoding
// anything or pinning any part of the credential schema in flow's source, it
// identifies nobody, and equality — the only operation a cache key needs — is
// exactly what a digest supports. A token rotation makes a new key, so the
// account's next reading lands in a new file; that costs one fetch, and the
// orphan is pruned within the hour.
func quotaAccountKey() string {
	quotaAccountOnce.Do(func() {
		token, reason := quotaCredential()
		if reason != "" || token == "" {
			quotaAccountKeyed = quotaKeyUnknown
			return
		}
		sum := sha256.Sum256([]byte(token))
		quotaAccountKeyed = hex.EncodeToString(sum[:])[:16]
	})
	return quotaAccountKeyed
}

// quotaRecord is the shared record on disk: the last reading that succeeded,
// when it was taken, and the refusal still in force. A failure NEVER clears the
// reading — that is what keeps the paced/unpaced asymmetry from opening — and a
// success always clears the failure.
type quotaRecord struct {
	Usage   []windowUsage `json:"usage,omitempty"`
	ReadAt  time.Time     `json:"read_at,omitempty"`
	Failure string        `json:"failure,omitempty"`
	RetryAt time.Time     `json:"retry_at,omitempty"`
}

// quotaReading is one answer to "what is the account using right now": the
// windows to pace and display against, when they were actually read, and the
// refresh failure in force if there is one. Failure is non-empty exactly when
// the numbers are being served past their refresh interval because the endpoint
// is refusing — the caller that DISPLAYS them has to say so, or the change
// would silently print stale figures as current and swallow the diagnostic
// operators read today.
type quotaReading struct {
	Usage   []windowUsage
	ReadAt  time.Time
	Failure string
}

// cachedQuota is the reader Run installs into App.Quota. It has readQuota's
// signature and readQuota's contract — windows, or an error that is
// informational and never blocks — and differs only in usually not making a
// request. App.Quota == nil still means no pacing; the cache lives behind the
// installed reader, not in the field's contract.
func cachedQuota() ([]windowUsage, error) {
	r, err := quotaNow()
	if err != nil {
		return nil, err
	}
	return r.Usage, nil
}

// quotaNow is the whole policy. A fresh record is served from one file read
// with no request at all; a stale one is refreshed unless a failure backoff is
// in force or another process is already refreshing, in which case the previous
// reading is served rather than adding to the pressure; a refresh that fails
// preserves what was there and records the refusal so every other process on
// the machine backs off with this one.
//
// The error is returned only when there is nothing to serve at all — no reading
// inside the outer bound and a refresh that did not succeed. That is the cold
// case, and its handling is unchanged: warn once, proceed unpaced.
func quotaNow() (quotaReading, error) {
	path, ok := quotaCachePath()
	if !ok {
		// No usable cache location on this machine. Every call fetches, which
		// is exactly what flow does today — the cache is an optimisation, and
		// its absence may not change an answer.
		usage, err := quotaFetch()
		if err != nil {
			return quotaReading{}, err
		}
		return quotaReading{Usage: usage, ReadAt: time.Now()}, nil
	}

	rec := loadQuotaRecord(path)
	if r, ok := servableReading(rec, time.Now(), quotaRefreshInterval); ok {
		return r, nil
	}

	// Stale, or nothing on disk. Refresh — unless the endpoint has already said
	// no and the machine is inside the backoff it asked for.
	if !inQuotaBackoff(rec, time.Now()) {
		rec = refreshQuota(path, rec)
	}

	// Whatever the refresh left: a new reading, a sibling's, or the previous one
	// with a refusal recorded against it. A reading inside the outer bound is
	// still the best estimate available, and serving it is what keeps this
	// process paced alongside the siblings that read the same file.
	//
	// The clock is read again here rather than carried down from the top: a
	// refresh takes as long as fetchUsage's timeout allows, and both the age of
	// the reading and the reading itself can move while it runs. Reusing a
	// timestamp taken before the request would age a reading that arrived
	// DURING it into the future and discard it.
	if r, ok := servableReading(rec, time.Now(), quotaStaleBound); ok {
		return r, nil
	}
	return quotaReading{}, quotaUnknown(rec)
}

// refreshQuota asks the endpoint at most once, under the machine-wide
// single-flight lock, stores what it learns for every other tool on the
// machine, and returns the record to serve from.
//
// It asks for nothing at all when it does not have to: the lock is held by
// somebody already asking, or the record turned out — between this process
// loading it and winning the slot — to have been refreshed by a sibling, or to
// have come under a refusal every process must back off on. A refusal it does
// earn is recorded against whatever reading the record already had, so a
// failure never costs the machine its last known-good numbers.
func refreshQuota(path string, prev *quotaRecord) *quotaRecord {
	release, held := acquireRefreshLock(path, time.Now())
	if !held {
		return prev
	}
	defer release()

	// Re-read under the lock. Winning the slot is not instantaneous, and the
	// record this process decided on was read before it. A sibling that
	// refreshed in that window has already answered the question — asking again
	// is precisely the duplicate request single-flight exists to save — and a
	// sibling that recorded a refusal in it binds this process too, or the
	// shared backoff is only shared with whoever reads late enough.
	if cur := loadQuotaRecord(path); cur != nil {
		prev = cur
		now := time.Now()
		if _, fresh := servableReading(cur, now, quotaRefreshInterval); fresh {
			return cur
		}
		if inQuotaBackoff(cur, now) {
			return cur
		}
	}

	usage, err := quotaFetch()
	if err != nil {
		next := quotaFailureRecord(path, prev, err)
		storeQuotaRecord(path, *next)
		return next
	}
	next := &quotaRecord{Usage: usage, ReadAt: time.Now()}
	storeQuotaRecord(path, *next)
	return next
}

// quotaFailureRecord records a failed refresh against the freshest reading the
// machine has: the one on disk when a sibling landed a newer one WHILE this
// process was asking, and otherwise the one this process started from.
//
// The re-read is not belt-and-braces. A failure never clearing the reading is
// what keeps the paced/unpaced asymmetry from opening, and the reading it must
// not clear can be one that arrived mid-request — a lock broken at its TTL, or
// a machine with nowhere to put one, is enough to overlap two refreshes. Writing
// this process's pre-request view back over a newer one would take the whole
// machine unpaced for a refresh interval, which is the failure this file exists
// to prevent.
func quotaFailureRecord(path string, prev *quotaRecord, err error) *quotaRecord {
	base := prev
	if cur := loadQuotaRecord(path); cur != nil && (base == nil || cur.ReadAt.After(base.ReadAt)) {
		base = cur
	}
	return withQuotaFailure(base, err, time.Now())
}

// servableReading returns the record's reading when it exists and is younger
// than maxAge.
//
// A reading dated in the FUTURE is treated as a miss rather than as maximally
// fresh: a machine whose clock jumped backwards would otherwise pin the cache
// until the clock caught up, which is the one way a cache built to be
// self-healing could stop refreshing indefinitely.
func servableReading(rec *quotaRecord, now time.Time, maxAge time.Duration) (quotaReading, bool) {
	if rec == nil || len(rec.Usage) == 0 || rec.ReadAt.IsZero() {
		return quotaReading{}, false
	}
	age := now.Sub(rec.ReadAt)
	if age < 0 || age >= maxAge {
		return quotaReading{}, false
	}
	return quotaReading{Usage: rec.Usage, ReadAt: rec.ReadAt, Failure: rec.Failure}, true
}

// inQuotaBackoff reports whether the endpoint's last answer still forbids
// asking again.
func inQuotaBackoff(rec *quotaRecord, now time.Time) bool {
	return rec != nil && !rec.RetryAt.IsZero() && now.Before(rec.RetryAt)
}

// withQuotaFailure records a failed refresh against the record, preserving
// whatever reading was already there. Returns the record to store.
func withQuotaFailure(rec *quotaRecord, err error, now time.Time) *quotaRecord {
	next := quotaRecord{}
	if rec != nil {
		next = *rec
	}
	next.Failure = err.Error()
	next.RetryAt = now.Add(quotaBackoffFor(err))
	return &next
}

// quotaBackoffFor is how long a failed refresh holds the machine off the
// endpoint: what the refusal asked for when it carried a Retry-After, and the
// default otherwise. A transport error carries nothing, so it takes the
// default too — a host that cannot be reached is not one to retry 50 times.
func quotaBackoffFor(err error) time.Duration {
	var refusal *quotaRefusal
	if errors.As(err, &refusal) && refusal.retryAfter > 0 {
		return refusal.retryAfter
	}
	return quotaFailureBackoff
}

// quotaUnknown is the error for "nothing to serve": the recorded refusal when
// there is one — so a process inside the shared backoff reports the same reason
// as the process that earned it, rather than a reason of its own — and
// otherwise a note that someone else is asking.
func quotaUnknown(rec *quotaRecord) error {
	if rec != nil && rec.Failure != "" {
		return errors.New(rec.Failure)
	}
	return fmt.Errorf("no reading available — another process is refreshing")
}

// userQuotaCacheDir is where the shared record lives: flow's directory under
// the OS user cache directory ($XDG_CACHE_HOME or ~/.cache on Linux,
// ~/Library/Caches on macOS). User-scoped rather than per-clone on purpose —
// the whole point is that 25 checkouts of one account share one reading.
//
// Reports false when the machine has no such directory, which is a machine
// without a cache, not an error.
func userQuotaCacheDir() (string, bool) {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return "", false
	}
	return filepath.Join(dir, "flow"), true
}

// quotaCachePath resolves this account's record path, creating the directory.
// Reports false when there is nowhere to cache — the caller then behaves as
// flow does with no cache at all.
//
// The account key lives in the file name and nowhere else: there is no Account
// field on quotaRecord, so no second copy of it to keep in sync, and nothing
// downstream of here — quotaNow, refreshQuota, quotaFailureRecord,
// servableReading — has to know an account exists. They take the path.
func quotaCachePath() (string, bool) {
	dir, ok := quotaCacheDir()
	if !ok {
		return "", false
	}
	// 0o700: the record says what an account is using, and nothing else on the
	// machine has a reason to read it.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false
	}
	return filepath.Join(dir, quotaCacheFilePrefix+quotaAccountKey()+quotaCacheFileExt), true
}

// loadQuotaRecord reads the record. Absent, torn, or unparseable all read as a
// miss — nil, no error — so a corrupt cache costs one refresh rather than a
// run.
func loadQuotaRecord(path string) *quotaRecord {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rec quotaRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil
	}
	return &rec
}

// storeQuotaRecord writes the record via temp file + rename, so a concurrent
// reader sees either the whole previous record or the whole new one and never
// half of either. Every failure is silent: not being able to cache a reading is
// not a reason to fail the run that took it.
func storeQuotaRecord(path string, rec quotaRecord) {
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	// CreateTemp makes the file 0o600, which the rename carries over.
	tmp, err := os.CreateTemp(dir, ".quota-*")
	if err != nil {
		return
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return
	}
	pruneQuotaRecords(dir, time.Now())
}

// pruneQuotaRecords removes the records nothing is reading any more.
//
// Naming a record after the credential it was read with is what introduces
// growth: a token rotation makes a new name and orphans the old file, and so
// does every account the machine has stopped driving. A record older than
// quotaRecordKeepFor can hold neither a servable reading nor a live backoff, so
// removing it takes nothing away from anyone; the same sweep retires the
// unkeyed quota.json the cache's first version left behind.
//
// Silent, like everything else on this path: a prune that cannot run is not a
// reason to fail the run whose reading was just stored. It runs after a store
// rather than on a timer of its own because a store is exactly when a new name
// can have appeared.
func pruneQuotaRecords(dir string, now time.Time) {
	matches, err := filepath.Glob(filepath.Join(dir, quotaRecordGlob))
	if err != nil {
		return
	}
	for _, path := range matches {
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			continue
		}
		if now.Sub(st.ModTime()) > quotaRecordKeepFor {
			os.Remove(path)
		}
	}
}

// acquireRefreshLock takes the machine-wide refresh slot, so that of several
// processes finding the record stale at the same moment one asks the endpoint
// and the rest serve what they already have.
//
// Returns (release, true) when the caller should refresh, and (nil, false) when
// somebody else is already doing it. A lock that cannot be created for any
// reason OTHER than already existing — an unwritable cache directory — is not
// treated as held: single-flight is an optimisation, and one that could stop
// every process on the machine from ever refreshing would be worse than the
// duplication it saves.
//
// The slot is per RECORD — the lock sits beside the record it guards — because
// two accounts refreshing are not the same question. A shared lock would let
// A's in-flight refresh suppress B's, leaving B with nothing to serve and
// unpaced, which is the defect keying exists to close.
func acquireRefreshLock(path string, now time.Time) (func(), bool) {
	lock := path + quotaLockSuffix
	take := func() (func(), bool) {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(lock) }, true
		}
		if !errors.Is(err, fs.ErrExist) {
			return func() {}, true
		}
		return nil, false
	}
	if release, ok := take(); ok {
		return release, true
	}
	// Held. Break it once it is older than the TTL — a process killed mid-
	// refresh leaves the file behind, and a lock nothing can clear would
	// disable refreshing on this machine permanently.
	st, err := os.Stat(lock)
	if err != nil || now.Sub(st.ModTime()) <= quotaRefreshLockTTL {
		return nil, false
	}
	os.Remove(lock)
	// Re-take rather than assume: if another process broke the same stale lock
	// first, it is refreshing and this one serves what it has.
	return take()
}
