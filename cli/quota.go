package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/promise-language/flow"
)

// readQuota fetches subscription window usage. Returns the windows on success,
// or an error whose message is suitable for display. Never blocks the caller on
// failure — the error is informational.
func readQuota() ([]windowUsage, error) {
	token, credErr := discoverOAuthToken()
	if credErr != "" {
		return nil, fmt.Errorf("%s", credErr)
	}

	apiBase, baseErr := discoverAPIBase()
	if baseErr != "" {
		return nil, fmt.Errorf("%s", baseErr)
	}

	return fetchUsage(apiBase, token)
}

// ServiceMeter is the optional hook an Orchestrator may implement to say what
// it spent at its outside seam — the cli.Doctor pattern, for the other end of
// the same question `reportQuota` answers about the agent.
//
// The answer is a rendered LINE rather than a count and a breakdown, because
// units are the backend's: one orchestrator counts HTTP requests against a rate
// limit, another counts nothing at all, and a struct here would make this
// package know which. An empty string means nothing to report, and no line is
// printed.
//
// docs/resolution.md § One seam per outside service requires a seam to meter.
// This is where the metering becomes visible, because a cost nobody sees is a
// cost nobody fixes.
type ServiceMeter interface {
	ServiceSpend() string
}

// reportSpend prints what the run has cost so far: the agent's subscription
// windows, and the orchestrator's own seam if it meters one.
//
// Together, at every terminal outcome, because they are two halves of one
// question. Splitting the call sites is how one of them would stop being
// printed on a path somebody added later.
func (app *App) reportSpend() {
	reportQuota(app.Err)
	if m, ok := app.Orchestrator.(ServiceMeter); ok {
		if line := m.ServiceSpend(); line != "" {
			fmt.Fprintln(app.Err, line)
		}
	}
}

// reportQuota prints Claude subscription window utilisation to w.
// Failure never blocks the run — prints the reason and returns.
//
// It reads through the machine-wide cache, which is what makes the seven
// display call sites free: `resolve` prints the block at startup and again on
// every terminal outcome, and none of those prints costs a request now.
//
// A reading served past its refresh interval because the endpoint is refusing
// gets one extra line. Without it the change would newly print stale figures as
// if they were current and swallow the "rejected — HTTP 429" diagnostic
// operators read today; the numbers are still the best estimate available,
// which is why they are printed at all.
func reportQuota(w io.Writer) {
	r, err := quotaNow()
	if err != nil {
		fmt.Fprintf(w, "quota: %s\n", err)
		return
	}

	// Whose allowance is being spent, above the figures spending it. A machine
	// may drive more than one agent account, so windows printed without one say
	// what is being spent without saying whose it is (docs/cli.md § The
	// announcement names the run's standing). It is reported HERE and not with
	// the run's standing deliberately: it is detected for no capability, backs
	// no role, and paying for a run is a different question from being
	// permitted to perform it.
	//
	// An account this host cannot name is REPORTED as unidentified, with what
	// was looked for and where — not dropped. The line is the one place an
	// operator learns that a park written now will be scoped to nothing, and a
	// missing line says nothing at all (docs/environment.md § It is never
	// synthesized).
	if rec, err := agentAccount(); err == nil {
		fmt.Fprintf(w, "agent account: %s\n", rec.Display())
	} else {
		fmt.Fprintf(w, "agent account: unidentified — %s\n", err)
	}

	now := time.Now()
	for _, win := range r.Usage {
		printWindow(w, win, now)
	}
	if r.Failure != "" {
		fmt.Fprintf(w, "quota: figures are %s old — refresh failing: %s\n",
			formatDurationCompact(now.Sub(r.ReadAt)), r.Failure)
	}
}

// windowUsage is the parsed response for one subscription window. It is also
// what the machine-wide cache stores, hence the tags.
type windowUsage struct {
	Label    string        `json:"label"` // "5h" or "7d"
	Length   time.Duration `json:"length"`
	Used     float64       `json:"used"`      // fraction [0,1], or -1 if absent
	ResetsAt time.Time     `json:"resets_at"` // when the window resets
}

func printWindow(w io.Writer, win windowUsage, now time.Time) {
	usedPct := "--%"
	if win.Used >= 0 {
		usedPct = fmt.Sprintf("%d%% used", int(math.Round(win.Used*100)))
	}

	var elapsedPct string
	if win.Length <= 0 {
		elapsedPct = "--% of window elapsed"
	} else {
		elapsed := clampFraction(1.0 - float64(win.ResetsAt.Sub(now))/float64(win.Length))
		elapsedPct = fmt.Sprintf("%d%% of window elapsed", int(math.Round(elapsed*100)))
	}

	remaining := win.ResetsAt.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	resetStr := "resets in " + formatDurationCompact(remaining)

	fmt.Fprintf(w, "%-3s %s · %s · %s\n", win.Label, usedPct, elapsedPct, resetStr)
}

// clampFraction clamps v to [0, 1].
func clampFraction(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// claudeCredentialsFile is the name the client writes, dot-prefixed. The
// undotted "credentials.json" is not a name it writes anywhere: reading it is
// what made pacing silently dead on every non-mac host (#183).
const claudeCredentialsFile = ".credentials.json"

// keychainCredentialsItem is the login Keychain item the client writes on macOS.
const keychainCredentialsItem = "Claude Code-credentials"

// credentialGOOS says which of the two credential sources this host holds. A
// package var rather than runtime.GOOS read inline — the quotaCredential
// pattern quota_cache.go documents — so both branches are exercisable wherever
// the tests run, and for the same reason deliberately NOT an environment
// variable.
var credentialGOOS = runtime.GOOS

// discoverOAuthToken reads the Claude OAuth token from the one place the
// installed client writes it on this OS. Returns (token, "") on success or
// ("", reason) on failure, with a distinct reason for each failure mode.
//
// The source is chosen BY OS and there is exactly one per OS: on macOS the
// login Keychain, where nothing lands on disk, and on every other host the file
// <config dir>/.credentials.json. There is no fallback between them in either
// direction, because a walk across candidate locations is precisely what hid
// this reader's defect: it reached the right directory, opened a name the client
// does not write, fell through to a `security` that does not exist off a Mac,
// and reported a generic "nothing found" that reads like an environment problem
// rather than a defect here.
func discoverOAuthToken() (string, string) {
	if credentialGOOS == "darwin" {
		return keychainOAuthToken()
	}
	return fileOAuthToken()
}

// claudeCredentialsPath is the one file the client writes credentials to:
// $CLAUDE_CONFIG_DIR when set, else $HOME/.claude, joined with the name above.
// Returns ("", reason) when neither is available.
//
// Called only from the file branch. The Keychain needs no directory, so a Mac
// with no home directory is not answered with a complaint about a path it would
// never have read.
func claudeCredentialsPath() (string, string) {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "no Claude credentials — neither $CLAUDE_CONFIG_DIR nor a home directory is set"
		}
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, claudeCredentialsFile), ""
}

// fileOAuthToken reads that one path, and nothing else.
func fileOAuthToken() (string, string) {
	path, reason := claudeCredentialsPath()
	if reason != "" {
		return "", reason
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Sprintf("no Claude credentials — %s does not exist (run claude to sign in)", path)
		}
		return "", fmt.Sprintf("no Claude credentials — cannot read %s: %v", path, err)
	}
	return parseClaudeCredentials(data, path, time.Now())
}

// keychainOAuthToken reads the login Keychain item, which is the whole of the
// credential location on macOS.
func keychainOAuthToken() (string, string) {
	out, err := exec.Command("security", "find-generic-password",
		"-s", keychainCredentialsItem, "-w").Output()
	if err != nil {
		return "", fmt.Sprintf("no Claude credentials — cannot read Keychain item %q: %v",
			keychainCredentialsItem, err)
	}
	return parseClaudeCredentials(out, "Keychain item "+keychainCredentialsItem, time.Now())
}

// parseClaudeCredentials decodes the JSON both sources hold — {"claudeAiOauth":
// {"accessToken": "...", "expiresAt": <unix ms>}} — and answers either the token
// or the one reason the source it was read from did not yield one. source names
// that place, so no failure has to be traced back to which branch produced it.
//
// The fields are stated, not discovered. A reader that scans for a key that
// looks like it might name the token, and takes the first field that looks like
// it might be one, returns a different answer on two hosts holding the same
// account, and the failure is silent because each answer is individually
// plausible (docs/environment.md § The agent account is a scope).
func parseClaudeCredentials(data []byte, source string, now time.Time) (string, string) {
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"` // unix milliseconds; 0 when absent
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Sprintf("Claude credentials in %s are not readable JSON: %v", source, err)
	}
	oauth := creds.ClaudeAiOauth
	if oauth.AccessToken == "" {
		return "", fmt.Sprintf("Claude credentials in %s carry no claudeAiOauth.accessToken", source)
	}
	// An absent or zero expiresAt is unknown, not expired: nothing is inferred
	// from a missing field. fetchUsage's 401/403 branch still catches a token
	// that died without saying so here.
	if oauth.ExpiresAt > 0 {
		expiry := time.UnixMilli(oauth.ExpiresAt)
		if now.After(expiry) {
			return "", fmt.Sprintf("Claude credentials in %s expired at %s — re-run claude to refresh",
				source, expiry.Format(time.RFC3339))
		}
	}
	return oauth.AccessToken, ""
}

// agentAccount is the seam this file's account reader is tested through,
// alongside quotaFetch and quotaCacheDir. A test must never read the
// developer's own account: redirected in TestMain, so a test added later
// cannot forget to.
var agentAccount = readAgentAccount

// agentAccountRecord is what the client's configuration says about the account
// the substrate spends as: the identifier everything is keyed by, and the
// display name it is read by.
//
// The two are separate fields because they are separate kinds of thing
// (docs/environment.md § The account is identified by the stable identifier).
// Id is what a park is scoped to and what two hosts compare; Email is for a
// person reading a report and is never compared, never published, and never
// substituted for the Id when the Id is missing.
type agentAccountRecord struct {
	Id    flow.AgentAccountId
	Email string
}

// Display renders the record for a person: the readable name with the
// identifier it stands for, or the identifier alone when the configuration
// carries no readable name. The identifier is always present — a record
// without one is never returned.
func (r agentAccountRecord) Display() string {
	if r.Email == "" {
		return string(r.Id)
	}
	return fmt.Sprintf("%s (%s)", r.Email, r.Id)
}

// The stated place and the stated fields. Named constants because the whole
// point is that they ARE stated: a reader that scans for a key that looks like
// it might name an account, and takes the first field that looks like it might
// be one, answers an e-mail on one host and a UUID on another for the same
// account — and the divergence is silent, because each answer is individually
// plausible (docs/environment.md § It is read from a stated field in a stated
// place).
const (
	agentAccountFile    = ".claude.json"
	agentAccountObject  = "oauthAccount"
	agentAccountIdField = "accountUuid"
)

// errAgentAccountFileAbsent marks the one failure that is not an answer about
// this host: the candidate simply is not there. It is skipped so a later
// candidate can answer, and it is never what the caller is told when some
// other candidate had something to say.
var errAgentAccountFileAbsent = errors.New("no such file")

// readAgentAccount names the account the agent substrate spends as, read from
// the stated field in the stated place: the client's .claude.json, its
// oauthAccount object, its accountUuid.
//
// It returns a record or an ERROR, never a synthesized identifier. A reader
// that cannot establish the identity says so and does not fall back to a host
// name, a configuration path, a token, or the e-mail address sitting next to
// the field it wanted — an unidentified account is not an identity and must
// never be used as one.
//
// Nothing here is pinned beyond those names, and nothing is asked of a
// subprocess or the network: what cannot be learned by reading is not learned
// here.
func readAgentAccount() (agentAccountRecord, error) {
	paths := agentAccountFiles()
	var firstFailure error
	for _, path := range paths {
		rec, err := readAgentAccountFile(path)
		if err == nil {
			return rec, nil
		}
		// A candidate that answers nothing — absent, or there and naming no
		// account — says only that this is not where the configuration is: a
		// host spends as ONE account, so the search goes on and the account it
		// finds is the account. What a non-absent candidate does decide is the
		// REASON, when no candidate answers at all: the highest-priority one's
		// is what the operator is told, so a person is pointed at the
		// directory they configured rather than at $HOME.
		if errors.Is(err, errAgentAccountFileAbsent) {
			continue
		}
		if firstFailure == nil {
			firstFailure = err
		}
	}
	if firstFailure != nil {
		return agentAccountRecord{}, firstFailure
	}
	return agentAccountRecord{}, fmt.Errorf("no %s naming %s.%s (searched %s)",
		agentAccountFile, agentAccountObject, agentAccountIdField, strings.Join(paths, ", "))
}

// readAgentAccountFile reads one candidate. Each way of not finding the
// account gets its OWN reason: an operator reading "unidentified" has to know
// whether to install the client, log it in, or look at a file that is there
// and wrong.
func readAgentAccountFile(path string) (agentAccountRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return agentAccountRecord{}, errAgentAccountFileAbsent
		}
		return agentAccountRecord{}, fmt.Errorf("%s cannot be read", path)
	}
	// The stated shape, spelled as tags because that is what a decoder reads:
	// oauthAccount.accountUuid is the identity, and oauthAccount.emailAddress
	// accompanies it for display only.
	var doc struct {
		OAuthAccount *struct {
			AccountUuid  string `json:"accountUuid"`
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return agentAccountRecord{}, fmt.Errorf("%s is not readable JSON", path)
	}
	if doc.OAuthAccount == nil {
		return agentAccountRecord{}, fmt.Errorf("%s carries no %s object — the client is installed but not logged in",
			path, agentAccountObject)
	}
	if doc.OAuthAccount.AccountUuid == "" {
		// The e-mail is NOT the fallback. It is a display name: it changes
		// while the account does not, and it is missing on hosts where the
		// account is perfectly usable — so an account known only by one is an
		// account this host cannot name.
		return agentAccountRecord{}, fmt.Errorf("%s.%s is empty in %s — a display name is not an identity",
			agentAccountObject, agentAccountIdField, path)
	}
	return agentAccountRecord{
		Id:    flow.AgentAccountId(doc.OAuthAccount.AccountUuid),
		Email: doc.OAuthAccount.EmailAddress,
	}, nil
}

// agentAccountID is the identifier a park is scoped to, or the empty one when
// this host cannot name the account.
//
// Empty is a legitimate answer and the park is still written: the condition is
// real whether or not anybody could name whose allowance it is. What must not
// happen is a stand-in — a park scoped to a synthesized key compares equal to
// itself and will eventually compare equal to something else, which is worse
// than a park scoped to nothing.
func agentAccountID() flow.AgentAccountId {
	rec, err := agentAccount()
	if err != nil {
		return ""
	}
	return rec.Id
}

// agentAccountFiles lists where the client's .claude.json may be, in priority
// order. The FILE and the FIELD are stated; only which directory holds them is
// resolved, because that is a property of the installation rather than of the
// account.
//
// $HOME is searched as well as the config directories, because the client's
// account lives beside its config directory rather than inside it — and the
// config directories come first, so a caller pointed at one by
// CLAUDE_CONFIG_DIR is answered from there. A host driving two accounts under
// two config directories is exactly the case a fallback to $HOME would answer
// wrongly.
func agentAccountFiles() []string {
	var paths []string
	for _, dir := range claudeConfigDirs() {
		paths = append(paths, filepath.Join(dir, agentAccountFile))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, agentAccountFile))
	}
	return paths
}

// claudeConfigDirs returns the directories to search for Claude credentials,
// in priority order. Nothing is hardcoded beyond the "claude" directory name.
func claudeConfigDirs() []string {
	var dirs []string
	if env := os.Getenv("CLAUDE_CONFIG_DIR"); env != "" {
		dirs = append(dirs, env)
	}
	if ucd, err := os.UserConfigDir(); err == nil {
		dirs = append(dirs, filepath.Join(ucd, "claude"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".claude"))
	}
	return dirs
}

// discoverAPIBase finds the Anthropic API base URL from the installed Claude
// client's configuration. Returns ("url", "") on success or ("", reason) on
// failure. Nothing is pinned in flow's source.
func discoverAPIBase() (string, string) {
	// Try to read from Claude's settings first.
	for _, dir := range claudeConfigDirs() {
		path := filepath.Join(dir, "settings.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var settings map[string]interface{}
		if err := json.Unmarshal(data, &settings); err != nil {
			continue
		}
		for _, key := range []string{"apiBaseUrl", "api_base_url", "apiBase"} {
			if v, ok := settings[key].(string); ok && v != "" {
				return strings.TrimSuffix(v, "/"), ""
			}
		}
	}

	// Nothing here invokes the agent binary. `claude config get apiBaseUrl` used
	// to run at this point: `config` is not a subcommand, so the whole argv was
	// taken as a PROMPT and every caller spawned a full agent turn — unbounded,
	// untimed, billed, and reached from `go test`, which is what made the gate's
	// runtime a function of account state rather than of the tree.
	//
	// Correcting the arguments would not be the fix. A tool must never be able
	// to emit a prompt, so discovery reads configuration and stops: settings on
	// disk above, this default otherwise. Anything that cannot be learned by
	// reading is not learned here.
	return "https://api.anthropic.com", ""
}

// fetchUsage makes an authenticated request for subscription usage.
func fetchUsage(apiBase, token string) ([]windowUsage, error) {
	url := apiBase + "/api/oauth/usage"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("transport — %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("transport — %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("transport — read body: %v", err)
	}

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, newQuotaRefusal(resp,
			fmt.Sprintf("rejected — HTTP %d (credentials may be expired; re-run claude to refresh)", resp.StatusCode))
	}
	if resp.StatusCode != 200 {
		return nil, newQuotaRefusal(resp, fmt.Sprintf("rejected — HTTP %d", resp.StatusCode))
	}

	return parseUsageResponse(body)
}

// quotaRefusal is an answer the usage endpoint gave that was not a reading,
// carrying how long it asked the caller to wait before asking again. The wait
// is what the machine-wide cache backs off on: a 429 that every process ignored
// is the amplifier, and the endpoint has already said how to stop being one.
//
// Message-compatible with the plain errors it replaces — the text an operator
// sees is unchanged.
type quotaRefusal struct {
	msg        string
	retryAfter time.Duration // 0 when the response carried no usable Retry-After
}

func (e *quotaRefusal) Error() string { return e.msg }

func newQuotaRefusal(resp *http.Response, msg string) error {
	return &quotaRefusal{msg: msg, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
}

// parseRetryAfter reads RFC 9110's Retry-After in both of its forms —
// delta-seconds, and an HTTP date — and returns how long to wait. Returns 0 for
// an absent, unparseable, or already-elapsed header, which means "no
// instruction" and leaves the caller on its own default.
//
// The result is capped at quotaRetryAfterCap. The header is honoured because
// the endpoint knows its own limits, but nothing it sends may take pacing off
// the air for longer than the reading would age out on its own.
func parseRetryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(header); err == nil {
		// Capped before the multiplication, not only after it: a delta-seconds
		// large enough to overflow a Duration would otherwise wrap into a small
		// or negative wait, which is the opposite of what it asked for.
		if secs > int(quotaRetryAfterCap/time.Second) {
			return quotaRetryAfterCap
		}
		d = time.Duration(secs) * time.Second
	} else if when, err := http.ParseTime(header); err == nil {
		d = when.Sub(now)
	} else {
		return 0
	}
	if d <= 0 {
		return 0
	}
	if d > quotaRetryAfterCap {
		return quotaRetryAfterCap
	}
	return d
}

// parseUsageResponse extracts window usage from the API response. The response
// has top-level "five_hour" and "seven_day" objects, each with "utilization"
// (percentage 0–100) and "resets_at" (RFC3339).
func parseUsageResponse(body []byte) ([]windowUsage, error) {
	type windowData struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    string   `json:"resets_at"`
	}
	var resp struct {
		FiveHour *windowData `json:"five_hour"`
		SevenDay *windowData `json:"seven_day"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("transport — cannot parse usage response")
	}

	var result []windowUsage

	if resp.FiveHour != nil {
		t, _ := time.Parse(time.RFC3339, resp.FiveHour.ResetsAt)
		used := -1.0
		if resp.FiveHour.Utilization != nil {
			used = *resp.FiveHour.Utilization / 100.0
		}
		result = append(result, windowUsage{
			Label: "5h", Length: 5 * time.Hour, Used: used, ResetsAt: t,
		})
	}

	if resp.SevenDay != nil {
		t, _ := time.Parse(time.RFC3339, resp.SevenDay.ResetsAt)
		used := -1.0
		if resp.SevenDay.Utilization != nil {
			used = *resp.SevenDay.Utilization / 100.0
		}
		result = append(result, windowUsage{
			Label: "7d", Length: 7 * 24 * time.Hour, Used: used, ResetsAt: t,
		})
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("transport — usage response contained no window data")
	}
	return result, nil
}

// bindingExhaustedWindow returns the window whose allowance is spent and that
// decides when the account is usable again — or nil when none is spent.
//
// One predicate, two callers: RunOne's pre-dispatch check parks on it, and
// `resolve`'s pacing wait declines to pace against it. A second reading of
// "the allowance is spent" is how the gate that withholds a dispatch and the
// wait that precedes it come to disagree about the same figures.
//
// THE BINDING WINDOW IS THE ONE THAT RESETS LAST. With two windows flat, the
// allowance returns when the later one does, and reporting the earlier instant
// hands a driver a time the system already knew was too early — which is the
// one thing clears_at exists to prevent.
//
// A window whose PUBLISHED RESET HAS PASSED is not spent, whatever the figure
// beside it says: the window it describes is over, and the reading is simply
// older than it. This is the ordinary case rather than a corner — a reading is
// served from the machine-wide cache for minutes after it was taken, and a
// driver that waits to the published instant and resumes there arrives inside
// exactly that interval. Parking on it would report a condition that has
// ended, with an instant already in the past, which tells that driver to come
// straight back and be told the same thing. The in-band refusal classifies the
// turn if the allowance really is still spent.
//
// Used is a fraction in [0,1], or -1 when the endpoint reported none; -1 is
// below the floor and reads as "not spent", which is the right direction for a
// figure nobody has.
func bindingExhaustedWindow(usage []windowUsage, now time.Time) *windowUsage {
	var binding *windowUsage
	for i := range usage {
		if usage[i].Used < 1.0 {
			continue
		}
		if !usage[i].ResetsAt.IsZero() && !usage[i].ResetsAt.After(now) {
			continue
		}
		if binding == nil || usage[i].ResetsAt.After(binding.ResetsAt) {
			binding = &usage[i]
		}
	}
	return binding
}

// paceTargets holds the per-window target fractions for pacing.
type paceTargets struct {
	FiveHour float64 // e.g. 0.90
	SevenDay float64 // e.g. 0.95
}

// paceDelay computes how long to wait before the next step, given current
// usage and the target fractions. Returns 0 when no delay is needed.
// Both windows are checked; the tighter constraint wins.
func paceDelay(usage []windowUsage, targets paceTargets, now time.Time) time.Duration {
	var maxDelay time.Duration
	for _, win := range usage {
		if win.Used < 0 || win.Length <= 0 {
			continue
		}
		target := windowTarget(win.Label, targets)
		if target <= 0 {
			continue
		}

		elapsed := clampFraction(1.0 - float64(win.ResetsAt.Sub(now))/float64(win.Length))

		ceiling := target * elapsed
		if win.Used <= ceiling {
			continue
		}

		// Used > ceiling: compute how long until elapsed catches up.
		// needed_elapsed = Used / target; delay = (needed - elapsed) * Length.
		neededElapsed := win.Used / target
		if neededElapsed > 1.0 {
			// Used exceeds what the target allows even at 100% elapsed —
			// delay is the full remaining time of the window.
			remaining := win.ResetsAt.Sub(now)
			if remaining < 0 {
				remaining = 0
			}
			if remaining > maxDelay {
				maxDelay = remaining
			}
			continue
		}
		delay := time.Duration((neededElapsed - elapsed) * float64(win.Length))
		if delay < 0 {
			delay = 0
		}
		if delay > maxDelay {
			maxDelay = delay
		}
	}
	return maxDelay
}

// windowTarget returns the pacing target fraction for the given window label.
func windowTarget(label string, targets paceTargets) float64 {
	switch label {
	case "5h":
		return targets.FiveHour
	case "7d":
		return targets.SevenDay
	}
	return 0
}
