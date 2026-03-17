package resolver

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

func newDocTestGraph() *graph.Graph {
	g := graph.New("test")
	g.SetRoot("/repo")
	return g
}

func TestResolveDocEdges_BacktickReference(t *testing.T) {
	g := newDocTestGraph()

	// Add a code entity.
	funcID := g.MakeNodeID("/repo/main.go", "FlatGraph")
	g.AddNode(&graph.Node{
		ID:   funcID,
		Type: graph.NodeStruct,
		Name: "FlatGraph",
		File: "/repo/main.go",
		Line: 10,
	})

	// Add a section node that references the entity in backticks.
	secID := g.MakeNodeID("/repo/README.md", "README.md § Architecture")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "README.md § Architecture",
		File: "/repo/README.md",
		Line: 5,
		Metadata: map[string]string{
			"title": "Architecture",
			"depth": "2",
			"body":  "The core data structure is `FlatGraph` which uses SoA layout.",
		},
		Domain: graph.DomainDocs,
	})

	n := ResolveDocEdges(g)
	if n != 1 {
		t.Fatalf("expected 1 EXPLAINS edge, got %d", n)
	}

	// Check EXPLAINS: section → code
	edges := g.OutEdges(secID)
	var foundExplains bool
	for _, e := range edges {
		if e.To == funcID && e.Type == graph.EdgeExplains {
			foundExplains = true
		}
	}
	if !foundExplains {
		t.Error("missing EXPLAINS edge from section to FlatGraph")
	}

	// Check DOCUMENTED_BY: code → section
	edges = g.OutEdges(funcID)
	var foundDocBy bool
	for _, e := range edges {
		if e.To == secID && e.Type == graph.EdgeDocumentedBy {
			foundDocBy = true
		}
	}
	if !foundDocBy {
		t.Error("missing DOCUMENTED_BY edge from FlatGraph to section")
	}
}

func TestResolveDocEdges_CamelCaseReference(t *testing.T) {
	g := newDocTestGraph()

	funcID := g.MakeNodeID("/repo/parser.go", "ParseFile")
	g.AddNode(&graph.Node{
		ID:   funcID,
		Type: graph.NodeMethod,
		Name: "ParseFile",
		File: "/repo/parser.go",
		Line: 20,
	})

	secID := g.MakeNodeID("/repo/docs.md", "docs.md § Parsing")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "docs.md § Parsing",
		File: "/repo/docs.md",
		Line: 1,
		Metadata: map[string]string{
			"title": "Parsing",
			"depth": "1",
			"body":  "Invoke ParseFile on each source file to extract entities.",
		},
		Domain: graph.DomainDocs,
	})

	n := ResolveDocEdges(g)
	if n != 1 {
		t.Fatalf("expected 1 edge, got %d", n)
	}
}

func TestResolveDocEdges_ShortNameSkipped(t *testing.T) {
	g := newDocTestGraph()

	// Short name (3 chars) — should be skipped.
	funcID := g.MakeNodeID("/repo/main.go", "New")
	g.AddNode(&graph.Node{
		ID:   funcID,
		Type: graph.NodeFunction,
		Name: "New",
		File: "/repo/main.go",
		Line: 5,
	})

	secID := g.MakeNodeID("/repo/README.md", "README.md § Usage")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "README.md § Usage",
		File: "/repo/README.md",
		Line: 1,
		Metadata: map[string]string{
			"title": "Usage",
			"depth": "1",
			"body":  "Call `New` to create an instance.",
		},
		Domain: graph.DomainDocs,
	})

	n := ResolveDocEdges(g)
	if n != 0 {
		t.Fatalf("expected 0 edges (short name skipped), got %d", n)
	}
}

func TestResolveDocEdges_NoMatch(t *testing.T) {
	g := newDocTestGraph()

	g.AddNode(&graph.Node{
		ID:   g.MakeNodeID("/repo/main.go", "BuildGraph"),
		Type: graph.NodeFunction,
		Name: "BuildGraph",
		File: "/repo/main.go",
		Line: 10,
	})

	secID := g.MakeNodeID("/repo/README.md", "README.md § Intro")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "README.md § Intro",
		File: "/repo/README.md",
		Line: 1,
		Metadata: map[string]string{
			"title": "Intro",
			"depth": "1",
			"body":  "This project does something else entirely with no code refs.",
		},
		Domain: graph.DomainDocs,
	})

	n := ResolveDocEdges(g)
	if n != 0 {
		t.Fatalf("expected 0 edges, got %d", n)
	}
}

func TestResolveDocEdges_EmptyBody(t *testing.T) {
	g := newDocTestGraph()

	g.AddNode(&graph.Node{
		ID:   g.MakeNodeID("/repo/main.go", "Walker"),
		Type: graph.NodeStruct,
		Name: "Walker",
		File: "/repo/main.go",
		Line: 10,
	})

	secID := g.MakeNodeID("/repo/README.md", "README.md § Empty")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "README.md § Empty",
		File: "/repo/README.md",
		Line: 1,
		Metadata: map[string]string{
			"title": "Empty",
			"depth": "1",
			// No body field.
		},
		Domain: graph.DomainDocs,
	})

	n := ResolveDocEdges(g)
	if n != 0 {
		t.Fatalf("expected 0 edges (empty body), got %d", n)
	}
}

func TestResolveDocEdges_DeduplicatesPerSection(t *testing.T) {
	g := newDocTestGraph()

	funcID := g.MakeNodeID("/repo/main.go", "FlatGraph")
	g.AddNode(&graph.Node{
		ID:   funcID,
		Type: graph.NodeStruct,
		Name: "FlatGraph",
		File: "/repo/main.go",
		Line: 10,
	})

	secID := g.MakeNodeID("/repo/README.md", "README.md § Arch")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "README.md § Arch",
		File: "/repo/README.md",
		Line: 1,
		Metadata: map[string]string{
			"title": "Arch",
			"depth": "1",
			// FlatGraph mentioned twice — should only create 1 edge.
			"body": "`FlatGraph` is the core. See FlatGraph docs for more.",
		},
		Domain: graph.DomainDocs,
	})

	n := ResolveDocEdges(g)
	if n != 1 {
		t.Fatalf("expected 1 edge (deduplicated), got %d", n)
	}
}

func TestResolveDocEdges_QualifiedName(t *testing.T) {
	g := newDocTestGraph()

	funcID := g.MakeNodeID("/repo/graph.go", "Graph.AddNode")
	g.AddNode(&graph.Node{
		ID:   funcID,
		Type: graph.NodeMethod,
		Name: "Graph.AddNode",
		File: "/repo/graph.go",
		Line: 15,
	})

	secID := g.MakeNodeID("/repo/README.md", "README.md § API")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "README.md § API",
		File: "/repo/README.md",
		Line: 1,
		Metadata: map[string]string{
			"title": "API",
			"depth": "1",
			"body":  "Use `Graph.AddNode` to insert entities.",
		},
		Domain: graph.DomainDocs,
	})

	n := ResolveDocEdges(g)
	if n != 1 {
		t.Fatalf("expected 1 edge, got %d", n)
	}
}

func TestExtractEntityRefs_BacktickAndCamelCase(t *testing.T) {
	body := "Use `FlatGraph` for storage. The Walker orchestrates parsing. The err variable is too short."
	refs := extractEntityRefs(body)

	has := make(map[string]bool, len(refs))
	for _, r := range refs {
		has[r] = true
	}

	if !has["FlatGraph"] {
		t.Error("missing FlatGraph (backtick)")
	}
	if !has["Walker"] {
		t.Error("missing Walker (CamelCase)")
	}
	if has["err"] {
		t.Error("err should be filtered (too short)")
	}
}

func TestIsCamelCase(t *testing.T) {
	tests := []struct {
		word string
		want bool
	}{
		{"FlatGraph", true},
		{"AddNode", true},
		{"ALLCAPS", false},
		{"lowercase", false},
		{"A", false},  // too short (but isCamelCase only checks pattern)
		{"Ab", true},  // technically CamelCase
		{"a_b", false}, // starts lowercase
		{"Store.Close", true},
	}
	for _, tt := range tests {
		got := isCamelCase(tt.word)
		if got != tt.want {
			t.Errorf("isCamelCase(%q) = %v, want %v", tt.word, got, tt.want)
		}
	}
}

func TestResolveDocEdges_NoSections(t *testing.T) {
	g := newDocTestGraph()
	// No section nodes at all.
	g.AddNode(&graph.Node{
		ID:   g.MakeNodeID("/repo/main.go", "Walker"),
		Type: graph.NodeStruct,
		Name: "Walker",
		File: "/repo/main.go",
		Line: 10,
	})

	n := ResolveDocEdges(g)
	if n != 0 {
		t.Fatalf("expected 0 edges (no sections), got %d", n)
	}
}
