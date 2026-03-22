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

	"github.com/SynapsesOS/synapses/internal/benchmark"
	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/contextfile"
	"github.com/SynapsesOS/synapses/internal/dataflow"
	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
	"github.com/SynapsesOS/synapses/internal/metrics"
	"github.com/SynapsesOS/synapses/internal/skills"
	"github.com/SynapsesOS/synapses/internal/parser"
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
		// Show update hint if a cached check found a newer version.
		if state := getUpdateState(); state != nil && state.UpdateAvailable {
			fmt.Printf("Update available: %s → %s (run 'synapses update')\n",
				state.CurrentVersion, state.LatestVersion)
		}
		return
	}

	if err := run(os.Args[1:]); err != nil {
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
		// Replaced by "synapses init". Print redirect.
		fmt.Println("'synapses setup' has been replaced by 'synapses init'.")
		fmt.Println("Run: synapses init --path <dir>")
		return nil
	case "mcp-setup":
		// Replaced by "synapses init". Print redirect.
		fmt.Println("'synapses mcp-setup' has been replaced by 'synapses init'.")
		fmt.Println("Run: synapses init --path <dir>")
		return nil
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
		// Replaced by "synapses init". Print redirect.
		fmt.Println("'synapses onboard' has been replaced by 'synapses init'.")
		fmt.Println("Run: synapses init --path <dir>")
		return nil
	case "connect":
		return cmdConnect(args[1:])
	case "memory":
		return cmdMemory(args[1:])
	case "allow-plugin":
		return cmdAllowPlugin(args[1:])
	case "approve":
		return cmdApprove(args[1:])
	case "benchmark":
		return cmdBenchmark(args[1:])
	case "update":
		return cmdUpdate(args[1:])
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
		logutil.Info("synapses: using config from %s\n", cfgDir)
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

	// Prune stale operational data at startup and then daily.
	// Prevents unbounded growth of tool_calls, events, agent_messages, and
	// episodes tables during long stdio process uptime (hours/days).
	go func() {
		st.PruneStaleData(30)
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				logutil.Info("synapses: daily prune running (30-day retention)\n")
				st.PruneStaleData(30)
			case <-appCtx.Done():
				return
			}
		}
	}()

	// Plugin security: per-machine opt-in for external parser commands.
	var pluginCheck *parser.PluginChecker
	if len(cfg.Plugins) > 0 {
		sHome, homeErr := synapsesHome()
		if homeErr != nil {
			logutil.Warn("cannot determine synapses home: %v (plugins disabled)\n", homeErr)
			cfg.Plugins = nil // fail-closed: cannot verify plugins → disable them
		} else {
			pluginCheck = parser.NewPluginChecker(sHome)
		}
	}

	g, err := loadOrBuildGraphWithStore(absPath, st, *forceReindex, cfg.Plugins, pluginCheck, nil, "")
	if err != nil {
		return err
	}

	// Federation: merge linked project graphs (monorepo support).
	for _, linkedPath := range cfg.Linked {
		if mergeErr := mergeLinkedProject(g, linkedPath); mergeErr != nil {
			logutil.Warn("synapses: skipping linked project %s: %v\n", linkedPath, mergeErr)
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
				logutil.Info("synapses: resolved %d cross-project CALLS edges\n", n)
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
			tombstoneLimit = 0.15
		)
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for {
			select {
			case <-appCtx.Done():
				return
			case <-ticker.C:
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
					logutil.Info("synapses: idle defrag complete (tombstone ratio was %.0f%%)\n",
						idx.TombstoneRatio()*100)
				}
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
			logutil.Info("synapses: loaded %d activation-context prompts\n", len(allPrompts))
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
		logutil.Info("synapses: loaded %d skill recipes\n", len(allRecipes))
	}

	// Federation resolver: cross-project drift detection + dependency tracking.
	var fedResolver *federation.Resolver
	if len(cfg.Federation) > 0 {
		fedResolver = federation.NewResolver(cfg.Federation, cfgDir)
		srv.SetFederationResolver(fedResolver)
		defer fedResolver.Close()
	}

	// Brain — now in-process; no external sidecar or port required.
	brainCli := brain.NewInProcess(cfg.Brain.ToBrainConfig())
	if cfg.Brain.Enabled {
		if model, _ := brainCli.HealthCheck(context.Background()); model != "" {
			logutil.Info("synapses: brain enabled in-process (%s)\n", model)
		} else {
			logutil.Info("synapses: brain enabled in-process (Ollama not yet reachable — will retry on use)\n")
		}
		srv.SetBrainClient(brainCli)
		go func() {
			bulkIngestToBrain(appCtx, brainCli, g, pathProjectID(absPath))
			fetchAndWriteBackSummaries(appCtx, brainCli, g, st)
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
	var sharedPulse *pulse.Client // P2-6: shared with embedAllMemories
	if cfg.Pulse.URL != "" {
		sharedPulse = pulse.NewClient(cfg.Pulse.URL, cfg.Pulse.TimeoutSec)
		srv.SetPulseClient(sharedPulse)
		logutil.Info("synapses: pulse analytics enabled at %s\n", cfg.Pulse.URL)
	}

	// Optional: vector embedding for semantic search (node embeddings).
	// Priority: (1) brain /v1/embed when brain is connected and embedding is available,
	//           (2) explicit embedding_endpoint in synapses.json (Ollama/OpenAI compat),
	//           (3) FTS5-only fallback (no embeddings).
	// Embeddings are built/updated in the background so startup is never delayed.
	{
		var embedCli *embed.Client
		if cfg.EmbeddingEndpoint != "" {
			embedCli = embed.NewClient(cfg.EmbeddingEndpoint, "")
			logutil.Info("synapses: embeddings via %s\n", cfg.EmbeddingEndpoint)
		}
		if embedCli != nil {
			srv.SetEmbedClient(embedCli)
			go embedAllNodes(appCtx, embedCli, g, st)
		}
	}

	// Memory embeddings: generate embeddings for memories on remember() writes
	// and provide vector search in recall(). Three modes:
	//   "builtin" (default) — pure-Go nomic-embed-text-v1.5, auto-downloads model
	//   "ollama"            — delegates to local Ollama instance
	//   "off"               — disabled, FTS5-only recall
	{
		memEmbedder := createMemoryEmbedder(cfg)
		if memEmbedder != nil {
			// P8-11: wire model download lifecycle events to Pulse.
			if be, ok := memEmbedder.(*embed.BuiltinEmbedder); ok && sharedPulse != nil {
				pc := sharedPulse
				be.OnModelEvent = func(eventType string) {
					pc.RecordEmbeddingEvent(pulse.EmbeddingEvent{
						EventType: eventType,
						Trigger:   "model_lifecycle",
						Model:     "all-MiniLM-L6-v2",
					})
				}
			}
			defer memEmbedder.Close()
			srv.SetMemoryEmbedder(memEmbedder)
			go embedAllMemories(appCtx, memEmbedder, st, sharedPulse)
		}
	}

	// Autosubscribe: detect tech stack from manifest files.
	go func() {
		entries := scout.DetectTechStack(absPath)
		if len(entries) == 0 {
			return
		}
		// Skip writing to server if shutdown has started.
		select {
		case <-appCtx.Done():
			return
		default:
		}
		srv.SetTechStack(entries)
		logutil.Info("synapses: tech stack detected (%d deps)\n", len(entries))
	}()

	// Start the file watcher so the graph stays current as files change.
	if !*noWatch {
		w := parser.NewWalker()
		for _, p := range cfg.Plugins {
			w.RegisterPlugin(p.Extensions, p.Command, pluginCheck)
		}
		fw, err := watcher.New(g, w, st)
		if err != nil {
			// Non-fatal: log and continue without watching.
			logutil.Error("synapses: file watcher unavailable: %v\n", err)
		} else {
			if err := fw.Start(absPath); err != nil {
				logutil.Error("synapses: file watcher start failed: %v\n", err)
			} else {
				defer fw.Stop()
				fw.SetConfig(cfg)                    // wire rules for proactive violation events
				fw.SetProjectID(pathProjectID(absPath)) // scope brain ingest to this project
				srv.SetChangeSource(fw)               // wire change log into get_working_state
				fw.SetPacketInvalidator(srv)          // clear brain packet cache on file change
				fw.SetBrainClient(brainCli)           // wire incremental ingest
				// Federation: wire cross-project dependency tracker into watcher.
				if fedResolver != nil {
					tracker := federation.NewDeterministicDetector(cfg.Federation, fedResolver)
					fw.SetCrossProjectTracker(tracker)
				}
				// Hot-reload synapses.json: reconnect brain when config changes.
				fw.SetConfigChangeHandler(func(newCfg *config.Config) {
					newBrain := brain.NewInProcess(newCfg.Brain.ToBrainConfig())
					srv.SetBrainClient(newBrain)
					fw.SetBrainClient(newBrain)
					logutil.Info("synapses: brain reloaded (enabled=%v)\n", newCfg.Brain.Enabled)
				})
				logutil.Info("synapses: watching %s for changes\n", absPath)
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
		fmt.Fprintln(os.Stderr) // visual separator
		logutil.Info("synapses: received %s, shutting down\n", sig)
		appCancel()
		time.AfterFunc(5*time.Second, func() {
			logutil.Error("synapses: graceful shutdown timed out, forcing exit\n")
			os.Exit(1)
		})
	}()

	// MCP server writes to stdout (protocol messages); all status goes to stderr.
	identity := g.ProjectIdentity()
	logutil.Info("synapses %s ready — %d nodes, %d edges (repo: %s)\n",
		version,
		identity.Summary.Files+identity.Summary.Functions+
			identity.Summary.Structs+identity.Summary.Interfaces,
		identity.Summary.Edges, identity.RepoID)
	logutil.Info("MCP server starting on stdio...\n")

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
		logutil.Info("Daemon not running at %s.\n", DaemonHTTPAddr)
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
// If the singleton daemon is running, it first queries /api/admin/health to
// surface live indexing progress before falling back to the SQLite snapshot.
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

	// If the daemon is running and a reindex is active, show live progress
	// before the cached stats. We do NOT return early — the caller also sees
	// the last-known index snapshot so they have full context (e.g. when
	// re-indexing a previously indexed repo).
	if IsSingletonDaemonRunning() {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, httpErr := client.Get("http://" + DaemonHTTPAddr + "/api/admin/health")
		if httpErr == nil {
			var health struct {
				IndexingProgress IndexingSnapshot `json:"indexing_progress"`
			}
			if jsonErr := json.NewDecoder(resp.Body).Decode(&health); jsonErr == nil {
				p := health.IndexingProgress
				if p.State == "indexing" {
					fmt.Printf("indexing: %d/%d files (%d%%)\n\n", p.Done, p.Total, p.Pct)
					// Fall through to show last-known cached stats below.
				}
			}
			resp.Body.Close()
		}
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
func loadOrBuildGraphWithStore(repoRoot string, st *store.Store, forceReindex bool, plugins []config.PluginConfig, pluginCheck *parser.PluginChecker, pc *pulse.Client, projectID string) (*graph.Graph, error) {
	// Always attempt smart reindex first: a fast filesystem mtime walk that
	// re-parses only changed files. This keeps line numbers accurate after
	// offline edits made between sessions (when the watcher was not running).
	// On repos with no changes the walk is cheap and returns immediately.
	g, err := smartReindex(repoRoot, st, plugins, pluginCheck)
	if err == nil {
		if saveErr := st.SaveGraph(g); saveErr != nil {
			logutil.Error("synapses: cache save failed: %v\n", saveErr)
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

// applyGoTypesIfEnabled runs the type-checked CALLS resolver when
// cfg.UseGoTypes is true. Errors are logged but never fatal — the graph
// already has tree-sitter CALLS edges and remains usable.
func applyGoTypesIfEnabled(g *graph.Graph, root string, cfg *config.Config) {
	if !cfg.UseGoTypes {
		return
	}
	logutil.Info("synapses: running go/types resolver (use_go_types=true)...\n")
	n, err := resolver.ResolveGoTypesCallEdges(g, root)
	if err != nil {
		logutil.Warn("synapses: go/types resolver failed (falling back to tree-sitter results): %v\n", err)
		return
	}
	logutil.Info("synapses: go/types added %d new CALLS edges\n", n)
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
			type lc struct{ name string; count int }
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
	resolverDurationMs := float64(time.Since(resolverStart).Milliseconds())
	// R31: resolve documentation → code entity links (EXPLAINS/DOCUMENTED_BY).
	if nd := resolver.ResolveDocEdges(g); nd > 0 {
		logutil.Info("synapses: resolved %d EXPLAINS edges\n", nd)
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
func smartReindex(repoRoot string, st *store.Store, plugins []config.PluginConfig, pluginCheck *parser.PluginChecker) (*graph.Graph, error) {
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
		w.RegisterPlugin(p.Extensions, p.Command, pluginCheck)
	}
	fresh, changed, removed, err := w.IncrementalReindex(g, repoRoot, known)
	if err != nil {
		return nil, fmt.Errorf("incremental reindex: %w", err)
	}

	unchanged := len(fresh) - changed
	logutil.Info("synapses: smart reindex: %d changed, %d unchanged, %d removed\n",
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
		logutil.Info("synapses: resolved %d CALLS edges\n", n)
		if ni := resolver.ResolveImplementsEdges(g); ni > 0 {
			logutil.Info("synapses: resolved %d IMPLEMENTS edges\n", ni)
		}
		// R31: re-resolve doc edges after incremental reparse.
		if nd := resolver.ResolveDocEdges(g); nd > 0 {
			logutil.Info("synapses: resolved %d EXPLAINS edges\n", nd)
		}
	}

	if saveErr := st.SaveFileMtimes(fresh); saveErr != nil {
		logutil.Error("synapses: save file mtimes: %v\n", saveErr)
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
	logutil.Info("synapses: merged linked project %s (%d nodes)\n",
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
// cmdAllowPlugin manages the per-machine plugin allowlist.
// Usage: synapses allow-plugin <command>
//
//	synapses allow-plugin --list
//	synapses allow-plugin --revoke <command>
func cmdAllowPlugin(args []string) error {
	sHome, err := synapsesHome()
	if err != nil {
		return fmt.Errorf("cannot determine synapses home: %w", err)
	}
	checker := parser.NewPluginChecker(sHome)

	if len(args) == 0 {
		fmt.Println("Usage: synapses allow-plugin <command>")
		fmt.Println("       synapses allow-plugin --list")
		fmt.Println("       synapses allow-plugin --revoke <command>")
		fmt.Println()
		fmt.Println("Approves an external parser plugin command for execution on this machine.")
		fmt.Println("Plugin commands are specified in synapses.json and execute arbitrary binaries.")
		fmt.Println("This allowlist prevents malicious repositories from running code on clone.")
		return nil
	}

	switch args[0] {
	case "--list":
		approved := checker.ListApproved()
		if len(approved) == 0 {
			fmt.Println("No plugins approved.")
			return nil
		}
		fmt.Printf("%d approved plugin(s):\n", len(approved))
		for _, cmd := range approved {
			fmt.Printf("  %s\n", cmd)
		}
		return nil

	case "--revoke":
		if len(args) < 2 {
			return fmt.Errorf("usage: synapses allow-plugin --revoke <command>")
		}
		command := strings.Join(args[1:], " ")
		if err := checker.RevokeCommand(command); err != nil {
			return fmt.Errorf("revoke plugin: %w", err)
		}
		fmt.Printf("Revoked plugin: %s\n", command)
		return nil

	default:
		command := strings.Join(args, " ")
		if err := checker.ApproveCommand(command); err != nil {
			return fmt.Errorf("approve plugin: %w", err)
		}
		fmt.Printf("Approved plugin: %s\n", command)
		fmt.Printf("This plugin will now execute when loading projects that reference it.\n")
		return nil
	}
}

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

### Key Tools

These 12 core tools cover 95% of workflows. All 40+ tools remain available — call ` + "`discover_tools(query=\"...\")`" + ` to find specialized ones.

| Goal | Tool |
|---|---|
| Start session | ` + "`session_init(agent_id=\"...\", intent=\"what you're doing\")`" + ` |
| Understand code | ` + "`prepare_context(intent=\"understand\", target=\"EntityName\")`" + ` |
| Prepare to modify | ` + "`prepare_context(intent=\"modify\", target=\"EntityName\")`" + ` |
| Find a symbol | ` + "`search(query=\"name\")`" + ` |
| Search by concept | ` + "`search(query=\"auth caching\", mode=\"semantic\")`" + ` |
| Check before implementing | ` + "`validate_plan(changes=[...])`" + ` |
| Verify after writing | ` + "`verify_implementation(files_written=[\"...\"])`" + ` |
| Save knowledge | ` + "`remember(decision=\"...\", agent_id=\"...\")`" + ` |
| Retrieve knowledge | ` + "`recall(query=\"...\")`" + ` |
| Plan tasks | ` + "`create_plan(title=\"...\", tasks=[...])`" + ` |
| Update task | ` + "`update_task(id=\"...\", status=\"done\")`" + ` |
| End session | ` + "`end_session(agent_id=\"...\")`" + ` — persists session knowledge, optionally reports usage |

### Need more?

` + "`session_init`" + ` suggests specialized tools based on your declared intent (e.g. ` + "`get_impact`" + `, ` + "`get_file_context`" + `).
Call ` + "`discover_tools(query=\"what you need\")`" + ` to find any tool by description.

### Cross-Project Queries
When multiple projects are registered with the daemon, query knowledge across them:
- ` + "`recall(query=\"...\", projects=\"*\")`" + ` — search memories across all projects
- ` + "`get_events(projects=\"backend\")`" + ` — events from a specific sibling
- ` + "`get_messages(agent_id=\"...\", projects=\"*\")`" + ` — messages across projects
- ` + "`get_agents(projects=\"*\")`" + ` — see who's working across all projects
Cross-project results appear in separate response fields (e.g. ` + "`cross_project_episodes`" + `).

### Anti-patterns
- **Prefer** Synapses tools over Grep/Glob for code exploration — they return callers, callees, and architecture rules that raw file scanning misses
- **Always** run ` + "`validate_plan()`" + ` before multi-file changes — it catches architecture violations before any code is written
- **Always** track discovered bugs as tasks via ` + "`create_plan()`" + ` immediately

### Memory Tiers

Synapses memory is organized in three tiers. Use ` + "`remember()`" + ` to save persistent knowledge about your work:

| Tier | Purpose | Persistence | Scope |
|------|---------|-----------|-------|
| **Tier 1 — Live** | In-session work tracking, todo lists, blocked tasks. Use ` + "`TodoWrite`" + ` for current work. | Session-only | This conversation |
| **Tier 2 — Anchored** | Code insights, discovered bugs, architecture decisions linked to graph nodes. Use ` + "`remember(anchor_nodes=...)`" + ` to tie memory to code entities. | Persistent; auto-flagged stale if linked node changes | All sessions |
| **Tier 3 — Durable** | User preferences, feedback, project context, external references. No code links — survives refactoring. Use ` + "`remember(decision=...)`" + ` without ` + "`anchor_nodes`" + ` for durable facts. | Persistent | All sessions |

**Memory storage rules:**
- Write memory to separate ` + "`.md`" + ` files in the project's ` + "`/Users/itachi/.claude/projects/{{project}}/memory/`" + ` directory, not to MEMORY.md.
- Never write these to MEMORY.md: code patterns, file paths, function signatures, architecture, git blame data. These belong in the living code, not memory.
- Index all memories in ` + "`MEMORY.md`" + ` with brief descriptions and links to the actual ` + "`.md`" + ` files.
- When a user asks you to remember something, always save it immediately as the correct tier.

### Workflow
` + "`session_init`" + ` → explore (` + "`prepare_context`" + `, ` + "`search`" + `) → ` + "`validate_plan`" + ` → edit files → ` + "`verify_implementation`" + ` → ` + "`end_session`" + `
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
| Save knowledge | ` + "`remember(decision=\"...\", agent_id=\"...\")`" + ` |
| Retrieve knowledge | ` + "`recall(query=\"...\")`" + ` |
| Plan tasks | ` + "`create_plan(title=\"...\", tasks=[...])`" + ` |
| Update task | ` + "`update_task(id=\"...\", status=\"done\")`" + ` |
| Get pending tasks | ` + "`get_pending_tasks()`" + ` |
| Send message | ` + "`send_message(from_agent=\"...\", topic=\"...\")`" + ` |
| Get messages | ` + "`get_messages(agent_id=\"...\")`" + ` |
| End session | ` + "`end_session(agent_id=\"...\")`" + ` |

### Cross-Project Queries
When multiple projects are registered with the daemon, query knowledge across them:
- ` + "`recall(query=\"...\", projects=\"*\")`" + ` — search memories across all projects
- ` + "`get_events(projects=\"backend\")`" + ` — events from a specific sibling
- ` + "`get_messages(agent_id=\"...\", projects=\"*\")`" + ` — messages across projects
- ` + "`get_agents(projects=\"*\")`" + ` — see who's working across all projects
Cross-project results appear in separate response fields (e.g. ` + "`cross_project_episodes`" + `).

### Workflow
` + "`session_init`" + ` → ` + "`recall`" + ` / ` + "`get_pending_tasks`" + ` → work → ` + "`remember`" + ` / ` + "`update_task`" + ` → ` + "`end_session`" + `
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
	removeHookEntry(hooks, "PostToolUse", "mcp__synapses__validate_plan")
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
			"WORKFLOW: session_init → prepare_context → validate_plan → edit files → verify_implementation.'"
	}
	upsertHookEntry(hooks, "SessionStart", "startup", map[string]interface{}{
		"type":    "command",
		"command": sessionStartCmd,
	})

	// ── PostToolUse: nudge verify_implementation after any file write/edit.
	upsertHookEntry(hooks, "PostToolUse", "Write|Edit", map[string]interface{}{
		"type": "command",
		"command": "echo '[Synapses] Files written. Now call verify_implementation(files_written=[\"<path>\"]) " +
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
		mcpFile := filepath.Join(absPath, ".windsurf", "mcp_config.json")
		add(mcpFile, writeHTTPMCPServerEntry(mcpFile, absPath))
		rulesFile := filepath.Join(absPath, ".windsurfrules")
		add(rulesFile, writeGuidanceFile(absPath, rulesFile, ""))

	case "zed":
		settingsFile := filepath.Join(absPath, ".zed", "settings.json")
		add(settingsFile, writeZedMCPConfig(absPath))

	case "antigravity":
		// Antigravity (https://antigravity.google) stores workspace MCP config
		// at .agent/mcp.json and agent rules at .agent/rules/
		mcpFile := filepath.Join(absPath, ".agent", "mcp.json")
		add(mcpFile, writeHTTPMCPServerEntry(mcpFile, absPath))
		rulesFile := filepath.Join(absPath, ".agent", "rules", "synapses.md")
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

// cmdQuery loads the cached graph for a project and outputs a single entity's
// context as JSON — name, type, file, line, callers, callees, and metadata.
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
	//      This lets callers (word-under-cursor) find methods without
	//      the fully-qualified "TypeName.Method" form.
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
  synapses init                     Set up everything: index, daemon, agents
  synapses init --path /project     Target a specific directory
  synapses init --yes               Non-interactive (CI/scripting)

DAEMON:
  start     -path <dir>   Start daemon and register project (MCP proxy)
  stop                    Stop the daemon
  projects                List registered projects
  logs      [-n N]        Tail daemon log
  status    -path <dir>   Index stats and daemon health
  doctor    -path <dir>   Full health check

INDEX:
  index     -path <dir>   Index/reindex a project
  list                    List all indexed projects
  reset     -path <dir>   Remove cached index
  reset     -all          Remove ALL cached indexes

AGENTS:
  connect   --agent <name> --path <dir>   Write per-agent MCP config

UPDATE:
  update               Check for updates and install
  update    --check    Check only, don't download

OTHER:
  query, brief, export, benchmark, memory, version, help
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

	logutil.Info("synapses: embedding %d nodes (model: %s) …\n", len(nodeIDs), ec.Model())

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
}

// createMemoryEmbedder creates a memory Embedder based on the config's Embeddings mode.
// Returns nil if embeddings are disabled.
func createMemoryEmbedder(cfg *config.Config) embed.Embedder {
	mode := cfg.Embeddings
	if mode == "" {
		mode = "builtin" // default
	}
	switch mode {
	case "off":
		return nil
	case "ollama":
		endpoint := cfg.EmbeddingEndpoint
		if endpoint == "" {
			endpoint = "http://localhost:11434/api/embeddings"
		}
		e := embed.NewOllamaEmbedder(endpoint, "")
		if e == nil {
			return nil
		}
		logutil.Info("synapses: memory embeddings via ollama (%s)\n", endpoint)
		return e
	case "builtin":
		homeDir, err := os.UserHomeDir()
		if err != nil {
			logutil.Error("synapses: cannot determine home dir for builtin embeddings: %v\n", err)
			return nil
		}
		modelsDir := filepath.Join(homeDir, ".synapses", "models")
		logutil.Info("synapses: memory embeddings via builtin nomic-embed-text-v1.5\n")
		return embed.NewBuiltinEmbedder(modelsDir)
	default:
		logutil.Warn("synapses: unknown embeddings mode %q, disabling\n", mode)
		return nil
	}
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

// cmdSetup is replaced by "synapses init" — see init.go.

// cmdBenchmark runs self-validating benchmark scenarios against an indexed repo.
// Each scenario derives ground truth from the graph's own topology — no hardcoded
// node IDs, portable across any indexed codebase.
func cmdBenchmark(args []string) error {
	fs := flag.NewFlagSet("benchmark", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Repository root")
	scenario := fs.String("scenario", "all", "Scenario to run: all, context-completeness, search-accuracy, impact-coverage, graph-reachability, fts-ranking")
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

	fmt.Fprintf(os.Stderr, "synapses benchmark: %d nodes, %d edges\n", g.NodeCount(), g.EdgeCount())

	var result *benchmark.Result
	if *scenario == "" || *scenario == "all" {
		result = benchmark.RunAll(g, st)
	} else {
		sc, err := benchmark.FindScenario(*scenario)
		if err != nil {
			return err
		}
		result = benchmark.RunScenarios(g, st, []benchmark.Scenario{sc})
	}

	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	fmt.Println(string(b))
	return nil
}

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

	cmd := exec.Command("git", "init", absPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("  git init failed: %v\n  %s\n", err, strings.TrimSpace(string(out)))
		return
	}
	fmt.Printf("  Initialized git repository at %s\n", absPath)
	fmt.Println()
}
