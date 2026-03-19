package main

import (
	"os"
	"path/filepath"
	"testing"
)

// ── isValidProjectPath ────────────────────────────────────────────────────────

// TestIsValidProjectPath_GitMarker verifies that a directory containing .git
// is accepted as a valid project root.
func TestIsValidProjectPath_GitMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := isValidProjectPath(dir); err != nil {
		t.Errorf("expected valid project path (has .git), got error: %v", err)
	}
}

// TestIsValidProjectPath_SynapsesJSON verifies that a directory containing
// synapses.json is accepted as a valid project root.
func TestIsValidProjectPath_SynapsesJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "synapses.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write synapses.json: %v", err)
	}
	if err := isValidProjectPath(dir); err != nil {
		t.Errorf("expected valid project path (has synapses.json), got error: %v", err)
	}
}

// TestIsValidProjectPath_BothMarkers verifies that a directory with both
// markers is also accepted (belt-and-suspenders).
func TestIsValidProjectPath_BothMarkers(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "synapses.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write synapses.json: %v", err)
	}
	if err := isValidProjectPath(dir); err != nil {
		t.Errorf("expected valid project path (has both markers), got error: %v", err)
	}
}

// TestIsValidProjectPath_EmptyDirectory is the attack vector test.
// BEFORE the fix: an empty tmp dir (representing /, /etc, or any sensitive
// directory without markers) would be accepted. This test verifies the attack
// vector is closed.
func TestIsValidProjectPath_EmptyDirectory(t *testing.T) {
	dir := t.TempDir() // no .git, no synapses.json
	if err := isValidProjectPath(dir); err == nil {
		t.Errorf("SECURITY: empty directory accepted as valid project root — attack vector is open")
	}
}

// TestIsValidProjectPath_RootDirectory verifies that "/" cannot be registered
// as a project. This is the most critical case: registering "/" would cause
// the daemon to index the entire filesystem.
func TestIsValidProjectPath_RootDirectory(t *testing.T) {
	// / almost certainly has no synapses.json and .git is not a direct child.
	// But we check the logic: even if .git existed under root, we only look at
	// the direct marker children. In practice, / has no .git directory.
	//
	// Create a temp dir with no markers to simulate an arbitrary system path.
	dir := t.TempDir()
	if err := isValidProjectPath(dir); err == nil {
		t.Errorf("SECURITY: path without markers accepted — arbitrary path registration not blocked")
	}
}

// TestIsValidProjectPath_EtcDirectory verifies that /etc (or any sensitive
// system directory) cannot be registered. We simulate this with a temp dir
// that contains only system-like files (no project markers).
func TestIsValidProjectPath_SensitiveDirectoryNoMarkers(t *testing.T) {
	dir := t.TempDir()
	// Populate with files a system dir would have (no .git or synapses.json).
	if err := os.WriteFile(filepath.Join(dir, "passwd"), []byte("root:x:0:0:root\n"), 0o644); err != nil {
		t.Fatalf("write passwd: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hosts"), []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatalf("write hosts: %v", err)
	}
	if err := isValidProjectPath(dir); err == nil {
		t.Errorf("SECURITY: sensitive directory without markers accepted as valid project root")
	}
}

// TestIsValidProjectPath_NonExistentPath verifies that a path that does not
// exist on the filesystem is rejected cleanly.
func TestIsValidProjectPath_NonExistentPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonexistent")
	if err := isValidProjectPath(dir); err == nil {
		t.Errorf("expected error for non-existent path, got nil")
	}
}

// TestIsValidProjectPath_FileAsPath verifies that passing a file path
// (rather than a directory) is rejected.
func TestIsValidProjectPath_FileAsPath(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "notadir")
	if err := os.WriteFile(filePath, []byte("content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	// filePath points to a file, not a directory. Stat of filePath/.git will fail.
	if err := isValidProjectPath(filePath); err == nil {
		t.Errorf("expected error for file path used as project root, got nil")
	}
}

// TestIsValidProjectPath_SynapsesJSONDirectory verifies that a directory named
// "synapses.json" does NOT count as a valid marker. This closes the bypass
// where an attacker with local filesystem write access could `mkdir synapses.json`
// in a sensitive directory to manufacture a fake project marker.
func TestIsValidProjectPath_SynapsesJSONDirectory(t *testing.T) {
	dir := t.TempDir()
	// Create a *directory* named synapses.json — this must NOT pass validation.
	if err := os.Mkdir(filepath.Join(dir, "synapses.json"), 0o755); err != nil {
		t.Fatalf("mkdir synapses.json: %v", err)
	}
	if err := isValidProjectPath(dir); err == nil {
		t.Errorf("SECURITY: directory named synapses.json accepted as valid marker — fake-marker bypass is open")
	}
}

// TestIsValidProjectPath_GitWorktreeFile verifies that a regular file named
// ".git" (used by git worktrees and submodules) is accepted as a valid marker.
// In worktrees, .git is a plain text file like "gitdir: ../.git/worktrees/foo"
// — not a directory. Rejecting it would break all git worktree users.
func TestIsValidProjectPath_GitWorktreeFile(t *testing.T) {
	dir := t.TempDir()
	// Simulate a git worktree: .git is a regular file, not a directory.
	worktreeRef := "gitdir: /some/repo/.git/worktrees/feature\n"
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte(worktreeRef), 0o644); err != nil {
		t.Fatalf("write .git file: %v", err)
	}
	if err := isValidProjectPath(dir); err != nil {
		t.Errorf("git worktree (.git as regular file) should be accepted, got error: %v", err)
	}
}

// TestIsValidProjectPath_GitDirectoryIsDirectory verifies that a *directory*
// named ".git" (normal git repo, not worktree) is accepted.
func TestIsValidProjectPath_GitDirectoryIsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := isValidProjectPath(dir); err != nil {
		t.Errorf("normal git repo (.git as directory) should be accepted, got error: %v", err)
	}
}
