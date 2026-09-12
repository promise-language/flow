package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	if acct := agentAccount(); acct != "" {
		fmt.Fprintf(w, "agent account: %s\n", acct)
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

// discoverOAuthToken reads the Claude OAuth token from the first available
// credentials location. Returns (token, "") on success or ("", reason) on
// failure with a distinct reason for each failure mode.
func discoverOAuthToken() (string, string) {
	dirs := claudeConfigDirs()
	if len(dirs) == 0 {
		return "", "no Claude credentials found — no config directories available"
	}

	for _, dir := range dirs {
		path := filepath.Join(dir, "credentials.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// The credentials file is a JSON object; the OAuth token key is
		// discovered at runtime rather than hardcoded. Look for the first key
		// containing "oauth" (case-insensitive) whose value has a "token" field.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}
		for key, val := range raw {
			if !strings.Contains(strings.ToLower(key), "oauth") {
				continue
			}
			var obj map[string]interface{}
			if err := json.Unmarshal(val, &obj); err != nil {
				continue
			}
			// Try multiple field names: the exact key the client uses is not
			// pinned here.
			for _, field := range []string{"token", "accessToken", "access_token"} {
				if tok, ok := obj[field].(string); ok && tok != "" {
					return tok, ""
				}
			}
			// The credentials exist but the token value is empty or absent.
			return "", fmt.Sprintf("credentials expired — re-run claude to refresh (read %s)", path)
		}
		// Found credentials.json but no OAuth key.
		return "", fmt.Sprintf("no Claude OAuth credentials found in %s", path)
	}
	// macOS Keychain fallback: Claude Code stores credentials there, not on disk.
	if out, err := exec.Command("security", "find-generic-password",
		"-s", "Claude Code-credentials", "-w").Output(); err == nil {
		var kc struct {
			ClaudeAiOauth struct {
				AccessToken string `json:"accessToken"`
			} `json:"claudeAiOauth"`
		}
		if err := json.Unmarshal(out, &kc); err == nil && kc.ClaudeAiOauth.AccessToken != "" {
			return kc.ClaudeAiOauth.AccessToken, ""
		}
	}

	searched := strings.Join(dirs, ", ")
	return "", fmt.Sprintf("no Claude credentials found (searched %s and macOS Keychain)", searched)
}

// agentAccount is the seam this file's account discovery is tested through,
// alongside quotaFetch and quotaCacheDir. A test must never read the
// developer's own account: redirected in TestMain, so a test added later
// cannot forget to.
var agentAccount = discoverAgentAccount

// discoverAgentAccount names the account the agent substrate spends as: the
// OAuth account object in the installed client's own configuration. Returns ""
// when nothing on disk says — an unknown account drops the line rather than
// printing a guess.
//
// The shape discoverOAuthToken uses, for the same reasons: nothing is pinned in
// flow's source, nothing is asked of a subprocess, and nothing touches the
// network. What cannot be learned by reading is not learned here.
func discoverAgentAccount() string {
	for _, path := range agentAccountFiles() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}
		// The key naming the OAuth account is discovered rather than
		// hardcoded, as the token's is — and a file whose OAuth object carries
		// no account (the credentials file does not) falls through to the next
		// candidate instead of answering empty for all of them.
		for key, val := range raw {
			if !strings.Contains(strings.ToLower(key), "oauth") {
				continue
			}
			var obj map[string]any
			if err := json.Unmarshal(val, &obj); err != nil {
				continue
			}
			// The e-mail first: it is what an operator recognises. The account
			// id is the fallback, because an id names the account exactly even
			// when nothing human-readable is stored.
			for _, field := range []string{"emailAddress", "email", "account_email", "accountUuid", "account_id", "accountId"} {
				if v, ok := obj[field].(string); ok && v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// agentAccountFiles lists the configuration files that may carry the OAuth
// account, in priority order.
//
// $HOME is searched as well as the config directories, because the client's
// account lives beside its config directory rather than inside it — and the
// config directories come first, so a caller pointed at one by
// CLAUDE_CONFIG_DIR is answered from there.
func agentAccountFiles() []string {
	var paths []string
	for _, dir := range claudeConfigDirs() {
		paths = append(paths, filepath.Join(dir, ".claude.json"), filepath.Join(dir, "config.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".claude.json"))
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
