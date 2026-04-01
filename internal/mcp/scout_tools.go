package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/pulse"
)

// Sprint 23.9: lookup_docs removed — agent browses docs itself.
// allowedDocsDomains, isAllowedDocsURL, handleLookupDocs, lookupPackageDocs,
// lookupURL, lookupEntityDocs removed along with the tool registration.

// handleWebAnnotate persists web findings as a graph node annotation so they
// survive across sessions and appear in get_context for that node.
// This is the "context sharing" pattern — web findings become first-class
// data objects attached to code entities.
// Sprint 23.9: exposed as memory(action="annotate_web").
func (s *Server) handleWebAnnotate(
	ctx context.Context,
	req mcpgo.CallToolRequest,
) (*mcpgo.CallToolResult, error) {
	if s.store == nil {
		return mcpgo.NewToolResultError("store not available (run synapses start, not synapses index)"), nil
	}

	nodeID, _ := req.GetArguments()["node_id"].(string)
	if nodeID == "" {
		return mcpgo.NewToolResultError("node_id is required (use search to get node IDs)"), nil
	}
	agentID, _ := req.GetArguments()["agent_id"].(string)
	note, noteErr := stringArgLimited(req, "note", maxArgLengthNote)
	if noteErr != nil {
		return mcpgo.NewToolResultError(stripInternalPaths(noteErr.Error())), nil
	}

	// Optional: structured hits JSON to format as a readable annotation.
	type searchHit struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
	}
	if hitsJSON, ok := req.GetArguments()["hits"].(string); ok && hitsJSON != "" {
		var hits []searchHit
		if err := json.Unmarshal([]byte(hitsJSON), &hits); err == nil && len(hits) > 0 {
			var sb strings.Builder
			sb.WriteString("[web findings]")
			if note != "" {
				sb.WriteString(" ")
				sb.WriteString(note)
			}
			for i, h := range hits {
				if i >= 5 {
					break
				}
				fmt.Fprintf(&sb, "\n  - [%s](%s)", h.Title, h.URL)
				if h.Snippet != "" {
					sb.WriteString(": ")
					sb.WriteString(h.Snippet)
				}
			}
			note = sb.String()
		}
	}

	if note == "" {
		return mcpgo.NewToolResultError("note or hits is required"), nil
	}

	// OF-S2: scan note content for prompt injection patterns.
	// web_annotate is highest risk — content originates from web pages.
	var injectionWarning string
	if scanResult, scanErr := s.scanContent("note", note); scanErr != nil {
		return mcpgo.NewToolResultError(stripInternalPaths(scanErr.Error())), nil
	} else {
		note = scanResult.sanitized
		if scanResult.warning != "" {
			injectionWarning = scanResult.warning
			// P7-1: emit guard event for injection scan trigger.
			if pc := s.getPulseClient(); pc != nil {
				pc.RecordGuardEvent(pulse.GuardEvent{
					GuardType: "injection_scan", ToolName: "web_annotate",
					Category: "warn", AgentID: agentID, ProjectID: s.projectID,
				})
			}
		}
	}

	id, err := s.store.AddAnnotation(nodeID, agentID, note)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("store annotation failed: %v", stripInternalPaths(err.Error()))), nil
	}
	// P7-12: emit memory op for web annotation write.
	if pc := s.getPulseClient(); pc != nil {
		pc.RecordMemoryOp(pulse.MemoryOperationEvent{
			Operation: "web_annotation_write", Tier: "entity",
			ResultCount: 1, AgentID: agentID, ProjectID: s.projectID,
		})
	}
	_ = ctx
	resp := map[string]interface{}{
		"id":      id,
		"node_id": nodeID,
		"note":    note,
		"status":  "annotated — visible in get_context for this node",
	}
	if injectionWarning != "" {
		resp["injection_warning"] = injectionWarning
	}
	return jsonResult(resp)
}
