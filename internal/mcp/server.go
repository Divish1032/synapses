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
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/scout"
	"github.com/SynapsesOS/synapses/internal/skills"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/SynapsesOS/synapses/internal/watcher"
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

// WatcherHealthChecker is an optional interface that ChangeSource implementations
// may satisfy to report whether the file-watching event loop is still alive.
type WatcherHealthChecker interface {
	IsAlive() bool
}

// NodeEmbedderSetter is an optional interface that ChangeSource implementations
// may satisfy to accept a node embedder for Tier 1 embedding-based entity resolution.
// Implemented by *watcher.Watcher; checked via type assertion in SetMemoryEmbedder.
type NodeEmbedderSetter interface {
	SetNodeEmbedder(embed.Embedder)
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
	pkt       *brain.ContextPacket
	expiresAt time.Time
}

// Server holds the MCP server and the dependencies that tool handlers need.
type Server struct {
	mcp                *server.MCPServer
	graph              *graph.Graph
	config             *config.Config
	store              *store.Store           // nil if started without a persistent store
	changeSource       ChangeSource           // nil if started without a file watcher
	federationResolver *federation.Resolver   // nil if no federation configured — set via SetFederationResolver
	projectRegistry    ProjectStoreProvider   // nil in single-project mode — set via SetProjectRegistry
	brainClient        *brain.Client          // set via SetBrainClient; nil if brain not configured
	pulseClient        *pulse.Client          // set via SetPulseClient; nil if pulse not configured
	embedClient        embed.Embedder          // nil if embeddings not configured
	memoryEmbedder     embed.Embedder         // nil if embeddings mode is "off" — set via SetMemoryEmbedder
	techStack          []scout.TechStackEntry // set via SetTechStack after autosubscribe; nil if not detected
	injectionScanner   *InjectionScanner      // prompt injection scanner for externally-sourced content (nil = disabled)
	knowledgeMode      bool                   // when true, only knowledge tools are registered (no code graph)
	projectID          string                 // stable project identifier (FNV hash of project root path)
	projectPath        string                 // absolute path to the project root (for go.mod parsing)
	rulesMu            sync.RWMutex           // protects s.config.Rules for concurrent dynamic upserts
	sdlcDetect         *sdlcDetector          // Sprint 27.1: auto-detects SDLC phase from tool-call patterns
	toolTracker        *sessionToolTracker    // Sprint 27.3: per-session tool call counts for suggestion suppression
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

	// P5 — SA-C1: per-session tool position counter for sequence capture.
	// Key: MCP sessionID. Protected by sessionCallsMu (reuse existing lock).
	toolPositions map[string]int

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

	// lifecycleCtx is cancelled by Close() to bound embedding and other
	// background work to the server lifetime. Background ops that accept a
	// context should use this rather than context.Background().
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc

	// Bounded background worker pool for fire-and-forget operations.
	// All handler goroutines (telemetry, embedding, store writes) go through
	// goBackground() which enqueues work items. Fixed workers drain the queue.
	// Close() rejects new work, closes the queue, and waits for workers to
	// drain remaining items — preventing goroutines from racing with Store.Close().
	bgQueue    chan func()  // buffered work queue (cap bgQueueCap)
	bgDrops    atomic.Int64 // BUG-020: total work items dropped due to full queue
	shutdownMu sync.RWMutex // guards bgClosed + bgQueue sends
	bgClosed   bool         // true after Close() — rejects new work

	// failedEmbedIDs tracks memory IDs whose embedding was dropped due to a
	// full background queue. A 60s retry goroutine re-embeds them. Bounded to
	// 256 entries to prevent unbounded growth.
	// Key: memoryID (string), Value: struct{}.
	failedEmbedIDs sync.Map

	// ledgerWatermarks tracks per-session deduplication state for cross-session alerts.
	// Key: Synapses session ID (string), Value: *ledgerWatermark.
	ledgerWatermarks sync.Map

	// recallFootprints tracks recent recall results per session for recall-to-action
	// quality correlation. Key: Synapses session ID (string), Value: *recallFootprintRing.
	recallFootprints sync.Map

	// formatterConvsMu protects formatterConvsPtr, which caches the result of
	// detectFormatterConventions so the filesystem stats run only once per
	// server lifetime (not on every session_init call).
	// nil = not yet computed; non-nil = computed (may point to an empty slice).
	formatterConvsMu  sync.Mutex
	formatterConvsPtr *[]string

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
	// toolDescs captures name → (description + "\x00" + JSON(inputSchema)) at
	// addOrDefer time, so both the tool description and parameter schemas are
	// included in the integrity baseline.
	// toolDescBaseline is the SHA256 hex of all sorted entries, computed once at
	// the end of registerTools(). handleSessionInit re-derives the hash from
	// toolDescs and compares it to detect runtime tampering of descriptions or
	// parameter definitions.
	toolDescs        map[string]string
	toolDescBaseline string

	// Sprint 15 #4: recall channel weight learning — rate controls.
	//
	// recallStatsLastNs is the unix-nanosecond timestamp of the last
	// UpdateRecallChannelStats trigger. CAS-updated so multiple concurrent
	// recall_hit events debounce to at most one aggregation per interval.
	recallStatsLastNs atomic.Int64

	// recallWeightsMu guards recallWeightsCache and recallWeightsCachedAt.
	// recallChannelWeights() reads from SQLite at most once per cache TTL
	// to keep the hot recall path free of per-call database round-trips.
	recallWeightsMu       sync.RWMutex
	recallWeightsCache    map[string]float64
	recallWeightsCachedAt time.Time

	// updateChecker is an optional function that returns the pending update
	// version string, or "" if up to date. Set via SetUpdateChecker.
	// Used by session_init to include an update_available hint.
	updateChecker func() string
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
// Returns true if the work was queued, false if it was dropped.
func (s *Server) goBackground(fn func()) bool {
	s.shutdownMu.RLock()
	if s.bgClosed {
		s.shutdownMu.RUnlock()
		return false
	}
	select {
	case s.bgQueue <- fn:
		// queued successfully
		if pc := s.getPulseClient(); pc != nil {
			pc.RecordBackgroundWorkerEnqueue()            // P2-5
			pc.RecordBackgroundQueueDepth(len(s.bgQueue)) // P9-4
		}
		s.shutdownMu.RUnlock()
		return true
	default:
		s.bgDrops.Add(1) // BUG-020: track drops for health endpoint
		logutil.Warn("synapses: background queue full (%d), dropping work\n", bgQueueCap)
		if pc := s.getPulseClient(); pc != nil {
			pc.RecordBackgroundWorkerDrop() // P2-5
		}
	}
	s.shutdownMu.RUnlock()
	return false
}

// getConfig returns a snapshot of the server config, safe for concurrent use.
// BUG-037: rulesMu protects config pointer from concurrent hot-reload replacement.
func (s *Server) getConfig() *config.Config {
	s.rulesMu.RLock()
	cfg := s.config
	s.rulesMu.RUnlock()
	return cfg
}

// Config returns the current server config, safe for concurrent use.
// Used by the hibernation sweeper to read per-project hibernate thresholds.
func (s *Server) Config() *config.Config {
	return s.getConfig()
}

// BackgroundQueueStats returns the current queue depth and total drop count
// for health endpoint reporting (BUG-020).
func (s *Server) BackgroundQueueStats() (depth int, drops int64) {
	return len(s.bgQueue), s.bgDrops.Load()
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
	model   string // agent model from session_init (e.g. "claude-opus-4-6")
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

// getSynapseAgentID returns the agent_id associated with an MCP session,
// or "" if the session hasn't called session_init yet.
func (s *Server) getSynapseAgentID(mcpSessionID string) string {
	s.synapseSessionsMu.RLock()
	entry, ok := s.synapsesSessions[synapseSessionKey(mcpSessionID)]
	s.synapseSessionsMu.RUnlock()
	if !ok {
		return ""
	}
	return entry.agentID
}

// registerSynapseSession associates a newly created Synapses session with an
// MCP connection. Called from handleSessionInit after CreateSession succeeds.
func (s *Server) registerSynapseSession(mcpSessionID, synapseSessionID, agentID, model string) {
	s.synapseSessionsMu.Lock()
	s.synapsesSessions[synapseSessionKey(mcpSessionID)] = &synapseSessionEntry{
		id:      synapseSessionID,
		agentID: agentID,
		model:   model,
	}
	s.synapseSessionsMu.Unlock()
}

// modelBudgetMultiplier returns a token budget multiplier based on the agent's
// declared model. Large-context models get richer context automatically.
// Unknown models default to 1.0 (no change).
func modelBudgetMultiplier(model string) float64 {
	// Normalize: strip suffixes like date stamps (e.g. "claude-sonnet-4-6-20250514")
	// by matching known prefixes.
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "claude-opus"):
		return 2.0
	case strings.HasPrefix(m, "claude-sonnet"):
		return 1.5
	case strings.HasPrefix(m, "gpt-4o-mini"):
		return 0.5
	case strings.HasPrefix(m, "gpt-4o"):
		return 1.0
	case strings.HasPrefix(m, "gpt-4.1-mini"), strings.HasPrefix(m, "gpt-4.1-nano"):
		return 0.5
	case strings.HasPrefix(m, "gpt-4.1"):
		return 1.5
	case strings.HasPrefix(m, "claude-haiku"):
		return 0.75
	case strings.HasPrefix(m, "gemini-2.5-pro"), strings.HasPrefix(m, "gemini-2.5-flash"):
		return 1.5
	default:
		return 1.0
	}
}

// getSessionBudgetMultiplier returns the token budget multiplier for the
// current MCP session based on the model declared in session_init.
// Returns 1.0 when no session or model is known.
func (s *Server) getSessionBudgetMultiplier(ctx context.Context) float64 {
	mcpSessionID := SessionIDFromContext(ctx)
	s.synapseSessionsMu.RLock()
	entry, ok := s.synapsesSessions[synapseSessionKey(mcpSessionID)]
	s.synapseSessionsMu.RUnlock()
	if !ok || entry.model == "" {
		return 1.0
	}
	return modelBudgetMultiplier(entry.model)
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

// ActiveSessionCount returns the number of active MCP sessions tracked by the
// loop guard. Used by the hibernation sweeper to avoid hibernating projects
// with active IDE/agent connections.
func (s *Server) ActiveSessionCount() int {
	return s.lg.ActiveSessionCount()
}

// ctxCallEntry tracks how many times an agent requested context for an entity.
// lastAt is updated on every call so timing-based signal classification can
// compute the interval between the previous delivery and the current refetch.
type ctxCallEntry struct {
	count   int
	firstAt time.Time
	lastAt  time.Time
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
		toolPositions:    make(map[string]int),
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
		sdlcDetect:       newSDLCDetector(),
		toolTracker:      newSessionToolTracker(),
	}
	s.lifecycleCtx, s.lifecycleCancel = context.WithCancel(context.Background())

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
		// P8-9: capture topic for send_message so message distribution is tracked.
		if entity == "" {
			entity, _ = req.GetArguments()["topic"].(string)
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
			var errorMsg string
			if result != nil && !result.IsError && len(result.Content) > 0 {
				if tc, ok := result.Content[0].(mcp.TextContent); ok {
					responseBytes = len(tc.Text)
				}
			}
			if result != nil && result.IsError && len(result.Content) > 0 {
				if tc, ok := result.Content[0].(mcp.TextContent); ok {
					errorMsg = tc.Text
					if len(errorMsg) > 200 {
						// Truncate at a valid UTF-8 boundary.
						errorMsg = truncateUTF8(errorMsg, 200)
					}
				}
			}
			pulseSessID := s.getSynapseSessionID(SessionIDFromContext(ctx))
			// P5 — Item 26: serialize request params (truncated for storage).
			var reqParams string
			if args := req.GetArguments(); len(args) > 0 {
				if b, jErr := json.Marshal(args); jErr == nil {
					reqParams = string(b)
					if len(reqParams) > 512 {
						reqParams = reqParams[:512]
					}
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
				SessionID:     pulseSessID,
				ErrorMessage:  errorMsg,
				RequestParams: reqParams,
			}
			s.goBackground(func() { pc.RecordToolCall(evt) })
		}
		// P5 — SA-C1: record tool sequence entry for workflow analysis.
		if pc := s.getPulseClient(); pc != nil {
			sessID := s.getSynapseSessionID(SessionIDFromContext(ctx))
			if sessID != "" {
				pos := s.nextToolPosition(SessionIDFromContext(ctx))
				toolName := req.Params.Name
				ok := success
				s.goBackground(func() { pc.RecordToolSequenceEntry(sessID, toolName, pos, ok) })
			}
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
		server.WithPromptCapabilities(false),        // static prompts; no listChanged notifications
		server.WithHooks(hooks),
		server.WithRecovery(), // recover panics in tool handlers — prevents daemon crash
	)

	// Knowledge mode detection: explicit config setting.
	// Must be set BEFORE registerTools() so addOrDefer sees knowledgeMode=true
	// and registers stubs for graph-dependent tools.
	if cfg != nil && cfg.Mode == "knowledge" {
		s.knowledgeMode = true
	}

	s.registerTools()
	s.registerResources()
	s.registerPrompts() // no-op until SetPromptTemplates is called
	// OF-S4: compute baseline AFTER all registrations.
	// handleSessionInit re-derives and compares to detect runtime tampering.
	s.toolDescBaseline = hashToolDescs(s.toolDescs)
	return s
}

// hashToolDescs computes a deterministic SHA256 hex digest of all tool
// name→(description+schema) pairs. Names are sorted alphabetically before
// hashing so the result is independent of registration order. The value
// includes both the tool description and the serialised input schema so that
// parameter-level tampering (renamed params, altered descriptions) is caught
// alongside top-level description changes. Called once at startup to establish
// the baseline, and re-called in handleSessionInit to detect drift.
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
func (s *Server) getPacketFromCache(key string) *brain.ContextPacket {
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
func (s *Server) setPacketCache(key string, pkt *brain.ContextPacket) {
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
func (s *Server) SetBrainClient(bc *brain.Client) {
	s.brainClient = bc
}

// SetProjectID sets the stable project identifier used when building context packets.
// Also propagates the ID to loop_guard and rate_limiter for guard event attribution.
func (s *Server) SetProjectID(id string) {
	s.projectID = id
	s.lg.projectID = id
	s.rl.projectID = id
}

// SetProjectPath stores the absolute project root path so that search
// can inject source snippets from the filesystem.
func (s *Server) SetProjectPath(path string) {
	s.projectPath = path
}

// getProjectRoot returns the best available project root path.
// Tries graph.Root() first (set by indexer), then projectPath (set by daemon).
// Safe to call when s.graph is nil (knowledge mode or tests).
func (s *Server) getProjectRoot() string {
	if s.graph != nil {
		if r := s.graph.Root(); r != "" {
			return r
		}
	}
	return s.projectPath
}

// cachedFormatterConventions returns the formatter conventions for the project
// root. The result is computed once on the first call where the project root is
// non-empty, then cached for the server's lifetime — satisfying the "one-time
// detection" intent without requiring graph schema changes.
//
// Thread-safe: concurrent session_init calls will block briefly on the first
// computation, then read from the cache without contention.
func (s *Server) cachedFormatterConventions() []string {
	root := s.getProjectRoot()
	if root == "" {
		return nil
	}
	s.formatterConvsMu.Lock()
	defer s.formatterConvsMu.Unlock()
	if s.formatterConvsPtr == nil {
		val := detectFormatterConventions(root)
		s.formatterConvsPtr = &val
	}
	return *s.formatterConvsPtr
}

// SetPulseClient wires a *pulse.Client into the server so that every tool
// call emits telemetry to the synapses-pulse analytics sidecar.
// Also propagates the pulse client to loop_guard and rate_limiter so they
// can emit guard events (P3-2, P3-3).
func (s *Server) SetPulseClient(pc *pulse.Client) {
	s.pulseClient = pc
	s.lg.SetPulseClient(pc)
	s.rl.SetPulseClient(pc)
	s.lg.projectID = s.projectID
	s.rl.projectID = s.projectID
	// P8-2: wire session resolver so guard events include Synapses session UUID.
	resolver := func(mcpSessID string) string {
		return s.getSynapseSessionID(mcpSessID)
	}
	s.lg.resolveSession = resolver
	s.rl.resolveSession = resolver
	// Agent-scoped rate limiting: same agent_id across reconnections shares one bucket.
	s.rl.resolveAgent = func(mcpSessID string) string {
		return s.getSynapseAgentID(mcpSessID)
	}
}

// getPulseClient returns the stored pulse client, or nil if not configured.
func (s *Server) getPulseClient() *pulse.Client {
	return s.pulseClient
}

// SetTechStack stores the detected tech stack entries so that
// get_project_identity can surface them as tech_stack.
// Called from cmdStart after autosubscribe detection completes.
func (s *Server) SetTechStack(ts []scout.TechStackEntry) {
	s.techStack = ts
}

// SetEmbedClient wires an embedder so handleSemanticSearch can
// perform vector similarity search in addition to FTS5 BM25 ranking.
// Accepts any embed.Embedder (builtin ONNX, external HTTP client, etc.).
// Pass nil to disable vector search (falls back to FTS5-only).
func (s *Server) SetEmbedClient(ec embed.Embedder) {
	s.embedClient = ec
}

// SetMemoryEmbedder wires an embedder for generating memory embeddings
// on remember() writes and for vector search in recall(). Pass nil to
// disable memory embeddings (FTS5-only recall).
// Also wires the embedder into the store's semantic dedup: when Jaccard
// similarity is inconclusive, the store uses the embedder to compare
// new content against existing memory embeddings.
func (s *Server) SetMemoryEmbedder(e embed.Embedder) {
	s.memoryEmbedder = e
	if s.store != nil && e != nil {
		s.store.SetSemanticDedupFunc(func(text string) ([]float32, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return e.Embed(ctx, text)
		})
	} else if s.store != nil {
		s.store.SetSemanticDedupFunc(nil)
	}
	// Wire the same embedder into the watcher for Tier 1 node embedding.
	// Uses an optional interface so this compiles without importing watcher directly.
	if setter, ok := s.changeSource.(NodeEmbedderSetter); ok {
		setter.SetNodeEmbedder(e)
	}

}

// SetUpdateChecker sets the function that returns the pending update version.
func (s *Server) SetUpdateChecker(fn func() string) {
	s.updateChecker = fn
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
								if pc := s.getPulseClient(); pc != nil {
									pc.RecordBackgroundWorkerPanic() // P2-5
								}
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
			// Periodic sweep for dropped memory embeddings (#23).
			// Catches embeddings lost due to background queue pressure.
			if s.memoryEmbedder != nil {
				embedder := s.memoryEmbedder
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()
					s.embedSweepLoop(embedder, st)
				}()
				// Retry goroutine for embeds dropped due to full bgQueue.
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()
					s.failedEmbedRetryLoop(embedder, st)
				}()
			}
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

	// 2. Signal long-running loops (memoryExpiryLoop) and cancel lifecycle context.
	close(s.stopCh)
	s.lifecycleCancel()

	if s.lg != nil {
		s.lg.close()
	}
	if s.rl != nil {
		s.rl.close()
	}

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
	_ = st.PruneExpiredWebCache()

	for {
		select {
		case <-ticker.C:
			st.ExpireMemories()
			_ = st.PruneExpiredWebCache()
		case <-s.stopCh:
			return
		}
	}
}

// embedSweepLoop periodically checks for memories missing embeddings (dropped
// by background queue pressure) and re-embeds them. Runs every 5 minutes.
func (s *Server) embedSweepLoop(embedder embed.Embedder, st *store.Store) {
	if st == nil || embedder == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ids, err := st.GetMemoriesWithoutEmbeddings(50)
			if err != nil || len(ids) == 0 {
				continue
			}
			logutil.Info("synapses: embed sweep: %d memories missing embeddings\n", len(ids))
			for _, memID := range ids {
				content, ok := st.GetMemoryContent(memID)
				if !ok || content == "" {
					continue
				}
				if !s.goBackground(func() { s.embedMemory(s.lifecycleCtx, embedder, st, memID, content) }) {
					s.trackFailedEmbed(memID)
				}
			}
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
// Sprint 23.9: consolidated to 8 tools. No tiers needed — all tools are always available.
var coreTierTools = map[string]bool{
	"session_init": true,
	"search":       true,
	"get_context":  true,
	"get_impact":   true,
	"validate":     true,
	"memory":       true,
	"end_session":  true,
	"tasks":        true,
}

// standardTierTools = coreTierTools (all 8 tools are core after Sprint 23.9 consolidation).
var standardTierTools = map[string]bool{
	"session_init": true,
	"search":       true,
	"get_context":  true,
	"get_impact":   true,
	"validate":     true,
	"memory":       true,
	"end_session":  true,
	"tasks":        true,
}

// knowledgeTools are the tools available when the server runs in knowledge mode
// (no code graph). All other tools return a clear error message.
// Sprint 24: removed deleted/absorbed tools.
var knowledgeTools = map[string]bool{
	"session_init": true,
	"end_session":  true,
	"memory":       true, // remember + recall + get_episodes
	"tasks":        true, // create_plan + get_plans + get_pending_tasks + update_task + save/get_session_state + link_task_nodes
	"validate":     true, // includes check_plan_safety (phase=safety)
}

// hiddenTools are deprecated or subsumed tools that remain callable but are
// filtered from tools/list to reduce the default tool surface.
// Sprint 24: many tools removed/absorbed — only plan_context remains hidden.
var hiddenTools = map[string]bool{
	// Sprint 25: all tools now merged — no hidden tools remain.
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
	// OF-S4: capture description + input schema for integrity baseline.
	// Encoding the full schema (parameter names, types, descriptions) alongside
	// the tool description means parameter-level tampering is also detected —
	// not just changes to the top-level tool description.
	schemaBytes, _ := json.Marshal(t.InputSchema)
	s.toolDescs[t.Name] = t.Description + "\x00" + string(schemaBytes)
	if s.knowledgeMode && !knowledgeTools[t.Name] {
		// In knowledge mode, register a stub that returns a clear error.
		stub := func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError(fmt.Sprintf(
				"Tool %q is not available in knowledge mode (no code graph). "+
					"Available tools: session_init, end_session, memory, tasks, validate.", t.Name)), nil
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

	// Sprint 24: Work Ledger wrapper — passively records entity/file signals
	// from every tool call and injects cross-session overlap alerts.
	toolName := t.Name
	ledgerWrapped := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		result, err := guarded(ctx, req)
		if err != nil || result == nil || s.store == nil {
			return result, err
		}

		args := req.GetArguments()
		entities, files := extractSignals(toolName, args)
		sessionID := s.getSynapseSessionID(SessionIDFromContext(ctx))
		if sessionID == "" {
			return result, nil
		}

		// Async write — never blocks the response
		if len(entities) > 0 || len(files) > 0 {
			entry := store.LedgerEntry{
				SessionID: sessionID,
				ProjectID: s.projectID,
				ToolName:  toolName,
				EntityIDs: entities,
				FilePaths: files,
			}
			s.goBackground(func() {
				_ = s.store.AppendLedger(entry)
			})
		}

		// Sprint 24.1: Exploration log — capture response-side findings for
		// compaction recovery ("what was found", not just "what was touched").
		// Only fires for the 4 exploration-heavy tools; async, never blocks.
		if cap := extractExplorationCapture(toolName, args, result); cap != nil {
			elogEntry := store.ExplorationEntry{
				SessionID:      sessionID,
				ProjectID:      s.projectID,
				ToolName:       toolName,
				EntityQueried:  cap.EntityQueried,
				QueryContext:   cap.QueryContext,
				FindingSummary: cap.FindingSummary,
			}
			s.goBackground(func() {
				_ = s.store.AppendExplorationEntry(elogEntry)
			})
		}

		// Sprint 27.3: Track tool calls per session for suggestion suppression.
		s.toolTracker.record(sessionID, toolName)

		// Sprint 27.1: SDLC auto-detection from tool-call patterns.
		if phase, mode, changed := s.sdlcDetect.recordCall(toolName, entities, files); changed {
			if bc := s.brainClient; bc != nil {
				p, m := phase, mode
				s.goBackground(func() {
					_ = bc.SetSDLCPhaseIfAuto(p, m, s.getLastAgent())
				})
			}
		}

		// Sync overlap check — fast indexed query, <1ms
		if len(entities) > 0 || len(files) > 0 {
			alerts := s.checkOverlaps(sessionID, entities, files)
			alerts = s.filterSeenAlerts(sessionID, alerts)
			if len(alerts) > 0 {
				injectAlerts(result, alerts)
			}
		}

		// Recall-to-action correlation: check if this tool call touches
		// entities/files from a recent recall. Fire-and-forget to Pulse.
		if len(entities) > 0 || len(files) > 0 {
			if fp, weight := s.checkRecallActedOn(sessionID, entities, files); fp != nil {
				if pc := s.getPulseClient(); pc != nil {
					pc.RecordRecallEffectiveness(pulse.RecallEffectivenessEvent{
						RecallID:         fp.RecallID,
						Query:            fp.Query,
						ResultCount:      fp.ResultCount,
						ActedOn:          true,
						ActedOnWeight:    weight,
						TopChannel:       fp.TopChannel,
						CrossProjectHits: fp.CrossProjectHits,
						SessionID:        sessionID,
						ProjectID:        s.projectID,
					})
				}
			}
		}

		return result, nil
	}

	s.mcp.AddTool(t, ledgerWrapped)
	s.toolHandlersMu.Lock()
	s.toolHandlers[t.Name] = ledgerWrapped
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
				"CALL FIRST at the start of every session — before reading any files. "+
					"Returns pending tasks, unfinished work, recent decisions, and project conventions. "+
					"Without this, you'll miss in-progress work and re-explore context that's already been captured. "+
					"scope='standard' (default): tasks + working state + scale guidance (~500 tokens). "+
					"scope='full': adds project identity, memories, and health stats. "+
					"scope='compaction': recovery briefing after context compaction — restores task state, "+
					"explored files, and recent decisions. "+
					"scope='resume': task continuity after reconnect. "+
					"Provide agent_id for incremental delivery — unchanged sections skipped on repeat calls. "+
					"Safety alerts (cross_project_alerts, agent_awareness, tool_integrity_alert) always included.",
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
					"\"standard\" (default): tasks + working_state + scale_guidance (~500 tokens); "+
					"rich sections are deferred — see more_available in the response for what is available. "+
					"\"full\": all sections; use when you need project_identity, brain_health, federation_health, "+
					"relevant_memories, knowledge_graph, or other rich sections. Empty arrays still omitted. "+
					"\"quick\": alias for standard (legacy). "+
					"\"resume\": tasks with session states + working_state + relevant_memories + federation_health "+
					"(for task context continuity across reconnects). "+
					"\"compaction\": use after context compaction — returns compaction_recovery with work summary, "+
					"session decisions/failures, applicable rules/violations, entity memories, and context snapshot. "+
					"Safety-critical alerts (cross_project_alerts, agent_awareness, tool_integrity_alert) are always included in all scopes."),
			),
		),
		s.handleSessionInit,
	)

	// Sprint 23.9: get_compaction_guide removed — replaced by automatic compaction recovery
	// via session_init(scope="compaction"). See Sprint 24.2 for recovery packet design.

	// Sprint 24: report_usage absorbed into end_session.
	// Sprint 24: get_my_analytics moved to MCP Resource synapses://analytics.
	// Sprint 24: explain_codebase, get_repo_map, get_project_identity absorbed into session_init.
	// Sprint 24: discover_tools removed — unnecessary with 12 tools.
	// Sprint 24: get_edge_types moved to MCP Resource synapses://edge-types.

	// ── Code Graph Tools ────────────────────────────────────────────────────

	// get_context (absorbs prepare_context via mode=intent, get_call_chain via mode=path)
	s.addOrDefer(
		mcp.NewTool(
			"get_context",
			mcp.WithDescription(
				"CALL before writing, debugging, or reviewing code that touches a known entity. "+
					"Returns relationships, callers, callees, entity signatures, and attached memories — "+
					"what reading files alone cannot tell you. "+
					"Without this, you'll miss hidden callers and make breaking changes you didn't see coming. "+
					"mode='context' (default): graph neighborhood traversal. "+
					"mode='intent': one-call context assembly for a declared goal (understand/modify/add/debug). "+
					"mode='path': shortest call chain between two entities. "+
					"mode='investigate': ranks suspicious entity locations by relevance to a described problem.",
			),
			mcp.WithString("entity",
				mcp.Description("Entity name for mode=context (e.g. 'AuthService'). Required for mode=context."),
			),
			mcp.WithString("mode",
				mcp.Description("'context' (default): BFS graph traversal. 'intent': intent-based context assembly. 'path': call chain between two entities."),
			),
			// mode=context params
			mcp.WithNumber("depth",
				mcp.Description("BFS hop limit (mode=context). Defaults to project config value."),
			),
			mcp.WithNumber("token_budget",
				mcp.Description("Max tokens in response. Defaults to 4000 (context) or intent-specific."),
			),
			mcp.WithString("task_id",
				mcp.Description("Optional task ID for relevance boosting."),
			),
			mcp.WithString("file",
				mcp.Description("Optional file path suffix to disambiguate entity names."),
			),
			mcp.WithString("format",
				mcp.Description("Output format: 'compact' (default) or 'json'. Mode=context only."),
			),
			mcp.WithString("detail_level",
				mcp.Description("Verbosity for format=compact: 'summary', 'neighbors', 'full' (default). Mode=context only."),
			),
			mcp.WithBoolean("helpful",
				mcp.Description("Feedback signal (true=useful, false=missed). Mode=context only."),
			),
			mcp.WithBoolean("include_inferred",
				mcp.Description("When false, strips inferred route nodes. Default true. Mode=context only."),
			),
			mcp.WithString("known_hash",
				mcp.Description("Entity hash for conditional fetch. Mode=context only."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Agent ID for peer tracking."),
			),
			mcp.WithString("intent",
				mcp.Description("For mode=context: shapes traversal weights. For mode=intent: required — 'modify'|'understand'|'review'|'debug'|'add'|'plan'."),
			),
			mcp.WithString("projects",
				mcp.Description("Comma-separated federation aliases for cross-project context."),
			),
			mcp.WithNumber("cross_domain_decay",
				mcp.Description("Cross-domain relevance multiplier (0,1]. Default 0.5. Mode=context only."),
			),
			// mode=intent params
			mcp.WithString("target",
				mcp.Description("Entity name, file path, or query. Required for mode=intent."),
			),
			// mode=path params
			mcp.WithString("from",
				mcp.Description("Starting entity for call chain. Required for mode=path."),
			),
			mcp.WithString("to",
				mcp.Description("Target entity for call chain. Required for mode=path."),
			),
			// mode=investigate params
			mcp.WithString("problem",
				mcp.Description("Free-text problem/bug description. Required for mode=investigate. Returns ranked code blocks with source."),
			),
			mcp.WithNumber("max_blocks",
				mcp.Description("Max code blocks to return (mode=investigate). Default 10, max 25."),
			),
			mcp.WithBoolean("include_tests",
				mcp.Description("Include test files in results (mode=investigate). Default false."),
			),
		),
		s.handleGetContextDispatch,
	)

	// Sprint 25: find_entity absorbed into search(mode="exact").

	// validate (merges validate_plan, verify_implementation, get_violations, plan_context, check_plan_safety)
	s.addOrDefer(
		mcp.NewTool(
			"validate",
			mcp.WithDescription(
				"CALL before writing code (phase=pre) and after writing code (phase=post) to catch violations early. "+
					"Without phase=pre, you may write code that breaks rules you didn't know existed. "+
					"Without phase=post, violations in written files go undetected until CI. "+
					"phase='pre' (default): check proposed changes against rules before writing. "+
					"phase='post': audit written files for new violations. "+
					"phase='list': active violations. "+
					"phase='full': compound gate — scope + safety + rules in one call. "+
					"phase='safety': check failure history for similar past mistakes. "+
					"Rule management: phase='upsert_rule' / 'delete_rule' / 'candidates'. "+
					"Decision records: phase='upsert_adr' / 'list_adrs'.",
			),
			mcp.WithString("phase",
				mcp.Description("'pre' (default), 'post', 'list', 'full', 'safety', "+
					"'upsert_rule', 'delete_rule', 'candidates', 'upsert_adr', 'list_adrs'."),
			),
			// phase=pre params
			mcp.WithString("changes",
				mcp.Description("JSON array of proposed changes. Required for phase=pre/full."),
			),
			mcp.WithBoolean("check_safety",
				mcp.Description("When true with phase=pre, also runs failure-episode safety check."),
			),
			mcp.WithString("plan_description",
				mcp.Description("Natural language plan description. Used by phase=pre (safety) and phase=safety/full."),
			),
			mcp.WithBoolean("skip_logic_checks",
				mcp.Description("Skip heuristic logic checks. Phase=pre only. Default false."),
			),
			// phase=post params
			mcp.WithString("files_written",
				mcp.Description("JSON array of written file paths. Required for phase=post."),
			),
			mcp.WithString("task_id",
				mcp.Description("Optional task ID for phase=post verification or phase=full relevance."),
			),
			// phase=list params
			mcp.WithString("rule_id",
				mcp.Description("Filter violations to a rule ID. Phase=list only."),
			),
			mcp.WithBoolean("include_log",
				mcp.Description("Include historical violation log. Phase=list only."),
			),
			mcp.WithNumber("log_limit",
				mcp.Description("Max log entries. Phase=list only. Default 50."),
			),
			// phase=full params
			mcp.WithString("target",
				mcp.Description("Entity/file for scope assessment. Required for phase=full."),
			),
			mcp.WithString("file",
				mcp.Description("File path suffix to disambiguate. Phase=full only."),
			),
			// phase=safety params
			mcp.WithString("agent_id",
				mcp.Description("Agent identifier for attribution."),
			),
			mcp.WithString("project_id",
				mcp.Description("Repo context for scoping failure search. Phase=safety only."),
			),
			// phase=upsert_rule params (absorbed from rules tool)
			mcp.WithString("description",
				mcp.Description("Rule description. Required for phase=upsert_rule. Gap description for phase=upsert_adr context."),
			),
			mcp.WithString("severity",
				mcp.Description("'error' or 'warning' for phase=upsert_rule. 'low|medium|high|critical' for memory gaps."),
			),
			mcp.WithString("edge_type",
				mcp.Description("Edge type to forbid. phase=upsert_rule."),
			),
			mcp.WithString("from_file_pattern",
				mcp.Description("Source file glob. phase=upsert_rule."),
			),
			mcp.WithString("to_file_pattern",
				mcp.Description("Target file glob. phase=upsert_rule."),
			),
			mcp.WithString("to_name_pattern",
				mcp.Description("Target entity name substring. phase=upsert_rule."),
			),
			mcp.WithString("path_pattern",
				mcp.Description("Comma-separated edge sequence for multi-hop checks. phase=upsert_rule."),
			),
			mcp.WithString("context_source",
				mcp.Description("Provenance. Rejects 'external'/'generated'. phase=upsert_rule."),
			),
			// phase=upsert_adr / list_adrs params
			mcp.WithString("id",
				mcp.Description("ADR ID (kebab-case). Required for phase=upsert_adr."),
			),
			mcp.WithString("title",
				mcp.Description("ADR title. Required for phase=upsert_adr."),
			),
			mcp.WithString("decision",
				mcp.Description("The decision made. Required for phase=upsert_adr."),
			),
			mcp.WithString("adr_status",
				mcp.Description("proposed|accepted|deprecated|superseded. phase=upsert_adr."),
			),
			mcp.WithString("context",
				mcp.Description("Problem context. phase=upsert_adr."),
			),
			mcp.WithString("consequences",
				mcp.Description("Trade-offs. phase=upsert_adr."),
			),
			mcp.WithArray("linked_files",
				mcp.Description("File patterns for ADR. phase=upsert_adr."),
				mcp.Items(map[string]any{"type": "string"}),
			),
		),
		s.handleValidateDispatch,
	)

	// Sprint 25: upsert_gap absorbed into annotate(action="add_gap").
	// Sprint 25: get_gaps absorbed into annotate(action="list_gaps").

	// Sprint 23.9: get_file_context removed — agent reads files directly; entity lookup
	// is served by search(mode="exact") and get_context.

	// search (absorbs semantic_search via mode param, find_entity via mode=exact)
	s.addOrDefer(
		mcp.NewTool(
			"search",
			mcp.WithDescription(
				"CALL when you need to find an entity by name or concept before exploring it. "+
					"Returns entity names and file:line locations — the map, not the territory. "+
					"Without this, you'll grep source files directly and miss graph-indexed relationships and relevance signals. "+
					"mode='keyword' (default): substring match across entity names. "+
					"mode='fulltext': BM25 ranked full-text search. "+
					"mode='semantic': concept-based vector search — describe what you're looking for, not the exact name. "+
					"mode='exact': precise name-to-node-ID lookup — use before get_context when you have the exact name.",
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Search term (case-insensitive)."),
			),
			mcp.WithString("mode",
				mcp.Description("'keyword' (default), 'fulltext', 'semantic', or 'exact' (name lookup)."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum results to return (default 20, max 50). Used for fulltext/semantic."),
			),
			mcp.WithBoolean("hyde",
				mcp.Description("HyDE hypothesis generation (default true). Only applies when mode='semantic'."),
			),
			mcp.WithString("format",
				mcp.Description("Output format for mode='exact': 'compact' (default) or 'json'."),
			),
		),
		s.handleSearchDispatch,
	)

	// Sprint 25: get_call_chain absorbed into get_context(mode="path").
	// Sprint 23.9: annotate merged into memory. Annotation actions are now memory(action="annotate"),
	// memory(action="annotate_web"), memory(action="add_gap"), memory(action="list_gaps"),
	// memory(action="history").

	// Sprint 24: link_entities, unlink_entities, confirm_edge removed.
	// Graph edge management is no longer agent-facing.

	// get_impact
	s.addOrDefer(
		mcp.NewTool(
			"get_impact",
			mcp.WithDescription(
				"CALL before changing any shared entity — function, interface, or type. "+
					"Without this, you won't know you're about to break callers across multiple packages. "+
					"Returns blast-radius analysis: direct callers (depth 1), indirect dependents (depth 2+), "+
					"and cross-domain impact across infrastructure, API endpoints, config files, and docs. "+
					"Summary in natural language: 'Changing X affects N callers across M packages.' "+
					"Use files= for PR-level blast radius across all entities in changed files. "+
					"scope='review' adds test gaps and risk flags for enriched code review output.",
			),
			mcp.WithString("symbol",
				mcp.Description("Name of the entity to analyse (e.g. 'CarveEgoGraph'). Required unless files= is provided."),
			),
			mcp.WithString("files",
				mcp.Description("Comma-separated file paths for change-set impact analysis (PR blast radius). "+
					"Aggregates impact across all entities in the specified files. Alternative to symbol=."),
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
			mcp.WithString("scope",
				mcp.Description("Optional. 'review': enriched output for code review — adds blast_radius summary "+
					"(direct/transitive/files/untested/high-risk counts), test_gaps (impacted entities with zero test coverage), "+
					"risk_flags (entities with negative quality scores from historical signals), and failure_history "+
					"(recent failure episodes related to this entity). One call replaces 10+ separate tool calls. "+
					"Default: standard impact analysis."),
			),
		),
		s.handleGetImpact,
	)

	// Sprint 25: get_entity_history absorbed into annotate(action="history").

	// ── Agent Task Memory Tools ──────────────────────────────────────────────
	// These tools give Synapses session continuity: plans and tasks agreed in
	// one LLM conversation are stored in SQLite and surfaced to future sessions.

	// tasks (merges create_plan, get_plans, get_pending_tasks, get_my_tasks, save_session_state, get_session_state, update_task, link_task_nodes)
	s.addOrDefer(
		mcp.NewTool(
			"tasks",
			mcp.WithDescription(
				"CALL action='create_plan' at the start of multi-step work to track progress across sessions. "+
					"Without a plan, resumed sessions start from scratch with no record of what was done or what remains. "+
					"action='create_plan': save a plan with tasks. "+
					"action='list_plans': overview of all plans. "+
					"action='pending': pending and in-progress tasks with suggested next step. "+
					"action='update': mark tasks done with notes (call immediately when a task completes). "+
					"action='save_state' / 'get_state': checkpoint and restore session working state. "+
					"action='link_nodes': connect tasks to graph entities for cross-session tracing.",
			),
			mcp.WithString("action",
				mcp.Required(),
				mcp.Description("'create_plan', 'list_plans', 'pending', 'update', 'save_state', 'get_state', 'link_nodes'."),
			),
			// create_plan params
			mcp.WithString("title",
				mcp.Description("Plan title. Required for action=create_plan."),
			),
			mcp.WithString("description",
				mcp.Description("Plan description. action=create_plan."),
			),
			mcp.WithString("tasks",
				mcp.Description("JSON array of task objects. Required for action=create_plan."),
			),
			// pending params
			mcp.WithString("plan_id",
				mcp.Description("Filter by plan ID. action=pending."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Agent identifier. Filters tasks (pending), attribution (create_plan/update/save_state)."),
			),
			mcp.WithBoolean("suggest_next",
				mcp.Description("Highlight top unblocked task. action=pending."),
			),
			// update params
			mcp.WithString("id",
				mcp.Description("Task ID. Required for action=update."),
			),
			mcp.WithString("status",
				mcp.Description("New status: pending|in_progress|done|cancelled. Required for action=update."),
			),
			mcp.WithString("notes",
				mcp.Description("Timestamped notes to append. action=update."),
			),
			mcp.WithString("intent",
				mcp.Description("Free-text intent visible to peers. action=update."),
			),
			// save_state params
			mcp.WithString("task_id",
				mcp.Description("Task ID. Required for action=save_state/get_state/link_nodes."),
			),
			mcp.WithString("approach",
				mcp.Description("Current approach. action=save_state."),
			),
			mcp.WithString("files_modified",
				mcp.Description("JSON array of modified files. action=save_state."),
			),
			mcp.WithString("completed_steps",
				mcp.Description("JSON array of completed steps. action=save_state."),
			),
			mcp.WithString("remaining_steps",
				mcp.Description("JSON array of remaining steps. action=save_state."),
			),
			mcp.WithString("blockers",
				mcp.Description("JSON array of blockers. action=save_state."),
			),
			mcp.WithString("decisions",
				mcp.Description("JSON array of decisions. action=save_state."),
			),
			mcp.WithString("context_snapshot",
				mcp.Description("Free-form context snapshot. action=save_state."),
			),
			// link_nodes params
			mcp.WithString("node_ids",
				mcp.Description("JSON array of node ID strings. Required for action=link_nodes."),
			),
		),
		s.handleTasksDispatch,
	)

	// end_session: captures session knowledge and optionally reports usage.
	s.addOrDefer(
		mcp.NewTool(
			"end_session",
			mcp.WithDescription(
				"CALL LAST before ending any session — even short ones. "+
					"Without this, everything you learned this session is discarded. "+
					"Future sessions will re-explore the same files and repeat the same mistakes. "+
					"Extracts files touched, entities examined, and task updates into structured memories "+
					"that surface in session_init and get_context in future sessions. "+
					"This is how institutional knowledge accumulates across sessions. "+
					"Returns effectiveness_report: context_hit_rate, first_fetch_right, tokens_saved, "+
					"and 7-day trend comparison. "+
					"Optionally records LLM token usage when model is provided.",
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

	// Sprint 25: get_session_state absorbed into tasks(action="get_state").
	// Sprint 25: update_task absorbed into tasks(action="update").

	// ── Coordination & Multi-Agent Tools ────────────────────────────────────

	// Sprint 25: get_plans absorbed into tasks(action="list_plans").
	// Sprint 25: link_task_nodes absorbed into tasks(action="link_nodes").

	// Sprint 24: get_agents, get_events, send_message, get_messages removed.
	// Cross-session awareness is now handled by the Work Ledger (ambient coordination).

	// Sprint 23.9: rules merged into validate. Rule/ADR management is now via:
	// validate(phase="upsert_rule"), validate(phase="delete_rule"),
	// validate(phase="candidates"), validate(phase="upsert_adr"), validate(phase="list_adrs").

	// ── Session Awareness Tools ──────────────────────────────────────────────

	// Sprint 24: get_working_state absorbed into session_init.

	// ── Web Tools ─────────────────────────────────────────────────────────────

	// Sprint 23.9: lookup_docs removed — agent browses docs itself.
	// Sprint 23.9: web_annotate absorbed into memory(action="annotate_web").
	// Sprint 25: prepare_context absorbed into get_context(mode="intent").

	// ── Agent Message Bus ────────────────────────────────────────────────────
	// Sprint 24: send_message, get_messages removed — replaced by Work Ledger.

	// memory (merges remember, recall, get_episodes)
	s.addOrDefer(
		mcp.NewTool(
			"memory",
			mcp.WithDescription(
				"CALL action='save' immediately after any decision, failed approach, or key finding — "+
					"before the next tool call. "+
					"Without this, discoveries are lost at context compaction and you'll re-explore the same ground next session. "+
					"action='save': record a decision, failure, or pattern episode. "+
					"action='search': retrieve prior decisions by keyword or concept. "+
					"action='list': chronological episode browser. "+
					"action='annotate': attach a note to a graph entity. "+
					"action='annotate_web': persist web research findings as an entity annotation. "+
					"action='add_gap' / 'list_gaps': track and query quality gaps. "+
					"action='history': entity change timeline.",
			),
			mcp.WithString("action",
				mcp.Required(),
				mcp.Description("'save', 'search', 'list', 'annotate', 'annotate_web', 'add_gap', 'list_gaps', 'history'."),
			),
			// action=save params
			mcp.WithString("agent_id",
				mcp.Description("Agent identifier. Required for action=save."),
			),
			mcp.WithString("decision",
				mcp.Description("Decision or failure text. Required for action=save."),
			),
			mcp.WithString("episode_type",
				mcp.Description("decision (default), failure, pattern, rule_proposal."),
			),
			mcp.WithString("outcome",
				mcp.Description("success, failure, partial, unknown (default). action=save."),
			),
			mcp.WithString("rationale",
				mcp.Description("Why this decision was made. action=save."),
			),
			mcp.WithString("trigger",
				mcp.Description("What prompted this episode. action=save."),
			),
			mcp.WithString("affected_files",
				mcp.Description("JSON array of file paths. action=save."),
			),
			mcp.WithString("affected_nodes",
				mcp.Description("JSON array of graph node IDs for scope. action=save."),
			),
			mcp.WithString("tags",
				mcp.Description("JSON array of tags (save) or comma-separated (search/list)."),
			),
			mcp.WithString("project_id",
				mcp.Description("Repo context filter."),
			),
			mcp.WithString("anchor_nodes",
				mcp.Description("JSON array of node IDs for staleness tracking. action=save."),
			),
			mcp.WithString("memory_importance",
				mcp.Description("Importance level: 'pinned' or float string. action=save. Default '1.0'."),
			),
			// action=search params
			mcp.WithString("query",
				mcp.Description("Search query. Omit for chronological browse. action=search."),
			),
			mcp.WithString("outcome_filter",
				mcp.Description("Filter by outcome. action=search only."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Max results. Default 5 (search), 20 (list/browse)."),
			),
			mcp.WithBoolean("include_stale",
				mcp.Description("Include invalidated memories. Default false. action=search."),
			),
			mcp.WithString("projects",
				mcp.Description("Comma-separated federation aliases for cross-project search."),
			),
			mcp.WithString("as_of",
				mcp.Description("Point-in-time query (RFC3339). action=search."),
			),
			mcp.WithString("since",
				mcp.Description("Lower time bound (RFC3339). action=search."),
			),
			mcp.WithString("until",
				mcp.Description("Upper time bound (RFC3339). action=search."),
			),
			mcp.WithNumber("depth",
				mcp.Description("Graph traversal depth (default 2, max 4). action=search."),
			),
			// action=list params
			mcp.WithNumber("since_days",
				mcp.Description("Only episodes from last N days. action=list."),
			),
			// action=annotate / annotate_web params (absorbed from annotate tool)
			mcp.WithString("node_id",
				mcp.Description("Node ID. Required for action=annotate/annotate_web/add_gap."),
			),
			mcp.WithString("note",
				mcp.Description("Annotation text. Required for action=annotate, optional for annotate_web."),
			),
			mcp.WithString("hits",
				mcp.Description("JSON array of {title,url,snippet} web hits. action=annotate_web."),
			),
			// action=add_gap params
			mcp.WithString("gap_id",
				mcp.Description("Short stable slug for gap dedup. Required for action=add_gap."),
			),
			mcp.WithString("gap_description",
				mcp.Description("Gap description. Required for action=add_gap."),
			),
			mcp.WithString("gap_severity",
				mcp.Description("low|medium|high|critical. action=add_gap/list_gaps."),
			),
			mcp.WithString("gap_status",
				mcp.Description("open|fixed|wontfix. action=add_gap. list_gaps: open|fixed|wontfix|all."),
			),
			mcp.WithString("fix_notes",
				mcp.Description("How the gap was fixed. action=add_gap with gap_status=fixed."),
			),
			// action=list_gaps / history params
			mcp.WithString("file",
				mcp.Description("Filter gaps by file path. action=list_gaps. Disambiguate for action=history."),
			),
			mcp.WithString("entity",
				mcp.Description("Entity name. Required for action=history."),
			),
		),
		s.handleMemoryDispatch,
	)

	// Sprint 25: check_plan_safety absorbed into validate(phase="safety").

	// Sprint 25→23.9: get_rule_candidates → rules(action="candidates") → validate(phase="candidates").
	// Sprint 25→23.9: get_adrs → rules(action="list_adrs") → validate(phase="list_adrs").

	// Sprint 24: get_decision_log moved to MCP Resource synapses://decision-log

	// Sprint 24: set_sdlc_phase, set_quality_mode absorbed into session_init params.

	// Sprint 25: plan_context absorbed into validate(phase="full").

	// ── Graph Query ─────────────────────────────────────────────────────────

	// Sprint 24: query_graph moved to MCP Resource synapses://query/{q}

	// ── Knowledge Export ─────────────────────────────────────────────────────

	// Sprint 24: export_knowledge, benchmark removed from MCP tools.
	// rank_candidates: REST-only (not in tools/list) — called by RepoBench-R benchmark binary.
	s.toolHandlersMu.Lock()
	s.toolHandlers["rank_candidates"] = s.handleRankCandidates
	s.toolHandlers["benchmark"] = s.handleBenchmark
	s.toolHandlersMu.Unlock()
}
