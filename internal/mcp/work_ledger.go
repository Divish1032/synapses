package mcp

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ledgerWatermark tracks which cross-session alerts a session has already seen.
// Prevents the same alert from being injected into repeated tool calls.
type ledgerWatermark struct {
	mu              sync.Mutex
	seenAlertHashes map[uint64]bool
}

// LedgerAlert is a cross-session overlap notification injected into tool responses.
type LedgerAlert struct {
	SessionID   string   `json:"session"`
	AgentID     string   `json:"agent_id,omitempty"`
	Intent      string   `json:"intent,omitempty"`
	OverlapType string   `json:"overlap_type"` // "file", "entity", or "entity_neighbor"
	Overlap     []string `json:"overlap"`
	LastActive  string   `json:"last_active"`
}

// signalSpec defines which argument keys carry entity/file references for a given tool.
type signalSpec struct {
	entityKeys []string
	fileKeys   []string
}

// signalSpecs maps tool name → which arg keys carry signals.
//
// IMPORTANT: Only keys that contain actual entity names or file paths should be listed.
// Do NOT include free-text fields (like search queries) — they would pollute the
// ledger with garbage and cause false-positive overlaps.
//
// This map includes both current tool names AND future merged tool names (Phase 5).
// During the transition, both old and new names will match.
var signalSpecs = map[string]signalSpec{
	// Current tools (Phase 2 state)
	"session_init":          {entityKeys: nil, fileKeys: nil},
	"find_entity":           {entityKeys: []string{"query"}, fileKeys: nil},
	"search":                {entityKeys: nil, fileKeys: nil}, // search queries are free-text, NOT entity IDs
	"get_context":           {entityKeys: []string{"entity", "from", "to"}, fileKeys: []string{"file"}},
	"prepare_context":       {entityKeys: []string{"target"}, fileKeys: []string{"file"}},
	"get_file_context":      {entityKeys: nil, fileKeys: []string{"file"}},
	"get_impact":            {entityKeys: []string{"symbol"}, fileKeys: []string{"files"}},
	"get_call_chain":        {entityKeys: []string{"from", "to"}, fileKeys: nil},
	"validate_plan":         {entityKeys: nil, fileKeys: nil}, // changes are structural, not file signals
	"verify_implementation": {entityKeys: nil, fileKeys: []string{"files_written"}},
	"get_violations":        {entityKeys: nil, fileKeys: nil},
	"plan_context":          {entityKeys: []string{"target"}, fileKeys: nil},
	"remember":              {entityKeys: nil, fileKeys: []string{"affected_files"}}, // affected_nodes are IDs but rarely useful for overlap
	"recall":                {entityKeys: nil, fileKeys: nil},                        // search query, not entity
	"get_episodes":          {entityKeys: nil, fileKeys: nil},
	"check_plan_safety":     {entityKeys: nil, fileKeys: nil},
	"annotate_node":         {entityKeys: []string{"node_id"}, fileKeys: nil},
	"web_annotate":          {entityKeys: []string{"node_id"}, fileKeys: nil},
	"upsert_gap":            {entityKeys: []string{"node_id"}, fileKeys: nil},
	"get_gaps":              {entityKeys: []string{"node_id"}, fileKeys: []string{"file"}},
	"get_entity_history":    {entityKeys: []string{"entity"}, fileKeys: []string{"file"}},
	"create_plan":           {entityKeys: nil, fileKeys: nil},
	"get_pending_tasks":     {entityKeys: nil, fileKeys: nil},
	"get_my_tasks":          {entityKeys: nil, fileKeys: nil},
	"update_task":           {entityKeys: nil, fileKeys: nil},
	"save_session_state":    {entityKeys: nil, fileKeys: []string{"files_modified"}},
	"get_session_state":     {entityKeys: nil, fileKeys: nil},
	"get_plans":             {entityKeys: nil, fileKeys: nil},
	"link_task_nodes":       {entityKeys: []string{"node_ids"}, fileKeys: nil},
	"upsert_rule":           {entityKeys: nil, fileKeys: nil},
	"delete_rule":           {entityKeys: nil, fileKeys: nil},
	"get_rule_candidates":   {entityKeys: nil, fileKeys: nil},
	"upsert_adr":            {entityKeys: nil, fileKeys: []string{"linked_files"}},
	"get_adrs":              {entityKeys: nil, fileKeys: []string{"file"}},
	"end_session":           {entityKeys: nil, fileKeys: nil},
	"report_usage":          {entityKeys: nil, fileKeys: nil},
	"lookup_docs":           {entityKeys: []string{"entity"}, fileKeys: nil},
	// Future merged tool names (Phase 5) — pre-registered so signals work after merge
	"validate": {entityKeys: nil, fileKeys: []string{"files_written"}},
	"memory":   {entityKeys: nil, fileKeys: []string{"affected_files"}},
	"tasks":    {entityKeys: nil, fileKeys: nil},
	"rules":    {entityKeys: nil, fileKeys: nil},
	"annotate": {entityKeys: []string{"node_id", "entity"}, fileKeys: []string{"file"}},
}

// extractSignals parses tool call arguments for entity and file references.
// Returns nil slices if no signals are found or the tool is not mapped.
func extractSignals(toolName string, args map[string]any) (entityIDs, filePaths []string) {
	spec, ok := signalSpecs[toolName]
	if !ok {
		return nil, nil
	}
	entityIDs = extractStringArgs(args, spec.entityKeys)
	filePaths = extractStringArgs(args, spec.fileKeys)
	return entityIDs, filePaths
}

// extractStringArgs extracts string values from args for the given keys.
// Handles single string values, JSON array strings, and comma-separated strings.
func extractStringArgs(args map[string]any, keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	var result []string
	for _, key := range keys {
		val, ok := args[key]
		if !ok {
			continue
		}
		switch v := val.(type) {
		case string:
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			// Try JSON array first
			if strings.HasPrefix(v, "[") {
				var arr []string
				if json.Unmarshal([]byte(v), &arr) == nil {
					for _, s := range arr {
						s = strings.TrimSpace(s)
						if s != "" {
							result = append(result, s)
						}
					}
					continue
				}
			}
			// Comma-separated (only for file lists, not entity names which may contain commas)
			if strings.Contains(v, ",") && !strings.Contains(v, "::") {
				for _, s := range strings.Split(v, ",") {
					s = strings.TrimSpace(s)
					if s != "" {
						result = append(result, s)
					}
				}
				continue
			}
			result = append(result, v)
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					result = append(result, strings.TrimSpace(s))
				}
			}
		}
	}
	return result
}

// checkOverlaps finds cross-session overlaps for the current tool call.
// Only returns Tier 1 (same entity/file) and Tier 2 (1-hop graph neighbor) overlaps.
// Returns nil if no overlaps found or if store is unavailable.
func (s *Server) checkOverlaps(sessionID string, myEntities, myFiles []string) []LedgerAlert {
	if s.store == nil || (len(myEntities) == 0 && len(myFiles) == 0) {
		return nil
	}
	others, err := s.store.ActiveSessionWork(s.projectID, sessionID, 15)
	if err != nil || len(others) == 0 {
		return nil
	}

	var alerts []LedgerAlert
	for _, other := range others {
		// Tier 1: exact file overlap (highest confidence)
		commonFiles := intersectStrings(myFiles, other.FilePaths)
		// Tier 1: exact entity overlap
		commonEntities := intersectStrings(myEntities, other.EntityIDs)

		overlapType := ""
		var overlap []string
		if len(commonFiles) > 0 {
			overlapType = "file"
			overlap = commonFiles
		} else if len(commonEntities) > 0 {
			overlapType = "entity"
			overlap = commonEntities
		}

		// Tier 2: 1-hop graph neighbor (only if no Tier 1 match and graph available).
		// NOTE: This requires entity strings to be graph NodeIDs (repo::file::Name format).
		// Entity names from tool args are usually short names, so this may not match.
		// That's acceptable — Tier 2 is best-effort, Tier 1 is the reliable path.
		if overlapType == "" && s.graph != nil && len(myEntities) > 0 && len(other.EntityIDs) > 0 {
			neighbors := s.graphNeighborIntersect(myEntities, other.EntityIDs)
			if len(neighbors) > 0 {
				overlapType = "entity_neighbor"
				overlap = neighbors
			}
		}

		if overlapType == "" {
			continue
		}
		alerts = append(alerts, LedgerAlert{
			SessionID:   other.SessionID,
			AgentID:     other.AgentID,
			Intent:      other.Intent,
			OverlapType: overlapType,
			Overlap:     overlap,
			LastActive:  formatRelativeTime(other.LastActive),
		})
	}
	return alerts
}

// graphNeighborIntersect checks if any entity in setA is a direct graph neighbor
// (1-hop via any edge) of any entity in setB. Returns the matching entity pairs.
//
// Entity strings may be short names ("AuthService") or full NodeIDs ("repo::file::AuthService").
// Only full NodeIDs will match graph lookups. Short names are attempted but will
// return no neighbors (DirectNeighbors returns nil for unknown IDs) — this is safe.
func (s *Server) graphNeighborIntersect(setA, setB []string) []string {
	if s.graph == nil {
		return nil
	}
	bSet := make(map[graph.NodeID]struct{}, len(setB))
	for _, b := range setB {
		bSet[graph.NodeID(b)] = struct{}{}
	}
	var matches []string
	for _, a := range setA {
		neighbors := s.graph.DirectNeighbors(graph.NodeID(a))
		for _, n := range neighbors {
			if _, ok := bSet[n]; ok {
				matches = append(matches, fmt.Sprintf("%s→%s", a, string(n)))
				if len(matches) >= 3 {
					return matches // cap to prevent noise
				}
			}
		}
	}
	return matches
}

// filterSeenAlerts removes alerts that this session has already seen.
// Thread-safe via per-session mutex.
func (s *Server) filterSeenAlerts(sessionID string, alerts []LedgerAlert) []LedgerAlert {
	if len(alerts) == 0 {
		return nil
	}
	wmI, _ := s.ledgerWatermarks.LoadOrStore(sessionID, &ledgerWatermark{
		seenAlertHashes: make(map[uint64]bool),
	})
	wm := wmI.(*ledgerWatermark)
	wm.mu.Lock()
	defer wm.mu.Unlock()

	var newAlerts []LedgerAlert
	for _, alert := range alerts {
		h := alertHash(alert)
		if wm.seenAlertHashes[h] {
			continue
		}
		wm.seenAlertHashes[h] = true
		newAlerts = append(newAlerts, alert)
	}
	return newAlerts
}

// clearLedgerWatermark removes the watermark for a session (called on session end).
// Prevents unbounded growth of the watermarks map.
func (s *Server) clearLedgerWatermark(sessionID string) {
	s.ledgerWatermarks.Delete(sessionID)
}

// injectAlerts appends cross_session_alerts to a JSON tool result.
// Enforces 200-token (~800 byte) hard cap on injected content.
//
// IMPORTANT: Instead of re-serializing the entire JSON response (which would change
// field ordering and numeric precision), we append the alerts as a separate text block.
// This preserves the original response byte-for-byte.
func injectAlerts(result *mcp.CallToolResult, alerts []LedgerAlert) {
	if result == nil || len(alerts) == 0 || len(result.Content) == 0 {
		return
	}

	// Build the alerts JSON
	alertsJSON, err := json.Marshal(alerts)
	if err != nil {
		return // silently skip on marshal error
	}

	// Hard cap: ~800 bytes (roughly 200 tokens)
	const maxAlertBytes = 800
	if len(alertsJSON) > maxAlertBytes {
		// Summarize instead
		var summaryParts []string
		for _, a := range alerts {
			sid := a.SessionID
			if len(sid) > 8 {
				sid = sid[:8]
			}
			summaryParts = append(summaryParts, fmt.Sprintf("Session %s (%s) overlaps on %s: %s",
				sid, a.Intent, a.OverlapType, strings.Join(a.Overlap, ", ")))
		}
		summary := strings.Join(summaryParts, "; ")
		if len(summary) > maxAlertBytes {
			summary = summary[:maxAlertBytes-3] + "..."
		}
		alertsJSON, _ = json.Marshal(map[string]string{"summary": summary})
	}

	// Append as a NEW content block rather than modifying existing content.
	// This preserves the original response byte-for-byte — no JSON re-serialization,
	// no field reordering, no numeric precision loss.
	alertText := "\n⚠️ Cross-Session Alert: " + string(alertsJSON)
	result.Content = append(result.Content, mcp.NewTextContent(alertText))
}

// alertHash computes a stable FNV-1a hash for deduplication.
// Two alerts with the same session + overlap type + overlap items produce the same hash.
func alertHash(a LedgerAlert) uint64 {
	h := fnv.New64a()
	h.Write([]byte(a.SessionID))
	h.Write([]byte{0}) // separator
	h.Write([]byte(a.OverlapType))
	h.Write([]byte{0})
	for _, o := range a.Overlap {
		h.Write([]byte(o))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// intersectStrings returns elements present in both slices.
// Returns nil if either slice is empty.
func intersectStrings(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(b))
	for _, s := range b {
		set[s] = struct{}{}
	}
	var result []string
	for _, s := range a {
		if _, ok := set[s]; ok {
			result = append(result, s)
		}
	}
	return result
}

// formatRelativeTime converts an ISO timestamp to a human-readable relative time.
// Tries multiple common formats. Falls back to the raw string on parse failure.
func formatRelativeTime(ts string) string {
	formats := []string{
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
		time.RFC3339,
		"2006-01-02 15:04:05",
	}
	var t time.Time
	var err error
	for _, f := range formats {
		t, err = time.Parse(f, ts)
		if err == nil {
			break
		}
	}
	if err != nil {
		return ts
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dmin ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}
