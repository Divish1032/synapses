package mcp

// White-box tests for unexported helpers and additional handler coverage
// in tools.go: normalizeSubgraph, fileHasTests, matchRulesForFile,
// toDirectionalContext, and handlers: GetImpact, GetViolations, FindEntity,
// ValidatePlan, GetFileContext, GetContext, GetWorkingState, GetCallChain.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── normalizeSubgraph ─────────────────────────────────────────────────────────

func TestNormalizeSubgraph_EmptyRepoRoot(t *testing.T) {
	sg := &graph.SubGraph{Root: "root"}
	result := normalizeSubgraph(sg, "")
	if result != sg {
		t.Error("expected same subgraph when repoRoot is empty")
	}
}

func TestNormalizeSubgraph_StripsPrefix(t *testing.T) {
	root := &graph.Node{ID: "n1", Name: "Foo", File: "/home/user/project/pkg/foo.go"}
	sg := &graph.SubGraph{
		Root:  "n1",
		Nodes: []graph.CarvedNode{{Node: root}},
	}
	result := normalizeSubgraph(sg, "/home/user/project")
	if len(result.Nodes) == 0 {
		t.Fatal("expected nodes in result")
	}
	if result.Nodes[0].Node.File != "pkg/foo.go" {
		t.Errorf("expected stripped path, got %q", result.Nodes[0].Node.File)
	}
}

func TestNormalizeSubgraph_NoMutationOfOriginal(t *testing.T) {
	originalFile := "/home/user/project/pkg/foo.go"
	root := &graph.Node{ID: "n1", Name: "Foo", File: originalFile}
	sg := &graph.SubGraph{
		Root:  "n1",
		Nodes: []graph.CarvedNode{{Node: root}},
	}
	normalizeSubgraph(sg, "/home/user/project")
	if root.File != originalFile {
		t.Errorf("original node.File was mutated: %q", root.File)
	}
}

func TestNormalizeSubgraph_PathNotMatchingPrefix(t *testing.T) {
	root := &graph.Node{ID: "n1", Name: "Foo", File: "/other/path/foo.go"}
	sg := &graph.SubGraph{
		Root:  "n1",
		Nodes: []graph.CarvedNode{{Node: root}},
	}
	result := normalizeSubgraph(sg, "/home/user/project")
	if result.Nodes[0].Node.File != "/other/path/foo.go" {
		t.Errorf("non-matching path should be unchanged, got %q", result.Nodes[0].Node.File)
	}
}

// ── fileHasTests ──────────────────────────────────────────────────────────────

func TestFileHasTests_Empty(t *testing.T) {
	if fileHasTests("") {
		t.Error("expected false for empty path")
	}
}

func TestFileHasTests_GoFileWithTestFile(t *testing.T) {
	dir := t.TempDir()
	srcFile := filepath.Join(dir, "auth.go")
	testFile := filepath.Join(dir, "auth_test.go")
	if err := os.WriteFile(srcFile, []byte("package p"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(testFile, []byte("package p"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !fileHasTests(srcFile) {
		t.Error("expected true when _test.go exists in same dir")
	}
}

func TestFileHasTests_GoFileNoTestFile(t *testing.T) {
	dir := t.TempDir()
	srcFile := filepath.Join(dir, "auth.go")
	if err := os.WriteFile(srcFile, []byte("package p"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fileHasTests(srcFile) {
		t.Error("expected false when no *_test.go exists")
	}
}

func TestFileHasTests_NonGoFileNoTest(t *testing.T) {
	dir := t.TempDir()
	srcFile := filepath.Join(dir, "app.py")
	if err := os.WriteFile(srcFile, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if fileHasTests(srcFile) {
		t.Error("expected false for .py with no test files")
	}
}

func TestFileHasTests_PythonTestFile(t *testing.T) {
	dir := t.TempDir()
	srcFile := filepath.Join(dir, "app.py")
	testFile := filepath.Join(dir, "test_app.py")
	if err := os.WriteFile(srcFile, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(testFile, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if !fileHasTests(srcFile) {
		t.Error("expected true for Python with test_*.py")
	}
}

// ── matchRulesForFile ─────────────────────────────────────────────────────────

func TestMatchRulesForFile_EmptyRules(t *testing.T) {
	cfg := &config.Config{}
	rules := matchRulesForFile(cfg, "pkg/auth.go")
	if len(rules) != 0 {
		t.Errorf("expected 0 rules for empty config, got %d", len(rules))
	}
}

func TestMatchRulesForFile_NoPatternAlwaysApplies(t *testing.T) {
	cfg := &config.Config{
		Rules: []config.Rule{
			{ID: "r1", Description: "no db in handlers", Severity: "error"},
		},
	}
	rules := matchRulesForFile(cfg, "any/file.go")
	if len(rules) != 1 {
		t.Errorf("expected 1 rule, got %d", len(rules))
	}
}

func TestMatchRulesForFile_PatternMatch(t *testing.T) {
	cfg := &config.Config{
		Rules: []config.Rule{
			{
				ID:          "r2",
				Description: "handler specific rule",
				Severity:    "warning",
				ForbiddenEdge: config.ForbiddenEdge{
					FromFilePattern: "handler*.go",
				},
			},
		},
	}
	matched := matchRulesForFile(cfg, "pkg/handler_auth.go")
	if len(matched) != 1 {
		t.Errorf("expected 1 matched rule, got %d", len(matched))
	}
	notMatched := matchRulesForFile(cfg, "pkg/service.go")
	if len(notMatched) != 0 {
		t.Errorf("expected 0 matched rules, got %d", len(notMatched))
	}
}

// ── handleGetImpact ───────────────────────────────────────────────────────────

func TestHandleGetImpact_NoSymbol(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result when symbol is missing")
	}
}

func TestHandleGetImpact_NotFound(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{"symbol": "NonExistentSymbol"})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for unknown symbol")
	}
}

func TestHandleGetImpact_FoundSymbol(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{"symbol": "AuthLogin"})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Errorf("unexpected tool error: %v", result.Content)
	}
}

func TestHandleGetImpact_WithDepth(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"symbol": "AuthLogin",
		"depth":  float64(2),
	})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestHandleGetImpact_ExcessiveDepthCapped(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"symbol": "AuthLogin",
		"depth":  float64(99),
	})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestHandleGetImpact_StructType(t *testing.T) {
	s := newTestServer(t)
	structID := s.graph.MakeNodeID("pkg/svc.go", "Store")
	s.graph.AddNode(&graph.Node{
		ID: structID, Name: "Store", Type: graph.NodeStruct,
		File: "pkg/svc.go", Package: "svc",
	})
	req := callTool(map[string]any{"symbol": "Store"})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Errorf("unexpected tool error: %v", result.Content)
	}
}

// ── handleGetViolations ───────────────────────────────────────────────────────

func TestHandleGetViolations_NoViolations(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{})
	result, err := s.handleGetViolations(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	if summary, _ := m["summary"].(string); summary != "no violations found" {
		t.Errorf("expected no violations, got %q", summary)
	}
}

func TestHandleGetViolations_WithRuleIDFilter(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{"rule_id": "some-rule"})
	result, err := s.handleGetViolations(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = mustResult(t, result, nil)
}

func TestHandleGetViolations_IncludeLog(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{"include_log": true})
	result, err := s.handleGetViolations(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── handleFindEntity ──────────────────────────────────────────────────────────

func TestHandleFindEntity_NoQuery(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{})
	result, err := s.handleFindEntity(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when query is missing")
	}
}

func TestHandleFindEntity_NotFound(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{"query": "NotExistsXYZ"})
	result, err := s.handleFindEntity(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	if count, _ := m["count"].(float64); count != 0 {
		t.Errorf("expected 0 matches, got %v", count)
	}
	if _, hasHint := m["hint"]; !hasHint {
		t.Error("expected hint when no results found")
	}
}

func TestHandleFindEntity_Found(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{"query": "AuthLogin"})
	result, err := s.handleFindEntity(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	if count, _ := m["count"].(float64); count == 0 {
		t.Error("expected at least 1 match for AuthLogin")
	}
}

func TestHandleFindEntity_DottedMethodName(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{"query": "auth.AuthLogin"})
	result, err := s.handleFindEntity(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── handleValidatePlan ────────────────────────────────────────────────────────

func TestHandleValidatePlan_NoChanges(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{})
	result, err := s.handleValidatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestHandleValidatePlan_WithChanges(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"changes": `[{"file": "pkg/auth/auth.go", "adds_call_to": "AuthLogout"}]`,
	})
	result, err := s.handleValidatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	if _, ok := m["violations"]; !ok {
		t.Error("expected 'violations' key in result")
	}
}

func TestHandleValidatePlan_InvalidJSON(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{"changes": "not-valid-json"})
	result, err := s.handleValidatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── handleGetFileContext ──────────────────────────────────────────────────────

func TestHandleGetFileContext_NoFile(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{})
	result, err := s.handleGetFileContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when file param is missing")
	}
}

func TestHandleGetFileContext_FoundFile(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{"file": "pkg/auth/auth.go"})
	result, err := s.handleGetFileContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestHandleGetFileContext_NotFound(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{"file": "nonexistent/file.go"})
	result, err := s.handleGetFileContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── handleGetContext additional coverage ──────────────────────────────────────

func TestHandleGetContext_NoEntity(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{})
	result, err := s.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when entity param is missing")
	}
}

func TestHandleGetContext_EntityFound(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{"entity": "AuthLogin"})
	result, err := s.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Errorf("unexpected tool error: %v", result.Content)
	}
}

func TestHandleGetContext_WithFilePin(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"entity": "AuthLogin",
		"file":   "pkg/auth/auth.go",
	})
	result, err := s.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestHandleGetContext_CompactFormat(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "compact",
	})
	result, err := s.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── toDirectionalContext ──────────────────────────────────────────────────────

func TestToDirectionalContext_WithEdges(t *testing.T) {
	n1 := &graph.Node{ID: "root", Name: "Root", Type: graph.NodeFunction}
	n2 := &graph.Node{ID: "callee", Name: "Callee", Type: graph.NodeFunction}
	n3 := &graph.Node{ID: "caller", Name: "Caller", Type: graph.NodeFunction}

	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: n1}, {Node: n2}, {Node: n3},
		},
		Edges: []*graph.Edge{
			{From: "root", To: "callee", Type: graph.EdgeCalls},
			{From: "caller", To: "root", Type: graph.EdgeCalls},
		},
	}
	dc := toDirectionalContext(sg)
	if dc.Root == nil {
		t.Fatal("expected non-nil root")
	}
	if dc.Root.Name != "Root" {
		t.Errorf("expected Root, got %q", dc.Root.Name)
	}
	if len(dc.Callees) == 0 {
		t.Error("expected non-empty callees")
	}
	if len(dc.Callers) == 0 {
		t.Error("expected non-empty callers")
	}
}

func TestToDirectionalContext_EmptySubgraph(t *testing.T) {
	sg := &graph.SubGraph{Root: "nonexistent"}
	dc := toDirectionalContext(sg)
	if dc.Root != nil {
		t.Error("expected nil root for empty subgraph")
	}
}

func TestToDirectionalContext_RelatedNode(t *testing.T) {
	n1 := &graph.Node{ID: "root", Name: "Root", Type: graph.NodeFunction}
	n2 := &graph.Node{ID: "impl", Name: "Impl", Type: graph.NodeStruct}

	// IMPLEMENTS edge (not CALLS) — should go to related bucket
	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: n1}, {Node: n2},
		},
		Edges: []*graph.Edge{
			{From: "impl", To: "root", Type: graph.EdgeImplements},
		},
	}
	dc := toDirectionalContext(sg)
	if len(dc.Related) == 0 {
		t.Error("expected non-empty related for non-CALLS edge")
	}
}

// ── handleGetWorkingState ─────────────────────────────────────────────────────

func TestHandleGetWorkingState_NonGitDir(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{})
	result, err := s.handleGetWorkingState(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── handleGetCallChain ────────────────────────────────────────────────────────

func TestHandleGetCallChain_NoParams(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{})
	result, err := s.handleGetCallChain(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestHandleGetCallChain_WithPath(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"from": "HandleRequest",
		"to":   "AuthLogin",
	})
	result, err := s.handleGetCallChain(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── handleSearch ──────────────────────────────────────────────────────────────

func TestHandleSearch_WithKeywordMode(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"query": "authentication",
		"mode":  "keyword",
	})
	result, err := s.handleSearch(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}
