package security

// engine_admin_elevation_test.go: tests for CheckTypeAdminElevation (Sprint 26.10).
//
// Covers all three detection strategies:
//   1. Route path patterns (AdminPathPatterns) — admin routes without elevated auth
//   2. Handler name patterns (AdminHandlerNamePatterns) — admin-named functions
//   3. Admin package paths (AdminPackagePaths) — files in admin directories
//
// Each strategy is tested independently and in combination with the compliance
// path (ElevatedAuthPatterns called → no violation).

import (
	"strings"
	"testing"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// adminElevationPattern returns a minimal admin_elevation SecurityPattern
// with the given detection fields.  The caller can further customise via
// the extra variadic funcs (same pattern as makeSinglePattern).
func adminElevationPattern(extra ...func(*Detection)) SecurityPattern {
	b := true
	p := SecurityPattern{
		ID:          "test-admin-elevation",
		Name:        "Test Admin Elevation",
		Language:    "*",
		Framework:   "*",
		PatternType: PatternTypeAdminElevation,
		Severity:    SeverityCritical,
		Description: "test admin elevation",
		Message:     "Admin target '{target}' in {file} lacks elevated auth",
		Enabled:     &b,
		Detection: Detection{
			CheckType:            CheckTypeAdminElevation,
			AdminPathPatterns:    []string{"/admin/*", "*/admin/*"},
			ElevatedAuthPatterns: []string{"RequireAdmin*", "RequireRole*", "IsAdmin*"},
		},
	}
	for _, fn := range extra {
		fn(&p.Detection)
	}
	return p
}

// findAdminViolation returns the first admin-elevation violation or nil.
func findAdminViolation(violations []Violation) *Violation {
	return findViolation(violations, "test-admin-elevation")
}

// ── Strategy 1: Route path patterns ──────────────────────────────────────────

func TestAdminElevation_RoutePathPattern_Fires(t *testing.T) {
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/routes.go", "net/http")
	addRouteNode(g, "/project/api/routes.go", "GET", "/admin/users")
	// No elevated auth call.

	e := makeEngine(adminElevationPattern())
	vs := e.CheckFile(g, "/project/api/routes.go", nil)
	v := findAdminViolation(vs)
	if v == nil {
		t.Fatal("expected admin-elevation violation for /admin/users route, got none")
	}
	if v.Target != "/admin/users" {
		t.Errorf("Target = %q, want /admin/users", v.Target)
	}
	if v.Severity != SeverityCritical {
		t.Errorf("Severity = %s, want CRITICAL", v.Severity)
	}
	if !strings.Contains(v.Evidence, "Admin route") {
		t.Errorf("Evidence should mention 'Admin route', got: %s", v.Evidence)
	}
}

func TestAdminElevation_RoutePathPattern_Compliant(t *testing.T) {
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/routes.go", "net/http")
	addRouteNode(g, "/project/api/routes.go", "GET", "/admin/users")
	addFunctionWithCalls(g, "/project/api/routes.go", "setupRoutes", "RequireAdmin")

	e := makeEngine(adminElevationPattern())
	vs := e.CheckFile(g, "/project/api/routes.go", nil)
	if findAdminViolation(vs) != nil {
		t.Error("no violation expected when RequireAdmin is called, but got one")
	}
}

func TestAdminElevation_RoutePathPattern_NonAdminRoute_NoFire(t *testing.T) {
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/routes.go", "net/http")
	addRouteNode(g, "/project/api/routes.go", "GET", "/api/users")
	// No elevated auth call — but route is not admin.

	e := makeEngine(adminElevationPattern())
	vs := e.CheckFile(g, "/project/api/routes.go", nil)
	if findAdminViolation(vs) != nil {
		t.Error("non-admin route should not trigger admin-elevation violation")
	}
}

func TestAdminElevation_RoutePathPattern_MatchAdminComponent(t *testing.T) {
	// matchAdminComponent catches "/api/admin/settings" even without an exact pattern.
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/routes.go", "net/http")
	addRouteNode(g, "/project/api/routes.go", "POST", "/api/admin/settings")

	e := makeEngine(adminElevationPattern())
	vs := e.CheckFile(g, "/project/api/routes.go", nil)
	if findAdminViolation(vs) == nil {
		t.Error("expected violation for /api/admin/settings via matchAdminComponent")
	}
}

func TestAdminElevation_RoutePathPattern_MultipleAdminRoutes_MultipleViolations(t *testing.T) {
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/routes.go", "net/http")
	addRouteNode(g, "/project/api/routes.go", "GET", "/admin/users")
	addRouteNode(g, "/project/api/routes.go", "DELETE", "/admin/users")

	e := makeEngine(adminElevationPattern())
	vs := e.CheckFile(g, "/project/api/routes.go", nil)
	count := 0
	for _, v := range vs {
		if v.PatternID == "test-admin-elevation" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected 2 admin-elevation violations (one per route), got %d", count)
	}
}

// ── Strategy 2: Handler name patterns ────────────────────────────────────────

func TestAdminElevation_HandlerNamePattern_Fires_LowerCase(t *testing.T) {
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/users.go", "net/http")
	addFunctionWithCalls(g, "/project/api/users.go", "adminUsers") // no elevated auth

	e := makeEngine(adminElevationPattern(func(d *Detection) {
		d.AdminHandlerNamePatterns = []string{"*admin*"}
	}))
	vs := e.CheckFile(g, "/project/api/users.go", nil)
	v := findAdminViolation(vs)
	if v == nil {
		t.Fatal("expected violation for function 'adminUsers', got none")
	}
	if v.Target != "adminUsers" {
		t.Errorf("Target = %q, want adminUsers", v.Target)
	}
	if !strings.Contains(v.Evidence, "admin handler") {
		t.Errorf("Evidence should mention 'admin handler', got: %s", v.Evidence)
	}
}

func TestAdminElevation_HandlerNamePattern_Fires_UpperCase(t *testing.T) {
	// Case-insensitive: AdminUsers should match "*admin*".
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/users.go", "net/http")
	addFunctionWithCalls(g, "/project/api/users.go", "AdminUsers")

	e := makeEngine(adminElevationPattern(func(d *Detection) {
		d.AdminHandlerNamePatterns = []string{"*admin*"}
	}))
	vs := e.CheckFile(g, "/project/api/users.go", nil)
	if findAdminViolation(vs) == nil {
		t.Fatal("expected violation for function 'AdminUsers' (case-insensitive match), got none")
	}
}

func TestAdminElevation_HandlerNamePattern_Fires_HandleAdmin(t *testing.T) {
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/panel.go", "net/http")
	addFunctionWithCalls(g, "/project/api/panel.go", "handleAdminPanel")

	e := makeEngine(adminElevationPattern(func(d *Detection) {
		d.AdminHandlerNamePatterns = []string{"*admin*"}
	}))
	vs := e.CheckFile(g, "/project/api/panel.go", nil)
	if findAdminViolation(vs) == nil {
		t.Fatal("expected violation for function 'handleAdminPanel', got none")
	}
}

func TestAdminElevation_HandlerNamePattern_Compliant_RequireRole(t *testing.T) {
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/users.go", "net/http")
	addFunctionWithCalls(g, "/project/api/users.go", "adminUsers", "RequireRole")

	e := makeEngine(adminElevationPattern(func(d *Detection) {
		d.AdminHandlerNamePatterns = []string{"*admin*"}
	}))
	vs := e.CheckFile(g, "/project/api/users.go", nil)
	if findAdminViolation(vs) != nil {
		t.Error("no violation expected when RequireRole is called, got one")
	}
}

func TestAdminElevation_HandlerNamePattern_NonAdminFunction_NoFire(t *testing.T) {
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/users.go", "net/http")
	addFunctionWithCalls(g, "/project/api/users.go", "getUsers")

	e := makeEngine(adminElevationPattern(func(d *Detection) {
		d.AdminHandlerNamePatterns = []string{"*admin*"}
	}))
	vs := e.CheckFile(g, "/project/api/users.go", nil)
	if findAdminViolation(vs) != nil {
		t.Error("non-admin function name should not trigger violation")
	}
}

func TestAdminElevation_HandlerNamePattern_NoRoutes_StillFires(t *testing.T) {
	// This verifies the key new behavior: function-name detection fires even
	// when there are no NodeRoute nodes in the file.
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/users.go", "net/http")
	// No addRouteNode call — no routes in this file.
	addFunctionWithCalls(g, "/project/api/users.go", "AdminDeleteUser")

	e := makeEngine(adminElevationPattern(func(d *Detection) {
		d.AdminHandlerNamePatterns = []string{"*admin*"}
	}))
	vs := e.CheckFile(g, "/project/api/users.go", nil)
	if findAdminViolation(vs) == nil {
		t.Fatal("expected violation for admin-named function even without route nodes")
	}
}

func TestAdminElevation_HandlerNamePattern_DeduplicatedPerFunction(t *testing.T) {
	// Ensure only one violation per function name, not one per pattern.
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/users.go", "net/http")
	addFunctionWithCalls(g, "/project/api/users.go", "adminUsers")

	e := makeEngine(adminElevationPattern(func(d *Detection) {
		// Two patterns that would both match "adminUsers".
		d.AdminHandlerNamePatterns = []string{"*admin*", "admin*"}
	}))
	vs := e.CheckFile(g, "/project/api/users.go", nil)
	count := 0
	for _, v := range vs {
		if v.PatternID == "test-admin-elevation" && v.Target == "adminUsers" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 violation for adminUsers (dedup), got %d", count)
	}
}

// ── Strategy 3: Admin package paths ──────────────────────────────────────────

func TestAdminElevation_AdminPackagePath_Fires(t *testing.T) {
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/internal/admin/users.go", "net/http")
	addFunctionWithCalls(g, "/project/internal/admin/users.go", "GetUsers")
	// Non-admin function name, non-route file, but in admin/ package.

	e := makeEngine(adminElevationPattern(func(d *Detection) {
		d.AdminPackagePaths = []string{"*/admin/*"}
	}))
	vs := e.CheckFile(g, "/project/internal/admin/users.go", nil)
	v := findAdminViolation(vs)
	if v == nil {
		t.Fatal("expected violation for file in admin/ package, got none")
	}
	if !strings.Contains(v.Evidence, "admin package") {
		t.Errorf("Evidence should mention 'admin package', got: %s", v.Evidence)
	}
}

func TestAdminElevation_AdminPackagePath_Compliant_IsAdminCalled(t *testing.T) {
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/internal/admin/users.go", "net/http")
	addFunctionWithCalls(g, "/project/internal/admin/users.go", "GetUsers", "IsAdmin")

	e := makeEngine(adminElevationPattern(func(d *Detection) {
		d.AdminPackagePaths = []string{"*/admin/*"}
	}))
	vs := e.CheckFile(g, "/project/internal/admin/users.go", nil)
	if findAdminViolation(vs) != nil {
		t.Error("no violation expected when IsAdmin is called, got one")
	}
}

func TestAdminElevation_AdminPackagePath_NonAdminDirectory_NoFire(t *testing.T) {
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/internal/users/users.go", "net/http")
	addFunctionWithCalls(g, "/project/internal/users/users.go", "GetUsers")

	e := makeEngine(adminElevationPattern(func(d *Detection) {
		d.AdminPackagePaths = []string{"*/admin/*"}
	}))
	vs := e.CheckFile(g, "/project/internal/users/users.go", nil)
	if findAdminViolation(vs) != nil {
		t.Error("non-admin directory file should not trigger violation")
	}
}

func TestAdminElevation_AdminPackagePath_NoFunctions_NoFire(t *testing.T) {
	// A file in admin/ with no functions (e.g. a constants file) should not fire.
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/internal/admin/constants.go", "net/http")
	// No functions added — only the file node and its imports.

	e := makeEngine(adminElevationPattern(func(d *Detection) {
		d.AdminPackagePaths = []string{"*/admin/*"}
	}))
	vs := e.CheckFile(g, "/project/internal/admin/constants.go", nil)
	if findAdminViolation(vs) != nil {
		t.Error("file in admin/ with no functions should not trigger violation")
	}
}

func TestAdminElevation_AdminPackagePath_SkippedWhenStrategy1Fires(t *testing.T) {
	// Strategy 3 must not add a second violation when Strategy 1 already fires.
	// Both should result in exactly one violation (not two).
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/internal/admin/routes.go", "net/http")
	addRouteNode(g, "/project/internal/admin/routes.go", "GET", "/admin/users")
	addFunctionWithCalls(g, "/project/internal/admin/routes.go", "setup")

	e := makeEngine(adminElevationPattern(func(d *Detection) {
		d.AdminPackagePaths = []string{"*/admin/*"}
	}))
	vs := e.CheckFile(g, "/project/internal/admin/routes.go", nil)
	count := 0
	for _, v := range vs {
		if v.PatternID == "test-admin-elevation" {
			count++
		}
	}
	// Strategy 1 fires for the route; Strategy 3 must be suppressed.
	if count != 1 {
		t.Errorf("expected exactly 1 violation (strategy 3 suppressed by strategy 1), got %d", count)
	}
}

func TestAdminElevation_AdminPackagePath_SkippedWhenStrategy2Fires(t *testing.T) {
	// Strategy 3 must not add a second violation when Strategy 2 already fires.
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/internal/admin/handlers.go", "net/http")
	addFunctionWithCalls(g, "/project/internal/admin/handlers.go", "adminDeleteUser")
	// adminDeleteUser matches both AdminHandlerNamePatterns (strategy 2) and
	// the file is in admin/ (strategy 3). Only strategy 2 should fire.

	e := makeEngine(adminElevationPattern(func(d *Detection) {
		d.AdminHandlerNamePatterns = []string{"*admin*"}
		d.AdminPackagePaths = []string{"*/admin/*"}
	}))
	vs := e.CheckFile(g, "/project/internal/admin/handlers.go", nil)
	count := 0
	for _, v := range vs {
		if v.PatternID == "test-admin-elevation" {
			count++
		}
	}
	// Strategy 2 fires for the function; Strategy 3 must be suppressed.
	if count != 1 {
		t.Errorf("expected exactly 1 violation (strategy 3 suppressed by strategy 2), got %d", count)
	}
}

// ── Combined strategies ───────────────────────────────────────────────────────

func TestAdminElevation_AllStrategies_IndependentTargets(t *testing.T) {
	// A file that triggers both Strategy 1 (admin route) and Strategy 2
	// (admin-named function) should produce violations for both distinct targets.
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/admin_routes.go", "net/http")
	addRouteNode(g, "/project/api/admin_routes.go", "GET", "/admin/users")
	addFunctionWithCalls(g, "/project/api/admin_routes.go", "adminDashboard")

	e := makeEngine(adminElevationPattern(func(d *Detection) {
		d.AdminHandlerNamePatterns = []string{"*admin*"}
	}))
	vs := e.CheckFile(g, "/project/api/admin_routes.go", nil)
	count := 0
	for _, v := range vs {
		if v.PatternID == "test-admin-elevation" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected 2 violations (one route + one function), got %d", count)
	}
}

func TestAdminElevation_ElevatedAuth_BlocksAllStrategies(t *testing.T) {
	// When elevated auth is called, all three strategies must be suppressed.
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/internal/admin/users.go", "net/http")
	addRouteNode(g, "/project/internal/admin/users.go", "GET", "/admin/users")
	addFunctionWithCalls(g, "/project/internal/admin/users.go", "adminGetUsers", "RequireAdmin")

	e := makeEngine(adminElevationPattern(func(d *Detection) {
		d.AdminHandlerNamePatterns = []string{"*admin*"}
		d.AdminPackagePaths = []string{"*/admin/*"}
	}))
	vs := e.CheckFile(g, "/project/internal/admin/users.go", nil)
	if findAdminViolation(vs) != nil {
		t.Error("no violation expected when RequireAdmin is called (elevated auth blocks all strategies)")
	}
}

// ── Loader integration: generic.json admin-elevation pattern ─────────────────

func TestAdminElevation_LoadBuiltin_GenericPatternPresent(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin() error: %v", err)
	}
	p, ok := ps.ByID("generic-admin-elevation")
	if !ok {
		t.Fatal("generic-admin-elevation pattern not found in built-in set")
	}
	if err := p.Validate(); err != nil {
		t.Errorf("generic-admin-elevation pattern fails Validate(): %v", err)
	}
	if len(p.Detection.AdminHandlerNamePatterns) == 0 {
		t.Error("generic-admin-elevation should have admin_handler_name_patterns populated")
	}
	if len(p.Detection.AdminPackagePaths) == 0 {
		t.Error("generic-admin-elevation should have admin_package_paths populated")
	}
	if len(p.Detection.ElevatedAuthPatterns) == 0 {
		t.Error("generic-admin-elevation should have elevated_auth_patterns populated")
	}
}

func TestAdminElevation_LoadBuiltin_GenericPattern_FunctionNameFires(t *testing.T) {
	// Smoke test: the built-in generic pattern catches admin-named functions.
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin() error: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/api/users.go", "net/http")
	addFunctionWithCalls(g, "/project/api/users.go", "adminUsers")

	vs := e.CheckFile(g, "/project/api/users.go", nil)
	found := findViolation(vs, "generic-admin-elevation")
	if found == nil {
		t.Fatal("generic-admin-elevation should fire for admin-named function, got none")
	}
}

func TestAdminElevation_LoadBuiltin_GenericPattern_AdminPackageFires(t *testing.T) {
	// Smoke test: the built-in generic pattern catches files in admin/ directories.
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin() error: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/internal/admin/handlers.go", "net/http")
	addFunctionWithCalls(g, "/project/internal/admin/handlers.go", "ListUsers")
	// ListUsers doesn't match admin name patterns, but file is in admin/.

	vs := e.CheckFile(g, "/project/internal/admin/handlers.go", nil)
	found := findViolation(vs, "generic-admin-elevation")
	if found == nil {
		t.Fatal("generic-admin-elevation should fire for file in admin/ package, got none")
	}
}
