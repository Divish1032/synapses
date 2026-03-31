// hibernate.go — project hibernate/wake lifecycle for the singleton daemon.
//
// Projects have two states: WARM (fully loaded) and HIBERNATED (snapshot on
// disk, tombstone in memory). The sweeper goroutine periodically hibernates
// idle projects to reclaim memory. Projects are woken on-demand when a new
// MCP request or IDE connection arrives.
//
// Hibernate flow:
//   1. Save graph index snapshot (zstd blob → SQLite meta table)
//   2. Close all resources via pi.Close()
//   3. Create lightweight tombstone with sentinel watcher
//
// Wake flow:
//   1. store.Open() + st.LoadGraph() + tryLoadSnapshot() — synchronous, <2s
//   2. Create MCP server + Unix socket — project is queryable
//   3. Background: smartReindex catch-up, watcher, federation, embeddings
//
// Sentinel watcher: polls .git/index mtime every 30s, sets dirty flag.
// This is an optimization hint only — smartReindex catches everything on wake.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/contextfile"
	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/logutil"
	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
	"github.com/SynapsesOS/synapses/internal/namematcher"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/scout"
	"github.com/SynapsesOS/synapses/internal/skills"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/SynapsesOS/synapses/internal/watcher"
	"github.com/SynapsesOS/synapses/internal/webcache"

)

// ── Hibernate ────────────────────────────────────────────────────────────────

// hibernateProject transitions a WARM project to HIBERNATED state.
// Uses BeginHibernate to atomically mark the project as HIBERNATING (no new
// requests can access it), then saves snapshot, closes resources, and installs
// a sentinel watcher.
func hibernateProject(reg *projectRegistry, absPath string) error {
	// 1. Atomically transition WARM → HIBERNATING under lock.
	//    After this, Get() returns (nil, false) — no new requests can use pi.
	pi := reg.BeginHibernate(absPath)
	if pi == nil {
		return nil // not WARM, has active sessions, or already gone
	}

	// 2. Save graph index snapshot to SQLite meta table so tryLoadSnapshot
	//    can restore it on wake. Safe: no one else can access pi.
	if pi.Graph != nil {
		if idx := pi.Graph.Index(); idx != nil && idx.Ready() {
			if blob, err := idx.SaveSnapshot(); err == nil && len(blob) > 0 {
				_ = pi.Store.SaveIndexSnapshot(blob)
			}
		}
	}

	// 3. Close all resources (store, watcher, MCP server, brain, embedder, etc.).
	//    Safe: pi is in HIBERNATING state, no concurrent users.
	pi.Close()

	// 4. Create tombstone with sentinel watcher.
	tomb := &HibernatedProject{
		AbsPath:      absPath,
		HibernatedAt: time.Now(),
		sentinelStop: make(chan struct{}),
	}
	go runSentinelWatcher(tomb)

	// 5. Atomically transition HIBERNATING → HIBERNATED.
	reg.FinishHibernate(absPath, tomb)

	// 6. Update projects.json.
	updateKnownProjectState(absPath, "hibernated")

	logutil.Info("synapses: hibernated %s\n", filepath.Base(absPath))
	return nil
}

// ── Sentinel Watcher ─────────────────────────────────────────────────────────

// runSentinelWatcher polls .git/index and the project root directory mtime
// every 30 seconds. If either changes, it sets the tombstone's Dirty flag.
// This is an optimization hint — smartReindex on wake always does a full mtime
// walk regardless.
//
// Cost: 1 goroutine, 0 persistent FDs, 2 stat calls every 30s.
func runSentinelWatcher(tomb *HibernatedProject) {
	gitIndex := filepath.Join(tomb.AbsPath, ".git", "index")
	lastGitMtime := statMtime(gitIndex)
	lastRootMtime := statMtime(tomb.AbsPath)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if m := statMtime(gitIndex); m.After(lastGitMtime) {
				tomb.Dirty.Store(true)
				lastGitMtime = m
			}
			if m := statMtime(tomb.AbsPath); m.After(lastRootMtime) {
				tomb.Dirty.Store(true)
				lastRootMtime = m
			}
		case <-tomb.sentinelStop:
			return
		}
	}
}

// statMtime returns the modification time of path, or zero time on error.
func statMtime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// ── Wake ─────────────────────────────────────────────────────────────────────

// wakeProjectInstance restores a hibernated project to WARM state.
// Synchronous part (<2s): store.Open + LoadGraph + tryLoadSnapshot + MCP server.
// Background: smartReindex catch-up, watcher, federation, embeddings.
func wakeProjectInstance(
	appCtx context.Context,
	absPath string,
	sharedPulse *pulse.Client,
	reg *projectRegistry,
) (*ProjectInstance, error) {
	// 1. Wait for hibernate to complete if in progress, then stop sentinel.
	//    If the project is HIBERNATING (hibernate in progress on another goroutine),
	//    poll briefly until it transitions to HIBERNATED.
	var entry *registryEntry
	for attempts := 0; attempts < 100; attempts++ { // up to 5 seconds
		reg.mu.RLock()
		e, ok := reg.projects[absPath]
		reg.mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("project %s not found in registry", absPath)
		}
		if e.state == stateHibernated {
			entry = e
			break
		}
		if e.state == stateWarm {
			// Already woken by someone else.
			return e.instance, nil
		}
		// stateHibernating — hibernate in progress, wait.
		time.Sleep(50 * time.Millisecond)
	}
	if entry == nil {
		return nil, fmt.Errorf("project %s: timed out waiting for hibernate to complete", absPath)
	}
	tomb := entry.hibernated
	tomb.StopSentinel() // idempotent via sync.Once — safe even if called again
	dirty := tomb.Dirty.Load()

	// 2. Verify project path still exists.
	if _, err := os.Stat(absPath); err != nil {
		// Remove from registry. StopSentinel already called above, so Delete
		// calling StopSentinel again is safe (idempotent via sync.Once).
		reg.Delete(absPath)
		removeKnownProject(absPath)
		return nil, fmt.Errorf("project directory gone: %w", err)
	}

	// restoreHibernated re-creates a sentinel watcher tombstone if wake fails
	// partway through, so the project stays in a valid HIBERNATED state.
	restoreHibernated := func(reason string) {
		newTomb := &HibernatedProject{
			AbsPath:      absPath,
			HibernatedAt: tomb.HibernatedAt,
			sentinelStop: make(chan struct{}),
		}
		newTomb.Dirty.Store(true) // assume dirty since wake failed
		go runSentinelWatcher(newTomb)
		reg.FinishHibernate(absPath, newTomb)
		logutil.Warn("synapses: wake %s failed (%s), restored hibernated state\n",
			filepath.Base(absPath), reason)
	}

	// 3. Load config.
	projCtx, projCancel := context.WithCancel(appCtx)

	cfgDir, found := config.FindConfigDir(absPath)
	cfg, err := config.Load(cfgDir)
	if err != nil {
		projCancel()
		restoreHibernated("config load failed")
		return nil, fmt.Errorf("load config: %w", err)
	}

	// Auto-detect knowledge mode.
	if cfg.Mode == "" && !found {
		if !hasSourceFiles(absPath) {
			cfg.Mode = "knowledge"
		}
	}

	// 4. Open store (synchronous, ~200ms).
	dbPath, err := store.DefaultPath(absPath)
	if err != nil {
		projCancel()
		restoreHibernated("db path failed")
		return nil, err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		projCancel()
		restoreHibernated("store open failed")
		return nil, fmt.Errorf("open store: %w", err)
	}

	// Start daily prune in background (same as initProjectInstance).
	go func() {
		if projCtx.Err() != nil {
			return
		}
		st.PruneStaleData(projCtx, 30)
		st.PruneOldSessions(90 * 24 * time.Hour) //nolint:errcheck
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				st.PruneStaleData(projCtx, 30)
				st.PruneOldSessions(90 * 24 * time.Hour) //nolint:errcheck
			case <-projCtx.Done():
				return
			}
		}
	}()

	// Knowledge mode: simplified wake (no graph/watcher needed).
	if cfg.Mode == "knowledge" {
		mcpsrv.Version = version
		srv := mcpsrv.NewKnowledge(cfg, st)
		srv.SetProjectID(pathProjectID(absPath))
		srv.SetProjectPath(absPath)
		srv.SetProjectRegistry(&registryAdapter{reg: reg})
		srv.SetUpdateChecker(getPendingUpdateVersion)
		srv.StartBackground()
		if sharedPulse != nil {
			srv.SetPulseClient(sharedPulse)
		}
		loadAndSetPrompts(srv, absPath)
		httpHandler := mcpserver.NewStreamableHTTPServer(srv.MCPServer())

		knowledgeEmbedder := createMemoryEmbedder(cfg)
		if knowledgeEmbedder != nil {
			srv.SetMemoryEmbedder(knowledgeEmbedder)
			go func() {
				if err := knowledgeEmbedder.WarmUp(projCtx); err != nil {
					logutil.Warn("synapses: knowledge-mode embedder warmup: %v\n", err)
				}
			}()
		}

		pi := &ProjectInstance{
			AbsPath:        absPath,
			Store:          st,
			MCPServer:       srv,
			HTTPHandler:    httpHandler,
			MemoryEmbedder: knowledgeEmbedder,
			cancel:         projCancel,
		}
		reg.mu.Lock()
		regEntry := &registryEntry{state: stateWarm, instance: pi}
		regEntry.lastAccess.Store(time.Now().UnixNano())
		reg.projects[absPath] = regEntry
		reg.mu.Unlock()
		startProjectSocket(projCtx, srv, absPath, reg)
		updateKnownProjectState(absPath, "warm")
		logutil.Info("synapses: woke %s (knowledge mode)\n", filepath.Base(absPath))
		return pi, nil
	}

	// 5. Load graph from SQLite cache (synchronous, ~1-1.5s).
	//    This loads the graph as it was at hibernate time.
	//    DO NOT call loadOrBuildGraphWithStore/smartReindex here — that would
	//    re-parse changed files synchronously and blow past the 2s budget.
	g, err := st.LoadGraph()
	if err != nil || g == nil {
		st.Close()
		projCancel()
		restoreHibernated("LoadGraph failed")
		return nil, fmt.Errorf("wake: LoadGraph failed (will need cold start): %w", err)
	}

	// 6. Restore columnar index from snapshot blob (synchronous, ~50-100ms).
	tryLoadSnapshot(g, st)

	// Enable FlatGraph if configured.
	if cfg.UseFlatGraph {
		g.EnableFlatGraph()
	}

	// 7. Create MCP server (synchronous, ~10ms).
	mcpsrv.Version = version
	srv := mcpsrv.New(g, cfg, st)
	srv.SetProjectID(pathProjectID(absPath))
	srv.SetProjectRegistry(&registryAdapter{reg: reg})
	srv.SetUpdateChecker(getPendingUpdateVersion)
	srv.StartBackground()

	loadAndSetPrompts(srv, absPath)

	// Brain client (lightweight, no network call).
	brainCli := brain.NewInProcess(cfg.Brain.ToBrainConfig())
	if cfg.Brain.Enabled {
		srv.SetBrainClient(brainCli)
	}

	// Web doc cache.
	wc := webcache.New(st)
	srv.SetWebCache(wc)
	srv.SetProjectPath(absPath)

	// Pulse.
	if sharedPulse != nil {
		srv.SetPulseClient(sharedPulse)
	}

	// Memory embeddings.
	memEmbedder := createMemoryEmbedder(cfg)
	if memEmbedder != nil {
		srv.SetMemoryEmbedder(memEmbedder)
	}

	// 8. HTTP handler (synchronous, ~10ms).
	httpHandler := mcpserver.NewStreamableHTTPServer(srv.MCPServer())

	// ── Project is now QUERYABLE ─────────────────────────────────────────
	pi := &ProjectInstance{
		AbsPath:        absPath,
		Graph:          g,
		Store:          st,
		MCPServer:      srv,
		HTTPHandler:    httpHandler,
		BrainClient:    brainCli,
		MemoryEmbedder: memEmbedder,
		cancel:         projCancel,
	}

	// 9. Update registry to WARM, then start socket with entry for conn tracking.
	reg.mu.Lock()
	regEntry := &registryEntry{state: stateWarm, instance: pi}
	regEntry.lastAccess.Store(time.Now().UnixNano())
	reg.projects[absPath] = regEntry
	reg.mu.Unlock()
	startProjectSocket(projCtx, srv, absPath, reg)
	updateKnownProjectState(absPath, "warm")

	// ── Background: catch-up and subsystem startup ───────────────────────

	// 10. Incremental reindex: detect files changed during hibernation.
	go func() {
		if projCtx.Err() != nil {
			return
		}
		known, err := st.LoadFileMtimes()
		if err != nil || len(known) == 0 {
			return
		}
		w := parser.NewWalker()
		var pluginCheck *parser.PluginChecker
		if len(cfg.Plugins) > 0 {
			if sHome, homeErr := synapsesHome(); homeErr == nil {
				pluginCheck = parser.NewPluginChecker(sHome)
				for _, p := range cfg.Plugins {
					w.RegisterPlugin(p.Extensions, p.Command, pluginCheck)
				}
			}
		}
		fresh, changed, removed, err := w.IncrementalReindex(g, absPath, known)
		if err != nil {
			logutil.Warn("synapses: wake reindex %s: %v\n", filepath.Base(absPath), err)
			return
		}
		if changed+removed > 0 {
			logutil.Info("synapses: wake reindex %s: %d changed, %d removed\n",
				filepath.Base(absPath), changed, removed)
			if stored, err := st.LoadCallSites(); err == nil {
				g.BulkAddCallSites(stored)
			}
			resolver.ResolveCallEdges(g)
			resolver.ResolveImplementsEdges(g)
			resolver.ResolveDocEdges(g)
			resolver.ResolveTerraformRefs(g)

			if cfg.UseFlatGraph {
				g.EnableFlatGraph()
			}
		}
		if saveErr := st.SaveFileMtimes(fresh); saveErr != nil {
			logutil.Error("synapses: wake save mtimes: %v\n", saveErr)
		}
		// Rebuild index after catch-up changes.
		if blob, err := g.RebuildIndex(); err == nil && len(blob) > 0 {
			_ = st.SaveIndexSnapshot(blob)
		}
	}()

	// 11. File watcher (background — directory walk takes 500ms-2s).
	go func() {
		if projCtx.Err() != nil {
			return
		}
		w := parser.NewWalker()
		if len(cfg.Plugins) > 0 {
			if sHome, homeErr := synapsesHome(); homeErr == nil {
				pluginCheck := parser.NewPluginChecker(sHome)
				for _, p := range cfg.Plugins {
					w.RegisterPlugin(p.Extensions, p.Command, pluginCheck)
				}
			}
		}
		fw2, err := watcher.New(g, w, st)
		if err != nil {
			logutil.Warn("synapses: wake watcher %s: %v\n", filepath.Base(absPath), err)
			return
		}
		if startErr := fw2.Start(absPath); startErr != nil {
			logutil.Warn("synapses: wake watcher start %s: %v\n", filepath.Base(absPath), startErr)
			return
		}
		fw2.SetConfig(cfg)
		fw2.SetProjectID(pathProjectID(absPath))
		if sharedPulse != nil {
			fw2.SetPulseClient(sharedPulse)
		}
		srv.SetChangeSource(fw2)
		fw2.SetPacketInvalidator(srv)
		fw2.SetBrainClient(brainCli)
		nm := namematcher.New(brainCli)
		nm.PrimeCrossDomain(g)
		fw2.SetNameMatcher(nm)
		if cfg.UseFlatGraph {
			fw2.SetAfterRebuildHook(func() { g.EnableFlatGraph() })
		}
		// Eager re-embed: when watcher marks embeddings stale, immediately
		// queue them for re-embedding instead of waiting for the 60s retry loop.
		if memEmbedder != nil {
			fw2.SetOnStaleEmbeddings(func(memoryIDs []string) {
				for _, memID := range memoryIDs {
					content, ok := st.GetMemoryContent(memID)
					if !ok || content == "" {
						continue
					}
					srv.QueueEmbedMemory(projCtx, memEmbedder, st, memID, content)
				}
			})
		}

		// Federation wiring.
		if len(cfg.Federation) > 0 {
			cfgDir2, _ := config.FindConfigDir(absPath)
			fedResolver := federation.NewResolver(cfg.Federation, cfgDir2)
			srv.SetFederationResolver(fedResolver)
			pi.SetFederationResolver(fedResolver)
			go func() { <-projCtx.Done(); fedResolver.Close() }()

			if cfg.Brain.Enabled {
				fedResolver.SetBrain(brainCli)
				fedResolver.SetBrainGenerate(brainCli.Generate)
			}
			fedTracker := federation.NewDeterministicDetector(cfg.Federation, fedResolver)
			fw2.SetCrossProjectTracker(fedTracker)
			if cfg.Brain.Enabled {
				aliases := fedResolver.Aliases()
				brainDet := federation.NewBrainDetector(brainCli.Generate, fedResolver, aliases)
				brainAdapter := federation.NewBrainTrackerAdapter(brainDet, fedTracker)
				if brainAdapter != nil {
					fw2.SetBrainCrossProjectTracker(brainAdapter)
				}
			}
			fw2.SetConfigChangeHandler(func(newCfg *config.Config) {
				newBrain := brain.NewInProcess(newCfg.Brain.ToBrainConfig())
				srv.SetBrainClient(newBrain)
				fw2.SetBrainClient(newBrain)
				fw2.SetNameMatcher(namematcher.New(newBrain))
				fedTracker.Rebuild(newCfg.Federation)
			})
		}
		pi.SetWatcher(fw2)
	}()

	// 12. Other background subsystems.
	go func() {
		if cfg.Brain.Enabled {
			// FederationResolver is set by the watcher goroutine above; brain
			// wiring for federation happens there. Here we only do brain ingest.
			go fetchTopNSummaries(projCtx, brainCli, g, st, 20)
			bulkIngestToBrain(projCtx, brainCli, g, pathProjectID(absPath))
			fetchAndWriteBackSummaries(projCtx, brainCli, g, st)
		}
	}()
	go webcache.IndexProjectImports(projCtx, absPath, g, wc, 20)
	go func() {
		entries := scout.DetectTechStack(absPath)
		if len(entries) > 0 {
			srv.SetTechStack(entries)
		}
	}()
	var wakeNodeEmbedder embed.Embedder
	if cfg.EmbeddingEndpoint != "" {
		wakeNodeEmbedder = embed.NewClient(cfg.EmbeddingEndpoint, "")
	} else if memEmbedder != nil {
		wakeNodeEmbedder = memEmbedder
	}
	if wakeNodeEmbedder != nil {
		srv.SetEmbedClient(wakeNodeEmbedder)
		go embedAllNodes(projCtx, wakeNodeEmbedder, g, st)
	}
	if memEmbedder != nil {
		go func() {
			if err := memEmbedder.WarmUp(projCtx); err != nil {
				logutil.Warn("synapses: wake embedder warmup: %v\n", err)
			}
		}()
		go embedAllMemories(projCtx, memEmbedder, st, sharedPulse)
	}
	// Context file (auto-injected session context).
	go func() {
		identity := g.ProjectIdentity()
		var tasks []store.Task
		if pending, err := st.GetPendingTasks("", ""); err == nil {
			tasks = pending
		}
		_ = contextfile.Write(absPath, identity, tasks)
	}()

	if dirty {
		logutil.Info("synapses: woke %s (dirty — background catch-up)\n", filepath.Base(absPath))
	} else {
		logutil.Info("synapses: woke %s (clean — snapshot restored)\n", filepath.Base(absPath))
	}
	return pi, nil
}

// ── Helpers shared between initProjectInstance and wakeProjectInstance ────────

// loadAndSetPrompts loads builtin + user + project prompts and sets them on srv.
func loadAndSetPrompts(srv *mcpsrv.Server, absPath string) {
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
	allPrompts = skills.DeduplicatePrompts(allPrompts)
	if len(allPrompts) > 0 {
		srv.SetPromptTemplates(allPrompts)
	}
}

// startProjectSocket creates the per-project Unix socket for stdio proxy compat.
// reg is passed for activeConns tracking; may be nil when registry isn't available.
func startProjectSocket(ctx context.Context, srv *mcpsrv.Server, absPath string, reg *projectRegistry) {
	sockPath, sockErr := daemonSocketPath(absPath)
	if sockErr != nil {
		return
	}
	os.Remove(sockPath) // clean stale socket
	listener, listenErr := net.Listen("unix", sockPath)
	if listenErr != nil {
		return
	}
	os.Chmod(sockPath, 0o700) //nolint:errcheck
	go serveProjectSocket(ctx, srv, listener, reg, absPath)
}

// ── Sweeper ──────────────────────────────────────────────────────────────────

// Sweeper defaults — used when HibernateConfig fields are zero.
const (
	defaultIdleMinutes     = 60
	defaultPressureMinutes = 30
	defaultHeapThresholdMB = 1024 // 1 GB
)

// startHibernationSweeper runs a goroutine that periodically hibernates idle projects.
func startHibernationSweeper(ctx context.Context, reg *projectRegistry) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			sweepOnce(reg)
		case <-ctx.Done():
			return
		}
	}
}

// sweepOnce checks all WARM projects and hibernates those that are idle and
// have no active connections or sessions.
func sweepOnce(reg *projectRegistry) {
	now := time.Now()

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	heapMB := memStats.HeapInuse / (1024 * 1024)

	reg.mu.RLock()
	var candidates []string
	for path, entry := range reg.projects {
		if entry.state != stateWarm {
			continue
		}
		// Never hibernate projects with active proxy connections (IDE is open).
		if entry.activeConns.Load() > 0 {
			continue
		}
		// Never hibernate projects with active MCP sessions.
		if entry.instance.MCPServer != nil && entry.instance.MCPServer.ActiveSessionCount() > 0 {
			continue
		}

		// Read per-project hibernate config thresholds.
		idleMin := defaultIdleMinutes
		pressureMin := defaultPressureMinutes
		thresholdMB := uint64(defaultHeapThresholdMB)
		disabled := false
		if entry.instance.MCPServer != nil {
			if cfg := entry.instance.MCPServer.Config(); cfg != nil {
				hc := cfg.Hibernate
				if hc.Disabled {
					disabled = true
				}
				if hc.IdleMinutes > 0 {
					idleMin = hc.IdleMinutes
				}
				if hc.PressureIdleMinutes > 0 {
					pressureMin = hc.PressureIdleMinutes
				}
				if hc.HeapThresholdMB > 0 {
					thresholdMB = uint64(hc.HeapThresholdMB)
				}
			}
		}
		if disabled {
			continue
		}

		memoryPressure := heapMB > thresholdMB
		idle := now.Sub(time.Unix(0, entry.lastAccess.Load()))
		if idle > time.Duration(idleMin)*time.Minute ||
			(idle > time.Duration(pressureMin)*time.Minute && memoryPressure) {
			candidates = append(candidates, path)
		}
	}
	reg.mu.RUnlock()

	for _, path := range candidates {
		if err := hibernateProject(reg, path); err != nil {
			logutil.Warn("synapses: hibernate %s failed: %v\n", filepath.Base(path), err)
		}
	}
}
