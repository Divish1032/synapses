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
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/metrics"
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
	ADRs                   []brain.ADR                   `json:"adrs,omitempty"`                 // relevant accepted ADRs for this entity's file
	StaleAnnotationWarning string                        `json:"stale_annotation_warning,omitempty"` // GAP-3: set when ≥1 annotation may be outdated
	RecentChanges          []metrics.CommitInfo          `json:"recent_changes,omitempty"`           // GAP-7: last 3 git commits that touched the entity's file
	GraphFreshness         string                        `json:"graph_freshness,omitempty"`          // GAP-4: warning when entity's file was recently modified
}

// handleGetContext returns an N-hop ego-subgraph around the named entity,
// split into callers, callees, and related buckets.
func (s *Server) handleGetContext(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	entityName, ok := req.GetArguments()["entity"].(string)
	if !ok || entityName == "" {
		return mcp.NewToolResultError("entity is required"), nil
	}

	// GAP-1: Feedback loop.
	// (a) Track repeat calls — ≥3 calls for the same entity by the same agent
	//     auto-records a pattern episode: initial context wasn't sufficient.
	// (b) Optional explicit feedback via helpful=true/false.
	agentIDForFeedback, _ := req.GetArguments()["agent_id"].(string)
	if agentIDForFeedback != "" && s.store != nil {
		repeatCount := s.trackContextCall(agentIDForFeedback, entityName)
		if repeatCount == 3 {
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

	// Allow per-call overrides of depth and token budget.
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

	// Resolve the entity name to a node ID.
	nodes := s.graph.FindByName(entityName)
	if len(nodes) == 0 {
		nodes = s.graph.FindByPattern(entityName)
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

	subgraph, err := s.graph.CarveEgoGraph(best.ID, cfg)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// normalizeSubgraph deep-copies nodes, so boosting relevance on the result
	// is safe — we never mutate the cached subgraph.
	sg := normalizeSubgraph(subgraph, s.graph.Root())
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

	// P2.2: impact mode — reverse-only BFS, same shape as get_impact.
	mode, _ := req.GetArguments()["mode"].(string)
	if mode == "impact" {
		maxDepth := cfg.MaxDepth
		result, err := s.graph.ImpactAnalysis(best.ID, maxDepth)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("impact analysis: %v", err)), nil
		}
		return jsonResult(result)
	}

	dc := toDirectionalContext(sg)

	// Brain enrichment: async pattern — serve raw graph immediately, enrich in background.
	// If a cached packet exists, attach it (fast path). Otherwise, kick off background
	// enrichment so the next get_context call for this entity picks up the enriched version.
	if bc := s.getBrainClient(); bc != nil {
		cacheKey := fmt.Sprintf("%s:%d", entityName, cfg.MaxDepth)
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

	// Attach annotations from all agents if the store is available.
	if s.store != nil {
		nodeIDs := make([]string, 0, len(sg.Nodes)+1)
		nodeIDs = append(nodeIDs, string(sg.Root))
		for _, cn := range sg.Nodes {
			nodeIDs = append(nodeIDs, string(cn.Node.ID))
		}
		if annMap, err := s.store.GetAnnotationsForNodes(nodeIDs); err == nil && len(annMap) > 0 {
			dc.Annotations = annMap
			// GAP-3: Warn when any annotation was written against a node whose
			// call-graph has since changed significantly (stale=true).
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
	}

	// ── Context enrichment: auto-inject rules, failures, and task context ──
	// This saves agents from making separate calls to get_violations, recall,
	// and get_pending_tasks when exploring an entity.
	if s.store != nil && best != nil {
		var enrichment contextEnrichment

		// 1. Applicable architectural rules for this file.
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
			// Also include dynamic rules from store.
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

		// 2. Recent failure episodes mentioning this entity (top 2).
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

		// 3. Active task linked to this entity.
		if taskID != "" {
			if task, err := s.store.GetTask(taskID); err == nil {
				enrichment.ActiveTask = &taskHint{
					ID:     task.ID,
					Title:  task.Title,
					Status: task.Status,
				}
			}
		}

		// Only attach if there's anything to show.
		if len(enrichment.ApplicableRules) > 0 || len(enrichment.RecentFailures) > 0 || enrichment.ActiveTask != nil {
			dc.Enrichment = &enrichment
		}
	}

	// Context-aware next-step suggestions.
	dc.SuggestedNextTools = suggestNextAfterContext(dc)

	// Hot Constitution: inject project principles if configured.
	if s.config != nil && s.config.Constitution.InjectInContext && len(s.config.Constitution.Principles) > 0 {
		dc.Principles = s.config.Constitution.Principles
	}

	// GAP-7: Git "why" layer — surface recent commits for the entity's file so
	// agents understand WHY the code looks the way it does without needing ADRs.
	if dc.Root != nil && dc.Root.File != "" {
		repoRoot := s.graph.Root()
		if repoRoot != "" {
			if commits := metrics.RecentCommitsForFile(repoRoot, dc.Root.File, 3); len(commits) > 0 {
				dc.RecentChanges = commits
			}
		}
	}

	// GAP-4: Graph freshness — warn when the entity's file was modified very recently,
	// meaning the graph may not yet reflect the latest changes (watcher latency).
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

	// ADRs: fetch relevant accepted ADRs for this entity's file (brain required, fail-silent).
	if bc := s.getBrainClient(); bc != nil && dc.Root != nil && dc.Root.File != "" {
		if adrs, err := bc.GetADRs(context.Background(), dc.Root.File); err == nil && len(adrs) > 0 {
			if len(adrs) > 2 {
				adrs = adrs[:2]
			}
			dc.ADRs = adrs
		}
	}

	// Pulse telemetry: emit context delivery metrics (token savings vs baseline).
	agentID, _ := req.GetArguments()["agent_id"].(string)
	s.emitContextDelivery(
		"get_context", agentID, entityName, best.File,
		dc, sg.Nodes, sg.Edges,
		sg.TruncatedCount,       // nodes_pruned: nodes dropped by the token budget
		sg.Truncated,
		dc.ContextPacket != nil, // brain_enriched
		false,                   // cache_hit (packet cache is internal; context always delivered fresh)
	)

	// Multi-agent awareness: emit event so other agents can see what's being examined.
	if agentID != "" && s.store != nil {
		payload, _ := json.Marshal(map[string]string{
			"entity": entityName,
			"file":   best.File,
		})
		if err := s.store.AppendEvent("agent_examining", agentID, string(payload)); err != nil {
			log.Printf("mcp: append agent_examining event: %v", err)
		}
	}

	// format=compact returns a natural-language briefing instead of the default JSON blob.
	// detail_level controls depth: "summary" (~50t), "neighbors" (~200t), "full" (~400-600t, default).
	format, _ := req.GetArguments()["format"].(string)
	if format == "compact" {
		detailLevel, _ := req.GetArguments()["detail_level"].(string)
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
		return jsonResult(&disambiguatedContext{
			directionalContext: dc,
			OtherCandidates:    disambiguationCandidates,
			DisambigHint:       fmt.Sprintf("%d entities named %q found. Showing best match. Re-call with file=\"path/suffix\" to pin to a specific file.", len(disambiguationCandidates), entityName),
		})
	}

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

	pkt := bc.BuildContextPacket(context.Background(), brain.ContextPacketRequest{
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
		EnableLLM: s.config.Brain.EnableLLM,
	})
	if pkt != nil {
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
	calleesOfRoot := make(map[graph.NodeID]bool)
	callersOfRoot := make(map[graph.NodeID]bool)
	for _, e := range sg.Edges {
		if e.Type != graph.EdgeCalls {
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

	// Per-file analysis.
	type fileReport struct {
		File             string              `json:"file"`
		InGraph          bool                `json:"in_graph"`
		NodeCount        int                 `json:"node_count"`
		Entities         []string            `json:"entities,omitempty"`
		Violations       []config.Violation  `json:"violations,omitempty"`
		FreshnessWarning string              `json:"freshness_warning,omitempty"`
	}

	var reports []fileReport
	totalViolations := 0

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
		"files":            reports,
	}
	if taskVerification != nil {
		result["task_verification"] = taskVerification
	}
	if notIndexed > 0 {
		result["indexing_hint"] = fmt.Sprintf("%d file(s) not yet in graph — wait for indexing or re-run verify_implementation.", notIndexed)
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

	rule := config.Rule{
		ID:          ruleID,
		Description: description,
		Severity:    severity,
		ForbiddenEdge: config.ForbiddenEdge{
			EdgeType:        graph.EdgeType(stringArg(req, "edge_type")),
			FromFilePattern: stringArg(req, "from_file_pattern"),
			ToFilePattern:   stringArg(req, "to_file_pattern"),
			ToNamePattern:   stringArg(req, "to_name_pattern"),
		},
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
	if s.store != nil {
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
		"status":  "ok",
		"rule_id": ruleID,
		"message": fmt.Sprintf("Rule %q is now active.", ruleID),
	})
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

	// Web
	{Name: "web_search", Category: "web", Description: "Search the web for docs/solutions", Keywords: []string{"web", "search", "internet", "docs", "documentation", "online"}, Example: `web_search(query="go context.WithTimeout best practices")`},
	{Name: "web_fetch", Category: "web", Description: "Fetch and read a web page", Keywords: []string{"fetch", "read", "url", "page", "website", "download"}, Example: `web_fetch(input="https://docs.example.com")`},
	{Name: "web_deep_search", Category: "web", Description: "Multi-query research on a topic", Keywords: []string{"deep", "research", "thorough", "comprehensive", "multiple"}, Example: `web_deep_search(query="Go MCP server patterns")`},
	{Name: "lookup_docs", Category: "web", Description: "One-shot documentation lookup", Keywords: []string{"docs", "documentation", "api", "reference", "lookup", "package"}, Example: `lookup_docs(query="openai python chat completions API")`},

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
		s.emitFileContextDelivery(agentIDFC, filePath, matches, payload)
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
	s.emitFileContextDelivery(agentIDFC, filePath, matches, multiPayload)
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

	// BFS following CALLS edges (forward) and IMPLEMENTS edges (both directions).
	// IMPLEMENTS edges are traversed bidirectionally so the search can cross
	// interface boundaries: struct → interface (forward) and interface → struct
	// (backward), enabling chains like: Caller → ConcreteType → Interface or
	// Caller → Interface → ConcreteImplementation.
	prev := map[graph.NodeID]graph.NodeID{fromNode.ID: ""}
	// viaImpl tracks which steps in the chain crossed an IMPLEMENTS boundary.
	viaImpl := make(map[graph.NodeID]bool)
	queue := []graph.NodeID{fromNode.ID}
	found := false

	for len(queue) > 0 && !found {
		curr := queue[0]
		queue = queue[1:]

		// Forward edges: CALLS and IMPLEMENTS (concrete → interface).
		for _, e := range s.graph.OutEdges(curr) {
			if e.Type != graph.EdgeCalls && e.Type != graph.EdgeImplements {
				continue
			}
			if _, visited := prev[e.To]; visited {
				continue
			}
			prev[e.To] = curr
			if e.Type == graph.EdgeImplements {
				viaImpl[e.To] = true
			}
			if e.To == toNode.ID {
				found = true
				break
			}
			queue = append(queue, e.To)
		}
		if found {
			break
		}

		// Backward IMPLEMENTS edges (interface → concrete struct).
		for _, e := range s.graph.InEdges(curr) {
			if e.Type != graph.EdgeImplements {
				continue
			}
			if _, visited := prev[e.From]; visited {
				continue
			}
			prev[e.From] = curr
			viaImpl[e.From] = true
			if e.From == toNode.ID {
				found = true
				break
			}
			queue = append(queue, e.From)
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
		return jsonResult(map[string]interface{}{
			"found":  false,
			"from":   map[string]interface{}{"name": fromName, "file": fromNode.File, "type": string(fromNode.Type)},
			"to":     map[string]interface{}{"name": toName, "file": toNode.File, "type": string(toNode.Type)},
			"reason": reason,
			"hint":   hint,
		})
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
		Via  string `json:"via,omitempty"` // "implements" when crossing an interface boundary
	}
	usedInterface := false
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
		chain = append(chain, step)
	}

	return jsonResult(map[string]interface{}{
		"found":         true,
		"hops":          len(chain) - 1,
		"via_interface": usedInterface,
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
	s.upsertAgentIfNeeded(agentID)

	// Notify pulse of session start so agent stats are trackable.
	if pc := s.getPulseClient(); pc != nil && agentID != "" {
		go pc.RecordSessionEvent(agentID, "start")
	}

	// ── Look up agent context profile for incremental delivery ───────────
	var agentCtx *store.AgentContext
	incremental := false
	if agentID != "" && s.store != nil {
		if ac, err := s.store.GetAgentContext(agentID); err == nil && ac != nil {
			agentCtx = ac
			incremental = true
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
		events, seq, err := s.store.GetEvents(sinceSeq, nil, limit)
		if err == nil {
			recentEvents = events
			latestEventSeq = seq
		}
	}
	if recentEvents == nil {
		recentEvents = []store.Event{}
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
		"scout": map[string]interface{}{
			"available": s.getScoutClient() != nil,
			"note":      "enables web_search, web_fetch, web_deep_search, lookup_docs",
		},
		"pulse": map[string]interface{}{
			"available": s.getPulseClient() != nil,
			"note":      "enables agent analytics and token tracking",
		},
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
		return jsonResult(merged)
	}

	result, err := s.graph.ImpactAnalysis(root.ID, maxDepth)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("impact analysis: %v", err)), nil
	}
	if result.Tiers == nil {
		result.Tiers = []graph.ImpactTier{}
	}

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

// trackContextCall increments and returns the call count for (agentID, entity)
// within the current server session. Entries older than 30m are lazily pruned.
// Used by the GAP-1 feedback loop to detect when initial context is insufficient.
func (s *Server) trackContextCall(agentID, entity string) int {
	key := agentID + "\x00" + entity
	s.ctxCallMu.Lock()
	defer s.ctxCallMu.Unlock()
	if s.ctxCalls == nil {
		s.ctxCalls = make(map[string]*ctxCallEntry)
	}
	// Lazy GC: purge entries older than 30 minutes on each write.
	now := time.Now()
	for k, e := range s.ctxCalls {
		if now.Sub(e.firstAt) > 30*time.Minute {
			delete(s.ctxCalls, k)
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
