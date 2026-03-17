package parser

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

func newMarkdownTestGraph() *graph.Graph {
	g := graph.New("test")
	g.SetRoot("/repo")
	return g
}

func TestMarkdownParser_Extensions(t *testing.T) {
	p := NewMarkdownParser()
	exts := p.Extensions()
	want := map[string]bool{".md": true, ".markdown": true, ".mdx": true}
	for _, ext := range exts {
		if !want[ext] {
			t.Errorf("unexpected extension %q", ext)
		}
		delete(want, ext)
	}
	for ext := range want {
		t.Errorf("missing extension %q", ext)
	}
}

func TestMarkdownParser_EmptyFile(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	if err := p.Parse(g, "/repo/README.md", []byte("")); err != nil {
		t.Fatal(err)
	}
	// Should only have the file node.
	nodes := g.AllNodes()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node (file), got %d", len(nodes))
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("expected NodeFile, got %s", nodes[0].Type)
	}
	if nodes[0].Domain != graph.DomainDocs {
		t.Errorf("expected DomainDocs, got %q", nodes[0].Domain)
	}
}

func TestMarkdownParser_SingleHeading(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	src := []byte("# Introduction\n\nThis is the intro text.\n")
	if err := p.Parse(g, "/repo/README.md", src); err != nil {
		t.Fatal(err)
	}
	nodes := g.AllNodes()
	// file + 1 section
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}

	var sec *graph.Node
	for _, n := range nodes {
		if n.Type == graph.NodeSection {
			sec = n
		}
	}
	if sec == nil {
		t.Fatal("no Section node found")
	}
	if sec.Metadata["title"] != "Introduction" {
		t.Errorf("title = %q, want %q", sec.Metadata["title"], "Introduction")
	}
	if sec.Metadata["depth"] != "1" {
		t.Errorf("depth = %q, want %q", sec.Metadata["depth"], "1")
	}
	if sec.Domain != graph.DomainDocs {
		t.Errorf("domain = %q, want %q", sec.Domain, graph.DomainDocs)
	}
	if sec.Line != 1 {
		t.Errorf("line = %d, want 1", sec.Line)
	}
	if !strings.Contains(sec.Metadata["body_preview"], "This is the intro text") {
		t.Errorf("body_preview missing body text: %q", sec.Metadata["body_preview"])
	}
}

func TestMarkdownParser_NestedHeadings(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	src := []byte(`# Top
## Sub A
Some text.
## Sub B
### Sub B.1
Deep text.
# Another Top
`)
	if err := p.Parse(g, "/repo/DOC.md", src); err != nil {
		t.Fatal(err)
	}

	sections := g.FindByType(graph.NodeSection)
	if len(sections) != 5 {
		t.Fatalf("expected 5 sections, got %d", len(sections))
	}

	// Verify CONTAINS edges form correct hierarchy.
	fileNodeID := g.MakeNodeID("/repo/DOC.md", "/repo/DOC.md")
	topID := g.MakeNodeID("/repo/DOC.md", "DOC.md § Top")
	subAID := g.MakeNodeID("/repo/DOC.md", "DOC.md § Sub A")
	subBID := g.MakeNodeID("/repo/DOC.md", "DOC.md § Sub B")
	subB1ID := g.MakeNodeID("/repo/DOC.md", "DOC.md § Sub B.1")
	anotherTopID := g.MakeNodeID("/repo/DOC.md", "DOC.md § Another Top")

	assertEdge(t, g, fileNodeID, topID, graph.EdgeContains)
	assertEdge(t, g, topID, subAID, graph.EdgeContains)
	assertEdge(t, g, topID, subBID, graph.EdgeContains)
	assertEdge(t, g, subBID, subB1ID, graph.EdgeContains)
	assertEdge(t, g, fileNodeID, anotherTopID, graph.EdgeContains)
}

func TestMarkdownParser_LinksTo(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()

	// Create the target file node first so the edge won't be silently dropped.
	targetID := g.MakeNodeID("/repo/other.md", "/repo/other.md")
	g.AddNode(&graph.Node{
		ID:   targetID,
		Type: graph.NodeFile,
		Name: "other.md",
		File: "/repo/other.md",
		Line: 1,
	})

	src := []byte("# Docs\nSee [other docs](other.md) and [external](https://example.com).\n")
	if err := p.Parse(g, "/repo/README.md", src); err != nil {
		t.Fatal(err)
	}

	fileNodeID := g.MakeNodeID("/repo/README.md", "/repo/README.md")
	assertEdge(t, g, fileNodeID, targetID, graph.EdgeLinksTo)

	// Should NOT have a LINKS_TO edge for the external URL.
	edges := g.OutEdges(fileNodeID)
	for _, e := range edges {
		if e.Type == graph.EdgeLinksTo && strings.Contains(string(e.To), "example.com") {
			t.Error("external link should not create LINKS_TO edge")
		}
	}
}

func TestMarkdownParser_NoHeadingWithoutSpace(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	// "#NoSpace" is NOT a valid ATX heading per CommonMark.
	src := []byte("#NoSpace\n## Valid Heading\n")
	if err := p.Parse(g, "/repo/test.md", src); err != nil {
		t.Fatal(err)
	}
	sections := g.FindByType(graph.NodeSection)
	if len(sections) != 1 {
		t.Fatalf("expected 1 section (only valid heading), got %d", len(sections))
	}
	if sections[0].Metadata["title"] != "Valid Heading" {
		t.Errorf("title = %q, want %q", sections[0].Metadata["title"], "Valid Heading")
	}
}

func TestMarkdownParser_BodyTruncation(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	longBody := strings.Repeat("x", 3000)
	src := []byte("# Section\n" + longBody + "\n")
	if err := p.Parse(g, "/repo/test.md", src); err != nil {
		t.Fatal(err)
	}
	sections := g.FindByType(graph.NodeSection)
	if len(sections) != 1 {
		t.Fatal("expected 1 section")
	}
	if len(sections[0].Metadata["body"]) > 2000 {
		t.Errorf("body should be truncated to 2000, got %d", len(sections[0].Metadata["body"]))
	}
	if len(sections[0].Metadata["body_preview"]) > 200 {
		t.Errorf("body_preview should be truncated to 200, got %d", len(sections[0].Metadata["body_preview"]))
	}
}

func TestMarkdownParser_H6Depth(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	src := []byte("###### Deep Heading\nBody text.\n")
	if err := p.Parse(g, "/repo/test.md", src); err != nil {
		t.Fatal(err)
	}
	sections := g.FindByType(graph.NodeSection)
	if len(sections) != 1 {
		t.Fatal("expected 1 section")
	}
	if sections[0].Metadata["depth"] != "6" {
		t.Errorf("depth = %q, want %q", sections[0].Metadata["depth"], "6")
	}
}

func TestMarkdownParser_ClosingHashes(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	src := []byte("## Title With Closing ##\nBody.\n")
	if err := p.Parse(g, "/repo/test.md", src); err != nil {
		t.Fatal(err)
	}
	sections := g.FindByType(graph.NodeSection)
	if len(sections) != 1 {
		t.Fatal("expected 1 section")
	}
	if sections[0].Metadata["title"] != "Title With Closing" {
		t.Errorf("title = %q, want %q", sections[0].Metadata["title"], "Title With Closing")
	}
}

func TestMarkdownParser_SelfRefAnchorLink(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	src := []byte("# Top\nSee [section](#top) for more.\n")
	if err := p.Parse(g, "/repo/test.md", src); err != nil {
		t.Fatal(err)
	}
	// Self-referencing anchor links (#top) should not create LINKS_TO edges.
	fileNodeID := g.MakeNodeID("/repo/test.md", "/repo/test.md")
	edges := g.OutEdges(fileNodeID)
	for _, e := range edges {
		if e.Type == graph.EdgeLinksTo {
			t.Error("self-referencing anchor link should not create LINKS_TO edge")
		}
	}
}

func TestMarkdownParser_FencedCodeBlockHeadingSkipped(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	// The # inside the fenced block must NOT be parsed as a section heading.
	src := []byte("# Real Section\nSome text.\n```yaml\n# This is YAML comment, not a heading\nkey: value\n```\n## Sub Section\n")
	if err := p.Parse(g, "/repo/test.md", src); err != nil {
		t.Fatal(err)
	}
	sections := g.FindByType(graph.NodeSection)
	if len(sections) != 2 {
		t.Fatalf("expected 2 sections (Real Section + Sub Section), got %d", len(sections))
	}
	for _, s := range sections {
		if s.Metadata["title"] == "This is YAML comment, not a heading" {
			t.Error("heading inside fenced code block should not create a section")
		}
	}
}

func TestMarkdownParser_DuplicateHeadingNames(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	// Two sections with the same title → should get disambiguated names, no data loss.
	src := []byte("# Introduction\nFirst intro.\n# Introduction\nSecond intro.\n")
	if err := p.Parse(g, "/repo/test.md", src); err != nil {
		t.Fatal(err)
	}
	sections := g.FindByType(graph.NodeSection)
	if len(sections) != 2 {
		t.Fatalf("expected 2 sections (disambiguated), got %d", len(sections))
	}
	titles := make(map[string]bool)
	for _, s := range sections {
		titles[s.Metadata["title"]] = true
	}
	if !titles["Introduction"] {
		t.Error("first section should have title 'Introduction'")
	}
	if !titles["Introduction (2)"] {
		t.Error("second duplicate section should be disambiguated to 'Introduction (2)'")
	}
}

func TestMarkdownParser_TildeFenceBlock(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	// ~~~-fenced blocks should also suppress heading detection inside.
	src := []byte("# Top\n~~~\n# Not a heading\n~~~\n## Sub\n")
	if err := p.Parse(g, "/repo/test.md", src); err != nil {
		t.Fatal(err)
	}
	sections := g.FindByType(graph.NodeSection)
	if len(sections) != 2 {
		t.Fatalf("expected 2 sections (Top + Sub), got %d", len(sections))
	}
}

func TestMarkdownParser_YAMLFrontmatter(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	src := []byte("---\ntitle: My Docs\ndate: 2024-01-01\n---\n# Real Heading\nBody text.\n")
	if err := p.Parse(g, "/repo/test.md", src); err != nil {
		t.Fatal(err)
	}
	sections := g.FindByType(graph.NodeSection)
	if len(sections) != 1 {
		t.Fatalf("expected 1 section (frontmatter skipped), got %d", len(sections))
	}
	if sections[0].Metadata["title"] != "Real Heading" {
		t.Errorf("title = %q, want 'Real Heading'", sections[0].Metadata["title"])
	}
}

func TestMarkdownParser_TOMLFrontmatter(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	src := []byte("+++\ntitle = \"My Docs\"\n+++\n# Heading\nContent.\n")
	if err := p.Parse(g, "/repo/test.md", src); err != nil {
		t.Fatal(err)
	}
	sections := g.FindByType(graph.NodeSection)
	if len(sections) != 1 {
		t.Fatalf("expected 1 section (TOML frontmatter skipped), got %d", len(sections))
	}
}

func TestMarkdownParser_SetextH1(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	src := []byte("My Title\n========\nSome body text.\n")
	if err := p.Parse(g, "/repo/test.md", src); err != nil {
		t.Fatal(err)
	}
	sections := g.FindByType(graph.NodeSection)
	if len(sections) != 1 {
		t.Fatalf("expected 1 setext H1 section, got %d", len(sections))
	}
	if sections[0].Metadata["title"] != "My Title" {
		t.Errorf("title = %q, want 'My Title'", sections[0].Metadata["title"])
	}
	if sections[0].Metadata["depth"] != "1" {
		t.Errorf("depth = %q, want '1'", sections[0].Metadata["depth"])
	}
	if !strings.Contains(sections[0].Metadata["body_preview"], "Some body text") {
		t.Errorf("body_preview missing body: %q", sections[0].Metadata["body_preview"])
	}
}

func TestMarkdownParser_SetextH2(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	src := []byte("Subtitle\n--------\nBody here.\n")
	if err := p.Parse(g, "/repo/test.md", src); err != nil {
		t.Fatal(err)
	}
	sections := g.FindByType(graph.NodeSection)
	if len(sections) != 1 {
		t.Fatalf("expected 1 setext H2 section, got %d", len(sections))
	}
	if sections[0].Metadata["depth"] != "2" {
		t.Errorf("depth = %q, want '2'", sections[0].Metadata["depth"])
	}
}

func TestMarkdownParser_SetextAndATXMixed(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	src := []byte("Top Section\n===========\nIntro.\n## Sub Heading\nDetails.\n")
	if err := p.Parse(g, "/repo/test.md", src); err != nil {
		t.Fatal(err)
	}
	sections := g.FindByType(graph.NodeSection)
	if len(sections) != 2 {
		t.Fatalf("expected 2 sections, got %d", len(sections))
	}
}

func TestMarkdownParser_FrontmatterThenSetextH1(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	src := []byte("---\ntitle: Doc\n---\nReal Title\n==========\nContent.\n")
	if err := p.Parse(g, "/repo/test.md", src); err != nil {
		t.Fatal(err)
	}
	sections := g.FindByType(graph.NodeSection)
	if len(sections) != 1 {
		t.Fatalf("expected 1 section (frontmatter skipped, setext parsed), got %d", len(sections))
	}
	if sections[0].Metadata["title"] != "Real Title" {
		t.Errorf("title = %q, want 'Real Title'", sections[0].Metadata["title"])
	}
}

func TestMarkdownParser_FrontmatterTitleExtracted(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	src := []byte("---\ntitle: \"FlatGraph Architecture\"\nauthor: foo\n---\n# Intro\nBody.\n")
	if err := p.Parse(g, "/repo/README.md", src); err != nil {
		t.Fatal(err)
	}
	fileNodeID := g.MakeNodeID("/repo/README.md", "/repo/README.md")
	var fileNode *graph.Node
	for _, n := range g.AllNodes() {
		if n.ID == fileNodeID {
			fileNode = n
			break
		}
	}
	if fileNode == nil {
		t.Fatal("file node not found")
	}
	if fileNode.Metadata["frontmatter_title"] != "FlatGraph Architecture" {
		t.Errorf("frontmatter_title = %q, want %q", fileNode.Metadata["frontmatter_title"], "FlatGraph Architecture")
	}
}

func TestMarkdownParser_TOMLFrontmatterTitleExtracted(t *testing.T) {
	g := newMarkdownTestGraph()
	p := NewMarkdownParser()
	src := []byte("+++\ntitle = \"Walker Subsystem\"\n+++\n# Docs\nContent.\n")
	if err := p.Parse(g, "/repo/README.md", src); err != nil {
		t.Fatal(err)
	}
	fileNodeID := g.MakeNodeID("/repo/README.md", "/repo/README.md")
	for _, n := range g.AllNodes() {
		if n.ID == fileNodeID {
			if n.Metadata["frontmatter_title"] != "Walker Subsystem" {
				t.Errorf("frontmatter_title = %q, want %q", n.Metadata["frontmatter_title"], "Walker Subsystem")
			}
			return
		}
	}
	t.Fatal("file node not found")
}

// assertEdge checks that an edge exists from→to with the given type.
func assertEdge(t *testing.T, g *graph.Graph, from, to graph.NodeID, edgeType graph.EdgeType) {
	t.Helper()
	edges := g.OutEdges(from)
	for _, e := range edges {
		if e.To == to && e.Type == edgeType {
			return
		}
	}
	t.Errorf("missing edge %s --%s--> %s", from, edgeType, to)
}
