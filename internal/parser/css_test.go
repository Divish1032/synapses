package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── CSS ─────────────────────────────────────────────────────────────────────

const cssSource = `@import "reset.css";
@import url("theme.css");

:root {
    --color-primary: #3498db;
    --font-size-base: 16px;
    --spacing-lg: 2rem;
}

@keyframes fadeIn {
    from { opacity: 0; }
    to { opacity: 1; }
}

@keyframes slideUp {
    0% { transform: translateY(100%); }
    100% { transform: translateY(0); }
}

body {
    color: var(--color-primary);
    font-size: var(--font-size-base);
}

.header {
    background: #fff;
    padding: 10px;
}
`

func parseCSS(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewCSSParser()
	if err := p.Parse(g, "/tmp/style.css", []byte(src)); err != nil {
		t.Fatalf("CSSParser.Parse() error: %v", err)
	}
	return g
}

func TestCSSParser_Extensions(t *testing.T) {
	exts := parser.NewCSSParser().Extensions()
	if !hasExtension(exts, ".css") {
		t.Errorf("Extensions() = %v, missing .css", exts)
	}
}

func TestCSSParser_FileNode(t *testing.T) {
	assertFileNode(t, parseCSS(t, cssSource), "style.css")
}

func TestCSSParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewCSSParser(), ".css", "")
}

func TestCSSParser_ExtractsImport(t *testing.T) {
	g := parseCSS(t, cssSource)
	fileID := g.FindByName("style.css")[0].ID
	importCount := 0
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}
	if importCount != 2 {
		t.Errorf("expected 2 import edges, got %d", importCount)
	}
}

func TestCSSParser_ImportStringPath(t *testing.T) {
	g := parseCSS(t, cssSource)
	nodes := g.FindByName("reset.css")
	if len(nodes) == 0 {
		t.Fatal("import node for reset.css not found")
	}
	if nodes[0].Type != graph.NodePackage {
		t.Errorf("import node type = %q, want NodePackage", nodes[0].Type)
	}
}

func TestCSSParser_ImportURLPath(t *testing.T) {
	g := parseCSS(t, cssSource)
	nodes := g.FindByName("theme.css")
	if len(nodes) == 0 {
		t.Fatal("import node for theme.css not found")
	}
	if nodes[0].Type != graph.NodePackage {
		t.Errorf("import node type = %q, want NodePackage", nodes[0].Type)
	}
}

func TestCSSParser_ExtractsKeyframes(t *testing.T) {
	g := parseCSS(t, cssSource)
	n := assertNode(t, g, "fadeIn", graph.NodeFunction)
	if !n.Exported {
		t.Error("keyframes fadeIn should be exported")
	}
	if n.Metadata == nil || n.Metadata["kind"] != "keyframes" {
		t.Errorf("fadeIn metadata kind = %q, want keyframes", n.Metadata["kind"])
	}
}

func TestCSSParser_ExtractsMultipleKeyframes(t *testing.T) {
	g := parseCSS(t, cssSource)
	assertNode(t, g, "slideUp", graph.NodeFunction)
}

func TestCSSParser_KeyframesDefinesEdge(t *testing.T) {
	g := parseCSS(t, cssSource)
	fileID := g.FindByName("style.css")[0].ID
	assertDefinesEdge(t, g, fileID, "fadeIn")
	assertDefinesEdge(t, g, fileID, "slideUp")
}

func TestCSSParser_ExtractsCustomProperties(t *testing.T) {
	g := parseCSS(t, cssSource)
	n := assertNode(t, g, "--color-primary", graph.NodeVariable)
	if !n.Exported {
		t.Error("custom property --color-primary should be exported")
	}
	if n.Metadata == nil || n.Metadata["kind"] != "custom-property" {
		t.Errorf("--color-primary metadata kind = %q, want custom-property", n.Metadata["kind"])
	}
}

func TestCSSParser_ExtractsAllCustomProperties(t *testing.T) {
	g := parseCSS(t, cssSource)
	assertNode(t, g, "--color-primary", graph.NodeVariable)
	assertNode(t, g, "--font-size-base", graph.NodeVariable)
	assertNode(t, g, "--spacing-lg", graph.NodeVariable)
}

func TestCSSParser_CustomPropertyDefinesEdge(t *testing.T) {
	g := parseCSS(t, cssSource)
	fileID := g.FindByName("style.css")[0].ID
	assertDefinesEdge(t, g, fileID, "--color-primary")
	assertDefinesEdge(t, g, fileID, "--font-size-base")
	assertDefinesEdge(t, g, fileID, "--spacing-lg")
}

func TestCSSParser_IgnoresRegularProperties(t *testing.T) {
	g := parseCSS(t, cssSource)
	// Regular properties like "color", "font-size", "background" should NOT be nodes.
	for _, name := range []string{"color", "font-size", "background", "padding"} {
		nodes := g.FindByName(name)
		if len(nodes) > 0 {
			t.Errorf("regular property %q should not produce a node", name)
		}
	}
}

func TestCSSParser_OnlyImports(t *testing.T) {
	src := `@import "a.css";
@import "b.css";
@import url("c.css");
`
	g := graph.New("testrepo")
	p := parser.NewCSSParser()
	if err := p.Parse(g, "/tmp/imports.css", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	fileID := g.FindByName("imports.css")[0].ID
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

func TestCSSParser_CustomPropertyOutsideRoot(t *testing.T) {
	// Custom properties defined outside :root should still be extracted.
	src := `.theme-dark {
    --bg-color: #1a1a1a;
    --text-color: #ffffff;
}
`
	g := graph.New("testrepo")
	p := parser.NewCSSParser()
	if err := p.Parse(g, "/tmp/theme.css", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "--bg-color", graph.NodeVariable)
	assertNode(t, g, "--text-color", graph.NodeVariable)
}

func TestCSSParser_MinimalKeyframes(t *testing.T) {
	src := `@keyframes spin {
    to { transform: rotate(360deg); }
}
`
	g := graph.New("testrepo")
	p := parser.NewCSSParser()
	if err := p.Parse(g, "/tmp/anim.css", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "spin", graph.NodeFunction)
	if n.Metadata == nil || n.Metadata["kind"] != "keyframes" {
		t.Errorf("spin metadata kind = %q, want keyframes", n.Metadata["kind"])
	}
}
