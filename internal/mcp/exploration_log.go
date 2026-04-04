package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// explorationCapture holds the structured intelligence extracted from a tool
// response for storage in the exploration log. All fields are natural language
// or entity names — never raw source code.
type explorationCapture struct {
	EntityQueried  string // primary entity / symbol / query term
	QueryContext   string // intent, mode, or supplemental context (≤100 chars)
	FindingSummary string // NL summary of what was found (≤300 chars)
}

// explorationToolSet is the set of tools whose responses are intercepted for
// exploration log capture. Only exploration-heavy tools are included.
// session_init, tasks, end_session are excluded — they don't produce
// "what was found" findings relevant to compaction recovery or the Archivist.
// memory(action=save) is included so explicit decisions flow into the
// deterministic Archivist as "decision recorded" entries (Sprint 30.2).
var explorationToolSet = map[string]bool{
	"get_context": true,
	"search":      true,
	"get_impact":  true,
	"validate":    true,
	"memory":      true,
}

// extractExplorationCapture parses tool call args and result to produce a
// compact description of what was queried and what was found. Returns nil
// when:
//   - the tool is not in explorationToolSet
//   - the result is nil, an error, or has no content
//   - the response is a cache-hit "unchanged" reply (no new findings)
//
// This function is called on the hot path (inside ledgerWrapped) so it must
// be fast and allocation-light. JSON parsing is bounded to the first content
// block only.
func extractExplorationCapture(toolName string, args map[string]any, result *mcp.CallToolResult) *explorationCapture {
	if !explorationToolSet[toolName] {
		return nil
	}
	if result == nil || result.IsError || len(result.Content) == 0 {
		return nil
	}

	// Extract the first text content block.
	text := firstTextContent(result)
	if text == "" {
		return nil
	}

	switch toolName {
	case "get_context":
		return extractGetContextCapture(args, text)
	case "search":
		return extractSearchCapture(args, text)
	case "get_impact":
		return extractGetImpactCapture(args, text)
	case "validate":
		return extractValidateCapture(args, text)
	case "memory":
		return extractMemorySaveCapture(args)
	}
	return nil
}

// ── Per-tool extraction ───────────────────────────────────────────────────────

// extractGetContextCapture extracts findings from a get_context response.
// Captures entity name, caller/callee counts, and security constraint presence.
// Returns nil for cache-hit responses ({unchanged: true}).
func extractGetContextCapture(args map[string]any, responseText string) *explorationCapture {
	entity, _ := args["entity"].(string)
	intent, _ := args["intent"].(string)
	if entity == "" {
		entity, _ = args["from"].(string) // path mode
	}
	if entity == "" {
		return nil
	}

	// Parse the response JSON to extract structural counts.
	var resp map[string]any
	if err := json.Unmarshal([]byte(responseText), &resp); err != nil {
		// Even without response parsing, we have the entity — record a basic entry.
		return &explorationCapture{
			EntityQueried: capStr(entity, 100),
			QueryContext:  capStr(intent, 100),
		}
	}

	// Cache hit: agent already has this context — no new findings to record.
	if unchanged, _ := resp["unchanged"].(bool); unchanged {
		return nil
	}

	// Extract entity name from root (may be more precise than the query term).
	entityName := entity
	if root, ok := resp["root"].(map[string]any); ok {
		if name, _ := root["name"].(string); name != "" {
			entityName = name
		}
	}

	calleeCount := countSlice(resp["callees"])
	callerCount := countSlice(resp["callers"])

	// Security constraints presence.
	constraintNote := ""
	if enrich, ok := resp["enrichment"].(map[string]any); ok {
		if sc, ok := enrich["security_constraints"].([]any); ok && len(sc) > 0 {
			constraintNote = fmt.Sprintf(", %d security constraint(s)", len(sc))
		} else if ra, ok := enrich["rule_alerts"].([]any); ok && len(ra) > 0 {
			constraintNote = fmt.Sprintf(", %d rule alert(s)", len(ra))
		}
	}

	summary := fmt.Sprintf("%s: %d caller(s), %d callee(s)%s",
		entityName, callerCount, calleeCount, constraintNote)

	return &explorationCapture{
		EntityQueried:  capStr(entityName, 100),
		QueryContext:   capStr(intent, 100),
		FindingSummary: capStr(summary, 300),
	}
}

// extractSearchCapture extracts findings from a search response.
// Uses the _summary field (already NL-formatted by handleSearch) and top entity names.
func extractSearchCapture(args map[string]any, responseText string) *explorationCapture {
	query, _ := args["query"].(string)
	if query == "" {
		return nil
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(responseText), &resp); err != nil {
		return &explorationCapture{
			EntityQueried: capStr(query, 100),
		}
	}

	// Extract the pre-built NL summary from handleSearch.
	summary, _ := resp["_summary"].(string)

	// Augment with top entity names so the recovery packet has concrete names.
	if results, ok := resp["results"].([]any); ok && len(results) > 0 {
		var topNames []string
		for i, r := range results {
			if i >= 3 {
				break
			}
			if rMap, ok := r.(map[string]any); ok {
				if name, _ := rMap["name"].(string); name != "" {
					topNames = append(topNames, name)
				}
			}
		}
		if len(topNames) > 0 {
			if summary != "" {
				summary += ": " + strings.Join(topNames, ", ")
			} else {
				summary = "top results: " + strings.Join(topNames, ", ")
			}
		}
	}

	return &explorationCapture{
		EntityQueried:  capStr(query, 100),
		FindingSummary: capStr(summary, 300),
	}
}

// extractGetImpactCapture extracts findings from a get_impact response.
// Uses the blast_radius_summary NL field (built by buildImpactNL in Sprint 23.3).
func extractGetImpactCapture(args map[string]any, responseText string) *explorationCapture {
	symbol, _ := args["symbol"].(string)
	if symbol == "" {
		symbol, _ = args["files"].(string) // file-based impact mode
	}
	if symbol == "" {
		return nil
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(responseText), &resp); err != nil {
		return &explorationCapture{
			EntityQueried: capStr(symbol, 100),
		}
	}

	// blast_radius_summary is the NL field built by buildImpactNL (Sprint 23.3).
	summary, _ := resp["blast_radius_summary"].(string)
	if summary == "" {
		// Fallback: synthesise from total_affected count.
		if total, ok := resp["total_affected"].(float64); ok {
			summary = fmt.Sprintf("%s affects %d entity(s)", symbol, int(total))
		}
	}

	return &explorationCapture{
		EntityQueried:  capStr(symbol, 100),
		FindingSummary: capStr(summary, 300),
	}
}

// extractValidateCapture extracts findings from a validate response.
// Only captures for phase=post (post-write audit) — pre-write and list phases
// are less discovery-oriented.
func extractValidateCapture(args map[string]any, responseText string) *explorationCapture {
	phase, _ := args["phase"].(string)
	if phase != "post" && phase != "list" {
		return nil
	}

	// Entity queried: the files being validated.
	filesParam, _ := args["files_written"].(string)
	if filesParam == "" {
		filesParam = "files"
	}
	entity := capStr(filesParam, 100)

	var resp map[string]any
	if err := json.Unmarshal([]byte(responseText), &resp); err != nil {
		return &explorationCapture{
			EntityQueried: entity,
			QueryContext:  phase,
		}
	}

	// Count violations and their severities.
	violations, ok := resp["violations"].([]any)
	if !ok {
		// Some validate responses wrap violations under a "results" key.
		violations, _ = resp["results"].([]any)
	}

	var critCount, highCount, medCount int
	for _, v := range violations {
		if vMap, ok := v.(map[string]any); ok {
			switch strings.ToUpper(fmt.Sprint(vMap["severity"])) {
			case "CRITICAL", "ERROR":
				critCount++
			case "HIGH", "WARNING":
				highCount++
			default:
				medCount++
			}
		}
	}

	var summary string
	total := len(violations)
	if total == 0 {
		summary = "no violations found"
	} else {
		parts := []string{fmt.Sprintf("%d violation(s)", total)}
		if critCount > 0 {
			parts = append(parts, fmt.Sprintf("%d CRITICAL", critCount))
		}
		if highCount > 0 {
			parts = append(parts, fmt.Sprintf("%d HIGH", highCount))
		}
		if medCount > 0 {
			parts = append(parts, fmt.Sprintf("%d MEDIUM", medCount))
		}
		summary = strings.Join(parts, ": ")
	}

	return &explorationCapture{
		EntityQueried:  entity,
		QueryContext:   phase,
		FindingSummary: capStr(summary, 300),
	}
}

// extractMemorySaveCapture captures an explicit memory(action=save) call so
// that decisions recorded by the agent flow into the deterministic Archivist
// as "decision recorded" entries (Sprint 30.2). Only fires for action=save;
// other memory actions (search, list, hypothesize, decide, abandon) are ignored.
//
// We read directly from args (not the response) because the response only
// confirms the save — the semantically useful data is in the request.
func extractMemorySaveCapture(args map[string]any) *explorationCapture {
	action, _ := args["action"].(string)
	if action != "save" {
		return nil
	}
	content, _ := args["content"].(string)
	if content == "" {
		return nil
	}
	key, _ := args["key"].(string)
	entityQueried := key
	if entityQueried == "" {
		entityQueried = "memory"
	}
	return &explorationCapture{
		EntityQueried:  capStr(entityQueried, 100),
		QueryContext:   "save",
		FindingSummary: capStr(content, 300),
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// firstTextContent returns the text of the first text content block in the result.
// Returns "" if none found or if the content is not text.
func firstTextContent(result *mcp.CallToolResult) string {
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok && tc.Text != "" {
			return tc.Text
		}
	}
	return ""
}

// countSlice counts the length of an []any value from a parsed JSON map.
// Returns 0 if the value is absent or not a slice.
func countSlice(v any) int {
	if sl, ok := v.([]any); ok {
		return len(sl)
	}
	return 0
}

// capStr truncates s to at most maxLen runes (UTF-8 safe).
// Returns s unchanged when maxLen <= 0 or s is already within the limit.
func capStr(s string, maxLen int) string {
	if maxLen <= 0 || s == "" {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}
