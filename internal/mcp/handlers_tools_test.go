package mcp

// White-box tests for unexported helpers and additional handler coverage
// in tools.go: normalizeSubgraph, fileHasTests, matchRulesForFile,
// toDirectionalContext, and handlers: GetImpact, GetViolations, FindEntity,
// ValidatePlan, GetFileContext, GetContext, GetWorkingState, GetCallChain.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

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
	req := callTool(map[string]any{"query": "NotExistsXYZ", "format": "json"})
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
	req := callTool(map[string]any{"query": "AuthLogin", "format": "json"})
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
		"format": "json",
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

// DIAG-3: caller-count confidence warning for methods with use_go_types=false.
func TestHandleGetContext_CallerCountWarning_MethodNoCallers(t *testing.T) {
	st := openTestStore(t)
	g := graph.New("test-repo")
	// cfg from a temp dir that has no go.mod → UseGoTypes stays false.
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Verify the test precondition: use_go_types must be false.
	if cfg.UseGoTypes {
		t.Skip("use_go_types=true (go.mod found in TempDir?); DIAG-3 warning only fires when false")
	}
	// Add a NodeMethod with no callers.
	id := g.MakeNodeID("pkg/db/store.go", "Store.Close")
	g.AddNode(&graph.Node{
		ID:      id,
		Type:    graph.NodeMethod,
		Name:    "Store.Close",
		File:    "pkg/db/store.go",
		Package: "db",
	})
	s := New(g, cfg, st)
	req := callTool(map[string]any{"entity": "Store.Close", "format": "json"})
	result, err := s.handleGetContext(ctx, req)
	m := mustResult(t, result, err)
	if _, ok := m["caller_count_warning"]; !ok {
		t.Error("expected caller_count_warning in response for zero-caller method with use_go_types=false")
	}
}

func TestHandleGetContext_NoCallerWarning_WhenUseGoTypesTrue(t *testing.T) {
	st := openTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.UseGoTypes = true // explicitly enabled
	id := g.MakeNodeID("pkg/db/store.go", "Store.Close")
	g.AddNode(&graph.Node{
		ID:      id,
		Type:    graph.NodeMethod,
		Name:    "Store.Close",
		File:    "pkg/db/store.go",
		Package: "db",
	})
	s := New(g, cfg, st)
	req := callTool(map[string]any{"entity": "Store.Close", "format": "json"})
	result, err := s.handleGetContext(ctx, req)
	m := mustResult(t, result, err)
	if _, ok := m["caller_count_warning"]; ok {
		t.Error("expected no caller_count_warning when use_go_types=true")
	}
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

// ── toDirectionalContext: CrossDomain bucket (Sprint 16 #4) ──────────────────

func TestToDirectionalContext_InfraDomainGoesToCrossDomain(t *testing.T) {
	codeNode := &graph.Node{ID: "root", Name: "DeployService", Type: graph.NodeFunction, Domain: graph.DomainCode}
	infraNode := &graph.Node{ID: "infra", Name: "aws_lambda", Type: graph.NodeFunction, Domain: graph.DomainInfra}

	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: codeNode}, {Node: infraNode},
		},
		Edges: []*graph.Edge{
			{From: "root", To: "infra", Type: graph.EdgeDeploys},
		},
	}
	dc := toDirectionalContext(sg)

	if len(dc.CrossDomain) == 0 {
		t.Error("expected infra node in CrossDomain bucket")
	}
	if dc.CrossDomain[0].Node.Name != "aws_lambda" {
		t.Errorf("expected aws_lambda in CrossDomain, got %q", dc.CrossDomain[0].Node.Name)
	}
	if len(dc.Related) != 0 {
		t.Errorf("infra node should not be in Related bucket, got %d nodes", len(dc.Related))
	}
}

func TestToDirectionalContext_APIDomainGoesToCrossDomain(t *testing.T) {
	codeNode := &graph.Node{ID: "root", Name: "CallAPI", Type: graph.NodeFunction, Domain: graph.DomainCode}
	apiNode := &graph.Node{ID: "api", Name: "POST /users", Type: graph.NodeFunction, Domain: graph.DomainAPI}

	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: codeNode}, {Node: apiNode},
		},
		Edges: []*graph.Edge{
			{From: "root", To: "api", Type: graph.EdgeConsumes},
		},
	}
	dc := toDirectionalContext(sg)

	if len(dc.CrossDomain) == 0 {
		t.Error("expected API node in CrossDomain bucket")
	}
	if len(dc.Related) != 0 {
		t.Errorf("API node should not be in Related bucket, got %d nodes", len(dc.Related))
	}
}

func TestToDirectionalContext_CodeNodeGoesToRelatedNotCrossDomain(t *testing.T) {
	codeNode := &graph.Node{ID: "root", Name: "Root", Type: graph.NodeFunction, Domain: graph.DomainCode}
	siblingCode := &graph.Node{ID: "sibling", Name: "Helper", Type: graph.NodeFunction, Domain: graph.DomainCode}

	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: codeNode}, {Node: siblingCode},
		},
		Edges: []*graph.Edge{
			{From: "sibling", To: "root", Type: graph.EdgeImplements},
		},
	}
	dc := toDirectionalContext(sg)

	if len(dc.CrossDomain) != 0 {
		t.Errorf("code domain node should not be in CrossDomain bucket, got %d nodes", len(dc.CrossDomain))
	}
	if len(dc.Related) == 0 {
		t.Error("code domain node should be in Related bucket")
	}
}

func TestToDirectionalContext_EmptyDomainTreatedAsCode(t *testing.T) {
	// Nodes with empty Domain field default to code domain — should go to Related.
	codeNode := &graph.Node{ID: "root", Name: "Root", Type: graph.NodeFunction}
	nodomainNode := &graph.Node{ID: "nodomain", Name: "NoDomain", Type: graph.NodeFunction} // Domain is ""

	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: codeNode}, {Node: nodomainNode},
		},
		Edges: []*graph.Edge{},
	}
	dc := toDirectionalContext(sg)

	if len(dc.CrossDomain) != 0 {
		t.Errorf("empty-domain node should not be in CrossDomain bucket (defaults to code), got %d", len(dc.CrossDomain))
	}
	if len(dc.Related) == 0 {
		t.Error("empty-domain node should be in Related bucket")
	}
}

func TestToDirectionalContext_CrossDomainSortedByRelevance(t *testing.T) {
	root := &graph.Node{ID: "root", Name: "Root", Type: graph.NodeFunction, Domain: graph.DomainCode}
	infra1 := &graph.Node{ID: "infra1", Name: "Resource1", Type: graph.NodeFunction, Domain: graph.DomainInfra}
	infra2 := &graph.Node{ID: "infra2", Name: "Resource2", Type: graph.NodeFunction, Domain: graph.DomainInfra}

	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: root, Relevance: 1.0},
			{Node: infra1, Relevance: 0.3}, // lower relevance
			{Node: infra2, Relevance: 0.7}, // higher relevance
		},
		Edges: []*graph.Edge{},
	}
	dc := toDirectionalContext(sg)

	if len(dc.CrossDomain) != 2 {
		t.Fatalf("expected 2 cross-domain nodes, got %d", len(dc.CrossDomain))
	}
	// Should be sorted descending by relevance: infra2 (0.7) before infra1 (0.3)
	if dc.CrossDomain[0].Node.Name != "Resource2" {
		t.Errorf("expected Resource2 (higher relevance) first, got %q", dc.CrossDomain[0].Node.Name)
	}
	if dc.CrossDomain[1].Node.Name != "Resource1" {
		t.Errorf("expected Resource1 (lower relevance) second, got %q", dc.CrossDomain[1].Node.Name)
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

// ── handleDiscoverTools keyword scoring (IMP-EVAL-5) ─────────────────────────

// TestDiscoverTools_KeywordScoring verifies that specific natural-language queries
// rank the correct tool first. This catches regressions if keywords are changed.
func TestDiscoverTools_KeywordScoring(t *testing.T) {
	s := newTestServer(t)

	cases := []struct {
		query   string
		wantTop string // expected name of the top-ranked tool
	}{
		// find_entity: locate/find/where/defined
		{"where is AuthService defined", "find_entity"},
		{"locate symbol named Store", "find_entity"},
		{"find function HandleRequest", "find_entity"}, // avoids "file" which scores get_file_context

		// get_impact: blast/radius/breaks/callers/dependents/downstream
		{"what callers depend on Store.Close", "get_impact"},
		{"blast radius of CarveEgoGraph", "get_impact"},
		{"what breaks when changing this symbol", "get_impact"},
		{"downstream dependents of Handler", "get_impact"},

		// upsert_rule: rule/architectural/constraint/forbid/enforce/ban/policy/restrict
		{"add architectural constraint forbid direct imports", "upsert_rule"},
		{"ban handler database access restrict policy", "upsert_rule"},
		{"enforce architectural rule circular imports", "upsert_rule"},

		// upsert_rule: natural-language architectural phrasing (expanded keyword coverage)
		{"ensure handlers never access the database directly", "upsert_rule"},
		{"disallow direct imports from handlers to the store", "upsert_rule"},
		{"never allow the service to access the store directly", "upsert_rule"},

		// get_impact: natural-language dependency questions (expanded keyword coverage)
		{"who depends on this function", "get_impact"},
		{"downstream impact of changing this", "get_impact"},
		{"will this change break callers", "get_impact"},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{"query": tc.query}))
			if err != nil {
				t.Fatalf("handleDiscoverTools error: %v", err)
			}
			m := mustResult(t, res, nil)
			matches, ok := m["matches"].([]any)
			if !ok || len(matches) == 0 {
				t.Fatalf("query %q: expected matches, got %v", tc.query, m["matches"])
			}
			top, ok := matches[0].(map[string]any)
			if !ok {
				t.Fatalf("query %q: expected map in matches[0], got %T", tc.query, matches[0])
			}
			gotName, _ := top["name"].(string)
			if gotName != tc.wantTop {
				t.Errorf("query %q: want top=%q got top=%q (full matches: %v)",
					tc.query, tc.wantTop, gotName, matches)
			}
		})
	}
}

// ── handleGetContext disambiguation (BUG-EVAL-9) ─────────────────────────────

// TestHandleGetContext_Disambiguation_CompactFormat_ShowsShownMarker verifies
// that when multiple nodes share a name, the compact format output includes a
// "← shown" marker on the file that was actually selected, and that all file
// paths in the disambiguation block are repo-relative (not absolute).
func TestHandleGetContext_Disambiguation_CompactFormat_ShowsShownMarker(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// Set a real repo root so TrimPrefix produces relative paths.
	repoRoot := t.TempDir()
	g.SetRoot(repoRoot)

	// Two nodes with the same name in different files.
	fileA := filepath.Join(repoRoot, "pkg/a/a.go")
	fileB := filepath.Join(repoRoot, "pkg/b/b.go")

	idA := g.MakeNodeID(fileA, "DupFunc")
	g.AddNode(&graph.Node{
		ID: idA, Type: graph.NodeFunction, Name: "DupFunc",
		File: fileA, Line: 1, Package: "a", Exported: true,
	})
	idB := g.MakeNodeID(fileB, "DupFunc")
	g.AddNode(&graph.Node{
		ID: idB, Type: graph.NodeFunction, Name: "DupFunc",
		File: fileB, Line: 1, Package: "b", Exported: true,
	})

	s := New(g, cfg, st)
	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity": "DupFunc",
		"format": "compact",
	}))
	if err != nil {
		t.Fatalf("handleGetContext error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %v", res.Content)
	}

	// Extract the raw text (compact format is plain text, not JSON).
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	text := tc.Text

	// Must have the disambiguation block with "\u2190 shown".
	if !strings.Contains(text, "\u2190 shown") {
		t.Errorf("compact output missing '\u2190 shown' marker;\ngot:\n%s", text)
	}

	// All paths in the block must be relative — no absolute repoRoot prefix.
	if strings.Contains(text, repoRoot) {
		t.Errorf("compact output contains absolute path %q; want repo-relative paths only;\ngot:\n%s", repoRoot, text)
	}
}

// TestHandleGetContext_Disambiguation_JSONFormat_RelativePaths verifies that
// other_candidates[].file in the JSON response contains repo-relative paths,
// not absolute paths. Agents use these values directly as the file= argument
// in a follow-up get_context call, so absolute paths would be unusable.
func TestHandleGetContext_Disambiguation_JSONFormat_RelativePaths(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	repoRoot := t.TempDir()
	g.SetRoot(repoRoot)

	fileA := filepath.Join(repoRoot, "pkg/a/a.go")
	fileB := filepath.Join(repoRoot, "pkg/b/b.go")

	idA := g.MakeNodeID(fileA, "DupFunc")
	g.AddNode(&graph.Node{
		ID: idA, Type: graph.NodeFunction, Name: "DupFunc",
		File: fileA, Line: 1, Package: "a", Exported: true,
	})
	idB := g.MakeNodeID(fileB, "DupFunc")
	g.AddNode(&graph.Node{
		ID: idB, Type: graph.NodeFunction, Name: "DupFunc",
		File: fileB, Line: 1, Package: "b", Exported: true,
	})

	s := New(g, cfg, st)
	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity": "DupFunc",
		// default format = json
		"format": "json",
	}))
	if err != nil {
		t.Fatalf("handleGetContext error: %v", err)
	}
	m := mustResult(t, res, nil)

	otherCandidates, ok := m["other_candidates"].([]any)
	if !ok || len(otherCandidates) < 2 {
		t.Fatalf("expected other_candidates with \u22652 entries, got %v", m["other_candidates"])
	}

	for i, c := range otherCandidates {
		candidate, ok := c.(map[string]any)
		if !ok {
			t.Fatalf("other_candidates[%d]: expected map, got %T", i, c)
		}
		file, _ := candidate["file"].(string)
		if file == "" {
			t.Errorf("other_candidates[%d]: empty file path", i)
			continue
		}
		// Must be relative — must not start with the absolute repoRoot.
		if strings.HasPrefix(file, repoRoot) {
			t.Errorf("other_candidates[%d]: file=%q is absolute (contains repoRoot %q); want repo-relative", i, file, repoRoot)
		}
		// Sanity: relative path should look like pkg/a/a.go or pkg/b/b.go.
		if !strings.HasPrefix(file, "pkg/") {
			t.Errorf("other_candidates[%d]: file=%q doesn't look repo-relative (expected pkg/... prefix)", i, file)
		}
	}
}
