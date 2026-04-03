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
		{"Flask.Cors", "flask-cors"},     // dot → hyphen
		{"scikit_learn", "scikit-learn"},
		{"Pillow", "pillow"},
		{"flask__cors", "flask-cors"},    // consecutive underscores collapsed (PEP 503)
		{"flask--cors", "flask-cors"},    // consecutive hyphens collapsed
		{"flask_.cors", "flask-cors"},    // mixed separators collapsed
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
		{"serde-json", "serde_json"},    // hyphen → underscore
		{"serde_json", "serde_json"},
		{"tokio-util", "tokio_util"},
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
		{"flask_cors", true},       // underscore normalizes to flask-cors
		{"Flask-Cors", true},       // case normalizes to flask-cors
		{"numpy", true},
		{"os", true},               // stdlib
		{"sys", true},              // stdlib
		{"json", true},             // stdlib
		{"flask-corse", false},     // hallucinated (typo)
		{"numpi", false},           // hallucinated
		{"./local", true},          // local import
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
		{"serde-json", true},       // hyphen normalizes to serde_json
		{"tokio", true},
		{"std", true},              // stdlib
		{"std::io", true},          // stdlib
		{"core::mem", true},        // stdlib
		{"serdes", false},          // hallucinated
		{"tokioo", false},          // hallucinated
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
