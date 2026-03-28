package parser

import (
	"testing"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CSS: verify @font-face is actually captured
func TestCSS_FontFace(t *testing.T) {
	src := []byte(`
@font-face {
  font-family: "MyFont";
  src: url("my-font.woff2");
}
@keyframes slide { from { } to { } }
`)
	g := graph.New("test")
	require.NoError(t, NewCSSParser().Parse(g, "test.css", src))
	vars := g.FindByType(graph.NodeVariable)
	names := make([]string, 0)
	for _, v := range vars { names = append(names, v.Name) }
	assert.Contains(t, names, "MyFont", "@font-face font-family should be captured")
}

// OCaml: verify include_module creates EdgeImports
func TestOCaml_Include(t *testing.T) {
	src := []byte(`
include Stdlib.List
let x = 1
`)
	g := graph.New("test")
	require.NoError(t, NewOCamlParser().Parse(g, "test.ml", src))
	edges := g.AllEdges()
	hasInclude := false
	for _, e := range edges {
		if e.Type == graph.EdgeImports {
			hasInclude = true
			break
		}
	}
	assert.True(t, hasInclude, "include Stdlib.List should emit EdgeImports")
}

// Elm: verify call sites are recorded (call sites are resolved later by resolver.ResolveCallEdges)
func TestElm_CallSites(t *testing.T) {
	src := []byte(`
module Main exposing (..)

renderItem : String -> String
renderItem item = item

view : List String -> String
view items = renderItem "hello"
`)
	g := graph.New("test")
	require.NoError(t, NewElmParser().Parse(g, "test.elm", src))
	sites := g.PeekCallSites()
	t.Logf("call sites: %+v", sites)
	hasRenderItem := false
	for _, s := range sites {
		if s.FuncName == "renderItem" {
			hasRenderItem = true
			break
		}
	}
	assert.True(t, hasRenderItem, "view calling renderItem should register a call site for 'renderItem'")
}

// HCL: verify var.x reference creates EdgeCalls (same-file)
func TestHCL_ReferenceEdges(t *testing.T) {
	src := []byte(`
variable "region" {
  default = "us-east-1"
}
resource "aws_instance" "web" {
  ami    = "ami-12345"
  region = var.region
}
`)
	g := graph.New("test")
	require.NoError(t, NewHCLParser().Parse(g, "main.tf", src))
	edges := g.AllEdges()
	hasCalls := false
	for _, e := range edges {
		if e.Type == graph.EdgeCalls { hasCalls = true; break }
	}
	assert.True(t, hasCalls, "resource referencing var.region should produce EdgeCalls")
}

// Dockerfile: verify CMD is captured
func TestDockerfile_CMD(t *testing.T) {
	src := []byte(`FROM ubuntu:22.04
CMD ["/app/server", "--port", "8080"]
`)
	g := graph.New("test")
	require.NoError(t, NewDockerfileParser().Parse(g, "Dockerfile", src))
	fns := g.FindByType(graph.NodeFunction)
	names := make([]string, 0)
	for _, n := range fns { names = append(names, n.Name) }
	assert.NotEmpty(t, fns, "CMD should create at least one NodeFunction")
	t.Logf("NodeFunction names: %v", names)
}

// Svelte: verify async function is captured
func TestSvelte_AsyncFunction(t *testing.T) {
	src := []byte(`<script>
async function fetchData() {
  const res = await fetch('/api/data');
  return res.json();
}
const handleClick = (e) => {
  e.preventDefault();
}
</script>
<div>hello</div>
`)
	g := graph.New("test")
	require.NoError(t, NewSvelteParser().Parse(g, "App.svelte", src))
	fns := g.FindByType(graph.NodeFunction)
	names := make([]string, 0)
	for _, n := range fns { names = append(names, n.Name) }
	assert.Contains(t, names, "fetchData", "async function should be NodeFunction")
	assert.Contains(t, names, "handleClick", "arrow function should be NodeFunction")
	t.Logf("NodeFunction names: %v", names)
}

// SCSS: verify @include creates EdgeCalls for same-file mixin
func TestSCSS_IncludeEdgeCalls(t *testing.T) {
	src := []byte(`
@mixin flex-center {
  display: flex;
  align-items: center;
}
.container {
  @include flex-center;
  color: red;
}
`)
	g := graph.New("test")
	require.NoError(t, NewSCSSParser().Parse(g, "test.scss", src))
	edges := g.AllEdges()
	hasCalls := false
	for _, e := range edges {
		if e.Type == graph.EdgeCalls { hasCalls = true; break }
	}
	assert.True(t, hasCalls, "@include flex-center should produce EdgeCalls to flex-center mixin")
}

// CUE: verify line_count is correct for multi-line fields
func TestCUE_LineCountCorrect(t *testing.T) {
	src := []byte(`package config

#Database: {
  host: string
  port: int
  name: string
  user: string
  password: string
  sslmode: string
  maxConns: int
  timeout: int
  keepalive: bool
}
`)
	g := graph.New("test")
	require.NoError(t, NewCUEParser().Parse(g, "config.cue", src))
	structs := g.FindByType(graph.NodeStruct)
	var dbNode *graph.Node
	for _, n := range structs {
		if n.Name == "#Database" { dbNode = n; break }
	}
	require.NotNil(t, dbNode, "#Database definition must exist")
	lc := dbNode.Metadata["line_count"]
	t.Logf("line_count = %q", lc)
	assert.NotEmpty(t, lc)
	// Must be a proper number, not a garbage character
	assert.NotContains(t, ";<=>?@ABCDE", lc, "line_count must be a digit string, not a garbage char")
	// #Database: { ... } spans 11 lines (from "#Database: {" to closing "}"), which
	// tree-sitter reports as EndRow - StartRow + 1 = 12 - 2 + 1 = 11.
	assert.Equal(t, "11", lc, "11-line block (#Database: { ... }) should have line_count=11")
}
