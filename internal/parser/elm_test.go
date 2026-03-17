package parser_test

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── Elm ──────────────────────────────────────────────────────────────────────

const elmSource = `module Main exposing (main, greet, Model, Msg)

import Html exposing (Html, text)
import Html.Attributes

{-| A greeting function that takes a name and returns a greeting string.
-}
greet : String -> String
greet name =
    "Hello, " ++ name

{-| The main entry point.
-}
main =
    text (greet "World")

type alias Model =
    { name : String
    , age : Int
    }

type Msg
    = Increment
    | Decrement
    | Reset

port sendMessage : String -> Cmd msg

port messageReceiver : (String -> msg) -> Sub msg
`

func parseElm(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewElmParser()
	if err := p.Parse(g, "/tmp/Main.elm", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	return g
}

func TestElmParser_Extensions(t *testing.T) {
	exts := parser.NewElmParser().Extensions()
	if !hasExtension(exts, ".elm") {
		t.Errorf("Extensions() = %v, missing .elm", exts)
	}
}

func TestElmParser_FileNode(t *testing.T) {
	assertFileNode(t, parseElm(t, elmSource), "Main.elm")
}

func TestElmParser_ModuleDeclaration(t *testing.T) {
	g := parseElm(t, elmSource)
	fileNodes := g.FindByName("Main.elm")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fn := fileNodes[0]
	if fn.Metadata == nil {
		t.Fatal("file node has no metadata")
	}
	if fn.Metadata["module"] != "Main" {
		t.Errorf("module = %q, want %q", fn.Metadata["module"], "Main")
	}
}

func TestElmParser_ExtractsFunction(t *testing.T) {
	g := parseElm(t, elmSource)
	n := assertNode(t, g, "greet", graph.NodeFunction)
	if !n.Exported {
		t.Error("greet should be exported")
	}
}

func TestElmParser_ExtractsFunctionWithAnnotation(t *testing.T) {
	g := parseElm(t, elmSource)
	n := assertNode(t, g, "greet", graph.NodeFunction)
	if n.Metadata == nil {
		t.Fatal("greet should have metadata")
	}
	sig := n.Metadata["signature"]
	if sig == "" {
		t.Error("greet should have a signature from its type annotation")
	}
	if !strings.Contains(sig, "String -> String") {
		t.Errorf("signature = %q, expected it to contain 'String -> String'", sig)
	}
}

func TestElmParser_ExtractsMainFunction(t *testing.T) {
	g := parseElm(t, elmSource)
	assertNode(t, g, "main", graph.NodeFunction)
}

func TestElmParser_ExtractsTypeAlias(t *testing.T) {
	g := parseElm(t, elmSource)
	n := assertNode(t, g, "Model", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "type_alias" {
		t.Errorf("Model kind = %q, want %q", n.Metadata["kind"], "type_alias")
	}
}

func TestElmParser_ExtractsCustomType(t *testing.T) {
	g := parseElm(t, elmSource)
	n := assertNode(t, g, "Msg", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "custom_type" {
		t.Errorf("Msg kind = %q, want %q", n.Metadata["kind"], "custom_type")
	}
}

func TestElmParser_ExtractsPort(t *testing.T) {
	g := parseElm(t, elmSource)
	n := assertNode(t, g, "sendMessage", graph.NodeFunction)
	if n.Metadata == nil || n.Metadata["kind"] != "port" {
		t.Errorf("sendMessage kind = %q, want %q", n.Metadata["kind"], "port")
	}
}

func TestElmParser_ExtractsMultiplePorts(t *testing.T) {
	g := parseElm(t, elmSource)
	assertNode(t, g, "sendMessage", graph.NodeFunction)
	assertNode(t, g, "messageReceiver", graph.NodeFunction)
}

func TestElmParser_ExtractsImports(t *testing.T) {
	g := parseElm(t, elmSource)
	fileID := g.FindByName("Main.elm")[0].ID
	importCount := 0
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}
	if importCount < 2 {
		t.Errorf("expected >= 2 import edges, got %d", importCount)
	}
}

func TestElmParser_ImportModuleName(t *testing.T) {
	g := parseElm(t, elmSource)
	// Html.Attributes should be imported as a dotted name.
	nodes := g.FindByName("Html.Attributes")
	if len(nodes) == 0 {
		t.Fatal("expected Html.Attributes import node")
	}
	if nodes[0].Type != graph.NodePackage {
		t.Errorf("Html.Attributes type = %q, want NodePackage", nodes[0].Type)
	}
}

func TestElmParser_DefinesEdges(t *testing.T) {
	g := parseElm(t, elmSource)
	fileID := g.FindByName("Main.elm")[0].ID
	assertDefinesEdge(t, g, fileID, "greet")
	assertDefinesEdge(t, g, fileID, "main")
	assertDefinesEdge(t, g, fileID, "Model")
	assertDefinesEdge(t, g, fileID, "Msg")
	assertDefinesEdge(t, g, fileID, "sendMessage")
}

func TestElmParser_DocComment(t *testing.T) {
	g := parseElm(t, elmSource)
	n := assertNode(t, g, "greet", graph.NodeFunction)
	if n.Metadata == nil {
		t.Fatal("greet should have metadata")
	}
	doc := n.Metadata["doc"]
	if doc == "" {
		t.Error("greet should have a doc comment")
	}
	if !strings.Contains(doc, "greeting") {
		t.Errorf("doc = %q, expected it to contain 'greeting'", doc)
	}
}

func TestElmParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewElmParser(), ".elm", "")
}

func TestElmParser_MinimalModule(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewElmParser()
	src := []byte(`module Minimal exposing (..)

identity x = x
`)
	if err := p.Parse(g, "/tmp/Minimal.elm", src); err != nil {
		t.Fatal(err)
	}
	assertNode(t, g, "identity", graph.NodeFunction)
}

func TestElmParser_LineCountMetadata(t *testing.T) {
	g := parseElm(t, elmSource)
	n := assertNode(t, g, "greet", graph.NodeFunction)
	if n.Metadata == nil {
		t.Fatal("greet should have metadata")
	}
	lc := n.Metadata["line_count"]
	if lc == "" {
		t.Error("greet should have line_count metadata")
	}
}

func TestElmParser_PortSignature(t *testing.T) {
	g := parseElm(t, elmSource)
	n := assertNode(t, g, "sendMessage", graph.NodeFunction)
	if n.Metadata == nil {
		t.Fatal("sendMessage should have metadata")
	}
	sig := n.Metadata["signature"]
	if sig == "" {
		t.Error("sendMessage port should have a signature")
	}
}

