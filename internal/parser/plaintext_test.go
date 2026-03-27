package parser

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// collectSections returns section nodes sorted by line number.
func collectSections(g *graph.Graph) []*graph.Node {
	var sections []*graph.Node
	for _, n := range g.AllNodes() {
		if n.Type == graph.NodeSection {
			sections = append(sections, n)
		}
	}
	sort.Slice(sections, func(i, j int) bool {
		return sections[i].Line < sections[j].Line
	})
	return sections
}

func newPlaintextTestGraph() *graph.Graph {
	g := graph.New("test")
	g.SetRoot("/repo")
	return g
}

// ── Extension Tests ─────────────────────────────────────────────────────────

func TestPlaintextParser_Extensions(t *testing.T) {
	p := NewPlaintextParser()
	exts := p.Extensions()
	want := map[string]bool{".txt": true, ".rst": true}
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

// ── RST Tests ───────────────────────────────────────────────────────────────

func TestPlaintextParser_RST_UnderlineOnly(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	src := []byte(`Introduction
============

This is the intro.

Getting Started
---------------

Install the tool first.

Advanced Usage
--------------

Use with flags.
`)

	if err := p.Parse(g, "/repo/docs/guide.rst", src); err != nil {
		t.Fatal(err)
	}

	sections := collectSections(g)

	if len(sections) != 3 {
		t.Fatalf("expected 3 sections, got %d", len(sections))
	}

	// Check depths: = first-seen → depth 1, - second-seen → depth 2
	wantTitles := []struct {
		title string
		depth string
	}{
		{"Introduction", "1"},
		{"Getting Started", "2"},
		{"Advanced Usage", "2"},
	}
	for i, want := range wantTitles {
		if sections[i].Metadata["title"] != want.title {
			t.Errorf("section %d: title = %q, want %q", i, sections[i].Metadata["title"], want.title)
		}
		if sections[i].Metadata["depth"] != want.depth {
			t.Errorf("section %d: depth = %q, want %q", i, sections[i].Metadata["depth"], want.depth)
		}
		if sections[i].Domain != graph.DomainDocs {
			t.Errorf("section %d: domain = %q, want %q", i, sections[i].Domain, graph.DomainDocs)
		}
	}

	// Check body text is populated.
	if !strings.Contains(sections[0].Metadata["body"], "This is the intro") {
		t.Errorf("section 0 body missing intro text: %q", sections[0].Metadata["body"])
	}
	if !strings.Contains(sections[1].Metadata["body"], "Install the tool") {
		t.Errorf("section 1 body missing install text: %q", sections[1].Metadata["body"])
	}
}

func TestPlaintextParser_RST_OverlineAndUnderline(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	src := []byte(`========
Overview
========

Top-level chapter.

Details
-------

Sub-section here.
`)

	if err := p.Parse(g, "/repo/docs/chapter.rst", src); err != nil {
		t.Fatal(err)
	}

	sections := collectSections(g)

	if len(sections) != 2 {
		t.Fatalf("expected 2 sections, got %d", len(sections))
	}

	// Overline+underline with = → depth 1 (first seen).
	// Underline-only with - → depth 2 (second seen).
	if sections[0].Metadata["title"] != "Overview" {
		t.Errorf("section 0: title = %q, want %q", sections[0].Metadata["title"], "Overview")
	}
	if sections[0].Metadata["depth"] != "1" {
		t.Errorf("section 0: depth = %q, want %q", sections[0].Metadata["depth"], "1")
	}
	if sections[1].Metadata["title"] != "Details" {
		t.Errorf("section 1: title = %q, want %q", sections[1].Metadata["title"], "Details")
	}
	if sections[1].Metadata["depth"] != "2" {
		t.Errorf("section 1: depth = %q, want %q", sections[1].Metadata["depth"], "2")
	}
}

func TestPlaintextParser_RST_DepthByCharOrder(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	// ~ appears first → depth 1, = second → depth 2, - third → depth 3
	src := []byte(`Part One
~~~~~~~~

Content A.

Section Alpha
=============

Content B.

Subsection
----------

Content C.
`)

	if err := p.Parse(g, "/repo/docs/ordered.rst", src); err != nil {
		t.Fatal(err)
	}

	sections := collectSections(g)

	if len(sections) != 3 {
		t.Fatalf("expected 3 sections, got %d", len(sections))
	}

	depths := []string{"1", "2", "3"}
	for i, want := range depths {
		if sections[i].Metadata["depth"] != want {
			t.Errorf("section %d (%s): depth = %q, want %q",
				i, sections[i].Metadata["title"], sections[i].Metadata["depth"], want)
		}
	}
}

func TestPlaintextParser_RST_DuplicateTitles(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	src := []byte(`Overview
========

First overview.

Overview
========

Second overview.
`)

	if err := p.Parse(g, "/repo/docs/dup.rst", src); err != nil {
		t.Fatal(err)
	}

	sections := collectSections(g)
	if len(sections) != 2 {
		t.Fatalf("expected 2 sections, got %d", len(sections))
	}
	if sections[0].Metadata["title"] != "Overview" {
		t.Errorf("first title = %q, want %q", sections[0].Metadata["title"], "Overview")
	}
	if sections[1].Metadata["title"] != "Overview (2)" {
		t.Errorf("second title = %q, want %q", sections[1].Metadata["title"], "Overview (2)")
	}
}

// ── TXT Tests ───────────────────────────────────────────────────────────────

func TestPlaintextParser_TXT_AllCapsHeadings(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	src := []byte(`INTRODUCTION

This is the introduction paragraph.

GETTING STARTED

Follow these steps to begin.
`)

	if err := p.Parse(g, "/repo/docs/readme.txt", src); err != nil {
		t.Fatal(err)
	}

	sections := collectSections(g)

	if len(sections) != 2 {
		t.Fatalf("expected 2 sections, got %d", len(sections))
	}

	// ALL-CAPS converted to Title Case.
	if sections[0].Metadata["title"] != "Introduction" {
		t.Errorf("section 0: title = %q, want %q", sections[0].Metadata["title"], "Introduction")
	}
	if sections[0].Metadata["depth"] != "1" {
		t.Errorf("section 0: depth = %q, want %q", sections[0].Metadata["depth"], "1")
	}
	if !strings.Contains(sections[0].Metadata["body"], "introduction paragraph") {
		t.Errorf("section 0 body missing content: %q", sections[0].Metadata["body"])
	}
}

func TestPlaintextParser_TXT_ColonHeadings(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	src := []byte(`OVERVIEW

Some intro text.

Installation steps:

1. Download the binary.
2. Run the installer.

Configuration options:

Set FOO=bar in your env.
`)

	if err := p.Parse(g, "/repo/docs/setup.txt", src); err != nil {
		t.Fatal(err)
	}

	sections := collectSections(g)

	// Expect: OVERVIEW (H1), "Installation steps" (H2), "Configuration options" (H2)
	if len(sections) != 3 {
		t.Fatalf("expected 3 sections, got %d", len(sections))
	}

	if sections[1].Metadata["title"] != "Installation steps" {
		t.Errorf("section 1: title = %q, want %q", sections[1].Metadata["title"], "Installation steps")
	}
	if sections[1].Metadata["depth"] != "2" {
		t.Errorf("section 1: depth = %q, want %q", sections[1].Metadata["depth"], "2")
	}
	if sections[2].Metadata["title"] != "Configuration options" {
		t.Errorf("section 2: title = %q, want %q", sections[2].Metadata["title"], "Configuration options")
	}
}

func TestPlaintextParser_TXT_ParagraphFallback(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	// No headings at all — should fall back to paragraph splitting.
	src := []byte(`This is the first paragraph about authentication.
It spans multiple lines.

This is the second paragraph about authorization.
It also has multiple lines.
`)

	if err := p.Parse(g, "/repo/docs/notes.txt", src); err != nil {
		t.Fatal(err)
	}

	sections := collectSections(g)

	if len(sections) != 2 {
		t.Fatalf("expected 2 sections (paragraphs), got %d", len(sections))
	}

	// First line of paragraph used as title.
	if !strings.Contains(sections[0].Metadata["title"], "authentication") {
		t.Errorf("section 0: title should contain 'authentication': %q", sections[0].Metadata["title"])
	}
	if !strings.Contains(sections[1].Metadata["title"], "authorization") {
		t.Errorf("section 1: title should contain 'authorization': %q", sections[1].Metadata["title"])
	}
}

// ── Edge Tests ──────────────────────────────────────────────────────────────

func TestPlaintextParser_ContainsEdges(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	src := []byte(`Overview
========

Top level.

Details
-------

Sub section.
`)

	if err := p.Parse(g, "/repo/docs/api.rst", src); err != nil {
		t.Fatal(err)
	}

	edges := g.AllEdges()
	var containsEdges []*graph.Edge
	for _, e := range edges {
		if e.Type == graph.EdgeContains {
			containsEdges = append(containsEdges, e)
		}
	}

	// File → Overview, Overview → Details
	if len(containsEdges) != 2 {
		t.Fatalf("expected 2 CONTAINS edges, got %d", len(containsEdges))
	}

	fileNodeID := g.MakeNodeID("/repo/docs/api.rst", "/repo/docs/api.rst")
	overviewNodeID := g.MakeNodeID("/repo/docs/api.rst", "api.rst § Overview")

	if containsEdges[0].From != fileNodeID {
		t.Errorf("edge 0: from = %q, want file node", containsEdges[0].From)
	}
	if containsEdges[0].To != overviewNodeID {
		t.Errorf("edge 0: to = %q, want Overview node", containsEdges[0].To)
	}
	// Details is depth 2, parent is Overview (depth 1).
	if containsEdges[1].From != overviewNodeID {
		t.Errorf("edge 1: from = %q, want Overview node", containsEdges[1].From)
	}
}

// ── Empty/Small File Tests ──────────────────────────────────────────────────

func TestPlaintextParser_EmptyFile(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	if err := p.Parse(g, "/repo/docs/empty.txt", []byte("")); err != nil {
		t.Fatal(err)
	}

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

func TestPlaintextParser_RST_EmptyFile(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	if err := p.Parse(g, "/repo/docs/empty.rst", []byte("")); err != nil {
		t.Fatal(err)
	}

	nodes := g.AllNodes()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node (file), got %d", len(nodes))
	}
}

// ── Body Truncation Tests ───────────────────────────────────────────────────

func TestPlaintextParser_BodyTruncation(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	// Create a section with >2000 chars of body.
	longBody := strings.Repeat("x", 2500)
	src := []byte("Overview\n========\n\n" + longBody + "\n")

	if err := p.Parse(g, "/repo/docs/long.rst", src); err != nil {
		t.Fatal(err)
	}

	var sec *graph.Node
	for _, n := range g.AllNodes() {
		if n.Type == graph.NodeSection {
			sec = n
		}
	}
	if sec == nil {
		t.Fatal("no section node found")
	}

	if len(sec.Metadata["body"]) > 2000 {
		t.Errorf("body should be truncated to 2000, got %d", len(sec.Metadata["body"]))
	}
	if len(sec.Metadata["body_preview"]) > 200 {
		t.Errorf("body_preview should be truncated to 200, got %d", len(sec.Metadata["body_preview"]))
	}
}

// ── Section Name Format Test ────────────────────────────────────────────────

func TestPlaintextParser_SectionNameFormat(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	src := []byte("Setup\n=====\n\nDo stuff.\n")

	if err := p.Parse(g, "/repo/docs/install.rst", src); err != nil {
		t.Fatal(err)
	}

	for _, n := range g.AllNodes() {
		if n.Type == graph.NodeSection {
			want := "install.rst § Setup"
			if n.Name != want {
				t.Errorf("name = %q, want %q", n.Name, want)
			}
		}
	}
}

// ── RST short underline (should not match) ──────────────────────────────────

func TestPlaintextParser_RST_ShortUnderlineIgnored(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	// Underline shorter than title → not a heading.
	src := []byte("This is a long title\n--\n\nJust text.\n")

	if err := p.Parse(g, "/repo/docs/short.rst", src); err != nil {
		t.Fatal(err)
	}

	for _, n := range g.AllNodes() {
		if n.Type == graph.NodeSection {
			t.Errorf("should not have created a section for short underline, got %q", n.Name)
		}
	}
}

// ── RST Code Block Tests ────────────────────────────────────────────────────

func TestPlaintextParser_RST_CodeBlockExtraction(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	src := []byte(`Introduction
============

Install the package:

.. code-block:: python

   from mylib import Client
   client = Client()

Then configure:

.. code-block:: bash

   export API_KEY=xxx
`)

	if err := p.Parse(g, "/repo/docs/guide.rst", src); err != nil {
		t.Fatal(err)
	}

	sections := collectSections(g)
	if len(sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(sections))
	}

	cbJSON := sections[0].Metadata["code_blocks"]
	if cbJSON == "" {
		t.Fatal("code_blocks metadata missing")
	}
	var blocks []codeBlock
	if err := json.Unmarshal([]byte(cbJSON), &blocks); err != nil {
		t.Fatalf("failed to unmarshal code_blocks: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 code blocks, got %d", len(blocks))
	}
	if blocks[0].Language != "python" {
		t.Errorf("block 0 language = %q, want python", blocks[0].Language)
	}
	if !strings.Contains(blocks[0].Content, "from mylib import Client") {
		t.Errorf("block 0 content missing expected code: %q", blocks[0].Content)
	}
	if blocks[1].Language != "bash" {
		t.Errorf("block 1 language = %q, want bash", blocks[1].Language)
	}
}

func TestPlaintextParser_RST_CodeBlockWithOptions(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	// RST code block with directive options (:linenos:, :emphasize-lines:)
	// that should NOT appear in the extracted content.
	src := []byte(`Introduction
============

Example:

.. code-block:: python
   :linenos:
   :emphasize-lines: 1

   from flask import Flask
   app = Flask(__name__)
`)

	if err := p.Parse(g, "/repo/docs/guide.rst", src); err != nil {
		t.Fatal(err)
	}

	sections := collectSections(g)
	if len(sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(sections))
	}

	cbJSON := sections[0].Metadata["code_blocks"]
	if cbJSON == "" {
		t.Fatal("code_blocks metadata missing")
	}
	var blocks []codeBlock
	if err := json.Unmarshal([]byte(cbJSON), &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 code block, got %d", len(blocks))
	}
	// Content should NOT contain the :linenos: directive option.
	if strings.Contains(blocks[0].Content, ":linenos:") {
		t.Error("directive options should be stripped from code block content")
	}
	if !strings.Contains(blocks[0].Content, "from flask import Flask") {
		t.Errorf("code block content missing expected code: %q", blocks[0].Content)
	}
}

func TestPlaintextParser_RST_CodeBlockTabIndent(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	// RST code block with tab indentation.
	src := []byte("Overview\n========\n\n.. code-block:: go\n\n\tfmt.Println(\"hello\")\n\tfmt.Println(\"world\")\n")

	if err := p.Parse(g, "/repo/docs/tab.rst", src); err != nil {
		t.Fatal(err)
	}

	sections := collectSections(g)
	if len(sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(sections))
	}

	cbJSON := sections[0].Metadata["code_blocks"]
	if cbJSON == "" {
		t.Fatal("code_blocks metadata missing for tab-indented block")
	}
	var blocks []codeBlock
	if err := json.Unmarshal([]byte(cbJSON), &blocks); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(blocks[0].Content, "fmt.Println") {
		t.Errorf("tab-indented code block content corrupted: %q", blocks[0].Content)
	}
}

func TestPlaintextParser_RST_CodeBlockMaxFive(t *testing.T) {
	g := newPlaintextTestGraph()
	p := NewPlaintextParser()

	var sb strings.Builder
	sb.WriteString("Overview\n========\n\n")
	for i := 0; i < 7; i++ {
		sb.WriteString(".. code-block:: python\n\n   print(" + string(rune('A'+i)) + ")\n\n")
	}

	if err := p.Parse(g, "/repo/docs/many.rst", []byte(sb.String())); err != nil {
		t.Fatal(err)
	}

	sections := collectSections(g)
	if len(sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(sections))
	}
	var blocks []codeBlock
	if err := json.Unmarshal([]byte(sections[0].Metadata["code_blocks"]), &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 5 {
		t.Errorf("expected max 5 code blocks, got %d", len(blocks))
	}
}
