// uninstall.go — "synapses uninstall" complete removal wizard.
//
// The inverse of "synapses init". Stops the daemon, removes agent configs,
// cleans indexes, and optionally removes ~/.synapses and the binary itself.
//
// Usage:
//
//	synapses uninstall                       Interactive project cleanup
//	synapses uninstall --path /my/project    Target a specific project
//	synapses uninstall --yes                 Non-interactive (skip prompts)
//	synapses uninstall --global              Full system removal (all data + binary)
//	synapses uninstall --keep-data           Preserve index cache
//	synapses uninstall --keep-binary         Preserve the synapses binary
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/charmbracelet/huh"

	"github.com/SynapsesOS/synapses/internal/store"
)

// cmdUninstall is the complete removal wizard — the inverse of init.
func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Project root to clean (default: current directory)")
	var yes bool
	fs.BoolVar(&yes, "yes", false, "Non-interactive mode — skip all prompts")
	fs.BoolVar(&yes, "y", false, "Non-interactive mode (shorthand)")
	global := fs.Bool("global", false, "Full system cleanup: all indexes, ~/.synapses, services, and binary")
	keepData := fs.Bool("keep-data", false, "Preserve index cache (only remove agent configs)")
	keepBinary := fs.Bool("keep-binary", false, "Preserve the synapses binary")
	if err := fs.Parse(args); err != nil {
		return err
	}

	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	interactive := isInteractive() && !yes

	// ── Header ───────────────────────────────────────────────────────────
	fmt.Println()
	fmt.Printf("  \033[1mSynapses %s\033[0m — Uninstall\n", version)
	fmt.Println()

	if *global {
		return uninstallGlobal(interactive, *keepData, *keepBinary)
	}
	return uninstallProject(absPath, interactive, *keepData)
}

// ── Project-level uninstall ──────────────────────────────────────────────────

func uninstallProject(absPath string, interactive, keepData bool) error {
	fmt.Printf("  Project: %s\n", absPath)
	fmt.Println()

	if interactive {
		var proceed bool
		form := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Remove Synapses from this project?").
					Description("Removes agent configs, synapses.json, and project index.\nSource code is never modified or deleted.").
					Affirmative("Yes, remove").
					Negative("Cancel").
					Value(&proceed),
			),
		)
		if err := form.Run(); err != nil || !proceed {
			fmt.Println("  Cancelled.")
			return nil
		}
		fmt.Println()
	}

	// 1. Remove agent config files written by "synapses connect" / "synapses init".
	fmt.Println("  \033[1m[1/4] Cleaning agent configs\033[0m")
	cleanAgentConfigs(absPath)
	fmt.Println()

	// 2. Remove synapses.json and .synapses/ project directory.
	fmt.Println("  \033[1m[2/4] Removing project config\033[0m")
	cleanProjectConfig(absPath)
	fmt.Println()

	// 3. Remove cached index for this project.
	fmt.Println("  \033[1m[3/4] Removing project index\033[0m")
	if keepData {
		fmt.Println("  Skipped (--keep-data)")
	} else {
		cleanProjectIndex(absPath)
	}
	fmt.Println()

	// 4. Deregister from the daemon's projects.json.
	fmt.Println("  \033[1m[4/4] Deregistering project\033[0m")
	removeKnownProject(absPath)
	fmt.Printf("  \033[32m✓\033[0m Removed from projects registry\n")
	fmt.Println()

	fmt.Println("  \033[32mDone.\033[0m Synapses has been removed from this project.")
	fmt.Println("  Your source code was not modified or deleted.")
	fmt.Println()
	return nil
}

// ── Global uninstall ─────────────────────────────────────────────────────────

func uninstallGlobal(interactive, keepData, keepBinary bool) error {
	fmt.Println("  \033[33mScope: GLOBAL\033[0m — this will remove Synapses from your entire system.")
	fmt.Println()

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home directory: %w", err)
	}
	synHome := filepath.Join(home, ".synapses")

	if interactive {
		var proceed bool
		desc := "This will:\n" +
			"  • Stop the daemon and remove system services\n" +
			"  • Remove ALL cached indexes\n" +
			"  • Delete ~/.synapses (logs, models, prompts, skills, projects)\n" +
			"  • Remove the synapses binary"
		if keepData {
			desc = strings.Replace(desc, "  • Remove ALL cached indexes\n", "", 1)
		}
		if keepBinary {
			desc = strings.Replace(desc, "  • Remove the synapses binary", "  • Keep the synapses binary", 1)
		}
		form := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Remove Synapses from this system?").
					Description(desc + "\n\nSource code in your projects is never modified or deleted.").
					Affirmative("Yes, remove everything").
					Negative("Cancel").
					Value(&proceed),
			),
		)
		if err := form.Run(); err != nil || !proceed {
			fmt.Println("  Cancelled.")
			return nil
		}
		fmt.Println()
	}

	// 1. Stop daemon and kill all synapses processes.
	fmt.Println("  \033[1m[1/5] Stopping daemon\033[0m")
	if err := cmdStop(nil); err != nil {
		fmt.Printf("  \033[33m!\033[0m %v\n", err)
	} else {
		fmt.Printf("  \033[32m✓\033[0m Daemon stopped\n")
	}
	killAllSynapsesProcesses()
	fmt.Println()

	// 2. Remove system services (launchd / systemd).
	fmt.Println("  \033[1m[2/5] Removing system services\033[0m")
	if err := daemonUninstall(); err != nil {
		fmt.Printf("  \033[33m!\033[0m %v\n", err)
	} else {
		fmt.Printf("  \033[32m✓\033[0m System services removed\n")
	}
	fmt.Println()

	// 3. Remove binary (before ~/.synapses so install.sh binaries are removed cleanly).
	fmt.Println("  \033[1m[3/5] Removing binary\033[0m")
	if keepBinary {
		fmt.Println("  Skipped (--keep-binary)")
	} else {
		removeBinary()
	}
	fmt.Println()

	// 4. Remove all cached indexes.
	fmt.Println("  \033[1m[4/5] Removing cached indexes\033[0m")
	if keepData {
		fmt.Println("  Skipped (--keep-data)")
	} else {
		cacheDir, err := store.CacheDir()
		if err == nil {
			if stats, _ := store.ScanAll(); len(stats) > 0 {
				for _, s := range stats {
					root := s.RepoRoot
					if root == "" {
						root = s.RepoID
					}
					fmt.Printf("  \033[32m✓\033[0m Removed index: %s\n", root)
				}
			}
			if err := os.RemoveAll(cacheDir); err != nil {
				fmt.Printf("  \033[33m!\033[0m Failed to remove cache: %v\n", err)
			} else {
				fmt.Printf("  \033[32m✓\033[0m Cache directory removed\n")
			}
		} else {
			fmt.Printf("  \033[33m!\033[0m Cannot locate cache directory: %v\n", err)
		}
	}
	fmt.Println()

	// 5. Remove ~/.synapses (logs, models, pids, context, prompts, skills, projects.json).
	fmt.Println("  \033[1m[5/5] Removing ~/.synapses\033[0m")
	if _, err := os.Stat(synHome); err == nil {
		// List what's being removed for transparency.
		entries, _ := os.ReadDir(synHome)
		for _, e := range entries {
			if keepData && e.Name() == "cache" {
				continue
			}
			fmt.Printf("  \033[32m✓\033[0m %s/\n", e.Name())
		}

		if keepData {
			// Remove everything except the cache directory.
			for _, e := range entries {
				if e.Name() == "cache" {
					continue
				}
				os.RemoveAll(filepath.Join(synHome, e.Name()))
			}
			fmt.Printf("  \033[32m✓\033[0m ~/.synapses cleaned (cache preserved)\n")
		} else {
			if err := os.RemoveAll(synHome); err != nil {
				fmt.Printf("  \033[31m✗\033[0m Failed to remove ~/.synapses: %v\n", err)
			} else {
				fmt.Printf("  \033[32m✓\033[0m ~/.synapses removed\n")
			}
		}
	} else {
		fmt.Println("  ~/.synapses does not exist — nothing to remove")
	}
	fmt.Println()

	fmt.Println("  ═══════════════════════════════════════════════════════════")
	fmt.Println("  \033[32mSynapses has been completely removed.\033[0m")
	fmt.Println("  Your source code was never modified or deleted.")
	fmt.Println()
	if keepBinary {
		fmt.Println("  To reinstall: synapses init --path <dir>")
	} else {
		fmt.Println("  To reinstall: curl -fsSL https://synapses.sh/install | sh")
	}
	fmt.Println("  ═══════════════════════════════════════════════════════════")
	fmt.Println()
	return nil
}

// ── Agent config cleanup ─────────────────────────────────────────────────────

// cleanAgentConfigs removes Synapses entries from all agent config files
// in the project directory. It does NOT delete the entire agent config —
// only the synapses-specific entries, preserving other MCP servers.
func cleanAgentConfigs(absPath string) {
	// Claude Code
	cleanMCPServerEntry(filepath.Join(absPath, ".mcp.json"), "claude")
	cleanSynapsesSection(filepath.Join(absPath, ".claude", "CLAUDE.md"), "claude")
	// Legacy: older versions wrote the synapses section into root CLAUDE.md
	// before migrating to .claude/CLAUDE.md. Clean both locations.
	cleanSynapsesSection(filepath.Join(absPath, "CLAUDE.md"), "claude")
	cleanClaudeSettings(filepath.Join(absPath, ".claude", "settings.json"))

	// Cursor
	cleanMCPServerEntry(filepath.Join(absPath, ".cursor", "mcp.json"), "cursor")
	cleanFile(filepath.Join(absPath, ".cursor", "rules", "synapses.mdc"), "cursor")

	// Windsurf
	cleanMCPServerEntry(filepath.Join(absPath, ".windsurf", "mcp_config.json"), "windsurf")
	cleanSynapsesSection(filepath.Join(absPath, ".windsurfrules"), "windsurf")

	// Zed
	cleanZedMCPConfig(filepath.Join(absPath, ".zed", "settings.json"))

	// Antigravity
	cleanMCPServerEntry(filepath.Join(absPath, ".agent", "mcp.json"), "antigravity")
	cleanFile(filepath.Join(absPath, ".agent", "rules", "synapses.md"), "antigravity")
}

// cleanMCPServerEntry removes the "synapses" key from a JSON file with
// { "mcpServers": { "synapses": { ... } } } structure. Only removes the
// synapses entry — all other servers and top-level keys are preserved.
// The file is only deleted if nothing remains after removal.
func cleanMCPServerEntry(file, agent string) {
	data, err := os.ReadFile(file)
	if err != nil {
		return // file doesn't exist — nothing to clean
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}

	servers, ok := raw["mcpServers"].(map[string]interface{})
	if !ok {
		return
	}

	if _, exists := servers["synapses"]; !exists {
		return
	}

	delete(servers, "synapses")

	// Remove the mcpServers key entirely if no servers remain.
	if len(servers) == 0 {
		delete(raw, "mcpServers")
	}

	// Only delete the file if the entire JSON object is now empty.
	// Other top-level keys (e.g. "settings", user properties) must be preserved.
	if len(raw) == 0 {
		os.Remove(file)
		cleanEmptyParentDirs(file, 1)
		fmt.Printf("  \033[32m✓\033[0m %s removed (%s)\n", relDisplay(file), agent)
		return
	}

	// Write back preserving all remaining content.
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(file, append(out, '\n'), 0o644) //nolint:errcheck
	fmt.Printf("  \033[32m✓\033[0m %s — synapses entry removed (%s)\n", relDisplay(file), agent)
}

// cleanZedMCPConfig removes the "synapses" entry from .zed/settings.json
// context_servers. Preserves other Zed settings.
func cleanZedMCPConfig(file string) {
	data, err := os.ReadFile(file)
	if err != nil {
		return
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}

	cs, ok := raw["context_servers"].(map[string]interface{})
	if !ok {
		return
	}

	if _, exists := cs["synapses"]; !exists {
		return
	}

	delete(cs, "synapses")

	if len(cs) == 0 {
		delete(raw, "context_servers")
	}

	// Only remove the file if it's completely empty now.
	if len(raw) == 0 {
		os.Remove(file)
		cleanEmptyParentDirs(file, 1)
		fmt.Printf("  \033[32m✓\033[0m %s removed (zed)\n", relDisplay(file))
		return
	}

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(file, append(out, '\n'), 0o644) //nolint:errcheck
	fmt.Printf("  \033[32m✓\033[0m %s — synapses entry removed (zed)\n", relDisplay(file))
}

// cleanSynapsesSection removes the <!-- synapses:start --> ... <!-- synapses:end -->
// block from a text file (CLAUDE.md, .windsurfrules, etc.).
// Only removes content between the sentinel markers — all other user content
// is preserved exactly as-is, including surrounding whitespace and formatting.
// The file is only deleted if no user content remains outside the markers.
func cleanSynapsesSection(file, agent string) {
	data, err := os.ReadFile(file)
	if err != nil {
		return
	}

	content := string(data)
	startIdx := strings.Index(content, synapsesSectionStart)
	if startIdx == -1 {
		return
	}

	endIdx := strings.Index(content, synapsesSectionEnd)
	if endIdx == -1 {
		return
	}

	// Remove the section including the markers, but preserve everything
	// before and after. Collapse at most one extra blank line at the seam
	// to avoid leaving a double-blank-line gap, but don't strip user whitespace.
	before := content[:startIdx]
	after := content[endIdx+len(synapsesSectionEnd):]

	// Trim trailing newlines from "before" and leading newlines from "after"
	// to collapse the gap left by the removed block, then rejoin with a
	// single blank line if both sides have content.
	beforeTrimmed := strings.TrimRight(before, "\n")
	afterTrimmed := strings.TrimLeft(after, "\n")

	var cleaned string
	switch {
	case beforeTrimmed == "" && afterTrimmed == "":
		cleaned = ""
	case beforeTrimmed == "":
		cleaned = afterTrimmed + "\n"
	case afterTrimmed == "":
		cleaned = beforeTrimmed + "\n"
	default:
		cleaned = beforeTrimmed + "\n\n" + afterTrimmed + "\n"
	}

	if strings.TrimSpace(cleaned) == "" {
		os.Remove(file)
		cleanEmptyParentDirs(file, 1)
		fmt.Printf("  \033[32m✓\033[0m %s removed (%s)\n", relDisplay(file), agent)
	} else {
		os.WriteFile(file, []byte(cleaned), 0o644) //nolint:errcheck
		fmt.Printf("  \033[32m✓\033[0m %s — synapses section removed (%s)\n", relDisplay(file), agent)
	}
}

// cleanClaudeSettings removes synapses-specific hooks and permissions from
// .claude/settings.json. Only removes entries that Synapses wrote — all
// user-defined hooks, permissions, and other settings are preserved exactly.
//
// Identification strategy (matches what writeClaudeSettings creates):
//   - Hooks: entries whose nested hook commands contain "[Synapses]" (our
//     marker text) or reference "/.synapses/" paths. We inspect the inner
//     "hooks" array, not just the top-level "command" field, because Claude
//     Code settings use { "matcher": "...", "hooks": [{ "command": "..." }] }.
//   - Permissions: only entries matching "mcp__synapses__*" exactly.
func cleanClaudeSettings(file string) {
	data, err := os.ReadFile(file)
	if err != nil {
		return
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}

	changed := false

	// Remove synapses hook entries by inspecting their inner commands.
	if hooks, ok := raw["hooks"].(map[string]interface{}); ok {
		var eventsToDelete []string
		for event, val := range hooks {
			entries, ok := val.([]interface{})
			if !ok {
				continue
			}
			var filtered []interface{}
			for _, entry := range entries {
				if isSynapsesHookEntry(entry) {
					changed = true
					continue
				}
				filtered = append(filtered, entry)
			}
			if len(filtered) == 0 && len(entries) > 0 {
				eventsToDelete = append(eventsToDelete, event)
				changed = true
			} else if len(filtered) != len(entries) {
				hooks[event] = filtered
			}
		}
		for _, event := range eventsToDelete {
			delete(hooks, event)
		}
		if len(hooks) == 0 {
			delete(raw, "hooks")
		}
	}

	// Remove only "mcp__synapses__*" from permissions allow list.
	if perms, ok := raw["permissions"].(map[string]interface{}); ok {
		if allow, ok := perms["allow"].([]interface{}); ok {
			var filtered []interface{}
			for _, p := range allow {
				s, ok := p.(string)
				if ok && isSynapsesPermission(s) {
					changed = true
					continue
				}
				filtered = append(filtered, p)
			}
			if len(filtered) == 0 && len(allow) > 0 {
				delete(perms, "allow")
			} else if len(filtered) != len(allow) {
				perms["allow"] = filtered
			}
		}
		// Only remove permissions key if truly empty (no "allow", no "deny", etc.).
		if len(perms) == 0 {
			delete(raw, "permissions")
		}
	}

	if !changed {
		return
	}

	// Only delete the file if the entire JSON object is empty.
	if len(raw) == 0 {
		os.Remove(file)
		cleanEmptyParentDirs(file, 1)
		fmt.Printf("  \033[32m✓\033[0m %s removed (claude)\n", relDisplay(file))
		return
	}

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(file, append(out, '\n'), 0o644) //nolint:errcheck
	fmt.Printf("  \033[32m✓\033[0m %s — synapses entries removed (claude)\n", relDisplay(file))
}

// isSynapsesHookEntry returns true if a hook entry was written by Synapses.
// It checks the nested hooks[].command fields for our specific markers:
//   - "[Synapses]" — the branded prefix in all our echo/cat commands
//   - "/.synapses/" — references to the ~/.synapses/ data directory
//
// This avoids false positives: a user hook mentioning "synapses" in prose
// won't match unless it also uses our exact "[Synapses]" bracket marker
// or references our dotfile directory.
func isSynapsesHookEntry(entry interface{}) bool {
	m, ok := entry.(map[string]interface{})
	if !ok {
		return false
	}

	// Claude Code hook structure: { "matcher": "...", "hooks": [{ "command": "..." }] }
	innerHooks, ok := m["hooks"].([]interface{})
	if !ok {
		return false
	}

	for _, h := range innerHooks {
		hm, ok := h.(map[string]interface{})
		if !ok {
			continue
		}
		cmd, _ := hm["command"].(string)
		if strings.Contains(cmd, "[Synapses]") || strings.Contains(cmd, "/.synapses/") {
			return true
		}
	}
	return false
}

// isSynapsesPermission returns true if a permission string was added by Synapses.
// Only matches the exact pattern we write: "mcp__synapses__*" or any entry
// starting with "mcp__synapses__".
func isSynapsesPermission(s string) bool {
	return s == "mcp__synapses__*" || strings.HasPrefix(s, "mcp__synapses__")
}

// cleanFile removes a synapses-specific file entirely (e.g. synapses.mdc).
func cleanFile(file, agent string) {
	if _, err := os.Stat(file); err != nil {
		return
	}
	os.Remove(file)
	cleanEmptyParentDirs(file, 2)
	fmt.Printf("  \033[32m✓\033[0m %s removed (%s)\n", relDisplay(file), agent)
}

// ── Project config cleanup ───────────────────────────────────────────────────

func cleanProjectConfig(absPath string) {
	// synapses.json
	jsonPath := filepath.Join(absPath, "synapses.json")
	if _, err := os.Stat(jsonPath); err == nil {
		os.Remove(jsonPath)
		fmt.Printf("  \033[32m✓\033[0m synapses.json removed\n")
	}

	// .synapses/ project directory (prompts, skills).
	dotDir := filepath.Join(absPath, ".synapses")
	if _, err := os.Stat(dotDir); err == nil {
		os.RemoveAll(dotDir)
		fmt.Printf("  \033[32m✓\033[0m .synapses/ directory removed\n")
	}

	// Context file for this project.
	home, err := os.UserHomeDir()
	if err == nil {
		ctxDir := filepath.Join(home, ".synapses", "context")
		if entries, err := os.ReadDir(ctxDir); err == nil {
			for _, e := range entries {
				if strings.Contains(e.Name(), hashPath(absPath)) {
					os.Remove(filepath.Join(ctxDir, e.Name()))
					fmt.Printf("  \033[32m✓\033[0m Context file removed\n")
				}
			}
		}
	}
}

// hashPath returns the FNV-1a hash of a path, matching store.DefaultPath's naming.
func hashPath(p string) string {
	h := uint64(14695981039346656037)
	for _, c := range []byte(p) {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return fmt.Sprintf("%016x", h)
}

// ── Index cleanup ────────────────────────────────────────────────────────────

func cleanProjectIndex(absPath string) {
	dbPath, err := store.DefaultPath(absPath)
	if err != nil {
		fmt.Printf("  \033[33m!\033[0m Cannot locate index: %v\n", err)
		return
	}

	if _, err := os.Stat(dbPath); err != nil {
		fmt.Println("  No cached index found")
		return
	}

	if err := os.Remove(dbPath); err != nil {
		fmt.Printf("  \033[31m✗\033[0m Failed to remove index: %v\n", err)
	} else {
		fmt.Printf("  \033[32m✓\033[0m Index removed: %s\n", dbPath)
	}

	// Also remove WAL/SHM files if present.
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")
}

// ── Binary removal ───────────────────────────────────────────────────────────

func removeBinary() {
	self, err := os.Executable()
	if err != nil {
		fmt.Printf("  \033[33m!\033[0m Cannot determine binary path: %v\n", err)
		return
	}
	// Resolve symlinks to find the actual binary (e.g. Homebrew Cellar path).
	resolved, err := filepath.EvalSymlinks(self)
	if err == nil {
		self = resolved
	}

	fmt.Printf("  Binary: %s\n", self)

	// On macOS, also check for the app bundle.
	if runtime.GOOS == "darwin" {
		removeAppBundle()
	}

	// Detect Homebrew install and use brew uninstall for clean removal.
	if isHomebrewInstall(self) {
		fmt.Println("  Detected Homebrew installation")
		if out, err := exec.Command("brew", "uninstall", "--force", "synapses").CombinedOutput(); err != nil {
			fmt.Printf("  \033[33m!\033[0m brew uninstall failed: %s\n", strings.TrimSpace(string(out)))
		} else {
			fmt.Printf("  \033[32m✓\033[0m brew uninstall synapses\n")
		}
		// Also untap if present.
		if out, err := exec.Command("brew", "untap", "synapsesos/tap").CombinedOutput(); err == nil {
			fmt.Printf("  \033[32m✓\033[0m brew untap synapsesos/tap\n")
		} else {
			_ = out // tap may not exist, that's fine
		}
	} else {
		// Direct removal for non-Homebrew installs.
		if err := os.Remove(self); err != nil {
			if !os.IsNotExist(err) {
				fmt.Printf("  \033[33m!\033[0m Cannot remove binary: %v\n", err)
				fmt.Printf("  You may need to remove it manually: sudo rm %s\n", self)
			}
		} else {
			fmt.Printf("  \033[32m✓\033[0m Binary removed\n")
		}
	}

	// Sweep all known locations for stale copies.
	removeStaleBindaries(self)
}

// isHomebrewInstall checks if the binary path is inside a Homebrew Cellar.
func isHomebrewInstall(binPath string) bool {
	return strings.Contains(binPath, "/Cellar/synapses/") ||
		strings.Contains(binPath, "/homebrew/")
}

// removeStaleBindaries scans well-known paths for leftover synapses binaries
// and removes them. This catches cases where users installed via multiple
// methods (e.g. brew + go install) or where a symlink points to a removed target.
func removeStaleBindaries(alreadyRemoved string) {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".synapses", "bin", "synapses"),
		filepath.Join(home, "go", "bin", "synapses"),
		"/usr/local/bin/synapses",
	}
	// Also check what `which` finds (may be different from os.Executable).
	if out, err := exec.Command("which", "synapses").Output(); err == nil {
		found := strings.TrimSpace(string(out))
		if found != "" {
			candidates = append(candidates, found)
		}
	}

	seen := map[string]bool{alreadyRemoved: true}
	for _, p := range candidates {
		// Resolve to canonical path for dedup.
		canonical := p
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			canonical = resolved
		}
		if seen[canonical] || seen[p] {
			continue
		}
		seen[canonical] = true
		seen[p] = true

		fi, err := os.Lstat(p)
		if err != nil {
			continue // doesn't exist
		}
		// Remove dangling symlinks.
		if fi.Mode()&os.ModeSymlink != 0 {
			if _, err := os.Stat(p); err != nil {
				// Dangling symlink.
				os.Remove(p)
				fmt.Printf("  \033[32m✓\033[0m Removed stale symlink: %s\n", p)
				continue
			}
		}
		// Remove actual binary.
		if err := os.Remove(p); err == nil {
			fmt.Printf("  \033[32m✓\033[0m Removed stale binary: %s\n", p)
		}
	}
}

// killAllSynapsesProcesses kills any remaining synapses processes (orphan
// daemons, proxy processes) that weren't caught by the PID-based stop.
// Skips the current process.
func killAllSynapsesProcesses() {
	myPID := os.Getpid()
	// Use pkill to find synapses processes. Ignore errors (no matches = exit 1).
	for _, pattern := range []string{"synapses daemon serve", "synapses start"} {
		out, _ := exec.Command("pgrep", "-f", pattern).Output()
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			pid := 0
			if _, err := fmt.Sscanf(line, "%d", &pid); err != nil || pid == myPID || pid == 0 {
				continue
			}
			if proc, err := os.FindProcess(pid); err == nil {
				proc.Signal(syscall.SIGKILL)
				fmt.Printf("  \033[32m✓\033[0m Killed orphan process %d (%s)\n", pid, pattern)
			}
		}
	}
}

// removeAppBundle checks for and removes the Synapses macOS app bundle
// at /Applications/Synapses.app.
func removeAppBundle() {
	appPaths := []string{
		"/Applications/Synapses.app",
		filepath.Join(os.Getenv("HOME"), "Applications", "Synapses.app"),
	}
	for _, app := range appPaths {
		if _, err := os.Stat(app); err == nil {
			if err := os.RemoveAll(app); err != nil {
				fmt.Printf("  \033[33m!\033[0m Cannot remove %s: %v\n", app, err)
				fmt.Printf("  You may need to remove it manually: sudo rm -rf %q\n", app)
			} else {
				fmt.Printf("  \033[32m✓\033[0m %s removed\n", app)
			}
		}
	}
}

// ── Utility helpers ──────────────────────────────────────────────────────────

// relDisplay returns a displayable relative path from pwd, or the absolute path.
func relDisplay(path string) string {
	if pwd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(pwd, path); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return path
}

// cleanEmptyParentDirs removes empty parent directories up to maxLevels.
// This cleans up directories like .cursor/rules/ after removing synapses.mdc.
func cleanEmptyParentDirs(file string, maxLevels int) {
	dir := filepath.Dir(file)
	for i := 0; i < maxLevels; i++ {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			break
		}
		os.Remove(dir)
		dir = filepath.Dir(dir)
	}
}
