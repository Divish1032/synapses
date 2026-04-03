package security

// engine_python_frameworks_test.go: integration tests for Python framework security
// patterns (Sprint 26.4). Tests verify that FastAPI, Django, and Flask patterns load
// correctly and fire with the right behavior: framework gate blocks non-matching files,
// auth patterns fire on missing auth, FastAPI input-validation fires on missing Pydantic,
// and the direct-DB-import pattern fires on route files importing raw database drivers.

import (
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────────
// Loader integration — verify python-frameworks.json loads via LoadBuiltin
// ──────────────────────────────────────────────────────────────────────────────

func TestPythonFrameworks_LoadBuiltin_ContainsAllPatterns(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin() error: %v", err)
	}

	wantIDs := []string{
		"python-fastapi-missing-auth",
		"python-fastapi-missing-input-validation",
		"python-django-missing-auth",
		"python-flask-missing-auth",
		"python-direct-db-import",
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

func TestPythonFrameworks_DefaultEngine_PatternCount(t *testing.T) {
	e := DefaultEngine()
	// 50 pre-existing patterns (from sprints 26.1-26.9) + 5 new Python framework patterns = 55 total.
	if e.PatternCount() < 55 {
		t.Errorf("DefaultEngine PatternCount = %d, want >= 55", e.PatternCount())
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// FastAPI — missing auth
// ──────────────────────────────────────────────────────────────────────────────

func TestPythonFrameworks_FastAPI_MissingAuth_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// FastAPI file: imports fastapi, calls route methods, no auth.
	addFileWithImports(g, "/project/api/routes.py", "fastapi")
	addFunctionWithCalls(g, "/project/api/routes.py", "register_routes", "get", "post")

	violations := e.CheckFile(g, "/project/api/routes.py", nil)
	found := findViolation(violations, "python-fastapi-missing-auth")
	if found == nil {
		t.Fatal("expected python-fastapi-missing-auth violation, got none")
	}
	if found.Severity != SeverityCritical {
		t.Errorf("severity = %s, want CRITICAL", found.Severity)
	}
}

func TestPythonFrameworks_FastAPI_MissingAuth_WithDepends_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/users.py", "fastapi")
	addFunctionWithCalls(g, "/project/api/users.py", "create_user", "post", "Depends")

	violations := e.CheckFile(g, "/project/api/users.py", nil)
	if findViolation(violations, "python-fastapi-missing-auth") != nil {
		t.Error("expected no violation when Depends is called")
	}
}

func TestPythonFrameworks_FastAPI_MissingAuth_WithHTTPBearer_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/secure.py", "fastapi")
	addFunctionWithCalls(g, "/project/api/secure.py", "setup", "get", "HTTPBearer")

	violations := e.CheckFile(g, "/project/api/secure.py", nil)
	if findViolation(violations, "python-fastapi-missing-auth") != nil {
		t.Error("expected no violation when HTTPBearer is called")
	}
}

func TestPythonFrameworks_FastAPI_MissingAuth_WithOAuth2_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/oauth.py", "fastapi")
	addFunctionWithCalls(g, "/project/api/oauth.py", "setup_oauth", "post", "OAuth2PasswordBearer")

	violations := e.CheckFile(g, "/project/api/oauth.py", nil)
	if findViolation(violations, "python-fastapi-missing-auth") != nil {
		t.Error("expected no violation when OAuth2PasswordBearer is called")
	}
}

func TestPythonFrameworks_FastAPI_MissingAuth_NoRoutes_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Imports fastapi but registers no routes — utility/config file.
	addFileWithImports(g, "/project/api/deps.py", "fastapi")
	addFunctionWithCalls(g, "/project/api/deps.py", "get_db", "Session")

	violations := e.CheckFile(g, "/project/api/deps.py", nil)
	if findViolation(violations, "python-fastapi-missing-auth") != nil {
		t.Error("expected no violation for fastapi file with no route registrations")
	}
}

func TestPythonFrameworks_FastAPI_MissingAuth_FrameworkGate_NonFastAPI_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Does NOT import fastapi — Django file.
	addFileWithImports(g, "/project/views.py", "django")
	addFunctionWithCalls(g, "/project/views.py", "register", "get", "post")

	violations := e.CheckFile(g, "/project/views.py", nil)
	if findViolation(violations, "python-fastapi-missing-auth") != nil {
		t.Error("framework gate failed: python-fastapi-missing-auth fired on non-fastapi file")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// FastAPI — missing input validation (Pydantic)
// ──────────────────────────────────────────────────────────────────────────────

func TestPythonFrameworks_FastAPI_MissingInputValidation_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// FastAPI file: mutation routes, no Pydantic function calls.
	addFileWithImports(g, "/project/api/items.py", "fastapi")
	addFunctionWithCalls(g, "/project/api/items.py", "create_item", "post")

	violations := e.CheckFile(g, "/project/api/items.py", nil)
	found := findViolation(violations, "python-fastapi-missing-input-validation")
	if found == nil {
		t.Fatal("expected python-fastapi-missing-input-validation violation, got none")
	}
	if found.Severity != SeverityHigh {
		t.Errorf("severity = %s, want HIGH", found.Severity)
	}
}

func TestPythonFrameworks_FastAPI_MissingInputValidation_WithFieldValidator_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/items.py", "fastapi")
	addFunctionWithCalls(g, "/project/api/items.py", "create_item", "post", "field_validator")

	violations := e.CheckFile(g, "/project/api/items.py", nil)
	if findViolation(violations, "python-fastapi-missing-input-validation") != nil {
		t.Error("expected no violation when field_validator is called")
	}
}

func TestPythonFrameworks_FastAPI_MissingInputValidation_WithBody_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/orders.py", "fastapi")
	addFunctionWithCalls(g, "/project/api/orders.py", "place_order", "post", "Body")

	violations := e.CheckFile(g, "/project/api/orders.py", nil)
	if findViolation(violations, "python-fastapi-missing-input-validation") != nil {
		t.Error("expected no violation when Body() is called")
	}
}

func TestPythonFrameworks_FastAPI_MissingInputValidation_GetOnly_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Only GET routes — input validation not required.
	addFileWithImports(g, "/project/api/list.py", "fastapi")
	addFunctionWithCalls(g, "/project/api/list.py", "list_items", "get")

	violations := e.CheckFile(g, "/project/api/list.py", nil)
	if findViolation(violations, "python-fastapi-missing-input-validation") != nil {
		t.Error("expected no violation for GET-only route file")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Django — missing auth
// ──────────────────────────────────────────────────────────────────────────────

func TestPythonFrameworks_Django_MissingAuth_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Django views file: imports django, no auth decorator.
	addFileWithImports(g, "/project/app/views.py", "django")
	addFunctionWithCalls(g, "/project/app/views.py", "UserDetailView", "render", "get_object_or_404")

	violations := e.CheckFile(g, "/project/app/views.py", nil)
	found := findViolation(violations, "python-django-missing-auth")
	if found == nil {
		t.Fatal("expected python-django-missing-auth violation, got none")
	}
	if found.Severity != SeverityCritical {
		t.Errorf("severity = %s, want CRITICAL", found.Severity)
	}
}

func TestPythonFrameworks_Django_MissingAuth_WithLoginRequired_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/app/views.py", "django")
	addFunctionWithCalls(g, "/project/app/views.py", "my_view", "login_required", "render")

	violations := e.CheckFile(g, "/project/app/views.py", nil)
	if findViolation(violations, "python-django-missing-auth") != nil {
		t.Error("expected no violation when login_required is called")
	}
}

func TestPythonFrameworks_Django_MissingAuth_WithPermissionRequired_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/app/views.py", "django")
	addFunctionWithCalls(g, "/project/app/views.py", "admin_view", "permission_required", "render")

	violations := e.CheckFile(g, "/project/app/views.py", nil)
	if findViolation(violations, "python-django-missing-auth") != nil {
		t.Error("expected no violation when permission_required is called")
	}
}

func TestPythonFrameworks_Django_MissingAuth_HandlerFilePattern_Scoping(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Django file NOT matching views pattern — should not fire.
	addFileWithImports(g, "/project/app/models.py", "django")
	addFunctionWithCalls(g, "/project/app/models.py", "get_user", "filter", "get")

	violations := e.CheckFile(g, "/project/app/models.py", nil)
	if findViolation(violations, "python-django-missing-auth") != nil {
		t.Error("expected no violation for models.py — not a view file")
	}
}

func TestPythonFrameworks_Django_MissingAuth_ViewsSubdir_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// views/ subdirectory pattern.
	addFileWithImports(g, "/project/app/views/user_views.py", "django")
	addFunctionWithCalls(g, "/project/app/views/user_views.py", "UserList", "render")

	violations := e.CheckFile(g, "/project/app/views/user_views.py", nil)
	found := findViolation(violations, "python-django-missing-auth")
	if found == nil {
		t.Fatal("expected python-django-missing-auth violation for views/ subdirectory file")
	}
}

func TestPythonFrameworks_Django_MissingAuth_FrameworkGate_FastAPI_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// File named views.py but imports fastapi not django.
	addFileWithImports(g, "/project/api/views.py", "fastapi")
	addFunctionWithCalls(g, "/project/api/views.py", "list_items", "get", "post")

	violations := e.CheckFile(g, "/project/api/views.py", nil)
	if findViolation(violations, "python-django-missing-auth") != nil {
		t.Error("framework gate failed: python-django-missing-auth fired on fastapi file")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Flask — missing auth
// ──────────────────────────────────────────────────────────────────────────────

func TestPythonFrameworks_Flask_MissingAuth_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Flask file: imports flask, registers routes via @app.route, no auth.
	addFileWithImports(g, "/project/app/routes.py", "flask")
	addFunctionWithCalls(g, "/project/app/routes.py", "register_routes", "route", "post")

	violations := e.CheckFile(g, "/project/app/routes.py", nil)
	found := findViolation(violations, "python-flask-missing-auth")
	if found == nil {
		t.Fatal("expected python-flask-missing-auth violation, got none")
	}
	if found.Severity != SeverityCritical {
		t.Errorf("severity = %s, want CRITICAL", found.Severity)
	}
}

func TestPythonFrameworks_Flask_MissingAuth_WithLoginRequired_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/app/routes.py", "flask")
	addFunctionWithCalls(g, "/project/app/routes.py", "protected_route", "route", "login_required")

	violations := e.CheckFile(g, "/project/app/routes.py", nil)
	if findViolation(violations, "python-flask-missing-auth") != nil {
		t.Error("expected no violation when login_required is called")
	}
}

func TestPythonFrameworks_Flask_MissingAuth_WithJWTRequired_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/app/api.py", "flask")
	addFunctionWithCalls(g, "/project/app/api.py", "get_users", "route", "jwt_required")

	violations := e.CheckFile(g, "/project/app/api.py", nil)
	if findViolation(violations, "python-flask-missing-auth") != nil {
		t.Error("expected no violation when jwt_required is called")
	}
}

func TestPythonFrameworks_Flask_MissingAuth_Flask2_GetRoute_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Flask 2.0+ style: @app.get() instead of @app.route()
	addFileWithImports(g, "/project/app/views.py", "flask")
	addFunctionWithCalls(g, "/project/app/views.py", "list_users", "get", "post")

	violations := e.CheckFile(g, "/project/app/views.py", nil)
	found := findViolation(violations, "python-flask-missing-auth")
	if found == nil {
		t.Fatal("expected python-flask-missing-auth violation for Flask 2.0+ style routes")
	}
}

func TestPythonFrameworks_Flask_MissingAuth_NoRoutes_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Imports flask but no route registrations.
	addFileWithImports(g, "/project/app/helpers.py", "flask")
	addFunctionWithCalls(g, "/project/app/helpers.py", "format_response", "jsonify")

	violations := e.CheckFile(g, "/project/app/helpers.py", nil)
	if findViolation(violations, "python-flask-missing-auth") != nil {
		t.Error("expected no violation for flask file with no route registrations")
	}
}

func TestPythonFrameworks_Flask_MissingAuth_FrameworkGate_Django_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Django file — should not trigger Flask auth pattern.
	addFileWithImports(g, "/project/app/views.py", "django")
	addFunctionWithCalls(g, "/project/app/views.py", "view_func", "route", "post")

	violations := e.CheckFile(g, "/project/app/views.py", nil)
	if findViolation(violations, "python-flask-missing-auth") != nil {
		t.Error("framework gate failed: python-flask-missing-auth fired on django file")
	}
}

func TestPythonFrameworks_Flask_BeforeRequestLogging_StillFires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Flask file with a before_request hook for logging only — routes are still unprotected.
	// before_request alone must NOT suppress the violation; what matters is what the hook calls.
	addFileWithImports(g, "/project/app/routes.py", "flask")
	addFunctionWithCalls(g, "/project/app/routes.py", "setup", "route", "get", "before_request", "logging")

	violations := e.CheckFile(g, "/project/app/routes.py", nil)
	found := findViolation(violations, "python-flask-missing-auth")
	if found == nil {
		t.Error("before_request for logging should not suppress missing-auth: the hook itself is not auth enforcement")
	}
}

func TestPythonFrameworks_Flask_BeforeRequestWithAuthenticate_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Flask file where before_request hook calls authenticate() — auth IS enforced.
	// The auth function called inside the hook appears in CALLS edges and suppresses correctly.
	addFileWithImports(g, "/project/app/routes.py", "flask")
	addFunctionWithCalls(g, "/project/app/routes.py", "setup", "route", "get", "before_request", "authenticate")

	violations := e.CheckFile(g, "/project/app/routes.py", nil)
	if findViolation(violations, "python-flask-missing-auth") != nil {
		t.Error("before_request hook calling authenticate() is valid auth enforcement — should not fire")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Python direct DB import
// ──────────────────────────────────────────────────────────────────────────────

func TestPythonFrameworks_DirectDBImport_FastAPI_SQLAlchemy_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// FastAPI router file importing sqlalchemy directly.
	addFileWithImports(g, "/project/api/router.py", "fastapi", "sqlalchemy")
	addFunctionWithCalls(g, "/project/api/router.py", "get_items", "get")

	violations := e.CheckFile(g, "/project/api/router.py", nil)
	found := findViolation(violations, "python-direct-db-import")
	if found == nil {
		t.Fatal("expected python-direct-db-import violation, got none")
	}
	if found.Severity != SeverityMedium {
		t.Errorf("severity = %s, want MEDIUM", found.Severity)
	}
}

func TestPythonFrameworks_DirectDBImport_Flask_Psycopg2_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Flask routes file importing psycopg2 directly.
	addFileWithImports(g, "/project/app/routes.py", "flask", "psycopg2")
	addFunctionWithCalls(g, "/project/app/routes.py", "get_users", "route")

	violations := e.CheckFile(g, "/project/app/routes.py", nil)
	found := findViolation(violations, "python-direct-db-import")
	if found == nil {
		t.Fatal("expected python-direct-db-import violation for psycopg2 import in Flask routes")
	}
}

func TestPythonFrameworks_DirectDBImport_Django_Views_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Django views.py — framework gate excludes django files from direct-db-import.
	// Django views importing models is intentional, not a layer violation.
	addFileWithImports(g, "/project/app/views.py", "django", "sqlalchemy")
	addFunctionWithCalls(g, "/project/app/views.py", "user_list", "render")

	violations := e.CheckFile(g, "/project/app/views.py", nil)
	if findViolation(violations, "python-direct-db-import") != nil {
		t.Error("framework gate failed: python-direct-db-import fired on django views file")
	}
}

func TestPythonFrameworks_DirectDBImport_FilePattern_NonHandlerFile_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// FastAPI file outside handler file patterns — should not fire.
	addFileWithImports(g, "/project/db/session.py", "fastapi", "sqlalchemy")
	addFunctionWithCalls(g, "/project/db/session.py", "get_session", "Session")

	violations := e.CheckFile(g, "/project/db/session.py", nil)
	if findViolation(violations, "python-direct-db-import") != nil {
		t.Error("expected no violation for db/session.py — not a handler file path")
	}
}

func TestPythonFrameworks_DirectDBImport_FastAPI_Pymongo_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/endpoints/users.py", "fastapi", "pymongo")
	addFunctionWithCalls(g, "/project/api/endpoints/users.py", "list_users", "get")

	violations := e.CheckFile(g, "/project/api/endpoints/users.py", nil)
	found := findViolation(violations, "python-direct-db-import")
	if found == nil {
		t.Fatal("expected python-direct-db-import violation for pymongo in endpoints file")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Language discrimination — Python patterns must not fire on non-Python files
// ──────────────────────────────────────────────────────────────────────────────

func TestPythonFrameworks_LanguageDiscrimination_GoFile_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// A Go file importing "fastapi" (impossible in practice but tests language gate).
	addFileWithImports(g, "/project/api/routes.go", "fastapi")
	addFunctionWithCalls(g, "/project/api/routes.go", "register", "get", "post")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	for _, v := range violations {
		if v.PatternID == "python-fastapi-missing-auth" ||
			v.PatternID == "python-django-missing-auth" ||
			v.PatternID == "python-flask-missing-auth" {
			t.Errorf("language gate failed: Python pattern %q fired on .go file", v.PatternID)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Regression: include_router / register_blueprint must not be route indicators
// ──────────────────────────────────────────────────────────────────────────────

func TestPythonFrameworks_FastAPI_IncludeRouter_NotARouteIndicator(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Main app file that only aggregates routers — no direct route registrations.
	// include_router is NOT in route_node_names so this should not fire.
	addFileWithImports(g, "/project/main.py", "fastapi")
	addFunctionWithCalls(g, "/project/main.py", "create_app", "include_router")

	violations := e.CheckFile(g, "/project/main.py", nil)
	if findViolation(violations, "python-fastapi-missing-auth") != nil {
		t.Error("include_router alone should not trigger missing-auth (app factory files aggregate routers, not define endpoints)")
	}
}

func TestPythonFrameworks_Flask_RegisterBlueprint_NotARouteIndicator(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Flask app factory that only registers blueprints — no direct route registrations.
	// register_blueprint is NOT in route_node_names so this should not fire.
	addFileWithImports(g, "/project/app/__init__.py", "flask")
	addFunctionWithCalls(g, "/project/app/__init__.py", "create_app", "register_blueprint", "Flask")

	violations := e.CheckFile(g, "/project/app/__init__.py", nil)
	if findViolation(violations, "python-flask-missing-auth") != nil {
		t.Error("register_blueprint alone should not trigger missing-auth (app factory files aggregate blueprints, not define routes)")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Regression: *views.py must not match reviews.py (false positive guard)
// ──────────────────────────────────────────────────────────────────────────────

func TestPythonFrameworks_Django_ReviewsPy_NotAViewFile(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// A Django reviews.py file — should NOT match the views.py handler_file_pattern.
	// Before the fix, *views.py would match reviews.py via suffix matching.
	addFileWithImports(g, "/project/app/reviews.py", "django")
	addFunctionWithCalls(g, "/project/app/reviews.py", "ReviewListView", "render")

	violations := e.CheckFile(g, "/project/app/reviews.py", nil)
	if findViolation(violations, "python-django-missing-auth") != nil {
		t.Error("false positive: python-django-missing-auth fired on reviews.py — it ends in 'views.py' but is not a view file")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Regression: current_user alone must NOT suppress Flask auth violation
// ──────────────────────────────────────────────────────────────────────────────

func TestPythonFrameworks_Flask_CurrentUserAlone_DoesNotSuppressAuth(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Flask route that accesses current_user to conditionally display info,
	// but has no actual auth enforcement (@login_required, jwt_required, etc.).
	// Accessing the current_user proxy does not protect the route.
	addFileWithImports(g, "/project/app/public.py", "flask")
	addFunctionWithCalls(g, "/project/app/public.py", "home", "route", "get", "current_user")

	violations := e.CheckFile(g, "/project/app/public.py", nil)
	found := findViolation(violations, "python-flask-missing-auth")
	if found == nil {
		t.Error("current_user access alone should not suppress missing-auth: accessing the proxy does not enforce authentication")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Additional coverage: FastAPI Security scopes
// ──────────────────────────────────────────────────────────────────────────────

func TestPythonFrameworks_FastAPI_MissingAuth_WithSecurity_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// FastAPI Security() is used for OAuth2 scopes: Security(get_current_user, scopes=["read"])
	addFileWithImports(g, "/project/api/scoped.py", "fastapi")
	addFunctionWithCalls(g, "/project/api/scoped.py", "read_items", "get", "Security")

	violations := e.CheckFile(g, "/project/api/scoped.py", nil)
	if findViolation(violations, "python-fastapi-missing-auth") != nil {
		t.Error("expected no violation when Security() is used for OAuth2 scopes")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Additional coverage: Django decorators and file patterns
// ──────────────────────────────────────────────────────────────────────────────

func TestPythonFrameworks_Django_MissingAuth_WithUserPassesTest_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/app/views.py", "django")
	addFunctionWithCalls(g, "/project/app/views.py", "staff_view", "user_passes_test", "render")

	violations := e.CheckFile(g, "/project/app/views.py", nil)
	if findViolation(violations, "python-django-missing-auth") != nil {
		t.Error("expected no violation when user_passes_test is called")
	}
}

func TestPythonFrameworks_Django_MissingAuth_WithLoginRequiredMixin_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Django CBV using LoginRequiredMixin as base class.
	// The Python parser stores base classes in NodeStruct.Metadata["heritage_extends"],
	// NOT as CALLS edges. checkMissingAnnotation reads heritage_extends to detect mixin auth.
	addFileWithImports(g, "/project/app/views.py", "django")
	addStructWithHeritage(g, "/project/app/views.py", "UserDetailView", "LoginRequiredMixin", "DetailView")

	violations := e.CheckFile(g, "/project/app/views.py", nil)
	if findViolation(violations, "python-django-missing-auth") != nil {
		t.Error("expected no violation: LoginRequiredMixin in heritage_extends should suppress django-missing-auth")
	}
}

func TestPythonFrameworks_Django_MissingAuth_CBV_NoMixin_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Django CBV with no auth mixin — only View as base. Should fire.
	addFileWithImports(g, "/project/app/views.py", "django")
	addStructWithHeritage(g, "/project/app/views.py", "UserDetailView", "View")

	violations := e.CheckFile(g, "/project/app/views.py", nil)
	found := findViolation(violations, "python-django-missing-auth")
	if found == nil {
		t.Fatal("expected python-django-missing-auth: CBV with only View base has no auth")
	}
}

func TestPythonFrameworks_Django_MissingAuth_CBV_PermissionRequiredMixin_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// PermissionRequiredMixin is the other common Django CBV auth mixin.
	addFileWithImports(g, "/project/app/views.py", "django")
	addStructWithHeritage(g, "/project/app/views.py", "AdminView", "PermissionRequiredMixin", "View")

	violations := e.CheckFile(g, "/project/app/views.py", nil)
	if findViolation(violations, "python-django-missing-auth") != nil {
		t.Error("expected no violation: PermissionRequiredMixin in heritage_extends suppresses django-missing-auth")
	}
}

func TestPythonFrameworks_Django_MissingAuth_ViewsetsFile_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Django REST Framework viewsets.py with no auth — should fire.
	addFileWithImports(g, "/project/api/viewsets.py", "django")
	addFunctionWithCalls(g, "/project/api/viewsets.py", "UserViewSet", "list", "retrieve")

	violations := e.CheckFile(g, "/project/api/viewsets.py", nil)
	found := findViolation(violations, "python-django-missing-auth")
	if found == nil {
		t.Fatal("expected python-django-missing-auth violation for viewsets.py file")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Additional coverage: Flask add_url_rule as route indicator
// ──────────────────────────────────────────────────────────────────────────────

func TestPythonFrameworks_Flask_MissingAuth_AddUrlRule_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Flask programmatic route registration via add_url_rule — no auth.
	addFileWithImports(g, "/project/app/routes.py", "flask")
	addFunctionWithCalls(g, "/project/app/routes.py", "register_routes", "add_url_rule")

	violations := e.CheckFile(g, "/project/app/routes.py", nil)
	found := findViolation(violations, "python-flask-missing-auth")
	if found == nil {
		t.Fatal("expected python-flask-missing-auth violation when add_url_rule is used without auth")
	}
}
