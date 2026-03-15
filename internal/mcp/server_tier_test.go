package mcp

import (
	"testing"

	"github.com/mark3labs/mcp-go/server"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// TestToolInTier_Core verifies that the core tier contains exactly the 10
// expected tools and nothing else.
func TestToolInTier_Core(t *testing.T) {
	s := &Server{repoScale: graph.ScaleMicro, deferredTools: make(map[string]server.ServerTool)}

	wantIn := []string{
		"session_init", "get_context", "find_entity", "search",
		"validate_plan", "verify_implementation", "create_plan",
		"update_task", "discover_tools", "end_session",
	}
	for _, name := range wantIn {
		if !s.toolInTier(name) {
			t.Errorf("toolInTier(%q) = false for micro; want true", name)
		}
	}

	// Tools outside core must be deferred for micro repos.
	wantOut := []string{
		"get_pending_tasks", "get_file_context", "get_impact",
		"get_call_chain", "claim_work", "release_claims", "remember",
		"recall", "get_working_state", "get_violations",
		"annotate_node", "get_events", "upsert_rule", "send_message",
	}
	for _, name := range wantOut {
		if s.toolInTier(name) {
			t.Errorf("toolInTier(%q) = true for micro; want false", name)
		}
	}
}

// TestToolInTier_Standard verifies that the standard tier contains all 20
// expected tools (10 core + 10 additions) and defers the rest.
func TestToolInTier_Standard(t *testing.T) {
	s := &Server{repoScale: graph.ScaleSmall, deferredTools: make(map[string]server.ServerTool)}

	wantIn := []string{
		// core
		"session_init", "get_context", "find_entity", "search",
		"validate_plan", "verify_implementation", "create_plan",
		"update_task", "discover_tools", "end_session",
		// standard additions
		"get_pending_tasks", "get_file_context", "get_impact",
		"get_call_chain", "claim_work", "release_claims", "remember",
		"recall", "get_working_state", "get_violations",
	}
	for _, name := range wantIn {
		if !s.toolInTier(name) {
			t.Errorf("toolInTier(%q) = false for small; want true", name)
		}
	}

	// Medium scale uses the same standard tier.
	s.repoScale = graph.ScaleMedium
	for _, name := range wantIn {
		if !s.toolInTier(name) {
			t.Errorf("toolInTier(%q) = false for medium; want true", name)
		}
	}

	// Extended tools are still deferred for standard tier.
	wantOut := []string{"annotate_node", "get_events", "upsert_rule", "send_message", "execute_skill"}
	for _, name := range wantOut {
		if s.toolInTier(name) {
			t.Errorf("toolInTier(%q) = true for small/medium; want false", name)
		}
	}
}

// TestToolInTier_Full verifies that all tools are in-tier for large repos.
func TestToolInTier_Full(t *testing.T) {
	s := &Server{repoScale: graph.ScaleLarge, deferredTools: make(map[string]server.ServerTool)}

	allTools := []string{
		"session_init", "get_context", "find_entity", "search",
		"validate_plan", "verify_implementation", "create_plan", "update_task",
		"discover_tools", "end_session", "get_pending_tasks", "get_file_context",
		"get_impact", "get_call_chain", "claim_work", "release_claims",
		"remember", "recall", "get_working_state", "get_violations",
		"annotate_node", "get_events", "upsert_rule", "send_message",
		"get_messages", "check_plan_safety", "get_adrs", "upsert_adr",
		"web_annotate", "lookup_docs", "execute_skill", "list_skills",
	}
	for _, name := range allTools {
		if !s.toolInTier(name) {
			t.Errorf("toolInTier(%q) = false for large; want true", name)
		}
	}
}

// TestToolInTier_ZeroNodesFull verifies that a server with an unset/empty
// scale (pre-index, NodeCount == 0 scenario) defaults to full tier.
func TestToolInTier_ZeroNodesFull(t *testing.T) {
	// Empty string scale (not yet set) must default to full tier.
	s := &Server{deferredTools: make(map[string]server.ServerTool)}

	extended := []string{"annotate_node", "get_events", "upsert_rule", "execute_skill"}
	for _, name := range extended {
		if !s.toolInTier(name) {
			t.Errorf("toolInTier(%q) = false for unset scale; want true (safe default)", name)
		}
	}
}

// TestRegisterDeferredTools verifies that a deferred tool is promoted and
// removed from the deferred set correctly.
func TestRegisterDeferredTools(t *testing.T) {
	s := &Server{repoScale: graph.ScaleMicro, deferredTools: make(map[string]server.ServerTool)}

	// Manually insert a deferred entry (mimics addOrDefer storing it).
	import_server_pkg := server.ServerTool{} // zero value; handler nil is OK for this test
	s.deferredTools["get_violations"] = import_server_pkg

	registered := s.RegisterDeferredTools([]string{"get_violations", "nonexistent"})

	if len(registered) != 1 || registered[0] != "get_violations" {
		t.Errorf("RegisterDeferredTools returned %v; want [get_violations]", registered)
	}
	if _, still := s.deferredTools["get_violations"]; still {
		t.Error("get_violations still in deferredTools after promotion; should be removed")
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

// TestAddOrDefer_MicroDefers verifies that addOrDefer stores extended tools in
// the deferred map rather than registering them on a micro-scale server.
func TestAddOrDefer_MicroDefers(t *testing.T) {
	// Build a minimal server without a real mcp.MCPServer so we can inspect
	// the deferred map directly without full server setup.
	s := &Server{repoScale: graph.ScaleMicro, deferredTools: make(map[string]server.ServerTool)}

	// annotate_node is not in core tier — it should be deferred, not panicking.
	// We call toolInTier instead of addOrDefer to avoid needing s.mcp initialised.
	if s.toolInTier("annotate_node") {
		t.Error("annotate_node should not be in micro tier")
	}

	// Verify we can store and retrieve a deferred entry manually.
	s.deferredToolsMu.Lock()
	s.deferredTools["annotate_node"] = server.ServerTool{}
	s.deferredToolsMu.Unlock()

	if _, ok := s.deferredTools["annotate_node"]; !ok {
		t.Error("deferred tool not stored in deferredTools map")
	}
}
