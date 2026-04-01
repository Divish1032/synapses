// Command synapses is the Synapses MCP server binary.
// It indexes a code repository into an in-memory graph and serves structured
// context to AI agents over the Model Context Protocol (stdio transport).
//
// Usage:
//
//	synapses start  --path <repo>           Start the MCP server
//	synapses index  --path <repo>           Index only (no server)
//	synapses status --path <repo>           Show index statistics
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/contextfile"
	"github.com/SynapsesOS/synapses/internal/dataflow"
	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/logutil"
	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
	"github.com/SynapsesOS/synapses/internal/metrics"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/store"
)

// version is set at build time via ldflags: -X main.version=<tag>
var version = "dev"

func main() {
	// Fast-path: print version and exit immediately.
	// This avoids loading SQLite drivers and other heavy init() code
	// that runs before main(). Keeps `synapses version` zero-cost.
	if len(os.Args) >= 2 && (os.Args[1] == "version" || os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("synapses %s\n", version)
		// Show update hint if a cached check found a newer version.
		if state := getUpdateState(); state != nil && state.UpdateAvailable {
			fmt.Printf("Update available: %s → %s (run 'synapses update')\n",
				state.CurrentVersion, state.LatestVersion)
		}
		return
	}

	if err := run(os.Args[1:]); err != nil {
		// --help on any subcommand returns flag.ErrHelp — exit cleanly.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		logutil.Error("%v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}

	switch args[0] {
	// ── Core commands ────────────────────────────────────────────────────
	case "init":
		return cmdInit(args[1:])
	case "start":
		return cmdStartProxy(args[1:])
	case "stop":
		return cmdStop(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "index":
		return cmdIndex(args[1:])
	case "config":
		return cmdConfig(args[1:])
	case "connect":
		return cmdConnect(args[1:])
	case "update":
		return cmdUpdate(args[1:])
	case "remove":
		return cmdRemove(args[1:])
	case "uninstall":
		return cmdUninstall(args[1:])
	case "version", "--version", "-v":
		fmt.Printf("synapses %s\n", version)
		return nil
	case "completion":
		return cmdCompletion(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil

	// ── Subcommand groups ────────────────────────────────────────────────
	case "dev":
		return cmdDev(args[1:])
	case "daemon":
		return cmdDaemon(args[1:])

	default:
		return fmt.Errorf("unknown command %q — run 'synapses help'", args[0])
	}
}

// cmdIndex parses the repo and saves to the persistent cache, then exits.
func cmdIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Path to the repository root")
	forceReindex := fs.Bool("reindex", false, "Force a full re-index even if cache is fresh")
	reset := fs.Bool("reset", false, "Remove cached index for this project")
	resetAll := fs.Bool("all", false, "Remove ALL project indexes (use with --reset)")
	clearMemory := fs.Bool("clear-memory", false, "Clear agent memory (plans, tasks, episodes) without touching the code graph")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Handle --reset
	if *reset || *resetAll {
		return indexReset(*repoPath, *resetAll)
	}

	// Handle --clear-memory
	if *clearMemory {
		return indexClearMemory(*resetAll)
	}

	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	// ── Check for git and offer to initialize ────────────────────────────
	offerGitInit(absPath)

	cfg, err := config.Load(absPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	start := time.Now()
	fmt.Printf("Indexing %s...\n", absPath)

	dbPath, err := store.DefaultPath(absPath)
	if err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// Plugin security: per-machine opt-in for external parser commands.
	var pluginCheck2 *parser.PluginChecker
	if len(cfg.Plugins) > 0 {
		sHome, homeErr := synapsesHome()
		if homeErr != nil {
			logutil.Warn("cannot determine synapses home: %v (plugins disabled)\n", homeErr)
			cfg.Plugins = nil // fail-closed: cannot verify plugins → disable them
		} else {
			pluginCheck2 = parser.NewPluginChecker(sHome)
		}
	}

	g, err := loadOrBuildGraphWithStore(absPath, st, *forceReindex, cfg.Plugins, pluginCheck2, nil, "")
	if err != nil {
		return err
	}

	// Federation: merge linked project graphs.
	for _, linkedPath := range cfg.Linked {
		if mergeErr := mergeLinkedProject(g, linkedPath); mergeErr != nil {
			logutil.Warn("synapses: skipping linked project %s: %v\n", linkedPath, mergeErr)
		}
	}

	// Re-resolve cross-project CALLS edges now that linked nodes are present.
	if len(cfg.Linked) > 0 {
		if sites, err := st.LoadCallSites(); err == nil && len(sites) > 0 {
			for _, cs := range sites {
				g.AddCallSite(cs)
			}
			if n := resolver.ResolveCallEdges(g); n > 0 {
				logutil.Info("synapses: resolved %d cross-project CALLS edges\n", n)
			}
		}
		if nt := resolver.ResolveTerraformRefs(g); nt > 0 {
			logutil.Info("synapses: resolved %d Terraform DEPENDS_ON edges\n", nt)
		}
	}

	// Optional: type-checked CALLS resolution for Go (use_go_types: true).
	applyGoTypesIfEnabled(g, absPath, cfg)

	// Optional: type-checked CALLS resolution for TypeScript (use_ts_types: true).
	applyTSTypesIfEnabled(g, absPath, cfg)

	// Optional: enrich nodes with git churn + test coverage (metrics_days, coverage_profile).
	enrichMetricsIfEnabled(g, absPath, cfg)

	// Tag source/sink nodes and create DATA_FLOWS summary edges.
	analyzeDataFlowIfEnabled(g, cfg)

	identity := g.ProjectIdentity()
	printSummaryTable(identity, time.Since(start), nil, 0, 0, 0, 0)

	// Silently ensure sidecars are running after indexing.
	if ensureDirs() == nil {
		daemonStart(allSidecars, true) //nolint:errcheck
	}

	return nil
}

// indexReset removes cached index for a project or all projects.
func indexReset(repoPath string, all bool) error {
	if all {
		stats, _ := store.ScanAll()
		synapsesCache, err := store.CacheDir()
		if err != nil {
			return fmt.Errorf("locate cache dir: %w", err)
		}
		if err := os.RemoveAll(synapsesCache); err != nil {
			return fmt.Errorf("remove cache dir: %w", err)
		}
		if len(stats) == 0 {
			fmt.Println("No indexes to remove.")
		} else {
			for _, s := range stats {
				root := s.RepoRoot
				if root == "" {
					root = s.RepoID
				}
				fmt.Printf("  removed  %s\n", root)
			}
			fmt.Printf("\n%d index(es) removed from %s\n", len(stats), synapsesCache)
		}
		return nil
	}

	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	dbPath, err := store.DefaultPath(absPath)
	if err != nil {
		return err
	}
	if err := os.Remove(dbPath); err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("No index found for %s\n", absPath)
			return nil
		}
		return fmt.Errorf("remove index: %w", err)
	}
	fmt.Printf("Index removed: %s\n", dbPath)
	return nil
}

// agentMemoryTables are the SQLite tables containing agent-generated data.
// These can be safely cleared without affecting the code graph.
var agentMemoryTables = []string{
	"episodes", "plans", "tasks", "memories", "annotations",
	"agent_messages", "session_logs", "events", "quality_gaps",
	"dynamic_rules", "violation_log", "web_cache", "work_claims",
	"agent_watched_symbols",
}

// indexClearMemory clears agent memory tables across all indexed projects
// without touching the code graph (nodes, edges).
func indexClearMemory(withLogs bool) error {
	tables := make([]string, len(agentMemoryTables))
	copy(tables, agentMemoryTables)
	if withLogs {
		tables = append(tables, "tool_calls")
	}

	cacheDir, err := store.CacheDir()
	if err != nil {
		return fmt.Errorf("locate cache dir: %w", err)
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No indexed projects found.")
			return nil
		}
		return fmt.Errorf("read cache dir: %w", err)
	}

	cleared := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		dbPath := filepath.Join(cacheDir, entry.Name())
		db, openErr := sql.Open("sqlite", dbPath)
		if openErr != nil {
			fmt.Printf("  warning: %s: %v\n", entry.Name(), openErr)
			continue
		}
		for _, table := range tables {
			db.Exec("DELETE FROM " + table) //nolint:errcheck
		}
		db.Close()
		fmt.Printf("  cleared  %s\n", entry.Name())
		cleared++
	}
	if cleared == 0 {
		fmt.Println("No project databases found.")
	} else {
		fmt.Printf("\nAgent memory cleared across %d project(s).\n", cleared)
	}
	return nil
}

// cmdStop stops the singleton daemon by sending SIGTERM via its PID file.
func cmdStop(args []string) error {
	pidPath, err := singletonPIDPath()
	if err != nil {
		return fmt.Errorf("resolve PID path: %w", err)
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("Daemon is not running.")
			return nil
		}
		return fmt.Errorf("read PID file: %w", err)
	}
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	pid, err := parseInt(strings.TrimSpace(lines[0]))
	if err != nil {
		return fmt.Errorf("invalid PID in %s: %w", pidPath, err)
	}
	// PID recycling check: if we recorded a start timestamp, verify the
	// process with this PID actually started at that time.
	if len(lines) >= 2 {
		if startNanos, parseErr := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64); parseErr == nil && startNanos > 0 {
			if procStart := processStartTime(pid); procStart > 0 {
				recorded := time.Unix(0, startNanos)
				actual := time.Unix(0, procStart)
				if diff := recorded.Sub(actual); diff < -2*time.Second || diff > 2*time.Second {
					fmt.Printf("Process %d was recycled — removing stale PID file.\n", pid)
					os.Remove(pidPath)
					return nil
				}
			}
		}
	}
	if !processAlive(pid) {
		fmt.Printf("Process %d not found — removing stale PID file.\n", pid)
		os.Remove(pidPath) //nolint:errcheck
		return nil
	}
	if err := killProcess(pid); err != nil {
		return fmt.Errorf("kill daemon (pid %d): %w", pid, err)
	}
	// Wait up to 10s for graceful exit.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if !processAlive(pid) {
			fmt.Printf("Daemon stopped (was pid %d).\n", pid)
			return nil
		}
	}
	forceKillProcess(pid) //nolint:errcheck
	fmt.Printf("Daemon force-killed (pid %d).\n", pid)
	return nil
}

// cmdStatus is the unified health check. Default: full system + project health.
// With --all: list all indexed projects.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Path to the repository root")
	all := fs.Bool("all", false, "List all indexed projects")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *all {
		return statusListAll()
	}

	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	cfgDir, _ := config.FindConfigDir(absPath)
	cfg, err := config.Load(cfgDir)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	fmt.Printf("Synapses %s — Status\n", version)
	fmt.Println()
	fmt.Printf("%-16s%-16s%s\n", "Component", "Status", "Details")
	fmt.Printf("%-16s%-16s%s\n", "─────────", "──────", "───────")

	// ── App ─────────────────────────────────────────────────────────────────
	appPath := appBundledBinaryPath()
	if appPath != "" {
		appDir := filepath.Dir(filepath.Dir(filepath.Dir(appPath)))
		fmt.Printf("%-16s%-16s%s\n", "App", "installed", appDir)
	} else {
		fmt.Printf("%-16s%-16s%s\n", "App", "not found", "(desktop app not installed)")
	}

	// ── CLI Binary ──────────────────────────────────────────────────────────
	cliBin := filepath.Join(synapsesDataDir("bin"), "synapses")
	if _, statErr := os.Stat(cliBin); statErr == nil {
		if out, err := exec.Command(cliBin, "version").Output(); err == nil {
			fmt.Printf("%-16s%-16s%s\n", "CLI Binary", "ok", fmt.Sprintf("%s %s", cliBin, strings.TrimSpace(string(out))))
		} else {
			fmt.Printf("%-16s%-16s%s\n", "CLI Binary", "error", fmt.Sprintf("%s (cannot get version)", cliBin))
		}
	} else {
		fmt.Printf("%-16s%-16s%s\n", "CLI Binary", "missing", fmt.Sprintf("%s not found", cliBin))
	}

	// ── CLI in PATH ─────────────────────────────────────────────────────────
	if whichPath, err := exec.LookPath("synapses"); err == nil {
		resolved, _ := filepath.EvalSymlinks(whichPath)
		if resolved == cliBin || whichPath == cliBin {
			fmt.Printf("%-16s%-16s%s\n", "CLI in PATH", "ok", fmt.Sprintf("%s → %s", whichPath, cliBin))
		} else {
			fmt.Printf("%-16s%-16s%s\n", "CLI in PATH", "ok", whichPath)
		}
	} else {
		fmt.Printf("%-16s%-16s%s\n", "CLI in PATH", "not in PATH", "add ~/.synapses/bin to PATH or create /usr/local/bin/synapses symlink")
	}

	// ── Global Config ───────────────────────────────────────────────────────
	gc, gcErr := config.LoadGlobalConfig()
	if gcErr != nil {
		fmt.Printf("%-16s%-16s%s\n", "Global Config", "parse error", gcErr.Error())
	} else if gc != nil {
		details := []string{}
		if gc.Brain.Enabled {
			details = append(details, "brain: enabled")
		}
		if gc.Pulse.URL != "" {
			details = append(details, "pulse: on")
		}
		if gc.Embeddings != "" {
			details = append(details, fmt.Sprintf("embeddings: %s", gc.Embeddings))
		}
		globalPath, _ := config.GlobalConfigPath()
		detail := globalPath
		if len(details) > 0 {
			detail += fmt.Sprintf(" (%s)", strings.Join(details, ", "))
		}
		fmt.Printf("%-16s%-16s%s\n", "Global Config", "loaded", detail)
	} else {
		fmt.Printf("%-16s%-16s%s\n", "Global Config", "not found", "(optional — create with 'synapses config --global')")
	}

	// ── Dev Link ────────────────────────────────────────────────────────────
	if devState, err := readDevLinkState(); err == nil && devState.Linked {
		fmt.Printf("%-16s%-16s%s\n", "Dev Link", "active", fmt.Sprintf("custom binary from %s", devState.Source))
	}

	// ── Graph Index ──────────────────────────────────────────────────────────
	dbPath, dbErr := store.DefaultPath(absPath)
	if dbErr != nil {
		fmt.Printf("%-16s%-16s%s\n", "Graph Index", "error", dbErr.Error())
	} else {
		st, openErr := store.OpenReadOnly(dbPath)
		if openErr != nil {
			fmt.Printf("%-16s%-16s%s\n", "Graph Index", "missing", "no index — run 'synapses index'")
		} else {
			stat, statErr := st.Stat(dbPath)
			st.Close()
			if statErr != nil || stat == nil {
				fmt.Printf("%-16s%-16s%s\n", "Graph Index", "empty", "index exists but contains no data")
			} else {
				ago := time.Since(stat.SavedAt).Truncate(time.Second)
				indexStatus := "fresh"
				if ago > 24*time.Hour {
					indexStatus = "stale"
				}
				fmt.Printf("%-16s%-16s%s\n", "Graph Index", indexStatus,
					fmt.Sprintf("%s nodes, %s edges (indexed %s ago)",
						formatCount(stat.NodeCount), formatCount(stat.EdgeCount), formatDuration(ago)))
			}
		}
	}

	// ── Daemon ───────────────────────────────────────────────────────────────
	if IsSingletonDaemonRunning() {
		sockPath, _ := daemonSocketPath(absPath)
		fmt.Printf("%-16s%-16s%s\n", "Daemon", "running", fmt.Sprintf("http://%s, socket %s", DaemonHTTPAddr, sockPath))
	} else {
		fmt.Printf("%-16s%-16s%s\n", "Daemon", "stopped", "(will auto-start on next 'synapses start')")
	}

	// ── Brain ────────────────────────────────────────────────────────────────
	if cfg.Brain.Enabled {
		fmt.Printf("%-16s%-16s%s\n", "Brain", "enabled", fmt.Sprintf("in-process (ollama: %s)", cfg.Brain.OllamaURL))
	} else {
		fmt.Printf("%-16s%-16s%s\n", "Brain", "disabled", "(set brain.enabled:true in config to activate)")
	}

	// ── Doc Cache ────────────────────────────────────────────────────────────
	fmt.Printf("%-16s%-16s%s\n", "Doc Cache", "built-in", "version-pinned Go package docs via webcache")

	// ── Pulse ────────────────────────────────────────────────────────────────
	if cfg.Pulse.URL != "" {
		pulseStatus, detail := pingHealth(cfg.Pulse.URL + "/v1/health")
		fmt.Printf("%-16s%-16s%s\n", "Pulse", pulseStatus, detail)
	} else {
		fmt.Printf("%-16s%-16s%s\n", "Pulse", "not configured", "(set pulse.url in config to activate)")
	}

	return nil
}

// statusListAll lists all indexed projects (replaces the old `list` command).
func statusListAll() error {
	stats, err := store.ScanAll()
	if err != nil {
		return fmt.Errorf("scan projects: %w", err)
	}

	if len(stats) == 0 {
		fmt.Println("No indexed projects found. Run: synapses init")
		return nil
	}

	fmt.Printf("%-30s  %6s  %6s  %6s  %s\n", "PROJECT", "FILES", "NODES", "EDGES", "INDEXED AT")
	fmt.Printf("%-30s  %6s  %6s  %6s  %s\n",
		"──────────────────────────────",
		"──────", "──────", "──────",
		"───────────────────────")
	for _, s := range stats {
		ts := "never"
		if !s.SavedAt.IsZero() {
			ts = s.SavedAt.Local().Format("2006-01-02 15:04:05")
		}
		name := s.RepoID
		if len(name) > 30 {
			name = name[:27] + "..."
		}
		fmt.Printf("%-30s  %6d  %6d  %6d  %s\n",
			name, s.FileCount, s.NodeCount, s.EdgeCount, ts)
		if s.RepoRoot != "" {
			fmt.Printf("  %s\n", s.RepoRoot)
		}
	}
	fmt.Printf("\n%d project(s) indexed\n", len(stats))
	return nil
}

// pingHealth sends an HTTP GET to url with a 2s timeout and returns a
// human-readable status and detail string.
func pingHealth(url string) (string, string) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "unreachable", fmt.Sprintf("%s (%s)", url, shortenErr(err))
	}
	resp.Body.Close()
	return "reachable", url
}

// shortenErr extracts the most useful suffix from a net/http error string,
// stripping the verbose URL and method prefix that Go wraps around the root cause.
func shortenErr(err error) string {
	s := err.Error()
	// net/http wraps: Get "http://…": dial tcp …: connect: connection refused
	// We want just the tail after the last colon-space.
	if idx := strings.LastIndex(s, ": "); idx >= 0 {
		return s[idx+2:]
	}
	return s
}

// formatCount renders an integer with thousands separators (e.g. 1842 → "1,842").
func formatCount(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	offset := len(s) % 3
	if offset > 0 {
		b.WriteString(s[:offset])
	}
	for i := offset; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// formatDuration renders a duration in a human-friendly short form.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// loadOrBuildGraphWithStore tries to load from an already-open SQLite store,
// falling back to a full parse if no cache exists or forceReindex is true.
// When forceReindex is true and a valid cache exists, a smart incremental reindex
// is attempted first — only changed files are re-parsed, saving significant time
// on large codebases. Falls back to a full parse if the smart reindex fails.
// plugins is forwarded to the Walker so external parser plugins handle their extensions.
func loadOrBuildGraphWithStore(repoRoot string, st *store.Store, forceReindex bool, plugins []config.PluginConfig, pluginCheck *parser.PluginChecker, pc *pulse.Client, projectID string) (*graph.Graph, error) {
	// Always attempt smart reindex first: a fast filesystem mtime walk that
	// re-parses only changed files. This keeps line numbers accurate after
	// offline edits made between sessions (when the watcher was not running).
	// On repos with no changes the walk is cheap and returns immediately.
	g, changedFiles, err := smartReindex(repoRoot, st, plugins, pluginCheck)
	if err == nil {
		// Use incremental SaveGraphDelta per changed file instead of full
		// SaveGraph to avoid O(total-graph) write amplification. On large repos
		// (e.g. vscode, 225K nodes) full SaveGraph produces an 878MB WAL and
		// blocks for minutes; delta saves are O(changed-files) only.
		if len(changedFiles) > 0 {
			for _, cf := range changedFiles {
				if saveErr := st.SaveGraphDelta(cf, g); saveErr != nil {
					logutil.Error("synapses: delta save %s: %v\n", cf, saveErr)
				}
			}
			logutil.Info("synapses: saved %d changed file(s) via delta\n", len(changedFiles))
		}
		emitGraphSnapshot(g, pc, projectID) // P2-7: graph topology snapshot
		// Warm-boot: try to restore the columnar index from the snapshot blob.
		// This is best-effort — failure is silent (the index will be rebuilt async).
		tryLoadSnapshot(g, st)
		return g, nil
	}

	// smartReindex failed (no stored mtimes = first run, or corrupted cache).
	if !forceReindex {
		// Fall back to plain cache load so startup stays fast on first-run
		// or on repos that predate file-mtime tracking.
		cached, cacheErr := st.LoadGraph()
		if cacheErr != nil {
			logutil.Warn("synapses: cache load failed (%v), re-indexing\n", cacheErr)
		} else if cached != nil {
			savedAt, _ := st.SavedAt()
			logutil.Info("synapses: loaded from cache (indexed %s)\n",
				savedAt.Local().Format("2006-01-02 15:04:05"))
			// Warm-boot: restore columnar index from snapshot blob.
			tryLoadSnapshot(cached, st)
			return cached, nil
		}
	} else {
		logutil.Warn("synapses: smart reindex skipped (%v), doing full reindex\n", err)
	}

	// No cache or smart reindex skipped: full parse from scratch.
	// Evaluate SYNAPSES_QUIET once here so both the progress display and
	// buildGraph use the same value without redundant env lookups.
	quiet := os.Getenv("SYNAPSES_QUIET") == "1"

	// Register per-project progress state so the health endpoint can report
	// live indexing state without a global singleton. BeginFunc (called by
	// WalkDir after Phase 1) will call progress.Start(total) with the real
	// file count, so no placeholder Start(0) call is needed here.
	progress := RegisterIndexing(repoRoot)
	defer UnregisterIndexing(repoRoot)

	start := time.Now()
	g, err = buildGraph(repoRoot, st, plugins, quiet, progress, pluginCheck, pc, projectID)
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(start).Round(time.Millisecond)
	snap := progress.Snapshot()
	if !quiet {
		logutil.Info("[synapses] indexing... %d/%d files — %d functions, %d edges (%s)\n",
			snap.Done, snap.Total, g.NodeCount(), g.EdgeCount(), elapsed)
		logutil.Info("[synapses] ready.\n")
	}
	progress.Done()

	if err := st.SaveGraph(g); err != nil {
		logutil.Error("synapses: cache save failed: %v\n", err)
	}
	emitGraphSnapshot(g, pc, projectID) // P2-7: graph topology snapshot

	return g, nil
}

// tryLoadSnapshot attempts to restore the columnar GraphIndex from the snapshot
// blob stored in the SQLite meta table. This enables warm-boot: the index is
// available immediately without waiting for the async RebuildIndex goroutine.
// Errors are logged but never fatal — the index will be rebuilt asynchronously.
func tryLoadSnapshot(g *graph.Graph, st *store.Store) {
	blob, err := st.LoadIndexSnapshot()
	if err != nil || len(blob) == 0 {
		return
	}
	idx, err := graph.LoadSnapshot(blob, graph.NewStringPool())
	if err != nil {
		logutil.Warn("synapses: snapshot load failed (%v), will rebuild\n", err)
		return
	}
	g.SetIndex(idx)
	logutil.Info("synapses: warm-boot: columnar index restored from snapshot\n")
}

// loadOrBuildGraph opens a temporary store, loads or builds the graph, then
// closes the store. Used by cmdInit which does not need a long-lived store.
func loadOrBuildGraph(repoRoot string, forceReindex bool) (*graph.Graph, error) {
	dbPath, err := store.DefaultPath(repoRoot)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	return loadOrBuildGraphWithStore(repoRoot, st, forceReindex, nil, nil, nil, "")
}

// emitGraphSnapshot computes graph topology metrics and emits a GraphSnapshotEvent
// as a fire-and-forget goroutine. Nil-safe: no-op if pc is nil. (P2-7/P2-11)
func emitGraphSnapshot(g *graph.Graph, pc *pulse.Client, projectID string) {
	if pc == nil || g == nil {
		return
	}
	go func() {
		nodes := g.AllNodes()
		edges := g.AllEdges()
		nodesTotal := len(nodes)
		edgesTotal := len(edges)

		// Single pass over nodes: build nodeFile map, degree arrays, type dist, orphans.
		nodeFile := make(map[graph.NodeID]string, nodesTotal)
		fiVals := make([]int, 0, nodesTotal)
		foVals := make([]int, 0, nodesTotal)
		typeDist := make(map[string]int)
		// Fan-in/out computed after edge pass; populate placeholders now.
		fanIn := make(map[graph.NodeID]int, nodesTotal)
		fanOut := make(map[graph.NodeID]int, nodesTotal)
		for _, n := range nodes {
			nodeFile[n.ID] = n.File
			typeDist[string(n.Type)]++
		}

		// Single pass over edges: count CALLS edges, cross-file edges, degrees, type dist.
		callsEdges := 0
		crossFileEdges := 0
		edgeTypeDist := make(map[string]int) // P9-2: edge type distribution
		for _, e := range edges {
			fanOut[e.From]++
			fanIn[e.To]++
			edgeTypeDist[string(e.Type)]++ // P9-2
			if e.Type == graph.EdgeCalls {
				callsEdges++
			}
			if nodeFile[e.From] != nodeFile[e.To] {
				crossFileEdges++
			}
		}

		// Second (and final) pass over nodes: orphans + percentile arrays.
		// Merged from the original three separate node loops.
		orphans := 0
		maxFanIn, maxFanOut := 0, 0
		for _, n := range nodes {
			fi := fanIn[n.ID]
			fo := fanOut[n.ID]
			fiVals = append(fiVals, fi)
			foVals = append(foVals, fo)
			if fi > maxFanIn {
				maxFanIn = fi
			}
			if fo > maxFanOut {
				maxFanOut = fo
			}
			if fi == 0 && fo == 0 {
				orphans++
			}
		}

		// Compute density: edges / (N*(N-1)). Use float64 casts to avoid
		// implicit integer overflow on very large graphs.
		var density float64
		if nodesTotal > 1 {
			density = float64(edgesTotal) / (float64(nodesTotal) * float64(nodesTotal-1))
		}

		var crossPct float64
		if edgesTotal > 0 {
			crossPct = float64(crossFileEdges) / float64(edgesTotal) * 100
		}

		sort.Ints(fiVals)
		sort.Ints(foVals)
		percentile := func(vals []int, pct int) int {
			if len(vals) == 0 {
				return 0
			}
			return vals[(len(vals)-1)*pct/100]
		}

		typeDistJSON, _ := json.Marshal(typeDist)
		edgeTypeDistJSON, _ := json.Marshal(edgeTypeDist) // P9-2

		ev := pulse.GraphSnapshotEvent{
			SnapshotType:     "full",
			NodesTotal:       nodesTotal,
			EdgesTotal:       edgesTotal,
			EdgesCalls:       callsEdges,
			OrphanNodes:      orphans,
			Density:          density,
			CrossFileEdgePct: crossPct,
			MaxFanin:         maxFanIn,
			MaxFanout:        maxFanOut,
			FanInP50:         percentile(fiVals, 50),
			FanInP95:         percentile(fiVals, 95),
			FanOutP50:        percentile(foVals, 50),
			FanOutP95:        percentile(foVals, 95),
			NodeTypeDistJSON: string(typeDistJSON),
			ProjectID:        projectID,
			EdgeTypeDist:     string(edgeTypeDistJSON), // P9-2
		}
		pc.RecordGraphSnapshot(ev)
	}()
}

// analyzeDataFlowIfEnabled tags source/sink nodes and creates DATA_FLOWS summary
// edges between reachable (source, sink) pairs via existing CALLS edges.
// Always runs — built-in heuristics detect common patterns even without config.
func analyzeDataFlowIfEnabled(g *graph.Graph, cfg *config.Config) {
	if n := dataflow.AnnotateGraph(g, cfg); n > 0 {
		logutil.Info("synapses: %d DATA_FLOWS edges created\n", n)
	}
}

// enrichMetricsIfEnabled annotates graph nodes with git churn and (optionally)
// test-coverage data when the relevant config fields are set.
// Complexity is already computed during parsing and needs no extra step here.
// Errors from git or missing profiles are logged but never fatal.
func enrichMetricsIfEnabled(g *graph.Graph, root string, cfg *config.Config) {
	days := cfg.MetricsDays
	if days == 0 {
		days = 90
	}
	metrics.EnrichChurn(g, root, days)
	// R3: blame must run after churn — staleness_score reads metadata["churn"].
	metrics.EnrichBlame(g, root)
	// R34: commit context — the "why" layer (last 3 commit subjects per function).
	metrics.EnrichCommitContext(g, root)

	if cfg.CoverageProfile != "" {
		metrics.EnrichCoverage(g, root, cfg.CoverageProfile)
		logutil.Info("synapses: coverage profile loaded: %s\n", cfg.CoverageProfile)
	}

	if cfg.PprofProfile != "" {
		metrics.EnrichPprof(g, root, cfg.PprofProfile)
		logutil.Info("synapses: pprof profile loaded: %s\n", cfg.PprofProfile)
	}
}

// applyGoTypesIfEnabled runs the type-checked CALLS and IMPLEMENTS resolvers
// when cfg.UseGoTypes is true. A single packages.Load call handles both passes.
// Errors are logged but never fatal — the graph already has tree-sitter edges.
func applyGoTypesIfEnabled(g *graph.Graph, root string, cfg *config.Config) {
	if !cfg.UseGoTypes {
		return
	}
	logutil.Info("synapses: running go/types resolver (use_go_types=true)...\n")
	calls, impls, err := resolver.ResolveGoTypesBoth(g, root)
	if err != nil {
		logutil.Warn("synapses: go/types resolver failed (falling back to tree-sitter results): %v\n", err)
		return
	}
	logutil.Info("synapses: go/types added %d new CALLS edges, %d new IMPLEMENTS edges\n", calls, impls)
}

// applyTSTypesIfEnabled runs the TypeScript compiler-API resolver when
// cfg.UseTSTypes is true. Requires Node.js on PATH and the "typescript" npm
// package. Errors are logged but never fatal — the graph remains usable with
// tree-sitter-only CALLS edges.
func applyTSTypesIfEnabled(g *graph.Graph, root string, cfg *config.Config) {
	if !cfg.UseTSTypes {
		return
	}
	logutil.Info("synapses: running TypeScript type resolver (use_ts_types=true)...\n")
	n, err := resolver.ResolveTSTypesCallEdges(g, root)
	if err != nil {
		logutil.Warn("synapses: TS type resolver failed (falling back to tree-sitter results): %v\n", err)
		return
	}
	logutil.Info("synapses: ts/types added %d new CALLS edges\n", n)
}

// buildGraph parses the repo at root into a new graph.
// If st is non-nil the parsed file mtimes are saved for future incremental reindexes.
// plugins registers any external parser plugins before the walk begins.
// extDisplayName maps common file extensions to short language names for the
// progress line, e.g. ".go" → "Go". Unknown extensions fall back to the
// extension string itself (uppercased, dot stripped).
func extDisplayName(ext string) string {
	switch ext {
	case ".go":
		return "Go"
	case ".ts", ".tsx":
		return "TS"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "JS"
	case ".py":
		return "Py"
	case ".rs":
		return "Rust"
	case ".java":
		return "Java"
	case ".rb":
		return "Ruby"
	case ".swift":
		return "Swift"
	case ".kt", ".kts":
		return "Kotlin"
	case ".cs":
		return "C#"
	case ".cpp", ".cc", ".cxx":
		return "C++"
	case ".c":
		return "C"
	case ".sh", ".bash":
		return "Shell"
	case ".php":
		return "PHP"
	case ".scala":
		return "Scala"
	case ".zig":
		return "Zig"
	case ".lua":
		return "Lua"
	case ".ml", ".mli":
		return "OCaml"
	case ".ex", ".exs":
		return "Elixir"
	case ".json":
		return "JSON"
	case ".yaml", ".yml":
		return "YAML"
	case ".toml":
		return "TOML"
	case ".proto":
		return "Proto"
	default:
		name := strings.TrimPrefix(ext, ".")
		if name == "" {
			return ext
		}
		return strings.ToUpper(name[:1]) + name[1:]
	}
}

// buildGraph performs a full parse from scratch.
// quiet suppresses stderr progress output (SYNAPSES_QUIET=1).
// progress, if non-nil, receives live done/total updates for the health endpoint.
// pc, if non-nil, receives parse and index telemetry events (P2-8/P2-9).
func buildGraph(root string, st *store.Store, plugins []config.PluginConfig, quiet bool, progress *IndexingState, pluginCheck *parser.PluginChecker, pc *pulse.Client, projectID string) (*graph.Graph, error) {
	repoID := filepath.Base(root)
	g := graph.New(repoID)
	g.SetRoot(root)
	w := parser.NewWalker()
	// Full-index: throttle to half workers + nice +10 so the machine stays
	// responsive during the first-impression index. Incremental updates
	// (smartReindex / watcher) don't go through buildGraph so are unaffected.
	w.Throttle = true
	// P2-9: wire pulse client so WalkDir emits per-file ParseEvents.
	w.PulseClient = pc
	w.ProjectID = projectID
	for _, p := range plugins {
		w.RegisterPlugin(p.Extensions, p.Command, pluginCheck)
	}

	// BeginFunc fires after Phase 1 (filesystem scan) with the real total.
	// This is the correct moment to initialise progress state — not before,
	// when total is unknown.
	if progress != nil {
		w.BeginFunc = func(total int) {
			progress.Start(int64(total))
		}
	}

	// ProgressFunc fires from worker goroutines after each file, throttled
	// to 200ms. Writes done count to the progress state and, if not quiet,
	// emits a structured line to stderr.
	if progress != nil || !quiet {
		w.ProgressFunc = func(done, total int, byExt map[string]int) {
			if progress != nil {
				progress.SetDone(int64(done))
			}
			if quiet {
				return
			}
			// Build compact language breakdown: top 3 extensions by count.
			type lc struct {
				name  string
				count int
			}
			langs := make([]lc, 0, len(byExt))
			for ext, cnt := range byExt {
				langs = append(langs, lc{extDisplayName(ext), cnt})
			}
			sort.Slice(langs, func(i, j int) bool { return langs[i].count > langs[j].count })
			var parts []string
			for i := 0; i < len(langs) && i < 3; i++ {
				parts = append(parts, fmt.Sprintf("%s: %d", langs[i].name, langs[i].count))
			}
			suffix := ""
			if len(parts) > 0 {
				suffix = " (" + strings.Join(parts, ", ") + ")"
			}
			logutil.Info("[synapses] indexing... %d/%d files%s\n", done, total, suffix)
		}
	}

	buildStart := time.Now() // P2-8: index timing
	mtimes, err := w.WalkDir(g, root)
	if err != nil {
		return nil, fmt.Errorf("parse repo: %w", err)
	}

	// File parsing is complete. Switch the progress label so the frontend
	// shows "Resolving edges…" instead of a confusing "99%" that never moves.
	if progress != nil {
		progress.SetLabel("Resolving edges…")
	}

	// P2-8: capture call site count before draining (for IndexEvent resolution rate).
	preResolveCallSites := g.PeekCallSites()
	totalCallSites := len(preResolveCallSites)

	// P9-3: capture per-language call site counts before ResolveCallEdges drains them.
	langCallSites := make(map[string]int, 16)
	for _, cs := range preResolveCallSites {
		if ext := filepath.Ext(cs.CallerFile); ext != "" {
			langCallSites[extDisplayName(ext)]++
		}
	}

	// Persist call sites BEFORE draining them so they can be reloaded and
	// re-resolved after MergeFrom for cross-project CALLS edge resolution.
	if st != nil {
		if saveErr := st.SaveCallSites(g.PeekCallSites()); saveErr != nil {
			logutil.Error("synapses: save call sites: %v\n", saveErr)
		}
	}

	resolverStart := time.Now()
	if na := resolver.ResolvePathAliases(g); na > 0 {
		logutil.Info("synapses: resolved %d tsconfig path aliases\n", na)
	}
	n := resolver.ResolveCallEdges(g)
	logutil.Info("synapses: resolved %d CALLS edges\n", n)
	nh := resolver.ResolveHeritageEdges(g)
	if nh > 0 {
		logutil.Info("synapses: resolved %d heritage IMPLEMENTS edges\n", nh)
	}
	ni := resolver.ResolveImplementsEdges(g)
	if ni > 0 {
		logutil.Info("synapses: resolved %d structural IMPLEMENTS edges\n", ni)
	}
	if nd := resolver.ResolveGoMethodDefinesEdges(g); nd > 0 {
		logutil.Info("synapses: resolved %d cross-file Go struct→method DEFINES edges\n", nd)
	}
	// Proto/GraphQL cross-file type reference resolution.
	if npt := parser.ResolveProtoTypeRefs(g); npt > 0 {
		logutil.Info("synapses: resolved %d proto type reference edges\n", npt)
	}
	resolverDurationMs := float64(time.Since(resolverStart).Milliseconds())
	// R31: resolve documentation → code entity links (EXPLAINS/DOCUMENTED_BY).
	if nd := resolver.ResolveDocEdges(g); nd > 0 {
		logutil.Info("synapses: resolved %d EXPLAINS edges\n", nd)
	}
	// Full-graph NL entity linking: link pre-existing docs/READMEs to code nodes
	// so they are immediately visible in the knowledge graph on first index.
	if nc := resolver.ResolveNLEntities(g, nil); len(nc) > 0 {
		logutil.Info("synapses: NL entity resolution: %d unresolved candidates\n", len(nc))
	}
	if nt := resolver.ResolveTerraformRefs(g); nt > 0 {
		logutil.Info("synapses: resolved %d Terraform DEPENDS_ON edges\n", nt)
	}

	if st != nil && len(mtimes) > 0 {
		if saveErr := st.SaveFileMtimes(mtimes); saveErr != nil {
			logutil.Error("synapses: save file mtimes: %v\n", saveErr)
		}
	}

	// P2-8: emit IndexEvent (fire-and-forget).
	if pc != nil {
		unresolved := totalCallSites - n
		if unresolved < 0 {
			unresolved = 0
		}
		var resRate float64
		if totalCallSites > 0 {
			resRate = float64(n) / float64(totalCallSites)
		}
		// Build language distribution JSON from mtimes keys (already unique per file).
		// Avoids allocating a full AllNodes() slice just for file-extension counting.
		langDist := make(map[string]int, 16)
		for f := range mtimes {
			if ext := filepath.Ext(f); ext != "" {
				langDist[extDisplayName(ext)]++
			}
		}
		langDistJSON, _ := json.Marshal(langDist)

		// P9-3: per-language call-site resolution rate.
		// langCallSites was captured before ResolveCallEdges drained the call sites.
		// The resolver doesn't track per-language resolved counts, so we use the
		// overall rate as a proxy. The JSON still shows which languages contribute
		// most call sites, enabling identification of problematic language coverage.
		resRateByLang := make(map[string]float64, len(langCallSites))
		for lang, count := range langCallSites {
			if count > 0 {
				resRateByLang[lang] = resRate
			}
		}
		resByLangJSON, _ := json.Marshal(resRateByLang)

		ev := pulse.IndexEvent{
			DurationMs:             time.Since(buildStart).Milliseconds(),
			FilesIndexed:           len(mtimes),
			TotalRepoFiles:        w.TotalFilesWalked,
			TotalNodes:             g.NodeCount(),
			TotalEdges:             g.EdgeCount(),
			CallSitesResolved:      n,
			CallSitesUnresolved:    unresolved,
			ResolutionRate:         resRate,
			LanguageDistJSON:       string(langDistJSON),
			ProjectID:              projectID,
			ResolverDurationMs:     resolverDurationMs,
			HeritageEdgesCreated:   nh,                    // P9-6
			ImplementsEdgesCreated: ni,                    // P9-6
			ResolutionByLangJSON:   string(resByLangJSON), // P9-3
		}
		// P9-5: compute per-language parser coverage on the background goroutine
		// to avoid blocking startup with a full AllNodes() copy on large repos.
		langDistCopy := langDist
		go func() {
			type langCoverage struct {
				Files    int `json:"files"`
				Entities int `json:"entities"`
			}
			coverageByLang := make(map[string]*langCoverage, len(langDistCopy))
			for lang, count := range langDistCopy {
				coverageByLang[lang] = &langCoverage{Files: count}
			}
			for _, node := range g.AllNodes() {
				if node.File != "" {
					lang := extDisplayName(filepath.Ext(node.File))
					if lang != "" {
						if c, ok := coverageByLang[lang]; ok {
							c.Entities++
						}
					}
				}
			}
			coverageJSON, _ := json.Marshal(coverageByLang)
			ev.CoverageJSON = string(coverageJSON)
			pc.RecordIndexEvent(ev)
		}()
	}

	return g, nil
}

// smartReindex loads the cached graph from st, re-parses only changed files,
// and returns the updated graph. Used when --reindex is requested and a valid
// cache exists, avoiding a full re-parse of unchanged files.
// plugins registers any external parser plugins before the incremental walk.
// smartReindex returns the loaded graph and the list of changed file paths
// (relative to the repo root). An empty slice means nothing changed.
func smartReindex(repoRoot string, st *store.Store, plugins []config.PluginConfig, pluginCheck *parser.PluginChecker) (*graph.Graph, []string, error) {
	g, err := st.LoadGraph()
	if err != nil || g == nil {
		return nil, nil, fmt.Errorf("load cached graph: %w", err)
	}

	known, err := st.LoadFileMtimes()
	if err != nil {
		return nil, nil, fmt.Errorf("load file mtimes: %w", err)
	}
	if len(known) == 0 {
		return nil, nil, fmt.Errorf("no stored file mtimes — falling back to full reindex")
	}

	w := parser.NewWalker()
	for _, p := range plugins {
		w.RegisterPlugin(p.Extensions, p.Command, pluginCheck)
	}
	fresh, changed, removed, err := w.IncrementalReindex(g, repoRoot, known)
	if err != nil {
		return nil, nil, fmt.Errorf("incremental reindex: %w", err)
	}

	unchanged := len(fresh) - changed
	logutil.Info("synapses: smart reindex: %d changed, %d unchanged, %d removed\n",
		changed, unchanged, removed)

	// Derive the changed file paths by comparing fresh mtimes against known.
	var changedFiles []string
	for path, mtime := range fresh {
		if stored, ok := known[path]; !ok || stored != mtime {
			changedFiles = append(changedFiles, path)
		}
	}
	// Also include removed files (present in known but not in fresh).
	for path := range known {
		if _, ok := fresh[path]; !ok {
			changedFiles = append(changedFiles, path)
		}
	}

	if changed+removed > 0 {
		// Reload stored call sites from ALL files so ResolveCallEdges can
		// recreate CALLS edges pointing INTO the re-parsed changed files.
		// IncrementalReindex called RemoveFile for each changed file, which
		// deleted those incoming edges. Only the changed files' new call sites
		// are pending in g.callSites; call sites from unchanged files were
		// drained during the previous full build and are not in memory.
		// This mirrors the fix applied to Watcher.reparseFile.
		if stored, err := st.LoadCallSites(); err == nil {
			g.BulkAddCallSites(stored)
		}
		n := resolver.ResolveCallEdges(g)
		logutil.Info("synapses: resolved %d CALLS edges\n", n)
		if ni := resolver.ResolveImplementsEdges(g); ni > 0 {
			logutil.Info("synapses: resolved %d IMPLEMENTS edges\n", ni)
		}
		// R31: re-resolve doc edges after incremental reparse.
		if nd := resolver.ResolveDocEdges(g); nd > 0 {
			logutil.Info("synapses: resolved %d EXPLAINS edges\n", nd)
		}
		if nt := resolver.ResolveTerraformRefs(g); nt > 0 {
			logutil.Info("synapses: resolved %d Terraform DEPENDS_ON edges\n", nt)
		}
	}

	if saveErr := st.SaveFileMtimes(fresh); saveErr != nil {
		logutil.Error("synapses: save file mtimes: %v\n", saveErr)
	}
	return g, changedFiles, nil
}

// mergeLinkedProject loads the pre-built index for linkedPath and merges it
// into g. The linked project must have been indexed with 'synapses index' first.
// Federated nodes are additive — they never overwrite primary-project nodes and
// are not persisted back to the primary project's store.
func mergeLinkedProject(g *graph.Graph, linkedPath string) error {
	absLinked, err := filepath.Abs(linkedPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	dbPath, err := store.DefaultPath(absLinked)
	if err != nil {
		return err
	}
	lst, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer lst.Close()

	linked, err := lst.LoadGraph()
	if err != nil {
		return fmt.Errorf("load graph: %w", err)
	}
	if linked == nil {
		return fmt.Errorf("no index found — run 'synapses index --path %s' first", absLinked)
	}

	g.MergeFrom(linked)
	logutil.Info("synapses: merged linked project %s (%d nodes)\n",
		filepath.Base(absLinked), linked.NodeCount())
	return nil
}

// cmdInit is defined in init.go — the unified 4-step interactive wizard.

// synapsesSectionStart / synapsesSectionEnd are the HTML comment sentinels
// that fence the Synapses-managed guidance block in every agent rules file.
// Identical markers across all agents allow `synapses connect` to be idempotent.
const (
	synapsesSectionStart = "<!-- synapses:start -->\n"
	synapsesSectionEnd   = "<!-- synapses:end -->"
)

// synapsesSection is the full guidance block (including sentinels) injected
// into every agent guidance file: .claude/CLAUDE.md, .cursor/rules/synapses.mdc,
// .windsurfrules. Content is identical for all agents; only the file path and
// any agent-specific frontmatter differ.
var synapsesSection = synapsesSectionStart + `## Synapses — Knowledge Substrate (MCP)

### Session Start
Call ` + "`session_init(agent_id=\"...\", intent=\"what you're doing\")`" + ` at the start of every session. Returns pending tasks, project identity, scale guidance, working state, and proactive tool suggestions in one round-trip. Declare your intent to get relevant tool suggestions automatically.

### After Context Compaction
If your context was compacted (conversation history summarized), call ` + "`session_init(agent_id=\"...\", scope=\"compaction\")`" + ` to recover your prior work state — decisions, entity memories, rules, and a ranked list of entities you were working on. This eliminates re-exploration.

### Key Tools

These 13 core tools cover 95% of workflows. All 40+ tools remain available — call ` + "`discover_tools(query=\"...\")`" + ` to find specialized ones.

| Goal | Tool |
|---|---|
| Start session | ` + "`session_init(agent_id=\"...\", intent=\"what you're doing\")`" + ` |
| Understand code | ` + "`get_context(entity=\"EntityName\", intent=\"understand\")`" + ` |
| Prepare to modify | ` + "`get_context(entity=\"EntityName\", intent=\"modify\")`" + ` |
| Find a symbol | ` + "`search(query=\"name\", mode=\"exact\")`" + ` |
| Search by concept | ` + "`search(query=\"auth caching\", mode=\"semantic\")`" + ` |
| Check before implementing | ` + "`validate(phase=\"pre\", changes=[...])`" + ` |
| Verify after writing | ` + "`validate(phase=\"post\", files_written=[\"...\"])`" + ` |
| Save knowledge | ` + "`memory(action=\"save\", decision=\"...\", agent_id=\"...\")`" + ` |
| Retrieve knowledge | ` + "`memory(action=\"search\", query=\"...\")`" + ` |
| Plan tasks | ` + "`tasks(action=\"create_plan\", title=\"...\", tasks=[...])`" + ` |
| Update task | ` + "`tasks(action=\"update\", id=\"...\", status=\"done\")`" + ` |
| End session | ` + "`end_session(agent_id=\"...\")`" + ` — persists session knowledge, optionally reports usage |

### Need more?

` + "`session_init`" + ` suggests specialized tools based on your declared intent (e.g. ` + "`get_impact`" + `, ` + "`search(mode=\"exact\")`" + `).
Synapses exposes 8 tools — read their descriptions via ` + "`tools/list`" + ` to find what you need.

### Cross-Project Queries
When multiple projects are registered with the daemon, query knowledge across them:
- ` + "`memory(action=\"search\", query=\"...\", projects=\"*\")`" + ` — search memories across all projects
Cross-project results appear in separate response fields (e.g. ` + "`cross_project_episodes`" + `).
Active sessions on related projects are automatically surfaced in ` + "`session_init`" + ` via the Work Ledger.

### Anti-patterns
- **Prefer** Synapses tools over Grep/Glob for code exploration — they return callers, callees, and architecture rules that raw file scanning misses
- **Always** run ` + "`validate(phase=\"pre\")`" + ` before multi-file changes — it catches architecture violations before any code is written
- **Always** track discovered bugs as tasks via ` + "`tasks(action=\"create_plan\")`" + ` immediately

### Memory Tiers

Synapses memory is organized in three tiers. Use ` + "`memory(action=\"save\")`" + ` to save persistent knowledge about your work:

| Tier | Purpose | Persistence | Scope |
|------|---------|-----------|-------|
| **Tier 1 — Live** | In-session work tracking, todo lists, blocked tasks. Use ` + "`TodoWrite`" + ` for current work. | Session-only | This conversation |
| **Tier 2 — Anchored** | Code insights, discovered bugs, architecture decisions linked to graph nodes. Use ` + "`memory(action=\"save\", anchor_nodes=[...])`" + ` to tie memory to code entities. | Persistent; auto-flagged stale if linked node changes | All sessions |
| **Tier 3 — Durable** | User preferences, feedback, project context, external references. No code links — survives refactoring. Use ` + "`memory(action=\"save\", decision=\"...\")`" + ` without ` + "`anchor_nodes`" + ` for durable facts. | Persistent | All sessions |

**Memory storage rules:**
- Write memory to separate ` + "`.md`" + ` files in the project's ` + "`/Users/itachi/.claude/projects/{{project}}/memory/`" + ` directory, not to MEMORY.md.
- Never write these to MEMORY.md: code patterns, file paths, function signatures, architecture, git blame data. These belong in the living code, not memory.
- Index all memories in ` + "`MEMORY.md`" + ` with brief descriptions and links to the actual ` + "`.md`" + ` files.
- When a user asks you to remember something, always save it immediately as the correct tier.

### Workflow
` + "`session_init`" + ` → explore (` + "`get_context`" + `, ` + "`search`" + `) → ` + "`validate(phase=\"pre\")`" + ` → edit files → ` + "`validate(phase=\"post\")`" + ` → ` + "`end_session`" + `
` + synapsesSectionEnd

// knowledgeSynapsesSection is the guidance block for knowledge-mode projects
// (no code graph). Focuses on memory, tasks, and cross-project collaboration.
var knowledgeSynapsesSection = synapsesSectionStart + `## Synapses — Knowledge Substrate (MCP)

This project runs in **knowledge mode** — no code graph, just memory, tasks, events, and cross-project collaboration.

### Session Start
Call ` + "`session_init(agent_id=\"...\", intent=\"what you're doing\")`" + ` at the start of every session. Returns pending tasks, project identity, and proactive tool suggestions.

### Key Tools

| Goal | Tool |
|---|---|
| Start session | ` + "`session_init(agent_id=\"...\", intent=\"what you're doing\")`" + ` |
| Save knowledge | ` + "`memory(action=\"save\", decision=\"...\", agent_id=\"...\")`" + ` |
| Retrieve knowledge | ` + "`memory(action=\"search\", query=\"...\")`" + ` |
| Plan tasks | ` + "`tasks(action=\"create_plan\", title=\"...\", tasks=[...])`" + ` |
| Update task | ` + "`tasks(action=\"update\", id=\"...\", status=\"done\")`" + ` |
| Get pending tasks | ` + "`tasks(action=\"pending\")`" + ` |
| End session | ` + "`end_session(agent_id=\"...\")`" + ` |

### Cross-Project Queries
When multiple projects are registered with the daemon, query knowledge across them:
- ` + "`memory(action=\"search\", query=\"...\", projects=\"*\")`" + ` — search memories across all projects
Cross-project awareness is handled automatically by the Work Ledger — ` + "`session_init`" + ` shows active sessions on related projects.

### Workflow
` + "`session_init`" + ` → ` + "`memory(action=\"search\")`" + ` / ` + "`tasks(action=\"pending\")`" + ` → work → ` + "`memory(action=\"save\")`" + ` / ` + "`tasks(action=\"update\")`" + ` → ` + "`end_session`" + `
` + synapsesSectionEnd

// writeProjectCLAUDE writes (or updates) a Synapses-managed section in
// .claude/CLAUDE.md (preferred by Claude Code). The section is delimited by
// HTML comments so it can be safely updated on subsequent connect runs without
// clobbering the rest of the file. If a root-level CLAUDE.md exists with a
// Synapses section it is migrated to .claude/CLAUDE.md and the section is
// removed from the root file.
func writeProjectCLAUDE(repoRoot string) error {
	// Choose template based on project mode from config.
	section := synapsesSection
	if cfg, err := config.Load(repoRoot); err == nil && cfg.Mode == "knowledge" {
		section = knowledgeSynapsesSection
	}

	clauDir := filepath.Join(repoRoot, ".claude")
	if err := os.MkdirAll(clauDir, 0o755); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}
	claudePath := filepath.Join(clauDir, "CLAUDE.md")

	// Migrate: if root-level CLAUDE.md has a Synapses section, remove it from
	// there so the content lives only in .claude/CLAUDE.md.
	rootCLAUDE := filepath.Join(repoRoot, "CLAUDE.md")
	if rootData, err := os.ReadFile(rootCLAUDE); err == nil {
		rs := string(rootData)
		si := strings.Index(rs, synapsesSectionStart)
		if si != -1 {
			ei := strings.Index(rs, synapsesSectionEnd)
			if ei != -1 {
				cleaned := rs[:si] + rs[ei+len(synapsesSectionEnd):]
				cleaned = strings.TrimRight(cleaned, "\n") + "\n"
				_ = os.WriteFile(rootCLAUDE, []byte(cleaned), 0o644)
			}
		}
		// Remove root CLAUDE.md entirely if it is now empty/whitespace-only.
		if strings.TrimSpace(string(rootData)) == "" || (si != -1 && strings.TrimSpace(rs[:si]) == "") {
			_ = os.Remove(rootCLAUDE)
		}
	}

	existing, err := os.ReadFile(claudePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read .claude/CLAUDE.md: %w", err)
	}

	content := string(existing)
	startIdx := strings.Index(content, synapsesSectionStart)
	if startIdx != -1 {
		// Replace the existing Synapses section.
		endIdx := strings.Index(content, synapsesSectionEnd)
		if endIdx != -1 {
			content = content[:startIdx] + section + content[endIdx+len(synapsesSectionEnd):]
		} else {
			content = content[:startIdx] + section
		}
	} else {
		// Append a new section, ensuring a blank line separator.
		if len(content) > 0 && !strings.HasSuffix(content, "\n\n") {
			if strings.HasSuffix(content, "\n") {
				content += "\n"
			} else {
				content += "\n\n"
			}
		}
		content += section + "\n"
	}

	return os.WriteFile(claudePath, []byte(content), 0o644)
}

// writeClaudeSettings writes (or updates) .claude/settings.json to add hooks
// that guide LLMs to use Synapses tools:
//   - SessionStart: cats the context file (auto-injected real data, no tool call needed)
//   - PostToolUse on Write|Edit: nudges agent to call verify_implementation
func writeClaudeSettings(repoRoot string) error {
	dir := filepath.Join(repoRoot, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}

	settingsPath := filepath.Join(dir, "settings.json")

	// Parse existing settings or start fresh.
	raw := map[string]interface{}{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse settings.json: %w", err)
		}
	}

	// Navigate / create: raw["hooks"]
	hooks, _ := raw["hooks"].(map[string]interface{})
	if hooks == nil {
		hooks = map[string]interface{}{}
		raw["hooks"] = hooks
	}

	// ── Clean up stale hooks from previous versions ──────────────────────
	// The Glob|Grep hard block and low-value PostToolUse confirmations were
	// removed — clean them from already-connected projects on re-connect.
	removeHookEntry(hooks, "PreToolUse", "Glob|Grep")
	removeHookEntry(hooks, "PostToolUse", "mcp__synapses__validate_plan") // legacy cleanup
	removeHookEntry(hooks, "PostToolUse", "mcp__synapses__validate")      // cleanup if validate hook was auto-added
	removeHookEntry(hooks, "PostToolUse", "mcp__synapses__create_plan")

	// ── SessionStart: cat the daemon-written context file instead of a static echo.
	// The context file contains project identity, pending tasks, and tool cheat sheet.
	// If the daemon is not running the file won't exist and the fallback echo fires.
	ctxFilePath, ctxErr := contextfile.ContextFilePath(repoRoot)
	var sessionStartCmd string
	if ctxErr == nil {
		sessionStartCmd = fmt.Sprintf(
			"cat %q 2>/dev/null || echo '[Synapses] Daemon not running — start via the Synapses app or: synapses start'",
			ctxFilePath,
		)
	} else {
		// Fallback to static reminder if we can't compute the path.
		sessionStartCmd = "echo '[Synapses] MANDATORY: Call session_init() as your FIRST action — " +
			"it returns pending tasks, project identity, working state, and scale_guidance in one call. " +
			"WORKFLOW: session_init → get_context → validate(phase=pre) → edit files → validate(phase=post) → end_session.'"
	}
	upsertHookEntry(hooks, "SessionStart", "startup", map[string]interface{}{
		"type":    "command",
		"command": sessionStartCmd,
	})

	// ── PostToolUse: nudge validate(phase=post) after any file write/edit.
	upsertHookEntry(hooks, "PostToolUse", "Write|Edit", map[string]interface{}{
		"type": "command",
		"command": "echo '[Synapses] Files written. Now call validate(phase=\"post\", files_written=[\"<path>\"]) " +
			"to check your changes against architecture rules before continuing.'",
	})

	// ── Pre-allow all Synapses MCP tools so users are never prompted ─────
	allow, _ := raw["permissions"].(map[string]interface{})
	if allow == nil {
		allow = map[string]interface{}{}
		raw["permissions"] = allow
	}
	allowList, _ := allow["allow"].([]interface{})
	const synapsesPattern = "mcp__synapses__*"
	found := false
	for _, v := range allowList {
		if s, ok := v.(string); ok && s == synapsesPattern {
			found = true
			break
		}
	}
	if !found {
		allowList = append(allowList, synapsesPattern)
		allow["allow"] = allowList
	}

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(settingsPath, append(out, '\n'), 0o644)
}

// removeHookEntry removes a hook entry from a given event type list by matcher.
// Used to clean up stale hooks from previously-connected projects on re-connect.
func removeHookEntry(hooks map[string]interface{}, eventType, matcher string) {
	list, _ := hooks[eventType].([]interface{})
	for i, existing := range list {
		if m, ok := existing.(map[string]interface{}); ok && m["matcher"] == matcher {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(hooks, eventType)
	} else {
		hooks[eventType] = list
	}
}

// upsertHookEntry adds or replaces a hook entry in a given event type list,
// matching by the "matcher" field. This avoids duplicate entries on repeated runs.
func upsertHookEntry(hooks map[string]interface{}, eventType, matcher string, hookDef map[string]interface{}) {
	list, _ := hooks[eventType].([]interface{})

	entry := map[string]interface{}{
		"matcher": matcher,
		"hooks":   []interface{}{hookDef},
	}

	replaced := false
	for i, existing := range list {
		if m, ok := existing.(map[string]interface{}); ok && m["matcher"] == matcher {
			list[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, entry)
	}
	hooks[eventType] = list
}

// writeMCPConfig writes (or updates) the .mcp.json file at mcpFile so it
// contains a `synapses` server entry pointing at repoRoot.
// If the file already exists, existing MCP server entries are preserved —
// only the `synapses` key is added or overwritten.
func writeMCPConfig(mcpFile, repoRoot string) error {
	// Seed with an empty config; overwritten below if the file already exists.
	raw := map[string]interface{}{
		"mcpServers": map[string]interface{}{},
	}

	if data, err := os.ReadFile(mcpFile); err == nil {
		// File exists — parse it to preserve existing MCP server entries.
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse existing .mcp.json: %w", err)
		}
		// Ensure the mcpServers key exists and is the right type.
		if _, ok := raw["mcpServers"]; !ok {
			raw["mcpServers"] = map[string]interface{}{}
		}
	}

	// Add / replace the synapses entry.
	servers, _ := raw["mcpServers"].(map[string]interface{})
	if servers == nil {
		servers = map[string]interface{}{}
		raw["mcpServers"] = servers
	}
	servers["synapses"] = map[string]interface{}{
		"type":    "stdio",
		"command": "synapses",
		"args":    []string{"start", "-path", repoRoot},
	}

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(mcpFile, append(out, '\n'), 0o644)
}

// ── Agent connect helpers ─────────────────────────────────────────────────────

// mcpURL returns the Synapses MCP endpoint with the project path embedded as
// a query parameter, which the daemon requires to route requests correctly.
func mcpURL(projectRoot string) string {
	return "http://127.0.0.1:11435/mcp?project=" + url.QueryEscape(projectRoot)
}

// writeHTTPMCPServerEntry merges a synapses HTTP entry into a JSON file that
// uses the standard { "mcpServers": { … } } shape (Claude .mcp.json, Cursor,
// Windsurf, Antigravity). Creates the file and its parent directories if missing.
func writeHTTPMCPServerEntry(file, projectRoot string) error {
	raw := map[string]interface{}{"mcpServers": map[string]interface{}{}}
	if data, err := os.ReadFile(file); err == nil {
		parsed := map[string]interface{}{}
		if json.Unmarshal(data, &parsed) == nil {
			raw = parsed
		}
	}
	if _, ok := raw["mcpServers"]; !ok {
		raw["mcpServers"] = map[string]interface{}{}
	}
	servers, _ := raw["mcpServers"].(map[string]interface{})
	if servers == nil {
		servers = map[string]interface{}{}
		raw["mcpServers"] = servers
	}
	servers["synapses"] = map[string]interface{}{
		"type": "http",
		"url":  mcpURL(projectRoot),
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, append(out, '\n'), 0o644)
}

// writeStdioMCPServerEntry writes a stdio-based MCP server entry to a global
// config file. Used for agents that only support global MCP (Windsurf, Antigravity).
// Stdio transport lets the IDE launch `synapses start` as a subprocess — the
// subprocess inherits the IDE's working directory, so it automatically connects
// to the correct project without a hardcoded path. This means one global config
// works for all projects.
func writeStdioMCPServerEntry(file string) error {
	raw := map[string]interface{}{"mcpServers": map[string]interface{}{}}
	if data, err := os.ReadFile(file); err == nil {
		parsed := map[string]interface{}{}
		if json.Unmarshal(data, &parsed) == nil {
			raw = parsed
		}
	}
	if _, ok := raw["mcpServers"]; !ok {
		raw["mcpServers"] = map[string]interface{}{}
	}
	servers, _ := raw["mcpServers"].(map[string]interface{})
	if servers == nil {
		servers = map[string]interface{}{}
		raw["mcpServers"] = servers
	}
	// Find the synapses binary path. Prefer the one on PATH so it survives updates.
	bin := "synapses"
	if p, err := exec.LookPath("synapses"); err == nil {
		bin = p
	}
	servers["synapses"] = map[string]interface{}{
		"command": bin,
		"args":    []string{"start"},
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, append(out, '\n'), 0o644)
}

// windsurfGlobalMCPPath returns the global MCP config path for Windsurf.
// Windsurf only reads MCP servers from this global file — no project-level support.
func windsurfGlobalMCPPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codeium", "windsurf", "mcp_config.json")
}

// antigravityGlobalMCPPath returns the global MCP config path for Antigravity.
// Antigravity only reads MCP servers from this global file — no project-level support.
func antigravityGlobalMCPPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "antigravity", "mcp_config.json")
}

// removeSynapsesSection strips the <!-- synapses:start --> ... <!-- synapses:end -->
// section from content, returning the remaining text.
func removeSynapsesSection(content string) string {
	startIdx := strings.Index(content, synapsesSectionStart)
	if startIdx == -1 {
		return content
	}
	endIdx := strings.Index(content, synapsesSectionEnd)
	if endIdx == -1 {
		return content
	}
	return strings.TrimSpace(content[:startIdx]+content[endIdx+len(synapsesSectionEnd):]) + "\n"
}

// writeZedMCPConfig merges a synapses entry into .zed/settings.json using
// Zed's context_servers format for HTTP MCP servers.
func writeZedMCPConfig(repoRoot string) error {
	file := filepath.Join(repoRoot, ".zed", "settings.json")
	raw := map[string]interface{}{}
	if data, err := os.ReadFile(file); err == nil {
		json.Unmarshal(data, &raw) //nolint:errcheck
	}
	cs, _ := raw["context_servers"].(map[string]interface{})
	if cs == nil {
		cs = map[string]interface{}{}
		raw["context_servers"] = cs
	}
	cs["synapses"] = map[string]interface{}{
		"settings": map[string]interface{}{"url": mcpURL(repoRoot)},
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".zed"), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, append(out, '\n'), 0o644)
}

// writeGuidanceFile writes (or updates) the Synapses guidance section in a
// plain-markdown rules file (e.g. .windsurfrules). frontmatter is prepended
// only on first creation; subsequent runs update only the synapses section.
func writeGuidanceFile(repoRoot, file, frontmatter string) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	section := synapsesSection
	if cfg, err := config.Load(repoRoot); err == nil && cfg.Mode == "knowledge" {
		section = knowledgeSynapsesSection
	}
	existing, _ := os.ReadFile(file)
	content := string(existing)
	if si := strings.Index(content, synapsesSectionStart); si != -1 {
		ei := strings.Index(content, synapsesSectionEnd)
		if ei != -1 {
			content = content[:si] + section + content[ei+len(synapsesSectionEnd):]
		} else {
			content = content[:si] + section
		}
	} else {
		if frontmatter != "" && len(content) == 0 {
			content = frontmatter
		} else if len(content) > 0 && !strings.HasSuffix(content, "\n\n") {
			if strings.HasSuffix(content, "\n") {
				content += "\n"
			} else {
				content += "\n\n"
			}
		}
		content += section + "\n"
	}
	return os.WriteFile(file, []byte(content), 0o644)
}

// cmdConnect wires an AI coding agent into an indexed project. For each agent
// it writes the MCP config file and an agent-specific guidance/rules file so
// the AI uses Synapses tools by default. For Claude Code it also writes
// .claude/settings.json (hooks + permissions).
func cmdConnect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	agent := fs.String("agent", "", "Agent to connect: claude, cursor, windsurf, zed, antigravity")
	repoPath := fs.String("path", ".", "Path to the project root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *agent == "" {
		return fmt.Errorf("--agent is required (claude, cursor, windsurf, zed, antigravity)")
	}
	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	type result struct {
		path string
		err  error
	}
	var results []result
	add := func(path string, err error) {
		results = append(results, result{path, err})
	}

	switch *agent {
	case "claude":
		mcpFile := filepath.Join(absPath, ".mcp.json")
		add(mcpFile, writeHTTPMCPServerEntry(mcpFile, absPath))
		claudeMD := filepath.Join(absPath, ".claude", "CLAUDE.md")
		add(claudeMD, writeProjectCLAUDE(absPath))
		settingsFile := filepath.Join(absPath, ".claude", "settings.json")
		add(settingsFile, writeClaudeSettings(absPath))

	case "cursor":
		mcpFile := filepath.Join(absPath, ".cursor", "mcp.json")
		add(mcpFile, writeHTTPMCPServerEntry(mcpFile, absPath))
		rulesFile := filepath.Join(absPath, ".cursor", "rules", "synapses.mdc")
		frontmatter := "---\ndescription: Synapses code intelligence — always use these MCP tools for code exploration\nalwaysApply: true\n---\n\n"
		add(rulesFile, writeGuidanceFile(absPath, rulesFile, frontmatter))

	case "windsurf":
		// Windsurf only reads MCP from global path. Use stdio so it works for any project.
		mcpFile := windsurfGlobalMCPPath()
		add(mcpFile, writeStdioMCPServerEntry(mcpFile))
		rulesFile := filepath.Join(absPath, ".windsurf", "rules", "synapses.md")
		add(rulesFile, writeGuidanceFile(absPath, rulesFile, ""))

	case "zed":
		settingsFile := filepath.Join(absPath, ".zed", "settings.json")
		add(settingsFile, writeZedMCPConfig(absPath))
		rulesFile := filepath.Join(absPath, ".rules")
		add(rulesFile, writeGuidanceFile(absPath, rulesFile, ""))

	case "antigravity":
		// Antigravity only reads MCP from global path. Use stdio so it works for any project.
		mcpFile := antigravityGlobalMCPPath()
		add(mcpFile, writeStdioMCPServerEntry(mcpFile))
		rulesFile := filepath.Join(absPath, "AGENTS.md")
		add(rulesFile, writeGuidanceFile(absPath, rulesFile, ""))

	default:
		return fmt.Errorf("unknown agent %q — supported: claude, cursor, windsurf, zed, antigravity", *agent)
	}

	for _, r := range results {
		if r.err != nil {
			logutil.Warn("%s: %v\n", r.path, r.err)
		} else {
			fmt.Printf("  wrote %s\n", r.path)
		}
	}
	return nil
}

func printSummaryTable(
	identity *graph.ProjectIdentity,
	elapsed time.Duration,
	edgeCounts map[graph.EdgeType]int,
	fileCount, staticRuleCount, dynamicRuleCount, violationCount int,
) {
	fmt.Printf("Repository:  %s\n", identity.RepoID)
	fmt.Printf("─────────────────────────────\n")
	fmt.Printf("  Files      %6d\n", identity.Summary.Files)
	fmt.Printf("  Packages   %6d\n", identity.Summary.Packages)
	fmt.Printf("  Functions  %6d\n", identity.Summary.Functions)
	fmt.Printf("  Methods    %6d\n", identity.Summary.Methods)
	fmt.Printf("  Structs    %6d\n", identity.Summary.Structs)
	fmt.Printf("  Interfaces %6d\n", identity.Summary.Interfaces)
	fmt.Printf("  Edges      %6d\n", identity.Summary.Edges)
	fmt.Printf("─────────────────────────────\n")
	if elapsed > 0 {
		fmt.Printf("Indexed in %s\n", elapsed.Round(time.Millisecond))
	}

	// Edge breakdown by type.
	if len(edgeCounts) > 0 {
		fmt.Printf("\n── Edges by type ────────────────\n")
		edgeOrder := []graph.EdgeType{
			graph.EdgeCalls, graph.EdgeImplements, graph.EdgeImports,
			graph.EdgeEmbeds, graph.EdgeDefines, graph.EdgeDependsOn, graph.EdgeExports,
		}
		for _, et := range edgeOrder {
			if n, ok := edgeCounts[et]; ok && n > 0 {
				fmt.Printf("  %-12s %6d\n", string(et), n)
			}
		}
	}

	// Rule and violation summary.
	fmt.Printf("\n── Rules ────────────────────────\n")
	totalRules := staticRuleCount + dynamicRuleCount
	if totalRules == 0 {
		fmt.Printf("  No rules configured\n")
	} else {
		fmt.Printf("  Active: %d (%d static + %d dynamic)\n", totalRules, staticRuleCount, dynamicRuleCount)
	}
	if violationCount == 0 {
		fmt.Printf("  Current violations: 0\n")
	} else {
		fmt.Printf("  Current violations: %d  ← run get_violations for details\n", violationCount)
	}

	// Index metadata.
	fmt.Printf("\n── Index ────────────────────────\n")
	if fileCount > 0 {
		fmt.Printf("  Indexed files: %d\n", fileCount)
	}
	fmt.Printf("  Cache: 20-slot FIFO, 30s TTL\n")

	if len(identity.EntryPoints) > 0 {
		fmt.Printf("\nEntry points:\n")
		for _, ep := range identity.EntryPoints {
			fmt.Printf("  %s  (%s:%d)\n", ep.Name, ep.File, ep.Line)
		}
	}
	if len(identity.KeyEntities) > 0 {
		fmt.Printf("\nKey entities (by connectivity):\n")
		for _, e := range identity.KeyEntities {
			fmt.Printf("  %-30s  fanin=%-3d fanout=%-3d  %s\n",
				e.Name, e.Fanin, e.Fanout, e.File)
		}
	}
}

func printUsage() {
	fmt.Printf(`Synapses %s — code intelligence for AI agents

USAGE:
  synapses <command> [flags]

COMMANDS:
  init      [--path <dir>] [--yes]           Set up a project (index + daemon + agents)
  start     [--path <dir>]                   Start MCP server for a project
  stop                                       Stop the daemon
  status    [--path <dir>] [--all]            Health check and project status
  index     [--path <dir>] [--reset] [--all]  Build or reset the code graph
  config    [--show] [--global] [key] [val]   Read/write configuration
  connect   [--agent <name>] [--path <dir>]   Connect an AI agent
  update    [--check] [--rollback]            Self-update or rollback
  remove    [--path <dir>]                    Remove Synapses from a project
  uninstall                                   Remove Synapses from the system

SUBCOMMANDS:
  dev       link|unlink|status               Developer binary management
  daemon    serve|install|uninstall|logs      Low-level daemon control

OTHER:
  version, completion <bash|zsh|fish>, help
`, version)
}

// cmdMCPSetup is replaced by "synapses init" — see init.go.

// buildIngestCode constructs the ingest code string for a node (signature + doc comment).
func buildIngestCode(n *graph.Node) string {
	code := ""
	if n.Metadata != nil {
		if sig := n.Metadata["signature"]; sig != "" {
			code = sig
		}
		if doc := n.Metadata["doc"]; doc != "" && code != "" {
			code = "// " + doc + "\n" + code
		}
	}
	return code
}

// fetchTopNSummaries eagerly fetches summaries for the top N most-connected
// nodes and writes them back as annotations. Called with a short initial wait
// so hot entities are enriched before the first agent context request.
// Runs concurrently with bulkIngestToBrain + fetchAndWriteBackSummaries.
// ctx should be the daemon's appCtx so goroutines cancel on shutdown.
func fetchTopNSummaries(ctx context.Context, bc *brain.Client, g *graph.Graph, st *store.Store, n int) {
	if st == nil || n <= 0 {
		return
	}
	all := g.AllNodes()
	// Collect non-structural nodes and sort by fanin descending.
	nodes := make([]*graph.Node, 0, len(all))
	for _, node := range all {
		t := string(node.Type)
		if t == "package" || t == "file" {
			continue
		}
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return g.Fanin(nodes[i].ID) > g.Fanin(nodes[j].ID)
	})
	if len(nodes) > n {
		nodes = nodes[:n]
	}

	// Poll brain readiness using HealthCheck (unambiguous signal: success means
	// brain is reachable and serving, regardless of whether summaries exist yet).
	// Up to 3 attempts with 1s gaps. Exits immediately if ctx is cancelled.
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return // daemon shutting down
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := bc.HealthCheck(probeCtx)
		probeCancel()
		if err == nil {
			break // brain is reachable
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
	// If ctx was cancelled during polling, exit before spawning goroutines.
	if ctx.Err() != nil {
		return
	}

	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	written := 0
	var mu sync.Mutex
	for _, node := range nodes {
		wg.Add(1)
		sem <- struct{}{}
		go func(nd *graph.Node) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return // daemon shutting down
			}
			summary := bc.GetSummary(ctx, string(nd.ID))
			if summary == "" || strings.Contains(strings.ToLower(summary), "in progress") {
				return
			}
			if _, ok, err := st.AddAnnotationIfNew(string(nd.ID), "brain", summary, 24*time.Hour); err == nil && ok {
				mu.Lock()
				written++
				mu.Unlock()
			}
		}(node)
	}
	wg.Wait()
	logutil.Info("synapses: eager brain write-back complete (%d/%d top entities enriched)\n", written, len(nodes))
}

// bulkIngestToBrain sends all code nodes to the brain sidecar for prose summary generation.
// With qwen3.5:0.8b as the ingest model (~3s per node on CPU), a 500-node codebase
// completes in ~3min at 8× concurrency — runs in background, does not block startup.
// Summaries are stored in brain.sqlite and surfaced in get_context responses.
// Sort order: high-fanin nodes first so the most-used code gets summaries soonest.
// mainEmbedResolver adapts embed.Client + store.Store into the resolver.EmbedResolver
// interface for post-embed discovery passes (doc↔code linking, knowledge relations).
type mainEmbedResolver struct {
	ec embed.Embedder
	st *store.Store
}

func (r *mainEmbedResolver) EmbedText(ctx context.Context, text string) ([]float32, error) {
	return r.ec.Embed(ctx, text)
}

func (r *mainEmbedResolver) SearchByVector(queryVec []float32, k int) []resolver.EmbedMatch {
	results, err := r.st.VectorSearch(queryVec, k)
	if err != nil || len(results) == 0 {
		return nil
	}
	out := make([]resolver.EmbedMatch, len(results))
	for i, sr := range results {
		out[i] = resolver.EmbedMatch{NodeID: sr.ID, Score: sr.Score}
	}
	return out
}

// embedAllNodes generates vector embeddings for every graph node that does not
// yet have one, storing results in the node_embeddings table. Runs in a
// background goroutine after startup so the MCP server is never delayed.
// Rate-limited to ~10 req/s to avoid saturating a local Ollama instance.
// Fail-silent: any error per-node is logged to stderr and skipped.
func embedAllNodes(ctx context.Context, ec embed.Embedder, g *graph.Graph, st *store.Store, onComplete ...func()) {
	if ec == nil || st == nil {
		return
	}

	nodeIDs, err := st.GetNodesWithoutEmbeddings(0) // 0 = no limit (includes stale)
	if err != nil || len(nodeIDs) == 0 {
		return
	}

	logutil.Info("synapses: embedding %d nodes (model: %s) …\n", len(nodeIDs), ec.Model())

	// Check if embedder supports batch embedding (embed.Client, BuiltinEmbedder do).
	type batchEmbedder interface {
		EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
	}
	batcher, hasBatch := ec.(batchEmbedder)

	const batchSize = 16
	done := 0

	for i := 0; i < len(nodeIDs); i += batchSize {
		select {
		case <-ctx.Done():
			return
		default:
		}

		end := i + batchSize
		if end > len(nodeIDs) {
			end = len(nodeIDs)
		}
		chunk := nodeIDs[i:end]

		// Collect texts; skip nodes with no embeddable content.
		texts := make([]string, 0, len(chunk))
		validIDs := make([]string, 0, len(chunk))
		for _, nodeID := range chunk {
			text, ok := st.GetNodeTextForEmbedding(nodeID)
			if !ok || text == "" {
				continue
			}
			texts = append(texts, text)
			validIDs = append(validIDs, nodeID)
		}
		if len(texts) == 0 {
			continue
		}

		// Try batch embed; fall back to per-node on error or length mismatch.
		var vecs [][]float32
		var batchErr error
		if hasBatch {
			batchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			vecs, batchErr = batcher.EmbedBatch(batchCtx, texts)
			cancel()
		} else {
			batchErr = fmt.Errorf("no batch support")
		}

		if batchErr != nil || len(vecs) != len(texts) {
			// Fallback: embed each node individually (fail-silent per node).
			for j, nodeID := range validIDs {
				sCtx, sCancel := context.WithTimeout(ctx, 10*time.Second)
				vec, sErr := ec.Embed(sCtx, texts[j])
				sCancel()
				if sErr != nil {
					continue
				}
				if err := st.UpsertEmbedding(nodeID, ec.Model(), vec); err != nil {
					logutil.Error("synapses: embed store error for %s: %v\n", nodeID, err)
				}
				done++
			}
			continue
		}

		// Store all batch results.
		for j, nodeID := range validIDs {
			if err := st.UpsertEmbedding(nodeID, ec.Model(), vecs[j]); err != nil {
				logutil.Error("synapses: embed store error for %s: %v\n", nodeID, err)
			}
			done++
		}
	}

	logutil.Info("synapses: embedding complete (%d/%d nodes indexed)\n", done, len(nodeIDs))

	// Rebuild HNSW so new vectors are immediately searchable.
	if done > 0 {
		st.RebuildNodeHNSW()
	}

	// Post-embed discovery: create doc↔code, knowledge, and community edges
	// now that HNSW is populated. These passes are idempotent.
	if g != nil {
		er := &mainEmbedResolver{ec: ec, st: st}
		dcCount := resolver.DiscoverDocCodeRelations(g, er, 0.60)
		erCount := resolver.DiscoverEmbedRelations(g, er, 0.55)
		comCount := resolver.DetectCommunities(g, 10)
		if dcCount+erCount+comCount > 0 {
			logutil.Info("synapses: post-embed discovery: %d doc-code, %d relations, %d communities\n",
				dcCount, erCount, comCount)
			// Persist new discovery edges to SQLite.
			var newEdges []graph.Edge
			for _, e := range g.AllEdges() {
				switch e.Type {
				case graph.EdgeExplains, graph.EdgeDocumentedBy, graph.EdgeRelatesTo,
					graph.EdgeCausedBy, graph.EdgeInstanceOf, graph.EdgeContradicts:
					newEdges = append(newEdges, *e)
				}
			}
			if len(newEdges) > 0 {
				if err := st.SaveDiscoveryEdges(newEdges); err != nil {
					logutil.Warn("synapses: persist discovery edges: %v\n", err)
				} else {
					logutil.Info("synapses: persisted %d discovery edges\n", len(newEdges))
				}
			}
		}
	}

	for _, fn := range onComplete {
		fn()
	}
}

// createMemoryEmbedder creates a memory Embedder based on the config's Embeddings mode.
// Returns nil if embeddings are disabled.
func createMemoryEmbedder(cfg *config.Config) embed.Embedder {
	mode := cfg.Embeddings
	if mode == "" {
		mode = detectEmbedMode()
	}
	switch mode {
	case "off":
		return nil
	case "ollama":
		endpoint := cfg.EmbeddingEndpoint
		if endpoint == "" {
			endpoint = "http://localhost:11434/api/embeddings"
		}
		e := embed.NewOllamaEmbedder(endpoint, cfg.EmbeddingModel)
		if e == nil {
			return nil
		}
		// Validate the model is actually available before committing to Ollama.
		// If warmup fails (model not pulled, server flaky), fall back to builtin
		// ONNX so embeddings still work — just slower.
		if err := e.WarmUp(context.Background()); err != nil {
			logutil.Warn("synapses: ollama embedder warmup failed (%v) — falling back to builtin\n", err)
			return createBuiltinEmbedder(cfg)
		}
		logutil.Info("synapses: memory embeddings via ollama (%s)\n", endpoint)
		return e
	case "builtin":
		return createBuiltinEmbedder(cfg)
	default:
		logutil.Warn("synapses: unknown embeddings mode %q, disabling\n", mode)
		return nil
	}
}

// createBuiltinEmbedder creates the in-process ONNX embedder. Extracted so
// the Ollama path can fall back to it when warmup fails.
func createBuiltinEmbedder(cfg *config.Config) embed.Embedder {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		logutil.Error("synapses: cannot determine home dir for builtin embeddings: %v\n", err)
		return nil
	}
	modelsDir := filepath.Join(homeDir, ".synapses", "models")
	logutil.Info("synapses: memory embeddings via builtin nomic-embed-text-v1.5\n")
	var e *embed.BuiltinEmbedder
	if cfg.EmbedPoolSize > 0 {
		e = embed.NewBuiltinEmbedderWithPoolSize(modelsDir, cfg.EmbedPoolSize)
	} else {
		e = embed.NewBuiltinEmbedder(modelsDir)
	}
	return e
}

// detectEmbedMode probes localhost:11434 for a running Ollama instance.
// If Ollama is reachable (responds within 500ms), returns "ollama" for 20x
// faster Metal GPU embeddings. Otherwise falls back to "builtin" (in-process
// ONNX on CPU). This makes Ollama the default when available without requiring
// explicit config — users can still override via synapses.json "embeddings" field.
func detectEmbedMode() string {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get("http://localhost:11434/api/version")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			logutil.Info("synapses: detected Ollama at localhost:11434 — using ollama embeddings\n")
			return "ollama"
		}
	}
	return "builtin"
}

// embedAllMemories generates embeddings for all un-embedded memories.
// Wraps the mcp package helper for use from main. pc may be nil (pulse disabled).
func embedAllMemories(ctx context.Context, embedder embed.Embedder, st *store.Store, pc *pulse.Client) {
	mcpsrv.EmbedAllMemories(ctx, embedder, st, pc)
}

// pathProjectID returns a stable 8-hex-char project identifier derived from the project root path.
func pathProjectID(absPath string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(absPath))
	return fmt.Sprintf("%x", h.Sum32())
}

func bulkIngestToBrain(ctx context.Context, bc *brain.Client, g *graph.Graph, projectID string) {
	all := g.AllNodes()

	// Collect all non-structural nodes (skip package/file nodes — no code to summarize).
	nodes := make([]*graph.Node, 0, len(all))
	for _, n := range all {
		t := string(n.Type)
		if t == "package" || t == "file" {
			continue
		}
		nodes = append(nodes, n)
	}

	// Sort by caller count descending — most-connected nodes get summaries first.
	sort.Slice(nodes, func(i, j int) bool {
		return g.Fanin(nodes[i].ID) > g.Fanin(nodes[j].ID)
	})

	sem := make(chan struct{}, 8) // 8 concurrent — qwen3.5:0.8b is fast enough to handle more
	var wg sync.WaitGroup
dispatch:
	for _, n := range nodes {
		// Acquire semaphore slot and check cancellation atomically.
		// A two-step check+block would allow the goroutine to block on sem
		// after cancellation, holding up shutdown for the duration of LLM calls.
		select {
		case <-ctx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(node *graph.Node) {
			defer wg.Done()
			defer func() { <-sem }()
			bc.Ingest(ctx, brain.IngestRequest{
				ProjectID: projectID,
				NodeID:    string(node.ID),
				NodeName:  node.Name,
				NodeType:  string(node.Type),
				Package:   node.Package,
				Code:      buildIngestCode(node),
			})
		}(n)
	}
	wg.Wait()
	logutil.Info("synapses: ingested %d nodes to brain (full coverage)\n", len(nodes))
}

// fetchAndWriteBackSummaries waits for the brain to process ingested nodes,
// then fetches each node's summary and writes it back as a graph annotation.
// This surfaces brain summaries in get_context.annotations, find_entity, etc.
// Runs after bulkIngestToBrain completes; all errors are silently discarded.
func fetchAndWriteBackSummaries(ctx context.Context, bc *brain.Client, g *graph.Graph, st *store.Store) {
	if st == nil {
		return
	}
	// Give the brain time to process the ingest queue before fetching summaries.
	// Use ctx-aware sleep so shutdown cancels the wait immediately.
	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Second):
	}

	nodes := g.AllNodes()
	sem := make(chan struct{}, 4) // 4 concurrent fetches (brain's LLM is mostly single-threaded)
	var wg sync.WaitGroup
	written := 0
	var mu sync.Mutex
dispatch:
	for _, n := range nodes {
		if string(n.Type) == "package" || string(n.Type) == "file" {
			continue
		}
		// Acquire semaphore slot and check cancellation atomically so shutdown
		// cannot be blocked by a slow LLM call holding all semaphore slots.
		select {
		case <-ctx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(node *graph.Node) {
			defer wg.Done()
			defer func() { <-sem }()
			summary := bc.GetSummary(ctx, string(node.ID))
			if summary != "" {
				// Use AddAnnotationIfNew with a 24-hour window to prevent duplicate
				// brain annotations when the daemon is restarted within a day.
				if _, ok, err := st.AddAnnotationIfNew(string(node.ID), "brain", summary, 24*time.Hour); err == nil && ok {
					mu.Lock()
					written++
					mu.Unlock()
				}
			}
		}(n)
	}
	wg.Wait()
	logutil.Info("synapses: brain write-back complete (%d summaries stored)\n", written)
}

// cmdSetup is replaced by "synapses init" — see init.go.

// offerGitInit checks whether absPath has a git repository. If not, it
// prompts the user to initialize one.  This is called during "synapses index"
// which is always run by a human at a terminal.
//
// Git gives synapses 6 features: churn analysis, blame/ownership tracking,
// commit context, working state diffs, task commit linking, and federation
// drift detection.  Without git, synapses still works but with reduced
// intelligence.
func offerGitInit(absPath string) {
	// Already has git?
	dotGit := filepath.Join(absPath, ".git")
	if info, err := os.Stat(dotGit); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
		return
	}

	fmt.Println()
	fmt.Println("  No git repository detected.")
	fmt.Println()
	fmt.Println("  Git enables richer intelligence for synapses:")
	fmt.Println("    - Churn analysis    (which files change most)")
	fmt.Println("    - Blame tracking    (who owns what code)")
	fmt.Println("    - Commit context    (why code was changed)")
	fmt.Println("    - Working state     (what changed since last commit)")
	fmt.Println("    - Task tracking     (link tasks to commits)")
	fmt.Println("    - Drift detection   (detect cross-project breakage)")
	fmt.Println()
	fmt.Printf("  Initialize git repository? [Y/n] ")

	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))

	if answer != "" && answer != "y" && answer != "yes" {
		fmt.Println("  Skipped. You can run 'git init' later to enable these features.")
		fmt.Println()
		return
	}

	ctx30, cancel30 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel30()
	cmd := exec.CommandContext(ctx30, "git", "init", absPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("  git init failed: %v\n  %s\n", err, strings.TrimSpace(string(out)))
		return
	}
	fmt.Printf("  Initialized git repository at %s\n", absPath)
	fmt.Println()
}
