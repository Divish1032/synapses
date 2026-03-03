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
	"github.com/SynapsesOS/synapses/internal/graph"
	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
	"github.com/SynapsesOS/synapses/internal/metrics"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/peer"
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/scout"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/SynapsesOS/synapses/internal/watcher"
)

// version is set at build time via ldflags: -X main.version=<tag>
var version = "dev"

func main() {
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
		return cmdStart(args[1:])
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
	case "query":
		return cmdQuery(args[1:])
	case "export":
		return cmdExport(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q — run 'synapses help'", args[0])
	}
}

// cmdStart indexes the repo (using cache if available), starts the file watcher
// for incremental updates, then starts the MCP server on stdio.
func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Path to the repository root")
	forceReindex := fs.Bool("reindex", false, "Force a full re-index even if cache is fresh")
	noWatch := fs.Bool("no-watch", false, "Disable the file watcher (useful for read-only mounts)")
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

	// Create the MCP server early so we can wire the watcher into it below.
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
	var scoutCli *scout.Client
	if cfg.Scout.URL != "" {
		scoutCli = scout.NewClient(cfg.Scout.URL, cfg.Scout.TimeoutSec)
		if scoutCli.Health(context.Background()) {
			fmt.Fprintf(os.Stderr, "synapses: scout connected at %s\n", cfg.Scout.URL)
			srv.SetScoutClient(scoutCli)
		} else {
			fmt.Fprintf(os.Stderr, "synapses: scout unreachable at %s (continuing without)\n", cfg.Scout.URL)
			scoutCli = nil
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
				fmt.Fprintf(os.Stderr, "synapses: watching %s for changes\n", absPath)
			}
		}
	}

	// Intercept OS signals so we can shut down cleanly (flush watcher, close store).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\nsynapses: received %s, shutting down\n", sig)
		os.Exit(0)
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
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("locate cache dir: %w", err)
		}
		synapsesCache := filepath.Join(cacheDir, "synapses", "cache")
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

	projectName := filepath.Base(absPath)
	fmt.Printf("Synapses — setting up %s\n\n", projectName)

	// ── Step 1: Index ──────────────────────────────────────────────────────────
	fmt.Printf("Indexing...\n")
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

	// ── Step 2: Write .mcp.json ────────────────────────────────────────────────
	mcpFile := filepath.Join(absPath, ".mcp.json")
	if err := writeMCPConfig(mcpFile, absPath); err != nil {
		return fmt.Errorf("write .mcp.json: %w", err)
	}
	fmt.Printf("Writing .mcp.json...\n")
	fmt.Printf("  ✓ %s\n\n", mcpFile)

	// ── Step 3: Write CLAUDE.md ─────────────────────────────────────────────────
	fmt.Printf("Writing CLAUDE.md...\n")
	if err := writeProjectCLAUDE(absPath); err != nil {
		fmt.Printf("  ! could not update CLAUDE.md: %v\n\n", err)
	} else {
		fmt.Printf("  ✓ %s\n\n", filepath.Join(absPath, ".claude", "CLAUDE.md"))
	}

	// ── Step 4: Write .claude/settings.json ────────────────────────────────────
	fmt.Printf("Writing .claude/settings.json...\n")
	if err := writeClaudeSettings(absPath); err != nil {
		fmt.Printf("  ! could not update .claude/settings.json: %v\n\n", err)
	} else {
		fmt.Printf("  ✓ %s\n\n", filepath.Join(absPath, ".claude", "settings.json"))
	}

	// ── Step 5: Next steps ─────────────────────────────────────────────────────
	fmt.Printf("Next step — reload MCP servers in Claude Code:\n")
	fmt.Printf("  Type  /mcp  in the chat, or close and reopen the chat panel.\n\n")
	fmt.Printf("Or register via CLI (user-scoped, works across all projects):\n")
	fmt.Printf("  claude mcp add --scope user synapses -- synapses start -path %s\n\n", absPath)
	fmt.Printf("Your agent will then have access to:\n")
	fmt.Printf("  get_project_identity   get_context   find_entity\n")
	fmt.Printf("  validate_plan          get_violations\n")
	return nil
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
| Understand a function, struct, or interface | ` + "`get_context(entity=\"Name\")`" + ` |
| Pin to a specific file (avoids wrong-entity picks) | ` + "`get_context(entity=\"Name\", file=\"cmd/server/main.go\")`" + ` |
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

### Rules
- **Read/Grep** are for *writing* code (editing a specific file you have already found). For *understanding* code structure, always prefer Synapses tools.
- **Call ` + "`session_init()`" + `** at the start of every session. It replaces the 3-call startup ritual.
- **Call ` + "`validate_plan()`" + `** before implementing multi-file changes.
- When ` + "`get_context`" + ` returns ` + "`other_candidates`" + `, re-call with ` + "`file=`" + ` to pin to the right entity.
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
		if strings.TrimSpace(string(rootData)) == "" || strings.TrimSpace(rs[:strings.Index(rs, sectionStart)]) == "" {
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
		"command": "echo '[Synapses] This project is indexed by Synapses code intelligence. " +
			"Call session_init() ONCE at session start — it returns pending tasks, project identity, " +
			"working state, and scale_guidance in one round-trip. " +
			"For code exploration use: get_context(entity, file=), find_entity, search, get_call_chain, get_impact, get_file_context. " +
			"Use validate_plan() before implementing multi-file changes.'",
	})

	// ── PreToolUse hook on Glob|Grep (visible in verbose mode) ───────────
	upsertHookEntry(hooks, "PreToolUse", "Glob|Grep", map[string]interface{}{
		"type":    "command",
		"command": "echo '[Synapses] This project is indexed — prefer get_context(entity, file=), find_entity, or search(mode=semantic) over file scanning for code exploration. Use Read/Grep only when writing to a specific file you have already identified.'",
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
  get_violations         List all current rule violations
`, version)
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

// bulkIngestToBrain sends high-value nodes to the brain sidecar for summarization.
// "High-value" means: exported with callers, heavily-used (fanin>3), entry points,
// or interface implementations. Low-fanin unexported helpers are skipped — they
// will be enriched on-demand when get_context is called for them.
// This reduces init-time ingest from ~700 nodes to ~80-100, keeping startup fast.
func bulkIngestToBrain(bc *brain.Client, g *graph.Graph) {
	all := g.AllNodes()

	// Collect high-value nodes only.
	nodes := make([]*graph.Node, 0, 150)
	for _, n := range all {
		t := string(n.Type)
		if t == "package" || t == "file" {
			continue
		}
		fanin := g.Fanin(n.ID) // caller count (EdgeCalls only)

		// Include if: heavily called, exported+used, entry point, or interface impl.
		isEntryPoint := n.Name == "main" || n.Name == "init" || strings.HasSuffix(n.Name, ".main") || strings.HasSuffix(n.Name, ".init")
		isHighFanin := fanin > 3
		isExportedUsed := n.Exported && fanin > 0
		isImpl := len(g.OutEdges(n.ID)) > 0 && t == "method" && n.Exported

		if isHighFanin || isEntryPoint || isExportedUsed || isImpl {
			nodes = append(nodes, n)
		}
	}

	// Sort by caller count descending — most-connected first.
	sort.Slice(nodes, func(i, j int) bool {
		return g.Fanin(nodes[i].ID) > g.Fanin(nodes[j].ID)
	})

	// Cap at 100 nodes to bound init time regardless of repo size.
	const maxIngest = 100
	if len(nodes) > maxIngest {
		nodes = nodes[:maxIngest]
	}

	sem := make(chan struct{}, 4) // 4 concurrent — 7b is slower than 1.5b
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
	fmt.Fprintf(os.Stderr, "synapses: ingested %d high-value nodes to brain (of %d total)\n", len(nodes), len(all))
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
				if _, err := st.AddAnnotation(string(node.ID), "brain", summary); err == nil {
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
