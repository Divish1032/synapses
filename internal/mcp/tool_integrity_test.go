package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// TestHashToolDescs_Deterministic verifies that hashToolDescs produces the
// same digest regardless of map insertion order, and that different inputs
// produce different digests.
func TestHashToolDescs_Deterministic(t *testing.T) {
	a := map[string]string{"alpha": "desc-a", "beta": "desc-b", "gamma": "desc-c"}
	b := map[string]string{"gamma": "desc-c", "alpha": "desc-a", "beta": "desc-b"}

	ha := hashToolDescs(a)
	hb := hashToolDescs(b)
	if ha != hb {
		t.Errorf("hashToolDescs not deterministic: a=%s b=%s", ha, hb)
	}
	if len(ha) != 64 {
		t.Errorf("expected 64-char SHA256 hex, got %d chars: %s", len(ha), ha)
	}

	// Different content → different hash.
	c := map[string]string{"alpha": "CHANGED", "beta": "desc-b", "gamma": "desc-c"}
	hc := hashToolDescs(c)
	if ha == hc {
		t.Error("hashToolDescs: different content produced the same hash — collision")
	}
}

// TestHashToolDescs_Empty verifies that an empty map produces a stable, non-empty hash.
func TestHashToolDescs_Empty(t *testing.T) {
	h := hashToolDescs(map[string]string{})
	if len(h) != 64 {
		t.Errorf("empty map: expected 64-char SHA256 hex, got %q", h)
	}
	// Hash of empty input must be the SHA256 of the empty string.
	// We just verify it's stable across two calls.
	h2 := hashToolDescs(map[string]string{})
	if h != h2 {
		t.Error("hashToolDescs(empty): non-deterministic output")
	}
}

// TestToolIntegrity_BaselineMatchesAtStartup verifies that a freshly created
// server passes its own integrity check — the baseline matches the live descs.
func TestToolIntegrity_BaselineMatchesAtStartup(t *testing.T) {
	s := newTestServer(t)

	if s.toolDescBaseline == "" {
		t.Fatal("toolDescBaseline is empty after New() — baseline was not computed")
	}
	if len(s.toolDescs) == 0 {
		t.Fatal("toolDescs is empty after New() — descriptions were not captured")
	}

	// Re-derive hash from live map — must match baseline.
	live := hashToolDescs(s.toolDescs)
	if live != s.toolDescBaseline {
		t.Errorf("baseline mismatch at startup:\n  baseline = %s\n  live     = %s", s.toolDescBaseline, live)
	}
}

// TestToolIntegrity_AllToolsCaptured verifies that at least the core session
// tools have their descriptions captured in toolDescs.
func TestToolIntegrity_AllToolsCaptured(t *testing.T) {
	s := newTestServer(t)

	required := []string{"session_init", "memory", "end_session"}
	for _, name := range required {
		if _, ok := s.toolDescs[name]; !ok {
			t.Errorf("toolDescs missing %q — addOrDefer capture may be incomplete", name)
		}
	}
}

// TestToolIntegrity_SessionInitNoAlertOnClean verifies that session_init does
// NOT emit a tool_integrity_alert when tool descriptions are unmodified.
func TestToolIntegrity_SessionInitNoAlertOnClean(t *testing.T) {
	s := newTestServer(t)

	res, err := s.DispatchTool(context.Background(), "session_init", map[string]interface{}{
		"agent_id": "integrity-test",
	})
	if err != nil {
		t.Fatalf("session_init: %v", err)
	}
	if res.IsError {
		t.Fatalf("session_init returned error: %v", res.Content)
	}

	var m map[string]interface{}
	if err := json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, found := m["tool_integrity_alert"]; found {
		t.Error("unexpected tool_integrity_alert in clean server session_init response")
	}
}

// TestToolIntegrity_SessionInitAlertsOnTampering simulates runtime tampering
// by mutating toolDescs after construction and verifying that session_init
// surfaces a tool_integrity_alert with the expected fields.
func TestToolIntegrity_SessionInitAlertsOnTampering(t *testing.T) {
	s := newTestServer(t)

	// Tamper: overwrite a description in the live map.
	// The baseline was computed before this change, so hashes will differ.
	s.toolDescs["session_init"] = "TAMPERED DESCRIPTION"

	res, err := s.DispatchTool(context.Background(), "session_init", map[string]interface{}{
		"agent_id": "integrity-test",
	})
	if err != nil {
		t.Fatalf("session_init: %v", err)
	}
	if res.IsError {
		t.Fatalf("session_init returned error: %v", res.Content)
	}

	var m map[string]interface{}
	if err := json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	alert, found := m["tool_integrity_alert"]
	if !found {
		t.Fatal("expected tool_integrity_alert in session_init response after tampering, got none")
	}

	alertMap, ok := alert.(map[string]interface{})
	if !ok {
		t.Fatalf("tool_integrity_alert is not a map: %T", alert)
	}

	if alertMap["severity"] != "HIGH" {
		t.Errorf("expected severity=HIGH, got %q", alertMap["severity"])
	}
	if alertMap["expected"] == alertMap["actual"] {
		t.Error("tool_integrity_alert: expected != actual should differ after tampering")
	}
}

// TestToolIntegrity_QuickScopeAlertsOnTampering verifies that scope="quick"
// does NOT suppress the integrity alert. Security alerts surface in all modes.
func TestToolIntegrity_QuickScopeAlertsOnTampering(t *testing.T) {
	s := newTestServer(t)
	s.toolDescs["memory"] = "TAMPERED"

	res, err := s.DispatchTool(context.Background(), "session_init", map[string]interface{}{
		"agent_id": "integrity-test",
		"scope":    "quick",
	})
	if err != nil {
		t.Fatalf("session_init (quick): %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, found := m["tool_integrity_alert"]; !found {
		t.Error("scope=quick suppressed tool_integrity_alert — security alerts must never be suppressed by scope")
	}
}

// TestToolDescriptions_WordLimit verifies that every registered tool description
// is at or below the 150-word limit specified by Sprint 23.10. This prevents
// future description changes from silently exceeding the budget.
//
// toolDescs stores "description\x00schemaJSON" — only the description portion
// (before the null separator) is counted toward the word limit.
func TestToolDescriptions_WordLimit(t *testing.T) {
	s := newTestServer(t)

	const maxWords = 150
	for tool, entry := range s.toolDescs {
		// entry format: "<description>\x00<jsonSchema>"
		desc := entry
		if idx := strings.IndexByte(entry, 0); idx >= 0 {
			desc = entry[:idx]
		}
		words := len(strings.Fields(desc))
		if words > maxWords {
			t.Errorf("tool %q description exceeds %d-word limit: %d words\n  desc: %s",
				tool, maxWords, words, desc)
		}
	}
}

// TestToolIntegrity_ResumeScopeAlertsOnTampering verifies scope="resume" does
// not suppress the integrity alert.
func TestToolIntegrity_ResumeScopeAlertsOnTampering(t *testing.T) {
	s := newTestServer(t)
	s.toolDescs["validate"] = "TAMPERED"

	res, err := s.DispatchTool(context.Background(), "session_init", map[string]interface{}{
		"agent_id": "integrity-test",
		"scope":    "resume",
	})
	if err != nil {
		t.Fatalf("session_init (resume): %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, found := m["tool_integrity_alert"]; !found {
		t.Error("scope=resume suppressed tool_integrity_alert — security alerts must never be suppressed by scope")
	}
}
