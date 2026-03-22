// init.go — "synapses init" interactive setup wizard.
//
// This is the single golden-path command for new users. It replaces the old
// init (stub), onboard (5-step wizard), setup (config writer), and mcp-setup
// (agent auto-discover) commands with one unified interactive flow:
//
//	[1/4] Project Setup  — git init prompt + synapses.json
//	[2/4] Indexing        — parse codebase, build graph
//	[3/4] Starting Engine — ensure singleton daemon is running
//	[4/4] Connect Agents  — multi-select detected AI agents
//
// Non-interactive mode: synapses init --yes --agents claude,cursor
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
	"time"

	"github.com/charmbracelet/huh"
	"github.com/mattn/go-isatty"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/store"
)

// AgentInfo describes a supported AI coding agent.
type AgentInfo struct {
	Key      string // "claude", "cursor", etc.
	Display  string // "Claude Code", "Cursor", etc.
	Detected bool   // whether the agent was found locally
}

// connectResult tracks the outcome of connecting a single agent.
type connectResult struct {
	Agent string
	Files []string
	Err   error
}

// ── cmdInit — the new unified init ──────────────────────────────────────────

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Project root (default: current directory)")
	var yes bool
	fs.BoolVar(&yes, "yes", false, "Non-interactive mode — accept all defaults")
	fs.BoolVar(&yes, "y", false, "Non-interactive mode (shorthand)")
	agentList := fs.String("agents", "", "Comma-separated agents to connect (claude,cursor,windsurf,zed,vscode,antigravity)")
	noAgents := fs.Bool("no-agents", false, "Skip agent connection step")
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
	fmt.Printf("  \033[1mSynapses %s\033[0m — Code intelligence for AI agents\n", version)
	fmt.Printf("  Project: %s\n", absPath)
	fmt.Println()

	// ── Step 1/4: Project Setup ──────────────────────────────────────────
	fmt.Println("  \033[1m[1/4] Project Setup\033[0m")

	if err := initGitIfNeeded(absPath, interactive); err != nil {
		logutil.Warn("  git init: %v\n", err)
	}

	if err := writeSynapsesJSON(absPath); err != nil {
		logutil.Warn("  synapses.json: %v\n", err)
	} else {
		fmt.Printf("  \033[32m✓\033[0m synapses.json ready\n")
	}
	fmt.Println()

	// ── Step 2/4: Indexing ───────────────────────────────────────────────
	fmt.Println("  \033[1m[2/4] Indexing\033[0m")

	if err := runIndexing(absPath); err != nil {
		// Non-fatal: daemon and agent connection should still proceed.
		// The user can re-index later with "synapses index --path <dir>".
		fmt.Printf("  \033[33m!\033[0m Indexing failed: %v\n", err)
		fmt.Println("        Run 'synapses index --path .' later to retry.")
	}
	fmt.Println()

	// ── Step 3/4: Starting Engine ────────────────────────────────────────
	fmt.Println("  \033[1m[3/4] Starting Engine\033[0m")

	if err := ensureProjectMarker(absPath); err != nil {
		logutil.Warn("  project marker: %v\n", err)
	}
	if err := ensureSingletonDaemon(absPath); err != nil {
		logutil.Warn("  daemon: %v\n", err)
	} else {
		fmt.Printf("  \033[32m✓\033[0m Daemon running on %s\n", DaemonHTTPAddr)
	}

	// Hint: install as system service for auto-restart on crash.
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		fmt.Println()
		fmt.Printf("  \033[33mTip:\033[0m Run '\033[1msynapses daemon install\033[0m' to auto-restart the daemon on crash.\n")
		fmt.Println("       (Registers a launchd/systemd service for this machine.)")
	}
	if _, err := registerProjectWithDaemon(absPath); err != nil {
		logutil.Warn("  register project: %v\n", err)
	}
	fmt.Println()

	// ── Step 4/4: Connect Agents ─────────────────────────────────────────
	if !*noAgents {
		fmt.Println("  \033[1m[4/4] Connect Agents\033[0m")

		selected, err := selectAgents(interactive, *agentList)
		if err != nil {
			logutil.Warn("  agent selection: %v\n", err)
		} else if len(selected) > 0 {
			results := connectAgents(absPath, selected)
			for _, r := range results {
				if r.Err != nil {
					fmt.Printf("  \033[31m✗\033[0m %s: %v\n", r.Agent, r.Err)
				} else {
					fmt.Printf("  \033[32m✓\033[0m %s → %s\n", r.Agent, strings.Join(r.Files, ", "))
				}
			}
		} else {
			fmt.Println("  No agents selected.")
		}
		fmt.Println()
	}

	// ── Footer ───────────────────────────────────────────────────────────
	fmt.Println("  ═══════════════════════════════════════════════════════════")
	fmt.Println("  \033[32mSynapses is ready!\033[0m Open your AI agent to start coding.")
	fmt.Println()
	fmt.Println("  Don't see your agent? Any MCP-compatible tool can connect:")
	fmt.Printf("    HTTP:  http://%s/mcp?project=%s\n", DaemonHTTPAddr, absPath)
	fmt.Printf("    Stdio: synapses start --path %s\n", absPath)
	fmt.Println("  ═══════════════════════════════════════════════════════════")
	fmt.Println()

	return nil
}

// ── Step 1 helpers ──────────────────────────────────────────────────────────

// initGitIfNeeded checks for .git and offers to initialize if missing.
func initGitIfNeeded(absPath string, interactive bool) error {
	dotGit := filepath.Join(absPath, ".git")
	if info, err := os.Stat(dotGit); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
		fmt.Println("  \033[32m✓\033[0m Git repository detected")
		return nil
	}

	// Check if git is available.
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Println("  \033[33m!\033[0m git not found — skipping (install git for richer intelligence)")
		return nil
	}

	initGit := true
	if interactive {
		form := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Initialize git repository?").
					Description("Enables: churn analysis, blame tracking, commit history,\nworking state, task linking, drift detection").
					Affirmative("Yes").
					Negative("No").
					Value(&initGit),
			),
		)
		if err := form.Run(); err != nil {
			return nil // user cancelled — skip silently
		}
	}

	if !initGit {
		fmt.Println("  Skipped git. Run 'git init' later to enable these features.")
		return nil
	}

	cmd := exec.Command("git", "init", absPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("  \033[32m✓\033[0m Git repository initialized\n")
	return nil
}

// writeSynapsesJSON creates or updates synapses.json with sensible defaults.
// Idempotent — merges with existing config.
func writeSynapsesJSON(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	cfgPath := filepath.Join(root, "synapses.json")

	existing := map[string]interface{}{}
	if data, err := os.ReadFile(cfgPath); err == nil {
		_ = json.Unmarshal(data, &existing)
	}

	// Populate sensible defaults for sections that are missing.
	// This gives users a working config with discoverable options
	// instead of an opaque `{"brain":{"enabled":false}}`.
	defaults := map[string]interface{}{
		"version": "1",
		"mode":    "full",
		"brain": map[string]interface{}{
			"enabled": false,
		},
		"context_carve": map[string]interface{}{
			"default_depth":      2,
			"token_budget":       4000,
			"exclude_test_files": true,
		},
		"embeddings": "builtin",
		"content_safety": map[string]interface{}{
			"enabled": true,
			"mode":    "reject",
		},
		"session": map[string]interface{}{
			"auto_end_threshold_calls": 80,
			"reconnect_window_secs":    300,
		},
	}
	for key, val := range defaults {
		if _, exists := existing[key]; !exists {
			existing[key] = val
		}
	}
	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfgPath, append(data, '\n'), 0o644)
}

// ── Step 2 helper ───────────────────────────────────────────────────────────

// runIndexing indexes the project codebase.
func runIndexing(absPath string) error {
	cfg, err := config.Load(absPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	start := time.Now()
	fmt.Printf("  Indexing %s...\n", absPath)

	dbPath, err := store.DefaultPath(absPath)
	if err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	var pluginCheck *parser.PluginChecker
	if len(cfg.Plugins) > 0 {
		sHome, homeErr := synapsesHome()
		if homeErr != nil {
			logutil.Warn("  cannot determine synapses home: %v (plugins disabled)\n", homeErr)
			cfg.Plugins = nil
		} else {
			pluginCheck = parser.NewPluginChecker(sHome)
		}
	}

	g, err := loadOrBuildGraphWithStore(absPath, st, false, cfg.Plugins, pluginCheck, nil, "")
	if err != nil {
		return err
	}

	// Federation: merge linked project graphs.
	for _, linkedPath := range cfg.Linked {
		if mergeErr := mergeLinkedProject(g, linkedPath); mergeErr != nil {
			logutil.Warn("  skipping linked project %s: %v\n", linkedPath, mergeErr)
		}
	}

	if len(cfg.Linked) > 0 {
		if sites, err := st.LoadCallSites(); err == nil && len(sites) > 0 {
			for _, cs := range sites {
				g.AddCallSite(cs)
			}
			if n := resolver.ResolveCallEdges(g); n > 0 {
				logutil.Info("  resolved %d cross-project CALLS edges\n", n)
			}
		}
	}

	applyGoTypesIfEnabled(g, absPath, cfg)
	applyTSTypesIfEnabled(g, absPath, cfg)
	enrichMetricsIfEnabled(g, absPath, cfg)
	analyzeDataFlowIfEnabled(g, cfg)

	elapsed := time.Since(start)
	if identity := g.ProjectIdentity(); identity != nil {
		fmt.Printf("  \033[32m✓\033[0m %d nodes · %d edges · %d files · %s\n",
			g.NodeCount(), identity.Summary.Edges, identity.Summary.Files, elapsed.Round(time.Millisecond))
	} else {
		fmt.Printf("  \033[32m✓\033[0m %d nodes · %s\n", g.NodeCount(), elapsed.Round(time.Millisecond))
	}

	return nil
}

// ── Step 4 helpers ──────────────────────────────────────────────────────────

// selectAgents presents agent selection UI and returns the chosen agent keys.
func selectAgents(interactive bool, agentFlag string) ([]string, error) {
	// If --agents flag provided, use it directly.
	if agentFlag != "" {
		var agents []string
		for _, a := range strings.Split(agentFlag, ",") {
			a = strings.TrimSpace(a)
			if a != "" {
				agents = append(agents, a)
			}
		}
		return agents, nil
	}

	allAgents := detectInstalledAgents()

	if !interactive {
		// Non-interactive: connect all detected agents.
		var selected []string
		for _, a := range allAgents {
			if a.Detected {
				selected = append(selected, a.Key)
			}
		}
		return selected, nil
	}

	// Interactive: multi-select with huh.
	options := make([]huh.Option[string], 0, len(allAgents))
	for _, a := range allAgents {
		label := a.Display
		if a.Detected {
			label += "  (detected)"
		}
		options = append(options, huh.NewOption(label, a.Key).Selected(a.Detected))
	}

	var selected []string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select agents to connect:").
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.Run(); err != nil {
		return nil, nil // user cancelled
	}
	return selected, nil
}

// detectInstalledAgents returns all supported agents with detection status.
func detectInstalledAgents() []AgentInfo {
	home, _ := os.UserHomeDir()
	agents := []AgentInfo{
		{Key: "claude", Display: "Claude Code", Detected: detectClaude(home)},
		{Key: "cursor", Display: "Cursor", Detected: detectCursor(home)},
		{Key: "vscode", Display: "VS Code", Detected: detectVSCode(home)},
		{Key: "windsurf", Display: "Windsurf", Detected: detectWindsurf(home)},
		{Key: "zed", Display: "Zed", Detected: detectZed(home)},
		{Key: "antigravity", Display: "Antigravity", Detected: detectAntigravity(home)},
	}
	return agents
}

// ── Per-agent detection (mirrors synapses-app/src-tauri/src/lib.rs) ─────────

func detectClaude(home string) bool {
	return pathExists(filepath.Join(home, ".claude"))
}

func detectCursor(home string) bool {
	switch runtime.GOOS {
	case "darwin":
		return pathExists("/Applications/Cursor.app") || pathExists(filepath.Join(home, ".cursor"))
	case "windows":
		return pathExistsInLocalAppData("Programs", "cursor") ||
			pathExists(filepath.Join(home, ".cursor"))
	default: // linux
		return pathExists(filepath.Join(home, ".cursor")) ||
			pathExists(filepath.Join(home, ".config", "cursor"))
	}
}

func detectVSCode(home string) bool {
	switch runtime.GOOS {
	case "darwin":
		return pathExists("/Applications/Visual Studio Code.app") ||
			pathExists(filepath.Join(home, ".vscode"))
	case "windows":
		return pathExistsInLocalAppData("Programs", "Microsoft VS Code") ||
			pathExists(filepath.Join(home, ".vscode"))
	default:
		return pathExists(filepath.Join(home, ".vscode")) ||
			pathExists(filepath.Join(home, ".config", "Code"))
	}
}

func detectWindsurf(home string) bool {
	switch runtime.GOOS {
	case "darwin":
		return pathExists("/Applications/Windsurf.app") ||
			pathExists(filepath.Join(home, ".codeium", "windsurf"))
	case "windows":
		return pathExistsInLocalAppData("Programs", "windsurf") ||
			pathExists(filepath.Join(home, ".codeium", "windsurf"))
	default:
		return pathExists(filepath.Join(home, ".codeium", "windsurf"))
	}
}

func detectZed(home string) bool {
	switch runtime.GOOS {
	case "darwin":
		return pathExists("/Applications/Zed.app") ||
			pathExists(filepath.Join(home, ".config", "zed"))
	case "windows":
		return pathExistsInLocalAppData("Zed")
	default:
		return pathExists(filepath.Join(home, ".config", "zed"))
	}
}

func detectAntigravity(home string) bool {
	switch runtime.GOOS {
	case "darwin":
		return pathExists("/Applications/Antigravity.app") ||
			pathExists(filepath.Join(home, ".gemini"))
	default: // linux + windows
		return pathExists(filepath.Join(home, ".gemini"))
	}
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// pathExistsInLocalAppData checks for a path under %LOCALAPPDATA% on Windows.
// Returns false if the env var is unset/empty (avoids relative path probe).
func pathExistsInLocalAppData(subPaths ...string) bool {
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		return false
	}
	parts := append([]string{localAppData}, subPaths...)
	return pathExists(filepath.Join(parts...))
}

// ── Agent connection ────────────────────────────────────────────────────────

// connectAgents writes MCP config and guidance files for the selected agents.
// Dispatches to the existing per-agent helpers in main.go (writeHTTPMCPServerEntry,
// writeProjectCLAUDE, writeClaudeSettings, writeGuidanceFile, etc.).
func connectAgents(absPath string, agents []string) []connectResult {
	var results []connectResult
	for _, key := range agents {
		r := connectSingleAgent(absPath, key)
		results = append(results, r)
	}
	return results
}

func connectSingleAgent(absPath, agent string) connectResult {
	// writeOp runs a write function and appends the display name on success.
	// Returns the first error encountered.
	type writeOp struct {
		fn      func() error
		display string
	}

	var ops []writeOp

	switch agent {
	case "claude":
		mcpFile := filepath.Join(absPath, ".mcp.json")
		ops = []writeOp{
			{func() error { return writeHTTPMCPServerEntry(mcpFile, absPath) }, ".mcp.json"},
			{func() error { return writeProjectCLAUDE(absPath) }, ".claude/CLAUDE.md"},
			{func() error { return writeClaudeSettings(absPath) }, ".claude/settings.json"},
		}

	case "cursor":
		mcpFile := filepath.Join(absPath, ".cursor", "mcp.json")
		rulesFile := filepath.Join(absPath, ".cursor", "rules", "synapses.mdc")
		frontmatter := "---\ndescription: Synapses code intelligence — always use these MCP tools for code exploration\nalwaysApply: true\n---\n\n"
		ops = []writeOp{
			{func() error { return writeHTTPMCPServerEntry(mcpFile, absPath) }, ".cursor/mcp.json"},
			{func() error { return writeGuidanceFile(absPath, rulesFile, frontmatter) }, ".cursor/rules/synapses.mdc"},
		}

	case "windsurf":
		mcpFile := filepath.Join(absPath, ".windsurf", "mcp_config.json")
		rulesFile := filepath.Join(absPath, ".windsurfrules")
		ops = []writeOp{
			{func() error { return writeHTTPMCPServerEntry(mcpFile, absPath) }, ".windsurf/mcp_config.json"},
			{func() error { return writeGuidanceFile(absPath, rulesFile, "") }, ".windsurfrules"},
		}

	case "zed":
		ops = []writeOp{
			{func() error { return writeZedMCPConfig(absPath) }, ".zed/settings.json"},
		}

	case "vscode":
		ops = []writeOp{
			{func() error { return writeVSCodeMCPConfig(absPath) }, ".vscode/mcp.json"},
		}

	case "antigravity":
		mcpFile := filepath.Join(absPath, ".agent", "mcp.json")
		rulesFile := filepath.Join(absPath, ".agent", "rules", "synapses.md")
		ops = []writeOp{
			{func() error { return writeHTTPMCPServerEntry(mcpFile, absPath) }, ".agent/mcp.json"},
			{func() error { return writeGuidanceFile(absPath, rulesFile, "") }, ".agent/rules/synapses.md"},
		}

	default:
		return connectResult{Agent: agent, Err: fmt.Errorf("unknown agent %q", agent)}
	}

	// Execute all ops. Track written files and first error.
	var files []string
	var firstErr error
	for _, op := range ops {
		if err := op.fn(); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			files = append(files, op.display)
		}
	}

	// Use display name for output.
	display := agent
	for _, a := range detectInstalledAgents() {
		if a.Key == agent {
			display = a.Display
			break
		}
	}

	return connectResult{Agent: display, Files: files, Err: firstErr}
}

// ── Utilities ───────────────────────────────────────────────────────────────

// isInteractive returns true if stdin is connected to a terminal.
func isInteractive() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())
}

// binaryExists checks if a binary is on PATH.
func binaryExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
