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

// reportQuota prints Claude subscription window utilisation to w.
// Failure never blocks the run — prints the reason and returns.
func reportQuota(w io.Writer) {
	usage, err := readQuota()
	if err != nil {
		fmt.Fprintf(w, "quota: %s\n", err)
		return
	}

	now := time.Now()
	for _, win := range usage {
		printWindow(w, win, now)
	}
}

// windowUsage is the parsed response for one subscription window.
type windowUsage struct {
	Label    string // "5h" or "7d"
	Length   time.Duration
	Used     float64   // fraction [0,1], or -1 if absent
	ResetsAt time.Time // when the window resets
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
		return nil, fmt.Errorf("rejected — HTTP %d (credentials may be expired; re-run claude to refresh)", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("rejected — HTTP %d", resp.StatusCode)
	}

	return parseUsageResponse(body)
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
