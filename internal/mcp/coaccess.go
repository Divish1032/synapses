// Package mcp — Sprint 27.5: Co-access pattern analysis.
//
// At session end, analyzes the work ledger to find entity pairs that were
// accessed together. Records these as co-occurrence patterns in the brain's
// context_patterns table. During BFS carving, co-accessed entities are
// injected as virtual neighbors.
package mcp

import (
	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

const (
	coAccessMinCount = 2   // minimum entity accesses before recording
	coAccessMaxInject = 5   // max co-accessed entities injected into BFS
	coAccessMinConf  = 0.4 // minimum confidence for BFS injection
)

// analyzeCoAccess extracts co-access patterns from a session's work ledger
// and records them via the brain client.
//
// Algorithm: reads entity access counts for the session. Any two entities
// each accessed >= coAccessMinCount times are considered co-accessed.
func analyzeCoAccess(st *store.Store, bc *brain.Client, sessionID string) {
	if st == nil || bc == nil || sessionID == "" {
		return
	}

	// Guard against store being closed between queueing and execution.
	// SessionLedgerEntityCounts will return an error on a closed DB.
	counts, err := st.SessionLedgerEntityCounts(sessionID)
	if err != nil || len(counts) < 2 {
		return
	}

	// Collect entities with sufficient access count.
	var entities []string
	for name, count := range counts {
		if count >= coAccessMinCount {
			entities = append(entities, name)
		}
	}
	if len(entities) < 2 {
		return
	}

	// Record all pairs as co-access patterns (bidirectional).
	// Cap to prevent combinatorial explosion on large sessions.
	maxPairs := 50
	recorded := 0
	for i := 0; i < len(entities) && recorded < maxPairs; i++ {
		for j := i + 1; j < len(entities) && recorded < maxPairs; j++ {
			_ = bc.UpsertCoAccessPattern(entities[i], entities[j])
			_ = bc.UpsertCoAccessPattern(entities[j], entities[i])
			recorded++
		}
	}

	// Decay: for ALL entities accessed this session (not just frequent ones),
	// reduce confidence of patterns where trigger was accessed but co_change wasn't.
	// This prevents stale patterns from permanently inflating context.
	allEntities := make([]string, 0, len(counts))
	for name := range counts {
		allEntities = append(allEntities, name)
	}
	_ = bc.DecayCoAccessPatterns(allEntities)
}

// loadCoAccessHints queries the brain client for co-access patterns matching
// the root entity name. Returns hints suitable for CarveConfig.CoAccessPatterns.
func loadCoAccessHints(bc *brain.Client, g *graph.Graph, rootName string) []graph.CoAccessHint {
	if bc == nil || rootName == "" {
		return nil
	}

	patterns := bc.GetPatterns(rootName, coAccessMaxInject*2)
	if len(patterns) == 0 {
		return nil
	}

	var hints []graph.CoAccessHint
	for _, p := range patterns {
		if p.Confidence < coAccessMinConf {
			continue
		}
		// Resolve the co-accessed entity name to a node ID.
		// Use pickBestNode for deterministic disambiguation (same logic as get_context).
		nodes := g.FindByName(p.CoChange)
		if len(nodes) == 0 {
			continue
		}
		best := pickBestNode(nodes, g, p.CoChange)
		if best == nil {
			continue
		}
		hints = append(hints, graph.CoAccessHint{
			NodeID:     best.ID,
			Confidence: p.Confidence,
		})
		if len(hints) >= coAccessMaxInject {
			break
		}
	}
	return hints
}
