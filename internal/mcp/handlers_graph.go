package mcp

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
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
	// CrossRepoCalls iterates the internal graph edge map directly, avoiding
	// the ~500 KB slice allocation that AllEdges() would produce on a large graph.
	primaryRepoID := s.graph.RepoID()
	crossCallCount, linkedRepos := s.graph.CrossRepoCalls(primaryRepoID)

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

// handleFindEntity returns all nodes whose name matches the query string.
// Default format is "compact" (one line per match: "Name · type · file:line").
// Pass format="json" for the full structured response.
func (s *Server) handleFindEntity(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	start := time.Now()

	query, ok := req.GetArguments()["query"].(string)
	if !ok || query == "" {
		return mcp.NewToolResultError("query is required (e.g., 'AuthService', 'handleLogin')"), nil
	}
	format, _ := req.GetArguments()["format"].(string)
	if format == "" {
		format = "compact"
	}
	// Optional: search sibling projects when projects= is specified.
	projectsRaw, _ := req.GetArguments()["projects"].(string)

	// Exact match first, then substring.
	nodes := s.graph.FindByName(query)
	if len(nodes) == 0 {
		nodes = s.graph.FindByPatternLimit(query, 50)
	}
	// Dotted method name fallback: "Store.Close" → search "Close", filter by "Store".
	// Go method nodes are stored by their short name (e.g. "Close") without the
	// receiver type prefix, so "Store.Close" matches nothing via substring.
	if len(nodes) == 0 && strings.Contains(query, ".") {
		parts := strings.SplitN(query, ".", 2)
		prefix, method := strings.ToLower(parts[0]), parts[1]
		candidates := s.graph.FindByName(method)
		if len(candidates) == 0 {
			candidates = s.graph.FindByPatternLimit(method, 50)
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
		Callers   int            `json:"callers,omitempty"`
		Callees   int            `json:"callees,omitempty"`
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
			if m.Doc == "" {
				m.Doc = n.Metadata["description"] // knowledge nodes use "description"
			}
			m.Signature = n.Metadata["signature"]
		}
		m.Callers = s.graph.Fanin(n.ID)
		m.Callees = s.graph.Fanout(n.ID)
		results = append(results, m)
	}

	// Sort results: code nodes (function/method/struct/interface) before doc
	// sections, then non-test before test, then by path depth (shorter = closer
	// to root). This ensures code definitions appear first when both a markdown
	// section and a Go function share the same name.
	nodeTypeTier := func(t graph.NodeType) int {
		switch t {
		case graph.NodeFunction, graph.NodeMethod:
			return 1
		case graph.NodeStruct, graph.NodeInterface:
			return 2
		case graph.NodeRoute, graph.NodeVariable:
			return 3
		case graph.NodeFile, graph.NodePackage:
			return 4
		case graph.NodeConcept, graph.NodeEntity, graph.NodeArtifact, graph.NodeDecision:
			return 5 // knowledge nodes: after code, before sections
		default: // NodeSection and anything else
			return 6
		}
	}
	sort.Slice(results, func(i, j int) bool {
		ti := nodeTypeTier(results[i].Type)
		tj := nodeTypeTier(results[j].Type)
		if ti != tj {
			return ti < tj // lower tier number wins (code before docs)
		}
		// Same type tier: non-test before test.
		iTest := isTestFile(results[i].File)
		jTest := isTestFile(results[j].File)
		if iTest != jTest {
			return !iTest
		}
		// Same test-ness: prefer shorter file path (closer to project root).
		if len(results[i].File) != len(results[j].File) {
			return len(results[i].File) < len(results[j].File)
		}
		return results[i].File < results[j].File
	})

	// Federation search: when projects= is specified and federation is configured,
	// search sibling stores for matching entities. Results are appended with
	// an [alias] prefix to distinguish them from local matches.
	var fedResults []federation.FederatedSearchResult
	if projectsRaw != "" && s.federationResolver != nil {
		var aliases []string
		if projectsRaw != "*" {
			for _, a := range strings.Split(projectsRaw, ",") {
				if a = strings.TrimSpace(a); a != "" {
					aliases = append(aliases, a)
				}
			}
		}
		fedCtx, fedCancel := context.WithTimeout(ctx, 2*time.Second)
		fedResults = s.federationResolver.FindEntities(fedCtx, query, aliases, 20)
		fedCancel()
	}

	if pc := s.getPulseClient(); pc != nil {
		agentID, _ := req.GetArguments()["agent_id"].(string)
		if agentID == "" {
			agentID = s.getLastAgent()
		}
		pulseSessID := s.getSynapseSessionID(SessionIDFromContext(ctx))
		count := len(results)
		durationMs := time.Since(start).Milliseconds()
		s.goBackground(func() {
			pc.RecordSearchEvent(pulse.SearchEvent{
				AgentID:     agentID,
				ProjectID:   s.projectID,
				Query:       query,
				Mode:        "exact",
				ResultCount: count,
				DurationMs:  durationMs,
				SessionID:   pulseSessID,
			})
		})
	}

	if format == "compact" {
		var sb strings.Builder
		if len(results) == 0 && len(fedResults) == 0 {
			fmt.Fprintf(&sb, "No matches for %q.\nHint: try search(query=%q, mode=\"semantic\") for concept-based lookup, or get_file_context(file=\"...\") for a specific file.", query, query)
			return mcp.NewToolResultText(sb.String()), nil
		}
		localCount := len(results)
		fedCount := 0
		for _, fr := range fedResults {
			fedCount += len(fr.Results)
		}
		fmt.Fprintf(&sb, "%d match(es) for %q:\n", localCount+fedCount, query)
		for _, r := range results {
			testMark := ""
			if isTestFile(r.File) {
				testMark = " (test)"
			}
			if r.Callers > 0 || r.Callees > 0 {
				fmt.Fprintf(&sb, "  [%s] %s · %s:%d%s · %d callers, %d callees\n", r.Name, r.Type, r.File, r.Line, testMark, r.Callers, r.Callees)
			} else {
				fmt.Fprintf(&sb, "  [%s] %s · %s:%d%s\n", r.Name, r.Type, r.File, r.Line, testMark)
			}
		}
		// Federation results (compact format).
		for _, fr := range fedResults {
			for _, r := range fr.Results {
				fmt.Fprintf(&sb, "  [%s] [%s] %s\n", r.Name, fr.Alias, r.ID)
			}
		}
		// Context-aware footer: single match → exact call; multiple → disambiguation hint.
		totalResults := localCount + fedCount
		if totalResults == 1 && localCount == 1 {
			fmt.Fprintf(&sb, "Call get_context(entity=%q) to explore.", results[0].Name)
		} else {
			sb.WriteString("Call get_context(entity=\"Name\", file=\"path/suffix\") to pin to a specific result.")
		}
		return mcp.NewToolResultText(sb.String()), nil
	}

	result := map[string]interface{}{
		"query":   query,
		"count":   len(results),
		"matches": results,
	}
	if len(fedResults) > 0 {
		result["federated"] = fedResults
	}
	if len(results) == 0 && len(fedResults) == 0 {
		result["hint"] = "No exact or substring match. Try search(mode=semantic) for concept-based lookup, or search(mode=fulltext) for broader text search."
	}
	return jsonResult(result)
}


// REMOVED: toolCatalog, workflowRecipes, handleDiscoverTools, splitAlpha, dotProduct, init()
// were removed as dormant code — discover_tools was unregistered in Sprint 24.
// Sprint 23.9: handleGetFileContext removed — agent reads files directly.
// The 8 active MCP tools are: session_init, get_context, validate, search,
// get_impact, tasks, end_session, memory.

// handleSearch performs a keyword search across entity names and doc comments.
// Results are ranked: exact name > name prefix > name substring > doc match.
func (s *Server) handleSearch(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	query, ok := req.GetArguments()["query"].(string)
	if !ok || query == "" {
		return mcp.NewToolResultError("query is required (e.g., 'auth caching', 'UserService login flow')"), nil
	}

	// R29: track repeated searches for the same query as a confusion signal.
	if agentIDSrch, _ := req.GetArguments()["agent_id"].(string); agentIDSrch != "" && s.store != nil {
		s.trackContextCall(agentIDSrch, "search:"+query)
	}

	// mode=semantic → HyDE-enhanced vector search (brain generates a hypothesis,
	// we embed the hypothesis and search HNSW against it). Falls back to raw
	// query embedding when brain is unavailable or the 500 ms timeout fires.
	// mode=fulltext → FTS5 BM25 full-text search (no HyDE, no vector path).
	if mode := stringArg(req, "mode"); mode == "semantic" || mode == "fulltext" {
		return s.handleSemanticSearch(ctx, req)
	}

	start := time.Now()

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
	const maxResults = 25
	highScoreCount := 0 // tracks hits with score >= 20 (exact/prefix)

	// O(N) scan with early termination: once we have maxResults exact/prefix
	// matches (score ≥ 20), lower-scored hits can never displace them, so we
	// stop scanning. On typical queries this terminates after a small fraction
	// of the graph is scanned.
	for _, n := range s.graph.AllNodes() {
		if n.Type == graph.NodeFile || n.Type == graph.NodePackage {
			continue
		}
		nameLow := strings.ToLower(n.Name)
		score := 0
		switch {
		case nameLow == lower:
			score = 30
		case strings.HasPrefix(nameLow, lower):
			score = 20
		case strings.Contains(nameLow, lower):
			score = 10
		default:
			// Skip expensive file-path, doc, and multi-word checks if we
			// already have enough high-quality hits to fill the results.
			if highScoreCount >= maxResults {
				continue
			}
			// Score 8: file path suffix match — lets agents search by package name
			// (e.g. "watcher" matches all nodes in internal/watcher/*.go).
			fileLow := strings.ToLower(n.File)
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
		if score == 0 && highScoreCount < maxResults {
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
			if score >= 20 {
				highScoreCount++
				// Early termination: enough exact/prefix matches found —
				// no lower-scored hit can displace these after sorting.
				if highScoreCount >= maxResults { //nolint:staticcheck // intentional empty branch: scanning continues for cheap name-based matches
				}
			}
		}
	}

	// Hybrid boost: when an embedder is available, run a parallel vector search
	// and inject high-scoring semantic matches that the lexical scan missed.
	// This catches NL-to-code matches (e.g., "auth handler" → validateCredentials)
	// without slowing down the common case (exact name queries still resolve via
	// the lexical scan above). Vector results that already appear in lexical hits
	// are skipped (dedup by node ID).
	if s.embedClient != nil && s.store != nil {
		embedCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		queryVec, embedErr := s.embedClient.Embed(embedCtx, query)
		cancel()
		if embedErr == nil && len(queryVec) > 0 {
			vr, verr := s.store.VectorSearch(queryVec, 10)
			if verr == nil {
				// Build dedup set from lexical hits.
				lexicalIDs := make(map[graph.NodeID]bool, len(hits))
				for _, h := range hits {
					lexicalIDs[graph.NodeID(h.node.ID)] = true
				}
				for _, sr := range vr {
					nid := graph.NodeID(sr.ID)
					if lexicalIDs[nid] {
						continue // already in lexical results
					}
					n := s.graph.GetNode(nid)
					if n == nil {
						continue
					}
					// Score 7: semantic match — above doc-only (5) but below
					// name-contains (10). High-similarity vectors get +2 boost.
					score := 7
					if sr.Score > 0.7 {
						score = 9
					}
					hits = append(hits, hit{n, score})
				}
			}
		}
	}

	// Deprioritize vendored/external/generated nodes so user-authored code
	// ranks higher. Without this, bundled third-party libraries (e.g.
	// vendor/configobj.py) can dominate results for common terms.
	// Also deprioritize doc section nodes: sections are supplemental context
	// and should not crowd out code entities in search results. An exact name
	// match on a section still ranks high; only weak matches are penalized.
	for i := range hits {
		switch hits[i].node.Provenance {
		case graph.ProvenanceVendored:
			hits[i].score -= 5
		case graph.ProvenanceExternal:
			hits[i].score -= 8
		case graph.ProvenanceGenerated:
			hits[i].score -= 3
		}
		// Doc sections: penalize weak matches (score < 10) so code entities
		// dominate. Exact/prefix name matches (score >= 10) keep their rank
		// because they are genuinely relevant.
		if hits[i].node.Type == graph.NodeSection && hits[i].score < 10 {
			hits[i].score = 1
		}
		if hits[i].score < 1 {
			hits[i].score = 1
		}
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].node.Name < hits[j].node.Name
	})

	// Cap results to control token budget. 8 results is sufficient for
	// agents to find the right entity; more adds noise without value.
	const resultCap = 8
	if len(hits) > resultCap {
		hits = hits[:resultCap]
	}

	type result struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		File      string `json:"file"`
		Line      int    `json:"line"`
		EndLine   int    `json:"end_line,omitempty"`
		Doc       string `json:"doc,omitempty"`
		Signature string `json:"signature,omitempty"`
		Source    string `json:"source,omitempty"`
	}

	// Source snippet injection: read entity bodies inline so the LLM
	// doesn't need a separate Read call. Cap at 15 lines per snippet.
	const snippetLines = 15
	// Resolve project root for source snippet extraction.
	repoRoot := s.graph.Root()
	if repoRoot == "" {
		repoRoot = s.projectPath
	}
	if repoRoot == "" {
		repoRoot = s.getProjectRoot()
	}
	srcCache := newSourceCache(repoRoot)

	// Build per-file node lists for end-line computation.
	fileNodes := make(map[string][]*graph.Node)
	for _, h := range hits {
		rel := strings.TrimPrefix(h.node.File, prefix)
		fileNodes[rel] = append(fileNodes[rel], h.node)
	}
	// Add neighboring nodes from the graph for better end-line estimates.
	for file := range fileNodes {
		for _, n := range s.graph.FindByFile(file) {
			fileNodes[file] = append(fileNodes[file], n)
		}
		sort.Slice(fileNodes[file], func(i, j int) bool {
			return fileNodes[file][i].Line < fileNodes[file][j].Line
		})
	}

	results := make([]result, len(hits))
	for i, h := range hits {
		rel := strings.TrimPrefix(h.node.File, prefix)

		// Compute end line from neighboring nodes.
		lineCount, _ := strconv.Atoi(h.node.Metadata["line_count"])
		nextStart := 0
		for _, fn := range fileNodes[rel] {
			if fn.Line > h.node.Line {
				nextStart = fn.Line
				break
			}
		}
		fileTotal := srcCache.TotalLines(rel)
		endLine := computeEndLine(h.node.Line, nextStart, lineCount, fileTotal)

		// Cap snippet at snippetLines.
		snippetEnd := endLine
		if snippetEnd-h.node.Line+1 > snippetLines {
			snippetEnd = h.node.Line + snippetLines - 1
		}

		results[i] = result{
			Type:      string(h.node.Type),
			Name:      h.node.Name,
			File:      rel,
			Line:      h.node.Line,
			EndLine:   endLine,
			Doc:       h.node.Metadata["doc"],
			Signature: h.node.Metadata["signature"],
			Source:    srcCache.Extract(rel, h.node.Line, snippetEnd),
		}
	}

	if pc := s.getPulseClient(); pc != nil {
		agentID, _ := req.GetArguments()["agent_id"].(string)
		if agentID == "" {
			agentID = s.getLastAgent()
		}
		pulseSessID := s.getSynapseSessionID(SessionIDFromContext(ctx))
		count := len(results)
		durationMs := time.Since(start).Milliseconds()
		s.goBackground(func() {
			pc.RecordSearchEvent(pulse.SearchEvent{
				AgentID:     agentID,
				ProjectID:   s.projectID,
				Query:       query,
				Mode:        "exact",
				ResultCount: count,
				DurationMs:  durationMs,
				SessionID:   pulseSessID,
			})
		})
	}

	return jsonResult(map[string]interface{}{
		"query":    query,
		"count":    len(results),
		"results":  results,
		"_summary": fmt.Sprintf("%d result(s) for %q", len(results), query),
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

	// resolveAll returns all candidate nodes for a name (exact + pattern, deduped).
	resolveAll := func(name string) []*graph.Node {
		exact := s.graph.FindByName(name)
		pattern := s.graph.FindByPatternLimit(name, 50)
		seen := make(map[graph.NodeID]bool, len(exact)+len(pattern))
		combined := make([]*graph.Node, 0, len(exact)+len(pattern))
		for _, n := range exact {
			if !seen[n.ID] {
				seen[n.ID] = true
				combined = append(combined, n)
			}
		}
		for _, n := range pattern {
			if !seen[n.ID] {
				seen[n.ID] = true
				combined = append(combined, n)
			}
		}
		return combined
	}

	fromCandidates := resolveAll(fromName)
	if len(fromCandidates) == 0 {
		return jsonResult(map[string]interface{}{
			"error": fmt.Sprintf("entity not found: %q", fromName),
			"hint":  "Use find_entity or semantic_search to discover the correct entity name.",
		})
	}
	toCandidates := resolveAll(toName)
	if len(toCandidates) == 0 {
		return jsonResult(map[string]interface{}{
			"error": fmt.Sprintf("entity not found: %q", toName),
			"hint":  "Use find_entity or semantic_search to discover the correct entity name.",
		})
	}
	// Best single node for display/warnings (pick by quality heuristic).
	fromNode := pickBestNode(fromCandidates, s.graph, fromName)
	toNode := pickBestNode(toCandidates, s.graph, toName)

	// Warn when resolution picked a doc section instead of a code node — the
	// caller probably meant the function/handler, not the documentation heading.
	var resolveWarnings []string
	if fromNode.Type == graph.NodeSection {
		resolveWarnings = append(resolveWarnings, fmt.Sprintf(
			"from=%q resolved to a documentation section (type=section, file=%s). "+
				"If you meant a code function, use the full node ID from find_entity().",
			fromName, fromNode.File))
	}
	if toNode.Type == graph.NodeSection {
		resolveWarnings = append(resolveWarnings, fmt.Sprintf(
			"to=%q resolved to a documentation section (type=section, file=%s). "+
				"If you meant a code function, use the full node ID from find_entity().",
			toName, toNode.File))
	}

	// Check if any from-candidate overlaps with any to-candidate (trivial path).
	toIDSet := make(map[graph.NodeID]bool, len(toCandidates))
	for _, tc := range toCandidates {
		toIDSet[tc.ID] = true
	}
	sameNode := fromNode.ID == toNode.ID
	if !sameNode {
		for _, fc := range fromCandidates {
			if toIDSet[fc.ID] {
				sameNode = true
				break
			}
		}
	}
	if sameNode {
		resp := map[string]interface{}{
			"found": true,
			"chain": []string{fromNode.Name},
		}
		if len(resolveWarnings) > 0 {
			resp["resolve_warnings"] = resolveWarnings
		}
		return jsonResult(resp)
	}

	// Sprint 28: Bidirectional BFS — search from both endpoints simultaneously.
	// This doubles effective reach within the same hop limit and finds paths
	// that unidirectional BFS misses when the graph is sparse or asymmetric.
	//
	// Forward BFS from `from` follows outgoing CALLS/HANDLES/IMPLEMENTS edges.
	// Backward BFS from `to` follows incoming CALLS/HANDLES edges (reverse callers).
	// Both directions follow IMPLEMENTS bidirectionally.

	type bfsEntry struct {
		id  graph.NodeID
		hop int
	}
	const maxBFSHops = 15 // per-direction (30 total reach with bidirectional)

	// Forward search state (from→to). Seed with all from-candidates so that
	// any alias/overload of the from-entity is a valid starting point.
	fwdPrev := make(map[graph.NodeID]graph.NodeID, len(fromCandidates))
	fwdQueue := make([]bfsEntry, 0, len(fromCandidates))
	for _, fc := range fromCandidates {
		if _, ok := fwdPrev[fc.ID]; !ok {
			fwdPrev[fc.ID] = ""
			fwdQueue = append(fwdQueue, bfsEntry{fc.ID, 0})
		}
	}
	// Backward search state (to→from). Seed with all to-candidates so that
	// whichever to-node the forward BFS reaches first counts as a match.
	bwdPrev := make(map[graph.NodeID]graph.NodeID, len(toCandidates))
	bwdQueue := make([]bfsEntry, 0, len(toCandidates))
	for _, tc := range toCandidates {
		if _, ok := bwdPrev[tc.ID]; !ok {
			bwdPrev[tc.ID] = ""
			bwdQueue = append(bwdQueue, bfsEntry{tc.ID, 0})
		}
	}

	viaImpl := make(map[graph.NodeID]bool)
	viaHandles := make(map[graph.NodeID]bool)

	var meetNode graph.NodeID   // node where forward and backward searches meet
	var metToNode graph.NodeID  // which to-candidate was actually reached
	found := false
	var closestReachableID graph.NodeID
	maxHop := 0

	// isCallChainEdge returns true for edge types valid in call chain traversal.
	isCallChainEdge := func(et graph.EdgeType) bool {
		return et == graph.EdgeCalls || et == graph.EdgeImplements || et == graph.EdgeHandles
	}

	// expandForward processes one level of forward BFS.
	expandForward := func() {
		if len(fwdQueue) == 0 || found {
			return
		}
		curr := fwdQueue[0]
		fwdQueue = fwdQueue[1:]
		if curr.hop >= maxBFSHops {
			return
		}
		if curr.hop > maxHop {
			maxHop = curr.hop
			closestReachableID = curr.id
		}
		// Outgoing: CALLS, HANDLES, IMPLEMENTS.
		for _, e := range s.graph.OutEdges(curr.id) {
			if !isCallChainEdge(e.Type) {
				continue
			}
			if _, visited := fwdPrev[e.To]; visited {
				continue
			}
			fwdPrev[e.To] = curr.id
			if e.Type == graph.EdgeImplements {
				viaImpl[e.To] = true
			}
			if e.Type == graph.EdgeHandles {
				viaHandles[e.To] = true
			}
			if _, inBwd := bwdPrev[e.To]; inBwd {
				meetNode = e.To
				found = true
				return
			}
			fwdQueue = append(fwdQueue, bfsEntry{e.To, curr.hop + 1})
		}
		// Backward IMPLEMENTS (interface → concrete).
		for _, e := range s.graph.InEdges(curr.id) {
			if e.Type != graph.EdgeImplements {
				continue
			}
			if _, visited := fwdPrev[e.From]; visited {
				continue
			}
			fwdPrev[e.From] = curr.id
			viaImpl[e.From] = true
			if _, inBwd := bwdPrev[e.From]; inBwd {
				meetNode = e.From
				found = true
				return
			}
			fwdQueue = append(fwdQueue, bfsEntry{e.From, curr.hop + 1})
		}
	}

	// expandBackward processes one level of backward BFS (reverse callers).
	expandBackward := func() {
		if len(bwdQueue) == 0 || found {
			return
		}
		curr := bwdQueue[0]
		bwdQueue = bwdQueue[1:]
		if curr.hop >= maxBFSHops {
			return
		}
		// Incoming: CALLS, HANDLES (who calls/handles this node).
		for _, e := range s.graph.InEdges(curr.id) {
			if !isCallChainEdge(e.Type) {
				continue
			}
			if _, visited := bwdPrev[e.From]; visited {
				continue
			}
			bwdPrev[e.From] = curr.id
			if e.Type == graph.EdgeImplements {
				viaImpl[e.From] = true
			}
			if e.Type == graph.EdgeHandles {
				viaHandles[e.From] = true
			}
			if _, inFwd := fwdPrev[e.From]; inFwd {
				meetNode = e.From
				found = true
				return
			}
			bwdQueue = append(bwdQueue, bfsEntry{e.From, curr.hop + 1})
		}
		// Forward IMPLEMENTS from backward direction.
		for _, e := range s.graph.OutEdges(curr.id) {
			if e.Type != graph.EdgeImplements {
				continue
			}
			if _, visited := bwdPrev[e.To]; visited {
				continue
			}
			bwdPrev[e.To] = curr.id
			viaImpl[e.To] = true
			if _, inFwd := fwdPrev[e.To]; inFwd {
				meetNode = e.To
				found = true
				return
			}
			bwdQueue = append(bwdQueue, bfsEntry{e.To, curr.hop + 1})
		}
	}

	// Alternate between forward and backward expansion.
	for !found && (len(fwdQueue) > 0 || len(bwdQueue) > 0) {
		expandForward()
		if !found {
			expandBackward()
		}
	}

	// Reconstruct prev map for path building: merge fwd and bwd paths through meetNode.
	prev := fwdPrev
	if found && meetNode != "" {
		// Walk backward from meetNode to the seeded to-candidate (bwdPrev[x]=="").
		// Record metToNode so path reconstruction starts from the correct endpoint.
		curr := meetNode
		for {
			next := bwdPrev[curr]
			if next == "" {
				// curr is a seeded to-candidate: the actual destination reached.
				metToNode = curr
				break
			}
			prev[next] = curr
			curr = next
		}
		// Also update toNode to the actual reached candidate for display purposes.
		if metToNode != toNode.ID {
			if n := s.graph.GetNode(metToNode); n != nil {
				toNode = n
			}
		}
	}

	if !found {
		// P7-9: emit search event for call chain BFS (not found).
		if pc := s.getPulseClient(); pc != nil {
			pc.RecordSearchEvent(pulse.SearchEvent{
				Mode: "call_chain", Query: fromName + " -> " + toName,
				ResultCount: 0, ProjectID: s.projectID,
			})
		}
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
		if len(resolveWarnings) > 0 {
			notFound["resolve_warnings"] = resolveWarnings
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

	// Reconstruct path starting from the actual to-candidate reached by BFS.
	startID := metToNode
	if startID == "" {
		startID = toNode.ID
	}
	var chainIDs []graph.NodeID
	curr := startID
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

	// P7-9: emit search event for call chain BFS (found).
	if pc := s.getPulseClient(); pc != nil {
		pc.RecordSearchEvent(pulse.SearchEvent{
			Mode: "call_chain", Query: fromName + " -> " + toName,
			ResultCount: len(chain) - 1, ProjectID: s.projectID,
		})
	}

	// Build from/to/path fields in addition to chain so parsers that expect
	// the path/from/to shape (e.g. GraphBench) can read the response correctly.
	var fromStep, toStep chainStep
	var pathSteps []chainStep
	if len(chain) > 0 {
		fromStep = chain[0]
	}
	if len(chain) > 1 {
		toStep = chain[len(chain)-1]
		pathSteps = chain[1:] // intermediate + destination
	}
	resp := map[string]interface{}{
		"found":         true,
		"hops":          len(chain) - 1,
		"via_interface": usedInterface,
		"via_handles":   usedHandles,
		"chain":         chain,  // full list (from + intermediates + to)
		"from":          fromStep,
		"to":            toStep,
		"path":          pathSteps, // intermediates + destination (matches callChainResponse.Path)
	}
	if len(resolveWarnings) > 0 {
		resp["resolve_warnings"] = resolveWarnings
	}
	return jsonResult(resp)
}

// isTestFile returns true for test files (_test.go, test_*.py, *_test.ts, etc.)
// so find_entity can rank implementation files above test files.
func (s *Server) handleSemanticSearch(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("semantic_search requires a persistent store (run 'synapses start' or 'synapses index' first)"), nil
	}

	query := stringArg(req, "query")
	if query == "" {
		return mcp.NewToolResultError("query is required (e.g., 'auth decisions', 'why we switched to OAuth')"), nil
	}

	limitRaw, _ := req.GetArguments()["limit"].(float64)
	limit := int(limitRaw)
	if limit <= 0 {
		limit = 20
	}

	mode := stringArg(req, "mode")
	domain := stringArg(req, "domain") // optional: "code", "docs", or "" (all)

	// --- HyDE: Hypothetical Document Embeddings (mode=semantic + brain available) ---
	// When the user requests semantic search and the brain is online, generate a
	// hypothetical code entity definition that "answers" the query. Embedding the
	// hypothesis instead of the raw query bridges the vocabulary gap between natural-
	// language queries and code names in the HNSW index.
	//
	// hyde=false opt-out: skip hypothesis generation for exact-name queries where
	// the raw name is already the best search signal.
	//
	// Fallback contract: if the brain is unavailable, ShouldDegrade fires, or the
	// 500 ms timeout expires, hydeHypothesis stays "" and we fall through to
	// embedding the raw query unchanged — zero degradation visible to the caller.
	var hydeHypothesis string
	if mode == "semantic" && s.brainClient != nil {
		// hyde=false is an explicit opt-out for exact-name queries where the raw
		// query name is already the best search signal and hypothesis generation
		// would only add latency. Any non-bool or absent value defaults to true.
		if b, ok := req.GetArguments()["hyde"].(bool); !ok || b {
			hydeHypothesis = s.brainClient.GenerateHypothetical(ctx, query)
		}
	}

	// --- Vector path (when embedding_endpoint is configured) ---
	// Embed the query (or the HyDE hypothesis) with a 2s timeout so a slow Ollama
	// never blocks the agent. On any error, silently fall through to FTS5-only.
	var vectorResults []store.SearchResult
	searchMode := "fts5_bm25"
	// embedFailed is true when the embed API call itself errored (timeout, endpoint
	// down, etc.) — distinct from "embed succeeded but no vector matches found".
	// Used below to produce an accurate fallback_reason for the caller.
	var embedFailed bool
	if s.embedClient != nil {
		// Prefer embedding the HyDE hypothesis; fall back to raw query.
		embedTarget := query
		if hydeHypothesis != "" {
			embedTarget = hydeHypothesis
		}
		embedCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		queryVec, embedErr := s.embedClient.Embed(embedCtx, embedTarget)
		cancel()
		if embedErr != nil || len(queryVec) == 0 {
			embedFailed = true
		} else {
			// Over-fetch when domain filter active, then post-filter.
			fetchLimit := limit
			if domain != "" {
				fetchLimit = limit * 3
			}
			vr, verr := s.store.VectorSearch(queryVec, fetchLimit)
			if verr == nil && len(vr) > 0 {
				if domain != "" {
					vr = s.store.FilterResultsByDomain(vr, domain)
				}
				if len(vr) > limit {
					vr = vr[:limit]
				}
				if len(vr) > 0 {
					vectorResults = vr
					searchMode = "vector_cosine"
				}
			}
		}
	}

	// --- FTS5 path (always runs as fallback / supplement) ---
	ftsResults, err := s.store.SemanticSearchWithDomain(query, limit, domain)
	if err != nil {
		return toolError("semantic search", err)
	}

	// --- Merge via Reciprocal Rank Fusion (RRF) when both channels have results ---
	var results []store.SearchResult
	if len(vectorResults) > 0 && len(ftsResults) > 0 {
		// Build ranked ID lists for each channel.
		channels := map[string][]string{
			"vector": make([]string, len(vectorResults)),
			"bm25":   make([]string, len(ftsResults)),
		}
		resultLookup := make(map[string]store.SearchResult, len(vectorResults)+len(ftsResults))
		for i, r := range vectorResults {
			channels["vector"][i] = r.ID
			resultLookup[r.ID] = r
		}
		for i, r := range ftsResults {
			channels["bm25"][i] = r.ID
			if _, exists := resultLookup[r.ID]; !exists {
				resultLookup[r.ID] = r
			}
		}

		mergedIDs, _ := store.RRFMergeWeighted(channels, limit, 60, map[string]float64{
			"vector": 1.0,
			"bm25":   1.0,
		})

		results = make([]store.SearchResult, 0, len(mergedIDs))
		for _, id := range mergedIDs {
			if r, ok := resultLookup[id]; ok {
				results = append(results, r)
			}
		}

		if hydeHypothesis != "" {
			searchMode = "hybrid_rrf+hyde"
		} else {
			searchMode = "hybrid_rrf"
		}
	} else if len(vectorResults) > 0 {
		results = vectorResults
		searchMode = "vector_cosine"
	} else {
		results = ftsResults
	}

	resp := map[string]interface{}{
		"query":       query,
		"count":       len(results),
		"results":     results,
		"search_mode": searchMode,
	}

	// Surface HyDE metadata so agents understand how results were ranked.
	// hyde_hypothesis is truncated to 200 chars — enough for debugging without
	// bloating the response when the LLM generates a long signature.
	if hydeHypothesis != "" {
		h := hydeHypothesis
		if len([]rune(h)) > 200 {
			rr := []rune(h)
			h = string(rr[:200]) + "…"
		}
		resp["hyde_hypothesis"] = h
	}

	embeddingCount := s.store.EmbeddingCount()
	if s.embedClient != nil && searchMode == "fts5_bm25" {
		if embeddingCount == 0 {
			resp["note"] = fmt.Sprintf("Vector embeddings not yet built. Run 'synapses index' or wait for the background embedding pass to complete (model: %s).", s.embedClient.Model())
		} else {
			resp["note"] = fmt.Sprintf("Vector index partial (%d nodes embedded). Results blended from cosine+FTS5 as more embeddings complete.", embeddingCount)
		}
	}
	// When mode="semantic" was explicitly requested but we fell back to FTS5,
	// surface a fallback_reason so the agent knows semantic ranking was not used.
	// Three distinct failure modes produce different diagnostics:
	//   1. embedClient nil     → embedder not configured
	//   2. embedFailed=true    → embed API call errored (timeout / endpoint down)
	//   3. neither             → embed worked but the vector index had no matches
	if mode == "semantic" && searchMode == "fts5_bm25" {
		if s.embedClient == nil {
			resp["fallback_reason"] = "semantic unavailable — embedder not ready (call session_init to check embeddings status)"
		} else if embedFailed {
			resp["fallback_reason"] = "semantic embedding failed (endpoint timeout or error) — FTS5 fallback used"
		} else {
			resp["fallback_reason"] = "semantic index has no vector matches for this query — FTS5 fallback used"
		}
	}
	if len(results) == 0 {
		resp["hint"] = "No matches found. Try broader terms, partial names, or use search() for exact substring matching."
	}
	// _summary with match-type breakdown when hybrid search is active.
	if len(vectorResults) > 0 {
		resp["_summary"] = fmt.Sprintf("%d result(s) for %q: %d vector, %d fts5 [%s]",
			len(results), query, len(vectorResults), len(ftsResults), searchMode)
	} else {
		resp["_summary"] = fmt.Sprintf("%d result(s) for %q [%s]", len(results), query, searchMode)
	}

	return jsonResult(resp)
}

// inlineFindEntity runs the find_entity lookup inline. Used by handleGetContext
// to surface candidates when the requested entity cannot be found by exact name,
// saving agents a separate find_entity round-trip.
func (s *Server) inlineFindEntity(query string) []map[string]interface{} {
	nodes := s.graph.FindByName(query)
	if len(nodes) == 0 {
		nodes = s.graph.FindByPatternLimit(query, 50)
	}
	// Dotted method name fallback: "Store.Close" → search "Close", filter by "Store".
	if len(nodes) == 0 && strings.Contains(query, ".") {
		parts := strings.SplitN(query, ".", 2)
		typePrefix, method := strings.ToLower(parts[0]), parts[1]
		candidates := s.graph.FindByName(method)
		if len(candidates) == 0 {
			candidates = s.graph.FindByPatternLimit(method, 50)
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

// truncateAtWord shortens s to at most maxChars Unicode code points, breaking
// at the last space before the limit and appending "…" when truncation occurs.
// Safe for multi-byte UTF-8 (operates on runes, not bytes).
func truncateAtWord(s string, maxChars int) string {
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	// Walk backward from maxChars-1 to find the last space.
	cut := maxChars - 1 // leave room for ellipsis rune
	for cut > 0 && runes[cut] != ' ' {
		cut--
	}
	if cut == 0 {
		// No space found — hard cut at maxChars-1 to fit the ellipsis.
		cut = maxChars - 1
	}
	return string(runes[:cut]) + "…"
}

// handleGetEdgeTypes returns the full EdgeTypeCatalog: every edge type registered
// in the graph with its semantic weight, BFS direction, domain tag, and description.
// The catalog is the foundation for multi-domain BFS (Sprint 12) — agents can
// query it to understand traversal semantics or to select domain-specific edge filters.
//
// Response format: {"edge_types": [EdgeTypeDescriptor...], "total": N}
// The array is sorted by descending semantic_weight (highest-impact edges first).
func (s *Server) handleGetEdgeTypes(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	format, _ := req.GetArguments()["format"].(string)

	catalog := graph.GetEdgeTypes()

	if format == "compact" {
		// Compact: one line per edge type, sorted by weight descending.
		// Useful for quick orientation without token budget pressure.
		var sb strings.Builder
		sb.WriteString("# Edge Type Catalog\n\n")
		sb.WriteString("Sorted by BFS semantic weight (descending). Higher weight = traversed first.\n\n")
		fmt.Fprintf(&sb, "%-20s %-8s %-10s %s\n", "TYPE", "WEIGHT", "DOMAIN", "DESCRIPTION")
		sb.WriteString(strings.Repeat("-", 80) + "\n")
		for _, d := range catalog {
			synMark := ""
			if d.Synthetic {
				synMark = "*"
			}
			fmt.Fprintf(&sb, "%-20s %-8.2f %-10s %s%s\n",
				string(d.Name), d.SemanticWeight, d.Domain, truncateAtWord(d.Description, 60), synMark)
		}
		sb.WriteString("\n* = synthetic edge (heuristic-injected, not AST-derived)\n")
		sb.WriteString("\nUse format=\"json\" for full descriptions and machine-readable output.\n")
		return mcp.NewToolResultText(sb.String()), nil
	}

	return jsonResult(map[string]interface{}{
		"edge_types": catalog,
		"total":      len(catalog),
		"note":       "Sorted by semantic_weight descending. Synthetic edges are heuristic-injected (not AST-derived). Sprint 9 adds infra/api domain edges; Sprint 12 adds cross-domain edges.",
	})
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
