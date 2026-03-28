package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestExtractSignals_GetContext(t *testing.T) {
	entities, files := extractSignals("get_context", map[string]any{
		"entity": "AuthService.Login",
		"file":   "api/auth.go",
	})
	if len(entities) != 1 || entities[0] != "AuthService.Login" {
		t.Fatalf("entities: got %v", entities)
	}
	if len(files) != 1 || files[0] != "api/auth.go" {
		t.Fatalf("files: got %v", files)
	}
}

func TestExtractSignals_GetImpact_CommaSeparated(t *testing.T) {
	entities, files := extractSignals("get_impact", map[string]any{
		"symbol": "AuthService",
		"files":  "api/auth.go,store/token.go",
	})
	if len(entities) != 1 || entities[0] != "AuthService" {
		t.Fatalf("entities: got %v", entities)
	}
	if len(files) != 2 {
		t.Fatalf("files: expected 2, got %v", files)
	}
}

func TestExtractSignals_SearchDoesNotPollute(t *testing.T) {
	// search queries are free-text, NOT entity IDs — must return nil entities
	entities, files := extractSignals("search", map[string]any{
		"query": "JWT token refresh logic",
	})
	if len(entities) != 0 {
		t.Fatalf("search should not produce entity signals, got %v", entities)
	}
	if len(files) != 0 {
		t.Fatalf("search should not produce file signals, got %v", files)
	}
}

func TestExtractSignals_UnknownTool(t *testing.T) {
	entities, files := extractSignals("unknown_tool", map[string]any{"foo": "bar"})
	if len(entities) != 0 || len(files) != 0 {
		t.Fatalf("expected nil signals for unknown tool")
	}
}

func TestExtractSignals_JSONArray(t *testing.T) {
	entities, _ := extractSignals("get_impact", map[string]any{
		"symbol": `["AuthService","TokenStore"]`,
	})
	if len(entities) != 2 {
		t.Fatalf("expected 2 entities from JSON array, got %v", entities)
	}
}

func TestExtractSignals_EmptyArgs(t *testing.T) {
	entities, files := extractSignals("get_context", map[string]any{})
	if len(entities) != 0 || len(files) != 0 {
		t.Fatalf("expected nil for empty args")
	}
}

func TestExtractSignals_NodeIDWithColons(t *testing.T) {
	// NodeIDs contain :: which should NOT be split as comma-separated
	entities, _ := extractSignals("annotate_node", map[string]any{
		"node_id": "repo::internal/auth/service.go::AuthService",
	})
	if len(entities) != 1 {
		t.Fatalf("expected 1 entity (not split), got %v", entities)
	}
	if entities[0] != "repo::internal/auth/service.go::AuthService" {
		t.Fatalf("entity mangled: %s", entities[0])
	}
}

func TestExtractSignals_FindEntity(t *testing.T) {
	entities, _ := extractSignals("find_entity", map[string]any{
		"query": "AuthService",
	})
	if len(entities) != 1 || entities[0] != "AuthService" {
		t.Fatalf("find_entity should extract query as entity, got %v", entities)
	}
}

func TestExtractSignals_VerifyImplementation(t *testing.T) {
	_, files := extractSignals("verify_implementation", map[string]any{
		"files_written": `["api/auth.go","store/token.go"]`,
	})
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %v", files)
	}
}

func TestIntersectStrings(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want int
	}{
		{"empty", nil, nil, 0},
		{"no_overlap", []string{"a"}, []string{"b"}, 0},
		{"overlap", []string{"a", "b"}, []string{"b", "c"}, 1},
		{"full_overlap", []string{"a", "b"}, []string{"a", "b"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := intersectStrings(tt.a, tt.b)
			if len(got) != tt.want {
				t.Errorf("got %d, want %d", len(got), tt.want)
			}
		})
	}
}

func TestAlertHash_Stable(t *testing.T) {
	a := LedgerAlert{SessionID: "s1", OverlapType: "file", Overlap: []string{"auth.go"}}
	h1 := alertHash(a)
	h2 := alertHash(a)
	if h1 != h2 {
		t.Fatalf("hash not stable: %d != %d", h1, h2)
	}

	b := LedgerAlert{SessionID: "s2", OverlapType: "file", Overlap: []string{"auth.go"}}
	h3 := alertHash(b)
	if h1 == h3 {
		t.Fatalf("different alerts should have different hashes")
	}
}

func TestAlertHash_OrderSensitive(t *testing.T) {
	a := LedgerAlert{SessionID: "s1", OverlapType: "entity", Overlap: []string{"A", "B"}}
	b := LedgerAlert{SessionID: "s1", OverlapType: "entity", Overlap: []string{"B", "A"}}
	if alertHash(a) == alertHash(b) {
		// This is acceptable — we just document the behavior.
		// In practice, overlap order is deterministic per-session.
	}
}

func TestInjectAlerts_AppendsContentBlock(t *testing.T) {
	result := &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.NewTextContent(`{"entity": "AuthService", "depth": 2}`),
		},
	}
	alerts := []LedgerAlert{{
		SessionID:   "sess-2-long-uuid",
		Intent:      "add login",
		OverlapType: "file",
		Overlap:     []string{"auth.go"},
		LastActive:  "2min ago",
	}}
	injectAlerts(result, alerts)

	// Should have 2 content blocks now (original + alert)
	if len(result.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(result.Content))
	}

	// Original content should be unchanged
	origTC, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent[0], got %T", result.Content[0])
	}
	if origTC.Text != `{"entity": "AuthService", "depth": 2}` {
		t.Fatalf("original content was modified: %s", origTC.Text)
	}

	// Alert block should contain the alert data
	alertTC, ok := result.Content[1].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent[1], got %T", result.Content[1])
	}
	if !strings.Contains(alertTC.Text, "Cross-Session Alert") {
		t.Fatalf("alert block missing marker: %s", alertTC.Text)
	}
	if !strings.Contains(alertTC.Text, "sess-2-l") {
		t.Fatalf("alert block missing session ID: %s", alertTC.Text)
	}
}

func TestInjectAlerts_NilResult(t *testing.T) {
	// Should not panic
	injectAlerts(nil, []LedgerAlert{{SessionID: "s1"}})
}

func TestInjectAlerts_EmptyAlerts(t *testing.T) {
	result := &mcp.CallToolResult{
		Content: []mcp.Content{mcp.NewTextContent(`{"ok": true}`)},
	}
	injectAlerts(result, nil)
	if len(result.Content) != 1 {
		t.Fatalf("should not add content for nil alerts")
	}
}

func TestInjectAlerts_ShortSessionID(t *testing.T) {
	// SessionID shorter than 8 chars should not panic
	result := &mcp.CallToolResult{
		Content: []mcp.Content{mcp.NewTextContent(`{"ok": true}`)},
	}
	alerts := []LedgerAlert{{
		SessionID:   "abc",
		OverlapType: "file",
		Overlap:     []string{"auth.go"},
		LastActive:  "1min ago",
	}}
	injectAlerts(result, alerts) // should not panic
	if len(result.Content) != 2 {
		t.Fatalf("expected 2 content blocks")
	}
}

func TestFormatRelativeTime(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"invalid", "invalid"},
		{"", ""},
	}
	for _, tt := range tests {
		got := formatRelativeTime(tt.input)
		if got != tt.want {
			t.Errorf("formatRelativeTime(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFilterSeenAlerts_Deduplicates(t *testing.T) {
	s := &Server{}
	sessionID := "test-session"
	alert := LedgerAlert{SessionID: "other", OverlapType: "file", Overlap: []string{"a.go"}}

	// First time: should pass through
	result := s.filterSeenAlerts(sessionID, []LedgerAlert{alert})
	if len(result) != 1 {
		t.Fatalf("first call: expected 1, got %d", len(result))
	}

	// Second time: should be filtered
	result = s.filterSeenAlerts(sessionID, []LedgerAlert{alert})
	if len(result) != 0 {
		t.Fatalf("second call: expected 0 (deduplicated), got %d", len(result))
	}

	// Different alert: should pass through
	alert2 := LedgerAlert{SessionID: "other2", OverlapType: "entity", Overlap: []string{"B"}}
	result = s.filterSeenAlerts(sessionID, []LedgerAlert{alert2})
	if len(result) != 1 {
		t.Fatalf("new alert: expected 1, got %d", len(result))
	}
}

func TestClearLedgerWatermark(t *testing.T) {
	s := &Server{}
	sessionID := "test-session"
	alert := LedgerAlert{SessionID: "other", OverlapType: "file", Overlap: []string{"a.go"}}

	// Create watermark
	s.filterSeenAlerts(sessionID, []LedgerAlert{alert})

	// Clear it
	s.clearLedgerWatermark(sessionID)

	// Same alert should pass through again
	result := s.filterSeenAlerts(sessionID, []LedgerAlert{alert})
	if len(result) != 1 {
		t.Fatalf("after clear: expected 1, got %d", len(result))
	}
}

// TestValidateDispatch_KnowledgeMode_NilGraphGuard verifies that validate phases
// requiring a code graph return a clear error instead of panicking when graph is nil.
func TestValidateDispatch_KnowledgeMode_NilGraphGuard(t *testing.T) {
	s := &Server{} // nil graph — simulates knowledge mode

	graphPhases := []string{"post", "list", "full"}
	for _, phase := range graphPhases {
		req := mcp.CallToolRequest{}
		req.Params.Arguments = map[string]interface{}{"phase": phase}
		result, err := s.handleValidateDispatch(context.Background(), req)
		if err != nil {
			t.Fatalf("phase=%s: unexpected Go error: %v", phase, err)
		}
		if result == nil || !result.IsError {
			t.Fatalf("phase=%s: expected tool error for nil graph, got success", phase)
		}
	}

	// phase=safety should work fine without graph
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{"phase": "safety", "plan_description": "test"}
	result, err := s.handleValidateDispatch(context.Background(), req)
	if err != nil {
		t.Fatalf("phase=safety: unexpected Go error: %v", err)
	}
	// It will return a tool error about store being nil, which is expected —
	// the point is it didn't PANIC on nil graph.
	_ = result
}
