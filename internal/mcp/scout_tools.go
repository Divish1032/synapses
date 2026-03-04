package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/scout"
)

// getScoutClient type-asserts the stored scoutClient to *scout.Client.
// Returns nil if no scout client is configured.
func (s *Server) getScoutClient() *scout.Client {
	if s.scoutClient == nil {
		return nil
	}
	sc, _ := s.scoutClient.(*scout.Client)
	return sc
}

// handleWebSearch calls scout POST /v1/search and returns structured results.
func (s *Server) handleWebSearch(
	ctx context.Context,
	req mcpgo.CallToolRequest,
) (*mcpgo.CallToolResult, error) {
	sc := s.getScoutClient()
	if sc == nil {
		return mcpgo.NewToolResultError("scout unavailable: configure scout.url in synapses.json"), nil
	}

	query, _ := req.Params.Arguments["query"].(string)
	if query == "" {
		return mcpgo.NewToolResultError("query is required"), nil
	}

	maxResults := 5
	if v, ok := req.Params.Arguments["max_results"].(float64); ok && v > 0 {
		maxResults = int(v)
	}
	region, _ := req.Params.Arguments["region"].(string)
	timelimit, _ := req.Params.Arguments["timelimit"].(string)

	resp := sc.Search(ctx, scout.SearchRequest{
		Query:      query,
		MaxResults: maxResults,
		Region:     region,
		Timelimit:  timelimit,
	})
	if resp == nil {
		return mcpgo.NewToolResultError("scout search failed: service unreachable or timed out"), nil
	}

	return jsonResult(map[string]interface{}{
		"query": resp.Query,
		"hits":  resp.Hits,
		"count": resp.Count,
	})
}

// handleWebFetch calls scout POST /v1/fetch and returns extracted Markdown content.
// Input may be a URL or a plain-text query — scout auto-routes.
func (s *Server) handleWebFetch(
	ctx context.Context,
	req mcpgo.CallToolRequest,
) (*mcpgo.CallToolResult, error) {
	sc := s.getScoutClient()
	if sc == nil {
		return mcpgo.NewToolResultError("scout unavailable: configure scout.url in synapses.json"), nil
	}

	input, _ := req.Params.Arguments["input"].(string)
	if input == "" {
		return mcpgo.NewToolResultError("input is required (URL or search query)"), nil
	}

	forceRefresh, _ := req.Params.Arguments["force_refresh"].(bool)

	resp := sc.Fetch(ctx, scout.FetchRequest{
		Input:        input,
		ForceRefresh: forceRefresh,
	})
	if resp == nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("scout fetch failed for input=%q: service unreachable or timed out", input)), nil
	}

	result := map[string]interface{}{
		"url":          resp.URL,
		"title":        resp.Title,
		"content_type": resp.ContentType,
		"content_md":   resp.ContentMD,
		"word_count":   resp.WordCount,
		"cached":       resp.Cached,
	}
	if resp.Fragment != nil {
		result["summary"] = resp.Fragment.Summary
		result["tags"] = resp.Fragment.Tags
	}

	// Fire-and-forget: send to intelligence for summarization so the content
	// surfaces in future context packets. No-op if brain is not configured.
	go s.ingestWebContent(resp.URL, resp.Title, resp.ContentMD)

	return jsonResult(result)
}

// handleWebAnnotate persists web findings as a graph node annotation so they
// survive across sessions and appear in get_context for that node.
// This is the "context sharing" pattern from Nia — web findings become
// first-class data objects attached to code entities.
func (s *Server) handleWebAnnotate(
	ctx context.Context,
	req mcpgo.CallToolRequest,
) (*mcpgo.CallToolResult, error) {
	if s.store == nil {
		return mcpgo.NewToolResultError("store not available (run synapses start, not synapses index)"), nil
	}

	nodeID, _ := req.Params.Arguments["node_id"].(string)
	if nodeID == "" {
		return mcpgo.NewToolResultError("node_id is required"), nil
	}
	agentID, _ := req.Params.Arguments["agent_id"].(string)
	note, _ := req.Params.Arguments["note"].(string)

	// Optional: structured hits JSON to format as a readable annotation.
	if hitsJSON, ok := req.Params.Arguments["hits"].(string); ok && hitsJSON != "" {
		var hits []scout.SearchHit
		if err := json.Unmarshal([]byte(hitsJSON), &hits); err == nil && len(hits) > 0 {
			var sb strings.Builder
			sb.WriteString("[web findings]")
			if note != "" {
				sb.WriteString(" ")
				sb.WriteString(note)
			}
			for i, h := range hits {
				if i >= 5 {
					break
				}
				fmt.Fprintf(&sb, "\n  - [%s](%s)", h.Title, h.URL)
				if h.Snippet != "" {
					sb.WriteString(": ")
					sb.WriteString(h.Snippet)
				}
			}
			note = sb.String()
		}
	}

	if note == "" {
		return mcpgo.NewToolResultError("note or hits is required"), nil
	}

	id, err := s.store.AddAnnotation(nodeID, agentID, note)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("store annotation failed: %v", err)), nil
	}
	_ = ctx
	return jsonResult(map[string]interface{}{
		"id":      id,
		"node_id": nodeID,
		"note":    note,
		"status":  "annotated — visible in get_context for this node",
	})
}

// handleWebDeepSearch calls scout POST /v1/deep-search for multi-query
// orchestrated search with fan-out and deduplication.
func (s *Server) handleWebDeepSearch(
	ctx context.Context,
	req mcpgo.CallToolRequest,
) (*mcpgo.CallToolResult, error) {
	sc := s.getScoutClient()
	if sc == nil {
		return mcpgo.NewToolResultError("scout unavailable: configure scout.url in synapses.json"), nil
	}

	query, _ := req.Params.Arguments["query"].(string)
	if query == "" {
		return mcpgo.NewToolResultError("query is required"), nil
	}

	maxResults := 10
	if v, ok := req.Params.Arguments["max_results"].(float64); ok && v > 0 {
		maxResults = int(v)
	}
	region, _ := req.Params.Arguments["region"].(string)
	timelimit, _ := req.Params.Arguments["timelimit"].(string)

	resp := sc.DeepSearch(ctx, scout.DeepSearchRequest{
		Query:      query,
		MaxResults: maxResults,
		Region:     region,
		Timelimit:  timelimit,
	})
	if resp == nil {
		return mcpgo.NewToolResultError("scout deep-search failed: service unreachable or timed out"), nil
	}

	return jsonResult(map[string]interface{}{
		"query":            resp.Query,
		"expanded_queries": resp.ExpandedQueries,
		"hits":             resp.Hits,
		"count":            resp.Count,
		"total_raw_hits":   resp.TotalRawHits,
		"deduplicated":     resp.DeduplicatedCount,
	})
}
