package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── cmdUninstall dispatch ────────────────────────────────────────────────────

func TestRun_Uninstall_Dispatch(t *testing.T) {
	// Verify that "uninstall" is recognized as a valid command (not "unknown command").
	err := run([]string{"uninstall", "--help"})
	// flag.FlagSet with ContinueOnError returns "flag: help requested" — that's fine,
	// it means the command was dispatched. An unknown command returns a different error.
	if err != nil && err.Error() != "flag: help requested" {
		t.Errorf("run(uninstall --help) returned unexpected error: %v", err)
	}
}

// ── cleanMCPServerEntry ──────────────────────────────────────────────────────

func TestCleanMCPServerEntry_RemovesSynapses(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "mcp.json")

	raw := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"synapses": map[string]interface{}{"type": "http", "url": "http://127.0.0.1:11435/mcp"},
			"other":    map[string]interface{}{"type": "stdio", "command": "other-tool"},
		},
	}
	data, _ := json.MarshalIndent(raw, "", "  ")
	os.WriteFile(file, data, 0o644)

	cleanMCPServerEntry(file, "test")

	// File should still exist with "other" but not "synapses".
	out, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("file removed unexpectedly: %v", err)
	}
	var result map[string]interface{}
	json.Unmarshal(out, &result)
	servers := result["mcpServers"].(map[string]interface{})
	if _, ok := servers["synapses"]; ok {
		t.Error("synapses entry still present after cleanup")
	}
	if _, ok := servers["other"]; !ok {
		t.Error("other entry was removed — should be preserved")
	}
}

func TestCleanMCPServerEntry_RemovesFileWhenOnlySynapses(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "mcp.json")

	raw := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"synapses": map[string]interface{}{"type": "http"},
		},
	}
	data, _ := json.MarshalIndent(raw, "", "  ")
	os.WriteFile(file, data, 0o644)

	cleanMCPServerEntry(file, "test")

	if _, err := os.Stat(file); err == nil {
		t.Error("file should have been removed when synapses was the only entry")
	}
}

func TestCleanMCPServerEntry_PreservesOtherTopLevelKeys(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "mcp.json")

	// File has synapses as only server but also has other top-level keys.
	raw := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"synapses": map[string]interface{}{"type": "http"},
		},
		"settings": map[string]interface{}{"debug": true},
	}
	data, _ := json.MarshalIndent(raw, "", "  ")
	os.WriteFile(file, data, 0o644)

	cleanMCPServerEntry(file, "test")

	// File should still exist because "settings" key must be preserved.
	out, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("file was removed — 'settings' key should have kept it alive")
	}
	var result map[string]interface{}
	json.Unmarshal(out, &result)
	if _, ok := result["mcpServers"]; ok {
		t.Error("empty mcpServers key should have been removed")
	}
	if _, ok := result["settings"]; !ok {
		t.Error("'settings' key was removed — should be preserved")
	}
}

func TestCleanMCPServerEntry_NoopOnMissing(t *testing.T) {
	// Should not panic on missing file.
	cleanMCPServerEntry("/nonexistent/mcp.json", "test")
}

// ── cleanZedMCPConfig ────────────────────────────────────────────────────────

func TestCleanZedMCPConfig_RemovesSynapses(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "settings.json")

	raw := map[string]interface{}{
		"theme": "dark",
		"context_servers": map[string]interface{}{
			"synapses": map[string]interface{}{"settings": map[string]interface{}{"url": "http://..."}},
			"other":    map[string]interface{}{},
		},
	}
	data, _ := json.MarshalIndent(raw, "", "  ")
	os.WriteFile(file, data, 0o644)

	cleanZedMCPConfig(file)

	out, _ := os.ReadFile(file)
	var result map[string]interface{}
	json.Unmarshal(out, &result)

	cs := result["context_servers"].(map[string]interface{})
	if _, ok := cs["synapses"]; ok {
		t.Error("synapses entry still present")
	}
	if _, ok := cs["other"]; !ok {
		t.Error("other entry removed — should be preserved")
	}
	if _, ok := result["theme"]; !ok {
		t.Error("theme setting removed — should be preserved")
	}
}

// ── cleanSynapsesSection ─────────────────────────────────────────────────────

func TestCleanSynapsesSection_RemovesBlock(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "CLAUDE.md")

	content := "# My Project\n\nSome notes.\n\n" +
		synapsesSectionStart + "synapses guidance here\n" + synapsesSectionEnd + "\n\nMore notes.\n"
	os.WriteFile(file, []byte(content), 0o644)

	cleanSynapsesSection(file, "test")

	out, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("file removed unexpectedly")
	}
	if s := string(out); strings.Contains(s, "synapses:start") || strings.Contains(s, "synapses guidance") {
		t.Error("synapses section still present")
	}
	if s := string(out); !strings.Contains(s, "My Project") || !strings.Contains(s, "More notes") {
		t.Error("non-synapses content was removed")
	}
}

func TestCleanSynapsesSection_RemovesFileWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "rules.md")

	content := synapsesSectionStart + "stuff\n" + synapsesSectionEnd
	os.WriteFile(file, []byte(content), 0o644)

	cleanSynapsesSection(file, "test")

	if _, err := os.Stat(file); err == nil {
		t.Error("file should be removed when only synapses content remains")
	}
}

func TestCleanSynapsesSection_PreservesUserContent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "CLAUDE.md")

	// User has their own content before and after the synapses block.
	userBefore := "# My Project Rules\n\nAlways use snake_case.\n"
	userAfter := "\n## Testing\n\nRun `go test ./...` before committing.\n"
	content := userBefore + "\n" +
		synapsesSectionStart + "synapses guidance here\n" + synapsesSectionEnd +
		userAfter
	os.WriteFile(file, []byte(content), 0o644)

	cleanSynapsesSection(file, "test")

	out, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("file was removed — user content should keep it alive")
	}
	result := string(out)

	if !strings.Contains(result, "Always use snake_case.") {
		t.Error("user content before synapses section was lost")
	}
	if !strings.Contains(result, "Run `go test ./...` before committing.") {
		t.Error("user content after synapses section was lost")
	}
	if strings.Contains(result, "synapses:start") || strings.Contains(result, "synapses guidance") {
		t.Error("synapses section was not removed")
	}
}

// ── cleanClaudeSettings ──────────────────────────────────────────────────────

func TestCleanClaudeSettings_RemovesSynapsesHooks(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "settings.json")

	// Mirrors the actual structure written by writeClaudeSettings:
	// { "matcher": "...", "hooks": [{ "type": "command", "command": "..." }] }
	raw := map[string]interface{}{
		"hooks": map[string]interface{}{
			"SessionStart": []interface{}{
				// Synapses hook — should be removed (contains [Synapses] marker).
				map[string]interface{}{
					"matcher": "startup",
					"hooks": []interface{}{
						map[string]interface{}{
							"type":    "command",
							"command": "cat \"/home/user/.synapses/context/abc.md\" 2>/dev/null || echo '[Synapses] Daemon not running'",
						},
					},
				},
			},
			"PostToolUse": []interface{}{
				// Synapses hook — should be removed (contains [Synapses] marker).
				map[string]interface{}{
					"matcher": "Write|Edit",
					"hooks": []interface{}{
						map[string]interface{}{
							"type":    "command",
							"command": "echo '[Synapses] Files written. Now call verify_implementation.'",
						},
					},
				},
				// User's own hook — must be preserved.
				map[string]interface{}{
					"matcher": "Write",
					"hooks": []interface{}{
						map[string]interface{}{
							"type":    "command",
							"command": "echo 'my custom lint hook'",
						},
					},
				},
			},
		},
		"permissions": map[string]interface{}{
			"allow": []interface{}{
				"mcp__synapses__*",
				"mcp__synapses__context_carve",
				"some_other_perm",
				"Bash(git *)",
			},
		},
		"theme": "dark", // unrelated user setting — must survive
	}
	data, _ := json.MarshalIndent(raw, "", "  ")
	os.WriteFile(file, data, 0o644)

	cleanClaudeSettings(file)

	out, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("file was removed: %v", err)
	}
	var result map[string]interface{}
	json.Unmarshal(out, &result)

	// Theme should survive untouched.
	if result["theme"] != "dark" {
		t.Error("unrelated 'theme' setting was removed")
	}

	// Hooks: SessionStart should be gone entirely, PostToolUse should keep user hook.
	hooks, ok := result["hooks"].(map[string]interface{})
	if !ok {
		t.Fatal("hooks section removed entirely — PostToolUse user hook should remain")
	}
	if _, ok := hooks["SessionStart"]; ok {
		t.Error("SessionStart (synapses) should have been removed")
	}
	post, ok := hooks["PostToolUse"].([]interface{})
	if !ok || len(post) != 1 {
		t.Errorf("PostToolUse should have exactly 1 entry (user hook), got %d", len(post))
	}

	// Permissions: synapses perms removed, user perms preserved.
	perms := result["permissions"].(map[string]interface{})
	allow := perms["allow"].([]interface{})
	if len(allow) != 2 {
		t.Errorf("expected 2 non-synapses permissions, got %v", allow)
	}
	for _, p := range allow {
		s := p.(string)
		if s == "mcp__synapses__*" || s == "mcp__synapses__context_carve" {
			t.Errorf("synapses permission %q should have been removed", s)
		}
	}
}

func TestCleanClaudeSettings_NoFalsePositives(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "settings.json")

	// User hooks that mention "synapses" in prose but are NOT synapses hooks.
	// These must NOT be removed.
	raw := map[string]interface{}{
		"hooks": map[string]interface{}{
			"PostToolUse": []interface{}{
				map[string]interface{}{
					"matcher": "Write",
					"hooks": []interface{}{
						map[string]interface{}{
							"type":    "command",
							"command": "echo 'remember to check synapses docs'",
						},
					},
				},
			},
		},
		"permissions": map[string]interface{}{
			"allow": []interface{}{
				"Bash(synapses *)", // user allowing synapses CLI — NOT an mcp__synapses__ perm
			},
		},
	}
	data, _ := json.MarshalIndent(raw, "", "  ")
	os.WriteFile(file, data, 0o644)

	cleanClaudeSettings(file)

	out, _ := os.ReadFile(file)
	var result map[string]interface{}
	json.Unmarshal(out, &result)

	// Nothing should have changed — no synapses markers present.
	hooks := result["hooks"].(map[string]interface{})
	post := hooks["PostToolUse"].([]interface{})
	if len(post) != 1 {
		t.Error("user hook mentioning 'synapses' in prose was incorrectly removed")
	}

	perms := result["permissions"].(map[string]interface{})
	allow := perms["allow"].([]interface{})
	if len(allow) != 1 || allow[0] != "Bash(synapses *)" {
		t.Error("user permission 'Bash(synapses *)' was incorrectly removed")
	}
}

// ── cleanProjectConfig ───────────────────────────────────────────────────────

func TestCleanProjectConfig(t *testing.T) {
	dir := t.TempDir()

	// Create synapses.json and .synapses/ directory.
	os.WriteFile(filepath.Join(dir, "synapses.json"), []byte(`{}`), 0o644)
	os.MkdirAll(filepath.Join(dir, ".synapses", "prompts"), 0o755)
	os.WriteFile(filepath.Join(dir, ".synapses", "prompts", "test.md"), []byte("test"), 0o644)

	cleanProjectConfig(dir)

	if _, err := os.Stat(filepath.Join(dir, "synapses.json")); err == nil {
		t.Error("synapses.json should be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, ".synapses")); err == nil {
		t.Error(".synapses/ directory should be removed")
	}
}

// ── cleanEmptyParentDirs ─────────────────────────────────────────────────────

func TestCleanEmptyParentDirs(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b")
	os.MkdirAll(nested, 0o755)
	file := filepath.Join(nested, "file.txt")

	// Simulate the file was already removed.
	cleanEmptyParentDirs(file, 2)

	// Both "b" and "a" should be removed since they're empty.
	if _, err := os.Stat(filepath.Join(dir, "a", "b")); err == nil {
		t.Error("empty b/ should be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); err == nil {
		t.Error("empty a/ should be removed")
	}
}

// ── hashPath ─────────────────────────────────────────────────────────────────

func TestHashPath_Deterministic(t *testing.T) {
	h1 := hashPath("/some/project")
	h2 := hashPath("/some/project")
	if h1 != h2 {
		t.Error("hashPath should be deterministic")
	}
	h3 := hashPath("/other/project")
	if h1 == h3 {
		t.Error("different paths should produce different hashes")
	}
	if len(h1) != 16 {
		t.Errorf("expected 16 hex chars, got %d", len(h1))
	}
}
