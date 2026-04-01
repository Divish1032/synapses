package mcp

// compaction_helpers.go — helpers for session_init(scope="compaction") recovery packet.
// The get_compaction_guide tool was removed in Sprint 23.9, but these helpers remain
// because session_init uses them to assemble the compaction recovery section.

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// entityScore represents a scored entity for the compaction recovery packet.
type entityScore struct {
	Entity      string  `json:"entity"`
	Score       float64 `json:"score"`
	SignalCount int     `json:"signal_count"`
	EdgeCount   int     `json:"edges"`
}

// rankEntityImportance scores entities by work ledger signal frequency and graph connectivity.
// score = normalize(signal_count * 0.7 + edge_count * 0.3)
func (s *Server) rankEntityImportance(sessionID string, entities []string) []entityScore {
	if len(entities) == 0 {
		return nil
	}

	signalCounts := s.countEntitySignals(sessionID)

	var scores []entityScore
	maxRaw := 0.0

	for _, entity := range entities {
		sc := signalCounts[entity]
		ec := 0
		if s.graph != nil {
			ec = len(s.graph.DirectNeighbors(graph.NodeID(entity)))
			if ec == 0 && !strings.Contains(entity, "::") {
				nodes := s.graph.FindByName(entity)
				if len(nodes) > 0 {
					ec = len(s.graph.DirectNeighbors(nodes[0].ID))
				}
			}
		}
		raw := float64(sc)*0.7 + float64(ec)*0.3
		if raw > maxRaw {
			maxRaw = raw
		}
		scores = append(scores, entityScore{
			Entity:      entity,
			SignalCount: sc,
			EdgeCount:   ec,
			Score:       raw,
		})
	}

	if maxRaw > 0 {
		for i := range scores {
			scores[i].Score = math.Round(scores[i].Score/maxRaw*100) / 100
		}
	}

	sort.Slice(scores, func(i, j int) bool {
		return scores[i].Score > scores[j].Score
	})

	if len(scores) > 10 {
		scores = scores[:10]
	}

	return scores
}

// countEntitySignals counts how many times each entity appears in the work ledger for a session.
func (s *Server) countEntitySignals(sessionID string) map[string]int {
	if s.store == nil {
		return make(map[string]int)
	}
	counts, err := s.store.SessionLedgerEntityCounts(sessionID)
	if err != nil {
		return make(map[string]int)
	}
	return counts
}

// buildRelationshipMap finds edges between entities in the work set.
// Only returns edges where both endpoints are in the work set.
func (s *Server) buildRelationshipMap(entities []string) []map[string]string {
	if s.graph == nil || len(entities) < 2 {
		return nil
	}

	resolved := s.resolveToNodeIDs(entities)
	if len(resolved) < 2 {
		return nil
	}

	nodeSet := make(map[graph.NodeID]struct{}, len(resolved))
	for _, id := range resolved {
		nodeSet[id] = struct{}{}
	}

	var relationships []map[string]string
	seen := make(map[string]bool)

	for _, id := range resolved {
		edges := s.graph.OutEdges(id)
		for _, e := range edges {
			if _, ok := nodeSet[e.To]; !ok {
				continue
			}
			key := fmt.Sprintf("%s→%s→%s", e.From, e.To, e.Type)
			if seen[key] {
				continue
			}
			seen[key] = true
			relationships = append(relationships, map[string]string{
				"from": string(e.From),
				"to":   string(e.To),
				"type": string(e.Type),
			})
			if len(relationships) >= 20 {
				return relationships
			}
		}
	}

	return relationships
}
