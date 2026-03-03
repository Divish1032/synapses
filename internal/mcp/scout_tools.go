package mcp

import (
	"context"
	"fmt"

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

	return jsonResult(result)
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
