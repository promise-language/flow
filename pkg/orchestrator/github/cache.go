package github

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/promise-language/flow/pkg/machinecache"
)

// The machine-wide GitHub cache.
//
// Nothing behind this seam was cached, and every flow process on the machine
// paid for that separately: a `Load` re-fetches the state comment and re-pages
// the comment list, a listing asks each item's blockers one request at a time
// (#171), and a machine running several arenas repeats all of it per arena, per
// step. The requests are identical, the answers are identical, and the account
// they are billed to is one.
//
// So the policy is per METHOD and the asymmetry is the point. Repository
// metadata does not move within a run; a collaborator's role lands within the
// hour; a blocker's status is exactly the kind of fact that need not be fresh
// on every listing, which is what turns #171's N+1 from a listing question into
// a caching one. The issue read is different again: its LABELS ride in the same
// response and docs/github-schema.md § claim requires them re-read live, so it
// is revalidated rather than aged — a 304 costs no primary budget and returns
// the cached body, which serves "issue bodies between dispatches" without ever
// answering stale about a holder.
//
// What is NEVER cached is the state document and the comment lists that find
// it. Staleness there is not a wasted request, it is a lost write.
//
// The record is per REPOSITORY AND ACCOUNT, and both are in its NAME — the
// truncated-digest idiom cli/quota_cache.go's quotaAccountKey and
// fingerprintArena already use. A different token or a different repository is
// a different FILE: a miss by construction, with no record either can overwrite
// with the other's answers, and no mismatch to detect on a hit. A token that
// cannot see a private repository must never read an answer the operator's own
// token cached.
//
// Nothing here may ever fail a call. Every filesystem error degrades to the
// behaviour this package has without a cache — see pkg/machinecache.

const (
	// repoMetaTTL — GET /repos/{o}/{r}. Default branch, permissions, name:
	// nothing a run can change under itself.
	repoMetaTTL = 24 * time.Hour

	// collaboratorTTL — .../collaborators/{login}/permission. Role derivation's
	// input. A role change is a human act and lands within the hour; holding it
	// longer would let a revoked maintainer keep carry-through for a day.
	collaboratorTTL = 1 * time.Hour

	// blockerTTL — .../issues/{n}/dependencies/blocked_by. #171's N+1: a wide
	// listing pays it once per minute per MACHINE instead of once per item per
	// invocation. Short enough that an item unblocked by a landing is runnable
	// on the next listing but one.
	blockerTTL = 60 * time.Second

	// seamRecordKeepFor is how long a record nothing is writing any more is
	// kept. Keying by repository and account is what introduces growth: a token
	// rotation or a second checkout makes a new name and orphans the old file.
	// Two orders of magnitude above the state-document lock's TTL, so the sweep
	// cannot reach a live lock.
	seamRecordKeepFor = 24 * time.Hour

	seamCacheFilePrefix = "github-"
	seamCacheFileExt    = ".json"
	seamRecordGlob      = seamCacheFilePrefix + "*" + seamCacheFileExt
	seamLockGlob        = seamCacheFilePrefix + "*" + machinecache.LockSuffix
)

// cacheDir is the seam this file is tested through: a package var, the
// quotaCacheDir pattern, and deliberately NOT an environment variable — an
// environment variable is never an input (docs/org/cli-guide.md § 2).
//
// A test that wrote the developer's real cache would answer their next real run
// from numbers a test made up, and one that read it would take its result from
// whatever the developer's machine last did. main_test.go redirects it for the
// whole package so no test in it can.
var cacheDir = machinecache.Dir

// seamCache is one repository-and-account's shared record. The zero value is
// not usable; newSeamCache returns nil when the machine has nowhere to cache,
// and every method tolerates a nil receiver so callers need no second branch.
type seamCache struct {
	path string
	dir  string
	key  string
}

// newSeamCache resolves the record for this repository and credential. Returns
// nil when there is nowhere to cache, which is a machine without a cache rather
// than an error.
func newSeamCache(owner, repo, token string) *seamCache {
	dir, ok := cacheDir()
	if !ok {
		return nil
	}
	sum := sha256.Sum256([]byte(owner + "/" + repo + "\x00" + token))
	key := hex.EncodeToString(sum[:])[:16]
	return &seamCache{
		dir:  dir,
		key:  key,
		path: filepath.Join(dir, seamCacheFilePrefix+key+seamCacheFileExt),
	}
}

// seamRecord is what every flow process on the machine shares for one
// repository and account: the refusal in force, the URL-keyed answers, and the
// state-comment id per issue.
//
// One file rather than one per row, because the alternative is a directory that
// grows a file per URL and a sweep that has to understand them. A store is a
// read-modify-write and the loser of a race loses a cached ANSWER, which costs
// one request — the one thing that must not be lost that way is the refusal,
// and storeLimit merges rather than overwrites.
type seamRecord struct {
	Limit         *seamLimit         `json:"limit,omitempty"`
	Rows          map[string]seamRow `json:"rows,omitempty"`
	StateComments map[string]int64   `json:"state_comments,omitempty"`
}

// seamLimit is a rate-limit refusal one process earned, recorded so the rest of
// the machine does not go and earn its own. Sharing it is the whole point: a
// secondary limit is counted per ACCOUNT, so three arenas discovering it
// independently is three refusals for one condition.
type seamLimit struct {
	Until    time.Time `json:"until"`
	Primary  bool      `json:"primary"`
	Endpoint string    `json:"endpoint"`
}

// seamRow is one cached answer: the bytes, when they were stored, and the tag
// to revalidate them with.
type seamRow struct {
	StoredAt time.Time `json:"stored_at"`
	ETag     string    `json:"etag,omitempty"`
	Body     []byte    `json:"body,omitempty"`
}

// seamSweep retires the records — and the locks beside them — that keying
// orphans. Passed to every store, which is when a new name can have appeared.
var seamSweep = machinecache.Sweep{
	Globs:   []string{seamRecordGlob, seamLockGlob},
	KeepFor: seamRecordKeepFor,
}

func (c *seamCache) load() *seamRecord {
	if c == nil {
		return nil
	}
	return machinecache.Load[seamRecord](c.path)
}

// update applies mutate to the record on disk and stores the result. A torn or
// absent record starts empty, so a corrupt cache costs requests rather than a
// run.
func (c *seamCache) update(mutate func(*seamRecord)) {
	if c == nil {
		return
	}
	rec := c.load()
	if rec == nil {
		rec = &seamRecord{}
	}
	mutate(rec)
	machinecache.Store(c.path, *rec, seamSweep)
}

// limitInForce reports the refusal this machine is still under, if any.
func (c *seamCache) limitInForce(now time.Time) *seamLimit {
	rec := c.load()
	if rec == nil || rec.Limit == nil || !now.Before(rec.Limit.Until) {
		return nil
	}
	return rec.Limit
}

// recordLimit shares a refusal with every other flow process on the machine.
// The LATER instant wins: two refusals in flight are one condition, and taking
// the earlier one would put the machine back on an endpoint that asked for
// longer.
func (c *seamCache) recordLimit(l seamLimit) {
	c.update(func(rec *seamRecord) {
		if rec.Limit != nil && rec.Limit.Until.After(l.Until) {
			return
		}
		rec.Limit = &l
	})
}

// clearLimit retires the refusal once a request has succeeded through it.
//
// It reads before it writes because it runs after EVERY successful request, and
// a store per request would be the cost this file exists to remove.
func (c *seamCache) clearLimit() {
	if rec := c.load(); rec == nil || rec.Limit == nil {
		return
	}
	c.update(func(rec *seamRecord) { rec.Limit = nil })
}

// row returns the cached answer for a key, and whether it is younger than
// maxAge. A row dated in the FUTURE is a miss rather than maximally fresh: a
// machine whose clock jumped backwards would otherwise pin the cache until the
// clock caught up.
func (c *seamCache) row(key string, now time.Time, maxAge time.Duration) (seamRow, bool, bool) {
	rec := c.load()
	if rec == nil {
		return seamRow{}, false, false
	}
	r, ok := rec.Rows[key]
	if !ok {
		return seamRow{}, false, false
	}
	age := now.Sub(r.StoredAt)
	return r, true, age >= 0 && age < maxAge
}

func (c *seamCache) storeRow(key string, r seamRow) {
	c.update(func(rec *seamRecord) {
		if rec.Rows == nil {
			rec.Rows = map[string]seamRow{}
		}
		rec.Rows[key] = r
	})
}

// refreshRow restamps a row whose revalidation came back 304 — the bytes are
// confirmed current as of now, and a row that kept its old timestamp would be
// revalidated again on a policy that ages.
func (c *seamCache) refreshRow(key string, now time.Time) {
	c.update(func(rec *seamRecord) {
		if r, ok := rec.Rows[key]; ok {
			r.StoredAt = now
			rec.Rows[key] = r
		}
	})
}

// stateCommentID reads the state-comment id this machine last observed for an
// issue, or 0.
//
// A cache and not an authority, exactly as the in-process memo it backs:
// fetchStateComment rescans newest-first whenever the id is empty or stale, so
// a wrong answer here costs one request and never a wrong document.
func (c *seamCache) stateCommentID(issueNum int) int64 {
	rec := c.load()
	if rec == nil {
		return 0
	}
	return rec.StateComments[strconv.Itoa(issueNum)]
}

func (c *seamCache) rememberStateCommentID(issueNum int, id int64) {
	if id == 0 {
		return
	}
	c.update(func(rec *seamRecord) {
		if rec.StateComments == nil {
			rec.StateComments = map[string]int64{}
		}
		rec.StateComments[strconv.Itoa(issueNum)] = id
	})
}

// stateLockPath is the machine-wide write lock for one item's state document.
// Per ITEM, not per repository: two arenas resolving different items write
// different comments and have no reason to wait for each other.
func (c *seamCache) stateLockPath(issueNum int) (string, bool) {
	if c == nil {
		return "", false
	}
	return filepath.Join(c.dir, seamCacheFilePrefix+c.key+".issue-"+strconv.Itoa(issueNum)+machinecache.LockSuffix), true
}

// freshness says how a response may be reused.
type freshness int

const (
	// freshNever — not cached at all. The state document, every comment list,
	// and everything this file does not name.
	freshNever freshness = iota
	// freshTTL — served without a request while younger than the policy's age.
	freshTTL
	// freshRevalidate — always requested, conditionally. A 304 serves the
	// cached bytes and costs no primary budget.
	freshRevalidate
)

// cachePolicy answers what may be reused for a URL: repository metadata for a
// day, a collaborator's permission for an hour, a blocker list for a minute, an
// issue read revalidated, everything else never.
//
// Keyed on the path RELATIVE to the "repos" segment so it holds against a base
// URL with a prefix — an enterprise host, or the httptest server the package's
// tests point the client at.
func cachePolicy(u *url.URL) (freshness, time.Duration) {
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	i := slices.Index(segs, "repos")
	// repos/{owner}/{repo} and then some.
	if i < 0 || len(segs) < i+3 {
		return freshNever, 0
	}
	rest := segs[i+3:]
	switch {
	case len(rest) == 0:
		return freshTTL, repoMetaTTL
	case len(rest) == 3 && rest[0] == "collaborators" && rest[2] == "permission":
		return freshTTL, collaboratorTTL
	case len(rest) == 4 && rest[0] == "issues" && isNumber(rest[1]) &&
		rest[2] == "dependencies" && rest[3] == "blocked_by":
		return freshTTL, blockerTTL
	case len(rest) == 2 && rest[0] == "issues" && isNumber(rest[1]):
		// The labels ride in this response and the claim protocol requires them
		// live, so this one is revalidated and never aged.
		return freshRevalidate, 0
	}
	return freshNever, 0
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil
}
