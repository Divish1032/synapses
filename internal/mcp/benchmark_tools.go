package mcp

import (
	"context"
	"strings"

	"github.com/SynapsesOS/synapses/internal/benchmark"
	"github.com/mark3labs/mcp-go/mcp"
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
