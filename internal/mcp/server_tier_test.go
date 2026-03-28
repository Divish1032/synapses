package mcp

import (
	"testing"
)

// TestToolInTier_AllToolsAlwaysAvailable verifies that all tools are
// registered at all scales (ADR: mcp-go has no custom dispatch).
func TestToolInTier_AllToolsAlwaysAvailable(t *testing.T) {
	s := &Server{}
	tools := []string{
		"session_init", "search", "get_context", "get_file_context",
		"get_impact", "validate", "memory", "end_session",
		"tasks", "rules", "annotate", "lookup_docs",
	}
	for _, name := range tools {
		if !s.toolInTier(name) {
			t.Errorf("toolInTier(%q) = false; all tools must always be available", name)
		}
	}
}

// TestCoreTierTools_DesignDocSet verifies core categorization matches
// the final 12-tool set after Sprint 24 consolidation.
func TestCoreTierTools_DesignDocSet(t *testing.T) {
	expected := []string{
		"session_init", "search", "get_context", "get_file_context",
		"get_impact", "validate", "memory", "end_session",
		"tasks", "rules", "annotate", "lookup_docs",
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
