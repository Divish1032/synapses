// daemon_serve.go — "synapses daemon serve" subcommand.
//
// Runs the Synapses MCP server as a long-lived daemon process. Instead of
// communicating over stdio (like the old cmdStart), the daemon listens on a
// Unix domain socket and accepts multiple concurrent MCP proxy connections.
//
// Architecture:
//
//	┌─────────────┐     ┌─────────────┐
//	│ Claude Code  │     │   Cursor    │
//	└──────┬───────┘     └──────┬──────┘
//	       │ stdio              │ stdio
//	       ▼                    ▼
//	┌────────────┐     ┌────────────┐
//	│  mcp proxy │     │  mcp proxy │   ← disposable, stateless
//	└──────┬─────┘     └──────┬─────┘
//	       └── Unix socket ───┘
//	                │
//	     ┌──────────▼────────────┐
//	     │   synapses daemon     │   ← one per project, owns DB + watcher
//	     └───────────────────────┘
//
// Socket path: ~/.synapses/daemons/<sha256(absPath)[:16]>.sock
// PID file:    ~/.synapses/daemons/<sha256(absPath)[:16]>.pid
// Version:     ~/.synapses/daemons/<sha256(absPath)[:16]>.version
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"net"
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
	"github.com/SynapsesOS/synapses/internal/embed"
	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/skills"
	"github.com/SynapsesOS/synapses/internal/peer"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/scout"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/SynapsesOS/synapses/internal/watcher"
)

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

// ── path helpers for daemon socket/pid/version files ─────────────────────────

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

func daemonSocketPath(absPath string) (string, error) {
	dir, err := daemonDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, projectHash(absPath)+".sock"), nil
}

func daemonPIDPath(absPath string) (string, error) {
	dir, err := daemonDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, projectHash(absPath)+".pid"), nil
}

func daemonVersionPath(absPath string) (string, error) {
	dir, err := daemonDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, projectHash(absPath)+".version"), nil
}

// isDaemonRunning tries to connect to the daemon socket. Returns true if the
// daemon is alive and accepting connections.
func isDaemonRunning(absPath string) bool {
	sockPath, err := daemonSocketPath(absPath)
	if err != nil {
		return false
	}
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// cleanStaleDaemonFiles removes socket and PID files if the daemon is not
// actually running. Called on startup to recover from hard crashes/reboots.
func cleanStaleDaemonFiles(absPath string) {
	sockPath, _ := daemonSocketPath(absPath)
	pidPath, _ := daemonPIDPath(absPath)
	verPath, _ := daemonVersionPath(absPath)

	if sockPath == "" {
		return
	}

	// If socket file exists but not connectable, it's stale.
	if _, err := os.Stat(sockPath); err == nil {
		conn, err := net.DialTimeout("unix", sockPath, 1*time.Second)
		if err != nil {
			// Stale socket — clean up.
			os.Remove(sockPath)
			os.Remove(pidPath)
			os.Remove(verPath)
		} else {
			conn.Close()
		}
	}
}

// ── connSession: per-connection MCP session ──────────────────────────────────

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

// ── connection tracker for idle timeout ──────────────────────────────────────

type connectionTracker struct {
	mu          sync.Mutex
	count       int
	lastDisconn time.Time
}

func (ct *connectionTracker) add() {
	ct.mu.Lock()
	ct.count++
	ct.mu.Unlock()
}

func (ct *connectionTracker) remove() {
	ct.mu.Lock()
	ct.count--
	if ct.count <= 0 {
		ct.count = 0
		ct.lastDisconn = time.Now()
	}
	ct.mu.Unlock()
}

func (ct *connectionTracker) idleSince() (time.Time, bool) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.count > 0 {
		return time.Time{}, false
	}
	return ct.lastDisconn, true
}

func (ct *connectionTracker) active() int {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.count
}

// ── cmdDaemonServe: the daemon entry point ───────────────────────────────────

func cmdDaemonServe(args []string) error {
	fs := flag.NewFlagSet("daemon-serve", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Path to the repository root")
	forceReindex := fs.Bool("reindex", false, "Force a full re-index even if cache is fresh")
	noWatch := fs.Bool("no-watch", false, "Disable the file watcher")
	idleTimeout := fs.Duration("idle-timeout", 30*time.Minute, "Shut down after this duration with no connections (0 = never)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	absPath, err := canonicalPath(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	// ── Singleton check: is another daemon already running for this project? ─
	cleanStaleDaemonFiles(absPath)
	if isDaemonRunning(absPath) {
		return fmt.Errorf("daemon already running for %s", absPath)
	}

	sockPath, err := daemonSocketPath(absPath)
	if err != nil {
		return fmt.Errorf("socket path: %w", err)
	}
	pidPath, err := daemonPIDPath(absPath)
	if err != nil {
		return fmt.Errorf("pid path: %w", err)
	}
	verPath, err := daemonVersionPath(absPath)
	if err != nil {
		return fmt.Errorf("version path: %w", err)
	}

	// Write PID file immediately so proxies know we're starting.
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return fmt.Errorf("write pid: %w", err)
	}
	defer os.Remove(pidPath)

	// Write version file so proxies can detect binary upgrades.
	if err := os.WriteFile(verPath, []byte(version), 0o600); err != nil {
		return fmt.Errorf("write version: %w", err)
	}
	defer os.Remove(verPath)

	// ── Context for graceful shutdown ────────────────────────────────────────
	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	// ── Heavy initialization (same as old cmdStart) ─────────────────────────
	cfgDir, found := config.FindConfigDir(absPath)
	if found && cfgDir != absPath {
		fmt.Fprintf(os.Stderr, "synapses: using config from %s\n", cfgDir)
	}
	cfg, err := config.Load(cfgDir)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
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

	go st.PruneStaleData(30)

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

	applyGoTypesIfEnabled(g, absPath, cfg)
	applyTSTypesIfEnabled(g, absPath, cfg)
	enrichMetricsIfEnabled(g, absPath, cfg)
	analyzeDataFlowIfEnabled(g, cfg)

	go func() {
		blob, err := g.RebuildIndex()
		if err == nil && len(blob) > 0 {
			_ = st.SaveIndexSnapshot(blob)
		}
	}()

	// Background idle-defrag goroutine.
	go func() {
		const (
			checkInterval  = 60 * time.Second
			tombstoneLimit = 0.15
		)
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				idx := g.Index()
				if idx == nil || !idx.Ready() {
					continue
				}
				if idx.TombstoneRatio() < tombstoneLimit {
					continue
				}
				blob, err := g.RebuildIndex()
				if err == nil && len(blob) > 0 {
					_ = st.SaveIndexSnapshot(blob)
					fmt.Fprintf(os.Stderr, "synapses: idle defrag complete (tombstone ratio was %.0f%%)\n",
						idx.TombstoneRatio()*100)
				}
			case <-appCtx.Done():
				return
			}
		}
	}()

	// Create the MCP server.
	mcpsrv.Version = version
	srv := mcpsrv.New(g, cfg, st)

	// Load activation-context prompts from all scopes (fail-silent per scope).
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
		if len(allPrompts) > 0 {
			srv.SetPromptTemplates(allPrompts)
			fmt.Fprintf(os.Stderr, "synapses: loaded %d activation-context prompts\n", len(allPrompts))
		}
	}

	// Load skill recipes from all scopes (fail-silent per scope).
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
		srv.SetSkillRecipes(allRecipes)
		fmt.Fprintf(os.Stderr, "synapses: loaded %d skill recipes\n", len(allRecipes))
	}

	// Peer API server.
	if cfg.PeerAPIPort > 0 {
		peerSrv := peer.NewPeerServer(g, cfg, st)
		if err := peerSrv.Start(cfg.PeerAPIPort); err != nil {
			fmt.Fprintf(os.Stderr, "synapses: peer API: %v\n", err)
		} else {
			defer peerSrv.Stop()
			fmt.Fprintf(os.Stderr, "synapses: peer API listening on :%d\n", cfg.PeerAPIPort)
		}
	}
	if len(cfg.Peers) > 0 {
		pm := peer.NewPeerManager(cfg, g, st)
		pm.Connect()
		pm.StartHealthMonitor(30 * time.Second)
		defer pm.Stop()
		srv.SetPeerManager(pm)
	}

	// Brain.
	var brainCli *brain.Client
	if cfg.Brain.URL != "" {
		brainCli = brain.NewClient(cfg.Brain.URL, cfg.Brain.TimeoutSec)
		if model, err := brainCli.HealthCheck(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "synapses: brain unreachable at %s: %v (continuing without — will retry)\n", cfg.Brain.URL, err)
			brainCli = nil
			// Retry brain connection in background every 15s until it becomes available.
			go retryBrainConnect(appCtx, cfg, srv, g, st)
		} else {
			fmt.Fprintf(os.Stderr, "synapses: brain connected (%s)\n", model)
			srv.SetBrainClient(brainCli)
			go func() {
				bulkIngestToBrain(brainCli, g)
				fetchAndWriteBackSummaries(brainCli, g, st)
			}()
		}
	}

	// Scout.
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

	// Pulse.
	if cfg.Pulse.URL != "" {
		pulseCli := pulse.NewClient(cfg.Pulse.URL, cfg.Pulse.TimeoutSec)
		srv.SetPulseClient(pulseCli)
		fmt.Fprintf(os.Stderr, "synapses: pulse analytics enabled at %s\n", cfg.Pulse.URL)
	}

	// Embeddings.
	{
		var embedCli *embed.Client
		if brainCli != nil {
			candidate := embed.NewBrainClient(cfg.Brain.URL)
			probeCtx, probeCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer probeCancel()
			if _, err := candidate.Embed(probeCtx, "probe"); err == nil {
				embedCli = candidate
				fmt.Fprintf(os.Stderr, "synapses: embeddings via brain /v1/embed\n")
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

	// Tech stack autosubscribe.
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

	// File watcher.
	if !*noWatch {
		w := parser.NewWalker()
		for _, p := range cfg.Plugins {
			w.RegisterPlugin(p.Extensions, p.Command)
		}
		fw, err := watcher.New(g, w, st)
		if err != nil {
			fmt.Fprintf(os.Stderr, "synapses: file watcher unavailable: %v\n", err)
		} else {
			if err := fw.Start(absPath); err != nil {
				fmt.Fprintf(os.Stderr, "synapses: file watcher start failed: %v\n", err)
			} else {
				defer fw.Stop()
				fw.SetConfig(cfg)
				srv.SetChangeSource(fw)
				fw.SetPacketInvalidator(srv)
				if brainCli != nil {
					fw.SetBrainClient(brainCli)
				}
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

	// ── Unix socket listener ─────────────────────────────────────────────────
	// Remove any stale socket before binding.
	os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", sockPath, err)
	}
	defer func() {
		listener.Close()
		os.Remove(sockPath)
	}()

	// Set socket permissions so only the owner can connect.
	os.Chmod(sockPath, 0o700)

	identity := g.ProjectIdentity()
	fmt.Fprintf(os.Stderr, "synapses %s ready — %d nodes, %d edges (repo: %s)\n",
		version,
		identity.Summary.Files+identity.Summary.Functions+
			identity.Summary.Structs+identity.Summary.Interfaces,
		identity.Summary.Edges, identity.RepoID)
	fmt.Fprintf(os.Stderr, "daemon listening on %s\n", sockPath)

	// ── Signal handling ──────────────────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\nsynapses daemon: received %s, shutting down\n", sig)
		appCancel()
		listener.Close() // unblock Accept
		time.AfterFunc(10*time.Second, func() {
			fmt.Fprintf(os.Stderr, "synapses daemon: graceful shutdown timed out, forcing exit\n")
			os.Exit(1)
		})
	}()

	// ── Idle timeout goroutine ───────────────────────────────────────────────
	tracker := &connectionTracker{}
	if *idleTimeout > 0 {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if since, idle := tracker.idleSince(); idle && !since.IsZero() {
						if time.Since(since) >= *idleTimeout {
							fmt.Fprintf(os.Stderr, "synapses daemon: idle for %s, shutting down\n", *idleTimeout)
							appCancel()
							listener.Close()
							return
						}
					}
				case <-appCtx.Done():
					return
				}
			}
		}()
	}

	// ── Accept loop ──────────────────────────────────────────────────────────
	var connID atomic.Int64
	var wg sync.WaitGroup

	for {
		conn, err := listener.Accept()
		if err != nil {
			if appCtx.Err() != nil {
				break // shutting down
			}
			fmt.Fprintf(os.Stderr, "synapses daemon: accept error: %v\n", err)
			continue
		}

		id := connID.Add(1)
		sessionID := fmt.Sprintf("daemon-%d", id)
		tracker.add()
		wg.Add(1)

		go func(c net.Conn, sid string) {
			defer wg.Done()
			defer c.Close()
			defer tracker.remove()

			fmt.Fprintf(os.Stderr, "synapses daemon: session %s connected\n", sid)
			if err := serveMCPConn(appCtx, srv.MCPServer(), c, sid); err != nil {
				// EOF is normal — proxy disconnected.
				if !strings.Contains(err.Error(), "EOF") &&
					!strings.Contains(err.Error(), "use of closed") {
					fmt.Fprintf(os.Stderr, "synapses daemon: session %s error: %v\n", sid, err)
				}
			}
			fmt.Fprintf(os.Stderr, "synapses daemon: session %s disconnected (%d active)\n", sid, tracker.active())
		}(conn, sessionID)
	}

	// Wait for active sessions to finish (up to 5s).
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		fmt.Fprintf(os.Stderr, "synapses daemon: timed out waiting for sessions to close\n")
	}

	fmt.Fprintf(os.Stderr, "synapses daemon: stopped\n")
	return nil
}

// serveMCPConn handles one MCP session over a Unix socket connection.
// It registers a unique session with the MCPServer, processes JSON-RPC
// messages, and forwards notifications to the client.
func serveMCPConn(ctx context.Context, mcpSrv *mcpserver.MCPServer, conn net.Conn, sessionID string) error {
	session := &connSession{
		id:            sessionID,
		notifications: make(chan mcp.JSONRPCNotification, 100),
	}

	if err := mcpSrv.RegisterSession(ctx, session); err != nil {
		return fmt.Errorf("register session: %w", err)
	}
	defer mcpSrv.UnregisterSession(ctx, sessionID)

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sessionCtx = mcpSrv.WithContext(sessionCtx, session)

	// writeMu protects conn.Write from concurrent access. Both the
	// notification goroutine and the message handler write responses;
	// without this mutex, large writes could interleave and corrupt JSON.
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

	// Forward notifications to the connection.
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

	// Process incoming JSON-RPC messages.
	reader := bufio.NewReader(conn)
	for {
		if sessionCtx.Err() != nil {
			return sessionCtx.Err()
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			return err // EOF = proxy disconnected
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var rawMsg json.RawMessage
		if err := json.Unmarshal([]byte(line), &rawMsg); err != nil {
			writeJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      nil,
				"error": map[string]interface{}{
					"code":    -32700,
					"message": "Parse error",
				},
			})
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
