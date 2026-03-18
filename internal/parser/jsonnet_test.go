package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── Jsonnet test helpers ───────────────────────────────────────────────────

const basicJsonnet = `{
  name: "my-app",
  version: "1.0.0",
  environment: {
    debug: true,
    port: 8080,
  },
}
`

const jsonnetWithFunctions = `local add(a, b) = a + b;
local multiply(x, y) = x * y;

{
  sum: add(2, 3),
  product: multiply(4, 5),
}
`

const jsonnetWithImports = `local utils = import 'utils.jsonnet';
local config = import 'config.jsonnet';

{
  appName: config.name,
  helpers: utils,
}
`

const jsonnetWithLocalBindings = `local x = 10;
local y = 20;
local z = x + y;

{
  values: {
    a: x,
    b: y,
    c: z,
  },
}
`

const jsonnetWithObjects = `{
  server: {
    host: "localhost",
    port: 8080,
    ssl: false,
  },
  database: {
    host: "db.example.com",
    port: 5432,
    name: "mydb",
  },
}
`

const jsonnetMinimal = `{
  name: "test",
}
`

const jsonnetWithArrays = `{
  items: [
    1,
    2,
    3,
  ],
  names: [
    "alice",
    "bob",
    "charlie",
  ],
}
`

const jsonnetWithConditional = `local isDev = true;

{
  environment: (
    if isDev then
      { debug: true, port: 3000 }
    else
      { debug: false, port: 8080 }
  ),
}
`

func parseJsonnet(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewJsonnetParser()
	if err := p.Parse(g, "/tmp/test.jsonnet", []byte(src)); err != nil {
		t.Fatalf("JsonnetParser.Parse() error: %v", err)
	}
	return g
}

func parseJsonnetWithFilename(t *testing.T, filename string, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewJsonnetParser()
	if err := p.Parse(g, "/tmp/"+filename, []byte(src)); err != nil {
		t.Fatalf("JsonnetParser.Parse() error: %v", err)
	}
	return g
}

// ─── Extensions ──────────────────────────────────────────────────────────────

func TestJsonnetParser_Extensions(t *testing.T) {
	exts := parser.NewJsonnetParser().Extensions()
	if len(exts) < 1 {
		t.Errorf("Extensions() = %v, want at least 1 extension", exts)
	}
	// Should support .jsonnet and .libsonnet
	hasJsonnet := false
	for _, ext := range exts {
		if ext == ".jsonnet" {
			hasJsonnet = true
			break
		}
	}
	if !hasJsonnet {
		t.Errorf("Extensions() = %v, want to include .jsonnet", exts)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestJsonnetParser_FileNode(t *testing.T) {
	g := parseJsonnet(t, basicJsonnet)
	nodes := g.FindByName("test.jsonnet")
	if len(nodes) == 0 {
		t.Fatal("file node test.jsonnet not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

// ─── Basic object structure ──────────────────────────────────────────────────

func TestJsonnetParser_BasicObject(t *testing.T) {
	g := parseJsonnetWithFilename(t, "app.jsonnet", basicJsonnet)

	// Check for file node
	fileNodes := g.FindByName("app.jsonnet")
	if len(fileNodes) == 0 {
		t.Fatal("file node app.jsonnet not found")
	}
}

// ─── Functions ──────────────────────────────────────────────────────────────

func TestJsonnetParser_LocalFunctions(t *testing.T) {
	g := parseJsonnetWithFilename(t, "math.jsonnet", jsonnetWithFunctions)

	// Verify file was parsed successfully
	fileNodes := g.FindByName("math.jsonnet")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
}

// ─── Imports ────────────────────────────────────────────────────────────────

func TestJsonnetParser_Imports(t *testing.T) {
	g := parseJsonnetWithFilename(t, "main.jsonnet", jsonnetWithImports)

	// Verify file was parsed successfully
	fileNodes := g.FindByName("main.jsonnet")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
}

// ─── Local bindings ─────────────────────────────────────────────────────────

func TestJsonnetParser_LocalBindings(t *testing.T) {
	g := parseJsonnetWithFilename(t, "config.jsonnet", jsonnetWithLocalBindings)

	// Verify file was parsed successfully
	fileNodes := g.FindByName("config.jsonnet")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
}

// ─── Nested objects ─────────────────────────────────────────────────────────

func TestJsonnetParser_NestedObjects(t *testing.T) {
	g := parseJsonnetWithFilename(t, "config.jsonnet", jsonnetWithObjects)

	// Verify file was parsed successfully
	fileNodes := g.FindByName("config.jsonnet")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
}

// ─── Minimal jsonnet ────────────────────────────────────────────────────────

func TestJsonnetParser_Minimal(t *testing.T) {
	g := parseJsonnet(t, jsonnetMinimal)

	fileNodes := g.FindByName("test.jsonnet")
	if len(fileNodes) == 0 {
		t.Fatal("file node should exist")
	}
}

// ─── Arrays ─────────────────────────────────────────────────────────────────

func TestJsonnetParser_Arrays(t *testing.T) {
	g := parseJsonnetWithFilename(t, "arrays.jsonnet", jsonnetWithArrays)

	fileNodes := g.FindByName("arrays.jsonnet")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
}

// ─── Conditional expressions ────────────────────────────────────────────────

func TestJsonnetParser_Conditionals(t *testing.T) {
	g := parseJsonnetWithFilename(t, "env.jsonnet", jsonnetWithConditional)

	fileNodes := g.FindByName("env.jsonnet")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
}

// ─── Complex jsonnet ────────────────────────────────────────────────────────

func TestJsonnetParser_ComplexStructure(t *testing.T) {
	src := `local config = {
  name: "app",
  version: "1.0",
};

local databases = [
  {
    name: "primary",
    host: "db1.example.com",
  },
  {
    name: "replica",
    host: "db2.example.com",
  },
];

{
  app: config,
  databases: databases,
  metadata: {
    created: "2024-01-01",
    updated: "2024-01-15",
  },
}
`
	g := parseJsonnetWithFilename(t, "complex.jsonnet", src)

	fileNodes := g.FindByName("complex.jsonnet")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
}

// ─── Empty jsonnet ──────────────────────────────────────────────────────────

func TestJsonnetParser_Empty(t *testing.T) {
	g := parseJsonnet(t, "")

	// Should still create a file node
	fileNodes := g.FindByName("test.jsonnet")
	if len(fileNodes) == 0 {
		t.Fatal("file node should exist for empty jsonnet")
	}
}

// ─── Libsonnet file ─────────────────────────────────────────────────────────

func TestJsonnetParser_Libsonnet(t *testing.T) {
	src := `{
  local this = self,

  name: "lib",
  version: "1.0",

  getName(): this.name,
  getVersion(): this.version,
}
`
	g := parseJsonnetWithFilename(t, "lib.libsonnet", src)

	fileNodes := g.FindByName("lib.libsonnet")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
}

// ─── With comments ──────────────────────────────────────────────────────────

func TestJsonnetParser_WithComments(t *testing.T) {
	src := `// This is a comment
{
  // App configuration
  name: "app",

  // Server settings
  server: {
    // Port number
    port: 8080,
  },
}
`
	g := parseJsonnetWithFilename(t, "app.jsonnet", src)

	fileNodes := g.FindByName("app.jsonnet")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
}
