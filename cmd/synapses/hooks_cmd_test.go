package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── cmdHooksInstall ───────────────────────────────────────────────────────────

func TestCmdHooksInstall_WritesHooks(t *testing.T) {
	dir := t.TempDir()
	if err := cmdHooksInstall([]string{"--path", dir}); err != nil {
		t.Fatalf("cmdHooksInstall: %v", err)
	}

	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("settings.json not created: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	hooks, ok := m["hooks"].(map[string]interface{})
	if !ok {
		t.Fatal("settings.json missing hooks section")
	}

	// PostToolUse Write|Edit hook must exist.
	postHooks, ok := hooks["PostToolUse"].([]interface{})
	if !ok || len(postHooks) == 0 {
		t.Fatal("no PostToolUse hooks found")
	}

	// Stop hook must exist.
	stopHooks, ok := hooks["Stop"].([]interface{})
	if !ok || len(stopHooks) == 0 {
		t.Fatal("no Stop hooks found")
	}
}

func TestCmdHooksInstall_PostToolUseCommandContainsValidate(t *testing.T) {
	dir := t.TempDir()
	if err := cmdHooksInstall([]string{"--path", dir}); err != nil {
		t.Fatalf("cmdHooksInstall: %v", err)
	}

	settings := readSettingsForTest(t, dir)
	hooks := settings["hooks"].(map[string]interface{})
	postHooks := hooks["PostToolUse"].([]interface{})

	var found bool
	for _, entry := range postHooks {
		m, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		if m["matcher"] != "Write|Edit" {
			continue
		}
		innerHooks, ok := m["hooks"].([]interface{})
		if !ok {
			continue
		}
		for _, h := range innerHooks {
			hm, ok := h.(map[string]interface{})
			if !ok {
				continue
			}
			cmd, _ := hm["command"].(string)
			if strings.Contains(cmd, "validate") && strings.Contains(cmd, "post_write") {
				found = true
			}
		}
	}
	if !found {
		t.Error("PostToolUse hook should contain 'validate --scope post_write'")
	}
}

func TestCmdHooksInstall_StopHookContainsEndSession(t *testing.T) {
	dir := t.TempDir()
	if err := cmdHooksInstall([]string{"--path", dir}); err != nil {
		t.Fatalf("cmdHooksInstall: %v", err)
	}

	settings := readSettingsForTest(t, dir)
	hooks := settings["hooks"].(map[string]interface{})
	stopHooks, ok := hooks["Stop"].([]interface{})
	if !ok || len(stopHooks) == 0 {
		t.Fatal("no Stop hooks found")
	}

	var found bool
	for _, entry := range stopHooks {
		m, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		innerHooks, ok := m["hooks"].([]interface{})
		if !ok {
			continue
		}
		for _, h := range innerHooks {
			hm, ok := h.(map[string]interface{})
			if !ok {
				continue
			}
			cmd, _ := hm["command"].(string)
			if strings.Contains(cmd, "end-session") && strings.Contains(cmd, "auto-summary") {
				found = true
			}
		}
	}
	if !found {
		t.Error("Stop hook should contain 'end-session --auto-summary'")
	}
}

func TestCmdHooksInstall_Idempotent(t *testing.T) {
	dir := t.TempDir()

	// Run twice — should not create duplicate hooks.
	cmdHooksInstall([]string{"--path", dir}) //nolint:errcheck
	cmdHooksInstall([]string{"--path", dir}) //nolint:errcheck

	settings := readSettingsForTest(t, dir)
	hooks := settings["hooks"].(map[string]interface{})

	// PostToolUse should have exactly one Write|Edit entry.
	postHooks := hooks["PostToolUse"].([]interface{})
	var writeEditCount int
	for _, entry := range postHooks {
		m, ok := entry.(map[string]interface{})
		if ok && m["matcher"] == "Write|Edit" {
			writeEditCount++
		}
	}
	if writeEditCount != 1 {
		t.Errorf("expected 1 Write|Edit hook, got %d", writeEditCount)
	}

	// Stop should have exactly one entry.
	stopHooks := hooks["Stop"].([]interface{})
	var stopCount int
	for _, entry := range stopHooks {
		m, ok := entry.(map[string]interface{})
		if ok && m["matcher"] == "stop" {
			stopCount++
		}
	}
	if stopCount != 1 {
		t.Errorf("expected 1 Stop hook, got %d", stopCount)
	}
}

func TestCmdHooksInstall_PreservesExistingHooks(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	os.MkdirAll(claudeDir, 0o755) //nolint:errcheck

	// Write pre-existing user hook.
	initial := map[string]interface{}{
		"hooks": map[string]interface{}{
			"PostToolUse": []interface{}{
				map[string]interface{}{
					"matcher": "Bash",
					"hooks": []interface{}{
						map[string]interface{}{"type": "command", "command": "echo user-hook"},
					},
				},
			},
		},
	}
	data, _ := json.Marshal(initial)
	os.WriteFile(filepath.Join(claudeDir, "settings.json"), data, 0o644) //nolint:errcheck

	if err := cmdHooksInstall([]string{"--path", dir}); err != nil {
		t.Fatalf("cmdHooksInstall: %v", err)
	}

	settings := readSettingsForTest(t, dir)
	hooks := settings["hooks"].(map[string]interface{})
	postHooks := hooks["PostToolUse"].([]interface{})

	var hasUserHook, hasSynapsesHook bool
	for _, entry := range postHooks {
		m, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		switch m["matcher"] {
		case "Bash":
			hasUserHook = true
		case "Write|Edit":
			hasSynapsesHook = true
		}
	}
	if !hasUserHook {
		t.Error("user Bash hook should be preserved")
	}
	if !hasSynapsesHook {
		t.Error("Synapses Write|Edit hook should be added")
	}
}

// ── cmdValidate — endpoint and body verification ─────────────────────────────

// TestCmdValidate_CallsValidateEndpointWithPhasePost verifies that cmdValidate
// calls the "validate" MCP tool (not the old "verify_implementation") and sends
// {"phase": "post", "files_written": ...} in the request body.
func TestCmdValidate_CallsValidateEndpointWithPhasePost(t *testing.T) {
	var gotPath string
	var gotBody map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &gotBody) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "ok"},
			},
		})
	}))
	defer srv.Close()

	// Override the daemon base URL to point at the test server.
	old := daemonBaseURL
	daemonBaseURL = srv.URL
	defer func() { daemonBaseURL = old }()

	dir := t.TempDir()
	if err := cmdValidate([]string{"--path", dir, "--file", "/some/file.go"}); err != nil {
		t.Fatalf("cmdValidate: %v", err)
	}

	if gotPath != "/v1/tools/validate" {
		t.Errorf("expected /v1/tools/validate, got %q", gotPath)
	}
	if gotBody["phase"] != "post" {
		t.Errorf("expected phase=post in body, got: %v", gotBody["phase"])
	}
	if _, ok := gotBody["files_written"]; !ok {
		t.Error("body missing files_written field")
	}
	if _, ok := gotBody["scope"]; ok {
		t.Error("body should NOT contain scope field (CLI-only, not forwarded to daemon)")
	}
}

// ── cmdValidate (daemon not running) ─────────────────────────────────────────

func TestCmdValidate_DaemonNotRunning_ExitsZero(t *testing.T) {
	dir := t.TempDir()
	// daemon is not running — should return nil (exit 0), not an error.
	err := cmdValidate([]string{"--scope", "post_write", "--path", dir, "--file", "nonexistent.go"})
	if err != nil {
		t.Errorf("cmdValidate should return nil when daemon is down, got: %v", err)
	}
}

func TestCmdValidate_NoFile_ExitsZero(t *testing.T) {
	dir := t.TempDir()
	// No --file and stdin is not a pipe here (test runner) — should be a no-op.
	err := cmdValidate([]string{"--scope", "post_write", "--path", dir})
	if err != nil {
		t.Errorf("cmdValidate with no file should return nil, got: %v", err)
	}
}

// ── cmdEndSession (daemon not running) ───────────────────────────────────────

func TestCmdEndSession_DaemonNotRunning_ExitsZero(t *testing.T) {
	dir := t.TempDir()
	err := cmdEndSession([]string{"--auto-summary", "--path", dir})
	if err != nil {
		t.Errorf("cmdEndSession should return nil when daemon is down, got: %v", err)
	}
}

// ── callDaemonTool (via callDaemonToolAt) ─────────────────────────────────────

func TestCallDaemonTool_ParsesTextContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/tools/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "Finding A"},
				map[string]interface{}{"type": "text", "text": "Finding B"},
			},
			"isError": false,
		})
	}))
	defer srv.Close()

	text, err := callDaemonToolAt(srv.URL, "verify_implementation", "/tmp/proj",
		map[string]interface{}{"files_written": `["/tmp/proj/main.go"]`},
		3*time.Second)
	if err != nil {
		t.Fatalf("callDaemonToolAt: %v", err)
	}
	if !strings.Contains(text, "Finding A") || !strings.Contains(text, "Finding B") {
		t.Errorf("expected both findings in output, got: %q", text)
	}
}

func TestCallDaemonTool_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "something broke"}) //nolint:errcheck
	}))
	defer srv.Close()

	_, err := callDaemonToolAt(srv.URL, "verify_implementation", "/tmp/proj", nil, 3*time.Second)
	if err == nil {
		t.Error("expected error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "something broke") {
		t.Errorf("error should contain server message, got: %v", err)
	}
}

func TestCallDaemonTool_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, err := callDaemonToolAt(srv.URL, "end_session", "/tmp/proj",
		map[string]interface{}{"agent_id": "test"},
		50*time.Millisecond) // shorter than server sleep
	if err == nil {
		t.Error("expected timeout error")
	}
}

// ── fileFromHookStdin ─────────────────────────────────────────────────────────

func TestFileFromHookStdin_ExtractsFilePath(t *testing.T) {
	payload := `{"tool_name":"Write","tool_input":{"file_path":"/project/main.go","content":"..."}}`
	oldStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	w.WriteString(payload) //nolint:errcheck
	w.Close()

	got := fileFromHookStdin()
	os.Stdin = oldStdin

	if got != "/project/main.go" {
		t.Errorf("expected /project/main.go, got %q", got)
	}
}

func TestFileFromHookStdin_FallsBackToPath(t *testing.T) {
	payload := `{"tool_name":"Edit","tool_input":{"path":"/project/utils.go"}}`
	oldStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	w.WriteString(payload) //nolint:errcheck
	w.Close()

	got := fileFromHookStdin()
	os.Stdin = oldStdin

	if got != "/project/utils.go" {
		t.Errorf("expected /project/utils.go, got %q", got)
	}
}

func TestFileFromHookStdin_EmptyStdin(t *testing.T) {
	oldStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	w.Close() // empty

	got := fileFromHookStdin()
	os.Stdin = oldStdin

	if got != "" {
		t.Errorf("expected empty string for empty stdin, got %q", got)
	}
}

func TestFileFromHookStdin_InvalidJSON(t *testing.T) {
	oldStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	w.WriteString("not json") //nolint:errcheck
	w.Close()

	got := fileFromHookStdin()
	os.Stdin = oldStdin

	if got != "" {
		t.Errorf("expected empty string for invalid JSON, got %q", got)
	}
}

// TestFileFromHookStdin_PipeReturnsPath verifies that a closed pipe (no TTY)
// correctly returns the file path — simulating the Claude Code hook context.
func TestFileFromHookStdin_PipeReturnsPath(t *testing.T) {
	payload := `{"tool_input":{"file_path":"/srv/api/handler.go"}}`
	oldStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	w.WriteString(payload) //nolint:errcheck
	w.Close()

	got := fileFromHookStdin()
	os.Stdin = oldStdin

	if got != "/srv/api/handler.go" {
		t.Errorf("expected /srv/api/handler.go, got %q", got)
	}
}

// ── hook cleanup (uninstall integration) ─────────────────────────────────────

// TestCmdHooksRemove_RemovesValidateAndStopHooks verifies that hooks installed by
// cmdHooksInstall are correctly removed by cmdHooksRemove (via cleanClaudeSettings).
// This is critical: if isSynapsesHookEntry doesn't match the new commands, they
// become permanent fixtures that users can't clean up.
func TestCmdHooksRemove_RemovesValidateAndStopHooks(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	os.MkdirAll(claudeDir, 0o755) //nolint:errcheck

	// Write a settings.json that mirrors what writeClaudeSettings produces
	// when synapsesBin is the installed path (contains /.synapses/).
	raw := map[string]interface{}{
		"hooks": map[string]interface{}{
			"PostToolUse": []interface{}{
				map[string]interface{}{
					"matcher": "Write|Edit",
					"hooks": []interface{}{
						map[string]interface{}{
							"type":    "command",
							"command": "/home/user/.synapses/bin/synapses validate --scope post_write --path \"/srv/project\"",
						},
					},
				},
				// user hook — must survive
				map[string]interface{}{
					"matcher": "Bash",
					"hooks": []interface{}{
						map[string]interface{}{"type": "command", "command": "echo lint"},
					},
				},
			},
			"Stop": []interface{}{
				map[string]interface{}{
					"matcher": "stop",
					"hooks": []interface{}{
						map[string]interface{}{
							"type":    "command",
							"command": "/home/user/.synapses/bin/synapses end-session --auto-summary --path \"/srv/project\"",
						},
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(raw, "", "  ")
	os.WriteFile(filepath.Join(claudeDir, "settings.json"), data, 0o644) //nolint:errcheck

	if err := cmdHooksRemove([]string{"--path", dir}); err != nil {
		t.Fatalf("cmdHooksRemove: %v", err)
	}

	result := readSettingsForTest(t, dir)
	hooks, _ := result["hooks"].(map[string]interface{})

	// Stop hook must be gone entirely.
	if _, ok := hooks["Stop"]; ok {
		t.Error("Stop event should be removed after cmdHooksRemove")
	}

	// PostToolUse should survive but only with the user's Bash hook.
	postHooks, ok := hooks["PostToolUse"].([]interface{})
	if !ok {
		t.Fatal("PostToolUse section missing — user hook was removed too")
	}
	for _, entry := range postHooks {
		m, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		if m["matcher"] == "Write|Edit" {
			t.Error("Write|Edit Synapses hook should have been removed")
		}
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func readSettingsForTest(t *testing.T, dir string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	return m
}

// callDaemonToolAt is a test helper that calls a tool endpoint at a custom base URL,
// bypassing the hardcoded 127.0.0.1:11435 address used in production.
func callDaemonToolAt(baseURL, toolName, projectPath string, body map[string]interface{}, timeout time.Duration) (string, error) {
	endpoint := baseURL + "/v1/tools/" + toolName + "?project=" + url.QueryEscape(projectPath)

	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("marshal body: %w", err)
		}
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
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
