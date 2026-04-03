package security

// engine_rust_frameworks_test.go: integration tests for Rust web framework security
// patterns (Sprint 26.6). Tests verify that Actix-web, Axum, and Rocket patterns load
// correctly and fire with the right behavior: framework gate blocks non-matching files,
// auth patterns fire on missing auth, and each framework's route registration idiom
// is correctly recognised (Actix .wrap(), Axum .layer(), Rocket .mount()/.attach()).

import (
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────────
// Loader integration — verify rust-frameworks.json loads via LoadBuiltin
// ──────────────────────────────────────────────────────────────────────────────

func TestRustFrameworks_LoadBuiltin_ContainsAllPatterns(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin() error: %v", err)
	}

	wantIDs := []string{
		"rust-actix-missing-auth",
		"rust-axum-missing-auth",
		"rust-rocket-missing-auth",
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

func TestRustFrameworks_DefaultEngine_PatternCount(t *testing.T) {
	e := DefaultEngine()
	// 58 pre-existing patterns (26.1-26.9 + 26.4 Python + 26.5 Java)
	// + 3 new Rust framework patterns = 61 total minimum.
	if e.PatternCount() < 61 {
		t.Errorf("DefaultEngine PatternCount = %d, want >= 61", e.PatternCount())
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Actix-web — missing auth
// ──────────────────────────────────────────────────────────────────────────────

func TestRustFrameworks_Actix_MissingAuth_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Actix-web file: imports actix_web, registers routes, no auth call.
	addFileWithImports(g, "/project/src/routes.rs", "actix_web")
	addFunctionWithCalls(g, "/project/src/routes.rs", "configure", "route", "get", "post")

	violations := e.CheckFile(g, "/project/src/routes.rs", nil)
	found := findViolation(violations, "rust-actix-missing-auth")
	if found == nil {
		t.Fatal("expected rust-actix-missing-auth violation, got none")
	}
	if found.Severity != SeverityCritical {
		t.Errorf("severity = %s, want CRITICAL", found.Severity)
	}
}

func TestRustFrameworks_Actix_MissingAuth_WithAuthMiddleware_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/src/routes.rs", "actix_web")
	addFunctionWithCalls(g, "/project/src/routes.rs", "configure", "route", "get", "AuthMiddleware")

	violations := e.CheckFile(g, "/project/src/routes.rs", nil)
	if findViolation(violations, "rust-actix-missing-auth") != nil {
		t.Error("expected no rust-actix-missing-auth violation when AuthMiddleware is called")
	}
}

func TestRustFrameworks_Actix_MissingAuth_WithHttpAuthentication_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/src/api.rs", "actix_web")
	// HttpAuthentication is the actix-web-httpauth crate's primary type.
	addFunctionWithCalls(g, "/project/src/api.rs", "configure_routes", "service", "HttpAuthentication")

	violations := e.CheckFile(g, "/project/src/api.rs", nil)
	if findViolation(violations, "rust-actix-missing-auth") != nil {
		t.Error("expected no violation when HttpAuthentication is called")
	}
}

func TestRustFrameworks_Actix_MissingAuth_WithJWT_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/src/api.rs", "actix_web")
	addFunctionWithCalls(g, "/project/src/api.rs", "register", "route", "JWTMiddleware")

	violations := e.CheckFile(g, "/project/src/api.rs", nil)
	if findViolation(violations, "rust-actix-missing-auth") != nil {
		t.Error("expected no violation when JWTMiddleware is called")
	}
}

// Framework gate: Actix pattern must not fire on non-Actix Rust files.
func TestRustFrameworks_Actix_FrameworkGate_NoViolationForAxumFile(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// axum import only — actix pattern must not fire.
	addFileWithImports(g, "/project/src/routes.rs", "axum")
	addFunctionWithCalls(g, "/project/src/routes.rs", "build_router", "route", "nest")

	violations := e.CheckFile(g, "/project/src/routes.rs", nil)
	if findViolation(violations, "rust-actix-missing-auth") != nil {
		t.Error("rust-actix-missing-auth must not fire on an axum file")
	}
}

// Framework gate: Actix pattern must not fire on non-Rust files.
func TestRustFrameworks_Actix_FrameworkGate_NoViolationForGoFile(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/api/routes.go", "actix_web")
	addFunctionWithCalls(g, "/project/api/routes.go", "Register", "route", "get", "post")

	violations := e.CheckFile(g, "/project/api/routes.go", nil)
	if findViolation(violations, "rust-actix-missing-auth") != nil {
		t.Error("rust-actix-missing-auth must not fire on a .go file")
	}
}

// Route-only gate: no routes means no violation, even with correct framework import.
func TestRustFrameworks_Actix_NoRoutes_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Imports actix_web but doesn't register any routes.
	addFileWithImports(g, "/project/src/models.rs", "actix_web")
	addFunctionWithCalls(g, "/project/src/models.rs", "build_model", "serialize", "deserialize")

	violations := e.CheckFile(g, "/project/src/models.rs", nil)
	if findViolation(violations, "rust-actix-missing-auth") != nil {
		t.Error("expected no violation when no routes are registered")
	}
}

// Framework identifier: actix-web (hyphen) as well as actix_web (underscore) must trigger.
func TestRustFrameworks_Actix_HyphenIdentifier_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Parser may record the import as "actix-web" (hyphen from Cargo.toml).
	addFileWithImports(g, "/project/src/routes.rs", "actix-web")
	addFunctionWithCalls(g, "/project/src/routes.rs", "setup", "route", "get", "post")

	violations := e.CheckFile(g, "/project/src/routes.rs", nil)
	if findViolation(violations, "rust-actix-missing-auth") == nil {
		t.Fatal("expected rust-actix-missing-auth violation for actix-web (hyphen) import, got none")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Axum — missing auth
// ──────────────────────────────────────────────────────────────────────────────

func TestRustFrameworks_Axum_MissingAuth_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Axum file: imports axum, registers routes, no auth call.
	addFileWithImports(g, "/project/src/router.rs", "axum")
	addFunctionWithCalls(g, "/project/src/router.rs", "build_router", "route", "nest")

	violations := e.CheckFile(g, "/project/src/router.rs", nil)
	found := findViolation(violations, "rust-axum-missing-auth")
	if found == nil {
		t.Fatal("expected rust-axum-missing-auth violation, got none")
	}
	if found.Severity != SeverityCritical {
		t.Errorf("severity = %s, want CRITICAL", found.Severity)
	}
}

func TestRustFrameworks_Axum_MissingAuth_WithAuthLayer_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/src/router.rs", "axum")
	addFunctionWithCalls(g, "/project/src/router.rs", "build_router", "route", "nest", "AuthLayer")

	violations := e.CheckFile(g, "/project/src/router.rs", nil)
	if findViolation(violations, "rust-axum-missing-auth") != nil {
		t.Error("expected no violation when AuthLayer is called")
	}
}

func TestRustFrameworks_Axum_MissingAuth_WithJWTClaims_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/src/router.rs", "axum")
	// Claims is a common extractor type for axum JWT auth.
	addFunctionWithCalls(g, "/project/src/router.rs", "build_router", "route", "Claims")

	violations := e.CheckFile(g, "/project/src/router.rs", nil)
	if findViolation(violations, "rust-axum-missing-auth") != nil {
		t.Error("expected no violation when Claims is called")
	}
}

func TestRustFrameworks_Axum_MissingAuth_WithMerge_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// .merge() also registers routes (merges sub-routers).
	addFileWithImports(g, "/project/src/app.rs", "axum")
	addFunctionWithCalls(g, "/project/src/app.rs", "create_app", "merge", "nest")

	violations := e.CheckFile(g, "/project/src/app.rs", nil)
	found := findViolation(violations, "rust-axum-missing-auth")
	if found == nil {
		t.Fatal("expected violation when merge() registers routes with no auth")
	}
}

// Framework gate: Axum pattern must not fire on non-Axum files.
func TestRustFrameworks_Axum_FrameworkGate_NoViolationForActixFile(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// actix_web import only — axum pattern must not fire.
	addFileWithImports(g, "/project/src/routes.rs", "actix_web")
	addFunctionWithCalls(g, "/project/src/routes.rs", "configure", "route", "get", "post")

	violations := e.CheckFile(g, "/project/src/routes.rs", nil)
	if findViolation(violations, "rust-axum-missing-auth") != nil {
		t.Error("rust-axum-missing-auth must not fire on an actix-web file")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Rocket — missing auth
// ──────────────────────────────────────────────────────────────────────────────

func TestRustFrameworks_Rocket_MissingAuth_Fires(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// Rocket file: imports rocket, mounts routes, no auth call.
	addFileWithImports(g, "/project/src/main.rs", "rocket")
	addFunctionWithCalls(g, "/project/src/main.rs", "rocket", "mount", "launch")

	violations := e.CheckFile(g, "/project/src/main.rs", nil)
	found := findViolation(violations, "rust-rocket-missing-auth")
	if found == nil {
		t.Fatal("expected rust-rocket-missing-auth violation, got none")
	}
	if found.Severity != SeverityCritical {
		t.Errorf("severity = %s, want CRITICAL", found.Severity)
	}
}

func TestRustFrameworks_Rocket_MissingAuth_WithFairing_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/src/main.rs", "rocket")
	// Rocket auth fairings use the Fairing trait pattern.
	addFunctionWithCalls(g, "/project/src/main.rs", "rocket", "mount", "launch", "AuthFairing")

	violations := e.CheckFile(g, "/project/src/main.rs", nil)
	if findViolation(violations, "rust-rocket-missing-auth") != nil {
		t.Error("expected no violation when AuthFairing is called")
	}
}

func TestRustFrameworks_Rocket_MissingAuth_WithAttachAndAuth_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/src/main.rs", "rocket")
	// .attach(AuthFairing) — both attach and AuthFairing present.
	addFunctionWithCalls(g, "/project/src/main.rs", "launch_rocket", "mount", "attach", "AuthGuard")

	violations := e.CheckFile(g, "/project/src/main.rs", nil)
	if findViolation(violations, "rust-rocket-missing-auth") != nil {
		t.Error("expected no violation when attach + auth guard are called")
	}
}

func TestRustFrameworks_Rocket_MissingAuth_WithJWT_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/src/main.rs", "rocket")
	addFunctionWithCalls(g, "/project/src/main.rs", "rocket_launch", "mount", "JWTFairing")

	violations := e.CheckFile(g, "/project/src/main.rs", nil)
	if findViolation(violations, "rust-rocket-missing-auth") != nil {
		t.Error("expected no violation when JWTFairing is called")
	}
}

// Framework gate: Rocket pattern must not fire on non-Rocket files.
func TestRustFrameworks_Rocket_FrameworkGate_NoViolationForAxumFile(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	// axum import only — rocket pattern must not fire.
	addFileWithImports(g, "/project/src/app.rs", "axum")
	addFunctionWithCalls(g, "/project/src/app.rs", "build_router", "route", "nest")

	violations := e.CheckFile(g, "/project/src/app.rs", nil)
	if findViolation(violations, "rust-rocket-missing-auth") != nil {
		t.Error("rust-rocket-missing-auth must not fire on an axum file")
	}
}

// No routes → no violation even with rocket import.
func TestRustFrameworks_Rocket_NoRoutes_NoViolation(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)
	g := buildTestGraph(t)

	addFileWithImports(g, "/project/src/config.rs", "rocket")
	addFunctionWithCalls(g, "/project/src/config.rs", "build_config", "configure", "set_port")

	violations := e.CheckFile(g, "/project/src/config.rs", nil)
	if findViolation(violations, "rust-rocket-missing-auth") != nil {
		t.Error("expected no violation when no routes are mounted")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Cross-framework isolation — all three frameworks only fire on their own files
// ──────────────────────────────────────────────────────────────────────────────

func TestRustFrameworks_CrossIsolation_AllThreePatterns(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps)

	tests := []struct {
		name          string
		file          string
		importPkg     string
		calls         []string
		shouldFire    string // pattern ID that should fire
		shouldNotFire []string
	}{
		{
			name:          "actix-only file fires actix not axum/rocket",
			file:          "/project/src/routes.rs",
			importPkg:     "actix_web",
			calls:         []string{"route", "get", "post"},
			shouldFire:    "rust-actix-missing-auth",
			shouldNotFire: []string{"rust-axum-missing-auth", "rust-rocket-missing-auth"},
		},
		{
			name:          "axum-only file fires axum not actix/rocket",
			file:          "/project/src/router.rs",
			importPkg:     "axum",
			calls:         []string{"route", "nest"},
			shouldFire:    "rust-axum-missing-auth",
			shouldNotFire: []string{"rust-actix-missing-auth", "rust-rocket-missing-auth"},
		},
		{
			name:          "rocket-only file fires rocket not actix/axum",
			file:          "/project/src/main.rs",
			importPkg:     "rocket",
			calls:         []string{"mount", "launch"},
			shouldFire:    "rust-rocket-missing-auth",
			shouldNotFire: []string{"rust-actix-missing-auth", "rust-axum-missing-auth"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := buildTestGraph(t)
			addFileWithImports(g, tc.file, tc.importPkg)
			addFunctionWithCalls(g, tc.file, "handler", tc.calls...)

			violations := e.CheckFile(g, tc.file, nil)
			if findViolation(violations, tc.shouldFire) == nil {
				t.Errorf("expected %s to fire", tc.shouldFire)
			}
			for _, id := range tc.shouldNotFire {
				if findViolation(violations, id) != nil {
					t.Errorf("expected %s NOT to fire", id)
				}
			}
		})
	}
}
