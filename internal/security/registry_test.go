package security

import (
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────────
// editDistance
// ──────────────────────────────────────────────────────────────────────────────

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// Identical
		{"flask-cors", "flask-cors", 0},
		{"", "", 0},
		// One empty
		{"", "abc", 3},
		{"abc", "", 3},
		// One substitution
		{"flask-corse", "flask-cors", 1},   // extra 'e' at end
		{"reqests", "requests", 1},          // missing 'u'
		{"expres", "express", 1},            // missing 's'
		// Two edits
		{"flask-corse", "flask-cors", 1},
		{"axois", "axios", 2},
		// "txopescript" → "typescript": x→y (sub), delete o (del) = 2 edits
		{"txopescript", "typescript", 2},
		// Exact longer
		{"github.com/go-chi/chi/v5", "github.com/go-chi/chi/v5", 0},
	}
	for _, tc := range cases {
		got := editDistance(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// normalizePackageName
// ──────────────────────────────────────────────────────────────────────────────

func TestNormalizePackageName_PyPI(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"Flask", "flask"},
		{"Flask-Cors", "flask-cors"},
		{"flask_cors", "flask-cors"},     // underscore → hyphen
		{"Flask.Cors", "flask-cors"},     // dot → hyphen (PEP 503 — within a package name)
		{"scikit_learn", "scikit-learn"},
		{"Pillow", "pillow"},
		{"flask__cors", "flask-cors"},    // consecutive underscores collapsed (PEP 503)
		{"flask--cors", "flask-cors"},    // consecutive hyphens collapsed
		{"flask_.cors", "flask-cors"},    // mixed separators collapsed
		// normalizePackageName does PEP 503 only — sub-module stripping is in registryKey.
		// "requests.adapters" treated as package name: "requests-adapters" (PEP 503).
		{"requests.adapters", "requests-adapters"}, // dots are PEP 503 separators in package names
		{"", ""},
	}
	for _, tc := range cases {
		got := normalizePackageName(regLangPyPI, tc.input)
		if got != tc.want {
			t.Errorf("normalizePackageName(pypi, %q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestNormalizePackageName_Crates(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"serde", "serde"},
		{"Serde", "serde"},
		{"serde-json", "serde_json"},        // hyphen → underscore
		{"serde_json", "serde_json"},
		{"tokio-util", "tokio_util"},
		// normalizePackageName does crate name normalization only — :: stripping is in registryKey.
		// "serde::Deserialize" treated as a raw crate name → lowercase + hyphen→underscore.
		{"serde::Deserialize", "serde::deserialize"}, // :: preserved in normalizer (stripping done in registryKey)
		{"", ""},
	}
	for _, tc := range cases {
		got := normalizePackageName(regLangCrates, tc.input)
		if got != tc.want {
			t.Errorf("normalizePackageName(crates, %q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestNormalizePackageName_NPM(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"React", "react"},
		{"@types/node", "@types/node"},
		{"@angular/core", "@angular/core"},
		{"express", "express"},
		// Sub-path export stripping: "lodash/fp" → "lodash", "@mui/material/Button" → "@mui/material"
		{"lodash/fp", "lodash"},             // unscoped sub-path
		{"date-fns/format", "date-fns"},     // sub-path export
		{"@mui/material/Button", "@mui/material"}, // scoped sub-path
		{"@radix-ui/react-dialog/Modal", "@radix-ui/react-dialog"}, // scoped sub-path
		{"", ""},
	}
	for _, tc := range cases {
		got := normalizePackageName(regLangNPM, tc.input)
		if got != tc.want {
			t.Errorf("normalizePackageName(npm, %q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestNormalizePackageName_Go(t *testing.T) {
	// Go module paths are case-sensitive and preserved as-is.
	cases := []struct {
		input, want string
	}{
		{"github.com/gin-gonic/gin", "github.com/gin-gonic/gin"},
		{"github.com/go-chi/chi/v5", "github.com/go-chi/chi/v5"},
		{"golang.org/x/crypto", "golang.org/x/crypto"},
		{"", ""},
	}
	for _, tc := range cases {
		got := normalizePackageName(regLangGo, tc.input)
		if got != tc.want {
			t.Errorf("normalizePackageName(go, %q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// isGoStdlib
// ──────────────────────────────────────────────────────────────────────────────

func TestIsGoStdlib(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"fmt", true},
		{"net/http", true},
		{"encoding/json", true},
		{"crypto/tls", true},
		{"io/fs", true},
		{"github.com/foo/bar", false},
		{"golang.org/x/crypto", false}, // golang.org has a dot
		{"gopkg.in/yaml.v2", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isGoStdlib(tc.path)
		if got != tc.want {
			t.Errorf("isGoStdlib(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// isRustStdlib
// ──────────────────────────────────────────────────────────────────────────────

func TestIsRustStdlib(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"std", true},
		{"std::collections", true},
		{"std::io", true},
		{"core", true},
		{"core::mem", true},
		{"alloc", true},
		{"alloc::vec", true},
		{"proc_macro", true},
		{"serde", false},
		{"tokio", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isRustStdlib(tc.path)
		if got != tc.want {
			t.Errorf("isRustStdlib(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// isPythonStdlib
// ──────────────────────────────────────────────────────────────────────────────

func TestIsPythonStdlib(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"os", true},
		{"os.path", true},    // dotted: root "os" is stdlib
		{"sys", true},
		{"json", true},
		{"typing", true},
		{"asyncio", true},
		{"collections", true},
		{"flask", false},
		{"numpy", false},
		{"requests", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isPythonStdlib(tc.path)
		if got != tc.want {
			t.Errorf("isPythonStdlib(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// isLocalImport
// ──────────────────────────────────────────────────────────────────────────────

func TestIsLocalImport(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"./utils", true},
		{"../shared", true},
		{".hidden", true},
		{"/absolute/path", true},
		{"express", false},
		{"github.com/foo/bar", false},
		{"flask", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isLocalImport(tc.path)
		if got != tc.want {
			t.Errorf("isLocalImport(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// isNodeBuiltin
// ──────────────────────────────────────────────────────────────────────────────

func TestIsNodeBuiltin(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"fs", true},
		{"path", true},
		{"crypto", true},
		{"node:fs", true},
		{"node:path", true},
		{"node:crypto/web", true},
		{"express", false},
		{"lodash", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isNodeBuiltin(tc.path)
		if got != tc.want {
			t.Errorf("isNodeBuiltin(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// goImportKnownByPrefix
// ──────────────────────────────────────────────────────────────────────────────

func TestGoImportKnownByPrefix(t *testing.T) {
	pkgs := map[string]struct{}{
		"github.com/go-chi/chi/v5": {},
		"github.com/gin-gonic/gin": {},
	}
	cases := []struct {
		imp  string
		want bool
	}{
		// Direct match
		{"github.com/go-chi/chi/v5", true},
		// Sub-package match
		{"github.com/go-chi/chi/v5/middleware", true},
		{"github.com/go-chi/chi/v5/render", true},
		// Module not in registry
		{"github.com/go-chi/chi/v6", false},
		{"github.com/gorilla/mux", false},
		// Too short (fewer than 3 components)
		{"github.com", false},
		{"github.com/foo", false},
	}
	for _, tc := range cases {
		got := goImportKnownByPrefix(pkgs, tc.imp)
		if got != tc.want {
			t.Errorf("goImportKnownByPrefix(%q) = %v, want %v", tc.imp, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// goModuleBase
// ──────────────────────────────────────────────────────────────────────────────

func TestGoModuleBase(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"github.com/go-chi/chi/v5", "chi"},
		{"github.com/gin-gonic/gin", "gin"},
		{"github.com/gorilla/mux", "mux"},
		{"golang.org/x/crypto", "crypto"},
		{"golang.org/x/net", "net"},
		{"github.com/pkg/errors", "errors"},
		// No version suffix
		{"github.com/foo/bar", "bar"},
		// Single component
		{"fmt", "fmt"},
	}
	for _, tc := range cases {
		got := goModuleBase(tc.input)
		if got != tc.want {
			t.Errorf("goModuleBase(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// PackageRegistry.IsKnown
// ──────────────────────────────────────────────────────────────────────────────

func buildTestRegistry(t *testing.T) *PackageRegistry {
	t.Helper()
	r := NewPackageRegistry()
	if err := r.AddPackages(regLangNPM, []byte("express\nlodash\n@types/node\n")); err != nil {
		t.Fatalf("AddPackages npm: %v", err)
	}
	if err := r.AddPackages(regLangPyPI, []byte("flask\nflask-cors\nnumpy\nrequests\n")); err != nil {
		t.Fatalf("AddPackages pypi: %v", err)
	}
	if err := r.AddPackages(regLangCrates, []byte("serde\nserde_json\ntokio\n")); err != nil {
		t.Fatalf("AddPackages crates: %v", err)
	}
	if err := r.AddPackages(regLangGo, []byte("github.com/gin-gonic/gin\ngithub.com/go-chi/chi/v5\ngolang.org/x/crypto\n")); err != nil {
		t.Fatalf("AddPackages go: %v", err)
	}
	return r
}

func TestPackageRegistry_IsKnown_NPM(t *testing.T) {
	r := buildTestRegistry(t)

	cases := []struct {
		lang, imp string
		want      bool
	}{
		// Known
		{"typescript", "express", true},
		{"javascript", "lodash", true},
		{"typescript", "@types/node", true},
		// Unknown
		{"typescript", "expres", false},   // typo
		{"typescript", "expresss", false}, // double s
		{"javascript", "flaskk", false},   // wrong language doesn't matter here
		// Local / stdlib (always known)
		{"typescript", "./utils", true},
		{"typescript", "fs", true},        // node builtin
		{"typescript", "path", true},
		{"typescript", "node:fs", true},
		// Sub-path exports: "lodash/fp" resolves to "lodash" which IS known
		{"typescript", "lodash/fp", true},
		{"typescript", "date-fns/format", false}, // date-fns not in test registry
		// Scoped sub-path: "@types/node/fs" resolves to "@types/node" which IS known
		{"typescript", "@types/node/fs", true},
	}
	for _, tc := range cases {
		got := r.IsKnown(tc.lang, tc.imp)
		if got != tc.want {
			t.Errorf("IsKnown(%q, %q) = %v, want %v", tc.lang, tc.imp, got, tc.want)
		}
	}
}

func TestPackageRegistry_IsKnown_PyPI(t *testing.T) {
	r := buildTestRegistry(t)

	cases := []struct {
		imp  string
		want bool
	}{
		{"flask", true},
		{"flask_cors", true},           // underscore normalizes to flask-cors
		{"Flask-Cors", true},           // case normalizes to flask-cors
		{"numpy", true},
		{"os", true},                   // stdlib
		{"sys", true},                  // stdlib
		{"json", true},                 // stdlib
		{"flask-corse", false},         // hallucinated (typo)
		{"numpi", false},               // hallucinated
		{"./local", true},              // local import
		// Sub-module imports: "requests.adapters" → root is "requests" which IS known
		{"requests.adapters", true},    // sub-module of known package
		{"flask.views", true},          // sub-module of known package
		{"os.path", true},              // stdlib sub-module (os is stdlib)
	}
	for _, tc := range cases {
		got := r.IsKnown("python", tc.imp)
		if got != tc.want {
			t.Errorf("IsKnown(python, %q) = %v, want %v", tc.imp, got, tc.want)
		}
	}
}

func TestPackageRegistry_IsKnown_Crates(t *testing.T) {
	r := buildTestRegistry(t)

	cases := []struct {
		imp  string
		want bool
	}{
		{"serde", true},
		{"serde_json", true},
		{"serde-json", true},           // hyphen normalizes to serde_json
		{"tokio", true},
		{"std", true},                  // stdlib
		{"std::io", true},              // stdlib
		{"core::mem", true},            // stdlib
		{"serdes", false},              // hallucinated
		{"tokioo", false},              // hallucinated
		// Module path stripping: "serde::Deserialize" → crate is "serde" which IS known
		{"serde::Deserialize", true},   // module path stripped to crate name
		{"tokio::io::AsyncRead", true}, // nested module path
		{"serde_json::Value", true},    // serde_json with module path
	}
	for _, tc := range cases {
		got := r.IsKnown("rust", tc.imp)
		if got != tc.want {
			t.Errorf("IsKnown(rust, %q) = %v, want %v", tc.imp, got, tc.want)
		}
	}
}

func TestPackageRegistry_IsKnown_Go(t *testing.T) {
	r := buildTestRegistry(t)

	cases := []struct {
		imp  string
		want bool
	}{
		// Stdlib always known
		{"fmt", true},
		{"net/http", true},
		{"encoding/json", true},
		// Direct module match
		{"github.com/gin-gonic/gin", true},
		{"github.com/go-chi/chi/v5", true},
		// Sub-package match via prefix
		{"github.com/go-chi/chi/v5/middleware", true},
		{"github.com/gin-gonic/gin/render", true},
		// Unknown modules
		{"github.com/gin-gonic/gin/v2", false},  // wrong version path
		{"github.com/gorilla/mux", false},
		// Local import
		{"./internal/foo", true},
	}
	for _, tc := range cases {
		got := r.IsKnown("go", tc.imp)
		if got != tc.want {
			t.Errorf("IsKnown(go, %q) = %v, want %v", tc.imp, got, tc.want)
		}
	}
}

func TestPackageRegistry_IsKnown_UnsupportedLang(t *testing.T) {
	r := buildTestRegistry(t)
	// Java, Ruby, PHP etc. → always known (no registry data)
	if !r.IsKnown("java", "org.springframework.boot") {
		t.Error("IsKnown(java, ...) should return true (unsupported lang)")
	}
	if !r.IsKnown("ruby", "rails") {
		t.Error("IsKnown(ruby, ...) should return true (unsupported lang)")
	}
}

func TestPackageRegistry_IsKnown_NilRegistry(t *testing.T) {
	var r *PackageRegistry
	// nil registry: everything is known
	if !r.IsKnown("python", "flask") {
		t.Error("nil registry should return IsKnown=true")
	}
	if !r.IsKnown("python", "some-fake-package") {
		t.Error("nil registry should return IsKnown=true even for unknown packages")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// PackageRegistry.Suggest
// ──────────────────────────────────────────────────────────────────────────────

func TestPackageRegistry_Suggest_PyPI(t *testing.T) {
	r := NewPackageRegistry()
	_ = r.AddPackages(regLangPyPI, []byte("flask\nflask-cors\nnumpy\nrequests\nscikit-learn\n"))

	cases := []struct {
		imp  string
		want string // expected suggestion (or "" for no suggestion)
	}{
		{"flask-corse", "flask-cors"}, // 1 edit: extra 'e'
		{"reqests", "requests"},       // 1 edit: missing 'u'
		{"numpyy", "numpy"},           // 1 edit: extra 'y'
		// Too different — no suggestion
		{"completelydifferent", ""},
		// Exact match (shouldn't be called in practice but handle gracefully)
		{"flask", "flask"},
	}
	for _, tc := range cases {
		got := r.Suggest("python", tc.imp)
		if got != tc.want {
			t.Errorf("Suggest(python, %q) = %q, want %q", tc.imp, got, tc.want)
		}
	}
}

func TestPackageRegistry_Suggest_NPM(t *testing.T) {
	r := NewPackageRegistry()
	_ = r.AddPackages(regLangNPM, []byte("express\nlodash\naxios\n"))

	cases := []struct {
		imp  string
		want string
	}{
		{"expres", "express"},  // 1 edit
		{"axois", "axios"},     // 1 edit (swap)
		{"loddash", "lodash"},  // 1 edit (extra d)
	}
	for _, tc := range cases {
		got := r.Suggest("typescript", tc.imp)
		if got != tc.want {
			t.Errorf("Suggest(typescript, %q) = %q, want %q", tc.imp, got, tc.want)
		}
	}
}

func TestPackageRegistry_Suggest_NilRegistry(t *testing.T) {
	var r *PackageRegistry
	got := r.Suggest("python", "flask-corse")
	if got != "" {
		t.Errorf("nil registry Suggest should return empty, got %q", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// PackageRegistry.AddPackages — dedup and loading
// ──────────────────────────────────────────────────────────────────────────────

func TestAddPackages_DedupAndComments(t *testing.T) {
	r := NewPackageRegistry()
	data := []byte(`# this is a comment
flask
flask
# another comment

requests
`)
	if err := r.AddPackages(regLangPyPI, data); err != nil {
		t.Fatalf("AddPackages: %v", err)
	}
	// flask appears twice but should only be counted once
	if r.Size() != 2 {
		t.Errorf("Size() = %d, want 2 (flask + requests deduplicated)", r.Size())
	}
}

func TestPackageRegistry_Size(t *testing.T) {
	r := buildTestRegistry(t)
	if r.Size() == 0 {
		t.Error("Size() should be > 0 after adding packages")
	}
}

func TestPackageRegistry_LoadedAt(t *testing.T) {
	before := time.Now()
	r := NewPackageRegistry()
	after := time.Now()
	if r.LoadedAt().Before(before) || r.LoadedAt().After(after) {
		t.Errorf("LoadedAt() = %v, expected between %v and %v", r.LoadedAt(), before, after)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// LoadBuiltinRegistry
// ──────────────────────────────────────────────────────────────────────────────

func TestLoadBuiltinRegistry(t *testing.T) {
	r, err := LoadBuiltinRegistry()
	if err != nil {
		t.Fatalf("LoadBuiltinRegistry: %v", err)
	}
	if r == nil {
		t.Fatal("LoadBuiltinRegistry returned nil")
	}
	if r.Size() == 0 {
		t.Error("LoadBuiltinRegistry returned empty registry — no packages loaded")
	}

	// Spot-check well-known packages
	type check struct {
		lang, pkg string
	}
	wellKnown := []check{
		{"typescript", "express"},
		{"typescript", "react"},
		{"typescript", "lodash"},
		{"typescript", "@types/node"},
		{"python", "flask"},
		{"python", "numpy"},
		{"python", "requests"},
		{"python", "django"},
		{"rust", "serde"},
		{"rust", "tokio"},
		{"go", "github.com/gin-gonic/gin"},
		{"go", "github.com/go-chi/chi/v5"},
		// Java — spot-check a few well-known libraries
		{"java", "org.springframework.boot.SpringApplication"},
		{"java", "com.fasterxml.jackson.databind.ObjectMapper"},
		{"java", "lombok.Data"},
		{"java", "jakarta.persistence.Entity"},
	}
	for _, c := range wellKnown {
		if !r.IsKnown(c.lang, c.pkg) {
			t.Errorf("LoadBuiltinRegistry: IsKnown(%q, %q) = false, want true", c.lang, c.pkg)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// isVendoredPath
// ──────────────────────────────────────────────────────────────────────────────

func TestIsVendoredPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/project/vendor/github.com/foo/bar.go", true},
		{"/project/node_modules/express/index.js", true},
		{"/project/.gen/proto.go", true},
		{"/project/generated/api.go", true},
		{"vendor/foo/bar.go", true},
		{"node_modules/react/index.js", true},
		{"/project/handler.go", false},
		{"/project/cmd/main.go", false},
		{"/project/internal/foo/bar.go", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isVendoredPath(tc.path)
		if got != tc.want {
			t.Errorf("isVendoredPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// isJavaStdlib
// ──────────────────────────────────────────────────────────────────────────────

func TestIsJavaStdlib(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Core JDK packages — always known
		{"java.lang.String", true},
		{"java.util.List", true},
		{"java.io.File", true},
		{"java.net.URL", true},
		{"java.nio.file.Path", true},
		{"java.time.LocalDate", true},
		{"java.math.BigDecimal", true},
		{"java.security.MessageDigest", true},
		{"java.sql.Connection", true},
		// javax.* — JDK internal (crypto, naming, etc.)
		{"javax.crypto.Cipher", true},
		{"javax.naming.Context", true},
		{"javax.net.ssl.SSLContext", true},
		// sun.* and com.sun.* — JDK internal
		{"sun.misc.Unsafe", true},
		{"com.sun.net.httpserver.HttpServer", true},
		{"com.sun.jndi.ldap.LdapClient", true},
		// jdk.* — Java 9+ internal modules
		{"jdk.internal.misc.Unsafe", true},
		// W3C DOM and SAX — shipped with JDK
		{"org.w3c.dom.Document", true},
		{"org.w3c.dom.Element", true},
		{"org.xml.sax.SAXParser", true},
		{"org.xml.sax.helpers.DefaultHandler", true},
		{"org.ietf.jgss.GSSContext", true},
		// Third-party packages — NOT stdlib
		{"org.springframework.boot.SpringApplication", false},
		{"com.fasterxml.jackson.databind.ObjectMapper", false},
		{"jakarta.persistence.Entity", false}, // Jakarta EE is NOT JDK stdlib
		{"javax.persistence.Entity", false},   // old JEE javax.persistence (not JDK)
		{"javax.servlet.http.HttpServlet", false}, // old JEE javax.servlet (not JDK)
		{"org.hibernate.Session", false},
		{"lombok.Data", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isJavaStdlib(tc.path)
		if got != tc.want {
			t.Errorf("isJavaStdlib(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// javaImportKnownByPrefix
// ──────────────────────────────────────────────────────────────────────────────

func TestJavaImportKnownByPrefix(t *testing.T) {
	pkgs := map[string]struct{}{
		"org.springframework":      {},
		"com.fasterxml.jackson":    {},
		"org.hibernate":            {},
		"lombok":                   {},
	}
	cases := []struct {
		imp  string
		want bool
	}{
		// Exact match
		{"org.springframework", true},
		{"lombok", true},
		// Prefix match: import extends a known prefix
		{"org.springframework.boot.SpringApplication", true},
		{"org.springframework.security.core.Authentication", true},
		{"com.fasterxml.jackson.databind.ObjectMapper", true},
		{"com.fasterxml.jackson.core.JsonParser", true},
		{"org.hibernate.Session", true},
		{"lombok.Data", true},
		{"lombok.extern.slf4j.Slf4j", true},
		// NOT a prefix match: "org.spring" is NOT a prefix of "org.springframework"
		{"org.spring", false},
		// Completely unknown
		{"com.example.MyClass", false},
		{"io.unknown.Library", false},
		// Dot-boundary matters: "org.springfr" is NOT a prefix of "org.springframework"
		{"org.springfr", false},
	}
	for _, tc := range cases {
		got := javaImportKnownByPrefix(pkgs, tc.imp)
		if got != tc.want {
			t.Errorf("javaImportKnownByPrefix(%q) = %v, want %v", tc.imp, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// javaImportPrefix
// ──────────────────────────────────────────────────────────────────────────────

func TestJavaImportPrefix(t *testing.T) {
	cases := []struct {
		imp   string
		depth int
		want  string
	}{
		// Normal cases
		{"com.google.guava2.collection.ImmutableList", 3, "com.google.guava2"},
		{"org.springframework.boot.App", 2, "org.springframework"},
		{"org.springframework.boot.App", 1, "org"},
		{"com.fasterxml.jackson.databind.ObjectMapper", 3, "com.fasterxml.jackson"},
		// Fewer components than depth: return full import
		{"lombok.Data", 5, "lombok.Data"},
		{"lombok", 3, "lombok"},
		// Exact depth
		{"org.springframework", 2, "org.springframework"},
		// Single component
		{"lombok", 1, "lombok"},
		{"lombok.Data", 1, "lombok"},
		// Depth 0
		{"com.example.Foo", 0, "com.example.Foo"},
		// Empty
		{"", 2, ""},
	}
	for _, tc := range cases {
		got := javaImportPrefix(tc.imp, tc.depth)
		if got != tc.want {
			t.Errorf("javaImportPrefix(%q, %d) = %q, want %q", tc.imp, tc.depth, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// PackageRegistry.IsKnown — Java
// ──────────────────────────────────────────────────────────────────────────────

func buildJavaTestRegistry(t *testing.T) *PackageRegistry {
	t.Helper()
	r := NewPackageRegistry()
	data := []byte(`
org.springframework
com.fasterxml.jackson
org.hibernate
lombok
com.google.guava
org.junit
io.netty
jakarta
`)
	if err := r.AddPackages(regLangJava, data); err != nil {
		t.Fatalf("AddPackages java: %v", err)
	}
	return r
}

func TestPackageRegistry_IsKnown_Java(t *testing.T) {
	r := buildJavaTestRegistry(t)

	cases := []struct {
		imp  string
		want bool
	}{
		// JDK stdlib — always known
		{"java.lang.String", true},
		{"java.util.ArrayList", true},
		{"javax.crypto.Cipher", true},
		{"sun.misc.Unsafe", true},
		{"org.w3c.dom.Document", true},
		// Exact match to registry entry
		{"org.springframework", true},
		{"lombok", true},
		// Prefix match: import extends a known registry entry
		{"org.springframework.boot.SpringApplication", true},
		{"org.springframework.security.core.Authentication", true},
		{"com.fasterxml.jackson.databind.ObjectMapper", true},
		{"org.hibernate.Session", true},
		{"lombok.Data", true},
		{"lombok.extern.slf4j.Slf4j", true},
		{"com.google.guava.collect.ImmutableList", true},
		{"jakarta.persistence.Entity", true},
		{"jakarta.servlet.http.HttpServlet", true},
		// Unknown packages — not in registry, not stdlib
		{"com.example.custom.MyClass", false},
		{"io.unknown.Library", false},
		{"net.fakecompany.FakeClass", false},
		// Slopsquatting attempts — unknown
		{"com.fasterxml.jakson.databind.ObjectMapper", false}, // typo: jakson
		{"com.google.guava2.collect.ImmutableList", false},    // typo: guava2
		// Local import
		{"./local.package", true},
	}
	for _, tc := range cases {
		got := r.IsKnown("java", tc.imp)
		if got != tc.want {
			t.Errorf("IsKnown(java, %q) = %v, want %v", tc.imp, got, tc.want)
		}
	}
}

func TestPackageRegistry_IsKnown_Java_SafeFallback(t *testing.T) {
	// When no Java packages are registered, IsKnown returns true (safe fallback).
	r := NewPackageRegistry()
	// No Java packages added.
	if !r.IsKnown("java", "org.springframework.boot") {
		t.Error("IsKnown(java, ...) should return true when no Java registry data loaded (safe fallback)")
	}
	if !r.IsKnown("java", "com.totally.fake.Package") {
		t.Error("IsKnown(java, ...) should return true when no Java registry data loaded (safe fallback)")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// PackageRegistry.Suggest — Java
// ──────────────────────────────────────────────────────────────────────────────

func TestPackageRegistry_Suggest_Java(t *testing.T) {
	r := buildJavaTestRegistry(t)

	cases := []struct {
		imp  string
		want string // "" means no suggestion expected
	}{
		// One edit in the 3rd component: "guava2" → "guava"
		{"com.google.guava2.collect.ImmutableList", "com.google.guava"},
		// One edit in the 2nd component: "fasterxml.jakson" → "fasterxml.jackson"
		{"com.fasterxml.jakson.databind.ObjectMapper", "com.fasterxml.jackson"},
		// Completely different org: too far for suggestion (com.* vs org.*)
		// First-byte filter ("c" vs "o") prevents cross-org suggestions.
		{"com.springsecurity.core.UserDetails", ""},
		// No close match: completely made up
		{"xyz.unknown.package.Class", ""},
	}
	for _, tc := range cases {
		got := r.Suggest("java", tc.imp)
		if got != tc.want {
			t.Errorf("Suggest(java, %q) = %q, want %q", tc.imp, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// LoadBuiltinRegistry — Java spot-checks
// ──────────────────────────────────────────────────────────────────────────────

func TestLoadBuiltinRegistry_Java(t *testing.T) {
	r, err := LoadBuiltinRegistry()
	if err != nil {
		t.Fatalf("LoadBuiltinRegistry: %v", err)
	}

	wellKnown := []struct {
		imp  string
		want bool
	}{
		// Stdlib — always known
		{"java.util.List", true},
		{"javax.crypto.Cipher", true},
		// Known library prefixes from builtin registry
		{"org.springframework.boot.SpringApplication", true},
		{"com.fasterxml.jackson.databind.ObjectMapper", true},
		{"org.hibernate.Session", true},
		{"com.google.guava.collect.ImmutableList", true},
		{"io.netty.channel.ChannelHandler", true},
		{"org.junit.jupiter.api.Test", true},
		{"org.mockito.Mockito", true},
		{"lombok.Data", true},
		{"jakarta.persistence.Entity", true},
		{"org.slf4j.Logger", true},
		{"ch.qos.logback.classic.Logger", true},
		// Unknown — not in registry, not stdlib
		{"com.fasterxml.jakson.databind.ObjectMapper", false}, // typo
		{"com.google.guava2.collect.ImmutableList", false},    // typo
		{"io.unknown.fake.Library", false},
	}
	for _, c := range wellKnown {
		got := r.IsKnown("java", c.imp)
		if got != c.want {
			t.Errorf("LoadBuiltinRegistry Java: IsKnown(java, %q) = %v, want %v", c.imp, got, c.want)
		}
	}
}
