package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A machine may drive more than one agent account, so figures printed without
// one say what is being spent without saying whose allowance is spending it.
func TestReportQuota_NamesTheAgentAccountAboveTheFigures(t *testing.T) {
	useTempQuotaCache(t)
	installFetch(t, func() ([]windowUsage, error) { return usageAt(0.5), nil })
	useStubAgentAccount(t, "pat@example.com")

	got := captureReportQuota(t)
	account := strings.Index(got, "agent account: pat@example.com")
	window := strings.Index(got, "5h")
	if account < 0 {
		t.Fatalf("the account paying for the run is not named; got %q", got)
	}
	if window < 0 || account > window {
		t.Errorf("the account must be named above the figures it pays for; got %q", got)
	}
}

// An account nothing on disk names drops the line rather than printing a guess
// — and rather than printing an empty one.
func TestReportQuota_OmitsTheAgentAccountWhenNothingNamesIt(t *testing.T) {
	useTempQuotaCache(t)
	installFetch(t, func() ([]windowUsage, error) { return usageAt(0.5), nil })
	useStubAgentAccount(t, "")

	if got := captureReportQuota(t); strings.Contains(got, "agent account") {
		t.Errorf("an unknown account must print no line at all; got %q", got)
	}
}

// isolateAgentAccountDiscovery points every directory discoverAgentAccount
// searches at throwaway ones — $HOME included, which no CLAUDE_CONFIG_DIR can
// shadow — and restores the real discovery for the duration of the test.
// Returns the config directory.
func isolateAgentAccountDiscovery(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", t.TempDir())
	prev := agentAccount
	agentAccount = discoverAgentAccount
	t.Cleanup(func() { agentAccount = prev })
	return dir
}

func writeJSON(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// The e-mail first: it is what an operator recognises.
func TestDiscoverAgentAccount_ReadsTheOAuthAccountsEmail(t *testing.T) {
	dir := isolateAgentAccountDiscovery(t)
	writeJSON(t, filepath.Join(dir, ".claude.json"),
		`{"oauthAccount":{"accountUuid":"uuid-1","emailAddress":"pat@example.com"}}`)

	if got := discoverAgentAccount(); got != "pat@example.com" {
		t.Errorf("discoverAgentAccount() = %q, want the account's e-mail", got)
	}
}

// The account id is the fallback: it names the account exactly even when
// nothing human-readable is stored.
func TestDiscoverAgentAccount_FallsBackToTheAccountId(t *testing.T) {
	dir := isolateAgentAccountDiscovery(t)
	writeJSON(t, filepath.Join(dir, ".claude.json"), `{"oauthAccount":{"accountUuid":"uuid-1"}}`)

	if got := discoverAgentAccount(); got != "uuid-1" {
		t.Errorf("discoverAgentAccount() = %q, want the account id", got)
	}
}

// An OAuth object carrying no account at all — the credentials file has one —
// must not answer for every later candidate: the search goes on.
func TestDiscoverAgentAccount_LooksPastAnOAuthObjectWithNoAccount(t *testing.T) {
	dir := isolateAgentAccountDiscovery(t)
	writeJSON(t, filepath.Join(dir, ".claude.json"), `{"claudeAiOauth":{"accessToken":"a-token"}}`)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	writeJSON(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"emailAddress":"pat@example.com"}}`)

	if got := discoverAgentAccount(); got != "pat@example.com" {
		t.Errorf("discoverAgentAccount() = %q, want the account the later file names", got)
	}
}

// Nothing readable says, so nothing is claimed. Discovery reads configuration
// and stops: no subprocess, no network, no guess.
func TestDiscoverAgentAccount_IsEmptyWhenNothingOnDiskSaysSo(t *testing.T) {
	isolateAgentAccountDiscovery(t)

	if got := discoverAgentAccount(); got != "" {
		t.Errorf("discoverAgentAccount() = %q, want empty when nothing on disk names an account", got)
	}
}

// A file that is not JSON at all is skipped rather than taken as an answer.
func TestDiscoverAgentAccount_SkipsAnUnreadableFile(t *testing.T) {
	dir := isolateAgentAccountDiscovery(t)
	writeJSON(t, filepath.Join(dir, ".claude.json"), "not json")

	if got := discoverAgentAccount(); got != "" {
		t.Errorf("discoverAgentAccount() = %q, want empty", got)
	}
}

// The configured directory is searched FIRST. A caller pointed at one by
// CLAUDE_CONFIG_DIR is answered from there — a search that fell back to $HOME
// first would name whichever account that machine's home directory happens to
// hold, which on a host driving more than one agent account is the wrong one.
func TestDiscoverAgentAccount_PrefersTheConfiguredDirectoryOverHome(t *testing.T) {
	dir := isolateAgentAccountDiscovery(t)
	writeJSON(t, filepath.Join(dir, ".claude.json"), `{"oauthAccount":{"emailAddress":"pat@example.com"}}`)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	writeJSON(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"emailAddress":"someone-else@example.com"}}`)

	if got := discoverAgentAccount(); got != "pat@example.com" {
		t.Errorf("discoverAgentAccount() = %q, want the configured directory's account", got)
	}
}
