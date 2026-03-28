package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── Svelte ──────────────────────────────────────────────────────────────────

const svelteSource = `<script>
  import { onMount } from 'svelte';
  import Button from './Button.svelte';
  import { writable } from 'svelte/store';

  let count = 0;
  const API_URL = 'https://api.example.com';
  export let name;
  export let title = 'Default';

  $: doubled = count * 2;
  $: greeting = 'Hello ' + name;

  // Increment the counter
  function increment() {
    count += 1;
  }

  function reset() {
    count = 0;
  }
</script>

<div>
  <h1>{title}</h1>
  <p>Hello {name}! Count is {count}, doubled is {doubled}.</p>
  <Button on:click={increment}>Click me</Button>
  <button on:click={reset}>Reset</button>
</div>

<style>
  h1 { color: red; }
</style>
`

func parseSvelte(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewSvelteParser()
	if err := p.Parse(g, "/tmp/App.svelte", []byte(src)); err != nil {
		t.Fatalf("SvelteParser.Parse() error: %v", err)
	}
	return g
}

// ─── Extensions ──────────────────────────────────────────────────────────────

func TestSvelteParser_Extensions(t *testing.T) {
	exts := parser.NewSvelteParser().Extensions()
	if !hasExtension(exts, ".svelte") {
		t.Errorf("Extensions() = %v, missing .svelte", exts)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestSvelteParser_FileNode(t *testing.T) {
	assertFileNode(t, parseSvelte(t, svelteSource), "App.svelte")
}

// ─── Imports ─────────────────────────────────────────────────────────────────

func TestSvelteParser_ExtractsImports(t *testing.T) {
	g := parseSvelte(t, svelteSource)
	fileID := g.FindByName("App.svelte")[0].ID
	importCount := 0
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}
	if importCount != 3 {
		t.Errorf("expected 3 import edges, got %d", importCount)
	}
}

func TestSvelteParser_ImportSvelteModule(t *testing.T) {
	g := parseSvelte(t, svelteSource)
	nodes := g.FindByName("svelte")
	found := false
	for _, n := range nodes {
		if n.Type == graph.NodePackage {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("import node for svelte (NodePackage) not found")
	}
}

func TestSvelteParser_ImportComponentPath(t *testing.T) {
	g := parseSvelte(t, svelteSource)
	nodes := g.FindByName("./Button.svelte")
	if len(nodes) == 0 {
		t.Fatal("import node for ./Button.svelte not found")
	}
	if nodes[0].Type != graph.NodePackage {
		t.Errorf("import node type = %q, want NodePackage", nodes[0].Type)
	}
}

func TestSvelteParser_ImportSvelteStore(t *testing.T) {
	g := parseSvelte(t, svelteSource)
	nodes := g.FindByName("svelte/store")
	if len(nodes) == 0 {
		t.Fatal("import node for svelte/store not found")
	}
	if nodes[0].Type != graph.NodePackage {
		t.Errorf("import node type = %q, want NodePackage", nodes[0].Type)
	}
}

// ─── Exported props ──────────────────────────────────────────────────────────

func TestSvelteParser_ExtractsExportedProp(t *testing.T) {
	g := parseSvelte(t, svelteSource)
	n := assertNode(t, g, "name", graph.NodeVariable)
	if !n.Exported {
		t.Error("export let name should be exported")
	}
	if n.Metadata == nil || n.Metadata["kind"] != "prop" {
		t.Errorf("name metadata kind = %q, want prop", n.Metadata["kind"])
	}
}

func TestSvelteParser_ExtractsExportedPropWithDefault(t *testing.T) {
	g := parseSvelte(t, svelteSource)
	n := assertNode(t, g, "title", graph.NodeVariable)
	if !n.Exported {
		t.Error("export let title should be exported")
	}
	if n.Metadata == nil || n.Metadata["kind"] != "prop" {
		t.Errorf("title metadata kind = %q, want prop", n.Metadata["kind"])
	}
}

// ─── Reactive declarations ───────────────────────────────────────────────────

func TestSvelteParser_ExtractsReactiveDeclaration(t *testing.T) {
	g := parseSvelte(t, svelteSource)
	n := assertNode(t, g, "doubled", graph.NodeVariable)
	if n.Exported {
		t.Error("reactive declaration doubled should not be exported")
	}
	if n.Metadata == nil || n.Metadata["kind"] != "reactive" {
		t.Errorf("doubled metadata kind = %q, want reactive", n.Metadata["kind"])
	}
}

func TestSvelteParser_ExtractsMultipleReactiveDeclarations(t *testing.T) {
	g := parseSvelte(t, svelteSource)
	assertNode(t, g, "doubled", graph.NodeVariable)
	assertNode(t, g, "greeting", graph.NodeVariable)
}

// ─── Function declarations ───────────────────────────────────────────────────

func TestSvelteParser_ExtractsFunction(t *testing.T) {
	g := parseSvelte(t, svelteSource)
	n := assertNode(t, g, "increment", graph.NodeFunction)
	if n.Exported {
		t.Error("increment function should not be exported")
	}
}

func TestSvelteParser_ExtractsMultipleFunctions(t *testing.T) {
	g := parseSvelte(t, svelteSource)
	assertNode(t, g, "increment", graph.NodeFunction)
	assertNode(t, g, "reset", graph.NodeFunction)
}

func TestSvelteParser_FunctionDocComment(t *testing.T) {
	g := parseSvelte(t, svelteSource)
	n := assertNode(t, g, "increment", graph.NodeFunction)
	if n.Metadata == nil {
		t.Fatal("increment should have metadata")
	}
	doc := n.Metadata["doc"]
	if doc == "" {
		t.Error("increment should have a doc comment")
	}
}

// ─── Script-level variables ──────────────────────────────────────────────────

func TestSvelteParser_ExtractsLetVariable(t *testing.T) {
	g := parseSvelte(t, svelteSource)
	n := assertNode(t, g, "count", graph.NodeVariable)
	if n.Exported {
		t.Error("count should not be exported")
	}
}

func TestSvelteParser_ExtractsConstVariable(t *testing.T) {
	g := parseSvelte(t, svelteSource)
	n := assertNode(t, g, "API_URL", graph.NodeVariable)
	if n.Exported {
		t.Error("API_URL should not be exported")
	}
}

// ─── Component usage ─────────────────────────────────────────────────────────

func TestSvelteParser_DetectsComponentUsage(t *testing.T) {
	g := parseSvelte(t, svelteSource)
	sites := g.PeekCallSites()
	foundButton := false
	for _, cs := range sites {
		if cs.FuncName == "Button" {
			foundButton = true
			break
		}
	}
	if !foundButton {
		t.Error("expected call site for Button component usage")
	}
}

// ─── DEFINES edges ───────────────────────────────────────────────────────────

func TestSvelteParser_DefinesEdges(t *testing.T) {
	g := parseSvelte(t, svelteSource)
	fileID := g.FindByName("App.svelte")[0].ID
	assertDefinesEdge(t, g, fileID, "name")
	assertDefinesEdge(t, g, fileID, "title")
	assertDefinesEdge(t, g, fileID, "doubled")
	assertDefinesEdge(t, g, fileID, "increment")
	assertDefinesEdge(t, g, fileID, "count")
	assertDefinesEdge(t, g, fileID, "API_URL")
}

// ─── Empty / minimal files ───────────────────────────────────────────────────

func TestSvelteParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewSvelteParser(), ".svelte", "")
}

func TestSvelteParser_TemplateOnly(t *testing.T) {
	src := `<div>Hello world</div>`
	g := graph.New("testrepo")
	p := parser.NewSvelteParser()
	if err := p.Parse(g, "/tmp/Simple.svelte", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.FindByName("Simple.svelte")
	if len(nodes) == 0 {
		t.Fatal("file node not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

func TestSvelteParser_StyleOnly(t *testing.T) {
	src := `<style>
  h1 { color: blue; }
</style>
`
	g := graph.New("testrepo")
	p := parser.NewSvelteParser()
	if err := p.Parse(g, "/tmp/Styled.svelte", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.FindByName("Styled.svelte")
	if len(nodes) == 0 {
		t.Fatal("file node not found")
	}
}

// ─── Module context script ───────────────────────────────────────────────────

func TestSvelteParser_ModuleScript(t *testing.T) {
	src := `<script context="module">
  export const COLORS = ['red', 'green', 'blue'];
</script>

<script>
  export let selected;

  function pick(color) {
    selected = color;
  }
</script>

<div>{selected}</div>
`
	g := graph.New("testrepo")
	p := parser.NewSvelteParser()
	if err := p.Parse(g, "/tmp/Picker.svelte", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// Module-level export const
	n := assertNode(t, g, "COLORS", graph.NodeVariable)
	if !n.Exported {
		t.Error("COLORS should be exported")
	}

	// Instance-level export let
	n2 := assertNode(t, g, "selected", graph.NodeVariable)
	if !n2.Exported {
		t.Error("selected should be exported")
	}

	// Instance-level function
	assertNode(t, g, "pick", graph.NodeFunction)
}

// ─── Complex component ──────────────────────────────────────────────────────

func TestSvelteParser_ComplexComponent(t *testing.T) {
	src := `<script>
  import Header from './Header.svelte';
  import Footer from './Footer.svelte';
  import { fade } from 'svelte/transition';

  export let items = [];

  let filter = '';

  $: filtered = items.filter(i => i.name.includes(filter));

  function clearFilter() {
    filter = '';
  }
</script>

<Header />
<main>
  {#each filtered as item}
    <div transition:fade>{item.name}</div>
  {/each}
</main>
<Footer />
`
	g := graph.New("testrepo")
	p := parser.NewSvelteParser()
	if err := p.Parse(g, "/tmp/List.svelte", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// Imports
	fileID := g.FindByName("List.svelte")[0].ID
	importCount := 0
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}
	if importCount != 3 {
		t.Errorf("expected 3 import edges, got %d", importCount)
	}

	// Props
	n := assertNode(t, g, "items", graph.NodeVariable)
	if !n.Exported {
		t.Error("items should be exported")
	}

	// Local variable
	assertNode(t, g, "filter", graph.NodeVariable)

	// Reactive
	n2 := assertNode(t, g, "filtered", graph.NodeVariable)
	if n2.Metadata == nil || n2.Metadata["kind"] != "reactive" {
		t.Errorf("filtered kind = %q, want reactive", n2.Metadata["kind"])
	}

	// Function
	assertNode(t, g, "clearFilter", graph.NodeFunction)

	// Component usage call sites
	sites := g.PeekCallSites()
	compNames := make(map[string]bool)
	for _, cs := range sites {
		compNames[cs.FuncName] = true
	}
	for _, want := range []string{"Header", "Footer"} {
		if !compNames[want] {
			t.Errorf("expected call site for %s component", want)
		}
	}
}
