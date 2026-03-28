package resolver_test

import (
	"context"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

// docCodeMockEmbedder returns controlled vectors and search results for testing
// the cross-domain doc↔code embedding discovery.
type docCodeMockEmbedder struct {
	vectors map[string][]float32
	results []resolver.EmbedMatch
}

func (m *docCodeMockEmbedder) EmbedText(_ context.Context, text string) ([]float32, error) {
	for prefix, vec := range m.vectors {
		if len(text) >= len(prefix) && text[:len(prefix)] == prefix {
			return vec, nil
		}
	}
	return []float32{0, 0, 0}, nil
}

func (m *docCodeMockEmbedder) SearchByVector(_ []float32, _ int) []resolver.EmbedMatch {
	return m.results
}

func TestDiscoverDocCodeRelations_LinksToFunction(t *testing.T) {
	g := graph.New("test")
	g.SetRoot("/repo")

	// Add a code function entity.
	funcID := g.MakeNodeID("/repo/app.py", "Flask.run")
	g.AddNode(&graph.Node{
		ID:     funcID,
		Type:   graph.NodeMethod,
		Name:   "Flask.run",
		File:   "/repo/app.py",
		Line:   42,
		Domain: graph.DomainCode,
	})

	// Add a doc section that semantically relates to the function
	// but does NOT mention it by name (no backtick ref, no CamelCase match).
	secID := g.MakeNodeID("/repo/docs/deploy.md", "deploy.md § Running the Server")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "deploy.md § Running the Server",
		File: "/repo/docs/deploy.md",
		Line: 5,
		Metadata: map[string]string{
			"title": "Running the Server",
			"body":  "Start the development server on port 5000. The application will auto-reload on code changes.",
		},
		Domain: graph.DomainDocs,
	})

	er := &docCodeMockEmbedder{
		vectors: map[string][]float32{
			"Running the Server": {1, 0, 0},
		},
		results: []resolver.EmbedMatch{
			{NodeID: string(funcID), Score: 0.72},
		},
	}

	created := resolver.DiscoverDocCodeRelations(g, er, 0.6)
	if created == 0 {
		t.Fatal("expected at least 1 embedding-based doc→code edge")
	}

	// Verify EXPLAINS edge exists.
	found := false
	for _, e := range g.OutEdges(secID) {
		if e.To == funcID && e.Type == graph.EdgeExplains {
			found = true
		}
	}
	if !found {
		t.Error("missing EXPLAINS edge from doc section to code function via embedding")
	}

	// Verify DOCUMENTED_BY reverse edge.
	foundRev := false
	for _, e := range g.OutEdges(funcID) {
		if e.To == secID && e.Type == graph.EdgeDocumentedBy {
			foundRev = true
		}
	}
	if !foundRev {
		t.Error("missing DOCUMENTED_BY edge from code function to doc section")
	}
}

func TestDiscoverDocCodeRelations_SkipsBelowThreshold(t *testing.T) {
	g := graph.New("test")
	g.SetRoot("/repo")

	funcID := g.MakeNodeID("/repo/app.py", "Flask.run")
	g.AddNode(&graph.Node{
		ID: funcID, Type: graph.NodeMethod, Name: "Flask.run",
		File: "/repo/app.py", Domain: graph.DomainCode,
	})

	secID := g.MakeNodeID("/repo/README.md", "README.md § License")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "README.md § License",
		File: "/repo/README.md",
		Metadata: map[string]string{
			"title": "License",
			"body":  "This project is licensed under the MIT license.",
		},
		Domain: graph.DomainDocs,
	})

	er := &docCodeMockEmbedder{
		vectors: map[string][]float32{"License": {1, 0, 0}},
		results: []resolver.EmbedMatch{
			{NodeID: string(funcID), Score: 0.30}, // below threshold
		},
	}

	created := resolver.DiscoverDocCodeRelations(g, er, 0.6)
	if created != 0 {
		t.Errorf("expected 0 edges for below-threshold score, got %d", created)
	}
}

func TestDiscoverDocCodeRelations_SkipsIfNameMatchExists(t *testing.T) {
	g := graph.New("test")
	g.SetRoot("/repo")

	funcID := g.MakeNodeID("/repo/app.py", "Flask.run")
	g.AddNode(&graph.Node{
		ID: funcID, Type: graph.NodeMethod, Name: "Flask.run",
		File: "/repo/app.py", Domain: graph.DomainCode,
	})

	secID := g.MakeNodeID("/repo/README.md", "README.md § Flask API")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "README.md § Flask API",
		File: "/repo/README.md",
		Metadata: map[string]string{
			"title": "Flask API",
			"body":  "The `Flask.run()` method starts the development server.",
		},
		Domain: graph.DomainDocs,
	})

	// Simulate existing name-match edge (from ResolveDocEdges).
	g.AddEdge(&graph.Edge{From: secID, To: funcID, Type: graph.EdgeExplains})

	er := &docCodeMockEmbedder{
		vectors: map[string][]float32{"Flask API": {1, 0, 0}},
		results: []resolver.EmbedMatch{
			{NodeID: string(funcID), Score: 0.85},
		},
	}

	// Should skip because entity-level edge already exists.
	created := resolver.DiscoverDocCodeRelations(g, er, 0.6)
	if created != 0 {
		t.Errorf("expected 0 new edges (name-match already exists), got %d", created)
	}
}

func TestDiscoverDocCodeRelations_FallsBackToFile(t *testing.T) {
	g := graph.New("test")
	g.SetRoot("/repo")

	// Code file node (no function-level match will happen).
	fileID := g.MakeNodeID("/repo/src/router.go", "/repo/src/router.go")
	g.AddNode(&graph.Node{
		ID:     fileID,
		Type:   graph.NodeFile,
		Name:   "src/router.go",
		File:   "/repo/src/router.go",
		Domain: graph.DomainCode,
	})

	secID := g.MakeNodeID("/repo/docs/routing.md", "routing.md § Route Matching")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "routing.md § Route Matching",
		File: "/repo/docs/routing.md",
		Metadata: map[string]string{
			"title": "Route Matching",
			"body":  "The router matches incoming HTTP requests to handler functions based on path patterns.",
		},
		Domain: graph.DomainDocs,
	})

	er := &docCodeMockEmbedder{
		vectors: map[string][]float32{"Route Matching": {1, 0, 0}},
		results: []resolver.EmbedMatch{
			// Only file-level match, no function-level
			{NodeID: string(fileID), Score: 0.62},
		},
	}

	created := resolver.DiscoverDocCodeRelations(g, er, 0.6)
	if created == 0 {
		t.Fatal("expected file-level fallback edge")
	}

	found := false
	for _, e := range g.OutEdges(secID) {
		if e.To == fileID && e.Type == graph.EdgeExplains {
			found = true
		}
	}
	if !found {
		t.Error("missing EXPLAINS edge to code file via file-level fallback")
	}
}

func TestDiscoverDocCodeRelations_FallsBackToPackage(t *testing.T) {
	g := graph.New("test")
	g.SetRoot("/repo")

	// Only a package node exists (no functions, no files match).
	pkgID := g.MakeNodeID("/repo/internal/parser", "/repo/internal/parser")
	g.AddNode(&graph.Node{
		ID:     pkgID,
		Type:   graph.NodePackage,
		Name:   "parser",
		File:   "/repo/internal/parser",
		Domain: graph.DomainCode,
	})

	secID := g.MakeNodeID("/repo/docs/parsing.md", "parsing.md § AST Parsing")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "parsing.md § AST Parsing",
		File: "/repo/docs/parsing.md",
		Metadata: map[string]string{
			"title": "AST Parsing",
			"body":  "The parser module converts source code into abstract syntax trees using tree-sitter grammars.",
		},
		Domain: graph.DomainDocs,
	})

	er := &docCodeMockEmbedder{
		vectors: map[string][]float32{"AST Parsing": {1, 0, 0}},
		results: []resolver.EmbedMatch{
			{NodeID: string(pkgID), Score: 0.58},
		},
	}

	created := resolver.DiscoverDocCodeRelations(g, er, 0)
	if created == 0 {
		t.Fatal("expected package-level fallback edge")
	}
}

func TestDiscoverDocCodeRelations_NilResolver(t *testing.T) {
	g := graph.New("test")
	created := resolver.DiscoverDocCodeRelations(g, nil, 0)
	if created != 0 {
		t.Errorf("expected 0 for nil resolver, got %d", created)
	}
}

func TestDiscoverDocCodeRelations_MaxEdgesPerSection(t *testing.T) {
	g := graph.New("test")
	g.SetRoot("/repo")

	// Create 5 code functions.
	var funcIDs []graph.NodeID
	for i := 0; i < 5; i++ {
		name := "Func" + string(rune('A'+i))
		id := g.MakeNodeID("/repo/app.go", name)
		g.AddNode(&graph.Node{
			ID: id, Type: graph.NodeFunction, Name: name,
			File: "/repo/app.go", Domain: graph.DomainCode,
		})
		funcIDs = append(funcIDs, id)
	}

	secID := g.MakeNodeID("/repo/README.md", "README.md § API")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "README.md § API",
		File: "/repo/README.md",
		Metadata: map[string]string{
			"title": "API Overview",
			"body":  "This section covers the main API functions.",
		},
		Domain: graph.DomainDocs,
	})

	var matches []resolver.EmbedMatch
	for _, id := range funcIDs {
		matches = append(matches, resolver.EmbedMatch{NodeID: string(id), Score: 0.75})
	}

	er := &docCodeMockEmbedder{
		vectors: map[string][]float32{"API Overview": {1, 0, 0}},
		results: matches,
	}

	created := resolver.DiscoverDocCodeRelations(g, er, 0.6)
	// Should cap at 3 edges per section per specificity level.
	if created > 3 {
		t.Errorf("expected at most 3 edges per section, got %d", created)
	}
}
