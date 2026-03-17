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
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/contextfile"
	"github.com/SynapsesOS/synapses/internal/dataflow"
	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/graph"
	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
	"github.com/SynapsesOS/synapses/internal/metrics"
	"github.com/SynapsesOS/synapses/internal/skills"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/peer"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/scout"
	"github.com/SynapsesOS/synapses/internal/webcache"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/SynapsesOS/synapses/internal/watcher"
)

// version is set at build time via ldflags: -X main.version=<tag>
var version = "dev"

func main() {
	// Fast-path: print version and exit immediately.
	// This avoids loading SQLite drivers and other heavy init() code
	// that runs before main(). Keeps `synapses version` zero-cost.
	if len(os.Args) >= 2 && (os.Args[1] == "version" || os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("synapses %s\n", version)
		return
	}

	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}

	switch args[0] {
	case "init":
		return cmdInit(args[1:])
	case "start":
		return cmdStartProxy(args[1:])
	case "stop":
		return cmdStop(args[1:])
	case "projects":
		return cmdProjects(args[1:])
	case "logs":
		return cmdLogs(args[1:])
	case "index":
		return cmdIndex(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "list":
		return cmdList(args[1:])
	case "reset":
		return cmdReset(args[1:])
	case "version", "--version", "-v":
		fmt.Printf("synapses %s\n", version)
		return nil
	case "brain":
		return cmdBrain(args[1:])
	case "setup":
		return cmdSetup(args[1:])
	case "mcp-setup":
		return cmdMCPSetup(args[1:])
	case "query":
		return cmdQuery(args[1:])
	case "export":
		return cmdExport(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	case "daemon":
		return cmdDaemon(args[1:])
	case "brief":
		return cmdBrief(args[1:])
	case "onboard":
		return cmdOnboard(args[1:])
	case "connect":
		return cmdConnect(args[1:])
	case "memory":
		return cmdMemory(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q — run 'synapses help'", args[0])
	}
}

// cmdStartDirect runs the MCP server directly on stdio (legacy mode).
// Used when "synapses start --direct" is passed, or for debugging.
// In normal operation, "synapses start" uses the proxy+daemon model instead.
func cmdStartDirect(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Path to the repository root")
	forceReindex := fs.Bool("reindex", false, "Force a full re-index even if cache is fresh")
	noWatch := fs.Bool("no-watch", false, "Disable the file watcher (useful for read-only mounts)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// appCtx is cancelled on SIGINT/SIGTERM so background goroutines can exit
	// gracefully. Declared early so it's available throughout cmdStart.
	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	// CONFIG-01: walk upward to find synapses.json (like .git discovery).
	// The index path (absPath) stays unchanged — we only adjust where config is loaded from.
	cfgDir, found := config.FindConfigDir(absPath)
	if found && cfgDir != absPath {
		fmt.Fprintf(os.Stderr, "synapses: using config from %s\n", cfgDir)
	}
	cfg, err := config.Load(cfgDir)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Open the store once and keep it open for the watcher to use.
	dbPath, err := store.DefaultPath(absPath)
	if err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// Prune stale operational data in the background (30-day retention).
	go st.PruneStaleData(30)

	g, err := loadOrBuildGraphWithStore(absPath, st, *forceReindex, cfg.Plugins)
	if err != nil {
		return err
	}

	// Federation: merge linked project graphs (monorepo support).
	for _, linkedPath := range cfg.Linked {
		if mergeErr := mergeLinkedProject(g, linkedPath); mergeErr != nil {
			fmt.Fprintf(os.Stderr, "synapses: skipping linked project %s: %v\n", linkedPath, mergeErr)
		}
	}

	// Re-resolve cross-project CALLS now that linked nodes are in the graph.
	// Reload the persisted call sites and run the resolver again; existing
	// intra-project CALLS edges are skipped via the seen-set in the resolver.
	if len(cfg.Linked) > 0 {
		if sites, err := st.LoadCallSites(); err == nil && len(sites) > 0 {
			for _, cs := range sites {
				g.AddCallSite(cs)
			}
			if n := resolver.ResolveCallEdges(g); n > 0 {
				fmt.Fprintf(os.Stderr, "synapses: resolved %d cross-project CALLS edges\n", n)
			}
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

	// Build the columnar GraphIndex asynchronously so startup is not blocked.
	// BFS queries fall back to the pointer-map path until the index is ready.
	// Persist the snapshot blob so warm-boot loads skip re-parsing.
	go func() {
		blob, err := g.RebuildIndex()
		if err == nil && len(blob) > 0 {
			_ = st.SaveIndexSnapshot(blob)
		}
	}()

	// Background idle-defrag goroutine.
	// If >15% of the columnar index is tombstoned and no file has changed in the
	// last 5 minutes, trigger a full index rebuild to compact the dead entries.
	go func() {
		const (
			checkInterval  = 60 * time.Second
			idleThreshold  = 5 * time.Minute
			tombstoneLimit = 0.15
		)
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for range ticker.C {
			idx := g.Index()
			if idx == nil || !idx.Ready() {
				continue
			}
			if idx.TombstoneRatio() < tombstoneLimit {
				continue
			}
			// Only defrag if the watcher reports idle (no changes recently).
			// We check this by looking at whether the graph's node count is stable
			// (a rough proxy — the watcher's RecentChanges API would be cleaner but
			// watcher is not accessible here without a circular import).
			blob, err := g.RebuildIndex()
			if err == nil && len(blob) > 0 {
				_ = st.SaveIndexSnapshot(blob)
				fmt.Fprintf(os.Stderr, "synapses: idle defrag complete (tombstone ratio was %.0f%%)\n",
					idx.TombstoneRatio()*100)
			}
		}
	}()

	// Create the MCP server early so we can wire the watcher into it below.
	mcpsrv.Version = version
	srv := mcpsrv.New(g, cfg, st)
	srv.SetProjectID(pathProjectID(absPath))
	srv.StartBackground()
	defer srv.Close()

	// Load activation-context prompts from all scopes (fail-silent per scope).
	// Order matters: builtin < user < project (project can override user).
	{
		var allPrompts []skills.PromptTemplate
		allPrompts = append(allPrompts, skills.BuiltinPrompts()...)
		if homeDir, err := os.UserHomeDir(); err == nil {
			userDir := filepath.Join(homeDir, ".synapses", "prompts")
			if pts, err := skills.LoadPromptDir(userDir, "user"); err == nil {
				allPrompts = append(allPrompts, pts...)
			}
		}
		projectDir := filepath.Join(absPath, ".synapses", "prompts")
		if pts, err := skills.LoadPromptDir(projectDir, "project"); err == nil {
			allPrompts = append(allPrompts, pts...)
		}
		allPrompts = skills.DeduplicatePrompts(allPrompts) // project overrides user, user overrides builtin
		if len(allPrompts) > 0 {
			srv.SetPromptTemplates(allPrompts)
			fmt.Fprintf(os.Stderr, "synapses: loaded %d activation-context prompts\n", len(allPrompts))
		}
	}

	// Load skill recipes from all scopes (fail-silent per scope).
	// Order: builtin < user < project (project recipes take precedence by ID).
	{
		allRecipes := skills.BuiltinRecipes()
		if homeDir, err := os.UserHomeDir(); err == nil {
			userDir := filepath.Join(homeDir, ".synapses", "skills")
			if rs, err := skills.LoadRecipeDir(userDir, "user"); err == nil {
				allRecipes = append(allRecipes, rs...)
			}
		}
		projectDir := filepath.Join(absPath, ".synapses", "skills")
		if rs, err := skills.LoadRecipeDir(projectDir, "project"); err == nil {
			allRecipes = append(allRecipes, rs...)
		}
		allRecipes = skills.DeduplicateRecipes(allRecipes) // project overrides user, user overrides builtin
		srv.SetSkillRecipes(allRecipes)
		fmt.Fprintf(os.Stderr, "synapses: loaded %d skill recipes\n", len(allRecipes))
	}

	// Start the peer API server if configured. Non-fatal on failure.
	if cfg.PeerAPIPort > 0 {
		peerSrv := peer.NewPeerServer(g, cfg, st)
		if err := peerSrv.Start(cfg.PeerAPIPort); err != nil {
			fmt.Fprintf(os.Stderr, "synapses: peer API: %v\n", err)
		} else {
			defer peerSrv.Stop()
			fmt.Fprintf(os.Stderr, "synapses: peer API listening on :%d\n", cfg.PeerAPIPort)
		}
	}

	// Connect to configured peers (non-blocking, 5s timeout per peer).
	// Start a health monitor that reconnects every 30s.
	if len(cfg.Peers) > 0 {
		pm := peer.NewPeerManager(cfg, g, st)
		pm.Connect()
		pm.StartHealthMonitor(30 * time.Second)
		defer pm.Stop()
		srv.SetPeerManager(pm)
	}

	// Brain — now in-process; no external sidecar or port required.
	brainCli := brain.NewInProcess(cfg.Brain.ToBrainConfig())
	if cfg.Brain.Enabled {
		if model, _ := brainCli.HealthCheck(context.Background()); model != "" {
			fmt.Fprintf(os.Stderr, "synapses: brain enabled in-process (%s)\n", model)
		} else {
			fmt.Fprintf(os.Stderr, "synapses: brain enabled in-process (Ollama not yet reachable — will retry on use)\n")
		}
		srv.SetBrainClient(brainCli)
		go func() {
			bulkIngestToBrain(brainCli, g, pathProjectID(absPath))
			fetchAndWriteBackSummaries(brainCli, g, st)
		}()
	}

	// Web doc cache: version-pinned Go package docs, cached locally in SQLite.
	if st != nil {
		wc := webcache.New(st)
		srv.SetWebCache(wc)
		srv.SetProjectPath(absPath)
		go webcache.IndexProjectImports(appCtx, absPath, g, wc, 20)
	}

	// Optional: connect to synapses-pulse analytics sidecar.
	// Pulse is fire-and-forget: if unreachable at startup or during operation,
	// all errors are silently discarded and the MCP server continues normally.
	if cfg.Pulse.URL != "" {
		pulseCli := pulse.NewClient(cfg.Pulse.URL, cfg.Pulse.TimeoutSec)
		srv.SetPulseClient(pulseCli)
		fmt.Fprintf(os.Stderr, "synapses: pulse analytics enabled at %s\n", cfg.Pulse.URL)
	}

	// Optional: vector embedding for semantic search.
	// Priority: (1) brain /v1/embed when brain is connected and embedding is available,
	//           (2) explicit embedding_endpoint in synapses.json (Ollama/OpenAI compat),
	//           (3) FTS5-only fallback (no embeddings).
	// Embeddings are built/updated in the background so startup is never delayed.
	{
		var embedCli *embed.Client
		if cfg.EmbeddingEndpoint != "" {
			embedCli = embed.NewClient(cfg.EmbeddingEndpoint, "")
			fmt.Fprintf(os.Stderr, "synapses: embeddings via %s\n", cfg.EmbeddingEndpoint)
		}
		if embedCli != nil {
			srv.SetEmbedClient(embedCli)
			go embedAllNodes(appCtx, embedCli, g, st)
		}
	}

	// Autosubscribe: detect tech stack from manifest files.
	go func() {
		entries := scout.DetectTechStack(absPath)
		if len(entries) == 0 {
			return
		}
		srv.SetTechStack(entries)
		fmt.Fprintf(os.Stderr, "synapses: tech stack detected (%d deps)\n", len(entries))
	}()

	// Start the file watcher so the graph stays current as files change.
	if !*noWatch {
		w := parser.NewWalker()
		for _, p := range cfg.Plugins {
			w.RegisterPlugin(p.Extensions, p.Command)
		}
		fw, err := watcher.New(g, w, st)
		if err != nil {
			// Non-fatal: log and continue without watching.
			fmt.Fprintf(os.Stderr, "synapses: file watcher unavailable: %v\n", err)
		} else {
			if err := fw.Start(absPath); err != nil {
				fmt.Fprintf(os.Stderr, "synapses: file watcher start failed: %v\n", err)
			} else {
				defer fw.Stop()
				fw.SetConfig(cfg)                    // wire rules for proactive violation events
				fw.SetProjectID(pathProjectID(absPath)) // scope brain ingest to this project
				srv.SetChangeSource(fw)               // wire change log into get_working_state
				fw.SetPacketInvalidator(srv)          // clear brain packet cache on file change
				fw.SetBrainClient(brainCli)           // wire incremental ingest
				// Hot-reload synapses.json: reconnect brain when config changes.
				fw.SetConfigChangeHandler(func(newCfg *config.Config) {
					newBrain := brain.NewInProcess(newCfg.Brain.ToBrainConfig())
					srv.SetBrainClient(newBrain)
					fw.SetBrainClient(newBrain)
					fmt.Fprintf(os.Stderr, "synapses: brain reloaded (enabled=%v)\n", newCfg.Brain.Enabled)
				})
				fmt.Fprintf(os.Stderr, "synapses: watching %s for changes\n", absPath)
			}
		}
	}

	// Intercept OS signals so we can shut down cleanly (flush watcher, close store).
	// We call appCancel() which should cause ServeStdio() to return, allowing
	// all deferred cleanups (st.Close, watcher stop, peer stop) to execute.
	// A 5s safety-net timer ensures we exit even if ServeStdio hangs.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\nsynapses: received %s, shutting down\n", sig)
		appCancel()
		time.AfterFunc(5*time.Second, func() {
			fmt.Fprintf(os.Stderr, "synapses: graceful shutdown timed out, forcing exit\n")
			os.Exit(1)
		})
	}()

	// MCP server writes to stdout (protocol messages); all status goes to stderr.
	identity := g.ProjectIdentity()
	fmt.Fprintf(os.Stderr, "synapses %s ready — %d nodes, %d edges (repo: %s)\n",
		version,
		identity.Summary.Files+identity.Summary.Functions+
			identity.Summary.Structs+identity.Summary.Interfaces,
		identity.Summary.Edges, identity.RepoID)
	fmt.Fprintf(os.Stderr, "MCP server starting on stdio...\n")

	return srv.ServeStdio()
}

// cmdIndex parses the repo and saves to the persistent cache, then exits.
func cmdIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Path to the repository root")
	forceReindex := fs.Bool("reindex", false, "Force a full re-index even if cache is fresh")
	if err := fs.Parse(args); err != nil {
		return err
	}

	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

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

	g, err := loadOrBuildGraphWithStore(absPath, st, *forceReindex, cfg.Plugins)
	if err != nil {
		return err
	}

	// Federation: merge linked project graphs.
	for _, linkedPath := range cfg.Linked {
		if mergeErr := mergeLinkedProject(g, linkedPath); mergeErr != nil {
			fmt.Fprintf(os.Stderr, "synapses: skipping linked project %s: %v\n", linkedPath, mergeErr)
		}
	}

	// Re-resolve cross-project CALLS edges now that linked nodes are present.
	if len(cfg.Linked) > 0 {
		if sites, err := st.LoadCallSites(); err == nil && len(sites) > 0 {
			for _, cs := range sites {
				g.AddCallSite(cs)
			}
			if n := resolver.ResolveCallEdges(g); n > 0 {
				fmt.Fprintf(os.Stderr, "synapses: resolved %d cross-project CALLS edges\n", n)
			}
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
	pid, err := parseInt(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("invalid PID in %s: %w", pidPath, err)
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

// cmdProjects lists all projects currently registered with the singleton daemon.
func cmdProjects(args []string) error {
	if !IsSingletonDaemonRunning() {
		fmt.Fprintf(os.Stderr, "Daemon not running at %s.\n", DaemonHTTPAddr)
		return nil
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + DaemonHTTPAddr + "/api/admin/projects")
	if err != nil {
		return fmt.Errorf("query daemon: %w", err)
	}
	defer resp.Body.Close()
	var result struct {
		Projects []struct {
			Path   string `json:"path"`
			Hash   string `json:"hash"`
			Socket string `json:"socket"`
		} `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(result.Projects) == 0 {
		fmt.Println("No projects registered with daemon.")
		return nil
	}
	fmt.Printf("%-6s  %s\n", "HASH", "PATH")
	for _, p := range result.Projects {
		hash := p.Hash
		if len(hash) > 8 {
			hash = hash[:8]
		}
		fmt.Printf("%-6s  %s\n", hash, p.Path)
	}
	return nil
}

// cmdLogs tails the singleton daemon log file (~/.synapses/daemon.log).
func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	n := fs.Int("n", 50, "Number of lines to show")
	if err := fs.Parse(args); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find home dir: %w", err)
	}
	logPath := filepath.Join(home, ".synapses", "daemon.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No daemon log found. Has the daemon started yet?")
			return nil
		}
		return fmt.Errorf("read log: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	start := 0
	if len(lines) > *n {
		start = len(lines) - *n
	}
	for _, line := range lines[start:] {
		fmt.Println(line)
	}
	return nil
}

// cmdStatus loads the cached graph (without re-parsing) and prints statistics.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Path to the repository root")
	if err := fs.Parse(args); err != nil {
		return err
	}

	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	dbPath, err := store.DefaultPath(absPath)
	if err != nil {
		return err
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	savedAt, err := st.SavedAt()
	if err != nil {
		return err
	}
	if savedAt.IsZero() {
		fmt.Println("No index found. Run: synapses index --path", absPath)
		return nil
	}

	g, err := st.LoadGraph()
	if err != nil {
		return fmt.Errorf("load graph: %w", err)
	}
	if g == nil {
		fmt.Println("No index found. Run: synapses index --path", absPath)
		return nil
	}

	identity := g.ProjectIdentity()
	edgeCounts := g.EdgeCountsByType()

	fileCount, _ := st.CountIndexedFiles()

	// Load static + dynamic rules and run a quick violation check.
	cfg, _ := config.Load(absPath)
	staticRuleCount := 0
	dynamicRuleCount := 0
	violationCount := 0
	if cfg != nil {
		staticRuleCount = len(cfg.Rules)
		dynamicRules, _ := st.LoadDynamicRules()
		dynamicRuleCount = len(dynamicRules)
		cfg.Rules = append(cfg.Rules, dynamicRules...)
		violationCount = len(cfg.CheckViolations(g))
	}

	fmt.Printf("Index last updated: %s\n\n", savedAt.Local().Format("2006-01-02 15:04:05"))
	printSummaryTable(identity, 0, edgeCounts, fileCount, staticRuleCount, dynamicRuleCount, violationCount)

	// Tool usage: last 7 days, top 10.
	if stats, err := st.ToolUsageStats(7, 10); err == nil && len(stats) > 0 {
		fmt.Printf("\n── Top tools (last 7 days) ──────\n")
		for _, s := range stats {
			bar := ""
			for i := 0; i < int(s.CallCount/5)+1 && i < 20; i++ {
				bar += "▪"
			}
			errSuffix := ""
			if s.ErrorRate > 0 {
				errSuffix = fmt.Sprintf("  err=%.0f%%", s.ErrorRate*100)
			}
			fmt.Printf("  %-30s %4d calls  %5.0fms avg  %s%s\n",
				s.ToolName, s.CallCount, s.AvgMs, bar, errSuffix)
		}
	}
	return nil
}

// cmdDoctor runs a quick health check of all Synapses components and prints a
// formatted status table: graph index freshness, daemon status, and brain/doc-cache state.
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Path to the repository root")
	if err := fs.Parse(args); err != nil {
		return err
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

	fmt.Println("synapses doctor — Health Check")
	fmt.Println()
	fmt.Printf("%-16s%-16s%s\n", "Component", "Status", "Details")
	fmt.Printf("%-16s%-16s%s\n", "---------", "------", "-------")

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
				status := "fresh"
				if ago > 24*time.Hour {
					status = "stale"
				}
				fmt.Printf("%-16s%-16s%s\n", "Graph Index", status,
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
		fmt.Printf("%-16s%-16s%s\n", "Brain", "disabled", "(set brain.enabled:true in synapses.json to activate)")
	}

	// ── Doc Cache ────────────────────────────────────────────────────────────
	fmt.Printf("%-16s%-16s%s\n", "Doc Cache", "built-in", "version-pinned Go package docs via webcache (no sidecar needed)")

	// ── Pulse ────────────────────────────────────────────────────────────────
	if cfg.Pulse.URL != "" {
		status, detail := pingHealth(cfg.Pulse.URL + "/v1/health")
		fmt.Printf("%-16s%-16s%s\n", "Pulse", status, detail)
	} else {
		fmt.Printf("%-16s%-16s%s\n", "Pulse", "not configured", "(no pulse.url in synapses.json)")
	}

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
func loadOrBuildGraphWithStore(repoRoot string, st *store.Store, forceReindex bool, plugins []config.PluginConfig) (*graph.Graph, error) {
	// Always attempt smart reindex first: a fast filesystem mtime walk that
	// re-parses only changed files. This keeps line numbers accurate after
	// offline edits made between sessions (when the watcher was not running).
	// On repos with no changes the walk is cheap and returns immediately.
	g, err := smartReindex(repoRoot, st, plugins)
	if err == nil {
		if saveErr := st.SaveGraph(g); saveErr != nil {
			fmt.Fprintf(os.Stderr, "synapses: cache save failed: %v\n", saveErr)
		}
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
			fmt.Fprintf(os.Stderr, "synapses: cache load failed (%v), re-indexing\n", cacheErr)
		} else if cached != nil {
			savedAt, _ := st.SavedAt()
			fmt.Fprintf(os.Stderr, "synapses: loaded from cache (indexed %s)\n",
				savedAt.Local().Format("2006-01-02 15:04:05"))
			// Warm-boot: restore columnar index from snapshot blob.
			tryLoadSnapshot(cached, st)
			return cached, nil
		}
	} else {
		fmt.Fprintf(os.Stderr, "synapses: smart reindex skipped (%v), doing full reindex\n", err)
	}

	// No cache or smart reindex skipped: full parse from scratch.
	fmt.Fprintf(os.Stderr, "synapses: indexing %s...\n", repoRoot)
	start := time.Now()
	g, err = buildGraph(repoRoot, st, plugins)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "synapses: indexed %d nodes in %s\n",
		g.NodeCount(), time.Since(start).Round(time.Millisecond))

	if err := st.SaveGraph(g); err != nil {
		fmt.Fprintf(os.Stderr, "synapses: cache save failed: %v\n", err)
	}

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
		fmt.Fprintf(os.Stderr, "synapses: snapshot load failed (%v), will rebuild\n", err)
		return
	}
	g.SetIndex(idx)
	fmt.Fprintf(os.Stderr, "synapses: warm-boot: columnar index restored from snapshot\n")
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
	return loadOrBuildGraphWithStore(repoRoot, st, forceReindex, nil)
}

// analyzeDataFlowIfEnabled tags source/sink nodes and creates DATA_FLOWS summary
// edges between reachable (source, sink) pairs via existing CALLS edges.
// Always runs — built-in heuristics detect common patterns even without config.
func analyzeDataFlowIfEnabled(g *graph.Graph, cfg *config.Config) {
	if n := dataflow.AnnotateGraph(g, cfg); n > 0 {
		fmt.Fprintf(os.Stderr, "synapses: %d DATA_FLOWS edges created\n", n)
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
		fmt.Fprintf(os.Stderr, "synapses: coverage profile loaded: %s\n", cfg.CoverageProfile)
	}

	if cfg.PprofProfile != "" {
		metrics.EnrichPprof(g, root, cfg.PprofProfile)
		fmt.Fprintf(os.Stderr, "synapses: pprof profile loaded: %s\n", cfg.PprofProfile)
	}
}

// applyGoTypesIfEnabled runs the type-checked CALLS resolver when
// cfg.UseGoTypes is true. Errors are logged but never fatal — the graph
// already has tree-sitter CALLS edges and remains usable.
func applyGoTypesIfEnabled(g *graph.Graph, root string, cfg *config.Config) {
	if !cfg.UseGoTypes {
		return
	}
	fmt.Fprintf(os.Stderr, "synapses: running go/types resolver (use_go_types=true)...\n")
	n, err := resolver.ResolveGoTypesCallEdges(g, root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "synapses: go/types resolver failed (falling back to tree-sitter results): %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "synapses: go/types added %d new CALLS edges\n", n)
}

// applyTSTypesIfEnabled runs the TypeScript compiler-API resolver when
// cfg.UseTSTypes is true. Requires Node.js on PATH and the "typescript" npm
// package. Errors are logged but never fatal — the graph remains usable with
// tree-sitter-only CALLS edges.
func applyTSTypesIfEnabled(g *graph.Graph, root string, cfg *config.Config) {
	if !cfg.UseTSTypes {
		return
	}
	fmt.Fprintf(os.Stderr, "synapses: running TypeScript type resolver (use_ts_types=true)...\n")
	n, err := resolver.ResolveTSTypesCallEdges(g, root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "synapses: TS type resolver failed (falling back to tree-sitter results): %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "synapses: ts/types added %d new CALLS edges\n", n)
}

// buildGraph parses the repo at root into a new graph.
// If st is non-nil the parsed file mtimes are saved for future incremental reindexes.
// plugins registers any external parser plugins before the walk begins.
func buildGraph(root string, st *store.Store, plugins []config.PluginConfig) (*graph.Graph, error) {
	repoID := filepath.Base(root)
	g := graph.New(repoID)
	g.SetRoot(root)
	w := parser.NewWalker()
	for _, p := range plugins {
		w.RegisterPlugin(p.Extensions, p.Command)
	}
	mtimes, err := w.WalkDir(g, root)
	if err != nil {
		return nil, fmt.Errorf("parse repo: %w", err)
	}
	// Persist call sites BEFORE draining them so they can be reloaded and
	// re-resolved after MergeFrom for cross-project CALLS edge resolution.
	if st != nil {
		if saveErr := st.SaveCallSites(g.PeekCallSites()); saveErr != nil {
			fmt.Fprintf(os.Stderr, "synapses: save call sites: %v\n", saveErr)
		}
	}

	n := resolver.ResolveCallEdges(g)
	fmt.Fprintf(os.Stderr, "synapses: resolved %d CALLS edges\n", n)
	if ni := resolver.ResolveImplementsEdges(g); ni > 0 {
		fmt.Fprintf(os.Stderr, "synapses: resolved %d IMPLEMENTS edges\n", ni)
	}
	// R31: resolve documentation → code entity links (EXPLAINS/DOCUMENTED_BY).
	if nd := resolver.ResolveDocEdges(g); nd > 0 {
		fmt.Fprintf(os.Stderr, "synapses: resolved %d EXPLAINS edges\n", nd)
	}

	if st != nil && len(mtimes) > 0 {
		if saveErr := st.SaveFileMtimes(mtimes); saveErr != nil {
			fmt.Fprintf(os.Stderr, "synapses: save file mtimes: %v\n", saveErr)
		}
	}
	return g, nil
}

// smartReindex loads the cached graph from st, re-parses only changed files,
// and returns the updated graph. Used when --reindex is requested and a valid
// cache exists, avoiding a full re-parse of unchanged files.
// plugins registers any external parser plugins before the incremental walk.
func smartReindex(repoRoot string, st *store.Store, plugins []config.PluginConfig) (*graph.Graph, error) {
	g, err := st.LoadGraph()
	if err != nil || g == nil {
		return nil, fmt.Errorf("load cached graph: %w", err)
	}

	known, err := st.LoadFileMtimes()
	if err != nil {
		return nil, fmt.Errorf("load file mtimes: %w", err)
	}
	if len(known) == 0 {
		return nil, fmt.Errorf("no stored file mtimes — falling back to full reindex")
	}

	w := parser.NewWalker()
	for _, p := range plugins {
		w.RegisterPlugin(p.Extensions, p.Command)
	}
	fresh, changed, removed, err := w.IncrementalReindex(g, repoRoot, known)
	if err != nil {
		return nil, fmt.Errorf("incremental reindex: %w", err)
	}

	unchanged := len(fresh) - changed
	fmt.Fprintf(os.Stderr, "synapses: smart reindex: %d changed, %d unchanged, %d removed\n",
		changed, unchanged, removed)

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
		fmt.Fprintf(os.Stderr, "synapses: resolved %d CALLS edges\n", n)
		if ni := resolver.ResolveImplementsEdges(g); ni > 0 {
			fmt.Fprintf(os.Stderr, "synapses: resolved %d IMPLEMENTS edges\n", ni)
		}
		// R31: re-resolve doc edges after incremental reparse.
		if nd := resolver.ResolveDocEdges(g); nd > 0 {
			fmt.Fprintf(os.Stderr, "synapses: resolved %d EXPLAINS edges\n", nd)
		}
	}

	if saveErr := st.SaveFileMtimes(fresh); saveErr != nil {
		fmt.Fprintf(os.Stderr, "synapses: save file mtimes: %v\n", saveErr)
	}
	return g, nil
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
	fmt.Fprintf(os.Stderr, "synapses: merged linked project %s (%d nodes)\n",
		filepath.Base(absLinked), linked.NodeCount())
	return nil
}

// cmdReset deletes the cached index for a specific project (-path) or all
// projects (-all). The source files are never touched.
func cmdReset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Repository root whose index should be removed")
	all := fs.Bool("all", false, "Remove ALL project indexes (ignores -path)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *all {
		// Show which projects will be removed before wiping.
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

	absPath, err := filepath.Abs(*repoPath)
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
	fmt.Printf("Index removed for %s\n  (was at %s)\n", absPath, dbPath)
	return nil
}

// agentMemoryTables are the tables that hold agent-created data — plans, tasks,
// episodic memory, session state, annotations, and inter-agent coordination.
// Clearing these removes all AI-generated memory while leaving the code graph intact.
var agentMemoryTables = []string{
	"plans", "tasks", "session_state",
	"episodes", "episodes_fts",
	"memories", "memories_fts",
	"annotations", "quality_gaps",
	"agent_messages", "work_claims",
	"agents", "agent_context", "events",
}

// cmdMemory dispatches "synapses memory <subcommand>".
func cmdMemory(args []string) error {
	if len(args) == 0 || args[0] == "help" {
		fmt.Println("Usage: synapses memory clear -all [--logs]")
		fmt.Println("  -all    Clear agent memory for all indexed projects")
		fmt.Println("  --logs  Also clear activity logs (tool_calls)")
		return nil
	}
	switch args[0] {
	case "clear":
		return cmdMemoryClear(args[1:])
	default:
		return fmt.Errorf("unknown memory subcommand %q — try 'synapses memory help'", args[0])
	}
}

// cmdMemoryClear erases agent-generated memory tables from all project databases
// while preserving the code graph (nodes, edges, file_hashes).
func cmdMemoryClear(args []string) error {
	fs := flag.NewFlagSet("memory clear", flag.ContinueOnError)
	all := fs.Bool("all", false, "Clear memory across all indexed projects")
	withLogs := fs.Bool("logs", false, "Also clear activity logs (tool_calls)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*all {
		return fmt.Errorf("specify -all to clear memory for all indexed projects")
	}

	tables := make([]string, len(agentMemoryTables))
	copy(tables, agentMemoryTables)
	if *withLogs {
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
		if err := clearTablesInDB(dbPath, tables); err != nil {
			fmt.Printf("  warning: %s: %v\n", entry.Name(), err)
		} else {
			fmt.Printf("  cleared  %s\n", entry.Name())
			cleared++
		}
	}
	if cleared == 0 {
		fmt.Println("No project databases found.")
	} else {
		fmt.Printf("\nAgent memory cleared across %d project(s).\n", cleared)
	}
	return nil
}

// clearTablesInDB opens a SQLite database and deletes all rows from the given
// tables, ignoring tables that do not exist (older DBs may lack some tables).
func clearTablesInDB(dbPath string, tables []string) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, table := range tables {
		// Use IF EXISTS equivalent: ignore errors from missing tables.
		if _, err := db.Exec("DELETE FROM " + table); err != nil {
			continue
		}
	}
	return nil
}

// cmdList scans the synapses cache directory and prints a summary row for every
// project that has been indexed, without loading any full graph.
func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON array")
	if err := fs.Parse(args); err != nil {
		return err
	}

	stats, err := store.ScanAll()
	if err != nil {
		return fmt.Errorf("scan projects: %w", err)
	}

	if *jsonOut {
		type jsonProject struct {
			Path        string `json:"path"`
			Name        string `json:"name"`
			Nodes       int    `json:"nodes"`
			Files       int    `json:"files"`
			Edges       int    `json:"edges"`
			Scale       string `json:"scale,omitempty"`
			LastIndexed string `json:"last_indexed,omitempty"`
		}
		nodeScale := func(n int) string {
			switch {
			case n < 100:
				return "micro"
			case n < 500:
				return "small"
			case n < 2000:
				return "medium"
			default:
				return "large"
			}
		}
		out := make([]jsonProject, 0, len(stats))
		for _, s := range stats {
			ts := ""
			if !s.SavedAt.IsZero() {
				ts = s.SavedAt.UTC().Format(time.RFC3339)
			}
			out = append(out, jsonProject{
				Path:        s.RepoRoot,
				Name:        s.RepoID,
				Nodes:       s.NodeCount,
				Files:       s.FileCount,
				Edges:       s.EdgeCount,
				Scale:       nodeScale(s.NodeCount),
				LastIndexed: ts,
			})
		}
		data, _ := json.Marshal(out)
		fmt.Println(string(data))
		return nil
	}

	if len(stats) == 0 {
		fmt.Println("No indexed projects found. Run: synapses index -path <dir>")
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

// cmdInit is the zero-friction onboarding command. It:
//  1. Indexes the project (uses cache if already fresh)
//  2. Writes / updates .mcp.json in the project root so Claude Code picks it
//     up automatically — without touching any existing MCP server entries
//  3. Prints the exact next step the user needs to take
func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Repository root to initialise (default: current directory)")
	forceReindex := fs.Bool("reindex", false, "Force a full re-index even if cache is fresh")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "[deprecated] 'synapses init' is superseded by the Synapses app, which\n"+
		"starts the daemon and writes IDE configs automatically. For headless use,\n"+
		"run: synapses index && synapses mcp-setup\n\n")

	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	initStart := time.Now()
	projectName := filepath.Base(absPath)
	fmt.Printf("\n  Synapses — setting up %s\n", projectName)
	fmt.Printf("  ─────────────────────────────────────\n\n")

	// ── Step 1: Detect languages ───────────────────────────────────────────────
	// GAP-9: Show what's in the project so the user knows parsing is relevant.
	fmt.Printf("  Detecting languages...\n")
	langs := detectProjectLanguages(absPath)
	if len(langs) > 0 {
		fmt.Printf("  ✓ Found: %s\n\n", strings.Join(langs, ", "))
	} else {
		fmt.Printf("  ✓ Generic parsing (no deep AST parsers matched)\n\n")
	}

	// ── Step 2: Generate synapses.json if missing ──────────────────────────────
	cfgPath := filepath.Join(absPath, "synapses.json")
	if _, statErr := os.Stat(cfgPath); os.IsNotExist(statErr) {
		fmt.Printf("  Generating synapses.json...\n")
		if err := writeOnboardSynapsesJSON(absPath); err != nil {
			fmt.Printf("  ! could not write synapses.json: %v\n\n", err)
		} else {
			fmt.Printf("  ✓ %s\n    brain+pulse+web-cache: all in-process (no sidecars needed)\n\n", cfgPath)
		}
	} else {
		fmt.Printf("  synapses.json already exists — skipping\n\n")
	}

	// ── Step 3: Index ──────────────────────────────────────────────────────────
	fmt.Printf("  Indexing...\n")
	start := time.Now()
	g, err := loadOrBuildGraph(absPath, *forceReindex)
	if err != nil {
		return err
	}
	identity := g.ProjectIdentity()
	elapsed := time.Since(start).Round(time.Millisecond)
	fmt.Printf("  ✓ %d files   %d nodes   %d edges   (%s)\n\n",
		identity.Summary.Files,
		identity.Summary.Files+identity.Summary.Functions+
			identity.Summary.Methods+identity.Summary.Structs+identity.Summary.Interfaces,
		identity.Summary.Edges,
		elapsed)

	// ── Step 4: Write .mcp.json ────────────────────────────────────────────────
	mcpFile := filepath.Join(absPath, ".mcp.json")
	if err := writeMCPConfig(mcpFile, absPath); err != nil {
		return fmt.Errorf("write .mcp.json: %w", err)
	}
	fmt.Printf("  Writing .mcp.json...\n")
	fmt.Printf("  ✓ %s\n\n", mcpFile)

	// ── Step 5: Write CLAUDE.md ────────────────────────────────────────────────
	fmt.Printf("  Writing CLAUDE.md...\n")
	if err := writeProjectCLAUDE(absPath); err != nil {
		fmt.Printf("  ! could not update CLAUDE.md: %v\n\n", err)
	} else {
		fmt.Printf("  ✓ %s\n\n", filepath.Join(absPath, ".claude", "CLAUDE.md"))
	}

	// ── Step 6: Write .claude/settings.json ────────────────────────────────────
	fmt.Printf("  Writing .claude/settings.json...\n")
	if err := writeClaudeSettings(absPath); err != nil {
		fmt.Printf("  ! could not update .claude/settings.json: %v\n\n", err)
	} else {
		fmt.Printf("  ✓ %s\n\n", filepath.Join(absPath, ".claude", "settings.json"))
	}

	// ── Step 7: Ensure sidecars running ────────────────────────────────────────
	fmt.Printf("  Starting background services...\n")
	if ensureDirs() == nil {
		daemonStart(allSidecars, false) //nolint:errcheck
	}

	// ── Summary ────────────────────────────────────────────────────────────────
	totalElapsed := time.Since(initStart).Round(time.Millisecond)
	fmt.Printf("\n  ✓ Setup complete in %s\n\n", totalElapsed)

	fmt.Printf("  Next step — reload MCP servers in your agent:\n")
	fmt.Printf("    Claude Code:  type /mcp or reopen the chat panel\n")
	fmt.Printf("    CLI:          claude mcp add --scope user synapses -- synapses start -path %s\n\n", absPath)

	fmt.Printf("  Your agent can now call session_init() to start.\n")
	fmt.Printf("  Run 'synapses onboard' for full interactive setup (install sidecars, configure brain).\n\n")
	return nil
}

// detectProjectLanguages scans the project root for known source file extensions
// and returns a sorted list of detected language names. Stops after 5000 files
// to keep init fast on large repos.
func detectProjectLanguages(root string) []string {
	extToLang := map[string]string{
		".go": "Go", ".py": "Python", ".pyi": "Python",
		".ts": "TypeScript", ".tsx": "TypeScript",
		".js": "JavaScript", ".jsx": "JavaScript", ".mjs": "JavaScript",
		".java": "Java", ".kt": "Kotlin", ".kts": "Kotlin",
		".rs": "Rust", ".c": "C", ".h": "C", ".cpp": "C++", ".cc": "C++",
		".cs": "C#", ".swift": "Swift", ".rb": "Ruby", ".php": "PHP",
		".lua": "Lua", ".ex": "Elixir", ".exs": "Elixir",
		".scala": "Scala", ".groovy": "Groovy", ".proto": "Protobuf",
	}

	seen := make(map[string]bool)
	count := 0
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") || base == "node_modules" || base == "vendor" || base == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		count++
		if count > 5000 {
			return filepath.SkipAll
		}
		ext := filepath.Ext(path)
		if lang, ok := extToLang[ext]; ok {
			seen[lang] = true
		}
		return nil
	})

	langs := make([]string, 0, len(seen))
	for lang := range seen {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	return langs
}

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
var synapsesSection = synapsesSectionStart + `## Synapses — Code Intelligence (MCP)

### Session Start
Call ` + "`session_init()`" + ` at the start of every session — returns pending tasks, project identity, scale guidance, and working state in one round-trip. Use ` + "`scope=\"quick\"`" + ` for lightweight sessions (~500 tokens instead of full response).

### Tool Selection (follow scale_guidance from session_init)

| Scale | Use Synapses for | Use Read/Grep for |
|---|---|---|
| micro/small | Structural analysis, cross-file understanding | Targeted single-file edits |
| medium/large | All code exploration — direct scan is too noisy at this scale | Writing to a specific file you already identified |

### Key Tools

| Goal | Tool |
|---|---|
| Understand a function, struct, or interface | ` + "`get_context(entity=\"Name\")`" + ` — returns compact summary by default |
| Need full callee/caller sub-tree detail | ` + "`get_context(entity=\"Name\", detail_level=\"full\")`" + ` |
| Pin to a specific file (avoids ambiguity) | ` + "`get_context(entity=\"Name\", file=\"path/suffix.go\")`" + ` |
| Find a symbol by name or substring | ` + "`find_entity(query=\"name\")`" + ` — returns compact list by default |
| Search by concept ("auth", "caching") | ` + "`search(query=\"...\", mode=\"semantic\")`" + ` |
| Find what breaks if a symbol changes | ` + "`get_impact(symbol=\"Name\")`" + ` |
| Check proposed changes against architecture rules | ` + "`validate_plan(changes=[...])`" + ` |
| Verify written files against rules | ` + "`verify_implementation(files_written=[\"...\"])`" + ` |
| Save a plan with tasks for future sessions | ` + "`create_plan(title=\"...\", tasks=[...])`" + ` |
| Resume a saved session | ` + "`get_session_state(task_id=\"...\")`" + ` |
| Not sure which tool fits | ` + "`discover_tools(query=\"what I'm trying to do\")`" + ` |

### Anti-patterns
- **NEVER** use Grep/Glob to understand code structure or find callers — use ` + "`get_context`" + ` or ` + "`get_impact`" + `
- **NEVER** skip ` + "`validate_plan()`" + ` before multi-file changes — it catches architecture violations before any code is written
- **NEVER** leave discovered bugs untracked — add them as tasks via ` + "`create_plan()`" + ` immediately

### Workflow
` + "`session_init`" + ` → explore (` + "`get_context`" + `, ` + "`find_entity`" + `) → ` + "`validate_plan`" + ` → edit files → ` + "`verify_implementation`" + `
` + synapsesSectionEnd

// writeProjectCLAUDE writes (or updates) a Synapses-managed section in
// .claude/CLAUDE.md (preferred by Claude Code). The section is delimited by
// HTML comments so it can be safely updated on subsequent connect runs without
// clobbering the rest of the file. If a root-level CLAUDE.md exists with a
// Synapses section it is migrated to .claude/CLAUDE.md and the section is
// removed from the root file.
func writeProjectCLAUDE(repoRoot string) error {
	section := synapsesSection

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
//   - PreToolUse on Glob|Grep: blocks with exit 2 — forces use of Synapses tools
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
			"WORKFLOW: session_init → prepare_context → validate_plan → edit files → verify_implementation.'"
	}
	upsertHookEntry(hooks, "SessionStart", "startup", map[string]interface{}{
		"type":    "command",
		"command": sessionStartCmd,
	})

	// ── PreToolUse: BLOCK Glob|Grep with exit 2 (not just advisory).
	// exit 2 causes Claude Code to reject the tool call and show the message.
	upsertHookEntry(hooks, "PreToolUse", "Glob|Grep", map[string]interface{}{
		"type": "command",
		"command": "echo '[Synapses] BLOCKED — this project is indexed. Use Synapses tools instead: " +
			"find_entity(query) to locate a symbol, get_context(entity) to understand it, " +
			"search(query, mode=semantic) to find by concept, get_impact(symbol) to find dependents. " +
			"Grep/Glob are only for WRITING to a specific file you have already identified.' && exit 2",
	})

	// ── PostToolUse: nudge verify_implementation after any file write/edit.
	upsertHookEntry(hooks, "PostToolUse", "Write|Edit", map[string]interface{}{
		"type": "command",
		"command": "echo '[Synapses] Files written. Now call verify_implementation(files_written=[\"<path>\"]) " +
			"to check your changes against architecture rules before continuing.'",
	})

	// ── PostToolUse: confirm after validate_plan so agent knows it is safe to edit.
	upsertHookEntry(hooks, "PostToolUse", "mcp__synapses__validate_plan", map[string]interface{}{
		"type":    "command",
		"command": "echo '[Synapses] Plan validated. Proceed with edits.'",
	})

	// ── PostToolUse: after create_plan remind agent to claim work before editing.
	upsertHookEntry(hooks, "PostToolUse", "mcp__synapses__create_plan", map[string]interface{}{
		"type":    "command",
		"command": "echo '[Synapses] Plan created. Call claim_work(agent_id=\"...\", scope=\"pkg/...\") before starting edits to prevent conflicts.'",
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

// writeVSCodeMCPConfig merges a synapses entry into .vscode/mcp.json for
// VS Code 1.99+ native MCP support (GitHub Copilot agent mode).
func writeVSCodeMCPConfig(repoRoot string) error {
	file := filepath.Join(repoRoot, ".vscode", "mcp.json")
	raw := map[string]interface{}{"servers": map[string]interface{}{}}
	if data, err := os.ReadFile(file); err == nil {
		parsed := map[string]interface{}{}
		if json.Unmarshal(data, &parsed) == nil {
			raw = parsed
		}
	}
	if _, ok := raw["servers"]; !ok {
		raw["servers"] = map[string]interface{}{}
	}
	servers, _ := raw["servers"].(map[string]interface{})
	if servers == nil {
		servers = map[string]interface{}{}
		raw["servers"] = servers
	}
	servers["synapses"] = map[string]interface{}{
		"type": "http",
		"url":  mcpURL(repoRoot),
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".vscode"), 0o755); err != nil {
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
func writeGuidanceFile(file, frontmatter string) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	section := synapsesSection
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
	agent := fs.String("agent", "", "Agent to connect: claude, cursor, windsurf, zed, vscode")
	repoPath := fs.String("path", ".", "Path to the project root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *agent == "" {
		return fmt.Errorf("--agent is required (claude, cursor, windsurf, zed, vscode)")
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
		add(rulesFile, writeGuidanceFile(rulesFile, frontmatter))

	case "windsurf":
		mcpFile := filepath.Join(absPath, ".windsurf", "mcp_config.json")
		add(mcpFile, writeHTTPMCPServerEntry(mcpFile, absPath))
		rulesFile := filepath.Join(absPath, ".windsurfrules")
		add(rulesFile, writeGuidanceFile(rulesFile, ""))

	case "zed":
		settingsFile := filepath.Join(absPath, ".zed", "settings.json")
		add(settingsFile, writeZedMCPConfig(absPath))

	case "vscode":
		mcpFile := filepath.Join(absPath, ".vscode", "mcp.json")
		add(mcpFile, writeVSCodeMCPConfig(absPath))

	case "antigravity":
		// Antigravity (https://antigravity.google) stores workspace MCP config
		// at .agent/mcp.json and agent rules at .agent/rules/
		mcpFile := filepath.Join(absPath, ".agent", "mcp.json")
		add(mcpFile, writeHTTPMCPServerEntry(mcpFile, absPath))
		rulesFile := filepath.Join(absPath, ".agent", "rules", "synapses.md")
		add(rulesFile, writeGuidanceFile(rulesFile, ""))

	default:
		return fmt.Errorf("unknown agent %q — supported: claude, cursor, windsurf, zed, vscode, antigravity", *agent)
	}

	for _, r := range results {
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "  warning: %s: %v\n", r.path, r.err)
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

// cmdQuery loads the cached graph for a project and outputs a single entity's
// context as JSON — name, type, file, line, callers, callees, and metadata.
// This is the backend used by the VS Code extension hover provider.
func cmdQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Repository root")
	entityName := fs.String("entity", "", "Entity name to look up (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *entityName == "" {
		return fmt.Errorf("query: -entity is required")
	}

	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	dbPath, err := store.DefaultPath(absPath)
	if err != nil {
		return err
	}
	// Open read-only so the query can run concurrently with a running MCP server.
	st, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	g, err := st.LoadGraph()
	if err != nil {
		return fmt.Errorf("load graph: %w", err)
	}
	if g == nil {
		return fmt.Errorf("no index found — run 'synapses index --path %s' first", absPath)
	}

	// Find the best-matching node for the given name.
	// Resolution order:
	//   1. Exact name match (e.g. "Store", "cmdQuery")
	//   2. Suffix match — bare word matches "Type.Method" suffix (e.g. "AddEdge" → "Graph.AddEdge")
	//      This lets the VS Code hover provider (word-under-cursor) find methods without
	//      the caller needing the fully-qualified "TypeName.Method" form.
	// Within each tier: fn/method beats struct/other.
	suffix := "." + *entityName
	pickBetter := func(cur, candidate *graph.Node) *graph.Node {
		if cur == nil {
			return candidate
		}
		isFn := func(n *graph.Node) bool {
			return n.Type == graph.NodeFunction || n.Type == graph.NodeMethod
		}
		if isFn(candidate) && !isFn(cur) {
			return candidate
		}
		return cur
	}
	var best, suffixBest *graph.Node
	for _, n := range g.AllNodes() {
		if n.Name == *entityName {
			best = pickBetter(best, n)
		} else if strings.HasSuffix(n.Name, suffix) {
			suffixBest = pickBetter(suffixBest, n)
		}
	}
	if best == nil {
		best = suffixBest
	}
	if best == nil {
		return fmt.Errorf("entity %q not found in index", *entityName)
	}

	type edgeRef struct {
		Name string `json:"name"`
		File string `json:"file"`
		Line int    `json:"line"`
		Type string `json:"type"`
	}
	type queryResult struct {
		Name     string            `json:"name"`
		Type     string            `json:"type"`
		File     string            `json:"file"`
		Line     int               `json:"line"`
		Doc      string            `json:"doc,omitempty"`
		Sig      string            `json:"signature,omitempty"`
		Callers  []edgeRef         `json:"callers"`
		Callees  []edgeRef         `json:"callees"`
		Metadata map[string]string `json:"metadata,omitempty"`
	}

	result := queryResult{
		Name:    best.Name,
		Type:    string(best.Type),
		File:    best.File,
		Line:    best.Line,
		Doc:     best.Metadata["doc"],
		Sig:     best.Metadata["signature"],
		Callers: []edgeRef{},
		Callees: []edgeRef{},
	}
	// Copy metadata without doc/signature (already promoted to top-level fields).
	if len(best.Metadata) > 0 {
		result.Metadata = make(map[string]string)
		for k, v := range best.Metadata {
			if k != "doc" && k != "signature" {
				result.Metadata[k] = v
			}
		}
	}

	// Collect callers (nodes that call best).
	for _, e := range g.InEdges(best.ID) {
		if e.Type != graph.EdgeCalls {
			continue
		}
		if caller := g.GetNode(e.From); caller != nil {
			result.Callers = append(result.Callers, edgeRef{
				Name: caller.Name,
				File: caller.File,
				Line: caller.Line,
				Type: string(caller.Type),
			})
		}
	}

	// Collect callees (nodes that best calls).
	for _, e := range g.OutEdges(best.ID) {
		if e.Type != graph.EdgeCalls {
			continue
		}
		if callee := g.GetNode(e.To); callee != nil {
			result.Callees = append(result.Callees, edgeRef{
				Name: callee.Name,
				File: callee.File,
				Line: callee.Line,
				Type: string(callee.Type),
			})
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

// cmdBrief outputs a concise markdown briefing (~150-300 tokens) for use as
// a Claude Code startup hook. It surfaces active agents, priority tasks,
// cross-project alerts, and recent failures in a single glance.
//
// Usage:
//
//	synapses brief --path <repo> [--agent-id <id>]
func cmdBrief(args []string) error {
	fs := flag.NewFlagSet("brief", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Repository root")
	agentID := fs.String("agent-id", "", "Agent identifier (for filtering tasks)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	dbPath, err := store.DefaultPath(absPath)
	if err != nil {
		return err
	}
	st, err := store.OpenReadOnly(dbPath)
	if err != nil {
		// No index yet — output a minimal brief rather than failing.
		fmt.Println("## Synapses Brief\n- No index found. Run `synapses init` first.")
		return nil
	}
	defer st.Close()

	var b strings.Builder
	b.WriteString("## Synapses Brief\n")

	// 1. Active agents
	if agents, err := st.GetAgents(); err == nil && len(agents) > 0 {
		names := make([]string, 0, len(agents))
		for _, a := range agents {
			names = append(names, a.ID)
		}
		if len(names) > 5 {
			names = names[:5]
		}
		b.WriteString(fmt.Sprintf("- **Active agents** (%d): %s\n", len(agents), strings.Join(names, ", ")))
	}

	// 2. Priority tasks (top 3, ordered by priority)
	if tasks, err := st.GetPendingTasks("", *agentID); err == nil && len(tasks) > 0 {
		limit := 3
		if len(tasks) < limit {
			limit = len(tasks)
		}
		for i := 0; i < limit; i++ {
			t := tasks[i]
			marker := ""
			if t.Status == "in_progress" {
				marker = " (in_progress)"
			}
			b.WriteString(fmt.Sprintf("- **[%s] %s**%s\n", t.Priority, t.Title, marker))
		}
		if len(tasks) > limit {
			b.WriteString(fmt.Sprintf("- ... and %d more task(s)\n", len(tasks)-limit))
		}
	} else {
		b.WriteString("- No pending tasks\n")
	}

	// 3. Cross-project alerts (unread)
	if msgs, _, err := st.GetMessages("", 0, "cross_project_impact", true, 5); err == nil && len(msgs) > 0 {
		b.WriteString(fmt.Sprintf("- **%d cross-project alert(s)**: recent changes may have broken linked dependencies\n", len(msgs)))
	}

	// 4. Recent failure episode (if any)
	repoID := filepath.Base(absPath)
	if g, err := st.LoadGraph(); err == nil && g != nil {
		repoID = g.RepoID()
	}
	if failures, err := st.GetEpisodes(repoID, "", "failure", nil, 1, 0); err == nil && len(failures) > 0 {
		f := failures[0]
		b.WriteString(fmt.Sprintf("- **Recent failure**: %s\n", f.Decision))
	}

	fmt.Print(b.String())
	return nil
}

func printUsage() {
	fmt.Printf(`Synapses %s — graph-based context manager for AI coding agents

USAGE:
  synapses <command> [flags]

GETTING STARTED:
  Open the Synapses app — it starts the daemon and writes IDE configs automatically.
  For headless/CI environments: cd /your/project && synapses index && synapses mcp-setup

DAEMON COMMANDS:
  start     -path <dir>   Ensure daemon is running and register project (proxy mode)
  stop                    Stop the singleton daemon
  projects                List projects registered with the running daemon
  logs      [-n N]        Tail the daemon log (~/.synapses/daemon.log)
  status    -path <dir>   Show index statistics and daemon health
  doctor    -path <dir>   Full health check (index, brain, scout)

INDEX COMMANDS:
  index     -path <dir>   Index repo and save to cache
  list                    List all indexed projects
  reset     -path <dir>   Remove the cached index for a project
  reset     -all          Remove ALL cached indexes

SETUP COMMANDS:
  brain setup               Pull qwen3.5:2b + register all 5 AI tier identities
  mcp-setup -agent <name>   Write MCP config for the specified agent
  init      -path <dir>     [deprecated] Index + write MCP config (use Synapses app instead)

OTHER:
  query   -path <dir> -entity <n>  Dump entity context as JSON
  brief   -path <dir>              Concise session brief
  export  -path <dir>              Export graph as DOT/JSON/GraphML
  version                          Print version
  help                             Print this message

Supported agents for mcp-setup: cursor, gemini, zed, windsurf, claude, all
`, version)
}

// cmdMCPSetup writes per-agent MCP configuration files so that AI coding
// agents other than Claude Code can discover and use the Synapses MCP server.
//
// Usage:
//
//	synapses mcp-setup --agent cursor|gemini|zed|windsurf|claude|all [--path .]
//
// All per-project config files are written relative to --path (default ".").
// Windsurf writes to the global user config (~/.codeium/windsurf/mcp_config.json).
func cmdMCPSetup(args []string) error {
	fs := flag.NewFlagSet("mcp-setup", flag.ContinueOnError)
	agent := fs.String("agent", "all", "Agent to configure: cursor|gemini|zed|windsurf|claude|all")
	repoPath := fs.String("path", ".", "Project root to write per-project configs into")
	transport := fs.String("transport", "stdio", "MCP transport: stdio|http")
	if err := fs.Parse(args); err != nil {
		return err
	}

	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	useHTTP := strings.ToLower(*transport) == "http"

	// mergeHTTPConfig writes an HTTP MCP entry (for IDEs that support HTTP transport).
	// URL format: http://127.0.0.1:11435/mcp?project=<absPath>
	mergeHTTPConfig := func(filePath string) error {
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return err
		}
		existing := make(map[string]interface{})
		if data, err := os.ReadFile(filePath); err == nil {
			_ = json.Unmarshal(data, &existing)
		}
		servers, _ := existing["mcpServers"].(map[string]interface{})
		if servers == nil {
			servers = make(map[string]interface{})
		}
		servers["synapses"] = map[string]interface{}{
			"type": "http",
			"url":  "http://" + DaemonHTTPAddr + "/mcp?project=" + absPath,
		}
		existing["mcpServers"] = servers
		out, err := json.MarshalIndent(existing, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filePath, append(out, '\n'), 0o644)
	}

	// The MCP server entry common to all stdio-based agents.
	type stdioServer struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Type    string   `json:"type"`
	}

	serverEntry := stdioServer{
		Command: "synapses",
		Args:    []string{"start", "--path", "."},
		Type:    "stdio",
	}

	// mergeStdioConfig reads an existing JSON file (if any), sets/updates the
	// "synapses" key inside the top-level "mcpServers" object, and writes back.
	mergeStdioConfig := func(filePath string) error {
		if useHTTP {
			return mergeHTTPConfig(filePath)
		}
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return err
		}
		existing := make(map[string]interface{})
		if data, err := os.ReadFile(filePath); err == nil {
			_ = json.Unmarshal(data, &existing)
		}
		servers, _ := existing["mcpServers"].(map[string]interface{})
		if servers == nil {
			servers = make(map[string]interface{})
		}
		servers["synapses"] = serverEntry
		existing["mcpServers"] = servers
		out, err := json.MarshalIndent(existing, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filePath, append(out, '\n'), 0o644)
	}

	// mergeZedConfig handles Zed's different "context_servers" key shape.
	mergeZedConfig := func(filePath string) error {
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return err
		}
		existing := make(map[string]interface{})
		if data, err := os.ReadFile(filePath); err == nil {
			_ = json.Unmarshal(data, &existing)
		}
		ctxServers, _ := existing["context_servers"].(map[string]interface{})
		if ctxServers == nil {
			ctxServers = make(map[string]interface{})
		}
		ctxServers["synapses"] = map[string]interface{}{
			"command": map[string]interface{}{
				"path": "synapses",
				"args": []string{"start", "--path", "."},
			},
		}
		existing["context_servers"] = ctxServers
		out, err := json.MarshalIndent(existing, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filePath, append(out, '\n'), 0o644)
	}

	type agentSetup struct {
		name    string
		setup   func() error
		cfgPath string // for display only
	}

	homeDir, _ := os.UserHomeDir()

	agents := []agentSetup{
		{
			name:    "cursor",
			cfgPath: filepath.Join(absPath, ".cursor", "mcp.json"),
			setup: func() error {
				return mergeStdioConfig(filepath.Join(absPath, ".cursor", "mcp.json"))
			},
		},
		{
			name:    "gemini",
			cfgPath: filepath.Join(absPath, ".gemini", "settings.json"),
			setup: func() error {
				return mergeStdioConfig(filepath.Join(absPath, ".gemini", "settings.json"))
			},
		},
		{
			name:    "zed",
			cfgPath: filepath.Join(absPath, ".zed", "settings.json"),
			setup: func() error {
				return mergeZedConfig(filepath.Join(absPath, ".zed", "settings.json"))
			},
		},
		{
			name:    "windsurf",
			cfgPath: filepath.Join(homeDir, ".codeium", "windsurf", "mcp_config.json"),
			setup: func() error {
				return mergeStdioConfig(filepath.Join(homeDir, ".codeium", "windsurf", "mcp_config.json"))
			},
		},
		{
			name:    "claude",
			cfgPath: "via `claude mcp add` (Claude Code CLI)",
			setup: func() error {
				if useHTTP {
					// HTTP transport: write to ~/.claude/mcp.json directly.
					homeDir2, _ := os.UserHomeDir()
					return mergeHTTPConfig(filepath.Join(homeDir2, ".claude", "mcp.json"))
				}
				cmd := exec.Command("claude", "mcp", "add", "synapses", "--", "synapses", "start", "--path", ".")
				cmd.Dir = absPath
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					// Not fatal — Claude Code may not be installed.
					fmt.Fprintf(os.Stderr, "  ! claude CLI not available (%v). Manual setup: claude mcp add synapses -- synapses start --path .\n", err)
				}
				return nil
			},
		},
	}

	target := strings.ToLower(*agent)
	wrote := 0
	for _, a := range agents {
		if target != "all" && target != a.name {
			continue
		}
		if err := a.setup(); err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ %-12s %v\n", a.name, err)
		} else {
			fmt.Printf("  \033[32m✓\033[0m %-12s %s\n", a.name, a.cfgPath)
			wrote++
		}
	}

	if wrote == 0 && target != "all" {
		return fmt.Errorf("unknown agent %q — choose one of: cursor, gemini, zed, windsurf, claude, all", *agent)
	}

	fmt.Printf("\n  Done. Restart your AI agent to pick up the new MCP server.\n")
	fmt.Printf("  Note: make sure 'synapses' is on PATH before starting your agent.\n")
	return nil
}

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
	fmt.Fprintf(os.Stderr, "synapses: eager brain write-back complete (%d/%d top entities enriched)\n", written, len(nodes))
}

// bulkIngestToBrain sends all code nodes to the brain sidecar for prose summary generation.
// With qwen3.5:0.8b as the ingest model (~3s per node on CPU), a 500-node codebase
// completes in ~3min at 8× concurrency — runs in background, does not block startup.
// Summaries are stored in brain.sqlite and surfaced in get_context responses.
// Sort order: high-fanin nodes first so the most-used code gets summaries soonest.
// embedAllNodes generates vector embeddings for every graph node that does not
// yet have one, storing results in the node_embeddings table. Runs in a
// background goroutine after startup so the MCP server is never delayed.
// Rate-limited to ~10 req/s to avoid saturating a local Ollama instance.
// Fail-silent: any error per-node is logged to stderr and skipped.
func embedAllNodes(ctx context.Context, ec *embed.Client, _ *graph.Graph, st *store.Store) {
	if ec == nil || st == nil {
		return
	}

	nodeIDs, err := st.GetNodesWithoutEmbeddings(0) // 0 = no limit (includes stale)
	if err != nil || len(nodeIDs) == 0 {
		return
	}

	fmt.Fprintf(os.Stderr, "synapses: embedding %d nodes (model: %s) …\n", len(nodeIDs), ec.Model())

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
		batchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		vecs, batchErr := ec.EmbedBatch(batchCtx, texts)
		cancel()

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
					fmt.Fprintf(os.Stderr, "synapses: embed store error for %s: %v\n", nodeID, err)
				}
				done++
			}
			continue
		}

		// Store all batch results.
		for j, nodeID := range validIDs {
			if err := st.UpsertEmbedding(nodeID, ec.Model(), vecs[j]); err != nil {
				fmt.Fprintf(os.Stderr, "synapses: embed store error for %s: %v\n", nodeID, err)
			}
			done++
		}
	}

	fmt.Fprintf(os.Stderr, "synapses: embedding complete (%d/%d nodes indexed)\n", done, len(nodeIDs))
}

// pathProjectID returns a stable 8-hex-char project identifier derived from the project root path.
func pathProjectID(absPath string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(absPath))
	return fmt.Sprintf("%x", h.Sum32())
}

func bulkIngestToBrain(bc *brain.Client, g *graph.Graph, projectID string) {
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
	for _, n := range nodes {
		wg.Add(1)
		sem <- struct{}{}
		go func(node *graph.Node) {
			defer wg.Done()
			defer func() { <-sem }()
			bc.Ingest(context.Background(), brain.IngestRequest{
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
	fmt.Fprintf(os.Stderr, "synapses: ingested %d nodes to brain (full coverage)\n", len(nodes))
}

// fetchAndWriteBackSummaries waits for the brain to process ingested nodes,
// then fetches each node's summary and writes it back as a graph annotation.
// This surfaces brain summaries in get_context.annotations, find_entity, etc.
// Runs after bulkIngestToBrain completes; all errors are silently discarded.
func fetchAndWriteBackSummaries(bc *brain.Client, g *graph.Graph, st *store.Store) {
	if st == nil {
		return
	}
	// Give the brain time to process the ingest queue before fetching summaries.
	time.Sleep(10 * time.Second)

	nodes := g.AllNodes()
	sem := make(chan struct{}, 4) // 4 concurrent fetches (brain's LLM is mostly single-threaded)
	var wg sync.WaitGroup
	written := 0
	var mu sync.Mutex
	for _, n := range nodes {
		if string(n.Type) == "package" || string(n.Type) == "file" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(node *graph.Node) {
			defer wg.Done()
			defer func() { <-sem }()
			summary := bc.GetSummary(context.Background(), string(node.ID))
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
	fmt.Fprintf(os.Stderr, "synapses: brain write-back complete (%d summaries stored)\n", written)
}

// cmdExport loads the cached graph and writes it to stdout as DOT, Mermaid, or
// GraphML. With --entity it exports an ego-subgraph; without it exports the
// full graph (file and package hub-nodes excluded for cleaner output).
func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Repository root")
	entityName := fs.String("entity", "", "Root entity for ego-subgraph (omit for full graph)")
	format := fs.String("format", "dot", "Output format: dot | mermaid | graphml")
	depth := fs.Int("depth", 2, "BFS depth for ego-subgraph")
	includeMeta := fs.Bool("meta", false, "Include signature metadata in node labels")
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch *format {
	case "dot", "mermaid", "graphml":
	default:
		return fmt.Errorf("export: --format must be dot, mermaid, or graphml")
	}

	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	dbPath, err := store.DefaultPath(absPath)
	if err != nil {
		return err
	}
	st, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	g, err := st.LoadGraph()
	if err != nil {
		return fmt.Errorf("load graph: %w", err)
	}
	if g == nil {
		return fmt.Errorf("no index found — run: synapses index --path %s", absPath)
	}

	var nodes []*graph.Node
	var edges []*graph.Edge

	if *entityName != "" {
		candidates := g.FindByName(*entityName)
		if len(candidates) == 0 {
			candidates = g.FindByPattern(*entityName)
		}
		if len(candidates) == 0 {
			return fmt.Errorf("entity %q not found", *entityName)
		}
		root := candidates[0]
		for _, c := range candidates {
			if c.Type == graph.NodeFunction || c.Type == graph.NodeMethod {
				root = c
				break
			}
		}
		cfg := graph.DefaultCarveConfig()
		cfg.MaxDepth = *depth
		cfg.TokenBudget = 500000
		cfg.MinRelevance = 0
		sg, err := g.CarveEgoGraph(root.ID, cfg)
		if err != nil {
			return fmt.Errorf("carve: %w", err)
		}
		seen := make(map[graph.NodeID]bool, len(sg.Nodes))
		for _, cn := range sg.Nodes {
			if !seen[cn.Node.ID] {
				nodes = append(nodes, cn.Node)
				seen[cn.Node.ID] = true
			}
		}
		edges = sg.Edges
	} else {
		all := g.AllNodes()
		nodes = make([]*graph.Node, 0, len(all))
		for _, n := range all {
			if n.Type != graph.NodeFile && n.Type != graph.NodePackage {
				nodes = append(nodes, n)
			}
		}
		edges = g.AllEdges()
	}

	var output string
	switch *format {
	case "dot":
		output = graph.ExportDOT(nodes, edges, g.Root(), *includeMeta)
	case "mermaid":
		output = graph.ExportMermaid(nodes, edges, g.Root(), *includeMeta)
	case "graphml":
		output = graph.ExportGraphML(nodes, edges, g.Root())
	}

	fmt.Print(output)
	return nil
}

// cmdSetup configures a project for use with synapses (and optionally the
// brain sidecar). It:
//  1. Checks whether `brain` is installed; if not, prints install instructions.
//  2. Runs `brain setup` to configure Ollama + pull the model.
//  3. Writes (or updates) synapses.json in the project root with the brain URL.
//  4. Prints the `claude mcp add` command to wire everything into Claude Code.
func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Project root (default: current directory)")
	skipBrain := fs.Bool("core", false, "Skip brain setup (Tier 1 only)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	fmt.Println()
	fmt.Println("  Synapses setup")
	fmt.Println("  ──────────────────────────────────────")

	// ── Step 1: brain ────────────────────────────────────────────────────────
	brainPath, brainFound := "", false
	if !*skipBrain {
		if p, err := exec.LookPath("brain"); err == nil {
			brainPath = p
			brainFound = true
			fmt.Printf("  \033[32m✓\033[0m brain found  (%s)\n", brainPath)
		} else {
			fmt.Println("  \033[33m!\033[0m brain not found — skipping AI enrichment (Tier 1 only)")
			fmt.Println()
			fmt.Println("    To install the brain sidecar later:")
			fmt.Println("      curl -fsSL https://raw.githubusercontent.com/synapses/synapses/main/install.sh | sh -s -- --full")
			fmt.Println("    or:  go install github.com/SynapsesOS/synapses-intelligence/cmd/brain@latest")
			fmt.Println("    Then re-run:  synapses setup --path", absPath)
		}
	}

	// ── Step 2: brain setup ──────────────────────────────────────────────────
	if brainFound {
		fmt.Println()
		fmt.Println("  \033[1mRunning brain setup...\033[0m")
		fmt.Println()
		cmd := exec.Command(brainPath, "setup")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Println()
			fmt.Println("  \033[33m!\033[0m brain setup failed (see above).")
			fmt.Println("    Fix the issue, then run:  brain setup")
			fmt.Println("    Continuing with Tier 1 config...")
			brainFound = false
		}
	}

	// ── Step 3: write / update synapses.json ─────────────────────────────────
	cfgFile := filepath.Join(absPath, "synapses.json")
	existingJSON := map[string]interface{}{}

	if data, err := os.ReadFile(cfgFile); err == nil {
		// Preserve existing keys; we only add/update the brain block.
		if jsonErr := json.Unmarshal(data, &existingJSON); jsonErr != nil {
			return fmt.Errorf("parse existing synapses.json: %w", jsonErr)
		}
	}

	if brainFound {
		existingJSON["brain"] = map[string]interface{}{
			"url":        "http://localhost:11435",
			"enable_llm": true,
		}
	}

	out, err := json.MarshalIndent(existingJSON, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal synapses.json: %w", err)
	}
	if err := os.WriteFile(cfgFile, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("write synapses.json: %w", err)
	}
	fmt.Printf("  \033[32m✓\033[0m synapses.json written  (%s)\n", cfgFile)

	// ── Step 4: final instructions ───────────────────────────────────────────
	fmt.Println()
	fmt.Println("  ──────────────────────────────────────")
	fmt.Println("  \033[1mDone. Final steps:\033[0m")
	fmt.Println()
	if brainFound {
		fmt.Println("  1. Start the brain (keep it running):")
		fmt.Println("       brain serve")
		fmt.Println()
		fmt.Println("  2. Wire synapses into Claude Code:")
		fmt.Printf("       claude mcp add synapses -- synapses start --path %s\n", absPath)
	} else {
		fmt.Println("  Wire synapses into Claude Code:")
		fmt.Printf("    claude mcp add synapses -- synapses start --path %s\n", absPath)
	}
	fmt.Println()
	return nil
}
