package security

// engine_java_frameworks_test.go: integration tests for Java framework security
// patterns (Sprint 26.5). Tests verify that Spring Boot and Jakarta EE patterns
// load correctly and behave correctly: framework gate blocks non-matching files,
// auth patterns fire on missing annotations, and the direct-repository check
// fires when a Spring controller directly imports a repository class.

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ──────────────────────────────────────────────────────────────────────────────
// Loader integration — verify java-frameworks.json loads via LoadBuiltin
// ──────────────────────────────────────────────────────────────────────────────

func TestJavaFrameworks_LoadBuiltin_ContainsAllPatterns(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin() error: %v", err)
	}

	wantIDs := []string{
		"java-spring-missing-auth",
		"java-jakartaee-missing-auth",
		"java-spring-direct-repository",
	}

	for _, id := range wantIDs {
		p, ok := ps.ByID(id)
		if !ok {
			t.Errorf("LoadBuiltin(): pattern %q not found", id)
			continue
		}
		if err := p.Validate(); err != nil {
			t.Errorf("LoadBuiltin(): pattern %q fails Validate(): %v", id, err)
		}
	}
}

func TestJavaFrameworks_DefaultEngine_PatternCount(t *testing.T) {
	e := DefaultEngine()
	// 55 pre-existing patterns (from sprints 26.1-26.4) + 3 new Java framework patterns = 58 total.
	if e.PatternCount() < 58 {
		t.Errorf("DefaultEngine PatternCount = %d, want >= 58", e.PatternCount())
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Spring Boot — missing auth annotation
// ──────────────────────────────────────────────────────────────────────────────

func TestJavaFrameworks_Spring_MissingAuth_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Spring controller: imports Spring Web, is a controller file, no auth annotation.
	addFileWithImports(g, "/project/src/UserController.java", "org.springframework.web.bind.annotation")
	addFunctionWithCalls(g, "/project/src/UserController.java", "getUser", "GetMapping", "ResponseEntity")

	violations := e.CheckFile(g, "/project/src/UserController.java", nil)
	found := findViolation(violations, "java-spring-missing-auth")
	if found == nil {
		t.Fatal("expected java-spring-missing-auth violation, got none")
	}
	if found.Severity != SeverityCritical {
		t.Errorf("severity = %s, want CRITICAL", found.Severity)
	}
}

func TestJavaFrameworks_Spring_MissingAuth_WithPreAuthorize_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/src/UserController.java", "org.springframework.web.bind.annotation")
	addFunctionWithCalls(g, "/project/src/UserController.java", "getUser", "GetMapping", "PreAuthorize")

	violations := e.CheckFile(g, "/project/src/UserController.java", nil)
	if findViolation(violations, "java-spring-missing-auth") != nil {
		t.Error("expected no violation when PreAuthorize is called")
	}
}

func TestJavaFrameworks_Spring_MissingAuth_WithSecured_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/src/OrderController.java", "org.springframework.web.bind.annotation")
	addFunctionWithCalls(g, "/project/src/OrderController.java", "createOrder", "PostMapping", "Secured")

	violations := e.CheckFile(g, "/project/src/OrderController.java", nil)
	if findViolation(violations, "java-spring-missing-auth") != nil {
		t.Error("expected no violation when Secured is called")
	}
}

func TestJavaFrameworks_Spring_MissingAuth_WithRolesAllowed_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/src/AdminController.java", "org.springframework.web.bind.annotation")
	addFunctionWithCalls(g, "/project/src/AdminController.java", "listUsers", "GetMapping", "RolesAllowed")

	violations := e.CheckFile(g, "/project/src/AdminController.java", nil)
	if findViolation(violations, "java-spring-missing-auth") != nil {
		t.Error("expected no violation when RolesAllowed is called")
	}
}

func TestJavaFrameworks_Spring_MissingAuth_WithPermitAll_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Intentionally public endpoint declared with @PermitAll.
	addFileWithImports(g, "/project/src/HealthController.java", "org.springframework.web.bind.annotation")
	addFunctionWithCalls(g, "/project/src/HealthController.java", "health", "GetMapping", "PermitAll")

	violations := e.CheckFile(g, "/project/src/HealthController.java", nil)
	if findViolation(violations, "java-spring-missing-auth") != nil {
		t.Error("expected no violation for @PermitAll (explicitly public endpoint)")
	}
}

func TestJavaFrameworks_Spring_MissingAuth_SignatureMetadata_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Controller file with @PreAuthorize in function signature metadata.
	addFileWithImports(g, "/project/src/PaymentController.java", "org.springframework.web.bind.annotation")
	fnID := g.MakeNodeID("/project/src/PaymentController.java", "processPayment")
	g.AddNode(&graph.Node{
		ID:   fnID,
		Type: graph.NodeFunction,
		Name: "processPayment",
		File: "/project/src/PaymentController.java",
		Metadata: map[string]string{
			"signature": "@PreAuthorize(\"hasRole('PAYMENT_ADMIN')\") public ResponseEntity<Void> processPayment(PaymentRequest request)",
		},
	})

	violations := e.CheckFile(g, "/project/src/PaymentController.java", nil)
	if findViolation(violations, "java-spring-missing-auth") != nil {
		t.Error("expected no violation: @PreAuthorize present in function signature metadata")
	}
}

func TestJavaFrameworks_Spring_MissingAuth_FrameworkGate_NonSpring_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Does NOT import Spring Web — Jakarta EE file.
	addFileWithImports(g, "/project/src/UserResource.java", "jakarta.ws.rs")
	addFunctionWithCalls(g, "/project/src/UserResource.java", "getUser", "GET", "Path")

	violations := e.CheckFile(g, "/project/src/UserResource.java", nil)
	if findViolation(violations, "java-spring-missing-auth") != nil {
		t.Error("framework gate failed: java-spring-missing-auth fired on non-Spring file")
	}
}

func TestJavaFrameworks_Spring_MissingAuth_HandlerFilePattern_NonController_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Spring file but NOT a controller — e.g. a service.
	addFileWithImports(g, "/project/src/UserService.java", "org.springframework.web.bind.annotation")
	addFunctionWithCalls(g, "/project/src/UserService.java", "findUser", "GetMapping")

	violations := e.CheckFile(g, "/project/src/UserService.java", nil)
	if findViolation(violations, "java-spring-missing-auth") != nil {
		t.Error("handler file pattern gate failed: java-spring-missing-auth fired on non-controller file")
	}
}

func TestJavaFrameworks_Spring_MissingAuth_WithRestControllerImport_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Uses the class-level import instead of package-level.
	addFileWithImports(g, "/project/src/api/ProductController.java", "org.springframework.web.bind.annotation.RestController")
	addFunctionWithCalls(g, "/project/src/api/ProductController.java", "listProducts", "GetMapping")

	violations := e.CheckFile(g, "/project/src/api/ProductController.java", nil)
	if findViolation(violations, "java-spring-missing-auth") == nil {
		t.Error("expected java-spring-missing-auth violation for RestController import with no auth")
	}
}

func TestJavaFrameworks_Spring_MissingAuth_ResourceNamedSpring_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Some Spring projects name controllers *Resource.java — should still fire.
	addFileWithImports(g, "/project/src/UserResource.java", "org.springframework.web.bind.annotation.RestController")
	addFunctionWithCalls(g, "/project/src/UserResource.java", "getUser", "GetMapping")

	violations := e.CheckFile(g, "/project/src/UserResource.java", nil)
	if findViolation(violations, "java-spring-missing-auth") == nil {
		t.Error("expected java-spring-missing-auth violation for Spring *Resource.java file with no auth")
	}
}

func TestJavaFrameworks_Spring_MissingAuth_ApiPackage_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// A utility/error-handler file in an api/ package — NOT a controller name.
	// */api/*.java was removed from handler_file_patterns to prevent CRITICAL false positives.
	addFileWithImports(g, "/project/src/api/ApiErrorHandler.java", "org.springframework.web.bind.annotation")
	addFunctionWithCalls(g, "/project/src/api/ApiErrorHandler.java", "handleError", "ExceptionHandler")

	violations := e.CheckFile(g, "/project/src/api/ApiErrorHandler.java", nil)
	if findViolation(violations, "java-spring-missing-auth") != nil {
		t.Error("false positive: java-spring-missing-auth should not fire on api/ package file that is not a controller")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Jakarta EE — missing authorization annotation
// ──────────────────────────────────────────────────────────────────────────────

func TestJavaFrameworks_JakartaEE_MissingAuth_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// JAX-RS resource: imports jakarta.ws.rs, is a resource file, no auth annotation.
	addFileWithImports(g, "/project/src/UserResource.java", "jakarta.ws.rs")
	addFunctionWithCalls(g, "/project/src/UserResource.java", "getUser", "GET", "Path", "Produces")

	violations := e.CheckFile(g, "/project/src/UserResource.java", nil)
	found := findViolation(violations, "java-jakartaee-missing-auth")
	if found == nil {
		t.Fatal("expected java-jakartaee-missing-auth violation, got none")
	}
	if found.Severity != SeverityCritical {
		t.Errorf("severity = %s, want CRITICAL", found.Severity)
	}
}

func TestJavaFrameworks_JakartaEE_MissingAuth_WithRolesAllowed_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/src/OrderResource.java", "jakarta.ws.rs")
	addFunctionWithCalls(g, "/project/src/OrderResource.java", "createOrder", "POST", "RolesAllowed")

	violations := e.CheckFile(g, "/project/src/OrderResource.java", nil)
	if findViolation(violations, "java-jakartaee-missing-auth") != nil {
		t.Error("expected no violation when RolesAllowed is declared")
	}
}

func TestJavaFrameworks_JakartaEE_MissingAuth_WithPermitAll_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Explicitly public endpoint via @PermitAll.
	addFileWithImports(g, "/project/src/HealthResource.java", "jakarta.ws.rs")
	addFunctionWithCalls(g, "/project/src/HealthResource.java", "ping", "GET", "PermitAll")

	violations := e.CheckFile(g, "/project/src/HealthResource.java", nil)
	if findViolation(violations, "java-jakartaee-missing-auth") != nil {
		t.Error("expected no violation for @PermitAll (explicitly public resource)")
	}
}

func TestJavaFrameworks_JakartaEE_MissingAuth_LegacyNamespace_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Older javax.ws.rs namespace (Jakarta EE 8 / Java EE).
	addFileWithImports(g, "/project/src/LegacyResource.java", "javax.ws.rs")
	addFunctionWithCalls(g, "/project/src/LegacyResource.java", "getData", "GET", "Produces")

	violations := e.CheckFile(g, "/project/src/LegacyResource.java", nil)
	if findViolation(violations, "java-jakartaee-missing-auth") == nil {
		t.Error("expected java-jakartaee-missing-auth violation for javax.ws.rs file with no auth")
	}
}

func TestJavaFrameworks_JakartaEE_MissingAuth_FrameworkGate_NonJakarta_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Spring file — should NOT trigger Jakarta EE pattern.
	addFileWithImports(g, "/project/src/UserController.java", "org.springframework.web.bind.annotation")
	addFunctionWithCalls(g, "/project/src/UserController.java", "getUser", "GetMapping")

	violations := e.CheckFile(g, "/project/src/UserController.java", nil)
	if findViolation(violations, "java-jakartaee-missing-auth") != nil {
		t.Error("framework gate failed: java-jakartaee-missing-auth fired on Spring file")
	}
}

func TestJavaFrameworks_JakartaEE_MissingAuth_SignatureMetadata_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/src/InvoiceResource.java", "jakarta.ws.rs")
	fnID := g.MakeNodeID("/project/src/InvoiceResource.java", "getInvoice")
	g.AddNode(&graph.Node{
		ID:   fnID,
		Type: graph.NodeFunction,
		Name: "getInvoice",
		File: "/project/src/InvoiceResource.java",
		Metadata: map[string]string{
			"signature": "@RolesAllowed({\"admin\", \"billing\"}) public Response getInvoice(@PathParam(\"id\") Long id)",
		},
	})

	violations := e.CheckFile(g, "/project/src/InvoiceResource.java", nil)
	if findViolation(violations, "java-jakartaee-missing-auth") != nil {
		t.Error("expected no violation: @RolesAllowed present in function signature metadata")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Spring Boot — direct repository access from controller
// ──────────────────────────────────────────────────────────────────────────────

func TestJavaFrameworks_Spring_DirectRepository_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Spring controller that directly imports a JPA repository.
	addFileWithImports(g,
		"/project/src/UserController.java",
		"org.springframework.web.bind.annotation",
		"com.example.repository.UserRepository",
	)
	addFunctionWithCalls(g, "/project/src/UserController.java", "getUser", "GetMapping")

	violations := e.CheckFile(g, "/project/src/UserController.java", nil)
	found := findViolation(violations, "java-spring-direct-repository")
	if found == nil {
		t.Fatal("expected java-spring-direct-repository violation, got none")
	}
	if found.Severity != SeverityMedium {
		t.Errorf("severity = %s, want MEDIUM", found.Severity)
	}
}

func TestJavaFrameworks_Spring_DirectRepository_SpringDataImport_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Controller importing Spring Data package directly.
	addFileWithImports(g,
		"/project/src/ProductController.java",
		"org.springframework.web.bind.annotation",
		"org.springframework.data.jpa.repository",
	)
	addFunctionWithCalls(g, "/project/src/ProductController.java", "listProducts", "GetMapping")

	violations := e.CheckFile(g, "/project/src/ProductController.java", nil)
	if findViolation(violations, "java-spring-direct-repository") == nil {
		t.Error("expected java-spring-direct-repository violation for Spring Data import")
	}
}

func TestJavaFrameworks_Spring_DirectRepository_ServiceOnly_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Controller importing only a service — correct layering.
	addFileWithImports(g,
		"/project/src/UserController.java",
		"org.springframework.web.bind.annotation",
		"com.example.service.UserService",
	)
	addFunctionWithCalls(g, "/project/src/UserController.java", "getUser", "GetMapping")

	violations := e.CheckFile(g, "/project/src/UserController.java", nil)
	if findViolation(violations, "java-spring-direct-repository") != nil {
		t.Error("expected no violation when controller imports only a service")
	}
}

func TestJavaFrameworks_Spring_DirectRepository_FrameworkGate_NonSpring_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Non-Spring file that imports a repository — should not fire Spring pattern.
	addFileWithImports(g,
		"/project/src/UserResource.java",
		"jakarta.ws.rs",
		"com.example.repository.UserRepository",
	)
	addFunctionWithCalls(g, "/project/src/UserResource.java", "getUser", "GET")

	violations := e.CheckFile(g, "/project/src/UserResource.java", nil)
	if findViolation(violations, "java-spring-direct-repository") != nil {
		t.Error("framework gate failed: java-spring-direct-repository fired on non-Spring file")
	}
}

func TestJavaFrameworks_Spring_DirectRepository_HandlerFilePattern_Service_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Service file (not a controller) importing a repository — valid pattern in service layer.
	addFileWithImports(g,
		"/project/src/UserService.java",
		"org.springframework.web.bind.annotation",
		"com.example.repository.UserRepository",
	)
	addFunctionWithCalls(g, "/project/src/UserService.java", "findUser")

	violations := e.CheckFile(g, "/project/src/UserService.java", nil)
	if findViolation(violations, "java-spring-direct-repository") != nil {
		t.Error("handler file pattern gate failed: direct-repository fired on service file")
	}
}

func TestJavaFrameworks_Spring_DirectRepository_JPAPersistence_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Controller using JPA EntityManager directly — bypasses repo layer.
	addFileWithImports(g,
		"/project/src/UserController.java",
		"org.springframework.web.bind.annotation",
		"javax.persistence.EntityManager",
	)
	addFunctionWithCalls(g, "/project/src/UserController.java", "getUser", "GetMapping")

	violations := e.CheckFile(g, "/project/src/UserController.java", nil)
	if findViolation(violations, "java-spring-direct-repository") == nil {
		t.Error("expected java-spring-direct-repository violation for JPA EntityManager import in controller")
	}
}

func TestJavaFrameworks_Spring_DirectRepository_DAOPattern_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Controller importing a DAO class (legacy pattern).
	addFileWithImports(g,
		"/project/src/rest/ItemController.java",
		"org.springframework.web.bind.annotation.RestController",
		"com.example.dao.ItemDao",
	)
	addFunctionWithCalls(g, "/project/src/rest/ItemController.java", "getItem", "GetMapping")

	violations := e.CheckFile(g, "/project/src/rest/ItemController.java", nil)
	if findViolation(violations, "java-spring-direct-repository") == nil {
		t.Error("expected java-spring-direct-repository violation for DAO import in controller")
	}
}
