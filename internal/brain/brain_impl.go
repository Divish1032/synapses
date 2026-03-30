package brain

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/archivist"
	"github.com/SynapsesOS/synapses/internal/brain/config"
	"github.com/SynapsesOS/synapses/internal/brain/contextbuilder"
	"github.com/SynapsesOS/synapses/internal/brain/enricher"
	"github.com/SynapsesOS/synapses/internal/brain/guardian"
	"github.com/SynapsesOS/synapses/internal/brain/ingestor"
	"github.com/SynapsesOS/synapses/internal/brain/llm"
	"github.com/SynapsesOS/synapses/internal/brain/orchestrator"
	"github.com/SynapsesOS/synapses/internal/brain/pruner"
	"github.com/SynapsesOS/synapses/internal/brain/sdlc"
	"github.com/SynapsesOS/synapses/internal/brain/store"
	"github.com/SynapsesOS/synapses/internal/logutil"
)

// ── Per-tier system prompts ──────────────────────────────────────────────────
// Passed per-request via WithSystemPrompt. Replaces the Ollama Modelfile
// identity system (synapses/sentry, synapses/librarian, etc.) — the raw base
// model (qwen3.5:0.8b, 2b, or 4b) receives these directly in the API call.

const systemPromptSentry = `You are the Synapses Sentry, a code entity summarizer for a code intelligence graph.

Given a code entity (name, type, package, and source code), write a 2-3 sentence technical briefing covering: what it does, its role in the system, and any important patterns or concerns.

Do not write code. Describe the entity in plain English sentences only.
Output ONLY valid JSON with no other text: {"summary": "2-3 sentence briefing", "tags": ["domain_tag1", "domain_tag2"]}

Tags should be 1-3 domain labels from: auth, http, db, cache, queue, config, util, test, cli, graph, store, parser, middleware, api, worker.`

const systemPromptCritic = `You are the Synapses Critic, an architectural rule violation explainer.

Given an architectural rule violation (rule description, severity, source file, and what it imports/calls), explain the violation and suggest a concrete fix.

Output ONLY valid JSON with no other text: {"explanation": "why this is a violation and what risk it creates", "fix": "specific actionable fix the developer should apply"}

Example:
Input: Rule: no-cross-layer-imports. Severity: error. File: internal/api/handler.go imports internal/store/sqlite.go
Output: {"explanation": "The API handler directly imports the SQLite store implementation, bypassing the store interface. This creates tight coupling — changing the database requires modifying the API layer.", "fix": "Import the store.Store interface instead of the concrete sqlite implementation. Use dependency injection to pass the store to the handler."}

Be direct and actionable. Reference actual file names and symbols from the input.`

const systemPromptLibrarian = `You are the Synapses Librarian, a code architecture analyst.

Given a code graph slice (entity name, type, package, callers, callees, and relationships), analyze it for architectural patterns, risks, and insights.

Output ONLY valid JSON — no explanation, no markdown:
{"insight":"2-sentence architectural analysis","concerns":["concern1","concern2"]}

Rules:
- insight: identify the entity's role in the architecture (hub, gateway, utility, etc.) and its most important characteristic
- concerns: list 0-3 specific risks (cyclic deps, missing error handling, god object, missing abstraction, etc.)
- If no concerns, return an empty array: "concerns":[]
- Be specific — reference actual entity names and relationships, not generic advice`

const systemPromptNavigator = `You are the Synapses Navigator. You resolve multi-agent work scope conflicts.

Input: A JSON description of agents with their active scopes, and the new agent requesting a scope.

Output ONLY valid JSON — no explanation, no markdown:
{"suggestion":"how to resolve the conflict or confirmation it is safe","alternative_scope":"a suggested non-overlapping scope for the new agent, or empty string if no conflict"}

Rules:
- If the new agent's scope overlaps with an active agent's scope, describe the conflict and suggest a narrower scope
- If there is no real conflict (different packages, non-overlapping files), return: {"suggestion":"No conflict. Safe to proceed.","alternative_scope":""}
- Be specific — reference actual package names and file paths from the input
- alternative_scope should be a valid Go package path or file glob pattern`

const systemPromptArchivist = `You are the Synapses Archivist. You synthesize agent session transcripts into persistent memories.

Input: JSON with session_events (tool calls with results) and existing_memory (already saved entries).

Output ONLY valid JSON — no explanation, no markdown:
{"new_memories":[{"key":"short_snake_case_key","content":"what to remember in one sentence","entities":"EntityName1,EntityName2"}],"annotations":[{"node":"EntityName","note":"specific observation about this entity"}]}

Note: entities is a comma-separated string, NOT an array.

Rules:
- Only save architectural discoveries, non-obvious relationships, or decisions that will matter in future sessions
- If the session is trivial (single lookup, no architectural discovery, only routine tool calls), return: {"new_memories":[],"annotations":[]}
- Never duplicate entries already present in existing_memory — check keys before adding
- Keep each memory content to one concise sentence
- Only annotate entities that were meaningfully analyzed, not just mentioned in passing
- key must be short_snake_case (e.g., "auth_service_is_hub", "graph_new_entry_point")`

// Brain is the public interface for the Thinking Brain.
// All methods are safe for concurrent use.
// Use New() to create a configured instance.
// Use NullBrain when the brain is disabled — all methods return zero values.
type Brain interface {
	// Ingest summarizes a changed code snippet and persists it in brain.sqlite.
	// Called on file-save events for changed functions/methods/structs.
	// Returns immediately if LLM is unavailable.
	Ingest(ctx context.Context, req IngestRequest) (IngestResponse, error)

	// Enrich adds semantic summaries and a 2-sentence insight to a carved context subgraph.
	// Summaries are loaded from brain.sqlite (fast path, no LLM call).
	// Insight is generated by the LLM only if Enrich feature is enabled.
	Enrich(ctx context.Context, req EnrichRequest) (EnrichResponse, error)

	// ExplainViolation generates a plain-English explanation and fix for a rule violation.
	// Results are cached in brain.sqlite to avoid repeated LLM calls for the same violation.
	ExplainViolation(ctx context.Context, req ViolationRequest) (ViolationResponse, error)

	// Coordinate suggests work distribution when agents conflict on a scope.
	Coordinate(ctx context.Context, req CoordinateRequest) (CoordinateResponse, error)

	// Summary returns the stored semantic summary for a node ID.
	// Returns "" if no summary has been ingested for this node.
	Summary(projectID, nodeID string) string

	// Available returns true if the configured LLM backend is reachable.
	Available() bool

	// ModelName returns the configured LLM model tag.
	ModelName() string

	// EnsureModel checks if the configured model is present locally; if not,
	// it pulls it (Ollama registry for ollama, HuggingFace GGUF for local).
	// Returns nil when the model is ready, an error if the download fails.
	EnsureModel(ctx context.Context, w io.Writer) error

	// --- v0.2.0: Context Packet, SDLC, Learning ---

	// BuildContextPacket assembles a structured Context Packet for an agent.
	// Returns nil (with no error) when the Brain is unavailable — callers fall
	// back to raw Synapses context unchanged.
	BuildContextPacket(ctx context.Context, req ContextPacketRequest) (*ContextPacket, error)

	// LogDecision records an agent's completed work and updates co-occurrence
	// patterns in brain.sqlite. Non-fatal; errors are returned but do not
	// block the calling agent.
	LogDecision(ctx context.Context, req DecisionRequest) error

	// SetSDLCPhase persists the project's current SDLC phase.
	SetSDLCPhase(phase SDLCPhase, agentID string) error

	// SetQualityMode persists the project's current quality mode.
	SetQualityMode(mode QualityMode, agentID string) error

	// GetSDLCConfig returns the current SDLC config.
	GetSDLCConfig() SDLCConfig

	// GetPatterns returns learned co-occurrence patterns sorted by confidence.
	// If trigger is non-empty, only patterns with that trigger are returned.
	// limit caps the number of results (0 = default of 20).
	GetPatterns(trigger string, limit int) []PatternHint

	// Prune strips boilerplate (navigation, ads, footers) from raw web page text
	// using the Tier 0 (0.8B) model. Returns cleaned technical content.
	// Falls back to returning the original content if the LLM is unavailable.
	Prune(ctx context.Context, content string) (string, error)

	// Memorize synthesizes a session transcript into persistent memory entries
	// and code annotations. Powered by the Archivist fine-tuned model (T2, cold standby).
	// Returns empty response (no error) when the Archivist LLM is unavailable.
	Memorize(ctx context.Context, req archivist.MemorizeRequest) (archivist.MemorizeResponse, error)

	// Generate sends a raw prompt to the brain's primary LLM and returns the
	// response. Used for one-off LLM calls (e.g., brain-enhanced drift summaries)
	// that don't fit into the ingest/enrich/guardian pipeline.
	// Returns ("", err) if the LLM is unavailable.
	Generate(ctx context.Context, prompt string) (string, error)

	// --- v0.6.0: ADRs ---

	// UpsertADR creates or updates an Architectural Decision Record.
	UpsertADR(req ADRRequest) error

	// GetADR returns the ADR with the given ID.
	GetADR(id string) (ADR, error)

	// AllADRs returns all ADRs ordered by updated_at descending.
	AllADRs() ([]ADR, error)

	// GetADRsForFile returns accepted ADRs whose linked_files patterns match the given file path.
	GetADRsForFile(filePath string, limit int) ([]ADR, error)

	// QueryDecisions returns up to limit decision log entries, optionally
	// filtered by entityName (empty = all recent), ordered by created_at DESC.
	QueryDecisions(ctx context.Context, entityName string, limit int) ([]DecisionLogEntry, error)
}

// TierState describes the current circuit-breaker state for one tier.
type TierState struct {
	Open              bool    `json:"open"`
	Failures          int     `json:"failures"`
	CooldownRemaining float64 `json:"cooldown_remaining_s"`
}

// TierStatusProvider is implemented by the production Brain to expose
// per-tier circuit-breaker status for the /v1/health/tiers endpoint.
// NullBrain does not implement this interface; callers should type-assert.
type TierStatusProvider interface {
	TierStatus() map[string]TierState
}

// BrainStatsProvider exposes cumulative telemetry counters for health dashboards.
// Type-assert Brain to this interface to access stats.
type BrainStatsProvider interface {
	BrainStats() map[string]interface{}
}

// impl is the production Brain backed by Ollama (or local CGo) + SQLite.
// brainStats tracks per-tier success/failure counts and cumulative latency.
// Thread-safe via sync/atomic. Exported via GetSummary for health checks.
type brainStats struct {
	mu                    sync.Mutex
	ingestCalls           int64
	ingestSuccess         int64
	ingestDeterministic   int64 // fast-path hits (no LLM)
	ingestLatencyMS       int64
	enrichCalls           int64
	enrichSuccess         int64
	enrichLatencyMS       int64
	guardianCalls         int64
	guardianSuccess       int64
	orchestrateCalls      int64
	orchestrateSuccess    int64
	archivistCalls        int64
	archivistSuccess      int64
	archivistRepairs      int64 // JSON bracket repairs
	contextBuilderCalls   int64
	contextBuilderSuccess int64
}

func (s *brainStats) record(tier string, success bool, latencyMS int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch tier {
	case "ingest":
		s.ingestCalls++
		s.ingestLatencyMS += latencyMS
		if success {
			s.ingestSuccess++
		}
	case "enrich":
		s.enrichCalls++
		s.enrichLatencyMS += latencyMS
		if success {
			s.enrichSuccess++
		}
	case "guardian":
		s.guardianCalls++
		if success {
			s.guardianSuccess++
		}
	case "orchestrate":
		s.orchestrateCalls++
		if success {
			s.orchestrateSuccess++
		}
	case "archivist":
		s.archivistCalls++
		if success {
			s.archivistSuccess++
		}
	case "context_builder":
		s.contextBuilderCalls++
		if success {
			s.contextBuilderSuccess++
		}
	}
}

func (s *brainStats) snapshot() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	avgIngest := int64(0)
	if s.ingestCalls > 0 {
		avgIngest = s.ingestLatencyMS / s.ingestCalls
	}
	avgEnrich := int64(0)
	if s.enrichCalls > 0 {
		avgEnrich = s.enrichLatencyMS / s.enrichCalls
	}
	return map[string]interface{}{
		"ingest_calls":            s.ingestCalls,
		"ingest_success":          s.ingestSuccess,
		"ingest_deterministic":    s.ingestDeterministic,
		"ingest_avg_ms":           avgIngest,
		"enrich_calls":            s.enrichCalls,
		"enrich_success":          s.enrichSuccess,
		"enrich_avg_ms":           avgEnrich,
		"guardian_calls":          s.guardianCalls,
		"guardian_success":        s.guardianSuccess,
		"orchestrate_calls":       s.orchestrateCalls,
		"orchestrate_success":     s.orchestrateSuccess,
		"archivist_calls":         s.archivistCalls,
		"archivist_success":       s.archivistSuccess,
		"archivist_repairs":       s.archivistRepairs,
		"context_builder_calls":   s.contextBuilderCalls,
		"context_builder_success": s.contextBuilderSuccess,
	}
}

type impl struct {
	cfg          config.BrainConfig
	llm          llm.LLMClient
	store        *store.Store
	ingestor     *ingestor.Ingestor
	enricher     *enricher.Enricher
	guardian     *guardian.Guardian
	orchestrator *orchestrator.Orchestrator
	pruner       *pruner.Pruner
	archivist    *archivist.Archivist
	sdlcMgr      *sdlc.Manager
	builder      *contextbuilder.Builder
	learner      *contextbuilder.Learner
	cb           *circuitBreaker
	stats        brainStats

	// Fallback components: pre-built at New() using lower-tier LLM clients.
	// Used when the primary tier's circuit breaker trips, so agents always
	// receive something rather than zero-values.
	fallbackEnricher *enricher.Enricher // T2 → T0: uses ingestClient model
	fallbackGuardian *guardian.Guardian // T1 → T0: uses ingestClient model
	// Note: orchestrate fallback does NOT use an LLM-backed orchestrator.
	// Sending orchestration prompts to Librarian (FT code-graph model) or Sentry
	// produces garbage — those models have no conflict-resolution capability.
	// orchestrator.DeterministicCoordinate() provides a guaranteed non-empty
	// rule-based response when the orchestrate circuit is open.

	cancelWarmup context.CancelFunc // cancels background model warm-up goroutines
}

// New creates a fully-configured Brain from cfg.
// Returns NullBrain if cfg.Enabled is false.
// Returns NullBrain (with logged warning) if the store cannot be opened.
func New(cfg config.BrainConfig) Brain {
	if !cfg.Enabled {
		return &NullBrain{}
	}

	// Build LLM clients.
	// When backend="local" and gguf_path is set, all four tiers share one LocalClient
	// that runs the fine-tuned SIL GGUF model directly in-process (no Ollama required).
	// Falls back to OllamaClient if the local model can't be loaded.
	var (
		ingestClient      llm.LLMClient
		guardianClient    llm.LLMClient
		enrichClient      llm.LLMClient
		orchestrateClient llm.LLMClient
		archivistClient   llm.LLMClient
	)

	if cfg.Backend == "local" && cfg.GGUFPath != "" {
		hw := llm.DetectHardware()
		localCli, err := llm.NewLocalClient(cfg.GGUFPath, hw)
		if err == nil {
			// Single fine-tuned model serves all five tiers.
			// Thinking enabled — SIL model was trained with <think> blocks.
			localCli.WithThinking(true)
			ingestClient = localCli
			guardianClient = localCli
			enrichClient = localCli
			orchestrateClient = localCli
			archivistClient = localCli
		}
		// err is non-fatal — fall through to Ollama below.
	}

	// Ollama path (default): per-tier model assignment via Ollama HTTP API.
	// System prompts are passed per-request — no Ollama Modelfile identities needed.
	if cfg.Backend == "ollama" && ingestClient == nil {
		// Warn once if OllamaURL points to a remote host — source code will be
		// transmitted to that endpoint during ingest/enrich operations.
		if u, err := url.Parse(cfg.OllamaURL); err == nil {
			host := u.Hostname()
			if host != "localhost" && host != "127.0.0.1" && host != "::1" && host != "" {
				logutil.Warn("brain: OllamaURL points to remote host %q — source code snippets will be transmitted to this endpoint\n", host)
			}
		}
		ka := cfg.KeepAlive()

		// Fallback chain: if the configured model isn't pulled, degrade to
		// the next smaller model. Order: 4b → 2b → 0.8b.
		// Sentry (0.8b) has no fallback — it's the smallest.
		fb08b := []string{} // no fallback for 0.8b
		fb2b := []string{"qwen3.5:0.8b"}
		fb4b := []string{"qwen3.5:2b", "qwen3.5:0.8b"}

		fallbackFor := func(model string) []string {
			switch {
			case strings.Contains(model, ":4b"):
				return fb4b
			case strings.Contains(model, ":2b"):
				return fb2b
			default:
				return fb08b
			}
		}

		ingestClient = llm.NewOllamaClient(cfg.OllamaURL, cfg.ModelIngest, cfg.TimeoutMS).
			WithThinking(false).
			WithChatMode(true).
			WithJSONFormat(true).
			WithKeepAlive(ka).
			WithSystemPrompt(systemPromptSentry).
			WithTemperature(0.0).
			WithNumPredict(256).
			WithFallbackModels(fallbackFor(cfg.ModelIngest)...)

		guardianClient = llm.NewOllamaClient(cfg.OllamaURL, cfg.ModelGuardian, cfg.TimeoutMS).
			WithThinking(false).
			WithChatMode(true).
			WithJSONFormat(true).
			WithKeepAlive(ka).
			WithSystemPrompt(systemPromptCritic).
			WithTemperature(0.1).
			WithNumPredict(512).
			WithFallbackModels(fallbackFor(cfg.ModelGuardian)...)

		enrichClient = llm.NewOllamaClient(cfg.OllamaURL, cfg.ModelEnrich, cfg.TimeoutMS).
			WithThinking(false).
			WithChatMode(true).
			WithJSONFormat(true).
			WithKeepAlive(ka).
			WithSystemPrompt(systemPromptLibrarian).
			WithTemperature(0.2).
			WithNumPredict(512).
			WithFallbackModels(fallbackFor(cfg.ModelEnrich)...)

		orchestrateClient = llm.NewOllamaClient(cfg.OllamaURL, cfg.ModelOrchestrate, cfg.TimeoutMS).
			WithThinking(false).
			WithChatMode(true).
			WithJSONFormat(true).
			WithKeepAlive(ka).
			WithSystemPrompt(systemPromptNavigator).
			WithTemperature(0.1).
			WithNumPredict(512).
			WithFallbackModels(fallbackFor(cfg.ModelOrchestrate)...)

		archivistClient = llm.NewOllamaClient(cfg.OllamaURL, cfg.ModelArchivist, cfg.TimeoutMS).
			WithThinking(false).
			WithChatMode(true).
			WithJSONFormat(true).
			WithKeepAlive(ka).
			WithSystemPrompt(systemPromptArchivist).
			WithTemperature(0.3).
			WithNumPredict(1024).
			WithFallbackModels(fallbackFor(cfg.ModelArchivist)...)
	}

	// No backend could be configured — degrade gracefully.
	if ingestClient == nil {
		logutil.Warn("brain: no LLM backend available (backend=%q, gguf_path=%q) — returning NullBrain\n", cfg.Backend, cfg.GGUFPath)
		return &NullBrain{}
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		// Can't open store — degrade gracefully.
		return &NullBrain{}
	}

	timeout := time.Duration(cfg.TimeoutMS) * time.Millisecond

	enr := enricher.New(enrichClient, st, timeout)
	if cfg.Backend == "local" {
		// SIL fine-tuned model: use graph JSON prompts that match training data.
		enr.WithSILMode()
	}
	mgr := sdlc.NewManager(st)

	// Build tier components — nil client means the tier GGUF was missing; component
	// is left nil and the corresponding Brain method returns an empty response.
	var guardianInst *guardian.Guardian
	if guardianClient != nil {
		guardianInst = guardian.New(guardianClient, st, timeout)
	}
	var orchestratorInst *orchestrator.Orchestrator
	if orchestrateClient != nil {
		orchestratorInst = orchestrator.New(orchestrateClient, timeout)
	}
	var archivistInst *archivist.Archivist
	if archivistClient != nil {
		archivistInst = archivist.New(archivistClient, timeout)
	}

	// primaryClient: prefer enrich (T1), fall back to ingest (T0) if T1 is nil.
	primaryClient := enrichClient
	if primaryClient == nil {
		primaryClient = ingestClient
	}

	// Pre-build fallback components using the ingest (T0) and enrich (T1) clients.
	// These are used when the primary tier's circuit breaker trips, so callers
	// always receive a degraded-but-present response instead of zero-values.
	fallbackEnr := enricher.New(ingestClient, st, timeout) // T2→T0: enrich with T0 model
	fallbackGrd := guardian.New(ingestClient, st, timeout) // T1→T0: explain with T0 model
	// No fallback LLM orchestrators: sending orchestration prompts to code-graph
	// FT models (Librarian, Sentry) produces garbage — they have no conflict-resolution
	// capability. coordinateFallback uses orchestrator.DeterministicCoordinate instead,
	// which guarantees a correct rule-based response with zero external dependencies.

	b := &impl{
		cfg:          cfg,
		llm:          primaryClient, // primary client used for Available() / ModelName()
		store:        st,
		ingestor:     ingestor.New(ingestClient, st, timeout),
		enricher:     enr,
		guardian:     guardianInst,
		orchestrator: orchestratorInst,
		pruner:       pruner.New(ingestClient, timeout), // Tier 0: 0.8B, same as ingest
		archivist:    archivistInst,
		sdlcMgr:      mgr,
		builder:      contextbuilder.New(st, mgr, enr),
		learner:      contextbuilder.NewLearner(st),
		cb:           newCircuitBreaker(3, 5*time.Minute),

		fallbackEnricher: fallbackEnr,
		fallbackGuardian: fallbackGrd,
	}

	// Pre-load all configured models in background so the first real request
	// hits a warm model instead of waiting 3-8s for Ollama to load from disk.
	warmCtx, cancelWarmup := context.WithCancel(context.Background())
	b.cancelWarmup = cancelWarmup
	go warmUpModels(warmCtx, ingestClient, guardianClient, enrichClient, orchestrateClient, archivistClient)

	return b
}

// warmUpModels pre-loads all unique Ollama models into memory concurrently.
// Runs in a background goroutine — non-blocking, logs results to stderr.
// Models that don't implement ModelWarmer (e.g. LocalClient) are skipped silently.
// The parent context allows cancellation when the Brain is closed.
// On first connection-refused error, all remaining warmups are cancelled fast to
// avoid a 90s hang (5 models × 30s / 2 parallelism).
func warmUpModels(parent context.Context, clients ...llm.LLMClient) {
	seen := make(map[string]bool)
	var wg sync.WaitGroup
	// Semaphore limits concurrent warmups to 2 to avoid GPU memory pressure
	// when multiple models compete for a single GPU.
	sem := make(chan struct{}, 2)
	// fast-fail context: cancelled on first connection-refused so remaining
	// queued warmups abort immediately instead of timing out after 30s each.
	warmCtx, warmCancel := context.WithCancel(parent)
	defer warmCancel()
	for _, c := range clients {
		if c == nil {
			continue
		}
		w, ok := c.(llm.ModelWarmer)
		if !ok {
			continue
		}
		name := c.ModelName()
		if seen[name] {
			continue // same model used by multiple tiers — warm once
		}
		seen[name] = true
		wg.Add(1)
		go func(w llm.ModelWarmer, name string) {
			defer wg.Done()
			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release
			ctx, cancel := context.WithTimeout(warmCtx, 30*time.Second)
			defer cancel()
			if err := w.WarmUp(ctx); err != nil {
				logutil.Error("brain: warmup %s: %v\n", name, err)
				// Cancel remaining warmups on connection refused — Ollama is not running.
				if isConnectionRefused(err) {
					warmCancel()
				}
			} else {
				logutil.Info("brain: warmup complete: %s\n", name)
			}
		}(w, name)
	}
	wg.Wait()
}

// isConnectionRefused returns true when the error indicates the target is not listening.
func isConnectionRefused(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "connect: connection refused")
}

func (b *impl) Ingest(ctx context.Context, req IngestRequest) (IngestResponse, error) {
	if !b.cfg.Ingest {
		return IngestResponse{NodeID: req.NodeID}, nil
	}

	// Circuit breaker: skip LLM call if tier is tripped.
	if b.cb.isOpen("ingest") {
		return IngestResponse{NodeID: req.NodeID}, nil
	}

	start := time.Now()
	r, err := b.ingestor.Summarize(ctx, ingestor.Request{
		ProjectID: req.ProjectID,
		NodeID:    req.NodeID,
		NodeName:  req.NodeName,
		NodeType:  req.NodeType,
		Package:   req.Package,
		Code:      req.Code,
	})
	latency := time.Since(start).Milliseconds()

	if err != nil {
		b.cb.recordFailure("ingest")
		b.stats.record("ingest", false, latency)
		return IngestResponse{NodeID: req.NodeID}, err
	}

	// Quality gate: validate the summary response.
	if !validateIngestResponse(r.Summary) {
		b.cb.recordFailure("ingest")
		b.stats.record("ingest", false, latency)
		logutil.Warn("brain: ingest response below quality threshold for node %s\n", req.NodeID)
		return IngestResponse{NodeID: req.NodeID}, nil
	}

	// Track deterministic fast-path hits (latency < 5ms = no LLM call).
	if latency < 5 {
		b.stats.mu.Lock()
		b.stats.ingestDeterministic++
		b.stats.mu.Unlock()
	}

	b.cb.recordSuccess("ingest")
	b.stats.record("ingest", true, latency)
	return IngestResponse{NodeID: r.NodeID, Summary: r.Summary, Tags: r.Tags}, nil
}

func (b *impl) Enrich(ctx context.Context, req EnrichRequest) (EnrichResponse, error) {
	// Always load summaries from SQLite — fast path, no LLM.
	summaries := b.store.GetSummaries(req.ProjectID, req.AllNodeIDs)

	if !b.cfg.Enrich {
		return EnrichResponse{Summaries: summaries}, nil
	}

	// Circuit breaker: if primary enrich tier is tripped, try fallback (T0 model).
	if b.cb.isOpen("enrich") {
		if b.fallbackEnricher != nil && !b.cb.isOpen("ingest") {
			enrReq := enricher.Request{
				RootID: req.RootID, RootName: req.RootName, RootType: req.RootType,
				RootFile: req.RootFile, FanIn: req.FanIn,
				CalleeNames: req.CalleeNames, CallerNames: req.CallerNames,
				RelatedNames: req.RelatedNames, TaskContext: req.TaskContext,
			}
			if r, err := b.fallbackEnricher.Enrich(ctx, enrReq); err == nil && validateResponse(r.Insight, 20) {
				logutil.Warn("brain: enrich degraded — using T0 fallback for %s\n", req.RootName)
				return EnrichResponse{Insight: r.Insight, Concerns: r.Concerns, Summaries: summaries, LLMUsed: r.LLMUsed, Degraded: true}, nil
			}
		}
		// All LLM tiers exhausted — return heuristic insight so agents always
		// receive a non-empty response even when the brain is fully unavailable.
		return EnrichResponse{Summaries: summaries, Insight: heuristicEnrichInsight(req), Degraded: true}, nil
	}

	start := time.Now()
	r, err := b.enricher.Enrich(ctx, enricher.Request{
		RootID:       req.RootID,
		RootName:     req.RootName,
		RootType:     req.RootType,
		RootFile:     req.RootFile,
		FanIn:        req.FanIn,
		CalleeNames:  req.CalleeNames,
		CallerNames:  req.CallerNames,
		RelatedNames: req.RelatedNames,
		TaskContext:  req.TaskContext,
	})
	latency := time.Since(start).Milliseconds()

	if err != nil {
		b.cb.recordFailure("enrich")
		b.stats.record("enrich", false, latency)
		return EnrichResponse{Summaries: summaries}, nil
	}

	// Quality gate: validate the insight response.
	if !validateEnrichResponse(r.Insight, r.Concerns) {
		b.cb.recordFailure("enrich")
		b.stats.record("enrich", false, latency)
		logutil.Warn("brain: enrich response below quality threshold, falling back to raw data\n")
		return EnrichResponse{Summaries: summaries}, nil
	}

	b.cb.recordSuccess("enrich")
	b.stats.record("enrich", true, latency)
	return EnrichResponse{
		Insight:   r.Insight,
		Concerns:  r.Concerns,
		Summaries: summaries,
		LLMUsed:   r.LLMUsed,
	}, nil
}

func (b *impl) ExplainViolation(ctx context.Context, req ViolationRequest) (ViolationResponse, error) {
	if !b.cfg.Guardian {
		return ViolationResponse{}, nil
	}

	// Circuit breaker check precedes the guardian nil-check: an open circuit
	// means the LLM path is unavailable regardless of component state, so we
	// fall through the fallback chain without needing a live guardian client.
	// Guard: b.cb is nil only in unit tests that construct impl directly.
	if b.cb != nil && b.cb.isOpen("guardian") {
		if b.fallbackGuardian != nil && !b.cb.isOpen("ingest") {
			grdReq := guardian.Request{
				RuleID: req.RuleID, RuleSeverity: req.RuleSeverity, Description: req.Description,
				SourceFile: req.SourceFile, TargetName: req.TargetName,
			}
			if r, err := b.fallbackGuardian.Explain(ctx, grdReq); err == nil && validateResponse(r.Explanation, 15) {
				logutil.Warn("brain: guardian degraded — using T0 fallback for rule %s\n", req.RuleID)
				return ViolationResponse{Explanation: r.Explanation, Fix: r.Fix, Degraded: true}, nil
			}
		}
		// All LLM tiers exhausted — return rule template so validate_plan always
		// surfaces an actionable message even when the brain is fully unavailable.
		return guardianTemplateFallback(req), nil
	}

	// Guardian component is required for the primary LLM path.
	if b.guardian == nil {
		return ViolationResponse{}, nil
	}

	start := time.Now()
	r, err := b.guardian.Explain(ctx, guardian.Request{
		RuleID:       req.RuleID,
		RuleSeverity: req.RuleSeverity,
		Description:  req.Description,
		SourceFile:   req.SourceFile,
		TargetName:   req.TargetName,
	})
	latency := time.Since(start).Milliseconds()

	if err != nil {
		b.cb.recordFailure("guardian")
		b.stats.record("guardian", false, latency)
		return ViolationResponse{}, err
	}

	// Quality gate: validate explanation and fix are both non-empty.
	if !validateGuardianResponse(r.Explanation, r.Fix) {
		b.cb.recordFailure("guardian")
		b.stats.record("guardian", false, latency)
		logutil.Warn("brain: guardian response missing explanation or fix\n")
		return ViolationResponse{}, nil
	}

	b.cb.recordSuccess("guardian")
	b.stats.record("guardian", true, latency)
	return ViolationResponse{Explanation: r.Explanation, Fix: r.Fix}, nil
}

func (b *impl) Coordinate(ctx context.Context, req CoordinateRequest) (CoordinateResponse, error) {
	if !b.cfg.Orchestrate || b.orchestrator == nil {
		return CoordinateResponse{}, nil
	}

	// Circuit breaker: if primary orchestrate tier is tripped, try fallback chain (T2→T0).
	if b.cb.isOpen("orchestrate") {
		return b.coordinateFallback(ctx, req)
	}

	claims := make([]orchestrator.WorkClaim, len(req.ConflictingClaims))
	for i, c := range req.ConflictingClaims {
		claims[i] = orchestrator.WorkClaim{
			AgentID:   c.AgentID,
			Scope:     c.Scope,
			ScopeType: c.ScopeType,
		}
	}
	start := time.Now()
	r, err := b.orchestrator.Coordinate(ctx, orchestrator.Request{
		NewAgentID:        req.NewAgentID,
		NewScope:          req.NewScope,
		ConflictingClaims: claims,
	})
	latency := time.Since(start).Milliseconds()

	if err != nil {
		b.cb.recordFailure("orchestrate")
		b.stats.record("orchestrate", false, latency)
		return CoordinateResponse{}, err
	}

	// Quality gate: validate suggestion is non-empty.
	if !validateCoordinateResponse(r.Suggestion) {
		b.cb.recordFailure("orchestrate")
		b.stats.record("orchestrate", false, latency)
		logutil.Warn("brain: orchestrate response empty suggestion\n")
		return CoordinateResponse{}, nil
	}

	b.cb.recordSuccess("orchestrate")
	b.stats.record("orchestrate", true, latency)
	return CoordinateResponse{
		Suggestion:       r.Suggestion,
		AlternativeScope: r.AlternativeScope,
	}, nil
}

// coordinateFallback is called when the primary orchestrate circuit is open.
// It returns a deterministic rule-based response via DeterministicCoordinate —
// no LLM call, no external dependencies, guaranteed non-empty result.
// LLM-backed fallbacks (T2/T0 models) are intentionally absent: Librarian and
// Sentry are code-graph FT models with no conflict-resolution capability; sending
// orchestration prompts to them produces garbage that corrupts downstream agents.
func (b *impl) coordinateFallback(_ context.Context, req CoordinateRequest) (CoordinateResponse, error) {
	claims := make([]orchestrator.WorkClaim, len(req.ConflictingClaims))
	for i, c := range req.ConflictingClaims {
		claims[i] = orchestrator.WorkClaim{AgentID: c.AgentID, Scope: c.Scope, ScopeType: c.ScopeType}
	}
	r := orchestrator.DeterministicCoordinate(orchestrator.Request{
		NewAgentID:        req.NewAgentID,
		NewScope:          req.NewScope,
		ConflictingClaims: claims,
	})
	logutil.Warn("brain: orchestrate circuit open — using deterministic fallback\n")
	return CoordinateResponse{Suggestion: r.Suggestion, AlternativeScope: r.AlternativeScope, Degraded: true}, nil
}

func (b *impl) Prune(ctx context.Context, content string) (string, error) {
	return b.pruner.Prune(ctx, content)
}

func (b *impl) Memorize(ctx context.Context, req archivist.MemorizeRequest) (archivist.MemorizeResponse, error) {
	if !b.cfg.Memorize || b.archivist == nil {
		return archivist.MemorizeResponse{}, nil
	}
	if b.cb.isOpen("archivist") {
		return archivist.MemorizeResponse{}, nil
	}
	resp, err := b.archivist.Memorize(ctx, req)
	if err != nil {
		b.cb.recordFailure("archivist")
		b.stats.record("archivist", false, 0)
		return resp, err
	}
	// Retry once if the response is completely empty (model returned garbage
	// that parsed to empty arrays). Non-trivial sessions should produce at
	// least one memory or annotation. Skip retry if context is already done.
	if len(req.SessionEvents) > 1 && len(resp.NewMemories) == 0 && len(resp.Annotations) == 0 {
		if ctx.Err() != nil {
			b.cb.recordSuccess("archivist")
			b.stats.record("archivist", true, 0)
			return resp, nil
		}
		resp2, err2 := b.archivist.Memorize(ctx, req)
		if err2 == nil && (len(resp2.NewMemories) > 0 || len(resp2.Annotations) > 0) {
			resp = resp2
		}
	}
	b.cb.recordSuccess("archivist")
	b.stats.record("archivist", true, 0)
	return resp, nil
}

func (b *impl) Generate(ctx context.Context, prompt string) (string, error) {
	if b.llm == nil {
		return "", fmt.Errorf("brain: no LLM available")
	}
	return b.llm.Generate(ctx, prompt)
}

func (b *impl) Summary(projectID, nodeID string) string {
	if b.store == nil {
		return ""
	}
	return b.store.GetSummary(projectID, nodeID)
}

func (b *impl) Close() error {
	if b.cancelWarmup != nil {
		b.cancelWarmup()
	}
	// Release GPU/CPU memory held by the LLM client (e.g. llama.cpp context).
	if closer, ok := b.llm.(io.Closer); ok {
		closer.Close()
	}
	if b.store != nil {
		return b.store.Close()
	}
	return nil
}

func (b *impl) Available() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return b.llm.Available(ctx)
}

func (b *impl) ModelName() string {
	return b.llm.ModelName()
}

func (b *impl) EnsureModel(ctx context.Context, w io.Writer) error {
	if b.llm.ModelPulled(ctx) {
		return nil
	}
	return b.llm.PullModel(ctx, w)
}

func (b *impl) BuildContextPacket(ctx context.Context, req ContextPacketRequest) (*ContextPacket, error) {
	if !b.cfg.ContextBuilder {
		return nil, nil // feature disabled; caller uses raw context
	}

	// Circuit breaker: skip LLM call if tier is tripped.
	if b.cb.isOpen("context_builder") {
		return nil, nil
	}

	start := time.Now()
	pkt, err := b.builder.Build(ctx, contextbuilder.Request{
		ProjectID:       req.ProjectID,
		AgentID:         req.AgentID,
		Phase:           string(req.Phase),
		QualityMode:     string(req.QualityMode),
		EnableLLM:       req.EnableLLM,
		RootNodeID:      req.Snapshot.RootNodeID,
		RootName:        req.Snapshot.RootName,
		RootType:        req.Snapshot.RootType,
		RootFile:        req.Snapshot.RootFile,
		CalleeNames:     req.Snapshot.CalleeNames,
		CallerNames:     req.Snapshot.CallerNames,
		RelatedNames:    req.Snapshot.RelatedNames,
		ApplicableRules: toBuilderRules(req.Snapshot.ApplicableRules),
		ActiveClaims:    toBuilderClaims(req.Snapshot.ActiveClaims),
		TaskContext:     req.Snapshot.TaskContext,
		TaskID:          req.Snapshot.TaskID,
		HasTests:        req.Snapshot.HasTests,
		FanIn:           req.Snapshot.FanIn,
		RootDoc:         req.Snapshot.RootDoc,
	})
	latency := time.Since(start).Milliseconds()
	if err != nil || pkt == nil {
		if err != nil {
			b.cb.recordFailure("context_builder")
			b.stats.record("context_builder", false, latency)
		}
		return nil, err
	}
	b.cb.recordSuccess("context_builder")
	b.stats.record("context_builder", true, latency)
	return toContextPacket(pkt), nil
}

func (b *impl) LogDecision(_ context.Context, req DecisionRequest) error {
	return b.learner.RecordDecision(contextbuilder.DecisionInput{
		AgentID:         req.AgentID,
		Phase:           req.Phase,
		EntityName:      req.EntityName,
		Action:          req.Action,
		RelatedEntities: req.RelatedEntities,
		Outcome:         req.Outcome,
		Notes:           req.Notes,
	})
}

func (b *impl) SetSDLCPhase(phase SDLCPhase, agentID string) error {
	return b.sdlcMgr.SetPhase(string(phase), agentID)
}

func (b *impl) SetQualityMode(mode QualityMode, agentID string) error {
	return b.sdlcMgr.SetQualityMode(string(mode), agentID)
}

func (b *impl) GetSDLCConfig() SDLCConfig {
	row := b.sdlcMgr.GetConfig()
	return SDLCConfig{
		Phase:       SDLCPhase(row.Phase),
		QualityMode: QualityMode(row.QualityMode),
		UpdatedAt:   row.UpdatedAt,
		UpdatedBy:   row.UpdatedBy,
	}
}

func (b *impl) GetPatterns(trigger string, limit int) []PatternHint {
	if limit <= 0 {
		limit = 20
	}
	var raw []store.ContextPattern
	if trigger != "" {
		raw = b.store.GetPatternsForTriggers([]string{trigger}, limit)
	} else {
		all, _ := b.store.AllPatterns()
		if len(all) > limit {
			all = all[:limit]
		}
		raw = all
	}
	out := make([]PatternHint, len(raw))
	for i, p := range raw {
		out[i] = PatternHint{Trigger: p.Trigger, CoChange: p.CoChange, Reason: p.Reason, Confidence: p.Confidence}
	}
	return out
}

// --- conversion helpers ---

func toBuilderRules(rules []RuleInput) []contextbuilder.RuleRef {
	out := make([]contextbuilder.RuleRef, len(rules))
	for i, r := range rules {
		out[i] = contextbuilder.RuleRef{RuleID: r.RuleID, Severity: r.Severity, Description: r.Description}
	}
	return out
}

func toBuilderClaims(claims []ClaimInput) []contextbuilder.ClaimRef {
	out := make([]contextbuilder.ClaimRef, len(claims))
	for i, c := range claims {
		out[i] = contextbuilder.ClaimRef{AgentID: c.AgentID, Scope: c.Scope, ScopeType: c.ScopeType, ExpiresAt: c.ExpiresAt}
	}
	return out
}

func toContextPacket(p *contextbuilder.Packet) *ContextPacket {
	pkt := &ContextPacket{
		AgentID:             p.AgentID,
		EntityName:          p.EntityName,
		EntityType:          p.EntityType,
		GeneratedAt:         p.GeneratedAt,
		Phase:               SDLCPhase(p.Phase),
		QualityMode:         QualityMode(p.QualityMode),
		RootSummary:         p.RootSummary,
		DependencySummaries: p.DependencySummaries,
		Insight:             p.Insight,
		Concerns:            p.Concerns,
		PhaseGuidance:       p.PhaseGuidance,
		LLMUsed:             p.LLMUsed,
		PacketQuality:       p.PacketQuality,
		GraphWarnings:       p.GraphWarnings,
		ComplexityScore:     p.ComplexityScore,
		DeterministicPath:   p.DeterministicPath,
		QualityGate: QualityGate{
			RequireTests:   p.Gate.RequireTests,
			RequireDocs:    p.Gate.RequireDocs,
			RequirePRCheck: p.Gate.RequirePRCheck,
			Checklist:      p.Gate.Checklist,
		},
	}
	for _, c := range p.ActiveConstraints {
		pkt.ActiveConstraints = append(pkt.ActiveConstraints, ConstraintItem{
			RuleID: c.RuleID, Severity: c.Severity, Description: c.Description, Hint: c.Hint,
		})
	}
	for _, a := range p.TeamStatus {
		pkt.TeamStatus = append(pkt.TeamStatus, AgentStatus{
			AgentID: a.AgentID, Scope: a.Scope, ScopeType: a.ScopeType, ExpiresIn: a.ExpiresIn,
		})
	}
	for _, ph := range p.PatternHints {
		pkt.PatternHints = append(pkt.PatternHints, PatternHint{
			Trigger: ph.Trigger, CoChange: ph.CoChange, Reason: ph.Reason, Confidence: ph.Confidence,
		})
	}
	return pkt
}

// --- ADR methods ---

func (b *impl) UpsertADR(req ADRRequest) error {
	return b.store.UpsertADR(store.ADR{
		ID:           req.ID,
		Title:        req.Title,
		Status:       req.Status,
		ContextText:  req.ContextText,
		Decision:     req.Decision,
		Consequences: req.Consequences,
		LinkedFiles:  req.LinkedFiles,
	})
}

func (b *impl) GetADR(id string) (ADR, error) {
	a, err := b.store.GetADR(id)
	if err != nil {
		return ADR{}, err
	}
	return storeADRtoBrain(a), nil
}

func (b *impl) AllADRs() ([]ADR, error) {
	adrs, err := b.store.AllADRs()
	if err != nil {
		return nil, err
	}
	out := make([]ADR, len(adrs))
	for i, a := range adrs {
		out[i] = storeADRtoBrain(a)
	}
	return out, nil
}

func (b *impl) GetADRsForFile(filePath string, limit int) ([]ADR, error) {
	adrs, err := b.store.GetADRsForFile(filePath, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ADR, len(adrs))
	for i, a := range adrs {
		out[i] = storeADRtoBrain(a)
	}
	return out, nil
}

func (b *impl) QueryDecisions(_ context.Context, entityName string, limit int) ([]DecisionLogEntry, error) {
	entries, err := b.store.GetDecisionLog(entityName, limit)
	if err != nil {
		return nil, err
	}
	out := make([]DecisionLogEntry, len(entries))
	for i, e := range entries {
		out[i] = DecisionLogEntry{
			ID:              e.ID,
			AgentID:         e.AgentID,
			Phase:           e.Phase,
			EntityName:      e.EntityName,
			Action:          e.Action,
			RelatedEntities: e.RelatedEntities,
			Outcome:         e.Outcome,
			Notes:           e.Notes,
			CreatedAt:       e.CreatedAt,
		}
	}
	return out, nil
}

func storeADRtoBrain(a store.ADR) ADR {
	return ADR{
		ID:           a.ID,
		Title:        a.Title,
		Status:       a.Status,
		ContextText:  a.ContextText,
		Decision:     a.Decision,
		Consequences: a.Consequences,
		LinkedFiles:  a.LinkedFiles,
		CreatedAt:    a.CreatedAt,
		UpdatedAt:    a.UpdatedAt,
	}
}

// --- Quality detection & circuit breaker ---

// validateResponse checks if an LLM response meets minimum quality thresholds.
// Returns false for empty responses, responses that are too short, or responses
// that show common failure patterns (wall-of-text, prompt echo, etc.).
func validateResponse(resp string, minLen int) bool {
	resp = strings.TrimSpace(resp)
	if len(resp) < minLen {
		return false
	}
	// Detect repetitive output (same word repeated many times)
	words := strings.Fields(resp)
	if len(words) > 10 {
		counts := make(map[string]int)
		for _, w := range words {
			counts[strings.ToLower(w)]++
		}
		for _, c := range counts {
			if c > len(words)/2 {
				return false // single word is >50% of output = garbage
			}
		}
	}
	return true
}

// validateEnrichResponse checks the enricher output has non-empty insight.
func validateEnrichResponse(insight string, concerns []string) bool {
	return strings.TrimSpace(insight) != ""
}

// validateGuardianResponse checks the guardian output has non-empty explanation and fix.
func validateGuardianResponse(explanation, fix string) bool {
	return strings.TrimSpace(explanation) != "" && strings.TrimSpace(fix) != ""
}

// validateCoordinateResponse checks the orchestrator output has non-empty suggestion.
func validateCoordinateResponse(suggestion string) bool {
	return strings.TrimSpace(suggestion) != ""
}

// validateIngestResponse checks the ingestor output has non-empty summary.
func validateIngestResponse(summary string) bool {
	return strings.TrimSpace(summary) != "" && len(summary) >= 10
}

// heuristicEnrichInsight builds a last-resort insight string from graph
// topology data in the EnrichRequest — no LLM required.
// Called when all LLM tiers (T2 and T0) have their circuit breakers open.
// Returns a non-empty sentence so agents always receive useful context.
func heuristicEnrichInsight(req EnrichRequest) string {
	name := req.RootName
	if name == "" {
		name = "this entity"
	}
	nodeType := req.RootType
	if nodeType == "" {
		nodeType = "entity"
	}
	callers := len(req.CallerNames)
	callees := len(req.CalleeNames)

	switch {
	case callers > 0 && callees > 0:
		return fmt.Sprintf("%s (%s) has %d caller(s) and %d callee(s).", name, nodeType, callers, callees)
	case callers > 0:
		return fmt.Sprintf("%s (%s) has %d caller(s) and no direct callees.", name, nodeType, callers)
	case callees > 0:
		return fmt.Sprintf("%s (%s) has no callers and %d callee(s).", name, nodeType, callees)
	default:
		return fmt.Sprintf("%s (%s) has no recorded callers or callees.", name, nodeType)
	}
}

// relativeSourcePath extracts a package-relative path from an absolute or
// project-rooted file path. It walks common Go project directory markers
// (internal, cmd, pkg, src) and returns everything from the marker directory
// onward so engineers and agents see "internal/mcp/handlers.go" instead of
// just "handlers.go". Falls back to filepath.Base when no marker is found.
func relativeSourcePath(filePath string) string {
	for _, marker := range []string{"/internal/", "/cmd/", "/pkg/", "/src/"} {
		if idx := strings.Index(filePath, marker); idx >= 0 {
			return filePath[idx+1:]
		}
	}
	return filepath.Base(filePath)
}

// guardianTemplateFallback builds a last-resort violation explanation from the
// request fields — no LLM required.
// Called when all LLM tiers (T1 and T0) have their circuit breakers open.
// Returns a non-empty, actionable response so validate_plan always surfaces a
// useful message even when the brain is fully unavailable.
func guardianTemplateFallback(req ViolationRequest) ViolationResponse {
	description := req.Description
	if description == "" {
		description = req.RuleID
	}
	sourceFile := relativeSourcePath(req.SourceFile)
	targetName := req.TargetName
	if targetName == "" {
		targetName = "a restricted entity"
	}
	severity := req.RuleSeverity
	if severity == "" {
		severity = "warning"
	}
	explanation := fmt.Sprintf(
		"Rule violation (%s): %s imports or calls %q, which is restricted by rule %q.",
		severity, sourceFile, targetName, description,
	)
	fix := fmt.Sprintf(
		"Remove or replace the usage of %q in %s to comply with rule %q.",
		targetName, sourceFile, req.RuleID,
	)
	return ViolationResponse{Explanation: explanation, Fix: fix, Degraded: true}
}

// circuitBreaker tracks consecutive failures per operation tier and temporarily
// disables a tier after too many failures, preventing cascading retries.
type circuitBreaker struct {
	mu            sync.Mutex
	failures      map[string]int       // tier -> consecutive failure count
	disabledUntil map[string]time.Time // tier -> re-enable time
	maxFailures   int
	cooldown      time.Duration
}

func newCircuitBreaker(maxFailures int, cooldown time.Duration) *circuitBreaker {
	return &circuitBreaker{
		failures:      make(map[string]int),
		disabledUntil: make(map[string]time.Time),
		maxFailures:   maxFailures,
		cooldown:      cooldown,
	}
}

// isOpen returns true if the tier is currently disabled (circuit is open).
func (cb *circuitBreaker) isOpen(tier string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	until, ok := cb.disabledUntil[tier]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		// Cooldown expired — reset and allow through.
		delete(cb.disabledUntil, tier)
		cb.failures[tier] = 0
		return false
	}
	return true
}

// recordSuccess resets the failure counter for a tier.
func (cb *circuitBreaker) recordSuccess(tier string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures[tier] = 0
}

// recordFailure increments the failure counter. If maxFailures is reached,
// the tier is disabled for the cooldown duration.
func (cb *circuitBreaker) recordFailure(tier string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures[tier]++
	if cb.failures[tier] >= cb.maxFailures {
		cb.disabledUntil[tier] = time.Now().Add(cb.cooldown)
		logutil.Error("brain: circuit breaker tripped for tier %q — disabling for %v after %d consecutive failures\n",
			tier, cb.cooldown, cb.failures[tier])
	}
}

// status returns a snapshot of the circuit breaker state for all known tiers.
func (cb *circuitBreaker) status() map[string]TierState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	tiers := []string{"ingest", "enrich", "guardian", "orchestrate", "archivist", "context_builder"}
	out := make(map[string]TierState, len(tiers))
	now := time.Now()
	for _, t := range tiers {
		until := cb.disabledUntil[t]
		open := now.Before(until)
		remaining := 0.0
		if open {
			remaining = until.Sub(now).Seconds()
		}
		out[t] = TierState{Open: open, Failures: cb.failures[t], CooldownRemaining: remaining}
	}
	return out
}

// TierStatus implements TierStatusProvider for the /v1/health/tiers endpoint.
func (b *impl) TierStatus() map[string]TierState {
	return b.cb.status()
}

// BrainStats implements BrainStatsProvider — cumulative telemetry counters.
func (b *impl) BrainStats() map[string]interface{} {
	return b.stats.snapshot()
}
