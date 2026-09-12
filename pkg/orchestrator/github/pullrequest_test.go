package github

import (
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// Opening and merging a pull request used to shell out to `gh`, and the tests
// for them asserted a command line. There is no command line any more: both
// authenticate with the same token against the same limits, so the split was a
// transport one, and only the client's traffic can be metered, cached or told
// apart from a rate limit.
//
// What replaces the argv assertions is the request itself. A test that mocked
// the RESULT would pass against a version that called the wrong endpoint, which
// is the shape of defect the `gh -C` flag shipped as.

func TestOpenPullRequest_PostsThePullAndReturnsItsURL(t *testing.T) {
	b, mock, _ := newSeamBackend(t)
	url, err := b.out.OpenPullRequest(t.Context(), "main", "flow/issue-42", "a title", "a body")
	if err != nil {
		t.Fatalf("OpenPullRequest: %v", err)
	}
	if n := mock.requestCount("POST /repos/o/r/pulls"); n != 1 {
		t.Errorf("made %d POST(s) to the pulls endpoint, want 1", n)
	}
	if url != "https://github.com/o/r/pull/1" {
		t.Errorf("URL = %q, want the html_url GitHub issued", url)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.prCreates) != 1 {
		t.Fatalf("recorded %d create(s)", len(mock.prCreates))
	}
	// The head needs no owner qualification: owner and repo come from this
	// checkout's origin, which is the repository the branch was pushed to.
	for field, want := range map[string]string{
		"base":  "main",
		"head":  "flow/issue-42",
		"title": "a title",
		"body":  "a body",
	} {
		if got, _ := mock.prCreates[0][field].(string); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
}

// The merge happens now or fails now; it is not queued. Naming squash is what
// keeps a strategy from drifting back to the repository default — #292 is open
// for whether this repository should name one at all.
func TestMergePullRequest_PutsTheMergeWithTheSquashStrategy(t *testing.T) {
	b, mock, _ := newSeamBackend(t)
	if err := b.out.MergePullRequest(t.Context(), "https://github.com/o/r/pull/7"); err != nil {
		t.Fatalf("MergePullRequest: %v", err)
	}
	if n := mock.requestCount("PUT /repos/o/r/pulls/7/merge"); n != 1 {
		t.Errorf("made %d merge request(s) for pull 7, want 1", n)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.prMerges) != 1 {
		t.Fatalf("recorded %d merge(s)", len(mock.prMerges))
	}
	if got, _ := mock.prMerges[0]["merge_method"].(string); got != "squash" {
		t.Errorf("merge_method = %q, want squash", got)
	}
}

// A URL naming no number is a CALLER defect, and it errors before the guard is
// consulted: there is no write for a guard to have an opinion about, and
// publish's contract is that it refuses before the act and never after.
func TestMergePullRequest_AnUnparseableURLErrorsBeforeTheGuard(t *testing.T) {
	b, mock, _ := newSeamBackend(t)
	guard := &recordingGuard{}
	b.out.guard = guard

	err := b.out.MergePullRequest(t.Context(), "https://github.com/o/r/pull/not-a-number")
	if err == nil {
		t.Fatal("an unparseable pull request URL was accepted")
	}
	if !strings.Contains(err.Error(), "names no number") {
		t.Errorf("error = %v, want it to say what is wrong with the URL", err)
	}
	if seen := guard.of(flow.ActMerge); len(seen) != 0 {
		t.Errorf("the guard was consulted about a merge that cannot happen: %v", seen)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.prMerges) != 0 {
		t.Errorf("a merge request left for an unparseable URL: %v", mock.prMerges)
	}
}

func TestPRNumberFromURL(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		ok   bool
	}{
		{in: "https://github.com/o/r/pull/1", want: 1, ok: true},
		{in: "https://github.com/o/r/pull/4321", want: 4321, ok: true},
		{in: "https://github.com/o/r/pull/12/", want: 12, ok: true}, // a trailing slash is not a defect
		{in: "  https://github.com/o/r/pull/9\n", want: 9, ok: true},
		{in: "https://github.com/o/r/pull/abc"},
		{in: "https://github.com/o/r/pull/0"}, // there is no pull request zero
		{in: "https://github.com/o/r/pull/-3"},
		{in: ""},
		{in: "42"},
	} {
		got, err := prNumberFromURL(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("prNumberFromURL(%q) = (%d, %v), want (%d, nil)", tc.in, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Errorf("prNumberFromURL(%q) = %d, want an error", tc.in, got)
		}
	}
}
