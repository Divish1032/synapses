// hooks_cmd.go — "synapses hooks", "synapses validate", and "synapses end-session"
// subcommands.
//
// These three commands form the Tier 2 (deterministic) security integration for
// Claude Code:
//
//	PostToolUse Write|Edit → synapses validate --scope post_write --path <repo>
//	Stop                   → synapses end-session --auto-summary --path <repo>
//
// Both commands are designed to be invoked from Claude Code hooks:
//   - They always exit 0 (a failing hook blocks Claude Code).
//   - They are silent when the daemon is not running.
//   - They print findings / confirmation to stdout which Claude Code injects
//     into the agent's context on the next turn.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cmdHooks dispatches the "synapses hooks" subcommand group.
func cmdHooks(args []string) error {
	if len(args) == 0 {
		fmt.Print(`Manage Claude Code hook configurations.

USAGE:
  synapses hooks <action> [flags]

ACTIONS:
  install   Install or upgrade Synapses security hooks into .claude/settings.json
  remove    Remove Synapses hooks from .claude/settings.json

`)
		return nil
	}
	switch args[0] {
	case "install":
		return cmdHooksInstall(args[1:])
	case "remove":
		return cmdHooksRemove(args[1:])
	default:
		return fmt.Errorf("unknown hooks action %q — use: install, remove", args[0])
	}
}

// cmdHooksInstall writes (or upgrades) the Claude Code security hooks into
// .claude/settings.json. Idempotent — safe to run multiple times.
func cmdHooksInstall(args []string) error {
	fs := flag.NewFlagSet("hooks install", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Path to the project root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	if err := writeClaudeSettings(absPath); err != nil {
		return fmt.Errorf("install hooks: %w", err)
	}
	settingsPath := filepath.Join(absPath, ".claude", "settings.json")
	fmt.Printf("Synapses hooks installed → %s\n", settingsPath)
	fmt.Println("  PostToolUse Write|Edit → synapses validate (post-write security scan)")
	fmt.Println("  Stop                  → synapses end-session (auto session persistence)")
	return nil
}

// cmdHooksRemove removes all Synapses hooks from .claude/settings.json without
// touching user-defined hooks or other settings.
func cmdHooksRemove(args []string) error {
	fs := flag.NewFlagSet("hooks remove", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Path to the project root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	cleanClaudeSettings(filepath.Join(absPath, ".claude", "settings.json"))
	return nil
}

// cmdValidate is a thin CLI wrapper around the daemon's verify_implementation
// tool. Designed for the Claude Code PostToolUse hook — always exits 0.
//
// When invoked from a Claude Code PostToolUse hook, the file path is read from
// stdin (Claude Code sends tool call details as JSON). The --file flag can
// override this for manual / scripted invocations.
func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	scope := fs.String("scope", "post_write", "Validation scope (post_write)")
	repoPath := fs.String("path", ".", "Path to the project root")
	fileArg := fs.String("file", "", "File path to validate (reads from stdin if omitted)")
	_ = fs.Parse(args) // ignore parse errors — hook commands must not fail

	// Resolve the project root.
	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return nil // hook must exit 0
	}

	// Determine which file to validate.
	filePath := *fileArg
	if filePath == "" {
		filePath = fileFromHookStdin()
	}
	if filePath == "" {
		// No file to validate — normal for non-file tools (Bash, etc.).
		return nil
	}

	// Resolve relative paths against the project root.
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(absPath, filePath)
	}

	filesJSON, err := json.Marshal([]string{filePath})
	if err != nil {
		return nil
	}

	body := map[string]interface{}{
		"files_written": string(filesJSON),
		"scope":         *scope,
	}

	output, err := callDaemonTool("verify_implementation", absPath, body, 5*time.Second)
	if err != nil {
		// Daemon not running or unreachable — exit silently, don't block the hook.
		return nil
	}
	if output != "" {
		fmt.Print(output)
	}
	return nil
}

// cmdEndSession is a thin CLI wrapper around the daemon's end_session tool.
// Designed for the Claude Code Stop hook — always exits 0.
func cmdEndSession(args []string) error {
	fs := flag.NewFlagSet("end-session", flag.ContinueOnError)
	autoSummary := fs.Bool("auto-summary", false, "Auto-generate and persist session summary")
	repoPath := fs.String("path", ".", "Path to the project root")
	agentID := fs.String("agent-id", "claude-code-hook", "Agent identifier")
	_ = fs.Parse(args) // ignore parse errors — hook commands must not fail

	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return nil // hook must exit 0
	}

	body := map[string]interface{}{
		"agent_id": *agentID,
	}
	if *autoSummary {
		body["summary"] = "auto"
	}

	_, err = callDaemonTool("end_session", absPath, body, 10*time.Second)
	if err != nil {
		// Daemon not running or unreachable — exit silently.
		return nil
	}
	fmt.Println("[Synapses] Session saved.")
	return nil
}

// fileFromHookStdin attempts to extract a file path from the Claude Code
// PostToolUse hook stdin payload. Claude Code sends a JSON object describing
// the tool call, including the file path for Write/Edit tools.
//
// Returns an empty string if stdin is absent, is a terminal (interactive),
// not JSON, or contains no file path.
func fileFromHookStdin() string {
	// Skip reading when stdin is an interactive terminal. This avoids hanging
	// if "synapses validate" is called manually without --file and without
	// piped input. Claude Code hook invocations always have stdin as a pipe.
	if stat, err := os.Stdin.Stat(); err == nil {
		if stat.Mode()&os.ModeCharDevice != 0 {
			return "" // interactive terminal — no piped hook data
		}
	}

	// LimitReader caps at 64 KiB — tool input payloads are small, this prevents
	// unbounded reads if stdin happens to be a long-running pipe.
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
	if err != nil || len(data) == 0 {
		return ""
	}

	// Claude Code PostToolUse stdin format (as of Claude Code 1.x):
	// {
	//   "tool_name":  "Write",
	//   "tool_input": { "file_path": "/abs/path/to/file", "content": "..." },
	//   "tool_response": { ... }
	// }
	//
	// Some hook versions use "path" instead of "file_path" for the Edit tool.
	var hookData struct {
		ToolInput struct {
			FilePath string `json:"file_path"`
			Path     string `json:"path"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(data, &hookData); err != nil {
		return ""
	}
	if hookData.ToolInput.FilePath != "" {
		return hookData.ToolInput.FilePath
	}
	return hookData.ToolInput.Path
}

// callDaemonTool calls a Synapses MCP tool via the daemon REST API and returns
// the tool's text output. Returns an error if the daemon is unreachable or
// returns a non-OK status.
//
// The daemon listens on http://127.0.0.1:11435 and accepts tool calls at:
//
//	POST /v1/tools/<name>?project=<abs-path>
//
// The response is a JSON-encoded *mcp.CallToolResult with a "content" array.
func callDaemonTool(toolName, projectPath string, body map[string]interface{}, timeout time.Duration) (string, error) {
	endpoint := "http://127.0.0.1:11435/v1/tools/" + toolName +
		"?project=" + url.QueryEscape(projectPath)

	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal body: %w", err)
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("daemon unavailable: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respData, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error string `json:"error"`
		}
		if jsonErr := json.Unmarshal(respData, &errResp); jsonErr == nil && errResp.Error != "" {
			return "", fmt.Errorf("daemon error: %s", errResp.Error)
		}
		return "", fmt.Errorf("daemon returned HTTP %d", resp.StatusCode)
	}

	// mcp.CallToolResult JSON shape:
	// { "content": [{"type": "text", "text": "..."}], "isError": false }
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respData, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	var sb strings.Builder
	for _, c := range result.Content {
		if c.Type == "text" && c.Text != "" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), nil
}
