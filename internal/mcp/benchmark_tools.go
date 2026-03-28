package mcp

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/benchmark"
)

// handleBenchmark runs one or all built-in benchmark scenarios against the
// current indexed graph and store. Returns structured metrics: precision,
// recall, F1, and latency for each query in each scenario.
//
// Sprint 15 #8 — self-validating: ground truth is derived from the graph's
// own topology, not hardcoded IDs. Portable across any indexed codebase.
func (s *Server) handleBenchmark(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("no graph loaded — run 'synapses index' first"), nil
	}
	if s.graph.NodeCount() == 0 {
		return mcp.NewToolResultError("graph is empty — run 'synapses index' on a non-empty codebase first"), nil
	}
	if s.store == nil {
		return mcp.NewToolResultError("store not available"), nil
	}

	scenario, _ := req.GetArguments()["scenario"].(string)

	if scenario == "" || scenario == "all" {
		result := benchmark.RunAll(s.graph, s.store)
		return jsonResult(result)
	}

	// Issue 6: normalize underscores → hyphens and lowercase so "memory_recall"
	// works the same as "memory-recall". Surfaces a note when normalization fires.
	normalizedScenario := strings.ToLower(strings.ReplaceAll(scenario, "_", "-"))
	var normalizeNote string
	if normalizedScenario != scenario {
		normalizeNote = "Scenario name normalized from " + scenario + " to " + normalizedScenario + "."
		scenario = normalizedScenario
	}

	sc, err := benchmark.FindScenario(scenario)
	if err != nil {
		return toolError("find scenario", err)
	}

	result := benchmark.RunScenarios(s.graph, s.store, []benchmark.Scenario{sc})
	if normalizeNote != "" {
		result.Note = normalizeNote
	}
	return jsonResult(result)
}

// handleRankCandidates embeds a query and a list of candidate code snippets
// using the daemon's built-in embedder (nomic-embed or Ollama), then returns
// the candidates ranked by cosine similarity to the query.
//
// Used by the external benchmark binary (cmd/benchmark) to run RepoBench-R
// with real neural embeddings instead of local TF-IDF.
//
// Request:
//
//	{
//	  "query":      "string",          // code context / query text
//	  "candidates": ["string", ...]    // snippets to rank (up to 50)
//	}
//
// Response (JSON array, highest score first):
//
//	[{"index": 0, "score": 0.923}, ...]
func (s *Server) handleRankCandidates(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.memoryEmbedder == nil {
		return mcp.NewToolResultError(
			"embedder not available — start the daemon with embeddings enabled (embeddings_mode != off)"), nil
	}

	args := req.GetArguments()

	query, _ := args["query"].(string)
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	// Candidates may arrive as []interface{} from JSON.
	rawCandidates, _ := args["candidates"].([]interface{})
	if len(rawCandidates) == 0 {
		return mcp.NewToolResultError("candidates must be a non-empty array of strings"), nil
	}
	const maxCandidates = 50
	if len(rawCandidates) > maxCandidates {
		return mcp.NewToolResultError(
			fmt.Sprintf("too many candidates: %d (max %d)", len(rawCandidates), maxCandidates)), nil
	}

	candidates := make([]string, len(rawCandidates))
	for i, v := range rawCandidates {
		s, ok := v.(string)
		if !ok {
			return mcp.NewToolResultError(
				fmt.Sprintf("candidates[%d] is not a string", i)), nil
		}
		candidates[i] = s
	}

	// Embed query.
	queryVec, err := s.memoryEmbedder.Embed(ctx, query)
	if err != nil {
		return toolError("embed query", err)
	}

	// Embed all candidates in parallel (pool has multiple ONNX instances).
	type scored struct {
		Index int     `json:"index"`
		Score float64 `json:"score"`
	}
	results := make([]scored, len(candidates))
	var wg sync.WaitGroup
	for i, c := range candidates {
		wg.Add(1)
		go func(idx int, text string) {
			defer wg.Done()
			vec, embedErr := s.memoryEmbedder.Embed(ctx, text)
			if embedErr != nil {
				results[idx] = scored{Index: idx, Score: 0}
				return
			}
			results[idx] = scored{Index: idx, Score: float64(cosineF32(queryVec, vec))}
		}(i, c)
	}
	wg.Wait()

	// Sort descending by score.
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].Score > results[j-1].Score; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}

	return jsonResult(results)
}

// cosineF32 computes cosine similarity between two float32 vectors.
func cosineF32(a, b []float32) float32 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
}
