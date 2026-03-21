package mcp

import (
	"context"
	"fmt"

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
	if s.store == nil {
		return mcp.NewToolResultError("store not available"), nil
	}

	scenario, _ := req.GetArguments()["scenario"].(string)

	if scenario == "" || scenario == "all" {
		result := benchmark.RunAll(s.graph, s.store)
		return jsonResult(result)
	}

	sc, err := benchmark.FindScenario(scenario)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("%v", err)), nil
	}

	result := benchmark.RunScenarios(s.graph, s.store, []benchmark.Scenario{sc})
	return jsonResult(result)
}
