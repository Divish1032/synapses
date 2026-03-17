package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── CUE test helpers ────────────────────────────────────────────────────────

func parseCUE(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewCUEParser()
	if err := p.Parse(g, "/tmp/config.cue", []byte(src)); err != nil {
		t.Fatalf("CUEParser.Parse() error: %v", err)
	}
	return g
}

const cueSource = `package config

import "encoding/json"
import "list"

// Database connection schema
#Database: {
	host: string
	port: int | *5432
}

// Hidden internal schema
_#Internal: {
	secret: string
}

// Primary connection
connection: #Database & {
	host: "localhost"
}

let defaultPort = 5432

name: "myapp"
`

// ─── Extensions ──────────────────────────────────────────────────────────────

func TestCUEParser_Extensions(t *testing.T) {
	exts := parser.NewCUEParser().Extensions()
	if len(exts) != 1 || exts[0] != ".cue" {
		t.Errorf("Extensions() = %v, want [\".cue\"]", exts)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestCUEParser_FileNode(t *testing.T) {
	g := parseCUE(t, cueSource)
	nodes := g.FindByName("config.cue")
	if len(nodes) == 0 {
		t.Fatal("file node config.cue not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

// ─── Package declaration ─────────────────────────────────────────────────────

func TestCUEParser_PackageDeclaration(t *testing.T) {
	g := parseCUE(t, cueSource)
	nodes := g.FindByName("config.cue")
	if len(nodes) == 0 {
		t.Fatal("file node not found")
	}
	pkg := nodes[0].Metadata["package"]
	if pkg != "config" {
		t.Errorf("package = %q, want 'config'", pkg)
	}
}

// ─── Import extraction ───────────────────────────────────────────────────────

func TestCUEParser_ExtractsImports(t *testing.T) {
	g := parseCUE(t, cueSource)
	fileNodes := g.FindByName("config.cue")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	wantImports := map[string]bool{
		"encoding/json": false,
		"list":          false,
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

func TestCUEParser_GroupedImports(t *testing.T) {
	src := `package test

import (
	"encoding/json"
	"strings"
	"list"
)
`
	g := graph.New("testrepo")
	p := parser.NewCUEParser()
	if err := p.Parse(g, "/tmp/grouped.cue", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	fileNodes := g.FindByName("grouped.cue")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	var imports []string
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil {
				imports = append(imports, n.Name)
			}
		}
	}

	if len(imports) != 3 {
		t.Errorf("expected 3 imports, got %d: %v", len(imports), imports)
	}
}

// ─── Definition extraction (#Name) ──────────────────────────────────────────

func TestCUEParser_ExtractsDefinition(t *testing.T) {
	g := parseCUE(t, cueSource)
	nodeID := g.MakeNodeID("/tmp/config.cue", "#Database")
	node := g.GetNode(nodeID)
	if node == nil {
		t.Fatal("expected #Database definition node")
	}
	if node.Type != graph.NodeStruct {
		t.Errorf("#Database type = %q, want NodeStruct", node.Type)
	}
	if node.Metadata["kind"] != "definition" {
		t.Errorf("#Database kind = %q, want 'definition'", node.Metadata["kind"])
	}
	if !node.Exported {
		t.Error("#Database should be exported")
	}
}

func TestCUEParser_DefinitionDocComment(t *testing.T) {
	g := parseCUE(t, cueSource)
	nodeID := g.MakeNodeID("/tmp/config.cue", "#Database")
	node := g.GetNode(nodeID)
	if node == nil {
		t.Fatal("expected #Database definition node")
	}
	doc := node.Metadata["doc"]
	if doc != "Database connection schema" {
		t.Errorf("#Database doc = %q, want 'Database connection schema'", doc)
	}
}

// ─── Hidden definition (_#Name) ──────────────────────────────────────────────

func TestCUEParser_ExtractsHiddenDefinition(t *testing.T) {
	g := parseCUE(t, cueSource)
	nodeID := g.MakeNodeID("/tmp/config.cue", "_#Internal")
	node := g.GetNode(nodeID)
	if node == nil {
		t.Fatal("expected _#Internal hidden definition node")
	}
	if node.Type != graph.NodeStruct {
		t.Errorf("_#Internal type = %q, want NodeStruct", node.Type)
	}
	if node.Metadata["kind"] != "definition" {
		t.Errorf("_#Internal kind = %q, want 'definition'", node.Metadata["kind"])
	}
}

func TestCUEParser_HiddenDefinitionDocComment(t *testing.T) {
	g := parseCUE(t, cueSource)
	nodeID := g.MakeNodeID("/tmp/config.cue", "_#Internal")
	node := g.GetNode(nodeID)
	if node == nil {
		t.Fatal("expected _#Internal hidden definition node")
	}
	doc := node.Metadata["doc"]
	if doc != "Hidden internal schema" {
		t.Errorf("_#Internal doc = %q, want 'Hidden internal schema'", doc)
	}
}

// ─── Top-level field extraction ──────────────────────────────────────────────

func TestCUEParser_ExtractsField(t *testing.T) {
	g := parseCUE(t, cueSource)
	nodeID := g.MakeNodeID("/tmp/config.cue", "connection")
	node := g.GetNode(nodeID)
	if node == nil {
		t.Fatal("expected 'connection' field node")
	}
	if node.Type != graph.NodeVariable {
		t.Errorf("connection type = %q, want NodeVariable", node.Type)
	}
	if node.Metadata["kind"] != "field" {
		t.Errorf("connection kind = %q, want 'field'", node.Metadata["kind"])
	}
	if !node.Exported {
		t.Error("connection should be exported")
	}
}

func TestCUEParser_FieldDocComment(t *testing.T) {
	g := parseCUE(t, cueSource)
	nodeID := g.MakeNodeID("/tmp/config.cue", "connection")
	node := g.GetNode(nodeID)
	if node == nil {
		t.Fatal("expected 'connection' field node")
	}
	doc := node.Metadata["doc"]
	if doc != "Primary connection" {
		t.Errorf("connection doc = %q, want 'Primary connection'", doc)
	}
}

func TestCUEParser_ExtractsNameField(t *testing.T) {
	g := parseCUE(t, cueSource)
	nodeID := g.MakeNodeID("/tmp/config.cue", "name")
	node := g.GetNode(nodeID)
	if node == nil {
		t.Fatal("expected 'name' field node")
	}
	if node.Type != graph.NodeVariable {
		t.Errorf("name type = %q, want NodeVariable", node.Type)
	}
}

// ─── Let binding extraction ─────────────────────────────────────────────────

func TestCUEParser_ExtractsLetBinding(t *testing.T) {
	g := parseCUE(t, cueSource)
	nodeID := g.MakeNodeID("/tmp/config.cue", "defaultPort")
	node := g.GetNode(nodeID)
	if node == nil {
		t.Fatal("expected 'defaultPort' let binding node")
	}
	if node.Type != graph.NodeVariable {
		t.Errorf("defaultPort type = %q, want NodeVariable", node.Type)
	}
	if node.Metadata["kind"] != "let" {
		t.Errorf("defaultPort kind = %q, want 'let'", node.Metadata["kind"])
	}
}

// ─── DEFINES edges ───────────────────────────────────────────────────────────

func TestCUEParser_DefinesEdges(t *testing.T) {
	g := parseCUE(t, cueSource)
	fileNodes := g.FindByName("config.cue")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	wantNames := map[string]bool{
		"#Database":   false,
		"_#Internal":  false,
		"connection":  false,
		"defaultPort": false,
		"name":        false,
	}

	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeDefines {
			n := g.GetNode(e.To)
			if n != nil {
				if _, ok := wantNames[n.Name]; ok {
					wantNames[n.Name] = true
				}
			}
		}
	}

	for name, found := range wantNames {
		if !found {
			t.Errorf("no DEFINES edge from file to %s", name)
		}
	}
}

// ─── Empty file ──────────────────────────────────────────────────────────────

func TestCUEParser_EmptyFile(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCUEParser()
	if err := p.Parse(g, "/tmp/empty.cue", []byte("")); err != nil {
		t.Fatalf("Parse() on empty .cue returned error: %v", err)
	}
	nodes := g.FindByName("empty.cue")
	if len(nodes) == 0 {
		t.Error("Parse() produced zero nodes; expected at least a file node")
	}
}

// ─── Package-only file ───────────────────────────────────────────────────────

func TestCUEParser_PackageOnly(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCUEParser()
	src := []byte("package myconfig\n")
	if err := p.Parse(g, "/tmp/pkgonly.cue", src); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.FindByName("pkgonly.cue")
	if len(nodes) == 0 {
		t.Fatal("file node not found")
	}
	if nodes[0].Metadata["package"] != "myconfig" {
		t.Errorf("package = %q, want 'myconfig'", nodes[0].Metadata["package"])
	}
}

// ─── Multiple definitions ────────────────────────────────────────────────────

func TestCUEParser_MultipleDefinitions(t *testing.T) {
	src := `package schemas

#User: {
	name: string
	age:  int
}

#Role: {
	name:  string
	level: int
}

#Permission: {
	action: string
}
`
	g := graph.New("testrepo")
	p := parser.NewCUEParser()
	if err := p.Parse(g, "/tmp/schemas.cue", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	for _, name := range []string{"#User", "#Role", "#Permission"} {
		nodeID := g.MakeNodeID("/tmp/schemas.cue", name)
		node := g.GetNode(nodeID)
		if node == nil {
			t.Errorf("expected %s definition node", name)
			continue
		}
		if node.Type != graph.NodeStruct {
			t.Errorf("%s: type = %q, want NodeStruct", name, node.Type)
		}
		if node.Metadata["kind"] != "definition" {
			t.Errorf("%s: kind = %q, want 'definition'", name, node.Metadata["kind"])
		}
	}
}

// ─── Complex CUE file ────────────────────────────────────────────────────────

func TestCUEParser_ComplexFile(t *testing.T) {
	src := `package deploy

import "encoding/json"

// Kubernetes deployment spec
#Deployment: {
	apiVersion: "apps/v1"
	kind:       "Deployment"
	metadata: name: string
	spec: replicas: int | *1
}

// Service definition
#Service: {
	apiVersion: "v1"
	kind:       "Service"
}

app: #Deployment & {
	metadata: name: "myapp"
	spec: replicas: 3
}

svc: #Service & {
	metadata: name: "myapp-svc"
}

let environment = "production"
`
	g := graph.New("testrepo")
	p := parser.NewCUEParser()
	if err := p.Parse(g, "/tmp/deploy.cue", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	checks := []struct {
		name     string
		nodeType graph.NodeType
		kind     string
	}{
		{"#Deployment", graph.NodeStruct, "definition"},
		{"#Service", graph.NodeStruct, "definition"},
		{"app", graph.NodeVariable, "field"},
		{"svc", graph.NodeVariable, "field"},
		{"environment", graph.NodeVariable, "let"},
	}

	for _, tc := range checks {
		nodeID := g.MakeNodeID("/tmp/deploy.cue", tc.name)
		node := g.GetNode(nodeID)
		if node == nil {
			t.Errorf("expected %s node", tc.name)
			continue
		}
		if node.Type != tc.nodeType {
			t.Errorf("%s: type = %q, want %q", tc.name, node.Type, tc.nodeType)
		}
		if node.Metadata["kind"] != tc.kind {
			t.Errorf("%s: kind = %q, want %q", tc.name, node.Metadata["kind"], tc.kind)
		}
	}

	// Verify import edge for encoding/json.
	fileNodes := g.FindByName("deploy.cue")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	found := false
	for _, e := range g.OutEdges(fileNodes[0].ID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Name == "encoding/json" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected IMPORTS edge for encoding/json")
	}
}

// ─── String-labeled fields ───────────────────────────────────────────────────

func TestCUEParser_StringLabeledField(t *testing.T) {
	src := `package test

"my-field": "value"
`
	g := graph.New("testrepo")
	p := parser.NewCUEParser()
	if err := p.Parse(g, "/tmp/strfield.cue", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	nodeID := g.MakeNodeID("/tmp/strfield.cue", "my-field")
	node := g.GetNode(nodeID)
	if node == nil {
		t.Fatal("expected 'my-field' field node")
	}
	if node.Type != graph.NodeVariable {
		t.Errorf("my-field type = %q, want NodeVariable", node.Type)
	}
}
