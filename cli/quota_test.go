package cli

import (
	"bytes"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func TestReportQuota_SkippedWhenRunner(t *testing.T) {
	t.Setenv(dispatchedByRunnerEnv, "1")
	var buf bytes.Buffer
	reportQuota(&buf)
	if buf.Len() != 0 {
		t.Errorf("runner-driven run should produce no quota output; got %q", buf.String())
	}
}

func TestReportQuota_AttemptedWhenHandDriven(t *testing.T) {
	t.Setenv(dispatchedByRunnerEnv, "")
	// Set CLAUDE_CONFIG_DIR to a nonexistent path so the credential discovery
	// fails deterministically — we're testing that the code path runs, not that
	// it succeeds.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var buf bytes.Buffer
	reportQuota(&buf)
	// Should print something (an error about missing credentials).
	if buf.Len() == 0 {
		t.Error("hand-driven run should attempt quota and print something")
	}
	if !strings.Contains(buf.String(), "quota:") {
		t.Errorf("output should be prefixed with 'quota:'; got %q", buf.String())
	}
}

func TestClampFraction(t *testing.T) {
	tests := []struct {
		in   float64
		want float64
	}{
		{-0.5, 0},
		{0, 0},
		{0.5, 0.5},
		{1.0, 1.0},
		{1.5, 1.0},
	}
	for _, tt := range tests {
		got := clampFraction(tt.in)
		if got != tt.want {
			t.Errorf("clampFraction(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestPrintWindow(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	win := windowUsage{
		Label:    "5h",
		Length:   5 * time.Hour,
		Used:     0.47,
		ResetsAt: now.Add(2*time.Hour + 24*time.Minute),
	}
	var buf bytes.Buffer
	printWindow(&buf, win, now)
	got := buf.String()

	if !strings.Contains(got, "47% used") {
		t.Errorf("expected '47%% used'; got %q", got)
	}
	if !strings.Contains(got, "of window elapsed") {
		t.Errorf("expected 'of window elapsed'; got %q", got)
	}
	if !strings.Contains(got, "resets in") {
		t.Errorf("expected 'resets in'; got %q", got)
	}
}

func TestPrintWindow_AbsentUsage(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	win := windowUsage{
		Label:    "7d",
		Length:   7 * 24 * time.Hour,
		Used:     -1, // absent
		ResetsAt: now.Add(3*24*time.Hour + 4*time.Hour),
	}
	var buf bytes.Buffer
	printWindow(&buf, win, now)
	got := buf.String()

	if !strings.Contains(got, "--% ") {
		t.Errorf("absent usage must render as '--%%'; got %q", got)
	}
	if strings.Contains(got, "0% used") {
		t.Errorf("absent usage must NOT render as '0%%'; got %q", got)
	}
}

func TestPrintWindow_ElapsedClamped(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	// Window already expired (resets_at is in the past).
	win := windowUsage{
		Label:    "5h",
		Length:   5 * time.Hour,
		Used:     0.90,
		ResetsAt: now.Add(-10 * time.Minute),
	}
	var buf bytes.Buffer
	printWindow(&buf, win, now)
	got := buf.String()

	// Elapsed must be clamped to 100%, not >100%.
	if !strings.Contains(got, "100% of window elapsed") {
		t.Errorf("expired window should show 100%% elapsed; got %q", got)
	}
	// Reset should show 0s or <1s, not a negative.
	if strings.Contains(got, "-") && !strings.Contains(got, "--% ") {
		t.Errorf("expired window should not show negative resets; got %q", got)
	}
}

func TestParseUsageResponse_WindowsFormat(t *testing.T) {
	body := `{
		"windows": [
			{
				"label": "5h",
				"length_seconds": 18000,
				"usage_fraction": 0.47,
				"resets_at": "2026-09-01T14:24:00Z"
			},
			{
				"label": "7d",
				"length_seconds": 604800,
				"usage_fraction": 0.31,
				"resets_at": "2026-09-04T16:00:00Z"
			}
		]
	}`
	result, err := parseUsageResponse([]byte(body))
	if err != nil {
		t.Fatalf("parseUsageResponse: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 windows; got %d", len(result))
	}
	if result[0].Label != "5h" {
		t.Errorf("window 0 label = %q, want 5h", result[0].Label)
	}
	if math.Abs(result[0].Used-0.47) > 0.001 {
		t.Errorf("window 0 used = %v, want 0.47", result[0].Used)
	}
}

func TestParseUsageResponse_EmptyBody(t *testing.T) {
	_, err := parseUsageResponse([]byte(`{}`))
	if err == nil {
		t.Error("expected error for empty response")
	}
}

func TestParseUsageResponse_RateLimitsFormat(t *testing.T) {
	body := `{
		"rate_limits": [
			{
				"window": "5h",
				"used": 470,
				"limit": 1000,
				"resets_at": "2026-09-01T14:24:00Z"
			},
			{
				"window": "7d",
				"used": 310,
				"limit": 1000,
				"resets_at": "2026-09-04T16:00:00Z"
			}
		]
	}`
	result, err := parseUsageResponse([]byte(body))
	if err != nil {
		t.Fatalf("parseUsageResponse: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 windows; got %d", len(result))
	}
	if result[0].Label != "5h" {
		t.Errorf("window 0 label = %q, want 5h", result[0].Label)
	}
	if math.Abs(result[0].Used-0.47) > 0.001 {
		t.Errorf("window 0 used = %v, want 0.47", result[0].Used)
	}
	if result[0].Length != 5*time.Hour {
		t.Errorf("window 0 length = %v, want 5h", result[0].Length)
	}
}

func TestParseUsageResponse_RateLimitsZeroLimit(t *testing.T) {
	body := `{
		"rate_limits": [
			{
				"window": "5h",
				"used": 100,
				"limit": 0,
				"resets_at": "2026-09-01T14:24:00Z"
			}
		]
	}`
	result, err := parseUsageResponse([]byte(body))
	if err != nil {
		t.Fatalf("parseUsageResponse: %v", err)
	}
	// Zero limit means used fraction is unknown (-1).
	if result[0].Used != -1 {
		t.Errorf("zero limit should produce Used=-1; got %v", result[0].Used)
	}
}

func TestPrintWindow_ZeroLengthWindow(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	win := windowUsage{
		Label:    "??",
		Length:   0, // unknown window length
		Used:     0.50,
		ResetsAt: now.Add(1 * time.Hour),
	}
	var buf bytes.Buffer
	printWindow(&buf, win, now)
	got := buf.String()

	if !strings.Contains(got, "--% of window elapsed") {
		t.Errorf("zero-length window must show '--%%' elapsed; got %q", got)
	}
	if !strings.Contains(got, "50% used") {
		t.Errorf("used fraction should still render; got %q", got)
	}
}

func TestDiscoverOAuthToken_ValidCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	creds := `{
		"claudeai_oauth": {
			"token": "test-token-abc123"
		}
	}`
	if err := os.WriteFile(dir+"/credentials.json", []byte(creds), 0644); err != nil {
		t.Fatal(err)
	}

	tok, reason := discoverOAuthToken()
	if reason != "" {
		t.Fatalf("expected success; got reason=%q", reason)
	}
	if tok != "test-token-abc123" {
		t.Errorf("token = %q, want test-token-abc123", tok)
	}
}

func TestDiscoverOAuthToken_EmptyToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	creds := `{
		"claudeai_oauth": {
			"token": ""
		}
	}`
	if err := os.WriteFile(dir+"/credentials.json", []byte(creds), 0644); err != nil {
		t.Fatal(err)
	}

	_, reason := discoverOAuthToken()
	if reason == "" {
		t.Error("expected failure reason for empty token")
	}
	if !strings.Contains(reason, "expired") {
		t.Errorf("reason should mention 'expired'; got %q", reason)
	}
}

func TestDiscoverOAuthToken_NoOAuthKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	creds := `{"some_other_key": {"value": "x"}}`
	if err := os.WriteFile(dir+"/credentials.json", []byte(creds), 0644); err != nil {
		t.Fatal(err)
	}

	_, reason := discoverOAuthToken()
	if reason == "" {
		t.Error("expected failure reason for missing OAuth key")
	}
	if !strings.Contains(reason, "no Claude OAuth") {
		t.Errorf("reason should mention 'no Claude OAuth'; got %q", reason)
	}
}

func TestWindowLengthFromLabel(t *testing.T) {
	if got := windowLengthFromLabel("5h"); got != 5*time.Hour {
		t.Errorf("5h = %v", got)
	}
	if got := windowLengthFromLabel("7d"); got != 7*24*time.Hour {
		t.Errorf("7d = %v", got)
	}
	if got := windowLengthFromLabel("unknown"); got != 0 {
		t.Errorf("unknown = %v", got)
	}
}
