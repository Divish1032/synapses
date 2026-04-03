package mcp

// Tests for Sprint 23.6: security_constraints section in get_context.
//
// Covers:
//   - matchesSecurityPattern: auth/middleware/validate/sanitize name matching
//   - observeFileNorms: graph-based observed norm detection
//   - serializeCompact rendering: 🔒 section present/absent
//   - contextEnrichment.SecurityConstraints: populated for add/modify intent, absent otherwise

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── matchesSecurityPattern ────────────────────────────────────────────────────

func TestMatchesSecurityPattern_AuthVariants(t *testing.T) {
	matches := []string{
		"AuthMiddleware", "authenticate", "Authorization",
		"checkAuth", "authHandler", "AUTH",
	}
	for _, name := range matches {
		if !matchesSecurityPattern(name) {
			t.Errorf("matchesSecurityPattern(%q) = false, want true", name)
		}
	}
}

func TestMatchesSecurityPattern_MiddlewareVariants(t *testing.T) {
	matches := []string{
		"Middleware", "middleware", "AuthMiddleware", "RateMiddleware",
	}
	for _, name := range matches {
		if !matchesSecurityPattern(name) {
			t.Errorf("matchesSecurityPattern(%q) = false, want true", name)
		}
	}
}

func TestMatchesSecurityPattern_ValidateVariants(t *testing.T) {
	matches := []string{
		"ValidateInput", "validateRequest", "Validate", "validate",
	}
	for _, name := range matches {
		if !matchesSecurityPattern(name) {
			t.Errorf("matchesSecurityPattern(%q) = false, want true", name)
		}
	}
}

func TestMatchesSecurityPattern_SanitizeVariants(t *testing.T) {
	matches := []string{
		"SanitizeInput", "sanitize", "SANITIZE",
	}
	for _, name := range matches {
		if !matchesSecurityPattern(name) {
			t.Errorf("matchesSecurityPattern(%q) = false, want true", name)
		}
	}
}

func TestMatchesSecurityPattern_NonSecurityNames(t *testing.T) {
	nonMatches := []string{
		"HandleRequest", "GetUser", "processOrder", "Database",
		"Repository", "Logger", "Config", "Parse", "Format",
		"Render", "buildQuery", "fetchData", "updateRecord",
	}
	for _, name := range nonMatches {
		if matchesSecurityPattern(name) {
			t.Errorf("matchesSecurityPattern(%q) = true, want false", name)
		}
	}
}

// ── observeFileNorms ─────────────────────────────────────────────────────────

func TestObserveFileNorms_NoneFound_NoSecurityCalls(t *testing.T) {
	g := graph.New("test-repo")
	file := "pkg/api/handler.go"
	// Two functions that call non-security targets
	fn1 := makeTestFuncNode(g, file, "HandleGet", 1)
	fn2 := makeTestFuncNode(g, file, "HandlePost", 2)
	db := makeTestFuncNode(g, "pkg/db/db.go", "QueryDB", 10)
	g.AddEdge(&graph.Edge{From: fn1, To: db, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: fn2, To: db, Type: graph.EdgeCalls})

	norms := observeFileNorms(g, file)
	if len(norms) != 0 {
		t.Errorf("expected no norms when no security calls, got %v", norms)
	}
}

func TestObserveFileNorms_SecurityCallsDetected(t *testing.T) {
	g := graph.New("test-repo")
	file := "pkg/api/handler.go"
	// Three functions — two call AuthMiddleware, one doesn't
	fn1 := makeTestFuncNode(g, file, "HandleGet", 1)
	fn2 := makeTestFuncNode(g, file, "HandlePost", 2)
	fn3 := makeTestFuncNode(g, file, "HandleDelete", 3)
	authFn := makeTestFuncNode(g, "pkg/middleware/auth.go", "AuthMiddleware", 5)
	dbFn := makeTestFuncNode(g, "pkg/db/db.go", "QueryDB", 10)
	g.AddEdge(&graph.Edge{From: fn1, To: authFn, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: fn2, To: authFn, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: fn3, To: dbFn, Type: graph.EdgeCalls})

	norms := observeFileNorms(g, file)
	if len(norms) != 1 {
		t.Fatalf("expected 1 norm, got %d: %v", len(norms), norms)
	}
	// Should report "2/3 functions in this file call auth/security patterns"
	if !strings.Contains(norms[0], "2/3") {
		t.Errorf("norm should contain '2/3', got %q", norms[0])
	}
	if !strings.Contains(norms[0], "auth") {
		t.Errorf("norm should mention 'auth', got %q", norms[0])
	}
}

func TestObserveFileNorms_TooFewNodes_ReturnsNil(t *testing.T) {
	g := graph.New("test-repo")
	file := "pkg/single/single.go"
	// Only one function — not enough to form "N/M"
	fn1 := makeTestFuncNode(g, file, "OnlyFunc", 1)
	auth := makeTestFuncNode(g, "pkg/auth/auth.go", "AuthMiddleware", 5)
	g.AddEdge(&graph.Edge{From: fn1, To: auth, Type: graph.EdgeCalls})

	norms := observeFileNorms(g, file)
	if len(norms) != 0 {
		t.Errorf("expected nil for single function, got %v", norms)
	}
}

func TestObserveFileNorms_EmptyFile_ReturnsNil(t *testing.T) {
	g := graph.New("test-repo")
	norms := observeFileNorms(g, "nonexistent/file.go")
	if len(norms) != 0 {
		t.Errorf("expected nil for empty file, got %v", norms)
	}
}

func TestObserveFileNorms_NonCallEdgesIgnored(t *testing.T) {
	g := graph.New("test-repo")
	file := "pkg/api/handler.go"
	// Two functions with non-CALLS edges to auth functions — should not count
	fn1 := makeTestFuncNode(g, file, "HandleGet", 1)
	fn2 := makeTestFuncNode(g, file, "HandlePost", 2)
	authFn := makeTestFuncNode(g, "pkg/middleware/auth.go", "AuthMiddleware", 5)
	// Use EdgeImplements, not EdgeCalls
	g.AddEdge(&graph.Edge{From: fn1, To: authFn, Type: graph.EdgeImplements})
	g.AddEdge(&graph.Edge{From: fn2, To: authFn, Type: graph.EdgeImplements})

	norms := observeFileNorms(g, file)
	if len(norms) != 0 {
		t.Errorf("non-CALLS edges should not trigger security norm, got %v", norms)
	}
}

// ── serializeCompact: SecurityConstraints rendering ──────────────────────────

func TestSerializeCompact_SecurityConstraints_Rendered(t *testing.T) {
	dc := minimalDCWithRoot()
	dc.Enrichment = &contextEnrichment{
		SecurityConstraints: []string{
			"Handlers must not import database/sql directly [ERROR]",
			"All API endpoints must use AuthMiddleware [WARNING]",
		},
	}

	out := serializeCompact(dc, "full")
	if !strings.Contains(out, "🔒 Security constraints") {
		t.Errorf("expected security constraints section, got:\n%s", out)
	}
	if !strings.Contains(out, "Handlers must not import database/sql directly") {
		t.Errorf("expected first constraint in output, got:\n%s", out)
	}
	if !strings.Contains(out, "All API endpoints must use AuthMiddleware") {
		t.Errorf("expected second constraint in output, got:\n%s", out)
	}
}

func TestSerializeCompact_SecurityConstraints_AbsentWhenEmpty(t *testing.T) {
	dc := minimalDCWithRoot()
	dc.Enrichment = &contextEnrichment{
		RuleAlerts: []ruleAlert{
			{RuleID: "r1", Severity: "HIGH", FromNode: "a", ToNode: "b", EdgeType: "CALLS"},
		},
		// No SecurityConstraints
	}

	out := serializeCompact(dc, "full")
	if strings.Contains(out, "🔒") {
		t.Errorf("security constraints section should be absent when empty, got:\n%s", out)
	}
	// RuleAlerts should still appear
	if !strings.Contains(out, "rule violation") {
		t.Errorf("rule alerts should still render, got:\n%s", out)
	}
}

func TestSerializeCompact_SecurityConstraints_AbsentWhenNilEnrichment(t *testing.T) {
	dc := minimalDCWithRoot()
	// dc.Enrichment is nil

	out := serializeCompact(dc, "full")
	if strings.Contains(out, "🔒") {
		t.Errorf("security section should be absent with nil enrichment, got:\n%s", out)
	}
}

func TestSerializeCompact_SecurityConstraints_RenderedAtSummaryLevel(t *testing.T) {
	// Security constraints should appear even at summary level — agents need safety info
	// at every detail level.
	dc := minimalDCWithRoot()
	dc.Enrichment = &contextEnrichment{
		SecurityConstraints: []string{"No direct DB access from handler [ERROR]"},
	}

	out := serializeCompact(dc, "summary")
	if !strings.Contains(out, "🔒 Security constraints") {
		t.Errorf("security constraints must appear at summary level, got:\n%s", out)
	}
}

func TestSerializeCompact_SecurityConstraints_ShowsCount(t *testing.T) {
	dc := minimalDCWithRoot()
	dc.Enrichment = &contextEnrichment{
		SecurityConstraints: []string{
			"Rule A [ERROR]",
			"Rule B [WARNING]",
			"Rule C [WARNING]",
		},
	}

	out := serializeCompact(dc, "full")
	if !strings.Contains(out, "(3)") {
		t.Errorf("security constraints header should show count (3), got:\n%s", out)
	}
}

// ── Integration: SecurityConstraints populated by enrichment goroutine ───────

func TestGetContext_SecurityConstraints_PopulatedForAddIntent(t *testing.T) {
	g := graph.New("test-repo")
	st := openMCPTestStore(t)
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Add a structural rule that applies to handlers
	cfg.Rules = []config.Rule{
		{
			ID:          "no-db-in-handler",
			Description: "Handlers must not import database/sql directly",
			Severity:    "error",
			ForbiddenEdge: config.ForbiddenEdge{
				FromFilePattern: "handler.go",
			},
		},
	}

	// Add a function in the target file
	fnID := g.MakeNodeID("handler.go", "HandleRequest")
	g.AddNode(&graph.Node{
		ID: fnID, Type: graph.NodeFunction,
		Name: "HandleRequest", File: "handler.go", Line: 1,
	})

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	req := callTool(map[string]any{
		"entity": "HandleRequest",
		"intent": "add",
		"format": "json",
	})

	result, err := srv.handleGetContext(ctx, req)
	m := mustResult(t, result, err)

	// The enrichment section must exist and contain security_constraints
	enrichRaw, ok := m["enrichment"]
	if !ok {
		t.Fatal("expected 'enrichment' key in response")
	}
	enrichMap, ok := enrichRaw.(map[string]any)
	if !ok {
		t.Fatalf("enrichment is not a map: %T", enrichRaw)
	}
	constraintsRaw, ok := enrichMap["security_constraints"]
	if !ok {
		t.Fatal("expected 'security_constraints' key in enrichment")
	}
	constraints, ok := constraintsRaw.([]any)
	if !ok {
		t.Fatalf("security_constraints is not a list: %T", constraintsRaw)
	}
	if len(constraints) == 0 {
		t.Fatal("expected at least one security constraint")
	}
	// Verify the constraint contains the rule description
	found := false
	for _, c := range constraints {
		if s, ok := c.(string); ok && strings.Contains(s, "Handlers must not import database/sql") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("rule description not found in constraints: %v", constraints)
	}
}

func TestGetContext_SecurityConstraints_AbsentForUnderstandIntent(t *testing.T) {
	g := graph.New("test-repo")
	st := openMCPTestStore(t)
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Rules = []config.Rule{
		{
			ID:          "no-db-in-handler",
			Description: "Handlers must not import database/sql directly",
			Severity:    "error",
		},
	}

	fnID := g.MakeNodeID("handler.go", "HandleRequest")
	g.AddNode(&graph.Node{
		ID: fnID, Type: graph.NodeFunction,
		Name: "HandleRequest", File: "handler.go", Line: 1,
	})

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	// "understand" intent should NOT trigger security_constraints
	req := callTool(map[string]any{
		"entity": "HandleRequest",
		"intent": "understand",
		"format": "json",
	})

	result, err := srv.handleGetContext(ctx, req)
	m := mustResult(t, result, err)

	if enrichRaw, ok := m["enrichment"]; ok {
		if enrichMap, ok := enrichRaw.(map[string]any); ok {
			if _, ok := enrichMap["security_constraints"]; ok {
				t.Error("security_constraints should be absent for intent=understand")
			}
		}
	}
	// Absence of enrichment entirely is also fine
}

func TestGetContext_SecurityConstraints_AbsentWhenNoIntent(t *testing.T) {
	g := graph.New("test-repo")
	st := openMCPTestStore(t)
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Rules = []config.Rule{
		{ID: "r1", Description: "Some rule", Severity: "warning"},
	}

	fnID := g.MakeNodeID("svc.go", "DoWork")
	g.AddNode(&graph.Node{
		ID: fnID, Type: graph.NodeFunction,
		Name: "DoWork", File: "svc.go", Line: 1,
	})

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	// No intent provided — security_constraints should not appear
	req := callTool(map[string]any{"entity": "DoWork", "format": "json"})

	result, err := srv.handleGetContext(ctx, req)
	m := mustResult(t, result, err)

	if enrichRaw, ok := m["enrichment"]; ok {
		if enrichMap, ok := enrichRaw.(map[string]any); ok {
			if _, ok := enrichMap["security_constraints"]; ok {
				t.Error("security_constraints should be absent when no intent given")
			}
		}
	}
}

func TestGetContext_SecurityConstraints_PopulatedForModifyIntent(t *testing.T) {
	g := graph.New("test-repo")
	st := openMCPTestStore(t)
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Rules = []config.Rule{
		{ID: "r1", Description: "Use repository layer for DB access", Severity: "warning"},
	}

	fnID := g.MakeNodeID("service.go", "UpdateUser")
	g.AddNode(&graph.Node{
		ID: fnID, Type: graph.NodeFunction,
		Name: "UpdateUser", File: "service.go", Line: 5,
	})

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	req := callTool(map[string]any{
		"entity": "UpdateUser",
		"intent": "modify",
		"format": "json",
	})

	result, err := srv.handleGetContext(ctx, req)
	m := mustResult(t, result, err)

	enrichRaw, ok := m["enrichment"]
	if !ok {
		t.Fatal("expected enrichment section")
	}
	enrichMap := enrichRaw.(map[string]any)
	constraintsRaw, ok := enrichMap["security_constraints"]
	if !ok {
		t.Fatal("expected security_constraints for intent=modify")
	}
	constraints := constraintsRaw.([]any)
	if len(constraints) == 0 {
		t.Fatal("expected at least one constraint for intent=modify")
	}
}

func TestGetContext_SecurityConstraints_AgentRulesExcluded(t *testing.T) {
	g := graph.New("test-repo")
	st := openMCPTestStore(t)
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Agent rule should NOT appear in security_constraints (already in session_init)
	// Structural rule SHOULD appear.
	cfg.Rules = []config.Rule{
		{ID: "agent-rule", Description: "Behavioral agent constraint", Severity: "warning", RuleType: "agent"},
		{ID: "struct-rule", Description: "Structural constraint for handlers", Severity: "error"},
	}

	fnID := g.MakeNodeID("handler.go", "Handler")
	g.AddNode(&graph.Node{
		ID: fnID, Type: graph.NodeFunction,
		Name: "Handler", File: "handler.go", Line: 1,
	})

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	req := callTool(map[string]any{"entity": "Handler", "intent": "add", "format": "json"})
	result, err := srv.handleGetContext(ctx, req)
	m := mustResult(t, result, err)

	enrichMap := m["enrichment"].(map[string]any)
	constraints := enrichMap["security_constraints"].([]any)

	for _, c := range constraints {
		if s, ok := c.(string); ok && strings.Contains(s, "Behavioral agent constraint") {
			t.Error("agent-type rules must not appear in security_constraints")
		}
	}
	found := false
	for _, c := range constraints {
		if s, ok := c.(string); ok && strings.Contains(s, "Structural constraint") {
			found = true
		}
	}
	if !found {
		t.Error("structural rules must appear in security_constraints")
	}
}

func TestGetContext_SecurityConstraints_FilePatternExclusion(t *testing.T) {
	// Rule with FromFilePattern "api/*.go" must NOT appear for entity in "service/service.go".
	g := graph.New("test-repo")
	st := openMCPTestStore(t)
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Rules = []config.Rule{
		{
			ID:          "api-only-rule",
			Description: "API handlers must use AuthMiddleware",
			Severity:    "error",
			ForbiddenEdge: config.ForbiddenEdge{
				FromFilePattern: "api/*.go",
			},
		},
	}

	fnID := g.MakeNodeID("service/service.go", "ProcessOrder")
	g.AddNode(&graph.Node{
		ID: fnID, Type: graph.NodeFunction,
		Name: "ProcessOrder", File: "service/service.go", Line: 1,
	})

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	req := callTool(map[string]any{
		"entity": "ProcessOrder",
		"intent": "add",
		"format": "json",
	})
	result, err := srv.handleGetContext(ctx, req)
	m := mustResult(t, result, err)

	if enrichRaw, ok := m["enrichment"]; ok {
		if enrichMap, ok := enrichRaw.(map[string]any); ok {
			if constraintsRaw, ok := enrichMap["security_constraints"]; ok {
				constraints := constraintsRaw.([]any)
				for _, c := range constraints {
					if s, ok := c.(string); ok && strings.Contains(s, "API handlers must use AuthMiddleware") {
						t.Errorf("rule with FromFilePattern 'api/*.go' must not appear for 'service/service.go', got: %s", s)
					}
				}
			}
		}
	}
}

func TestGetContext_SecurityConstraints_SeverityFormat(t *testing.T) {
	// Verify severity is uppercased: "error" → "[ERROR]", empty → "[WARNING]".
	g := graph.New("test-repo")
	st := openMCPTestStore(t)
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Rules = []config.Rule{
		{ID: "r-error", Description: "Error level rule", Severity: "error"},
		{ID: "r-empty", Description: "No-severity rule", Severity: ""},
	}

	fnID := g.MakeNodeID("handler.go", "Handle")
	g.AddNode(&graph.Node{
		ID: fnID, Type: graph.NodeFunction,
		Name: "Handle", File: "handler.go", Line: 1,
	})

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	req := callTool(map[string]any{"entity": "Handle", "intent": "add", "format": "json"})
	result, err := srv.handleGetContext(ctx, req)
	m := mustResult(t, result, err)

	enrichMap := m["enrichment"].(map[string]any)
	constraints := enrichMap["security_constraints"].([]any)

	var foundError, foundWarning bool
	for _, c := range constraints {
		s, ok := c.(string)
		if !ok {
			continue
		}
		if strings.Contains(s, "Error level rule") && strings.Contains(s, "[ERROR]") {
			foundError = true
		}
		if strings.Contains(s, "No-severity rule") && strings.Contains(s, "[WARNING]") {
			foundWarning = true
		}
	}
	if !foundError {
		t.Errorf("severity 'error' should render as [ERROR], constraints: %v", constraints)
	}
	if !foundWarning {
		t.Errorf("empty severity should render as [WARNING], constraints: %v", constraints)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// minimalDCWithRoot creates the smallest directionalContext that produces
// a non-empty serializeCompact output (root node is required).
func minimalDCWithRoot() *directionalContext {
	return &directionalContext{
		Root: &graph.Node{
			ID: "root", Name: "DoWork", Type: graph.NodeFunction,
			File: "svc.go", Line: 1,
		},
	}
}

// makeTestFuncNode adds a NodeFunction to g and returns its NodeID.
func makeTestFuncNode(g *graph.Graph, file, name string, line int) graph.NodeID {
	id := g.MakeNodeID(file, name)
	g.AddNode(&graph.Node{
		ID:   id,
		Type: graph.NodeFunction,
		Name: name,
		File: file,
		Line: line,
	})
	return id
}

// ── Sprint 27.1: Pattern engine violations in SecurityConstraints ─────────────

// makeChiRouteGraph sets up a minimal graph that triggers the "go-chi-missing-auth"
// pattern: a file that imports chi and registers routes (via "Get"/"Post" calls)
// but has no auth middleware call. The graph is rooted at root so file paths are
// absolute (required by the pattern engine's framework gate).
func makeChiRouteGraph(t *testing.T, root, filePath string) *graph.Graph {
	t.Helper()
	g := graph.New("test-repo")
	g.SetRoot(root)

	// NodeFile for the routes file.
	fileID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID: fileID, Type: graph.NodeFile,
		Name: filePath, File: filePath,
	})

	// chi import — triggers the framework gate.
	chiID := g.MakeNodeID("github.com/go-chi/chi/v5", "github.com/go-chi/chi/v5")
	g.AddNode(&graph.Node{
		ID: chiID, Type: graph.NodePackage,
		Name:    "github.com/go-chi/chi/v5",
		Package: "github.com/go-chi/chi/v5",
		File:    filePath,
	})
	g.AddEdge(&graph.Edge{From: fileID, To: chiID, Type: graph.EdgeImports})

	// Handler function that calls route registration methods but NO auth.
	fnID := g.MakeNodeID(filePath, "RegisterRoutes")
	g.AddNode(&graph.Node{
		ID: fnID, Type: graph.NodeFunction,
		Name: "RegisterRoutes", File: filePath, Line: 5,
	})
	getID := g.MakeNodeID("callee:Get", "Get")
	g.AddNode(&graph.Node{ID: getID, Type: graph.NodeFunction, Name: "Get", File: "chi.go"})
	g.AddEdge(&graph.Edge{From: fnID, To: getID, Type: graph.EdgeCalls})

	return g
}

func TestGetContext_PatternEngine_ViolationAppearsInSecurityConstraints(t *testing.T) {
	root := t.TempDir()
	filePath := root + "/api/routes.go"
	g := makeChiRouteGraph(t, root, filePath)

	st := openMCPTestStore(t)
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	// intent=add on a chi route file without auth → pattern engine fires.
	req := callTool(map[string]any{
		"entity": "RegisterRoutes",
		"intent": "add",
		"format": "json",
	})
	result, err := srv.handleGetContext(ctx, req)
	m := mustResult(t, result, err)

	enrichRaw, ok := m["enrichment"]
	if !ok {
		t.Fatal("expected enrichment section")
	}
	enrichMap := enrichRaw.(map[string]any)
	constraintsRaw, ok := enrichMap["security_constraints"]
	if !ok {
		t.Fatal("expected security_constraints from pattern engine")
	}
	constraints, ok := constraintsRaw.([]any)
	if !ok || len(constraints) == 0 {
		t.Fatalf("expected non-empty security_constraints, got %v", constraintsRaw)
	}

	// At least one constraint must reference chi auth.
	found := false
	for _, c := range constraints {
		s, ok := c.(string)
		if !ok {
			continue
		}
		if strings.Contains(strings.ToLower(s), "chi") ||
			strings.Contains(strings.ToLower(s), "auth") ||
			strings.Contains(strings.ToLower(s), "middleware") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no chi/auth/middleware constraint found in: %v", constraints)
	}
}

func TestGetContext_PatternEngine_ViolationAbsentForDebugIntent(t *testing.T) {
	root := t.TempDir()
	filePath := root + "/api/routes.go"
	g := makeChiRouteGraph(t, root, filePath)

	st := openMCPTestStore(t)
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	// intent=debug must NOT trigger pattern violations in SecurityConstraints.
	req := callTool(map[string]any{
		"entity": "RegisterRoutes",
		"intent": "debug",
		"format": "json",
	})
	result, err := srv.handleGetContext(ctx, req)
	m := mustResult(t, result, err)

	if enrichRaw, ok := m["enrichment"]; ok {
		if enrichMap, ok := enrichRaw.(map[string]any); ok {
			if _, ok := enrichMap["security_constraints"]; ok {
				t.Error("security_constraints must be absent for intent=debug")
			}
		}
	}
}

func TestGetContext_PatternEngine_NilEngineSafe(t *testing.T) {
	// Verify no panic when patternEngine is nil (explicitly cleared after construction).
	g := graph.New("test-repo")
	fnID := g.MakeNodeID("api/routes.go", "Handler")
	g.AddNode(&graph.Node{
		ID: fnID, Type: graph.NodeFunction,
		Name: "Handler", File: "api/routes.go", Line: 1,
	})

	st := openMCPTestStore(t)
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	srv := New(g, cfg, st)
	srv.patternEngine = nil // force nil to test guard
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	req := callTool(map[string]any{
		"entity": "Handler",
		"intent": "add",
		"format": "json",
	})
	// Must not panic.
	result, err := srv.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("handleGetContext with nil patternEngine: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestGetContext_PatternEngine_ConstraintIncludesSeverityAndName(t *testing.T) {
	// Verify the format: "SEVERITY [PatternName]: Message Evidence"
	root := t.TempDir()
	filePath := root + "/api/routes.go"
	g := makeChiRouteGraph(t, root, filePath)

	st := openMCPTestStore(t)
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	req := callTool(map[string]any{
		"entity": "RegisterRoutes",
		"intent": "modify",
		"format": "json",
	})
	result, err := srv.handleGetContext(ctx, req)
	m := mustResult(t, result, err)

	enrichMap, ok := m["enrichment"].(map[string]any)
	if !ok {
		t.Fatalf("enrichment key missing or wrong type in response: %v", m)
	}
	constraints, ok := enrichMap["security_constraints"].([]any)
	if !ok || len(constraints) == 0 {
		t.Fatal("no pattern violations for this graph — makeChiRouteGraph always produces violations; check engine wiring")
	}

	// At least one constraint from the pattern engine should follow the format:
	// "SEVERITY [PatternName]: Message"
	found := false
	for _, c := range constraints {
		s, ok := c.(string)
		if !ok {
			continue
		}
		// Pattern engine constraints have the form "CRITICAL [chi: missing auth middleware]: ..."
		if (strings.HasPrefix(s, "CRITICAL") || strings.HasPrefix(s, "HIGH") || strings.HasPrefix(s, "MEDIUM")) &&
			strings.Contains(s, "[") && strings.Contains(s, "]:") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no pattern-engine-formatted constraint found in: %v", constraints)
	}
}

func TestGetContext_PatternEngine_CompactFormatRendersViolation(t *testing.T) {
	// Compact format (the default) must also render pattern engine violations
	// via the 🔒 section in digest.go.
	root := t.TempDir()
	filePath := root + "/api/routes.go"
	g := makeChiRouteGraph(t, root, filePath)

	st := openMCPTestStore(t)
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	req := callTool(map[string]any{
		"entity": "RegisterRoutes",
		"intent": "add",
		"format": "compact",
	})
	result, err := srv.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("handleGetContext: %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("empty result")
	}
	// Compact format returns a single TextContent item.
	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected mcp.TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(tc.Text, "🔒") {
		t.Errorf("compact output must contain 🔒 security section when pattern violations exist; got:\n%s", tc.Text)
	}
}
