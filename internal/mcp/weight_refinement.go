package mcp

import (
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// applyEdgeWeightRefinements updates per-edge learned weight multipliers based
// on this session's context deliveries and task outcome (Sprint 15 #3).
//
// Algorithm:
//  1. Fetch all entity names that received context in this session.
//  2. Map entity names to graph node IDs via FindByName (best-effort; ambiguous
//     names may produce multiple IDs — all are included).
//  3. Build a set of delivered node IDs.
//  4. For each delivered node, scan its outgoing edges. If the neighbor is also
//     in the delivered set → the edge was traversed during the session.
//  5. Apply delta to those edges: +0.1 for success, -0.05 for abandoned.
//  6. Mark edges untouched for 30+ days as dormant (one-time 0.7× penalty
//     baked into weight_mult; no special-casing needed at traversal time).
//
// Must be called in a background goroutine — it acquires graph.RLocks and
// performs SQLite writes. Errors are silently swallowed to preserve the
// instrumentation-must-not-block invariant.
func (s *Server) applyEdgeWeightRefinements(sessionID, outcome string) {
	if s.store == nil || s.graph == nil || sessionID == "" {
		return
	}

	// Step 1: entity names delivered in this session.
	entities := s.store.GetSessionAllDeliveredEntities(sessionID)
	if len(entities) == 0 {
		// Nothing delivered → nothing to refine. Still mark dormant edges.
		s.store.MarkDormantEdges(time.Now().UTC().Add(-dormancyDuration))
		return
	}

	// Step 2: map entity names → node IDs.
	delivered := make(map[graph.NodeID]struct{}, len(entities)*2)
	for _, name := range entities {
		nodes := s.graph.FindByName(name)
		for _, n := range nodes {
			delivered[n.ID] = struct{}{}
		}
	}
	if len(delivered) == 0 {
		s.store.MarkDormantEdges(time.Now().UTC().Add(-dormancyDuration))
		return
	}

	// Step 3: infer traversed edges — outgoing edges where both endpoints are
	// in the delivered set. This is a co-delivery approximation: if two nodes
	// were both delivered in the same session, the edge between them was very
	// likely traversed during BFS/PPR context carving.
	edgeSet := make(map[graph.EdgeWeightKey]struct{}, len(delivered)*4)
	for id := range delivered {
		for _, e := range s.graph.OutEdges(id) {
			if _, ok := delivered[e.To]; ok {
				edgeSet[graph.EdgeWeightKey{From: e.From, To: e.To, Type: e.Type}] = struct{}{}
			}
		}
	}
	if len(edgeSet) == 0 {
		s.store.MarkDormantEdges(time.Now().UTC().Add(-dormancyDuration))
		return
	}

	// Step 4: compute delta from outcome.
	// success → weak positive (+0.1); unknown (abandoned) → penalty (-0.05).
	delta := -0.05 // default: abandoned
	if outcome == "success" {
		delta = 0.1
	}

	// Step 5: apply delta in one batch.
	keys := make([]graph.EdgeWeightKey, 0, len(edgeSet))
	for k := range edgeSet {
		keys = append(keys, k)
	}
	s.store.UpsertLearnedEdgeWeights(keys, delta)

	// Step 6: mark dormant edges (30-day inactivity → 0.7× penalty applied once).
	s.store.MarkDormantEdges(time.Now().UTC().Add(-dormancyDuration))
}

// dormancyDuration is the inactivity window before an edge is marked dormant.
// Matches the ROADMAP spec: "Edges untouched 30+ days get dormant=true."
const dormancyDuration = 30 * 24 * time.Hour
