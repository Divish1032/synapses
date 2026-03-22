package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// ── ensureProjectMarker ──────────────────────────────────────────────────────

// TestEnsureProjectMarker_GitDir verifies that when a real .git/ directory
// exists, the function returns without creating synapses.json.
func TestEnsureProjectMarker_GitDir(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "init", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	if err := ensureProjectMarker(dir); err != nil {
		t.Fatalf("ensureProjectMarker: %v", err)
	}

	// synapses.json should NOT be created — .git is sufficient marker.
	if _, err := os.Stat(filepath.Join(dir, "synapses.json")); err == nil {
		t.Fatalf("synapses.json should not be created when .git exists")
	}
}

// TestEnsureProjectMarker_GitFile verifies that a .git file (worktree) is
// accepted as a valid marker.
func TestEnsureProjectMarker_GitFile(t *testing.T) {
	dir := t.TempDir()
	// Create a .git file (like a git worktree).
	os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /tmp/some-worktree\n"), 0o644)

	if err := ensureProjectMarker(dir); err != nil {
		t.Fatalf("ensureProjectMarker: %v", err)
	}

	// synapses.json should NOT be created.
	if _, err := os.Stat(filepath.Join(dir, "synapses.json")); err == nil {
		t.Fatalf("synapses.json should not be created when .git file exists")
	}
}

// TestEnsureProjectMarker_SynapsesJSON verifies that existing synapses.json
// prevents creating a new one.
func TestEnsureProjectMarker_SynapsesJSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "synapses.json")
	original := `{"rules": []}` + "\n"
	os.WriteFile(cfgPath, []byte(original), 0o644)

	if err := ensureProjectMarker(dir); err != nil {
		t.Fatalf("ensureProjectMarker: %v", err)
	}

	data, _ := os.ReadFile(cfgPath)
	if string(data) != original {
		t.Fatalf("synapses.json was overwritten:\nbefore: %s\nafter:  %s", original, data)
	}
}

// TestEnsureProjectMarker_CreatesJSON verifies that when no marker exists,
// a minimal synapses.json is created.
func TestEnsureProjectMarker_CreatesJSON(t *testing.T) {
	dir := t.TempDir()

	if err := ensureProjectMarker(dir); err != nil {
		t.Fatalf("ensureProjectMarker: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "synapses.json"))
	if err != nil {
		t.Fatalf("synapses.json not created: %v", err)
	}
	if string(data) != "{}\n" {
		t.Fatalf("unexpected content: %q", data)
	}
}

// TestEnsureProjectMarker_Idempotent verifies calling multiple times is safe.
func TestEnsureProjectMarker_Idempotent(t *testing.T) {
	dir := t.TempDir()

	for i := 0; i < 3; i++ {
		if err := ensureProjectMarker(dir); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	data, _ := os.ReadFile(filepath.Join(dir, "synapses.json"))
	if string(data) != "{}\n" {
		t.Fatalf("unexpected content after 3 calls: %q", data)
	}
}
