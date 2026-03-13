package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// endSessionResult is the response from end_session.
type endSessionResult struct {
	Status          string          `json:"status"`
	AgentID         string          `json:"agent_id"`
	SessionDuration string          `json:"session_duration,omitempty"`
	MemoriesSaved   int             `json:"memories_saved"`
	SessionSummary  *sessionSummary `json:"session_summary,omitempty"`
	MemoriesExpired int64           `json:"memories_expired"`
}

// sessionSummary captures the structured extraction from a session.
type sessionSummary struct {
	FilesTouched     []string `json:"files_touched,omitempty"`
	EntitiesExamined []string `json:"entities_examined,omitempty"`
	TasksUpdated     []string `json:"tasks_updated,omitempty"`
}

// handleEndSession captures session knowledge and persists it as memories.
// This is the key tool for the "coordination and memory infrastructure" pivot:
// agents call this at session end, and institutional knowledge accumulates
// automatically without manual remember() calls.
func (s *Server) handleEndSession(
	_ context.Context,
	req mcplib.CallToolRequest,
) (*mcplib.CallToolResult, error) {
	agentID, _ := req.GetArguments()["agent_id"].(string)
	if agentID == "" {
		return mcplib.NewToolResultError("agent_id is required"), nil
	}

	taskID, _ := req.GetArguments()["task_id"].(string)
	summary, _ := req.GetArguments()["summary"].(string)

	if s.store == nil {
		return mcplib.NewToolResultError("store not available"), nil
	}

	result := endSessionResult{
		Status:  "ok",
		AgentID: agentID,
	}

	var memoriesSaved int

	// ── Step 1: Structured extraction from events ──
	sessSummary := s.extractSessionSummary(agentID)
	result.SessionSummary = sessSummary

	// ── Step 2: Save session-log memory ──
	sessionContent := buildSessionLogContent(agentID, taskID, summary, sessSummary)
	if sessionContent != "" {
		_, err := s.store.InsertMemory(store.Memory{
			Tier:    store.TierSessionLog,
			Content: sessionContent,
			AgentID: agentID,
			TaskID:  taskID,
			Source:  store.SourceAuto,
			Tags:    `["session_end","auto"]`,
		})
		if err == nil {
			memoriesSaved++
		}
	}

	// ── Step 3: Extract entity memories ──
	// Only for entities examined AND whose files were modified (anti-noise).
	// FilesTouched is already the agent-attributed intersection, computed in
	// extractSessionSummary. Reuse it here instead of querying watcher again.
	if sessSummary != nil {
		for _, entityName := range sessSummary.EntitiesExamined {
			// Best-effort lookup: find the node by name.
			nodes := s.graph.FindByName(entityName)
			if len(nodes) == 0 {
				continue
			}
			node := nodes[0]
			if !containsFile(sessSummary.FilesTouched, node.File) {
				continue
			}

			content := fmt.Sprintf("Agent %s examined and modified %s", agentID, node.Name)
			if summary != "" {
				content += ": " + summary
			}

			_, err := s.store.InsertMemory(store.Memory{
				Tier:     store.TierEntity,
				Content:  content,
				EntityID: string(node.ID),
				AgentID:  agentID,
				TaskID:   taskID,
				Source:   store.SourceAuto,
				Tags:     `["session_end","entity_change","auto"]`,
			})
			if err == nil {
				memoriesSaved++
			}
		}
	}

	// ── Step 4: Save user-provided summary as project memory ──
	if summary != "" && len(summary) >= 10 {
		_, err := s.store.InsertMemory(store.Memory{
			Tier:    store.TierProject,
			Content: summary,
			AgentID: agentID,
			TaskID:  taskID,
			Source:  store.SourceManual,
			Tags:    `["session_end","summary"]`,
		})
		if err == nil {
			memoriesSaved++
		}
	}

	// ── Step 5: Run memory expiry ──
	expired, _ := s.store.ExpireMemories()
	result.MemoriesExpired = expired
	result.MemoriesSaved = memoriesSaved

	// ── Step 6: Compute session duration from agent registry ──
	if agents, err := s.store.GetAgents(); err == nil {
		for _, a := range agents {
			if a.ID == agentID {
				if start, err := time.Parse(time.RFC3339, a.LastSeen); err == nil {
					result.SessionDuration = time.Since(start).Round(time.Second).String()
				}
				break
			}
		}
	}

	return jsonResult(result)
}

// extractSessionSummary collects structured data about what happened during
// this agent's session from the event log.
func (s *Server) extractSessionSummary(agentID string) *sessionSummary {
	summary := &sessionSummary{}

	if s.store == nil {
		return summary
	}

	// Get recent events (all types, last 200).
	events, _, err := s.store.GetEvents(0, nil, 200)
	if err != nil {
		return summary
	}

	entitiesSet := make(map[string]bool)
	tasksSet := make(map[string]bool)

	for _, ev := range events {
		// Only process events attributed to this agent.
		if ev.AgentID != agentID {
			continue
		}
		switch ev.Type {
		case "agent_examining":
			var payload struct {
				Entity string `json:"entity"`
			}
			if json.Unmarshal([]byte(ev.Payload), &payload) == nil && payload.Entity != "" {
				entitiesSet[payload.Entity] = true
			}
		case "task_updated":
			var payload struct {
				TaskID string `json:"task_id"`
			}
			if json.Unmarshal([]byte(ev.Payload), &payload) == nil && payload.TaskID != "" {
				tasksSet[payload.TaskID] = true
			}
		}
	}

	// Derive FilesTouched from entities this agent examined whose files were
	// recently modified. This gives proper agent attribution: file_change events
	// from the watcher are unattributed (any agent or external tool), so we
	// intersect agent-examined entities with the watcher's recent-change list.
	modifiedFiles := s.getRecentlyModifiedFiles()
	filesSet := make(map[string]bool)
	for entityName := range entitiesSet {
		nodes := s.graph.FindByName(entityName)
		if len(nodes) > 0 && nodes[0].File != "" && containsFile(modifiedFiles, nodes[0].File) {
			filesSet[nodes[0].File] = true
		}
	}

	for f := range filesSet {
		summary.FilesTouched = append(summary.FilesTouched, f)
	}
	for e := range entitiesSet {
		summary.EntitiesExamined = append(summary.EntitiesExamined, e)
	}
	for t := range tasksSet {
		summary.TasksUpdated = append(summary.TasksUpdated, t)
	}

	return summary
}

// buildSessionLogContent creates a concise structured log of the session.
func buildSessionLogContent(agentID, taskID, summary string, sess *sessionSummary) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Session by %s", agentID))
	if taskID != "" {
		b.WriteString(fmt.Sprintf(" (task: %s)", taskID))
	}
	b.WriteString(fmt.Sprintf(" at %s.", time.Now().UTC().Format("2006-01-02 15:04")))

	if sess != nil {
		if len(sess.FilesTouched) > 0 {
			b.WriteString(fmt.Sprintf(" Files: %s.", strings.Join(truncateSlice(sess.FilesTouched, 5), ", ")))
		}
		if len(sess.EntitiesExamined) > 0 {
			b.WriteString(fmt.Sprintf(" Examined: %s.", strings.Join(truncateSlice(sess.EntitiesExamined, 5), ", ")))
		}
		if len(sess.TasksUpdated) > 0 {
			b.WriteString(fmt.Sprintf(" Tasks: %s.", strings.Join(sess.TasksUpdated, ", ")))
		}
	}

	if summary != "" {
		b.WriteString(" Summary: " + summary)
	}

	content := b.String()
	if len(content) < 10 {
		return ""
	}
	return content
}

// getRecentlyModifiedFiles returns files changed recently (from watcher events).
func (s *Server) getRecentlyModifiedFiles() []string {
	if s.changeSource == nil {
		return nil
	}
	changes := s.changeSource.RecentChanges(30) // last 30 minutes
	files := make([]string, 0, len(changes))
	for _, c := range changes {
		files = append(files, c.File)
	}
	return files
}

// containsFile checks if a file path is in the list (suffix match).
func containsFile(files []string, file string) bool {
	for _, f := range files {
		if f == file || strings.HasSuffix(f, "/"+file) || strings.HasSuffix(file, "/"+f) {
			return true
		}
	}
	return false
}

// truncateSlice returns at most n items from s, appending "..." if truncated.
func truncateSlice(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	result := make([]string, n+1)
	copy(result, s[:n])
	result[n] = fmt.Sprintf("(+%d more)", len(s)-n)
	return result
}

// Ensure graph.NodeID is used (imported for FindByName return type).
var _ graph.NodeID
