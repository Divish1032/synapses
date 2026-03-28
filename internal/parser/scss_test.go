package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

func parseSCSS(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewSCSSParser()
	if err := p.Parse(g, "/tmp/style.scss", []byte(src)); err != nil {
		t.Fatalf("SCSSParser.Parse() error: %v", err)
	}
	return g
}

func TestSCSSParser_Extensions(t *testing.T) {
	exts := parser.NewSCSSParser().Extensions()
	has := func(e string) bool {
		for _, x := range exts {
			if x == e {
				return true
			}
		}
		return false
	}
	for _, e := range []string{".scss", ".sass"} {
		if !has(e) {
			t.Errorf("missing extension %s", e)
		}
	}
}

func TestSCSSParser_FileNode(t *testing.T) {
	g := parseSCSS(t, "")
	for _, n := range g.AllNodes() {
		if n.Type == graph.NodeFile {
			return
		}
	}
	t.Error("missing file node")
}

func TestSCSSParser_ExtractsMixin(t *testing.T) {
	g := parseSCSS(t, "@mixin flex-center { display: flex; }")
	n := assertNode(t, g, "flex-center", graph.NodeFunction)
	if n.Metadata["kind"] != "mixin" {
		t.Errorf("kind = %q, want mixin", n.Metadata["kind"])
	}
}

func TestSCSSParser_ExtractsMixinWithParams(t *testing.T) {
	g := parseSCSS(t, "@mixin respond-to($bp) { @media (min-width: $bp) { @content; } }")
	assertNode(t, g, "respond-to", graph.NodeFunction)
}

func TestSCSSParser_ExtractsFunction(t *testing.T) {
	g := parseSCSS(t, "@function rem($px) { @return $px / 16 * 1rem; }")
	n := assertNode(t, g, "rem", graph.NodeFunction)
	if n.Metadata["kind"] != "function" {
		t.Errorf("kind = %q, want function", n.Metadata["kind"])
	}
}

func TestSCSSParser_ExtractsTopLevelVariable(t *testing.T) {
	g := parseSCSS(t, "$primary: #333;\n$font-size: 16px;")
	assertNode(t, g, "$primary", graph.NodeVariable)
	assertNode(t, g, "$font-size", graph.NodeVariable)
}

func TestSCSSParser_VariableMetadata(t *testing.T) {
	g := parseSCSS(t, "$color: red;")
	n := assertNode(t, g, "$color", graph.NodeVariable)
	if n.Metadata["kind"] != "variable" {
		t.Errorf("kind = %q, want variable", n.Metadata["kind"])
	}
}

func TestSCSSParser_MixinParamNotTopLevelVariable(t *testing.T) {
	g := parseSCSS(t, "@mixin foo($x: 0) { color: $x; }")
	for _, n := range g.AllNodes() {
		if n.Name == "$x" {
			t.Error("mixin parameter $x should not be extracted as top-level variable")
		}
	}
}

func TestSCSSParser_ExtractsClassSelector(t *testing.T) {
	g := parseSCSS(t, ".container { max-width: 1200px; }")
	n := assertNode(t, g, ".container", graph.NodeStruct)
	if n.Metadata["kind"] != "selector" {
		t.Errorf("kind = %q, want selector", n.Metadata["kind"])
	}
}

func TestSCSSParser_ExtractsIDSelector(t *testing.T) {
	g := parseSCSS(t, "#hero { font-size: 2em; }")
	n := assertNode(t, g, "#hero", graph.NodeStruct)
	if n.Metadata["kind"] != "selector" {
		t.Errorf("kind = %q, want selector", n.Metadata["kind"])
	}
}

func TestSCSSParser_ExtractsPlaceholder(t *testing.T) {
	g := parseSCSS(t, "%button-reset { border: none; cursor: pointer; }")
	n := assertNode(t, g, "%button-reset", graph.NodeStruct)
	if n.Metadata["kind"] != "placeholder" {
		t.Errorf("kind = %q, want placeholder", n.Metadata["kind"])
	}
}

func TestSCSSParser_MultiSelectorLine(t *testing.T) {
	g := parseSCSS(t, ".btn, .btn-primary, .btn-secondary { display: inline-block; }")
	assertNode(t, g, ".btn", graph.NodeStruct)
	assertNode(t, g, ".btn-primary", graph.NodeStruct)
	assertNode(t, g, ".btn-secondary", graph.NodeStruct)
}

func TestSCSSParser_MixedMultiSelector(t *testing.T) {
	g := parseSCSS(t, ".card, #panel, %card-base { border-radius: 4px; }")
	assertNode(t, g, ".card", graph.NodeStruct)
	assertNode(t, g, "#panel", graph.NodeStruct)
	n := assertNode(t, g, "%card-base", graph.NodeStruct)
	if n.Metadata["kind"] != "placeholder" {
		t.Errorf("%%card-base kind = %q, want placeholder", n.Metadata["kind"])
	}
}

func TestSCSSParser_UseImport(t *testing.T) {
	g := parseSCSS(t, "@use 'sass:math';")
	hasImport := false
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeImports {
			hasImport = true
		}
	}
	if !hasImport {
		t.Error("expected EdgeImports from @use")
	}
}

func TestSCSSParser_ForwardImport(t *testing.T) {
	g := parseSCSS(t, "@forward 'tokens';")
	hasImport := false
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeImports {
			hasImport = true
		}
	}
	if !hasImport {
		t.Error("expected EdgeImports from @forward")
	}
}

func TestSCSSParser_IncludeAddsCallSite(t *testing.T) {
	g := parseSCSS(t, ".btn {\n  @include flex-center;\n}")
	found := false
	for _, cs := range g.PeekCallSites() {
		if cs.FuncName == "flex-center" {
			found = true
		}
	}
	if !found {
		t.Error("expected call site for @include flex-center")
	}
}

func TestSCSSParser_IncludeSameFileDirectEdge(t *testing.T) {
	g := parseSCSS(t, "@mixin foo { color: red; }\n.bar {\n  @include foo;\n}")
	hasCall := false
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeCalls {
			hasCall = true
		}
	}
	if !hasCall {
		t.Error("expected direct EdgeCalls for same-file @include")
	}
}

func TestSCSSParser_ExtendSameFilePlaceholder(t *testing.T) {
	g := parseSCSS(t, "%btn-base {\n  border: none;\n}\n.btn {\n  @extend %btn-base;\n}")
	hasCall := false
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeCalls {
			hasCall = true
		}
	}
	if !hasCall {
		t.Error("expected EdgeCalls from @extend to same-file placeholder")
	}
}

func TestSCSSParser_ExtendCrossFileAddsCallSite(t *testing.T) {
	g := parseSCSS(t, ".btn {\n  @extend .external-btn;\n}")
	found := false
	for _, cs := range g.PeekCallSites() {
		if cs.FuncName == ".external-btn" {
			found = true
		}
	}
	if !found {
		t.Error("expected call site for cross-file @extend .external-btn")
	}
}

func TestSCSSParser_DefinesEdges(t *testing.T) {
	g := parseSCSS(t, "$x: 1;\n@mixin foo {}\n.bar {}")
	count := 0
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeDefines {
			count++
		}
	}
	if count < 3 {
		t.Errorf("expected >=3 DEFINES edges, got %d", count)
	}
}

func TestSCSSParser_EmptyFile(t *testing.T) {
	g := parseSCSS(t, "")
	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFile {
			t.Errorf("empty file produced non-file node: %s", n.Name)
		}
	}
}

func TestSCSSParser_NestedVariableNotExtracted(t *testing.T) {
	g := parseSCSS(t, ".parent { $local: red; color: $local; }")
	for _, n := range g.AllNodes() {
		if n.Name == "$local" {
			t.Error("nested variable $local should not be extracted as top-level")
		}
	}
}
