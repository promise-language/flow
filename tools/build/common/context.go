package common

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TrackContext pushes/pops a single ContextFrame on the tracker's per-agent
// ai_context stack for "work tools" (substantive actions: Bash/Edit/Write/Task/
// Skill/mcp__*). It is best-effort: any failure (no runner identity, no tracker
// URL, HTTP error) returns silently so the tool decision is never affected.
//
// The arena identity comes from runner-set env vars (TRACKER_AGENT_NAME /
// TRACKER_AGENT_HOST). A hand-run claude session has no TRACKER_AGENT_NAME and
// therefore reports nothing — no ghost frames in the tracker.
//
// This is a port of the tracker repo's tools/guard/guard.go ai_context logic
// (tracker commit 7fda6bb). Its caller is the workspace-supplied tool-guard,
// which this repository does not build: it must route both Pre and PostToolUse
// events through here, because the push/pop pairing is what keeps the live
// "what is the agent doing now" line on the arena card accurate.
func TrackContext(in HookInput) {
	if in.HookEventName != "PreToolUse" && in.HookEventName != "PostToolUse" {
		return
	}
	if !isWorkTool(in.ToolName) {
		return
	}
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	agent, host := resolveAgentIdentity()
	if agent == "" || host == "" {
		return
	}
	trackerURL := findTrackerURL(cwd)
	if trackerURL == "" {
		return
	}
	kind, name, input := frameFor(in)
	if name == "" {
		return
	}
	var path string
	switch in.HookEventName {
	case "PreToolUse":
		path = "/api/agent/context/push"
	case "PostToolUse":
		path = "/api/agent/context/pop"
	}
	postContext(trackerURL+path, agent, host, kind, name, input, cwd)
}

// isWorkTool reports whether a tool should appear in the ai_context stack.
// Work tools only (per design): substantive actions, not near-instant
// Read/Grep/Glob. MCP tool calls (mcp__*) count as work.
func isWorkTool(tool string) bool {
	switch tool {
	case "Bash", "Edit", "Write", "Task", "Skill":
		return true
	}
	return strings.HasPrefix(tool, "mcp__")
}

// frameFor summarizes a hook input into (kind, name, input) for a ContextFrame.
// kind is "skill" for the Skill tool, else "tool". input is a short,
// human-readable summary truncated to keep the arena card line compact.
func frameFor(in HookInput) (kind, name, input string) {
	var t struct {
		Command      string `json:"command"`       // Bash
		FilePath     string `json:"file_path"`     // Edit / Write
		Skill        string `json:"skill"`         // Skill
		Args         string `json:"args"`          // Skill
		Description  string `json:"description"`   // Task
		SubagentType string `json:"subagent_type"` // Task
	}
	_ = json.Unmarshal(in.ToolInput, &t)
	switch in.ToolName {
	case "Skill":
		kind, name, input = "skill", t.Skill, t.Args
	case "Bash":
		kind, name, input = "tool", "Bash", t.Command
	case "Edit", "Write":
		kind, name, input = "tool", in.ToolName, t.FilePath
	case "Task":
		kind, name, input = "tool", "Task", t.Description
		if t.SubagentType != "" {
			input = t.SubagentType + ": " + t.Description
		}
	default:
		kind, name = "tool", in.ToolName // mcp__* and any other work tool
	}
	const maxInput = 120
	if len(input) > maxInput {
		input = input[:maxInput-1] + "…"
	}
	return
}

// postContext fires a single best-effort POST to the tracker. Errors are
// swallowed — ai_context tracking must never block or meaningfully slow a
// tool.
func postContext(url, agent, host, kind, name, input, cwd string) {
	body, err := json.Marshal(map[string]string{
		"agent": agent,
		"host":  host,
		"kind":  kind,
		"name":  name,
		"input": input,
		"cwd":   cwd,
	})
	if err != nil {
		return
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// resolveAgentIdentity returns the runner-set agent name + short host the
// tracker keys the ai_context stack by. No TRACKER_AGENT_NAME ⇒ a hand-run
// claude session: return empty so we never create ghost context.
func resolveAgentIdentity() (agent, host string) {
	agent = os.Getenv("TRACKER_AGENT_NAME")
	if agent == "" {
		return "", ""
	}
	host = os.Getenv("TRACKER_AGENT_HOST")
	if host == "" {
		host, _ = os.Hostname()
	}
	return agent, shortHostName(host)
}

// shortHostName returns the first dotted segment, matching the tracker's host
// normalization so the guard's host matches the agent registry's.
func shortHostName(host string) string {
	host = strings.TrimSpace(host)
	if before, _, ok := strings.Cut(host, "."); ok {
		return before
	}
	return host
}

// mcpConfig mirrors the .mcp.json structure (just the tracker server URL).
type mcpConfig struct {
	MCPServers map[string]struct {
		URL string `json:"url"`
	} `json:"mcpServers"`
}

// findTrackerURL walks up from cwd looking for .mcp.json and returns the
// tracker server base URL (without the /mcp suffix).
func findTrackerURL(cwd string) string {
	dir := cwd
	for {
		data, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
		if err == nil {
			var cfg mcpConfig
			if json.Unmarshal(data, &cfg) == nil {
				if srv, ok := cfg.MCPServers["tracker"]; ok && srv.URL != "" {
					return strings.TrimSuffix(srv.URL, "/mcp")
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
