// Package mcp — Sprint 27.3: Reactive tool guidance in tool responses.
//
// Enriches suggestNextAfterContext with SDLC-phase-aware hints and suppresses
// tools the agent has already called frequently this session. Also provides
// suggestion functions for validate and search responses.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/config"
)

// metaTools are session management tools excluded from suggestion suppression.
var metaTools = map[string]bool{
	"session_init": true,
	"end_session":  true,
}

// ── Sprint 24.7: Proactive memory save nudge ─────────────────────────────────

// saveNudgeThreshold is the number of non-meta tool calls without a memory write
// that triggers the nudge. The nudge re-arms after another saveNudgeThreshold
// calls pass without a save (suppressible by calling memory with a write action).
const saveNudgeThreshold = 10

// explorationTools are counted separately in the nudge message so the agent
// gets specific context: "you've explored N entities".
var explorationTools = map[string]bool{
	"get_context": true,
	"search":      true,
	"get_impact":  true,
	"validate":    true,
}

// memorySaveWriteActions are memory actions that constitute a "save" and reset
// the nudge counter. Read-only actions (search, list, history, list_*) do NOT
// reset the counter — they don't persist new findings.
var memorySaveWriteActions = map[string]bool{
	"save":         true,
	"annotate":     true,
	"annotate_web": true,
	"hypothesize":  true,
	"decide":       true,
	"abandon":      true,
}

// nudgeEntry holds per-session nudge state.
type nudgeEntry struct {
	callsSinceSave     int       // non-meta calls since last memory write
	explorationSince   int       // exploration calls since last memory write
	lastNudgeCallCount int       // callsSinceSave value when the last nudge fired (0 = never)
	created            time.Time // for GC
}

// memorySaveNudger tracks tool calls since the last memory write per session.
// When an agent accumulates ≥ saveNudgeThreshold calls without persisting
// anything, the next tool response piggybacks a nudge. The nudge re-arms after
// another saveNudgeThreshold calls — it never fires more than once per threshold
// window.
type memorySaveNudger struct {
	mu      sync.RWMutex
	entries map[string]*nudgeEntry
	lastGC  time.Time
}

func newMemorySaveNudger() *memorySaveNudger {
	return &memorySaveNudger{
		entries: make(map[string]*nudgeEntry),
		lastGC:  time.Now(),
	}
}

// record processes one tool call for the given session. If toolName+args represent
// a memory write action the counter is reset and "" is returned. Otherwise the
// counter is incremented and a nudge message is returned when the threshold is
// crossed (empty string means no nudge needed yet).
func (n *memorySaveNudger) record(sessionID, toolName string, args map[string]any) string {
	if metaTools[toolName] {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()

	// Periodic GC — same cadence as sessionToolTracker.
	if time.Since(n.lastGC) > toolTrackerGCInterval {
		n.lastGC = time.Now()
		cutoff := time.Now().Add(-toolTrackerMaxAge)
		for sid, e := range n.entries {
			if e.created.Before(cutoff) {
				delete(n.entries, sid)
			}
		}
	}

	e, ok := n.entries[sessionID]
	if !ok {
		e = &nudgeEntry{created: time.Now()}
		n.entries[sessionID] = e
	}

	// Memory write → reset; no nudge for this call.
	if toolName == "memory" {
		if action, _ := args["action"].(string); memorySaveWriteActions[action] {
			e.callsSinceSave = 0
			e.explorationSince = 0
			e.lastNudgeCallCount = 0
			return ""
		}
	}

	// Increment counters.
	e.callsSinceSave++
	if explorationTools[toolName] {
		e.explorationSince++
	}

	// Nudge fires when threshold crossed AND has not fired in the current window.
	// "Current window" = calls since the last nudge (re-arms every saveNudgeThreshold).
	if e.callsSinceSave < saveNudgeThreshold {
		return ""
	}
	if e.callsSinceSave-e.lastNudgeCallCount < saveNudgeThreshold {
		return ""
	}
	e.lastNudgeCallCount = e.callsSinceSave
	return fmt.Sprintf(
		"You've explored %d entities and made %d tool calls this session without saving to memory. "+
			"Consider calling memory(action=save) to protect key findings against context loss.",
		e.explorationSince, e.callsSinceSave,
	)
}

// clear removes session state (called by end_session cleanup).
func (n *memorySaveNudger) clear(sessionID string) {
	n.mu.Lock()
	delete(n.entries, sessionID)
	n.mu.Unlock()
}

// sessionToolEntry holds per-session tool counts with a creation timestamp.
type sessionToolEntry struct {
	counts  map[string]int
	created time.Time
}

// sessionToolTracker counts how many times each tool has been called per session.
// Used to suppress suggestions for tools the agent already uses frequently.
// Entries are garbage-collected after 2 hours to prevent memory leaks from
// sessions that don't cleanly end.
type sessionToolTracker struct {
	mu     sync.RWMutex
	entries map[string]*sessionToolEntry // sessionID → entry
	lastGC  time.Time
}

const toolTrackerGCInterval = 30 * time.Minute
const toolTrackerMaxAge = 2 * time.Hour

func newSessionToolTracker() *sessionToolTracker {
	return &sessionToolTracker{
		entries: make(map[string]*sessionToolEntry),
		lastGC:  time.Now(),
	}
}

func (t *sessionToolTracker) record(sessionID, toolName string) {
	// Skip meta tools — they shouldn't count toward suggestion suppression.
	if metaTools[toolName] {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[sessionID]
	if !ok {
		e = &sessionToolEntry{
			counts:  make(map[string]int),
			created: time.Now(),
		}
		t.entries[sessionID] = e
	}
	e.counts[toolName]++
	// Periodic GC: remove entries older than 2 hours.
	if time.Since(t.lastGC) > toolTrackerGCInterval {
		t.lastGC = time.Now()
		cutoff := time.Now().Add(-toolTrackerMaxAge)
		for sid, entry := range t.entries {
			if entry.created.Before(cutoff) {
				delete(t.entries, sid)
			}
		}
	}
}

func (t *sessionToolTracker) get(sessionID string) map[string]int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e := t.entries[sessionID]
	if e == nil {
		return nil
	}
	cp := make(map[string]int, len(e.counts))
	for k, v := range e.counts {
		cp[k] = v
	}
	return cp
}

func (t *sessionToolTracker) clear(sessionID string) {
	t.mu.Lock()
	delete(t.entries, sessionID)
	t.mu.Unlock()
}

func (t *sessionToolTracker) totalCalls(sessionID string) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	total := 0
	if e, ok := t.entries[sessionID]; ok {
		for _, v := range e.counts {
			total += v
		}
	}
	return total
}

// suppressOverused filters out tool suggestions for tools called 3+ times this session.
// Also caps suggestions at maxSuggestions.
func suppressOverused(suggestions []toolSuggestion, sessionCounts map[string]int, maxSuggestions int) []toolSuggestion {
	if len(suggestions) == 0 {
		return suggestions
	}
	var filtered []toolSuggestion
	for _, s := range suggestions {
		if sessionCounts[s.Tool] >= 3 {
			continue
		}
		filtered = append(filtered, s)
		if len(filtered) >= maxSuggestions {
			break
		}
	}
	return filtered
}

// phaseSuggestions returns additional tool suggestions based on the current SDLC phase.
func phaseSuggestions(phase brain.SDLCPhase, entityName string) []toolSuggestion {
	switch phase {
	case brain.PhaseTesting:
		return []toolSuggestion{
			{Tool: "validate", Reason: "testing phase — verify test coverage and arch rules (phase=post)"},
		}
	case brain.PhaseReview:
		return []toolSuggestion{
			{Tool: "get_impact", Reason: "review phase — check blast radius before approving changes"},
		}
	case brain.PhasePlanning:
		return []toolSuggestion{
			{Tool: "search", Reason: "planning phase — explore related patterns across the codebase"},
		}
	case brain.PhaseDeployment:
		return []toolSuggestion{
			{Tool: "validate", Reason: "deployment phase — run full validation before deploying (phase=full)"},
		}
	}
	return nil
}

// suggestAfterValidate returns suggestions after a validate call.
func suggestAfterValidate(violationCount int, entityName string) []toolSuggestion {
	if violationCount == 0 {
		return nil
	}
	suggestions := []toolSuggestion{
		{
			Tool:   "get_context",
			Reason: fmt.Sprintf("%d violation(s) found — inspect violating entities for context", violationCount),
		},
	}
	return suggestions
}

// suggestAfterSearch returns suggestions after a search call with results.
func suggestAfterSearch(topEntityName string, resultCount int) []toolSuggestion {
	if resultCount == 0 || topEntityName == "" {
		return nil
	}
	return []toolSuggestion{
		{
			Tool:   "get_context",
			Reason: fmt.Sprintf("deep-dive into top result '%s' for full context", topEntityName),
		},
	}
}

// matchRuleCount returns how many architectural rules reference the given file path.
func matchRuleCount(cfg *config.Config, filePath string) int {
	if cfg == nil {
		return 0
	}
	return len(matchRulesForFile(cfg, filePath))
}

// injectSearchSuggestions appends a suggested_next_tools text block to a search
// result. Parses the result JSON to extract the top entity name and result count.
func (s *Server) injectSearchSuggestions(_ context.Context, result *mcp.CallToolResult, _ mcp.CallToolRequest) {
	if result == nil || result.IsError || len(result.Content) == 0 {
		return
	}
	// Extract result count and top entity from the JSON response.
	text, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		return
	}
	var parsed map[string]interface{}
	if json.Unmarshal([]byte(text.Text), &parsed) != nil {
		return
	}
	topEntity := ""
	resultCount := 0
	if results, ok := parsed["results"].([]interface{}); ok {
		resultCount = len(results)
		if len(results) > 0 {
			if first, ok := results[0].(map[string]interface{}); ok {
				if name, ok := first["name"].(string); ok {
					topEntity = name
				}
			}
		}
	}
	suggestions := suggestAfterSearch(topEntity, resultCount)
	if len(suggestions) == 0 {
		return
	}
	sugJSON, _ := json.Marshal(suggestions)
	result.Content = append(result.Content, mcp.TextContent{
		Type: "text",
		Text: fmt.Sprintf("\n---\nsuggested_next_tools: %s", string(sugJSON)),
	})
}

// injectValidateSuggestions appends a suggested_next_tools text block to a
// validate result. Extracts violation count from the JSON.
func (s *Server) injectValidateSuggestions(_ context.Context, result *mcp.CallToolResult, _ mcp.CallToolRequest) {
	if result == nil || result.IsError || len(result.Content) == 0 {
		return
	}
	text, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		return
	}
	// Count "violation" occurrences in the response text as a heuristic.
	violationCount := strings.Count(strings.ToLower(text.Text), "violation")
	suggestions := suggestAfterValidate(violationCount, "")
	if len(suggestions) == 0 {
		return
	}
	sugJSON, _ := json.Marshal(suggestions)
	result.Content = append(result.Content, mcp.TextContent{
		Type: "text",
		Text: fmt.Sprintf("\n---\nsuggested_next_tools: %s", string(sugJSON)),
	})
}
