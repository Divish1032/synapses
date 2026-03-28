package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── Zig test helpers ───────────────────────────────────────────────────────

const zigBasicSource = `pub fn add(a: i32, b: i32) i32 {
    return a + b;
}

pub fn greet(name: []const u8) void {
    std.debug.print("Hello {s}!", .{name});
}

fn private_helper() void {
}
`

const zigStructSource = `pub const Point = struct {
    x: i32,
    y: i32,

    pub fn distance(self: Point) i32 {
        return self.x + self.y;
    }

    pub fn move(self: *Point, dx: i32, dy: i32) void {
        self.x += dx;
        self.y += dy;
    }
};
`

const zigEnumSource = `pub const Color = enum {
    Red,
    Green,
    Blue,

    pub fn name(self: Color) []const u8 {
        return switch (self) {
            .Red => "red",
            .Green => "green",
            .Blue => "blue",
        };
    }
};
`

const zigErrorSetSource = `pub const FileError = error {
    FileNotFound,
    PermissionDenied,
    IOError,
};

const ParseError = error {
    InvalidFormat,
    UnexpectedEOF,
};
`

const zigImportSource = `const std = @import("std");
pub const my_module = @import("./my_module.zig");
const math = @import("math.zig");

pub fn main() void {
    std.debug.print("Hello\n", .{});
}
`

const zigUnionSource = `pub const Value = union {
    int: i32,
    float: f32,
    string: []const u8,

    pub fn toString(self: Value) []const u8 {
        return switch (self) {
            .int => "int",
            .float => "float",
            .string => "string",
        };
    }
};
`

const zigTestSource = `test "addition" {
    const result = add(2, 3);
    try std.testing.expectEqual(@as(i32, 5), result);
}

test "string matching" {
    try std.testing.expectEqualStrings("hello", "hello");
}

pub fn add(a: i32, b: i32) i32 {
    return a + b;
}
`

func parseZig(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewZigParser()
	if err := p.Parse(g, "/tmp/test.zig", []byte(src)); err != nil {
		t.Fatalf("ZigParser.Parse() error: %v", err)
	}
	return g
}

// ─── Extensions ──────────────────────────────────────────────────────────────

func TestZigParser_Extensions(t *testing.T) {
	exts := parser.NewZigParser().Extensions()
	if len(exts) != 1 || exts[0] != ".zig" {
		t.Errorf("Extensions() = %v, want [.zig]", exts)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestZigParser_FileNode(t *testing.T) {
	g := parseZig(t, zigBasicSource)
	nodes := g.FindByName("test.zig")
	if len(nodes) == 0 {
		t.Fatal("file node test.zig not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

// ─── Top-level functions ──────────────────────────────────────────────────────

func TestZigParser_ExtractPublicFunction(t *testing.T) {
	g := parseZig(t, zigBasicSource)
	nodes := g.FindByName("add")
	if len(nodes) == 0 {
		t.Fatal("expected add function node")
	}
	n := nodes[0]
	if n.Type != graph.NodeFunction {
		t.Errorf("add: type = %q, want NodeFunction", n.Type)
	}
	if !n.Exported {
		t.Error("add should be exported (pub keyword)")
	}
	if n.Metadata["params"] == "" {
		t.Error("add should have params metadata")
	}
	if n.Metadata["return_type"] != "i32" {
		t.Errorf("add return_type = %q, want i32", n.Metadata["return_type"])
	}
}

func TestZigParser_ExtractPrivateFunction(t *testing.T) {
	g := parseZig(t, zigBasicSource)
	nodes := g.FindByName("private_helper")
	if len(nodes) == 0 {
		t.Fatal("expected private_helper function node")
	}
	n := nodes[0]
	if n.Exported {
		t.Error("private_helper should not be exported")
	}
}

func TestZigParser_FunctionWithStringParameter(t *testing.T) {
	g := parseZig(t, zigBasicSource)
	nodes := g.FindByName("greet")
	if len(nodes) == 0 {
		t.Fatal("expected greet function")
	}
	n := nodes[0]
	if !n.Exported {
		t.Error("greet should be exported")
	}
	if n.Metadata["return_type"] != "void" {
		t.Errorf("greet return_type = %q, want void", n.Metadata["return_type"])
	}
}

// ─── Struct extraction ──────────────────────────────────────────────────────────

func TestZigParser_ExtractStruct(t *testing.T) {
	g := parseZig(t, zigStructSource)
	nodes := g.FindByName("Point")
	if len(nodes) == 0 {
		t.Fatal("expected Point struct node")
	}
	n := nodes[0]
	if n.Type != graph.NodeStruct {
		t.Errorf("Point: type = %q, want NodeStruct", n.Type)
	}
	if !n.Exported {
		t.Error("Point should be exported (pub keyword)")
	}
	if n.Metadata["kind"] != "struct" {
		t.Errorf("Point kind = %q, want struct", n.Metadata["kind"])
	}
}

func TestZigParser_ExtractStructMethods(t *testing.T) {
	g := parseZig(t, zigStructSource)

	// Check for distance method
	nodes := g.FindByName("Point.distance")
	if len(nodes) == 0 {
		t.Fatal("expected Point.distance method")
	}
	distNode := nodes[0]
	if distNode.Type != graph.NodeMethod {
		t.Errorf("Point.distance: type = %q, want NodeMethod", distNode.Type)
	}
	if !distNode.Exported {
		t.Error("Point.distance should be exported (pub keyword)")
	}

	// Check for move method
	moveNodes := g.FindByName("Point.move")
	if len(moveNodes) == 0 {
		t.Fatal("expected Point.move method")
	}
	moveNode := moveNodes[0]
	if moveNode.Type != graph.NodeMethod {
		t.Errorf("Point.move: type = %q, want NodeMethod", moveNode.Type)
	}
}

// ─── Enum extraction ────────────────────────────────────────────────────────────

func TestZigParser_ExtractEnum(t *testing.T) {
	g := parseZig(t, zigEnumSource)
	nodes := g.FindByName("Color")
	if len(nodes) == 0 {
		t.Fatal("expected Color enum node")
	}
	n := nodes[0]
	if n.Type != graph.NodeStruct {
		t.Errorf("Color: type = %q, want NodeStruct", n.Type)
	}
	if n.Metadata["kind"] != "enum" {
		t.Errorf("Color kind = %q, want enum", n.Metadata["kind"])
	}
	if n.Metadata["values"] == "" {
		t.Error("Color should have values metadata")
	}
}

func TestZigParser_EnumMethods(t *testing.T) {
	g := parseZig(t, zigEnumSource)
	nodes := g.FindByName("Color.name")
	if len(nodes) == 0 {
		t.Fatal("expected Color.name method")
	}
	n := nodes[0]
	if n.Type != graph.NodeMethod {
		t.Errorf("Color.name: type = %q, want NodeMethod", n.Type)
	}
}

// ─── Error set extraction ──────────────────────────────────────────────────────

func TestZigParser_ExtractErrorSet(t *testing.T) {
	g := parseZig(t, zigErrorSetSource)
	nodes := g.FindByName("FileError")
	if len(nodes) == 0 {
		t.Fatal("expected FileError error set node")
	}
	n := nodes[0]
	if n.Type != graph.NodeStruct {
		t.Errorf("FileError: type = %q, want NodeStruct", n.Type)
	}
	if n.Metadata["kind"] != "error_set" {
		t.Errorf("FileError kind = %q, want error_set", n.Metadata["kind"])
	}
	if n.Metadata["errors"] == "" {
		t.Error("FileError should have errors metadata")
	}
}

func TestZigParser_PrivateErrorSet(t *testing.T) {
	g := parseZig(t, zigErrorSetSource)
	nodes := g.FindByName("ParseError")
	if len(nodes) == 0 {
		t.Fatal("expected ParseError error set")
	}
	n := nodes[0]
	if n.Exported {
		t.Error("ParseError should not be exported (no pub keyword)")
	}
}

// ─── Import extraction ──────────────────────────────────────────────────────────

func TestZigParser_ExtractImports(t *testing.T) {
	g := parseZig(t, zigImportSource)

	// Check std import
	stdNodes := g.FindByName("std")
	if len(stdNodes) == 0 {
		t.Fatal("expected std import")
	}
	stdNode := stdNodes[0]
	if stdNode.Type != graph.NodePackage {
		t.Errorf("std: type = %q, want NodePackage", stdNode.Type)
	}
	if stdNode.Package != "std" {
		t.Errorf("std: Package = %q, want std", stdNode.Package)
	}

	// Check my_module import
	myModuleNodes := g.FindByName("my_module")
	if len(myModuleNodes) == 0 {
		t.Fatal("expected my_module import")
	}
	myModuleNode := myModuleNodes[0]
	if myModuleNode.Type != graph.NodePackage {
		t.Errorf("my_module: type = %q, want NodePackage", myModuleNode.Type)
	}

	// Check math import (private)
	mathNodes := g.FindByName("math")
	if len(mathNodes) == 0 {
		t.Fatal("expected math import")
	}
	mathNode := mathNodes[0]
	if mathNode.Type != graph.NodePackage {
		t.Errorf("math: type = %q, want NodePackage", mathNode.Type)
	}
}

// ─── Union extraction ───────────────────────────────────────────────────────────

func TestZigParser_ExtractUnion(t *testing.T) {
	g := parseZig(t, zigUnionSource)
	nodes := g.FindByName("Value")
	if len(nodes) == 0 {
		t.Fatal("expected Value union node")
	}
	n := nodes[0]
	if n.Type != graph.NodeStruct {
		t.Errorf("Value: type = %q, want NodeStruct", n.Type)
	}
	if n.Metadata["kind"] != "union" {
		t.Errorf("Value kind = %q, want union", n.Metadata["kind"])
	}
}

func TestZigParser_UnionMethods(t *testing.T) {
	g := parseZig(t, zigUnionSource)
	nodes := g.FindByName("Value.toString")
	if len(nodes) == 0 {
		t.Fatal("expected Value.toString method")
	}
}

// ─── Test declarations ──────────────────────────────────────────────────────────

func TestZigParser_ExtractTestDeclarations(t *testing.T) {
	g := parseZig(t, zigTestSource)

	// Check first test
	testNodes := g.FindByName("test_addition")
	if len(testNodes) == 0 {
		t.Fatal("expected test_addition")
	}
	testNode := testNodes[0]
	if testNode.Type != graph.NodeFunction {
		t.Errorf("test_addition: type = %q, want NodeFunction", testNode.Type)
	}
	if testNode.Metadata["kind"] != "test" {
		t.Errorf("test_addition kind = %q, want test", testNode.Metadata["kind"])
	}

	// Check second test - verify at least 2 tests are extracted
	allTests := 0
	for _, node := range g.AllNodes() {
		if node.Type == graph.NodeFunction && node.Metadata["kind"] == "test" {
			allTests++
		}
	}
	if allTests < 2 {
		t.Errorf("expected at least 2 tests, got %d", allTests)
	}
}

// ─── Variable declarations ──────────────────────────────────────────────────────

func TestZigParser_ExtractVariable(t *testing.T) {
	src := `const VERSION = "1.0.0";
pub const API_KEY = "secret";
var counter: i32 = 0;
`
	g := parseZig(t, src)

	// Check constant
	constNodes := g.FindByName("VERSION")
	if len(constNodes) == 0 {
		t.Fatal("expected VERSION constant")
	}
	constNode := constNodes[0]
	if constNode.Type != graph.NodeVariable {
		t.Errorf("VERSION: type = %q, want NodeVariable", constNode.Type)
	}
	if constNode.Exported {
		t.Error("VERSION should not be exported")
	}

	// Check public constant
	pubConstNodes := g.FindByName("API_KEY")
	if len(pubConstNodes) == 0 {
		t.Fatal("expected API_KEY")
	}
	pubConstNode := pubConstNodes[0]
	if !pubConstNode.Exported {
		t.Error("API_KEY should be exported (pub keyword)")
	}
}

// ─── Empty file ────────────────────────────────────────────────────────────────

func TestZigParser_EmptyFile(t *testing.T) {
	g := parseZig(t, "")
	nodes := g.FindByName("test.zig")
	if len(nodes) == 0 {
		t.Fatal("file node should exist even for empty file")
	}
}

// ─── Complex nested structure ──────────────────────────────────────────────────

func TestZigParser_ComplexStructure(t *testing.T) {
	src := `pub const Calculator = struct {
    value: i32,

    pub fn add(self: *Calculator, n: i32) void {
        self.value += n;
    }

    pub fn subtract(self: *Calculator, n: i32) void {
        self.value -= n;
    }

    pub fn reset(self: *Calculator) void {
        self.value = 0;
    }
};

pub fn createCalculator() Calculator {
    return Calculator{ .value = 0 };
}
`
	g := parseZig(t, src)

	// Check struct exists
	structNodes := g.FindByName("Calculator")
	if len(structNodes) == 0 {
		t.Fatal("expected Calculator struct")
	}

	// Check all three methods exist
	methods := []string{"add", "subtract", "reset"}
	for _, method := range methods {
		nodes := g.FindByName("Calculator." + method)
		if len(nodes) == 0 {
			t.Errorf("expected Calculator.%s method", method)
		}
	}

	// Check createCalculator function
	funcNodes := g.FindByName("createCalculator")
	if len(funcNodes) == 0 {
		t.Fatal("expected createCalculator function")
	}
}

// ─── Multiple top-level items ──────────────────────────────────────────────────

func TestZigParser_MultipleTopLevelItems(t *testing.T) {
	src := `pub fn function1() void {}
pub fn function2() void {}
pub const CONST1 = 42;
pub const CONST2 = "hello";
pub const Struct1 = struct {};
pub const Enum1 = enum { A, B };
`
	g := parseZig(t, src)

	expectedNames := []string{"function1", "function2", "CONST1", "CONST2", "Struct1", "Enum1"}
	for _, name := range expectedNames {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected %s to be extracted", name)
		}
	}
}

// ─── Return type variations ────────────────────────────────────────────────────

func TestZigParser_ReturnTypeVariations(t *testing.T) {
	src := `pub fn returnsVoid() void {}
pub fn returnsInt() i32 { return 42; }
pub fn returnsBool() bool { return true; }
pub fn returnsU8() u8 { return 0; }
pub fn returnsF32() f32 { return 0.0; }
pub fn returnsU64() u64 { return 0; }
`
	g := parseZig(t, src)

	testCases := []struct {
		name string
		want string
	}{
		{"returnsVoid", "void"},
		{"returnsInt", "i32"},
		{"returnsBool", "bool"},
		{"returnsU8", "u8"},
		{"returnsF32", "f32"},
		{"returnsU64", "u64"},
	}

	for _, tc := range testCases {
		nodes := g.FindByName(tc.name)
		if len(nodes) == 0 {
			t.Errorf("expected %s function", tc.name)
			continue
		}
		if got := nodes[0].Metadata["return_type"]; got != tc.want {
			t.Errorf("%s return_type = %q, want %q", tc.name, got, tc.want)
		}
	}
}
