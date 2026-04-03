// Package security defines the declarative security pattern specification format.
//
// Sprint 26.1: Security patterns are the foundation of the framework-aware security
// engine. Each pattern specifies what constitutes a security violation for a given
// framework, how to detect it in the AST graph, and what message to surface when
// it fires.
//
// Architecture:
//   - Patterns are loaded at startup from embedded built-ins and optional user dirs.
//   - The pattern matching engine (Sprint 26.7) queries patterns via PatternSet.
//   - Per-language patterns (Sprints 26.2–26.6) are JSON files in builtin/.
//   - Users can add custom patterns or override built-ins via SecurityPatternsDir.
package security

import (
	"fmt"
	"strings"
)

// PatternType classifies the security concern a pattern detects.
type PatternType string

const (
	// PatternTypeAuthMiddleware detects routes or handlers missing authentication middleware.
	// This is OWASP A01 (Broken Access Control) — the most critical class.
	PatternTypeAuthMiddleware PatternType = "auth_middleware"

	// PatternTypeRateLimiting detects endpoints without rate limiting applied.
	// Absence enables credential stuffing, enumeration, and DDoS.
	PatternTypeRateLimiting PatternType = "rate_limiting"

	// PatternTypeInputValidation detects handlers that accept user input without
	// a validation/sanitization step in the call path.
	PatternTypeInputValidation PatternType = "input_validation"

	// PatternTypeCSRFProtection detects POST/PUT/DELETE endpoints missing CSRF protection.
	// Relevant for web apps with session-cookie auth.
	PatternTypeCSRFProtection PatternType = "csrf_protection"

	// PatternTypeHardcodedSecret detects credentials, tokens, or keys embedded
	// as string literals rather than loaded from environment or secret stores.
	PatternTypeHardcodedSecret PatternType = "hardcoded_secret"

	// PatternTypeLayerViolation detects when code in one architectural layer
	// directly accesses code in a non-adjacent layer (e.g. handler → database).
	PatternTypeLayerViolation PatternType = "layer_violation"

	// PatternTypeAdminElevation detects admin routes or handlers that use the
	// same authorization level as regular user routes — missing role-based access.
	PatternTypeAdminElevation PatternType = "admin_elevation"
)

// Severity determines how the pattern matching engine responds when a violation fires.
type Severity string

const (
	// SeverityCritical means the agent must fix this before proceeding.
	// Example: unauthenticated destructive endpoint, hardcoded JWT secret.
	SeverityCritical Severity = "CRITICAL"

	// SeverityHigh means a strong warning is surfaced. The agent can override
	// with justification, but must acknowledge the finding.
	// Example: missing rate limiting on a login endpoint.
	SeverityHigh Severity = "HIGH"

	// SeverityMedium means the finding is informational. The agent decides.
	// Example: handler directly importing the ORM (layer warning, not block).
	SeverityMedium Severity = "MEDIUM"
)

// CheckType selects the detection algorithm in the pattern matching engine (Sprint 26.7).
// Each value maps to a specific analysis method applied to the parsed AST graph.
type CheckType string

const (
	// CheckTypeMissingMiddleware fires when a route or handler node does not call
	// any function matching RequiredCallPatterns in its call graph.
	// Primary check for auth_middleware and rate_limiting pattern types.
	CheckTypeMissingMiddleware CheckType = "missing_middleware"

	// CheckTypeDirectImport fires when a file matching HandlerFilePatterns imports
	// a package matching ForbiddenImportPatterns.
	// Primary check for layer_violation pattern type.
	CheckTypeDirectImport CheckType = "direct_import"

	// CheckTypeMissingAnnotation fires when a class or function in a handler file
	// lacks a required annotation or decorator matching AnnotationPatterns.
	// Primary check for Java Spring, Python FastAPI/Django.
	CheckTypeMissingAnnotation CheckType = "missing_annotation"

	// CheckTypeHardcodedSecret fires when a string literal assignment in the source
	// matches SecretPatterns AND is assigned to a variable whose name suggests a
	// credential (secret, key, token, password, api_key, etc.).
	// AST-aware: test files and config struct declarations receive lower severity.
	CheckTypeHardcodedSecret CheckType = "hardcoded_secret"

	// CheckTypeAdminElevation fires when a route or handler matching AdminPathPatterns
	// is found but does NOT call any function matching ElevatedAuthPatterns that is
	// distinct from the RequiredCallPatterns used for regular routes.
	CheckTypeAdminElevation CheckType = "admin_elevation"

	// CheckTypeCrossTransportAuth fires when auth middleware is applied to some
	// transport types (HTTP) but not others (WebSocket, gRPC) in the same project.
	// Requires project-scope analysis.
	CheckTypeCrossTransportAuth CheckType = "cross_transport_auth"
)

// DetectionScope limits the breadth of analysis the engine performs for a pattern.
type DetectionScope string

const (
	// ScopeFile scopes the check to a single changed file.
	// Fastest; suitable for most checks (auth, layer violations, secrets).
	ScopeFile DetectionScope = "file"

	// ScopePackage scopes the check to all files in the same package as the changed file.
	// Use when a violation requires comparing the new handler against sibling handlers.
	ScopePackage DetectionScope = "package"

	// ScopeProject scopes the check to the entire project graph.
	// Use only for checks that require cross-package consistency (cross_transport_auth).
	ScopeProject DetectionScope = "project"
)

// Detection specifies the algorithmic check the pattern matching engine will perform.
// Fields are algorithm-specific: each CheckType consumes a subset of these fields.
// Unused fields for a given CheckType are ignored.
type Detection struct {
	// CheckType is the algorithm selector. Required.
	CheckType CheckType `json:"check_type"`

	// FrameworkIdentifiers are import paths or call patterns that positively identify
	// the framework in use. If non-empty and NONE match the file being analyzed, this
	// pattern is skipped entirely. This is the zero-false-positive gate.
	//
	// Examples:
	//   "github.com/go-chi/chi/v5"    (exact Go import path)
	//   "github.com/gin-gonic/gin"    (exact Go import path)
	//   "express"                      (npm package name in imports)
	//   "fastapi"                      (Python import name)
	//   "org.springframework.boot"     (Java package prefix)
	FrameworkIdentifiers []string `json:"framework_identifiers,omitempty"`

	// RequiredCallPatterns are glob patterns for function or method names that
	// handlers MUST call (directly or transitively within scope). A violation fires
	// when a handler/route node does NOT call any matching function.
	//
	// Used by: CheckTypeMissingMiddleware, CheckTypeAdminElevation
	// Examples: ["Auth*", "JWT*", "authenticate", "checkToken", "RequireAuth"]
	RequiredCallPatterns []string `json:"required_call_patterns,omitempty"`

	// ElevatedAuthPatterns are glob patterns for the higher-privilege auth functions
	// required by admin routes. Must be a STRICT SUPERSET of RequiredCallPatterns
	// to be meaningful (i.e. admin routes need BOTH regular auth AND elevated auth).
	//
	// Used by: CheckTypeAdminElevation
	// Examples: ["RequireAdmin*", "RequireRole*", "hasRole*", "@PreAuthorize*"]
	ElevatedAuthPatterns []string `json:"elevated_auth_patterns,omitempty"`

	// ForbiddenImportPatterns are import path globs that MUST NOT appear in files
	// matching HandlerFilePatterns.
	//
	// Used by: CheckTypeDirectImport
	// Examples: ["*/database/sql*", "*/db/*", "database/*", "gorm.io/*"]
	ForbiddenImportPatterns []string `json:"forbidden_import_patterns,omitempty"`

	// HandlerFilePatterns are glob patterns matching files that contain handlers
	// or controllers. Used to scope import and annotation checks to the right layer.
	//
	// Used by: CheckTypeDirectImport, CheckTypeMissingAnnotation
	// Examples: ["*/handler*", "*/api/*", "*_handler.go", "*/routes/*"]
	HandlerFilePatterns []string `json:"handler_file_patterns,omitempty"`

	// RouteNodeNames are function or method names that register routes in this framework.
	// Used to identify route registration call sites so the engine can check each one.
	//
	// Used by: CheckTypeMissingMiddleware
	// Examples (chi/gin): ["Get", "Post", "Put", "Delete", "Patch", "Head", "Options", "Handle", "HandleFunc"]
	// Examples (echo): ["GET", "POST", "PUT", "DELETE"]
	RouteNodeNames []string `json:"route_node_names,omitempty"`

	// MiddlewareNodeNames are function or method names that apply middleware.
	// Used to distinguish middleware application from route registration.
	//
	// Used by: CheckTypeMissingMiddleware (to detect group-level middleware coverage)
	// Examples: ["Use", "Group", "With"]
	MiddlewareNodeNames []string `json:"middleware_node_names,omitempty"`

	// AnnotationPatterns are required annotation or decorator patterns that handlers
	// must carry. Used for frameworks where auth is declared via annotations.
	//
	// Used by: CheckTypeMissingAnnotation
	// Examples (Java):  ["@PreAuthorize*", "@Secured", "@RolesAllowed"]
	// Examples (Python): ["@login_required", "Depends(get_current_user)"]
	AnnotationPatterns []string `json:"annotation_patterns,omitempty"`

	// SecretPatterns are regex patterns for detecting hardcoded credential values.
	// Matched against string literal values in variable assignments where the
	// variable name suggests a credential.
	//
	// Used by: CheckTypeHardcodedSecret
	// Examples: ["(?i)^[a-zA-Z0-9+/]{32,}={0,2}$", "^sk-[a-zA-Z0-9]{32,}$"]
	SecretPatterns []string `json:"secret_patterns,omitempty"`

	// DetectFallback enables detection of the "load from env with hardcoded fallback"
	// anti-pattern. When true, the engine also scans for:
	//   Go:     secret := os.Getenv("X"); if secret == "" { secret = "hardcoded" }
	//   TS/JS:  process.env.X || "fallback"  or  process.env.X ?? "fallback"
	//   Python: os.environ.get("X", "fallback")  or  os.getenv("X") or "fallback"
	//   Java:   System.getenv("X") != null ? System.getenv("X") : "fallback"
	//   Rust:   env::var("X").unwrap_or("fallback")
	//
	// Used by: CheckTypeHardcodedSecret
	DetectFallback bool `json:"detect_fallback,omitempty"`

	// AdminPathPatterns are URL path patterns identifying admin routes.
	// Routes matching these patterns require elevated authorization.
	//
	// Used by: CheckTypeAdminElevation
	// Examples: ["/admin/*", "/management/*", "/internal/*", "*/admin*"]
	AdminPathPatterns []string `json:"admin_path_patterns,omitempty"`

	// AdminHandlerNamePatterns are glob patterns (matched case-insensitively)
	// for function or method names that indicate admin functionality. Functions
	// whose names match are treated as admin handlers and require elevated
	// authorization when they do not call ElevatedAuthPatterns.
	//
	// Used by: CheckTypeAdminElevation
	// Examples: ["*admin*", "handleAdmin*", "*AdminHandler"]
	// Note: "*management*" is intentionally excluded — it matches unrelated names
	// like "documentManagement" or "UserManager". Use AdminPackagePaths for
	// management-scoped detection instead.
	AdminHandlerNamePatterns []string `json:"admin_handler_name_patterns,omitempty"`

	// AdminPackagePaths are file path patterns identifying files that contain
	// admin-level handlers by virtue of their location (e.g. an admin/ package).
	// Files matching these patterns are treated as admin handler files and trigger
	// a file-level violation when ElevatedAuthPatterns is absent, but only when
	// strategies 1 (route path) and 2 (function name) find no violations — this
	// prevents double-reporting the same file via multiple strategies.
	//
	// Used by: CheckTypeAdminElevation
	// Examples: ["*/admin/*", "*/admin.go", "*_admin.go", "*/management/*"]
	AdminPackagePaths []string `json:"admin_package_paths,omitempty"`

	// WebSocketNodeNames are function or method names that identify WebSocket upgrade
	// handlers or WebSocket connection handlers in the project. Used by
	// CheckTypeCrossTransportAuth to classify route nodes as WebSocket transport.
	//
	// These supplement the built-in path-based heuristics (/ws, /websocket, etc.).
	// Specify framework-specific WebSocket upgrade function names here.
	//
	// Used by: CheckTypeCrossTransportAuth
	// Examples (Go):  ["Upgrade", "Accept", "HandleWS", "HandleWebSocket", "upgrader.Upgrade"]
	// Examples (Node.js): ["handleUpgrade", "on('connection')", "io.on"]
	WebSocketNodeNames []string `json:"websocket_node_names,omitempty"`

	// GRPCNodeNames are function or method names that register gRPC service
	// implementations. Used by CheckTypeCrossTransportAuth to classify route nodes
	// as gRPC transport.
	//
	// These supplement the built-in path and name heuristics.
	// Specify framework-specific gRPC registration function names here.
	//
	// Used by: CheckTypeCrossTransportAuth
	// Examples (Go):  ["Register*Server", "RegisterService", "grpc.NewServer"]
	// Examples (Java): ["addService", "bindService"]
	GRPCNodeNames []string `json:"grpc_node_names,omitempty"`

	// Scope controls how broadly the engine analyzes the codebase for this pattern.
	// Defaults to ScopeFile when empty.
	Scope DetectionScope `json:"scope,omitempty"`
}

// SecurityPattern is the declarative specification for a single security check.
//
// Patterns are the atomic unit of the Sprint 26 security library. Each pattern
// targets one specific vulnerability class in one specific framework. They are
// loaded from JSON files at startup and consumed by the pattern matching engine.
//
// Example (Go, chi, missing auth):
//
//	{
//	  "id": "go-chi-missing-auth",
//	  "name": "Chi route missing auth middleware",
//	  "language": "go",
//	  "framework": "chi",
//	  "pattern_type": "auth_middleware",
//	  "severity": "CRITICAL",
//	  "description": "A chi route is registered without auth middleware in its call chain.",
//	  "detection": {
//	    "check_type": "missing_middleware",
//	    "framework_identifiers": ["github.com/go-chi/chi/v5"],
//	    "required_call_patterns": ["Auth*", "JWT*", "authenticate", "RequireAuth"],
//	    "route_node_names": ["Get", "Post", "Put", "Delete", "Patch"],
//	    "middleware_node_names": ["Use", "Group", "With"]
//	  },
//	  "message": "Route {target} in {file} is not protected by auth middleware. All {count} other routes in this router use auth middleware."
//	}
type SecurityPattern struct {
	// ID is the unique slug identifier for this pattern.
	// Convention: "{language}-{framework}-{short-description}", e.g. "go-chi-missing-auth".
	// User patterns can override built-ins by using the same ID.
	ID string `json:"id"`

	// Name is the human-readable display name for this pattern.
	Name string `json:"name"`

	// Language identifies which source language this pattern applies to.
	// One of: "go", "typescript", "javascript", "python", "java", "rust".
	// Use "*" for patterns that apply across all languages (uncommon).
	Language string `json:"language"`

	// Framework identifies the specific web framework this pattern targets.
	// Examples: "chi", "gin", "echo", "express", "fastapi", "spring", "actix-web", "axum".
	// Use "*" for language-wide patterns (e.g. hardcoded secrets in any Go file).
	Framework string `json:"framework"`

	// PatternType classifies the security concern this pattern detects.
	PatternType PatternType `json:"pattern_type"`

	// Severity determines the response when this pattern fires.
	Severity Severity `json:"severity"`

	// Description explains what the pattern detects and why it matters.
	// Written for an AI agent reading a validate response — concise, action-oriented.
	Description string `json:"description"`

	// Detection specifies how the pattern matching engine identifies violations.
	Detection Detection `json:"detection"`

	// Message is the natural-language template injected into validate responses.
	// Supports placeholders: {target} (route/handler name), {file} (file path),
	// {count} (number of compliant siblings), {total} (total sibling count).
	Message string `json:"message"`

	// Tags are optional labels for grouping and filtering.
	// Examples: "owasp-a01", "auth", "middleware", "production-critical".
	Tags []string `json:"tags,omitempty"`

	// Enabled controls whether this pattern is active. Defaults to true when omitted.
	// Users can disable a built-in pattern by placing a file with the same ID
	// and enabled:false in their SecurityPatternsDir.
	Enabled *bool `json:"enabled,omitempty"`
}

// IsEnabled reports whether this pattern should be applied.
// A nil Enabled field defaults to true (opt-out model).
func (p SecurityPattern) IsEnabled() bool {
	if p.Enabled == nil {
		return true
	}
	return *p.Enabled
}

// Validate checks that the pattern has all required fields populated and that
// enum values are valid. Returns an error describing the first problem found.
func (p SecurityPattern) Validate() error {
	if p.ID == "" {
		return fmt.Errorf("security pattern missing required field: id")
	}
	if p.Name == "" {
		return fmt.Errorf("security pattern %q missing required field: name", p.ID)
	}
	if p.Language == "" {
		return fmt.Errorf("security pattern %q missing required field: language", p.ID)
	}
	if p.Framework == "" {
		return fmt.Errorf("security pattern %q missing required field: framework", p.ID)
	}
	if p.PatternType == "" {
		return fmt.Errorf("security pattern %q missing required field: pattern_type", p.ID)
	}
	if err := p.PatternType.Validate(); err != nil {
		return fmt.Errorf("security pattern %q: %w", p.ID, err)
	}
	if p.Severity == "" {
		return fmt.Errorf("security pattern %q missing required field: severity", p.ID)
	}
	if err := p.Severity.Validate(); err != nil {
		return fmt.Errorf("security pattern %q: %w", p.ID, err)
	}
	if p.Description == "" {
		return fmt.Errorf("security pattern %q missing required field: description", p.ID)
	}
	if p.Message == "" {
		return fmt.Errorf("security pattern %q missing required field: message", p.ID)
	}
	if p.Detection.CheckType == "" {
		return fmt.Errorf("security pattern %q missing required field: detection.check_type", p.ID)
	}
	if err := p.Detection.CheckType.Validate(); err != nil {
		return fmt.Errorf("security pattern %q: %w", p.ID, err)
	}
	return nil
}

// Validate reports whether pt is a known PatternType value.
func (pt PatternType) Validate() error {
	switch pt {
	case PatternTypeAuthMiddleware, PatternTypeRateLimiting, PatternTypeInputValidation,
		PatternTypeCSRFProtection, PatternTypeHardcodedSecret, PatternTypeLayerViolation,
		PatternTypeAdminElevation:
		return nil
	default:
		return fmt.Errorf("unknown pattern_type %q (valid: auth_middleware, rate_limiting, input_validation, csrf_protection, hardcoded_secret, layer_violation, admin_elevation)", pt)
	}
}

// Validate reports whether s is a known Severity value.
func (s Severity) Validate() error {
	switch s {
	case SeverityCritical, SeverityHigh, SeverityMedium:
		return nil
	default:
		return fmt.Errorf("unknown severity %q (valid: CRITICAL, HIGH, MEDIUM)", s)
	}
}

// Validate reports whether ct is a known CheckType value.
func (ct CheckType) Validate() error {
	switch ct {
	case CheckTypeMissingMiddleware, CheckTypeDirectImport, CheckTypeMissingAnnotation,
		CheckTypeHardcodedSecret, CheckTypeAdminElevation, CheckTypeCrossTransportAuth:
		return nil
	default:
		return fmt.Errorf("unknown check_type %q (valid: missing_middleware, direct_import, missing_annotation, hardcoded_secret, admin_elevation, cross_transport_auth)", ct)
	}
}

// PatternSet is a validated, queryable collection of SecurityPatterns.
// Loaded at startup via LoadAll(); consumed by the pattern matching engine (Sprint 26.7).
type PatternSet struct {
	patterns []SecurityPattern
}

// newPatternSet constructs a PatternSet from a slice of already-validated patterns.
func newPatternSet(patterns []SecurityPattern) *PatternSet {
	return &PatternSet{patterns: patterns}
}

// All returns all enabled patterns in the set.
func (ps *PatternSet) All() []SecurityPattern {
	if ps == nil {
		return nil
	}
	out := make([]SecurityPattern, 0, len(ps.patterns))
	for _, p := range ps.patterns {
		if p.IsEnabled() {
			out = append(out, p)
		}
	}
	return out
}

// ForLanguage returns all enabled patterns whose Language matches lang OR is "*".
// The match is case-insensitive.
func (ps *PatternSet) ForLanguage(lang string) []SecurityPattern {
	if ps == nil {
		return nil
	}
	var out []SecurityPattern
	for _, p := range ps.patterns {
		if !p.IsEnabled() {
			continue
		}
		if p.Language == "*" || equalFold(p.Language, lang) {
			out = append(out, p)
		}
	}
	return out
}

// ForFramework returns all enabled patterns whose Framework matches framework OR is "*".
// The match is case-insensitive.
func (ps *PatternSet) ForFramework(framework string) []SecurityPattern {
	if ps == nil {
		return nil
	}
	var out []SecurityPattern
	for _, p := range ps.patterns {
		if !p.IsEnabled() {
			continue
		}
		if p.Framework == "*" || equalFold(p.Framework, framework) {
			out = append(out, p)
		}
	}
	return out
}

// ForLanguageAndFramework returns all enabled patterns matching both language and framework.
// Either field can be "*" in the pattern (wildcard matches anything).
func (ps *PatternSet) ForLanguageAndFramework(lang, framework string) []SecurityPattern {
	if ps == nil {
		return nil
	}
	var out []SecurityPattern
	for _, p := range ps.patterns {
		if !p.IsEnabled() {
			continue
		}
		langMatch := p.Language == "*" || equalFold(p.Language, lang)
		frameworkMatch := p.Framework == "*" || equalFold(p.Framework, framework)
		if langMatch && frameworkMatch {
			out = append(out, p)
		}
	}
	return out
}

// ForCheckType returns all enabled patterns with the given CheckType.
func (ps *PatternSet) ForCheckType(ct CheckType) []SecurityPattern {
	if ps == nil {
		return nil
	}
	var out []SecurityPattern
	for _, p := range ps.patterns {
		if p.IsEnabled() && p.Detection.CheckType == ct {
			out = append(out, p)
		}
	}
	return out
}

// ForPatternType returns all enabled patterns with the given PatternType.
func (ps *PatternSet) ForPatternType(pt PatternType) []SecurityPattern {
	if ps == nil {
		return nil
	}
	var out []SecurityPattern
	for _, p := range ps.patterns {
		if p.IsEnabled() && p.PatternType == pt {
			out = append(out, p)
		}
	}
	return out
}

// ByID returns the first pattern with the given ID, or zero value + false if not found.
// Disabled patterns are included (callers may need to inspect the enabled state).
func (ps *PatternSet) ByID(id string) (SecurityPattern, bool) {
	if ps == nil {
		return SecurityPattern{}, false
	}
	for _, p := range ps.patterns {
		if p.ID == id {
			return p, true
		}
	}
	return SecurityPattern{}, false
}

// Len returns the total number of patterns in the set (including disabled).
func (ps *PatternSet) Len() int {
	if ps == nil {
		return 0
	}
	return len(ps.patterns)
}

// equalFold compares two strings case-insensitively.
// Delegates to strings.EqualFold for correct Unicode handling.
func equalFold(a, b string) bool {
	return strings.EqualFold(a, b)
}
