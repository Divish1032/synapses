package security

// engine_go_frameworks_test.go: integration tests for Go HTTP framework security patterns
// (Sprint 26.2). Tests verify that chi, gin, echo, and net/http patterns load correctly
// and fire with the right behavior: framework gate blocks non-matching files, auth patterns
// fire on missing auth, rate-limit patterns fire on missing rate limiting.

import (
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────────
// Loader integration — verify go-frameworks.json loads via LoadBuiltin
// ──────────────────────────────────────────────────────────────────────────────

func TestGoFrameworks_LoadBuiltin_ContainsFrameworkPatterns(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin() error: %v", err)
	}

	wantIDs := []string{
		"go-chi-missing-auth",
		"go-chi-missing-rate-limit",
		"go-gin-missing-auth",
		"go-gin-missing-rate-limit",
		"go-echo-missing-auth",
		"go-echo-missing-rate-limit",
		"go-nethttp-missing-auth",
		"go-nethttp-missing-rate-limit",
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

func TestGoFrameworks_DefaultEngine_PatternCount(t *testing.T) {
	e := DefaultEngine()
	// 2 go-generic (hardcoded-secret, direct-db-import)
	// + 1 generic (admin-elevation)
	// + 6 cross-transport (go-cross-transport-* from Sprint 26.8)
	// + 8 go-framework patterns (chi auth/rate, gin auth/rate, echo auth/rate, nethttp auth/rate)
	// = 17 total minimum.
	if e.PatternCount() < 17 {
		t.Errorf("DefaultEngine PatternCount = %d, want >= 17", e.PatternCount())
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Chi — missing auth
// ──────────────────────────────────────────────────────────────────────────────

func TestGoFrameworks_Chi_MissingAuth_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// chi file: imports chi, registers routes via Get, no auth call.
	addFileWithImports(g, "/project/api/routes.go", "github.com/go-chi/chi/v5")
	addFunctionWithCalls(g, "/project/api/routes.go", "RegisterRoutes", "Get", "Post")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	found := findViolation(violations, "go-chi-missing-auth")
	if found == nil {
		t.Fatal("expected go-chi-missing-auth violation, got none")
	}
	if found.Severity != SeverityCritical {
		t.Errorf("severity = %s, want CRITICAL", found.Severity)
	}
}

func TestGoFrameworks_Chi_MissingAuth_WithAuth_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go", "github.com/go-chi/chi/v5")
	addFunctionWithCalls(g, "/project/api/routes.go", "RegisterRoutes", "Get", "Post", "AuthMiddleware")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if findViolation(violations, "go-chi-missing-auth") != nil {
		t.Error("expected no go-chi-missing-auth violation when AuthMiddleware is called")
	}
}

func TestGoFrameworks_Chi_MissingAuth_WithJWT_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go", "github.com/go-chi/chi/v5")
	// JWT* glob matches "JWTAuth".
	addFunctionWithCalls(g, "/project/api/routes.go", "setup", "Get", "JWTAuth")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if findViolation(violations, "go-chi-missing-auth") != nil {
		t.Error("expected no violation: JWTAuth matches JWT* pattern")
	}
}

func TestGoFrameworks_Chi_FrameworkGate_NonChiFile_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Same file structure but imports gin instead of chi.
	addFileWithImports(g, "/project/api/routes.go", "github.com/gin-gonic/gin")
	addFunctionWithCalls(g, "/project/api/routes.go", "RegisterRoutes", "Get", "Post")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if findViolation(violations, "go-chi-missing-auth") != nil {
		t.Error("chi pattern fired on non-chi file (framework gate failure)")
	}
}

func TestGoFrameworks_Chi_RouteDetectionViaNodeRoute(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Route detected via NodeRoute (heuristic pass), no explicit route method call needed.
	addFileWithImports(g, "/project/api/users.go", "github.com/go-chi/chi/v5")
	addRouteNode(g, "/project/api/users.go", "GET", "/users")
	// No auth call.

	violations := e.CheckFile(g, "/project/api/users.go", nil)
	if findViolation(violations, "go-chi-missing-auth") == nil {
		t.Fatal("expected violation: NodeRoute present, no auth")
	}
}

func TestGoFrameworks_Chi_V4Import_FrameworkGate(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// chi v4 (without /v5) should also be covered.
	addFileWithImports(g, "/project/api/routes.go", "github.com/go-chi/chi")
	addFunctionWithCalls(g, "/project/api/routes.go", "setup", "Get", "Post")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if findViolation(violations, "go-chi-missing-auth") == nil {
		t.Fatal("expected violation: github.com/go-chi/chi (v4) should match framework gate")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Chi — missing rate limit
// ──────────────────────────────────────────────────────────────────────────────

func TestGoFrameworks_Chi_MissingRateLimit_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go", "github.com/go-chi/chi/v5")
	addFunctionWithCalls(g, "/project/api/routes.go", "RegisterRoutes", "Get", "Post", "AuthMiddleware")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	found := findViolation(violations, "go-chi-missing-rate-limit")
	if found == nil {
		t.Fatal("expected go-chi-missing-rate-limit violation, got none")
	}
	if found.Severity != SeverityHigh {
		t.Errorf("severity = %s, want HIGH", found.Severity)
	}
}

func TestGoFrameworks_Chi_MissingRateLimit_WithRateLimit_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go", "github.com/go-chi/chi/v5")
	addFunctionWithCalls(g, "/project/api/routes.go", "setup", "Get", "RateLimiter")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if findViolation(violations, "go-chi-missing-rate-limit") != nil {
		t.Error("expected no rate-limit violation when RateLimiter is called")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Gin — missing auth
// ──────────────────────────────────────────────────────────────────────────────

func TestGoFrameworks_Gin_MissingAuth_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Gin uses uppercase route methods: GET, POST, etc.
	addFileWithImports(g, "/project/api/routes.go", "github.com/gin-gonic/gin")
	addFunctionWithCalls(g, "/project/api/routes.go", "SetupRouter", "GET", "POST")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	found := findViolation(violations, "go-gin-missing-auth")
	if found == nil {
		t.Fatal("expected go-gin-missing-auth violation, got none")
	}
	if found.Severity != SeverityCritical {
		t.Errorf("severity = %s, want CRITICAL", found.Severity)
	}
}

func TestGoFrameworks_Gin_MissingAuth_WithAuth_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go", "github.com/gin-gonic/gin")
	addFunctionWithCalls(g, "/project/api/routes.go", "SetupRouter", "GET", "POST", "RequireAuth")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if findViolation(violations, "go-gin-missing-auth") != nil {
		t.Error("expected no violation: RequireAuth matches RequireAuth* pattern")
	}
}

func TestGoFrameworks_Gin_FrameworkGate_NonGinFile_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Same file calls GET/POST but uses echo, not gin.
	addFileWithImports(g, "/project/api/routes.go", "github.com/labstack/echo/v4")
	addFunctionWithCalls(g, "/project/api/routes.go", "setup", "GET", "POST")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if findViolation(violations, "go-gin-missing-auth") != nil {
		t.Error("gin pattern fired on non-gin file (framework gate failure)")
	}
}

func TestGoFrameworks_Gin_MissingRateLimit_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go", "github.com/gin-gonic/gin")
	addFunctionWithCalls(g, "/project/api/routes.go", "SetupRouter", "GET", "POST")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	found := findViolation(violations, "go-gin-missing-rate-limit")
	if found == nil {
		t.Fatal("expected go-gin-missing-rate-limit violation, got none")
	}
	if found.Severity != SeverityHigh {
		t.Errorf("severity = %s, want HIGH", found.Severity)
	}
}

func TestGoFrameworks_Gin_MissingRateLimit_WithThrottle_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go", "github.com/gin-gonic/gin")
	// Throttle* matches "ThrottleMiddleware".
	addFunctionWithCalls(g, "/project/api/routes.go", "setup", "GET", "ThrottleMiddleware")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if findViolation(violations, "go-gin-missing-rate-limit") != nil {
		t.Error("expected no rate-limit violation: ThrottleMiddleware matches Throttle* pattern")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Echo — missing auth
// ──────────────────────────────────────────────────────────────────────────────

func TestGoFrameworks_Echo_MissingAuth_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go", "github.com/labstack/echo/v4")
	addFunctionWithCalls(g, "/project/api/routes.go", "RegisterRoutes", "GET", "POST")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	found := findViolation(violations, "go-echo-missing-auth")
	if found == nil {
		t.Fatal("expected go-echo-missing-auth violation, got none")
	}
	if found.Severity != SeverityCritical {
		t.Errorf("severity = %s, want CRITICAL", found.Severity)
	}
}

func TestGoFrameworks_Echo_MissingAuth_WithAuth_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go", "github.com/labstack/echo/v4")
	// Verifier is an echo-specific JWT middleware function.
	addFunctionWithCalls(g, "/project/api/routes.go", "setup", "GET", "Verifier")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if findViolation(violations, "go-echo-missing-auth") != nil {
		t.Error("expected no violation: Verifier is in required_call_patterns")
	}
}

func TestGoFrameworks_Echo_V3Import_FrameworkGate(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// echo v3 (without /v4) should also be covered.
	addFileWithImports(g, "/project/api/routes.go", "github.com/labstack/echo")
	addFunctionWithCalls(g, "/project/api/routes.go", "setup", "GET", "POST")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if findViolation(violations, "go-echo-missing-auth") == nil {
		t.Fatal("expected violation: github.com/labstack/echo (v3) should match framework gate")
	}
}

func TestGoFrameworks_Echo_FrameworkGate_NonEchoFile_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// No echo import — framework gate must block.
	addFileWithImports(g, "/project/api/routes.go", "net/http")
	addFunctionWithCalls(g, "/project/api/routes.go", "setup", "GET", "POST")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if findViolation(violations, "go-echo-missing-auth") != nil {
		t.Error("echo pattern fired on non-echo file (framework gate failure)")
	}
}

func TestGoFrameworks_Echo_MissingRateLimit_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go", "github.com/labstack/echo/v4")
	addFunctionWithCalls(g, "/project/api/routes.go", "RegisterRoutes", "GET", "POST")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	found := findViolation(violations, "go-echo-missing-rate-limit")
	if found == nil {
		t.Fatal("expected go-echo-missing-rate-limit violation, got none")
	}
	if found.Severity != SeverityHigh {
		t.Errorf("severity = %s, want HIGH", found.Severity)
	}
}

func TestGoFrameworks_Echo_MissingRateLimit_WithLimiter_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go", "github.com/labstack/echo/v4")
	// RateLimit* matches "RateLimiter".
	addFunctionWithCalls(g, "/project/api/routes.go", "setup", "GET", "RateLimiter")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if findViolation(violations, "go-echo-missing-rate-limit") != nil {
		t.Error("expected no rate-limit violation: RateLimiter matches RateLimit* pattern")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// net/http — missing auth
// ──────────────────────────────────────────────────────────────────────────────

func TestGoFrameworks_NetHTTP_MissingAuth_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/cmd/server/main.go", "net/http")
	addFunctionWithCalls(g, "/project/cmd/server/main.go", "main", "HandleFunc", "ListenAndServe")

	violations := e.CheckFile(g, "/project/cmd/server/main.go", nil)
	found := findViolation(violations, "go-nethttp-missing-auth")
	if found == nil {
		t.Fatal("expected go-nethttp-missing-auth violation, got none")
	}
	if found.Severity != SeverityCritical {
		t.Errorf("severity = %s, want CRITICAL", found.Severity)
	}
}

func TestGoFrameworks_NetHTTP_MissingAuth_WithAuthWrapper_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/cmd/server/main.go", "net/http")
	// AuthMiddleware wraps the handler — it's called in the same file.
	addFunctionWithCalls(g, "/project/cmd/server/main.go", "main", "HandleFunc", "AuthMiddleware")

	violations := e.CheckFile(g, "/project/cmd/server/main.go", nil)
	if findViolation(violations, "go-nethttp-missing-auth") != nil {
		t.Error("expected no violation: AuthMiddleware matches Auth* pattern")
	}
}

func TestGoFrameworks_NetHTTP_MissingAuth_WithIsAuthenticated_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/cmd/server/main.go", "net/http")
	// isAuthenticated is net/http-specific addition for inline auth checks.
	addFunctionWithCalls(g, "/project/cmd/server/main.go", "handleUsers", "HandleFunc", "isAuthenticated")

	violations := e.CheckFile(g, "/project/cmd/server/main.go", nil)
	if findViolation(violations, "go-nethttp-missing-auth") != nil {
		t.Error("expected no violation: isAuthenticated is in required_call_patterns")
	}
}

func TestGoFrameworks_NetHTTP_NoRouteRegistration_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Imports net/http but only uses http.Request / http.ResponseWriter — no route registration.
	addFileWithImports(g, "/project/internal/middleware/auth.go", "net/http")
	addFunctionWithCalls(g, "/project/internal/middleware/auth.go", "AuthMiddleware", "json.NewEncoder")

	violations := e.CheckFile(g, "/project/internal/middleware/auth.go", nil)
	if findViolation(violations, "go-nethttp-missing-auth") != nil {
		t.Error("expected no violation: file doesn't call HandleFunc or Handle")
	}
}

func TestGoFrameworks_NetHTTP_HandleOnlyNoHandleFunc_NoViolation(t *testing.T) {
	// net/http pattern only triggers on HandleFunc, not Handle.
	// Reason: chi/gin/echo projects universally import net/http and expose their own
	// Handle(pattern, handler) router methods. Using Handle alone as a discriminator
	// causes false positives on chi files that call r.Handle(). HandleFunc is the
	// clean discriminator for raw net/http route registration.
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/server.go", "net/http")
	// Calls http.Handle but NOT http.HandleFunc — not in route_node_names.
	addFunctionWithCalls(g, "/project/server.go", "setupServer", "Handle", "ListenAndServe")

	violations := e.CheckFile(g, "/project/server.go", nil)
	if findViolation(violations, "go-nethttp-missing-auth") != nil {
		t.Error("net/http pattern should not fire on Handle alone — HandleFunc is the discriminator to avoid false positives on chi files")
	}
}

func TestGoFrameworks_NetHTTP_HandleFuncUsedWithoutAuth_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/server.go", "net/http")
	addFunctionWithCalls(g, "/project/server.go", "setupServer", "HandleFunc", "ListenAndServe")

	violations := e.CheckFile(g, "/project/server.go", nil)
	if findViolation(violations, "go-nethttp-missing-auth") == nil {
		t.Fatal("expected violation: HandleFunc is the primary net/http route registration discriminator")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// net/http — missing rate limit
// ──────────────────────────────────────────────────────────────────────────────

func TestGoFrameworks_NetHTTP_MissingRateLimit_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/cmd/server/main.go", "net/http")
	addFunctionWithCalls(g, "/project/cmd/server/main.go", "main", "HandleFunc", "ListenAndServe")

	violations := e.CheckFile(g, "/project/cmd/server/main.go", nil)
	found := findViolation(violations, "go-nethttp-missing-rate-limit")
	if found == nil {
		t.Fatal("expected go-nethttp-missing-rate-limit violation, got none")
	}
	if found.Severity != SeverityHigh {
		t.Errorf("severity = %s, want HIGH", found.Severity)
	}
}

func TestGoFrameworks_NetHTTP_MissingRateLimit_WithRateLimiter_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/cmd/server/main.go", "net/http")
	// RateLimit* matches "RateLimitMiddleware".
	addFunctionWithCalls(g, "/project/cmd/server/main.go", "main", "HandleFunc", "RateLimitMiddleware")

	violations := e.CheckFile(g, "/project/cmd/server/main.go", nil)
	if findViolation(violations, "go-nethttp-missing-rate-limit") != nil {
		t.Error("expected no rate-limit violation: RateLimitMiddleware matches RateLimit* pattern")
	}
}

func TestGoFrameworks_NetHTTP_RateLimit_HandleOnlyNoHandleFunc_NoViolation(t *testing.T) {
	// Same as auth pattern: Handle alone should not trigger net/http rate-limit pattern.
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/server.go", "net/http")
	addFunctionWithCalls(g, "/project/server.go", "setup", "Handle", "ListenAndServe")

	violations := e.CheckFile(g, "/project/server.go", nil)
	if findViolation(violations, "go-nethttp-missing-rate-limit") != nil {
		t.Error("net/http rate-limit pattern should not fire on Handle alone")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Cross-framework isolation — patterns don't bleed across frameworks
// ──────────────────────────────────────────────────────────────────────────────

func TestGoFrameworks_CrossFramework_Isolation(t *testing.T) {
	// A single file with gin import should only trigger gin patterns, not chi/echo.
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go", "github.com/gin-gonic/gin")
	addFunctionWithCalls(g, "/project/api/routes.go", "SetupRouter", "GET", "POST")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)

	// chi and echo patterns must NOT fire.
	if findViolation(violations, "go-chi-missing-auth") != nil {
		t.Error("chi pattern fired on gin file")
	}
	if findViolation(violations, "go-echo-missing-auth") != nil {
		t.Error("echo pattern fired on gin file")
	}
	// gin pattern MUST fire.
	if findViolation(violations, "go-gin-missing-auth") == nil {
		t.Error("gin pattern did not fire on gin file")
	}
}

func TestGoFrameworks_MultiFramework_BothFire_WhenBothImported(t *testing.T) {
	// Edge case: file imports both chi and gin (unusual but possible in adapter code).
	// Both framework patterns should fire since both framework gates pass.
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/adapter.go",
		"github.com/go-chi/chi/v5",
		"github.com/gin-gonic/gin",
	)
	// Calls route methods from both frameworks, no auth.
	addFunctionWithCalls(g, "/project/api/adapter.go", "setup", "Get", "GET", "Post", "POST")

	violations := e.CheckFile(g, "/project/api/adapter.go", nil)

	if findViolation(violations, "go-chi-missing-auth") == nil {
		t.Error("chi pattern should fire when chi is imported")
	}
	if findViolation(violations, "go-gin-missing-auth") == nil {
		t.Error("gin pattern should fire when gin is imported")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Pattern metadata validation
// ──────────────────────────────────────────────────────────────────────────────

func TestGoFrameworks_PatternMetadata(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}

	authPatterns := []string{
		"go-chi-missing-auth",
		"go-gin-missing-auth",
		"go-echo-missing-auth",
		"go-nethttp-missing-auth",
	}
	for _, id := range authPatterns {
		p, ok := ps.ByID(id)
		if !ok {
			t.Errorf("pattern %q not found", id)
			continue
		}
		if p.PatternType != PatternTypeAuthMiddleware {
			t.Errorf("%s: PatternType = %q, want auth_middleware", id, p.PatternType)
		}
		if p.Severity != SeverityCritical {
			t.Errorf("%s: Severity = %q, want CRITICAL", id, p.Severity)
		}
		if p.Detection.CheckType != CheckTypeMissingMiddleware {
			t.Errorf("%s: CheckType = %q, want missing_middleware", id, p.Detection.CheckType)
		}
		if len(p.Detection.FrameworkIdentifiers) == 0 {
			t.Errorf("%s: no FrameworkIdentifiers (zero-false-positive gate missing)", id)
		}
	}

	rateLimitPatterns := []string{
		"go-chi-missing-rate-limit",
		"go-gin-missing-rate-limit",
		"go-echo-missing-rate-limit",
		"go-nethttp-missing-rate-limit",
	}
	for _, id := range rateLimitPatterns {
		p, ok := ps.ByID(id)
		if !ok {
			t.Errorf("pattern %q not found", id)
			continue
		}
		if p.PatternType != PatternTypeRateLimiting {
			t.Errorf("%s: PatternType = %q, want rate_limiting", id, p.PatternType)
		}
		if p.Severity != SeverityHigh {
			t.Errorf("%s: Severity = %q, want HIGH", id, p.Severity)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Helper
// ──────────────────────────────────────────────────────────────────────────────

// findViolation returns the first violation with the given PatternID, or nil.
func findViolation(violations []Violation, patternID string) *Violation {
	for i := range violations {
		if violations[i].PatternID == patternID {
			return &violations[i]
		}
	}
	return nil
}
