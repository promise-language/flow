package github

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/promise-language/flow"
)

// The metered transport.
//
// outward declares itself the only route to GitHub and forwarded: thirty-three
// methods through one seam that counted nothing, named nothing and kept
// nothing. A secondary rate limit therefore arrived as an opaque 403 on a READ,
// which is what a run looks like when it dies rather than waits — and the
// instant it would clear was in the error go-github had already parsed and this
// package printed without reading.
//
// A round tripper rather than thirty-three edits. Both raw NewRequest/Do
// endpoints (ListBlockedBy, AddBlockedBy) travel through it too, with no method
// aware it exists — which is the property that keeps a method added next year
// metered by default instead of by memory.
//
// Three things happen here and nowhere else:
//
//   - every request is counted, so a resolution can say what it spent;
//   - a refusal is RECOGNISED, waited out when the wait is short, and otherwise
//     returned as flow.ErrUnavailable — the sentinel errs.go already defines for
//     "a rate limit is in force. Retrying is what a caller should do";
//   - a cacheable read is answered from the machine-wide record (cache.go)
//     without a request, or revalidated for free.

const (
	// seamWaitCap is how long a refusal may be slept off inside a request. A
	// limit that clears in seconds is a wait; one that clears in twenty minutes
	// is a condition the step must PARK on, because a handler blocked in a
	// round tripper is a handler holding a claim, an arena and a deadline while
	// doing nothing.
	seamWaitCap = 2 * time.Minute

	// seamDefaultBackoff is what a refusal naming no instant imposes. GitHub's
	// secondary limiter answers 429 without headers under load, and a caller
	// that retried immediately would be the load.
	seamDefaultBackoff = 60 * time.Second

	// headerRetryAfter / headerRateRemaining / headerRateReset are the three
	// facts a refusal carries. go-github reads them for its own error types;
	// this reads them from the RESPONSE, because the classification has to
	// happen before go-github decides whether the status is an error at all.
	headerRetryAfter    = "Retry-After"
	headerRateRemaining = "X-RateLimit-Remaining"
	headerRateReset     = "X-RateLimit-Reset"
)

// rateLimited is a refusal named as the condition it is.
//
// It wraps flow.ErrUnavailable, so errors.Is holds at every one of the
// thirty-three call sites with no signature changed and no branch added:
// net/http wraps a round tripper's error in *url.Error, which unwraps, and
// go-github hands that back unchanged. A caller that wrapped it in turn —
// fmt.Errorf("get issue %d: %w", …) — keeps the classification too.
type rateLimited struct {
	// Endpoint is the method and path the refusal was earned on, so an
	// operator reads a cause rather than a status code.
	Endpoint string
	// Until is when GitHub says it clears. Zero when the refusal named no
	// instant at all.
	Until time.Time
	// Primary distinguishes the documented hourly budget from the secondary
	// limiter. The distinction is the diagnosis: at the moment of the refusal
	// that provoked this change, /rate_limit reported zero used on every
	// resource, which is what a secondary limit looks like from outside and is
	// invisible to the endpoint an operator would check.
	Primary bool
}

func (e *rateLimited) Error() string {
	kind := "secondary"
	if e.Primary {
		kind = "primary"
	}
	msg := fmt.Sprintf("GitHub %s rate limit in force on %s", kind, e.Endpoint)
	if e.Until.IsZero() {
		return msg + "; it named no reset instant"
	}
	return fmt.Sprintf("%s; clears at %s (in %s)", msg,
		e.Until.UTC().Format(time.RFC3339), roundedUntil(e.Until))
}

// Unwrap makes this the retryable condition rather than an error. See
// errs.go: collapsing the two "turns a missing capability into a retry loop
// that never terminates, and a rate limit into a permanent verdict".
func (e *rateLimited) Unwrap() error { return flow.ErrUnavailable }

// roundedUntil renders the wait the way GitHub's own error does — "7m33s" —
// which is the string this seam used to print while inspecting nothing.
func roundedUntil(until time.Time) time.Duration {
	d := time.Until(until)
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second)
}

// ServiceUsage is what one orchestrator spent at this seam. A seam that meters
// can report how many requests a resolution cost, which is the only way any of
// this stays fixed.
type ServiceUsage struct {
	// Requests actually made.
	Requests int
	// ByMethod splits them, because a secondary limit counts content-generating
	// requests and a POST is not a GET.
	ByMethod map[string]int
	// Served is the reads answered from the machine-wide cache with no request,
	// and Revalidated the ones a 304 confirmed for free.
	Served      int
	Revalidated int
	// Waits and Waited are the refusals slept off inside a request.
	Waits  int
	Waited time.Duration
}

// meter counts. One per orchestrator, shared with whatever client
// WithHTTPClient installs later, so a count does not restart when a test swaps
// the transport underneath.
type meter struct {
	mu sync.Mutex
	u  ServiceUsage
}

func newMeter() *meter { return &meter{u: ServiceUsage{ByMethod: map[string]int{}}} }

func (m *meter) request(method string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.u.Requests++
	m.u.ByMethod[method]++
}

func (m *meter) served()      { m.bump(func(u *ServiceUsage) { u.Served++ }) }
func (m *meter) revalidated() { m.bump(func(u *ServiceUsage) { u.Revalidated++ }) }

func (m *meter) waited(d time.Duration) {
	m.bump(func(u *ServiceUsage) { u.Waits++; u.Waited += d })
}

func (m *meter) bump(f func(*ServiceUsage)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(&m.u)
}

// Snapshot is a copy: a caller reading the counts while a request lands must
// not be reading the map the transport is writing.
func (m *meter) Snapshot() ServiceUsage {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.u
	u.ByMethod = maps.Clone(m.u.ByMethod)
	return u
}

// seamTransport wraps the http.RoundTripper the client uses.
type seamTransport struct {
	base  http.RoundTripper
	meter *meter
	// cache may be nil — a machine with nowhere to cache still meters and still
	// names a limit, it just buys everything again.
	cache *seamCache
	// now and sleep are the clock, replaced wholesale by a test. A rate-limit
	// test that actually slept for the cap would be a two-minute test.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

func newSeamTransport(base http.RoundTripper, m *meter, c *seamCache) *seamTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &seamTransport{base: base, meter: m, cache: c, now: time.Now, sleep: sleepCtx}
}

// sleepCtx waits, or gives up the moment the caller does. A step that was
// cancelled must not spend its last two minutes asleep in a round tripper.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (t *seamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	endpoint := req.Method + " " + req.URL.Path

	// A refusal one process on this machine earned binds the rest. Asking again
	// inside it earns a fresh 403 against the same account and extends the
	// window that is already in force — which is how three arenas turn one
	// condition into three dead runs.
	if l := t.cache.limitInForce(t.now()); l != nil {
		return nil, &rateLimited{Endpoint: endpoint, Until: l.Until, Primary: l.Primary}
	}

	key, policy, maxAge := t.cacheable(req)
	var cached seamRow
	var haveCached bool
	if policy != freshNever {
		row, ok, fresh := t.cache.row(key, t.now(), maxAge)
		cached, haveCached = row, ok
		if ok && fresh && policy == freshTTL {
			t.meter.served()
			return cachedResponse(req, row), nil
		}
		if ok && policy == freshRevalidate && row.ETag != "" {
			req = req.Clone(ctx)
			req.Header.Set("If-None-Match", row.ETag)
		}
	}

	resp, err := t.do(req, endpoint, true)
	if err != nil {
		return nil, err
	}

	if policy != freshNever {
		return t.reconcile(req, resp, key, cached, haveCached), nil
	}
	return resp, nil
}

// do makes the request, counts it, and decides what a refusal means. retry is
// false on the second attempt, so a limit can cost at most one wait per call.
func (t *seamTransport) do(req *http.Request, endpoint string, retry bool) (*http.Response, error) {
	t.meter.request(req.Method)
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		// A transport failure is returned as it always was. Widening the retry
		// to cover it would hide real failures behind a seam built to name
		// them.
		return nil, err
	}

	limit, ok := refusalIn(resp, t.now())
	if !ok {
		// A request that got through retires a refusal this machine was still
		// recording — the window GitHub named is an upper bound, not a promise.
		t.cache.clearLimit()
		return resp, nil
	}
	drain(resp)

	wait := limit.Until.Sub(t.now())
	if limit.Until.IsZero() {
		wait = seamDefaultBackoff
		limit.Until = t.now().Add(wait)
	}
	limit.Endpoint = endpoint
	if !retry || wait > seamWaitCap {
		// Past the cap this is a CONDITION, not a wait: it is shared with the
		// machine so no sibling earns it again, and returned as the retryable
		// sentinel so the step parks with the instant it clears rather than
		// failing with a status code.
		t.cache.recordLimit(limit)
		return nil, &rateLimited{Endpoint: endpoint, Until: limit.Until, Primary: limit.Primary}
	}
	if wait < 0 {
		wait = 0
	}
	if err := t.sleep(req.Context(), wait); err != nil {
		return nil, err
	}
	t.meter.waited(wait)
	next, err := rewind(req)
	if err != nil {
		// Nothing to replay the body from, so the refusal is the answer.
		t.cache.recordLimit(limit)
		return nil, &rateLimited{Endpoint: endpoint, Until: limit.Until, Primary: limit.Primary}
	}
	return t.do(next, endpoint, false)
}

// cacheable answers whether this request may be served or revalidated from the
// machine-wide record, and under what key.
//
// GET only, and only with a cache: a write is never a cache hit, and a HEAD
// answers no body to serve.
func (t *seamTransport) cacheable(req *http.Request) (string, freshness, time.Duration) {
	if t.cache == nil || req.Method != http.MethodGet {
		return "", freshNever, 0
	}
	policy, maxAge := cachePolicy(req.URL)
	if policy == freshNever {
		return "", freshNever, 0
	}
	return cacheKey(req.URL), policy, maxAge
}

// reconcile turns the response into what the caller sees and updates the
// record: a 304 is answered with the bytes already held, a 200 is stored, and
// anything else passes through untouched — an error must never be cached, and
// must never be replaced by a stale success either.
func (t *seamTransport) reconcile(req *http.Request, resp *http.Response, key string, cached seamRow, haveCached bool) *http.Response {
	switch {
	case resp.StatusCode == http.StatusNotModified && haveCached:
		drain(resp)
		t.meter.revalidated()
		t.cache.refreshRow(key, t.now())
		return cachedResponse(req, cached)
	case resp.StatusCode == http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			// The bytes are gone either way; hand the caller the read error
			// rather than a body it cannot use.
			resp.Body = io.NopCloser(errReader{err})
			return resp
		}
		t.cache.storeRow(key, seamRow{
			StoredAt: t.now(),
			ETag:     resp.Header.Get("ETag"),
			Body:     body,
		})
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		return resp
	}
	return resp
}

// cachedResponse is the answer served from the record: a plain 200 carrying the
// bytes and the tag, shaped so go-github decodes it exactly as it would the
// response it came from.
func cachedResponse(req *http.Request, row seamRow) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	if row.ETag != "" {
		h.Set("ETag", row.ETag)
	}
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(row.Body)),
		ContentLength: int64(len(row.Body)),
		Request:       req,
	}
}

// cacheKey names one answer. The query is in it because a paged or filtered
// read is a different question, even at the same path.
func cacheKey(u *url.URL) string {
	if u.RawQuery == "" {
		return u.Path
	}
	return u.Path + "?" + u.RawQuery
}

// refusalIn recognises a rate limit in a response, by the three shapes GitHub
// answers one with:
//
//   - 429, the documented secondary-limit status;
//   - 403 carrying Retry-After, which is the secondary limiter asking for a
//     specific wait;
//   - 403 with X-RateLimit-Remaining: 0, which is the primary budget spent.
//
// A 403 that is none of those is NOT a rate limit and must not be reclassified:
// "the token may not read collaborators" is a fact about the caller, and
// CollaboratorPermission's "detected nothing versus could not ask" distinction
// depends on it staying an error.
func refusalIn(resp *http.Response, now time.Time) (seamLimit, bool) {
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusForbidden {
		return seamLimit{}, false
	}
	retryAfter, hasRetryAfter := parseRetryAfter(resp.Header.Get(headerRetryAfter), now)
	primary := resp.Header.Get(headerRateRemaining) == "0"
	if resp.StatusCode == http.StatusForbidden && !hasRetryAfter && !primary {
		return seamLimit{}, false
	}
	l := seamLimit{Primary: primary}
	switch {
	case hasRetryAfter:
		l.Until = retryAfter
	default:
		if reset, ok := parseRateReset(resp.Header.Get(headerRateReset)); ok {
			l.Until = reset
		}
	}
	return l, true
}

// parseRetryAfter reads the header in both forms the spec allows: a count of
// seconds, and an HTTP date.
func parseRetryAfter(v string, now time.Time) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			secs = 0
		}
		return now.Add(time.Duration(secs) * time.Second), true
	}
	if at, err := http.ParseTime(v); err == nil {
		return at, true
	}
	return time.Time{}, false
}

// parseRateReset reads X-RateLimit-Reset, a Unix second.
func parseRateReset(v string) (time.Time, bool) {
	secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// rewind rebuilds a request for the retry. A body already read cannot be sent
// twice, and GetBody is what net/http provides for exactly this; a request that
// has a body and no GetBody is one this must not replay.
func rewind(req *http.Request) (*http.Request, error) {
	next := req.Clone(req.Context())
	if req.Body == nil || req.Body == http.NoBody {
		return next, nil
	}
	if req.GetBody == nil {
		return nil, fmt.Errorf("request body cannot be replayed")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	next.Body = body
	return next, nil
}

// drain releases a response whose body the caller will never see. Skipping it
// leaks the connection rather than the memory, which is the failure that shows
// up as a stall much later.
func drain(resp *http.Response) {
	if resp.Body == nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// errReader hands a read failure to the decoder rather than an empty body that
// would parse as an absent answer.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// meteredHTTPClient wraps base — which may be nil — in the transport above.
// outward.go turns it into the package's one github.Client; building that here
// would put a client in a second file, which the `service seams` gate refuses
// and which is the whole property the seam rests on.
func meteredHTTPClient(base *http.Client, m *meter, c *seamCache) *http.Client {
	inner := http.DefaultTransport
	if base != nil && base.Transport != nil {
		inner = base.Transport
	}
	metered := &http.Client{Transport: newSeamTransport(inner, m, c)}
	if base != nil {
		metered.Timeout = base.Timeout
		metered.CheckRedirect = base.CheckRedirect
		metered.Jar = base.Jar
	}
	return metered
}
