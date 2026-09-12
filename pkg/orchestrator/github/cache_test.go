package github

import (
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// The cache's whole claim is that a request is NOT made, which is a claim only
// the server can answer: a test asserting on the caller's return value cannot
// tell a cached answer from a fetched one. So every test here counts requests
// at the mock.

// Repository metadata does not move within a run, and every command reads it.
func TestCache_RepoMetadataIsServedWithoutASecondRequest(t *testing.T) {
	b, mock, _ := newSeamBackend(t)
	ctx := t.Context()
	const path = "GET /repos/o/r"

	if _, err := b.out.GetRepo(ctx); err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	mock.resetRequests()
	if _, err := b.out.GetRepo(ctx); err != nil {
		t.Fatalf("GetRepo (second): %v", err)
	}
	if got := mock.requestCount(path); got != 0 {
		t.Errorf("a second GetRepo made %d request(s); repository metadata is cached for %s", got, repoMetaTTL)
	}
	if u := b.ServiceRequests(); u.Served == 0 {
		t.Error("the meter recorded nothing served from cache")
	}

	// Past the TTL it is asked again — a cache that never expires is a stale
	// answer with no way back.
	expireSeamRows(t, b, repoMetaTTL+time.Minute)
	if _, err := b.out.GetRepo(ctx); err != nil {
		t.Fatalf("GetRepo (expired): %v", err)
	}
	if got := mock.requestCount(path); got != 1 {
		t.Errorf("past the TTL GetRepo made %d request(s), want 1", got)
	}
}

// #171: List costs one dependency request per listed item, because
// docs/orchestrator.md requires every listed item to carry its blockers and
// GitHub exposes that per issue. The requirement stands; what changes is that a
// wide listing pays it once per minute per MACHINE instead of once per item per
// invocation.
func TestCache_BlockersAreAskedOncePerTTLAndStillReported(t *testing.T) {
	b, mock, _ := newSeamBackend(t)
	ctx := t.Context()
	mock.mu.Lock()
	mock.blockedBy = []ghMockBlocker{
		{Number: 43, ID: 4300, State: "open", Title: "the blocker"},
	}
	mock.mu.Unlock()
	const path = "GET /repos/o/r/issues/42/dependencies/blocked_by"

	first, err := b.blockersOf(ctx, 42)
	if err != nil {
		t.Fatalf("blockersOf: %v", err)
	}
	if len(first) != 1 || first[0].Status == "" {
		t.Fatalf("blockers = %+v, want one carrying its status", first)
	}
	mock.resetRequests()

	// The N+1 shape: the same blocker read many times over, as a listing does.
	for range 10 {
		got, err := b.blockersOf(ctx, 42)
		if err != nil {
			t.Fatalf("blockersOf: %v", err)
		}
		if len(got) != len(first) || got[0].Status != first[0].Status {
			t.Fatalf("a cached listing reported %+v, want %+v — the requirement is unchanged", got, first)
		}
	}
	if n := mock.requestCount(path); n != 0 {
		t.Errorf("ten listings inside the TTL made %d dependency request(s), want 0", n)
	}

	expireSeamRows(t, b, blockerTTL+time.Second)
	if _, err := b.blockersOf(ctx, 42); err != nil {
		t.Fatalf("blockersOf (expired): %v", err)
	}
	if n := mock.requestCount(path); n != 1 {
		t.Errorf("past the %s TTL the listing made %d request(s), want 1", blockerTTL, n)
	}
}

// The issue read is REVALIDATED, never aged. Its labels ride in the same
// response and docs/github-schema.md § claim requires them re-read live, so
// this one is always asked — and a 304 answers it for no primary budget while a
// label added between two loads is visible on the second.
func TestCache_IssueReadIsAlwaysAskedAndNeverStale(t *testing.T) {
	b, mock, _ := newSeamBackend(t)
	ctx := t.Context()
	const path = "GET /repos/o/r/issues/42"

	if _, err := b.out.GetIssue(ctx, 42); err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	mock.resetRequests()
	if _, err := b.out.GetIssue(ctx, 42); err != nil {
		t.Fatalf("GetIssue (second): %v", err)
	}
	if n := mock.requestCount(path); n != 1 {
		t.Errorf("the second issue read made %d request(s), want 1 — this one is revalidated, not aged", n)
	}
	mock.mu.Lock()
	conditional := mock.conditionalIssueGets
	mock.mu.Unlock()
	if conditional != 1 {
		t.Errorf("the second read was answered %d time(s) with 304, want 1", conditional)
	}
	if u := b.ServiceRequests(); u.Revalidated == 0 {
		t.Error("the meter recorded no revalidation")
	}

	// A label added between the two loads is on the second. This is the
	// property the whole policy exists to keep: a stale answer here would let a
	// claim be granted over a holder that is already recorded.
	mock.mu.Lock()
	mock.issueLabels = append(mock.issueLabels, "flow:owner:carol")
	mock.mu.Unlock()
	iss, err := b.out.GetIssue(ctx, 42)
	if err != nil {
		t.Fatalf("GetIssue (after the label): %v", err)
	}
	if !hasLabel(labelNamesOf(iss.Labels), "flow:owner:carol") {
		t.Errorf("the issue read served a body from before the label landed: %v", labelNamesOf(iss.Labels))
	}
}

// The state document is NEVER cached. Staleness there is not a wasted request,
// it is a lost write: a mutation computed from a stale document overwrites
// whatever landed in between.
func TestCache_TheStateCommentIsNeverServedFromCache(t *testing.T) {
	b, mock, _ := newSeamBackend(t)
	ctx := t.Context()
	ref := b.refFromIssue(42)
	if _, err := b.Claim(ctx, ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// A recorded dispatch is what brings the state comment into being; nothing
	// seeds an item, so without one there is no document to read at all.
	if err := b.RecordDispatch(ctx, ref, "plan"); err != nil {
		t.Fatalf("RecordDispatch: %v", err)
	}
	if _, err := b.Load(ctx, ref); err != nil {
		t.Fatalf("Load: %v", err)
	}
	mock.resetRequests()
	if _, err := b.Load(ctx, ref); err != nil {
		t.Fatalf("Load (second): %v", err)
	}
	comments := 0
	mock.mu.Lock()
	for k, n := range mock.requests {
		if strings.HasPrefix(k, "GET /repos/o/r/issues/comments/") {
			comments += n
		}
	}
	mock.mu.Unlock()
	if comments == 0 {
		t.Error("the second Load read no state comment at all — it was served from cache, which is a lost write waiting to happen")
	}
}

// The record is shared by every flow process on the host — that is the whole
// point — and keyed by repository AND account, so a different token cannot read
// an answer it was never entitled to.
func TestCache_IsSharedByAccountAndRepositoryAndNotAcross(t *testing.T) {
	b, mock, _ := newSeamBackend(t)
	ctx := t.Context()
	if _, err := b.out.GetRepo(ctx); err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	mock.resetRequests()

	// A SECOND orchestrator, same repository and token, same cache directory —
	// a sibling arena's process.
	sibling := siblingSeam(t, b, mock, "fake-token")
	if _, err := sibling.GetRepo(ctx); err != nil {
		t.Fatalf("sibling GetRepo: %v", err)
	}
	if n := mock.requestCount("GET /repos/o/r"); n != 0 {
		t.Errorf("the sibling made %d request(s) for what this process already read", n)
	}

	// A different token is a different FILE: a miss by construction, with no
	// mismatch to detect on a hit.
	other := siblingSeam(t, b, mock, "another-account")
	if _, err := other.GetRepo(ctx); err != nil {
		t.Fatalf("other-account GetRepo: %v", err)
	}
	if n := mock.requestCount("GET /repos/o/r"); n != 1 {
		t.Errorf("a different token made %d request(s), want 1 — it must not read the first account's answers", n)
	}
}

// Nothing here may ever fail a call. A cache directory that cannot be read, a
// record that is torn, and a machine with nowhere to cache at all all degrade
// to the behaviour this package has without one.
func TestCache_DegradesToNoCache(t *testing.T) {
	t.Run("a torn record", func(t *testing.T) {
		b, mock, _ := newSeamBackend(t)
		ctx := t.Context()
		if _, err := b.out.GetRepo(ctx); err != nil {
			t.Fatalf("GetRepo: %v", err)
		}
		if err := os.WriteFile(b.out.cache.path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		mock.resetRequests()
		if _, err := b.out.GetRepo(ctx); err != nil {
			t.Fatalf("GetRepo over a torn record: %v", err)
		}
		if n := mock.requestCount("GET /repos/o/r"); n != 1 {
			t.Errorf("a torn record cost %d request(s), want the one it degrades to", n)
		}
	})

	t.Run("nowhere to cache", func(t *testing.T) {
		b, mock, _ := newSeamBackend(t)
		// The seam rebuilt on a machine that reports no cache directory at
		// all — every call fetches, which is exactly what this package did
		// before the cache existed.
		prev := cacheDir
		cacheDir = func() (string, bool) { return "", false }
		t.Cleanup(func() { cacheDir = prev })

		out := siblingSeam(t, b, mock, "fake-token")
		if out.cache != nil {
			t.Fatal("a machine with no cache directory got a cache")
		}
		mock.resetRequests()
		for range 2 {
			if _, err := out.GetRepo(t.Context()); err != nil {
				t.Fatalf("GetRepo with no cache: %v", err)
			}
		}
		if n := mock.requestCount("GET /repos/o/r"); n != 2 {
			t.Errorf("made %d request(s) with no cache, want 2 — every call fetches", n)
		}
	})
}

// The state-comment id is a cache and not an authority, and it is now shared:
// a fresh process otherwise pays a full newest-first comment scan to rediscover
// an id a sibling learnt a second earlier.
func TestCache_StateCommentIDIsRememberedAcrossProcesses(t *testing.T) {
	b, mock, _ := newSeamBackend(t)
	ctx := t.Context()
	ref := b.refFromIssue(42)
	if _, err := b.Claim(ctx, ref, nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.RecordDispatch(ctx, ref, "plan"); err != nil {
		t.Fatalf("RecordDispatch: %v", err)
	}
	if _, err := b.Load(ctx, ref); err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := b.cachedStateCommentID(42)
	if want == 0 {
		t.Fatal("no state-comment id was remembered")
	}
	if got := b.out.cache.stateCommentID(42); got != want {
		t.Errorf("the machine-wide record holds %d, the memo %d — one write path means they cannot differ", got, want)
	}

	// A fresh Orchestrator with an empty memo reads the record through.
	fresh := newMockedOrchestrator(t, mock, srvOf(t, mock))
	fresh.out.cache = b.out.cache
	if got := fresh.cachedStateCommentID(42); got != want {
		t.Errorf("a fresh process read %d, want %d from the shared record", got, want)
	}
}

// cachePolicy is the table the whole file rests on, asserted directly because
// each row is a decision with a reason and a wrong one is invisible in the
// behaviour tests above.
func TestCachePolicy_Table(t *testing.T) {
	for _, tc := range []struct {
		path    string
		want    freshness
		maxAge  time.Duration
		because string
	}{
		{"/repos/o/r", freshTTL, repoMetaTTL, "repository metadata does not move within a run"},
		{"/repos/o/r/collaborators/alice/permission", freshTTL, collaboratorTTL, "a role change lands within the hour"},
		{"/repos/o/r/issues/42/dependencies/blocked_by", freshTTL, blockerTTL, "#171's N+1"},
		{"/repos/o/r/issues/42", freshRevalidate, 0, "the labels ride in this response"},
		{"/repos/o/r/issues", freshNever, 0, "a listing is not one item"},
		{"/repos/o/r/issues/42/comments", freshNever, 0, "a comment list is how the state document is found"},
		{"/repos/o/r/issues/comments/1001", freshNever, 0, "the state document itself"},
		{"/repos/o/r/pulls", freshNever, 0, "a PR listing is a signal poll"},
		{"/api/v3/repos/o/r", freshTTL, repoMetaTTL, "a base URL with a prefix keys the same"},
		{"/user", freshNever, 0, "not under /repos at all"},
	} {
		u := mustParseURL(t, "https://example.test"+tc.path)
		got, age := cachePolicy(u)
		if got != tc.want || age != tc.maxAge {
			t.Errorf("cachePolicy(%s) = (%v, %s), want (%v, %s) — %s",
				tc.path, got, age, tc.want, tc.maxAge, tc.because)
		}
	}
}

// --- helpers ---

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// expireSeamRows ages every cached row by d, which is how a test reaches a TTL
// without waiting for one.
func expireSeamRows(t *testing.T, b *Orchestrator, d time.Duration) {
	t.Helper()
	b.out.cache.update(func(rec *seamRecord) {
		for k, row := range rec.Rows {
			row.StoredAt = row.StoredAt.Add(-d)
			rec.Rows[k] = row
		}
	})
}

// siblingSeam is a second seam over the same server and the same cache
// directory: another flow process on this machine.
func siblingSeam(t *testing.T, b *Orchestrator, mock *ghMock, token string) *outward {
	t.Helper()
	out := newOutward(token, b.git, mock.owner, mock.repo, allowing())
	out.client = newGitHubClient(token, nil, out.meter, out.cache)
	base, err := url.Parse(b.out.client.BaseURL.String())
	if err != nil {
		t.Fatal(err)
	}
	out.client.BaseURL = base
	out.client.UploadURL = base
	return out
}

// srvOf starts a second server over the same mock state, for the fresh-process
// case.
func srvOf(t *testing.T, mock *ghMock) *httptest.Server {
	t.Helper()
	srv := mock.server()
	t.Cleanup(srv.Close)
	return srv
}
