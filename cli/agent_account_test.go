package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// A machine may drive more than one agent account, so figures printed without
// one say what is being spent without saying whose allowance is spending it.
func TestReportQuota_NamesTheAgentAccountAboveTheFigures(t *testing.T) {
	useTempQuotaCache(t)
	installFetch(t, func() ([]windowUsage, error) { return usageAt(0.5), nil })
	useStubAgentAccount(t, agentAccountRecord{Id: "uuid-1", Email: "pat@example.com"}, nil)

	got := captureReportQuota(t)
	account := strings.Index(got, "agent account: pat@example.com (uuid-1)")
	window := strings.Index(got, "5h")
	if account < 0 {
		t.Fatalf("the account paying for the run is not named by identifier and display name; got %q", got)
	}
	if window < 0 || account > window {
		t.Errorf("the account must be named above the figures it pays for; got %q", got)
	}
}

// An account this host cannot name is REPORTED as unidentified, naming what
// was looked for. Dropping the line would leave the operator unable to tell a
// named account from an unnamed one, and a park written now is scoped to
// nothing.
func TestReportQuota_ReportsAnUnidentifiedAccountRatherThanDroppingTheLine(t *testing.T) {
	useTempQuotaCache(t)
	installFetch(t, func() ([]windowUsage, error) { return usageAt(0.5), nil })
	useStubAgentAccount(t, agentAccountRecord{},
		errors.New("no .claude.json naming oauthAccount.accountUuid (searched /nowhere)"))

	got := captureReportQuota(t)
	if !strings.Contains(got, "agent account: unidentified — ") {
		t.Fatalf("an account nothing names must be reported as unidentified; got %q", got)
	}
	if !strings.Contains(got, "oauthAccount.accountUuid") {
		t.Errorf("the line must name the field looked for; got %q", got)
	}
}

// A record with no readable name prints the identifier alone: the display name
// is optional, the identity is not.
func TestAgentAccountRecord_DisplayFallsBackToTheIdentifierAlone(t *testing.T) {
	if got := (agentAccountRecord{Id: "uuid-1"}).Display(); got != "uuid-1" {
		t.Errorf("Display() = %q, want the bare identifier", got)
	}
}

// isolateAgentAccountRead points every directory the reader consults at
// throwaway ones — $HOME included, which no CLAUDE_CONFIG_DIR can shadow — and
// restores the real reader for the duration of the test. Returns the config
// directory.
func isolateAgentAccountRead(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", t.TempDir())
	prev := agentAccount
	agentAccount = readAgentAccount
	t.Cleanup(func() { agentAccount = prev })
	return dir
}

func writeJSON(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// The identifier is the substrate-issued uuid, read from the stated field. The
// e-mail comes back too, for a person to read, and is not the identity.
func TestReadAgentAccount_ReadsTheStatedFieldForTheIdentifier(t *testing.T) {
	dir := isolateAgentAccountRead(t)
	writeJSON(t, filepath.Join(dir, ".claude.json"),
		`{"oauthAccount":{"accountUuid":"uuid-1","emailAddress":"pat@example.com"}}`)

	rec, err := readAgentAccount()
	if err != nil {
		t.Fatalf("readAgentAccount: %v", err)
	}
	if rec.Id != flow.AgentAccountId("uuid-1") {
		t.Errorf("Id = %q, want the accountUuid", rec.Id)
	}
	if rec.Email != "pat@example.com" {
		t.Errorf("Email = %q, want the emailAddress carried for display", rec.Email)
	}
}

// The case the old reader got wrong, and the reason this one exists: it
// returned the e-mail as though it were the identity, so one account read as
// an e-mail on one host and a uuid on another — and each answer was
// individually plausible, so nothing said so.
func TestReadAgentAccount_AnEmailAloneIsNotAnIdentity(t *testing.T) {
	dir := isolateAgentAccountRead(t)
	writeJSON(t, filepath.Join(dir, ".claude.json"),
		`{"oauthAccount":{"emailAddress":"pat@example.com"}}`)

	rec, err := readAgentAccount()
	if err == nil {
		t.Fatalf("readAgentAccount() = %+v, want a refusal: a display name is not an identity", rec)
	}
	if rec.Id != "" {
		t.Errorf("Id = %q, want empty", rec.Id)
	}
	if strings.Contains(err.Error(), "pat@example.com") {
		t.Errorf("the reason must not offer the e-mail as the answer; got %q", err)
	}
}

// Each way of not finding the account gets its own reason: an operator has to
// know whether to install the client, log it in, or look at a file that is
// there and wrong. And none of them ever answers with an identifier.
func TestReadAgentAccount_DistinctReasonPerFailure(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{name: "absent", setup: func(*testing.T, string) {}},
		{
			// A directory where the file belongs, rather than a mode of 0:
			// the test must fail the same way for root, who may read a file
			// whatever its permissions say.
			name: "unreadable",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
		},
		{name: "malformed", setup: func(t *testing.T, path string) { writeJSON(t, path, "not json") }},
		{name: "no oauthAccount object", setup: func(t *testing.T, path string) { writeJSON(t, path, `{"numStartups":4}`) }},
		{
			name: "empty accountUuid",
			setup: func(t *testing.T, path string) {
				writeJSON(t, path, `{"oauthAccount":{"emailAddress":"pat@example.com"}}`)
			},
		},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolateAgentAccountRead(t)
			tc.setup(t, filepath.Join(dir, ".claude.json"))

			rec, err := readAgentAccount()
			if err == nil {
				t.Fatalf("readAgentAccount() = %+v, want a refusal", rec)
			}
			if rec.Id != "" {
				t.Fatalf("Id = %q, want empty: an unidentified account is not an identity", rec.Id)
			}
			if other, dup := seen[err.Error()]; dup {
				t.Errorf("%q reports the same reason as %q: %q", tc.name, other, err)
			}
			seen[err.Error()] = tc.name
		})
	}
	if len(seen) != len(cases) {
		t.Errorf("%d failure modes produced %d distinct reasons", len(cases), len(seen))
	}
}

// A reader that cannot establish the identity never falls back to something
// that happens to be at hand. A synthesized identity is worse than none: it
// compares equal to itself and will eventually compare equal to something
// else.
func TestReadAgentAccount_NeverSynthesizesFromWhatIsAtHand(t *testing.T) {
	dir := isolateAgentAccountRead(t)
	writeJSON(t, filepath.Join(dir, ".claude.json"),
		`{"claudeAiOauth":{"accessToken":"a-secret-token"},"oauthAccount":{"emailAddress":"pat@example.com"}}`)
	host, _ := os.Hostname()

	rec, err := readAgentAccount()
	if err == nil {
		t.Fatalf("readAgentAccount() = %+v, want a refusal", rec)
	}
	for _, forbidden := range []string{"a-secret-token", host} {
		if forbidden == "" {
			continue
		}
		if strings.Contains(string(rec.Id)+err.Error(), forbidden) {
			t.Errorf("the answer offers %q as the account: %q / %q", forbidden, rec.Id, err)
		}
	}
	// agentAccountID answers the empty identifier rather than a stand-in, and
	// a park scoped to nothing is what a caller then writes.
	if got := agentAccountID(); got != "" {
		t.Errorf("agentAccountID() = %q, want empty when the account is unidentified", got)
	}
}

// The configured directory is read FIRST. A caller pointed at one by
// CLAUDE_CONFIG_DIR is answered from there — a read that fell back to $HOME
// first would name whichever account that machine's home directory happens to
// hold, which on a host driving more than one agent account is the wrong one.
func TestReadAgentAccount_PrefersTheConfiguredDirectoryOverHome(t *testing.T) {
	dir := isolateAgentAccountRead(t)
	writeJSON(t, filepath.Join(dir, ".claude.json"), `{"oauthAccount":{"accountUuid":"uuid-configured"}}`)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	writeJSON(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"accountUuid":"uuid-home"}}`)

	rec, err := readAgentAccount()
	if err != nil {
		t.Fatalf("readAgentAccount: %v", err)
	}
	if rec.Id != flow.AgentAccountId("uuid-configured") {
		t.Errorf("Id = %q, want the configured directory's account", rec.Id)
	}
}

// An ABSENT candidate says nothing about this host — a machine has one client
// configuration and the rest of the list is where it might have been — so the
// search goes on to the one that is there.
func TestReadAgentAccount_LooksPastACandidateThatIsNotThere(t *testing.T) {
	isolateAgentAccountRead(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	writeJSON(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"accountUuid":"uuid-home"}}`)

	rec, err := readAgentAccount()
	if err != nil {
		t.Fatalf("readAgentAccount: %v", err)
	}
	if rec.Id != flow.AgentAccountId("uuid-home") {
		t.Errorf("Id = %q, want the account the candidate that exists names", rec.Id)
	}
}

// A candidate that is there and does NOT name the account is not the end of
// the search either. A host spends as one account, so a client directory
// standing empty — installed and never logged in — says where the account is
// not, and the reader goes on and answers with the one that is there. It is
// the account the substrate will spend, wherever its configuration sits.
func TestReadAgentAccount_LooksPastACandidateThatNamesNoAccount(t *testing.T) {
	dir := isolateAgentAccountRead(t)
	writeJSON(t, filepath.Join(dir, ".claude.json"), `{"numStartups":4}`)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	writeJSON(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"accountUuid":"uuid-home"}}`)

	rec, err := readAgentAccount()
	if err != nil {
		t.Fatalf("readAgentAccount: %v", err)
	}
	if rec.Id != flow.AgentAccountId("uuid-home") {
		t.Errorf("Id = %q, want the account the candidate that names one holds", rec.Id)
	}
}

// When NOTHING names the account, the reason the operator is given is the
// highest-priority candidate's. The reasons differ in what they ask for —
// install the client, log it in, look at a file that is there and wrong — so
// reporting the last candidate's instead would point a person at $HOME over
// the directory they configured, and at the wrong remedy.
func TestReadAgentAccount_ReportsTheHighestPriorityCandidatesReason(t *testing.T) {
	dir := isolateAgentAccountRead(t)
	writeJSON(t, filepath.Join(dir, ".claude.json"), "not json")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	writeJSON(t, filepath.Join(home, ".claude.json"), `{"numStartups":4}`)

	rec, err := readAgentAccount()
	if err == nil {
		t.Fatalf("readAgentAccount() = %+v, want a refusal: nothing names the account", rec)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, ".claude.json")) {
		t.Errorf("reason = %q, want the configured directory's candidate", err)
	}
	if strings.Contains(err.Error(), filepath.Join(home, ".claude.json")) {
		t.Errorf("reason = %q, want the configured directory's candidate, not $HOME's", err)
	}
}
