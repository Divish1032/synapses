package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// allowlistFileName is the per-machine file that stores approved plugin commands.
const allowlistFileName = "allowed_plugins.json"

// allowlistEnvOverride bypasses the allowlist when set to "1".
// Intended for CI/testing environments where the user controls all configs.
const allowlistEnvOverride = "SYNAPSES_ALLOW_ALL_PLUGINS"

// pluginAllowlist is the JSON structure persisted at ~/.synapses/allowed_plugins.json.
type pluginAllowlist struct {
	// Approved maps SHA-256 hex hashes of approved command strings to the
	// original command string (for display purposes only — the hash is
	// authoritative).
	Approved map[string]string `json:"approved"`
}

// PluginChecker validates whether a plugin command is allowed to execute.
// It reads the allowlist from the user's home directory (~/.synapses/).
type PluginChecker struct {
	synapsesDir    string
	mu             sync.Mutex
	loaded         bool
	approved       map[string]string // hash → original command
	envWarnLogged  bool
}

// NewPluginChecker creates a checker that reads allowlists from synapsesDir.
// synapsesDir is typically ~/.synapses.
func NewPluginChecker(synapsesDir string) *PluginChecker {
	return &PluginChecker{
		synapsesDir: synapsesDir,
	}
}

// IsAllowed checks whether a plugin command is approved for execution.
// Returns nil if allowed, a descriptive error if not.
func (pc *PluginChecker) IsAllowed(command string) error {
	// Environment override for CI/testing — only honoured when running
	// automated tests (SYNAPSES_TEST=1). A .env file sourced by a
	// malicious agent cannot enable this bypass without also controlling
	// the test flag (BUG-032).
	if os.Getenv(allowlistEnvOverride) == "1" && os.Getenv("SYNAPSES_TEST") == "1" {
		pc.mu.Lock()
		if !pc.envWarnLogged {
			logutil.Warn("%s=1 — all plugin parsers are allowed (test bypass mode)\n", allowlistEnvOverride)
			pc.envWarnLogged = true
		}
		pc.mu.Unlock()
		return nil
	}

	pc.mu.Lock()
	defer pc.mu.Unlock()

	if !pc.loaded {
		pc.loadAllowlist()
		pc.loaded = true
	}

	hash := commandHash(command)
	if _, ok := pc.approved[hash]; ok {
		return nil
	}

	return fmt.Errorf("plugin command not approved: %q\n"+
		"  To approve, run: synapses allow-plugin %s\n"+
		"  Or set SYNAPSES_ALLOW_ALL_PLUGINS=1 to bypass (CI/testing only)",
		command, shellescape(command))
}

// ApproveCommand adds a command to the per-machine allowlist.
func (pc *PluginChecker) ApproveCommand(command string) error {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	// Reload to avoid overwriting concurrent approvals.
	pc.loadAllowlist()

	hash := commandHash(command)
	pc.approved[hash] = command

	return pc.saveAllowlist()
}

// RevokeCommand removes a command from the per-machine allowlist.
func (pc *PluginChecker) RevokeCommand(command string) error {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	pc.loadAllowlist()

	hash := commandHash(command)
	delete(pc.approved, hash)

	return pc.saveAllowlist()
}

// ListApproved returns all currently approved commands.
func (pc *PluginChecker) ListApproved() map[string]string {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	pc.loadAllowlist()

	out := make(map[string]string, len(pc.approved))
	for k, v := range pc.approved {
		out[k] = v
	}
	return out
}

// loadAllowlist reads the allowlist file. If missing or unparseable, the
// approved map is reset to empty (fail-closed).
func (pc *PluginChecker) loadAllowlist() {
	pc.approved = make(map[string]string)

	path := filepath.Join(pc.synapsesDir, allowlistFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return // file missing → empty allowlist (fail-closed)
	}

	var al pluginAllowlist
	if err := json.Unmarshal(data, &al); err != nil {
		logutil.Warn("failed to parse %s: %v (all plugins rejected)\n", path, err)
		return
	}

	if al.Approved != nil {
		pc.approved = al.Approved
	}
}

// saveAllowlist atomically writes the current approved set to disk.
// Uses write-to-temp-then-rename to prevent corruption on crash.
func (pc *PluginChecker) saveAllowlist() error {
	if err := os.MkdirAll(pc.synapsesDir, 0o700); err != nil {
		return fmt.Errorf("create synapses dir: %w", err)
	}

	al := pluginAllowlist{Approved: pc.approved}
	data, err := json.MarshalIndent(al, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal allowlist: %w", err)
	}
	data = append(data, '\n')

	path := filepath.Join(pc.synapsesDir, allowlistFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp allowlist: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) // clean up on failure
		return fmt.Errorf("rename allowlist: %w", err)
	}
	return nil
}

// commandHash returns the SHA-256 hex digest of a command string.
func commandHash(command string) string {
	h := sha256.Sum256([]byte(command))
	return hex.EncodeToString(h[:])
}

// shellescape does minimal quoting for display in shell instructions.
func shellescape(s string) string {
	if strings.ContainsAny(s, " \t\n\"'\\$`!#&|;(){}[]<>?*~") {
		return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
	}
	return s
}
