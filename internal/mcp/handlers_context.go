package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/metrics"
	"github.com/SynapsesOS/synapses/internal/pulse"
	pulsetypes "github.com/SynapsesOS/synapses/internal/pulse/types"
	"github.com/SynapsesOS/synapses/internal/store"
)

// directionalContext is the response shape for get_context.
// Nodes are split into callers/callees/related so the LLM can immediately
// understand call direction without inspecting raw edge types.
// Annotations are surfaced first so agents see peer knowledge before graph structure.
// contextEnrichment holds auto-injected rules, failures, and task context
// appended to get_context responses without requiring extra tool calls.
type contextEnrichment struct {
	ApplicableRules []ruleHint    `json:"applicable_rules,omitempty"` // architectural rules for this entity's file
	RuleAlerts      []ruleAlert   `json:"rule_alerts,omitempty"`      // R19: actual violations found in the carved subgraph
	RecentFailures  []failureHint `json:"recent_failures,omitempty"`  // relevant failure episodes
	ActiveTask      *taskHint     `json:"active_task,omitempty"`      // linked task context
}
type ruleAlert struct {
	RuleID       string `json:"rule_id"`
	Description  string `json:"description"`
	Severity     string `json:"severity"`
	FromNode     string `json:"from_node"`
	ToNode       string `json:"to_node"`
	EdgeType     string `json:"edge_type"`
	SuggestedFix string `json:"suggested_fix,omitempty"`
}
type ruleHint struct {
	RuleID      string `json:"rule_id"`
	Description string `json:"description"`
	Severity    string `json:"severity"`
}
type failureHint struct {
	Decision  string `json:"decision"`
	Outcome   string `json:"outcome"`
	CreatedAt int64  `json:"created_at"`
}
type taskHint struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type directionalContext struct {
	Root                   *graph.Node                   `json:"root"`
	Annotations            map[string][]store.Annotation `json:"annotations,omitempty"`              // node_id → []Annotation — surfaced first for multi-agent visibility
	Enrichment             *contextEnrichment            `json:"enrichment,omitempty"`               // auto-injected rules, failures, task context
	Callees                []graph.CarvedNode            `json:"callees"`                            // root --CALLS--> node
	Callers                []graph.CarvedNode            `json:"callers"`                            // node --CALLS--> root
	Related                []graph.CarvedNode            `json:"related"`                            // everything else
	ContextPacket          *brain.ContextPacket          `json:"context_packet,omitempty"`           // LLM-enriched packet (present when brain is available)
	SuggestedNextTools     []toolSuggestion              `json:"suggested_next_tools,omitempty"`     // context-aware next steps
	Truncated              bool                          `json:"truncated,omitempty"`                // true when token budget cut results
	TruncatedCount         int                           `json:"truncated_count,omitempty"`          // nodes dropped by budget
	BrainHint              string                        `json:"brain,omitempty"`                    // set when brain is not configured
	Principles             []string                      `json:"principles,omitempty"`               // Hot Constitution principles from synapses.json
	ActivePrompts          []activePrompt                `json:"active_prompts,omitempty"`           // matched activation-context snippets from .synapses/prompts/
	ADRs                   []brain.ADR                   `json:"adrs,omitempty"`                     // relevant accepted ADRs for this entity's file
	StaleAnnotationWarning string                        `json:"stale_annotation_warning,omitempty"` // GAP-3: set when ≥1 annotation may be outdated
	RecentChanges          []metrics.CommitInfo          `json:"recent_changes,omitempty"`           // GAP-7: last 3 git commits that touched the entity's file
	GraphFreshness         string                        `json:"graph_freshness,omitempty"`          // GAP-4: warning when entity's file was recently modified
	AdaptiveHint           string                        `json:"adaptive_hint,omitempty"`            // F17: set when BFS depth/detail was auto-expanded based on prior feedback
	EntityMemories         []entityMemoryHint            `json:"entity_memories,omitempty"`          // R10: institutional knowledge attached to this entity
	QualityGaps            []store.QualityGap            `json:"quality_gaps,omitempty"`             // R32: open quality gaps on this entity
	EntityHash             string                        `json:"entity_hash,omitempty"`              // R14: SHA1 of node+neighbor IDs; stable cache key for clients
	CallerCountWarning     string                        `json:"caller_count_warning,omitempty"`     // DIAG-3: set when caller count is 0 for a method and use_go_types=false
	// R31: documentation sections linked to this code entity via DOCUMENTED_BY edges.
	Documentation []graph.CarvedNode `json:"documentation,omitempty"`
	// BUG-EVAL-9: disambiguation — present when multiple entities share the same name
	// and no file= hint was provided. Available in both JSON and compact formats.
	OtherCandidates []map[string]interface{} `json:"other_candidates,omitempty"` // all matching entities (including the one shown)
	DisambigHint    string                   `json:"disambig_hint,omitempty"`    // human-readable re-call instruction
	// RX2 Phase 4: cross-project BFS context from sibling stores (opt-in via projects= parameter).
	FederatedContexts []*federation.FederatedContext `json:"federated_contexts,omitempty"`
}

// computeEntityHash returns a short SHA1 hex digest that identifies the
// current ego-graph structure for a given root node. It is stable across
// identical graphs and changes when any node or edge in the subgraph changes.
// Clients can pass this back as known_hash to get an early {"unchanged":true}
// response instead of a full context payload.
//
// The hash covers:
//   - root node ID
//   - all neighbor node IDs (excluding root to avoid double-count)
//   - all edges as "from>to:type" strings
//
// Including edge types means interface/struct refactors (which change IMPLEMENTS
// edges but not node membership) correctly produce a different hash.
func computeEntityHash(rootID graph.NodeID, nodes []graph.CarvedNode, edges []*graph.Edge) string {
	parts := make([]string, 0, len(nodes)+1+len(edges))
	parts = append(parts, string(rootID))
	for _, cn := range nodes {
		// Skip root itself — CarveEgoGraph includes it in Nodes; counting it
		// twice would make the hash depend on irrelevant dedup order.
		if cn.Node.ID != rootID {
			parts = append(parts, string(cn.Node.ID))
		}
	}
	for _, e := range edges {
		parts = append(parts, string(e.From)+">"+string(e.To)+":"+string(e.Type))
	}
	sort.Strings(parts)
	h := sha256.New()
	for _, s := range parts {
		io.WriteString(h, s) // hash.Hash.Write never returns an error per Go spec
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:12] // 12 hex chars = 48 bits — ample for cache hit detection
}

// activePrompt is a matched activation-context snippet included in get_context
// responses. The Body is the full Markdown text from the prompt template.
type activePrompt struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

// entityMemoryHint is an institutional-knowledge entry surfaced alongside
// entity context from the unified memories table.
type entityMemoryHint struct {
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
	Source    string `json:"source"`
}

// handleGetContext returns an N-hop ego-subgraph around the named entity,
// split into callers, callees, and related buckets.
func (s *Server) handleGetContext(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	handlerStart := time.Now()
	entityName, ok := req.GetArguments()["entity"].(string)
	if !ok || entityName == "" {
		return mcp.NewToolResultError("entity is required (e.g., 'AuthService', 'handleLogin')"), nil
	}

	// Session ID for server-side auto-caching. Empty for stdio/test paths — auto-cache
	// is silently disabled when no session ID is present (getSessionHash returns "").
	sessionID := SessionIDFromContext(ctx)

	// Extract format/detailLevel early so they can be included in the session cache key.
	// These same values are used again at rendering time — reading from the same request,
	// so values are identical. Declaring here avoids a second GetArguments call later.
	//
	// Default format: "compact" (natural-language, ~200-600 tokens) rather than "json"
	// (~2000-3800 tokens). JSON is 4-6x larger with no benefit for agents that read text.
	// Agents that need structured JSON can pass format="json" explicitly.
	format, _ := req.GetArguments()["format"].(string)
	if format == "" {
		format = "compact"
	}
	detailLevel, _ := req.GetArguments()["detail_level"].(string)

	// GAP-1: Feedback loop.
	// (a) Track repeat calls — ≥3 calls for the same entity by the same agent
	//     auto-records a pattern episode: initial context wasn't sufficient.
	// (b) Optional explicit feedback via helpful=true/false.
	agentIDForFeedback, _ := req.GetArguments()["agent_id"].(string)
	// contextRefetched is set true when this is a repeat request for the same entity
	// in the same session. Captured here so it can be written to context_deliveries below.
	contextRefetched := false
	if agentIDForFeedback != "" && s.store != nil {
		repeatCount, sinceLast := s.trackContextCall(agentIDForFeedback, entityName)
		contextRefetched = repeatCount > 1
		// R29: disambiguate entity name for pulse signals when same name exists
		// in multiple packages. Resolves to "Name@dir/file" format.
		// Use pickBestNode (same scoring used by the main resolution path) for
		// deterministic selection — map iteration order is non-deterministic so
		// nodes[0] varies across process runs for common names like New/Close.
		pulseEntity := entityName
		if pulseNodes := s.graph.FindByName(entityName); len(pulseNodes) > 0 {
			pulseNode := pickBestNode(pulseNodes, s.graph)
			pulseEntity = entityWithPath(pulseNode.Name, pulseNode.File)
		}
		// P6-3: resolve pulse session ID early for outcome signals.
		earlyPulseSessID := s.getSynapseSessionID(sessionID)
		if repeatCount == 2 {
			// Sprint 15 #1: timing-aware correction signal.
			// < 5 min = moderate negative (context was immediately insufficient).
			// 5–30 min = mild negative (may be a different subtask angle).
			// ≥ 30 min = neutral: the GC already removed the entry and count is 1,
			//            so this branch is only reached for sinceLast < 30 min.
			sigType, sigWeight, emitSig := classifyRefetchSignal(sinceLast)
			if emitSig {
				if pc := s.getPulseClient(); pc != nil {
					evt := pulse.OutcomeSignalEvent{
						ProjectID:    s.projectID,
						AgentID:      agentIDForFeedback,
						Entity:       pulseEntity,
						SignalType:   sigType,
						Count:        repeatCount,
						SessionID:    earlyPulseSessID,
						SignalWeight: sigWeight,
					}
					s.goBackground(func() { pc.RecordOutcomeSignal(evt) })
				}
			}
		}
		if repeatCount == 3 {
			// R29: escalation signal — three or more fetches; strong negative.
			if pc := s.getPulseClient(); pc != nil {
				evt := pulse.OutcomeSignalEvent{
					ProjectID:    s.projectID,
					AgentID:      agentIDForFeedback,
					Entity:       pulseEntity,
					SignalType:   "escalation",
					Count:        repeatCount,
					SessionID:    earlyPulseSessID,
					SignalWeight: pulsetypes.SignalWeightEscalation,
				}
				s.goBackground(func() { pc.RecordOutcomeSignal(evt) })
			}
			ep := store.Episode{
				AgentID:     agentIDForFeedback,
				EpisodeType: "pattern",
				Outcome:     "partial",
				Trigger:     fmt.Sprintf("get_context called %dx for %q", repeatCount, entityName),
				Decision:    fmt.Sprintf("Repeated context requests for %q — initial slice may be too shallow or entity is large", entityName),
				Rationale:   "Three or more get_context calls for the same entity in one session signals the initial BFS depth or token budget wasn't sufficient. Consider increasing depth or using get_call_chain for deep traces.",
				Tags:        `["feedback","repeated_context","auto"]`,
				Importance:  0.3,
			}
			s.goBackground(func() {
				if _, err := s.store.RememberEpisode(ep); err != nil {
					log.Printf("mcp: auto-record repeat context episode: %v", err)
				}
			})
		}
	}
	if helpful, ok := req.GetArguments()["helpful"].(bool); ok && agentIDForFeedback != "" && s.store != nil {
		outcome, decision := "success", fmt.Sprintf("Context for %q was helpful", entityName)
		if !helpful {
			outcome, decision = "failure", fmt.Sprintf("Context for %q was not helpful — agent signalled miss", entityName)
		}
		ep := store.Episode{
			AgentID:     agentIDForFeedback,
			EpisodeType: "pattern",
			Outcome:     outcome,
			Trigger:     fmt.Sprintf("explicit feedback on get_context(%q)", entityName),
			Decision:    decision,
			Tags:        `["feedback","context_quality","explicit"]`,
			Importance:  0.4,
		}
		s.goBackground(func() { _, _ = s.store.RememberEpisode(ep) })
	}

	cfg := s.config.CarveConfig()

	// Sprint 13 gap: apply intent-based edge weights and directional bias when
	// the caller passes intent=. This mirrors prepare_context behaviour — same
	// applyIntentCarveConfig path — so get_context and prepare_context produce
	// identically-shaped traversals for the same intent.
	// When intent is absent, DirectionBoost stays at the default (0.2, slight
	// callee preference) set by DefaultCarveConfig.
	if intent, ok := req.GetArguments()["intent"].(string); ok && intent != "" {
		applyIntentCarveConfig(&cfg, intent)
	}

	// Sprint 13 #3: Semantic-structural hybrid scoring.
	// Wire in the embedding lookup so CarveEgoGraph can blend structural BFS/PPR
	// scores with cosine similarity to the root node's embedding.
	//
	// Lambda resolution (checked in order):
	//   1. context_carve.hybrid_lambda in synapses.json (explicit value including 0)
	//   2. Default 0.3 when not configured (70% structural, 30% semantic)
	//   3. Disabled when store is nil (no embeddings available)
	//
	// Uses *float64 config type so hybrid_lambda: 0 (disable) is distinguishable
	// from unset (apply default). Falls back to pure structural when embeddings
	// are not yet indexed (BatchGetNodeEmbeddings returns nil → no blend applied).
	if s.store != nil {
		st := s.store
		cfg.EmbeddingLookup = func(ids []graph.NodeID) map[graph.NodeID][]float32 {
			strIDs := make([]string, len(ids))
			for i, id := range ids {
				strIDs[i] = string(id)
			}
			raw := st.BatchGetNodeEmbeddings(strIDs)
			if raw == nil {
				return nil
			}
			out := make(map[graph.NodeID][]float32, len(raw))
			for k, v := range raw {
				out[graph.NodeID(k)] = v
			}
			return out
		}
		// Apply lambda: explicit config value wins (including 0.0 to disable);
		// nil (unset) falls back to default 0.3.
		if s.config != nil && s.config.ContextCarve.HybridLambda != nil {
			cfg.HybridLambda = *s.config.ContextCarve.HybridLambda
		} else {
			cfg.HybridLambda = 0.3 // default: 70% structural + 30% semantic
		}
	}

	// F17: Adaptive Context Learning — auto-expand depth/detail based on
	// stored feedback for this entity+agent before per-call explicit overrides
	// are applied. Explicit caller values always win over adaptive adjustments.
	adaptiveForceFullDetail := false
	if agentIDForFeedback != "" && s.store != nil {
		adaptiveForceFullDetail = s.adaptiveCarveConfig(&cfg, entityName, agentIDForFeedback)
	}

	// Sprint 11: apply model-based budget multiplier to the default budget.
	// Only applies when the agent did NOT explicitly pass token_budget.
	if mult := s.getSessionBudgetMultiplier(ctx); mult != 1.0 {
		cfg.TokenBudget = int(float64(cfg.TokenBudget) * mult)
	}

	// Allow per-call overrides of depth and token budget (always win over adaptive).
	if d, ok := req.GetArguments()["depth"].(float64); ok && d > 0 {
		cfg.MaxDepth = int(d)
	}
	if b, ok := req.GetArguments()["token_budget"].(float64); ok && b > 0 {
		cfg.TokenBudget = int(b)
	}

	// P1.6: Task-aware relevance boost — nodes linked to the active task float up.
	taskID, _ := req.GetArguments()["task_id"].(string)
	var boostedNodes map[graph.NodeID]bool
	if taskID != "" && s.store != nil {
		if task, err := s.store.GetTask(taskID); err == nil && len(task.LinkedNodes) > 0 {
			boostedNodes = make(map[graph.NodeID]bool, len(task.LinkedNodes))
			for _, id := range task.LinkedNodes {
				boostedNodes[graph.NodeID(id)] = true
			}
		}
	}

	// Optional file hint — narrows lookup to a specific file when entity names are ambiguous.
	fileHint, _ := req.GetArguments()["file"].(string)

	// Session auto-cache key: encodes the full request shape that determines which
	// subgraph is carved and which output format is produced. All fields that affect
	// either the entity_hash (depth, tokenBudget) or the usability of a cached
	// response (format, detailLevel, fileHint) are included.
	//
	// task_id is intentionally excluded: it only adjusts relevance scores AFTER the
	// subgraph is carved, so it does not change the entity_hash and two calls with
	// different task_ids for the same entity produce identical hashes.
	// R1: include_inferred controls whether synthetic route/HANDLES nodes appear.
	// Default is true (include them); false strips NodeRoute nodes from output.
	includeInferred := true
	if v, ok := req.GetArguments()["include_inferred"].(bool); ok {
		includeInferred = v
	}

	// cfg.UsePPR is included so that toggling use_ppr in synapses.json produces
	// a different cache key and agents never receive a stale {unchanged:true}
	// response that was computed under a different traversal algorithm.
	entityCacheKey := fmt.Sprintf("%s|%s|%s|%s|%d|%d|inferred:%v|ppr:%v",
		entityName, fileHint, format, detailLevel, cfg.MaxDepth, cfg.TokenBudget, includeInferred, cfg.UsePPR)

	// Resolve the entity name to a node ID.
	nodes := s.graph.FindByName(entityName)
	if len(nodes) == 0 {
		nodes = s.graph.FindByPatternLimit(entityName, 50)
	}
	// Dotted-name resolution: "Graph.New" where "New" is a standalone function
	// (not a method). FindByName only does suffix matching on stored names, so
	// "Graph.New" won't match a node named "New". Split on dot and filter by
	// whether the prefix appears in the node's ID or file path.
	if len(nodes) == 0 && strings.Contains(entityName, ".") {
		parts := strings.SplitN(entityName, ".", 2)
		prefix, method := strings.ToLower(parts[0]), parts[1]
		for _, n := range s.graph.FindByName(method) {
			if strings.Contains(strings.ToLower(string(n.ID)), prefix) ||
				strings.Contains(strings.ToLower(n.File), prefix) {
				nodes = append(nodes, n)
			}
		}
	}
	if len(nodes) == 0 {
		// Auto-fuzzy: surface candidates inline so agents don't need an extra find_entity round-trip.
		candidates := s.inlineFindEntity(entityName)
		if len(candidates) == 0 {
			if format == "compact" {
				return mcp.NewToolResultText(fmt.Sprintf(
					"entity not found: %q\nHint: no substring match. Try search(query=\"...\", mode=\"semantic\") for concept-based lookup.",
					entityName,
				)), nil
			}
			return jsonResult(map[string]interface{}{
				"error": fmt.Sprintf("entity not found: %q", entityName),
				"hint":  "No substring match. Try search(query=\"...\", mode=\"semantic\") for concept-based lookup.",
			})
		}
		if format == "compact" {
			var sb strings.Builder
			fmt.Fprintf(&sb, "entity not found: %q\nDid you mean one of these?\n", entityName)
			for _, c := range candidates {
				name, _ := c["name"].(string)
				file, _ := c["file"].(string)
				typ, _ := c["type"].(string)
				fmt.Fprintf(&sb, "  • %s (%s) in %s\n", name, typ, file)
			}
			sb.WriteString("Re-call get_context with entity= set to one of the exact names above. Add file= to pin if multiple files match.")
			return mcp.NewToolResultText(sb.String()), nil
		}
		return jsonResult(map[string]interface{}{
			"entity_not_found": entityName,
			"candidates":       candidates,
			"hint":             "Re-call get_context with entity= set to one of the exact names above. Add file= to pin if multiple files match.",
		})
	}

	// If a file hint is given, filter candidates to that file path (suffix match).
	if fileHint != "" {
		var filtered []*graph.Node
		for _, n := range nodes {
			if strings.HasSuffix(n.File, fileHint) || strings.Contains(n.File, fileHint) {
				filtered = append(filtered, n)
			}
		}
		if len(filtered) > 0 {
			nodes = filtered
		}
		// If no match, fall through to pick best from all candidates.
	}

	// Disambiguation: when multiple candidates exist and no file hint was given,
	// surface a disambiguation list alongside the best-guess result so agents
	// can confirm or re-call with file= if needed.
	// "file" is stored as the repo-relative path so it can be used directly
	// as the file= argument in a follow-up get_context call.
	var disambiguationCandidates []map[string]interface{}
	if len(nodes) > 1 && fileHint == "" {
		repoRoot := s.graph.Root()
		for _, n := range nodes {
			relFile := n.File
			if repoRoot != "" {
				relFile = strings.TrimPrefix(n.File, repoRoot+"/")
			}
			disambiguationCandidates = append(disambiguationCandidates, map[string]interface{}{
				"name": n.Name,
				"type": n.Type,
				"file": relFile, // repo-relative; pass as file= to pin
				"line": n.Line,
				"pkg":  n.Package,
			})
		}
	}

	best := pickBestNode(nodes, s.graph)

	if agentIDForFeedback != "" && s.store != nil {
		relFile := strings.TrimPrefix(best.File, s.graph.Root()+"/")
		s.upsertAgentWithActivity(agentIDForFeedback, &store.AgentActivity{
			Focus:      best.Name,
			FocusFile:  relFile,
			FocusSince: time.Now().UTC().Format(time.RFC3339),
		})
	}

	traversalStart := time.Now()
	subgraph, err := s.graph.CarveEgoGraph(best.ID, cfg)
	traversalDurationMs := float64(time.Since(traversalStart).Microseconds()) / 1000.0
	if err != nil {
		return toolError("carve ego graph", err)
	}

	// normalizeSubgraph deep-copies nodes, so boosting relevance on the result
	// is safe — we never mutate the cached subgraph.
	sg := normalizeSubgraph(subgraph, s.graph.Root())

	// P2.2: impact mode — reverse-only BFS, same shape as get_impact.
	// Extract mode early so the known_hash short-circuit below is skipped for
	// impact requests (impact uses a different BFS and has no entity_hash).
	mode, _ := req.GetArguments()["mode"].(string)

	// R14 Part B: entity_hash — stable fingerprint of the ego-graph structure.
	// If the caller provides known_hash matching the current hash, return an
	// early "unchanged" response so agents can skip re-processing stale context.
	// Skipped for mode=impact — that path produces a different response shape.
	entityHash := computeEntityHash(best.ID, sg.Nodes, sg.Edges)
	// Resolve pulse session ID early so cache-hit paths can use it too.
	pulseSessID := s.getSynapseSessionID(sessionID)
	if mode == "" {
		explicitKnownHash, _ := req.GetArguments()["known_hash"].(string)
		if explicitKnownHash != "" {
			// Agent is explicitly managing cache with their own hash.
			// Only return unchanged when it matches; a mismatch falls through to
			// the full response. Session auto-cache is bypassed entirely when an
			// explicit known_hash is present — the agent owns the decision.
			if explicitKnownHash == entityHash {
				cacheAgentID := agentIDForFeedback
				if cacheAgentID == "" {
					cacheAgentID = s.getLastAgent()
				}
				cacheResp := map[string]interface{}{"unchanged": true, "entity_hash": entityHash, "entity": entityName}
				durationMs := time.Since(handlerStart).Milliseconds()
				s.goBackground(func() {
					s.emitContextDelivery("get_context", cacheAgentID, entityName, best.File,
						cacheResp, sg.Nodes, sg.Edges, sg.TruncatedCount, sg.Truncated,
						false, true, durationMs, pulseSessID, nil)
				})
				return jsonResult(cacheResp)
			}
		} else if sessionID != "" {
			// No explicit hash — use server-side session auto-cache: if this session
			// already received a full response for this entity+config and the graph
			// hasn't changed since, return {unchanged:true} automatically.
			// Disabled when sessionID=="" (stdio path, tests).
			if s.getSessionHash(sessionID, entityCacheKey) == entityHash {
				cacheAgentID := agentIDForFeedback
				if cacheAgentID == "" {
					cacheAgentID = s.getLastAgent()
				}
				cacheResp := map[string]interface{}{"unchanged": true, "entity_hash": entityHash, "entity": entityName, "cache_source": "session"}
				durationMs := time.Since(handlerStart).Milliseconds()
				s.goBackground(func() {
					s.emitContextDelivery("get_context", cacheAgentID, entityName, best.File,
						cacheResp, sg.Nodes, sg.Edges, sg.TruncatedCount, sg.Truncated,
						false, true, durationMs, pulseSessID, nil)
				})
				// Respect the requested format — agent expected compact text, not JSON.
				// Explicit known_hash paths stay JSON (agent manages their own cache protocol).
				if format == "compact" {
					return mcp.NewToolResultText(fmt.Sprintf(
						"unchanged: true\nentity_hash: %s\nentity: %s\ncache_source: session",
						entityHash, entityName,
					)), nil
				}
				return jsonResult(cacheResp)
			}
		}
	}

	if len(boostedNodes) > 0 {
		for i := range sg.Nodes {
			if boostedNodes[sg.Nodes[i].Node.ID] {
				sg.Nodes[i].Relevance *= 1.5
				if sg.Nodes[i].Relevance > 1.0 {
					sg.Nodes[i].Relevance = 1.0
				}
			}
		}
	}
	if mode == "impact" {
		maxDepth := cfg.MaxDepth
		result, err := s.graph.ImpactAnalysis(best.ID, maxDepth)
		if err != nil {
			return toolError("impact analysis", err)
		}
		return jsonResult(result)
	}

	dc := toDirectionalContext(sg)

	// R1: strip synthetic route/inferred nodes when include_inferred=false.
	if !includeInferred {
		dc.Callees = filterInferredNodes(dc.Callees)
		dc.Callers = filterInferredNodes(dc.Callers)
		dc.Related = filterInferredNodes(dc.Related)
	}

	// Brain enrichment: async pattern — serve raw graph immediately, enrich in background.
	// If a cached packet exists, attach it (fast path). Otherwise, kick off background
	// enrichment so the next get_context call for this entity picks up the enriched version.
	if bc := s.getBrainClient(); bc != nil {
		// R1: include includeInferred in cache key — a packet built from filtered
		// callees must not be served for an unfiltered request (different content).
		cacheKey := fmt.Sprintf("%s:%d:%v", entityName, cfg.MaxDepth, includeInferred)
		if pkt := s.getPacketFromCache(cacheKey); pkt != nil {
			dc.ContextPacket = pkt
		} else {
			// Async enrichment: return raw graph now, enrich in background.
			dc.BrainHint = "enrichment in progress — call get_context again in a few seconds for brain-enriched results"
			s.goBackground(func() { s.asyncEnrichContext(bc, cacheKey, dc, best, taskID) })
		}
	} else {
		dc.BrainHint = "not configured — add brain.url to synapses.json for semantic enrichment"
	}

	// ── Parallel enrichment: run independent I/O-bound queries concurrently ──
	// Each goroutine writes to a separate field of dc — no shared writes.
	var enrichWg sync.WaitGroup

	// 1. Annotations from all agents (store query).
	enrichWg.Add(1)
	go func() {
		defer enrichWg.Done()
		if s.store == nil {
			return
		}
		nodeIDs := make([]string, 0, len(sg.Nodes)+1)
		nodeIDs = append(nodeIDs, string(sg.Root))
		for _, cn := range sg.Nodes {
			nodeIDs = append(nodeIDs, string(cn.Node.ID))
		}
		if annMap, err := s.store.GetAnnotationsForNodes(nodeIDs); err == nil && len(annMap) > 0 {
			dc.Annotations = annMap
			var staleCount int
			for _, anns := range annMap {
				for _, a := range anns {
					if a.Stale {
						staleCount++
					}
				}
			}
			if staleCount > 0 {
				dc.StaleAnnotationWarning = fmt.Sprintf(
					"⚠ %d annotation(s) may be stale — the code was significantly refactored since they were written. Treat them as hints, not facts. Re-annotate with annotate_node() if they are wrong.",
					staleCount,
				)
			}
		}
	}()

	// 2. Context enrichment: rules, failures, and task context (store + config queries).
	enrichWg.Add(1)
	go func() {
		defer enrichWg.Done()
		if s.store == nil || best == nil {
			return
		}
		var enrichment contextEnrichment

		// Load all rules once (config + dynamic) to avoid redundant SQLite queries.
		var allRules []config.Rule
		if s.config != nil {
			allRules = append(allRules, s.config.Rules...)
		}
		if dynRules, err := s.store.LoadDynamicRules(); err == nil {
			allRules = append(allRules, dynRules...)
		}

		// ApplicableRules: which rules match this entity's file (informational).
		for _, r := range allRules {
			matched := r.ForbiddenEdge.FromFilePattern == ""
			if !matched {
				matched, _ = filepath.Match(r.ForbiddenEdge.FromFilePattern, filepath.Base(best.File))
			}
			if matched {
				enrichment.ApplicableRules = append(enrichment.ApplicableRules, ruleHint{
					RuleID:      r.ID,
					Description: r.Description,
					Severity:    r.Severity,
				})
			}
		}

		// R19: Proactive rule alerts — check carved subgraph edges against all rules.
		// This surfaces actual violations in the entity's neighborhood, not just
		// which rules apply to the file. Cap at 5 to avoid overwhelming the response.
		if len(allRules) > 0 && len(sg.Edges) > 0 {
			checker := &config.Config{Rules: allRules}
			violations := checker.CheckViolationsForEdges(sg.Edges, s.graph.GetNode)
			for i, v := range violations {
				if i >= 5 {
					break
				}
				enrichment.RuleAlerts = append(enrichment.RuleAlerts, ruleAlert{
					RuleID:       v.RuleID,
					Description:  v.Description,
					Severity:     v.Severity,
					FromNode:     string(v.FromNode),
					ToNode:       string(v.ToNode),
					EdgeType:     string(v.EdgeType),
					SuggestedFix: v.SuggestedFix,
				})
			}
		}

		if matches, err := s.store.RecallEpisodes(
			best.Name, s.graph.RepoID(), "", "failure", "", 2, 0,
		); err == nil {
			for _, ep := range matches {
				enrichment.RecentFailures = append(enrichment.RecentFailures, failureHint{
					Decision:  ep.Decision,
					Outcome:   ep.Outcome,
					CreatedAt: ep.CreatedAt,
				})
			}
		}

		if taskID != "" {
			if task, err := s.store.GetTask(taskID); err == nil {
				enrichment.ActiveTask = &taskHint{
					ID:     task.ID,
					Title:  task.Title,
					Status: task.Status,
				}
			}
		}

		if len(enrichment.ApplicableRules) > 0 || len(enrichment.RuleAlerts) > 0 || len(enrichment.RecentFailures) > 0 || enrichment.ActiveTask != nil {
			dc.Enrichment = &enrichment
		}
	}()

	// 3. Git "why" layer — recent commits for the entity's file (spawns git subprocess).
	enrichWg.Add(1)
	go func() {
		defer enrichWg.Done()
		if dc.Root == nil || dc.Root.File == "" {
			return
		}
		repoRoot := s.graph.Root()
		if repoRoot != "" {
			if commits := metrics.RecentCommitsForFile(ctx, repoRoot, dc.Root.File, 3); len(commits) > 0 {
				dc.RecentChanges = commits
			}
		}
	}()

	// 4. ADRs from brain sidecar (HTTP call, fail-silent).
	enrichWg.Add(1)
	go func() {
		defer enrichWg.Done()
		bc := s.getBrainClient()
		if bc == nil || dc.Root == nil || dc.Root.File == "" {
			return
		}
		if adrs, err := bc.GetADRs(ctx, dc.Root.File); err == nil && len(adrs) > 0 {
			if len(adrs) > 2 {
				adrs = adrs[:2]
			}
			dc.ADRs = adrs
		}
	}()

	// 5. Entity memories from unified memories table (R10).
	// Guard: best.ID must be non-empty — empty string would fetch ALL entity memories.
	enrichWg.Add(1)
	go func() {
		defer enrichWg.Done()
		if s.store == nil || best == nil || best.ID == "" {
			return
		}
		mems, err := s.store.QueryMemories(store.TierEntity, string(best.ID), "", 3)
		if err != nil || len(mems) == 0 {
			return
		}
		hints := make([]entityMemoryHint, 0, len(mems))
		for _, m := range mems {
			hints = append(hints, entityMemoryHint{
				Content:   m.Content,
				CreatedAt: m.CreatedAt,
				Source:    m.Source,
			})
			s.store.TouchMemory(m.ID)
		}
		dc.EntityMemories = hints
	}()

	// Wait for all enrichment goroutines to complete.
	enrichWg.Wait()

	// DIAG-3: warn when caller count is 0 for a method and use_go_types is disabled.
	// Method calls via local variables (e.g. g.Method()) are silently unresolved
	// without use_go_types, so "0 callers" is often false rather than accurate.
	if len(dc.Callers) == 0 && best != nil && best.Type == graph.NodeMethod &&
		s.config != nil && !s.config.UseGoTypes {
		dc.CallerCountWarning = "⚠ caller_confidence: incomplete — this is a method and use_go_types=false in synapses.json; callers invoked via local variables (e.g. v.Method()) are not in the graph. Actual caller count may be higher. Enable use_go_types: true for complete Go method call tracking."
	}

	// R32: surface open quality gaps on this entity.
	if s.store != nil && dc.Root != nil {
		if gaps, err := s.store.GetGaps(store.GapFilter{NodeID: string(dc.Root.ID), Status: "open"}); err == nil && len(gaps) > 0 {
			dc.QualityGaps = gaps
		}
	}

	// ── Sequential enrichment (fast, in-memory only) ──

	// Context-aware next-step suggestions.
	dc.SuggestedNextTools = suggestNextAfterContext(dc)

	// Hot Constitution: inject project principles if configured.
	if s.config != nil && s.config.Constitution.InjectInContext && len(s.config.Constitution.Principles) > 0 {
		dc.Principles = s.config.Constitution.Principles
	}

	// Activation-context prompts: inject matching snippets from .synapses/prompts/.
	if dc.Root != nil {
		matched := s.getMatchingPrompts(dc.Root.File, dc.Root.Name, dc.Root.Package)
		if len(matched) > 0 {
			dc.ActivePrompts = make([]activePrompt, 0, len(matched))
			for _, pt := range matched {
				dc.ActivePrompts = append(dc.ActivePrompts, activePrompt{ID: pt.ID, Body: pt.Body})
			}
		}
	}

	// GAP-4: Graph freshness — warn when the entity's file was modified very recently.
	if dc.Root != nil && dc.Root.File != "" {
		if fi, err := os.Stat(dc.Root.File); err == nil {
			age := time.Since(fi.ModTime())
			if age < 10*time.Second {
				dc.GraphFreshness = fmt.Sprintf(
					"⚠ File modified %s ago — graph may not reflect latest changes. Re-call after a few seconds if results seem stale.",
					age.Round(time.Second),
				)
			}
		}
	}

	// F17: surface adaptive expansion hint in the response so agents know why
	// they received deeper context than the default.
	if adaptiveForceFullDetail {
		dc.AdaptiveHint = "⟳ Context depth auto-expanded based on prior feedback for this entity."
	}

	// Attach entity_hash to the response so clients can cache and compare.
	// Must be set BEFORE the telemetry goroutine reads dc via json.Marshal.
	dc.EntityHash = entityHash

	// ── Fire-and-forget telemetry (non-blocking) ──
	agentID, _ := req.GetArguments()["agent_id"].(string)
	if agentID == "" {
		agentID = s.getLastAgent()
	}
	durationMs := time.Since(handlerStart).Milliseconds()
	hasBrain := dc.ContextPacket != nil

	// P6-1: compute extra delivery metrics for full (non-cache-hit) responses.
	depthAchieved := 0
	for _, cn := range sg.Nodes {
		if cn.Hop > depthAchieved {
			depthAchieved = cn.Hop
		}
	}
	edgeDist := make(map[string]int, 4)
	for _, e := range sg.Edges {
		edgeDist[string(e.Type)]++
	}
	edgeDistJSON, _ := json.Marshal(edgeDist)
	var rulesMatched, violationsFound int
	if dc.Enrichment != nil {
		rulesMatched = len(dc.Enrichment.ApplicableRules)
		violationsFound = len(dc.Enrichment.RuleAlerts)
	}

	deliveryExtras := &contextDeliveryExtras{
		Intent:               mode,
		DepthRequested:       cfg.MaxDepth,
		DepthAchieved:        depthAchieved,
		NodesVisited:         len(sg.Nodes) + sg.TruncatedCount,
		AnnotationsIncluded:  dc.Annotations != nil,
		OutputFormat:         format,
		EdgeTypesDist:        string(edgeDistJSON),
		TraversalDurationMs:  traversalDurationMs,
		GraphSizeAtTraversal: s.graph.NodeCount(),
		DetailLevel:          detailLevel,
		RulesMatched:         rulesMatched,
		ViolationsFound:      violationsFound,
		MinRelevanceHits:     sg.TruncatedCount,
		TokenBudgetHit:       sg.Truncated,
		Refetched:            contextRefetched,
		CacheSize:            s.graph.CacheLen(), // P9-8
	}

	s.goBackground(func() {
		s.emitContextDelivery(
			"get_context", agentID, entityName, best.File,
			dc, sg.Nodes, sg.Edges,
			sg.TruncatedCount,
			sg.Truncated,
			hasBrain,
			false,
			durationMs,
			pulseSessID,
			deliveryExtras,
		)
	})

	// Multi-agent awareness: fire-and-forget event so peers can see this via get_events.
	// Uses agentIDForFeedback (same value as agentID when provided). upsertAgentWithActivity
	// is NOT called here — it already ran synchronously above with the full
	// Focus+FocusFile+FocusSince payload, making a second call redundant.
	if agentIDForFeedback != "" && s.store != nil {
		relFileForEvent := strings.TrimPrefix(best.File, s.graph.Root()+"/")
		aid := agentIDForFeedback
		s.goBackground(func() {
			payload, _ := json.Marshal(map[string]string{
				"entity": entityName,
				"file":   relFileForEvent,
			})
			if err := s.store.AppendEvent("agent_examining", aid, string(payload)); err != nil {
				log.Printf("mcp: append agent_examining event: %v", err)
			}
		})
	}

	// BUG-EVAL-9: populate disambiguation fields on dc BEFORE format dispatch
	// so both compact and JSON responses include other_candidates when ambiguous.
	if len(disambiguationCandidates) > 1 {
		dc.OtherCandidates = disambiguationCandidates
		dc.DisambigHint = fmt.Sprintf("%d entities named %q found. Showing best match. Re-call with file=\"path/suffix\" to pin to a specific file.", len(disambiguationCandidates), entityName)
	}

	// format=compact returns a natural-language briefing instead of the default JSON blob.
	// Cross-project context: when projects= is specified, include BFS context
	// from sibling stores. This is opt-in for deep cross-project exploration.
	projectsParam, _ := req.GetArguments()["projects"].(string)
	if projectsParam != "" && s.federationResolver != nil {
		var aliases []string
		if projectsParam != "*" {
			for _, a := range strings.Split(projectsParam, ",") {
				if a = strings.TrimSpace(a); a != "" {
					aliases = append(aliases, a)
				}
			}
		} else {
			aliases = s.federationResolver.Aliases()
		}
		fedCtx, fedCancel := context.WithTimeout(ctx, 2*time.Second)
		for _, alias := range aliases {
			fedStart := time.Now()
			fc := s.federationResolver.GetEntityContext(fedCtx, entityName, alias, cfg.MaxDepth)
			fedDuration := float64(time.Since(fedStart).Milliseconds())
			if fc != nil {
				dc.FederatedContexts = append(dc.FederatedContexts, fc)
			}
			// P10-2: emit federation resolver timing to Pulse.
			if pc := s.getPulseClient(); pc != nil {
				nodeCount := 0
				if fc != nil {
					nodeCount = fc.NodeCount
				}
				pc.RecordFederationEvent(pulse.FederationDetectEvent{
					AgentID:        agentIDForFeedback,
					ProjectID:      s.projectID,
					SiblingProject: alias,
					DepsFound:      nodeCount,
					DurationMs:     fedDuration,
					EventType:      "resolver_context",
				})
			}
		}
		fedCancel()
	}

	// Sprint 6.7: passive context delivery instrumentation for Sprint 11 feedback loop.
	// Fire-and-forget — no latency added to hot path.
	if s.store != nil {
		synapseSessionID := s.getSynapseSessionID(sessionID)
		cd := store.ContextDelivery{
			SessionID: synapseSessionID,
			AgentID:   agentIDForFeedback,
			ToolName:  "get_context",
			Entity:    entityName,
			Refetched: contextRefetched,
		}
		s.goBackground(func() { s.store.InsertContextDelivery(cd) })
	}

	// detail_level controls depth: "summary" (~50t), "neighbors" (~200t), "full" (~400-600t).
	// format and detailLevel were extracted early for the session cache key; reused here.
	if format == "compact" {
		// F17: if no explicit detail_level given, honour the adaptive expansion first.
		// Otherwise default to "neighbors" (~200 tokens) — leans lean; agents can
		// pass detail_level="full" explicitly when they need callee detail blocks.
		if detailLevel == "" && adaptiveForceFullDetail {
			detailLevel = "full"
		} else if detailLevel == "" {
			detailLevel = "neighbors"
		}
		s.setSessionHash(sessionID, entityCacheKey, entityHash)
		return mcp.NewToolResultText(serializeCompact(dc, detailLevel)), nil
	}

	s.setSessionHash(sessionID, entityCacheKey, entityHash)
	return jsonResult(dc)
}

// suggestNextAfterContext returns ordered tool suggestions based on what get_context found.
// Helps agents decide what to do after exploring an entity.
// toolSuggestion is a structured next-step hint for LLMs.
type toolSuggestion struct {
	Tool   string `json:"tool"`
	Reason string `json:"reason"`
}

func suggestNextAfterContext(dc *directionalContext) []toolSuggestion {
	var suggestions []toolSuggestion

	// Surface quality gaps first — agent should review known debt before editing.
	if len(dc.QualityGaps) > 0 {
		suggestions = append(suggestions, toolSuggestion{
			Tool:   "get_gaps",
			Reason: fmt.Sprintf("%d open quality gap(s) on this entity — review before modifying", len(dc.QualityGaps)),
		})
	}

	// Suggest prepare_context first when the agent likely needs a broader view.
	if len(dc.Callers) > 3 {
		suggestions = append(suggestions, toolSuggestion{
			Tool:   "prepare_context",
			Reason: fmt.Sprintf("high blast radius (%d callers) — use intent=\"modify\" for a safe-edit briefing or intent=\"plan\" for scope assessment", len(dc.Callers)),
		})
	}

	if len(dc.Callers) > 0 {
		suggestions = append(suggestions, toolSuggestion{
			Tool:   "get_impact",
			Reason: fmt.Sprintf("%d callers found — check blast radius before modifying", len(dc.Callers)),
		})
	}
	if len(dc.Callees) > 0 {
		suggestions = append(suggestions, toolSuggestion{
			Tool:   "get_call_chain",
			Reason: fmt.Sprintf("%d callees found — trace exact path between two entities", len(dc.Callees)),
		})
	}
	suggestions = append(suggestions,
		toolSuggestion{Tool: "validate_plan", Reason: "check proposed changes against architectural rules"},
		toolSuggestion{Tool: "annotate_node", Reason: "leave a note for other agents on a key finding"},
	)
	return suggestions
}

// asyncEnrichContext kicks off a background brain enrichment for a get_context call.
// The result is cached so subsequent get_context calls for the same entity pick it up.
// This is fire-and-forget — errors are logged but do not affect the caller.
func (s *Server) asyncEnrichContext(
	bc *brain.Client,
	cacheKey string,
	dc *directionalContext,
	best *graph.Node,
	taskID string,
) {
	calleeNames := make([]string, 0, len(dc.Callees))
	for _, c := range dc.Callees {
		calleeNames = append(calleeNames, c.Node.Name)
	}
	callerNames := make([]string, 0, len(dc.Callers))
	for _, c := range dc.Callers {
		callerNames = append(callerNames, c.Node.Name)
	}

	rules := matchRulesForFile(s.config, best.File)

	hasTests := fileHasTests(best.File)
	fanIn := s.graph.Fanin(best.ID)

	// Hard 200ms timeout: brain enrichment must not block the cache write path.
	// If brain is slow or unavailable, we silently skip caching — the next
	// get_context call will trigger another background attempt.
	enrichCtx, enrichCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer enrichCancel()

	brainStart := time.Now()
	pkt := bc.BuildContextPacket(enrichCtx, brain.ContextPacketRequest{
		ProjectID: s.projectID,
		Snapshot: brain.SnapshotInput{
			RootNodeID:      string(best.ID),
			RootName:        best.Name,
			RootType:        string(best.Type),
			RootFile:        best.File,
			RootDoc:         best.Metadata["doc"],
			CalleeNames:     calleeNames,
			CallerNames:     callerNames,
			ApplicableRules: rules,
			ActiveClaims:    nil,
			TaskID:          taskID,
			HasTests:        hasTests,
			FanIn:           fanIn,
		},
		EnableLLM: s.config.Brain.ContextBuilder,
	})
	brainDuration := time.Since(brainStart).Milliseconds()

	// Record brain usage to pulse for cost/usage tracking (non-blocking).
	if pc := s.getPulseClient(); pc != nil {
		brainSuccess := pkt != nil
		s.goBackground(func() {
			pc.RecordBrainUsage(pulse.BrainUsageEvent{
				Tier:         "enrich",
				Endpoint:     "BuildContextPacket",
				DurationMs:   brainDuration,
				ProjectID:    s.projectID,
				TargetEntity: best.Name,
				Success:      brainSuccess,
			})
		})
	}

	// Sanitize: don't cache "enrichment in progress" placeholder packets —
	// they would poison the cache and suppress future real enrichments.
	if pkt != nil && !strings.Contains(strings.ToLower(pkt.RootSummary), "in progress") {
		s.setPacketCache(cacheKey, pkt)
	}
}

// fileHasTests returns true if a _test.go file exists in the same directory
// as the given source file (Go convention). For non-Go files, checks for
// a test file with common naming patterns in the same directory.
func fileHasTests(file string) bool {
	if file == "" {
		return false
	}
	dir := filepath.Dir(file)
	base := filepath.Base(file)
	ext := filepath.Ext(base)
	stem := base[:len(base)-len(ext)]

	// Go: check for stem_test.go or any *_test.go in the directory.
	if ext == ".go" {
		testFile := filepath.Join(dir, stem+"_test.go")
		if _, err := filepath.Glob(testFile); err == nil {
			if _, statErr := filepath.Abs(testFile); statErr == nil {
				// Use exec.LookPath-style check via os.Stat.
				if matches, _ := filepath.Glob(filepath.Join(dir, "*_test.go")); len(matches) > 0 {
					return true
				}
			}
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
		return len(matches) > 0
	}

	// Python/TypeScript: check for test_*.py, *.test.ts, *.spec.ts, etc.
	patterns := []string{
		filepath.Join(dir, "test_*.py"),
		filepath.Join(dir, "*_test.py"),
		filepath.Join(dir, stem+".test"+ext),
		filepath.Join(dir, stem+".spec"+ext),
	}
	for _, p := range patterns {
		if m, _ := filepath.Glob(p); len(m) > 0 {
			return true
		}
	}
	return false
}

// matchRulesForFile returns the slim rule descriptors applicable to the given
// file. Rules with no from_file_pattern always apply; rules with a pattern
// apply only when the file path matches.
func matchRulesForFile(cfg *config.Config, file string) []brain.RuleInput {
	var out []brain.RuleInput
	for _, r := range cfg.Rules {
		if r.ForbiddenEdge.FromFilePattern == "" {
			out = append(out, brain.RuleInput{
				RuleID:      r.ID,
				Severity:    r.Severity,
				Description: r.Description,
			})
			continue
		}
		matched, _ := filepath.Match(r.ForbiddenEdge.FromFilePattern, filepath.Base(file))
		if matched {
			out = append(out, brain.RuleInput{
				RuleID:      r.ID,
				Severity:    r.Severity,
				Description: r.Description,
			})
		}
	}
	return out
}

// toDirectionalContext reshapes a flat SubGraph into the directional form.
// Classification is based on direct CALLS edges incident on the root node.
func toDirectionalContext(sg *graph.SubGraph) *directionalContext {
	// Build sets of nodes directly called by / directly calling the root.
	// R1: also include HANDLES edges so route nodes surface as callees.
	// R31: also track DOCUMENTED_BY targets (section nodes documenting root).
	calleesOfRoot := make(map[graph.NodeID]bool)
	callersOfRoot := make(map[graph.NodeID]bool)
	docsOfRoot := make(map[graph.NodeID]bool)
	for _, e := range sg.Edges {
		switch e.Type {
		case graph.EdgeCalls, graph.EdgeHandles:
			if e.From == sg.Root {
				calleesOfRoot[e.To] = true
			}
			if e.To == sg.Root {
				callersOfRoot[e.From] = true
			}
		case graph.EdgeDocumentedBy:
			// code entity → section: section is a doc of root
			if e.From == sg.Root {
				docsOfRoot[e.To] = true
			}
		case graph.EdgeExplains:
			// section → code entity: section explains root
			if e.To == sg.Root {
				docsOfRoot[e.From] = true
			}
		}
	}

	dc := &directionalContext{
		Truncated:      sg.Truncated,
		TruncatedCount: sg.TruncatedCount,
	}
	for i := range sg.Nodes {
		cn := sg.Nodes[i]
		id := cn.Node.ID
		switch {
		case id == sg.Root:
			dc.Root = cn.Node
		case docsOfRoot[id]:
			dc.Documentation = append(dc.Documentation, cn)
		case calleesOfRoot[id]:
			dc.Callees = append(dc.Callees, cn)
		case callersOfRoot[id]:
			dc.Callers = append(dc.Callers, cn)
		default:
			dc.Related = append(dc.Related, cn)
		}
	}

	// Sort each bucket by relevance descending.
	byRelevance := func(a, b graph.CarvedNode) int {
		if a.Relevance > b.Relevance {
			return -1
		}
		if a.Relevance < b.Relevance {
			return 1
		}
		return 0
	}
	sort.Slice(dc.Callees, func(i, j int) bool { return byRelevance(dc.Callees[i], dc.Callees[j]) < 0 })
	sort.Slice(dc.Callers, func(i, j int) bool { return byRelevance(dc.Callers[i], dc.Callers[j]) < 0 })
	sort.Slice(dc.Related, func(i, j int) bool { return byRelevance(dc.Related[i], dc.Related[j]) < 0 })
	sort.Slice(dc.Documentation, func(i, j int) bool { return byRelevance(dc.Documentation[i], dc.Documentation[j]) < 0 })

	return dc
}
func (s *Server) handleGetImpact(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	symbol, _ := req.GetArguments()["symbol"].(string)
	if symbol == "" {
		return mcp.NewToolResultError("symbol is required (e.g., 'AuthService', 'handleLogin')"), nil
	}

	maxDepth := 3
	if d, ok := req.GetArguments()["depth"].(float64); ok && d > 0 {
		maxDepth = int(d)
		if maxDepth > 10 {
			maxDepth = 10
		}
	}

	tokenBudget := 2000
	// Sprint 11: apply model-based budget multiplier to the default budget.
	if mult := s.getSessionBudgetMultiplier(ctx); mult != 1.0 {
		tokenBudget = int(float64(tokenBudget) * mult)
	}
	if tb, ok := req.GetArguments()["token_budget"].(float64); ok && tb > 0 {
		tokenBudget = int(tb)
	}

	// Resolve symbol name → node. Fall back to pattern match (same as get_context).
	candidates := s.graph.FindByName(symbol)
	if len(candidates) == 0 {
		candidates = s.graph.FindByPatternLimit(symbol, 50)
	}
	if len(candidates) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("entity not found: %q", symbol)), nil
	}
	root := pickBestNode(candidates, s.graph)

	// For struct/interface nodes, aggregate impact across all their methods.
	// A struct itself has no incoming CALLS edges — its methods do.
	if root.Type == graph.NodeStruct || root.Type == graph.NodeInterface {
		methods := s.graph.FindByPatternLimit(root.Name, 100)
		merged := &graph.ImpactResult{Tiers: []graph.ImpactTier{}}
		seen := make(map[graph.NodeID]bool)
		for _, m := range methods {
			if m.Type != graph.NodeMethod || m.ID == root.ID {
				continue
			}
			r, err2 := s.graph.ImpactAnalysis(m.ID, maxDepth)
			if err2 != nil || r == nil {
				continue
			}
			for _, tier := range r.Tiers {
				var tierNodes []graph.EntityRef
				for _, ref := range tier.Nodes {
					if !seen[ref.ID] {
						seen[ref.ID] = true
						tierNodes = append(tierNodes, ref)
					}
				}
				if len(tierNodes) == 0 {
					continue
				}
				// Merge into existing tier or append new.
				found := false
				for i, mt := range merged.Tiers {
					if mt.Label == tier.Label {
						merged.Tiers[i].Nodes = append(merged.Tiers[i].Nodes, tierNodes...)
						merged.Tiers[i].TotalNodes += len(tierNodes)
						found = true
						break
					}
				}
				if !found {
					merged.Tiers = append(merged.Tiers, graph.ImpactTier{
						Label:      tier.Label,
						Depth:      tier.Depth,
						Confidence: tier.Confidence,
						Nodes:      tierNodes,
						TotalNodes: len(tierNodes),
					})
				}
				merged.AffectedFiles = append(merged.AffectedFiles, r.AffectedFiles...)
				merged.TotalAffected += len(tierNodes)
			}
		}
		// Deduplicate AffectedFiles.
		seenFiles := make(map[string]bool)
		unique := merged.AffectedFiles[:0]
		for _, f := range merged.AffectedFiles {
			if !seenFiles[f] {
				seenFiles[f] = true
				unique = append(unique, f)
			}
		}
		merged.AffectedFiles = unique
		// R2: Collect test coverage across all methods of the struct/interface.
		seenTestFiles := make(map[string]bool)
		for _, m2 := range methods {
			if m2.Type != graph.NodeMethod || m2.ID == root.ID {
				continue
			}
			for _, tf := range s.graph.FindTestsFor(m2.ID) {
				if !seenTestFiles[tf] {
					seenTestFiles[tf] = true
					merged.TestCoverage = append(merged.TestCoverage, tf)
				}
			}
		}
		merged.TestCoverage = s.trimRepoRoot(merged.TestCoverage)
		sort.Strings(merged.TestCoverage)
		applyImpactTokenBudget(merged, tokenBudget)
		// P7-10: emit search event for impact analysis.
		if pc := s.getPulseClient(); pc != nil {
			pc.RecordSearchEvent(pulse.SearchEvent{
				Mode: "impact", Query: symbol,
				ResultCount: merged.TotalAffected, ProjectID: s.projectID,
			})
		}
		return jsonResult(merged)
	}

	result, err := s.graph.ImpactAnalysis(root.ID, maxDepth)
	if err != nil {
		return toolError("impact analysis", err)
	}
	if result.Tiers == nil {
		result.Tiers = []graph.ImpactTier{}
	}

	// R2: Attach test coverage — files that exercise this entity via reverse CALLS BFS.
	// trimRepoRoot converts absolute paths to repo-relative paths for consistency
	// with all other file references in get_context/get_impact responses.
	result.TestCoverage = s.trimRepoRoot(s.graph.FindTestsFor(root.ID))
	applyImpactTokenBudget(result, tokenBudget)

	// P7-10: emit search event for impact analysis.
	if pc := s.getPulseClient(); pc != nil {
		pc.RecordSearchEvent(pulse.SearchEvent{
			Mode: "impact", Query: symbol,
			ResultCount: result.TotalAffected, ProjectID: s.projectID,
		})
	}

	// Cross-project impact: when projects= is specified, include cross-project
	// dependency status so the agent sees the full blast radius including siblings.
	// Uses an embedding wrapper to preserve the flat ImpactResult JSON shape
	// (all existing fields at top level) while adding cross_project_deps.
	projectsParam, _ := req.GetArguments()["projects"].(string)
	if projectsParam != "" && s.federationResolver != nil && s.store != nil {
		fedCtx, fedCancel := context.WithTimeout(ctx, 2*time.Second)
		crossDeps := s.federationResolver.GetDepsForEntity(fedCtx, string(root.ID), s.store)
		fedCancel()
		if len(crossDeps) > 0 {
			type impactWithFederation struct {
				*graph.ImpactResult
				CrossProjectDeps []federation.CrossProjectDepStatus `json:"cross_project_deps,omitempty"`
			}
			return jsonResult(impactWithFederation{
				ImpactResult:     result,
				CrossProjectDeps: crossDeps,
			})
		}
	}

	return jsonResult(result)
}

// applyImpactTokenBudget truncates an ImpactResult to fit within the token budget
// (1 token ≈ 4 bytes of JSON). Drops peripheral (depth 3+) tiers first, then
// indirect (depth 2), always keeping direct (depth 1) callers.
// Sets result.Truncated=true when any tier is dropped.
func applyImpactTokenBudget(result *graph.ImpactResult, tokenBudget int) {
	if tokenBudget <= 0 {
		return
	}
	raw, err := json.Marshal(result)
	if err != nil || len(raw) <= tokenBudget*4 {
		return // within budget
	}
	// Drop tiers from highest depth first until within budget.
	// Order: peripheral (depth≥3), then indirect (depth=2). Never drop depth=1.
	for pass := 0; pass < 2; pass++ {
		minDropDepth := 3
		if pass == 1 {
			minDropDepth = 2
		}
		filtered := result.Tiers[:0]
		dropped := false
		for _, tier := range result.Tiers {
			if tier.Depth >= minDropDepth {
				dropped = true
				continue
			}
			filtered = append(filtered, tier)
		}
		if dropped {
			result.Tiers = filtered
			result.Truncated = true
			raw, err = json.Marshal(result)
			if err != nil || len(raw) <= tokenBudget*4 {
				return
			}
		}
	}
}

// handleSemanticSearch runs a two-path search and merges results:
//  1. Vector cosine similarity — when an embed client is configured (brain /v1/embed
//     or explicit embedding_endpoint in synapses.json). This is the true semantic
//     path: concept queries like "how does auth work" find TokenValidator even if
//     those words never appear in the query.
//  2. FTS5 BM25 keyword ranking — always runs as fallback / supplement.
//
// Results are merged: vector hits first, then unique FTS5 hits appended up to
func (s *Server) adaptiveCarveConfig(cfg *graph.CarveConfig, entityName, agentID string) (forceFullDetail bool) {
	episodes, err := s.store.RecallEpisodes(
		entityName, s.graph.RepoID(), agentID, "pattern", "", 5, 30,
	)
	if err != nil || len(episodes) == 0 {
		return false
	}

	sevenDaysAgo := time.Now().AddDate(0, 0, -7).Unix()
	var recentUnhelpful, crossSessionRepeats int
	for _, ep := range episodes {
		if strings.Contains(ep.Tags, "context_quality") && ep.Outcome == "failure" && ep.CreatedAt >= sevenDaysAgo {
			recentUnhelpful++
		}
		if strings.Contains(ep.Tags, "repeated_context") {
			crossSessionRepeats++
		}
	}

	if recentUnhelpful > 0 {
		cfg.MaxDepth++
		forceFullDetail = true
	}
	if crossSessionRepeats >= 2 && cfg.MaxDepth < 3 {
		cfg.MaxDepth = 3
		forceFullDetail = true
	}
	return forceFullDetail
}

// trackContextCall increments the call count for (agentID, entity) within the
// current server session and returns the new count together with the duration
// since the previous call for the same key.
//
// sinceLast is zero on the first call (no prior delivery to measure against).
// Callers use sinceLast to classify refetch signals by the Sprint 15 #1
// quality discipline (< 5 min = moderate negative, 5–30 min = mild negative,
// ≥ 30 min = neutral/new-subtask — but the GC below removes entries after
// 30 min so a call after that window always returns count=1 with sinceLast=0).
//
// Entries older than 30m are pruned at most once every 5 minutes to avoid
// O(n) iteration on every write (R29 GAP3).
func (s *Server) trackContextCall(agentID, entity string) (count int, sinceLast time.Duration) {
	key := agentID + "\x00" + entity
	s.ctxCallMu.Lock()
	defer s.ctxCallMu.Unlock()
	if s.ctxCalls == nil {
		s.ctxCalls = make(map[string]*ctxCallEntry)
	}
	// Time-gated GC: only scan the map if 5+ minutes have passed since the last
	// sweep. This bounds GC cost to O(n) once per window rather than per call.
	now := time.Now()
	if now.Sub(s.ctxCallLastGC) > 5*time.Minute {
		s.ctxCallLastGC = now
		for k, e := range s.ctxCalls {
			if now.Sub(e.firstAt) > 30*time.Minute {
				delete(s.ctxCalls, k)
			}
		}
	}
	e, ok := s.ctxCalls[key]
	if !ok {
		s.ctxCalls[key] = &ctxCallEntry{count: 1, firstAt: now, lastAt: now}
		return 1, 0
	}
	sinceLast = now.Sub(e.lastAt)
	e.count++
	e.lastAt = now
	return e.count, sinceLast
}
