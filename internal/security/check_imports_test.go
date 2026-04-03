package security

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ──────────────────────────────────────────────────────────────────────────────
// Test helpers
// ──────────────────────────────────────────────────────────────────────────────

func buildRegistryEngine(t *testing.T) *Engine {
	t.Helper()
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	r := NewPackageRegistry()
	_ = r.AddPackages(regLangNPM, []byte("express\nlodash\naxios\n@types/node\n"))
	_ = r.AddPackages(regLangPyPI, []byte("flask\nflask-cors\nnumpy\nrequests\ndjango\nfastapi\n"))
	_ = r.AddPackages(regLangCrates, []byte("serde\nserde_json\ntokio\nactix_web\n"))
	_ = r.AddPackages(regLangGo, []byte("github.com/gin-gonic/gin\ngithub.com/go-chi/chi/v5\ngolang.org/x/crypto\n"))
	return NewEngine(ps).WithRegistry(r)
}

// ──────────────────────────────────────────────────────────────────────────────
// Engine.WithRegistry
// ──────────────────────────────────────────────────────────────────────────────

func TestWithRegistry_ReturnsNewEngine(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	original := NewEngine(ps)
	r := NewPackageRegistry()
	withReg := original.WithRegistry(r)

	if original.registry != nil {
		t.Error("WithRegistry modified original Engine (should be immutable)")
	}
	if withReg.registry != r {
		t.Error("WithRegistry did not attach registry to new Engine")
	}
	if withReg.patterns != original.patterns {
		t.Error("WithRegistry should preserve patterns from original Engine")
	}
}

func TestWithRegistry_NilRegistry(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	e := NewEngine(ps).WithRegistry(nil)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/app.py", "flask-corse") // unknown package
	// nil registry → no violations
	violations := e.CheckImports(g, "/project/app.py")
	if len(violations) != 0 {
		t.Errorf("nil registry: expected 0 violations, got %d", len(violations))
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Engine.CheckImports — no violations expected
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckImports_KnownPackages_NoViolation(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/routes.py", "flask", "flask_cors", "numpy")

	violations := e.CheckImports(g, "/project/routes.py")
	if len(violations) != 0 {
		t.Errorf("expected no violations for known packages, got: %v", violations)
	}
}

func TestCheckImports_StdlibImports_NoViolation(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	// Python stdlib + third-party known
	addFileWithImports(g, "/project/utils.py", "os", "sys", "json", "pathlib", "flask")

	violations := e.CheckImports(g, "/project/utils.py")
	if len(violations) != 0 {
		t.Errorf("stdlib imports should not fire: %v", violations)
	}
}

func TestCheckImports_GoStdlib_NoViolation(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/main.go", "fmt", "net/http", "encoding/json", "github.com/go-chi/chi/v5")

	violations := e.CheckImports(g, "/project/main.go")
	if len(violations) != 0 {
		t.Errorf("Go stdlib + known module: expected no violations, got: %v", violations)
	}
}

func TestCheckImports_NodeBuiltins_NoViolation(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/server.ts", "express", "fs", "path", "crypto", "node:fs")

	violations := e.CheckImports(g, "/project/server.ts")
	if len(violations) != 0 {
		t.Errorf("node builtins + known: expected no violations, got: %v", violations)
	}
}

func TestCheckImports_LocalImports_NoViolation(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/handler.ts", "./utils", "../shared/types", "express")

	violations := e.CheckImports(g, "/project/handler.ts")
	if len(violations) != 0 {
		t.Errorf("local imports should not fire: %v", violations)
	}
}

func TestCheckImports_VendoredFile_Skipped(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/vendor/github.com/foo/bar/bar.go", "flask-corse")

	violations := e.CheckImports(g, "/project/vendor/github.com/foo/bar/bar.go")
	if len(violations) != 0 {
		t.Errorf("vendor files should be skipped: %v", violations)
	}
}

func TestCheckImports_EmptyFile_NoViolation(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	// File with no imports
	g.AddNode(&graph.Node{
		ID:   g.MakeNodeID("/project/empty.py", "/project/empty.py"),
		Type: graph.NodeFile,
		Name: "/project/empty.py",
		File: "/project/empty.py",
	})

	violations := e.CheckImports(g, "/project/empty.py")
	if len(violations) != 0 {
		t.Errorf("empty file: expected no violations, got %d", len(violations))
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Engine.CheckImports — violations expected
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckImports_UnknownPyPI_WithSuggestion(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	// "flask-corse" is a typo of "flask-cors"
	addFileWithImports(g, "/project/app.py", "flask", "flask_corse")

	violations := e.CheckImports(g, "/project/app.py")
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation (flask_corse), got %d: %v", len(violations), violations)
	}
	v := violations[0]
	if v.PatternID != "unknown-package-pypi" {
		t.Errorf("PatternID = %q, want %q", v.PatternID, "unknown-package-pypi")
	}
	if v.Severity != SeverityHigh {
		t.Errorf("Severity = %q, want HIGH", v.Severity)
	}
	if v.Target != "flask_corse" {
		t.Errorf("Target = %q, want %q", v.Target, "flask_corse")
	}
	if !strings.Contains(v.Message, "flask-cors") {
		t.Errorf("Message should suggest flask-cors, got: %q", v.Message)
	}
	if !strings.Contains(strings.ToLower(v.Message), "did you mean") {
		t.Errorf("Message should contain 'did you mean' (case-insensitive), got: %q", v.Message)
	}
	if !strings.Contains(v.Message, "20%") {
		t.Errorf("Message should mention 20%% hallucination rate, got: %q", v.Message)
	}
}

func TestCheckImports_UnknownNPM_WithSuggestion(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	// "axois" is a typo of "axios"
	addFileWithImports(g, "/project/api.ts", "express", "axois")

	violations := e.CheckImports(g, "/project/api.ts")
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation (axois), got %d: %v", len(violations), violations)
	}
	v := violations[0]
	if v.PatternID != "unknown-package-npm" {
		t.Errorf("PatternID = %q, want %q", v.PatternID, "unknown-package-npm")
	}
	if v.Target != "axois" {
		t.Errorf("Target = %q, want %q", v.Target, "axois")
	}
	if !strings.Contains(v.Message, "axios") {
		t.Errorf("Message should suggest axios, got: %q", v.Message)
	}
}

func TestCheckImports_UnknownCrate_WithSuggestion(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	// "serdes" is a typo of "serde"
	addFileWithImports(g, "/project/main.rs", "serde", "serdes")

	violations := e.CheckImports(g, "/project/main.rs")
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation (serdes), got %d: %v", len(violations), violations)
	}
	v := violations[0]
	if v.PatternID != "unknown-package-crates.io" {
		t.Errorf("PatternID = %q, want %q", v.PatternID, "unknown-package-crates.io")
	}
	if !strings.Contains(v.Message, "serde") {
		t.Errorf("Message should suggest serde, got: %q", v.Message)
	}
}

func TestCheckImports_UnknownGoModule_NoSuggestion(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	// completely made-up module
	addFileWithImports(g, "/project/main.go", "fmt", "github.com/nonexistent/fakepackage")

	violations := e.CheckImports(g, "/project/main.go")
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %v", len(violations), violations)
	}
	v := violations[0]
	if v.PatternID != "unknown-package-go-modules" {
		t.Errorf("PatternID = %q, want %q", v.PatternID, "unknown-package-go-modules")
	}
	if v.Target != "github.com/nonexistent/fakepackage" {
		t.Errorf("Target = %q, want github.com/nonexistent/fakepackage", v.Target)
	}
}

func TestCheckImports_MultipleUnknown(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	// Two unknown packages in one file
	addFileWithImports(g, "/project/app.py", "flask", "flask_corse", "numpi")

	violations := e.CheckImports(g, "/project/app.py")
	if len(violations) != 2 {
		t.Fatalf("expected 2 violations, got %d: %v", len(violations), violations)
	}
	// Both should be PyPI violations
	for _, v := range violations {
		if v.PatternID != "unknown-package-pypi" {
			t.Errorf("expected unknown-package-pypi, got %q", v.PatternID)
		}
	}
}

func TestCheckImports_UnknownNPM_Hallucinated_NoSuggestion(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	// Completely hallucinated package with no close match
	addFileWithImports(g, "/project/app.ts", "express", "totally-made-up-pkg-xyz")

	violations := e.CheckImports(g, "/project/app.ts")
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(violations))
	}
	v := violations[0]
	if strings.Contains(v.Message, "did you mean") {
		t.Errorf("should not have a suggestion for a completely different name, got: %q", v.Message)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Engine.CheckImports — nil/zero cases
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckImports_NilEngine(t *testing.T) {
	var e *Engine
	g := buildTestGraph(t)
	violations := e.CheckImports(g, "/project/app.py")
	if violations != nil {
		t.Errorf("nil engine: expected nil, got %v", violations)
	}
}

func TestCheckImports_NilGraph(t *testing.T) {
	e := buildRegistryEngine(t)
	violations := e.CheckImports(nil, "/project/app.py")
	if violations != nil {
		t.Errorf("nil graph: expected nil, got %v", violations)
	}
}

func TestCheckImports_EmptyFilePath(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	violations := e.CheckImports(g, "")
	if violations != nil {
		t.Errorf("empty filePath: expected nil, got %v", violations)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Violation structure validation
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckImports_ViolationFields(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/service.py", "flask_corse")

	violations := e.CheckImports(g, "/project/service.py")
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(violations))
	}
	v := violations[0]

	// All required fields must be populated
	if v.PatternID == "" {
		t.Error("Violation.PatternID must not be empty")
	}
	if v.PatternName == "" {
		t.Error("Violation.PatternName must not be empty")
	}
	if v.Severity == "" {
		t.Error("Violation.Severity must not be empty")
	}
	if v.File != "/project/service.py" {
		t.Errorf("Violation.File = %q, want /project/service.py", v.File)
	}
	if v.Target == "" {
		t.Error("Violation.Target must not be empty")
	}
	if v.Message == "" {
		t.Error("Violation.Message must not be empty")
	}
	if v.Evidence == "" {
		t.Error("Violation.Evidence must not be empty")
	}

	// Tags must include supply-chain
	hasTag := false
	for _, tag := range v.Tags {
		if tag == "supply-chain" {
			hasTag = true
		}
	}
	if !hasTag {
		t.Errorf("Violation.Tags should include 'supply-chain', got %v", v.Tags)
	}

	// Severity must be HIGH (not CRITICAL — agents should verify, not be blocked)
	if v.Severity != SeverityHigh {
		t.Errorf("Violation.Severity = %q, want HIGH", v.Severity)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// DefaultEngineWithRegistry
// ──────────────────────────────────────────────────────────────────────────────

func TestDefaultEngineWithRegistry_NotNil(t *testing.T) {
	e := DefaultEngineWithRegistry()
	if e == nil {
		t.Fatal("DefaultEngineWithRegistry returned nil")
	}
	if e.registry == nil {
		t.Error("DefaultEngineWithRegistry: registry should be set")
	}
	if e.patterns == nil {
		t.Error("DefaultEngineWithRegistry: patterns should be set")
	}
}

func TestDefaultEngineWithRegistry_KnownPackageNoViolation(t *testing.T) {
	e := DefaultEngineWithRegistry()
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/app.py", "flask", "numpy", "requests")

	violations := e.CheckImports(g, "/project/app.py")
	if len(violations) != 0 {
		t.Errorf("well-known packages: expected 0 violations, got %d: %v", len(violations), violations)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// registryLangLabel
// ──────────────────────────────────────────────────────────────────────────────

func TestRegistryLangLabel(t *testing.T) {
	cases := []struct {
		lang, want string
	}{
		{"go", "Go modules"},
		{"Go", "Go modules"},
		{"typescript", "npm"},
		{"javascript", "npm"},
		{"python", "PyPI"},
		{"rust", "crates.io"},
		{"java", "Maven Central"},
		{"Java", "Maven Central"},
		{"ruby", "ruby"}, // unsupported: returns lang as-is
	}
	for _, tc := range cases {
		got := registryLangLabel(tc.lang)
		if got != tc.want {
			t.Errorf("registryLangLabel(%q) = %q, want %q", tc.lang, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Go sub-package known via prefix
// ──────────────────────────────────────────────────────────────────────────────

func TestCheckImports_GoSubPackageKnown(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	// chi/v5/middleware is known via prefix match on github.com/go-chi/chi/v5
	addFileWithImports(g, "/project/middleware.go",
		"github.com/go-chi/chi/v5/middleware",
		"github.com/go-chi/chi/v5/render",
		"net/http",
	)

	violations := e.CheckImports(g, "/project/middleware.go")
	if len(violations) != 0 {
		t.Errorf("chi sub-packages should be known via prefix: %v", violations)
	}
}

func TestCheckImports_PythonSubModule_NoViolation(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	// "requests.adapters" should be treated as the "requests" package
	// "flask.views" should be treated as "flask"
	// "os.path" is stdlib (os is in pythonStdlib)
	addFileWithImports(g, "/project/app.py", "requests.adapters", "flask.views", "os.path")

	violations := e.CheckImports(g, "/project/app.py")
	if len(violations) != 0 {
		t.Errorf("Python sub-module imports should resolve to root package: %v", violations)
	}
}

func TestCheckImports_RustModulePath_NoViolation(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	// "serde::Deserialize" should be treated as the "serde" crate
	// "tokio::io::AsyncRead" should be treated as "tokio"
	// "std::collections::HashMap" is stdlib
	addFileWithImports(g, "/project/main.rs", "serde::Deserialize", "tokio::io::AsyncRead", "std::collections::HashMap")

	violations := e.CheckImports(g, "/project/main.rs")
	if len(violations) != 0 {
		t.Errorf("Rust module paths should resolve to crate name: %v", violations)
	}
}

func TestCheckImports_NPMSubPath_NoViolation(t *testing.T) {
	e := buildRegistryEngine(t)
	g := buildTestGraph(t)
	// "lodash/fp" should resolve to "lodash"
	// "@types/node/fs" should resolve to "@types/node"
	addFileWithImports(g, "/project/util.ts", "lodash/fp", "@types/node/fs", "express")

	violations := e.CheckImports(g, "/project/util.ts")
	if len(violations) != 0 {
		t.Errorf("npm sub-path imports should resolve to package name: %v", violations)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Java — CheckImports tests
// ──────────────────────────────────────────────────────────────────────────────

// buildJavaRegistryEngine creates an Engine with a small Java registry for tests.
func buildJavaRegistryEngine(t *testing.T) *Engine {
	t.Helper()
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	r := NewPackageRegistry()
	// Include existing languages so cross-language tests work.
	_ = r.AddPackages(regLangNPM, []byte("express\n"))
	_ = r.AddPackages(regLangPyPI, []byte("flask\n"))
	// Java registry with curated prefixes.
	javaData := []byte(`
org.springframework
com.fasterxml.jackson
org.hibernate
lombok
com.google.guava
org.junit
io.netty
jakarta
org.slf4j
ch.qos.logback
org.mockito
`)
	_ = r.AddPackages(regLangJava, javaData)
	return NewEngine(ps).WithRegistry(r)
}

func TestCheckImports_Java_KnownPackages_NoViolation(t *testing.T) {
	e := buildJavaRegistryEngine(t)
	g := buildTestGraph(t)
	// Known packages: stdlib + known registry prefixes
	addFileWithImports(g, "/project/MyService.java",
		"java.util.List",                              // JDK stdlib
		"java.lang.String",                            // JDK stdlib
		"javax.crypto.Cipher",                         // JDK stdlib
		"org.springframework.boot.SpringApplication",  // prefix match: org.springframework
		"com.fasterxml.jackson.databind.ObjectMapper", // prefix match: com.fasterxml.jackson
		"lombok.Data",                                 // prefix match: lombok
		"jakarta.persistence.Entity",                  // prefix match: jakarta
		"org.slf4j.Logger",                            // prefix match: org.slf4j
	)

	violations := e.CheckImports(g, "/project/MyService.java")
	if len(violations) != 0 {
		t.Errorf("known Java imports: expected 0 violations, got %d: %v", len(violations), violations)
	}
}

func TestCheckImports_Java_JDKStdlib_NoViolation(t *testing.T) {
	e := buildJavaRegistryEngine(t)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/Utils.java",
		"java.util.ArrayList",
		"java.io.BufferedReader",
		"java.nio.file.Paths",
		"java.time.LocalDateTime",
		"javax.net.ssl.SSLContext",
		"sun.misc.Unsafe",
		"org.w3c.dom.Document",
		"org.xml.sax.SAXParser",
	)

	violations := e.CheckImports(g, "/project/Utils.java")
	if len(violations) != 0 {
		t.Errorf("JDK stdlib imports should not fire: %v", violations)
	}
}

func TestCheckImports_Java_UnknownPackage_WithSuggestion(t *testing.T) {
	e := buildJavaRegistryEngine(t)
	g := buildTestGraph(t)
	// "com.fasterxml.jakson" is a typo of "com.fasterxml.jackson"
	addFileWithImports(g, "/project/Config.java",
		"org.springframework.boot.SpringApplication",
		"com.fasterxml.jakson.databind.ObjectMapper", // typo: jakson
	)

	violations := e.CheckImports(g, "/project/Config.java")
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation (com.fasterxml.jakson.*), got %d: %v", len(violations), violations)
	}
	v := violations[0]
	if v.PatternID != "unknown-package-maven-central" {
		t.Errorf("PatternID = %q, want %q", v.PatternID, "unknown-package-maven-central")
	}
	if v.Severity != SeverityHigh {
		t.Errorf("Severity = %q, want HIGH", v.Severity)
	}
	if !strings.Contains(v.Message, "com.fasterxml.jackson") {
		t.Errorf("Message should suggest com.fasterxml.jackson, got: %q", v.Message)
	}
	if !strings.Contains(strings.ToLower(v.Message), "did you mean") {
		t.Errorf("Message should contain 'did you mean', got: %q", v.Message)
	}
	if !strings.Contains(v.Message, "Maven Central") {
		t.Errorf("Message should mention Maven Central, got: %q", v.Message)
	}
}

func TestCheckImports_Java_UnknownPackage_NoSuggestion(t *testing.T) {
	e := buildJavaRegistryEngine(t)
	g := buildTestGraph(t)
	// Completely hallucinated package
	addFileWithImports(g, "/project/Fake.java", "com.example.totallyhallucinatedpackage.FakeClass")

	violations := e.CheckImports(g, "/project/Fake.java")
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(violations))
	}
	v := violations[0]
	if v.PatternID != "unknown-package-maven-central" {
		t.Errorf("PatternID = %q, want %q", v.PatternID, "unknown-package-maven-central")
	}
	if strings.Contains(v.Message, "did you mean") {
		t.Errorf("should not have suggestion for completely different name, got: %q", v.Message)
	}
}

func TestCheckImports_Java_MixedKnownUnknown(t *testing.T) {
	e := buildJavaRegistryEngine(t)
	g := buildTestGraph(t)
	// Mix of stdlib, known, and unknown
	addFileWithImports(g, "/project/Service.java",
		"java.util.Map",                                  // known: JDK stdlib
		"org.springframework.stereotype.Service",         // known: registry prefix
		"com.google.guava2.collect.ImmutableList",        // unknown: typo (guava2 not in registry)
		"io.unknown.fake.Library",                        // unknown: no match
	)

	violations := e.CheckImports(g, "/project/Service.java")
	if len(violations) != 2 {
		t.Fatalf("expected 2 violations, got %d: %v", len(violations), violations)
	}
	for _, v := range violations {
		if v.PatternID != "unknown-package-maven-central" {
			t.Errorf("expected unknown-package-maven-central, got %q", v.PatternID)
		}
	}
}

func TestCheckImports_Java_SlopsquattingGuava(t *testing.T) {
	e := buildJavaRegistryEngine(t)
	g := buildTestGraph(t)
	// ROADMAP slopsquatting example: "com.google.guava2" instead of "com.google.guava"
	addFileWithImports(g, "/project/App.java", "com.google.guava2.collect.ImmutableList")

	violations := e.CheckImports(g, "/project/App.java")
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation for guava2, got %d", len(violations))
	}
	v := violations[0]
	// Should detect AND suggest the correct package
	if !strings.Contains(v.Message, "com.google.guava") {
		t.Errorf("Message should suggest com.google.guava, got: %q", v.Message)
	}
}

func TestCheckImports_Java_DotBoundaryExact(t *testing.T) {
	// "org.spring" must NOT match registry entry "org.springframework"
	// The dot-boundary rule: prefix must end at a dot, not in the middle of a component.
	e := buildJavaRegistryEngine(t)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/Bad.java", "org.spring.SomeClass")

	violations := e.CheckImports(g, "/project/Bad.java")
	if len(violations) != 1 {
		t.Fatalf("org.spring should NOT be known (not a dot-boundary prefix of org.springframework), got %d violations: %v", len(violations), violations)
	}
}

func TestCheckImports_Java_VendoredFile_Skipped(t *testing.T) {
	e := buildJavaRegistryEngine(t)
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/vendor/legacy/Old.java", "com.totally.fake.Package")

	violations := e.CheckImports(g, "/project/vendor/legacy/Old.java")
	if len(violations) != 0 {
		t.Errorf("vendor files should be skipped: %v", violations)
	}
}
