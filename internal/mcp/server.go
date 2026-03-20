// Package mcp implements the Model Context Protocol server for Synapses.
// Agents (Claude Code, Cursor, etc.) connect to this server over stdio and
// call the registered tools to query the code graph.
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/skills"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/SynapsesOS/synapses/internal/watcher"
	"github.com/SynapsesOS/synapses/internal/webcache"
)

// loadAppSettingsJSON reads ~/.synapses/app_settings.json and returns the
// decoded map. Returns an error (and nil map) if the file is absent or invalid.
func loadAppSettingsJSON() (map[string]interface{}, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(home, ".synapses", "app_settings.json"))
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// sessionContextKey is an unexported type for context values to avoid collisions
// with other packages that also store values in context.
type sessionContextKey int

const sessionIDCtxKey sessionContextKey = iota

// WithSessionID stores a MCP session ID in ctx. Called during daemon connection
// setup so all tool handlers can read the session ID via SessionIDFromContext.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDCtxKey, sessionID)
}

// SessionIDFromContext retrieves the session ID injected by WithSessionID.
// Returns "" when no session ID was injected (stdio path, tests).
func SessionIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(sessionIDCtxKey).(string)
	return v
}

// ChangeSource is implemented by types that maintain a recent file-change log.
// Typically this is *watcher.Watcher, wired in cmdStart via SetChangeSource.
type ChangeSource interface {
	RecentChanges(windowMinutes int) []watcher.ChangeEvent
}

// ProjectStoreProvider gives access to a sibling project's store for cross-project queries.
// Implemented by the daemon's project registry; nil in single-project (stdio) mode.
type ProjectStoreProvider interface {
	// ListProjects returns human-readable names of all registered projects.
	ListProjects() []string
	// GetStore returns the store for the named project, or nil if not found.
	// The name is the directory basename (e.g. "backend" for /Users/x/code/backend).
	GetStore(name string) *store.Store
}

const serverName = "synapses"

// Version is the server version advertised to MCP clients.
// Set from main.go via ldflags-injected version before creating the server.
var Version = "dev"

// packetCacheEntry holds a cached context packet with an expiry time.
type packetCacheEntry struct {
	pkt       interface{} // *brain.ContextPacket (typed as interface{} to avoid import cycle)
	expiresAt time.Time
}

// Server holds the MCP server and the dependencies that tool handlers need.
type Server struct {
	mcp          *server.MCPServer
	graph        *graph.Graph
	config       *config.Config
	store        *store.Store  // nil if started without a persistent store
	changeSource ChangeSource  // nil if started without a file watcher
	federationResolver  *federation.Resolver   // nil if no federation configured — set via SetFederationResolver
	projectRegistry     ProjectStoreProvider   // nil in single-project mode — set via SetProjectRegistry
	brainClient  interface{}   // *brain.Client — set via SetBrainClient; nil if brain not configured
	webCache     *webcache.Cache // nil if webcache not configured
	pulseClient  interface{}    // *pulse.Client — set via SetPulseClient; nil if pulse not configured
	embedClient    *embed.Client  // nil if embedding_endpoint not configured
	memoryEmbedder embed.Embedder // nil if embeddings mode is "off" — set via SetMemoryEmbedder
	techStack    interface{}    // []TechStackEntry — set via SetTechStack after autosubscribe
	injectionScanner *InjectionScanner // prompt injection scanner for externally-sourced content (nil = disabled)
	knowledgeMode bool          // when true, only knowledge tools are registered (no code graph)
	projectID    string         // stable project identifier (FNV hash of project root path)
	projectPath  string         // absolute path to the project root (for go.mod parsing)
	rulesMu      sync.RWMutex  // protects s.config.Rules for concurrent dynamic upserts

	// appSettings mirrors relevant fields from ~/.synapses/app_settings.json.
	// Loaded once at startup. When false, the corresponding data collection is skipped.
	logToolCalls     bool // controls RecordToolCall recording (default: true)
	logSessions      bool // controls pulse session tracking (default: true)
	cacheWebSearches bool // controls web_cache inserts (default: true)

	// Context-packet cache: 20 slots max, 30s TTL. Keyed by "entityName:depth".
	packetCacheMu sync.RWMutex
	packetCache   map[string]*packetCacheEntry

	// Brain cache warming debounce: prevents hammering the brain on rapid saves.
	warmMu   sync.Mutex
	lastWarm time.Time

	// GAP-1: Feedback loop — track get_context call counts per (agentID, entity)
	// within a session. When the same agent calls get_context ≥3 times for the
	// same entity, we auto-record a "context_repeated" episode as a signal that
	// the initial context slice wasn't sufficient. Entries expire after 30m.
	ctxCallMu     sync.Mutex
	ctxCalls      map[string]*ctxCallEntry
	ctxCallLastGC time.Time // tracks when we last ran GC on ctxCalls (R29 GAP3)

	// lastAgentID is the agent_id from the most recent session_init call.
	// Used as a fallback when individual tool calls don't include agent_id,
	// ensuring Pulse can attribute token savings to the correct agent.
	lastAgentMu sync.RWMutex
	lastAgentID string

	// promptTemplates holds activation-context prompts loaded at startup.
	// They are auto-injected into get_context and session_init responses
	// when their patterns (file, entity, module) match the queried entity.
	// Populated via SetPromptTemplates after the server is constructed.
	promptTemplates []skills.PromptTemplate

	// skillRecipes holds named multi-step workflow definitions.
	// Populated via SetSkillRecipes after the server is constructed.
	skillRecipes  []skills.Recipe
	skillExecutor *skills.Executor

	// sessionHashes auto-caches entity_hash per session to allow the server to
	// detect unchanged context without requiring agents to pass known_hash manually.
	// Key format: "sessionID::entityCacheKey", Value: entity_hash string.
	// sync.Map gives safe concurrent reads/writes with no lock contention.
	// Entries are cleared by ClearSessionHashes when a session disconnects.
	sessionHashes sync.Map

	// RX1: per-connection call counters for auto end_session detection.
	// Key: "sessionID::agentID", Value: *sessionCallEntry.
	// Protected by sessionCallsMu. Entries removed when auto-log fires or
	// agent calls end_session explicitly.
	sessionCallsMu sync.Mutex
	sessionCalls   map[string]*sessionCallEntry

	// Session Intelligence (Layer 1): maps MCP connection session ID → Synapses session UUID.
	// Created on session_init(); used to attribute every subsequent tool call to a session.
	// Key: MCP sessionID (from SessionIDFromContext) or "stdio" for single-connection mode.
	// Protected by synapseSessionsMu.
	synapseSessionsMu sync.RWMutex
	synapsesSessions  map[string]*synapseSessionEntry

	// startTime records when the server was constructed, used by session_init
	// to report daemon uptime in the daemon_health field (IMP-EVAL-3).
	startTime time.Time

	// stopCh is closed by Close() to signal background goroutines to exit.
	// startOnce ensures StartBackground() is truly idempotent.
	// wg tracks all goroutines started by StartBackground so Close() can
	// wait for them to finish before returning.
	stopCh    chan struct{}
	startOnce sync.Once
	wg        sync.WaitGroup

	// Bounded background worker pool for fire-and-forget operations.
	// All handler goroutines (telemetry, embedding, store writes) go through
	// goBackground() which enqueues work items. Fixed workers drain the queue.
	// Close() rejects new work, closes the queue, and waits for workers to
	// drain remaining items — preventing goroutines from racing with Store.Close().
	bgQueue    chan func()   // buffered work queue (cap bgQueueCap)
	shutdownMu sync.RWMutex // guards bgClosed + bgQueue sends
	bgClosed   bool         // true after Close() — rejects new work

	// toolHandlers is the dispatch table for the REST API (POST /v1/tools/{name}).
	// Populated in addOrDefer alongside mcp-go registration so REST and MCP share
	// the exact same handler functions. In knowledge mode, graph-tool entries hold
	// the knowledge-mode stub (same as what mcp-go has).
	toolHandlersMu sync.RWMutex
	toolHandlers   map[string]server.ToolHandlerFunc

	// orientMu protects the orientation result cache (explain_codebase, get_repo_map).
	// Stored separately from the 20-slot packet cache so that heavy get_context
	// traffic cannot evict orientation results. Invalidated whenever the graph
	// structure changes (see InvalidatePacketCacheForFile in resources.go).
	orientMu          sync.RWMutex
	orientExplain     *string // nil = not yet computed or invalidated
	orientRepoCompact *string
	orientRepoFull    *string

	// B28: repo scale (micro/small/medium/large) computed from node count.
	// Used by coreTierTools/standardTierTools for discover_tools status labels.
	repoScale graph.Scale

	// OF-S5: per-session loop guard. Tracks fingerprints of the last 20 calls
	// per session. Warns at 3 identical calls, trips circuit breaker at 5.
	// Resets on every file-change event.
	lg *loopGuard

	// Security F10: per-session token-bucket rate limiter for write operations,
	// expensive reads (recall), and cross-project queries. Configured via
	// synapses.json "rate_limits". Cleared on session close.
	rl *rateLimiter

	// Phase 6: component health tracker for prepare_context pipeline.
	// Components that panic or timeout ≥3 times in a session are auto-disabled.
	// Per-agent scoped — concurrent agents don't interfere.
	componentHealth componentHealthTracker

	// OF-E3: cross-project write approval gates.
	// Stores pending approval tokens for broadcast send_message and
	// cross-project remember operations. Tokens expire after 5 minutes.
	approvals *approvalStore

	// OF-S4: tool description integrity.
	// toolDescs captures name → description at addOrDefer time.
	// toolDescBaseline is the SHA256 hex of the sorted descriptions, computed
	// once at the end of registerTools(). handleSessionInit re-derives the hash
	// from toolDescs and compares it to detect runtime tampering.
	toolDescs        map[string]string
	toolDescBaseline string
}

const (
	// bgWorkers is the number of fixed worker goroutines processing the
	// background queue. 8 is sufficient — most work items are fast SQLite
	// writes serialized by WAL anyway; brain/pulse HTTP calls are IO-bound
	// and benefit from concurrency.
	bgWorkers = 8

	// bgQueueCap is the buffer size for the background work queue.
	// Absorbs burst traffic (~50 concurrent tool calls × 5 background ops).
	// When full, new work is dropped with a stderr warning (back-pressure).
	bgQueueCap = 256
)

// goBackground enqueues fn for execution by the bounded worker pool.
// Safe to call from any handler goroutine. Work is silently dropped after
// Close() is called or when the queue is full (back-pressure).
func (s *Server) goBackground(fn func()) {
	s.shutdownMu.RLock()
	if s.bgClosed {
		s.shutdownMu.RUnlock()
		return
	}
	select {
	case s.bgQueue <- fn:
		// queued successfully
	default:
		logutil.Warn("synapses: background queue full (%d), dropping work\n", bgQueueCap)
	}
	s.shutdownMu.RUnlock()
}

// getSessionHash returns the last stored entity_hash for this session+entityKey, or "".
func (s *Server) getSessionHash(sessionID, entityKey string) string {
	if sessionID == "" {
		return ""
	}
	v, ok := s.sessionHashes.Load(sessionID + "::" + entityKey)
	if !ok {
		return ""
	}
	return v.(string)
}

// setSessionHash stores an entity_hash for a session+entityKey pair.
// Called after successfully serving a full get_context response so agents
// can receive {unchanged:true} automatically on the next identical call.
func (s *Server) setSessionHash(sessionID, entityKey, hash string) {
	if sessionID == "" || entityKey == "" || hash == "" {
		return
	}
	s.sessionHashes.Store(sessionID+"::"+entityKey, hash)
}

// ClearSessionHashes removes all cached hashes for a session. Must be called
// when a session disconnects to prevent unbounded memory growth in long-running
// daemons with many short-lived connections.
func (s *Server) ClearSessionHashes(sessionID string) {
	if sessionID == "" {
		return
	}
	prefix := sessionID + "::"
	s.sessionHashes.Range(func(k, _ interface{}) bool {
		if strings.HasPrefix(k.(string), prefix) {
			s.sessionHashes.Delete(k)
		}
		return true
	})
}

// synapseSessionEntry maps an MCP connection to its Synapses session UUID.
// One entry exists per active connection; keyed by MCP sessionID (or "stdio").
type synapseSessionEntry struct {
	id      string // UUID in the sessions table
	agentID string
}

// synapseSessionKey returns the map key for a given MCP session ID.
// Stdio mode passes "" as the MCP session ID; we normalise to "stdio" so the
// map key is never empty (empty key causes ambiguous lookups in daemon mode).
func synapseSessionKey(mcpSessionID string) string {
	if mcpSessionID == "" {
		return "stdio"
	}
	return mcpSessionID
}

// getSynapseSessionID returns the Synapses session UUID for the given MCP
// connection, or "" when no session_init has been called on that connection yet.
func (s *Server) getSynapseSessionID(mcpSessionID string) string {
	s.synapseSessionsMu.RLock()
	entry, ok := s.synapsesSessions[synapseSessionKey(mcpSessionID)]
	s.synapseSessionsMu.RUnlock()
	if !ok {
		return ""
	}
	return entry.id
}

// registerSynapseSession associates a newly created Synapses session with an
// MCP connection. Called from handleSessionInit after CreateSession succeeds.
func (s *Server) registerSynapseSession(mcpSessionID, synapseSessionID, agentID string) {
	s.synapseSessionsMu.Lock()
	s.synapsesSessions[synapseSessionKey(mcpSessionID)] = &synapseSessionEntry{
		id:      synapseSessionID,
		agentID: agentID,
	}
	s.synapseSessionsMu.Unlock()
}

// ClearSynapseSession removes the session mapping for an MCP connection.
// Should be called when a connection closes (daemon) or end_session fires.
// Safe to call with an empty or unknown session ID.
func (s *Server) ClearSynapseSession(mcpSessionID string) {
	key := synapseSessionKey(mcpSessionID)
	s.synapseSessionsMu.Lock()
	delete(s.synapsesSessions, key)
	s.synapseSessionsMu.Unlock()
	// OF-S5: release loop-guard memory for the closed session.
	s.lg.clearSession(key)
	// Security F10: release rate-limiter state for the closed session.
	s.rl.clearSession(key)
}

// ctxCallEntry tracks how many times an agent requested context for an entity.
type ctxCallEntry struct {
	count   int
	firstAt time.Time
}

// setLastAgent records the agent_id from the most recent session_init call.
func (s *Server) setLastAgent(id string) {
	if id == "" {
		return
	}
	s.lastAgentMu.Lock()
	s.lastAgentID = id
	s.lastAgentMu.Unlock()
}

// getLastAgent returns the agent_id from the most recent session_init call, or "".
func (s *Server) getLastAgent() string {
	s.lastAgentMu.RLock()
	defer s.lastAgentMu.RUnlock()
	return s.lastAgentID
}

// New creates a Server wired to the given graph, config, and optional store.
// The store is required for Agent Task Memory tools (create_plan, get_pending_tasks, etc.).
// Pass nil for st if running in a context without persistence (e.g. tests).
// All tools are registered during construction.
func New(g *graph.Graph, cfg *config.Config, st *store.Store) *Server {
	s := &Server{
		graph:            g,
		config:           cfg,
		store:            st,
		packetCache:      make(map[string]*packetCacheEntry, 20),
		sessionCalls:     make(map[string]*sessionCallEntry),
		synapsesSessions: make(map[string]*synapseSessionEntry),
		toolHandlers:     make(map[string]server.ToolHandlerFunc),
		stopCh:           make(chan struct{}),
		bgQueue:          make(chan func(), bgQueueCap),
		logToolCalls:     true, // default on
		logSessions:      true,
		cacheWebSearches: true,
		startTime:        time.Now(),
		lg:               newLoopGuard(),
		approvals:        newApprovalStore(),
		toolDescs:        make(map[string]string),
	}

	// Security F10: build rate limiter from config (or defaults if cfg is nil).
	if cfg != nil {
		s.rl = newRateLimiter(cfg.RateLimits)
	} else {
		s.rl = newRateLimiter(config.RateLimitConfig{})
	}

	// OF-S2: prompt injection scanner for externally-sourced content.
	// Default: enabled with "warn" mode. Configurable via content_safety in synapses.json.
	{
		var csc config.ContentSafetyConfig
		if cfg != nil {
			csc = cfg.ContentSafety
		}
		if csc.ContentSafetyEnabled() {
			s.injectionScanner = NewInjectionScanner(ScanMode(csc.ContentSafetyMode()))
		}
	}

	// Load app-level settings from ~/.synapses/app_settings.json.
	// This file is written by the Synapses desktop app; the daemon reads it
	// once at startup. Missing file or missing keys → defaults (all enabled).
	if appSettingsJSON, err := loadAppSettingsJSON(); err == nil {
		if v, ok := appSettingsJSON["log_tool_calls"]; ok {
			if b, ok := v.(bool); ok {
				s.logToolCalls = b
			}
		}
		if v, ok := appSettingsJSON["log_sessions"]; ok {
			if b, ok := v.(bool); ok {
				s.logSessions = b
			}
		}
		if v, ok := appSettingsJSON["cache_web_searches"]; ok {
			if b, ok := v.(bool); ok {
				s.cacheWebSearches = b
			}
		}
	}

	// Restore dynamic rules persisted from previous sessions. This runs before
	// registerTools() so that validate_plan / get_violations are immediately aware
	// of all previously upserted rules without requiring a daemon restart.
	if st != nil {
		if dynamicRules, err := st.LoadDynamicRules(); err == nil && len(dynamicRules) > 0 {
			cfg.Rules = append(cfg.Rules, dynamicRules...)
		}
	}

	// B28: compute repo scale for scale-aware tool registration.
	// NodeCount == 0 means the graph is not yet indexed → default to full tier
	// so agents always have access to all tools on first run.
	{
		var nc int
		if g != nil {
			nc = g.NodeCount()
		}
		switch {
		case nc == 0:
			s.repoScale = graph.ScaleLarge // not yet indexed — register all tools
		case nc < 100:
			s.repoScale = graph.ScaleMicro
		case nc < 500:
			s.repoScale = graph.ScaleSmall
		case nc < 2000:
			s.repoScale = graph.ScaleMedium
		default:
			s.repoScale = graph.ScaleLarge
		}
	}
	// Usage observability: wire before/after hooks to record every tool call
	// timing and success status into the tool_calls SQLite table.
	hooks := &server.Hooks{}
	startTimes := &callStartTimes{}
	hooks.AddBeforeCallTool(func(_ context.Context, _ any, req *mcp.CallToolRequest) {
		startTimes.push(req, time.Now())
	})
	hooks.AddAfterCallTool(func(ctx context.Context, _ any, req *mcp.CallToolRequest, result *mcp.CallToolResult) {
		elapsed := time.Since(startTimes.pop(req))
		success := result == nil || !result.IsError
		agentID, _ := req.GetArguments()["agent_id"].(string)
		if agentID == "" {
			agentID = s.getLastAgent()
		}
		entity, _ := req.GetArguments()["entity"].(string)
		if entity == "" {
			entity, _ = req.GetArguments()["query"].(string)
		}

		// Session Intelligence: touch heartbeat and record audit row.
		// Both ops are fire-and-forget — never block the response path.
		mcpSessionID := SessionIDFromContext(ctx)
		synapseSessionID := s.getSynapseSessionID(mcpSessionID)
		if s.store != nil && synapseSessionID != "" {
			s.store.TouchSession(synapseSessionID)
			if s.logToolCalls {
				toolName := req.Params.Name
				aid := agentID
				sessID := synapseSessionID
				ent := entity
				ms := elapsed.Milliseconds()
				ok := success
				s.goBackground(func() { s.store.RecordToolCall(toolName, aid, sessID, ent, ms, ok) })
			}
		}

		// Fire-and-forget telemetry to synapses-pulse (if configured).
		if pc := s.getPulseClient(); pc != nil && s.logToolCalls {
			var responseBytes int
			if result != nil && !result.IsError && len(result.Content) > 0 {
				if tc, ok := result.Content[0].(mcp.TextContent); ok {
					responseBytes = len(tc.Text)
				}
			}
			evt := pulse.ToolCallEvent{
				ToolName:      req.Params.Name,
				AgentID:       agentID,
				ProjectID:     s.projectID,
				Entity:        entity,
				DurationMs:    elapsed.Milliseconds(),
				Success:       success,
				ResponseBytes: responseBytes,
			}
			s.goBackground(func() { pc.RecordToolCall(evt) })
		}
		// RX1: track per-connection call depth and auto-trigger session log on threshold.
		// Skip end_session calls themselves to avoid recursion.
		if req.Params.Name != "end_session" && agentID != "" && s.store != nil {
			sessionID := SessionIDFromContext(ctx)
			s.trackSessionCall(sessionID, agentID)
		}
	})

	// Sprint 8 #1: hide deprecated/subsumed tools from tools/list.
	// Tools remain registered (callable) but are filtered from the listing.
	// Power users can still find them via discover_tools.
	hooks.AddAfterListTools(func(_ context.Context, _ any, _ *mcp.ListToolsRequest, result *mcp.ListToolsResult) {
		if result == nil {
			return
		}
		filtered := result.Tools[:0]
		for _, t := range result.Tools {
			if !hiddenTools[t.Name] {
				filtered = append(filtered, t)
			}
		}
		result.Tools = filtered
	})

	s.mcp = server.NewMCPServer(serverName, Version,
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true), // subscribe + listChanged
		server.WithPromptCapabilities(false),         // static prompts; no listChanged notifications
		server.WithHooks(hooks),
	)

	// Knowledge mode detection: explicit config setting.
	// Must be set BEFORE registerTools() so addOrDefer sees knowledgeMode=true
	// and registers stubs for graph-dependent tools.
	if cfg != nil && cfg.Mode == "knowledge" {
		s.knowledgeMode = true
	}

	s.registerTools()
	s.registerResources()
	s.registerPrompts()      // no-op until SetPromptTemplates is called
	s.registerSkillTools()   // no-op until SetSkillRecipes is called
	// OF-S4: compute baseline AFTER all registrations so skill tools are included.
	// handleSessionInit re-derives and compares to detect runtime tampering.
	s.toolDescBaseline = hashToolDescs(s.toolDescs)
	return s
}

// hashToolDescs computes a deterministic SHA256 hex digest of all tool
// name→description pairs. Names are sorted alphabetically before hashing so
// the result is independent of registration order. Called once at startup to
// establish the baseline, and re-called in handleSessionInit to detect drift.
func hashToolDescs(descs map[string]string) string {
	names := make([]string, 0, len(descs))
	for name := range descs {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00", name, descs[name])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// NewKnowledge creates a Server in knowledge mode — memory, events, tasks, and
// messages only, no code graph. Used for non-code domains (marketing, ops, QA).
func NewKnowledge(cfg *config.Config, st *store.Store) *Server {
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Mode = "knowledge"
	return New(nil, cfg, st)
}

// callStartTimes tracks per-tool-call start timestamps keyed by request pointer.
// Each *mcp.CallToolRequest is a unique allocation per invocation, so using the
// pointer as the key is correct for all transports (stdio, HTTP/SSE) including
// concurrent calls to the same tool — no FIFO ordering assumption is needed.
type callStartTimes struct {
	mu   sync.Mutex
	data map[*mcp.CallToolRequest]time.Time
}

// callStartTimesGCThreshold is how long an entry can sit in callStartTimes
// before being considered leaked (from a panic that skipped the after-hook).
// Normal tool calls complete in well under 5 minutes.
const callStartTimesGCThreshold = 5 * time.Minute

func (c *callStartTimes) push(req *mcp.CallToolRequest, t time.Time) {
	c.mu.Lock()
	if c.data == nil {
		c.data = make(map[*mcp.CallToolRequest]time.Time)
	}
	// GC entries that are suspiciously old — these are leaked by panics that
	// caused the after-hook to be skipped. O(n) over concurrent calls (≪100).
	for k, v := range c.data {
		if t.Sub(v) > callStartTimesGCThreshold {
			delete(c.data, k)
		}
	}
	c.data[req] = t
	c.mu.Unlock()
}

func (c *callStartTimes) pop(req *mcp.CallToolRequest) time.Time {
	c.mu.Lock()
	t := c.data[req]
	delete(c.data, req)
	c.mu.Unlock()
	return t
}

const (
	packetCacheTTL = 30 * time.Second
	packetCacheMax = 20
)

// getPacketFromCache returns a cached context packet for the given key, or nil
// if the entry is absent or expired. Uses a read lock so concurrent reads do
// not serialise; expired entries are lazily evicted by setPacketCache.
func (s *Server) getPacketFromCache(key string) interface{} {
	s.packetCacheMu.RLock()
	defer s.packetCacheMu.RUnlock()
	e, ok := s.packetCache[key]
	if !ok || time.Now().After(e.expiresAt) {
		return nil
	}
	return e.pkt
}

// setPacketCache stores a context packet under key with a 30s TTL.
// When the cache exceeds packetCacheMax entries, the entry with the oldest
// expiresAt timestamp is evicted (LRU-style). With ≤20 slots, the linear
// scan is trivial.
func (s *Server) setPacketCache(key string, pkt interface{}) {
	s.packetCacheMu.Lock()
	defer s.packetCacheMu.Unlock()
	if len(s.packetCache) >= packetCacheMax {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range s.packetCache {
			if oldestKey == "" || v.expiresAt.Before(oldestTime) {
				oldestKey, oldestTime = k, v.expiresAt
			}
		}
		delete(s.packetCache, oldestKey)
	}
	s.packetCache[key] = &packetCacheEntry{pkt: pkt, expiresAt: time.Now().Add(packetCacheTTL)}
}

// InvalidatePacketCache clears the entire context-packet cache. Satisfies the
// watcher.PacketCacheInvalidator interface; delegates to InvalidatePacketCacheForFile
// with an empty path so resource notifications and brain warming are also triggered.
func (s *Server) InvalidatePacketCache() {
	s.InvalidatePacketCacheForFile("")
}

// SetChangeSource wires a change event source (typically the file watcher) so
// get_working_state can report recent file activity to agents.
func (s *Server) SetChangeSource(cs ChangeSource) {
	s.changeSource = cs
}

// SetFederationResolver wires a federation.Resolver into the server for
// cross-project dependency tracking and drift detection. When set,
// session_init includes federation health and drift alerts, prepare_context
// enriches responses with cross-project dependency data.
// Pass nil to disable federation features.
func (s *Server) SetFederationResolver(fr *federation.Resolver) {
	s.federationResolver = fr
}

// SetProjectRegistry wires a cross-project store provider so that recall,
// get_events, get_messages, and get_agents can query sibling projects.
func (s *Server) SetProjectRegistry(pr ProjectStoreProvider) {
	s.projectRegistry = pr
}

// resolveProjectStores parses a comma-separated projects param (or "*" for all)
// and returns a map of projectName → *store.Store. Excludes the current project.
// The second return value lists any requested project names that were not found
// (empty for "*" queries). Callers can surface this to help agents fix typos.
//
// Federation ACL enforcement: if s.config.FederationACL is configured, only
// projects listed in AllowReadFrom (or "*" for all) are returned. Projects
// blocked by the ACL appear in the third return value so agents understand why.
// Default (nil ACL or empty AllowReadFrom): deny-all — no cross-project reads.
func (s *Server) resolveProjectStores(projectsParam string) (map[string]*store.Store, []string) {
	if s.projectRegistry == nil || projectsParam == "" {
		return nil, nil
	}
	projectsParam = strings.TrimSpace(projectsParam)

	allNames := s.projectRegistry.ListProjects()
	allSet := make(map[string]bool, len(allNames))
	for _, n := range allNames {
		allSet[n] = true
	}

	var wanted map[string]bool
	if projectsParam == "*" {
		wanted = allSet
	} else {
		parts := strings.Split(projectsParam, ",")
		wanted = make(map[string]bool, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				wanted[p] = true
			}
		}
	}

	// ACL enforcement: check federation_acl.allow_read_from.
	acl := s.config.FederationACL

	result := make(map[string]*store.Store)
	var notFound []string
	for name := range wanted {
		// ACL check — deny-all by default.
		if !acl.IsAllowed(name) {
			if projectsParam != "*" {
				notFound = append(notFound, name+" (denied by federation_acl)")
			}
			continue
		}

		st := s.projectRegistry.GetStore(name)
		if st == nil {
			if projectsParam != "*" {
				// Only report not-found for explicit names, not wildcard.
				notFound = append(notFound, name)
			}
			continue
		}
		// Skip self: compare store pointers rather than names to handle
		// disambiguated names like "api (work)" correctly.
		if st == s.store {
			continue
		}
		result[name] = st
	}
	return result, notFound
}

// allowedProjectNames returns the subset of registered project names that the
// current project's ACL permits reading from. Used in error messages to avoid
// leaking the names of projects the agent is not allowed to access.
func (s *Server) allowedProjectNames() []string {
	if s.projectRegistry == nil {
		return nil
	}
	acl := s.config.FederationACL
	var allowed []string
	for _, name := range s.projectRegistry.ListProjects() {
		if acl.IsAllowed(name) {
			allowed = append(allowed, name)
		}
	}
	return allowed
}

// SetBrainClient wires a *brain.Client into the server so that get_context
// returns enriched Context Packets and violations include LLM explanations.
// Using interface{} avoids an import cycle (brain imports only stdlib).
func (s *Server) SetBrainClient(bc interface{}) {
	s.brainClient = bc
}

// SetProjectID sets the stable project identifier used when building context packets.
func (s *Server) SetProjectID(id string) {
	s.projectID = id
}

// SetWebCache wires a *webcache.Cache into the server so that lookup_docs
// can serve version-pinned package documentation and URL caching.
func (s *Server) SetWebCache(wc *webcache.Cache) {
	s.webCache = wc
}

// SetProjectPath stores the absolute project root path so that lookup_docs
// can parse go.mod for version-pinned package documentation.
func (s *Server) SetProjectPath(path string) {
	s.projectPath = path
}

// SetPulseClient wires a *pulse.Client into the server so that every tool
// call emits telemetry to the synapses-pulse analytics sidecar.
func (s *Server) SetPulseClient(pc *pulse.Client) {
	s.pulseClient = pc
}

// getPulseClient type-asserts the stored pulseClient to *pulse.Client.
// Returns nil if no pulse client is configured.
func (s *Server) getPulseClient() *pulse.Client {
	if s.pulseClient == nil {
		return nil
	}
	pc, _ := s.pulseClient.(*pulse.Client)
	return pc
}

// SetTechStack stores the detected tech stack entries so that
// get_project_identity can surface them as tech_stack.
// Called from cmdStart after autosubscribe detection completes.
func (s *Server) SetTechStack(ts interface{}) {
	s.techStack = ts
}

// SetEmbedClient wires an embedding client so handleSemanticSearch can
// perform vector similarity search in addition to FTS5 BM25 ranking.
// Pass nil to disable vector search (falls back to FTS5-only).
func (s *Server) SetEmbedClient(ec *embed.Client) {
	s.embedClient = ec
}

// SetMemoryEmbedder wires an embedder for generating memory embeddings
// on remember() writes and for vector search in recall(). Pass nil to
// disable memory embeddings (FTS5-only recall).
func (s *Server) SetMemoryEmbedder(e embed.Embedder) {
	s.memoryEmbedder = e
}

// ServeStdio starts the MCP server on stdin/stdout. This call blocks until
// the client disconnects or the process receives a signal.
func (s *Server) ServeStdio() error {
	return server.ServeStdio(s.mcp)
}

// MCPServer returns the underlying mcp-go MCPServer instance.
// Used by the daemon to serve MCP sessions over Unix socket connections
// instead of stdio.
func (s *Server) MCPServer() *server.MCPServer {
	return s.mcp
}

// StartBackground launches background maintenance goroutines and the
// bounded worker pool that processes fire-and-forget handler work.
// Call Close() to stop them and wait for graceful completion.
// Idempotent — safe to call multiple times.
//
// IMPORTANT: must be called before any handler executes. All production paths
// (stdio, daemon full-mode, daemon knowledge-mode) call this immediately after
// New(). goBackground() enqueues work into the buffered channel even before
// StartBackground runs, so a brief gap is tolerable — items accumulate in the
// buffer and are consumed once workers start. However, if StartBackground is
// never called, queued items are silently lost on Close().
func (s *Server) StartBackground() {
	s.startOnce.Do(func() {
		// Start bounded worker pool — processes all goBackground() work items.
		// Workers drain the queue on shutdown via close(bgQueue) + range loop.
		for i := 0; i < bgWorkers; i++ {
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				for fn := range s.bgQueue {
					func() {
						defer func() {
							if r := recover(); r != nil {
								logutil.Error("synapses: background worker panic: %v\nstack:\n%s\n", r, debug.Stack())
							}
						}()
						fn()
					}()
				}
			}()
		}

		// Memory expiry loop (requires store).
		// Capture s.store now — tests may set s.store = nil after StartBackground.
		if st := s.store; st != nil {
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.memoryExpiryLoop(st)
			}()
		}
	})
}

// closeGracefulTimeout is how long Close() waits for background goroutines
// before giving up. Workers drain remaining bgQueue items; memoryExpiryLoop
// calls store.ExpireMemories() — both fast in practice. The cap prevents
// daemon shutdown from hanging if the store is locked or unresponsive.
const closeGracefulTimeout = 5 * time.Second

// Close signals all background goroutines to stop and waits up to 5 seconds
// for graceful completion. Shutdown sequence:
//  1. Reject new work (shutdownMu write lock ensures no concurrent goBackground sends)
//  2. Signal long-running loops via stopCh
//  3. Close bgQueue — workers drain remaining items then exit their range loop
//  4. Wait with timeout for all tracked goroutines
//
// Safe to call multiple times — subsequent calls are no-ops.
func (s *Server) Close() {
	// 1. Reject new work — atomic with bgQueue access via shutdownMu.
	// After this, goBackground() returns immediately without enqueuing.
	s.shutdownMu.Lock()
	alreadyClosed := s.bgClosed
	s.bgClosed = true
	s.shutdownMu.Unlock()

	if alreadyClosed {
		return
	}

	// 2. Signal long-running loops (memoryExpiryLoop).
	close(s.stopCh)

	// 3. Close queue — workers drain remaining items then exit.
	close(s.bgQueue)

	// 4. Wait with timeout.
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeGracefulTimeout):
		logutil.Error("synapses: background workers did not drain within %v\n", closeGracefulTimeout)
	}
}

// memoryExpiryLoop runs ExpireMemories every 6 hours until stopCh is closed.
// st is captured at call-site to avoid reading s.store which may be set to nil
// by tests. Returns immediately when st is nil.
func (s *Server) memoryExpiryLoop(st *store.Store) {
	if st == nil {
		return
	}
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()

	// Run once at startup to clear any stale memories from previous sessions.
	st.ExpireMemories()

	for {
		select {
		case <-ticker.C:
			st.ExpireMemories()
		case <-s.stopCh:
			return
		}
	}
}

// ── B28: Scale-aware tool registration ───────────────────────────────────────

// coreTierTools are the ~12 tools registered at every repo scale (micro+).
// They cover the minimal viable session: bootstrap, navigate, plan, implement,
// and finish. Always available regardless of repo size.
// Phase 6 (Proactive Context Engine): updated to match the design doc's core set.
// All other tools are deferred and auto-promoted on first call.
var coreTierTools = map[string]bool{
	"session_init":          true,
	"prepare_context":       true,
	"search":                true,
	"validate_plan":         true,
	"verify_implementation": true,
	"remember":              true,
	"recall":                true,
	"create_plan":           true,
	"update_task":           true,
	"end_session":           true,
	"discover_tools":        true,
	"annotate_node":         true,
}

// standardTierTools are the ~20 tools registered for small and medium repos.
// Adds coordination, memory, and exploration tools on top of the core set.
var standardTierTools = map[string]bool{
	// Core (same as above, duplicated for O(1) lookup).
	"session_init":          true,
	"prepare_context":       true,
	"search":                true,
	"validate_plan":         true,
	"verify_implementation": true,
	"remember":              true,
	"recall":                true,
	"create_plan":           true,
	"update_task":           true,
	"end_session":           true,
	"discover_tools":        true,
	"annotate_node":         true,
	// Standard additions.
	"get_context":       true,
	"find_entity":       true,
	"get_pending_tasks": true,
	"get_file_context":  true,
	"get_impact":        true,
	"get_call_chain":    true,
	"get_working_state": true,
	"get_violations":    true,
}

// knowledgeTools are the tools available when the server runs in knowledge mode
// (no code graph). All other tools return a clear error message.
var knowledgeTools = map[string]bool{
	"session_init":       true,
	"end_session":        true,
	"remember":           true,
	"recall":             true,
	"send_message":       true,
	"get_messages":       true,
	"create_plan":        true,
	"get_pending_tasks":  true,
	"update_task":        true,
	"save_session_state": true,
	"get_session_state":  true,
	"get_agents":         true,
	"get_events":         true,
	"discover_tools":     true,
	"get_plans":          true,
	"get_my_tasks":       true,
	"link_task_nodes":    true,
	"check_plan_safety":  true,
	"report_usage":       true,
}

// hiddenTools are deprecated or subsumed tools that remain callable but are
// filtered from tools/list to reduce the default tool surface (Sprint 8 #1).
// Power users can still discover and call them via discover_tools.
var hiddenTools = map[string]bool{
	"get_working_state":    true, // subsumed by session_init
	"get_project_identity": true, // subsumed by session_init
	"report_usage":         true, // absorbed by end_session
	"check_plan_safety":    true, // absorbed by plan_context and validate_plan(check_safety=true)
	"get_edge_types":       true, // implementation detail
	"plan_context":         true, // power-user compound tool; Sprint 8 #2
}

// toolInTier reports whether name should be registered at startup given
// s.repoScale. Phase 6 (Proactive Context Engine): all tools are always
// registered at all scales. The design doc mandates "the agent NEVER sees
// 'tool not found' for any previously-existing tool" (Section 5.2).
// Scale-aware discoverability is now handled via suggested_tools in
// session_init and the status field in discover_tools, not via tool hiding.
// The coreTierTools and standardTierTools maps are retained for
// categorization purposes (e.g. discover_tools status field).
func (s *Server) toolInTier(_ string) bool {
	return true
}

// addOrDefer registers tool t with the MCP server. In knowledge mode,
// non-knowledge tools are replaced with a stub returning a clear error message.
// Always populates the REST dispatch table (toolHandlers) with the effective handler
// so POST /v1/tools/{name} calls the same function as the MCP path.
func (s *Server) addOrDefer(t mcp.Tool, h server.ToolHandlerFunc) {
	// OF-S4: capture description for integrity baseline.
	s.toolDescs[t.Name] = t.Description
	if s.knowledgeMode && !knowledgeTools[t.Name] {
		// In knowledge mode, register a stub that returns a clear error.
		stub := func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError(fmt.Sprintf(
				"Tool %q is not available in knowledge mode (no code graph). "+
					"Available tools: session_init, remember, recall, create_plan, "+
					"update_task, get_pending_tasks, send_message, get_messages, "+
					"get_agents, get_events, discover_tools, end_session.", t.Name)), nil
		}
		s.mcp.AddTool(t, stub)
		s.toolHandlersMu.Lock()
		s.toolHandlers[t.Name] = stub
		s.toolHandlersMu.Unlock()
		return
	}
	// Security F10: apply per-session rate limiting before loop-guard so that
	// rate-limited calls never consume a loop-guard slot. Rate limiting is
	// evaluated first (cheapest check first), then loop detection.
	rateLimited := s.rl.wrap(t.Name, h)
	// OF-S5: wrap every handler with the loop guard so that agent loops are
	// detected and rejected uniformly — no per-handler wiring needed.
	guarded := s.lg.wrap(rateLimited)
	s.mcp.AddTool(t, guarded)
	s.toolHandlersMu.Lock()
	s.toolHandlers[t.Name] = guarded
	s.toolHandlersMu.Unlock()
}

// DispatchTool calls a registered tool handler directly by name.
// Used by the REST API (POST /v1/tools/{name}) to bypass the JSON-RPC layer.
// Returns (nil, ErrUnknownTool) when name is not registered.
// The caller is responsible for injecting the session ID into ctx if needed.
func (s *Server) DispatchTool(ctx context.Context, name string, args map[string]interface{}) (*mcp.CallToolResult, error) {
	s.toolHandlersMu.RLock()
	h, ok := s.toolHandlers[name]
	s.toolHandlersMu.RUnlock()
	if !ok {
		return nil, &ErrUnknownTool{Name: name}
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	return h(ctx, req)
}

// ErrUnknownTool is returned by DispatchTool when the tool name is not registered.
type ErrUnknownTool struct {
	Name string
}

func (e *ErrUnknownTool) Error() string {
	return "unknown tool: " + e.Name
}

// SuggestToolsForIntent returns tool suggestions based on the agent's declared
// intent. Does not modify server state — pure query.
func (s *Server) SuggestToolsForIntent(intent string) []ToolSuggestion {
	return suggestToolsForIntent(intent)
}

// registerTools wires all Synapses tool definitions to their handlers.
func (s *Server) registerTools() {
	// ── Session Bootstrap ────────────────────────────────────────────────────

	// session_init: single round-trip startup replacing the 3-call ritual.
	// Supports incremental mode: when agent_id is provided and the agent has
	// called session_init before, unchanged sections (e.g. project_identity)
	// are omitted to save tokens. The agent's context profile is updated
	// after each call so subsequent calls are incremental.
	s.addOrDefer(
		mcp.NewTool(
			"session_init",
			mcp.WithDescription(
				"Single-call session bootstrap. Returns pending_tasks, project_identity, "+
					"working_state, and recent_events in one round-trip — replacing the "+
					"three-step startup ritual. Includes scale_guidance so agents self-tune "+
					"their tool usage to repo size. Call this INSTEAD of the three individual tools. "+
					"Incremental mode: when agent_id is provided and the agent has called "+
					"session_init before, unchanged sections are skipped to save tokens "+
					"(e.g. project_identity is omitted if the graph hasn't changed). "+
					"Also surfaces invalidated_memories — beliefs anchored to graph nodes that "+
					"were removed or changed since the last session. Per-agent: each agent sees "+
					"invalidations independently. "+
					"Lean defaults: pending_tasks.tasks is omitted when empty (no tasks exist) — "+
					"check pending_tasks.summary instead. recent_events is omitted when empty — "+
					"latest_event_seq is always present for event-stream subscription.",
			),
			mcp.WithString("agent_id",
				mcp.Description("Self-declared agent identifier. Enables incremental delivery: "+
					"subsequent calls skip unchanged project_identity and filter events to only "+
					"those since the last session. Always provide for token savings."),
			),
			mcp.WithString("model",
				mcp.Description("Optional. The model you are running on, e.g. 'claude-sonnet-4-6'. "+
					"Recorded in pulse analytics so cost savings are calculated against your actual model price."),
			),
			mcp.WithString("provider",
				mcp.Description("Optional. Model provider: 'anthropic', 'openai', etc."),
			),
			mcp.WithString("intent",
				mcp.Description("Optional. Short free-text declaration of what you are working on — visible to peer agents as a Tier 2/3 signal. E.g. 'implementing R1 framework edge injection'. Pass empty string to clear."),
			),
			mcp.WithString("scope",
				mcp.Description("Optional. Controls response verbosity. "+
					"\"full\" (default): all sections; empty arrays omitted (pending_tasks.tasks, recent_events absent when empty — check pending_tasks.summary and latest_event_seq). "+
					"\"quick\": tasks + working_state + scale_guidance (~500 tokens for single-repo; "+
					"safety-critical alerts like cross_project_alerts and agent_awareness are always included). "+
					"\"resume\": tasks with session states + working_state + relevant_memories (for task context continuity)."),
			),
		),
		s.handleSessionInit,
	)

	// report_usage: agent self-reports its LLM token usage after a response (Option B).
	s.addOrDefer(
		mcp.NewTool(
			"report_usage",
			mcp.WithDescription(
				"Prefer end_session(model=..., input_tokens=..., output_tokens=...) instead — "+
					"it absorbs report_usage and also persists session knowledge. "+
					"Report your LLM token usage for this response. Call after completing a major task "+
					"to give Synapses accurate data on model cost and token consumption. "+
					"All fields are optional but model is strongly recommended. "+
					"This is the complement to session_init(model=...) — session_init records the model once, "+
					"report_usage records per-response token counts.",
			),
			mcp.WithString("model",
				mcp.Required(),
				mcp.Description("Model name, e.g. 'claude-sonnet-4-6' or 'gpt-4o'."),
			),
			mcp.WithString("provider",
				mcp.Description("Model provider: 'anthropic', 'openai', 'google', etc."),
			),
			mcp.WithNumber("input_tokens",
				mcp.Description("Input/prompt tokens consumed by this response."),
			),
			mcp.WithNumber("output_tokens",
				mcp.Description("Output/completion tokens generated by this response."),
			),
			mcp.WithNumber("cost_usd",
				mcp.Description("Actual USD cost if known. Leave unset to let Synapses estimate from token counts."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Agent identifier. Defaults to the agent_id from the most recent session_init."),
			),
		),
		s.handleReportUsage,
	)

	// explain_codebase: first-5-minutes orientation narrative (R11)
	s.addOrDefer(
		mcp.NewTool(
			"explain_codebase",
			mcp.WithDescription(
				"Returns a ~1000 token natural-language orientation of the codebase: "+
					"entry points, key types by fanin, architectural patterns detected, "+
					"package structure, and tech stack. Built entirely from the graph — no LLM required. "+
					"Cached until a structural change occurs. "+
					"Use this at the start of a session on an unfamiliar repo instead of 5-10 Grep/Read calls.",
			),
		),
		s.handleExplainCodebase,
	)

	// get_repo_map: navigable package+entity overview (R12)
	s.addOrDefer(
		mcp.NewTool(
			"get_repo_map",
			mcp.WithDescription(
				"Returns a navigable text map of the repository: packages grouped by architectural layer "+
					"(entry points, api surface, core logic, persistence, config) with their top entities by fanin. "+
					"detail=\"compact\" (~500 tokens, top 3 entities per package — default). "+
					"detail=\"full\" (~2000 tokens, top 10 entities per package). "+
					"Pure graph query — no LLM. Cached until structural change. "+
					"Use to explore an unfamiliar area of the codebase without burning tokens on Glob/Read.",
			),
			mcp.WithString("detail",
				mcp.Description("Output verbosity: \"compact\" (top 3 entities/package, default) or \"full\" (top 10)."),
			),
		),
		s.handleGetRepoMap,
	)

	// discover_tools: lightweight tool finder
	s.addOrDefer(
		mcp.NewTool(
			"discover_tools",
			mcp.WithDescription(
				"Finds the right Synapses tool for a task. Describe what you need in natural language "+
					"and get back the top matching tools with usage examples. "+
					"Use this instead of scanning all tool definitions. ~300 tokens vs ~4200.",
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Natural language description of what you need, e.g. 'check what calls this function' or 'save my progress'."),
			),
			mcp.WithBoolean("debug",
				mcp.Description("When true, returns all scored tools (not just top 3) with per-tool score breakdowns showing which keywords, name tokens, and description words matched. Useful for tuning and auditing scoring logic."),
			),
		),
		s.handleDiscoverTools,
	)

	// ── Code Graph Tools ────────────────────────────────────────────────────

	// get_project_identity
	s.addOrDefer(
		mcp.NewTool(
			"get_project_identity",
			mcp.WithDescription(
				"Prefer session_init instead — it includes project_identity along with "+
					"pending tasks, working state, and scale guidance in one call. "+
					"Returns a compact architectural summary of the indexed project: "+
					"node counts, entry points, highest-connectivity entities, and active rules.",
			),
		),
		s.handleGetProjectIdentity,
	)

	// get_edge_types: semantic catalog of all graph edge types (R36)
	s.addOrDefer(
		mcp.NewTool(
			"get_edge_types",
			mcp.WithDescription(
				"Returns the semantic catalog of all graph edge types: name, BFS weight, direction, domain tag (code/docs/infra/api), "+
					"and human-readable description. "+
					"Foundation for multi-domain BFS — use this to understand how traversal weights are assigned "+
					"or to select domain-specific edges for cross-domain queries (Sprint 12). "+
					"format=\"json\" (default): full structured catalog. format=\"compact\": aligned text table.",
			),
			mcp.WithString("format",
				mcp.Description("Output format: \"json\" (default, full structured catalog) or \"compact\" (text table, ~200 tokens)."),
			),
		),
		s.handleGetEdgeTypes,
	)

	// get_context
	s.addOrDefer(
		mcp.NewTool(
			"get_context",
			mcp.WithDescription(
				"For most use cases, use prepare_context instead — "+
					"it provides intent-based context assembly in one structured call "+
					"(e.g. prepare_context(intent='modify', target='AuthService')). "+
					"Returns a relevance-ranked subgraph centred on the named entity. "+
					"Uses BFS with edge-type-weighted decay so the closest, most semantically "+
					"significant relationships appear first. This replaces grep: ask for what you "+
					"need structurally, not textually.",
			),
			mcp.WithString("entity",
				mcp.Required(),
				mcp.Description("The name of the code entity to carve context around (e.g. 'AuthService')."),
			),
			mcp.WithNumber("depth",
				mcp.Description("BFS hop limit. Defaults to the project config value (usually 2)."),
			),
			mcp.WithNumber("token_budget",
				mcp.Description("Maximum approximate tokens in the response. Defaults to 4000."),
			),
			mcp.WithString("task_id",
				mcp.Description("Optional task ID from get_pending_tasks. Nodes linked to this task get a relevance boost."),
			),
			mcp.WithString("mode",
				mcp.Description("'explore' (default): ego-subgraph BFS. 'impact': reverse-BFS showing what depends on this entity (same as get_impact)."),
			),
			mcp.WithString("file",
				mcp.Description("Optional file path suffix to pin the lookup to a specific file (e.g. 'cmd/synapses/main.go'). Use when entity names are ambiguous across multiple files."),
			),
			mcp.WithString("format",
				mcp.Description("Output format: 'compact' (default, natural-language briefing ~400-600 tokens) or 'json' (full JSON blob ~2000-3800 tokens). Use 'json' when you need structured machine-readable data."),
			),
			mcp.WithString("detail_level",
				mcp.Description("Only used with format='compact'. Controls verbosity: 'summary' (~50 tokens, root entity header + warnings only), 'neighbors' (~200 tokens, adds Calls/Called-by name lists), 'full' (default, ~400-600 tokens, adds callee detail blocks and insight)."),
			),
			mcp.WithBoolean("helpful",
				mcp.Description("Optional explicit feedback signal (true=context was useful, false=context missed what you needed). Recorded as an episode to improve future context delivery. Omit if you don't have a clear signal yet."),
			),
			mcp.WithBoolean("include_inferred",
				mcp.Description("R1: When false, strips synthetic framework routing nodes (NodeRoute / ⚡ inferred) from the response, returning only AST-proven static edges. Default: true (inferred route nodes included)."),
			),
			mcp.WithString("known_hash",
				mcp.Description("Optional. Pass the entity_hash value from a previous get_context response for the same entity. "+
					"If the ego-graph is structurally unchanged, returns {\"unchanged\": true, \"entity_hash\": \"...\", \"entity\": \"...\"} "+
					"instead of the full payload — saving tokens on repeated calls in tight reasoning loops. "+
					"Ignored when mode='impact'."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Optional. Your agent ID (same value passed to session_init). "+
					"When provided, this call is recorded as a watched symbol — if another agent "+
					"subsequently edits that file, a dependency_alert will surface in your next session_init. "+
					"Omit in read-only or exploratory sessions where you don't want peer tracking."),
			),
			mcp.WithString("projects",
				mcp.Description("Optional. Comma-separated federation aliases to include sibling project results "+
					"(e.g. 'core,app'). When provided, also returns matching entities from sibling stores."),
			),
		),
		s.handleGetContext,
	)

	// find_entity
	s.addOrDefer(
		mcp.NewTool(
			"find_entity",
			mcp.WithDescription(
				"Locates nodes in the graph by name or substring. "+
					"Returns matching node references (ID, type, file, line) without full context. "+
					"Use this to discover the exact entity name before calling get_context.",
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Name or substring to search for (case-insensitive)."),
			),
			mcp.WithString("format",
				mcp.Description("Output format. \"compact\" (default): one line per match — \"[Name] type · file:line\". "+
					"\"json\": full structured response with IDs and metadata."),
			),
		),
		s.handleFindEntity,
	)

	// validate_plan
	s.addOrDefer(
		mcp.NewTool(
			"validate_plan",
			mcp.WithDescription(
				"Checks a list of proposed code changes against the project's architectural rules. "+
					"Returns any violations before a single line of code is written. "+
					"Call this before implementing a plan that touches multiple files. "+
					"Pass check_safety=true to also run a failure-episode safety check inline. "+
					`Example: validate_plan(changes=[{"file": "internal/auth/service.go", "adds_call_to": "ValidateToken"}]).`,
			),
			mcp.WithString("changes",
				mcp.Required(),
				mcp.Description(
					"JSON array of proposed changes. "+
						`Minimum: [{"file": "internal/auth/service.go"}]. `+
						`Full form: [{"file": "path/to/file.go", "adds_call_to": "SomeFunction", "removes_call_to": "OtherFunction"}]. `+
						"With file only: freshness warning (if modified <10s ago), logic anomaly checks, and cross-project drift checks all run — "+
						"but architectural graph-rule checks are skipped (they require adds_call_to to build the proposed edge). "+
						"Note: removes_call_to is accepted but not yet enforced against rules; use it for documentation purposes.",
				),
			),
			mcp.WithBoolean("check_safety",
				mcp.Description("When true, also runs a failure-episode safety check inline and adds safety_check to the response."),
			),
			mcp.WithString("plan_description",
				mcp.Description("Natural language description of the plan (used for safety check). Auto-derived from changed files if omitted."),
			),
			mcp.WithBoolean("skip_logic_checks",
				mcp.Description("When true, skips heuristic logic-level anomaly checks (zero-value identifiers, missing cleanup, tilde paths, nil method calls, concurrent map writes). Default false."),
			),
		),
		s.handleValidatePlan,
	)

	// verify_implementation — post-write complement to validate_plan
	s.addOrDefer(
		mcp.NewTool(
			"verify_implementation",
			mcp.WithDescription(
				"Post-write verification: checks the actual graph state of files you just wrote "+
					"against architectural rules. Returns violations, entity counts, and freshness warnings. "+
					"Optionally pass task_id to verify linked entities still exist in the graph. "+
					"Call this AFTER writing code to close the plan→implement→verify loop.",
			),
			mcp.WithString("files_written",
				mcp.Required(),
				mcp.Description(
					`JSON array of file paths that were written, e.g. ["internal/auth/service.go", "internal/auth/handler.go"].`,
				),
			),
			mcp.WithString("task_id",
				mcp.Description("Optional task ID. If provided, verifies that the task's linked_nodes still exist in the graph after implementation."),
			),
		),
		s.handleVerifyImplementation,
	)

	// get_violations (absorbs get_violation_log via rule_id + limit params)
	s.addOrDefer(
		mcp.NewTool(
			"get_violations",
			mcp.WithDescription(
				"Lists all current architectural rule violations found in the graph. "+
					"Returns rule ID, severity, affected nodes, and a human-readable description. "+
					"Pass rule_id to filter to a specific rule. Pass include_log=true to also return the historical audit log.",
			),
			mcp.WithString("rule_id",
				mcp.Description("Optional. Filter violations to a specific rule ID."),
			),
			mcp.WithBoolean("include_log",
				mcp.Description("When true, also returns the historical violation log entries. Default false."),
			),
			mcp.WithNumber("log_limit",
				mcp.Description("Max historical log entries to return when include_log=true. Default 50."),
			),
		),
		s.handleGetViolations,
	)

	// upsert_gap — R32: record a quality gap on a code entity
	s.addOrDefer(
		mcp.NewTool(
			"upsert_gap",
			mcp.WithDescription(
				"Record or update a quality gap on a specific code entity. "+
					"Quality gaps are agent-discovered findings that require reasoning to find — "+
					"edge cases, incomplete coverage, known limitations — unlike architecture violations "+
					"which are deterministic rule checks. Gaps persist across sessions and surface in "+
					"get_violations() and get_context() so future agents never re-discover the same issue. "+
					"Use status=\"fixed\" with fix_notes to close a gap after it is resolved.",
			),
			mcp.WithString("node_id",
				mcp.Required(),
				mcp.Description("The node ID of the code entity this gap applies to. Use find_entity() to resolve the ID."),
			),
			mcp.WithString("gap_id",
				mcp.Required(),
				mcp.Description("A short stable slug for this gap, e.g. \"dist-relative-path\". Used as the dedup key."),
			),
			mcp.WithString("description",
				mcp.Required(),
				mcp.Description("Human-readable description of the gap, including what is missing and why it matters."),
			),
			mcp.WithString("severity",
				mcp.Description("low | medium | high | critical. Default: medium."),
			),
			mcp.WithString("status",
				mcp.Description("open | fixed | wontfix. Default: open. Use \"fixed\" once the gap is resolved."),
			),
			mcp.WithString("fix_notes",
				mcp.Description("Optional. Explanation of how the gap was fixed. Only relevant when status=\"fixed\"."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Optional. Self-declared agent identifier for attribution."),
			),
		),
		s.handleUpsertGap,
	)

	// get_gaps — R32: query quality gaps
	s.addOrDefer(
		mcp.NewTool(
			"get_gaps",
			mcp.WithDescription(
				"Query quality gaps on code entities. Default returns open gaps only. "+
					"Use as a tech debt inventory (no filters), a pre-merge quality gate (file= filter), "+
					"or to check a specific entity's known issues (node_id= filter). "+
					"Open gaps also appear in get_violations() and at the top of get_context() responses.",
			),
			mcp.WithString("node_id",
				mcp.Description("Optional. Filter to gaps on a specific node ID."),
			),
			mcp.WithString("file",
				mcp.Description("Optional. Filter to gaps on nodes belonging to this file path."),
			),
			mcp.WithString("severity",
				mcp.Description("Optional. Filter by severity: low | medium | high | critical."),
			),
			mcp.WithString("status",
				mcp.Description("Optional. Filter by status: open | fixed | wontfix | all. Default: open."),
			),
		),
		s.handleGetGaps,
	)

	// get_file_context
	s.addOrDefer(
		mcp.NewTool(
			"get_file_context",
			mcp.WithDescription(
				"Returns all entities (functions, methods, structs, interfaces) defined in a file, "+
					"ordered by line number. Accepts a partial path suffix (e.g. 'store/tasks.go'). "+
					"Use this when working on a specific file to get an instant overview.",
			),
			mcp.WithString("file",
				mcp.Required(),
				mcp.Description("File path or suffix, e.g. 'internal/store/tasks.go' or 'tasks.go'."),
			),
			mcp.WithNumber("token_budget",
				mcp.Description("Max response size in tokens (default 4000). When exceeded, entities are dropped from the bottom of the file (highest line numbers first). Response includes truncated=true and total_entities=N when trimmed."),
			),
		),
		s.handleGetFileContext,
	)

	// search (absorbs semantic_search via mode param)
	s.addOrDefer(
		mcp.NewTool(
			"search",
			mcp.WithDescription(
				"Keyword search across entity names and doc comments. "+
					"Results are ranked: exact name match > name prefix > name substring > doc comment match. "+
					"Returns up to 25 results. Use this to find auth-related code, error handlers, etc. "+
					"Set mode='fulltext' for FTS5 BM25 search by concept ('rate limiting', 'JWT validation'). 'semantic' is accepted as alias. "+
					"CamelCase names are auto-split: searching 'carve' finds 'CarveEgoGraph'.",
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Search term (case-insensitive)."),
			),
			mcp.WithString("mode",
				mcp.Description("Search mode: 'keyword' (default, exact/prefix/substring) or 'semantic' (FTS BM25 by concept)."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum results to return (default 20, max 50). Only used for mode=semantic."),
			),
		),
		s.handleSearch,
	)

	// get_call_chain
	s.addOrDefer(
		mcp.NewTool(
			"get_call_chain",
			mcp.WithDescription(
				"Finds the shortest call path between two entities by following CALLS edges. "+
					"Answers 'how does A reach B?' Use this to understand the execution path "+
					"between entry points and deep implementation details.",
			),
			mcp.WithString("from",
				mcp.Required(),
				mcp.Description("Name of the starting entity (caller side)."),
			),
			mcp.WithString("to",
				mcp.Required(),
				mcp.Description("Name of the target entity (callee side)."),
			),
		),
		s.handleGetCallChain,
	)

	// annotate_node
	s.addOrDefer(
		mcp.NewTool(
			"annotate_node",
			mcp.WithDescription(
				"Attaches a note to a graph node, visible to all agents via get_context. "+
					"Use this as a shared whiteboard: Agent A can annotate a function with "+
					"'known race condition here' and Agent B will see it in context queries. "+
					"Annotations persist across sessions.",
			),
			mcp.WithString("node_id",
				mcp.Required(),
				mcp.Description("The node ID to annotate (from find_entity or get_context)."),
			),
			mcp.WithString("note",
				mcp.Required(),
				mcp.Description("The annotation text."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Optional. Self-declared agent identifier for attribution."),
			),
		),
		s.handleAnnotateNode,
	)

	// get_impact
	s.addOrDefer(
		mcp.NewTool(
			"get_impact",
			mcp.WithDescription(
				"Performs blast-radius analysis: reverse-BFS from a named entity "+
					"following incoming CALLS and IMPLEMENTS edges to find everything that "+
					"could break if the entity changes. "+
					"Results grouped by depth: direct (depth 1, confidence 1.0), "+
					"indirect (depth 2, confidence 0.6), peripheral (depth 3+, confidence 0.3). "+
					"Answers: 'what breaks if I change X?'",
			),
			mcp.WithString("symbol",
				mcp.Required(),
				mcp.Description("Name of the entity to analyse (e.g. 'CarveEgoGraph')."),
			),
			mcp.WithNumber("depth",
				mcp.Description("Max hop depth. Default 3, max 10."),
			),
			mcp.WithNumber("token_budget",
				mcp.Description("Max response size in tokens (default 2000). When exceeded, peripheral (depth 3+) nodes are dropped first, then indirect (depth 2). Direct callers (depth 1) are always kept. Response includes truncated=true when trimmed."),
			),
			mcp.WithString("projects",
				mcp.Description("Optional. Comma-separated federation aliases to also check sibling project dependents "+
					"(e.g. 'app'). Returns cross-project callers alongside local ones."),
			),
		),
		s.handleGetImpact,
	)

	// get_entity_history: chronological timeline compositing 5 data sources (Sprint 10 #4)
	s.addOrDefer(
		mcp.NewTool(
			"get_entity_history",
			mcp.WithDescription(
				"Returns a chronological timeline for a named code entity, compositing "+
					"memories, episodes, annotations, task references, and git changes. "+
					"One tool call instead of 5. Answers: 'what happened to this entity?'",
			),
			mcp.WithString("entity",
				mcp.Required(),
				mcp.Description("Name of the code entity (e.g. 'AuthService', 'handleLogin'). "+
					"Resolved via FindByName with automatic disambiguation."),
			),
			mcp.WithString("file",
				mcp.Description("Optional file path suffix to disambiguate when multiple entities share the same name "+
					"(e.g. 'internal/auth/service.go')."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum timeline events to return (default 50, max 200). "+
					"Events are sorted by timestamp descending (newest first)."),
			),
		),
		s.handleGetEntityHistory,
	)

	// ── Agent Task Memory Tools ──────────────────────────────────────────────
	// These tools give Synapses session continuity: plans and tasks agreed in
	// one LLM conversation are stored in SQLite and surfaced to future sessions.

	// create_plan
	s.addOrDefer(
		mcp.NewTool(
			"create_plan",
			mcp.WithDescription(
				"Saves a named plan with actionable tasks to persistent storage. "+
					"Call this when the user approves an implementation plan so that future "+
					"LLM sessions can resume the work via get_pending_tasks. "+
					"Each task has a title, description, priority (p0–p3), and optional linked node IDs.",
			),
			mcp.WithString("title",
				mcp.Required(),
				mcp.Description("Short name for the plan, e.g. 'v1.0.1 context quality improvements'."),
			),
			mcp.WithString("description",
				mcp.Description("Optional longer description of what the plan achieves."),
			),
			mcp.WithString("tasks",
				mcp.Required(),
				mcp.Description(
					`JSON array of task objects. Each: {"title":"...", "description":"...", "priority":"p0|p1|p2|p3", "linked_nodes":["nodeID",...]}.`,
				),
			),
			mcp.WithString("agent_id",
				mcp.Description("Optional. Self-declared agent identifier, e.g. 'claude-code-session-1'. Recorded as plan creator."),
			),
		),
		s.handleCreatePlan,
	)

	// get_pending_tasks (absorbs get_my_tasks via agent_id + suggest_next)
	s.addOrDefer(
		mcp.NewTool(
			"get_pending_tasks",
			mcp.WithDescription(
				"Returns all pending and in-progress tasks, ordered by priority (p0 first). "+
					"Call this at the start of every session to discover what work was agreed "+
					"in previous sessions and resume from exactly where the last session stopped. "+
					"Pass agent_id= to scope to your own tasks. Pass suggest_next=true to get the top unblocked task highlighted.",
			),
			mcp.WithString("plan_id",
				mcp.Description("Optional. Filter to tasks belonging to a specific plan."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Optional. Filter to tasks assigned to a specific agent. Use your own agent_id to see only your tasks."),
			),
			mcp.WithBoolean("suggest_next",
				mcp.Description("When true, adds a suggested_next field with the top unblocked pending task."),
			),
		),
		s.handleGetPendingTasks,
	)

	// save_session_state
	s.addOrDefer(
		mcp.NewTool(
			"save_session_state",
			mcp.WithDescription(
				"Saves the exact working state for an in-progress task so the next LLM session "+
					"can resume from precisely where this session stopped. Call this at the end of "+
					"a session or whenever significant progress is made. "+
					"The state is included automatically in get_pending_tasks() for in_progress tasks.",
			),
			mcp.WithString("task_id",
				mcp.Required(),
				mcp.Description("The task ID from get_pending_tasks."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Optional. Self-declared agent identifier."),
			),
			mcp.WithString("approach",
				mcp.Description("Current strategy or approach being taken for this task."),
			),
			mcp.WithString("files_modified",
				mcp.Description("JSON array of file paths modified so far, e.g. [\"internal/store/tasks.go\"]."),
			),
			mcp.WithString("completed_steps",
				mcp.Description("JSON array of step descriptions already completed."),
			),
			mcp.WithString("remaining_steps",
				mcp.Description("JSON array of step descriptions still to do."),
			),
			mcp.WithString("blockers",
				mcp.Description("JSON array of blocker descriptions (empty if none)."),
			),
			mcp.WithString("decisions",
				mcp.Description("JSON array of key decisions made during this session."),
			),
			mcp.WithString("context_snapshot",
				mcp.Description("Free-form text snapshot of the current LLM context (key facts, state, etc.)."),
			),
		),
		s.handleSaveSessionState,
	)

	// end_session: captures session knowledge and optionally reports usage.
	s.addOrDefer(
		mcp.NewTool(
			"end_session",
			mcp.WithDescription(
				"Captures session knowledge and persists it as structured memories. "+
					"Call at the end of a session to automatically extract: files touched, "+
					"entities examined, tasks updated. Saves session-log, entity, and project "+
					"memories that future sessions will see in session_init and get_context. "+
					"This is how institutional knowledge accumulates across sessions. "+
					"Optionally reports LLM token usage (absorbs report_usage) if model is provided.",
			),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("Self-declared agent identifier."),
			),
			mcp.WithString("task_id",
				mcp.Description("Optional. Link session memories to this task."),
			),
			mcp.WithString("summary",
				mcp.Description("Optional. High-level summary of what was accomplished. "+
					"Saved as a project-tier memory visible to all future sessions."),
			),
			mcp.WithString("model",
				mcp.Description("Optional. Model name for usage reporting (e.g. 'claude-sonnet-4-6'). "+
					"When provided, token usage is reported to pulse analytics (absorbs report_usage)."),
			),
			mcp.WithString("provider",
				mcp.Description("Optional. Model provider: 'anthropic', 'openai', etc. Used with model for usage reporting."),
			),
			mcp.WithNumber("input_tokens",
				mcp.Description("Optional. Total input tokens consumed during this session."),
			),
			mcp.WithNumber("output_tokens",
				mcp.Description("Optional. Total output tokens generated during this session."),
			),
			mcp.WithNumber("cost_usd",
				mcp.Description("Optional. Total USD cost for this session if known."),
			),
		),
		s.handleEndSession,
	)

	// get_session_state
	s.addOrDefer(
		mcp.NewTool(
			"get_session_state",
			mcp.WithDescription(
				"Returns the saved session state for a task, enabling exact-moment resumption "+
					"of work started in a previous LLM session. "+
					"Note: get_pending_tasks() already includes session state for in_progress tasks inline; "+
					"use this tool only when you need to fetch state for a specific task explicitly.",
			),
			mcp.WithString("task_id",
				mcp.Required(),
				mcp.Description("The task ID to retrieve session state for."),
			),
		),
		s.handleGetSessionState,
	)

	// update_task
	s.addOrDefer(
		mcp.NewTool(
			"update_task",
			mcp.WithDescription(
				"Updates the status of a task and optionally appends timestamped notes. "+
					"Use this to mark tasks done as you complete them, "+
					"or to leave context notes for the next LLM session.",
			),
			mcp.WithString("id",
				mcp.Required(),
				mcp.Description("The task ID returned by get_pending_tasks."),
			),
			mcp.WithString("status",
				mcp.Required(),
				mcp.Description("New status: pending | in_progress | done | cancelled."),
			),
			mcp.WithString("notes",
				mcp.Description("Optional notes to append (timestamped). Use to leave context for the next session."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Optional. Self-declared agent identifier. Recorded as last_updated_by."),
			),
			mcp.WithString("intent",
				mcp.Description("Optional. Short free-text declaration of what you are working on — visible to peer agents. E.g. 'implementing R1 framework edge injection'. Pass space to clear."),
			),
		),
		s.handleUpdateTask,
	)

	// ── Coordination & Multi-Agent Tools ────────────────────────────────────

	// get_plans
	s.addOrDefer(
		mcp.NewTool(
			"get_plans",
			mcp.WithDescription(
				"List all saved plans with task completion counts. "+
					"Use this to get an overview of all active and completed work across sessions.",
			),
		),
		s.handleGetPlans,
	)



	// link_task_nodes
	s.addOrDefer(
		mcp.NewTool(
			"link_task_nodes",
			mcp.WithDescription(
				"Explicitly links a task to graph node IDs. "+
					"Linked nodes get a relevance boost when get_context is called with task_id=. "+
					"Replaces any existing links for the task.",
			),
			mcp.WithString("task_id",
				mcp.Required(),
				mcp.Description("The task ID to link nodes to."),
			),
			mcp.WithString("node_ids",
				mcp.Required(),
				mcp.Description("JSON array of node ID strings, e.g. [\"repo::pkg/auth.go::AuthService\"]."),
			),
		),
		s.handleLinkTaskNodes,
	)

	// get_agents
	s.addOrDefer(
		mcp.NewTool(
			"get_agents",
			mcp.WithDescription(
				"Returns all agents that have interacted with Synapses, ordered by last-seen timestamp. "+
					"Useful for understanding who else is working in this repository.",
			),
			mcp.WithString("projects",
				mcp.Description("Comma-separated project names (or \"*\" for all) to also query agents from sibling daemon-registered projects. Only works in daemon mode."),
			),
		),
		s.handleGetAgents,
	)

	// get_events
	s.addOrDefer(
		mcp.NewTool(
			"get_events",
			mcp.WithDescription(
				"Returns recent events from the pull-based event log: file_change, task_update, "+
					"annotation_added, agent_activity. Poll with since_seq cursor (from session_init "+
					"or the previous call's latest_seq) to get only new events.",
			),
			mcp.WithNumber("since_seq",
				mcp.Description("Return events with seq greater than this value. Use 0 for all recent events."),
			),
			mcp.WithString("types",
				mcp.Description("Comma-separated list of event types to filter by, e.g. 'file_change,task_update'. Omit for all types."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum events to return. Defaults to 50."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Optional. Filter events to only those emitted by this agent ID. Use to view a specific peer's activity stream (Tier 3 on-demand signal)."),
			),
			mcp.WithString("projects",
				mcp.Description("Comma-separated project names (or \"*\" for all) to also query from sibling daemon-registered projects. Only works in daemon mode."),
			),
		),
		s.handleGetEvents,
	)

	// ── Rule Management Tools ────────────────────────────────────────────────

	// upsert_rule
	s.addOrDefer(
		mcp.NewTool(
			"upsert_rule",
			mcp.WithDescription(
				"Create or update a dynamic architectural rule. "+
					"Persisted to SQLite and active immediately — no daemon restart required. "+
					"Subsequent validate_plan and get_violations calls enforce it. "+
					"Use this when you detect a pattern that should be formalised as a constraint.",
			),
			mcp.WithString("rule_id",
				mcp.Required(),
				mcp.Description("Unique identifier for the rule, e.g. 'no-db-in-handler'. Used to update an existing rule."),
			),
			mcp.WithString("description",
				mcp.Required(),
				mcp.Description("Human-readable explanation of what this rule prevents and why."),
			),
			mcp.WithString("severity",
				mcp.Required(),
				mcp.Description("'error' or 'warning'."),
			),
			mcp.WithString("edge_type",
				mcp.Description("Edge type to forbid: CALLS, IMPORTS, IMPLEMENTS, etc. Empty = any edge type."),
			),
			mcp.WithString("from_file_pattern",
				mcp.Description("Glob matched against the base name of the source file, e.g. '*/handlers/*'."),
			),
			mcp.WithString("to_file_pattern",
				mcp.Description("Glob matched against the base name of the target file, e.g. '*/db/*'."),
			),
			mcp.WithString("to_name_pattern",
				mcp.Description("Substring that must appear in the target entity name."),
			),
			mcp.WithString("context_source",
				mcp.Description(
					"Optional provenance of the context that led to this rule. "+
						"If 'external' or 'generated', the call is rejected — rules derived from "+
						"low-trust sources (web content, codegen headers) must not become "+
						"architectural constraints. Omit when context is user-authored code.",
				),
			),
		),
		s.handleUpsertRule,
	)

	// ── Session Awareness Tools ──────────────────────────────────────────────

	// get_working_state
	s.addOrDefer(
		mcp.NewTool(
			"get_working_state",
			mcp.WithDescription(
				"Prefer session_init instead — it includes working_state along with "+
					"pending tasks, project identity, and scale guidance in one call. "+
					"Returns recent file changes detected by the file watcher, answering "+
					"'what was the developer just working on?' "+
					"Also includes a git diff stat for the current working tree.",
			),
			mcp.WithNumber("window_minutes",
				mcp.Description("Look-back window in minutes. Defaults to 15."),
			),
		),
		s.handleGetWorkingState,
	)

	// ── Web Tools ─────────────────────────────────────────────────────────────

	// web_annotate
	s.addOrDefer(
		mcp.NewTool(
			"web_annotate",
			mcp.WithDescription(
				"Persists web findings as a graph node annotation so they survive across sessions "+
					"and appear in get_context for that node. "+
					"This is the 'context sharing' pattern: web research becomes a first-class "+
					"data object attached to a code entity, visible to all future agent sessions. "+
					"Pass hits (JSON array of {title,url,snippet} objects) or a plain note string, or both.",
			),
			mcp.WithString("node_id",
				mcp.Required(),
				mcp.Description("The node ID to annotate (from find_entity or get_context)."),
			),
			mcp.WithString("note",
				mcp.Description("Plain-text note summarising what was found. Used as-is or prepended to hits."),
			),
			mcp.WithString("hits",
				mcp.Description("JSON array of web hits, e.g. [{\"title\":\"...\",\"url\":\"...\",\"snippet\":\"...\"}]."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Optional. Self-declared agent identifier for attribution."),
			),
		),
		s.handleWebAnnotate,
	)

	// lookup_docs
	s.addOrDefer(
		mcp.NewTool(
			"lookup_docs",
			mcp.WithDescription(
				"Returns cached Go package documentation or arbitrary URL content from the "+
					"local Synapses doc cache. Package docs are version-pinned from go.mod and "+
					"never expire — re-fetched only when go.mod changes. URL content is cached "+
					"for 24 hours. Use this to verify API signatures before writing code. "+
					"Provide exactly one of: package=, url=, or entity=.",
			),
			mcp.WithString("package",
				mcp.Description(
					"Go import path to look up, e.g. 'github.com/mark3labs/mcp-go'. "+
						"Returns docs at the version pinned in go.mod.",
				),
			),
			mcp.WithString("url",
				mcp.Description("Arbitrary URL to fetch and cache (24h TTL)."),
			),
			mcp.WithString("entity",
				mcp.Description(
					"Code entity name (function, struct, file). Returns docs for all external "+
						"packages imported by that entity.",
				),
			),
		),
		s.handleLookupDocs,
	)

	// upsert_adr
	s.addOrDefer(
		mcp.NewTool(
			"upsert_adr",
			mcp.WithDescription(
				"Creates or updates an Architectural Decision Record (ADR) in the brain. "+
					"ADRs are persistent cold-memory entries for significant design choices — "+
					"they appear in get_context compact output when linked_files match the entity's file. "+
					"IMPORTANT: always pass linked_files=[\"path/to/file.go\"] when creating an ADR — "+
					"without it, get_adrs(file=...) will never return this ADR for that file. "+
					"Requires brain.url to be configured in synapses.json.",
			),
			mcp.WithString("id",
				mcp.Required(),
				mcp.Description("Unique identifier for the ADR, e.g. 'adr-001-no-cgo'. Use kebab-case."),
			),
			mcp.WithString("title",
				mcp.Required(),
				mcp.Description("Short, declarative title of the decision, e.g. 'No CGo — use modernc/sqlite'."),
			),
			mcp.WithString("decision",
				mcp.Required(),
				mcp.Description("The decision made, in 1-3 sentences."),
			),
			mcp.WithString("status",
				mcp.Description("One of: proposed, accepted, deprecated, superseded. Defaults to 'proposed'."),
			),
			mcp.WithString("context",
				mcp.Description("Problem context and forces that led to this decision."),
			),
			mcp.WithString("consequences",
				mcp.Description("Consequences and trade-offs of this decision."),
			),
			mcp.WithArray("linked_files",
				mcp.Description("File path patterns to associate with this ADR (e.g. [\"internal/auth/\", \"cmd/server.go\"]). Used by get_adrs(file=) to surface relevant ADRs for a given file."),
				mcp.Items(map[string]any{"type": "string"}),
			),
		),
		s.handleUpsertADR,
	)

	// prepare_context — intent-based context assembly (replaces multi-tool chains).
	s.addOrDefer(
		mcp.NewTool(
			"prepare_context",
			mcp.WithDescription(
				"THE canonical context tool — start here for all code exploration tasks. "+
					"Intent-based context assembly: declare WHAT you need and a target; "+
					"Synapses composes the right context in one round-trip. "+
					"Replaces chains like get_context→get_impact→get_violations.\n"+
					"Intents: 'modify' (safe-edit briefing), 'understand' (structure), "+
					"'review' (quality/risk), 'debug' (call-path trace), "+
					"'add' (conventions for new code), 'plan' (dry-run scope assessment).",
			),
			mcp.WithString("intent",
				mcp.Required(),
				mcp.Description("'modify' | 'understand' | 'review' | 'debug' | 'add' | 'plan'"),
			),
			mcp.WithString("target",
				mcp.Required(),
				mcp.Description("Entity name, file path suffix, or search query."),
			),
			mcp.WithString("file",
				mcp.Description("Optional file path suffix to disambiguate entity names."),
			),
			mcp.WithString("task_id",
				mcp.Description("Optional task ID for relevance boosting."),
			),
			mcp.WithNumber("token_budget",
				mcp.Description("Max tokens for the response. Defaults vary by intent (1500–3500)."),
			),
			mcp.WithString("projects",
				mcp.Description("Optional. Comma-separated federation aliases to include cross-project context "+
					"(e.g. 'core'). Enriches response with sibling entity data, memories, and drift status."),
			),
		),
		s.handlePrepareContext,
	)

	// ── Agent Message Bus ────────────────────────────────────────────────────
	// Phase 1: direct + broadcast messaging between ephemeral agent sessions.
	// Transport: SQLite (not HTTP) — agents are ephemeral, need a broker.
	// Semantics: A2A-lite (topic-based, task-linked payloads, lifecycle: unread→read).

	// send_message
	s.addOrDefer(
		mcp.NewTool(
			"send_message",
			mcp.WithDescription(
				"Sends a message from one agent to another, or broadcasts it to all agents. "+
					"Use this to notify peers about API changes, task blockers, or plan updates. "+
					"to_agent omitted = broadcast. payload must be valid JSON.",
			),
			mcp.WithString("from_agent",
				mcp.Required(),
				mcp.Description("Self-declared sender identity (e.g. 'backend-claude')."),
			),
			mcp.WithString("topic",
				mcp.Required(),
				mcp.Description("Message topic, e.g. 'api_changed', 'task_blocked', 'plan_updated'."),
			),
			mcp.WithString("payload",
				mcp.Description("JSON payload with message details. Defaults to '{}'. Example: '{\"endpoint\":\"/api/users\",\"change\":\"added role field\"}'."),
			),
			mcp.WithString("to_agent",
				mcp.Description("Target agent ID. Omit (or leave empty) to broadcast to all agents."),
			),
			mcp.WithString("project_id",
				mcp.Description("Optional repo context identifier (e.g. 'my-backend'). Used for cross-project coordination."),
			),
			mcp.WithString("approval_token",
				mcp.Description("Required for broadcast messages (to_agent omitted). "+
					"Call without this param first to receive an approval token, "+
					"then re-call with the token after user confirmation. Expires in 5 minutes."),
			),
		),
		s.handleSendMessage,
	)

	// get_messages
	s.addOrDefer(
		mcp.NewTool(
			"get_messages",
			mcp.WithDescription(
				"Retrieves messages from the agent message bus visible to the calling agent. "+
					"Includes direct messages and broadcasts (to_agent omitted). "+
					"Use since_seq cursor for efficient polling. unread_only=true by default.",
			),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("The calling agent's ID. Only messages addressed to this agent or broadcast are returned."),
			),
			mcp.WithNumber("since_seq",
				mcp.Description("Return messages with seq greater than this value. Use 0 or omit for all. Use latest_seq from previous call as cursor."),
			),
			mcp.WithString("topic_filter",
				mcp.Description("Optional. Only return messages with this exact topic (e.g. 'api_changed')."),
			),
			mcp.WithString("unread_only",
				mcp.Description("If 'true' (default), only return unread messages. Pass 'false' to retrieve all messages including already-read ones."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum messages to return. Defaults to 50."),
			),
			mcp.WithString("mark_read_ids",
				mcp.Description("JSON array of message IDs to mark as read in the same call, e.g. [\"id1\",\"id2\"]. Replaces separate mark_read calls."),
			),
			mcp.WithString("projects",
				mcp.Description("Comma-separated project names (or \"*\" for all) to also query messages from sibling daemon-registered projects. Only works in daemon mode."),
			),
		),
		s.handleGetMessages,
	)

	// remember
	s.addOrDefer(
		mcp.NewTool(
			"remember",
			mcp.WithDescription(
				"Records a decision or failure as an episode in persistent memory. "+
					"Use episode_type='failure' + outcome='failure' to populate the Hall of Shame "+
					"so validate_plan(check_safety=true) can warn future agents. "+
					"Use episode_type='decision' for general architectural choices.",
			),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("Self-declared agent identifier (e.g. 'claude-code-session-1')."),
			),
			mcp.WithString("decision",
				mcp.Required(),
				mcp.Description("The decision made or failure observed. Concise, 1-2 sentences."),
			),
			mcp.WithString("episode_type",
				mcp.Description("One of: decision (default), failure, pattern, rule_proposal."),
			),
			mcp.WithString("outcome",
				mcp.Description("One of: success, failure, partial, unknown (default)."),
			),
			mcp.WithString("rationale",
				mcp.Description("Why this decision was made or why it failed (1-3 sentences)."),
			),
			mcp.WithString("trigger",
				mcp.Description("What prompted this episode, e.g. 'modifying auth_handler.go'."),
			),
			mcp.WithString("affected_files",
				mcp.Description("JSON array of file paths involved, e.g. '[\"cmd/server/main.go\"]'."),
			),
			mcp.WithString("affected_nodes",
				mcp.Description("Documents scope — records which graph entities were involved, for fix-task linking and entity-tier memory creation on failure/pattern episodes. "+
					"Does NOT create explicit staleness anchors (no entry in memory_anchors table). "+
					"Use anchor_nodes instead to have this memory auto-invalidate when specific entities change in the graph."),
			),
			mcp.WithString("tags",
				mcp.Description("JSON array of tags for filtering, e.g. '[\"auth\",\"breaking\"]'."),
			),
			mcp.WithString("project_id",
				mcp.Description("Repo context (leave empty to use current project)."),
			),
			mcp.WithString("anchor_nodes",
				mcp.Description("Enables staleness tracking — when anchored entities change in the code graph, this memory is auto-flagged stale. "+
					"JSON array of graph node IDs to anchor this memory to. "+
					"Use for codebase-derived facts: architecture decisions, component status, API signatures. "+
					"Example: '[\"repo::pkg/auth.go::AuthService\"]'. "+
					"Omit for durable facts (user preferences, feedback) that have no codebase anchor."),
			),
			mcp.WithString("approval_token",
				mcp.Description("Required when project_id targets a different project. "+
					"Call without this param first to receive an approval token, "+
					"then re-call with the token after user confirmation. Expires in 5 minutes."),
			),
			mcp.WithString("memory_importance",
				mcp.Description("Importance level for the knowledge memory written alongside this episode. "+
					"Use 'pinned' for critical long-lived knowledge that must never be demoted by decay scoring "+
					"(security configs, compliance decisions, architectural invariants). "+
					"Use a float string (e.g. '0.8', '1.5') for high-importance but non-critical knowledge — "+
					"values multiply the recency decay score (higher = longer-lived in recall results). "+
					"Minimum effective value is 0.05 (the decay visibility threshold); values below this are "+
					"clamped up to 0.05 automatically. Default '1.0' — decays naturally by time-since-last-access. "+
					"Pinned memories always appear in recall() regardless of age."),
			),
		),
		s.handleRemember,
	)

	// recall (absorbs get_episodes: omit query for chronological browse)
	s.addOrDefer(
		mcp.NewTool(
			"recall",
			mcp.WithDescription(
				"Searches or browses episodic memory. "+
					"With query: FTS5 BM25 semantic search, results ordered by relevance — use before starting work on a component. "+
					"Without query: chronological browse (newest first), equivalent to the deprecated get_episodes. "+
					"Also surfaces dynamic_rules derived from similar past failures. "+
					"Response may include stale_embedding_ids: memory IDs whose anchored code entity changed since the memory was written — verify these before trusting.",
			),
			mcp.WithString("query",
				mcp.Description("Natural language search query, e.g. 'auth handler redirect loop'. Omit for chronological browse."),
			),
			mcp.WithString("project_id",
				mcp.Description("Filter to episodes for this project (empty = all projects)."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Filter to episodes recorded by this agent (empty = all agents)."),
			),
			mcp.WithString("episode_type",
				mcp.Description("Filter by type: decision, failure, pattern, rule_proposal (empty = all)."),
			),
			mcp.WithString("outcome_filter",
				mcp.Description("Filter by outcome: success, failure, partial, unknown (empty = all). Search mode only."),
			),
			mcp.WithString("tags",
				mcp.Description("Comma-separated tags to filter by, e.g. 'auth,breaking'. Browse mode only."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Max episodes to return. Default 5 (search) or 20 (browse)."),
			),
			mcp.WithBoolean("include_stale",
				mcp.Description("When true, include stale (invalidated) memories in results. Default false. Use for audit queries to review full history including beliefs that were invalidated by graph changes."),
			),
			mcp.WithString("projects",
				mcp.Description("Comma-separated list of federation aliases to also search. When provided, searches sibling projects' memories too, labeling results with [alias]. Without this parameter, only local memories are searched."),
			),
			mcp.WithString("as_of",
				mcp.Description("Return memory content as it existed at this point in time. "+
					"Accepts RFC3339 (e.g. '2026-03-15T12:00:00Z') or date-only (e.g. '2026-03-15'). "+
					"Memories created after this time are excluded. "+
					"For memories with version history (updated via dedup), returns the content that was active at that time. "+
					"Search mode only."),
			),
			mcp.WithNumber("depth",
				mcp.Description("Graph traversal depth for multi-hop knowledge discovery (default 2, max 4). " +
					"depth=2: surfaces memories about entities 2 hops from query-matching anchors " +
					"(e.g. querying auth finds TokenValidator memories because AuthService -[CALLS]- TokenValidator). " +
					"depth=3: extends one more hop (AuthService -[CALLS]- TokenValidator -[CALLS]- EncryptionService). " +
					"Response includes graph_traversal.paths showing each structural connection found. " +
					"Increasing beyond 2 may surface more distant but less relevant context."),
			),
		),
		s.handleRecall,
	)

	// check_plan_safety
	s.addOrDefer(
		mcp.NewTool(
			"check_plan_safety",
			mcp.WithDescription(
				"Prefer validate_plan(check_safety=true) instead — it runs this check inline. "+
					"Searches failure episodes for the closest match to the proposed plan (Reactive Interjection). "+
					"Returns a Recovery Packet if a similar past failure is found — the agent decides relevance. "+
					"Non-blocking: returns 'clear' if no failures recorded yet or on timeout.",
			),
			mcp.WithString("plan_description",
				mcp.Required(),
				mcp.Description("Natural language description of what you plan to do, e.g. 'modify auth_provider.dart login flow without fallback'."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Self-declared agent identifier (used to record the interjection event)."),
			),
			mcp.WithString("project_id",
				mcp.Description("Repo context for scoping the failure search (empty = all projects)."),
			),
		),
		s.handleCheckPlanSafety,
	)

	// get_rule_candidates
	s.addOrDefer(
		mcp.NewTool(
			"get_rule_candidates",
			mcp.WithDescription(
				"Returns failure episodes that have appeared ≥N times and have not yet been promoted to a dynamic_rule. "+
					"Use this to close the feedback loop: review candidates, call upsert_rule() to enforce the pattern, "+
					"then call mark_episode_promoted() to mark the episode as promoted.",
			),
		),
		s.handleGetRuleCandidates,
	)

	// get_adrs
	s.addOrDefer(
		mcp.NewTool(
			"get_adrs",
			mcp.WithDescription(
				"Returns Architectural Decision Records (ADRs) from the brain. "+
					"ADRs are persistent cold-memory entries for significant design choices. "+
					"When a file param is provided, returns only accepted ADRs whose linked_files patterns match. "+
					"Requires brain.url to be configured in synapses.json.",
			),
			mcp.WithString("file",
				mcp.Description("Optional file path suffix to filter ADRs by linked_files patterns. Returns only accepted ADRs that match."),
			),
		),
		s.handleGetADRs,
	)

	// plan_context — single-call pre-implementation gate (replaces the 3-step ritual:
	// check_plan_safety → validate_plan → prepare_context(intent=plan)).
	s.addOrDefer(
		mcp.NewTool(
			"plan_context",
			mcp.WithDescription(
				"Power-user compound tool — for most agents, use validate_plan(check_safety=true) + "+
					"prepare_context(intent='plan') individually instead. "+
					"Single-call pre-implementation gate. Runs three checks in one round-trip: "+
					"(1) safety check — searches failure episodes for past matches (500ms cap); "+
					"(2) validate_plan — checks proposed changes against architectural rules; "+
					"(3) prepare_context(intent=plan) — scope assessment: files, interfaces, risk level. "+
					"Returns a verdict: 'clear' | 'warnings' | 'violations' | 'blocked'. "+
					"Find this tool via discover_tools(query='plan implementation gate').",
			),
			mcp.WithString("target",
				mcp.Required(),
				mcp.Description("Entity name, file path, or concept to assess scope for (e.g. 'AuthService', 'internal/auth')."),
			),
			mcp.WithString("changes",
				mcp.Description("JSON array of proposed changes for structural validation. Same format as validate_plan. Omit to skip rule checking."),
			),
			mcp.WithString("plan_description",
				mcp.Description("Natural language description for the safety check (e.g. 'refactor auth to remove direct DB access'). Defaults to target if omitted."),
			),
			mcp.WithString("file",
				mcp.Description("Optional file path suffix to disambiguate entity names."),
			),
			mcp.WithString("task_id",
				mcp.Description("Optional task ID for relevance boosting in scope assessment."),
			),
		),
		s.handlePlanContext,
	)

}
