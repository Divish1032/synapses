package resolver

import (
	"fmt"
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
		t.Fatalf("expected 1 DOCUMENTS edge, got %d", n)
	}

	// Check DOCUMENTS: section → code
	edges := g.OutEdges(secID)
	var foundDocuments bool
	for _, e := range edges {
		if e.To == funcID && e.Type == graph.EdgeDocuments {
			foundDocuments = true
		}
	}
	if !foundDocuments {
		t.Error("missing DOCUMENTS edge from section to FlatGraph")
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
		{"A", false},   // too short (but isCamelCase only checks pattern)
		{"Ab", true},   // technically CamelCase
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

// ── New extraction behaviors ──────────────────────────────────────────────────

func TestExtractEntityRefs_BacktickCallSignature(t *testing.T) {
	// `Store.Close(ctx)` should yield "Store.Close" and "Close", not be dropped.
	body := "Call `Store.Close(ctx)` to release resources."
	refs := extractEntityRefs(body)
	has := make(map[string]bool, len(refs))
	for _, r := range refs {
		has[r] = true
	}
	if !has["Store.Close"] {
		t.Error("expected Store.Close extracted from `Store.Close(ctx)`")
	}
	if !has["Close"] {
		t.Error("expected Close (dot-segment of Store.Close)")
	}
}

func TestExtractEntityRefs_BacktickWithBrackets(t *testing.T) {
	// `AddNode(n *Node)` → "AddNode"
	body := "Invoke `AddNode(n *Node)` to insert a node."
	refs := extractEntityRefs(body)
	has := make(map[string]bool, len(refs))
	for _, r := range refs {
		has[r] = true
	}
	if !has["AddNode"] {
		t.Error("expected AddNode extracted from `AddNode(n *Node)`")
	}
}

func TestExtractEntityRefs_HTMLCodeTag(t *testing.T) {
	// <code>FlatGraph</code> should be extracted.
	body := "The <code>FlatGraph</code> structure uses SoA layout."
	refs := extractEntityRefs(body)
	has := make(map[string]bool, len(refs))
	for _, r := range refs {
		has[r] = true
	}
	if !has["FlatGraph"] {
		t.Error("expected FlatGraph extracted from <code>FlatGraph</code>")
	}
}

func TestExtractEntityRefs_HTMLCodeTagQualified(t *testing.T) {
	body := "Use <code>Graph.AddNode</code> to insert entities."
	refs := extractEntityRefs(body)
	has := make(map[string]bool, len(refs))
	for _, r := range refs {
		has[r] = true
	}
	if !has["Graph.AddNode"] {
		t.Error("expected Graph.AddNode extracted from HTML code tag")
	}
}

func TestExtractEntityRefs_TrailingDotsStripped(t *testing.T) {
	// Trailing punctuation should not contaminate the identifier.
	body := "See `Walker.` for details."
	refs := extractEntityRefs(body)
	for _, r := range refs {
		if r == "Walker." {
			t.Error("trailing dot should be stripped from extracted ref")
		}
	}
}

func TestLeadingIdentifier(t *testing.T) {
	cases := []struct{ input, want string }{
		{"Store.Close(ctx)", "Store.Close"},
		{"AddNode(n *Node)", "AddNode"},
		{"graph.New", "graph.New"},
		{"FlatGraph", "FlatGraph"},
		{"err", "err"},
		{"Walker.", "Walker"},
		{"Close)", "Close"},
	}
	for _, c := range cases {
		got := leadingIdentifier(c.input)
		if got != c.want {
			t.Errorf("leadingIdentifier(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestResolveDocEdgesForFile_OnlyLinksTargetFile(t *testing.T) {
	g := newDocTestGraph()

	// Code entity.
	funcID := g.MakeNodeID("/repo/main.go", "FlatGraph")
	g.AddNode(&graph.Node{
		ID:   funcID,
		Type: graph.NodeStruct,
		Name: "FlatGraph",
		File: "/repo/main.go",
		Line: 10,
	})

	// Section in file A.
	secA := g.MakeNodeID("/repo/README.md", "README.md § Arch")
	g.AddNode(&graph.Node{
		ID:   secA,
		Type: graph.NodeSection,
		Name: "README.md § Arch",
		File: "/repo/README.md",
		Line: 1,
		Metadata: map[string]string{
			"title": "Arch",
			"depth": "1",
			"body":  "The `FlatGraph` is central.",
		},
		Domain: graph.DomainDocs,
	})

	// Section in file B — also mentions FlatGraph.
	secB := g.MakeNodeID("/repo/docs.md", "docs.md § Overview")
	g.AddNode(&graph.Node{
		ID:   secB,
		Type: graph.NodeSection,
		Name: "docs.md § Overview",
		File: "/repo/docs.md",
		Line: 1,
		Metadata: map[string]string{
			"title": "Overview",
			"depth": "1",
			"body":  "See `FlatGraph` for details.",
		},
		Domain: graph.DomainDocs,
	})

	// Scoped to README.md only → should link secA but NOT secB.
	n := ResolveDocEdgesForFile(g, "/repo/README.md")
	if n != 1 {
		t.Fatalf("expected 1 edge from README.md, got %d", n)
	}

	// secB should have NO edges yet.
	edgesB := g.OutEdges(secB)
	for _, e := range edgesB {
		if e.Type == graph.EdgeDocuments {
			t.Error("docs.md section should NOT have DOCUMENTS edge from file-scoped resolution")
		}
	}
}

func TestResolveDocEdges_EntityInTitle(t *testing.T) {
	g := newDocTestGraph()

	funcID := g.MakeNodeID("/repo/main.go", "FlatGraph")
	g.AddNode(&graph.Node{
		ID:   funcID,
		Type: graph.NodeStruct,
		Name: "FlatGraph",
		File: "/repo/main.go",
		Line: 10,
	})

	// Section title names the entity; body is empty.
	// Research: section heading = highest-confidence doc-code signal (weight 0.80).
	secID := g.MakeNodeID("/repo/README.md", "README.md § FlatGraph Architecture")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "README.md § FlatGraph Architecture",
		File: "/repo/README.md",
		Line: 5,
		Metadata: map[string]string{
			"title": "FlatGraph Architecture",
			"depth": "2",
			// No body — entity reference is solely in the heading.
		},
		Domain: graph.DomainDocs,
	})

	n := ResolveDocEdges(g)
	if n != 1 {
		t.Fatalf("expected 1 edge from section title match, got %d", n)
	}
	edges := g.OutEdges(secID)
	var found bool
	for _, e := range edges {
		if e.To == funcID && e.Type == graph.EdgeDocuments {
			found = true
		}
	}
	if !found {
		t.Error("missing DOCUMENTS edge from section with entity name in title")
	}
}

func TestExtractEntityRefs_BoldMarkup(t *testing.T) {
	// **FlatGraph** should yield FlatGraph after * delimiters strip the markers.
	body := "The **FlatGraph** is the core data structure."
	refs := extractEntityRefs(body)
	has := make(map[string]bool, len(refs))
	for _, r := range refs {
		has[r] = true
	}
	if !has["FlatGraph"] {
		t.Error("expected FlatGraph extracted from **FlatGraph**")
	}
}

func TestExtractEntityRefs_ItalicMarkup(t *testing.T) {
	// _Walker_ and *ParseFile* should both be extracted.
	body := "The _Walker_ orchestrates parsing. Use *ParseFile* on each file."
	refs := extractEntityRefs(body)
	has := make(map[string]bool, len(refs))
	for _, r := range refs {
		has[r] = true
	}
	if !has["Walker"] {
		t.Error("expected Walker extracted from _Walker_")
	}
	if !has["ParseFile"] {
		t.Error("expected ParseFile extracted from *ParseFile*")
	}
}

func TestResolveDocEdges_FrontmatterTitle(t *testing.T) {
	g := newDocTestGraph()

	funcID := g.MakeNodeID("/repo/main.go", "FlatGraph")
	g.AddNode(&graph.Node{
		ID:   funcID,
		Type: graph.NodeStruct,
		Name: "FlatGraph",
		File: "/repo/main.go",
		Line: 10,
	})

	// A markdown file node with frontmatter_title — no sections, no body.
	// This simulates: `title: "FlatGraph Storage Layer"` in the frontmatter.
	fileID := g.MakeNodeID("/repo/README.md", "/repo/README.md")
	g.AddNode(&graph.Node{
		ID:   fileID,
		Type: graph.NodeFile,
		Name: "README.md",
		File: "/repo/README.md",
		Line: 1,
		Metadata: map[string]string{
			"frontmatter_title": "FlatGraph Storage Layer",
		},
		Domain: graph.DomainDocs,
	})

	n := ResolveDocEdges(g)
	if n != 1 {
		t.Fatalf("expected 1 edge from frontmatter title, got %d", n)
	}
	edges := g.OutEdges(fileID)
	var found bool
	for _, e := range edges {
		if e.To == funcID && e.Type == graph.EdgeDocuments {
			found = true
		}
	}
	if !found {
		t.Error("missing DOCUMENTS edge from file node with frontmatter_title")
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

// ── File-level linking tests ──────────────────────────────────────────────────

func TestResolveDocEdges_FilePathInBacktick(t *testing.T) {
	g := newDocTestGraph()

	// Code file node.
	fileID := g.MakeNodeID("/repo/src/app/main.go", "/repo/src/app/main.go")
	g.AddNode(&graph.Node{
		ID:   fileID,
		Type: graph.NodeFile,
		Name: "main.go",
		File: "/repo/src/app/main.go",
		Line: 1,
	})

	// Doc section referencing the file path in backticks.
	secID := g.MakeNodeID("/repo/README.md", "README.md § Setup")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "README.md § Setup",
		File: "/repo/README.md",
		Line: 1,
		Metadata: map[string]string{
			"title": "Setup",
			"body":  "Edit `src/app/main.go` to configure the app.",
		},
		Domain: graph.DomainDocs,
	})

	n := ResolveDocEdges(g)
	if n < 1 {
		t.Fatalf("expected at least 1 file-path edge, got %d", n)
	}

	var found bool
	for _, e := range g.OutEdges(secID) {
		if e.To == fileID && e.Type == graph.EdgeDocuments {
			found = true
		}
	}
	if !found {
		t.Error("missing DOCUMENTS edge from section to file referenced in backtick")
	}
}

func TestResolveDocEdges_FilePathNoSelfLink(t *testing.T) {
	g := newDocTestGraph()

	// Doc file node — should NOT link to itself.
	fileID := g.MakeNodeID("/repo/README.md", "/repo/README.md")
	g.AddNode(&graph.Node{
		ID:     fileID,
		Type:   graph.NodeFile,
		Name:   "README.md",
		File:   "/repo/README.md",
		Line:   1,
		Domain: graph.DomainDocs,
	})

	// Section in the same doc file mentioning its own name.
	secID := g.MakeNodeID("/repo/README.md", "README.md § Intro")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "README.md § Intro",
		File: "/repo/README.md",
		Line: 1,
		Metadata: map[string]string{
			"title": "Intro",
			"body":  "This is `README.md` introduction.",
		},
		Domain: graph.DomainDocs,
	})

	n := ResolveDocEdges(g)
	// Doc files are excluded from the file index (DomainDocs filtered out),
	// so no file-path edges should be created.
	if n != 0 {
		t.Fatalf("expected 0 edges (doc file excluded from index), got %d", n)
	}
}

func TestBuildFileIndex_MultipleSuffixes(t *testing.T) {
	g := newDocTestGraph()

	fileID := g.MakeNodeID("/repo/src/pkg/handler.go", "/repo/src/pkg/handler.go")
	g.AddNode(&graph.Node{
		ID:   fileID,
		Type: graph.NodeFile,
		Name: "handler.go",
		File: "/repo/src/pkg/handler.go",
		Line: 1,
	})

	idx := buildFileIndex(g)

	// Should be findable by multiple suffixes.
	for _, key := range []string{"handler.go", "pkg/handler.go", "src/pkg/handler.go"} {
		if nodes, ok := idx[key]; !ok || len(nodes) == 0 {
			t.Errorf("buildFileIndex: expected to find %q", key)
		}
	}
}

func TestResolveDocEdges_AmbiguityCap(t *testing.T) {
	g := newDocTestGraph()

	// Create 5 entities named "Handler" — should exceed the ambiguity cap of 3.
	for i := 0; i < 5; i++ {
		file := fmt.Sprintf("/repo/pkg%d/handler.go", i)
		id := g.MakeNodeID(file, "Handler")
		g.AddNode(&graph.Node{
			ID:   id,
			Type: graph.NodeStruct,
			Name: "Handler",
			File: file,
			Line: 1,
		})
	}

	secID := g.MakeNodeID("/repo/README.md", "README.md § API")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "README.md § API",
		File: "/repo/README.md",
		Line: 1,
		Metadata: map[string]string{
			"title": "API",
			"body":  "The `Handler` processes all requests.",
		},
		Domain: graph.DomainDocs,
	})

	n := ResolveDocEdges(g)
	if n != 0 {
		t.Fatalf("expected 0 edges (ambiguity cap: Handler matches 5 entities), got %d", n)
	}
}

func TestResolveDocEdges_TestFileFiltered(t *testing.T) {
	g := newDocTestGraph()

	// Entity in a test file — should be filtered out.
	testID := g.MakeNodeID("/repo/main_test.go", "TestHelper")
	g.AddNode(&graph.Node{
		ID:   testID,
		Type: graph.NodeFunction,
		Name: "TestHelper",
		File: "/repo/main_test.go",
		Line: 1,
	})

	// Entity in production file — should be linked.
	prodID := g.MakeNodeID("/repo/main.go", "BuildGraph")
	g.AddNode(&graph.Node{
		ID:   prodID,
		Type: graph.NodeFunction,
		Name: "BuildGraph",
		File: "/repo/main.go",
		Line: 1,
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
			"body":  "Use `TestHelper` and `BuildGraph` in your code.",
		},
		Domain: graph.DomainDocs,
	})

	n := ResolveDocEdges(g)
	if n != 1 {
		t.Fatalf("expected 1 edge (TestHelper filtered), got %d", n)
	}

	// Should only link to BuildGraph.
	var found bool
	for _, e := range g.OutEdges(secID) {
		if e.To == testID && e.Type == graph.EdgeDocuments {
			t.Error("should NOT link to test file entity")
		}
		if e.To == prodID && e.Type == graph.EdgeDocuments {
			found = true
		}
	}
	if !found {
		t.Error("missing DOCUMENTS edge to production entity BuildGraph")
	}
}

func TestResolveDocEdges_ProvenanceMetadata(t *testing.T) {
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
			"body":  "The `FlatGraph` is central.",
		},
		Domain: graph.DomainDocs,
	})

	ResolveDocEdges(g)

	sec := g.GetNode(secID)
	if sec.Metadata["doc_link_source"] != "name_match" {
		t.Errorf("doc_link_source = %q, want 'name_match'", sec.Metadata["doc_link_source"])
	}
}

func TestIsTestFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/repo/main_test.go", true},
		{"/repo/main.go", false},
		{"/repo/src/app.test.ts", true},
		{"/repo/src/app.ts", false},
		{"/repo/tests/helper.py", true},
		{"/repo/__tests__/foo.js", true},
		{"/repo/testdata/fixture.json", true},
		{"/repo/src/handler_test.py", true},
		{"/repo/spec/foo_spec.rb", true},
	}
	for _, c := range cases {
		got := isTestFile(c.path)
		if got != c.want {
			t.Errorf("isTestFile(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// ── Phase 3: Code block identifier linking ──────────────────────────────────

func TestResolveDocEdges_CodeBlockIdentifiers(t *testing.T) {
	g := newDocTestGraph()

	// Code entity.
	flaskID := g.MakeNodeID("/repo/app.py", "Flask")
	g.AddNode(&graph.Node{
		ID:   flaskID,
		Type: graph.NodeStruct,
		Name: "Flask",
		File: "/repo/app.py",
		Line: 1,
	})

	renderID := g.MakeNodeID("/repo/app.py", "render_template")
	g.AddNode(&graph.Node{
		ID:   renderID,
		Type: graph.NodeFunction,
		Name: "render_template",
		File: "/repo/app.py",
		Line: 10,
	})

	// Section with code blocks in metadata.
	secID := g.MakeNodeID("/repo/README.md", "README.md § Quick Start")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "README.md § Quick Start",
		File: "/repo/README.md",
		Line: 1,
		Metadata: map[string]string{
			"title":       "Quick Start",
			"body":        "Install Flask and run the app.",
			"code_blocks": `[{"language":"python","content":"from flask import Flask, render_template\napp = Flask(__name__)","line":5}]`,
		},
		Domain: graph.DomainDocs,
	})

	n := ResolveDocEdges(g)
	// Should link to Flask (from body CamelCase + code block import)
	// and render_template (from code block import)
	if n < 2 {
		t.Fatalf("expected at least 2 edges (Flask + render_template), got %d", n)
	}

	var foundFlask, foundRender bool
	for _, e := range g.OutEdges(secID) {
		if e.Type == graph.EdgeDocuments {
			if e.To == flaskID {
				foundFlask = true
			}
			if e.To == renderID {
				foundRender = true
			}
		}
	}
	if !foundFlask {
		t.Error("missing DOCUMENTS edge to Flask from code block")
	}
	if !foundRender {
		t.Error("missing DOCUMENTS edge to render_template from code block")
	}
}

func TestResolveDocEdges_CodeBlockMaxFiveEdges(t *testing.T) {
	g := newDocTestGraph()

	// Create 7 code entities.
	for i := 0; i < 7; i++ {
		name := fmt.Sprintf("Entity%d", i)
		id := g.MakeNodeID("/repo/main.go", name)
		g.AddNode(&graph.Node{
			ID:   id,
			Type: graph.NodeFunction,
			Name: name,
			File: "/repo/main.go",
			Line: i + 1,
		})
	}

	// Code block that imports all 7.
	imports := ""
	for i := 0; i < 7; i++ {
		imports += fmt.Sprintf("from x import Entity%d\n", i)
	}

	secID := g.MakeNodeID("/repo/README.md", "README.md § All")
	g.AddNode(&graph.Node{
		ID:   secID,
		Type: graph.NodeSection,
		Name: "README.md § All",
		File: "/repo/README.md",
		Line: 1,
		Metadata: map[string]string{
			"title":       "All",
			"body":        "No entity refs in body.",
			"code_blocks": fmt.Sprintf(`[{"language":"python","content":%q,"line":5}]`, imports),
		},
		Domain: graph.DomainDocs,
	})

	n := ResolveDocEdges(g)
	if n > 5 {
		t.Errorf("expected max 5 edges from code blocks, got %d", n)
	}
}

func TestExtractCodeBlockIdentifiers_Python(t *testing.T) {
	content := `from flask import Flask, render_template
import os
app = Flask(__name__)
`
	idents := extractCodeBlockIdentifiers(content, "python")
	has := make(map[string]bool)
	for _, id := range idents {
		has[id] = true
	}
	if !has["flask"] {
		t.Error("expected 'flask' from 'from flask import ...'")
	}
	if !has["Flask"] {
		t.Error("expected 'Flask' from import")
	}
	if !has["render_template"] {
		t.Error("expected 'render_template' from import")
	}
}

func TestExtractCodeBlockIdentifiers_QualifiedCall(t *testing.T) {
	content := `app = Flask.create()
result = Client.send(data)
`
	idents := extractCodeBlockIdentifiers(content, "python")
	has := make(map[string]bool)
	for _, id := range idents {
		has[id] = true
	}
	if !has["Flask"] {
		t.Error("expected Flask from Flask.create()")
	}
	if !has["Client"] {
		t.Error("expected Client from Client.send()")
	}
}

func TestExtractCodeBlockIdentifiers_ParenthesizedImport(t *testing.T) {
	content := `from flask import (Flask, render_template)
`
	idents := extractCodeBlockIdentifiers(content, "python")
	has := make(map[string]bool)
	for _, id := range idents {
		has[id] = true
	}
	if !has["Flask"] {
		t.Error("expected Flask from parenthesized import")
	}
	if !has["render_template"] {
		t.Error("expected render_template from parenthesized import")
	}
}

func TestExtractCodeBlockIdentifiers_TypeAnnotation(t *testing.T) {
	content := `def process(handler: RequestHandler) -> Response:
    pass
`
	idents := extractCodeBlockIdentifiers(content, "python")
	has := make(map[string]bool)
	for _, id := range idents {
		has[id] = true
	}
	if !has["RequestHandler"] {
		t.Error("expected RequestHandler from type annotation")
	}
	if !has["Response"] {
		t.Error("expected Response from return type")
	}
}

func TestLooksLikeFilePath(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"src/app/main.go", true},
		{"handler.go", true},
		{"main.py", true},
		{"FlatGraph", false},
		{"https://example.com/file.go", false},
		{"README.md", true},
	}
	for _, c := range cases {
		got := looksLikeFilePath(c.input)
		if got != c.want {
			t.Errorf("looksLikeFilePath(%q) = %v, want %v", c.input, got, c.want)
		}
	}
}
