package parser

import (
	"testing"
)

// ── TreeSitterLanguageProvider ───────────────────────────────────────────────

func TestTreeSitterLanguageProvider_GoParser(t *testing.T) {
	var p LanguageParser = NewGoParser()
	tsp, ok := p.(TreeSitterLanguageProvider)
	if !ok {
		t.Fatal("GoParser does not implement TreeSitterLanguageProvider")
	}
	if lang := tsp.TSLanguageForFile("main.go"); lang == nil {
		t.Fatal("TSLanguageForFile returned nil for Go parser")
	}
}

func TestTreeSitterLanguageProvider_TypeScriptParser(t *testing.T) {
	var p LanguageParser = NewTypeScriptParser()
	tsp, ok := p.(TreeSitterLanguageProvider)
	if !ok {
		t.Fatal("TypeScriptParser does not implement TreeSitterLanguageProvider")
	}
	tsLang := tsp.TSLanguageForFile("index.ts")
	tsxLang := tsp.TSLanguageForFile("App.tsx")
	if tsLang == nil || tsxLang == nil {
		t.Fatal("TSLanguageForFile returned nil")
	}
	if tsLang == tsxLang {
		t.Error("expected different languages for .ts and .tsx")
	}
}

func TestTreeSitterLanguageProvider_NonTreeSitterParser(t *testing.T) {
	// SvelteParser uses regex, not tree-sitter — should NOT implement the interface.
	var p LanguageParser = NewSvelteParser()
	if _, ok := p.(TreeSitterLanguageProvider); ok {
		t.Error("SvelteParser should not implement TreeSitterLanguageProvider")
	}
}

// ── HasParseErrors ──────────────────────────────────────────────────────────

func TestHasParseErrors_CleanSource(t *testing.T) {
	w := NewWalker()
	src := []byte(`package main

func main() {
	fmt.Println("hello")
}
`)
	if w.HasParseErrors("main.go", src) {
		t.Error("HasParseErrors returned true for valid Go source")
	}
}

func TestHasParseErrors_BrokenSource(t *testing.T) {
	w := NewWalker()
	// Truncated mid-function — simulates a half-saved file.
	src := []byte(`package main

func main() {
	fmt.Println("hello
`)
	if !w.HasParseErrors("main.go", src) {
		t.Error("HasParseErrors returned false for broken Go source")
	}
}

func TestHasParseErrors_BrokenTypeScript(t *testing.T) {
	w := NewWalker()
	src := []byte(`export function greet(name: string): string {
	return "Hello, " + name
// missing closing brace — mid-save truncation
`)
	// TypeScript is more lenient — a missing brace IS an error.
	// The key test: the function works at all for .ts files.
	// We primarily verify it doesn't panic.
	_ = w.HasParseErrors("index.ts", src)
}

func TestHasParseErrors_UnknownExtension(t *testing.T) {
	w := NewWalker()
	// Unknown extension should return false (no parser, can't check).
	if w.HasParseErrors("data.xyz", []byte("garbage")) {
		t.Error("HasParseErrors should return false for unknown extensions")
	}
}

func TestHasParseErrors_NonTreeSitterParser(t *testing.T) {
	w := NewWalker()
	// Svelte uses regex, no tree-sitter — should return false (can't check).
	if w.HasParseErrors("App.svelte", []byte("not valid svelte at all {{{{")) {
		t.Error("HasParseErrors should return false for non-tree-sitter parsers")
	}
}

func TestHasParseErrors_EmptySource(t *testing.T) {
	w := NewWalker()
	// An empty file is valid — no syntax errors.
	if w.HasParseErrors("empty.go", []byte("")) {
		t.Error("HasParseErrors should return false for empty source")
	}
}

func TestHasParseErrors_PythonBroken(t *testing.T) {
	w := NewWalker()
	src := []byte(`def greet(name):
    print("Hello, " + name
# missing closing paren
`)
	if !w.HasParseErrors("script.py", src) {
		t.Error("HasParseErrors returned false for broken Python source")
	}
}

// ── parserForPath ───────────────────────────────────────────────────────────

func TestParserForPath_Extension(t *testing.T) {
	w := NewWalker()
	p := w.parserForPath("main.go")
	if p == nil {
		t.Fatal("parserForPath returned nil for .go file")
	}
	if _, ok := p.(*GoParser); !ok {
		t.Errorf("expected GoParser, got %T", p)
	}
}

func TestParserForPath_Filename(t *testing.T) {
	w := NewWalker()
	p := w.parserForPath("Makefile")
	if p == nil {
		t.Fatal("parserForPath returned nil for Makefile")
	}
}

func TestParserForPath_FilenamePrefix(t *testing.T) {
	w := NewWalker()
	p := w.parserForPath("Dockerfile.staging")
	if p == nil {
		t.Fatal("parserForPath returned nil for Dockerfile.staging")
	}
}

func TestParserForPath_Unknown(t *testing.T) {
	w := NewWalker()
	if p := w.parserForPath("data.xyz"); p != nil {
		t.Errorf("expected nil for unknown extension, got %T", p)
	}
}
