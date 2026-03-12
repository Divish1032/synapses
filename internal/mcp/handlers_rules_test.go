package mcp

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── handleValidatePlan ────────────────────────────────────────────────────────

func TestHandleValidatePlan_NoRules(t *testing.T) {
	s := newTestServer(t)
	// validate_plan expects changes as a JSON string.
	res, err := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": `[{"file":"pkg/auth/auth.go","action":"modify","description":"add token refresh"}]`,
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "violations")
	violations, _ := m["violations"].([]any)
	if len(violations) != 0 {
		t.Errorf("expected no violations with no rules, got %d", len(violations))
	}
}

func TestHandleValidatePlan_WithRule(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpsertRule(ctx, callTool(map[string]any{
		"rule_id":     "test-rule",
		"description": "no direct DB calls from handlers",
		"severity":    "error",
	}))
	mustResult(t, res, err)

	res2, err2 := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": `[{"file":"internal/store/store.go","action":"modify","description":"add new method"}]`,
	}))
	m := mustResult(t, res2, err2)
	hasKey(t, m, "violations")
}

func TestHandleValidatePlan_MissingChanges_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleValidatePlan(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// ── handleVerifyImplementation ────────────────────────────────────────────────

func TestHandleVerifyImplementation_SingleFile(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// verify_implementation takes files_written (JSON string of file paths).
	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": `["pkg/auth/auth.go"]`,
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "files")
}

func TestHandleVerifyImplementation_MissingFilesWritten_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleVerifyImplementation(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// ── handleGetViolations ───────────────────────────────────────────────────────

func TestHandleGetViolations_Empty(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetViolations(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "violations")
}

func TestHandleGetViolations_AfterRuleUpsert(t *testing.T) {
	// Build a server with a graph that actually violates a rule.
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())

	// Add a forbidden import edge: cmd imports internal.
	cmdID := g.MakeNodeID("cmd/main.go", "main")
	g.AddNode(&graph.Node{ID: cmdID, Name: "main", File: "cmd/main.go", Package: "main", Type: graph.NodeFunction})
	internalID := g.MakeNodeID("internal/secret/secret.go", "Secret")
	g.AddNode(&graph.Node{ID: internalID, Name: "Secret", File: "internal/secret/secret.go", Package: "secret", Type: graph.NodeFunction})
	g.AddEdge(&graph.Edge{From: cmdID, To: internalID, Type: graph.EdgeImports})

	s := New(g, cfg, st)

	// Add rule that catches this.
	res, err := s.handleUpsertRule(ctx, callTool(map[string]any{
		"rule_id":     "no-secret-import",
		"description": "cmd must not import secret package",
		"severity":    "error",
		"forbidden_imports": []any{
			map[string]any{"from": "cmd/.*", "to": "internal/secret/.*"},
		},
	}))
	mustResult(t, res, err)

	res2, err2 := s.handleGetViolations(ctx, callTool(nil))
	m := mustResult(t, res2, err2)
	hasKey(t, m, "violations")
}

// ── handleUpsertRule ──────────────────────────────────────────────────────────

func TestHandleUpsertRule_CreatesRule(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpsertRule(ctx, callTool(map[string]any{
		"rule_id":     "test-rule-1",
		"description": "no circular imports",
		"severity":    "error",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "rule_id")

	// Rule should be retrievable and affect future validate_plan calls.
	res2, err2 := s.handleGetViolations(ctx, callTool(nil))
	m2 := mustResult(t, res2, err2)
	hasKey(t, m2, "violations")
}

func TestHandleUpsertRule_MissingRuleID_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpsertRule(ctx, callTool(map[string]any{
		"description": "some rule",
		"severity":    "error",
	}))
	mustErrorResult(t, res, err)
}
