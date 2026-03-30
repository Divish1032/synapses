package mcp

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// handleGetCompactionGuide returns structured hints for efficient context compaction.
// Pure queries + ranking — no LLM dependency.
func (s *Server) handleGetCompactionGuide(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	agentID, _ := req.GetArguments()["agent_id"].(string)
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}

	sessionID := s.getSynapseSessionID(SessionIDFromContext(ctx))
	if sessionID == "" || s.store == nil {
		return jsonResult(map[string]interface{}{
			"must_preserve":     map[string]interface{}{},
			"safe_to_forget":    defaultSafeToForget,
			"entity_importance": []interface{}{},
			"relationship_map":  []interface{}{},
			"hint":              "No active session found. Call session_init first.",
		})
	}

	// 1. Get work ledger signals for this session
	entities, files, _ := s.store.SessionLedgerEntities(sessionID)

	// 2. Build must_preserve from session state
	mustPreserve := s.buildMustPreserve(agentID, sessionID)

	// 3. Build entity importance ranking
	importance := s.rankEntityImportance(sessionID, entities)

	// 4. Build relationship map (edges between work-set entities)
	relationships := s.buildRelationshipMap(entities)

	guide := map[string]interface{}{
		"must_preserve":     mustPreserve,
		"safe_to_forget":    defaultSafeToForget,
		"entity_importance": importance,
		"relationship_map":  relationships,
	}

	if len(files) > 0 {
		// Add file list for reference
		shown := files
		if len(shown) > 10 {
			shown = shown[:10]
		}
		guide["active_files"] = shown
	}

	return jsonResult(guide)
}

var defaultSafeToForget = []string{
	"File contents (re-read via editor or get_file_context)",
	"Search results (re-query via search tool)",
	"Graph traversals (re-query via get_context)",
	"Package documentation (re-query via lookup_docs)",
	"Violation details (re-query via get_violations)",
}

// buildMustPreserve extracts critical context that should survive compaction.
func (s *Server) buildMustPreserve(agentID, sessionID string) map[string]interface{} {
	result := map[string]interface{}{}

	if s.store == nil {
		return result
	}

	// Get active task session state
	tasks, err := s.store.GetPendingTasks("", agentID)
	if err == nil {
		for _, t := range tasks {
			if t.Status != "in_progress" {
				continue
			}
			if st, stErr := s.store.GetSessionState(t.ID); stErr == nil && st != nil {
				if st.Approach != "" {
					result["task_approach"] = st.Approach
				}
				if len(st.RemainingSteps) > 0 {
					result["remaining_steps"] = st.RemainingSteps
				}
				if len(st.Decisions) > 0 {
					result["key_decisions"] = st.Decisions
				}
				if len(st.Blockers) > 0 {
					result["blockers"] = st.Blockers
				}
				break // only first in-progress task
			}
		}
	}

	// Get active rule violations for touched files
	if sessionID != "" {
		_, files, _ := s.store.SessionLedgerEntities(sessionID)
		if violations, vErr := s.store.GetViolationsForFiles(files, 5); vErr == nil && len(violations) > 0 {
			var vList []map[string]string
			for _, v := range violations {
				vList = append(vList, map[string]string{
					"rule":     v.RuleID,
					"severity": v.Severity,
					"from":     v.FromNode,
					"to":       v.ToNode,
				})
			}
			result["rule_violations"] = vList
		}
	}

	return result
}

// entityScore represents a scored entity for the compaction guide.
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

	// Count signal frequency per entity from work ledger
	signalCounts := s.countEntitySignals(sessionID)

	var scores []entityScore
	maxRaw := 0.0

	for _, entity := range entities {
		sc := signalCounts[entity]
		ec := 0
		if s.graph != nil {
			ec = len(s.graph.DirectNeighbors(graph.NodeID(entity)))
			// Also try resolving short names
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
			Score:       raw, // normalized below
		})
	}

	// Normalize scores to [0, 1]
	if maxRaw > 0 {
		for i := range scores {
			scores[i].Score = math.Round(scores[i].Score/maxRaw*100) / 100
		}
	}

	// Sort by score descending
	sort.Slice(scores, func(i, j int) bool {
		return scores[i].Score > scores[j].Score
	})

	// Cap at 10
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

	// Resolve all entities to NodeIDs
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

