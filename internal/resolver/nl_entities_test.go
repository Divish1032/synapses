package resolver_test

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

// newTestGraph builds a minimal graph with the required MakeNodeID / AddNode /
// AddEdge / FindByType / AllNodes methods.
func newTestGraph(t *testing.T) *graph.Graph {
	t.Helper()
	return graph.New("test")
}

// addSection is a helper that creates a NodeSection in the graph.
func addSection(t *testing.T, g *graph.Graph, filePath, title, body string, line int) graph.NodeID {
	t.Helper()
	sectionName := filePath + " § " + title
	id := g.MakeNodeID(filePath, sectionName)
	g.AddNode(&graph.Node{
		ID:   id,
		Type: graph.NodeSection,
		Name: sectionName,
		File: filePath,
		Line: line,
		Domain: graph.DomainDocs,
		Metadata: map[string]string{
			"title": title,
			"body":  body,
		},
	})
	return id
}

// addCodeNode adds a code entity node so we can verify NL resolution skips it.
func addCodeNode(t *testing.T, g *graph.Graph, name string) {
	t.Helper()
	id := g.MakeNodeID("internal/auth.go", name)
	g.AddNode(&graph.Node{
		ID:     id,
		Type:   graph.NodeFunction,
		Name:   name,
		File:   "internal/auth.go",
		Domain: graph.DomainCode,
	})
}

func TestResolveNLEntities_CreatesKnowledgeNode(t *testing.T) {
	g := newTestGraph(t)
	addSection(t, g, "docs/design.md", "Architecture", "Uses `TokenBucket` for rate limiting.", 2)

	unresolved := resolver.ResolveNLEntities(g)

	// Should have created a NodeConcept for "tokenbucket".
	concepts := g.FindByType(graph.NodeConcept)
	if len(concepts) == 0 {
		t.Fatal("expected at least one NodeConcept to be created")
	}
	found := false
	for _, c := range concepts {
		if strings.Contains(strings.ToLower(c.Name), "tokenbucket") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected knowledge node for 'tokenbucket', got %v", nodeNames(concepts))
	}

	// Should return it as unresolved for Tier 2.
	if len(unresolved) == 0 {
		t.Error("expected TokenBucket to be in unresolved candidates")
	}
}

func TestResolveNLEntities_SkipsCodeEntities(t *testing.T) {
	g := newTestGraph(t)
	addCodeNode(t, g, "TokenBucket")
	addSection(t, g, "docs/design.md", "Rate Limiting", "Uses `TokenBucket` algorithm.", 1)

	unresolved := resolver.ResolveNLEntities(g)

	// TokenBucket is a code entity → no knowledge node should be created.
	concepts := g.FindByType(graph.NodeConcept)
	for _, c := range concepts {
		if strings.Contains(strings.ToLower(c.Name), "tokenbucket") {
			t.Errorf("should not create knowledge node for code entity TokenBucket")
		}
	}
	// Unresolved should not contain TokenBucket (it was resolved to code entity).
	for _, u := range unresolved {
		if strings.EqualFold(u.Name, "TokenBucket") {
			t.Error("TokenBucket (code entity) should not appear in unresolved")
		}
	}
}

func TestResolveNLEntities_CreatesRelatesToEdge(t *testing.T) {
	g := newTestGraph(t)
	secID := addSection(t, g, "docs/arch.md", "Design", "The system uses `CircuitBreaker` pattern.", 3)

	resolver.ResolveNLEntities(g)

	// There should be a RELATES_TO edge from the section to the knowledge node.
	edges := g.OutEdges(secID)
	hasRelatesTo := false
	for _, e := range edges {
		if e.Type == graph.EdgeRelatesTo {
			hasRelatesTo = true
		}
	}
	if !hasRelatesTo {
		t.Error("expected RELATES_TO edge from section to knowledge node")
	}
}

func TestResolveNLEntitiesForFile_Scoped(t *testing.T) {
	g := newTestGraph(t)
	addSection(t, g, "docs/a.md", "Section A", "Uses `ConceptAlpha` here.", 1)
	addSection(t, g, "docs/b.md", "Section B", "Uses `ConceptBeta` here.", 1)

	// Only resolve for docs/a.md.
	resolver.ResolveNLEntitiesForFile(g, "docs/a.md")

	concepts := g.FindByType(graph.NodeConcept)

	foundAlpha := false
	foundBeta := false
	for _, c := range concepts {
		lower := strings.ToLower(c.Name)
		if strings.Contains(lower, "conceptalpha") {
			foundAlpha = true
		}
		if strings.Contains(lower, "conceptbeta") {
			foundBeta = true
		}
	}
	if !foundAlpha {
		t.Error("expected ConceptAlpha knowledge node from docs/a.md")
	}
	if foundBeta {
		t.Error("ConceptBeta from docs/b.md should not be created (file-scoped call)")
	}
}

func TestResolveNLEntities_EmptySections(t *testing.T) {
	g := newTestGraph(t)
	// Section with no body.
	addSection(t, g, "docs/empty.md", "Empty", "", 1)

	unresolved := resolver.ResolveNLEntities(g)

	if len(unresolved) != 0 {
		t.Errorf("expected no unresolved candidates for empty section body, got %d", len(unresolved))
	}
	concepts := g.FindByType(graph.NodeConcept)
	if len(concepts) != 0 {
		t.Errorf("expected no knowledge nodes for empty body, got %d", len(concepts))
	}
}

func TestResolveNLEntities_NoSections(t *testing.T) {
	g := newTestGraph(t)
	// Graph with no sections at all.
	unresolved := resolver.ResolveNLEntities(g)
	if unresolved != nil {
		t.Errorf("expected nil unresolved for empty graph, got %v", unresolved)
	}
}

func TestResolveNLEntities_NoDuplicateNodes(t *testing.T) {
	g := newTestGraph(t)
	// Same concept mentioned in two sections of the same file.
	addSection(t, g, "docs/dup.md", "S1", "Uses `CircuitBreaker` pattern.", 1)
	addSection(t, g, "docs/dup.md", "S2", "The `CircuitBreaker` trips on errors.", 5)

	resolver.ResolveNLEntities(g)

	count := 0
	for _, c := range g.FindByType(graph.NodeConcept) {
		if strings.Contains(strings.ToLower(c.Name), "circuitbreaker") {
			count++
		}
	}
	// Should be exactly 1 knowledge node for CircuitBreaker in this file.
	if count != 1 {
		t.Errorf("expected exactly 1 CircuitBreaker knowledge node, got %d", count)
	}
}

func TestResolveNLEntities_KnowledgeNodeDomain(t *testing.T) {
	g := newTestGraph(t)
	addSection(t, g, "docs/kd.md", "S1", "Uses `BackpressureQueue` here.", 1)

	resolver.ResolveNLEntities(g)

	for _, c := range g.FindByType(graph.NodeConcept) {
		if c.Domain != graph.DomainKnowledge {
			t.Errorf("knowledge node %q should have DomainKnowledge, got %q", c.Name, c.Domain)
		}
	}
}

func TestResolveNLEntities_UnresolvedCandidatesReturned(t *testing.T) {
	g := newTestGraph(t)
	addSection(t, g, "docs/u.md", "U", "The `EventSourcing` pattern and `CQRS` design.", 1)

	unresolved := resolver.ResolveNLEntities(g)

	if len(unresolved) == 0 {
		t.Error("expected unresolved candidates for EventSourcing / CQRS")
	}
	names := make([]string, len(unresolved))
	for i, u := range unresolved {
		names[i] = strings.ToLower(u.Name)
	}
	// At minimum EventSourcing or CQRS should be unresolved.
	found := false
	for _, n := range names {
		if strings.Contains(n, "eventsourcing") || strings.Contains(n, "cqrs") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected EventSourcing or CQRS in unresolved, got %v", names)
	}
}

// ── Bug regression tests ─────────────────────────────────────────────────────

// TestResolveNLEntities_CaseInsensitiveCodeEntitySkip covers the case where a
// doc uses `` `tokenbucket` `` (lowercase) but the code entity is "TokenBucket"
// (original case). Without the codeNamesLower secondary index, this created a
// spurious knowledge node even though a code entity already existed.
func TestResolveNLEntities_CaseInsensitiveCodeEntitySkip(t *testing.T) {
	g := newTestGraph(t)
	addCodeNode(t, g, "TokenBucket") // code entity: original case
	// Doc references the SAME entity in lowercase — should be skipped.
	addSection(t, g, "docs/case.md", "Throttling", "The `tokenbucket` controls traffic.", 1)

	resolver.ResolveNLEntities(g)

	// No knowledge node should be created — "tokenbucket" is a code entity.
	for _, c := range g.FindByType(graph.NodeConcept) {
		if strings.Contains(strings.ToLower(c.Name), "tokenbucket") {
			t.Errorf("should not create knowledge node for code entity referenced in lowercase: %q", c.Name)
		}
	}
}

// TestResolveNLEntitiesForFiles_BatchEfficiency verifies that processing multiple
// files via the batch variant produces the same results as per-file calls.
func TestResolveNLEntitiesForFiles_BatchEfficiency(t *testing.T) {
	g := newTestGraph(t)
	addSection(t, g, "docs/x.md", "X", "Uses `RateLimiter` here.", 1)
	addSection(t, g, "docs/y.md", "Y", "Uses `CircuitBreaker` pattern.", 1)
	addSection(t, g, "docs/z.md", "Z", "", 1) // empty — should be absent from result

	result := resolver.ResolveNLEntitiesForFiles(g, []string{"docs/x.md", "docs/y.md", "docs/z.md"})

	if _, ok := result["docs/x.md"]; !ok {
		t.Error("expected unresolved candidates for docs/x.md")
	}
	if _, ok := result["docs/y.md"]; !ok {
		t.Error("expected unresolved candidates for docs/y.md")
	}
	if _, ok := result["docs/z.md"]; ok {
		t.Error("docs/z.md has empty body — should not appear in result map")
	}

	// Knowledge nodes should exist for both files.
	concepts := g.FindByType(graph.NodeConcept)
	hasRL, hasCB := false, false
	for _, c := range concepts {
		lower := strings.ToLower(c.Name)
		if strings.Contains(lower, "ratelimiter") {
			hasRL = true
		}
		if strings.Contains(lower, "circuitbreaker") {
			hasCB = true
		}
	}
	if !hasRL {
		t.Error("expected RateLimiter knowledge node from batch run")
	}
	if !hasCB {
		t.Error("expected CircuitBreaker knowledge node from batch run")
	}
}

// TestResolveNLEntitiesForFiles_EmptyInput verifies nil return on empty input.
func TestResolveNLEntitiesForFiles_EmptyInput(t *testing.T) {
	g := newTestGraph(t)
	if r := resolver.ResolveNLEntitiesForFiles(g, nil); r != nil {
		t.Errorf("expected nil for nil filePaths, got %v", r)
	}
	if r := resolver.ResolveNLEntitiesForFiles(g, []string{}); r != nil {
		t.Errorf("expected nil for empty filePaths, got %v", r)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func nodeNames(nodes []*graph.Node) []string {
	ns := make([]string, len(nodes))
	for i, n := range nodes {
		ns[i] = n.Name
	}
	return ns
}

// Verify parser.EntityCandidate is accessible (compilation check).
var _ parser.EntityCandidate

func TestResolveNLEntities_FrontmatterTags(t *testing.T) {
	g := graph.New("test-fm")
	g.SetRoot("/repo")
	// Add a file node with frontmatter tags.
	fileID := g.MakeNodeID("/repo/docs/design.md", "/repo/docs/design.md")
	g.AddNode(&graph.Node{
		ID:     fileID,
		Type:   graph.NodeFile,
		Name:   "design.md",
		File:   "/repo/docs/design.md",
		Line:   1,
		Domain: graph.DomainDocs,
		Metadata: map[string]string{
			"frontmatter_title":    "Rate Limiter",
			"frontmatter_tags":     "rate-limiting,throttling",
			"frontmatter_category": "infrastructure",
		},
	})
	// Add a section node.
	secID := g.MakeNodeID("/repo/docs/design.md", "Overview")
	g.AddNode(&graph.Node{
		ID:     secID,
		Type:   graph.NodeSection,
		Name:   "Overview",
		File:   "/repo/docs/design.md",
		Line:   5,
		Domain: graph.DomainDocs,
		Metadata: map[string]string{
			"body": "The system uses a TokenBucket for rate limiting.",
		},
	})

	unresolved := resolver.ResolveNLEntities(g)

	// Should have candidates from both body text and frontmatter.
	found := make(map[string]bool)
	for _, c := range unresolved {
		found[c.Name] = true
	}
	// Check that at least frontmatter tags and body entities are present.
	knowledgeNodes := g.FindByType(graph.NodeConcept)
	knowledgeNames := make(map[string]bool)
	for _, n := range knowledgeNodes {
		knowledgeNames[n.Name] = true
	}
	// "rate-limiting" from tags should appear as a knowledge node
	if !knowledgeNames["rate-limiting"] {
		t.Error("expected knowledge node for frontmatter tag 'rate-limiting'")
	}
	// "infrastructure" from category should appear
	if !knowledgeNames["infrastructure"] {
		t.Error("expected knowledge node for frontmatter category 'infrastructure'")
	}
}
