package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── Dart test helpers ────────────────────────────────────────────────────────

func parseDart(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewDartParser()
	if err := p.Parse(g, "/tmp/widget.dart", []byte(src)); err != nil {
		t.Fatalf("DartParser.Parse() error: %v", err)
	}
	return g
}

// dartSource is a realistic Flutter widget file used as the main fixture.
const dartSource = `import 'package:flutter/material.dart';
import 'dart:async';
import 'src/utils.dart';
export 'src/widget.dart';

/// A counter widget that increments a value.
class CounterWidget extends StatelessWidget {
  /// The initial count value.
  final int initialCount;

  const CounterWidget({Key? key, this.initialCount = 0}) : super(key: key);

  /// Builds the widget tree.
  @override
  Widget build(BuildContext context) {
    return Text('Count: $initialCount');
  }

  void _privateMethod() {}
}

/// Abstract base for all validators.
abstract class Validator {
  bool validate(String value);
}

/// Mixin for logging functionality.
mixin Logging on Object {
  void log(String message) {
    print(message);
  }
}

/// Extension on String to add helper methods.
extension StringExt on String {
  bool get isBlank => trim().isEmpty;
}

/// Supported color options.
enum AppColor { red, green, blue, purple }

/// Entry point of the application.
void main() {
  runApp(const CounterWidget());
}

/// Fetches data from the network asynchronously.
Future<void> fetchData() async {
  await Future.delayed(Duration(seconds: 1));
}

// Private top-level helper (not exported).
void _internalHelper() {}

/// A private class not exported.
class _PrivateState extends State<CounterWidget> {
  @override
  Widget build(BuildContext context) => const SizedBox();
}
`

// ─── Extensions ──────────────────────────────────────────────────────────────

func TestDartParser_Extensions(t *testing.T) {
	exts := parser.NewDartParser().Extensions()
	if !hasExtension(exts, ".dart") {
		t.Errorf("Extensions() = %v, missing .dart", exts)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestDartParser_FileNode(t *testing.T) {
	assertFileNode(t, parseDart(t, dartSource), "widget.dart")
}

// ─── Class extraction ────────────────────────────────────────────────────────

func TestDartParser_RegularClass(t *testing.T) {
	g := parseDart(t, dartSource)
	n := assertNode(t, g, "CounterWidget", graph.NodeStruct)
	if !n.Exported {
		t.Error("CounterWidget should be exported (no underscore prefix)")
	}
	if n.Metadata == nil || n.Metadata["kind"] != "class" {
		t.Errorf("CounterWidget kind = %q, want 'class'", n.Metadata["kind"])
	}
}

func TestDartParser_AbstractClass(t *testing.T) {
	g := parseDart(t, dartSource)
	n := assertNode(t, g, "Validator", graph.NodeStruct)
	if !n.Exported {
		t.Error("Validator should be exported")
	}
	if n.Metadata == nil || n.Metadata["kind"] != "class" {
		t.Errorf("Validator kind = %q, want 'class'", n.Metadata["kind"])
	}
	if n.Metadata["abstract"] != "true" {
		t.Errorf("Validator should have abstract=true in metadata, got %q", n.Metadata["abstract"])
	}
}

func TestDartParser_ClassWithExtendsAndSuperclass(t *testing.T) {
	src := `
class Dog extends Animal implements Runnable {
  void bark() {}
}
`
	g := graph.New("testrepo")
	p := parser.NewDartParser()
	if err := p.Parse(g, "/tmp/dog.dart", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.FindByName("Dog")
	if len(nodes) == 0 {
		t.Fatal("class Dog not found")
	}
	if nodes[0].Type != graph.NodeStruct {
		t.Errorf("Dog type = %q, want NodeStruct", nodes[0].Type)
	}
}

// ─── Mixin extraction ────────────────────────────────────────────────────────

func TestDartParser_Mixin(t *testing.T) {
	g := parseDart(t, dartSource)
	n := assertNode(t, g, "Logging", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "mixin" {
		t.Errorf("Logging kind = %q, want 'mixin'", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("Logging mixin should be exported")
	}
}

// ─── Extension extraction ────────────────────────────────────────────────────

func TestDartParser_Extension(t *testing.T) {
	g := parseDart(t, dartSource)
	n := assertNode(t, g, "StringExt", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "extension" {
		t.Errorf("StringExt kind = %q, want 'extension'", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("StringExt extension should be exported")
	}
}

// ─── Enum extraction ─────────────────────────────────────────────────────────

func TestDartParser_Enum(t *testing.T) {
	g := parseDart(t, dartSource)
	n := assertNode(t, g, "AppColor", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "enum" {
		t.Errorf("AppColor kind = %q, want 'enum'", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("AppColor enum should be exported")
	}
}

// ─── Top-level function extraction ───────────────────────────────────────────

func TestDartParser_TopLevelVoidMain(t *testing.T) {
	g := parseDart(t, dartSource)
	n := assertNode(t, g, "main", graph.NodeFunction)
	if !n.Exported {
		t.Error("main() should be exported")
	}
}

func TestDartParser_AsyncFunction(t *testing.T) {
	g := parseDart(t, dartSource)
	n := assertNode(t, g, "fetchData", graph.NodeFunction)
	if !n.Exported {
		t.Error("fetchData should be exported")
	}
	if n.Metadata == nil || n.Metadata["async"] != "true" {
		t.Errorf("fetchData should have async=true metadata, got %v", n.Metadata)
	}
}

// ─── Private symbols (not exported) ─────────────────────────────────────────

func TestDartParser_PrivateClass(t *testing.T) {
	g := parseDart(t, dartSource)
	n := assertNode(t, g, "_PrivateState", graph.NodeStruct)
	if n.Exported {
		t.Error("_PrivateState should NOT be exported (underscore prefix = private in Dart)")
	}
}

func TestDartParser_PrivateTopLevelFunction(t *testing.T) {
	g := parseDart(t, dartSource)
	n := assertNode(t, g, "_internalHelper", graph.NodeFunction)
	if n.Exported {
		t.Error("_internalHelper should NOT be exported")
	}
}

func TestDartParser_PrivateMethod(t *testing.T) {
	g := parseDart(t, dartSource)
	// _privateMethod is inside CounterWidget — stored as CounterWidget._privateMethod
	nodes := g.FindByName("CounterWidget._privateMethod")
	if len(nodes) == 0 {
		t.Fatal("method CounterWidget._privateMethod not found in graph")
	}
	if nodes[0].Exported {
		t.Error("_privateMethod should NOT be exported")
	}
}

// ─── Import extraction ───────────────────────────────────────────────────────

func TestDartParser_ImportPackageFlutter(t *testing.T) {
	g := parseDart(t, dartSource)
	nodes := g.FindByName("package:flutter/material.dart")
	if len(nodes) == 0 {
		t.Fatal("import node for package:flutter/material.dart not found")
	}
	if nodes[0].Type != graph.NodePackage {
		t.Errorf("import node type = %q, want NodePackage", nodes[0].Type)
	}
}

func TestDartParser_ImportDartSDK(t *testing.T) {
	g := parseDart(t, dartSource)
	nodes := g.FindByName("dart:async")
	if len(nodes) == 0 {
		t.Fatal("import node for dart:async not found")
	}
	if nodes[0].Type != graph.NodePackage {
		t.Errorf("import node type = %q, want NodePackage", nodes[0].Type)
	}
}

func TestDartParser_ImportRelativePath(t *testing.T) {
	g := parseDart(t, dartSource)
	nodes := g.FindByName("src/utils.dart")
	if len(nodes) == 0 {
		t.Fatal("import node for src/utils.dart not found")
	}
	if nodes[0].Type != graph.NodePackage {
		t.Errorf("import node type = %q, want NodePackage", nodes[0].Type)
	}
}

// ─── Export declaration ──────────────────────────────────────────────────────

func TestDartParser_ExportDeclaration(t *testing.T) {
	g := parseDart(t, dartSource)
	// The exported path should have an EdgeImports with kind=export.
	fileNodes := g.FindByName("widget.dart")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	found := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Name == "src/widget.dart" {
				found = true
				if n.Metadata == nil || n.Metadata["kind"] != "export" {
					t.Errorf("export node kind = %q, want 'export'", n.Metadata["kind"])
				}
				break
			}
		}
	}
	if !found {
		t.Error("export 'src/widget.dart' not found in graph")
	}
}

// ─── Doc comments ────────────────────────────────────────────────────────────

func TestDartParser_DocCommentTripleSlash(t *testing.T) {
	g := parseDart(t, dartSource)
	n := assertNode(t, g, "CounterWidget", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["doc"] == "" {
		t.Error("CounterWidget should have a doc comment extracted from /// lines")
	}
	doc := n.Metadata["doc"]
	if !containsAny(doc, "counter", "Counter") {
		t.Errorf("CounterWidget doc = %q, expected to contain 'counter'", doc)
	}
}

func TestDartParser_DocCommentBlockStyle(t *testing.T) {
	src := `
/** Computes the sum of two numbers. */
int add(int a, int b) {
  return a + b;
}
`
	g := graph.New("testrepo")
	p := parser.NewDartParser()
	if err := p.Parse(g, "/tmp/math.dart", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.FindByName("add")
	if len(nodes) == 0 {
		t.Fatal("function 'add' not found")
	}
	doc := nodes[0].Metadata["doc"]
	if doc == "" {
		t.Error("function 'add' should have a doc comment from /** */ block")
	}
}

// ─── Edge: file → class (DEFINES) ───────────────────────────────────────────

func TestDartParser_DefinesEdgeFileToClass(t *testing.T) {
	g := parseDart(t, dartSource)
	fileNodes := g.FindByName("widget.dart")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	assertDefinesEdge(t, g, fileID, "CounterWidget")
	assertDefinesEdge(t, g, fileID, "Validator")
	assertDefinesEdge(t, g, fileID, "AppColor")
}

// ─── Edge: file → package (IMPORTS) ─────────────────────────────────────────

func TestDartParser_ImportsEdge(t *testing.T) {
	g := parseDart(t, dartSource)
	fileNodes := g.FindByName("widget.dart")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	wantImports := map[string]bool{
		"package:flutter/material.dart": false,
		"dart:async":                    false,
		"src/utils.dart":                false,
	}
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil {
				if _, ok := wantImports[n.Name]; ok {
					wantImports[n.Name] = true
				}
			}
		}
	}
	for name, found := range wantImports {
		if !found {
			t.Errorf("missing IMPORTS edge for %q", name)
		}
	}
}

// ─── Empty file ──────────────────────────────────────────────────────────────

func TestDartParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewDartParser(), ".dart", "")
}

// ─── Minimal function-only file ──────────────────────────────────────────────

func TestDartParser_MinimalFunctionFile(t *testing.T) {
	src := `void greet(String name) {
  print('Hello $name');
}
`
	g := graph.New("testrepo")
	p := parser.NewDartParser()
	if err := p.Parse(g, "/tmp/greet.dart", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.FindByName("greet")
	if len(nodes) == 0 {
		t.Fatal("function 'greet' not found")
	}
	if nodes[0].Type != graph.NodeFunction {
		t.Errorf("greet type = %q, want NodeFunction", nodes[0].Type)
	}
	if !nodes[0].Exported {
		t.Error("greet should be exported (no underscore prefix)")
	}
}

// ─── Method inside class is a child of that class ───────────────────────────

func TestDartParser_MethodEdgeClassToMethod(t *testing.T) {
	g := parseDart(t, dartSource)
	classNodes := g.FindByName("CounterWidget")
	if len(classNodes) == 0 {
		t.Fatal("CounterWidget class node not found")
	}
	classID := classNodes[0].ID

	// build() method should be defined as a child of CounterWidget.
	found := false
	for _, e := range g.OutEdges(classID) {
		if e.Type == graph.EdgeDefines {
			n := g.GetNode(e.To)
			if n != nil && (n.Name == "CounterWidget.build" || n.Name == "build") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected DEFINES edge from CounterWidget to its build() method")
	}
}

// ─── Multiple imports from single file ──────────────────────────────────────

func TestDartParser_MultipleImports(t *testing.T) {
	src := `import 'package:http/http.dart';
import 'package:provider/provider.dart';
import 'dart:convert';
import 'dart:io';
import 'local/models.dart';
`
	g := graph.New("testrepo")
	p := parser.NewDartParser()
	if err := p.Parse(g, "/tmp/multi.dart", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	fileNodes := g.FindByName("multi.dart")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	importCount := 0
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}
	if importCount != 5 {
		t.Errorf("expected 5 import edges, got %d", importCount)
	}
}

// ─── Enum values inside enum body are not extracted as functions ─────────────

func TestDartParser_EnumBodyNotExtractedAsFunctions(t *testing.T) {
	src := `enum Status {
  active,
  inactive,
  pending;

  String get label => name;
}
`
	g := graph.New("testrepo")
	p := parser.NewDartParser()
	if err := p.Parse(g, "/tmp/status.dart", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// active/inactive/pending should not be NodeFunction
	for _, name := range []string{"active", "inactive", "pending"} {
		nodes := g.FindByName(name)
		for _, n := range nodes {
			if n.Type == graph.NodeFunction {
				t.Errorf("enum value %q should not be NodeFunction", name)
			}
		}
	}
}

func TestDartParserTypedef(t *testing.T) {
	src := []byte(`
import 'dart:async';

/// New-style typedef (Dart 2.0+)
typedef VoidCallback = void Function();
typedef Predicate<T> = bool Function(T value);
typedef AsyncCallback = Future<void> Function();

/// Old-style typedef (legacy)
typedef void OldCallback(int x);
typedef String Mapper(int value);

/// Private typedef
typedef _InternalFn = void Function(String);

void main() {
  VoidCallback cb = () => print('hello');
}
`)
	g := graph.New("testrepo")
	p := parser.NewDartParser()
	if err := p.Parse(g, "test.dart", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name     string
		exported bool
	}{
		{"VoidCallback", true},
		{"Predicate", true},
		{"AsyncCallback", true},
		{"OldCallback", true},
		{"Mapper", true},
		{"_InternalFn", false},
	}
	for _, tc := range tests {
		nodes := g.FindByName(tc.name)
		if len(nodes) == 0 {
			t.Errorf("expected typedef %q not found", tc.name)
			continue
		}
		n := nodes[0]
		if n.Type != graph.NodeStruct {
			t.Errorf("%q: expected NodeStruct, got %v", tc.name, n.Type)
		}
		if n.Metadata["kind"] != "typedef" {
			t.Errorf("%q: expected kind=typedef, got %q", tc.name, n.Metadata["kind"])
		}
		if n.Exported != tc.exported {
			t.Errorf("%q: expected Exported=%v, got %v", tc.name, tc.exported, n.Exported)
		}
	}

	// Ensure "Function" is NOT extracted as a false-positive function node
	fnNodes := g.FindByName("Function")
	for _, n := range fnNodes {
		if n.Type == graph.NodeFunction {
			t.Errorf("false-positive: 'Function' should not be extracted as NodeFunction from typedef lines")
		}
	}
}

// ─── part of detection ───────────────────────────────────────────────────────

func TestDartParserPartOf(t *testing.T) {
	src := []byte(`part of 'mylib.dart';

class PartClass {
  void doSomething() {}
}
`)
	g := graph.New("testrepo")
	p := parser.NewDartParser()
	if err := p.Parse(g, "/src/part_file.dart", src); err != nil {
		t.Fatalf("DartParser.Parse() error: %v", err)
	}
	// Should have an EdgeImports to mylib.dart
	edges := g.AllEdges()
	found := false
	for _, e := range edges {
		if e.Type == graph.EdgeImports {
			toNode := g.GetNode(e.To)
			if toNode != nil && toNode.Name == "mylib.dart" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected EdgeImports to mylib.dart from part of declaration")
	}
}

// ─── typedef nested generics ─────────────────────────────────────────────────

func TestDartParserTypedefNestedGenerics(t *testing.T) {
	src := []byte(`typedef JsonMap = Map<String, dynamic>;
typedef NestedMap = Map<String, List<int>>;
typedef Callback<T> = void Function(T value);
`)
	g := graph.New("testrepo")
	p := parser.NewDartParser()
	if err := p.Parse(g, "/src/types.dart", src); err != nil {
		t.Fatalf("DartParser.Parse() error: %v", err)
	}
	for _, name := range []string{"JsonMap", "NestedMap", "Callback"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected typedef node %q", name)
			continue
		}
		if nodes[0].Metadata["kind"] != "typedef" {
			t.Errorf("%q: kind = %q, want typedef", name, nodes[0].Metadata["kind"])
		}
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// containsAny returns true if s contains any of the given substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) <= len(s) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
