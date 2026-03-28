package parser_test

// Extended tests covering: Walker (WalkDir, IncrementalReindex, ParseFile,
// RegisterPlugin, shouldSkipDir via WalkDir), JavaScript, Python, generic,
// and plugin parsers.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ── JavaScript parser ─────────────────────────────────────────────────────────

func TestJavaScriptParser_Extensions(t *testing.T) {
	p := parser.NewJavaScriptParser()
	exts := p.Extensions()
	for _, want := range []string{".js", ".jsx"} {
		if !hasExtension(exts, want) {
			t.Errorf("expected extension %q in JavaScript parser", want)
		}
	}
}

func TestJavaScriptParser_Parse_BasicFunction(t *testing.T) {
	src := `
import { helper } from './helper.js';

function greet(name) {
  helper(name);
  return "Hello " + name;
}

const arrowFn = (x) => x * 2;

class Greeter {
  constructor(name) { this.name = name; }
  greet() { return greet(this.name); }
}

export default Greeter;
`
	g := graph.New("testrepo")
	p := parser.NewJavaScriptParser()
	if err := p.Parse(g, "src/greeter.js", []byte(src)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Error("expected nodes from JavaScript parse")
	}
}

func TestJavaScriptParser_Parse_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewJavaScriptParser(), ".js", "")
}

func TestJavaScriptParser_Parse_JSX(t *testing.T) {
	src := `
import React from 'react';

function Button({ label, onClick }) {
  return <button onClick={onClick}>{label}</button>;
}

export { Button };
`
	g := graph.New("testrepo")
	if err := parser.NewJavaScriptParser().Parse(g, "Button.jsx", []byte(src)); err != nil {
		t.Fatalf("JSX Parse: %v", err)
	}
}

// ── Python parser ──────────────────────────────────────────────────────────────

func TestPythonParser_Extensions(t *testing.T) {
	p := parser.NewPythonParser()
	exts := p.Extensions()
	for _, want := range []string{".py", ".pyi"} {
		if !hasExtension(exts, want) {
			t.Errorf("expected extension %q in Python parser", want)
		}
	}
}

func TestPythonParser_Parse_BasicFunction(t *testing.T) {
	src := `
import os
from pathlib import Path

def greet(name: str) -> str:
    """Return a greeting."""
    return f"Hello {name}"

class Greeter:
    def __init__(self, name: str) -> None:
        self.name = name

    def greet(self) -> str:
        return greet(self.name)
`
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "greeter.py", []byte(src)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Error("expected nodes from Python parse")
	}
}

func TestPythonParser_Parse_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewPythonParser(), ".py", "")
}

func TestPythonParser_Parse_WithCallSites(t *testing.T) {
	src := `
import utils

def process(data):
    result = utils.transform(data)
    return result
`
	g := graph.New("testrepo")
	if err := parser.NewPythonParser().Parse(g, "proc.py", []byte(src)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

// ── Walker.WalkDir ─────────────────────────────────────────────────────────────

func TestWalker_WalkDir_GoFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New("testrepo")
	g.SetRoot(dir)
	w := parser.NewWalker()
	mtimes, err := w.WalkDir(g, dir)
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	if len(mtimes) == 0 {
		t.Error("expected non-empty mtime map from WalkDir")
	}
	if !hasNode(g, "main") {
		t.Error("expected 'main' function node after WalkDir")
	}
}

func TestWalker_WalkDir_MultipleLanguages(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"main.go": "package main\nfunc Run() {}\n",
		"util.py": "def helper(): pass\n",
		"app.js":  "function init() {}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	g := graph.New("testrepo")
	w := parser.NewWalker()
	_, err := w.WalkDir(g, dir)
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
}

func TestWalker_WalkDir_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("testrepo")
	w := parser.NewWalker()
	mtimes, err := w.WalkDir(g, dir)
	if err != nil {
		t.Fatalf("WalkDir on empty dir: %v", err)
	}
	if len(mtimes) != 0 {
		t.Errorf("expected empty mtime map for empty dir, got %d entries", len(mtimes))
	}
}

func TestWalker_WalkDir_SkipsHiddenDirs(t *testing.T) {
	dir := t.TempDir()
	// Hidden dir should be skipped.
	hiddenDir := filepath.Join(dir, ".hidden")
	if err := os.MkdirAll(hiddenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hiddenDir, "secret.go"),
		[]byte("package secret\nfunc Hidden() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Visible file should still be parsed.
	if err := os.WriteFile(filepath.Join(dir, "visible.go"),
		[]byte("package main\nfunc Visible() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New("testrepo")
	w := parser.NewWalker()
	if _, err := w.WalkDir(g, dir); err != nil {
		t.Fatalf("WalkDir: %v", err)
	}

	if hasNode(g, "Hidden") {
		t.Error("expected 'Hidden' to be skipped (in .hidden dir)")
	}
	if !hasNode(g, "Visible") {
		t.Error("expected 'Visible' to be present")
	}
}

func TestWalker_WalkDir_SkipsVendorDir(t *testing.T) {
	dir := t.TempDir()
	vendorDir := filepath.Join(dir, "vendor")
	if err := os.MkdirAll(vendorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vendorDir, "dep.go"),
		[]byte("package dep\nfunc VendorFunc() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New("testrepo")
	w := parser.NewWalker()
	if _, err := w.WalkDir(g, dir); err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	if hasNode(g, "VendorFunc") {
		t.Error("expected VendorFunc to be skipped (in vendor dir)")
	}
}

// ── Walker.ParseFile ───────────────────────────────────────────────────────────

func TestWalker_ParseFile_GoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "svc.go")
	if err := os.WriteFile(path, []byte("package svc\n\nfunc Serve() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New("testrepo")
	w := parser.NewWalker()
	if err := w.ParseFile(g, path); err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if !hasNode(g, "Serve") {
		t.Error("expected 'Serve' node after ParseFile")
	}
}

func TestWalker_ParseFile_UnsupportedExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(path, []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New("testrepo")
	w := parser.NewWalker()
	// No parser for .bin → should return nil silently.
	if err := w.ParseFile(g, path); err != nil {
		t.Fatalf("ParseFile on unsupported ext: %v", err)
	}
}

func TestWalker_ParseFile_NonExistentFile(t *testing.T) {
	g := graph.New("testrepo")
	w := parser.NewWalker()
	err := w.ParseFile(g, "/nonexistent/path/svc.go")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

// ── Walker.IncrementalReindex ──────────────────────────────────────────────────

func TestWalker_IncrementalReindex_AllNew(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "svc.go"),
		[]byte("package svc\nfunc Run() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New("testrepo")
	w := parser.NewWalker()

	// First index — nothing known yet.
	fresh, changed, removed, err := w.IncrementalReindex(g, dir, nil)
	if err != nil {
		t.Fatalf("IncrementalReindex: %v", err)
	}
	if len(fresh) == 0 {
		t.Error("expected fresh mtime map")
	}
	if changed == 0 {
		t.Error("expected changed > 0 for new files")
	}
	_ = removed
}

func TestWalker_IncrementalReindex_NothingChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "svc.go")
	if err := os.WriteFile(path, []byte("package svc\nfunc Run() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New("testrepo")
	w := parser.NewWalker()

	// Do a full WalkDir first to get mtimes.
	mtimes, err := w.WalkDir(g, dir)
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}

	// Pass existing mtimes → nothing should be reparsed.
	fresh, changed, removed, err := w.IncrementalReindex(g, dir, mtimes)
	if err != nil {
		t.Fatalf("IncrementalReindex: %v", err)
	}
	if changed != 0 {
		t.Errorf("expected 0 changed files, got %d", changed)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed files, got %d", removed)
	}
	_ = fresh
}

func TestWalker_IncrementalReindex_RemovedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.go")
	if err := os.WriteFile(path, []byte("package old\nfunc Old() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := parser.NewWalker()
	g := graph.New("testrepo")
	mtimes, err := w.WalkDir(g, dir)
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}

	// Delete the file — IncrementalReindex should count it as removed.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	_, _, removed, err := w.IncrementalReindex(g, dir, mtimes)
	if err != nil {
		t.Fatalf("IncrementalReindex: %v", err)
	}
	if removed == 0 {
		t.Error("expected removed > 0 after file deletion")
	}
}

// ── Walker.RegisterPlugin ──────────────────────────────────────────────────────

func TestWalker_RegisterPlugin_EmptyCommand(t *testing.T) {
	w := parser.NewWalker()
	// Empty command → plugin registered but Parse will return error.
	w.RegisterPlugin([]string{".graphql"}, "", nil)
}

func TestWalker_RegisterPlugin_ExtensionOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schema.graphql")
	if err := os.WriteFile(path, []byte("type Query { hello: String }"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := parser.NewWalker()
	// Register a plugin with empty command — ParseFile will call it and get an error,
	// but the extension is properly registered.
	w.RegisterPlugin([]string{".graphql"}, "", nil)

	g := graph.New("testrepo")
	// ParseFile should call the plugin and get an error (empty command) — must not panic.
	err := w.ParseFile(g, path)
	// Either nil (file not found by extension check) or an error from empty command.
	_ = err
}

// ── Walker coverage: WalkDir / IncrementalReindex edge cases ──────────────────

func TestWalker_WalkDir_NonExistentRoot(t *testing.T) {
	w := parser.NewWalker()
	g := graph.New("test")
	_, err := w.WalkDir(g, "/nonexistent/path/that/does/not/exist")
	if err == nil {
		t.Error("expected error for non-existent root")
	}
}

func TestWalker_IncrementalReindex_WithVendorDir(t *testing.T) {
	dir := t.TempDir()
	// Create a vendor/ subdir with a Go file — should be skipped.
	vendorDir := filepath.Join(dir, "vendor")
	if err := os.MkdirAll(vendorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vendorDir, "lib.go"),
		[]byte("package lib\nfunc Lib() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create a real Go file in the root.
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := parser.NewWalker()
	g := graph.New("test")
	fresh, changed, _, err := w.IncrementalReindex(g, dir, nil)
	if err != nil {
		t.Fatalf("IncrementalReindex: %v", err)
	}
	_ = fresh
	_ = changed
	// vendor/lib.go must NOT appear in fresh mtimes.
	for path := range fresh {
		if filepath.Base(filepath.Dir(path)) == "vendor" {
			t.Errorf("vendor file should be skipped, got %s", path)
		}
	}
}

func TestWalker_IncrementalReindex_UnsupportedExtension(t *testing.T) {
	dir := t.TempDir()
	// .xyz is genuinely unsupported — not handled by any built-in or generic parser.
	if err := os.WriteFile(filepath.Join(dir, "data.xyz"),
		[]byte("some binary blob"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := parser.NewWalker()
	g := graph.New("test")
	fresh, _, _, err := w.IncrementalReindex(g, dir, nil)
	if err != nil {
		t.Fatalf("IncrementalReindex: %v", err)
	}
	// data.xyz must not be in fresh (unsupported extension).
	for path := range fresh {
		if filepath.Ext(path) == ".xyz" {
			t.Errorf("unsupported .xyz file should not appear in fresh mtimes, got %s", path)
		}
	}
	// main.go must be in fresh.
	found := false
	for path := range fresh {
		if filepath.Base(path) == "main.go" {
			found = true
		}
	}
	if !found {
		t.Error("expected main.go in fresh mtimes")
	}
}

// hasNode is a local helper (mirrors the one in watcher_test.go).
func hasNode(g *graph.Graph, name string) bool {
	return len(g.FindByName(name)) > 0
}
