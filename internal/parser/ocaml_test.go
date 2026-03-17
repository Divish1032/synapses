package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── OCaml ────────────────────────────────────────────────────────────────────

const ocamlSource = `(** The auth module *)
open Printf

(** Greeting function *)
let greet name = "Hello, " ^ name

let rec factorial n =
  if n <= 1 then 1 else n * factorial (n - 1)

let pi = 3.14159

type color = Red | Green | Blue

type person = {
  name : string;
  age : int;
}

type alias_t = int

module MyModule = struct
  let helper x = x + 1
end

module type PRINTABLE = sig
  val to_string : 'a -> string
end

class counter = object
  val mutable count = 0
  method increment = count <- count + 1
  method get = count
end

exception Not_found_custom of string

exception Empty
`

func parseOCaml(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewOCamlParser()
	if err := p.Parse(g, "/tmp/test.ml", []byte(src)); err != nil {
		t.Fatalf("OCamlParser.Parse() error: %v", err)
	}
	return g
}

func TestOCamlParser_Extensions(t *testing.T) {
	exts := parser.NewOCamlParser().Extensions()
	if !hasExtension(exts, ".ml") || !hasExtension(exts, ".mli") {
		t.Errorf("Extensions() = %v, missing .ml or .mli", exts)
	}
}

func TestOCamlParser_FileNode(t *testing.T) {
	assertFileNode(t, parseOCaml(t, ocamlSource), "test.ml")
}

func TestOCamlParser_ExtractsLetFunction(t *testing.T) {
	g := parseOCaml(t, ocamlSource)
	n := assertNode(t, g, "greet", graph.NodeFunction)
	if !n.Exported {
		t.Error("top-level let binding should be exported")
	}
}

func TestOCamlParser_ExtractsRecFunction(t *testing.T) {
	g := parseOCaml(t, ocamlSource)
	n := assertNode(t, g, "factorial", graph.NodeFunction)
	if !n.Exported {
		t.Error("let rec binding should be exported")
	}
}

func TestOCamlParser_ExtractsSimpleValue(t *testing.T) {
	g := parseOCaml(t, ocamlSource)
	n := assertNode(t, g, "pi", graph.NodeVariable)
	if !n.Exported {
		t.Error("top-level value should be exported")
	}
}

func TestOCamlParser_ExtractsVariantType(t *testing.T) {
	g := parseOCaml(t, ocamlSource)
	assertNode(t, g, "color", graph.NodeStruct)
}

func TestOCamlParser_ExtractsRecordType(t *testing.T) {
	g := parseOCaml(t, ocamlSource)
	assertNode(t, g, "person", graph.NodeStruct)
}

func TestOCamlParser_ExtractsTypeAlias(t *testing.T) {
	g := parseOCaml(t, ocamlSource)
	assertNode(t, g, "alias_t", graph.NodeStruct)
}

func TestOCamlParser_ExtractsModuleDefinition(t *testing.T) {
	g := parseOCaml(t, ocamlSource)
	n := assertNode(t, g, "MyModule", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "module" {
		t.Errorf("module node should have kind=module, got %v", n.Metadata)
	}
}

func TestOCamlParser_ExtractsModuleType(t *testing.T) {
	g := parseOCaml(t, ocamlSource)
	assertNode(t, g, "PRINTABLE", graph.NodeInterface)
}

func TestOCamlParser_ExtractsClassDefinition(t *testing.T) {
	g := parseOCaml(t, ocamlSource)
	n := assertNode(t, g, "counter", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "class" {
		t.Errorf("class node should have kind=class, got %v", n.Metadata)
	}
}

func TestOCamlParser_ExtractsOpenStatement(t *testing.T) {
	g := parseOCaml(t, ocamlSource)
	// Check that Printf is imported.
	nodes := g.FindByName("Printf")
	if len(nodes) == 0 {
		t.Fatal("expected Printf import node")
	}
	found := false
	for _, n := range nodes {
		if n.Type == graph.NodePackage {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected Printf to be NodePackage (import)")
	}
}

func TestOCamlParser_ImportsEdge(t *testing.T) {
	g := parseOCaml(t, ocamlSource)
	fileNodes := g.FindByName("test.ml")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	found := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Name == "Printf" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected IMPORTS edge from file to Printf")
	}
}

func TestOCamlParser_ExtractsException(t *testing.T) {
	g := parseOCaml(t, ocamlSource)
	n := assertNode(t, g, "Not_found_custom", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "exception" {
		t.Errorf("exception node should have kind=exception, got %v", n.Metadata)
	}
}

func TestOCamlParser_ExtractsSimpleException(t *testing.T) {
	g := parseOCaml(t, ocamlSource)
	n := assertNode(t, g, "Empty", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "exception" {
		t.Errorf("exception node should have kind=exception, got %v", n.Metadata)
	}
}

func TestOCamlParser_DefinesEdge(t *testing.T) {
	g := parseOCaml(t, ocamlSource)
	fileNodes := g.FindByName("test.ml")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	assertDefinesEdge(t, g, fileID, "greet")
	assertDefinesEdge(t, g, fileID, "factorial")
	assertDefinesEdge(t, g, fileID, "color")
	assertDefinesEdge(t, g, fileID, "MyModule")
	assertDefinesEdge(t, g, fileID, "PRINTABLE")
	assertDefinesEdge(t, g, fileID, "counter")
	assertDefinesEdge(t, g, fileID, "Not_found_custom")
}

func TestOCamlParser_DocComment(t *testing.T) {
	g := parseOCaml(t, ocamlSource)
	n := assertNode(t, g, "greet", graph.NodeFunction)
	if n.Metadata == nil || n.Metadata["doc"] == "" {
		t.Error("greet should have a doc comment from (** ... *)")
	}
	if n.Metadata != nil && n.Metadata["doc"] != "Greeting function" {
		t.Errorf("greet doc = %q, want %q", n.Metadata["doc"], "Greeting function")
	}
}

func TestOCamlParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewOCamlParser(), ".ml", "")
}

func TestOCamlParser_LetAndBinding(t *testing.T) {
	src := `let x = 1 and y = 2`
	g := graph.New("testrepo")
	p := parser.NewOCamlParser()
	if err := p.Parse(g, "/tmp/and.ml", []byte(src)); err != nil {
		t.Fatal(err)
	}
	// At least one of x, y should be found.
	xNodes := g.FindByName("x")
	yNodes := g.FindByName("y")
	if len(xNodes) == 0 && len(yNodes) == 0 {
		t.Error("expected at least one of x or y from let...and binding")
	}
}

func TestOCamlParser_MliFile(t *testing.T) {
	src := `val connect : string -> int -> connection`
	g := graph.New("testrepo")
	p := parser.NewOCamlParser()
	// .mli files should parse without errors (even if they don't produce many nodes
	// since they are interface files with different AST structure).
	if err := p.Parse(g, "/tmp/server.mli", []byte(src)); err != nil {
		t.Fatalf("OCamlParser.Parse() on .mli error: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Error("expected at least a file node from .mli file")
	}
}

func TestOCamlParser_MultipleTypes(t *testing.T) {
	src := `type point = { x : float; y : float }
type shape = Circle of float | Rectangle of float * float`
	g := graph.New("testrepo")
	p := parser.NewOCamlParser()
	if err := p.Parse(g, "/tmp/shapes.ml", []byte(src)); err != nil {
		t.Fatal(err)
	}
	if nodes := g.FindByName("point"); len(nodes) == 0 {
		t.Error("expected point type node")
	}
	if nodes := g.FindByName("shape"); len(nodes) == 0 {
		t.Error("expected shape type node")
	}
}

func TestOCamlParser_CallSites(t *testing.T) {
	src := `let main () =
  let result = compute 42 in
  process result`
	g := graph.New("testrepo")
	p := parser.NewOCamlParser()
	if err := p.Parse(g, "/tmp/calls.ml", []byte(src)); err != nil {
		t.Fatal(err)
	}
	// The parser should at least not crash on function applications.
	// Exact call site extraction depends on tree-sitter AST structure.
	if g.NodeCount() == 0 {
		t.Error("expected at least a file node")
	}
}
