package mcp

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/metrics"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/store"
)

// handleGetProjectIdentity returns the compact architectural summary,
// enriched with federation status and workflow guidance.
func (s *Server) handleGetProjectIdentity(
	_ context.Context,
	_ mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	identity := s.graph.ProjectIdentity()

	// Enrich with federation status (absorbed from get_federation_status).
	primaryRepoID := s.graph.RepoID()
	repoNodeCounts := make(map[string]int)
	for _, n := range s.graph.AllNodes() {
		if idx := strings.Index(string(n.ID), "::"); idx >= 0 {
			repoNodeCounts[string(n.ID)[:idx]]++
		}
	}
	crossCallCount := 0
	var linkedRepos []string
	linkedSet := make(map[string]bool)
	for _, e := range s.graph.AllEdges() {
		if e.Type != graph.EdgeCalls {
			continue
		}
		fromIdx := strings.Index(string(e.From), "::")
		toIdx := strings.Index(string(e.To), "::")
		if fromIdx < 0 || toIdx < 0 {
			continue
		}
		fromRepo := string(e.From)[:fromIdx]
		toRepo := string(e.To)[:toIdx]
		if fromRepo != toRepo {
			crossCallCount++
			if fromRepo != primaryRepoID && !linkedSet[fromRepo] {
				linkedSet[fromRepo] = true
				linkedRepos = append(linkedRepos, fromRepo)
			}
			if toRepo != primaryRepoID && !linkedSet[toRepo] {
				linkedSet[toRepo] = true
				linkedRepos = append(linkedRepos, toRepo)
			}
		}
	}
	sort.Strings(linkedRepos)

	// Build the enriched result as a map so we can add fields.
	out := map[string]interface{}{
		"identity": identity,
		"federation": map[string]interface{}{
			"is_federated":        len(linkedRepos) > 0,
			"linked_repos":        linkedRepos,
			"cross_project_edges": crossCallCount,
		},
		"workflow_hints": []string{
			"1. session_init → single call to get pending tasks, project identity, and working state",
			"2. validate_plan → check proposed changes against architectural rules",
			"3. get_context → explore entity structure (callees, callers, annotations)",
			"4. annotate_node → leave findings for other agents",
			"5. update_task → mark work done as you go",
		},
	}
	// Autosubscribe: surface detected tech stack (populated by cmdStart after indexing).
	if s.techStack != nil {
		out["tech_stack"] = s.techStack
	}
	return jsonResult(out)
}

// directionalContext is the response shape for get_context.
// Nodes are split into callers/callees/related so the LLM can immediately
// understand call direction without inspecting raw edge types.
// Annotations are surfaced first so agents see peer knowledge before graph structure.
// contextEnrichment holds auto-injected rules, failures, and task context
// appended to get_context responses without requiring extra tool calls.
type contextEnrichment struct {
	ApplicableRules []ruleHint    `json:"applicable_rules,omitempty"` // architectural rules for this entity's file
	RecentFailures  []failureHint `json:"recent_failures,omitempty"`  // relevant failure episodes
	ActiveTask      *taskHint     `json:"active_task,omitempty"`      // linked task context
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
	Root               *graph.Node                   `json:"root"`
	Annotations        map[string][]store.Annotation `json:"annotations,omitempty"`          // node_id → []Annotation — surfaced first for multi-agent visibility
	Enrichment         *contextEnrichment            `json:"enrichment,omitempty"`            // auto-injected rules, failures, task context
	Callees            []graph.CarvedNode            `json:"callees"`                        // root --CALLS--> node
	Callers            []graph.CarvedNode            `json:"callers"`                        // node --CALLS--> root
	Related            []graph.CarvedNode            `json:"related"`                        // everything else
	ContextPacket      *brain.ContextPacket          `json:"context_packet,omitempty"`       // LLM-enriched packet (present when brain is available)
	SuggestedNextTools []toolSuggestion              `json:"suggested_next_tools,omitempty"` // context-aware next steps
	Truncated          bool                          `json:"truncated,omitempty"`            // true when token budget cut results
	TruncatedCount     int                           `json:"truncated_count,omitempty"`      // nodes dropped by budget
	BrainHint              string                        `json:"brain,omitempty"`                // set when brain is not configured
	Principles             []string                      `json:"principles,omitempty"`           // Hot Constitution principles from synapses.json
	ActivePrompts          []activePrompt                `json:"active_prompts,omitempty"`           // matched activation-context snippets from .synapses/prompts/
	ADRs                   []brain.ADR                   `json:"adrs,omitempty"`                 // relevant accepted ADRs for this entity's file
	StaleAnnotationWarning string                        `json:"stale_annotation_warning,omitempty"` // GAP-3: set when ≥1 annotation may be outdated
	RecentChanges          []metrics.CommitInfo          `json:"recent_changes,omitempty"`           // GAP-7: last 3 git commits that touched the entity's file
	GraphFreshness         string                        `json:"graph_freshness,omitempty"`          // GAP-4: warning when entity's file was recently modified
	AdaptiveHint           string                        `json:"adaptive_hint,omitempty"`            // F17: set when BFS depth/detail was auto-expanded based on prior feedback
	EntityMemories         []entityMemoryHint            `json:"entity_memories,omitempty"`          // R10: institutional knowledge attached to this entity
	QualityGaps            []store.QualityGap            `json:"quality_gaps,omitempty"`             // R32: open quality gaps on this entity
	EntityHash             string                        `json:"entity_hash,omitempty"`              // R14: SHA1 of node+neighbor IDs; stable cache key for clients
	CallerCountWarning     string                        `json:"caller_count_warning,omitempty"`     // DIAG-3: set when caller count is 0 for a method and use_go_types=false
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
	h := sha1.New()
	for _, s := range parts {
		_, _ = h.Write([]byte(s))
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
		return mcp.NewToolResultError("entity is required"), nil
	}

	// Session ID for server-side auto-caching. Empty for stdio/test paths — auto-cache
	// is silently disabled when no session ID is present (getSessionHash returns "").
	sessionID := SessionIDFromContext(ctx)

	// Extract format/detailLevel early so they can be included in the session cache key.
	// These same values are used again at rendering time — reading from the same request,
	// so values are identical. Declaring here avoids a second GetArguments call later.
	format, _ := req.GetArguments()["format"].(string)
	detailLevel, _ := req.GetArguments()["detail_level"].(string)

	// GAP-1: Feedback loop.
	// (a) Track repeat calls — ≥3 calls for the same entity by the same agent
	//     auto-records a pattern episode: initial context wasn't sufficient.
	// (b) Optional explicit feedback via helpful=true/false.
	agentIDForFeedback, _ := req.GetArguments()["agent_id"].(string)
	if agentIDForFeedback != "" && s.store != nil {
		repeatCount := s.trackContextCall(agentIDForFeedback, entityName)
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
		if repeatCount == 2 {
			// R29: correction signal — second fetch of same entity in session.
			if pc := s.getPulseClient(); pc != nil {
				go pc.RecordOutcomeSignal(pulse.OutcomeSignalEvent{
					ProjectID:  s.projectID,
					AgentID:    agentIDForFeedback,
					Entity:     pulseEntity,
					SignalType: "correction",
					Count:      repeatCount,
				})
			}
		}
		if repeatCount == 3 {
			// R29: escalation signal — three or more fetches.
			if pc := s.getPulseClient(); pc != nil {
				go pc.RecordOutcomeSignal(pulse.OutcomeSignalEvent{
					ProjectID:  s.projectID,
					AgentID:    agentIDForFeedback,
					Entity:     pulseEntity,
					SignalType: "escalation",
					Count:      repeatCount,
				})
			}
			go func() {
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
				if _, err := s.store.RememberEpisode(ep); err != nil {
					log.Printf("mcp: auto-record repeat context episode: %v", err)
				}
			}()
		}
	}
	if helpful, ok := req.GetArguments()["helpful"].(bool); ok && agentIDForFeedback != "" && s.store != nil {
		go func() {
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
			_, _ = s.store.RememberEpisode(ep)
		}()
	}

	cfg := s.config.CarveConfig()

	// F17: Adaptive Context Learning — auto-expand depth/detail based on
	// stored feedback for this entity+agent before per-call explicit overrides
	// are applied. Explicit caller values always win over adaptive adjustments.
	adaptiveForceFullDetail := false
	if agentIDForFeedback != "" && s.store != nil {
		adaptiveForceFullDetail = s.adaptiveCarveConfig(&cfg, entityName, agentIDForFeedback)
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

	entityCacheKey := fmt.Sprintf("%s|%s|%s|%s|%d|%d|inferred:%v",
		entityName, fileHint, format, detailLevel, cfg.MaxDepth, cfg.TokenBudget, includeInferred)

	// Resolve the entity name to a node ID.
	nodes := s.graph.FindByName(entityName)
	if len(nodes) == 0 {
		nodes = s.graph.FindByPattern(entityName)
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
			return jsonResult(map[string]interface{}{
				"error": fmt.Sprintf("entity not found: %q", entityName),
				"hint":  "No substring match. Try search(query=\"...\", mode=\"semantic\") for concept-based lookup.",
			})
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
	var disambiguationCandidates []map[string]interface{}
	if len(nodes) > 1 && fileHint == "" {
		for _, n := range nodes {
			disambiguationCandidates = append(disambiguationCandidates, map[string]interface{}{
				"name": n.Name,
				"type": n.Type,
				"file": s.graph.Root() + "/" + strings.TrimPrefix(n.File, s.graph.Root()+"/"),
				"line": n.Line,
				"pkg":  n.Package,
			})
		}
	}

	best := pickBestNode(nodes, s.graph)

	// B29: Auto-track watched symbol for dependency alert detection.
	// When a peer later modifies the same file, session_init will surface a Tier 2 alert.
	if agentIDForFeedback != "" && s.store != nil {
		relFile := strings.TrimPrefix(best.File, s.graph.Root()+"/")
		s.store.WatchSymbol(agentIDForFeedback, string(best.ID), best.Name, relFile)
		s.upsertAgentWithActivity(agentIDForFeedback, &store.AgentActivity{
			Focus:      best.Name,
			FocusFile:  relFile,
			FocusSince: time.Now().UTC().Format(time.RFC3339),
		})
	}

	subgraph, err := s.graph.CarveEgoGraph(best.ID, cfg)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
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
				go s.emitContextDelivery("get_context", cacheAgentID, entityName, best.File,
					cacheResp, sg.Nodes, sg.Edges, sg.TruncatedCount, sg.Truncated,
					false, true, time.Since(handlerStart).Milliseconds())
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
				go s.emitContextDelivery("get_context", cacheAgentID, entityName, best.File,
					cacheResp, sg.Nodes, sg.Edges, sg.TruncatedCount, sg.Truncated,
					false, true, time.Since(handlerStart).Milliseconds())
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
			return mcp.NewToolResultError(fmt.Sprintf("impact analysis: %v", err)), nil
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
		if cached := s.getPacketFromCache(cacheKey); cached != nil {
			dc.ContextPacket = cached.(*brain.ContextPacket)
		} else {
			// Async enrichment: return raw graph now, enrich in background.
			dc.BrainHint = "enrichment in progress — call get_context again in a few seconds for brain-enriched results"
			go s.asyncEnrichContext(bc, cacheKey, dc, best, taskID)
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

		if s.config != nil {
			for _, r := range s.config.Rules {
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
			if dynRules, err := s.store.LoadDynamicRules(); err == nil {
				for _, dr := range dynRules {
					matched := dr.ForbiddenEdge.FromFilePattern == ""
					if !matched {
						matched, _ = filepath.Match(dr.ForbiddenEdge.FromFilePattern, filepath.Base(best.File))
					}
					if matched {
						enrichment.ApplicableRules = append(enrichment.ApplicableRules, ruleHint{
							RuleID:      dr.ID,
							Description: dr.Description,
							Severity:    dr.Severity,
						})
					}
				}
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

		if len(enrichment.ApplicableRules) > 0 || len(enrichment.RecentFailures) > 0 || enrichment.ActiveTask != nil {
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
			if commits := metrics.RecentCommitsForFile(repoRoot, dc.Root.File, 3); len(commits) > 0 {
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
		if adrs, err := bc.GetADRs(context.Background(), dc.Root.File); err == nil && len(adrs) > 0 {
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
	go s.emitContextDelivery(
		"get_context", agentID, entityName, best.File,
		dc, sg.Nodes, sg.Edges,
		sg.TruncatedCount,
		sg.Truncated,
		dc.ContextPacket != nil,
		false,
		time.Since(handlerStart).Milliseconds(),
	)

	// Multi-agent awareness: fire-and-forget event so peers can see this via get_peer_activity.
	// Uses agentIDForFeedback (same value as agentID when provided). upsertAgentWithActivity
	// is NOT called here — it already ran synchronously above in the WatchSymbol block with
	// the full Focus+FocusFile+FocusSince payload, making a second call redundant.
	if agentIDForFeedback != "" && s.store != nil {
		relFileForEvent := strings.TrimPrefix(best.File, s.graph.Root()+"/")
		go func() {
			payload, _ := json.Marshal(map[string]string{
				"entity": entityName,
				"file":   relFileForEvent, // repo-relative, consistent with agent_watched_symbols
			})
			if err := s.store.AppendEvent("agent_examining", agentIDForFeedback, string(payload)); err != nil {
				log.Printf("mcp: append agent_examining event: %v", err)
			}
		}()
	}

	// format=compact returns a natural-language briefing instead of the default JSON blob.
	// detail_level controls depth: "summary" (~50t), "neighbors" (~200t), "full" (~400-600t, default).
	// format and detailLevel were extracted early for the session cache key; reused here.
	if format == "compact" {
		// F17: if no explicit detail_level given, honour the adaptive expansion.
		if detailLevel == "" && adaptiveForceFullDetail {
			detailLevel = "full"
		}
		s.setSessionHash(sessionID, entityCacheKey, entityHash)
		return mcp.NewToolResultText(serializeCompact(dc, detailLevel)), nil
	}

	// If multiple candidates existed, attach disambiguation list so agents
	// can re-call with file= if the selected entity is not what they wanted.
	if len(disambiguationCandidates) > 1 {
		type disambiguatedContext struct {
			*directionalContext
			OtherCandidates []map[string]interface{} `json:"other_candidates,omitempty"`
			DisambigHint    string                   `json:"disambig_hint,omitempty"`
		}
		s.setSessionHash(sessionID, entityCacheKey, entityHash)
		return jsonResult(&disambiguatedContext{
			directionalContext: dc,
			OtherCandidates:    disambiguationCandidates,
			DisambigHint:       fmt.Sprintf("%d entities named %q found. Showing best match. Re-call with file=\"path/suffix\" to pin to a specific file.", len(disambiguationCandidates), entityName),
		})
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

	var claims []brain.ClaimInput
	if s.store != nil {
		if allClaims, err := s.store.GetAllClaims(); err == nil {
			for _, c := range allClaims {
				claims = append(claims, brain.ClaimInput{
					AgentID:   c.AgentID,
					Scope:     c.Scope,
					ScopeType: c.ScopeType,
					ExpiresAt: c.ExpiresAt.Format(time.RFC3339),
				})
			}
		}
	}

	hasTests := fileHasTests(best.File)
	fanIn := s.graph.Fanin(best.ID)

	// Hard 200ms timeout: brain enrichment must not block the cache write path.
	// If brain is slow or unavailable, we silently skip caching — the next
	// get_context call will trigger another background attempt.
	enrichCtx, enrichCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer enrichCancel()

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
			ActiveClaims:    claims,
			TaskID:          taskID,
			HasTests:        hasTests,
			FanIn:           fanIn,
		},
		EnableLLM: s.config.Brain.ContextBuilder,
	})
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
	calleesOfRoot := make(map[graph.NodeID]bool)
	callersOfRoot := make(map[graph.NodeID]bool)
	for _, e := range sg.Edges {
		if e.Type != graph.EdgeCalls && e.Type != graph.EdgeHandles {
			continue
		}
		if e.From == sg.Root {
			calleesOfRoot[e.To] = true
		}
		if e.To == sg.Root {
			callersOfRoot[e.From] = true
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

	return dc
}

// handleFindEntity returns all nodes whose name matches the query string.
func (s *Server) handleFindEntity(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	query, ok := req.GetArguments()["query"].(string)
	if !ok || query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	// Exact match first, then substring.
	nodes := s.graph.FindByName(query)
	if len(nodes) == 0 {
		nodes = s.graph.FindByPattern(query)
	}
	// Dotted method name fallback: "Store.Close" → search "Close", filter by "Store".
	// Go method nodes are stored by their short name (e.g. "Close") without the
	// receiver type prefix, so "Store.Close" matches nothing via substring.
	if len(nodes) == 0 && strings.Contains(query, ".") {
		parts := strings.SplitN(query, ".", 2)
		prefix, method := strings.ToLower(parts[0]), parts[1]
		candidates := s.graph.FindByName(method)
		if len(candidates) == 0 {
			candidates = s.graph.FindByPattern(method)
		}
		for _, n := range candidates {
			if strings.Contains(strings.ToLower(string(n.ID)), prefix) ||
				strings.Contains(strings.ToLower(n.File), prefix) {
				nodes = append(nodes, n)
			}
		}
	}

	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	type entityMatch struct {
		ID        graph.NodeID   `json:"id"`
		Name      string         `json:"name"`
		Type      graph.NodeType `json:"type"`
		File      string         `json:"file"`
		Line      int            `json:"line"`
		Doc       string         `json:"doc,omitempty"`
		Signature string         `json:"signature,omitempty"`
	}
	results := make([]entityMatch, 0, len(nodes))
	for _, n := range nodes {
		file := n.File
		if prefix != "" {
			file = strings.TrimPrefix(file, prefix)
		}
		m := entityMatch{
			ID:   n.ID,
			Name: n.Name,
			Type: n.Type,
			File: file,
			Line: n.Line,
		}
		if n.Metadata != nil {
			m.Doc = n.Metadata["doc"]
			m.Signature = n.Metadata["signature"]
		}
		results = append(results, m)
	}

	// Sort results: implementation files before test files, then by path depth
	// (shorter = closer to root). This ensures the authoritative definition
	// appears first when both a_test.go and a.go define the same function name.
	sort.Slice(results, func(i, j int) bool {
		ti := isTestFile(results[i].File)
		tj := isTestFile(results[j].File)
		if ti != tj {
			return !ti // non-test wins
		}
		// Same test-ness: prefer shorter file path (closer to project root).
		if len(results[i].File) != len(results[j].File) {
			return len(results[i].File) < len(results[j].File)
		}
		return results[i].File < results[j].File
	})

	result := map[string]interface{}{
		"query":   query,
		"count":   len(results),
		"matches": results,
	}
	if len(results) == 0 {
		result["hint"] = "No exact or substring match. Try search(mode=semantic) for concept-based lookup, or check get_file_context for a specific file."
	}
	return jsonResult(result)
}

// ProposedChange is a single entry in a validate_plan request.
type ProposedChange struct {
	File          string `json:"file"`
	AddsCallTo    string `json:"adds_call_to,omitempty"`
	RemovesCallTo string `json:"removes_call_to,omitempty"`
}

// handleValidatePlan checks proposed changes against architectural rules.
// When check_safety=true is passed, it also runs check_plan_safety inline
// (500ms cap) so agents get both history-based warnings and structural
// violations in a single round-trip.
func (s *Server) handleValidatePlan(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	changesRaw, ok := req.GetArguments()["changes"].(string)
	if !ok || changesRaw == "" {
		return mcp.NewToolResultError("changes is required"), nil
	}

	var changes []ProposedChange
	if err := json.Unmarshal([]byte(changesRaw), &changes); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid changes JSON: %v", err)), nil
	}

	// Optional inline safety check — runs check_plan_safety before structural validation.
	var safetyCheck map[string]interface{}
	if checkSafety, _ := req.GetArguments()["check_safety"].(bool); checkSafety && s.store != nil {
		planDesc := stringArg(req, "plan_description")
		if planDesc == "" {
			var files []string
			for _, c := range changes {
				if c.File != "" {
					files = append(files, c.File)
				}
			}
			if len(files) > 0 {
				planDesc = strings.Join(files, ", ")
			}
		}
		if planDesc != "" {
			type safetyRes struct {
				ep  *store.Episode
				err error
			}
			ch := make(chan safetyRes, 1)
			go func() {
				ep, err := s.store.CheckPlanSafety(planDesc, "")
				ch <- safetyRes{ep, err}
			}()
			safetyCtx, safetyCancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer safetyCancel()
			select {
			case r := <-ch:
				if r.err == nil && r.ep != nil {
					safetyCheck = map[string]interface{}{
						"status": "warning",
						"match": map[string]interface{}{
							"episode_id": r.ep.ID,
							"decision":   r.ep.Decision,
							"outcome":    r.ep.Outcome,
							"rationale":  r.ep.Rationale,
						},
						"message": fmt.Sprintf("⚠ Past failure match: %q (outcome: %s). Review before proceeding.", r.ep.Decision, r.ep.Outcome),
					}
				} else {
					safetyCheck = map[string]interface{}{"status": "clear"}
				}
			case <-safetyCtx.Done():
				safetyCheck = map[string]interface{}{"status": "clear", "note": "safety check timed out (>500ms)"}
			}
		}
	}

	// GAP-4: Check freshness of files in the changes array.
	var freshWarnings []string
	repoRoot := s.graph.Root()
	for _, c := range changes {
		if c.File == "" {
			continue
		}
		absFile := c.File
		if repoRoot != "" && !filepath.IsAbs(absFile) {
			absFile = filepath.Join(repoRoot, absFile)
		}
		if fi, err := os.Stat(absFile); err == nil {
			if age := time.Since(fi.ModTime()); age < 10*time.Second {
				freshWarnings = append(freshWarnings, fmt.Sprintf("%s (modified %s ago)", c.File, age.Round(time.Second)))
			}
		}
	}

	// Build a temporary overlay graph that includes the proposed additions.
	overlay := cloneGraph(s.graph)
	var warnings []string
	var skipped []string
	for _, change := range changes {
		if change.AddsCallTo == "" {
			continue
		}
		// Find the callee node.
		callees := overlay.FindByName(change.AddsCallTo)
		if len(callees) == 0 {
			// Not alarming — the target may not exist yet (new symbol).
			// Skip this edge; it cannot violate rules that reference existing nodes.
			skipped = append(skipped, fmt.Sprintf("adds_call_to %q not yet in graph — edge skipped (no rules can fire for unknown targets)", change.AddsCallTo))
			continue
		}
		// Find nodes in the source file. Accepts both absolute and relative paths
		// (e.g. "synapses/internal/graph/graph.go" resolves against absolute paths
		// stored by the parser via the suffix-based FindByFile match).
		sources := overlay.FindByFile(change.File)
		if len(sources) == 0 {
			skipped = append(skipped, fmt.Sprintf("file %q: no nodes found in graph (check path is correct relative to repo root)", change.File))
			continue
		}
		// Add edges to all name-matched callees so CheckViolations can
		// detect rule violations regardless of which callee is the intended target.
		for _, callee := range callees {
			overlay.AddEdge(&graph.Edge{
				From: sources[0].ID,
				To:   callee.ID,
				Type: graph.EdgeCalls,
			})
		}
	}

	s.rulesMu.RLock()
	violations := s.config.CheckViolations(overlay)
	hasRules := len(s.config.Rules) > 0
	s.rulesMu.RUnlock()

	status := "ok"
	if len(violations) > 0 {
		status = "violations_found"
	}

	// GAP-8: Auto pattern extraction — when violations are found, record an
	// episode so check_plan_safety surfaces this warning for similar future plans.
	// This fills episodic memory without requiring agents to call remember() manually.
	if len(violations) > 0 && s.store != nil {
		go func() {
			agentIDForEp := stringArg(req, "agent_id")
			planDescForEp := stringArg(req, "plan_description")
			if planDescForEp == "" {
				var files []string
				for _, c := range changes {
					if c.File != "" {
						files = append(files, c.File)
					}
				}
				planDescForEp = strings.Join(files, ", ")
			}
			var sb strings.Builder
			for i, v := range violations {
				if i >= 3 {
					fmt.Fprintf(&sb, "... and %d more", len(violations)-3)
					break
				}
				fmt.Fprintf(&sb, "[%s] %s; ", v.RuleID, v.Description)
			}
			ep := store.Episode{
				AgentID:     agentIDForEp,
				EpisodeType: "failure",
				Outcome:     "failure",
				Trigger:     fmt.Sprintf("validate_plan: %d violation(s) for: %s", len(violations), planDescForEp),
				Decision:    fmt.Sprintf("Plan failed validation: %s", sb.String()),
				Rationale:   "Auto-recorded when validate_plan detected violations. check_plan_safety will surface this for similar future plans.",
				Tags:        `["auto","validate_plan","violation"]`,
				Importance:  0.6,
			}
			if _, err := s.store.RememberEpisode(ep); err != nil {
				log.Printf("mcp: auto-record validate_plan episode: %v", err)
			}
		}()
	}

	result := map[string]interface{}{
		"status":     status,
		"violations": violations,
	}
	if len(skipped) > 0 {
		result["skipped"] = skipped
	}
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}
	if !hasRules {
		result["hint"] = "No architectural rules configured. Add rules via upsert_rule or in synapses.json to enable validation."
	}
	if safetyCheck != nil {
		result["safety_check"] = safetyCheck
	}
	if len(freshWarnings) > 0 {
		result["graph_freshness"] = fmt.Sprintf("⚠ %d file(s) modified very recently — graph may not reflect latest changes. Consider re-indexing: %s", len(freshWarnings), strings.Join(freshWarnings, "; "))
	}
	return jsonResult(result)
}

// handleVerifyImplementation checks the actual graph state of written files
// against architectural rules and (optionally) a task's expectations.
// This is the write-side complement to validate_plan: validate_plan checks
// *before* writing, verify_implementation checks *after*.
func (s *Server) handleVerifyImplementation(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	filesRaw := stringArg(req, "files_written")
	if filesRaw == "" {
		return mcp.NewToolResultError("files_written is required"), nil
	}

	var files []string
	if err := json.Unmarshal([]byte(filesRaw), &files); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid files_written JSON: %v", err)), nil
	}
	if len(files) == 0 {
		return mcp.NewToolResultError("files_written must contain at least one file path"), nil
	}

	taskID := stringArg(req, "task_id")
	repoRoot := s.graph.Root()

	// callerRef is a minimal reference to a node that calls an exported symbol.
	type callerRef struct {
		Name string `json:"name"`
		File string `json:"file"`
		Line int    `json:"line"`
	}

	// signatureImpactEntry reports callers of one exported symbol whose signature changed.
	type signatureImpactEntry struct {
		Symbol    string      `json:"symbol"`
		Type      string      `json:"type"`
		Before    string      `json:"before,omitempty"`    // signature before the change
		Signature string      `json:"signature,omitempty"` // current signature
		Callers   []callerRef `json:"callers"`
	}

	// Per-file analysis.
	type fileReport struct {
		File             string                 `json:"file"`
		InGraph          bool                   `json:"in_graph"`
		NodeCount        int                    `json:"node_count"`
		Entities         []string               `json:"entities,omitempty"`
		Violations       []config.Violation     `json:"violations,omitempty"`
		SignatureImpact  []signatureImpactEntry `json:"signature_impact,omitempty"`
		FreshnessWarning string                 `json:"freshness_warning,omitempty"`
	}

	var reports []fileReport
	totalViolations := 0
	totalImpactWarnings := 0

	for _, f := range files {
		r := fileReport{File: f}

		nodes := s.graph.FindByFile(f)
		r.InGraph = len(nodes) > 0
		r.NodeCount = len(nodes)

		for _, n := range nodes {
			r.Entities = append(r.Entities, n.Name)
		}

		// Check architectural violations for this file.
		if r.InGraph {
			s.rulesMu.RLock()
			violations := s.config.CheckViolationsForFile(s.graph, f)
			s.rulesMu.RUnlock()
			r.Violations = violations
			totalViolations += len(violations)
		}

		// Signature impact: find exported entities in this file whose signature
		// actually changed since the last graph save. Only symbols with real changes
		// are reported — no noise for files where nothing changed.
		// Falls back to no-op when store is unavailable.
		const maxCallersPerSymbol = 30

		if s.store != nil {
			sigChanges, err := s.store.GetSignatureChanges(f)
			if err != nil {
				log.Printf("mcp: GetSignatureChanges(%s): %v", f, err)
			}
			for _, sc := range sigChanges {
				nid := graph.NodeID(sc.NodeID)
				impact, err := s.graph.ImpactAnalysis(nid, 1)
				if err != nil || impact == nil || impact.TotalAffected == 0 {
					continue
				}

				// Collect direct callers (depth-1 tier only).
				var callers []callerRef
				for _, tier := range impact.Tiers {
					if tier.Depth != 1 {
						continue
					}
					for i, ref := range tier.Nodes {
						if i >= maxCallersPerSymbol {
							break
						}
						callers = append(callers, callerRef{
							Name: ref.Name,
							File: ref.File,
							Line: ref.Line,
						})
					}
				}
				if len(callers) == 0 {
					continue
				}

				r.SignatureImpact = append(r.SignatureImpact, signatureImpactEntry{
					Symbol:    sc.Name,
					Type:      sc.NodeType,
					Signature: sc.NewSig,
					Before:    sc.OldSig,
					Callers:   callers,
				})
				totalImpactWarnings++
			}
		}

		// Freshness check.
		absFile := f
		if repoRoot != "" && !filepath.IsAbs(absFile) {
			absFile = filepath.Join(repoRoot, absFile)
		}
		if fi, err := os.Stat(absFile); err == nil {
			if age := time.Since(fi.ModTime()); age < 10*time.Second {
				r.FreshnessWarning = fmt.Sprintf("modified %s ago — graph may be stale", age.Round(time.Second))
			}
		}

		reports = append(reports, r)
	}

	// Task-level verification: compare actual graph entities against task's linked_nodes.
	var taskVerification map[string]interface{}
	if taskID != "" && s.store != nil {
		task, err := s.store.GetTask(taskID)
		if err == nil && task != nil && len(task.LinkedNodes) > 0 {
			var found, missing []string
			for _, nodeID := range task.LinkedNodes {
				if n := s.graph.GetNode(graph.NodeID(nodeID)); n != nil {
					found = append(found, n.Name)
				} else {
					missing = append(missing, nodeID)
				}
			}
			taskVerification = map[string]interface{}{
				"task_id":       taskID,
				"task_title":    task.Title,
				"linked_found":  found,
				"linked_missing": missing,
			}
		}
	}

	// Build result.
	status := "pass"
	if totalViolations > 0 {
		status = "violations_found"
	}
	// Check if any files are not yet in the graph.
	notIndexed := 0
	for _, r := range reports {
		if !r.InGraph {
			notIndexed++
		}
	}
	if notIndexed > 0 && status == "pass" {
		status = "pending_indexing"
	}

	result := map[string]interface{}{
		"status":           status,
		"total_violations": totalViolations,
		"impact_warnings":  totalImpactWarnings,
		"files":            reports,
	}
	if taskVerification != nil {
		result["task_verification"] = taskVerification
	}
	if notIndexed > 0 {
		result["indexing_hint"] = fmt.Sprintf("%d file(s) not yet in graph — wait for indexing or re-run verify_implementation.", notIndexed)
	}
	if totalImpactWarnings > 0 {
		result["impact_hint"] = fmt.Sprintf("%d exported symbol(s) have callers — review signature_impact in each file to ensure call sites are still valid.", totalImpactWarnings)
	}

	// Auto-record episode when post-implementation violations are found.
	if totalViolations > 0 && s.store != nil {
		go func() {
			var fileSummary []string
			for _, r := range reports {
				if len(r.Violations) > 0 {
					fileSummary = append(fileSummary, fmt.Sprintf("%s: %d violation(s)", r.File, len(r.Violations)))
				}
			}
			ep := store.Episode{
				EpisodeType: "failure",
				Outcome:     "failure",
				Trigger:     "verify_implementation found post-write violations",
				Decision:    fmt.Sprintf("Post-implementation violations in: %s", strings.Join(fileSummary, "; ")),
				Rationale:   "Code was written that violates architectural rules. Fix violations or update rules.",
				Tags:        `["auto","verify_implementation","violation"]`,
				Importance:  0.7,
			}
			if _, err := s.store.RememberEpisode(ep); err != nil {
				log.Printf("mcp: auto-record verify_implementation episode: %v", err)
			}
		}()
	}

	return jsonResult(result)
}

// handleGetViolations returns all current architectural rule violations.
// Optional rule_id filters to a specific rule. Optional include_log=true appends the historical log.
func (s *Server) handleGetViolations(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ruleIDFilter := stringArg(req, "rule_id")
	includeLog, _ := req.GetArguments()["include_log"].(bool)
	logLimit := 50
	if l, ok := req.GetArguments()["log_limit"].(float64); ok && l > 0 {
		logLimit = int(l)
	}

	s.rulesMu.RLock()
	violations := s.config.CheckViolations(s.graph)
	s.rulesMu.RUnlock()

	// Apply optional rule_id filter.
	if ruleIDFilter != "" {
		filtered := violations[:0]
		for _, v := range violations {
			if v.RuleID == ruleIDFilter {
				filtered = append(filtered, v)
			}
		}
		violations = filtered
	}

	// Persist to the audit log so agents can query violation history later.
	if s.store != nil && len(violations) > 0 {
		if err := s.store.LogViolations(violations); err != nil {
			log.Printf("mcp: log violations: %v", err)
		}
	}

	// Brain enrichment: add plain-English LLM explanations for each violation.
	if bc := s.getBrainClient(); bc != nil && len(violations) > 0 {
		for i := range violations {
			v := &violations[i]
			fromNode := s.graph.GetNode(v.FromNode)
			toNode := s.graph.GetNode(v.ToNode)
			sourceFile := ""
			targetName := string(v.ToNode)
			if fromNode != nil {
				sourceFile = fromNode.File
			}
			if toNode != nil {
				targetName = toNode.Name
			}
			explanation, fix := bc.ExplainViolation(context.Background(), brain.ViolationRequest{
				RuleID:       v.RuleID,
				RuleSeverity: v.Severity,
				Description:  v.Description,
				SourceFile:   sourceFile,
				TargetName:   targetName,
			})
			if explanation != "" {
				v.Explanation = explanation
			}
			if fix != "" && v.SuggestedFix == "" {
				v.SuggestedFix = fix
			}
		}
	}

	summary := "no violations found"
	if len(violations) > 0 {
		errorCount := 0
		for _, v := range violations {
			if v.Severity == "error" {
				errorCount++
			}
		}
		summary = fmt.Sprintf("%d violations (%d errors)", len(violations), errorCount)
	}

	result := map[string]interface{}{
		"summary":    summary,
		"violations": violations,
	}

	// Include historical log when requested.
	if includeLog && s.store != nil {
		if entries, err := s.store.GetViolationLog(ruleIDFilter, logLimit); err == nil {
			result["log"] = entries
		}
	}

	// R32: Append open quality gaps so agents see the full quality picture in
	// one call. Gaps are agent-discovered findings (reasoning-based) vs.
	// violations which are deterministic rule checks.
	// Always write both keys (even when store is nil) so callers can assert
	// m["quality_gap_count"] safely without a nil-key panic.
	if s.store != nil {
		if gaps, err := s.store.GetGaps(store.GapFilter{Status: "open"}); err == nil && len(gaps) > 0 {
			result["open_quality_gaps"] = gaps
			result["quality_gap_count"] = len(gaps)
		} else {
			result["open_quality_gaps"] = []interface{}{}
			result["quality_gap_count"] = 0
		}
	} else {
		result["open_quality_gaps"] = []interface{}{}
		result["quality_gap_count"] = 0
	}

	return jsonResult(result)
}

// handleUpsertRule creates or updates a dynamic architectural rule.
// The rule is persisted to SQLite first (so failure is safe — in-memory state
// stays consistent) and then atomically upserted into s.config.Rules.
func (s *Server) handleUpsertRule(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ruleID := stringArg(req, "rule_id")
	description := stringArg(req, "description")
	severity := stringArg(req, "severity")

	if ruleID == "" || description == "" {
		return mcp.NewToolResultError("rule_id and description are required"), nil
	}
	if severity != "error" && severity != "warning" {
		return mcp.NewToolResultError("severity must be 'error' or 'warning'"), nil
	}

	// R28: Semantic Firewall — reject rule creation when the triggering context
	// is not user-authored code. Two layers:
	// 1. Explicit: agent declared context_source="external"|"generated".
	// 2. Automatic: any entity name in rule_id or description resolves to a
	//    non-user-authored graph node (generated protobuf, vendored lib, etc.).
	if src := stringArg(req, "context_source"); src == "external" || src == "generated" {
		return mcp.NewToolResultError(
			fmt.Sprintf(
				"upsert_rule blocked: context_source=%q — architectural rules must be derived "+
					"from user-authored code, not %s content. "+
					"Re-evaluate the pattern against your own codebase before creating a rule.",
				src, src,
			),
		), nil
	}
	if detectedProv, detectedNode := s.detectRuleProvenance(ruleID, description); detectedProv != "" {
		return mcp.NewToolResultError(
			fmt.Sprintf(
				"upsert_rule blocked: the entity %q referenced in this rule is %s code, not user-authored. "+
					"Architectural rules must be grounded in your own codebase.",
				detectedNode, detectedProv,
			),
		), nil
	}

	fe := config.ForbiddenEdge{
		EdgeType:        graph.EdgeType(stringArg(req, "edge_type")),
		FromFilePattern: stringArg(req, "from_file_pattern"),
		ToFilePattern:   stringArg(req, "to_file_pattern"),
		ToNamePattern:   stringArg(req, "to_name_pattern"),
	}

	// Auto-detect rule type: if no ForbiddenEdge fields are set, this is a
	// behavioral/agent rule (conversation-level constraint), not a structural
	// code-graph rule. Agent rules are surfaced in session_init as
	// agent_constraints rather than being checked against the call graph.
	ruleType := "structural"
	if fe.EdgeType == "" && fe.FromFilePattern == "" && fe.ToFilePattern == "" && fe.ToNamePattern == "" {
		ruleType = "agent"
	}

	rule := config.Rule{
		ID:            ruleID,
		Description:   description,
		Severity:      severity,
		ForbiddenEdge: fe,
		RuleType:      ruleType,
	}

	// Persist first — if the DB write fails, don't mutate in-memory state.
	if s.store != nil {
		if err := s.store.UpsertDynamicRule(rule); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("persist rule: %v", err)), nil
		}
	}

	// Atomically upsert into the in-memory rule slice.
	s.rulesMu.Lock()
	upserted := false
	for i, r := range s.config.Rules {
		if r.ID == ruleID {
			s.config.Rules[i] = rule
			upserted = true
			break
		}
	}
	if !upserted {
		s.config.Rules = append(s.config.Rules, rule)
	}
	s.rulesMu.Unlock()

	// Retroactive scan: check existing graph edges against the new rule and
	// log any violations so get_violations() surfaces them immediately without
	// requiring a new validate_plan call.
	// Agent rules have no ForbiddenEdge, so skip the graph scan for them.
	if s.store != nil && ruleType == "structural" {
		go func(r config.Rule) {
			snapshot := config.Config{Rules: []config.Rule{r}}
			violations := snapshot.CheckViolations(s.graph)
			if len(violations) > 0 {
				if err := s.store.LogViolations(violations); err != nil {
					log.Printf("mcp: log violations (upsert_rule): %v", err)
				}
			}
		}(rule)
	}

	return jsonResult(map[string]interface{}{
		"status":    "ok",
		"rule_id":   ruleID,
		"rule_type": ruleType,
		"message":   fmt.Sprintf("Rule %q (%s) is now active.", ruleID, ruleType),
	})
}

// detectRuleProvenance auto-detects whether a rule references non-user-authored
// entities. It tokenises ruleID and description, looks up each token in the
// graph, and returns (provenance, entityName) for the first non-user-authored
// node found. Returns ("", "") when all referenced entities are user-authored
// or when no tokens match graph nodes.
// This powers the automatic layer of the R28 Semantic Firewall so agents
// don't need to declare context_source="generated" explicitly.
func (s *Server) detectRuleProvenance(ruleID, description string) (string, string) {
	if s.graph == nil {
		return "", ""
	}
	seen := make(map[string]bool)
	for _, word := range strings.FieldsFunc(ruleID+" "+description, func(r rune) bool {
		return !('a' <= r && r <= 'z') && !('A' <= r && r <= 'Z') && !('0' <= r && r <= '9') && r != '_'
	}) {
		if len(word) < 3 || seen[word] {
			continue
		}
		seen[word] = true
		for _, n := range s.graph.FindByName(word) {
			p := string(n.Provenance)
			if p == "generated" || p == "vendored" || p == "external" {
				return p, n.Name
			}
		}
	}
	return "", ""
}

// ── Tool Catalog for discover_tools ─────────────────────────────────────────

// toolCatalogEntry describes a single Synapses tool for discovery purposes.
type toolCatalogEntry struct {
	Name        string
	Category    string
	Description string
	Keywords    []string
	Example     string
}

// toolCatalog is the static catalog of all major Synapses tools, grouped by
// category and annotated with keywords for lightweight matching.
var toolCatalog = []toolCatalogEntry{
	// Session
	{Name: "session_init", Category: "session", Description: "Single-call session bootstrap", Keywords: []string{"start", "begin", "init", "session", "bootstrap", "startup"}, Example: `session_init(agent_id="my-agent")`},

	// Code exploration
	{Name: "explain_codebase", Category: "exploration", Description: "First-5-minutes orientation: entry points, key types, patterns, packages, tech stack", Keywords: []string{"explain", "codebase", "orientation", "overview", "introduce", "what is", "architecture", "summary", "new", "unfamiliar", "onboard"}, Example: `explain_codebase()`},
	{Name: "get_repo_map", Category: "exploration", Description: "Navigable package+entity map grouped by architectural layer", Keywords: []string{"repo", "map", "packages", "layout", "navigate", "overview", "structure", "where", "layers"}, Example: `get_repo_map(detail="compact")`},
	{Name: "get_context", Category: "exploration", Description: "Relevance-ranked subgraph around an entity", Keywords: []string{"context", "understand", "entity", "function", "struct", "interface", "subgraph", "explore", "code", "definition"}, Example: `get_context(entity="AuthService")`},
	{Name: "find_entity", Category: "exploration", Description: "Locate nodes by name or substring", Keywords: []string{"find", "search", "locate", "entity", "name", "symbol", "discover"}, Example: `find_entity(query="Auth")`},
	{Name: "get_file_context", Category: "exploration", Description: "All entities in a file", Keywords: []string{"file", "entities", "overview", "list", "defined"}, Example: `get_file_context(file="internal/store/tasks.go")`},
	{Name: "search", Category: "exploration", Description: "Keyword/fulltext search across entities", Keywords: []string{"search", "keyword", "concept", "fulltext", "semantic", "grep"}, Example: `search(query="rate limiting", mode="fulltext")`},
	{Name: "get_call_chain", Category: "exploration", Description: "Shortest call path between two entities", Keywords: []string{"call", "chain", "path", "trace", "reach", "how", "calls"}, Example: `get_call_chain(from="Handler", to="Repository")`},
	{Name: "get_impact", Category: "exploration", Description: "Blast-radius analysis of what breaks if entity changes", Keywords: []string{"impact", "blast", "radius", "breaks", "change", "depends", "dependents", "affected"}, Example: `get_impact(symbol="CarveEgoGraph")`},

	// Architecture
	{Name: "validate_plan", Category: "architecture", Description: "Check changes against architectural rules", Keywords: []string{"validate", "plan", "check", "rules", "architecture", "violations", "before"}, Example: `validate_plan(changes=[{"file":"auth.go","adds_call_to":"DB"}])`},
	{Name: "verify_implementation", Category: "architecture", Description: "Post-write check: verify written files against rules and task expectations", Keywords: []string{"verify", "implementation", "after", "written", "check", "post", "validate", "confirm"}, Example: `verify_implementation(files_written=["internal/auth/service.go"])`},
	{Name: "get_violations", Category: "architecture", Description: "List current architectural violations", Keywords: []string{"violations", "rules", "broken", "forbidden", "architecture"}, Example: `get_violations()`},
	{Name: "upsert_rule", Category: "architecture", Description: "Create or update an architectural constraint", Keywords: []string{"rule", "create", "constraint", "forbid", "enforce", "pattern"}, Example: `upsert_rule(rule_id="no-db-in-handler", description="...", severity="error")`},

	// Task management
	{Name: "create_plan", Category: "tasks", Description: "Save a plan with tasks for future sessions", Keywords: []string{"plan", "create", "tasks", "save", "work", "implement"}, Example: `create_plan(title="v1.1 improvements", tasks=[...])`},
	{Name: "get_pending_tasks", Category: "tasks", Description: "List pending/in-progress tasks (suggest_next=true for recommendation)", Keywords: []string{"pending", "tasks", "todo", "remaining", "work", "resume", "my", "assigned"}, Example: `get_pending_tasks(suggest_next=true)`},
	{Name: "update_task", Category: "tasks", Description: "Mark task done or add notes", Keywords: []string{"update", "task", "done", "complete", "status", "notes"}, Example: `update_task(id="...", status="done")`},
	{Name: "save_session_state", Category: "tasks", Description: "Save progress for session resumption", Keywords: []string{"save", "session", "state", "progress", "resume", "checkpoint"}, Example: `save_session_state(task_id="...", completed_steps=[...])`},
	{Name: "get_session_state", Category: "tasks", Description: "Resume from saved session state", Keywords: []string{"get", "session", "state", "resume", "restore"}, Example: `get_session_state(task_id="...")`},

	// Coordination
	{Name: "claim_work", Category: "coordination", Description: "Register work scope to prevent conflicts", Keywords: []string{"claim", "lock", "scope", "editing", "conflict", "reserve"}, Example: `claim_work(agent_id="...", scope="pkg/auth")`},
	{Name: "release_claims", Category: "coordination", Description: "Release work claims when done", Keywords: []string{"release", "unlock", "free", "claims", "done"}, Example: `release_claims(agent_id="...")`},
	{Name: "get_conflicts", Category: "coordination", Description: "Check for conflicting work claims", Keywords: []string{"conflicts", "overlap", "other", "agents", "clash"}, Example: `get_conflicts(agent_id="...")`},
	{Name: "get_agents", Category: "coordination", Description: "List all agents in this repository", Keywords: []string{"agents", "who", "list", "working", "active"}, Example: `get_agents()`},

	// Memory
	{Name: "remember", Category: "memory", Description: "Record a decision or failure as an episode", Keywords: []string{"remember", "record", "episode", "decision", "failure", "learn"}, Example: `remember(agent_id="...", decision="...", episode_type="failure")`},
	{Name: "recall", Category: "memory", Description: "Search or browse episodic memory (empty query=chronological, query=FTS5 search)", Keywords: []string{"recall", "remember", "past", "history", "episode", "memory", "similar", "browse", "episodes"}, Example: `recall(query="auth handler redirect loop")`},
	{Name: "check_plan_safety", Category: "memory", Description: "Check if similar plans failed before", Keywords: []string{"safety", "check", "failed", "before", "similar", "risk", "interjection"}, Example: `check_plan_safety(plan_description="modify auth login flow")`},

	// Messaging
	{Name: "send_message", Category: "messaging", Description: "Send message to another agent", Keywords: []string{"send", "message", "notify", "tell", "broadcast", "communicate"}, Example: `send_message(from_agent="...", topic="api_changed", payload="{...}")`},
	{Name: "get_messages", Category: "messaging", Description: "Retrieve messages + batch-ack via mark_read_ids", Keywords: []string{"messages", "inbox", "unread", "received", "poll", "mark", "read"}, Example: `get_messages(agent_id="...", mark_read_ids=["id1"])`},

	// Web / Doc Cache
	{Name: "web_annotate", Category: "web", Description: "Persist web findings to a code entity (survives across sessions)", Keywords: []string{"annotate", "web", "save", "persist", "findings", "research"}, Example: `web_annotate(node_id="...", note="...", hits=[...])`},
	{Name: "lookup_docs", Category: "web", Description: "Look up version-pinned package docs or cache a URL (cross-session)", Keywords: []string{"docs", "documentation", "api", "reference", "lookup", "package", "cache"}, Example: `lookup_docs(package="github.com/mark3labs/mcp-go")`},

	// Intent-based
	{Name: "prepare_context", Category: "meta", Description: "Intent-based context assembly (replaces multi-tool chains)", Keywords: []string{"prepare", "intent", "modify", "understand", "review", "debug", "add", "plan", "context"}, Example: `prepare_context(intent="modify", target="AuthService")`},
	{Name: "plan_context", Category: "meta", Description: "Compound pre-implementation check: safety + validation + scope in one call", Keywords: []string{"plan", "implement", "check", "safety", "validate", "before", "compound"}, Example: `plan_context(target="AuthService", changes=[{"file":"auth.go","adds_call_to":"DB"}])`},

	// Events
	{Name: "get_events", Category: "coordination", Description: "Recent events (file changes, task updates, annotations)", Keywords: []string{"events", "recent", "changes", "updates", "activity", "poll"}, Example: `get_events(since_seq=0)`},

	// ADRs
	{Name: "upsert_adr", Category: "architecture", Description: "Create/update an Architectural Decision Record", Keywords: []string{"adr", "architecture", "decision", "record", "create"}, Example: `upsert_adr(id="adr-001", title="No CGo", decision="...")`},
	{Name: "get_adrs", Category: "architecture", Description: "List Architectural Decision Records", Keywords: []string{"adr", "adrs", "architecture", "decisions", "records", "list"}, Example: `get_adrs()`},
}

// ── Workflow Recipes for discover_tools (GAP-5: Navigator short-term) ──────

// workflowStep describes one step in a multi-tool workflow recipe.
type workflowStep struct {
	Tool       string `json:"tool"`
	ArgsHint   string `json:"args"`        // template with {placeholders}
	Expects    string `json:"expects"`     // what this step returns
	UsesOutput string `json:"uses_output"` // what from the previous step feeds into this
}

// workflowRecipe is a pre-built tool sequence for a common intent.
// Returned by discover_tools when the query matches the intent keywords,
// so agents don't have to reason about tool chaining from scratch.
type workflowRecipe struct {
	ID       string         `json:"id"`
	Intent   string         `json:"intent"`
	Keywords []string       `json:"-"`
	Steps    []workflowStep `json:"steps"`
}

var workflowRecipes = []workflowRecipe{
	{
		ID:       "understand_entity",
		Intent:   "Understand how a function, struct, or module works",
		Keywords: []string{"understand", "explore", "what", "how", "entity", "function", "struct", "works", "does"},
		Steps: []workflowStep{
			{Tool: "find_entity", ArgsHint: `query="{name}"`, Expects: "List of matching nodes with name, type, file, line. Pick the best match.", UsesOutput: ""},
			{Tool: "get_context", ArgsHint: `entity="{name}", file="{file}"`, Expects: "Subgraph: root entity, callers, callees, annotations, recent_changes (git commits), brain enrichment.", UsesOutput: "Use exact name and file from find_entity results"},
			{Tool: "get_call_chain", ArgsHint: `from="{entity}", to="{callee}"`, Expects: "Step-by-step call path between two entities.", UsesOutput: "Optional: pick an interesting callee from get_context's callees list to trace deeper"},
		},
	},
	{
		ID:       "implement_change",
		Intent:   "Safely implement a code change across files",
		Keywords: []string{"implement", "modify", "change", "edit", "write", "code", "add", "feature", "refactor"},
		Steps: []workflowStep{
			{Tool: "get_context", ArgsHint: `entity="{target}"`, Expects: "Current structure, callers/callees, annotations, and applicable rules.", UsesOutput: ""},
			{Tool: "plan_context", ArgsHint: `target="{target}", changes=[{"file":"...","adds_call_to":"..."}]`, Expects: "verdict: clear|warnings|violations|blocked + safety check + scope assessment.", UsesOutput: "Use the entity's file and planned dependencies from get_context"},
			{Tool: "claim_work", ArgsHint: `agent_id="...", scope="{file}"`, Expects: "Confirmation of scope reservation.", UsesOutput: "Use files from your plan"},
			{Tool: "verify_implementation", ArgsHint: `files_written=["file1.go","file2.go"]`, Expects: "pass|violations_found|pending_indexing + per-file entity counts and violations.", UsesOutput: "After writing code: verify the implementation matches expectations"},
			{Tool: "update_task", ArgsHint: `id="...", status="done"`, Expects: "Task marked complete.", UsesOutput: "After verify passes: mark the task done and release_claims"},
		},
	},
	{
		ID:       "debug_issue",
		Intent:   "Debug a bug — find the source, trace the call path, assess blast radius",
		Keywords: []string{"debug", "bug", "fix", "broken", "error", "issue", "trace", "wrong", "fails"},
		Steps: []workflowStep{
			{Tool: "search", ArgsHint: `query="{symptom}", mode="semantic"`, Expects: "Matching entities ranked by relevance.", UsesOutput: ""},
			{Tool: "get_context", ArgsHint: `entity="{suspect}"`, Expects: "Subgraph around the suspected entity + recent_changes (who modified it last).", UsesOutput: "Pick the most relevant entity from search results"},
			{Tool: "get_call_chain", ArgsHint: `from="{entrypoint}", to="{suspect}"`, Expects: "How the entry point reaches the buggy code.", UsesOutput: "Use the caller that triggers the bug as 'from'"},
			{Tool: "get_impact", ArgsHint: `symbol="{suspect}"`, Expects: "Reverse-BFS: everything that depends on this entity.", UsesOutput: "Assess blast radius before fixing"},
		},
	},
	{
		ID:       "resume_work",
		Intent:   "Resume a previous session's work where you left off",
		Keywords: []string{"resume", "continue", "pick", "left", "session", "previous", "start", "pending"},
		Steps: []workflowStep{
			{Tool: "session_init", ArgsHint: `agent_id="..."`, Expects: "pending_tasks, project_identity, working_state, sidecars, scale_guidance.", UsesOutput: ""},
			{Tool: "get_pending_tasks", ArgsHint: `suggest_next=true`, Expects: "Tasks list + suggested_next (first unblocked task).", UsesOutput: "Use suggested_next to decide what to work on"},
			{Tool: "get_session_state", ArgsHint: `task_id="{suggested_task_id}"`, Expects: "Saved progress: completed_steps, current_step, context_snapshot.", UsesOutput: "Use task ID from suggested_next"},
			{Tool: "get_context", ArgsHint: `entity="{from_session_state}", task_id="{task_id}"`, Expects: "Fresh context with task-boosted relevance.", UsesOutput: "Use entity from session state's context_snapshot"},
		},
	},
	{
		ID:       "check_impact",
		Intent:   "Assess what breaks if an entity is changed or removed",
		Keywords: []string{"impact", "blast", "radius", "breaks", "change", "refactor", "remove", "depends", "dependents", "safe"},
		Steps: []workflowStep{
			{Tool: "get_impact", ArgsHint: `symbol="{entity}"`, Expects: "Reverse-BFS: all dependents and their distance from the entity.", UsesOutput: ""},
			{Tool: "validate_plan", ArgsHint: `changes=[{"file":"{entity_file}","adds_call_to":"..."}], check_safety=true`, Expects: "violations + safety_check from episodic memory.", UsesOutput: "Use affected files from get_impact to build your changes array"},
		},
	},
	{
		ID:       "review_architecture",
		Intent:   "Review and enforce architectural rules and constraints",
		Keywords: []string{"architecture", "rules", "violations", "enforce", "review", "constraints", "quality"},
		Steps: []workflowStep{
			{Tool: "get_violations", ArgsHint: ``, Expects: "List of all current rule violations with severity, suggested_fix.", UsesOutput: ""},
			{Tool: "get_context", ArgsHint: `entity="{violating_entity}"`, Expects: "Context around the violating entity to understand why the violation exists.", UsesOutput: "Use from_node or to_node from violations list"},
			{Tool: "upsert_rule", ArgsHint: `rule_id="...", description="...", severity="error"`, Expects: "Rule created/updated.", UsesOutput: "Only if you need to add new constraints"},
		},
	},
	{
		ID:       "search_concept",
		Intent:   "Find code related to a concept or feature area",
		Keywords: []string{"find", "search", "concept", "feature", "where", "which", "related", "handles", "about"},
		Steps: []workflowStep{
			{Tool: "search", ArgsHint: `query="{concept}", mode="semantic"`, Expects: "Entities ranked by relevance. search_mode shows if vector or FTS5 was used.", UsesOutput: ""},
			{Tool: "get_context", ArgsHint: `entity="{top_result}"`, Expects: "Full subgraph around the best match.", UsesOutput: "Use the top result's name and file"},
		},
	},
}

// handleDiscoverTools is a lightweight keyword matcher that helps agents find
// the right tool without scanning all tool definitions.
func (s *Server) handleDiscoverTools(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := strings.ToLower(stringArg(req, "query"))

	// Empty query: return categorized overview of all tools.
	if query == "" {
		categories := make(map[string][]map[string]string)
		for _, t := range toolCatalog {
			categories[t.Category] = append(categories[t.Category], map[string]string{
				"name":        t.Name,
				"description": t.Description,
			})
		}
		return jsonResult(map[string]interface{}{
			"hint":       "Pass a query to get targeted results, e.g. discover_tools(query=\"check what calls this function\")",
			"categories": categories,
		})
	}

	// Tokenize query.
	queryWords := strings.Fields(query)

	// Score each tool by keyword overlap.
	type scored struct {
		entry toolCatalogEntry
		score int
	}
	var results []scored
	for _, tool := range toolCatalog {
		score := 0
		for _, qw := range queryWords {
			for _, kw := range tool.Keywords {
				if strings.Contains(kw, qw) || strings.Contains(qw, kw) {
					score++
				}
			}
			// Also check tool name and description.
			if strings.Contains(tool.Name, qw) {
				score += 2
			}
			if strings.Contains(strings.ToLower(tool.Description), qw) {
				score++
			}
		}
		if score > 0 {
			results = append(results, scored{tool, score})
		}
	}

	// Sort by score descending.
	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })

	// Return top 3.
	limit := 3
	if len(results) < limit {
		limit = len(results)
	}
	results = results[:limit]

	// Format output.
	type toolMatch struct {
		Name        string `json:"name"`
		Category    string `json:"category"`
		Description string `json:"description"`
		Example     string `json:"example"`
		Score       int    `json:"score"`
	}
	matches := make([]toolMatch, len(results))
	for i, r := range results {
		matches[i] = toolMatch{
			Name:        r.entry.Name,
			Category:    r.entry.Category,
			Description: r.entry.Description,
			Example:     r.entry.Example,
			Score:       r.score,
		}
	}

	resp := map[string]interface{}{
		"query":   query,
		"matches": matches,
	}
	if len(matches) == 0 {
		resp["hint"] = "No matches. Try broader terms like 'explore', 'task', 'web', 'architecture'."
	}

	// GAP-5: Navigator short-term — match against workflow recipes and return
	// the best one as recommended_workflow so agents get a full tool sequence.
	var bestWorkflow *workflowRecipe
	bestWfScore := 0
	for i := range workflowRecipes {
		wf := &workflowRecipes[i]
		score := 0
		for _, qw := range queryWords {
			for _, kw := range wf.Keywords {
				if strings.Contains(kw, qw) || strings.Contains(qw, kw) {
					score++
				}
			}
			if strings.Contains(strings.ToLower(wf.Intent), qw) {
				score++
			}
		}
		if score > bestWfScore {
			bestWfScore = score
			bestWorkflow = wf
		}
	}
	if bestWorkflow != nil && bestWfScore > 0 {
		resp["recommended_workflow"] = bestWorkflow
	}

	// B28: promote any matched tools that are currently deferred (not yet
	// registered at startup due to repo scale) so the agent can call them
	// immediately after this response. Triggers notifications/tools/list_changed
	// to all connected clients.
	matchedNames := make([]string, len(matches))
	for i, m := range matches {
		matchedNames[i] = m.Name
	}
	if newlyRegistered := s.RegisterDeferredTools(matchedNames); len(newlyRegistered) > 0 {
		resp["newly_registered"] = newlyRegistered
		resp["registration_hint"] = fmt.Sprintf(
			"%d tool(s) newly registered: %v. They are now available in this session. "+
				"If using Claude Code: reconnect the MCP client to see them in the tool list "+
				"(known issue: github.com/anthropics/claude-code/issues/4118).",
			len(newlyRegistered), newlyRegistered)
	}

	return jsonResult(resp)
}

// stringArg extracts a string argument from a CallToolRequest by key.
// Returns "" if the key is absent or not a string.
func stringArg(req mcp.CallToolRequest, key string) string {
	v, _ := req.GetArguments()[key].(string)
	return v
}

// camelWords splits a CamelCase or mixedCase identifier into lowercase words.
//
//	"CarveEgoGraph" → ["carve", "ego", "graph"]
//	"BFSCarver"     → ["bfs", "carver"]   (consecutive capitals = one word)
//	"handleGetNode" → ["handle", "get", "node"]
func camelWords(name string) []string {
	runes := []rune(name)
	var words []string
	var cur strings.Builder
	for i, r := range runes {
		isUp := r >= 'A' && r <= 'Z'
		prevUp := i > 0 && runes[i-1] >= 'A' && runes[i-1] <= 'Z'
		nextLo := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
		// Start a new word at an uppercase letter when it's a standard camelCase
		// boundary (previous was lower, e.g. "Ego" in "CarveEgoGraph") OR when
		// it's the last capital in an acronym run (e.g. "C" in "BFSCarver").
		if isUp && cur.Len() > 0 && (!prevUp || nextLo) {
			words = append(words, strings.ToLower(cur.String()))
			cur.Reset()
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		words = append(words, strings.ToLower(cur.String()))
	}
	return words
}

// pickBestNode selects the most-relevant node from a candidate list using a
// tiered priority system that avoids test files and structural noise:
//
//	Tier 1: function or method in a non-test file  (exact semantic match)
//	Tier 2: struct or interface in a non-test file
//	Tier 3: function or method in a test file
//	Tier 4: any other type in a non-test file
//	Tier 5: everything else
//
// Within each tier the node with the highest connectivity (fanin+fanout) wins.
func pickBestNode(nodes []*graph.Node, g *graph.Graph) *graph.Node {
	tierOf := func(n *graph.Node) int {
		isTest := strings.HasSuffix(n.File, "_test.go")
		switch n.Type {
		case graph.NodeFunction, graph.NodeMethod:
			if !isTest {
				return 1
			}
			return 3
		case graph.NodeStruct, graph.NodeInterface:
			if !isTest {
				return 2
			}
			return 4
		default:
			if !isTest {
				return 4
			}
			return 5
		}
	}

	best := nodes[0]
	bestTier := tierOf(best)
	bestScore := g.Fanin(best.ID) + g.Fanout(best.ID)

	for _, n := range nodes[1:] {
		tier := tierOf(n)
		score := g.Fanin(n.ID) + g.Fanout(n.ID)
		if tier < bestTier || (tier == bestTier && score > bestScore) {
			best = n
			bestTier = tier
			bestScore = score
		}
	}
	return best
}

// normalizeSubgraph trims the repo root prefix from all File fields so that
// the LLM sees short relative paths instead of machine-specific absolute ones.
// This meaningfully reduces token consumption in large responses.
func normalizeSubgraph(sg *graph.SubGraph, repoRoot string) *graph.SubGraph {
	if repoRoot == "" {
		return sg
	}
	prefix := repoRoot
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	trimFile := func(path string) string {
		if strings.HasPrefix(path, prefix) {
			return strings.TrimPrefix(path, prefix)
		}
		return path
	}

	// Deep-copy nodes with trimmed File fields to avoid mutating the live graph.
	outNodes := make([]graph.CarvedNode, len(sg.Nodes))
	for i, cn := range sg.Nodes {
		nodeCopy := *cn.Node
		nodeCopy.File = trimFile(nodeCopy.File)
		outNodes[i] = graph.CarvedNode{
			Node:      &nodeCopy,
			Relevance: cn.Relevance,
			Hop:       cn.Hop,
		}
	}
	return &graph.SubGraph{
		Root:  sg.Root,
		Nodes: outNodes,
		Edges: sg.Edges,
	}
}

// handleGetFileContext returns all entities defined in a file, ordered by line.
// Accepts a partial path (e.g. "store/tasks.go" matches the full absolute path).
func (s *Server) handleGetFileContext(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	handlerStart := time.Now()
	filePath, ok := req.GetArguments()["file"].(string)
	if !ok || filePath == "" {
		return mcp.NewToolResultError("file is required"), nil
	}

	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	var matches []*graph.Node
	for _, n := range s.graph.AllNodes() {
		if n.Type == graph.NodeFile || n.Type == graph.NodePackage {
			continue
		}
		// Match against absolute path or repo-relative path.
		rel := strings.TrimPrefix(n.File, prefix)
		if strings.HasSuffix(n.File, filePath) || strings.HasSuffix(rel, filePath) {
			matches = append(matches, n)
		}
	}

	if len(matches) == 0 {
		return jsonResult(map[string]interface{}{
			"error": fmt.Sprintf("no entities found for file: %q", filePath),
			"hint":  "Use a path suffix (e.g. 'store/tasks.go' not full absolute path). The file may not be indexed yet — try reindex.",
		})
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].File != matches[j].File {
			return matches[i].File < matches[j].File
		}
		return matches[i].Line < matches[j].Line
	})

	type fileEntity struct {
		Type     graph.NodeType    `json:"type"`
		Name     string            `json:"name"`
		Line     int               `json:"line"`
		Exported bool              `json:"exported"`
		Metadata map[string]string `json:"metadata,omitempty"`
	}

	// Check how many distinct files were matched.
	fileSet := make(map[string]struct{})
	for _, n := range matches {
		fileSet[n.File] = struct{}{}
	}

	agentIDFC, _ := req.GetArguments()["agent_id"].(string)
	if agentIDFC == "" {
		agentIDFC = s.getLastAgent()
	}
	// R29: track repeated file-context fetches as a confusion signal, the same
	// way get_context tracks repeated entity fetches.
	if agentIDFC != "" && s.store != nil {
		s.trackContextCall(agentIDFC, "file:"+filePath)
	}

	if len(fileSet) == 1 {
		// Single file — keep existing flat format.
		out := make([]fileEntity, len(matches))
		for i, n := range matches {
			out[i] = fileEntity{Type: n.Type, Name: n.Name, Line: n.Line, Exported: n.Exported, Metadata: n.Metadata}
		}
		payload := map[string]interface{}{
			"file":     strings.TrimPrefix(matches[0].File, prefix),
			"package":  matches[0].Package,
			"count":    len(out),
			"entities": out,
		}
		s.emitFileContextDelivery(agentIDFC, filePath, matches, payload, time.Since(handlerStart).Milliseconds())
		return jsonResult(payload)
	}

	// Multiple files matched — group by file with attribution.
	byFile := make(map[string][]fileEntity)
	fileOrder := make([]string, 0, len(fileSet))
	for _, n := range matches {
		rel := strings.TrimPrefix(n.File, prefix)
		if _, seen := byFile[rel]; !seen {
			fileOrder = append(fileOrder, rel)
		}
		byFile[rel] = append(byFile[rel], fileEntity{Type: n.Type, Name: n.Name, Line: n.Line, Exported: n.Exported, Metadata: n.Metadata})
	}
	sort.Strings(fileOrder)
	multiPayload := map[string]interface{}{
		"files_matched":    len(fileSet),
		"total_count":      len(matches),
		"entities_by_file": byFile,
		"hint":             fmt.Sprintf("%d files named %q found. Use file= param with a longer path suffix to pin to one file.", len(fileSet), filePath),
	}
	s.emitFileContextDelivery(agentIDFC, filePath, matches, multiPayload, time.Since(handlerStart).Milliseconds())
	return jsonResult(multiPayload)
}

// handleSearch performs a keyword search across entity names and doc comments.
// Results are ranked: exact name > name prefix > name substring > doc match.
func (s *Server) handleSearch(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	query, ok := req.GetArguments()["query"].(string)
	if !ok || query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	// R29: track repeated searches for the same query as a confusion signal.
	if agentIDSrch, _ := req.GetArguments()["agent_id"].(string); agentIDSrch != "" && s.store != nil {
		s.trackContextCall(agentIDSrch, "search:"+query)
	}

	// mode=fulltext (or legacy alias "semantic") delegates to FTS5 BM25 search.
	if mode := stringArg(req, "mode"); mode == "semantic" || mode == "fulltext" {
		return s.handleSemanticSearch(ctx, req)
	}

	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	lower := strings.ToLower(query)

	type hit struct {
		node  *graph.Node
		score int
	}
	var hits []hit

	for _, n := range s.graph.AllNodes() {
		if n.Type == graph.NodeFile || n.Type == graph.NodePackage {
			continue
		}
		nameLow := strings.ToLower(n.Name)
		fileLow := strings.ToLower(n.File)
		score := 0
		switch {
		case nameLow == lower:
			score = 30
		case strings.HasPrefix(nameLow, lower):
			score = 20
		case strings.Contains(nameLow, lower):
			score = 10
		default:
			// Score 8: file path suffix match — lets agents search by package name
			// (e.g. "watcher" matches all nodes in internal/watcher/*.go).
			if strings.HasSuffix(fileLow, "/"+lower+".go") ||
				strings.Contains(fileLow, "/"+lower+"/") {
				score = 8
			} else if doc, ok := n.Metadata["doc"]; ok && strings.Contains(strings.ToLower(doc), lower) {
				score = 5
			}
		}
		// Multi-word AND query: each query word must appear in the name components
		// or doc comment. Handles stemmed/derived forms like "BFS carver" matching
		// "CarveEgoGraph" (query "carver" prefix-matches name component "carve").
		if score == 0 {
			words := strings.Fields(lower)
			if len(words) > 1 {
				nameWords := camelWords(n.Name)
				docLow := strings.ToLower(n.Metadata["doc"])
				matchCount := 0
				inNameCount := 0
				for _, qw := range words {
					// Check name components: exact match, or qw starts with component
					// (handles "carver"→"carve"), or component starts with qw.
					inName := false
					for _, nw := range nameWords {
						if nw == qw || strings.HasPrefix(qw, nw) || strings.HasPrefix(nw, qw) {
							inName = true
							break
						}
					}
					if inName {
						matchCount++
						inNameCount++
					} else if strings.Contains(docLow, qw) {
						matchCount++
					}
				}
				if matchCount == len(words) {
					if inNameCount > 0 {
						score = 6 // partial name + doc match
					} else {
						score = 3 // all words only in doc
					}
				}
			}
		}

		if score > 0 {
			hits = append(hits, hit{n, score})
		}
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].node.Name < hits[j].node.Name
	})

	// Cap at 25 results to stay within token budget.
	if len(hits) > 25 {
		hits = hits[:25]
	}

	type result struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		File      string `json:"file"`
		Line      int    `json:"line"`
		Doc       string `json:"doc,omitempty"`
		Signature string `json:"signature,omitempty"`
	}
	results := make([]result, len(hits))
	for i, h := range hits {
		results[i] = result{
			Type:      string(h.node.Type),
			Name:      h.node.Name,
			File:      strings.TrimPrefix(h.node.File, prefix),
			Line:      h.node.Line,
			Doc:       h.node.Metadata["doc"],
			Signature: h.node.Metadata["signature"],
		}
	}

	return jsonResult(map[string]interface{}{
		"query":   query,
		"count":   len(results),
		"results": results,
	})
}

// handleGetCallChain finds the shortest call path between two entities using
// BFS over CALLS edges. Answers "how does A reach B?"
func (s *Server) handleGetCallChain(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	fromName, ok1 := req.GetArguments()["from"].(string)
	toName, ok2 := req.GetArguments()["to"].(string)
	if !ok1 || !ok2 || fromName == "" || toName == "" {
		return mcp.NewToolResultError("from and to are required"), nil
	}

	resolve := func(name string) *graph.Node {
		nodes := s.graph.FindByName(name)
		if len(nodes) == 0 {
			nodes = s.graph.FindByPattern(name)
		}
		if len(nodes) == 0 {
			return nil
		}
		return pickBestNode(nodes, s.graph)
	}

	fromNode := resolve(fromName)
	if fromNode == nil {
		return jsonResult(map[string]interface{}{
			"error": fmt.Sprintf("entity not found: %q", fromName),
			"hint":  "Use find_entity or semantic_search to discover the correct entity name.",
		})
	}
	toNode := resolve(toName)
	if toNode == nil {
		return jsonResult(map[string]interface{}{
			"error": fmt.Sprintf("entity not found: %q", toName),
			"hint":  "Use find_entity or semantic_search to discover the correct entity name.",
		})
	}

	if fromNode.ID == toNode.ID {
		return jsonResult(map[string]interface{}{
			"found": true,
			"chain": []string{fromNode.Name},
		})
	}

	// BFS following CALLS + HANDLES edges (forward) and IMPLEMENTS edges (both
	// directions). HANDLES edges allow traversal through framework routing
	// registrations (R1): setupFn --CALLS--> routeNode --HANDLES--> handlerFn.
	// IMPLEMENTS edges are traversed bidirectionally so the search can cross
	// interface boundaries: struct → interface (forward) and interface → struct
	// (backward), enabling chains like: Caller → ConcreteType → Interface or
	// Caller → Interface → ConcreteImplementation.
	prev := map[graph.NodeID]graph.NodeID{fromNode.ID: ""}
	// viaImpl tracks which steps in the chain crossed an IMPLEMENTS boundary.
	viaImpl := make(map[graph.NodeID]bool)
	// viaHandles tracks which steps crossed a synthetic HANDLES boundary (R1).
	viaHandles := make(map[graph.NodeID]bool)
	// Single queue carries both the node ID and hop distance — no parallel slice needed.
	type bfsEntry struct {
		id  graph.NodeID
		hop int
	}
	queue := []bfsEntry{{fromNode.ID, 0}}
	found := false
	// closestReachable tracks the deepest node reached (by hop count from root).
	// Used in the not-found response to show agents where the static graph ends.
	var closestReachableID graph.NodeID
	maxHop := 0

	for len(queue) > 0 && !found {
		curr := queue[0]
		queue = queue[1:]

		if curr.hop > maxHop {
			maxHop = curr.hop
			closestReachableID = curr.id
		}

		// Forward edges: CALLS, HANDLES, and IMPLEMENTS (concrete → interface).
		for _, e := range s.graph.OutEdges(curr.id) {
			if e.Type != graph.EdgeCalls && e.Type != graph.EdgeImplements && e.Type != graph.EdgeHandles {
				continue
			}
			if _, visited := prev[e.To]; visited {
				continue
			}
			prev[e.To] = curr.id
			if e.Type == graph.EdgeImplements {
				viaImpl[e.To] = true
			}
			if e.Type == graph.EdgeHandles {
				viaHandles[e.To] = true
			}
			if e.To == toNode.ID {
				found = true
				break
			}
			queue = append(queue, bfsEntry{e.To, curr.hop + 1})
		}
		if found {
			break
		}

		// Backward IMPLEMENTS edges (interface → concrete struct).
		for _, e := range s.graph.InEdges(curr.id) {
			if e.Type != graph.EdgeImplements {
				continue
			}
			if _, visited := prev[e.From]; visited {
				continue
			}
			prev[e.From] = curr.id
			viaImpl[e.From] = true
			if e.From == toNode.ID {
				found = true
				break
			}
			queue = append(queue, bfsEntry{e.From, curr.hop + 1})
		}
	}

	if !found {
		// Build a helpful explanation for why no path was found.
		fromPkg := topLevelPackage(fromNode.File)
		toPkg := topLevelPackage(toNode.File)
		var reason, hint string
		if fromPkg != toPkg && fromPkg != "" && toPkg != "" {
			// Different top-level packages — likely a cross-binary boundary.
			reason = fmt.Sprintf(
				"No direct CALLS path found. %q (%s) and %q (%s) are in different packages (%s vs %s). "+
					"Cross-binary calls (e.g. HTTP, gRPC, queue) are not captured as CALLS edges.",
				fromName, fromNode.File, toName, toNode.File, fromPkg, toPkg,
			)
			hint = "If these communicate via HTTP or another protocol, use get_context on each entity to understand their APIs, then trace the integration manually."
		} else {
			reason = fmt.Sprintf(
				"No direct CALLS path found between %q and %q. "+
					"They may be unrelated, or connected only at runtime (e.g. via interface dispatch, reflection, or dynamic config).",
				fromName, toName,
			)
			hint = "Use get_context on each entity to see their callers/callees, or get_impact to find what depends on them."
		}
		notFound := map[string]interface{}{
			"found":  false,
			"from":   map[string]interface{}{"name": fromName, "file": fromNode.File, "type": string(fromNode.Type)},
			"to":     map[string]interface{}{"name": toName, "file": toNode.File, "type": string(toNode.Type)},
			"reason": reason,
			"hint":   hint,
		}
		// R2: surface the deepest reachable node so agents know where the static
		// graph ends — especially useful for dynamic-dispatch gaps.
		if closestReachableID != "" && closestReachableID != fromNode.ID {
			if n := s.graph.GetNode(closestReachableID); n != nil {
				notFound["closest_reachable"] = map[string]interface{}{
					"name": n.Name,
					"file": strings.TrimPrefix(n.File, s.graph.Root()+"/"),
					"type": string(n.Type),
					"hops": maxHop,
				}
			}
		}
		return jsonResult(notFound)
	}

	// Reconstruct path.
	var chainIDs []graph.NodeID
	curr := toNode.ID
	for curr != "" {
		chainIDs = append([]graph.NodeID{curr}, chainIDs...)
		curr = prev[curr]
	}

	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	type chainStep struct {
		Name string `json:"name"`
		Type string `json:"type"`
		File string `json:"file"`
		Line int    `json:"line"`
		Via  string `json:"via,omitempty"` // "implements" | "handles" when crossing a dispatch boundary
	}
	usedInterface := false
	usedHandles := false
	chain := make([]chainStep, 0, len(chainIDs))
	for _, id := range chainIDs {
		n := s.graph.GetNode(id)
		if n == nil {
			continue
		}
		step := chainStep{
			Name: n.Name,
			Type: string(n.Type),
			File: strings.TrimPrefix(n.File, prefix),
			Line: n.Line,
		}
		if viaImpl[id] {
			step.Via = "implements"
			usedInterface = true
		}
		if viaHandles[id] {
			step.Via = "handles" // R1: inferred framework routing edge
			usedHandles = true
		}
		chain = append(chain, step)
	}

	return jsonResult(map[string]interface{}{
		"found":         true,
		"hops":          len(chain) - 1,
		"via_interface": usedInterface,
		"via_handles":   usedHandles, // R1: true when path crossed a synthetic routing edge
		"chain":         chain,
	})
}

// isTestFile returns true for test files (_test.go, test_*.py, *_test.ts, etc.)
// so find_entity can rank implementation files above test files.
func isTestFile(filePath string) bool {
	base := filePath
	if i := strings.LastIndex(filePath, "/"); i >= 0 {
		base = filePath[i+1:]
	}
	return strings.HasSuffix(base, "_test.go") ||
		strings.HasPrefix(base, "test_") ||
		strings.HasSuffix(base, "_test.ts") ||
		strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, "_test.py") ||
		strings.HasSuffix(base, ".spec.ts") ||
		strings.HasSuffix(base, "_test.js") ||
		strings.HasSuffix(base, ".test.js")
}

// topLevelPackage returns the first path component of filePath (the top-level
// directory name), which typically corresponds to the binary or package root.
// Returns "" if filePath has no directory component.
func topLevelPackage(filePath string) string {
	if filePath == "" {
		return ""
	}
	// Normalize to forward slashes so this works on Windows (where graph paths
	// may use backslashes) and Unix alike.
	clean := filepath.ToSlash(filePath)
	clean = strings.TrimLeft(clean, "/")
	parts := strings.SplitN(clean, "/", 2)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

// changeEntry is one record in the working-state change log.
type changeEntry struct {
	File         string `json:"file"`
	At           string `json:"at"`
	NodesAdded   int    `json:"nodes_added"`
	NodesRemoved int    `json:"nodes_removed"`
	EdgesAdded   int    `json:"edges_added"`
}

// handleGetWorkingState returns recent file changes from the watcher's change log,
// plus an optional git diff stat for the working tree.
// Answers "what was the developer just working on before calling this agent?"
func (s *Server) handleGetWorkingState(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	windowMinutes := 15
	if w, ok := req.GetArguments()["window_minutes"].(float64); ok && w > 0 {
		windowMinutes = int(w)
	}

	var events []changeEntry
	if s.changeSource != nil {
		for _, e := range s.changeSource.RecentChanges(windowMinutes) {
			events = append(events, changeEntry{
				File:         e.File,
				At:           e.Timestamp.Format("15:04:05"),
				NodesAdded:   e.NodesAdded,
				NodesRemoved: e.NodesRemoved,
				EdgesAdded:   e.EdgesAdded,
			})
		}
	}
	if events == nil {
		events = []changeEntry{}
	}

	// Also surface recently-modified config files (synapses.json, Makefile, etc.)
	// that the watcher ignores since they aren't source code.
	root := s.graph.Root()
	if root != "" {
		cutoff := time.Now().Add(-time.Duration(windowMinutes) * time.Minute)
		configPatterns := []string{"synapses.json", "Makefile", "Dockerfile"}
		if entries, err := os.ReadDir(root); err == nil {
			for _, de := range entries {
				if de.IsDir() {
					continue
				}
				name := de.Name()
				ext := filepath.Ext(name)
				isConfig := ext == ".json" || ext == ".yaml" || ext == ".yml" || ext == ".toml"
				if !isConfig {
					for _, p := range configPatterns {
						if name == p {
							isConfig = true
							break
						}
					}
				}
				if !isConfig {
					continue
				}
				if info, err := de.Info(); err == nil && info.ModTime().After(cutoff) {
					events = append(events, changeEntry{
						File: filepath.Join(root, name),
						At:   info.ModTime().Format("15:04:05"),
					})
				}
			}
		}
	}

	result := map[string]interface{}{
		"window_minutes": windowMinutes,
		"recent_changes": events,
	}

	// Best-effort git diff stat — omitted when git is unavailable or not a git repo.
	if root != "" {
		if out, err := exec.Command("git", "-C", root, "diff", "--stat", "HEAD").Output(); err == nil {
			if stat := strings.TrimSpace(string(out)); stat != "" {
				result["git_diff_stat"] = stat
			}
		}
	}

	// When no recent file watcher changes, fall back to recent git log so agents
	// always get meaningful orientation rather than an empty response.
	if len(events) == 0 && root != "" {
		if out, err := exec.Command("git", "-C", root, "log", "--oneline", "-7").Output(); err == nil {
			if log := strings.TrimSpace(string(out)); log != "" {
				result["fallback_git_log"] = log
				result["fallback_note"] = fmt.Sprintf("No file changes in the last %d minutes. Showing recent git commits for context.", windowMinutes)
			}
		}
	}

	// Context-aware suggestions based on what was recently changed.
	result["suggested_tools"] = suggestToolsForChanges(events)

	return jsonResult(result)
}

// suggestToolsForChanges returns ordered tool suggestions based on the recently changed files.
// Helps agents decide what to investigate or verify after seeing working state.
func suggestToolsForChanges(events []changeEntry) []toolSuggestion {
	if len(events) == 0 {
		return []toolSuggestion{
			{Tool: "get_project_identity", Reason: "orient yourself before starting work"},
			{Tool: "get_pending_tasks", Reason: "find your next task"},
		}
	}
	suggestions := []toolSuggestion{
		{Tool: "get_violations", Reason: "check if recent edits introduced architectural violations"},
		{Tool: "update_task", Reason: "mark in-progress tasks complete if work is finished"},
	}
	// Suggest exploring changed files.
	seen := make(map[string]bool)
	for _, e := range events {
		if !seen[e.File] && e.File != "" {
			seen[e.File] = true
			suggestions = append(suggestions, toolSuggestion{
				Tool:   "get_file_context",
				Reason: "see all entities in recently changed file: " + e.File,
			})
		}
		if len(seen) >= 2 {
			break // limit to 2 file suggestions
		}
	}
	return suggestions
}

// hashIdentity produces a SHA-1 hex digest of the serialised ProjectIdentity.
// Used to detect whether the project structure has changed since the last
// session_init call, allowing incremental responses that skip unchanged data.
func hashIdentity(identity *graph.ProjectIdentity) string {
	b, err := json.Marshal(identity)
	if err != nil {
		return ""
	}
	h := sha1.Sum(b)
	return fmt.Sprintf("%x", h)
}

// handleSessionInit is the single-call session bootstrap that replaces the
// three-step startup ritual (get_pending_tasks → get_project_identity →
// get_working_state). One MCP round-trip returns all the context an agent
// needs to start work, including scale-aware tool guidance and recent events.
//
// Incremental mode: when agent_id is provided and the agent has called
// session_init before, unchanged sections are omitted to save tokens.
// The agent's context profile is updated after each call.
func (s *Server) handleSessionInit(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	agentID, _ := req.GetArguments()["agent_id"].(string)
	model, _    := req.GetArguments()["model"].(string)
	provider, _ := req.GetArguments()["provider"].(string)
	intent, _   := req.GetArguments()["intent"].(string)
	s.upsertAgentIfNeeded(agentID)
	// B29: Store declared intent so peers can see what this agent is working on.
	if agentID != "" && intent != "" && s.store != nil {
		s.upsertAgentWithActivity(agentID, &store.AgentActivity{Intent: intent})
	}
	// Remember this agent so subsequent tool calls that omit agent_id
	// can still be attributed correctly in Pulse analytics.
	s.setLastAgent(agentID)
	// R29: Reset the correction/escalation counters for this agent when a new
	// session starts. Without this, a reconnecting agent (same agentID, new
	// session) inherits stale repeat-counts and fires spurious signals.
	if agentID != "" {
		s.ctxCallMu.Lock()
		for k := range s.ctxCalls {
			if strings.HasPrefix(k, agentID+"\x00") {
				delete(s.ctxCalls, k)
			}
		}
		s.ctxCallMu.Unlock()
	}

	// Notify pulse of session start and record the model if provided (Option A).
	if pc := s.getPulseClient(); pc != nil && agentID != "" && s.logSessions {
		go func() {
			pc.RecordSessionEvent(agentID, s.projectID, "start")
			if model != "" {
				pc.RecordSessionModel(agentID, s.projectID, model, provider)
			}
		}()
	}

	// Emit session-start event so peers polling get_events see a new agent arrive.
	if s.store != nil && agentID != "" {
		_ = s.store.AppendEvent("agent_session_start", agentID,
			fmt.Sprintf(`{"agent_id":%q}`, agentID))
	}

	// ── Look up agent context profile for incremental delivery ───────────
	var agentCtx *store.AgentContext
	incremental := false
	var collisionWarning string
	if agentID != "" && s.store != nil {
		if ac, err := s.store.GetAgentContext(agentID); err == nil && ac != nil {
			agentCtx = ac
			incremental = true
			// Collision detection: if the same agent_id started a session within the
			// last 2 minutes, warn that two sessions may be competing under the same ID.
			if ac.LastSession != "" {
				if prev, parseErr := time.Parse(time.RFC3339, ac.LastSession); parseErr == nil {
					if time.Since(prev) < 2*time.Minute {
						collisionWarning = fmt.Sprintf(
							"agent_id %q was last seen %.0f seconds ago — another session may already be using this ID. "+
								"If this is a new session, append a unique suffix (e.g. %q) to avoid overwriting peer state.",
							agentID, time.Since(prev).Seconds(), agentID+"-2",
						)
					}
				}
			}
		}
	}

	// ── 1. Project identity + scale guidance ─────────────────────────────
	identity := s.graph.ProjectIdentity()
	currentHash := hashIdentity(identity)

	// Enrich with federation summary (mirrors handleGetProjectIdentity).
	primaryRepoID := s.graph.RepoID()
	crossCallCount := 0
	linkedSet := make(map[string]bool)
	var linkedRepos []string
	for _, e := range s.graph.AllEdges() {
		if e.Type != graph.EdgeCalls {
			continue
		}
		fromIdx := strings.Index(string(e.From), "::")
		toIdx := strings.Index(string(e.To), "::")
		if fromIdx < 0 || toIdx < 0 {
			continue
		}
		fromRepo := string(e.From)[:fromIdx]
		toRepo := string(e.To)[:toIdx]
		if fromRepo != toRepo {
			crossCallCount++
			for _, r := range []string{fromRepo, toRepo} {
				if r != primaryRepoID && !linkedSet[r] {
					linkedSet[r] = true
					linkedRepos = append(linkedRepos, r)
				}
			}
		}
	}
	sort.Strings(linkedRepos)

	// In incremental mode, skip project_identity if the hash hasn't changed.
	identitySkipped := false
	var projectSection interface{}
	if incremental && agentCtx.IdentityHash == currentHash && currentHash != "" {
		identitySkipped = true
		projectSection = map[string]interface{}{
			"skipped": true,
			"reason":  "unchanged since last session — identity_hash matches",
		}
	} else {
		projectSection = map[string]interface{}{
			"identity": identity,
			"federation": map[string]interface{}{
				"is_federated":        len(linkedRepos) > 0,
				"linked_repos":        linkedRepos,
				"cross_project_edges": crossCallCount,
			},
		}
	}

	// ── 2. Pending tasks ──────────────────────────────────────────────────
	type taskWithState struct {
		store.Task
		SessionState *store.SessionState `json:"session_state,omitempty"`
	}
	var pendingSection map[string]interface{}
	if s.store != nil {
		tasks, err := s.store.GetPendingTasks("", agentID)
		if err == nil {
			inProgressIDs := make([]string, 0)
			for _, t := range tasks {
				if t.Status == "in_progress" {
					inProgressIDs = append(inProgressIDs, t.ID)
				}
			}
			stateMap, _ := s.store.GetSessionStateForTasks(inProgressIDs)
			result := make([]taskWithState, len(tasks))
			for i, t := range tasks {
				result[i] = taskWithState{Task: t}
				if stateMap != nil {
					result[i].SessionState = stateMap[t.ID]
				}
			}
			summary := "no pending tasks"
			if len(tasks) > 0 {
				summary = fmt.Sprintf("%d task(s) pending/in-progress", len(tasks))
			}
			pendingSection = map[string]interface{}{
				"summary":  summary,
				"tasks":    result,
				"reminder": "Call update_task(id, 'in_progress') before starting a task and update_task(id, 'done', notes) immediately when finished. Never batch completions.",
			}
		}
	}
	if pendingSection == nil {
		pendingSection = map[string]interface{}{"summary": "no pending tasks", "tasks": []interface{}{}}
	}

	// ── 3. Working state ──────────────────────────────────────────────────
	windowMinutes := 15
	var recentChanges []changeEntry
	if s.changeSource != nil {
		for _, e := range s.changeSource.RecentChanges(windowMinutes) {
			recentChanges = append(recentChanges, changeEntry{
				File:         e.File,
				At:           e.Timestamp.Format("15:04:05"),
				NodesAdded:   e.NodesAdded,
				NodesRemoved: e.NodesRemoved,
				EdgesAdded:   e.EdgesAdded,
			})
		}
	}
	if recentChanges == nil {
		recentChanges = []changeEntry{}
	}
	workingSection := map[string]interface{}{
		"recent_changes": recentChanges,
	}
	// R32: include open quality gap count so agents know whether to check get_violations().
	// Always write the key (zero when store is nil) so callers can assert it safely.
	workingSection["open_quality_gaps"] = 0
	if s.store != nil {
		if gaps, err := s.store.GetGaps(store.GapFilter{Status: "open"}); err == nil {
			workingSection["open_quality_gaps"] = len(gaps)
		}
	}
	root := s.graph.Root()
	if root != "" {
		if out, err := exec.Command("git", "-C", root, "diff", "--stat", "HEAD").Output(); err == nil {
			if stat := strings.TrimSpace(string(out)); stat != "" {
				workingSection["git_diff_stat"] = stat
			}
		}
		if len(recentChanges) == 0 {
			if out, err := exec.Command("git", "-C", root, "log", "--oneline", "-7").Output(); err == nil {
				if log := strings.TrimSpace(string(out)); log != "" {
					workingSection["fallback_git_log"] = log
					workingSection["fallback_note"] = fmt.Sprintf("No file changes in the last %d minutes. Showing recent git commits for context.", windowMinutes)
				}
			}
		}
	}

	// ── 4. Recent agent events ───────────────────────────────────────────
	// In incremental mode, only return events since the agent's last known seq.
	var recentEvents []store.Event
	var latestEventSeq int64
	if s.store != nil {
		sinceSeq := int64(0)
		limit := 20
		if incremental && agentCtx.LastEventSeq > 0 {
			sinceSeq = agentCtx.LastEventSeq
			limit = 50 // allow more events when fetching delta
		}
		events, seq, err := s.store.GetEvents(sinceSeq, nil, "", limit)
		if err == nil {
			recentEvents = events
			latestEventSeq = seq
		}
	}
	if recentEvents == nil {
		recentEvents = []store.Event{}
	}

	// ── 4b. Agent awareness — 3-tier signal system (B29) ─────────────────
	// Zero noise by default. agent_awareness is omitted entirely when the calling
	// agent has no scope conflicts and no dependency alerts. Most solo sessions
	// produce no output here at all.
	//
	// Tier 1 — conflicts: another agent has a claim overlapping this agent's scope.
	// Tier 2 — dependency_alerts: a peer modified a symbol this agent examined.
	// active_count: integer, not a list — use get_peer_activity for the full digest.
	var agentAwareness map[string]interface{}
	var unreadMsgs []store.Message
	if s.store != nil && agentID != "" {
		// Tier 1: scope conflicts (always surface when present).
		var conflicts []store.WorkClaim
		if cls, err := s.store.GetConflicts(agentID); err == nil {
			conflicts = cls
		}

		// Tier 2: dependency alerts (surface when a peer touched a watched symbol).
		var depAlerts []store.DependencyAlert
		if alerts, err := s.store.GetDependencyAlerts(agentID); err == nil {
			depAlerts = alerts
		}

		// active_count: peers present (integer only — no list surfaced here).
		var activeCount int
		if n, err := s.store.CountActiveAgents(agentID); err == nil {
			activeCount = n
		}

		// Only build agent_awareness when there is a signal worth surfacing.
		if len(conflicts) > 0 || len(depAlerts) > 0 || activeCount > 0 {
			agentAwareness = map[string]interface{}{}
			if len(conflicts) > 0 {
				agentAwareness["conflicts"] = conflicts
			}
			if len(depAlerts) > 0 {
				agentAwareness["dependency_alerts"] = depAlerts
			}
			if activeCount > 0 {
				agentAwareness["active_count"] = activeCount
				agentAwareness["hint"] = "Call get_peer_activity(agent_id='<id>') or get_events(agent_id='<id>', types=['agent_examining','claim_work']) for a peer's activity stream."
			}
		}

		if unread, err := s.store.CountUnreadMessages(agentID); err == nil && unread > 0 {
			if agentAwareness == nil {
				agentAwareness = map[string]interface{}{}
			}
			agentAwareness["unread_messages"] = unread
		}
		// Auto-deliver up to 10 unread messages so agents don't need a separate call.
		if msgs, _, err := s.store.GetMessages(agentID, 0, "", true, 10); err == nil && len(msgs) > 0 {
			unreadMsgs = msgs
		}
	}

	// ── 5. Cross-project impact alerts (federated projects only) ──────────
	// Pull unread cross_project_impact messages from the message bus. These are
	// broadcast by the file watcher whenever a local change breaks a dependency
	// in a linked project. Surfacing them here ensures agents see the warning
	// at the very start of a session, before they commit to a plan.
	var crossProjectAlerts []store.Message
	if s.store != nil && crossCallCount > 0 {
		msgs, _, err := s.store.GetMessages("", 0, "cross_project_impact", true, 10)
		if err == nil && len(msgs) > 0 {
			crossProjectAlerts = msgs
		}
	}

	// ── Proactive failure injection ───────────────────────────────────────
	// If ≥5 failure episodes exist for this project, recall the most relevant
	// one relative to the most recently changed file. Cold-start safe: the
	// field is omitted entirely when fewer than 5 failures are recorded.
	var recentFailure map[string]interface{}
	if s.store != nil && len(recentChanges) > 0 {
		failures, err := s.store.GetEpisodes(primaryRepoID, "", "failure", nil, 5, 0)
		if err == nil && len(failures) >= 5 {
			query := filepath.Base(recentChanges[0].File)
			if matches, mErr := s.store.RecallEpisodes(query, primaryRepoID, "", "failure", "", 1, 0); mErr == nil && len(matches) > 0 {
				e := matches[0]
				recentFailure = map[string]interface{}{
					"decision":   e.Decision,
					"rationale":  e.Rationale,
					"outcome":    e.Outcome,
					"created_at": e.CreatedAt,
				}
			}
		}
	}

	// ── Assemble response ─────────────────────────────────────────────────
	resp := map[string]interface{}{
		"project_identity": projectSection,
		"pending_tasks":    pendingSection,
		"working_state":    workingSection,
		"recent_events":    recentEvents,
		"latest_event_seq": latestEventSeq,
		"scale_guidance":   identity.ToolGuidance,
		"session_hint":     "Pass latest_event_seq to get_events on the next call to receive only new events. Use scale_guidance to decide when to use Synapses tools vs Read/Grep.",
	}
	if incremental {
		resp["incremental"] = true
		if identitySkipped {
			resp["identity_skipped"] = true
		}
	}
	if agentAwareness != nil {
		resp["agent_awareness"] = agentAwareness
	}
	if len(unreadMsgs) > 0 {
		resp["unread_messages"] = map[string]interface{}{
			"count":    len(unreadMsgs),
			"messages": unreadMsgs,
			"hint":     "Call mark_read(message_id, agent_id) to acknowledge. Messages are NOT auto-marked as read.",
		}
	}
	if collisionWarning != "" {
		resp["warning"] = collisionWarning
	}
	if recentFailure != nil {
		resp["recent_failure"] = recentFailure
	}
	if len(crossProjectAlerts) > 0 {
		resp["cross_project_alerts"] = map[string]interface{}{
			"count":    len(crossProjectAlerts),
			"messages": crossProjectAlerts,
			"warning":  fmt.Sprintf("%d unread cross-project impact alert(s). A recent change may have broken dependencies in a linked project. Review before proceeding.", len(crossProjectAlerts)),
		}
	}

	// ── 6. Constitution (Hot Constitution — project principles) ───────────
	if s.config != nil && s.config.Constitution.InjectInSessionInit && len(s.config.Constitution.Principles) > 0 {
		resp["constitution"] = map[string]interface{}{
			"principles": s.config.Constitution.Principles,
			"count":      len(s.config.Constitution.Principles),
			"note":       "These project laws apply to all work in this session.",
		}
	}

	// ── Pre-warm brain cache for top entities (silent background op) ─────
	if bc := s.getBrainClient(); bc != nil {
		seen := make(map[string]bool)
		var warmFiles []string
		// Gather unique file paths from entry points and key entities.
		for _, ep := range identity.EntryPoints {
			if ep.File != "" && !seen[ep.File] {
				seen[ep.File] = true
				warmFiles = append(warmFiles, ep.File)
				if len(warmFiles) >= 5 {
					break
				}
			}
		}
		if len(warmFiles) < 5 {
			for _, ke := range identity.KeyEntities {
				if ke.File != "" && !seen[ke.File] {
					seen[ke.File] = true
					warmFiles = append(warmFiles, ke.File)
					if len(warmFiles) >= 5 {
						break
					}
				}
			}
		}
		if len(warmFiles) > 0 {
			go func() {
				for _, f := range warmFiles {
					s.warmBrainCache(f)
				}
			}()
		}
	}

	// ── 7. Sidecar availability ───────────────────────────────────────────
	// Let agents skip tool calls for unavailable sidecars without trial-and-error.
	resp["sidecars"] = map[string]interface{}{
		"brain": map[string]interface{}{
			"available": s.getBrainClient() != nil,
			"note":      "enriches get_context with LLM summaries; required by upsert_adr, get_adrs",
		},
		"doc_cache": map[string]interface{}{
			"available": s.webCache != nil,
			"note":      "enables lookup_docs with version-pinned package documentation",
		},
		"pulse": map[string]interface{}{
			"available": s.getPulseClient() != nil,
			"note":      "enables agent analytics and token tracking",
		},
	}

	// R29: surface effectiveness hints for low-scoring entities so agents
	// know upfront which entities typically need deeper context fetches.
	// Run in a goroutine with a 100ms deadline so a slow or unavailable pulse
	// sidecar never adds latency to the critical session_init response path.
	if pc := s.getPulseClient(); pc != nil {
		type hintResult struct{ hints []pulse.EntityEffectiveness }
		hintCh := make(chan hintResult, 1)
		projID := s.projectID
		// minSignals=5: require at least 5 signals before surfacing hints.
		// With 2 signals a single correction event would score 0.0 and trigger
		// a false "frequently insufficient" warning — 5 provides minimal
		// statistical validity before an entity is flagged.
		go func() { hintCh <- hintResult{hints: pc.FetchEffectiveness(projID, 5)} }()
		select {
		case res := <-hintCh:
			if len(res.hints) > 0 {
				// Only surface entities with a non-empty suggestion (score < 0.6).
				var lowScoring []map[string]interface{}
				for _, h := range res.hints {
					if h.Suggestion == "" {
						continue
					}
					lowScoring = append(lowScoring, map[string]interface{}{
						"entity":     h.Entity,
						"score":      h.Score,
						"suggestion": h.Suggestion,
					})
					if len(lowScoring) >= 5 {
						break
					}
				}
				if len(lowScoring) > 0 {
					resp["context_effectiveness_hints"] = map[string]interface{}{
						"note":     "These entities historically required multiple context fetches. Use detail_level=full or increase depth on first call.",
						"entities": lowScoring,
					}
				}
			}
		case <-time.After(100 * time.Millisecond):
			// Pulse sidecar is slow or unavailable — skip hints rather than blocking.
		}
	}

	// ── 8. Agent constraints (behavioral rules) ───────────────────────────
	// Surface all agent-type rules (no ForbiddenEdge, conversation-level constraints)
	// so every new session inherits decisions made in prior sessions without
	// the agent needing to re-discover or re-ask for them.
	s.rulesMu.RLock()
	var agentConstraints []map[string]string
	for _, r := range s.config.Rules {
		if r.IsAgentRule() {
			agentConstraints = append(agentConstraints, map[string]string{
				"id":          r.ID,
				"description": r.Description,
				"severity":    r.Severity,
			})
		}
	}
	s.rulesMu.RUnlock()
	if len(agentConstraints) > 0 {
		resp["agent_constraints"] = map[string]interface{}{
			"count":       len(agentConstraints),
			"constraints": agentConstraints,
			"note":        "These behavioral rules were established in prior sessions. Apply them throughout this session.",
		}
	}

	// ── 9. Relevant memories ─────────────────────────────────────────────
	// Surface institutional knowledge at session start so agents benefit from
	// prior sessions automatically. Three tiers:
	//   1. Entity memories for nodes linked to in-progress tasks
	//   2. Project-tier memories (always relevant)
	//   3. Session logs from this agent's recent sessions
	// Capped at ~500 chars total; all surfaced memories get touched (TTL renewal).
	if s.store != nil {
		const memCap = 500
		var memoryItems []map[string]string
		var touchIDs []string
		totalLen := 0

		addMemory := func(m store.Memory, label string) bool {
			if totalLen >= memCap {
				return false
			}
			content := m.Content
			// UTF-8 safe cap: truncate by rune count, not byte count.
			if runes := []rune(content); totalLen+len(runes) > memCap {
				content = string(runes[:memCap-totalLen])
			}
			memoryItems = append(memoryItems, map[string]string{
				"tier":    m.Tier,
				"content": content,
				"label":   label,
			})
			touchIDs = append(touchIDs, m.ID)
			totalLen += len(content)
			return totalLen < memCap
		}

		// 1. Entity memories for in-progress task linked nodes.
		if pendingSection != nil {
			if tasks, ok := pendingSection["tasks"].([]taskWithState); ok {
				var linkedNodeIDs []string
				for _, t := range tasks {
					if t.Status == "in_progress" {
						linkedNodeIDs = append(linkedNodeIDs, t.LinkedNodes...)
					}
				}
				if len(linkedNodeIDs) > 0 {
					entityMems, _ := s.store.QueryMemoriesForEntities(linkedNodeIDs, 5)
					for _, nodeID := range linkedNodeIDs {
						for _, m := range entityMems[nodeID] {
							if !addMemory(m, "task_entity") {
								break
							}
						}
					}
				}
			}
		}

		// 2. Project-tier memories.
		if totalLen < memCap {
			projMems, _ := s.store.QueryMemories(store.TierProject, "", "", 5)
			for _, m := range projMems {
				if !addMemory(m, "project") {
					break
				}
			}
		}

		// 3. Session logs from this agent's recent sessions.
		if totalLen < memCap && agentID != "" {
			sessMems, _ := s.store.QueryRecentSessionMemories(agentID, 3)
			for _, m := range sessMems {
				if !addMemory(m, "session_history") {
					break
				}
			}
		}

		if len(memoryItems) > 0 {
			resp["relevant_memories"] = map[string]interface{}{
				"count":    len(memoryItems),
				"memories": memoryItems,
				"note":     "Institutional knowledge from prior sessions. These memories are auto-surfaced and renewed on access.",
			}
			// Touch surfaced memories in background — TTL renewal must not add
			// latency to the session_init response.
			if len(touchIDs) > 0 {
				ids := make([]string, len(touchIDs))
				copy(ids, touchIDs)
				go func() {
					for _, id := range ids {
						s.store.TouchMemory(id)
					}
				}()
			}
		}
	}

	// ── R14C: Stale context hints ─────────────────────────────────────────
	// Cross-reference task-linked nodes and the previous session's entity register
	// against recently changed files. Returns entities whose containing file was
	// modified since the last session — signalling the agent to re-fetch them.
	if len(recentChanges) > 0 {
		// 1. Collect node IDs from in-progress task linked nodes.
		var nodeIDs []string
		if tasks, ok := pendingSection["tasks"].([]taskWithState); ok {
			for _, t := range tasks {
				nodeIDs = append(nodeIDs, t.LinkedNodes...)
			}
		}

		// 2. Augment with entity register from the most recent session log.
		// The session log embeds "Examined: X, Y, Z." — resolve names to node IDs.
		if s.store != nil && agentID != "" {
			if regMems, _ := s.store.QueryRecentSessionMemories(agentID, 1); len(regMems) > 0 {
				for _, name := range parseExaminedEntities(regMems[0].Content) {
					if nodes := s.graph.FindByName(name); len(nodes) > 0 {
						nodeIDs = append(nodeIDs, string(nodes[0].ID))
					}
				}
			}
		}

		if len(nodeIDs) > 0 {
			// Build a file-path → changed_at map for O(1) lookup.
			changedFiles := make(map[string]string)
			for _, rc := range recentChanges {
				changedFiles[rc.File] = rc.At
			}

			seen := make(map[string]bool)
			var hints []map[string]interface{}
			for _, nodeID := range nodeIDs {
				if seen[nodeID] {
					continue
				}
				seen[nodeID] = true

				// Node IDs are formatted as "repo::file::entityName".
				parts := strings.SplitN(nodeID, "::", 3)
				if len(parts) < 3 {
					continue
				}
				nodeFile, entityName := parts[1], parts[2]

				for changedFile, changedAt := range changedFiles {
					if containsFile([]string{changedFile}, nodeFile) {
						hints = append(hints, map[string]interface{}{
							"entity":     entityName,
							"file":       nodeFile,
							"changed_at": changedAt,
						})
						break
					}
				}
				if len(hints) >= 10 {
					break
				}
			}
			if len(hints) > 0 {
				resp["stale_context_hints"] = hints
			}
		}
	}

	// ── Activation-context prompts (auto_load: true) ──────────────────────
	// Surface project-wide conventions so agents apply them from the first message.
	// Only prompts with auto_load: true are included here; entity-specific prompts
	// surface in individual get_context calls.
	if autoPrompts := s.getAutoLoadPrompts(); len(autoPrompts) > 0 {
		promptList := make([]map[string]string, 0, len(autoPrompts))
		for _, pt := range autoPrompts {
			promptList = append(promptList, map[string]string{
				"id":     pt.ID,
				"source": pt.Source,
				"body":   pt.Body,
			})
		}
		resp["active_prompts"] = map[string]interface{}{
			"count":   len(promptList),
			"prompts": promptList,
			"note":    "Project-wide activation context. Apply these conventions throughout the session.",
		}
	}

	// ── Update agent context profile ─────────────────────────────────────
	// Record what this agent now knows so the next session_init can be incremental.
	if agentID != "" && s.store != nil {
		if err := s.store.UpsertAgentContext(&store.AgentContext{
			AgentID:      agentID,
			LastEventSeq: latestEventSeq,
			IdentityHash: currentHash,
			LastSession:  time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			log.Printf("mcp: upsert agent context: %v", err)
		}
	}

	return jsonResult(resp)
}

// jsonResult marshals v to JSON and wraps it in a text tool result.
func jsonResult(v interface{}) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

// handleReportUsage records agent-self-reported LLM token usage (Option B).
// The agent calls this after completing a response to give Synapses accurate
// model cost data that cannot be inferred from the MCP layer alone.
func (s *Server) handleReportUsage(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	model, _    := args["model"].(string)
	provider, _ := args["provider"].(string)
	agentID, _  := args["agent_id"].(string)
	if agentID == "" {
		agentID = s.getLastAgent()
	}

	inputTokens  := 0
	outputTokens := 0
	costUSD       := 0.0
	if v, ok := args["input_tokens"].(float64); ok {
		inputTokens = int(v)
	}
	if v, ok := args["output_tokens"].(float64); ok {
		outputTokens = int(v)
	}
	if v, ok := args["cost_usd"].(float64); ok {
		costUSD = v
	}

	if model == "" {
		return mcp.NewToolResultError("model is required"), nil
	}

	pc := s.getPulseClient()
	if pc != nil {
		sessionID := agentID + ":" + s.projectID + ":" + time.Now().UTC().Format("2006-01-02")
		go pc.RecordAgentLLMUsage(pulse.AgentLLMUsageEvent{
			SessionID:    sessionID,
			AgentID:      agentID,
			ProjectID:    s.projectID,
			Model:        model,
			Provider:     provider,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			CostUSD:      costUSD,
		})
	}

	return jsonResult(map[string]interface{}{
		"recorded": true,
		"model":    model,
		"note":     "Usage recorded. Thank you — this improves cost-savings accuracy in Analytics.",
	})
}

// cloneGraph creates a shallow copy of g with an independent edge set.
// Used to simulate plan additions without mutating the live graph.
func cloneGraph(g *graph.Graph) *graph.Graph {
	clone := graph.New(g.RepoID())
	for _, n := range g.AllNodes() {
		clone.AddNode(n)
	}
	for _, e := range g.AllEdges() {
		clone.AddEdge(e)
	}
	return clone
}

// handleAnnotateNode attaches a note to a graph node, visible to all agents.
func (s *Server) handleAnnotateNode(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("annotations unavailable: server started without a persistent store"), nil
	}

	nodeID, _ := req.GetArguments()["node_id"].(string)
	if nodeID == "" {
		return mcp.NewToolResultError("node_id is required"), nil
	}
	note, _ := req.GetArguments()["note"].(string)
	if note == "" {
		return mcp.NewToolResultError("note is required"), nil
	}
	agentID, _ := req.GetArguments()["agent_id"].(string)

	// Verify the node exists in the graph.
	if s.graph.GetNode(graph.NodeID(nodeID)) == nil {
		return mcp.NewToolResultError(fmt.Sprintf("node not found: %q", nodeID)), nil
	}

	id, err := s.store.AddAnnotation(nodeID, agentID, note)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("add annotation: %v", err)), nil
	}

	// Dual-write to unified memories table (entity tier).
	_, _ = s.store.InsertMemory(store.Memory{
		Tier:     store.TierEntity,
		Content:  note,
		EntityID: nodeID,
		AgentID:  agentID,
		Source:   store.SourceManual,
		Tags:     `["annotation"]`,
	})

	// Emit event.
	if err := s.store.AppendEvent("annotation_added", agentID,
		fmt.Sprintf(`{"annotation_id":%q,"node_id":%q}`, id, nodeID)); err != nil {
		fmt.Fprintf(os.Stderr, "synapses: append annotation_added event: %v\n", err)
	}

	return jsonResult(map[string]interface{}{
		"annotation_id": id,
		"node_id":       nodeID,
		"message":       "Annotation saved. It will appear in get_context responses for this node.",
	})
}

// trimRepoRoot strips the repo root prefix from a slice of absolute file paths,
// returning paths relative to the repo root. Mirrors the normalizeSubgraph
// behaviour so TestCoverage paths are consistent with all other file references
// in get_context/get_impact responses.
func (s *Server) trimRepoRoot(paths []string) []string {
	root := s.graph.Root()
	if root == "" || len(paths) == 0 {
		return paths
	}
	prefix := root
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = strings.TrimPrefix(p, prefix)
	}
	return out
}

// handleGetImpact performs reverse-BFS blast-radius analysis from a named entity.
// Returns nodes grouped by depth tier: direct (depth 1, confidence 1.0),
// indirect (depth 2, confidence 0.6), peripheral (depth 3+, confidence 0.3).
func (s *Server) handleGetImpact(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	symbol, _ := req.GetArguments()["symbol"].(string)
	if symbol == "" {
		return mcp.NewToolResultError("symbol is required"), nil
	}

	maxDepth := 3
	if d, ok := req.GetArguments()["depth"].(float64); ok && d > 0 {
		maxDepth = int(d)
		if maxDepth > 10 {
			maxDepth = 10
		}
	}

	// Resolve symbol name → node. Fall back to pattern match (same as get_context).
	candidates := s.graph.FindByName(symbol)
	if len(candidates) == 0 {
		candidates = s.graph.FindByPattern(symbol)
	}
	if len(candidates) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("entity not found: %q", symbol)), nil
	}
	root := pickBestNode(candidates, s.graph)

	// For struct/interface nodes, aggregate impact across all their methods.
	// A struct itself has no incoming CALLS edges — its methods do.
	if root.Type == graph.NodeStruct || root.Type == graph.NodeInterface {
		methods := s.graph.FindByPattern(root.Name)
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
		return jsonResult(merged)
	}

	result, err := s.graph.ImpactAnalysis(root.ID, maxDepth)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("impact analysis: %v", err)), nil
	}
	if result.Tiers == nil {
		result.Tiers = []graph.ImpactTier{}
	}

	// R2: Attach test coverage — files that exercise this entity via reverse CALLS BFS.
	// trimRepoRoot converts absolute paths to repo-relative paths for consistency
	// with all other file references in get_context/get_impact responses.
	result.TestCoverage = s.trimRepoRoot(s.graph.FindTestsFor(root.ID))

	return jsonResult(result)
}

// handleSemanticSearch runs a two-path search and merges results:
//  1. Vector cosine similarity — when an embed client is configured (brain /v1/embed
//     or explicit embedding_endpoint in synapses.json). This is the true semantic
//     path: concept queries like "how does auth work" find TokenValidator even if
//     those words never appear in the query.
//  2. FTS5 BM25 keyword ranking — always runs as fallback / supplement.
//
// Results are merged: vector hits first, then unique FTS5 hits appended up to
// the requested limit. search_mode in the response reports which path fired.
func (s *Server) handleSemanticSearch(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("semantic_search requires a persistent store (run 'synapses start' or 'synapses index' first)"), nil
	}

	query := stringArg(req, "query")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	limitRaw, _ := req.GetArguments()["limit"].(float64)
	limit := int(limitRaw)
	if limit <= 0 {
		limit = 20
	}

	// --- Vector path (when embedding_endpoint is configured) ---
	// Embed the query with a 2s timeout so a slow Ollama never blocks the agent.
	// On any error, silently fall through to FTS5-only results.
	var vectorResults []store.SearchResult
	searchMode := "fts5_bm25"
	if s.embedClient != nil {
		embedCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		queryVec, embedErr := s.embedClient.Embed(embedCtx, query)
		cancel()
		if embedErr == nil && len(queryVec) > 0 {
			vr, verr := s.store.VectorSearch(queryVec, limit)
			if verr == nil && len(vr) > 0 {
				vectorResults = vr
				searchMode = "vector_cosine"
			}
		}
	}

	// --- FTS5 path (always runs as fallback / supplement) ---
	ftsResults, err := s.store.SemanticSearch(query, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("semantic search: %v", err)), nil
	}

	// --- Merge: vector results first, then FTS results not already returned ---
	var results []store.SearchResult
	if len(vectorResults) > 0 {
		results = vectorResults
		seen := make(map[string]bool, len(vectorResults))
		for _, r := range vectorResults {
			seen[r.ID] = true
		}
		for _, r := range ftsResults {
			if !seen[r.ID] {
				results = append(results, r)
			}
		}
		if len(results) > limit {
			results = results[:limit]
		}
		if len(vectorResults) > 0 {
			searchMode = "hybrid_vector+fts5"
		}
	} else {
		results = ftsResults
	}

	resp := map[string]interface{}{
		"query":       query,
		"count":       len(results),
		"results":     results,
		"search_mode": searchMode,
	}

	embeddingCount := s.store.EmbeddingCount()
	if s.embedClient != nil && searchMode == "fts5_bm25" {
		if embeddingCount == 0 {
			resp["note"] = fmt.Sprintf("Vector embeddings not yet built. Run 'synapses index' or wait for the background embedding pass to complete (model: %s).", s.embedClient.Model())
		} else {
			resp["note"] = fmt.Sprintf("Vector index partial (%d nodes embedded). Results blended from cosine+FTS5 as more embeddings complete.", embeddingCount)
		}
	}
	if len(results) == 0 {
		resp["hint"] = "No matches found. Try broader terms, partial names, or use search() for exact substring matching."
	}

	return jsonResult(resp)
}

// inlineFindEntity runs the find_entity lookup inline. Used by handleGetContext
// to surface candidates when the requested entity cannot be found by exact name,
// saving agents a separate find_entity round-trip.
func (s *Server) inlineFindEntity(query string) []map[string]interface{} {
	nodes := s.graph.FindByName(query)
	if len(nodes) == 0 {
		nodes = s.graph.FindByPattern(query)
	}
	// Dotted method name fallback: "Store.Close" → search "Close", filter by "Store".
	if len(nodes) == 0 && strings.Contains(query, ".") {
		parts := strings.SplitN(query, ".", 2)
		typePrefix, method := strings.ToLower(parts[0]), parts[1]
		candidates := s.graph.FindByName(method)
		if len(candidates) == 0 {
			candidates = s.graph.FindByPattern(method)
		}
		for _, n := range candidates {
			if strings.Contains(strings.ToLower(string(n.ID)), typePrefix) ||
				strings.Contains(strings.ToLower(n.File), typePrefix) {
				nodes = append(nodes, n)
			}
		}
	}

	pathPrefix := s.graph.Root()
	if pathPrefix != "" && !strings.HasSuffix(pathPrefix, "/") {
		pathPrefix += "/"
	}

	results := make([]map[string]interface{}, 0, len(nodes))
	for _, n := range nodes {
		file := n.File
		if pathPrefix != "" {
			file = strings.TrimPrefix(file, pathPrefix)
		}
		results = append(results, map[string]interface{}{
			"name": n.Name,
			"type": string(n.Type),
			"file": file,
			"line": n.Line,
		})
	}
	return results
}

// adaptiveCarveConfig adjusts cfg in-place based on stored feedback episodes
// for the given entity+agent pair. Returns true when the detail level should
// be forced to "full" (caller handles compact-format override).
//
// Two signals are read from the episode store (last 30 days):
//  1. context_quality + failure (explicit helpful=false) within the last 7 days
//     → depth += 1, force full detail
//  2. repeated_context (≥2 cross-session repeat episodes within 30 days)
//     → expand to depth=3 (if not already deeper), force full detail
//
// Older feedback (7–30 days) is ignored so decay is natural over time.
// Fail-silent: any store error leaves cfg unchanged.
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

// trackContextCall increments and returns the call count for (agentID, entity)
// within the current server session. Entries older than 30m are pruned at most
// once every 5 minutes to avoid O(n) iteration on every write (R29 GAP3).
func (s *Server) trackContextCall(agentID, entity string) int {
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
		s.ctxCalls[key] = &ctxCallEntry{count: 1, firstAt: now}
		return 1
	}
	e.count++
	return e.count
}
