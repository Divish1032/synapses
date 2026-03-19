// daemon_serve.go — "synapses daemon serve" subcommand.
//
// Runs ONE singleton daemon per machine. Serves multiple projects
// within a single process. Each project gets its own graph, store,
// file watcher, and Unix socket (for stdio proxy backward compat).
//
// Architecture (after singleton refactor):
//
//	┌─────────────┐   ┌───────────┐
//	│ Claude Code  │   │  Cursor   │    (stdio MCP — backward compat)
//	└──────┬───────┘   └─────┬─────┘
//	       │ stdio             │ stdio
//	       ▼                   ▼
//	┌────────────┐    ┌────────────┐
//	│ mcp proxy  │    │ mcp proxy  │   ← "synapses start --path <repo>"
//	└──────┬─────┘    └──────┬─────┘
//	       │ Unix socket       │
//	       └───────────────────┘
//	                   │
//	┌──────────────────▼──────────────────────┐
//	│       synapses singleton daemon          │  ← ONE per machine
//	│  HTTP 127.0.0.1:11435                    │
//	│  GET  /api/admin/health                  │
//	│  GET  /api/admin/projects                │
//	│  POST /api/admin/projects                │
//	│  GET  /api/admin/pulse/summary[?days=N]  │  analytics
//	│  POST|GET|DELETE /mcp?project=<path>     │  HTTP MCP transport
//	│  ~/.synapses/daemons/<hash>.sock per-proj │  stdio proxy compat
//	└─────────────────────────────────────────┘
//
// PID:     ~/.synapses/daemon.pid  (singleton)
// Sockets: ~/.synapses/daemons/<sha256(path)[:16]>.sock  (one per registered project)
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/contextfile"
	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/federation"
	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/scout"
	"github.com/SynapsesOS/synapses/internal/skills"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/SynapsesOS/synapses/internal/watcher"
	"github.com/SynapsesOS/synapses/internal/webcache"
)

// DaemonHTTPPort is the fixed port for the singleton daemon HTTP server.
// Port 11434 is Ollama's default — we use 11435 to avoid conflicts.
const DaemonHTTPPort = "11435"

// DaemonHTTPAddr is the loopback address the singleton daemon binds to.
const DaemonHTTPAddr = "127.0.0.1:" + DaemonHTTPPort

// canonicalPath resolves a path to its absolute, symlink-free form.
// This ensures that /project and /symlink-to-project map to the same daemon.
func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// If symlink resolution fails (e.g., path doesn't exist yet),
		// fall back to the absolute path.
		return abs, nil
	}
	return resolved, nil
}

// ── path helpers ──────────────────────────────────────────────────────────────

func daemonDir() (string, error) {
	base, err := synapsesHome()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "daemons")
	return dir, os.MkdirAll(dir, 0o700)
}

func projectHash(absPath string) string {
	h := sha256.Sum256([]byte(absPath))
	return fmt.Sprintf("%x", h[:8]) // 16-char hex
}

// daemonSocketPath returns the per-project Unix socket path.
// Used by both the daemon (to create the socket) and the proxy (to connect).
func daemonSocketPath(absPath string) (string, error) {
	dir, err := daemonDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, projectHash(absPath)+".sock"), nil
}

// singletonPIDPath returns the path to the singleton daemon PID file.
func singletonPIDPath() (string, error) {
	base, err := synapsesHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "daemon.pid"), nil
}

// singletonLogPath returns the singleton daemon log path.
func singletonLogPath() (string, error) {
	base, err := synapsesHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "daemon.log"), nil
}

// IsSingletonDaemonRunning checks if the singleton daemon is up by hitting
// the health endpoint. Returns true if the daemon responds within 2 seconds.
func IsSingletonDaemonRunning() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + DaemonHTTPAddr + "/api/admin/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// cleanStaleSingletonPID removes the PID file if the process is not alive.
func cleanStaleSingletonPID() {
	pidPath, err := singletonPIDPath()
	if err != nil {
		return
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || !processAlive(pid) {
		os.Remove(pidPath)
	}
}

// ── connSession: per-connection MCP session (used for Unix socket serving) ───

// connSession implements the mcp-go ClientSession, SessionWithLogging, and
// SessionWithClientInfo interfaces. Each proxy connection gets its own session.
type connSession struct {
	id            string
	notifications chan mcp.JSONRPCNotification
	initialized   atomic.Bool
	loggingLevel  atomic.Value
	clientInfo    atomic.Value
}

func (s *connSession) SessionID() string                                  { return s.id }
func (s *connSession) NotificationChannel() chan<- mcp.JSONRPCNotification { return s.notifications }
func (s *connSession) Initialize() {
	s.loggingLevel.Store(mcp.LoggingLevelError)
	s.initialized.Store(true)
}
func (s *connSession) Initialized() bool { return s.initialized.Load() }
func (s *connSession) GetClientInfo() mcp.Implementation {
	if v := s.clientInfo.Load(); v != nil {
		if ci, ok := v.(mcp.Implementation); ok {
			return ci
		}
	}
	return mcp.Implementation{}
}
func (s *connSession) SetClientInfo(ci mcp.Implementation) { s.clientInfo.Store(ci) }
func (s *connSession) SetLogLevel(level mcp.LoggingLevel)  { s.loggingLevel.Store(level) }
func (s *connSession) GetLogLevel() mcp.LoggingLevel {
	if v := s.loggingLevel.Load(); v != nil {
		return v.(mcp.LoggingLevel)
	}
	return mcp.LoggingLevelError
}

// ── cmdDaemonServe: the singleton daemon entry point ─────────────────────────

func cmdDaemonServe(args []string) error {
	// No flags — the singleton daemon serves all projects.
	// Projects are registered on-demand via the admin API.

	// ── Singleton check ──────────────────────────────────────────────────────
	cleanStaleSingletonPID()
	if IsSingletonDaemonRunning() {
		return fmt.Errorf("singleton daemon already running at %s", DaemonHTTPAddr)
	}

	pidPath, err := singletonPIDPath()
	if err != nil {
		return fmt.Errorf("pid path: %w", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return fmt.Errorf("write pid: %w", err)
	}
	defer os.Remove(pidPath)

	// ── App context for graceful shutdown ────────────────────────────────────
	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	// ── Project registry ─────────────────────────────────────────────────────
	reg := newProjectRegistry()
	defer reg.Close()

	// ── Pulse (shared across all projects) ───────────────────────────────────
	var sharedPulse *pulse.Client
	if pulseDBPath, err := pulse.DefaultDBPath(); err == nil {
		if pulseCli, err := pulse.New(pulseDBPath); err == nil {
			sharedPulse = pulseCli
			defer pulseCli.Close()
			fmt.Fprintf(os.Stderr, "synapses: pulse analytics enabled (in-process, db: %s)\n", pulseDBPath)
		}
	}

	// ── HTTP MCP router ───────────────────────────────────────────────────────
	// /mcp?project=<absPath>  → per-project StreamableHTTPServer
	// /api/admin/*            → daemon management API
	mux := http.NewServeMux()

	// Admin: health
	mux.HandleFunc("/api/admin/health", func(w http.ResponseWriter, r *http.Request) {
		projects := reg.All()
		paths := make([]string, 0, len(projects))
		for _, p := range projects {
			paths = append(paths, p.AbsPath)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":            "ok",
			"version":           version,
			"projects":          paths,
			"indexing_progress": ActiveSnapshot(),
		})
	})

	// Admin: list projects
	mux.HandleFunc("/api/admin/projects", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			projects := reg.All()
			type projectInfo struct {
				Path   string `json:"path"`
				Hash   string `json:"hash"`
				Socket string `json:"socket"`
			}
			infos := make([]projectInfo, 0, len(projects))
			for _, p := range projects {
				sock, _ := daemonSocketPath(p.AbsPath)
				infos = append(infos, projectInfo{
					Path:   p.AbsPath,
					Hash:   projectHash(p.AbsPath),
					Socket: sock,
				})
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"projects": infos})

		case http.MethodPost:
			var req struct {
				Path string `json:"path"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			absPath, err := canonicalPath(req.Path)
			if err != nil {
				http.Error(w, "invalid path: "+err.Error(), http.StatusBadRequest)
				return
			}
			sockPath, err := daemonSocketPath(absPath)
			if err != nil {
				http.Error(w, "socket path error: "+err.Error(), http.StatusInternalServerError)
				return
			}
			// GetOrSet: lazy-initialize the project if not already registered.
			_, initErr := reg.GetOrSet(absPath, func() (*ProjectInstance, error) {
				return initProjectInstance(appCtx, absPath, sharedPulse)
			})
			if initErr != nil {
				http.Error(w, "init project: "+initErr.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "ok",
				"path":   absPath,
				"socket": sockPath,
			})

		case http.MethodDelete:
			var req struct {
				Path string `json:"path"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			absPath, _ := canonicalPath(req.Path)
			reg.Delete(absPath)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Admin: pulse analytics summary
	mux.HandleFunc("/api/admin/pulse/summary", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		days := 7
		if d := r.URL.Query().Get("days"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sharedPulse.GetSummary(days))
	})

	// MCP: route to per-project StreamableHTTPServer
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		projectPath := r.URL.Query().Get("project")
		if projectPath == "" {
			http.Error(w, "missing ?project=<abs-path> query parameter", http.StatusBadRequest)
			return
		}

		// Decode URL-encoded path
		if decoded, err := url.QueryUnescape(projectPath); err == nil {
			projectPath = decoded
		}

		absPath, err := canonicalPath(projectPath)
		if err != nil {
			http.Error(w, "invalid project path: "+err.Error(), http.StatusBadRequest)
			return
		}

		pi, initErr := reg.GetOrSet(absPath, func() (*ProjectInstance, error) {
			return initProjectInstance(appCtx, absPath, sharedPulse)
		})
		if initErr != nil {
			http.Error(w, "init project: "+initErr.Error(), http.StatusInternalServerError)
			return
		}

		// Strip the ?project= param before forwarding so the MCP server
		// doesn't see unknown query parameters.
		r2 := r.Clone(r.Context())
		q := r2.URL.Query()
		q.Del("project")
		r2.URL.RawQuery = q.Encode()

		pi.HTTPHandler.ServeHTTP(w, r2)
	})

	// ── HTTP server ───────────────────────────────────────────────────────────
	// Wrap the mux with a CORS handler so the Tauri WebView (origin tauri://localhost)
	// can call /api/admin/* endpoints directly from the frontend.
	corsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, r)
	})
	httpSrv := &http.Server{
		Addr:         DaemonHTTPAddr,
		Handler:      corsHandler,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 0, // SSE streams can be indefinite
		IdleTimeout:  120 * time.Second,
	}

	fmt.Fprintf(os.Stderr, "synapses %s singleton daemon starting on %s\n", version, DaemonHTTPAddr)

	// ── Background self-update check (every 6 hours, silent) ─────────────────
	startSelfUpdateLoop(appCtx)

	// ── Signal handling ──────────────────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\nsynapses daemon: received %s, shutting down\n", sig)
		appCancel()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		httpSrv.Shutdown(shutCtx) //nolint:errcheck
	}()

	// ── Start HTTP server ─────────────────────────────────────────────────────
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	fmt.Fprintf(os.Stderr, "synapses daemon: stopped\n")
	return nil
}

// ── initProjectInstance: bootstrap one project in the singleton daemon ────────

// initProjectInstance loads/builds all resources for one project and starts
// its per-project Unix socket listener (for stdio proxy backward compat)
// and registers an HTTP MCP handler for the project.
func initProjectInstance(appCtx context.Context, absPath string, sharedPulse *pulse.Client) (*ProjectInstance, error) {
	projCtx, projCancel := context.WithCancel(appCtx)

	cfgDir, found := config.FindConfigDir(absPath)
	if found && cfgDir != absPath {
		fmt.Fprintf(os.Stderr, "synapses [%s]: using config from %s\n", projectHash(absPath), cfgDir)
	}
	cfg, err := config.Load(cfgDir)
	if err != nil {
		projCancel()
		return nil, fmt.Errorf("load config: %w", err)
	}

	dbPath, err := store.DefaultPath(absPath)
	if err != nil {
		projCancel()
		return nil, err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		projCancel()
		return nil, fmt.Errorf("open store: %w", err)
	}
	go st.PruneStaleData(30)

	g, err := loadOrBuildGraphWithStore(absPath, st, false, cfg.Plugins)
	if err != nil {
		st.Close()
		projCancel()
		return nil, err
	}

	// Federation.
	for _, linkedPath := range cfg.Linked {
		if mergeErr := mergeLinkedProject(g, linkedPath); mergeErr != nil {
			fmt.Fprintf(os.Stderr, "synapses [%s]: skipping linked project %s: %v\n",
				projectHash(absPath), linkedPath, mergeErr)
		}
	}
	if len(cfg.Linked) > 0 {
		if sites, err := st.LoadCallSites(); err == nil && len(sites) > 0 {
			for _, cs := range sites {
				g.AddCallSite(cs)
			}
			if n := resolver.ResolveCallEdges(g); n > 0 {
				fmt.Fprintf(os.Stderr, "synapses [%s]: resolved %d cross-project CALLS edges\n",
					projectHash(absPath), n)
			}
		}
	}

	applyGoTypesIfEnabled(g, absPath, cfg)
	applyTSTypesIfEnabled(g, absPath, cfg)
	enrichMetricsIfEnabled(g, absPath, cfg)
	analyzeDataFlowIfEnabled(g, cfg)

	// Context file (auto-injected session context).
	writeCtxFile := func() {
		identity := g.ProjectIdentity()
		var tasks []store.Task
		if pending, err := st.GetPendingTasks("", ""); err == nil {
			tasks = pending
		}
		_ = contextfile.Write(absPath, identity, tasks)
	}
	go writeCtxFile()
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				writeCtxFile()
			case <-projCtx.Done():
				return
			}
		}
	}()

	// Background index rebuild.
	go func() {
		blob, err := g.RebuildIndex()
		if err == nil && len(blob) > 0 {
			_ = st.SaveIndexSnapshot(blob)
		}
	}()
	// Background idle-defrag.
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				idx := g.Index()
				if idx == nil || !idx.Ready() || idx.TombstoneRatio() < 0.15 {
					continue
				}
				blob, err := g.RebuildIndex()
				if err == nil && len(blob) > 0 {
					_ = st.SaveIndexSnapshot(blob)
				}
			case <-projCtx.Done():
				return
			}
		}
	}()

	// MCP server.
	mcpsrv.Version = version
	srv := mcpsrv.New(g, cfg, st)
	srv.SetProjectID(pathProjectID(absPath))
	srv.StartBackground()

	// Skills / prompts.
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
		allPrompts = skills.DeduplicatePrompts(allPrompts)
		if len(allPrompts) > 0 {
			srv.SetPromptTemplates(allPrompts)
		}
	}
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
		allRecipes = skills.DeduplicateRecipes(allRecipes)
		srv.SetSkillRecipes(allRecipes)
	}

	// Federation resolver: cross-project drift detection + dependency tracking.
	// Only created when federation entries are configured in synapses.json.
	// The dependency tracker is wired into the watcher later (after watcher creation).
	var fedResolver *federation.Resolver
	if len(cfg.Federation) > 0 {
		fedResolver = federation.NewResolver(cfg.Federation, cfgDir)
		srv.SetFederationResolver(fedResolver)
		go func() {
			<-projCtx.Done()
			fedResolver.Close()
		}()
	}

	// Brain.
	brainCli := brain.NewInProcess(cfg.Brain.ToBrainConfig())
	if cfg.Brain.Enabled {
		srv.SetBrainClient(brainCli)
		if fedResolver != nil {
			fedResolver.SetBrain(brainCli)
			fedResolver.SetBrainGenerate(brainCli.Generate)
		}
		go func() {
			go fetchTopNSummaries(projCtx, brainCli, g, st, 20)
			bulkIngestToBrain(brainCli, g, pathProjectID(absPath))
			fetchAndWriteBackSummaries(brainCli, g, st)
		}()
	}

	// Web doc cache: version-pinned Go package docs, cached locally in SQLite.
	wc := webcache.New(st)
	srv.SetWebCache(wc)
	srv.SetProjectPath(absPath)
	go webcache.IndexProjectImports(projCtx, absPath, g, wc, 20)

	// Tech stack detection (no longer enriched via scout).
	go func() {
		entries := scout.DetectTechStack(absPath)
		if len(entries) > 0 {
			srv.SetTechStack(entries)
		}
	}()

	// Pulse.
	if sharedPulse != nil {
		srv.SetPulseClient(sharedPulse)
	}

	// Embeddings.
	if cfg.EmbeddingEndpoint != "" {
		embedCli := embed.NewClient(cfg.EmbeddingEndpoint, "")
		srv.SetEmbedClient(embedCli)
		go embedAllNodes(projCtx, embedCli, g, st)
	}

	// File watcher.
	var fw *watcher.Watcher
	w := parser.NewWalker()
	for _, p := range cfg.Plugins {
		w.RegisterPlugin(p.Extensions, p.Command)
	}
	fw2, err := watcher.New(g, w, st)
	if err == nil {
		if startErr := fw2.Start(absPath); startErr == nil {
			fw = fw2
			fw.SetConfig(cfg)
			fw.SetProjectID(pathProjectID(absPath))
			srv.SetChangeSource(fw)
			fw.SetPacketInvalidator(srv)
			fw.SetBrainClient(brainCli)
			fw.SetConfigChangeHandler(func(newCfg *config.Config) {
				newBrain := brain.NewInProcess(newCfg.Brain.ToBrainConfig())
				srv.SetBrainClient(newBrain)
				fw.SetBrainClient(newBrain)
			})
			// Wire federation dependency tracker into the watcher so
			// cross-project imports are detected on every file re-parse.
			if fedResolver != nil {
				tracker := federation.NewDeterministicDetector(cfg.Federation, fedResolver)
				fw.SetCrossProjectTracker(tracker)
				// Tier 2: brain-enhanced cross-project detection for languages
				// Tier 1 doesn't cover well (Python, Ruby, Java, etc.).
				if cfg.Brain.Enabled {
					aliases := fedResolver.Aliases()
					brainDet := federation.NewBrainDetector(brainCli.Generate, fedResolver, aliases)
					brainAdapter := federation.NewBrainTrackerAdapter(brainDet, tracker)
					if brainAdapter != nil {
						fw.SetBrainCrossProjectTracker(brainAdapter)
					}
				}
			}
		}
	}

	// HTTP MCP handler for this project (used via /mcp?project=<path>).
	httpHandler := mcpserver.NewStreamableHTTPServer(srv.MCPServer())

	// Per-project Unix socket (for stdio proxy backward compat).
	sockPath, sockErr := daemonSocketPath(absPath)
	if sockErr == nil {
		os.Remove(sockPath) // clean stale socket
		listener, listenErr := net.Listen("unix", sockPath)
		if listenErr == nil {
			os.Chmod(sockPath, 0o700) //nolint:errcheck
			go serveProjectSocket(projCtx, srv, listener)
		}
	}

	identity := g.ProjectIdentity()
	fmt.Fprintf(os.Stderr, "synapses: project ready — %s (%d nodes, %d edges)\n",
		identity.RepoID,
		identity.Summary.Files+identity.Summary.Functions+
			identity.Summary.Structs+identity.Summary.Interfaces,
		identity.Summary.Edges)

	return &ProjectInstance{
		AbsPath:     absPath,
		Graph:       g,
		Store:       st,
		MCPServer:   srv,
		HTTPHandler: httpHandler,
		BrainClient: brainCli,
		Watcher:     fw,
		cancel:      projCancel,
	}, nil
}

// serveProjectSocket accepts MCP sessions on the per-project Unix socket.
// This provides backward compatibility for "synapses start" stdio proxies.
func serveProjectSocket(ctx context.Context, srv *mcpsrv.Server, listener net.Listener) {
	defer func() {
		listener.Close()
		// Remove the socket file on exit.
		if ul, ok := listener.(*net.UnixListener); ok {
			_ = ul.Close()
			// Retrieve socket path via the addr.
			if addr := listener.Addr(); addr != nil {
				os.Remove(addr.String())
			}
		}
	}()

	// Stop accepting when context is cancelled.
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	var connID atomic.Int64
	var wg sync.WaitGroup

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			continue
		}

		id := connID.Add(1)
		sessionID := fmt.Sprintf("sock-%d", id)
		wg.Add(1)
		go func(c net.Conn, sid string) {
			defer wg.Done()
			defer c.Close()
			if err := serveMCPConn(ctx, srv.MCPServer(), srv, c, sid); err != nil {
				if !strings.Contains(err.Error(), "EOF") &&
					!strings.Contains(err.Error(), "use of closed") {
					fmt.Fprintf(os.Stderr, "synapses socket session %s error: %v\n", sid, err)
				}
			}
		}(conn, sessionID)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// serveMCPConn handles one MCP session over a Unix socket connection.
func serveMCPConn(ctx context.Context, mcpSrv *mcpserver.MCPServer, synSrv *mcpsrv.Server, conn net.Conn, sessionID string) error {
	session := &connSession{
		id:            sessionID,
		notifications: make(chan mcp.JSONRPCNotification, 100),
	}

	if err := mcpSrv.RegisterSession(ctx, session); err != nil {
		return fmt.Errorf("register session: %w", err)
	}
	defer mcpSrv.UnregisterSession(ctx, sessionID)
	defer synSrv.ClearSessionHashes(sessionID)

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sessionCtx = mcpsrv.WithSessionID(sessionCtx, sessionID)
	sessionCtx = mcpSrv.WithContext(sessionCtx, session)

	var writeMu sync.Mutex
	writeJSON := func(v interface{}) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		writeMu.Lock()
		_, err = conn.Write(data)
		writeMu.Unlock()
		return err
	}

	go func() {
		for {
			select {
			case notif := <-session.notifications:
				if err := writeJSON(notif); err != nil {
					return
				}
			case <-sessionCtx.Done():
				return
			}
		}
	}()

	reader := bufio.NewReader(conn)
	for {
		if sessionCtx.Err() != nil {
			return sessionCtx.Err()
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rawMsg json.RawMessage
		if err := json.Unmarshal([]byte(line), &rawMsg); err != nil {
			writeJSON(map[string]interface{}{
				"jsonrpc": "2.0", "id": nil,
				"error": map[string]interface{}{"code": -32700, "message": "Parse error"},
			}) //nolint:errcheck
			continue
		}
		response := mcpSrv.HandleMessage(sessionCtx, rawMsg)
		if response != nil {
			if err := writeJSON(response); err != nil {
				return err
			}
		}
	}
}
