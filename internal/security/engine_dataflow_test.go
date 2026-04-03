package security

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ──────────────────────────────────────────────────────────────────────────────
// helpers for data flow tests
// ──────────────────────────────────────────────────────────────────────────────

// dataFlowPattern returns a minimal SecurityPattern for CheckTypeDataFlowPath
// with the given DB sink and validation patterns.
func dataFlowPattern(dbSinks, validationPats []string, maxDepth int) SecurityPattern {
	return SecurityPattern{
		ID:          "test-data-flow",
		Name:        "Test data flow",
		Language:    "go",
		Framework:   "*",
		PatternType: PatternTypeInputValidation,
		Severity:    SeverityMedium,
		Description: "test",
		Detection: Detection{
			CheckType:          CheckTypeDataFlowPath,
			DBSinkPatterns:     dbSinks,
			ValidationPatterns: validationPats,
			MaxCallDepth:       maxDepth,
			Scope:              ScopeFile,
		},
		Message: "Handler in {file} calls {sink} without validation.",
	}
}

// buildRouteHandlerGraph creates a graph with:
//
//	NodeRoute in handlerFile
//	NodeFunction handlerFn in handlerFile, calling the given direct callees
//
// Returns the graph.
func buildRouteHandlerGraph(t *testing.T, handlerFile, handlerFn string, callees ...string) *graph.Graph {
	t.Helper()
	g := buildTestGraph(t)
	addRouteNode(g, handlerFile, "POST", "/users")
	addFunctionWithCalls(g, handlerFile, handlerFn, callees...)
	return g
}

// addFnNode creates a NodeFunction in g at (filePath, fnName). Returns its NodeID.
// Unlike addFunctionWithCalls, this does NOT create any callee nodes or CALLS edges —
// use addCallBetween to wire up the call graph explicitly.
func addFnNode(g *graph.Graph, filePath, fnName string) graph.NodeID {
	id := g.MakeNodeID(filePath, fnName)
	g.AddNode(&graph.Node{
		ID:   id,
		Type: graph.NodeFunction,
		Name: fnName,
		File: filePath,
	})
	return id
}

// addCallBetween adds a CALLS edge from (srcFile,srcFn) to (dstFile,dstFn).
// Both nodes must already exist in the graph (created via addFnNode or addFunctionWithCalls).
func addCallBetween(g *graph.Graph, srcFile, srcFn, dstFile, dstFn string) {
	srcID := g.MakeNodeID(srcFile, srcFn)
	dstID := g.MakeNodeID(dstFile, dstFn)
	g.AddEdge(&graph.Edge{From: srcID, To: dstID, Type: graph.EdgeCalls})
}

// ──────────────────────────────────────────────────────────────────────────────
// reachableCallees
// ──────────────────────────────────────────────────────────────────────────────

func TestReachableCallees_Empty(t *testing.T) {
	g := buildTestGraph(t)
	result := reachableCallees(g, nil, 5)
	if len(result) != 0 {
		t.Errorf("expected empty map, got %v", result)
	}
}

func TestReachableCallees_DirectCallees(t *testing.T) {
	g := buildTestGraph(t)
	// handler → validateInput, queryDB
	addFunctionWithCalls(g, "/project/handler.go", "handleCreate", "validateInput", "queryDB")

	nodes := g.FindByFile("/project/handler.go")
	result := reachableCallees(g, nodes, 5)

	for _, want := range []string{"validateInput", "queryDB"} {
		if !result[want] {
			t.Errorf("expected %q in reachable, got %v", want, result)
		}
	}
}

func TestReachableCallees_TransitiveCallees(t *testing.T) {
	g := buildTestGraph(t)
	// handleCreate → ServiceCreate → RepoInsert (each in their own file).
	// Nodes must share consistent IDs so the CALLS edge from handleCreate points to
	// the same node that owns the edge to RepoInsert.
	addFnNode(g, "/project/handler.go", "handleCreate")
	addFnNode(g, "/project/service.go", "ServiceCreate")
	addFnNode(g, "/project/repo.go", "RepoInsert")
	addCallBetween(g, "/project/handler.go", "handleCreate", "/project/service.go", "ServiceCreate")
	addCallBetween(g, "/project/service.go", "ServiceCreate", "/project/repo.go", "RepoInsert")

	nodes := g.FindByFile("/project/handler.go")
	result := reachableCallees(g, nodes, 3)

	if !result["ServiceCreate"] {
		t.Errorf("expected direct callee ServiceCreate in reachable")
	}
	if !result["RepoInsert"] {
		t.Errorf("expected transitive callee RepoInsert in reachable at depth 2")
	}
}

func TestReachableCallees_DepthLimit(t *testing.T) {
	g := buildTestGraph(t)
	// depth chain: A → B → C → D, each function in its own file.
	// Consistent node IDs ensure the CALLS chain is properly linked.
	addFnNode(g, "/project/a.go", "A")
	addFnNode(g, "/project/b.go", "B")
	addFnNode(g, "/project/c.go", "C")
	addFnNode(g, "/project/d.go", "D")
	addCallBetween(g, "/project/a.go", "A", "/project/b.go", "B")
	addCallBetween(g, "/project/b.go", "B", "/project/c.go", "C")
	addCallBetween(g, "/project/c.go", "C", "/project/d.go", "D")

	nodes := g.FindByFile("/project/a.go")

	// depth=1: only B is reachable
	result1 := reachableCallees(g, nodes, 1)
	if !result1["B"] {
		t.Error("expected B at depth 1")
	}
	if result1["C"] {
		t.Error("C should NOT be reachable at depth 1")
	}

	// depth=2: B and C reachable
	result2 := reachableCallees(g, nodes, 2)
	if !result2["B"] || !result2["C"] {
		t.Error("expected B and C at depth 2")
	}
	if result2["D"] {
		t.Error("D should NOT be reachable at depth 2")
	}

	// depth=3: B, C, and D all reachable
	result3 := reachableCallees(g, nodes, 3)
	if !result3["D"] {
		t.Error("expected D at depth 3")
	}
}

func TestReachableCallees_CycleHandling(t *testing.T) {
	g := buildTestGraph(t)
	// Cycle: A → B → A (should not loop forever)
	fnA := g.MakeNodeID("/project/a.go", "A")
	g.AddNode(&graph.Node{ID: fnA, Type: graph.NodeFunction, Name: "A", File: "/project/a.go"})
	fnB := g.MakeNodeID("/project/b.go", "B")
	g.AddNode(&graph.Node{ID: fnB, Type: graph.NodeFunction, Name: "B", File: "/project/b.go"})
	g.AddEdge(&graph.Edge{From: fnA, To: fnB, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: fnB, To: fnA, Type: graph.EdgeCalls})

	nodes := g.FindByFile("/project/a.go")
	result := reachableCallees(g, nodes, 10) // large depth with cycle

	if !result["B"] {
		t.Error("expected B in reachable")
	}
	// Should complete without hanging.
}

func TestReachableCallees_MaxDepthZero(t *testing.T) {
	g := buildTestGraph(t)
	addFunctionWithCalls(g, "/project/handler.go", "handleCreate", "SomeCallee")
	nodes := g.FindByFile("/project/handler.go")

	// maxDepth=0: BFS skips all dequeued items immediately → no callees collected
	result := reachableCallees(g, nodes, 0)
	if result["SomeCallee"] {
		t.Error("expected no callees at depth 0")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// checkDataFlowPath — no violation cases
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckDataFlowPath_NoRoutes_NoViolation(t *testing.T) {
	g := buildTestGraph(t)
	// File has a function with DB calls but NO route node.
	addFunctionWithCalls(g, "/project/service.go", "DoWork", "InsertUser")
	fc := buildFileContext(g, "/project/service.go")

	p := dataFlowPattern([]string{"*Insert*"}, []string{"Validate*"}, 5)
	violations := checkDataFlowPath(fc, p, g)
	if violations != nil {
		t.Errorf("expected no violation for non-route file, got %v", violations)
	}
}

func TestCheckDataFlowPath_NoDBSink_NoViolation(t *testing.T) {
	g := buildRouteHandlerGraph(t, "/project/handler.go", "handleCreate", "logRequest", "respond")
	fc := buildFileContext(g, "/project/handler.go")

	p := dataFlowPattern([]string{"*QueryRow*"}, []string{"Validate*"}, 5)
	violations := checkDataFlowPath(fc, p, g)
	if violations != nil {
		t.Errorf("expected no violation when DB sink not reachable, got %v", violations)
	}
}

func TestCheckDataFlowPath_ValidationPresent_NoViolation(t *testing.T) {
	g := buildRouteHandlerGraph(t, "/project/handler.go", "handleCreate",
		"ValidateInput", "InsertUser")
	fc := buildFileContext(g, "/project/handler.go")

	p := dataFlowPattern([]string{"*Insert*"}, []string{"Validate*"}, 5)
	violations := checkDataFlowPath(fc, p, g)
	if violations != nil {
		t.Errorf("expected no violation when validation is present, got %v", violations)
	}
}

func TestCheckDataFlowPath_ValidationTransitive_NoViolation(t *testing.T) {
	// handler → ServiceCreate → ValidateInput + InsertUser (validation is transitive).
	// Use consistent node IDs so the CALLS chain is properly linked.
	g := buildTestGraph(t)
	addRouteNode(g, "/project/handler.go", "POST", "/users")
	addFnNode(g, "/project/handler.go", "handleCreate")
	addFnNode(g, "/project/service.go", "ServiceCreate")
	addFnNode(g, "/project/service.go", "ValidateInput")
	addFnNode(g, "/project/repo.go", "InsertUser")
	addCallBetween(g, "/project/handler.go", "handleCreate", "/project/service.go", "ServiceCreate")
	addCallBetween(g, "/project/service.go", "ServiceCreate", "/project/service.go", "ValidateInput")
	addCallBetween(g, "/project/service.go", "ServiceCreate", "/project/repo.go", "InsertUser")
	fc := buildFileContext(g, "/project/handler.go")

	p := dataFlowPattern([]string{"*Insert*"}, []string{"Validate*"}, 5)
	violations := checkDataFlowPath(fc, p, g)
	if violations != nil {
		t.Errorf("expected no violation when validation is transitive, got %v", violations)
	}
}

func TestCheckDataFlowPath_TestFile_NoViolation(t *testing.T) {
	g := buildRouteHandlerGraph(t, "/project/handler_test.go", "TestHandleCreate", "InsertUser")
	fc := buildFileContext(g, "/project/handler_test.go")

	p := dataFlowPattern([]string{"*Insert*"}, []string{"Validate*"}, 5)
	violations := checkDataFlowPath(fc, p, g)
	if violations != nil {
		t.Errorf("expected no violation for test file, got %v", violations)
	}
}

func TestCheckDataFlowPath_EmptyDBSinkPatterns_NoViolation(t *testing.T) {
	g := buildRouteHandlerGraph(t, "/project/handler.go", "handleCreate", "InsertUser")
	fc := buildFileContext(g, "/project/handler.go")

	// No DBSinkPatterns → check is a no-op
	p := dataFlowPattern(nil, []string{"Validate*"}, 5)
	violations := checkDataFlowPath(fc, p, g)
	if violations != nil {
		t.Errorf("expected no violation when DBSinkPatterns is empty, got %v", violations)
	}
}

func TestCheckDataFlowPath_EmptyValidationPatterns_NoViolation(t *testing.T) {
	g := buildRouteHandlerGraph(t, "/project/handler.go", "handleCreate", "InsertUser")
	fc := buildFileContext(g, "/project/handler.go")

	// No ValidationPatterns → can't detect validation presence; skip rather than over-fire
	p := dataFlowPattern([]string{"*Insert*"}, nil, 5)
	violations := checkDataFlowPath(fc, p, g)
	if violations != nil {
		t.Errorf("expected no violation when ValidationPatterns is empty, got %v", violations)
	}
}

func TestCheckDataFlowPath_DBSinkBeyondMaxDepth_NoViolation(t *testing.T) {
	// chain: handler → ServiceA → RepoB → InsertUser (depth 3 from handler).
	g := buildTestGraph(t)
	addRouteNode(g, "/project/handler.go", "POST", "/users")
	addFnNode(g, "/project/handler.go", "handleCreate")
	addFnNode(g, "/project/service.go", "ServiceA")
	addFnNode(g, "/project/repo.go", "RepoB")
	addFnNode(g, "/project/repo.go", "InsertUser")
	addCallBetween(g, "/project/handler.go", "handleCreate", "/project/service.go", "ServiceA")
	addCallBetween(g, "/project/service.go", "ServiceA", "/project/repo.go", "RepoB")
	addCallBetween(g, "/project/repo.go", "RepoB", "/project/repo.go", "InsertUser")
	fc := buildFileContext(g, "/project/handler.go")

	// maxDepth=1: InsertUser is at depth 3 — beyond limit; ServiceA is at depth 1 but not a DB sink
	p := dataFlowPattern([]string{"*Insert*"}, []string{"Validate*"}, 1)
	violations := checkDataFlowPath(fc, p, g)
	if violations != nil {
		t.Errorf("expected no violation when DB sink is beyond maxDepth, got %v", violations)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// checkDataFlowPath — violation cases
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckDataFlowPath_MissingValidation_DirectCall(t *testing.T) {
	// handler → InsertUser (no validation)
	g := buildRouteHandlerGraph(t, "/project/handler.go", "handleCreate", "InsertUser")
	fc := buildFileContext(g, "/project/handler.go")

	p := dataFlowPattern([]string{"*Insert*"}, []string{"Validate*"}, 5)
	violations := checkDataFlowPath(fc, p, g)
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(violations))
	}
	v := violations[0]
	if v.PatternID != "test-data-flow" {
		t.Errorf("unexpected PatternID: %q", v.PatternID)
	}
	if v.Severity != SeverityMedium {
		t.Errorf("expected MEDIUM severity, got %q", v.Severity)
	}
	if v.File != "/project/handler.go" {
		t.Errorf("unexpected File: %q", v.File)
	}
	// Action is set by CheckFile's withActions boundary, not by the check function
	// itself. See TestCheckFile_DataFlowPath_ActionSet for the full pipeline test.
}

func TestCheckDataFlowPath_MissingValidation_TransitiveDBSink(t *testing.T) {
	// handler → ServiceCreate → InsertUser (no validation anywhere, DB sink is transitive).
	g := buildTestGraph(t)
	addRouteNode(g, "/project/handler.go", "POST", "/users")
	addFnNode(g, "/project/handler.go", "handleCreate")
	addFnNode(g, "/project/service.go", "ServiceCreate")
	addFnNode(g, "/project/repo.go", "InsertUser")
	addCallBetween(g, "/project/handler.go", "handleCreate", "/project/service.go", "ServiceCreate")
	addCallBetween(g, "/project/service.go", "ServiceCreate", "/project/repo.go", "InsertUser")
	fc := buildFileContext(g, "/project/handler.go")

	p := dataFlowPattern([]string{"*Insert*"}, []string{"Validate*"}, 5)
	violations := checkDataFlowPath(fc, p, g)
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation for transitive DB sink with no validation, got %d", len(violations))
	}
}

func TestCheckDataFlowPath_ViolationEvidenceContainsSink(t *testing.T) {
	g := buildRouteHandlerGraph(t, "/project/handler.go", "handleUpdate", "UpdateUser")
	fc := buildFileContext(g, "/project/handler.go")

	p := dataFlowPattern([]string{"*Update*"}, []string{"Validate*"}, 5)
	violations := checkDataFlowPath(fc, p, g)
	if len(violations) == 0 {
		t.Fatal("expected violation")
	}
	v := violations[0]
	if v.Evidence == "" {
		t.Error("evidence should not be empty")
	}
}

func TestCheckDataFlowPath_ViolationMessageTemplate(t *testing.T) {
	g := buildRouteHandlerGraph(t, "/project/api/users.go", "handleCreate", "QueryRowContext")
	fc := buildFileContext(g, "/project/api/users.go")

	p := dataFlowPattern([]string{"*QueryRow*", "*QueryContext*"}, []string{"Validate*"}, 5)
	violations := checkDataFlowPath(fc, p, g)
	if len(violations) == 0 {
		t.Fatal("expected violation")
	}
	// Message should not contain unfilled placeholders.
	if v := violations[0].Message; v == "" {
		t.Error("message should not be empty")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// CheckFile dispatch — data_flow_path via Engine
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckFile_DataFlowPath_Dispatched(t *testing.T) {
	g := buildRouteHandlerGraph(t, "/project/handler.go", "handleCreate", "ExecContext")
	p := dataFlowPattern([]string{"*Exec*"}, []string{"Validate*"}, 5)
	eng := makeEngine(p)

	violations := eng.CheckFile(g, "/project/handler.go", nil)
	if len(violations) == 0 {
		t.Fatal("expected CheckFile to dispatch CheckTypeDataFlowPath and return a violation")
	}
	if violations[0].PatternID != "test-data-flow" {
		t.Errorf("unexpected PatternID %q", violations[0].PatternID)
	}
}

func TestCheckFile_DataFlowPath_ActionSet(t *testing.T) {
	g := buildRouteHandlerGraph(t, "/project/handler.go", "handleCreate", "InsertUser")
	p := dataFlowPattern([]string{"*Insert*"}, []string{"Validate*"}, 5)
	eng := makeEngine(p)

	violations := eng.CheckFile(g, "/project/handler.go", nil)
	if len(violations) == 0 {
		t.Fatal("expected a violation")
	}
	if violations[0].Action != "inform" {
		t.Errorf("expected action 'inform' for MEDIUM severity, got %q", violations[0].Action)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Nil / empty guard
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckDataFlowPath_NilGraph(t *testing.T) {
	eng := DefaultEngine()
	violations := eng.CheckFile(nil, "/project/handler.go", nil)
	if violations != nil {
		t.Errorf("expected nil with nil graph, got %v", violations)
	}
}

func TestCheckDataFlowPath_EmptyFile(t *testing.T) {
	g := buildTestGraph(t)
	fc := buildFileContext(g, "/project/handler.go") // no nodes

	p := dataFlowPattern([]string{"*Insert*"}, []string{"Validate*"}, 5)
	violations := checkDataFlowPath(fc, p, g)
	if violations != nil {
		t.Errorf("expected nil with no nodes, got %v", violations)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Built-in patterns load correctly
// ──────────────────────────────────────────────────────────────────────────────

func TestDataFlowBuiltinPatternsLoad(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin() error: %v", err)
	}
	dataFlowPatterns := ps.ForCheckType(CheckTypeDataFlowPath)
	if len(dataFlowPatterns) == 0 {
		t.Fatal("expected at least one data_flow_path pattern to be loaded from builtin")
	}
	langs := make(map[string]bool)
	for _, p := range dataFlowPatterns {
		langs[p.Language] = true
	}
	for _, lang := range []string{"go", "typescript", "javascript", "python", "java", "rust"} {
		if !langs[lang] {
			t.Errorf("expected data_flow_path pattern for language %q, not found", lang)
		}
	}
}
