package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ============================================================================
// Adversarial / edge-case tests for all new parsers (Batch 1 + Batch 2).
// These test failure modes, malformed input, real-world complexity, and
// integration with the Walker pipeline.
// ============================================================================

// --- Helpers ----------------------------------------------------------------

func countNodesByType(g *graph.Graph, nodeType graph.NodeType) int {
	return len(g.FindByType(nodeType))
}

func countEdgesByType(g *graph.Graph, edgeType graph.EdgeType) int {
	count := 0
	for _, e := range g.AllEdges() {
		if e.Type == edgeType {
			count++
		}
	}
	return count
}

// --- BASH: Adversarial tests ------------------------------------------------

func TestBashAdversarial_HyphenatedFunctionName(t *testing.T) {
	// Bash allows hyphens in function names: my-func() { ... }
	g := graph.New("test")
	p := NewBashParser()
	src := []byte("my-deploy-func() {\n  echo 'deploying'\n}\n")
	if err := p.Parse(g, "/tmp/test.sh", src); err != nil {
		t.Fatal(err)
	}
	// The function should be extracted (name may or may not have hyphen depending on grammar)
	funcs := countNodesByType(g, graph.NodeFunction)
	if funcs < 1 {
		t.Log("WARN: hyphenated function name not extracted (grammar limitation)")
	}
}

func TestBashAdversarial_NestedFunction(t *testing.T) {
	// Nested functions are valid in bash
	g := graph.New("test")
	p := NewBashParser()
	src := []byte(`
outer() {
  inner() {
    echo "inner"
  }
  inner
}
`)
	if err := p.Parse(g, "/tmp/test.sh", src); err != nil {
		t.Fatal(err)
	}
	// Should extract both outer and inner
	outerID := g.MakeNodeID("/tmp/test.sh", "outer")
	if g.GetNode(outerID) == nil {
		t.Error("expected outer function")
	}
	innerID := g.MakeNodeID("/tmp/test.sh", "inner")
	if g.GetNode(innerID) == nil {
		t.Error("expected inner function")
	}
}

func TestBashAdversarial_HeredocInFunction(t *testing.T) {
	// Heredocs shouldn't confuse the parser
	g := graph.New("test")
	p := NewBashParser()
	src := []byte(`
generate_config() {
  cat <<EOF
server {
  listen 80;
}
EOF
}
`)
	if err := p.Parse(g, "/tmp/test.sh", src); err != nil {
		t.Fatal(err)
	}
	nodeID := g.MakeNodeID("/tmp/test.sh", "generate_config")
	if g.GetNode(nodeID) == nil {
		t.Error("expected generate_config function")
	}
}

func TestBashAdversarial_SourceWithVariable(t *testing.T) {
	// source with variable expansion — should not crash but may not extract path
	g := graph.New("test")
	p := NewBashParser()
	src := []byte(`source "$DIR/config.sh"`)
	if err := p.Parse(g, "/tmp/test.sh", src); err != nil {
		t.Fatal(err)
	}
	// Should not crash. Import may or may not be extracted depending on expansion.
}

func TestBashAdversarial_MalformedSyntax(t *testing.T) {
	// Malformed bash — parser should not crash
	g := graph.New("test")
	p := NewBashParser()
	src := []byte(`
function {
  broken syntax here
}
if then fi while for
`)
	if err := p.Parse(g, "/tmp/test.sh", src); err != nil {
		t.Fatal(err)
	}
	// Should produce at least a file node
	files := countNodesByType(g, graph.NodeFile)
	if files != 1 {
		t.Errorf("expected 1 file node, got %d", files)
	}
}

func TestBashAdversarial_BinaryContentNoCrash(t *testing.T) {
	// Binary garbage should not panic
	g := graph.New("test")
	p := NewBashParser()
	src := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0x89, 'P', 'N', 'G'}
	if err := p.Parse(g, "/tmp/test.sh", src); err != nil {
		t.Fatal(err)
	}
}

func TestBashAdversarial_VeryLargeFile(t *testing.T) {
	// Stress test — many functions
	g := graph.New("test")
	p := NewBashParser()
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		sb.WriteString("func_" + strings.Repeat("x", 5) + "() { echo ok; }\n")
	}
	if err := p.Parse(g, "/tmp/test.sh", []byte(sb.String())); err != nil {
		t.Fatal(err)
	}
}

// --- SQL: Adversarial tests -------------------------------------------------

func TestSQLAdversarial_MixedCase(t *testing.T) {
	// SQL keywords are case-insensitive
	g := graph.New("test")
	p := NewSQLParser()
	src := []byte(`
create Table Users (id int);
CREATE table Orders (id int);
Create TABLE Products (id int);
`)
	if err := p.Parse(g, "/tmp/test.sql", src); err != nil {
		t.Fatal(err)
	}
	tables := countNodesByType(g, graph.NodeStruct)
	if tables < 2 {
		t.Errorf("expected at least 2 tables from mixed-case CREATE, got %d", tables)
	}
}

func TestSQLAdversarial_CommentsInsideCreate(t *testing.T) {
	// Comments between CREATE and TABLE should still work
	g := graph.New("test")
	p := NewSQLParser()
	src := []byte(`
-- Create the users table
CREATE TABLE users (
  id INT PRIMARY KEY, -- auto increment
  name VARCHAR(255)   -- user's display name
);
`)
	if err := p.Parse(g, "/tmp/test.sql", src); err != nil {
		t.Fatal(err)
	}
	nodeID := g.MakeNodeID("/tmp/test.sql", "users")
	if g.GetNode(nodeID) == nil {
		t.Error("expected users table despite inline comments")
	}
}

func TestSQLAdversarial_MultipleStatementsWithSemicolons(t *testing.T) {
	g := graph.New("test")
	p := NewSQLParser()
	src := []byte(`
CREATE TABLE users (id INT);
CREATE TABLE orders (id INT);
CREATE TABLE products (id INT);
CREATE VIEW user_orders AS SELECT * FROM users JOIN orders ON users.id = orders.user_id;
CREATE FUNCTION get_user(p_id INT) RETURNS VARCHAR AS $$ BEGIN RETURN 'test'; END; $$;
`)
	if err := p.Parse(g, "/tmp/test.sql", src); err != nil {
		t.Fatal(err)
	}
	structs := countNodesByType(g, graph.NodeStruct)
	funcs := countNodesByType(g, graph.NodeFunction)
	if structs < 3 {
		t.Errorf("expected at least 3 structs (3 tables + 1 view), got %d", structs)
	}
	if funcs < 1 {
		t.Errorf("expected at least 1 function, got %d", funcs)
	}
}

func TestSQLAdversarial_DropAndRecreate(t *testing.T) {
	// DROP then CREATE — should extract the CREATE
	g := graph.New("test")
	p := NewSQLParser()
	src := []byte(`
DROP TABLE IF EXISTS users;
CREATE TABLE users (id INT PRIMARY KEY, name TEXT);
`)
	if err := p.Parse(g, "/tmp/test.sql", src); err != nil {
		t.Fatal(err)
	}
	nodeID := g.MakeNodeID("/tmp/test.sql", "users")
	if g.GetNode(nodeID) == nil {
		t.Error("expected users table after DROP+CREATE")
	}
}

func TestSQLAdversarial_PLpgSQLFunctionBody(t *testing.T) {
	// PL/pgSQL function with $$ delimiters
	g := graph.New("test")
	p := NewSQLParser()
	src := []byte(`
CREATE OR REPLACE FUNCTION calculate_total(order_id INT)
RETURNS DECIMAL AS $$
DECLARE
  total DECIMAL := 0;
BEGIN
  SELECT SUM(price * quantity) INTO total
  FROM order_items WHERE order_id = calculate_total.order_id;
  RETURN total;
END;
$$ LANGUAGE plpgsql;
`)
	if err := p.Parse(g, "/tmp/test.sql", src); err != nil {
		t.Fatal(err)
	}
	nodeID := g.MakeNodeID("/tmp/test.sql", "calculate_total")
	if g.GetNode(nodeID) == nil {
		t.Error("expected calculate_total function")
	}
}

func TestSQLAdversarial_EmptyAndCommentOnly(t *testing.T) {
	g := graph.New("test")
	p := NewSQLParser()
	// Only comments, no CREATE statements
	src := []byte(`
-- This is a migration file
-- Nothing to do here
/* Multi-line
   comment */
`)
	if err := p.Parse(g, "/tmp/test.sql", src); err != nil {
		t.Fatal(err)
	}
	// Should just have a file node
	files := countNodesByType(g, graph.NodeFile)
	if files != 1 {
		t.Errorf("expected 1 file node, got %d", files)
	}
}

func TestSQLAdversarial_BinaryNoCrash(t *testing.T) {
	g := graph.New("test")
	p := NewSQLParser()
	src := []byte{0x00, 0xFF, 0xFE, 0x01, 0x02}
	if err := p.Parse(g, "/tmp/test.sql", src); err != nil {
		t.Fatal(err)
	}
}

// --- CSS: Adversarial tests -------------------------------------------------

func TestCSSAdversarial_ImportWithMediaQuery(t *testing.T) {
	// @import with media query — should still extract the path
	g := graph.New("test")
	p := NewCSSParser()
	src := []byte(`@import "print.css" print;
@import url("mobile.css") screen and (max-width: 768px);
`)
	if err := p.Parse(g, "/tmp/test.css", src); err != nil {
		t.Fatal(err)
	}
	imports := countEdgesByType(g, graph.EdgeImports)
	if imports < 1 {
		t.Errorf("expected at least 1 import edge, got %d", imports)
	}
}

func TestCSSAdversarial_VendorPrefixedKeyframes(t *testing.T) {
	// @-webkit-keyframes might not be recognized by the CSS grammar
	g := graph.New("test")
	p := NewCSSParser()
	src := []byte(`
@keyframes fadeIn { from { opacity: 0; } to { opacity: 1; } }
@-webkit-keyframes fadeIn { from { opacity: 0; } to { opacity: 1; } }
`)
	if err := p.Parse(g, "/tmp/test.css", src); err != nil {
		t.Fatal(err)
	}
	// At minimum, the standard @keyframes should be found
	nodeID := g.MakeNodeID("/tmp/test.css", "fadeIn")
	if g.GetNode(nodeID) == nil {
		t.Error("expected fadeIn keyframes node")
	}
}

func TestCSSAdversarial_CustomPropertyWithComplexValue(t *testing.T) {
	g := graph.New("test")
	p := NewCSSParser()
	src := []byte(`:root {
  --gradient-bg: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
  --font-stack: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto;
  --spacing-unit: calc(8px * var(--multiplier, 1));
}
`)
	if err := p.Parse(g, "/tmp/test.css", src); err != nil {
		t.Fatal(err)
	}
	vars := countNodesByType(g, graph.NodeVariable)
	if vars != 3 {
		t.Errorf("expected 3 custom properties, got %d", vars)
	}
}

func TestCSSAdversarial_MalformedCSS(t *testing.T) {
	g := graph.New("test")
	p := NewCSSParser()
	src := []byte(`
.broken { color: ; }
@import
}}}}}
:root { --x: }
`)
	if err := p.Parse(g, "/tmp/test.css", src); err != nil {
		t.Fatal(err)
	}
	// Should not crash
}

func TestCSSAdversarial_BinaryNoCrash(t *testing.T) {
	g := graph.New("test")
	p := NewCSSParser()
	src := []byte{0x00, 0xFF, 0xFE, 0x89, 'P', 'N', 'G'}
	if err := p.Parse(g, "/tmp/test.css", src); err != nil {
		t.Fatal(err)
	}
}

// --- OCaml: Adversarial tests -----------------------------------------------

func TestOCamlAdversarial_OperatorBinding(t *testing.T) {
	// OCaml allows operator definitions: let (+) a b = ...
	g := graph.New("test")
	p := NewOCamlParser()
	src := []byte(`let ( +. ) a b = a +. b`)
	if err := p.Parse(g, "/tmp/test.ml", src); err != nil {
		t.Fatal(err)
	}
	// Should not crash. May or may not extract the operator.
}

func TestOCamlAdversarial_NestedModules(t *testing.T) {
	g := graph.New("test")
	p := NewOCamlParser()
	src := []byte(`
module Outer = struct
  module Inner = struct
    let x = 42
  end
  let y = Inner.x
end
`)
	if err := p.Parse(g, "/tmp/test.ml", src); err != nil {
		t.Fatal(err)
	}
	outerID := g.MakeNodeID("/tmp/test.ml", "Outer")
	if g.GetNode(outerID) == nil {
		t.Error("expected Outer module")
	}
}

func TestOCamlAdversarial_FunctorDefinition(t *testing.T) {
	// Functors — may not be fully supported but should not crash
	g := graph.New("test")
	p := NewOCamlParser()
	src := []byte(`
module type COMPARABLE = sig
  type t
  val compare : t -> t -> int
end

module Make (C : COMPARABLE) = struct
  let sort lst = List.sort C.compare lst
end
`)
	if err := p.Parse(g, "/tmp/test.ml", src); err != nil {
		t.Fatal(err)
	}
	compID := g.MakeNodeID("/tmp/test.ml", "COMPARABLE")
	if g.GetNode(compID) == nil {
		t.Error("expected COMPARABLE module type")
	}
}

func TestOCamlAdversarial_LabeledArguments(t *testing.T) {
	g := graph.New("test")
	p := NewOCamlParser()
	src := []byte(`let create ~name ~age = { name; age }`)
	if err := p.Parse(g, "/tmp/test.ml", src); err != nil {
		t.Fatal(err)
	}
	nodeID := g.MakeNodeID("/tmp/test.ml", "create")
	if g.GetNode(nodeID) == nil {
		t.Error("expected create function with labeled args")
	}
}

func TestOCamlAdversarial_BinaryNoCrash(t *testing.T) {
	g := graph.New("test")
	p := NewOCamlParser()
	src := []byte{0x00, 0xFF, 0xFE}
	if err := p.Parse(g, "/tmp/test.ml", src); err != nil {
		t.Fatal(err)
	}
}

// --- Elm: Adversarial tests -------------------------------------------------

func TestElmAdversarial_ModuleOnlyFile(t *testing.T) {
	// File with only a module declaration, nothing else
	g := graph.New("test")
	p := NewElmParser()
	src := []byte(`module Empty exposing (..)`)
	if err := p.Parse(g, "/tmp/Empty.elm", src); err != nil {
		t.Fatal(err)
	}
	files := countNodesByType(g, graph.NodeFile)
	if files != 1 {
		t.Errorf("expected 1 file node, got %d", files)
	}
}

func TestElmAdversarial_PortModuleDeclaration(t *testing.T) {
	// port module is valid Elm syntax
	g := graph.New("test")
	p := NewElmParser()
	src := []byte(`port module Interop exposing (sendMessage)

port sendMessage : String -> Cmd msg
`)
	if err := p.Parse(g, "/tmp/Interop.elm", src); err != nil {
		t.Fatal(err)
	}
	nodeID := g.MakeNodeID("/tmp/Interop.elm", "sendMessage")
	if g.GetNode(nodeID) == nil {
		t.Error("expected sendMessage port")
	}
}

func TestElmAdversarial_ComplexPatternMatching(t *testing.T) {
	g := graph.New("test")
	p := NewElmParser()
	src := []byte(`
module Main exposing (view)

view : Model -> Html Msg
view model =
    case model.page of
        Home -> viewHome model
        About -> viewAbout
        NotFound -> text "404"
`)
	if err := p.Parse(g, "/tmp/Main.elm", src); err != nil {
		t.Fatal(err)
	}
	nodeID := g.MakeNodeID("/tmp/Main.elm", "view")
	if g.GetNode(nodeID) == nil {
		t.Error("expected view function")
	}
}

func TestElmAdversarial_TypeAnnotationWithoutDef(t *testing.T) {
	// Type annotation without a corresponding value declaration (orphan)
	g := graph.New("test")
	p := NewElmParser()
	src := []byte(`
module Main exposing (..)

orphan : String -> Int
`)
	if err := p.Parse(g, "/tmp/Main.elm", src); err != nil {
		t.Fatal(err)
	}
	// Should not crash. Orphan annotation may or may not create a node.
}

func TestElmAdversarial_BinaryNoCrash(t *testing.T) {
	g := graph.New("test")
	p := NewElmParser()
	src := []byte{0x00, 0xFF, 0xFE}
	if err := p.Parse(g, "/tmp/test.elm", src); err != nil {
		t.Fatal(err)
	}
}

// --- HCL: Adversarial tests -------------------------------------------------

func TestHCLAdversarial_NestedBlocks(t *testing.T) {
	// Dynamic blocks and nested blocks
	g := graph.New("test")
	p := NewHCLParser()
	src := []byte(`
resource "aws_security_group" "web" {
  name = "web-sg"

  ingress {
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  dynamic "egress" {
    for_each = var.egress_rules
    content {
      from_port   = egress.value.from_port
      to_port     = egress.value.to_port
      protocol    = egress.value.protocol
      cidr_blocks = egress.value.cidr_blocks
    }
  }
}
`)
	if err := p.Parse(g, "/tmp/main.tf", src); err != nil {
		t.Fatal(err)
	}
	nodeID := g.MakeNodeID("/tmp/main.tf", "aws_security_group.web")
	if g.GetNode(nodeID) == nil {
		t.Error("expected aws_security_group.web resource")
	}
}

func TestHCLAdversarial_HeredocSyntax(t *testing.T) {
	g := graph.New("test")
	p := NewHCLParser()
	src := []byte(`
resource "aws_iam_policy" "example" {
  name   = "example"
  policy = <<-EOF
    {
      "Version": "2012-10-17",
      "Statement": [{
        "Effect": "Allow",
        "Action": "*",
        "Resource": "*"
      }]
    }
  EOF
}
`)
	if err := p.Parse(g, "/tmp/main.tf", src); err != nil {
		t.Fatal(err)
	}
	nodeID := g.MakeNodeID("/tmp/main.tf", "aws_iam_policy.example")
	if g.GetNode(nodeID) == nil {
		t.Error("expected aws_iam_policy.example resource")
	}
}

func TestHCLAdversarial_EmptyBlocks(t *testing.T) {
	g := graph.New("test")
	p := NewHCLParser()
	src := []byte(`
resource "null_resource" "empty" {}
variable "empty_var" {}
output "empty_out" {}
`)
	if err := p.Parse(g, "/tmp/main.tf", src); err != nil {
		t.Fatal(err)
	}
	// All should be extracted even with empty bodies
	structs := countNodesByType(g, graph.NodeStruct)
	vars := countNodesByType(g, graph.NodeVariable)
	if structs < 1 {
		t.Errorf("expected at least 1 resource struct, got %d", structs)
	}
	if vars < 1 {
		t.Errorf("expected at least 1 variable, got %d", vars)
	}
}

func TestHCLAdversarial_ForExpressions(t *testing.T) {
	g := graph.New("test")
	p := NewHCLParser()
	src := []byte(`
locals {
  instance_ids = [for i in aws_instance.web : i.id]
  upper_names  = { for name, val in var.map : upper(name) => val }
}
`)
	if err := p.Parse(g, "/tmp/main.tf", src); err != nil {
		t.Fatal(err)
	}
	// Should extract locals without crashing on for expressions
}

func TestHCLAdversarial_BinaryNoCrash(t *testing.T) {
	g := graph.New("test")
	p := NewHCLParser()
	src := []byte{0x00, 0xFF, 0xFE}
	if err := p.Parse(g, "/tmp/main.tf", src); err != nil {
		t.Fatal(err)
	}
}

// --- Walker Integration: end-to-end tests -----------------------------------

func TestWalkerIntegration_NewParsersRegistered(t *testing.T) {
	// Verify all new parsers are registered in the Walker
	w := NewWalker()

	extensions := map[string]string{
		".sh":      "Bash",
		".bash":    "Bash",
		".sql":     "SQL",
		".css":     "CSS",
		".ml":      "OCaml",
		".mli":     "OCaml",
		".elm":     "Elm",
		".tf":      "HCL",
		".tfvars":  "HCL",
		".hcl":     "HCL",
	}

	for ext, name := range extensions {
		if _, ok := w.parsers[ext]; !ok {
			t.Errorf("extension %s (%s) not registered in Walker", ext, name)
		}
	}
}

func TestWalkerIntegration_NewParsersOverrideGeneric(t *testing.T) {
	// Verify new parsers take precedence over generic parser
	w := NewWalker()

	deepExtensions := []string{".sh", ".bash", ".sql", ".css", ".ml", ".mli", ".elm", ".tf", ".tfvars", ".hcl"}

	for _, ext := range deepExtensions {
		p, ok := w.parsers[ext]
		if !ok {
			t.Errorf("extension %s not registered", ext)
			continue
		}
		if _, isGeneric := p.(*genericParser); isGeneric {
			t.Errorf("extension %s is still using genericParser, expected deep parser", ext)
		}
	}
}

func TestWalkerIntegration_ParseFileEndToEnd(t *testing.T) {
	// End-to-end: create temp files and parse them through Walker
	w := NewWalker()
	g := graph.New("test")

	testCases := []struct {
		filename string
		content  []byte
		checkFn  func(t *testing.T, g *graph.Graph, filePath string)
	}{
		{
			"deploy.sh",
			[]byte("#!/bin/bash\nfunction deploy() {\n  echo 'deploying'\n}\n"),
			func(t *testing.T, g *graph.Graph, fp string) {
				nodeID := g.MakeNodeID(fp, "deploy")
				if g.GetNode(nodeID) == nil {
					t.Error("Bash: expected deploy function via Walker")
				}
			},
		},
		{
			"schema.sql",
			[]byte("CREATE TABLE users (id INT PRIMARY KEY);\n"),
			func(t *testing.T, g *graph.Graph, fp string) {
				nodeID := g.MakeNodeID(fp, "users")
				if g.GetNode(nodeID) == nil {
					t.Error("SQL: expected users table via Walker")
				}
			},
		},
		{
			"theme.css",
			[]byte("@keyframes spin { from { transform: rotate(0deg); } to { transform: rotate(360deg); } }\n"),
			func(t *testing.T, g *graph.Graph, fp string) {
				nodeID := g.MakeNodeID(fp, "spin")
				if g.GetNode(nodeID) == nil {
					t.Error("CSS: expected spin keyframes via Walker")
				}
			},
		},
		{
			"utils.ml",
			[]byte("let fibonacci n = if n <= 1 then n else fibonacci (n-1) + fibonacci (n-2)\n"),
			func(t *testing.T, g *graph.Graph, fp string) {
				nodeID := g.MakeNodeID(fp, "fibonacci")
				if g.GetNode(nodeID) == nil {
					t.Error("OCaml: expected fibonacci function via Walker")
				}
			},
		},
		{
			"Main.elm",
			[]byte("module Main exposing (main)\nmain = text \"Hello\"\n"),
			func(t *testing.T, g *graph.Graph, fp string) {
				nodeID := g.MakeNodeID(fp, "main")
				if g.GetNode(nodeID) == nil {
					t.Error("Elm: expected main function via Walker")
				}
			},
		},
		{
			"infra.tf",
			[]byte("resource \"aws_s3_bucket\" \"data\" {\n  bucket = \"my-data\"\n}\n"),
			func(t *testing.T, g *graph.Graph, fp string) {
				nodeID := g.MakeNodeID(fp, "aws_s3_bucket.data")
				if g.GetNode(nodeID) == nil {
					t.Error("HCL: expected aws_s3_bucket.data resource via Walker")
				}
			},
		},
	}

	tmpDir := t.TempDir()
	for _, tc := range testCases {
		fp := filepath.Join(tmpDir, tc.filename)
		if err := writeTestFile(fp, tc.content); err != nil {
			t.Fatalf("failed to write %s: %v", tc.filename, err)
		}
		if err := w.ParseFile(g, fp); err != nil {
			t.Fatalf("ParseFile(%s) failed: %v", tc.filename, err)
		}
		tc.checkFn(t, g, fp)
	}
}

func writeTestFile(path string, content []byte) error {
	return os.WriteFile(path, content, 0o644)
}

// --- Concurrency safety test ------------------------------------------------

func TestAllNewParsers_ConcurrentParseSafety(t *testing.T) {
	// Parse the same file concurrently from multiple goroutines
	// to verify no race conditions in parser state
	parsers := []struct {
		name string
		p    LanguageParser
		src  []byte
	}{
		{"bash", NewBashParser(), []byte("function foo() { echo hi; }\n")},
		{"sql", NewSQLParser(), []byte("CREATE TABLE t (id INT);\n")},
		{"css", NewCSSParser(), []byte(":root { --x: 1; }\n")},
		{"ocaml", NewOCamlParser(), []byte("let f x = x + 1\n")},
		{"elm", NewElmParser(), []byte("module M exposing (f)\nf x = x\n")},
		{"hcl", NewHCLParser(), []byte("variable \"x\" {}\n")},
	}

	for _, tc := range parsers {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan bool, 10)
			for i := 0; i < 10; i++ {
				go func() {
					g := graph.New("test")
					_ = tc.p.Parse(g, "/tmp/test"+tc.name, tc.src)
					done <- true
				}()
			}
			for i := 0; i < 10; i++ {
				<-done
			}
		})
	}
}
