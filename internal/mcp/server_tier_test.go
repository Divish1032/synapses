package mcp

import (
	"testing"
)

// TestToolInTier_AllToolsAlwaysAvailable verifies that all tools are
// registered at all scales (ADR: mcp-go has no custom dispatch).
func TestToolInTier_AllToolsAlwaysAvailable(t *testing.T) {
	s := &Server{}
	tools := []string{
		"session_init", "prepare_context", "search", "validate_plan",
		"verify_implementation", "remember", "recall", "create_plan",
		"update_task", "end_session", "discover_tools", "annotate_node",
		"get_context", "find_entity", "get_pending_tasks", "get_file_context",
		"get_impact", "get_call_chain",
		"get_working_state", "get_violations",
		"get_events", "upsert_rule", "send_message", "execute_skill",
	}
	for _, name := range tools {
		if !s.toolInTier(name) {
			t.Errorf("toolInTier(%q) = false; all tools must always be available", name)
		}
	}
}

// TestCoreTierTools_DesignDocSet verifies core categorization matches
// the design doc's 12-tool set (used for discover_tools status labels).
func TestCoreTierTools_DesignDocSet(t *testing.T) {
	expected := []string{
		"session_init", "prepare_context", "search", "validate_plan",
		"verify_implementation", "remember", "recall", "create_plan",
		"update_task", "end_session", "discover_tools", "annotate_node",
	}
	for _, name := range expected {
		if !coreTierTools[name] {
			t.Errorf("expected %q in coreTierTools", name)
		}
	}
	if len(coreTierTools) != len(expected) {
		t.Errorf("coreTierTools has %d entries, expected %d", len(coreTierTools), len(expected))
	}
}

// TestStandardTierTools_IncludesCore verifies standard is a superset of core.
func TestStandardTierTools_IncludesCore(t *testing.T) {
	for name := range coreTierTools {
		if !standardTierTools[name] {
			t.Errorf("core tool %q missing from standardTierTools", name)
		}
	}
}
