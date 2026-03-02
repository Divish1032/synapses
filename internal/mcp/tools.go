package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Divish1032/synapses/internal/brain"
	"github.com/Divish1032/synapses/internal/config"
	"github.com/Divish1032/synapses/internal/git"
	"github.com/Divish1032/synapses/internal/graph"
	"github.com/Divish1032/synapses/internal/peer"
	"github.com/Divish1032/synapses/internal/store"
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
			"1. get_project_identity → understand scope, entry points, active rules",
			"2. claim_work → register your scope before editing",
			"3. validate_plan → check proposed changes against architectural rules",
			"4. get_context → explore entity structure (callees, callers, annotations)",
			"5. annotate_node → leave findings for other agents",
			"6. update_task → mark work done as you go",
			"7. release_claims → free scope when done",
		},
	}
	return jsonResult(out)
}

// directionalContext is the response shape for get_context.
// Nodes are split into callers/callees/related so the LLM can immediately
// understand call direction without inspecting raw edge types.
type directionalContext struct {
	Root               *graph.Node                   `json:"root"`
	Callees            []graph.CarvedNode            `json:"callees"`                        // root --CALLS--> node
	Callers            []graph.CarvedNode            `json:"callers"`                        // node --CALLS--> root
	Related            []graph.CarvedNode            `json:"related"`                        // everything else
	Annotations        map[string][]store.Annotation `json:"annotations,omitempty"`          // node_id → []Annotation
	ContextPacket      *brain.ContextPacket          `json:"context_packet,omitempty"`       // LLM-enriched packet (present when brain is available)
	SuggestedNextTools []toolSuggestion              `json:"suggested_next_tools,omitempty"` // context-aware next steps
	Truncated          bool                          `json:"truncated,omitempty"`            // true when token budget cut results
	TruncatedCount     int                           `json:"truncated_count,omitempty"`      // nodes dropped by budget
}

// handleGetContext returns an N-hop ego-subgraph around the named entity,
// split into callers, callees, and related buckets.
func (s *Server) handleGetContext(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	entityName, ok := req.Params.Arguments["entity"].(string)
	if !ok || entityName == "" {
		return mcp.NewToolResultError("entity is required"), nil
	}

	cfg := s.config.CarveConfig()

	// Allow per-call overrides of depth and token budget.
	if d, ok := req.Params.Arguments["depth"].(float64); ok && d > 0 {
		cfg.MaxDepth = int(d)
	}
	if b, ok := req.Params.Arguments["token_budget"].(float64); ok && b > 0 {
		cfg.TokenBudget = int(b)
	}

	// P1.6: Task-aware relevance boost — nodes linked to the active task float up.
	taskID, _ := req.Params.Arguments["task_id"].(string)
	var boostedNodes map[graph.NodeID]bool
	if taskID != "" && s.store != nil {
		if task, err := s.store.GetTask(taskID); err == nil && len(task.LinkedNodes) > 0 {
			boostedNodes = make(map[graph.NodeID]bool, len(task.LinkedNodes))
			for _, id := range task.LinkedNodes {
				boostedNodes[graph.NodeID(id)] = true
			}
		}
	}

	// Resolve the entity name to a node ID.
	nodes := s.graph.FindByName(entityName)
	if len(nodes) == 0 {
		nodes = s.graph.FindByPattern(entityName)
	}
	if len(nodes) == 0 {
		return jsonResult(map[string]interface{}{
			"error": fmt.Sprintf("entity not found: %q", entityName),
			"hint":  "Try search(mode=semantic) for fuzzy/concept matching, or find_entity to check exact name. For methods, use TypeName.MethodName format.",
		})
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
	mode, _ := req.Params.Arguments["mode"].(string)
	if mode == "impact" {
		maxDepth := cfg.MaxDepth
		result, err := s.graph.ImpactAnalysis(best.ID, maxDepth)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("impact analysis: %v", err)), nil
		}
		return jsonResult(result)
	}

	dc := toDirectionalContext(sg)

	// Brain enrichment: build a Context Packet if the intelligence sidecar is available.
	// Results are cached for 30s (keyed by entity:depth) to avoid re-hitting the brain
	// on repeated get_context calls within the same agent session.
	if bc := s.getBrainClient(); bc != nil {
		cacheKey := fmt.Sprintf("%s:%d", entityName, cfg.MaxDepth)
		var pkt *brain.ContextPacket
		if cached := s.getPacketFromCache(cacheKey); cached != nil {
			pkt = cached.(*brain.ContextPacket)
		} else {
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

			// Compute graph topology signals for enhanced context packets.
			hasTests := fileHasTests(best.File)
			fanIn := s.graph.Fanin(best.ID)

			pkt = bc.BuildContextPacket(context.Background(), brain.ContextPacketRequest{
				Snapshot: brain.SnapshotInput{
					RootNodeID:      string(best.ID),
					RootName:        best.Name,
					RootType:        string(best.Type),
					RootFile:        best.File,
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
		dc.ContextPacket = pkt // nil if brain unavailable — callers check for nil
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
		}
	}

	// Context-aware next-step suggestions.
	dc.SuggestedNextTools = suggestNextAfterContext(dc)

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
	if dc.Root != nil && dc.Root.Metadata != nil {
		if dc.Root.Metadata["complexity"] != "" {
			suggestions = append(suggestions, toolSuggestion{
				Tool:   "get_communities",
				Reason: "complexity metadata present — check if this entity bridges module clusters",
			})
		}
	}
	suggestions = append(suggestions,
		toolSuggestion{Tool: "claim_work", Reason: "reserve this entity before editing to prevent conflicts"},
		toolSuggestion{Tool: "validate_plan", Reason: "check proposed changes against architectural rules"},
		toolSuggestion{Tool: "annotate_node", Reason: "leave a note for other agents on a key finding"},
	)
	return suggestions
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
	query, ok := req.Params.Arguments["query"].(string)
	if !ok || query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	// Exact match first, then substring.
	nodes := s.graph.FindByName(query)
	if len(nodes) == 0 {
		nodes = s.graph.FindByPattern(query)
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
func (s *Server) handleValidatePlan(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	changesRaw, ok := req.Params.Arguments["changes"].(string)
	if !ok || changesRaw == "" {
		return mcp.NewToolResultError("changes is required"), nil
	}

	var changes []ProposedChange
	if err := json.Unmarshal([]byte(changesRaw), &changes); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid changes JSON: %v", err)), nil
	}

	// Build a temporary overlay graph that includes the proposed additions.
	overlay := cloneGraph(s.graph)
	var warnings []string
	for _, change := range changes {
		if change.AddsCallTo == "" {
			continue
		}
		// Find the callee node.
		callees := overlay.FindByName(change.AddsCallTo)
		if len(callees) == 0 {
			warnings = append(warnings, fmt.Sprintf("adds_call_to %q: not found in graph — cannot validate this edge", change.AddsCallTo))
			continue
		}
		// Find a node representing the source file.
		sources := overlay.FindByPattern(change.File)
		if len(sources) == 0 {
			warnings = append(warnings, fmt.Sprintf("file %q: no matching node in graph", change.File))
			continue
		}
		overlay.AddEdge(&graph.Edge{
			From: sources[0].ID,
			To:   callees[0].ID,
			Type: graph.EdgeCalls,
		})
	}

	s.rulesMu.RLock()
	violations := s.config.CheckViolations(overlay)
	s.rulesMu.RUnlock()

	status := "ok"
	if len(violations) > 0 {
		status = "violations_found"
	}

	result := map[string]interface{}{
		"status":     status,
		"violations": violations,
	}
	if len(warnings) > 0 {
		result["warnings"] = warnings
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
	includeLog, _ := req.Params.Arguments["include_log"].(bool)
	logLimit := 50
	if l, ok := req.Params.Arguments["log_limit"].(float64); ok && l > 0 {
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
		_ = s.store.LogViolations(violations)
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

	return jsonResult(map[string]interface{}{
		"status":  "ok",
		"rule_id": ruleID,
		"message": fmt.Sprintf("Rule %q is now active.", ruleID),
	})
}

// handleGetViolationLog returns the audit trail of violations detected by
// get_violations. Re-detecting the same violation increments its occurrence
// count rather than creating duplicate rows.
func (s *Server) handleGetViolationLog(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ruleID := stringArg(req, "rule_id")
	limit := 50
	if l, ok := req.Params.Arguments["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	if s.store == nil {
		return jsonResult(map[string]interface{}{"entries": []store.ViolationLogEntry{}, "total": 0})
	}
	entries, err := s.store.GetViolationLog(ruleID, limit)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if entries == nil {
		entries = []store.ViolationLogEntry{}
	}
	return jsonResult(map[string]interface{}{"entries": entries, "total": len(entries)})
}

// stringArg extracts a string argument from a CallToolRequest by key.
// Returns "" if the key is absent or not a string.
func stringArg(req mcp.CallToolRequest, key string) string {
	v, _ := req.Params.Arguments[key].(string)
	return v
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
	filePath, ok := req.Params.Arguments["file"].(string)
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

	sort.Slice(matches, func(i, j int) bool { return matches[i].Line < matches[j].Line })

	// Trim absolute paths for output.
	type fileEntity struct {
		Type     graph.NodeType    `json:"type"`
		Name     string            `json:"name"`
		Line     int               `json:"line"`
		Exported bool              `json:"exported"`
		Metadata map[string]string `json:"metadata,omitempty"`
	}
	out := make([]fileEntity, len(matches))
	for i, n := range matches {
		out[i] = fileEntity{
			Type:     n.Type,
			Name:     n.Name,
			Line:     n.Line,
			Exported: n.Exported,
			Metadata: n.Metadata,
		}
	}

	return jsonResult(map[string]interface{}{
		"file":     strings.TrimPrefix(matches[0].File, prefix),
		"package":  matches[0].Package,
		"count":    len(out),
		"entities": out,
	})
}

// handleSearch performs a keyword search across entity names and doc comments.
// Results are ranked: exact name > name prefix > name substring > doc match.
func (s *Server) handleSearch(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	query, ok := req.Params.Arguments["query"].(string)
	if !ok || query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	// mode=semantic delegates to FTS BM25 semantic search.
	if mode := stringArg(req, "mode"); mode == "semantic" {
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

// handleGetApiContract detects HTTP/gRPC API endpoints using framework
// conventions and optional api_entries config patterns. Returns each
// endpoint's signature, detected framework, direct callers (route
// registrations), and direct callees (service/repository dependencies).
func (s *Server) handleGetApiContract(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	pkgFilter := stringArg(req, "package")
	fileFilter := stringArg(req, "file")

	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	trimFile := func(p string) string { return strings.TrimPrefix(p, prefix) }

	type endpointRef struct {
		Name string `json:"name"`
		Type string `json:"type"`
		File string `json:"file"`
		Line int    `json:"line"`
	}
	type endpoint struct {
		Name      string        `json:"name"`
		Type      string        `json:"type"`
		File      string        `json:"file"`
		Line      int           `json:"line"`
		Signature string        `json:"signature,omitempty"`
		Doc       string        `json:"doc,omitempty"`
		Framework string        `json:"framework"`
		Callers   []endpointRef `json:"callers"`
		Callees   []endpointRef `json:"callees"`
	}

	var endpoints []endpoint
	for _, n := range s.graph.AllNodes() {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		if pkgFilter != "" && !strings.Contains(strings.ToLower(n.Package), strings.ToLower(pkgFilter)) {
			continue
		}
		if fileFilter != "" && !strings.HasSuffix(n.File, fileFilter) {
			continue
		}

		sig := n.Metadata["signature"]
		framework := detectFramework(sig, n.File, n.Type)

		// Config-override: apply user-defined api_entries patterns.
		if framework == "" {
			for _, pattern := range s.config.ApiEntries {
				if matchesApiEntry(n, pattern) {
					framework = "custom"
					break
				}
			}
		}
		if framework == "" {
			continue
		}

		// Direct callers (who calls/registers this endpoint), capped at 10.
		var callers []endpointRef
		for _, e := range s.graph.InEdges(n.ID) {
			if e.Type != graph.EdgeCalls {
				continue
			}
			if cn := s.graph.GetNode(e.From); cn != nil {
				callers = append(callers, endpointRef{
					Name: cn.Name, Type: string(cn.Type),
					File: trimFile(cn.File), Line: cn.Line,
				})
			}
			if len(callers) >= 10 {
				break
			}
		}
		if callers == nil {
			callers = []endpointRef{}
		}

		// Direct callees (what this endpoint calls — service/repo layer), capped at 10.
		var callees []endpointRef
		for _, e := range s.graph.OutEdges(n.ID) {
			if e.Type != graph.EdgeCalls {
				continue
			}
			if cn := s.graph.GetNode(e.To); cn != nil {
				callees = append(callees, endpointRef{
					Name: cn.Name, Type: string(cn.Type),
					File: trimFile(cn.File), Line: cn.Line,
				})
			}
			if len(callees) >= 10 {
				break
			}
		}
		if callees == nil {
			callees = []endpointRef{}
		}

		endpoints = append(endpoints, endpoint{
			Name:      n.Name,
			Type:      string(n.Type),
			File:      trimFile(n.File),
			Line:      n.Line,
			Signature: sig,
			Doc:       n.Metadata["doc"],
			Framework: framework,
			Callers:   callers,
			Callees:   callees,
		})
	}

	// Stable sort: file then line.
	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].File != endpoints[j].File {
			return endpoints[i].File < endpoints[j].File
		}
		return endpoints[i].Line < endpoints[j].Line
	})

	// Summary: count endpoints per framework.
	frameworks := make(map[string]int)
	for _, ep := range endpoints {
		frameworks[ep.Framework]++
	}

	return jsonResult(map[string]interface{}{
		"total":      len(endpoints),
		"frameworks": frameworks,
		"endpoints":  endpoints,
	})
}

// detectFramework identifies the HTTP/gRPC framework a function belongs to by
// inspecting its signature and file path. Returns "" if no convention matches.
func detectFramework(sig, file string, typ graph.NodeType) string {
	// Proto RPC: NodeMethod defined in a .proto file.
	if strings.HasSuffix(file, ".proto") && typ == graph.NodeMethod {
		return "grpc-proto"
	}
	// Go HTTP / gRPC frameworks — identified via parameter types in the signature.
	switch {
	case strings.Contains(sig, "http.ResponseWriter") && strings.Contains(sig, "http.Request"):
		return "net/http"
	case strings.Contains(sig, "*gin.Context"):
		return "gin"
	case strings.Contains(sig, "echo.Context"):
		return "echo"
	case strings.Contains(sig, "*fiber.Ctx"):
		return "fiber"
	case strings.Contains(sig, "*fasthttp.RequestCtx"):
		return "fasthttp"
	}
	// gRPC server method heuristic: (context.Context, *XxxRequest) (*XxxResponse, error)
	// Require both "Request" (param) and "Response" (return) to avoid false positives on
	// non-gRPC methods that happen to take context.Context and return error.
	if strings.Contains(sig, "context.Context") &&
		strings.Contains(sig, "Request") &&
		strings.Contains(sig, "Response") &&
		strings.Contains(sig, ", error)") {
		return "grpc"
	}
	return ""
}

// matchesApiEntry returns true if node n satisfies all non-empty fields in pattern.
// AND semantics: every specified field must match (same as ForbiddenEdge patterns).
func matchesApiEntry(n *graph.Node, pattern config.ApiEntryPattern) bool {
	if pattern.NodeType != "" && n.Type != pattern.NodeType {
		return false
	}
	if pattern.NamePattern != "" &&
		!strings.Contains(strings.ToLower(n.Name), strings.ToLower(pattern.NamePattern)) {
		return false
	}
	if pattern.FilePattern != "" {
		// Try base-name glob first; fall back to substring match for directory-style
		// patterns like "*/handlers/*" which filepath.Match cannot handle on basenames.
		matched, _ := filepath.Match(pattern.FilePattern, filepath.Base(n.File))
		if !matched {
			matched = strings.Contains(n.File, strings.Trim(pattern.FilePattern, "*"))
		}
		if !matched {
			return false
		}
	}
	return true
}

// handleGetCallChain finds the shortest call path between two entities using
// BFS over CALLS edges. Answers "how does A reach B?"
func (s *Server) handleGetCallChain(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	fromName, ok1 := req.Params.Arguments["from"].(string)
	toName, ok2 := req.Params.Arguments["to"].(string)
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
		return jsonResult(map[string]interface{}{
			"found":   false,
			"from":    fromName,
			"to":      toName,
			"message": "no call chain exists between these entities",
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

// handleFindOrphans returns unexported functions and methods that have no callers
// (fanin = 0) and are not Go runtime entry points. These are candidate dead code.
// Interface implementations may appear as false positives until IMPLEMENTS edges exist.
func (s *Server) handleFindOrphans(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	includeTests, _ := req.Params.Arguments["include_tests"].(bool)

	minConfidence := 0.5
	if v, ok := req.Params.Arguments["min_confidence"].(float64); ok && v >= 0 {
		minConfidence = v
	}

	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	// Build a set of struct IDs that implement at least one interface (IMPLEMENTS edges).
	// Methods on those structs are NOT dead even if fanin==0.
	interfaceImplementors := map[graph.NodeID]bool{} // struct NodeID
	for _, e := range s.graph.AllEdges() {
		if e.Type == graph.EdgeImplements {
			interfaceImplementors[e.From] = true
		}
	}

	// Build a set of method parent structs via name prefix (StructName.MethodName).
	implementorMethods := map[graph.NodeID]bool{}
	if len(interfaceImplementors) > 0 {
		for _, n := range s.graph.AllNodes() {
			if n.Type != graph.NodeMethod {
				continue
			}
			// Method node names are "StructType.MethodName" — extract the struct part.
			dotIdx := strings.Index(n.Name, ".")
			if dotIdx <= 0 {
				continue
			}
			structName := n.Name[:dotIdx]
			// Find matching struct nodes.
			for _, sn := range s.graph.FindByName(structName) {
				if sn.Type == graph.NodeStruct && interfaceImplementors[sn.ID] {
					implementorMethods[n.ID] = true
					break
				}
			}
		}
	}

	type orphan struct {
		Name       string  `json:"name"`
		Type       string  `json:"type"`
		File       string  `json:"file"`
		Line       int     `json:"line"`
		Signature  string  `json:"signature,omitempty"`
		Confidence float64 `json:"confidence"`
	}

	var orphans []orphan
	for _, n := range s.graph.AllNodes() {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		if n.Exported && strings.HasSuffix(n.File, ".go") {
			// Go exported symbols (capitalised) are part of the public API.
			// Python/TypeScript Exported=true only means "not private by convention"
			// and does NOT exclude a symbol from orphan detection.
			continue
		}
		isTestFile := strings.HasSuffix(n.File, "_test.go")
		if isTestFile && !includeTests {
			continue
		}
		switch n.Name {
		case "main", "init", "__init__", "__new__", "__del__":
			// Go: main/init are runtime entry points.
			// Python: __init__/__new__/__del__ are called via instantiation, not direct calls.
			continue
		}
		if isTestFile {
			if strings.HasPrefix(n.Name, "Test") || strings.HasPrefix(n.Name, "Benchmark") ||
				strings.HasPrefix(n.Name, "Example") || strings.HasPrefix(n.Name, "Fuzz") {
				continue
			}
		}
		// Only count incoming CALLS edges (not DEFINES edges, which are structural).
		// A function is an orphan only if no other code *calls* it.
		callsIn := 0
		for _, e := range s.graph.InEdges(n.ID) {
			if e.Type == graph.EdgeCalls {
				callsIn++
			}
		}
		if callsIn > 0 {
			continue
		}

		// P1.4: Framework-aware exemptions — skip interface implementors.
		if implementorMethods[n.ID] {
			continue
		}

		// P1.4: Skip common Go framework patterns even if fanin==0.
		// These are typically registered via callbacks, not direct calls.
		sig := n.Metadata["signature"]
		name := n.Name
		if isFrameworkEntrypoint(name, sig) {
			continue
		}

		// P1.4: Confidence scoring.
		// 1.0 = fully isolated (no edges at all)
		// 0.7 = has outgoing CALLS but no incoming
		// 0.5 = only DEFINES edges incoming (defined by a file node)
		fanout := s.graph.Fanout(n.ID)
		confidence := 1.0
		if fanout > 0 {
			confidence = 0.7
		} else {
			// Check if it has any DEFINES-only incoming edges.
			inEdges := s.graph.InEdges(n.ID)
			for _, e := range inEdges {
				if e.Type == graph.EdgeDefines {
					confidence = 0.5
					break
				}
			}
		}

		if confidence < minConfidence {
			continue
		}

		orphans = append(orphans, orphan{
			Name:       n.Name,
			Type:       string(n.Type),
			File:       strings.TrimPrefix(n.File, prefix),
			Line:       n.Line,
			Signature:  sig,
			Confidence: confidence,
		})
	}

	sort.Slice(orphans, func(i, j int) bool {
		if orphans[i].File != orphans[j].File {
			return orphans[i].File < orphans[j].File
		}
		return orphans[i].Line < orphans[j].Line
	})

	return jsonResult(map[string]interface{}{
		"count":   len(orphans),
		"orphans": orphans,
		"note":    "Go exported symbols, runtime entry points (main, init), interface implementors, and framework entry points are excluded. Python/TypeScript exported symbols are included. confidence: 1.0=fully isolated, 0.7=has outgoing calls, 0.5=only file-defines edge.",
	})
}

// isFrameworkEntrypoint returns true for Go function/method names and signatures
// that are typically registered via callbacks rather than direct calls.
func isFrameworkEntrypoint(name, sig string) bool {
	// net/http Handler interface: ServeHTTP(ResponseWriter, *Request)
	if name == "ServeHTTP" {
		return true
	}
	// Cobra command runners.
	if name == "RunE" || name == "Run" || name == "PersistentPreRunE" || name == "PreRunE" {
		return true
	}
	// Common hook names used by frameworks (gorilla mux, chi, fiber, gin).
	switch name {
	case "Handle", "Handler", "Middleware", "Next":
		return true
	}
	// gRPC service methods typically have (context.Context, *Request) (*Response, error) shape.
	if strings.Contains(sig, "context.Context") && strings.Contains(sig, "error)") &&
		strings.Contains(sig, "Request") && strings.Contains(sig, "Response") {
		return true
	}
	return false
}

// handleGetFederationStatus reports which linked projects are merged into the
// current graph and how many cross-project CALLS edges exist between them.
// Node IDs encode the repo prefix (repoID::path::name), so federation state
// can be derived purely from the in-memory graph with no extra bookkeeping.
func (s *Server) handleGetFederationStatus(
	_ context.Context,
	_ mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	primaryRepoID := s.graph.RepoID()

	// Count nodes per repo by parsing the repoID prefix of each NodeID.
	repoNodeCounts := make(map[string]int)
	for _, n := range s.graph.AllNodes() {
		if idx := strings.Index(string(n.ID), "::"); idx >= 0 {
			repoNodeCounts[string(n.ID)[:idx]]++
		}
	}

	// Count cross-project CALLS edges per linked repo.
	crossCalls := make(map[string]int)
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
		if fromRepo == toRepo {
			continue // intra-project edge
		}
		// Attribute the cross-project call to the non-primary repo.
		if fromRepo != primaryRepoID {
			crossCalls[fromRepo]++
		}
		if toRepo != primaryRepoID {
			crossCalls[toRepo]++
		}
	}

	type linkedProject struct {
		RepoID            string `json:"repo_id"`
		Nodes             int    `json:"nodes"`
		CrossProjectCalls int    `json:"cross_project_calls"`
	}

	var linked []linkedProject
	for repoID, count := range repoNodeCounts {
		if repoID == primaryRepoID {
			continue
		}
		linked = append(linked, linkedProject{
			RepoID:            repoID,
			Nodes:             count,
			CrossProjectCalls: crossCalls[repoID],
		})
	}
	sort.Slice(linked, func(i, j int) bool { return linked[i].RepoID < linked[j].RepoID })

	totalNodes := 0
	for _, c := range repoNodeCounts {
		totalNodes += c
	}

	return jsonResult(map[string]interface{}{
		"primary":       primaryRepoID,
		"is_federated":  len(linked) > 0,
		"primary_nodes": repoNodeCounts[primaryRepoID],
		"total_nodes":   totalNodes,
		"linked":        linked,
	})
}

// handleGetUsageGuide returns a project-specific guide for using Synapses tools.
// It combines a fixed tool catalogue with live graph data (entry points, key entities)
// so agents know how to navigate this project without reading external docs.
func (s *Server) handleGetUsageGuide(
	_ context.Context,
	_ mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	identity := s.graph.ProjectIdentity()

	type toolRef struct {
		Name      string `json:"name"`
		WhenToUse string `json:"when_to_use"`
	}

	// All MCP tools, grouped by purpose.
	toolsByCategory := map[string][]toolRef{
		"orientation": {
			{"get_project_identity", "Session start — overview of entry points, key entities, node counts, suggested rules"},
			{"get_pending_tasks", "Session start — resume work from prior sessions, ordered by priority"},
			{"get_my_tasks", "Session start (multi-agent) — your own unblocked tasks with a suggested next"},
			{"get_working_state", "Session start — see what files the developer just edited and current git diff"},
			{"get_usage_guide", "When lost — shows all tools, workflow, antipatterns, and this project's entry points"},
		},
		"discovery": {
			{"semantic_search", "Find code by concept ('rate limiting', 'JWT validation') — best for open-ended search"},
			{"search", "Find entities by name/keyword substring — faster than semantic_search when you know the name"},
			{"find_entity", "Resolve exact name + file + signature before calling get_context — use after search"},
			{"get_file_context", "List all entities in a specific file ordered by line — use when editing a known file"},
		},
		"exploration": {
			{"get_context", "BFS ego-subgraph: callers, callees, related nodes — the main structural exploration tool"},
			{"get_call_chain", "Trace the shortest call path from function A to function B through CALLS/IMPLEMENTS edges"},
			{"get_impact", "Blast-radius analysis — what breaks if entity X changes? Use before editing shared code"},
			{"get_api_contract", "List all public HTTP/gRPC endpoints with their callers and service dependencies"},
			{"get_communities", "Detect emergent module clusters via LPA — reveals hidden coupling across package lines"},
			{"export_graph", "Export subgraph or full graph as DOT/Mermaid/GraphML for visualization or documentation"},
			{"find_data_paths", "Trace data flow from sources (HTTP inputs) to sinks (SQL, file writes) — security analysis"},
		},
		"architecture": {
			{"validate_plan", "ALWAYS call before implementing — checks proposed changes against all architectural rules"},
			{"get_violations", "List current architectural rule violations in the graph"},
			{"upsert_rule", "Create or update a persistent architectural rule (e.g. no DB calls in handlers)"},
			{"get_violation_log", "Audit trail of violations — when they first appeared and how often"},
			{"get_change_coupling", "Find files that change together in git — surfaces implicit dependencies"},
			{"find_orphans", "Dead code detection — unexported functions with no callers; supports confidence threshold"},
			{"get_federation_status", "Multi-repo status — shows linked projects, node counts, cross-project edge counts"},
		},
		"coordination": {
			{"claim_work", "ALWAYS call before editing — reserves a file/package/entity scope, prevents conflicts"},
			{"get_conflicts", "Check if other agents have claimed scopes that overlap with your work"},
			{"annotate_node", "Leave a persistent note on a graph node visible to all agents (shared whiteboard)"},
			{"get_events", "Poll event log for other agents' activity since a given sequence number"},
			{"get_agents", "List all agents that have interacted with Synapses, ordered by last-seen time"},
			{"propose_change", "Propose an architectural change for other agents to vote on — use for cross-team decisions"},
			{"vote_on_proposal", "Cast approve/reject/abstain vote on an open proposal — auto-resolves at threshold"},
			{"withdraw_proposal", "Cancel your own open proposal if circumstances changed"},
			{"get_proposals", "List architectural change proposals, optionally filtered by status"},
			{"release_claims", "Release all your active work claims when done editing — frees scopes for other agents"},
		},
		"task_management": {
			{"create_plan", "Save an approved implementation plan with tasks so future sessions can resume"},
			{"get_pending_tasks", "List all pending/in-progress tasks ordered by priority"},
			{"update_task", "Mark a task in_progress BEFORE starting; mark done IMMEDIATELY after finishing"},
			{"get_plans", "Overview of all plans with task completion counts"},
			{"link_task_nodes", "Link a task to specific graph nodes — improves get_context relevance boost"},
		},
		"changes_metrics": {
			{"detect_changes", "Map a git diff to affected graph symbols — run after editing to see what changed"},
			{"get_working_state", "Recent file changes from the watcher + git diff stat (use at session start)"},
		},
		"peer_communication": {
			{"list_peers", "List configured peer synapses instances with connection status and shared entity counts"},
			{"get_peer_context", "Fetch context subgraph for an entity in a peer project (project+entity params)"},
			{"get_dependency_graph", "Inter-project dependency overview with Mermaid diagram of entity sharing"},
		},
	}

	// Top-5 key entities for the guide.
	top5 := identity.KeyEntities
	if len(top5) > 5 {
		top5 = top5[:5]
	}

	return jsonResult(map[string]interface{}{
		"project": identity.RepoID,
		"quick_start_sequence": []string{
			"1. get_project_identity        → orient yourself: entry points, summary, suggested rules",
			"2. get_pending_tasks           → resume prior session work (or get_my_tasks for your queue)",
			"3. get_working_state           → see what files the developer just edited",
			"4. semantic_search('concept')  → find relevant entities by concept or name",
			"5. find_entity('Name')         → resolve exact node before calling get_context",
			"6. get_context('Entity')       → structural context: callers, callees, related",
		},
		"implementation_workflow": []string{
			"1. claim_work(scope, agent_id)         → reserve the file/package before editing",
			"2. get_file_context(file) or find_entity → understand existing code structure",
			"3. get_impact(entity)                  → know what depends on what you'll change",
			"4. validate_plan([{file, adds_call_to}]) → confirm no architectural violations",
			"5. implement the changes",
			"6. detect_changes()                    → verify which graph symbols were touched",
			"7. update_task(id, 'done', notes)      → mark task complete with context for next session",
		},
		"session_discipline": []string{
			"Call update_task(id, 'in_progress') BEFORE starting each task",
			"Call update_task(id, 'done', notes) IMMEDIATELY after finishing — never batch completions",
			"Call claim_work(scope, agent_id) before editing any file — prevents multi-agent conflicts",
			"Pass your agent_id consistently so ownership and history are tracked across sessions",
			"Never mark a task done if tests are failing or implementation is partial",
			"Configure peers in synapses.json → list_peers → get_peer_context for cross-project context",
		},
		"antipatterns": []string{
			"Reading raw files with cat/grep instead of using find_entity + get_context + get_file_context",
			"Spinning up subagents for search instead of using semantic_search or search directly",
			"Editing files without calling claim_work first — causes silent conflicts with other agents",
			"Skipping validate_plan before implementing — leads to architectural violations",
			"Skipping detect_changes after editing — leaves task notes without verified scope",
			"Guessing entity names instead of calling find_entity or semantic_search",
			"Calling get_context without first resolving the entity via find_entity",
			"Batching task completions — mark each done the moment it is finished",
		},
		"when_to_ask_for_help": []string{
			"get_conflicts returns claims from other agents on your target scope",
			"get_violations returns errors that require a cross-team architectural decision",
			"validate_plan blocks your change and the rule can't be safely removed",
			"get_impact shows a very large blast radius (>20 nodes) for a change you plan",
			"Task depends_on another task that is stuck or unassigned",
		},
		"tools_by_category": toolsByCategory,
		"entry_points":      identity.EntryPoints,
		"key_entities":      top5,
	})
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
	if w, ok := req.Params.Arguments["window_minutes"].(float64); ok && w > 0 {
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

	result := map[string]interface{}{
		"window_minutes": windowMinutes,
		"recent_changes": events,
	}

	// Best-effort git diff stat — omitted when git is unavailable or not a git repo.
	if root := s.graph.Root(); root != "" {
		if out, err := exec.Command("git", "-C", root, "diff", "--stat", "HEAD").Output(); err == nil {
			if stat := strings.TrimSpace(string(out)); stat != "" {
				result["git_diff_stat"] = stat
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
		{Tool: "detect_changes", Reason: "map the git diff to affected graph symbols"},
		{Tool: "get_violations", Reason: "check if recent edits introduced architectural violations"},
		{Tool: "update_task", Reason: "mark in-progress tasks complete if work is finished"},
	}
	// If nodes were removed, dead code may now exist.
	removals := 0
	for _, e := range events {
		removals += e.NodesRemoved
	}
	if removals > 0 {
		suggestions = append(suggestions, toolSuggestion{
			Tool:   "find_orphans",
			Reason: fmt.Sprintf("%d nodes removed — check for dead code", removals),
		})
	}
	// If multiple files changed, cross-file coupling may be relevant.
	if len(events) > 2 {
		suggestions = append(suggestions, toolSuggestion{
			Tool:   "get_change_coupling",
			Reason: fmt.Sprintf("%d files changed — check which historically change together", len(events)),
		})
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

// handleFindDataPaths queries the DATA_FLOWS edges created by the dataflow
// analyzer to surface source-to-sink security paths in the codebase.
//   - No params  → list all detected sources and sinks
//   - source only → list sinks reachable from that entity
//   - sink only   → list sources that can reach that entity
//   - both        → confirm reachability and show hop count
func (s *Server) handleFindDataPaths(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	sourceName := stringArg(req, "source")
	sinkName := stringArg(req, "sink")

	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	type nodeRef struct {
		Name  string `json:"name"`
		Label string `json:"label"`
		File  string `json:"file"`
		Line  int    `json:"line"`
	}
	type pair struct {
		Source string `json:"source"`
		Sink   string `json:"sink"`
		Hops   int    `json:"hops,omitempty"`
	}

	// Collect all source and sink nodes from metadata.
	var allSources, allSinks []nodeRef
	sourceIDs := make(map[graph.NodeID]*graph.Node)
	sinkIDs := make(map[graph.NodeID]*graph.Node)

	for _, n := range s.graph.AllNodes() {
		if n.Metadata == nil {
			continue
		}
		role := n.Metadata["data_role"]
		if role == "" {
			continue
		}
		label := n.Metadata["data_label"]
		file := strings.TrimPrefix(n.File, prefix)
		ref := nodeRef{Name: n.Name, Label: label, File: file, Line: n.Line}
		if role == "source" {
			allSources = append(allSources, ref)
			sourceIDs[n.ID] = n
		} else {
			allSinks = append(allSinks, ref)
			sinkIDs[n.ID] = n
		}
	}

	// Collect DATA_FLOWS edges.
	var reachable []pair
	for _, e := range s.graph.AllEdges() {
		if e.Type != graph.EdgeDataFlows {
			continue
		}
		srcNode := s.graph.GetNode(e.From)
		snkNode := s.graph.GetNode(e.To)
		if srcNode == nil || snkNode == nil {
			continue
		}
		// Filter by source/sink name if provided.
		if sourceName != "" && !strings.EqualFold(srcNode.Name, sourceName) &&
			!strings.Contains(strings.ToLower(srcNode.Name), strings.ToLower(sourceName)) {
			continue
		}
		if sinkName != "" && !strings.EqualFold(snkNode.Name, sinkName) &&
			!strings.Contains(strings.ToLower(snkNode.Name), strings.ToLower(sinkName)) {
			continue
		}
		reachable = append(reachable, pair{Source: srcNode.Name, Sink: snkNode.Name})
	}

	// Filter source/sink lists when a name filter is active.
	if sourceName != "" {
		var filtered []nodeRef
		for _, r := range allSources {
			if strings.Contains(strings.ToLower(r.Name), strings.ToLower(sourceName)) {
				filtered = append(filtered, r)
			}
		}
		allSources = filtered
	}
	if sinkName != "" {
		var filtered []nodeRef
		for _, r := range allSinks {
			if strings.Contains(strings.ToLower(r.Name), strings.ToLower(sinkName)) {
				filtered = append(filtered, r)
			}
		}
		allSinks = filtered
	}

	srcCount := len(allSources)
	snkCount := len(allSinks)
	pathCount := len(reachable)

	summary := fmt.Sprintf("%d source(s), %d sink(s), %d data-flow path(s) detected",
		srcCount, snkCount, pathCount)
	if srcCount == 0 && snkCount == 0 {
		summary = "No sources or sinks detected. Run 'synapses index --reindex' if the graph is stale, " +
			"or add custom patterns via data_flow_sources/data_flow_sinks in synapses.json."
	}

	return jsonResult(map[string]interface{}{
		"sources":         allSources,
		"sinks":           allSinks,
		"reachable_pairs": reachable,
		"summary":         summary,
	})
}

// jsonResult marshals v to JSON and wraps it in a text tool result.
func jsonResult(v interface{}) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

// cloneGraph creates a shallow copy of g that shares the same nodes but
// handleGetChangeCoupling analyses git history to find files that frequently
// co-change, surfacing implicit dependencies that static analysis misses.
func (s *Server) handleGetChangeCoupling(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	root := s.graph.Root()
	if root == "" {
		return mcp.NewToolResultError("repo root is not set — index the project first"), nil
	}

	commitLimit := 500
	if v, ok := req.Params.Arguments["commit_limit"].(float64); ok && v > 0 {
		commitLimit = int(v)
	}

	minConfidence := 0.3
	if v, ok := req.Params.Arguments["min_confidence"].(float64); ok && v > 0 {
		minConfidence = v
	}

	pairs, err := git.AnalyzeChangeCoupling(root, commitLimit, minConfidence)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("change coupling: %v", err)), nil
	}

	return jsonResult(map[string]interface{}{
		"count": len(pairs),
		"pairs": pairs,
		"note":  "confidence = co_changes / max(total_commits_A, total_commits_B). Commits touching >50 files are skipped (merges/reformats). Only pairs with co_changes >= 3 are returned.",
	})
}

// has an independent edge set. Used to simulate plan additions without
// mutating the live graph.
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

// handleGetEvents returns events from the pull-based event log since a cursor seq.
// Agents pass the returned latest_seq back on the next call to receive only new events.
func (s *Server) handleGetEvents(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("event log unavailable: server started without a persistent store"), nil
	}

	var sinceSeq int64
	if v, ok := req.Params.Arguments["since_seq"].(float64); ok {
		sinceSeq = int64(v)
	}

	var types []string
	if raw, ok := req.Params.Arguments["types"].(string); ok && raw != "" {
		for _, t := range strings.Split(raw, ",") {
			if t = strings.TrimSpace(t); t != "" {
				types = append(types, t)
			}
		}
	}

	limit := 100
	if v, ok := req.Params.Arguments["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	events, latestSeq, err := s.store.GetEvents(sinceSeq, types, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get events: %v", err)), nil
	}

	return jsonResult(map[string]interface{}{
		"count":      len(events),
		"latest_seq": latestSeq,
		"events":     events,
		"hint":       "Pass latest_seq as since_seq on your next call to receive only new events.",
	})
}

// handleAnnotateNode attaches a note to a graph node, visible to all agents.
func (s *Server) handleAnnotateNode(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("annotations unavailable: server started without a persistent store"), nil
	}

	nodeID, _ := req.Params.Arguments["node_id"].(string)
	if nodeID == "" {
		return mcp.NewToolResultError("node_id is required"), nil
	}
	note, _ := req.Params.Arguments["note"].(string)
	if note == "" {
		return mcp.NewToolResultError("note is required"), nil
	}
	agentID, _ := req.Params.Arguments["agent_id"].(string)

	// Verify the node exists in the graph.
	if s.graph.GetNode(graph.NodeID(nodeID)) == nil {
		return mcp.NewToolResultError(fmt.Sprintf("node not found: %q", nodeID)), nil
	}

	id, err := s.store.AddAnnotation(nodeID, agentID, note)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("add annotation: %v", err)), nil
	}

	// Emit event.
	_ = s.store.AppendEvent("annotation_added", agentID,
		fmt.Sprintf(`{"annotation_id":%q,"node_id":%q}`, id, nodeID))

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
	symbol, _ := req.Params.Arguments["symbol"].(string)
	if symbol == "" {
		return mcp.NewToolResultError("symbol is required"), nil
	}

	maxDepth := 3
	if d, ok := req.Params.Arguments["depth"].(float64); ok && d > 0 {
		maxDepth = int(d)
		if maxDepth > 10 {
			maxDepth = 10
		}
	}

	// Resolve symbol name → node ID.
	candidates := s.graph.FindByName(symbol)
	if len(candidates) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("entity not found: %q", symbol)), nil
	}
	root := pickBestNode(candidates, s.graph)

	result, err := s.graph.ImpactAnalysis(root.ID, maxDepth)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("impact analysis: %v", err)), nil
	}

	return jsonResult(result)
}

// handleDetectChanges maps a git diff to affected graph symbols.
// Runs `git diff --unified=0 [ref]` against the repo root, parses the output,
// and returns all graph nodes whose file+line fall within changed ranges.
func (s *Server) handleDetectChanges(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ref, _ := req.Params.Arguments["ref"].(string)

	root := s.graph.Root()
	if root == "" {
		return mcp.NewToolResultError("repo root is not set — index the project first"), nil
	}

	// Build git diff command.
	args := []string{"-C", root, "diff", "--unified=0"}
	if ref != "" {
		args = append(args, ref)
	}
	args = append(args, "--")

	out, err := exec.Command("git", args...).Output()
	if err != nil {
		// git diff exits 1 on no-changes in some modes; treat empty output as no changes.
		if len(out) == 0 {
			return jsonResult(map[string]interface{}{
				"files_changed":    0,
				"symbols_affected": 0,
				"symbols":          []interface{}{},
				"summary":          "no changes detected",
			})
		}
		return mcp.NewToolResultError(fmt.Sprintf("git diff failed: %v", err)), nil
	}

	diffs := parseDiff(string(out))
	if len(diffs) == 0 {
		return jsonResult(map[string]interface{}{
			"files_changed":    0,
			"symbols_affected": 0,
			"symbols":          []interface{}{},
			"summary":          "no changes detected",
		})
	}

	// Map changed ranges to graph nodes.
	type changedSymbol struct {
		Name       string `json:"name"`
		Type       string `json:"type"`
		File       string `json:"file"`
		Line       int    `json:"line"`
		ChangeType string `json:"change_type"` // modified | added | deleted
	}

	allNodes := s.graph.AllNodes()
	var symbols []changedSymbol
	seen := map[graph.NodeID]struct{}{}

	for _, fd := range diffs {
		// Normalise file path to match how the graph stores it (relative).
		relFile := fd.file
		if strings.HasPrefix(relFile, root) {
			relFile = strings.TrimPrefix(relFile, root)
			relFile = strings.TrimPrefix(relFile, "/")
		}

		for _, n := range allNodes {
			nodeFile := n.File
			if strings.HasPrefix(nodeFile, root) {
				nodeFile = strings.TrimPrefix(nodeFile, root)
				nodeFile = strings.TrimPrefix(nodeFile, "/")
			}
			if nodeFile != relFile && !strings.HasSuffix(nodeFile, relFile) {
				continue
			}
			if _, already := seen[n.ID]; already {
				continue
			}
			// Check if this node's line falls within any changed range.
			for _, r := range fd.ranges {
				if n.Line >= r.start-5 && n.Line <= r.end+5 {
					seen[n.ID] = struct{}{}
					ct := "modified"
					if r.isAdd {
						ct = "added"
					} else if r.isDel {
						ct = "deleted"
					}
					symbols = append(symbols, changedSymbol{
						Name:       n.Name,
						Type:       string(n.Type),
						File:       n.File,
						Line:       n.Line,
						ChangeType: ct,
					})
					break
				}
			}
		}
	}

	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].File != symbols[j].File {
			return symbols[i].File < symbols[j].File
		}
		return symbols[i].Line < symbols[j].Line
	})

	return jsonResult(map[string]interface{}{
		"files_changed":    len(diffs),
		"symbols_affected": len(symbols),
		"symbols":          symbols,
		"summary":          fmt.Sprintf("%d file(s) changed, %d symbol(s) affected", len(diffs), len(symbols)),
	})
}

// fileDiff holds a file path and its changed line ranges from a unified diff.
type fileDiff struct {
	file   string
	ranges []lineRange
}

type lineRange struct {
	start int
	end   int
	isAdd bool
	isDel bool
}

// parseDiff parses unified diff output into per-file changed line ranges.
// Only the new-file (+) side ranges are tracked (hunk header: @@ -a,b +start,count @@).
func parseDiff(diff string) []fileDiff {
	var results []fileDiff
	var current *fileDiff

	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			// "diff --git a/path b/path" — extract b/ side.
			parts := strings.Fields(line)
			if len(parts) >= 4 {
				bFile := strings.TrimPrefix(parts[3], "b/")
				results = append(results, fileDiff{file: bFile})
				current = &results[len(results)-1]
			}
		case strings.HasPrefix(line, "+++ ") && current != nil:
			// "+++ b/path" — use this as the canonical file path.
			p := strings.TrimPrefix(line, "+++ ")
			p = strings.TrimPrefix(p, "b/")
			current.file = p

		case strings.HasPrefix(line, "@@ ") && current != nil:
			// "@@ -old_start,old_count +new_start,new_count @@"
			var oldStart, oldCount, newStart, newCount int
			// Parse both sides.
			_, _ = fmt.Sscanf(line, "@@ -%d,%d +%d,%d @@", &oldStart, &oldCount, &newStart, &newCount)
			if newStart == 0 && newCount == 0 {
				// Single-line form: @@ -a +b @@
				_, _ = fmt.Sscanf(line, "@@ -%d +%d @@", &oldStart, &newStart)
				newCount = 1
				oldCount = 1
			}
			if newCount == 0 {
				// Deletion hunk: lines deleted, new side empty.
				current.ranges = append(current.ranges, lineRange{
					start: oldStart,
					end:   oldStart + oldCount - 1,
					isDel: true,
				})
			} else {
				end := newStart + newCount - 1
				if end < newStart {
					end = newStart
				}
				isAdd := oldCount == 0
				current.ranges = append(current.ranges, lineRange{
					start: newStart,
					end:   end,
					isAdd: isAdd,
				})
			}
		}
	}

	// Remove entries with no ranges (e.g. binary files).
	filtered := results[:0]
	for _, fd := range results {
		if len(fd.ranges) > 0 {
			filtered = append(filtered, fd)
		}
	}
	return filtered
}

// handleSemanticSearch queries the FTS5 full-text index over node names,
// signatures, and doc comments. Results are ranked by BM25 relevance so
// concept-based queries ("find code that handles rate limiting") work without
// knowing exact function names. When embedding_endpoint is configured in
// synapses.json, future versions will re-rank using vector similarity.
func (s *Server) handleSemanticSearch(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("semantic_search requires a persistent store (run 'synapses start' or 'synapses index' first)"), nil
	}

	query := stringArg(req, "query")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	limitRaw, _ := req.Params.Arguments["limit"].(float64)
	limit := int(limitRaw)
	if limit <= 0 {
		limit = 20
	}

	results, err := s.store.SemanticSearch(query, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("semantic search: %v", err)), nil
	}

	embeddingNote := ""
	if s.config != nil && s.config.EmbeddingEndpoint != "" {
		embeddingNote = fmt.Sprintf("embedding_endpoint configured (%s) — vector re-ranking coming in a future version", s.config.EmbeddingEndpoint)
	}

	resp := map[string]interface{}{
		"query":   query,
		"count":   len(results),
		"results": results,
	}
	if embeddingNote != "" {
		resp["note"] = embeddingNote
	}
	if len(results) == 0 {
		resp["hint"] = "No FTS matches. Try broader terms, partial names, or use search() for exact substring matching."
	}

	return jsonResult(resp)
}

// handleClaimWork registers an agent's active work on a scope (file, package,
// or directory). Returns any conflicting claims from other agents so the caller
// can decide whether to proceed or coordinate first.
func (s *Server) handleClaimWork(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("work claims unavailable: server started without a persistent store"), nil
	}

	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}
	scope := stringArg(req, "scope")
	if scope == "" {
		return mcp.NewToolResultError("scope is required (file path, package name, or directory)"), nil
	}
	scopeType := stringArg(req, "scope_type")
	if scopeType == "" {
		scopeType = "file"
	}
	validTypes := map[string]bool{"file": true, "package": true, "directory": true, "entity": true}
	if !validTypes[scopeType] {
		return mcp.NewToolResultError(`scope_type must be one of: file, package, directory, entity`), nil
	}

	var ttlMinutes int
	if raw, ok := req.Params.Arguments["ttl_minutes"]; ok {
		switch v := raw.(type) {
		case float64:
			ttlMinutes = int(v)
		}
	}
	if ttlMinutes <= 0 {
		ttlMinutes = 30
	}

	s.upsertAgentIfNeeded(agentID)

	conflicts, err := s.store.ClaimWork(agentID, scope, scopeType, ttlMinutes)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("claim work: %v", err)), nil
	}

	// Notify connected peers about the new claim (fire-and-forget).
	if pm := s.getPeerManager(); pm != nil {
		go pm.BroadcastIntent(agentID, scope, scopeType)
	}

	// Brain coordination: get a concrete suggestion when conflicts exist.
	var coordinationSuggestion string
	if bc := s.getBrainClient(); bc != nil && len(conflicts) > 0 {
		var conflictClaims []brain.ClaimInput
		for _, c := range conflicts {
			conflictClaims = append(conflictClaims, brain.ClaimInput{
				AgentID:   c.AgentID,
				Scope:     c.Scope,
				ScopeType: c.ScopeType,
			})
		}
		coordinationSuggestion = bc.Coordinate(context.Background(), brain.CoordinateRequest{
			NewAgentID:        agentID,
			NewScope:          scope,
			ConflictingClaims: conflictClaims,
		})
	}

	resp := map[string]interface{}{
		"agent_id":    agentID,
		"scope":       scope,
		"scope_type":  scopeType,
		"ttl_minutes": ttlMinutes,
		"claimed":     true,
		"conflicts":   conflicts,
	}
	if len(conflicts) > 0 {
		resp["warning"] = fmt.Sprintf(
			"%d other agent(s) are actively working on overlapping scope. Consider coordinating before proceeding.",
			len(conflicts),
		)
		if coordinationSuggestion != "" {
			resp["coordination_suggestion"] = coordinationSuggestion
		}
	} else {
		resp["message"] = "Scope claimed. No conflicts detected."
	}
	return jsonResult(resp)
}

// handleGetConflicts returns all work claims by other agents that overlap with
// any scope the given agent currently holds. Also checks connected peers for
// cross-project conflicts.
func (s *Server) handleGetConflicts(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("work claims unavailable: server started without a persistent store"), nil
	}

	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}

	s.upsertAgentIfNeeded(agentID)

	myClaims, err := s.store.GetMyClaims(agentID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get claims: %v", err)), nil
	}

	conflicts, err := s.store.GetConflicts(agentID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get conflicts: %v", err)), nil
	}

	// Fetch claims from connected peers and find cross-project overlaps.
	crossConflicts := []peer.CrossProjectConflict{}
	if pm := s.getPeerManager(); pm != nil {
		peerClaims := pm.FetchAllPeerClaims(ctx)
		for peerName, pClaims := range peerClaims {
			for _, pc := range pClaims {
				for _, ac := range myClaims {
					if peer.ScopesOverlap(ac.Scope, pc.Scope) {
						crossConflicts = append(crossConflicts, peer.CrossProjectConflict{
							PeerName: peerName,
							Claim:    pc,
						})
					}
				}
			}
		}
	}

	summary := "no active claims"
	if len(myClaims) > 0 {
		summary = fmt.Sprintf("%d active claim(s)", len(myClaims))
	}
	conflictSummary := "no conflicts"
	total := len(conflicts) + len(crossConflicts)
	if total > 0 {
		conflictSummary = fmt.Sprintf("%d conflict(s) detected (%d local, %d cross-project)",
			total, len(conflicts), len(crossConflicts))
	}

	return jsonResult(map[string]interface{}{
		"agent_id":                agentID,
		"my_claims":               myClaims,
		"my_claims_summary":       summary,
		"conflicts":               conflicts,
		"cross_project_conflicts": crossConflicts,
		"conflicts_summary":       conflictSummary,
	})
}

// handleReleaseClaims releases all active work claims for the given agent.
func (s *Server) handleReleaseClaims(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("work claims unavailable: server started without a persistent store"), nil
	}

	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}

	if err := s.store.ReleaseClaims(agentID); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("release claims: %v", err)), nil
	}

	return jsonResult(map[string]interface{}{
		"agent_id": agentID,
		"message":  "All active claims released.",
	})
}

// handleGetCommunities runs Label Propagation on the graph and returns
// the discovered community clusters with modularity score and package-mix analysis.
func (s *Server) handleGetCommunities(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	maxIter := 10
	if v, ok := req.Params.Arguments["max_iterations"].(float64); ok && v > 0 {
		maxIter = int(v)
	}
	minSize := 2
	if v, ok := req.Params.Arguments["min_community_size"].(float64); ok && v > 0 {
		minSize = int(v)
	}
	includeNodes, _ := req.Params.Arguments["include_nodes"].(bool)

	result, err := s.graph.DetectCommunities(maxIter, minSize)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Trim absolute file paths to repo-relative paths.
	root := s.graph.Root()
	prefix := ""
	if root != "" {
		prefix = root + "/"
	}

	if includeNodes {
		if prefix != "" {
			for i := range result.Communities {
				for j := range result.Communities[i].Nodes {
					result.Communities[i].Nodes[j].File =
						strings.TrimPrefix(result.Communities[i].Nodes[j].File, prefix)
				}
			}
		}
		return jsonResult(result)
	}

	// Default: strip node lists to keep output compact. Agents can drill into
	// a specific community by calling get_file_context or get_context instead.
	type communitySlim struct {
		ID              int      `json:"id"`
		Size            int      `json:"size"`
		DominantPackage string   `json:"dominant_package"`
		PackageMix      []string `json:"package_mix"`
		IsMixed         bool     `json:"is_mixed"`
	}
	slim := make([]communitySlim, len(result.Communities))
	for i, c := range result.Communities {
		slim[i] = communitySlim{
			ID:              c.ID,
			Size:            c.Size,
			DominantPackage: c.DominantPackage,
			PackageMix:      c.PackageMix,
			IsMixed:         c.IsMixed,
		}
	}

	return jsonResult(map[string]interface{}{
		"algorithm":       result.Algorithm,
		"modularity":      result.Modularity,
		"iteration_count": result.IterationCount,
		"community_count": result.CommunityCount,
		"communities":     slim,
		"tip":             "Pass include_nodes:true to include member node lists per community.",
	})
}
