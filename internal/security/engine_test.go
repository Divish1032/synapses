package security

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// buildTestGraph creates a minimal Graph for use in engine tests.
// Each addXxx helper method below populates the graph with test nodes/edges.
func buildTestGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New("test-repo")
	g.SetRoot("/project")
	return g
}

// addFileWithImports adds a NodeFile and the given import paths as NodePackage
// nodes connected via IMPORTS edges. Returns the file NodeID.
func addFileWithImports(g *graph.Graph, filePath string, imports ...string) graph.NodeID {
	fileID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileID,
		Type: graph.NodeFile,
		Name: filePath,
		File: filePath,
	})
	for _, imp := range imports {
		impID := g.MakeNodeID(imp, imp)
		g.AddNode(&graph.Node{
			ID:      impID,
			Type:    graph.NodePackage,
			Name:    imp,
			Package: imp,
			File:    filePath,
		})
		g.AddEdge(&graph.Edge{From: fileID, To: impID, Type: graph.EdgeImports})
	}
	return fileID
}

// addFunctionWithCalls adds a NodeFunction and CALLS edges to the named callees.
// Returns the function NodeID.
func addFunctionWithCalls(g *graph.Graph, filePath, fnName string, callees ...string) graph.NodeID {
	fnID := g.MakeNodeID(filePath, fnName)
	g.AddNode(&graph.Node{
		ID:   fnID,
		Type: graph.NodeFunction,
		Name: fnName,
		File: filePath,
	})
	for _, callee := range callees {
		calleeID := g.MakeNodeID("callee", callee)
		g.AddNode(&graph.Node{
			ID:   calleeID,
			Type: graph.NodeFunction,
			Name: callee,
			File: "other.go",
		})
		g.AddEdge(&graph.Edge{From: fnID, To: calleeID, Type: graph.EdgeCalls})
	}
	return fnID
}

// addRouteNode adds a synthetic NodeRoute to the graph. Returns the route NodeID.
func addRouteNode(g *graph.Graph, filePath, method, routePath string) graph.NodeID {
	routeName := method + " " + routePath
	routeID := g.MakeNodeID(filePath, "route:"+routeName)
	g.UpsertRouteNode(&graph.Node{
		ID:   routeID,
		Type: graph.NodeRoute,
		Name: routeName,
		File: filePath,
		Metadata: map[string]string{
			"method": method,
			"path":   routePath,
		},
	})
	return routeID
}

// makeSinglePattern creates a minimal Pattern for a given check type.
func makeSinglePattern(checkType CheckType, extra ...func(*SecurityPattern)) SecurityPattern {
	b := true
	p := SecurityPattern{
		ID:          "test-pattern",
		Name:        "Test Pattern",
		Language:    "go",
		Framework:   "*",
		PatternType: PatternTypeAuthMiddleware,
		Severity:    SeverityCritical,
		Description: "test",
		Message:     "Violation in {file} for {target}",
		Enabled:     &b,
		Detection:   Detection{CheckType: checkType},
	}
	for _, fn := range extra {
		fn(&p)
	}
	return p
}

// makeEngine wraps a single pattern in an Engine.
func makeEngine(p SecurityPattern) *Engine {
	return NewEngine(newPatternSet([]SecurityPattern{p}))
}

// ──────────────────────────────────────────────────────────────────────────────
// Engine nil-safety
// ──────────────────────────────────────────────────────────────────────────────

func TestEngine_NilEngine(t *testing.T) {
	var e *Engine
	g := buildTestGraph(t)
	if got := e.CheckFile(g, "/project/main.go", nil); got != nil {
		t.Errorf("nil Engine.CheckFile should return nil, got %v", got)
	}
	if got := e.CheckProject(g); got != nil {
		t.Errorf("nil Engine.CheckProject should return nil, got %v", got)
	}
	if got := e.PatternCount(); got != 0 {
		t.Errorf("nil Engine.PatternCount should return 0, got %d", got)
	}
}

func TestEngine_NilPatternSet(t *testing.T) {
	e := NewEngine(nil)
	g := buildTestGraph(t)
	if got := e.CheckFile(g, "/project/main.go", nil); got != nil {
		t.Errorf("nil PatternSet.CheckFile should return nil, got %v", got)
	}
}

func TestEngine_NilGraph(t *testing.T) {
	e := DefaultEngine()
	if got := e.CheckFile(nil, "/project/main.go", nil); got != nil {
		t.Errorf("nil graph.CheckFile should return nil, got %v", got)
	}
}

func TestEngine_EmptyFilePath(t *testing.T) {
	e := DefaultEngine()
	g := buildTestGraph(t)
	if got := e.CheckFile(g, "", nil); got != nil {
		t.Errorf("empty filePath.CheckFile should return nil, got %v", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// DefaultEngine
// ──────────────────────────────────────────────────────────────────────────────

func TestDefaultEngine_LoadsBuiltins(t *testing.T) {
	e := DefaultEngine()
	// Built-in patterns are embedded in the binary; count must be > 0.
	if e.PatternCount() == 0 {
		t.Error("DefaultEngine() should load built-in patterns, got 0")
	}
}

func TestDefaultEngine_NotNil(t *testing.T) {
	e := DefaultEngine()
	if e == nil {
		t.Fatal("DefaultEngine() returned nil")
	}
}

func TestDefaultEngineWithDir_EmptyDir(t *testing.T) {
	e := DefaultEngineWithDir("")
	if e == nil {
		t.Fatal("DefaultEngineWithDir('') returned nil")
	}
	if e.PatternCount() == 0 {
		t.Error("DefaultEngineWithDir('') should load built-in patterns")
	}
}

func TestDefaultEngineWithDir_InvalidDir(t *testing.T) {
	// When extraDir loading fails (invalid path with malformed JSON inside),
	// DefaultEngineWithDir must fall back to built-ins and never return nil.
	e := DefaultEngineWithDir("/nonexistent/path/that/does/not/exist")
	if e == nil {
		t.Fatal("DefaultEngineWithDir(invalid) returned nil")
	}
	// Should have fallen back to built-in patterns.
	if e.PatternCount() == 0 {
		t.Error("DefaultEngineWithDir(invalid) should fall back to built-in patterns")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Framework gate
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckFile_FrameworkGate_NoImport(t *testing.T) {
	// Pattern requires chi import but file doesn't import it.
	p := makeSinglePattern(CheckTypeMissingMiddleware, func(sp *SecurityPattern) {
		sp.Detection.FrameworkIdentifiers = []string{"github.com/go-chi/chi/v5"}
		sp.Detection.RouteNodeNames = []string{"Get", "Post"}
		sp.Detection.RequiredCallPatterns = []string{"Auth*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	// File has routes but no chi import.
	addFileWithImports(g, "/project/api/routes.go") // no chi import
	addRouteNode(g, "/project/api/routes.go", "GET", "/users")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if violations != nil {
		t.Errorf("expected no violations (framework gate should block), got %v", violations)
	}
}

func TestCheckFile_FrameworkGate_WithImport(t *testing.T) {
	// Pattern requires chi import and file DOES import it, but has no auth.
	p := makeSinglePattern(CheckTypeMissingMiddleware, func(sp *SecurityPattern) {
		sp.Detection.FrameworkIdentifiers = []string{"github.com/go-chi/chi/v5"}
		sp.Detection.RouteNodeNames = []string{"Get", "Post"}
		sp.Detection.RequiredCallPatterns = []string{"Auth*", "JWT*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go", "github.com/go-chi/chi/v5")
	addRouteNode(g, "/project/api/routes.go", "GET", "/users")
	// No auth call added.

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if len(violations) == 0 {
		t.Error("expected violation: chi file has routes but no auth")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// CheckTypeDirectImport
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckDirectImport_HandlerImportsForbidden(t *testing.T) {
	p := makeSinglePattern(CheckTypeDirectImport, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeLayerViolation
		sp.Severity = SeverityMedium
		sp.Detection.HandlerFilePatterns = []string{"*handler*.go", "*/handler/*.go"}
		sp.Detection.ForbiddenImportPatterns = []string{"database/sql", "gorm.io/*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/internal/handler/users.go", "database/sql")

	violations := e.CheckFile(g, "/project/internal/handler/users.go", nil)
	if len(violations) == 0 {
		t.Fatal("expected violation: handler directly imports database/sql")
	}
	if violations[0].Target != "database/sql" {
		t.Errorf("expected target 'database/sql', got %q", violations[0].Target)
	}
}

func TestCheckDirectImport_NonHandlerFile(t *testing.T) {
	p := makeSinglePattern(CheckTypeDirectImport, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeLayerViolation
		sp.Severity = SeverityMedium
		sp.Detection.HandlerFilePatterns = []string{"*handler*.go"}
		sp.Detection.ForbiddenImportPatterns = []string{"database/sql"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	// This file is a repository, not a handler.
	addFileWithImports(g, "/project/internal/repo/users.go", "database/sql")

	violations := e.CheckFile(g, "/project/internal/repo/users.go", nil)
	if violations != nil {
		t.Errorf("expected no violation: repo file is allowed to import database/sql, got %v", violations)
	}
}

func TestCheckDirectImport_AllowedImport(t *testing.T) {
	p := makeSinglePattern(CheckTypeDirectImport, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeLayerViolation
		sp.Severity = SeverityMedium
		sp.Detection.HandlerFilePatterns = []string{"*handler*.go"}
		sp.Detection.ForbiddenImportPatterns = []string{"database/sql", "gorm.io/*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	// Handler imports a safe package.
	addFileWithImports(g, "/project/internal/handler/users.go", "encoding/json")

	violations := e.CheckFile(g, "/project/internal/handler/users.go", nil)
	if violations != nil {
		t.Errorf("expected no violation: encoding/json is not forbidden, got %v", violations)
	}
}

func TestCheckDirectImport_GormGlob(t *testing.T) {
	p := makeSinglePattern(CheckTypeDirectImport, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeLayerViolation
		sp.Severity = SeverityMedium
		sp.Detection.HandlerFilePatterns = []string{"*handler*.go"}
		sp.Detection.ForbiddenImportPatterns = []string{"gorm.io/*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/internal/handler/users.go", "gorm.io/gorm")

	violations := e.CheckFile(g, "/project/internal/handler/users.go", nil)
	if len(violations) == 0 {
		t.Fatal("expected violation: gorm.io/gorm matches gorm.io/*")
	}
}

func TestCheckDirectImport_EmptyHandlerPatterns_MatchAll(t *testing.T) {
	// When HandlerFilePatterns is empty, all files are eligible.
	p := makeSinglePattern(CheckTypeDirectImport, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeLayerViolation
		sp.Severity = SeverityMedium
		// No HandlerFilePatterns — matches any file.
		sp.Detection.ForbiddenImportPatterns = []string{"unsafe"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/internal/service/users.go", "unsafe")

	violations := e.CheckFile(g, "/project/internal/service/users.go", nil)
	if len(violations) == 0 {
		t.Error("expected violation: empty handler patterns should match all files")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// CheckTypeMissingMiddleware
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckMissingMiddleware_NoRoutes(t *testing.T) {
	p := makeSinglePattern(CheckTypeMissingMiddleware, func(sp *SecurityPattern) {
		sp.Detection.RouteNodeNames = []string{"Get", "Post"}
		sp.Detection.RequiredCallPatterns = []string{"Auth*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	// File with functions but no route nodes.
	addFileWithImports(g, "/project/api/service.go")
	addFunctionWithCalls(g, "/project/api/service.go", "doWork")

	violations := e.CheckFile(g, "/project/api/service.go", nil)
	if violations != nil {
		t.Errorf("expected no violation: no routes registered, got %v", violations)
	}
}

func TestCheckMissingMiddleware_RoutesWithAuth(t *testing.T) {
	p := makeSinglePattern(CheckTypeMissingMiddleware, func(sp *SecurityPattern) {
		sp.Detection.RouteNodeNames = []string{"Get", "Post"}
		sp.Detection.RequiredCallPatterns = []string{"Auth*", "JWT*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go")
	addRouteNode(g, "/project/api/routes.go", "GET", "/users")
	addFunctionWithCalls(g, "/project/api/routes.go", "setupRoutes", "AuthMiddleware")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if violations != nil {
		t.Errorf("expected no violation: auth call exists, got %v", violations)
	}
}

func TestCheckMissingMiddleware_RoutesWithoutAuth(t *testing.T) {
	p := makeSinglePattern(CheckTypeMissingMiddleware, func(sp *SecurityPattern) {
		sp.Detection.RouteNodeNames = []string{"Get", "Post"}
		sp.Detection.RequiredCallPatterns = []string{"Auth*", "JWT*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go")
	addRouteNode(g, "/project/api/routes.go", "GET", "/users")
	addFunctionWithCalls(g, "/project/api/routes.go", "setupRoutes", "json.Marshal") // no auth call

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if len(violations) == 0 {
		t.Fatal("expected violation: routes without auth")
	}
	if violations[0].Severity != SeverityCritical {
		t.Errorf("expected CRITICAL severity, got %s", violations[0].Severity)
	}
}

func TestCheckMissingMiddleware_RouteNodeDetectionViaHeuristic(t *testing.T) {
	// Route detection via NodeRoute (heuristic pass output) — no route names needed.
	p := makeSinglePattern(CheckTypeMissingMiddleware, func(sp *SecurityPattern) {
		sp.Detection.RouteNodeNames = nil // rely on NodeRoute nodes
		sp.Detection.RequiredCallPatterns = []string{"RequireAuth"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go")
	addRouteNode(g, "/project/api/routes.go", "POST", "/login")
	// No auth call.

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if len(violations) == 0 {
		t.Fatal("expected violation: route found via NodeRoute, no auth")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// CheckTypeMissingAnnotation
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckMissingAnnotation_NoAnnotationCall_Fires(t *testing.T) {
	p := makeSinglePattern(CheckTypeMissingAnnotation, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeAuthMiddleware
		sp.Severity = SeverityHigh
		sp.Language = "go"
		sp.Detection.AnnotationPatterns = []string{"RequireAuth*", "AuthGuard"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/handler.go")
	addFunctionWithCalls(g, "/project/api/handler.go", "GetUsers", "db.Query") // no annotation call

	violations := e.CheckFile(g, "/project/api/handler.go", nil)
	if len(violations) == 0 {
		t.Fatal("expected violation: handler file with no annotation call")
	}
	if violations[0].Severity != SeverityHigh {
		t.Errorf("expected HIGH severity, got %s", violations[0].Severity)
	}
}

func TestCheckMissingAnnotation_WithAnnotationCall_NoViolation(t *testing.T) {
	p := makeSinglePattern(CheckTypeMissingAnnotation, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeAuthMiddleware
		sp.Language = "go"
		sp.Detection.AnnotationPatterns = []string{"RequireAuth*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/handler.go")
	addFunctionWithCalls(g, "/project/api/handler.go", "GetUsers", "RequireAuthMiddleware")

	violations := e.CheckFile(g, "/project/api/handler.go", nil)
	if violations != nil {
		t.Errorf("expected no violation: annotation call present, got %v", violations)
	}
}

func TestCheckMissingAnnotation_HandlerFilePattern_Scopes(t *testing.T) {
	p := makeSinglePattern(CheckTypeMissingAnnotation, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeAuthMiddleware
		sp.Detection.HandlerFilePatterns = []string{"*/views/*.go"}
		sp.Detection.AnnotationPatterns = []string{"RequireAuth"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	// File NOT in views/ — should be skipped even without auth call.
	addFileWithImports(g, "/project/internal/repo/users.go")
	addFunctionWithCalls(g, "/project/internal/repo/users.go", "getUser")

	violations := e.CheckFile(g, "/project/internal/repo/users.go", nil)
	if violations != nil {
		t.Errorf("expected no violation: file doesn't match HandlerFilePatterns, got %v", violations)
	}
}

func TestCheckMissingAnnotation_SignatureMetadata_Suppresses(t *testing.T) {
	// If a function's metadata["signature"] contains the annotation, no violation.
	p := makeSinglePattern(CheckTypeMissingAnnotation, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeAuthMiddleware
		sp.Language = "java"
		sp.Detection.AnnotationPatterns = []string{"@PreAuthorize*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/src/Controller.java")
	// Add a function node with the annotation in its signature metadata.
	fnID := g.MakeNodeID("/project/src/Controller.java", "getUsers")
	g.AddNode(&graph.Node{
		ID:   fnID,
		Type: graph.NodeFunction,
		Name: "getUsers",
		File: "/project/src/Controller.java",
		Metadata: map[string]string{
			"signature": "@PreAuthorize(\"hasRole('USER')\")",
		},
	})

	violations := e.CheckFile(g, "/project/src/Controller.java", nil)
	if violations != nil {
		t.Errorf("expected no violation: annotation in function signature metadata, got %v", violations)
	}
}

func TestCheckMissingAnnotation_EmptyAnnotationPatterns_NoViolation(t *testing.T) {
	// Empty AnnotationPatterns means the check can't fire.
	p := makeSinglePattern(CheckTypeMissingAnnotation, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeAuthMiddleware
		sp.Detection.AnnotationPatterns = nil
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/views.go")
	violations := e.CheckFile(g, "/project/api/views.go", nil)
	if violations != nil {
		t.Errorf("expected no violation: empty AnnotationPatterns, got %v", violations)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// CheckTypeAdminElevation
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckAdminElevation_NoAdminRoute(t *testing.T) {
	p := makeSinglePattern(CheckTypeAdminElevation, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeAdminElevation
		sp.Detection.AdminPathPatterns = []string{"/admin/*", "/management/*"}
		sp.Detection.ElevatedAuthPatterns = []string{"RequireAdmin*", "IsAdmin*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go")
	addRouteNode(g, "/project/api/routes.go", "GET", "/users")  // regular route, not admin

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if violations != nil {
		t.Errorf("expected no violation: no admin route, got %v", violations)
	}
}

func TestCheckAdminElevation_AdminRouteWithElevatedAuth(t *testing.T) {
	p := makeSinglePattern(CheckTypeAdminElevation, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeAdminElevation
		sp.Detection.AdminPathPatterns = []string{"/admin/*"}
		sp.Detection.ElevatedAuthPatterns = []string{"RequireAdmin*", "IsAdmin*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/admin.go")
	addRouteNode(g, "/project/api/admin.go", "GET", "/admin/users")
	addFunctionWithCalls(g, "/project/api/admin.go", "setupAdmin", "RequireAdminRole")

	violations := e.CheckFile(g, "/project/api/admin.go", nil)
	if violations != nil {
		t.Errorf("expected no violation: elevated auth present, got %v", violations)
	}
}

func TestCheckAdminElevation_AdminRouteWithoutElevatedAuth(t *testing.T) {
	p := makeSinglePattern(CheckTypeAdminElevation, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeAdminElevation
		sp.Detection.AdminPathPatterns = []string{"/admin/*"}
		sp.Detection.ElevatedAuthPatterns = []string{"RequireAdmin*", "IsAdmin*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/admin.go")
	addRouteNode(g, "/project/api/admin.go", "DELETE", "/admin/users")
	addFunctionWithCalls(g, "/project/api/admin.go", "setupAdmin", "AuthMiddleware") // only basic auth

	violations := e.CheckFile(g, "/project/api/admin.go", nil)
	if len(violations) == 0 {
		t.Fatal("expected violation: admin route without elevated auth")
	}
	if violations[0].Target != "/admin/users" {
		t.Errorf("expected target '/admin/users', got %q", violations[0].Target)
	}
}

func TestCheckAdminElevation_AdminComponentInPath(t *testing.T) {
	// Route path contains "/admin" as a component even without explicit pattern match.
	p := makeSinglePattern(CheckTypeAdminElevation, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeAdminElevation
		sp.Detection.AdminPathPatterns = []string{"/admin/*"}
		sp.Detection.ElevatedAuthPatterns = []string{"RequireAdmin*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/panel.go")
	// Path "/v2/admin/settings" has "admin" as a component.
	addRouteNode(g, "/project/api/panel.go", "PUT", "/v2/admin/settings")

	violations := e.CheckFile(g, "/project/api/panel.go", nil)
	if len(violations) == 0 {
		t.Fatal("expected violation: /v2/admin/settings has admin component without elevated auth")
	}
}

func TestCheckAdminElevation_NoRoutes(t *testing.T) {
	p := makeSinglePattern(CheckTypeAdminElevation, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeAdminElevation
		sp.Detection.AdminPathPatterns = []string{"/admin/*"}
		sp.Detection.ElevatedAuthPatterns = []string{"RequireAdmin*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/util.go")

	violations := e.CheckFile(g, "/project/api/util.go", nil)
	if violations != nil {
		t.Errorf("expected no violation: no routes, got %v", violations)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// CheckTypeHardcodedSecret
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckHardcodedSecret_Base64CredentialValue(t *testing.T) {
	p := makeSinglePattern(CheckTypeHardcodedSecret, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeHardcodedSecret
		sp.Detection.SecretPatterns = []string{`(?i)^[a-zA-Z0-9+/]{32,}={0,2}$`}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/auth/jwt.go")

	content := []byte(`package auth

var jwtSecret = "supersecretvalue12345678901234567890ab"
`)
	violations := e.CheckFile(g, "/project/auth/jwt.go", content)
	if len(violations) == 0 {
		t.Fatal("expected violation: hardcoded base64-like secret")
	}
	if violations[0].Target != "jwtSecret" {
		t.Errorf("expected target 'jwtSecret', got %q", violations[0].Target)
	}
}

func TestCheckHardcodedSecret_OpenAIKey(t *testing.T) {
	p := makeSinglePattern(CheckTypeHardcodedSecret, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeHardcodedSecret
		sp.Detection.SecretPatterns = []string{`^sk-[a-zA-Z0-9]{32,}$`}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/client/openai.go")

	content := []byte(`package client

const apiKey = "sk-abcdefghijklmnopqrstuvwxyz12345678"
`)
	violations := e.CheckFile(g, "/project/client/openai.go", content)
	if len(violations) == 0 {
		t.Fatal("expected violation: hardcoded OpenAI-style key")
	}
}

func TestCheckHardcodedSecret_NonCredentialVariable(t *testing.T) {
	p := makeSinglePattern(CheckTypeHardcodedSecret, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeHardcodedSecret
		sp.Detection.SecretPatterns = []string{`(?i)^[a-zA-Z0-9+/]{32,}={0,2}$`}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/config/base.go")

	// Variable name "greeting" does not match credentialVarRE — no violation.
	content := []byte(`package config

var greeting = "HelloWorldThisIsALongStringThatLooksLikeBase64But"
`)
	violations := e.CheckFile(g, "/project/config/base.go", content)
	if violations != nil {
		t.Errorf("expected no violation: 'greeting' is not a credential variable, got %v", violations)
	}
}

func TestCheckHardcodedSecret_NilContent(t *testing.T) {
	p := makeSinglePattern(CheckTypeHardcodedSecret, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeHardcodedSecret
		sp.Detection.SecretPatterns = []string{`(?i)^[a-zA-Z0-9+/]{32,}={0,2}$`}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/auth/jwt.go")

	// nil content → secret check is skipped.
	violations := e.CheckFile(g, "/project/auth/jwt.go", nil)
	if violations != nil {
		t.Errorf("expected no violations when content is nil, got %v", violations)
	}
}

func TestCheckHardcodedSecret_TestFileDowngradesTo_MEDIUM(t *testing.T) {
	p := makeSinglePattern(CheckTypeHardcodedSecret, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeHardcodedSecret
		sp.Severity = SeverityCritical
		sp.Detection.SecretPatterns = []string{`(?i)^[a-zA-Z0-9+/]{32,}={0,2}$`}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/auth/jwt_test.go")

	content := []byte(`package auth_test

var jwtSecret = "supersecretvalue12345678901234567890ab"
`)
	violations := e.CheckFile(g, "/project/auth/jwt_test.go", content)
	if len(violations) == 0 {
		t.Fatal("expected violation in test file")
	}
	if violations[0].Severity != SeverityMedium {
		t.Errorf("expected MEDIUM severity for test file, got %s", violations[0].Severity)
	}
}

func TestCheckHardcodedSecret_SafeEnvLoad(t *testing.T) {
	p := makeSinglePattern(CheckTypeHardcodedSecret, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeHardcodedSecret
		sp.Detection.SecretPatterns = []string{`(?i)^[a-zA-Z0-9+/]{32,}={0,2}$`}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/auth/jwt.go")

	// Loading from env — not a hardcoded value.
	content := []byte(`package auth

var jwtSecret = os.Getenv("JWT_SECRET")
`)
	violations := e.CheckFile(g, "/project/auth/jwt.go", content)
	if violations != nil {
		t.Errorf("expected no violation: value loaded from env, got %v", violations)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Language filtering
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckFile_WrongLanguage(t *testing.T) {
	// Pattern is Go-only; TypeScript file should produce no violations.
	p := makeSinglePattern(CheckTypeDirectImport, func(sp *SecurityPattern) {
		sp.Language = "go"
		sp.PatternType = PatternTypeLayerViolation
		sp.Severity = SeverityMedium
		sp.Detection.HandlerFilePatterns = []string{"*.ts"}
		sp.Detection.ForbiddenImportPatterns = []string{"database/sql"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/routes.ts", "database/sql")

	violations := e.CheckFile(g, "/project/api/routes.ts", nil)
	if violations != nil {
		t.Errorf("expected no violation: Go pattern should not fire on .ts file, got %v", violations)
	}
}

func TestCheckFile_WildcardLanguage(t *testing.T) {
	// Wildcard pattern fires on any language.
	p := makeSinglePattern(CheckTypeAdminElevation, func(sp *SecurityPattern) {
		sp.Language = "*"
		sp.PatternType = PatternTypeAdminElevation
		sp.Detection.AdminPathPatterns = []string{"/admin/*"}
		sp.Detection.ElevatedAuthPatterns = []string{"RequireAdmin*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/admin.ts")
	addRouteNode(g, "/project/api/admin.ts", "DELETE", "/admin/users")

	violations := e.CheckFile(g, "/project/api/admin.ts", nil)
	if len(violations) == 0 {
		t.Fatal("expected violation: wildcard pattern fires on .ts file")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Disabled pattern
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckFile_DisabledPattern(t *testing.T) {
	f := false
	p := makeSinglePattern(CheckTypeDirectImport, func(sp *SecurityPattern) {
		sp.Enabled = &f // explicitly disabled
		sp.PatternType = PatternTypeLayerViolation
		sp.Severity = SeverityMedium
		sp.Detection.ForbiddenImportPatterns = []string{"database/sql"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/handler.go", "database/sql")

	violations := e.CheckFile(g, "/project/api/handler.go", nil)
	if violations != nil {
		t.Errorf("expected no violations from disabled pattern, got %v", violations)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// fileMatchesAny
// ──────────────────────────────────────────────────────────────────────────────

func TestFileMatchesAny(t *testing.T) {
	cases := []struct {
		name     string
		filePath string
		patterns []string
		want     bool
	}{
		{
			name:     "basename match wildcard",
			filePath: "/project/internal/handler/users.go",
			patterns: []string{"*handler*.go"},
			want:     true,
		},
		{
			name:     "directory suffix match",
			filePath: "/project/internal/handler/users.go",
			patterns: []string{"*/handler/*.go"},
			want:     true,
		},
		{
			name:     "handlers plural directory",
			filePath: "/project/internal/handlers/users.go",
			patterns: []string{"*/handlers/*.go"},
			want:     true,
		},
		{
			name:     "no match",
			filePath: "/project/internal/repo/users.go",
			patterns: []string{"*handler*.go", "*/handler/*.go"},
			want:     false,
		},
		{
			name:     "controller match",
			filePath: "/project/web/controller/users_controller.go",
			patterns: []string{"*controller*.go"},
			want:     true,
		},
		{
			name:     "api directory",
			filePath: "/project/internal/api/users.go",
			patterns: []string{"*/api/*.go"},
			want:     true,
		},
		{
			name:     "empty patterns",
			filePath: "/project/handler.go",
			patterns: nil,
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fileMatchesAny(tc.filePath, tc.patterns)
			if got != tc.want {
				t.Errorf("fileMatchesAny(%q, %v) = %v, want %v", tc.filePath, tc.patterns, got, tc.want)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// matchGlob
// ──────────────────────────────────────────────────────────────────────────────

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"Auth*", "AuthMiddleware", true},
		{"Auth*", "Authenticate", true},
		{"Auth*", "authentication", false}, // case-sensitive
		{"JWT*", "JWTAuthentication", true},
		{"authenticate", "authenticate", true},
		{"authenticate", "Authenticate", false},
		{"gorm.io/*", "gorm.io/gorm", true},
		{"gorm.io/*", "gorm.io/driver/postgres", true},
		{"gorm.io/*", "github.com/other/pkg", false},
		{"database/sql", "database/sql", true},
		{"database/sql", "database/sql/driver", false},
		{"*", "anything", true},
		{"*", "", true},
		{"", "anything", false},
		{"RequireAdmin*", "RequireAdminRole", true},
		{"RequireAdmin*", "RequireUser", false},
	}

	for _, tc := range cases {
		got := matchGlob(tc.pattern, tc.name)
		if got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// languageFromPath
// ──────────────────────────────────────────────────────────────────────────────

func TestLanguageFromPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"main.go", "go"},
		{"handler.GO", "go"}, // case-insensitive extension
		{"index.ts", "typescript"},
		{"App.tsx", "typescript"},
		{"index.js", "javascript"},
		{"main.jsx", "javascript"},
		{"server.mjs", "javascript"},
		{"app.py", "python"},
		{"Service.java", "java"},
		{"main.rs", "rust"},
		{"controller.rb", "ruby"},
		{"Service.cs", "csharp"},
		{"index.php", "php"},
		{"Makefile", ""},
		{"config.yaml", "yaml"},
	}

	for _, tc := range cases {
		got := languageFromPath(tc.path)
		if got != tc.want {
			t.Errorf("languageFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// fillTemplate
// ──────────────────────────────────────────────────────────────────────────────

func TestFillTemplate(t *testing.T) {
	cases := []struct {
		name   string
		tmpl   string
		vars   map[string]string
		want   string
	}{
		{
			name: "all placeholders",
			tmpl: "Route {target} in {file}: {count}/{total} have auth",
			vars: map[string]string{"target": "/admin/users", "file": "admin.go", "count": "8", "total": "9"},
			want: "Route /admin/users in admin.go: 8/9 have auth",
		},
		{
			name: "unknown placeholder preserved",
			tmpl: "Violation in {file} for {unknown}",
			vars: map[string]string{"file": "x.go"},
			want: "Violation in x.go for {unknown}",
		},
		{
			name: "no placeholders",
			tmpl: "no placeholders here",
			vars: map[string]string{"file": "x.go"},
			want: "no placeholders here",
		},
		{
			name: "empty template",
			tmpl: "",
			vars: map[string]string{"file": "x.go"},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fillTemplate(tc.tmpl, tc.vars)
			if got != tc.want {
				t.Errorf("fillTemplate(%q) = %q, want %q", tc.tmpl, got, tc.want)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// isTestFile
// ──────────────────────────────────────────────────────────────────────────────

func TestIsTestFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/project/auth/jwt_test.go", true},
		{"/project/testdata/fixtures.go", true},
		{"/project/test/helpers.go", true},
		{"/project/tests/util.go", true},
		{"/project/mocks/auth.go", true},
		{"/project/auth/jwt.go", false},
		{"/project/internal/service.go", false},
	}
	for _, tc := range cases {
		got := isTestFile(tc.path)
		if got != tc.want {
			t.Errorf("isTestFile(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Integration: builtin patterns fire correctly
// ──────────────────────────────────────────────────────────────────────────────

func TestBuiltinPattern_DirectImport_Handler(t *testing.T) {
	e := DefaultEngine()
	g := buildTestGraph(t)

	// Handler file importing database/sql — builtin go-generic-direct-db-import should fire.
	addFileWithImports(g, "/project/internal/handlers/users.go", "database/sql")

	violations := e.CheckFile(g, "/project/internal/handlers/users.go", nil)
	// Check that at least one violation fires for the layer violation.
	found := false
	for _, v := range violations {
		if v.PatternID == "go-generic-direct-db-import" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("builtin go-generic-direct-db-import should fire for handler importing database/sql; violations: %v", violations)
	}
}

func TestBuiltinPattern_AdminElevation(t *testing.T) {
	e := DefaultEngine()
	g := buildTestGraph(t)

	// File with admin route and only basic auth — generic-admin-elevation should fire.
	// The generic pattern matches "*" language, so we use a .go file.
	addFileWithImports(g, "/project/api/admin.go")
	addRouteNode(g, "/project/api/admin.go", "POST", "/admin/users")
	addFunctionWithCalls(g, "/project/api/admin.go", "setupAdmin", "BasicAuth")

	violations := e.CheckFile(g, "/project/api/admin.go", nil)
	found := false
	for _, v := range violations {
		if v.PatternID == "generic-admin-elevation" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("builtin generic-admin-elevation should fire; violations: %v", violations)
	}
}

func TestBuiltinPattern_HardcodedSecret(t *testing.T) {
	e := DefaultEngine()
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/auth/config.go")

	// Value matches the base64-like pattern from go-generic-hardcoded-secret.
	content := []byte(`package auth

var jwtSecret = "supersecretvalue12345678901234567890ab"
`)
	violations := e.CheckFile(g, "/project/auth/config.go", content)
	found := false
	for _, v := range violations {
		if v.PatternID == "go-generic-hardcoded-secret" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("builtin go-generic-hardcoded-secret should fire; violations: %v", violations)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// CheckProject: cross-transport
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckProject_NoRoutes(t *testing.T) {
	p := makeSinglePattern(CheckTypeCrossTransportAuth, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeAuthMiddleware
		sp.Detection.RequiredCallPatterns = []string{"Auth*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	violations := e.CheckProject(g)
	if violations != nil {
		t.Errorf("expected no violations: no routes in project, got %v", violations)
	}
}

func TestCheckProject_SingleTransport_NoViolation(t *testing.T) {
	p := makeSinglePattern(CheckTypeCrossTransportAuth, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeAuthMiddleware
		sp.Detection.RequiredCallPatterns = []string{"Auth*"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go")
	addRouteNode(g, "/project/api/routes.go", "GET", "/users")
	addRouteNode(g, "/project/api/routes.go", "POST", "/users")

	// Only HTTP routes — no cross-transport inconsistency.
	violations := e.CheckProject(g)
	if violations != nil {
		t.Errorf("expected no violations with single transport type, got %v", violations)
	}
}

func TestCheckProject_CrossTransport_Fires(t *testing.T) {
	p := makeSinglePattern(CheckTypeCrossTransportAuth, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeAuthMiddleware
		sp.Detection.RequiredCallPatterns = []string{"AuthMiddleware"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	// HTTP routes with auth — protected.
	addFileWithImports(g, "/project/api/http_routes.go")
	addRouteNode(g, "/project/api/http_routes.go", "GET", "/users")
	addRouteNode(g, "/project/api/http_routes.go", "POST", "/users")
	addRouteNode(g, "/project/api/http_routes.go", "DELETE", "/users")
	addFunctionWithCalls(g, "/project/api/http_routes.go", "setupRoutes", "AuthMiddleware")

	// WebSocket handler WITHOUT auth — inconsistent.
	addFileWithImports(g, "/project/ws/handler.go")
	addRouteNode(g, "/project/ws/handler.go", "WS", "/ws/events")
	addFunctionWithCalls(g, "/project/ws/handler.go", "handleWS", "json.Unmarshal") // no auth

	violations := e.CheckProject(g)
	if len(violations) == 0 {
		t.Fatal("expected cross-transport violation: HTTP routes have auth but WebSocket handler does not")
	}
	if violations[0].Severity != SeverityCritical {
		t.Errorf("expected CRITICAL severity, got %s", violations[0].Severity)
	}
}

func TestCheckProject_CrossTransport_BothProtected_NoViolation(t *testing.T) {
	p := makeSinglePattern(CheckTypeCrossTransportAuth, func(sp *SecurityPattern) {
		sp.PatternType = PatternTypeAuthMiddleware
		sp.Detection.RequiredCallPatterns = []string{"AuthMiddleware"}
	})
	e := makeEngine(p)
	g := buildTestGraph(t)

	// Both HTTP and WebSocket have auth.
	addFileWithImports(g, "/project/api/http_routes.go")
	addRouteNode(g, "/project/api/http_routes.go", "GET", "/users")
	addFunctionWithCalls(g, "/project/api/http_routes.go", "setupRoutes", "AuthMiddleware")

	addFileWithImports(g, "/project/ws/handler.go")
	addRouteNode(g, "/project/ws/handler.go", "WS", "/ws/events")
	addFunctionWithCalls(g, "/project/ws/handler.go", "handleWS", "AuthMiddleware")

	violations := e.CheckProject(g)
	if violations != nil {
		t.Errorf("expected no violations: both transports have auth, got %v", violations)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// nilIfEmpty helper
// ──────────────────────────────────────────────────────────────────────────────

func TestNilIfEmpty(t *testing.T) {
	if nilIfEmpty(nil) != nil {
		t.Error("nilIfEmpty(nil) should return nil")
	}
	if nilIfEmpty([]Violation{}) != nil {
		t.Error("nilIfEmpty(empty) should return nil")
	}
	v := []Violation{{PatternID: "x"}}
	if nilIfEmpty(v) == nil {
		t.Error("nilIfEmpty(non-empty) should return non-nil")
	}
}
