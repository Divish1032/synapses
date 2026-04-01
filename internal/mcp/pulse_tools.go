package mcp

import (
	"encoding/json"
	"os"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/pulse"
)

// contextDeliveryExtras carries optional metrics that are only available in the
// full (non-cache-hit) response path. Nil is safe — emitContextDelivery treats
// nil as "no extras".
type contextDeliveryExtras struct {
	Intent               string
	DepthRequested       int
	DepthAchieved        int
	NodesVisited         int
	AnnotationsIncluded  bool
	OutputFormat         string
	EdgeTypesDist        string // JSON map of edge type -> count
	TraversalDurationMs  float64
	GraphSizeAtTraversal int
	DetailLevel          string
	RulesMatched         int
	ViolationsFound      int
	MinRelevanceHits     int
	TokenBudgetHit       bool
	Refetched            bool
	CacheSize            int // P9-8: BFS cache size at delivery time
}

// emitContextDelivery fires a ContextDeliveryEvent to the pulse sidecar for
// a get_context / prepare_context call. It is called
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
	durationMs int64,
	sessionID string,
	opts *contextDeliveryExtras,
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

	// P6-4: normalize entity name to "Name@dir/file" format to match
	// outcome_signals and enable JOINs between context_deliveries and outcome_signals.
	normalizedEntity := entityWithPath(entity, file)

	evt := pulse.ContextDeliveryEvent{
		ToolName:       toolName,
		AgentID:        agentID,
		ProjectID:      s.projectID,
		Entity:         normalizedEntity,
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
		DurationMs:     durationMs,
		SessionID:      sessionID,
		// P5 — Item 30: entity was found (caller only invokes emitContextDelivery on success).
		EntityFound: true,
	}

	// P6-1: populate the 14 fields that were previously always zero/empty.
	if opts != nil {
		evt.Intent = opts.Intent
		evt.DepthRequested = opts.DepthRequested
		evt.DepthAchieved = opts.DepthAchieved
		evt.NodesVisited = opts.NodesVisited
		evt.AnnotationsIncluded = opts.AnnotationsIncluded
		evt.OutputFormat = opts.OutputFormat
		evt.EdgeTypesDist = opts.EdgeTypesDist
		evt.TraversalDurationMs = opts.TraversalDurationMs
		evt.GraphSizeAtTraversal = opts.GraphSizeAtTraversal
		evt.DetailLevel = opts.DetailLevel
		evt.RulesMatched = opts.RulesMatched
		evt.ViolationsFound = opts.ViolationsFound
		evt.MinRelevanceHits = opts.MinRelevanceHits
		evt.TokenBudgetHit = opts.TokenBudgetHit
		evt.Refetched = opts.Refetched
		evt.CacheSize = opts.CacheSize
	}

	// Synchronous — callers are expected to wrap in goBackground.
	pc.RecordContextDelivery(evt)
}

// Sprint 23.9: emitFileContextDelivery removed — get_file_context tool removed.

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
