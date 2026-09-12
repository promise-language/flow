package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/machinecache"
)

// A rate limit used to arrive as an opaque 403 and kill the run. Everything
// below is about the two answers the seam owes instead: NAME the limit, and
// return it as the retryable condition the error vocabulary already defines.

// roundTripFunc drives the transport with a scripted server, so a test can
// answer a refusal without an httptest server and a real socket.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// canned builds one response with the status and headers a refusal carries.
func canned(status int, headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: status,
		Status:     strconv.Itoa(status),
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(`{"message":"API rate limit exceeded"}`)),
	}
}

// scriptedTransport wires a seamTransport to a scripted base and a stopped
// clock. The clock is stopped because the point of the cap is what the seam
// does with a wait it will NOT take — a test that slept two minutes to assert
// it does not sleep two minutes would be the defect it is testing for.
type scriptedTransport struct {
	*seamTransport
	now    time.Time
	slept  []time.Duration
	calls  int
	script []*http.Response
	errs   []error
}

func newScriptedTransport(t *testing.T, responses ...*http.Response) *scriptedTransport {
	t.Helper()
	useTempSeamCache(t)
	s := &scriptedTransport{now: time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC), script: responses}
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		i := s.calls
		s.calls++
		if i < len(s.errs) && s.errs[i] != nil {
			return nil, s.errs[i]
		}
		if i >= len(s.script) {
			t.Fatalf("the transport made request %d, and only %d were scripted", i+1, len(s.script))
		}
		return s.script[i], nil
	})
	s.seamTransport = newSeamTransport(base, newMeter(), newSeamCache("o", "r", "tok"))
	s.seamTransport.now = func() time.Time { return s.now }
	s.seamTransport.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.slept = append(s.slept, d)
		s.now = s.now.Add(d)
		return nil
	}
	return s
}

func getThrough(t *testing.T, s *scriptedTransport) (*http.Response, error) {
	t.Helper()
	return getThroughCtx(t, s, context.Background())
}

func getThroughCtx(t *testing.T, s *scriptedTransport, ctx context.Context) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://example.test/repos/o/r/issues/42/timeline", nil)
	if err != nil {
		t.Fatal(err)
	}
	return s.RoundTrip(req)
}

// getPathThrough is getThrough against a chosen path, for the tests whose
// subject is the cache policy that path falls under rather than the refusal.
func getPathThrough(t *testing.T, s *scriptedTransport, path string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest("GET", "https://example.test"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s.RoundTrip(req)
}

func okResponse() *http.Response {
	r := canned(http.StatusOK, nil)
	r.Body = io.NopCloser(strings.NewReader(`{}`))
	return r
}

// A wait short enough to take is TAKEN, and the request is retried once. This
// is the case that must not become a park: a limit clearing in a second is a
// wait, and parking on it would cost a re-dispatch to learn nothing.
func TestRateLimit_ShortRetryAfterIsSleptOffAndRetried(t *testing.T) {
	s := newScriptedTransport(t,
		canned(http.StatusForbidden, map[string]string{"Retry-After": "1"}),
		okResponse(),
	)
	resp, err := getThrough(t, s)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after the retry", resp.StatusCode)
	}
	if len(s.slept) != 1 || s.slept[0] != time.Second {
		t.Errorf("slept %v, want one wait of 1s — the header's own answer", s.slept)
	}
	u := s.meter.Snapshot()
	if u.Requests != 2 {
		t.Errorf("meter counted %d requests, want 2 — the refusal and the retry", u.Requests)
	}
	if u.Waits != 1 || u.Waited != time.Second {
		t.Errorf("meter recorded %d wait(s) totalling %s, want 1 of 1s", u.Waits, u.Waited)
	}
}

// Past the cap it is a CONDITION, not a wait: nothing sleeps, and what comes
// back is flow.ErrUnavailable naming the endpoint and the instant it clears.
// Holding a claim and an arena asleep for twenty minutes is what the cap is
// for.
func TestRateLimit_LongWindowIsReturnedAsACondition(t *testing.T) {
	s := newScriptedTransport(t)
	reset := s.now.Add(20 * time.Minute)
	s.script = []*http.Response{canned(http.StatusForbidden, map[string]string{
		"X-RateLimit-Remaining": "0",
		"X-RateLimit-Reset":     strconv.FormatInt(reset.Unix(), 10),
	})}

	_, err := getThrough(t, s)
	if err == nil {
		t.Fatal("a rate limit past the wait cap returned no error")
	}
	if !errors.Is(err, flow.ErrUnavailable) {
		t.Errorf("error = %v, want it to carry flow.ErrUnavailable — a rate limit is a condition, not a verdict", err)
	}
	var limited *rateLimited
	if !errors.As(err, &limited) {
		t.Fatalf("error = %v, want a *rateLimited naming what happened", err)
	}
	if !limited.Until.Equal(reset) {
		t.Errorf("Until = %s, want the instant the response named (%s)", limited.Until, reset)
	}
	if !limited.Primary {
		t.Error("a refusal with x-ratelimit-remaining: 0 is the PRIMARY budget, and the distinction is the diagnosis")
	}
	for _, want := range []string{"rate limit", "/repos/o/r/issues/42/timeline", "2026-09-12T10:20:00Z"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err, want)
		}
	}
	if len(s.slept) != 0 {
		t.Errorf("slept %v on a window past the cap; the step must park, not block", s.slept)
	}
}

// GitHub's secondary limiter answers 429 with no headers at all under load. A
// caller that retried immediately would BE the load, so the seam imposes its own
// backoff rather than treating "no instant named" as "no limit".
func TestRateLimit_BareTooManyRequestsTakesTheDefaultBackoff(t *testing.T) {
	s := newScriptedTransport(t, canned(http.StatusTooManyRequests, nil), okResponse())
	if _, err := getThrough(t, s); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if len(s.slept) != 1 || s.slept[0] != seamDefaultBackoff {
		t.Errorf("slept %v, want one wait of %s", s.slept, seamDefaultBackoff)
	}
}

// A cancelled caller must not be held in the round tripper for the wait it
// would otherwise take.
func TestRateLimit_CancelledContextReturnsPromptly(t *testing.T) {
	s := newScriptedTransport(t, canned(http.StatusForbidden, map[string]string{"Retry-After": "30"}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := getThroughCtx(t, s, ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if len(s.slept) != 0 {
		t.Errorf("slept %v after the caller gave up", s.slept)
	}
}

// cannedBody is a refusal carrying a specific document, which is how the
// secondary limiter answers: the headers say nothing, and the body is the only
// place the condition is named.
func cannedBody(status int, headers map[string]string, body string) *http.Response {
	r := canned(status, headers)
	r.Body = io.NopCloser(strings.NewReader(body))
	return r
}

// The shape this whole change was filed over: a 403 on a READ with the primary
// budget untouched. No Retry-After, X-RateLimit-Remaining well above zero, and
// nothing in the headers to tell it from a permission refusal — the condition is
// named in the body, which is where go-github reads it too.
//
// Classifying on headers alone lets exactly this one through as an ordinary
// error, which is the defect, not the fix.
func TestRateLimit_SecondaryLimitNamedOnlyInTheBodyIsRecognised(t *testing.T) {
	const doc = `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.",` +
		`"documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#secondary-rate-limits"}`
	s := newScriptedTransport(t,
		cannedBody(http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "4321"}, doc),
		okResponse(),
	)
	if _, err := getThrough(t, s); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if len(s.slept) != 1 || s.slept[0] != seamDefaultBackoff {
		t.Fatalf("slept %v, want the default backoff — the refusal named the limit but no instant", s.slept)
	}

	// And past the point where it can be waited out it is the CONDITION, named
	// as the secondary limit it is rather than as the primary budget.
	s2 := newScriptedTransport(t,
		cannedBody(http.StatusForbidden, map[string]string{"Retry-After": "900"}, doc))
	_, err := getThrough(t, s2)
	if !errors.Is(err, flow.ErrUnavailable) {
		t.Fatalf("error = %v, want flow.ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "secondary") {
		t.Errorf("message %q does not name the secondary limit; the distinction is the diagnosis", err)
	}
}

// A secondary refusal must not be parked on the PRIMARY window's reset. The
// primary reset is running whether anything is spending against it or not, so
// reading it here would hold a machine off for up to an hour on a condition
// that clears in a minute.
func TestRateLimit_SecondaryRefusalIgnoresThePrimaryReset(t *testing.T) {
	s := newScriptedTransport(t)
	s.script = []*http.Response{
		cannedBody(http.StatusTooManyRequests, map[string]string{
			"X-RateLimit-Remaining": "4999",
			"X-RateLimit-Reset":     strconv.FormatInt(s.now.Add(50*time.Minute).Unix(), 10),
		}, `{"message":"You have exceeded a secondary rate limit."}`),
		okResponse(),
	}
	if _, err := getThrough(t, s); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if len(s.slept) != 1 || s.slept[0] != seamDefaultBackoff {
		t.Errorf("slept %v, want the default backoff rather than the primary window's 50 minutes", s.slept)
	}
}

// A refusal cannot take this machine off the air for longer than one could
// honestly ask for. The recorded window short-circuits every request BEFORE it
// is made, so clearLimit — which runs only after one gets through — can never
// fire early: the clock is the only way out, and an uncapped header is a wedge
// with no way back but deleting the record by hand.
func TestRateLimit_AnAbsurdWindowIsCapped(t *testing.T) {
	for name, headers := range map[string]map[string]string{
		// Milliseconds where seconds are specified — the ordinary way a proxy
		// gets this wrong.
		"a reset in the year 57680": {
			"X-RateLimit-Remaining": "0",
			"X-RateLimit-Reset":     "1757664000000",
		},
		"a Retry-After of a century": {"Retry-After": "3153600000"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newScriptedTransport(t, canned(http.StatusForbidden, headers))
			_, err := getThrough(t, s)
			var limited *rateLimited
			if !errors.As(err, &limited) {
				t.Fatalf("error = %v, want a *rateLimited", err)
			}
			if got := limited.Until.Sub(s.now); got > seamLimitCap {
				t.Errorf("the machine is held off for %s, want no more than %s", got, seamLimitCap)
			}
			if l := s.cache.limitInForce(s.now.Add(seamLimitCap).Add(time.Second)); l != nil {
				t.Errorf("the shared record still refuses past the cap: %+v", l)
			}
		})
	}
}

// A 403 that is NOT a rate limit must not be reclassified. The whole
// distinction CollaboratorPermission rests on is "detected nothing" versus
// "could not ask", and turning a permission refusal into a retryable condition
// would make role derivation retry forever against a token that will never be
// allowed.
func TestRateLimit_APlainForbiddenIsNotReclassified(t *testing.T) {
	s := newScriptedTransport(t, canned(http.StatusForbidden, nil))
	resp, err := getThrough(t, s)
	if err != nil {
		t.Fatalf("RoundTrip: %v, want the 403 passed through untouched", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want the 403 as it arrived", resp.StatusCode)
	}
	// And with its DOCUMENT intact. Classifying a refusal means reading the
	// body; a classifier that consumed it would trade one misread refusal for
	// another, because go-github parses this same body for the message
	// AddBlockedBy and CollaboratorPermission discriminate on.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the passed-through body: %v", err)
	}
	if !strings.Contains(string(body), "API rate limit exceeded") {
		t.Errorf("body = %q, want the document the response arrived with", body)
	}
}

// The same thing through the whole stack, on the endpoint the distinction is
// about: a token that may not read collaborators still gets an ERROR, and not a
// condition a caller would wait on.
func TestRateLimit_CollaboratorForbiddenStaysAnError(t *testing.T) {
	mock := newGHMock(t)
	mock.collaboratorPermsForbidden = true
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)

	_, _, err := b.out.CollaboratorPermission(t.Context(), "alice")
	if err == nil {
		t.Fatal("a forbidden collaborator read returned no error")
	}
	if errors.Is(err, flow.ErrUnavailable) {
		t.Errorf("error = %v, classified as a retryable condition — 'could not ask' is a fact about the caller", err)
	}
}

// The classification has to survive the wrapping every caller does, or it is
// worth nothing: Load wraps the transport's error in "get issue N: %w", and the
// step that receives it is the one deciding between a park and a failure.
func TestRateLimit_SurvivesTheWrappingLoadDoes(t *testing.T) {
	mock := newGHMock(t)
	reset := time.Now().Add(20 * time.Minute)
	mock.refusals = []ghMockRefusal{{
		Status: http.StatusForbidden,
		Headers: map[string]string{
			"X-RateLimit-Remaining": "0",
			"X-RateLimit-Reset":     strconv.FormatInt(reset.Unix(), 10),
		},
	}}
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)

	_, err := b.Load(t.Context(), b.refFromIssue(42))
	if err == nil {
		t.Fatal("Load against a rate-limited API returned no error")
	}
	if !errors.Is(err, flow.ErrUnavailable) {
		t.Errorf("Load error = %v, want flow.ErrUnavailable through the wrapping", err)
	}
	if !strings.Contains(err.Error(), "get issue 42") {
		t.Errorf("Load error = %v, want it to still say what it was doing", err)
	}
}

// A refusal one process earns binds the machine. Three arenas independently
// discovering the same secondary limit is three refusals for one condition, and
// each one extends the window already in force.
func TestRateLimit_IsSharedAcrossTheMachineAndExpires(t *testing.T) {
	dir := useTempSeamCache(t)
	cache := newSeamCache("o", "r", "tok")
	if cache == nil {
		t.Fatalf("no cache in %s", dir)
	}
	until := time.Now().Add(10 * time.Minute)
	cache.recordLimit(seamLimit{Until: until, Endpoint: "GET /repos/o/r/issues/1"})

	// A SECOND transport over the same directory — a sibling arena's process.
	sibling := &scriptedTransport{now: time.Now()}
	sibling.seamTransport = newSeamTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		sibling.calls++
		return okResponse(), nil
	}), newMeter(), newSeamCache("o", "r", "tok"))
	sibling.seamTransport.now = func() time.Time { return sibling.now }

	_, err := getThrough(t, sibling)
	if !errors.Is(err, flow.ErrUnavailable) {
		t.Fatalf("error = %v, want the sibling to inherit the refusal", err)
	}
	if sibling.calls != 0 {
		t.Errorf("the sibling made %d request(s) into a limit already in force", sibling.calls)
	}
	if got := sibling.meter.Snapshot().Requests; got != 0 {
		t.Errorf("meter counted %d requests for a call that made none", got)
	}

	// And the record expires: past the instant, the machine asks again. A
	// backoff that outlived its window would be an outage of its own.
	sibling.now = until.Add(time.Second)
	if _, err := getThrough(t, sibling); err != nil {
		t.Fatalf("after the window closed: %v", err)
	}
	if sibling.calls != 1 {
		t.Errorf("made %d request(s) after the window closed, want 1", sibling.calls)
	}
}

// A request that gets through retires a refusal the machine was still holding:
// the window GitHub names is an upper bound, not a promise.
func TestRateLimit_ASuccessfulRequestClearsTheSharedRefusal(t *testing.T) {
	s := newScriptedTransport(t, okResponse())
	s.cache.recordLimit(seamLimit{Until: s.now.Add(-time.Minute), Endpoint: "GET /x"})

	if _, err := getThrough(t, s); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if l := s.cache.limitInForce(s.now.Add(-2 * time.Minute)); l != nil {
		t.Errorf("the refusal is still recorded after a request got through: %+v", l)
	}

	// And retiring one nobody recorded writes NOTHING. This runs after every
	// successful request, so a store per request would be the cost the cache
	// exists to remove.
	fresh := newScriptedTransport(t, okResponse())
	if _, err := getThrough(t, fresh); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if _, err := os.Stat(fresh.cache.limitPath); !os.IsNotExist(err) {
		t.Errorf("a request that met no refusal wrote the refusal record; stat err = %v", err)
	}
}

// The refusal lives in a record of its own, so an arena storing a cached ANSWER
// cannot take it out from under the arenas that are honouring it. That race is
// the one thing this file's read-modify-write must not lose: a refusal is shared
// precisely so three arenas on one host do not each earn their own.
func TestRateLimit_ARowStoreCannotClobberTheSharedRefusal(t *testing.T) {
	useTempSeamCache(t)
	writer, honourer := newSeamCache("o", "r", "tok"), newSeamCache("o", "r", "tok")

	// The losing shape, exactly: one process READS the record, another records a
	// refusal, and the first then stores what it read — with its own answer
	// added — over the top. The store is direct because that is the point: the
	// writer is acting on a read taken before the refusal existed, which is what
	// no in-process merge can see.
	stale := seamRecord{Rows: map[string]seamRow{"/repos/o/r": {StoredAt: time.Now(), Body: []byte("{}")}}}
	honourer.recordLimit(seamLimit{Until: time.Now().Add(10 * time.Minute), Endpoint: "GET /repos/o/r/issues/1"})
	machinecache.Store(writer.path, stale, seamSweep)

	if l := honourer.limitInForce(time.Now()); l == nil {
		t.Fatal("the refusal is gone — a cached answer overwrote it, which puts every sibling back on a refusing endpoint")
	}
}

// The meter is what makes the cost visible, and cli prints the line it renders.
func TestServiceSpend_ReportsWhatTheRunCost(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)

	if line := b.ServiceSpend(); line != "" {
		t.Errorf("ServiceSpend before any request = %q, want nothing to report", line)
	}
	if _, err := b.Load(t.Context(), b.refFromIssue(42)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	u := b.ServiceRequests()
	if u.Requests == 0 {
		t.Fatal("the meter counted no requests for a Load that made several")
	}
	if u.ByMethod["GET"] == 0 {
		t.Errorf("ByMethod = %v, want the reads split out", u.ByMethod)
	}
	if line := b.ServiceSpend(); !strings.Contains(line, "github:") || !strings.Contains(line, "request") {
		t.Errorf("ServiceSpend = %q, want a line naming the seam and the count", line)
	}

	// A wait is part of what the run cost, and the only place an operator sees
	// that a limit was in force at all on a run that otherwise succeeded.
	b.out.meter.waited(3 * time.Second)
	if line := b.ServiceSpend(); !strings.Contains(line, "waited 3s") || !strings.Contains(line, "rate limit") {
		t.Errorf("ServiceSpend = %q, want it to report the wait a rate limit cost", line)
	}
}

// A limit costs at most ONE wait per call. The second refusal is the condition,
// not a second sleep: a seam that waited on every refusal would hold a claim,
// an arena and a deadline for as long as the limiter kept saying no, and a
// retry that recursed with retry=true would do it forever.
func TestRateLimit_ASecondRefusalIsNotWaitedOnAgain(t *testing.T) {
	s := newScriptedTransport(t,
		canned(http.StatusForbidden, map[string]string{"Retry-After": "1"}),
		canned(http.StatusForbidden, map[string]string{"Retry-After": "1"}),
	)
	_, err := getThrough(t, s)
	if !errors.Is(err, flow.ErrUnavailable) {
		t.Fatalf("error = %v, want the second refusal returned as the condition", err)
	}
	if len(s.slept) != 1 {
		t.Errorf("slept %v, want one wait — a refusal that survives the retry is a condition to park on", s.slept)
	}
	if s.calls != 2 {
		t.Errorf("made %d request(s), want 2 — the refusal and its one retry", s.calls)
	}
}

// A WRITE that is retried after a wait must send the same body again.
//
// The first attempt consumed it, so a retry that re-sent nothing would land an
// empty PATCH on the state document or an empty label set on the issue —
// success, with the write silently gone. net/http's GetBody is what makes the
// replay possible, and a request that has a body and no GetBody is one the seam
// must refuse to replay rather than send empty.
func TestRateLimit_ARetriedWriteResendsItsBody(t *testing.T) {
	const body = `{"labels":["flow:seeded"]}`
	s := newScriptedTransport(t,
		canned(http.StatusForbidden, map[string]string{"Retry-After": "1"}),
		okResponse(),
	)
	var sent []string
	scripted := s.seamTransport.base
	s.seamTransport.base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		sent = append(sent, string(b))
		return scripted.RoundTrip(r)
	})

	req, err := http.NewRequest("POST", "https://example.test/repos/o/r/issues/42/labels", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if len(sent) != 2 {
		t.Fatalf("the write was sent %d time(s), want the refusal and the retry", len(sent))
	}
	if sent[1] != body {
		t.Errorf("the retry carried %q, want the body the caller wrote (%q)", sent[1], body)
	}

	// Nothing to replay from: the refusal is the answer, and no second request
	// goes out at all.
	s2 := newScriptedTransport(t, canned(http.StatusForbidden, map[string]string{"Retry-After": "1"}))
	unreplayable := &http.Request{
		Method: "POST",
		URL:    mustParseURL(t, "https://example.test/repos/o/r/issues/42/labels"),
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
	_, err = s2.RoundTrip(unreplayable)
	if !errors.Is(err, flow.ErrUnavailable) {
		t.Errorf("error = %v, want the refusal rather than a replay of a body that cannot be replayed", err)
	}
	if s2.calls != 1 {
		t.Errorf("made %d request(s), want 1 — an empty retry is a lost write reported as a success", s2.calls)
	}
}

// A failure answer is neither cached nor replaced by what the cache holds.
//
// Caching one would make a transient 500 the answer for the whole TTL; serving
// the cached body INSTEAD of it would hide the failure behind a stale success,
// which is worse — the caller would act on an answer nothing confirmed.
func TestTransport_AnErrorAnswerIsNeitherCachedNorHiddenByTheCachedBody(t *testing.T) {
	s := newScriptedTransport(t)
	first := okResponse()
	first.Body = io.NopCloser(strings.NewReader(`{"full_name":"o/r"}`))
	failure := canned(http.StatusInternalServerError, nil)
	failure.Body = io.NopCloser(strings.NewReader(`{"message":"upstream is unwell"}`))
	s.script = []*http.Response{first, failure}

	if _, err := getPathThrough(t, s, "/repos/o/r"); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	// Past the TTL, so the failure is what the next read gets.
	s.now = s.now.Add(repoMetaTTL + time.Minute)
	resp, err := getPathThrough(t, s, "/repos/o/r")
	if err != nil {
		t.Fatalf("RoundTrip (over the failure): %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the 500 — a stale success in its place is an answer nothing confirmed", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "upstream is unwell") {
		t.Errorf("body = %q, want what the server actually said", got)
	}
	row, ok, _ := s.cache.row("/repos/o/r", s.now, repoMetaTTL)
	if !ok || !strings.Contains(string(row.Body), "full_name") {
		t.Errorf("the record holds %q, want the last SUCCESS — a cached failure is the answer for the whole TTL", row.Body)
	}
}

// The test seam is built through the same construction path as the real client,
// so a swapped HTTP client still carries the metered transport and the SAME
// meter. A seam whose test path bypassed its own metering would be a seam
// nothing about it was ever tested.
func TestWithHTTPClient_KeepsTheMeteredTransportAndTheMeter(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedOrchestrator(t, mock, srv)

	if _, err := b.out.GetIssue(t.Context(), 42); err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	before := b.ServiceRequests().Requests
	if before == 0 {
		t.Fatal("the meter counted nothing before the swap")
	}

	b.WithHTTPClient(srv.Client())
	if _, err := b.WithBaseURL(srv.URL+"/", srv.URL+"/"); err != nil {
		t.Fatalf("WithBaseURL: %v", err)
	}
	if _, err := b.out.GetIssue(t.Context(), 42); err != nil {
		t.Fatalf("GetIssue through the swapped client: %v", err)
	}
	if got := b.ServiceRequests().Requests; got != before+1 {
		t.Errorf("the meter counts %d requests, want %d — a swapped client must keep the transport and the count",
			got, before+1)
	}
}

// A primary refusal whose reset header is not an instant takes the seam's own
// backoff. Reading "0" as a Unix second would put the window in 1970, and a
// window already past is a wait of zero — an immediate retry straight back into
// the limiter that just refused.
func TestRateLimit_APrimaryRefusalWithNoUsableResetStillBacksOff(t *testing.T) {
	s := newScriptedTransport(t,
		canned(http.StatusForbidden, map[string]string{
			"X-RateLimit-Remaining": "0",
			"X-RateLimit-Reset":     "0",
		}),
		okResponse(),
	)
	if _, err := getThrough(t, s); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if len(s.slept) != 1 || s.slept[0] != seamDefaultBackoff {
		t.Errorf("slept %v, want one wait of %s rather than an immediate retry", s.slept, seamDefaultBackoff)
	}
}

// The two halves of parseRetryAfter, because the header is specified both ways
// and a seam that read only one would take the default backoff on the other —
// silently ignoring exactly what GitHub asked for.
func TestParseRetryAfter_ReadsSecondsAndDates(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{in: "30", want: now.Add(30 * time.Second), ok: true},
		{in: "0", want: now, ok: true},
		{in: "-5", want: now, ok: true}, // a window already closed is not a window into the past
		{in: "Sat, 12 Sep 2026 10:07:33 GMT", want: time.Date(2026, 9, 12, 10, 7, 33, 0, time.UTC), ok: true},
		{in: "", ok: false},
		{in: "soon", ok: false},
	} {
		got, ok := parseRetryAfter(tc.in, now)
		if ok != tc.ok {
			t.Errorf("parseRetryAfter(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && !got.Equal(tc.want) {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
