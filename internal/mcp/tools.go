package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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
type directionalContext struct {
	Root               *graph.Node                   `json:"root"`
	Annotations        map[string][]store.Annotation `json:"annotations,omitempty"`          // node_id → []Annotation — surfaced first for multi-agent visibility
	Callees            []graph.CarvedNode            `json:"callees"`                        // root --CALLS--> node
	Callers            []graph.CarvedNode            `json:"callers"`                        // node --CALLS--> root
	Related            []graph.CarvedNode            `json:"related"`                        // everything else
	ContextPacket      *brain.ContextPacket          `json:"context_packet,omitempty"`       // LLM-enriched packet (present when brain is available)
	SuggestedNextTools []toolSuggestion              `json:"suggested_next_tools,omitempty"` // context-aware next steps
	Truncated          bool                          `json:"truncated,omitempty"`            // true when token budget cut results
	TruncatedCount     int                           `json:"truncated_count,omitempty"`      // nodes dropped by budget
	BrainHint          string                        `json:"brain,omitempty"`                // set when brain is not configured
	Principles         []string                      `json:"principles,omitempty"`           // Hot Constitution principles from synapses.json
	ADRs               []brain.ADR                   `json:"adrs,omitempty"`                 // relevant accepted ADRs for this entity's file
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

	// Optional file hint — narrows lookup to a specific file when entity names are ambiguous.
	fileHint, _ := req.Params.Arguments["file"].(string)

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
		dc.ContextPacket = pkt // nil if brain unavailable — callers check for nil
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
		}
	}

	// Context-aware next-step suggestions.
	dc.SuggestedNextTools = suggestNextAfterContext(dc)

	// Hot Constitution: inject project principles if configured.
	if s.config != nil && s.config.Constitution.InjectInContext && len(s.config.Constitution.Principles) > 0 {
		dc.Principles = s.config.Constitution.Principles
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

	// format=compact returns a natural-language briefing instead of the default JSON blob.
	// detail_level controls depth: "summary" (~50t), "neighbors" (~200t), "full" (~400-600t, default).
	format, _ := req.Params.Arguments["format"].(string)
	if format == "compact" {
		detailLevel, _ := req.Params.Arguments["detail_level"].(string)
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

	if len(fileSet) == 1 {
		// Single file — keep existing flat format.
		out := make([]fileEntity, len(matches))
		for i, n := range matches {
			out[i] = fileEntity{Type: n.Type, Name: n.Name, Line: n.Line, Exported: n.Exported, Metadata: n.Metadata}
		}
		return jsonResult(map[string]interface{}{
			"file":     strings.TrimPrefix(matches[0].File, prefix),
			"package":  matches[0].Package,
			"count":    len(out),
			"entities": out,
		})
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
	return jsonResult(map[string]interface{}{
		"files_matched":    len(fileSet),
		"total_count":      len(matches),
		"entities_by_file": byFile,
		"hint":             fmt.Sprintf("%d files named %q found. Use file= param with a longer path suffix to pin to one file.", len(fileSet), filePath),
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
	parts := strings.SplitN(strings.TrimLeft(filePath, "/"), "/", 2)
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

// handleSessionInit is the single-call session bootstrap that replaces the
// three-step startup ritual (get_pending_tasks → get_project_identity →
// get_working_state). One MCP round-trip returns all the context an agent
// needs to start work, including scale-aware tool guidance and recent events.
func (s *Server) handleSessionInit(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	agentID, _ := req.Params.Arguments["agent_id"].(string)
	s.upsertAgentIfNeeded(agentID)

	// ── 1. Project identity + scale guidance ─────────────────────────────
	identity := s.graph.ProjectIdentity()

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

	projectSection := map[string]interface{}{
		"identity": identity,
		"federation": map[string]interface{}{
			"is_federated":        len(linkedRepos) > 0,
			"linked_repos":        linkedRepos,
			"cross_project_edges": crossCallCount,
		},
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

	// ── 4. Recent agent events (last 20) ──────────────────────────────────
	var recentEvents []store.Event
	var latestEventSeq int64
	if s.store != nil {
		events, seq, err := s.store.GetEvents(0, nil, 20)
		if err == nil {
			recentEvents = events
			latestEventSeq = seq
		}
	}
	if recentEvents == nil {
		recentEvents = []store.Event{}
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

	// ── 5. Constitution (Hot Constitution — project principles) ───────────
	if s.config != nil && s.config.Constitution.InjectInSessionInit && len(s.config.Constitution.Principles) > 0 {
		resp["constitution"] = map[string]interface{}{
			"principles": s.config.Constitution.Principles,
			"count":      len(s.config.Constitution.Principles),
			"note":       "These project laws apply to all work in this session.",
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
	if result.Tiers == nil {
		result.Tiers = []graph.ImpactTier{}
	}

	return jsonResult(result)
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
