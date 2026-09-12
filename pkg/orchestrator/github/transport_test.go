package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
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
	if rec := s.cache.load(); rec != nil && rec.Limit != nil {
		t.Errorf("the refusal is still recorded after a request got through: %+v", rec.Limit)
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
