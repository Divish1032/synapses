package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── Bash test helpers ───────────────────────────────────────────────────────

const bashSource = `#!/bin/bash

# Greet the user
# with a hello message
function greet() {
  echo "Hello $1"
  log_message "greeting sent"
}

# Deploy the application
deploy() {
  echo "Deploying..."
  greet "world"
  run_tests
}

source ./lib/helpers.sh
. /etc/profile.d/custom.sh

# Shortcut for listing
alias ll='ls -la'
alias gs='git status'
`

func parseBash(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewBashParser()
	if err := p.Parse(g, "/tmp/deploy.sh", []byte(src)); err != nil {
		t.Fatalf("BashParser.Parse() error: %v", err)
	}
	return g
}

// ─── Extensions ──────────────────────────────────────────────────────────────

func TestBashParser_Extensions(t *testing.T) {
	exts := parser.NewBashParser().Extensions()
	hasSH, hasBash := false, false
	for _, e := range exts {
		if e == ".sh" {
			hasSH = true
		}
		if e == ".bash" {
			hasBash = true
		}
	}
	if !hasSH || !hasBash {
		t.Errorf("Extensions() = %v, missing .sh or .bash", exts)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestBashParser_FileNode(t *testing.T) {
	g := parseBash(t, bashSource)
	nodes := g.FindByName("deploy.sh")
	if len(nodes) == 0 {
		t.Fatal("file node deploy.sh not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

// ─── Function extraction (function keyword style) ────────────────────────────

func TestBashParser_ExtractsFunctionKeywordStyle(t *testing.T) {
	g := parseBash(t, bashSource)
	nodes := g.FindByName("greet")
	if len(nodes) == 0 {
		t.Fatal("expected greet function node")
	}
	n := nodes[0]
	if n.Type != graph.NodeFunction {
		t.Errorf("greet: type = %q, want NodeFunction", n.Type)
	}
	if !n.Exported {
		t.Error("greet should be exported (bash has no visibility modifiers)")
	}
}

// ─── Function extraction (bare name style) ───────────────────────────────────

func TestBashParser_ExtractsFunctionBareStyle(t *testing.T) {
	g := parseBash(t, bashSource)
	nodes := g.FindByName("deploy")
	if len(nodes) == 0 {
		t.Fatal("expected deploy function node")
	}
	n := nodes[0]
	if n.Type != graph.NodeFunction {
		t.Errorf("deploy: type = %q, want NodeFunction", n.Type)
	}
	if !n.Exported {
		t.Error("deploy should be exported")
	}
}

// ─── Doc comment extraction ──────────────────────────────────────────────────

func TestBashParser_ExtractsDocComment(t *testing.T) {
	g := parseBash(t, bashSource)
	nodes := g.FindByName("greet")
	if len(nodes) == 0 {
		t.Fatal("expected greet function node")
	}
	doc := nodes[0].Metadata["doc"]
	if doc == "" {
		t.Error("greet should have a doc comment")
	}
	if doc != "Greet the user with a hello message" {
		t.Errorf("greet doc = %q, want %q", doc, "Greet the user with a hello message")
	}
}

func TestBashParser_ExtractsDocCommentDeploy(t *testing.T) {
	g := parseBash(t, bashSource)
	nodes := g.FindByName("deploy")
	if len(nodes) == 0 {
		t.Fatal("expected deploy function node")
	}
	doc := nodes[0].Metadata["doc"]
	if doc == "" {
		t.Error("deploy should have a doc comment")
	}
	if doc != "Deploy the application" {
		t.Errorf("deploy doc = %q, want %q", doc, "Deploy the application")
	}
}

// ─── Source/dot imports ──────────────────────────────────────────────────────

func TestBashParser_ExtractsSourceImport(t *testing.T) {
	g := parseBash(t, bashSource)
	fileNodes := g.FindByName("deploy.sh")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	found := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Package == "./lib/helpers.sh" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected import edge for source ./lib/helpers.sh")
	}
}

func TestBashParser_ExtractsDotImport(t *testing.T) {
	g := parseBash(t, bashSource)
	fileNodes := g.FindByName("deploy.sh")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	found := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Package == "/etc/profile.d/custom.sh" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected import edge for . /etc/profile.d/custom.sh")
	}
}

func TestBashParser_ImportCount(t *testing.T) {
	g := parseBash(t, bashSource)
	fileNodes := g.FindByName("deploy.sh")
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
	if importCount != 2 {
		t.Errorf("expected 2 import edges (source + dot), got %d", importCount)
	}
}

// ─── Alias extraction ────────────────────────────────────────────────────────

func TestBashParser_ExtractsAlias(t *testing.T) {
	g := parseBash(t, bashSource)
	nodes := g.FindByName("ll")
	if len(nodes) == 0 {
		t.Fatal("expected ll alias node")
	}
	n := nodes[0]
	if n.Type != graph.NodeFunction {
		t.Errorf("ll: type = %q, want NodeFunction", n.Type)
	}
	if n.Metadata["kind"] != "alias" {
		t.Errorf("ll: metadata[kind] = %q, want 'alias'", n.Metadata["kind"])
	}
}

func TestBashParser_ExtractsMultipleAliases(t *testing.T) {
	g := parseBash(t, bashSource)
	for _, name := range []string{"ll", "gs"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected %s alias node", name)
			continue
		}
		if nodes[0].Metadata["kind"] != "alias" {
			t.Errorf("%s: metadata[kind] = %q, want 'alias'", name, nodes[0].Metadata["kind"])
		}
	}
}

func TestBashParser_AliasDocComment(t *testing.T) {
	g := parseBash(t, bashSource)
	nodes := g.FindByName("ll")
	if len(nodes) == 0 {
		t.Fatal("expected ll alias node")
	}
	doc := nodes[0].Metadata["doc"]
	if doc != "Shortcut for listing" {
		t.Errorf("ll doc = %q, want 'Shortcut for listing'", doc)
	}
}

// ─── Call site collection ────────────────────────────────────────────────────

func TestBashParser_CallSiteFromFunction(t *testing.T) {
	g := parseBash(t, bashSource)
	sites := g.PeekCallSites()

	// deploy() calls greet and run_tests
	foundGreet, foundRunTests := false, false
	for _, cs := range sites {
		if cs.FuncName == "greet" {
			foundGreet = true
		}
		if cs.FuncName == "run_tests" {
			foundRunTests = true
		}
	}
	if !foundGreet {
		t.Error("expected call site for greet from deploy")
	}
	if !foundRunTests {
		t.Error("expected call site for run_tests from deploy")
	}
}

func TestBashParser_CallSiteCallerResolution(t *testing.T) {
	g := parseBash(t, bashSource)
	sites := g.PeekCallSites()

	// log_message is called from greet — verify caller resolution
	for _, cs := range sites {
		if cs.FuncName == "log_message" {
			callerNode := g.GetNode(cs.CallerID)
			if callerNode == nil {
				t.Fatal("caller node for log_message not found")
			}
			if callerNode.Name != "greet" {
				t.Errorf("log_message caller = %q, want 'greet'", callerNode.Name)
			}
			return
		}
	}
	t.Error("expected call site for log_message")
}

func TestBashParser_BuiltinsFiltered(t *testing.T) {
	g := parseBash(t, bashSource)
	sites := g.PeekCallSites()

	for _, cs := range sites {
		if cs.FuncName == "echo" {
			t.Error("echo should be filtered as a builtin")
		}
	}
}

// ─── DEFINES edges ───────────────────────────────────────────────────────────

func TestBashParser_DefinesEdgeForFunction(t *testing.T) {
	g := parseBash(t, bashSource)
	fileNodes := g.FindByName("deploy.sh")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	foundGreet, foundDeploy := false, false
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeDefines {
			n := g.GetNode(e.To)
			if n == nil {
				continue
			}
			if n.Name == "greet" {
				foundGreet = true
			}
			if n.Name == "deploy" {
				foundDeploy = true
			}
		}
	}
	if !foundGreet {
		t.Error("no DEFINES edge from file to greet")
	}
	if !foundDeploy {
		t.Error("no DEFINES edge from file to deploy")
	}
}

// ─── Empty/minimal file ─────────────────────────────────────────────────────

func TestBashParser_EmptyFile(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewBashParser()
	if err := p.Parse(g, "empty.sh", []byte("")); err != nil {
		t.Fatalf("Parse() on empty .sh returned error: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Error("Parse() produced zero nodes; expected at least a file node")
	}
}

func TestBashParser_ShebangOnly(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewBashParser()
	src := []byte("#!/bin/bash\n")
	if err := p.Parse(g, "shebang.sh", src); err != nil {
		t.Fatalf("Parse() on shebang-only .sh returned error: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Error("Parse() produced zero nodes; expected at least a file node")
	}
}

// ─── Complex source with nested calls ────────────────────────────────────────

func TestBashParser_ComplexScript(t *testing.T) {
	src := `#!/bin/bash

source ./config.sh

# Initialize the database
init_db() {
  create_tables
  seed_data
}

function cleanup() {
  remove_temp_files
}

# Run all tasks
function main() {
  init_db
  cleanup
  notify_admin
}

alias dc='docker-compose'

main "$@"
`
	g := graph.New("testrepo")
	p := parser.NewBashParser()
	if err := p.Parse(g, "/app/run.sh", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// Check functions
	for _, name := range []string{"init_db", "cleanup", "main"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected %s function node", name)
			continue
		}
		if nodes[0].Type != graph.NodeFunction {
			t.Errorf("%s: type = %q, want NodeFunction", name, nodes[0].Type)
		}
	}

	// Check import
	fileNodes := g.FindByName("run.sh")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	importFound := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Package == "./config.sh" {
				importFound = true
			}
		}
	}
	if !importFound {
		t.Error("expected import edge for ./config.sh")
	}

	// Check alias
	dcNodes := g.FindByName("dc")
	if len(dcNodes) == 0 {
		t.Error("expected dc alias node")
	} else if dcNodes[0].Metadata["kind"] != "alias" {
		t.Errorf("dc: metadata[kind] = %q, want 'alias'", dcNodes[0].Metadata["kind"])
	}

	// Check call sites include function-to-function calls
	sites := g.PeekCallSites()
	callees := make(map[string]bool)
	for _, cs := range sites {
		callees[cs.FuncName] = true
	}
	for _, expected := range []string{"create_tables", "seed_data", "remove_temp_files", "init_db", "cleanup", "notify_admin", "main"} {
		if !callees[expected] {
			t.Errorf("expected call site for %q", expected)
		}
	}
}
