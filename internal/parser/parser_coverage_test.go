package parser

// White-box coverage tests for internal parser functions that can't be
// reached from the external test package:
// - newGenericParser().Parse (0%)
// - shouldSkipDir all case groups
// - pluginNodeType all values
// - plugin.Parse success/failure paths
// - isTSBuiltin / isBuiltinPython

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── genericParser ─────────────────────────────────────────────────────────────

func TestGenericParser_Parse_HTMLFile(t *testing.T) {
	p := newGenericParser()
	g := graph.New("testrepo")
	if err := p.Parse(g, "src/index.html", []byte("<html><body>Hello</body></html>")); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	nodes := g.FindByName("index.html")
	if len(nodes) == 0 {
		t.Fatal("expected a file node named 'index.html'")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("expected NodeFile, got %q", nodes[0].Type)
	}
}

func TestGenericParser_Extensions_Contains(t *testing.T) {
	p := newGenericParser()
	exts := p.Extensions()
	found := false
	for _, e := range exts {
		if e == ".html" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected .html in generic parser extensions")
	}
}

// ── shouldSkipDir ─────────────────────────────────────────────────────────────

func TestShouldSkipDir_DependencyManagers(t *testing.T) {
	for _, name := range []string{"node_modules", "vendor", "bower_components", "jspm_packages", "Pods"} {
		if !shouldSkipDir(name) {
			t.Errorf("shouldSkipDir(%q) = false, want true", name)
		}
	}
}

func TestShouldSkipDir_BuildArtifacts(t *testing.T) {
	for _, name := range []string{"dist", "build", "out", "target", "obj", "storybook-static", "coverage", ".nyc_output"} {
		if !shouldSkipDir(name) {
			t.Errorf("shouldSkipDir(%q) = false, want true", name)
		}
	}
}

func TestShouldSkipDir_TempDirs(t *testing.T) {
	for _, name := range []string{"tmp", "temp", "cache"} {
		if !shouldSkipDir(name) {
			t.Errorf("shouldSkipDir(%q) = false, want true", name)
		}
	}
}

func TestShouldSkipDir_GeneratedDirs(t *testing.T) {
	for _, name := range []string{"generated", "gen", "__generated__", "__mocks__"} {
		if !shouldSkipDir(name) {
			t.Errorf("shouldSkipDir(%q) = false, want true", name)
		}
	}
}

func TestShouldSkipDir_ThirdPartyDirs(t *testing.T) {
	for _, name := range []string{"third_party", "vendor_ruby", "testdata"} {
		if !shouldSkipDir(name) {
			t.Errorf("shouldSkipDir(%q) = false, want true", name)
		}
	}
}

func TestShouldSkipDir_IDEDirs(t *testing.T) {
	for _, name := range []string{".git", ".svn", ".hg", ".idea", ".vscode", "__pycache__"} {
		if !shouldSkipDir(name) {
			t.Errorf("shouldSkipDir(%q) = false, want true", name)
		}
	}
}

func TestShouldSkipDir_HiddenDir(t *testing.T) {
	// Any dir starting with "." is skipped via strings.HasPrefix.
	for _, name := range []string{".next", ".nuxt", ".turbo", ".cache", ".gradle"} {
		if !shouldSkipDir(name) {
			t.Errorf("shouldSkipDir(%q) = false, want true", name)
		}
	}
}

func TestShouldSkipDir_Regular(t *testing.T) {
	for _, name := range []string{"src", "internal", "pkg", "cmd", "lib"} {
		if shouldSkipDir(name) {
			t.Errorf("shouldSkipDir(%q) = true, want false", name)
		}
	}
}

// ── pluginNodeType ─────────────────────────────────────────────────────────────

func TestPluginNodeType_AllValues(t *testing.T) {
	cases := []struct {
		input string
		want  graph.NodeType
	}{
		{"function", graph.NodeFunction},
		{"func", graph.NodeFunction},
		{"FUNCTION", graph.NodeFunction}, // ToLower
		{"method", graph.NodeMethod},
		{"struct", graph.NodeStruct},
		{"class", graph.NodeStruct},
		{"model", graph.NodeStruct},
		{"type", graph.NodeStruct},
		{"interface", graph.NodeInterface},
		{"trait", graph.NodeInterface},
		{"protocol", graph.NodeInterface},
		{"package", graph.NodePackage},
		{"module", graph.NodePackage},
		{"namespace", graph.NodePackage},
		{"unknown_thing", graph.NodeFunction}, // default
		{"", graph.NodeFunction},              // default
	}
	for _, c := range cases {
		got := pluginNodeType(c.input)
		if got != c.want {
			t.Errorf("pluginNodeType(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// ── plugin.Parse ──────────────────────────────────────────────────────────────

// writePluginScript writes a shell script that emits jsonOutput and returns
// the path. Skips the test on Windows.
func writePluginScript(t *testing.T, jsonOutput []byte) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts not supported on Windows")
	}
	dir := t.TempDir()
	jsonFile := filepath.Join(dir, "out.json")
	if err := os.WriteFile(jsonFile, jsonOutput, 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(dir, "plugin")
	script := fmt.Sprintf("#!/bin/sh\ncat %s\n", jsonFile)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return scriptPath
}

func TestPluginParser_EmptyCommand(t *testing.T) {
	// newPluginParser with empty string → p.command == "" → Parse returns error.
	p := newPluginParser([]string{".foo"}, "")
	if p == nil {
		t.Fatal("expected non-nil plugin parser")
	}
	g := graph.New("test")
	err := p.Parse(g, "file.foo", []byte("content"))
	if err == nil {
		t.Error("expected error for empty command")
	}
}

func TestPluginParser_EmptyOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts not supported on Windows")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "empty-plugin")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := newPluginParser([]string{".foo"}, scriptPath)
	g := graph.New("test")
	if err := p.Parse(g, "file.foo", []byte("content")); err != nil {
		t.Fatalf("Parse with empty output: %v", err)
	}
	// No nodes beyond file node since output was empty.
}

func TestPluginParser_ValidJSON(t *testing.T) {
	output := pluginOutput{
		Nodes: []pluginNode{
			// Valid nodes
			{Name: "MyFunc", Type: "function", Line: 1, Exported: true, Doc: "does stuff", Signature: "func MyFunc()"},
			{Name: "MyClass", Type: "class", Line: 10},
			{Name: "MyIface", Type: "interface", Line: 20},
			{Name: "MyPkg", Type: "package", Line: 30},
			{Name: "MyMethod", Type: "method", Line: 40},
			// Skipped nodes
			{Name: "", Type: "function", Line: 5},       // empty name → skip
			{Name: "NoLine", Type: "function", Line: 0}, // line 0 → skip
		},
		Edges: []pluginEdge{
			{From: "MyFunc", To: "MyClass", Type: "CALLS"}, // valid edge
			{From: "", To: "MyClass", Type: "CALLS"},       // empty from → skip
			{From: "MyFunc", To: "", Type: "CALLS"},        // empty to → skip
			{From: "MyFunc", To: "MyClass", Type: ""},      // empty type → skip
			{From: "MyFunc", To: "Missing", Type: "CALLS"}, // missing endpoint → skip
		},
	}
	jsonBytes, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := writePluginScript(t, jsonBytes)

	p := newPluginParser([]string{".foo"}, scriptPath)
	g := graph.New("test")
	if err := p.Parse(g, "file.foo", []byte("source")); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if nodes := g.FindByName("MyFunc"); len(nodes) == 0 {
		t.Error("expected MyFunc node")
	}
	if nodes := g.FindByName("MyClass"); len(nodes) == 0 {
		t.Error("expected MyClass node")
	}
}

func TestPluginParser_NonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts not supported on Windows")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fail-plugin")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := newPluginParser([]string{".foo"}, scriptPath)
	g := graph.New("test")
	err := p.Parse(g, "file.foo", []byte("content"))
	if err == nil {
		t.Error("expected error for non-zero exit")
	}
}

func TestPluginParser_InvalidJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts not supported on Windows")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "invalid-plugin")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nprintf 'not valid json'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := newPluginParser([]string{".foo"}, scriptPath)
	g := graph.New("test")
	err := p.Parse(g, "file.foo", []byte("content"))
	if err == nil {
		t.Error("expected error for invalid JSON output")
	}
}

func TestPluginParser_WithArgs(t *testing.T) {
	// newPluginParser splits "cmd arg1 arg2" into binary + args.
	p := newPluginParser([]string{".foo"}, "my-cmd --arg value")
	if p.command != "my-cmd" {
		t.Errorf("expected command %q, got %q", "my-cmd", p.command)
	}
	if len(p.args) != 2 {
		t.Errorf("expected 2 args, got %d", len(p.args))
	}
}

// ── isTSBuiltin ───────────────────────────────────────────────────────────────

func TestIsTSBuiltin_KnownBuiltins(t *testing.T) {
	builtins := []string{
		"push", "pop", "map", "filter", "reduce", "forEach",
		"stringify", "parse", "toString", "setTimeout", "setInterval",
		"Promise", "resolve", "reject", "parseInt", "parseFloat",
		"isNaN", "isFinite", "now", "from", "call", "apply", "bind",
	}
	for _, name := range builtins {
		if !isTSBuiltin(name) {
			t.Errorf("isTSBuiltin(%q) = false, want true", name)
		}
	}
}

func TestIsTSBuiltin_CustomFunc(t *testing.T) {
	customs := []string{"myCustomFunc", "handleLogin", "validateToken", ""}
	for _, name := range customs {
		if isTSBuiltin(name) {
			t.Errorf("isTSBuiltin(%q) = true, want false", name)
		}
	}
}

// ── isBuiltinPython ───────────────────────────────────────────────────────────

func TestIsBuiltinPython_KnownBuiltins(t *testing.T) {
	builtins := []string{
		"print", "len", "range", "enumerate", "zip",
		"list", "dict", "set", "str", "int", "float", "bool",
		"isinstance", "getattr", "setattr", "open", "super",
		"abs", "max", "min", "sum", "any", "all",
		"Exception", "ValueError", "TypeError",
	}
	for _, name := range builtins {
		if !isBuiltinPython(name) {
			t.Errorf("isBuiltinPython(%q) = false, want true", name)
		}
	}
}

func TestIsBuiltinPython_CustomFunc(t *testing.T) {
	customs := []string{"my_func", "process_data", "handle_request", ""}
	for _, name := range customs {
		if isBuiltinPython(name) {
			t.Errorf("isBuiltinPython(%q) = true, want false", name)
		}
	}
}

// ── isJavaPublicNode ─────────────────────────────────────────────────────────
// isJavaPublicNode now inspects AST modifier children, so it cannot be tested
// with simple string arguments. Integration tests in languages_test.go cover it.

// ── extractLineDoc ────────────────────────────────────────────────────────────

func TestExtractLineDoc_StartLine1(t *testing.T) {
	// startLine < 2 → returns "" early
	lines := []string{"# comment", "def foo():"}
	result := extractLineDoc(lines, 1, "#")
	if result != "" {
		t.Errorf("expected empty for startLine=1, got %q", result)
	}
}

func TestExtractLineDoc_WithComment(t *testing.T) {
	// line before declaration is a comment → should collect it
	lines := []string{"# does auth", "def login():"}
	result := extractLineDoc(lines, 2, "#")
	if result != "does auth" {
		t.Errorf("expected %q, got %q", "does auth", result)
	}
}

func TestExtractLineDoc_MultipleComments(t *testing.T) {
	lines := []string{"# first line", "# second line", "def foo():"}
	result := extractLineDoc(lines, 3, "#")
	if result == "" {
		t.Error("expected non-empty doc from multiple comment lines")
	}
}

func TestExtractLineDoc_NoComment(t *testing.T) {
	lines := []string{"some_other_code()", "def foo():"}
	result := extractLineDoc(lines, 2, "#")
	if result != "" {
		t.Errorf("expected empty when no comment precedes, got %q", result)
	}
}

// ── extractDocComment ─────────────────────────────────────────────────────────

func TestExtractDocComment_StartLine1(t *testing.T) {
	// startLine < 2 → returns "" early
	result := extractDocComment([]string{"func Foo() {}"}, 1)
	if result != "" {
		t.Errorf("expected empty for startLine=1, got %q", result)
	}
}

func TestExtractDocComment_WithComment(t *testing.T) {
	lines := []string{"// Login authenticates a user.", "func Login() {}"}
	result := extractDocComment(lines, 2)
	if result != "Login authenticates a user." {
		t.Errorf("unexpected doc %q", result)
	}
}

// ── extractReceiverType ───────────────────────────────────────────────────────

func TestExtractReceiverType_Nil(t *testing.T) {
	result := extractReceiverType(sitter.Node{}, nil)
	if result != "" {
		t.Errorf("expected empty for nil receiver, got %q", result)
	}
}

// ── buildLangMeta ─────────────────────────────────────────────────────────────

func TestBuildLangMeta_AllFields(t *testing.T) {
	meta := buildLangMeta(declMeta{Signature: "func Foo() error", Doc: "does stuff", LineCount: 5})
	if meta == nil {
		t.Fatal("expected non-nil meta")
	}
	if meta["signature"] != "func Foo() error" {
		t.Errorf("expected signature, got %v", meta["signature"])
	}
	if meta["doc"] != "does stuff" {
		t.Error("expected doc")
	}
	if meta["line_count"] != "5" {
		t.Error("expected line_count")
	}
}

func TestBuildLangMeta_Empty(t *testing.T) {
	meta := buildLangMeta(declMeta{})
	if meta != nil {
		t.Errorf("expected nil for empty declMeta, got %v", meta)
	}
}

func TestBuildLangMeta_OnlyDoc(t *testing.T) {
	meta := buildLangMeta(declMeta{Doc: "just a doc"})
	if meta == nil || meta["doc"] != "just a doc" {
		t.Errorf("expected doc, got %v", meta)
	}
	if _, ok := meta["signature"]; ok {
		t.Error("expected no signature key")
	}
}
