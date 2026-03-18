package mcp

import (
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

// TestToolInTier_AllToolsAlwaysAvailable verifies that Phase 6 ensures all
// tools are registered at all scales (auto-promotion backwards compatibility).
func TestToolInTier_AllToolsAlwaysAvailable(t *testing.T) {
	s := &Server{deferredTools: make(map[string]server.ServerTool)}
	tools := []string{
		// Core (Phase 6)
		"session_init", "prepare_context", "search", "validate_plan",
		"verify_implementation", "remember", "recall", "create_plan",
		"update_task", "end_session", "discover_tools", "annotate_node",
		// Previously standard/extended
		"get_context", "find_entity", "get_pending_tasks", "get_file_context",
		"get_impact", "get_call_chain", "claim_work", "release_claims",
		"get_working_state", "get_violations",
		"get_events", "upsert_rule", "send_message", "execute_skill",
	}
	for _, name := range tools {
		if !s.toolInTier(name) {
			t.Errorf("toolInTier(%q) = false; Phase 6 requires all tools always available", name)
		}
	}
}

// TestCoreTierTools_DesignDocSet verifies the core tier categorization matches
// the design doc's ~12 tool set (used for status labels in discover_tools).
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

// TestStandardTierTools_IncludesCore verifies that the standard tier is a
// superset of the core tier (every core tool is also standard).
func TestStandardTierTools_IncludesCore(t *testing.T) {
	for name := range coreTierTools {
		if !standardTierTools[name] {
			t.Errorf("core tool %q missing from standardTierTools", name)
		}
	}
}

// TestRegisterDeferredTools verifies that a deferred tool is promoted and
// removed from the deferred set correctly.
func TestRegisterDeferredTools(t *testing.T) {
	// Use a real server so s.mcp is initialized and AddTool works.
	s := newTestServer(t)

	// Manually insert a deferred entry.
	s.deferredToolsMu.Lock()
	s.deferredTools["test_promoted_tool"] = server.ServerTool{}
	s.deferredToolsMu.Unlock()

	registered := s.RegisterDeferredTools([]string{"test_promoted_tool", "nonexistent"})

	if len(registered) != 1 || registered[0] != "test_promoted_tool" {
		t.Errorf("RegisterDeferredTools returned %v; want [test_promoted_tool]", registered)
	}
	if _, still := s.deferredTools["test_promoted_tool"]; still {
		t.Error("test_promoted_tool still in deferredTools after promotion; should be removed")
	}
}

// TestRegisterDeferredTools_Empty verifies that calling with no names is safe.
func TestRegisterDeferredTools_Empty(t *testing.T) {
	s := &Server{deferredTools: make(map[string]server.ServerTool)}
	result := s.RegisterDeferredTools(nil)
	if result != nil {
		t.Errorf("RegisterDeferredTools(nil) = %v; want nil", result)
	}
	result = s.RegisterDeferredTools([]string{})
	if result != nil {
		t.Errorf("RegisterDeferredTools([]) = %v; want nil", result)
	}
}

// TestIsDeferredTool verifies the helper correctly reports deferred state.
func TestIsDeferredTool(t *testing.T) {
	s := &Server{deferredTools: make(map[string]server.ServerTool)}
	s.deferredTools["test_tool"] = server.ServerTool{}

	if !s.IsDeferredTool("test_tool") {
		t.Error("expected test_tool to be deferred")
	}
	if s.IsDeferredTool("nonexistent") {
		t.Error("nonexistent should not be deferred")
	}
}

// TestGetDeferredToolNames verifies the helper returns correct names.
func TestGetDeferredToolNames(t *testing.T) {
	s := &Server{deferredTools: make(map[string]server.ServerTool)}
	s.deferredTools["tool_a"] = server.ServerTool{}
	s.deferredTools["tool_b"] = server.ServerTool{}

	names := s.GetDeferredToolNames()
	if len(names) != 2 {
		t.Fatalf("expected 2 deferred tools, got %d", len(names))
	}
	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
	}
	if !nameSet["tool_a"] || !nameSet["tool_b"] {
		t.Errorf("expected tool_a and tool_b, got %v", names)
	}
}
