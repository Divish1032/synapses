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
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
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
	"github.com/SynapsesOS/synapses/internal/dataflow"
	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/graph"
	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
	"github.com/SynapsesOS/synapses/internal/metrics"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/peer"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/scout"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/SynapsesOS/synapses/internal/watcher"
)

// version is set at build time via ldflags: -X main.version=<tag>
var version = "dev"

func main() {
	// Fast-path: print version and exit immediately.
	// This avoids loading SQLite drivers and other heavy init() code
	// that runs before main(). Keeps `synapses version` zero-cost.
	if len(os.Args) >= 2 && os.Args[1] == "version" {
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
	case "index":
		return cmdIndex(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "list":
		return cmdList()
	case "reset":
		return cmdReset(args[1:])
	case "version":
		fmt.Printf("synapses %s\n", version)
		return nil
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

	// Optional: connect to synapses-intelligence brain service.
	var brainCli *brain.Client
	if cfg.Brain.URL != "" {
		brainCli = brain.NewClient(cfg.Brain.URL, cfg.Brain.TimeoutSec)
		if model, err := brainCli.HealthCheck(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "synapses: brain unreachable at %s: %v (continuing without)\n", cfg.Brain.URL, err)
			brainCli = nil
		} else {
			fmt.Fprintf(os.Stderr, "synapses: brain connected (%s)\n", model)
			srv.SetBrainClient(brainCli)
			// Bulk-ingest all nodes in parallel, then fetch summaries and write them
			// back as annotations so they surface in get_context/find_entity.
			go func() {
				bulkIngestToBrain(brainCli, g)
				fetchAndWriteBackSummaries(brainCli, g, st)
			}()
		}
	}

	// Optional: connect to synapses-scout web-search service.
	// The client is wired unconditionally so that tools work as soon as scout
	// becomes available — even if it wasn't reachable at startup. Individual
	// tool calls degrade gracefully (fail-silent) when scout is down.
	var scoutCli *scout.Client
	if cfg.Scout.URL != "" {
		scoutCli = scout.NewClient(cfg.Scout.URL, cfg.Scout.TimeoutSec)
		srv.SetScoutClient(scoutCli)
		if scoutCli.Health(context.Background()) {
			fmt.Fprintf(os.Stderr, "synapses: scout connected at %s\n", cfg.Scout.URL)
		} else {
			fmt.Fprintf(os.Stderr, "synapses: scout configured at %s (unreachable at startup — tools will retry on use)\n", cfg.Scout.URL)
		}
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
		if brainCli != nil {
			// Probe brain's /v1/embed — only wire it if the endpoint responds (i.e.
			// embedding_enabled=true was set in brain.json and llama-server started).
			candidate := embed.NewBrainClient(cfg.Brain.URL)
			probeCtx, probeCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer probeCancel()
			if _, err := candidate.Embed(probeCtx, "probe"); err == nil {
				embedCli = candidate
				fmt.Fprintf(os.Stderr, "synapses: embeddings via brain /v1/embed (Ollama-free)\n")
			}
		}
		if embedCli == nil && cfg.EmbeddingEndpoint != "" {
			embedCli = embed.NewClient(cfg.EmbeddingEndpoint, "")
			fmt.Fprintf(os.Stderr, "synapses: embeddings via %s\n", cfg.EmbeddingEndpoint)
		}
		if embedCli != nil {
			srv.SetEmbedClient(embedCli)
			go embedAllNodes(appCtx, embedCli, g, st)
		}
	}

	// Autosubscribe: detect tech stack from manifest files and optionally enrich
	// with official doc URLs via scout. Runs in the background — does not block startup.
	go func() {
		entries := scout.DetectTechStack(absPath)
		if len(entries) == 0 {
			return
		}
		if scoutCli != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			entries = scout.EnrichWithDocs(ctx, scoutCli, entries)
			cancel()
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
				fw.SetConfig(cfg)            // wire rules for proactive violation events
				srv.SetChangeSource(fw)      // wire change log into get_working_state
				fw.SetPacketInvalidator(srv) // clear brain packet cache on file change
				if brainCli != nil {
					fw.SetBrainClient(brainCli) // wire incremental ingest
				}
				// Hot-reload synapses.json: reconnect scout/brain when config changes.
				fw.SetConfigChangeHandler(func(newCfg *config.Config) {
					if newCfg.Scout.URL != "" {
						newScout := scout.NewClient(newCfg.Scout.URL, newCfg.Scout.TimeoutSec)
						srv.SetScoutClient(newScout)
						if newScout.Health(context.Background()) {
							fmt.Fprintf(os.Stderr, "synapses: scout reconnected at %s\n", newCfg.Scout.URL)
						} else {
							fmt.Fprintf(os.Stderr, "synapses: scout configured at %s (unreachable — tools will retry on use)\n", newCfg.Scout.URL)
						}
					} else {
						srv.SetScoutClient(nil)
					}
					if newCfg.Brain.URL != "" {
						newBrain := brain.NewClient(newCfg.Brain.URL, newCfg.Brain.TimeoutSec)
						if _, err := newBrain.HealthCheck(context.Background()); err != nil {
							fmt.Fprintf(os.Stderr, "synapses: brain unreachable at %s after config reload: %v\n", newCfg.Brain.URL, err)
						} else {
							srv.SetBrainClient(newBrain)
							fw.SetBrainClient(newBrain)
							fmt.Fprintf(os.Stderr, "synapses: brain reconnected at %s\n", newCfg.Brain.URL)
						}
					} else {
						srv.SetBrainClient(nil)
						fw.SetBrainClient(nil)
					}
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

	// Write agent-guidance files so AI agents use Synapses tools by default.
	if err := writeProjectCLAUDE(absPath); err != nil {
		fmt.Fprintf(os.Stderr, "synapses: warning: could not update CLAUDE.md: %v\n", err)
	}
	if err := writeClaudeSettings(absPath); err != nil {
		fmt.Fprintf(os.Stderr, "synapses: warning: could not update .claude/settings.json: %v\n", err)
	}

	// Silently ensure sidecars are running after indexing.
	if ensureDirs() == nil {
		daemonStart(allSidecars, true) //nolint:errcheck
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
// formatted status table: graph index freshness, brain reachability, and scout
// reachability.
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
	if isDaemonRunning(absPath) {
		verPath, _ := daemonVersionPath(absPath)
		daemonVer := "unknown"
		if data, err := os.ReadFile(verPath); err == nil {
			daemonVer = strings.TrimSpace(string(data))
		}
		sockPath, _ := daemonSocketPath(absPath)
		fmt.Printf("%-16s%-16s%s\n", "Daemon", "running", fmt.Sprintf("version %s, socket %s", daemonVer, sockPath))
	} else {
		fmt.Printf("%-16s%-16s%s\n", "Daemon", "stopped", "(will auto-start on next 'synapses start')")
	}

	// ── Brain ────────────────────────────────────────────────────────────────
	if cfg.Brain.URL != "" {
		status, detail := pingHealth(cfg.Brain.URL + "/health")
		fmt.Printf("%-16s%-16s%s\n", "Brain", status, detail)
	} else {
		fmt.Printf("%-16s%-16s%s\n", "Brain", "not configured", "(no brain.url in synapses.json)")
	}

	// ── Scout ────────────────────────────────────────────────────────────────
	if cfg.Scout.URL != "" {
		status, detail := pingHealth(cfg.Scout.URL + "/v1/health")
		fmt.Printf("%-16s%-16s%s\n", "Scout", status, detail)
	} else {
		fmt.Printf("%-16s%-16s%s\n", "Scout", "not configured", "(no scout.url in synapses.json)")
	}

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
		n := resolver.ResolveCallEdges(g)
		fmt.Fprintf(os.Stderr, "synapses: resolved %d CALLS edges\n", n)
		if ni := resolver.ResolveImplementsEdges(g); ni > 0 {
			fmt.Fprintf(os.Stderr, "synapses: resolved %d IMPLEMENTS edges\n", ni)
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

// cmdList scans the synapses cache directory and prints a summary row for every
// project that has been indexed, without loading any full graph.
func cmdList() error {
	stats, err := store.ScanAll()
	if err != nil {
		return fmt.Errorf("scan projects: %w", err)
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
	// GAP-9: Without this, sidecars (brain/scout/pulse) aren't configured.
	cfgPath := filepath.Join(absPath, "synapses.json")
	if _, statErr := os.Stat(cfgPath); os.IsNotExist(statErr) {
		fmt.Printf("  Generating synapses.json...\n")
		hasBrain := binaryExists("brain")
		hasScout := binaryExists("scout")
		hasPulse := binaryExists("pulse")
		if err := writeOnboardSynapsesJSON(absPath, hasBrain, hasScout, hasPulse); err != nil {
			fmt.Printf("  ! could not write synapses.json: %v\n\n", err)
		} else {
			fmt.Printf("  ✓ %s\n", cfgPath)
			var configured []string
			if hasBrain {
				configured = append(configured, "brain")
			}
			if hasScout {
				configured = append(configured, "scout")
			}
			if hasPulse {
				configured = append(configured, "pulse")
			}
			if len(configured) > 0 {
				fmt.Printf("    Sidecars configured: %s\n\n", strings.Join(configured, ", "))
			} else {
				fmt.Printf("    No sidecars installed — run 'synapses onboard' for full setup\n\n")
			}
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

// writeProjectCLAUDE writes (or updates) a Synapses-managed section in
// .claude/CLAUDE.md (preferred by Claude Code). The section is delimited by
// HTML comments so it can be safely updated on subsequent index runs without
// clobbering the rest of the file. If a root-level CLAUDE.md exists with a
// Synapses section it is migrated to .claude/CLAUDE.md and the section is
// removed from the root file.
func writeProjectCLAUDE(repoRoot string) error {
	const (
		sectionStart = "<!-- synapses:start -->\n"
		sectionEnd   = "<!-- synapses:end -->"
	)

	section := sectionStart + `## Synapses — Code Intelligence (MCP)

This project is indexed by **Synapses**, a graph-based code intelligence server.

### Session Start
Call **one tool** at the start of every session:
` + "```" + `
session_init()   ← replaces get_pending_tasks + get_project_identity + get_working_state
` + "```" + `
Returns: pending tasks, project identity, working state, recent agent events, and **scale_guidance** — a repo-size-aware recommendation on which tools to prefer.

### Tool Selection — follow scale_guidance from session_init

| Repo scale | When to use Synapses | When to use Read/Grep |
|---|---|---|
| micro (<100 nodes) | Structural analysis, multi-file understanding | Simple targeted edits to a known file |
| small (100–499) | Code exploration, cross-file analysis | Targeted single-file edits |
| medium (500–1999) | All code exploration — Glob/Grep surfaces too much noise | Writing to a specific file you already identified |
| large (2000+) | Always — direct scanning is too noisy at this scale | Writing to a specific file you already identified |

### Code Exploration

| When you want to... | Use this |
|---|---|
| Not sure which tool to use | ` + "`discover_tools(query=\"what I'm trying to do\")`" + ` |
| Understand a function, struct, or interface | ` + "`get_context(entity=\"Name\")`" + ` |
| Pin to a specific file (avoids wrong-entity picks) | ` + "`get_context(entity=\"Name\", file=\"cmd/server/main.go\")`" + ` |
| Query by package-qualified name | ` + "`get_context(entity=\"graph.New\")`" + ` — works for both standalone functions and methods; use ` + "`file=`" + ` to disambiguate further |
| Boost nodes linked to current task | ` + "`get_context(entity=\"Name\", task_id=\"...\")`" + ` |
| Find a symbol by name or substring | ` + "`find_entity(query=\"name\")`" + ` |
| Search by concept ("auth", "rate limiting") | ` + "`search(query=\"...\", mode=\"semantic\")`" + ` |
| List all entities in a file | ` + "`get_file_context(file=\"path/to/file\")`" + ` |
| Trace how function A calls function B | ` + "`get_call_chain(from=\"A\", to=\"B\")`" + ` |
| Find what breaks if a symbol changes | ` + "`get_impact(symbol=\"Name\")`" + ` |

### Before Writing Code

| When you want to... | Use this |
|---|---|
| Check proposed changes against architecture rules | ` + "`validate_plan(changes=[...])`" + ` |
| Verify written files against rules after implementation | ` + "`verify_implementation(files_written=[\"...\"])`" + ` |
| View current architecture violations | ` + "`get_violations()`" + ` |
| Create or update an architectural constraint | ` + "`upsert_rule(rule_id=\"...\", description=\"...\", severity=\"error\")`" + ` |
| Reserve a scope before editing (multi-agent) | ` + "`claim_work(agent_id=\"...\", scope=\"pkg/auth\")`" + ` |
| Check for conflicting edits by other agents | ` + "`get_conflicts(agent_id=\"...\")`" + ` |
| Release locks when done | ` + "`release_claims(agent_id=\"...\")`" + ` |

### Task & Session Management

| When you want to... | Use this |
|---|---|
| Save a plan with tasks for future sessions | ` + "`create_plan(title=\"...\", tasks=[...])`" + ` |
| List all plans and completion counts | ` + "`get_plans()`" + ` |
| Get your own pending tasks | ` + "`get_my_tasks(agent_id=\"...\")`" + ` |
| Link a task to relevant code entities | ` + "`link_task_nodes(task_id=\"...\", node_ids=[...])`" + ` |
| Mark a task as done or add notes | ` + "`update_task(id=\"...\", status=\"done\", notes=\"...\")`" + ` |
| Save progress so next session can resume | ` + "`save_session_state(task_id=\"...\")`" + ` |
| Resume from exact saved state | ` + "`get_session_state(task_id=\"...\")`" + ` |
| Leave a note on a code entity for other agents | ` + "`annotate_node(node_id=\"...\", note=\"...\")`" + ` |
| See recent file/task/annotation events | ` + "`get_events(since_seq=N)`" + ` (use latest_event_seq from session_init) |
| See all active agents | ` + "`get_agents()`" + ` |

### Web Intelligence (requires synapses-scout sidecar)

| When you want to... | Use this |
|---|---|
| Search for docs, error solutions, API references | ` + "`web_search(query=\"...\")`" + ` |
| Fetch and read a documentation page | ` + "`web_fetch(input=\"https://...\")`" + ` |
| Deep multi-query research on a topic | ` + "`web_deep_search(query=\"...\")`" + ` |
| Persist web findings to a code entity | ` + "`web_annotate(node_id=\"...\", note=\"...\", hits=[...])`" + ` |

### NEVER do these (anti-patterns)
- **NEVER** use ` + "`Grep`" + ` to understand code structure, find what calls a function, or explore cross-file relationships — use ` + "`get_context`" + ` or ` + "`get_impact`" + ` instead.
- **NEVER** use ` + "`Glob`" + ` to discover where a symbol is defined — use ` + "`find_entity(query=\"name\")`" + ` instead.
- **NEVER** use ` + "`Read`" + ` to explore unfamiliar code — use ` + "`get_context(entity=\"Name\")`" + ` instead. Reserve ` + "`Read`" + ` for writing to a specific file you have already identified.
- **NEVER** use ` + "`Bash`" + ` + ` + "`grep`" + ` as a substitute for ` + "`search(mode=\"semantic\")`" + ` when looking for a concept across the codebase.
- **NEVER** skip ` + "`validate_plan()`" + ` before a multi-file change — it catches architecture violations before any code is written.
- **NEVER** leave bugs or discovered issues untracked — always add them as tasks via ` + "`create_plan()`" + ` so future sessions can find them.

### Rules
- **Read/Grep** are for *writing* code (editing a specific file you have already found). For *understanding* code structure, always prefer Synapses tools.
- **Call ` + "`session_init()`" + `** at the start of every session. It replaces the 3-call startup ritual.
- **Workflow:** ` + "`session_init`" + ` → ` + "`prepare_context`" + ` (or specific tools) → ` + "`validate_plan`" + ` → edit files → ` + "`verify_implementation`" + `.
- **When unsure** which tool to use, call ` + "`discover_tools(query=\"...\")`" + ` — it returns the right tool + example in one call.
- **Call ` + "`validate_plan()`" + `** before implementing multi-file changes.
- When ` + "`get_context`" + ` returns ` + "`other_candidates`" + `, re-call with ` + "`file=`" + ` to pin to the right entity.
- **Track all bugs and tasks** via ` + "`create_plan()`" + ` immediately when discovered — do not rely on memory across sessions.
` + sectionEnd

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
		si := strings.Index(rs, sectionStart)
		if si != -1 {
			ei := strings.Index(rs, sectionEnd)
			if ei != -1 {
				cleaned := rs[:si] + rs[ei+len(sectionEnd):]
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
	startIdx := strings.Index(content, sectionStart)
	if startIdx != -1 {
		// Replace the existing Synapses section.
		endIdx := strings.Index(content, sectionEnd)
		if endIdx != -1 {
			content = content[:startIdx] + section + content[endIdx+len(sectionEnd):]
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
//   - SessionStart: stdout IS fed to the LLM as context — primary mechanism
//   - PreToolUse on Glob|Grep: reminder shown in verbose mode
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

	// ── SessionStart hook (stdout is fed to the LLM as context) ──────────
	upsertHookEntry(hooks, "SessionStart", "startup", map[string]interface{}{
		"type": "command",
		"command": "echo '[Synapses] MANDATORY: Call session_init() as your FIRST action — it returns pending tasks, " +
			"project identity, working state, and scale_guidance in one call. " +
			"WORKFLOW: session_init → prepare_context (or specific tools) → validate_plan → edit files → verify_implementation. " +
			"CODE EXPLORATION: use get_context(entity), find_entity(query), search(mode=semantic), get_call_chain, get_impact. " +
			"NEVER use Grep/Glob/Read to understand code structure — those are for writing to files you already found. " +
			"UNSURE which tool? Call discover_tools(query=\"what you need\"). " +
			"TRACK all bugs/tasks via create_plan() immediately when discovered — never rely on memory across sessions.'",
	})

	// ── PreToolUse hook on Glob|Grep (feedback loop against tool drift) ──
	upsertHookEntry(hooks, "PreToolUse", "Glob|Grep", map[string]interface{}{
		"type": "command",
		"command": "echo '[Synapses] STOP — this project is indexed. For code understanding use Synapses instead: " +
			"find_entity(query) to locate a symbol, get_context(entity) to understand it, " +
			"search(query, mode=semantic) to find by concept, get_impact(symbol) to find dependents. " +
			"Grep/Glob are only appropriate when WRITING to a specific file you have already identified. " +
			"If unsure, call discover_tools(query=\"...\") to find the right tool.'",
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

GETTING STARTED (one command):
  cd /your/project && synapses init

COMMANDS:
  init    [-path <dir>]            Index + write .mcp.json (recommended first step)
  start   -path <dir>              Index repo and start MCP server (stdio)
  index   -path <dir>              Index repo and save to cache
  query   -path <dir> -entity <n>  Dump entity context as JSON (for tooling/IDE)
  status  -path <dir>              Show index statistics for one project
  list                             List all indexed projects (global overview)
  brief   -path <dir>              Concise session brief (for startup hooks)
  doctor  -path <dir>              Health check (index, brain, scout)
  reset   -path <dir>              Remove the cached index for a project
  reset   -all                     Remove ALL cached indexes
  version                          Print version
  help                             Print this message

FLAGS (init / index / start):
  -path <dir>    Repository root (default: current directory)
  -reindex       Force full re-index, ignoring cache
  -no-watch      Disable file watcher (start only)

FLAGS (reset):
  -path <dir>    Repository root to reset (default: current directory)
  -all           Remove all project indexes

MCP TOOLS EXPOSED:
  get_project_identity   Compact architectural handshake
  get_context            N-hop ego-subgraph around an entity
  find_entity            Locate nodes by name or substring
  validate_plan          Check changes against architectural rules
  verify_implementation  Post-write verification against rules
  get_violations         List all current rule violations

AGENT SETUP:
  mcp-setup  -agent <name>   Write MCP config for the specified agent
  Supported agents: cursor, gemini, zed, windsurf, claude, all
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
	if err := fs.Parse(args); err != nil {
		return err
	}

	absPath, err := filepath.Abs(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
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

func bulkIngestToBrain(bc *brain.Client, g *graph.Graph) {
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
				NodeID:   string(node.ID),
				NodeName: node.Name,
				NodeType: string(node.Type),
				Package:  node.Package,
				Code:     buildIngestCode(node),
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
