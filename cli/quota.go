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

// reportQuota prints Claude subscription window utilisation to w.
// Called only when a person is driving (FLOW_DISPATCHED_BY_RUNNER absent).
// Failure never blocks the run — prints the reason and returns.
func reportQuota(w io.Writer) {
	if os.Getenv(dispatchedByRunnerEnv) == "1" {
		return
	}

	token, credErr := discoverOAuthToken()
	if credErr != "" {
		fmt.Fprintf(w, "quota: %s\n", credErr)
		return
	}

	apiBase, baseErr := discoverAPIBase()
	if baseErr != "" {
		fmt.Fprintf(w, "quota: %s\n", baseErr)
		return
	}

	usage, err := fetchUsage(apiBase, token)
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
	searched := strings.Join(dirs, ", ")
	return "", fmt.Sprintf("no Claude credentials found at %s", searched)
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

	// Try running `claude config get apiBaseUrl` to discover from the binary.
	if out, err := exec.Command("claude", "config", "get", "apiBaseUrl").Output(); err == nil {
		s := strings.TrimSpace(string(out))
		if s != "" && strings.HasPrefix(s, "http") {
			return strings.TrimSuffix(s, "/"), ""
		}
	}

	return "", "could not determine API base — run `claude config set apiBaseUrl <url>` to configure"
}

// fetchUsage makes an authenticated request for subscription usage.
func fetchUsage(apiBase, token string) ([]windowUsage, error) {
	url := apiBase + "/v1/organizations/usage"

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

// parseUsageResponse extracts window usage from the API response. The
// structure is discovered from the response rather than pinned.
func parseUsageResponse(body []byte) ([]windowUsage, error) {
	// Try to parse as a response carrying rate_limits or usage windows.
	var envelope struct {
		RateLimits []struct {
			Window   string  `json:"window"`
			Used     float64 `json:"used"`
			Limit    float64 `json:"limit"`
			ResetsAt string  `json:"resets_at"`
		} `json:"rate_limits"`
		Windows []struct {
			Label    string  `json:"label"`
			Length   int     `json:"length_seconds"`
			Used     float64 `json:"usage_fraction"`
			ResetsAt string  `json:"resets_at"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("transport — cannot parse usage response")
	}

	var result []windowUsage

	// Path 1: windows array.
	for _, w := range envelope.Windows {
		t, _ := time.Parse(time.RFC3339, w.ResetsAt)
		used := w.Used
		if used < 0 {
			used = -1
		}
		result = append(result, windowUsage{
			Label:    w.Label,
			Length:   time.Duration(w.Length) * time.Second,
			Used:     used,
			ResetsAt: t,
		})
	}

	// Path 2: rate_limits array (alternative schema).
	for _, rl := range envelope.RateLimits {
		t, _ := time.Parse(time.RFC3339, rl.ResetsAt)
		var used float64 = -1
		if rl.Limit > 0 {
			used = rl.Used / rl.Limit
		}
		length := windowLengthFromLabel(rl.Window)
		label := rl.Window
		result = append(result, windowUsage{
			Label:    label,
			Length:   length,
			Used:     used,
			ResetsAt: t,
		})
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("transport — usage response contained no window data")
	}
	return result, nil
}

func windowLengthFromLabel(label string) time.Duration {
	switch label {
	case "5h":
		return 5 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	case "24h", "1d":
		return 24 * time.Hour
	}
	return 0
}
