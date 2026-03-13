package mcp

import (
	"encoding/json"
	"os"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/pulse"
)

// emitContextDelivery fires a ContextDeliveryEvent to the pulse sidecar for
// a get_context / get_file_context / prepare_context call. It is called
// asynchronously (via goroutine) so it never blocks the MCP response path.
//
// responsePayload is the value being returned (marshaled to JSON to measure bytes).
// nodes is the slice of CarvedNode from the ego-subgraph (used to compute baseline).
// subgraph carries truncation metadata.
func (s *Server) emitContextDelivery(
	toolName, agentID, entity, file string,
	responsePayload interface{},
	nodes []graph.CarvedNode,
	edges []*graph.Edge,
	nodesPruned int,
	truncated bool,
	brainEnriched bool,
	cacheHit bool,
) {
	pc := s.getPulseClient()
	if pc == nil {
		return
	}
	b, _ := json.Marshal(responsePayload)
	responseBytes := len(b)
	responseTokens := responseBytes / 4

	// Baseline = sum of unique source file sizes / 4.
	// This is the honest cost of what the agent would have read via cat/grep.
	baselineTokens := fileBaselineTokens(nodes)

	go pc.RecordContextDelivery(pulse.ContextDeliveryEvent{
		ToolName:       toolName,
		AgentID:        agentID,
		ProjectID:      s.projectID,
		Entity:         entity,
		File:           file,
		ResponseBytes:  responseBytes,
		ResponseTokens: responseTokens,
		BaselineTokens: baselineTokens,
		NodesDelivered: len(nodes),
		NodesPruned:    nodesPruned,
		EdgesDelivered: len(edges),
		Truncated:      truncated,
		BrainEnriched:  brainEnriched,
		CacheHit:       cacheHit,
	})
}

// emitFileContextDelivery fires a ContextDeliveryEvent for get_file_context.
// nodes is the raw slice of graph.Node from the file lookup.
func (s *Server) emitFileContextDelivery(
	agentID, filePath string,
	nodes []*graph.Node,
	responsePayload interface{},
) {
	pc := s.getPulseClient()
	if pc == nil {
		return
	}
	b, _ := json.Marshal(responsePayload)
	responseBytes := len(b)

	// Collect unique files and compute baseline.
	seen := make(map[string]bool, len(nodes))
	var total int64
	for _, n := range nodes {
		if n.File == "" || seen[n.File] {
			continue
		}
		seen[n.File] = true
		if fi, err := os.Stat(n.File); err == nil {
			total += fi.Size()
		}
	}

	go pc.RecordContextDelivery(pulse.ContextDeliveryEvent{
		ToolName:       "get_file_context",
		AgentID:        agentID,
		ProjectID:      s.projectID,
		File:           filePath,
		ResponseBytes:  responseBytes,
		ResponseTokens: responseBytes / 4,
		BaselineTokens: int(total / 4),
		NodesDelivered: len(nodes),
	})
}

// fileBaselineTokens computes the baseline token count for a subgraph.
// Baseline = sum of actual on-disk sizes / 4 for all unique source files
// that contributed nodes. This is the verifiable cost of reading those files
// with cat/grep instead of using Synapses — with no inflation.
func fileBaselineTokens(nodes []graph.CarvedNode) int {
	seen := make(map[string]bool, len(nodes))
	var total int64
	for _, cn := range nodes {
		if cn.Node == nil || cn.Node.File == "" || seen[cn.Node.File] {
			continue
		}
		seen[cn.Node.File] = true
		if fi, err := os.Stat(cn.Node.File); err == nil {
			total += fi.Size()
		}
	}
	return int(total / 4)
}
