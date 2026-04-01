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

// ── toDirectionalContext: CrossDomain structured buckets ──────────────────────

func TestToDirectionalContext_DeployEdgeGoesToDeploys(t *testing.T) {
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

	if dc.CrossDomain == nil || len(dc.CrossDomain.Deploys) != 1 {
		t.Fatal("expected infra node in CrossDomain.Deploys")
	}
	if dc.CrossDomain.Deploys[0].Node.Name != "aws_lambda" {
		t.Errorf("expected aws_lambda in Deploys, got %q", dc.CrossDomain.Deploys[0].Node.Name)
	}
	if len(dc.Related) != 0 {
		t.Errorf("infra node should not be in Related bucket, got %d nodes", len(dc.Related))
	}
}

func TestToDirectionalContext_ConsumeEdgeGoesToConsumes(t *testing.T) {
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

	if dc.CrossDomain == nil || len(dc.CrossDomain.Consumes) != 1 {
		t.Fatal("expected API node in CrossDomain.Consumes")
	}
	if len(dc.Related) != 0 {
		t.Errorf("API node should not be in Related bucket, got %d nodes", len(dc.Related))
	}
}

func TestToDirectionalContext_ConfiguredByEdgeGoesToConfiguredBy(t *testing.T) {
	codeNode := &graph.Node{ID: "root", Name: "PaymentService", Type: graph.NodeFunction, Domain: graph.DomainCode}
	cfgNode := &graph.Node{ID: "cfg", Name: "payment.yaml", Type: graph.NodeFunction, Domain: graph.DomainCustom}

	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: codeNode}, {Node: cfgNode},
		},
		Edges: []*graph.Edge{
			{From: "root", To: "cfg", Type: graph.EdgeConfiguredBy},
		},
	}
	dc := toDirectionalContext(sg)

	if dc.CrossDomain == nil || len(dc.CrossDomain.ConfiguredBy) != 1 {
		t.Fatal("expected config node in CrossDomain.ConfiguredBy")
	}
}

func TestToDirectionalContext_MentionsEdgeGoesToMentions(t *testing.T) {
	codeNode := &graph.Node{ID: "root", Name: "AuthService", Type: graph.NodeFunction, Domain: graph.DomainCode}
	kNode := &graph.Node{ID: "kb", Name: "Auth Guide", Type: graph.NodeFunction, Domain: graph.DomainKnowledge}

	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: codeNode}, {Node: kNode},
		},
		Edges: []*graph.Edge{
			{From: "kb", To: "root", Type: graph.EdgeMentions},
		},
	}
	dc := toDirectionalContext(sg)

	if dc.CrossDomain == nil || len(dc.CrossDomain.Mentions) != 1 {
		t.Fatal("expected knowledge node in CrossDomain.Mentions")
	}
}

func TestToDirectionalContext_ManualEdgeGoesToManual(t *testing.T) {
	codeNode := &graph.Node{ID: "root", Name: "Root", Type: graph.NodeFunction, Domain: graph.DomainCode}
	infraNode := &graph.Node{ID: "infra", Name: "manual-link", Type: graph.NodeFunction, Domain: graph.DomainInfra}

	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: codeNode}, {Node: infraNode},
		},
		Edges: []*graph.Edge{
			{From: "root", To: "infra", Type: graph.EdgeManual},
		},
	}
	dc := toDirectionalContext(sg)

	if dc.CrossDomain == nil || len(dc.CrossDomain.Manual) != 1 {
		t.Fatal("expected node in CrossDomain.Manual")
	}
}

func TestToDirectionalContext_MultiHopCrossDomainGoesToRelated(t *testing.T) {
	// Infra node reached via multi-hop (no direct edge from root) → CrossDomain.Related
	root := &graph.Node{ID: "root", Name: "Root", Type: graph.NodeFunction, Domain: graph.DomainCode}
	mid := &graph.Node{ID: "mid", Name: "Mid", Type: graph.NodeFunction, Domain: graph.DomainCode}
	infraNode := &graph.Node{ID: "infra", Name: "aws_ec2", Type: graph.NodeFunction, Domain: graph.DomainInfra}

	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: root}, {Node: mid}, {Node: infraNode},
		},
		Edges: []*graph.Edge{
			{From: "root", To: "mid", Type: graph.EdgeCalls},
			{From: "mid", To: "infra", Type: graph.EdgeDeploys},
		},
	}
	dc := toDirectionalContext(sg)

	if dc.CrossDomain == nil || len(dc.CrossDomain.Related) != 1 {
		t.Fatal("expected multi-hop infra node in CrossDomain.Related")
	}
	if dc.CrossDomain.Related[0].Node.Name != "aws_ec2" {
		t.Errorf("expected aws_ec2, got %q", dc.CrossDomain.Related[0].Node.Name)
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

	if dc.CrossDomain != nil {
		t.Errorf("code domain node should not produce CrossDomain struct")
	}
	if len(dc.Related) == 0 {
		t.Error("code domain node should be in Related bucket")
	}
}

func TestToDirectionalContext_EmptyDomainTreatedAsCode(t *testing.T) {
	codeNode := &graph.Node{ID: "root", Name: "Root", Type: graph.NodeFunction}
	nodomainNode := &graph.Node{ID: "nodomain", Name: "NoDomain", Type: graph.NodeFunction}

	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: codeNode}, {Node: nodomainNode},
		},
		Edges: []*graph.Edge{},
	}
	dc := toDirectionalContext(sg)

	if dc.CrossDomain != nil {
		t.Errorf("empty-domain node should not produce CrossDomain struct")
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
			{Node: infra1, Relevance: 0.3},
			{Node: infra2, Relevance: 0.7},
		},
		Edges: []*graph.Edge{
			{From: "root", To: "infra1", Type: graph.EdgeDeploys},
			{From: "root", To: "infra2", Type: graph.EdgeDeploys},
		},
	}
	dc := toDirectionalContext(sg)

	if dc.CrossDomain == nil || len(dc.CrossDomain.Deploys) != 2 {
		t.Fatalf("expected 2 nodes in Deploys, got %v", dc.CrossDomain)
	}
	if dc.CrossDomain.Deploys[0].Node.Name != "Resource2" {
		t.Errorf("expected Resource2 (higher relevance) first, got %q", dc.CrossDomain.Deploys[0].Node.Name)
	}
	if dc.CrossDomain.Deploys[1].Node.Name != "Resource1" {
		t.Errorf("expected Resource1 (lower relevance) second, got %q", dc.CrossDomain.Deploys[1].Node.Name)
	}
}

func TestToDirectionalContext_NilWhenNoCrossDomainNodes(t *testing.T) {
	root := &graph.Node{ID: "root", Name: "Root", Type: graph.NodeFunction, Domain: graph.DomainCode}
	peer := &graph.Node{ID: "peer", Name: "Peer", Type: graph.NodeFunction, Domain: graph.DomainCode}

	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: root}, {Node: peer},
		},
		Edges: []*graph.Edge{
			{From: "root", To: "peer", Type: graph.EdgeCalls},
		},
	}
	dc := toDirectionalContext(sg)

	if dc.CrossDomain != nil {
		t.Error("CrossDomain should be nil when no cross-domain nodes exist")
	}
}

func TestToDirectionalContext_DocNodeWithDirectLinkGoesToDocumentation(t *testing.T) {
	// A doc node directly linked to root via DOCUMENTED_BY goes to Documentation, not CrossDomain.
	root := &graph.Node{ID: "root", Name: "Root", Type: graph.NodeFunction, Domain: graph.DomainCode}
	docNode := &graph.Node{ID: "doc", Name: "README section", Type: graph.NodeFunction, Domain: graph.DomainDocs}

	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: root}, {Node: docNode},
		},
		Edges: []*graph.Edge{
			{From: "root", To: "doc", Type: graph.EdgeDocumentedBy},
		},
	}
	dc := toDirectionalContext(sg)

	if len(dc.Documentation) != 1 {
		t.Fatal("expected doc node in Documentation bucket")
	}
	if dc.CrossDomain != nil {
		t.Error("doc node with direct DOCUMENTED_BY should not appear in CrossDomain")
	}
}

func TestToDirectionalContext_EdgeDocumentsGoesToDocumentation(t *testing.T) {
	// EdgeDocuments (doc→root) is the reverse of EdgeDocumentedBy — should also
	// route to Documentation, not CrossDomain.DocumentedIn.
	root := &graph.Node{ID: "root", Name: "Root", Type: graph.NodeFunction, Domain: graph.DomainCode}
	docNode := &graph.Node{ID: "doc", Name: "API doc section", Type: graph.NodeFunction, Domain: graph.DomainDocs}

	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: root}, {Node: docNode},
		},
		Edges: []*graph.Edge{
			{From: "doc", To: "root", Type: graph.EdgeDocuments},
		},
	}
	dc := toDirectionalContext(sg)

	if len(dc.Documentation) != 1 {
		t.Fatalf("expected doc node in Documentation bucket, got %d", len(dc.Documentation))
	}
	if dc.CrossDomain != nil {
		t.Error("doc node with direct DOCUMENTS edge to root should not appear in CrossDomain")
	}
}

func TestToDirectionalContext_AllEdgeTypesRouteCorrectly(t *testing.T) {
	// Integration test: one node per edge type, verify all route to correct sub-bucket.
	root := &graph.Node{ID: "root", Name: "Root", Type: graph.NodeFunction, Domain: graph.DomainCode}
	infra := &graph.Node{ID: "n1", Name: "Lambda", Type: graph.NodeFunction, Domain: graph.DomainInfra}
	api := &graph.Node{ID: "n2", Name: "POST /pay", Type: graph.NodeFunction, Domain: graph.DomainAPI}
	cfg := &graph.Node{ID: "n3", Name: "pay.yaml", Type: graph.NodeFunction, Domain: graph.DomainCustom}
	kb := &graph.Node{ID: "n4", Name: "Wiki page", Type: graph.NodeFunction, Domain: graph.DomainKnowledge}
	manual := &graph.Node{ID: "n5", Name: "linked", Type: graph.NodeFunction, Domain: graph.DomainInfra}
	multiHop := &graph.Node{ID: "n6", Name: "distant", Type: graph.NodeFunction, Domain: graph.DomainAPI}

	sg := &graph.SubGraph{
		Root: "root",
		Nodes: []graph.CarvedNode{
			{Node: root}, {Node: infra}, {Node: api}, {Node: cfg},
			{Node: kb}, {Node: manual}, {Node: multiHop},
		},
		Edges: []*graph.Edge{
			{From: "root", To: "n1", Type: graph.EdgeDeploys},
			{From: "root", To: "n2", Type: graph.EdgeConsumes},
			{From: "root", To: "n3", Type: graph.EdgeConfiguredBy},
			{From: "n4", To: "root", Type: graph.EdgeMentions},
			{From: "root", To: "n5", Type: graph.EdgeManual},
			// n6 has no direct edge from root — multi-hop
		},
	}
	dc := toDirectionalContext(sg)

	if dc.CrossDomain == nil {
		t.Fatal("expected non-nil CrossDomain")
	}
	if len(dc.CrossDomain.Deploys) != 1 || dc.CrossDomain.Deploys[0].Node.Name != "Lambda" {
		t.Errorf("Deploys: got %v", dc.CrossDomain.Deploys)
	}
	if len(dc.CrossDomain.Consumes) != 1 || dc.CrossDomain.Consumes[0].Node.Name != "POST /pay" {
		t.Errorf("Consumes: got %v", dc.CrossDomain.Consumes)
	}
	if len(dc.CrossDomain.ConfiguredBy) != 1 || dc.CrossDomain.ConfiguredBy[0].Node.Name != "pay.yaml" {
		t.Errorf("ConfiguredBy: got %v", dc.CrossDomain.ConfiguredBy)
	}
	if len(dc.CrossDomain.Mentions) != 1 || dc.CrossDomain.Mentions[0].Node.Name != "Wiki page" {
		t.Errorf("Mentions: got %v", dc.CrossDomain.Mentions)
	}
	if len(dc.CrossDomain.Manual) != 1 || dc.CrossDomain.Manual[0].Node.Name != "linked" {
		t.Errorf("Manual: got %v", dc.CrossDomain.Manual)
	}
	if len(dc.CrossDomain.Related) != 1 || dc.CrossDomain.Related[0].Node.Name != "distant" {
		t.Errorf("Related: got %v", dc.CrossDomain.Related)
	}
	if len(dc.CrossDomain.DocumentedIn) != 0 {
		t.Errorf("DocumentedIn should be empty, got %v", dc.CrossDomain.DocumentedIn)
	}
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
