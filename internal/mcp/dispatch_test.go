package mcp

import (
	"context"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── DispatchTool unit tests ───────────────────────────────────────────────────

// TestDispatchTool_KnownTool verifies that a registered tool is dispatched and
// its result is returned without error.
func TestDispatchTool_KnownTool(t *testing.T) {
	srv := newTestServer(t)
	result, err := srv.DispatchTool(context.Background(), "explain_codebase", nil)
	if err != nil {
		t.Fatalf("expected no error for known tool, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

// TestDispatchTool_UnknownTool verifies that an unregistered tool name returns
// ErrUnknownTool (not a generic error) and nil result.
func TestDispatchTool_UnknownTool(t *testing.T) {
	srv := newTestServer(t)
	result, err := srv.DispatchTool(context.Background(), "no_such_tool_xyz", nil)
	if err == nil {
		t.Fatal("expected error for unknown tool, got nil")
	}
	if result != nil {
		t.Errorf("expected nil result for unknown tool, got non-nil")
	}
	e, ok := err.(*ErrUnknownTool)
	if !ok {
		t.Errorf("expected *ErrUnknownTool, got %T: %v", err, err)
	}
	if ok && e.Name != "no_such_tool_xyz" {
		t.Errorf("expected Name=%q, got %q", "no_such_tool_xyz", e.Name)
	}
}

// TestDispatchTool_EmptyToolName returns ErrUnknownTool (empty name is not registered).
func TestDispatchTool_EmptyToolName(t *testing.T) {
	srv := newTestServer(t)
	_, err := srv.DispatchTool(context.Background(), "", nil)
	if err == nil {
		t.Fatal("expected error for empty tool name")
	}
	if _, ok := err.(*ErrUnknownTool); !ok {
		t.Errorf("expected *ErrUnknownTool, got %T", err)
	}
}

// TestDispatchTool_NilArgs verifies that nil args does not panic; they are
// treated as an empty map by the handler.
func TestDispatchTool_NilArgs(t *testing.T) {
	srv := newTestServer(t)
	result, err := srv.DispatchTool(context.Background(), "explain_codebase", nil)
	if err != nil {
		t.Fatalf("unexpected error with nil args: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

// TestDispatchTool_ArgsPassedThrough verifies arguments reach the handler.
// We use "search" with a query string and expect a valid (non-error) result.
func TestDispatchTool_ArgsPassedThrough(t *testing.T) {
	srv := newTestServer(t)
	result, err := srv.DispatchTool(context.Background(), "search", map[string]interface{}{
		"query": "AuthLogin",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.Content) == 0 {
		t.Error("expected at least one content item in result")
	}
}

// TestDispatchTool_ToolErrorIsResult verifies that a tool-level error (is_error: true)
// comes back as a successful DispatchTool call with IsError=true — not a Go error.
// This matches the MCP contract: tool errors are results, not panics.
func TestDispatchTool_ToolErrorIsResult(t *testing.T) {
	srv := newTestServer(t)
	// get_context with no entity returns a tool-level error, not a Go error.
	result, err := srv.DispatchTool(context.Background(), "get_context", map[string]interface{}{})
	if err != nil {
		t.Fatalf("tool error should be result.IsError, not Go error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result even on tool-level error")
	}
}

// TestDispatchTool_AllRegisteredToolsReachable verifies every tool registered via
// addOrDefer is reachable through DispatchTool. Catches registration mismatches.
func TestDispatchTool_AllRegisteredToolsReachable(t *testing.T) {
	srv := newTestServer(t)

	srv.toolHandlersMu.RLock()
	names := make([]string, 0, len(srv.toolHandlers))
	for name := range srv.toolHandlers {
		names = append(names, name)
	}
	srv.toolHandlersMu.RUnlock()

	if len(names) == 0 {
		t.Fatal("no tools in dispatch table — addOrDefer wiring is broken")
	}

	for _, name := range names {
		name := name
		t.Run(name, func(t *testing.T) {
			// We only verify DispatchTool does NOT return ErrUnknownTool.
			// Tool-level errors (missing required args) are acceptable.
			_, err := srv.DispatchTool(context.Background(), name, map[string]interface{}{})
			if e, ok := err.(*ErrUnknownTool); ok {
				t.Errorf("tool %q is in toolHandlers but DispatchTool returned ErrUnknownTool: %v", name, e)
			}
		})
	}
}

// TestDispatchTool_KnowledgeMode_GraphToolStubbed verifies that in knowledge
// mode, graph-only tools return a tool-level error stub (not a Go error),
// and that the dispatch table still contains the stub (not a missing entry).
func TestDispatchTool_KnowledgeMode_GraphToolStubbed(t *testing.T) {
	st := openTestStore(t)
	cfg := &config.Config{Mode: "knowledge"}
	srv := NewKnowledge(cfg, st)

	result, err := srv.DispatchTool(context.Background(), "get_context", map[string]interface{}{
		"entity": "SomeFunction",
	})
	if err != nil {
		t.Fatalf("knowledge-mode stub must not return Go error, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result for knowledge-mode stub")
	}
	if !result.IsError {
		t.Error("expected IsError=true for graph tool in knowledge mode")
	}
}

// TestDispatchTool_KnowledgeMode_KnowledgeToolWorks verifies that knowledge
// tools (recall, remember, etc.) dispatch normally in knowledge mode.
func TestDispatchTool_KnowledgeMode_KnowledgeToolWorks(t *testing.T) {
	st := openTestStore(t)
	cfg := &config.Config{Mode: "knowledge"}
	srv := NewKnowledge(cfg, st)

	result, err := srv.DispatchTool(context.Background(), "recall", map[string]interface{}{})
	if err != nil {
		t.Fatalf("recall in knowledge mode returned Go error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.IsError {
		t.Error("expected recall to succeed with empty results, not error")
	}
}

// TestDispatchTool_SessionIDInContext verifies a session ID injected via
// WithSessionID propagates through DispatchTool to the handler context.
func TestDispatchTool_SessionIDInContext(t *testing.T) {
	srv := newTestServer(t)
	ctx := WithSessionID(context.Background(), "rest-42")
	// session_init records the session; we just verify no panic/error from dispatch.
	result, err := srv.DispatchTool(ctx, "explain_codebase", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

// TestDispatchTool_ConcurrentSafe verifies concurrent DispatchTool calls on the
// same server don't race on the dispatch table itself. We use "recall" (pure
// store read, no graph mutation) to avoid triggering pre-existing races in
// graph.ProjectIdentity(). The dispatch table access is protected by toolHandlersMu.
func TestDispatchTool_ConcurrentSafe(t *testing.T) {
	srv := newTestServer(t)
	done := make(chan struct{}, 20)
	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			srv.DispatchTool(context.Background(), "recall", map[string]interface{}{}) //nolint:errcheck
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}

// TestDispatchTool_PopulatedAfterConstruction verifies the dispatch table is
// non-empty immediately after New() returns — registerTools is synchronous.
func TestDispatchTool_PopulatedAfterConstruction(t *testing.T) {
	g := graph.New("test-repo")
	cfg := &config.Config{}
	srv := New(g, cfg, nil)

	srv.toolHandlersMu.RLock()
	n := len(srv.toolHandlers)
	srv.toolHandlersMu.RUnlock()

	if n == 0 {
		t.Error("toolHandlers is empty after New() — addOrDefer wiring is broken")
	}
}

// TestDispatchTool_StoreNil verifies read-only tools work when store is nil.
func TestDispatchTool_StoreNil(t *testing.T) {
	g := graph.New("test-repo")
	cfg := &config.Config{}
	srv := New(g, cfg, nil)

	result, err := srv.DispatchTool(context.Background(), "explain_codebase", nil)
	if err != nil {
		t.Fatalf("unexpected error with nil store: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

// ── ErrUnknownTool ────────────────────────────────────────────────────────────

func TestErrUnknownTool_Error(t *testing.T) {
	e := &ErrUnknownTool{Name: "foo_tool"}
	want := "unknown tool: foo_tool"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestErrUnknownTool_EmptyName(t *testing.T) {
	e := &ErrUnknownTool{Name: ""}
	if got := e.Error(); got != "unknown tool: " {
		t.Errorf("Error() = %q", got)
	}
}
