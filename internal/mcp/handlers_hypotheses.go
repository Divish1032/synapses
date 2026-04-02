package mcp

// handlers_hypotheses.go — Sprint 24.4: Hypothesis tracking in memory.
//
// Hypotheses are mutable working theories ("I think the bug is in X because Y")
// with a state machine: ACTIVE → CONFIRMED or ACTIVE → REJECTED.
// Unlike episodes (append-only event records), hypotheses change state over time.
//
// Two actions wired into handleMemoryDispatch:
//   memory(action="hypothesize")       — create new or update existing hypothesis
//   memory(action="list_hypotheses")   — list hypotheses filtered by state

import (
	"context"
	"fmt"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/store"
)

// handleHypothesize creates a new hypothesis or updates an existing one.
//
// Create:  agent_id + content required. hypothesis_id must be absent.
// Update:  agent_id + hypothesis_id required. state or evidence must be provided.
//
// When a hypothesis is updated to "rejected", the response includes an
// invalidation prompt so the agent adjusts its reasoning approach.
func (s *Server) handleHypothesize(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("hypothesis tracking unavailable: run 'synapses start' to create a persistent store"), nil
	}

	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}

	hypothesisID := stringArg(req, "hypothesis_id")

	content, contentErr := stringArgLimited(req, "content", maxArgLengthDecision)
	if contentErr != nil {
		return mcp.NewToolResultError(stripInternalPaths(contentErr.Error())), nil
	}

	evidence, evidenceErr := stringArgLimited(req, "evidence", maxArgLengthRationale)
	if evidenceErr != nil {
		return mcp.NewToolResultError(stripInternalPaths(evidenceErr.Error())), nil
	}

	newState := strings.ToLower(stringArg(req, "state"))

	// Scan content for prompt injection.
	if content != "" {
		scanResult, scanErr := s.scanContent("content", content)
		if scanErr != nil {
			return mcp.NewToolResultError(stripInternalPaths(scanErr.Error())), nil
		}
		content = scanResult.sanitized
	}
	if evidence != "" {
		scanResult, scanErr := s.scanContent("evidence", evidence)
		if scanErr != nil {
			return mcp.NewToolResultError(stripInternalPaths(scanErr.Error())), nil
		}
		evidence = scanResult.sanitized
	}

	projectID := s.projectID

	// ── UPDATE path ──────────────────────────────────────────────────────────
	if hypothesisID != "" {
		if newState == "" && evidence == "" {
			return mcp.NewToolResultError("when hypothesis_id is provided, at least one of state or evidence is required"), nil
		}
		// Default to keeping the same state if only evidence is updated.
		if newState == "" {
			// Fetch current state so we can pass it through.
			current, err := s.store.GetHypothesisByID(hypothesisID)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("hypothesis not found: %v", stripInternalPaths(err.Error()))), nil
			}
			newState = current.State
		}

		updated, err := s.store.UpdateHypothesisState(hypothesisID, newState, evidence)
		if err != nil {
			return toolError("update hypothesis", err)
		}

		resp := map[string]interface{}{
			"hypothesis_id": hypothesisID,
			"state":         newState,
		}
		if newState == store.HypothesisStateRejected {
			resp["message"] = fmt.Sprintf(
				"Hypothesis invalidated: %q. Evidence: %s. Adjust your approach — this working theory no longer holds.",
				updated.Content, evidenceOrDefault(updated.Evidence),
			)
			resp["invalidation_prompt"] = "Your prior reasoning may have been influenced by this hypothesis. Re-examine conclusions that depended on it."
		} else if newState == store.HypothesisStateConfirmed {
			resp["message"] = fmt.Sprintf("Hypothesis confirmed: %q. Store as a decision or pattern if it warrants a permanent record.", updated.Content)
		} else {
			resp["message"] = "Hypothesis updated."
		}
		return jsonResult(resp)
	}

	// ── CREATE path ──────────────────────────────────────────────────────────
	if content == "" {
		return mcp.NewToolResultError("content is required when creating a new hypothesis (e.g. 'I think the bug is in X because Y')"), nil
	}

	h := store.Hypothesis{
		AgentID:   agentID,
		ProjectID: projectID,
		Content:   content,
		Evidence:  evidence,
		State:     store.HypothesisStateActive,
	}

	id, err := s.store.InsertHypothesis(h)
	if err != nil {
		return toolError("insert hypothesis", err)
	}

	return jsonResult(map[string]interface{}{
		"hypothesis_id": id,
		"state":         store.HypothesisStateActive,
		"message":       fmt.Sprintf("Hypothesis recorded (id=%s). Update with state=confirmed or state=rejected as evidence accumulates.", id),
	})
}

// handleListHypotheses returns hypotheses for the current project/agent,
// optionally filtered by state.
//
// Params: agent_id (required), state_filter (optional: active/confirmed/rejected/all),
//         limit (optional, default 20).
func (s *Server) handleListHypotheses(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("hypothesis tracking unavailable: run 'synapses start' to create a persistent store"), nil
	}

	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}

	stateFilter := strings.ToLower(stringArg(req, "state_filter"))
	if stateFilter == "all" {
		stateFilter = "" // empty = no filter in store layer
	}

	limit := intArgDefault(req, "limit", 20)
	if limit < 1 || limit > 100 {
		limit = 20
	}

	hyps, err := s.store.GetHypotheses(agentID, s.projectID, stateFilter, limit)
	if err != nil {
		return toolError("list hypotheses", err)
	}

	type hypothesisItem struct {
		ID       string `json:"id"`
		Content  string `json:"content"`
		State    string `json:"state"`
		Evidence string `json:"evidence,omitempty"`
	}
	items := make([]hypothesisItem, 0, len(hyps))
	for _, h := range hyps {
		items = append(items, hypothesisItem{
			ID:       h.ID,
			Content:  h.Content,
			State:    h.State,
			Evidence: h.Evidence,
		})
	}

	filter := stateFilter
	if filter == "" {
		filter = "all"
	}
	return jsonResult(map[string]interface{}{
		"hypotheses":   items,
		"count":        len(items),
		"state_filter": filter,
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

func evidenceOrDefault(evidence string) string {
	if evidence == "" {
		return "(no evidence recorded)"
	}
	return evidence
}

// intArgDefault extracts an integer argument with a default fallback.
func intArgDefault(req mcp.CallToolRequest, key string, def int) int {
	v, ok := req.GetArguments()[key].(float64)
	if !ok {
		return def
	}
	return int(v)
}
