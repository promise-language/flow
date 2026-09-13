package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

func TestReportQuota_PrintsOnFailure(t *testing.T) {
	// Set CLAUDE_CONFIG_DIR to a nonexistent path and strip PATH so both
	// file-based and Keychain credential discovery fail deterministically.
	useTempQuotaCache(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	var buf bytes.Buffer
	reportQuota(&buf)
	// Should print something (an error about missing credentials).
	if buf.Len() == 0 {
		t.Error("reportQuota should print a diagnostic on failure")
	}
	if !strings.Contains(buf.String(), "quota:") {
		t.Errorf("output should be prefixed with 'quota:'; got %q", buf.String())
	}
}

func TestReadQuota_ReturnsErrorOnFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	usage, err := readQuota()
	if err == nil {
		t.Error("expected error when credentials are missing")
	}
	if usage != nil {
		t.Errorf("expected nil usage on error; got %v", usage)
	}
}

// The acceptance line for #183: a valid .credentials.json on a non-mac host
// yields a READING, not just a token. The discoverOAuthToken tests stop at the
// token and TestFetchUsage_Success starts from one, so the join — readQuota
// reading the credential at all, and handing the file's token to the request —
// is what neither of them would miss going wrong. That join is the whole of
// what "pacing disabled" was reporting.
func TestReadQuota_ReadsTheFileCredentialEndToEnd(t *testing.T) {
	useCredentialGOOS(t, "linux")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"five_hour":{"utilization":47,"resets_at":"2026-09-01T14:24:00Z"}}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeCredentialsFile(t, dir, ".credentials.json",
		claudeCredentials("file-token", time.Now().Add(time.Hour).UnixMilli()))
	if err := os.WriteFile(filepath.Join(dir, "settings.json"),
		fmt.Appendf(nil, `{"apiBaseUrl":%q}`, srv.URL), 0o600); err != nil {
		t.Fatal(err)
	}

	usage, err := readQuota()
	if err != nil {
		t.Fatalf("readQuota: %v — a present, fresh credential must produce a reading", err)
	}
	if len(usage) != 1 || math.Abs(usage[0].Used-0.47) > 0.001 {
		t.Errorf("usage = %+v, want one window at 0.47", usage)
	}
	if gotAuth != "Bearer file-token" {
		t.Errorf("Authorization = %q, want the token from the file", gotAuth)
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

func TestParseUsageResponse_RealSchema(t *testing.T) {
	body := `{
		"five_hour": {
			"utilization": 47.0,
			"resets_at": "2026-09-01T14:24:00Z"
		},
		"seven_day": {
			"utilization": 31.0,
			"resets_at": "2026-09-04T16:00:00Z"
		}
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
	if result[1].Label != "7d" {
		t.Errorf("window 1 label = %q, want 7d", result[1].Label)
	}
	if math.Abs(result[1].Used-0.31) > 0.001 {
		t.Errorf("window 1 used = %v, want 0.31", result[1].Used)
	}
	if result[1].Length != 7*24*time.Hour {
		t.Errorf("window 1 length = %v, want 7d", result[1].Length)
	}
}

func TestParseUsageResponse_EmptyBody(t *testing.T) {
	_, err := parseUsageResponse([]byte(`{}`))
	if err == nil {
		t.Error("expected error for empty response")
	}
}

func TestParseUsageResponse_NullUtilization(t *testing.T) {
	body := `{
		"five_hour": {
			"utilization": null,
			"resets_at": "2026-09-01T14:24:00Z"
		}
	}`
	result, err := parseUsageResponse([]byte(body))
	if err != nil {
		t.Fatalf("parseUsageResponse: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 window; got %d", len(result))
	}
	if result[0].Used != -1 {
		t.Errorf("null utilization should produce Used=-1; got %v", result[0].Used)
	}
}

func TestParseUsageResponse_OnlyFiveHour(t *testing.T) {
	body := `{
		"five_hour": {
			"utilization": 17.0,
			"resets_at": "2026-09-01T14:24:00Z"
		}
	}`
	result, err := parseUsageResponse([]byte(body))
	if err != nil {
		t.Fatalf("parseUsageResponse: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 window; got %d", len(result))
	}
	if result[0].Label != "5h" {
		t.Errorf("label = %q, want 5h", result[0].Label)
	}
	if math.Abs(result[0].Used-0.17) > 0.001 {
		t.Errorf("used = %v, want 0.17", result[0].Used)
	}
}

func TestDiscoverAPIBase_DefaultFallback(t *testing.T) {
	// Point config dir to an empty temp dir so no settings file is found,
	// and ensure the claude binary is not on PATH.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())

	base, reason := discoverAPIBase()
	if reason != "" {
		t.Fatalf("expected success; got reason=%q", reason)
	}
	if base != "https://api.anthropic.com" {
		t.Errorf("base = %q, want https://api.anthropic.com", base)
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

// useCredentialGOOS pins which of the two credential sources
// discoverOAuthToken selects, so both branches are exercised wherever the tests
// run rather than only on the OS the developer happens to be on — which is how
// the file branch stayed broken for #144's whole life (#183).
func useCredentialGOOS(t *testing.T, goos string) {
	t.Helper()
	prev := credentialGOOS
	credentialGOOS = goos
	t.Cleanup(func() { credentialGOOS = prev })
}

// stubSecurity puts a `security` on PATH that prints body and succeeds. It is
// what exercises the Keychain branch off a Mac, and the only way to assert the
// file branch never reaches a Keychain that WOULD have answered.
func stubSecurity(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the security stub is POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "security")
	// `echo` and not a heredoc through `cat`: PATH is replaced by the stub's own
	// directory, so the script may use nothing but shell builtins.
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+body+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil { // WriteFile respects umask; Chmod does not
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// claudeCredentials renders the JSON both sources hold. expiresAt is unix
// milliseconds; 0 omits the field entirely.
func claudeCredentials(token string, expiresAt int64) string {
	if expiresAt == 0 {
		return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q}}`, token)
	}
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d}}`, token, expiresAt)
}

// writeCredentialsFile writes body to dir/name, creating dir, and returns the
// path. name is a parameter because one test writes the undotted name the
// client never writes.
func writeCredentialsFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverOAuthToken_FileCredentials(t *testing.T) {
	useCredentialGOOS(t, "linux")
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeCredentialsFile(t, dir, ".credentials.json",
		claudeCredentials("test-token-abc123", time.Now().Add(time.Hour).UnixMilli()))

	tok, reason := discoverOAuthToken()
	if reason != "" {
		t.Fatalf("expected success; got reason=%q", reason)
	}
	if tok != "test-token-abc123" {
		t.Errorf("token = %q, want test-token-abc123", tok)
	}
}

// $CLAUDE_CONFIG_DIR overrides the location, matching the client — so the
// home-directory default must not be consulted when it is set.
func TestDiscoverOAuthToken_ConfigDirOverridesHome(t *testing.T) {
	useCredentialGOOS(t, "linux")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writeCredentialsFile(t, filepath.Join(home, ".claude"), ".credentials.json",
		claudeCredentials("from-home", 0))

	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	writeCredentialsFile(t, configDir, ".credentials.json", claudeCredentials("from-config-dir", 0))

	tok, reason := discoverOAuthToken()
	if reason != "" {
		t.Fatalf("expected success; got reason=%q", reason)
	}
	if tok != "from-config-dir" {
		t.Errorf("token = %q, want from-config-dir", tok)
	}
}

func TestDiscoverOAuthToken_DefaultsToHomeClaude(t *testing.T) {
	useCredentialGOOS(t, "linux")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writeCredentialsFile(t, filepath.Join(home, ".claude"), ".credentials.json",
		claudeCredentials("from-home", 0))

	tok, reason := discoverOAuthToken()
	if reason != "" {
		t.Fatalf("expected success; got reason=%q", reason)
	}
	if tok != "from-home" {
		t.Errorf("token = %q, want from-home", tok)
	}
}

// os.UserConfigDir()/claude — ~/.config/claude on Linux — was one of the
// directories the walk probed, and the client writes nothing there. A
// credential sitting in it is not a credential this reader has found.
func TestDiscoverOAuthToken_UserConfigDirIsNotRead(t *testing.T) {
	useCredentialGOOS(t, "linux")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Resolved AFTER the redirect, and asserted to be inside it: a test must
	// never write a credential into the developer's own configuration.
	ucd, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("no user config dir on this host: %v", err)
	}
	if !strings.HasPrefix(ucd, home) {
		t.Skipf("user config dir %s is not under the redirected home", ucd)
	}
	writeCredentialsFile(t, filepath.Join(ucd, "claude"), ".credentials.json",
		claudeCredentials("from-user-config-dir", 0))

	tok, reason := discoverOAuthToken()
	if tok != "" {
		t.Errorf("token = %q, want empty — the client writes nothing under %s", tok, ucd)
	}
	if want := filepath.Join(home, ".claude", ".credentials.json"); !strings.Contains(reason, want) {
		t.Errorf("reason should name %s and nothing else; got %q", want, reason)
	}
}

// Neither source can be located: the file branch needs a directory, and saying
// so is not the same as saying the file is missing from one.
func TestDiscoverOAuthToken_NoConfigDirAndNoHome(t *testing.T) {
	useCredentialGOOS(t, "linux")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("this host reports a home directory with the environment cleared")
	}

	tok, reason := discoverOAuthToken()
	if tok != "" {
		t.Errorf("token = %q, want empty", tok)
	}
	if !strings.Contains(reason, "CLAUDE_CONFIG_DIR") || !strings.Contains(reason, "home directory") {
		t.Errorf("reason should name both places a directory could come from; got %q", reason)
	}
	if strings.Contains(reason, ".credentials.json") {
		t.Errorf("no path was read, so none may be named; got %q", reason)
	}
}

// The missing-file reason names the one path that was read, and mentions no
// Keychain: the generic "(searched … and macOS Keychain)" message is what made
// a one-character path defect read like an environment problem.
func TestDiscoverOAuthToken_FileMissingNamesThePath(t *testing.T) {
	useCredentialGOOS(t, "linux")
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	tok, reason := discoverOAuthToken()
	if tok != "" {
		t.Errorf("token = %q, want empty", tok)
	}
	want := filepath.Join(dir, ".credentials.json")
	if !strings.Contains(reason, want) {
		t.Errorf("reason should name %s; got %q", want, reason)
	}
	if strings.Contains(strings.ToLower(reason), "keychain") {
		t.Errorf("reason must not mention the Keychain off darwin; got %q", reason)
	}
}

func TestDiscoverOAuthToken_FileUnreadableNamesThePath(t *testing.T) {
	useCredentialGOOS(t, "linux")
	if runtime.GOOS == "windows" {
		t.Skip("mode 0000 does not deny reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 0000 file")
	}
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := writeCredentialsFile(t, dir, ".credentials.json", claudeCredentials("unreadable", 0))
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}

	tok, reason := discoverOAuthToken()
	if tok != "" {
		t.Errorf("token = %q, want empty", tok)
	}
	if !strings.Contains(reason, path) {
		t.Errorf("reason should name %s; got %q", path, reason)
	}
	if strings.Contains(reason, "does not exist") {
		t.Errorf("an unreadable file is not a missing one; got %q", reason)
	}
}

func TestDiscoverOAuthToken_MalformedJSON(t *testing.T) {
	useCredentialGOOS(t, "linux")
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeCredentialsFile(t, dir, ".credentials.json", `{"claudeAiOauth": `)

	tok, reason := discoverOAuthToken()
	if tok != "" {
		t.Errorf("token = %q, want empty", tok)
	}
	if !strings.Contains(reason, "not readable JSON") {
		t.Errorf("reason should say the JSON is unreadable; got %q", reason)
	}
}

// An empty token is a missing token, not an expired one: the two say different
// things to whoever reads the warning.
func TestDiscoverOAuthToken_EmptyToken(t *testing.T) {
	useCredentialGOOS(t, "linux")
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeCredentialsFile(t, dir, ".credentials.json", claudeCredentials("", 0))

	tok, reason := discoverOAuthToken()
	if tok != "" {
		t.Errorf("token = %q, want empty", tok)
	}
	if !strings.Contains(reason, "no claudeAiOauth.accessToken") {
		t.Errorf("reason should name the missing field; got %q", reason)
	}
	if strings.Contains(reason, "expired") {
		t.Errorf("a missing token must not be reported as expired; got %q", reason)
	}
}

func TestDiscoverOAuthToken_ExpiredToken(t *testing.T) {
	useCredentialGOOS(t, "linux")
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	expiry := time.Now().Add(-2 * time.Hour)
	writeCredentialsFile(t, dir, ".credentials.json",
		claudeCredentials("stale-token", expiry.UnixMilli()))

	tok, reason := discoverOAuthToken()
	if tok != "" {
		t.Errorf("token = %q, want empty", tok)
	}
	if !strings.Contains(reason, "expired") {
		t.Errorf("reason should say expired; got %q", reason)
	}
	if want := expiry.Truncate(time.Second).Format(time.RFC3339); !strings.Contains(reason, want) {
		t.Errorf("reason should name the instant %s; got %q", want, reason)
	}
}

// Nothing is inferred from a field that is not there: a credential with no
// expiresAt is returned, and fetchUsage's 401 branch catches it if it is dead.
func TestDiscoverOAuthToken_AbsentExpiryIsNotExpired(t *testing.T) {
	useCredentialGOOS(t, "linux")
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeCredentialsFile(t, dir, ".credentials.json", `{"claudeAiOauth":{"accessToken":"no-expiry"}}`)

	tok, reason := discoverOAuthToken()
	if reason != "" {
		t.Fatalf("expected success; got reason=%q", reason)
	}
	if tok != "no-expiry" {
		t.Errorf("token = %q, want no-expiry", tok)
	}
}

// The defect itself: the undotted name is not one the client writes, so finding
// it there is not finding a credential.
func TestDiscoverOAuthToken_UndottedNameIsNotRead(t *testing.T) {
	useCredentialGOOS(t, "linux")
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeCredentialsFile(t, dir, "credentials.json", claudeCredentials("undotted", 0))

	tok, reason := discoverOAuthToken()
	if tok != "" {
		t.Errorf("token = %q, want empty — credentials.json is not a name the client writes", tok)
	}
	if !strings.Contains(reason, filepath.Join(dir, ".credentials.json")) {
		t.Errorf("reason should name the dotted path; got %q", reason)
	}
}

// No cross-OS fallback: a Keychain that would have answered is not asked.
func TestDiscoverOAuthToken_FileBranchNeverAsksTheKeychain(t *testing.T) {
	useCredentialGOOS(t, "linux")
	stubSecurity(t, claudeCredentials("from-keychain", 0))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	tok, reason := discoverOAuthToken()
	if tok != "" {
		t.Errorf("token = %q, want empty — the Keychain must not be consulted off darwin", tok)
	}
	if reason == "" {
		t.Error("expected a reason naming the file that was read")
	}
}

// The selection is darwin against everything else, not darwin against Linux.
// Windows holds the file too (the issue's table names both), and every other
// file-branch test here pins "linux" — so a reader narrowed to one non-mac name
// would pass all of them and leave Windows reading a Keychain it does not have.
func TestDiscoverOAuthToken_EveryNonDarwinHostReadsTheFile(t *testing.T) {
	for _, goos := range []string{"linux", "windows", "freebsd"} {
		t.Run(goos, func(t *testing.T) {
			useCredentialGOOS(t, goos)
			// A Keychain that WOULD answer, so a host that fell through to it
			// succeeds with the wrong token rather than failing quietly.
			stubSecurity(t, claudeCredentials("from-keychain", 0))
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			writeCredentialsFile(t, dir, ".credentials.json", claudeCredentials("from-file", 0))

			tok, reason := discoverOAuthToken()
			if reason != "" {
				t.Fatalf("expected success; got reason=%q", reason)
			}
			if tok != "from-file" {
				t.Errorf("token = %q, want from-file — %s reads the file, not the Keychain", tok, goos)
			}
		})
	}
}

// The other direction: on darwin the Keychain is the whole source, and a file
// holding a different token is not read.
func TestDiscoverOAuthToken_KeychainIsTheOnlyDarwinSource(t *testing.T) {
	useCredentialGOOS(t, "darwin")
	stubSecurity(t, claudeCredentials("from-keychain", time.Now().Add(time.Hour).UnixMilli()))
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeCredentialsFile(t, dir, ".credentials.json", claudeCredentials("from-file", 0))

	tok, reason := discoverOAuthToken()
	if reason != "" {
		t.Fatalf("expected success; got reason=%q", reason)
	}
	if tok != "from-keychain" {
		t.Errorf("token = %q, want from-keychain", tok)
	}
}

func TestDiscoverOAuthToken_KeychainUnreadable(t *testing.T) {
	useCredentialGOOS(t, "darwin")
	t.Setenv("PATH", t.TempDir()) // no `security` to run
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	tok, reason := discoverOAuthToken()
	if tok != "" {
		t.Errorf("token = %q, want empty", tok)
	}
	if !strings.Contains(reason, "Keychain item") {
		t.Errorf("reason should name the Keychain item; got %q", reason)
	}
	if strings.Contains(reason, ".credentials.json") {
		t.Errorf("reason must not name a file on darwin; got %q", reason)
	}
}

// One decoder serves both sources: the Keychain's malformed, empty and expired
// credentials produce the file branch's reasons, named after the Keychain.
func TestDiscoverOAuthToken_KeychainSharesTheDecoder(t *testing.T) {
	expiry := time.Now().Add(-time.Hour)
	for _, c := range []struct {
		name string
		body string
		want string
	}{
		{"empty token", claudeCredentials("", 0), "no claudeAiOauth.accessToken"},
		{"expired token", claudeCredentials("stale", expiry.UnixMilli()), "expired at " + expiry.Truncate(time.Second).Format(time.RFC3339)},
		{"malformed", `{"claudeAiOauth": `, "not readable JSON"},
	} {
		t.Run(c.name, func(t *testing.T) {
			useCredentialGOOS(t, "darwin")
			stubSecurity(t, c.body)

			tok, reason := discoverOAuthToken()
			if tok != "" {
				t.Errorf("token = %q, want empty", tok)
			}
			if !strings.Contains(reason, c.want) {
				t.Errorf("reason should contain %q; got %q", c.want, reason)
			}
			if !strings.Contains(reason, keychainCredentialsItem) {
				t.Errorf("reason should name the Keychain item; got %q", reason)
			}
		})
	}
}

// Every test above pins credentialGOOS, so nothing else here would notice if
// the seam's own default stopped being this host's OS — and a default stuck at
// "darwin" is #183 again: every Linux host asking a Keychain it does not have,
// reported as an environment problem. The seam-doctrine test
// TestQuotaCache_TestsNeverUseTheRealCredential guards quotaCredential for the
// same reason.
func TestCredentialGOOS_IsThisHostsOS(t *testing.T) {
	if credentialGOOS != runtime.GOOS {
		t.Errorf("credentialGOOS = %q, want %q — the seam must default to the real OS, "+
			"or the source is chosen for a host nobody is on", credentialGOOS, runtime.GOOS)
	}
}

func TestPaceDelay_NoDelayNeeded(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	usage := []windowUsage{{
		Label:    "5h",
		Length:   5 * time.Hour,
		Used:     0.30,
		ResetsAt: now.Add(2*time.Hour + 30*time.Minute), // elapsed=50%
	}}
	targets := paceTargets{FiveHour: 0.90, SevenDay: 0.95}
	d := paceDelay(usage, targets, now)
	if d != 0 {
		t.Errorf("expected no delay; got %v", d)
	}
}

func TestPaceDelay_DelayNeeded5h(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	usage := []windowUsage{{
		Label:    "5h",
		Length:   5 * time.Hour,
		Used:     0.60,
		ResetsAt: now.Add(2*time.Hour + 30*time.Minute), // elapsed=50%
	}}
	targets := paceTargets{FiveHour: 0.90, SevenDay: 0.95}
	d := paceDelay(usage, targets, now)
	// needed_elapsed = 0.60/0.90 = 0.6667; delay = (0.6667 - 0.50) * 5h ≈ 50 min
	if d < 49*time.Minute || d > 51*time.Minute {
		t.Errorf("expected ~50min delay; got %v", d)
	}
}

func TestPaceDelay_BothWindowsTighterWins(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	usage := []windowUsage{
		{
			Label:    "5h",
			Length:   5 * time.Hour,
			Used:     0.50,
			ResetsAt: now.Add(2*time.Hour + 30*time.Minute), // elapsed=50%
		},
		{
			Label:    "7d",
			Length:   7 * 24 * time.Hour,
			Used:     0.80,
			ResetsAt: now.Add(3 * 24 * time.Hour), // elapsed ~57%
		},
	}
	targets := paceTargets{FiveHour: 0.90, SevenDay: 0.95}
	d := paceDelay(usage, targets, now)

	// 5h: needed=0.50/0.90=0.556, elapsed=0.50, delay=(0.056)*5h ≈ 16.7 min
	// 7d: needed=0.80/0.95=0.842, elapsed≈0.571, delay=(0.271)*7d ≈ 45.5h
	// Tighter is 7d (~45.5h)
	if d < 45*time.Hour || d > 46*time.Hour {
		t.Errorf("expected ~45h delay (7d tighter); got %v", d)
	}
}

func TestPaceDelay_TargetZeroDisables(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	usage := []windowUsage{{
		Label:    "5h",
		Length:   5 * time.Hour,
		Used:     0.99,
		ResetsAt: now.Add(2*time.Hour + 30*time.Minute),
	}}
	targets := paceTargets{FiveHour: 0, SevenDay: 0}
	d := paceDelay(usage, targets, now)
	if d != 0 {
		t.Errorf("target 0 should disable pacing; got %v", d)
	}
}

func TestPaceDelay_Target100PacesToRawElapsed(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	usage := []windowUsage{{
		Label:    "5h",
		Length:   5 * time.Hour,
		Used:     0.55,
		ResetsAt: now.Add(2*time.Hour + 30*time.Minute), // elapsed=50%
	}}
	targets := paceTargets{FiveHour: 1.0, SevenDay: 0}
	d := paceDelay(usage, targets, now)
	// needed=0.55/1.0=0.55, delay=(0.55-0.50)*5h=15min
	if d < 14*time.Minute || d > 16*time.Minute {
		t.Errorf("expected ~15min delay at target=100%%; got %v", d)
	}
}

func TestPaceDelay_AbsentUsageSkipped(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	usage := []windowUsage{{
		Label:    "5h",
		Length:   5 * time.Hour,
		Used:     -1, // absent
		ResetsAt: now.Add(2*time.Hour + 30*time.Minute),
	}}
	targets := paceTargets{FiveHour: 0.90, SevenDay: 0.95}
	d := paceDelay(usage, targets, now)
	if d != 0 {
		t.Errorf("absent usage should be skipped; got %v", d)
	}
}

func TestPaceDelay_ZeroLengthSkipped(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	usage := []windowUsage{{
		Label:    "??",
		Length:   0,
		Used:     0.90,
		ResetsAt: now.Add(1 * time.Hour),
	}}
	targets := paceTargets{FiveHour: 0.90, SevenDay: 0.95}
	d := paceDelay(usage, targets, now)
	if d != 0 {
		t.Errorf("zero-length window should be skipped; got %v", d)
	}
}

func TestPaceDelay_ElapsedZeroAnyUsageDelays(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	usage := []windowUsage{{
		Label:    "5h",
		Length:   5 * time.Hour,
		Used:     0.10,
		ResetsAt: now.Add(5 * time.Hour), // elapsed=0%
	}}
	targets := paceTargets{FiveHour: 0.90, SevenDay: 0.95}
	d := paceDelay(usage, targets, now)
	// ceiling = 0.90 * 0 = 0; used=0.10 > 0, so delay is needed.
	// needed=0.10/0.90=0.111; delay=0.111*5h ≈ 33.3 min
	if d < 32*time.Minute || d > 35*time.Minute {
		t.Errorf("expected ~33min delay at elapsed=0; got %v", d)
	}
}

func TestPaceDelay_UsedExceedsTargetAtFullElapsed(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	usage := []windowUsage{{
		Label:    "5h",
		Length:   5 * time.Hour,
		Used:     0.95,
		ResetsAt: now.Add(30 * time.Minute), // elapsed=90%
	}}
	targets := paceTargets{FiveHour: 0.90, SevenDay: 0.95}
	d := paceDelay(usage, targets, now)
	// needed=0.95/0.90=1.056 > 1.0 → delay is full remaining time = 30 min
	if d < 29*time.Minute || d > 31*time.Minute {
		t.Errorf("expected ~30min delay (full remaining); got %v", d)
	}
}

func TestPaceDelay_EmptyUsage(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	targets := paceTargets{FiveHour: 0.90, SevenDay: 0.95}
	d := paceDelay(nil, targets, now)
	if d != 0 {
		t.Errorf("empty usage should return 0; got %v", d)
	}
}

func TestPaceDelay_UnknownLabelSkipped(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	usage := []windowUsage{{
		Label:    "24h",
		Length:   24 * time.Hour,
		Used:     0.99,
		ResetsAt: now.Add(12 * time.Hour), // elapsed=50%
	}}
	targets := paceTargets{FiveHour: 0.90, SevenDay: 0.95}
	d := paceDelay(usage, targets, now)
	if d != 0 {
		t.Errorf("unknown label should be skipped; got %v", d)
	}
}

func TestPaceDelay_ExpiredWindowNoDelay(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	usage := []windowUsage{{
		Label:    "5h",
		Length:   5 * time.Hour,
		Used:     0.80,
		ResetsAt: now.Add(-10 * time.Minute), // already expired
	}}
	targets := paceTargets{FiveHour: 0.90, SevenDay: 0.95}
	d := paceDelay(usage, targets, now)
	// elapsed clamped to 1.0; ceiling = 0.90 * 1.0 = 0.90; Used 0.80 <= 0.90 → no delay.
	if d != 0 {
		t.Errorf("expired window with Used < target should not delay; got %v", d)
	}
}

func TestPaceDelay_ExactlyAtCeilingNoDelay(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	// Set up so that Used == target * elapsed exactly.
	// elapsed = 50%, target = 0.90 → ceiling = 0.45; Used = 0.45.
	usage := []windowUsage{{
		Label:    "5h",
		Length:   5 * time.Hour,
		Used:     0.45,
		ResetsAt: now.Add(2*time.Hour + 30*time.Minute), // elapsed=50%
	}}
	targets := paceTargets{FiveHour: 0.90, SevenDay: 0.95}
	d := paceDelay(usage, targets, now)
	if d != 0 {
		t.Errorf("Used exactly at ceiling should not delay; got %v", d)
	}
}

func TestPaceDelay_SevenDayOnly(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	usage := []windowUsage{{
		Label:    "7d",
		Length:   7 * 24 * time.Hour,
		Used:     0.60,
		ResetsAt: now.Add(3 * 24 * time.Hour), // elapsed ≈ 57.1%
	}}
	targets := paceTargets{FiveHour: 0.90, SevenDay: 0.95}
	d := paceDelay(usage, targets, now)
	// ceiling = 0.95 * 0.571 ≈ 0.543; Used 0.60 > 0.543 → delay.
	// needed = 0.60/0.95 ≈ 0.6316; delay = (0.6316 - 0.5714) * 7d ≈ 10.2h
	if d < 9*time.Hour || d > 11*time.Hour {
		t.Errorf("expected ~10h delay for 7d-only; got %v", d)
	}
}

func TestWindowTarget(t *testing.T) {
	targets := paceTargets{FiveHour: 0.90, SevenDay: 0.95}
	if got := windowTarget("5h", targets); got != 0.90 {
		t.Errorf("5h target = %v, want 0.90", got)
	}
	if got := windowTarget("7d", targets); got != 0.95 {
		t.Errorf("7d target = %v, want 0.95", got)
	}
	if got := windowTarget("unknown", targets); got != 0 {
		t.Errorf("unknown target = %v, want 0", got)
	}
}

func TestParseUsageResponse_MalformedJSON(t *testing.T) {
	_, err := parseUsageResponse([]byte(`not json`))
	if err == nil {
		t.Error("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "cannot parse") {
		t.Errorf("error should mention 'cannot parse'; got %q", err)
	}
}

func TestParseUsageResponse_OnlySevenDay(t *testing.T) {
	body := `{
		"seven_day": {
			"utilization": 62.0,
			"resets_at": "2026-09-04T16:00:00Z"
		}
	}`
	result, err := parseUsageResponse([]byte(body))
	if err != nil {
		t.Fatalf("parseUsageResponse: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 window; got %d", len(result))
	}
	if result[0].Label != "7d" {
		t.Errorf("label = %q, want 7d", result[0].Label)
	}
	if math.Abs(result[0].Used-0.62) > 0.001 {
		t.Errorf("used = %v, want 0.62", result[0].Used)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"absent", "", 0},
		{"delta seconds", "120", 2 * time.Minute},
		{"delta seconds padded", "  45  ", 45 * time.Second},
		{"http date", now.Add(90 * time.Second).UTC().Format(http.TimeFormat), 90 * time.Second},
		{"http date in the past", now.Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
		{"garbage", "soon please", 0},
		{"negative", "-30", 0},
		{"zero", "0", 0},
		{"capped", "604800", quotaRetryAfterCap},
		{"capped past a Duration's range", "9223372036854775807", quotaRetryAfterCap},
		// The date form takes the same cap as the delta form: a header naming a
		// day next week must not take pacing off the air until then.
		{"http date past the cap", now.Add(7 * 24 * time.Hour).UTC().Format(http.TimeFormat), quotaRetryAfterCap},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseRetryAfter(tt.header, now); got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

func TestFetchUsage_RefusalCarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := fetchUsage(srv.URL, "tok")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if got := err.Error(); got != "rejected — HTTP 429" {
		t.Errorf("message = %q, want the unchanged operator-facing text", got)
	}
	var refusal *quotaRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error is %T, want *quotaRefusal", err)
	}
	if refusal.retryAfter != time.Minute {
		t.Errorf("retryAfter = %v, want 1m", refusal.retryAfter)
	}
	if got := quotaBackoffFor(err); got != time.Minute {
		t.Errorf("quotaBackoffFor = %v, want the header's 1m", got)
	}
}

func TestFetchUsage_RefusalWithoutRetryAfterTakesTheDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := fetchUsage(srv.URL, "tok")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "credentials may be expired") {
		t.Errorf("401 should still name the likely cause; got %q", err)
	}
	if got := quotaBackoffFor(err); got != quotaFailureBackoff {
		t.Errorf("quotaBackoffFor = %v, want the default %v", got, quotaFailureBackoff)
	}
}

func TestFetchUsage_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		fmt.Fprint(w, `{"five_hour":{"utilization":47,"resets_at":"2026-09-01T14:24:00Z"}}`)
	}))
	defer srv.Close()

	usage, err := fetchUsage(srv.URL, "tok")
	if err != nil {
		t.Fatalf("fetchUsage: %v", err)
	}
	if len(usage) != 1 || math.Abs(usage[0].Used-0.47) > 0.001 {
		t.Errorf("usage = %+v, want one window at 0.47", usage)
	}
}

func TestWindowUsage_RoundTripsThroughJSON(t *testing.T) {
	// The cache stores these, so the tags have to survive a round trip.
	want := windowUsage{
		Label:    "7d",
		Length:   7 * 24 * time.Hour,
		Used:     0.31,
		ResetsAt: time.Date(2026, 9, 4, 16, 0, 0, 0, time.UTC),
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got windowUsage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Label != want.Label || got.Length != want.Length || got.Used != want.Used || !got.ResetsAt.Equal(want.ResetsAt) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestDiscoverAPIBase_FromSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("PATH", t.TempDir()) // prevent claude binary fallback

	settings := `{"apiBaseUrl": "https://custom.example.com/v1/"}`
	if err := os.WriteFile(dir+"/settings.json", []byte(settings), 0644); err != nil {
		t.Fatal(err)
	}

	base, reason := discoverAPIBase()
	if reason != "" {
		t.Fatalf("expected success; got reason=%q", reason)
	}
	// Trailing slash should be stripped.
	if base != "https://custom.example.com/v1" {
		t.Errorf("base = %q, want https://custom.example.com/v1", base)
	}
}

// meteringBackend is a fake.Orchestrator that also reports what it spent at its
// outside seam — the cli.ServiceMeter half of the hook.
type meteringBackend struct {
	*fake.Orchestrator
	line string
}

func (m meteringBackend) ServiceSpend() string { return m.line }

// docs/resolution.md § One seam per outside service requires a seam to meter.
// reportSpend is where the metering becomes visible, and a cost nobody sees is
// a cost nobody fixes.
func TestReportSpend_PrintsTheOrchestratorsSeamBesideTheQuota(t *testing.T) {
	useTempQuotaCache(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())

	var buf bytes.Buffer
	app := &App{Err: &buf, Orchestrator: meteringBackend{fake.New(), "github: 41 request(s), 12 served from cache"}}
	app.reportSpend()
	if !strings.Contains(buf.String(), "github: 41 request(s)") {
		t.Errorf("the seam's own meter was not printed: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "quota:") {
		t.Errorf("the agent's quota stopped being printed: %q", buf.String())
	}

	// An orchestrator that meters nothing prints no line, and one that does not
	// implement the hook at all is not asked.
	for name, o := range map[string]flow.Orchestrator{
		"nothing to report": meteringBackend{fake.New(), ""},
		"no hook":           fake.New(),
	} {
		t.Run(name, func(t *testing.T) {
			var b2 bytes.Buffer
			(&App{Err: &b2, Orchestrator: o}).reportSpend()
			if strings.Contains(b2.String(), "github:") {
				t.Errorf("a line was printed for %s: %q", name, b2.String())
			}
		})
	}
}
