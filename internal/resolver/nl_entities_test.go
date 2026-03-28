package resolver_test

import (
	"context"
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

	unresolved := resolver.ResolveNLEntities(g, nil)

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

	unresolved := resolver.ResolveNLEntities(g, nil)

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

	resolver.ResolveNLEntities(g, nil)

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
	resolver.ResolveNLEntitiesForFile(g, "docs/a.md", nil)

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

	unresolved := resolver.ResolveNLEntities(g, nil)

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
	unresolved := resolver.ResolveNLEntities(g, nil)
	if unresolved != nil {
		t.Errorf("expected nil unresolved for empty graph, got %v", unresolved)
	}
}

func TestResolveNLEntities_NoDuplicateNodes(t *testing.T) {
	g := newTestGraph(t)
	// Same concept mentioned in two sections of the same file.
	addSection(t, g, "docs/dup.md", "S1", "Uses `CircuitBreaker` pattern.", 1)
	addSection(t, g, "docs/dup.md", "S2", "The `CircuitBreaker` trips on errors.", 5)

	resolver.ResolveNLEntities(g, nil)

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

	resolver.ResolveNLEntities(g, nil)

	for _, c := range g.FindByType(graph.NodeConcept) {
		if c.Domain != graph.DomainKnowledge {
			t.Errorf("knowledge node %q should have DomainKnowledge, got %q", c.Name, c.Domain)
		}
	}
}

func TestResolveNLEntities_UnresolvedCandidatesReturned(t *testing.T) {
	g := newTestGraph(t)
	addSection(t, g, "docs/u.md", "U", "The `EventSourcing` pattern and `CQRS` design.", 1)

	unresolved := resolver.ResolveNLEntities(g, nil)

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

	resolver.ResolveNLEntities(g, nil)

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

	result := resolver.ResolveNLEntitiesForFiles(g, []string{"docs/x.md", "docs/y.md", "docs/z.md"}, nil)

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
	if r := resolver.ResolveNLEntitiesForFiles(g, nil, nil); r != nil {
		t.Errorf("expected nil for nil filePaths, got %v", r)
	}
	if r := resolver.ResolveNLEntitiesForFiles(g, []string{}, nil); r != nil {
		t.Errorf("expected nil for empty filePaths, got %v", r)
	}
}

// ── Embedding-based resolution tests ─────────────────────────────────────────

// mockEmbedResolver is a test double for resolver.EmbedResolver.
type mockEmbedResolver struct {
	embedErr  error
	embedVec  []float32
	matches   []resolver.EmbedMatch
}

func (m *mockEmbedResolver) EmbedText(_ context.Context, _ string) ([]float32, error) {
	if m.embedErr != nil {
		return nil, m.embedErr
	}
	return m.embedVec, nil
}

func (m *mockEmbedResolver) SearchByVector(_ []float32, _ int) []resolver.EmbedMatch {
	return m.matches
}

// TestResolveNLEntities_EmbedHighScore verifies that a high-similarity match
// (cosine > 0.6) wires a DOCUMENTED_BY edge directly to the existing code node
// and does NOT create a new knowledge node or add to unresolved.
func TestResolveNLEntities_EmbedHighScore(t *testing.T) {
	g := newTestGraph(t)
	// Add a code entity node to match against.
	codeID := g.MakeNodeID("internal/limiter.go", "RateLimiter")
	g.AddNode(&graph.Node{
		ID:     codeID,
		Type:   graph.NodeStruct,
		Name:   "RateLimiter",
		File:   "internal/limiter.go",
		Domain: graph.DomainCode,
	})
	secID := addSection(t, g, "docs/rl.md", "Rate Limiting", "The `rate limiting strategy` controls burst.", 1)

	er := &mockEmbedResolver{
		embedVec: []float32{1, 0, 0},
		matches: []resolver.EmbedMatch{
			{NodeID: string(codeID), Score: 0.85}, // above 0.6 threshold
		},
	}

	unresolved := resolver.ResolveNLEntities(g, er)

	// High-score match: EXPLAINS edge should exist section → code node.
	// (EdgeExplains = Section→CodeEntity; EdgeDocumentedBy is the reverse.)
	edges := g.OutEdges(secID)
	hasExplains := false
	for _, e := range edges {
		if e.Type == graph.EdgeExplains && e.To == codeID {
			hasExplains = true
		}
	}
	if !hasExplains {
		t.Error("expected EXPLAINS edge from section to matched code node")
	}

	// No new knowledge node should be created for the matched candidate.
	concepts := g.FindByType(graph.NodeConcept)
	for _, c := range concepts {
		if strings.Contains(strings.ToLower(c.Name), "rate limiting") {
			t.Errorf("should not create knowledge node for high-score embed match: %q", c.Name)
		}
	}

	// Matched candidate should not appear in unresolved.
	for _, u := range unresolved {
		if strings.EqualFold(u.Name, "rate limiting strategy") {
			t.Error("high-score embed match should not appear in unresolved")
		}
	}
}

// TestResolveNLEntities_EmbedMidScore verifies that a mid-range similarity
// (0.4–0.6) creates a knowledge node with embed_hint metadata and adds to unresolved.
func TestResolveNLEntities_EmbedMidScore(t *testing.T) {
	g := newTestGraph(t)
	codeID := g.MakeNodeID("internal/limiter.go", "RateLimiter")
	g.AddNode(&graph.Node{
		ID:     codeID,
		Type:   graph.NodeStruct,
		Name:   "RateLimiter",
		File:   "internal/limiter.go",
		Domain: graph.DomainCode,
	})
	addSection(t, g, "docs/rl.md", "Throttle", "The `throttle mechanism` prevents abuse.", 1)

	er := &mockEmbedResolver{
		embedVec: []float32{1, 0, 0},
		matches: []resolver.EmbedMatch{
			{NodeID: string(codeID), Score: 0.52}, // in the 0.4–0.6 band
		},
	}

	unresolved := resolver.ResolveNLEntities(g, er)

	// A knowledge node should be created with embed_hint metadata.
	concepts := g.FindByType(graph.NodeConcept)
	var matched *graph.Node
	for _, c := range concepts {
		if strings.Contains(strings.ToLower(c.Name), "throttle") {
			matched = c
		}
	}
	if matched == nil {
		t.Fatal("expected knowledge node for mid-score embed match")
	}
	if matched.Metadata["embed_hint"] == "" {
		t.Error("expected embed_hint metadata on mid-score knowledge node")
	}
	if matched.Metadata["tier"] != "1" {
		t.Errorf("expected tier=1 on embed-assisted node, got %q", matched.Metadata["tier"])
	}

	// Mid-score candidate should appear in unresolved for Tier 2.
	found := false
	for _, u := range unresolved {
		if strings.Contains(strings.ToLower(u.Name), "throttle") {
			found = true
		}
	}
	if !found {
		t.Error("expected mid-score candidate in unresolved for Tier 2 review")
	}
}

// TestResolveNLEntities_EmbedLowScore verifies that a low similarity score
// (< 0.4) creates a plain knowledge node with tier=0, same as no-embedding path.
func TestResolveNLEntities_EmbedLowScore(t *testing.T) {
	g := newTestGraph(t)
	codeID := g.MakeNodeID("internal/limiter.go", "RateLimiter")
	g.AddNode(&graph.Node{
		ID:     codeID,
		Type:   graph.NodeStruct,
		Name:   "RateLimiter",
		File:   "internal/limiter.go",
		Domain: graph.DomainCode,
	})
	addSection(t, g, "docs/ev.md", "Events", "The `event sourcing pattern` is used.", 1)

	er := &mockEmbedResolver{
		embedVec: []float32{1, 0, 0},
		matches: []resolver.EmbedMatch{
			{NodeID: string(codeID), Score: 0.20}, // below mid threshold
		},
	}

	unresolved := resolver.ResolveNLEntities(g, er)

	// Should create a knowledge node with tier=0 (genuinely new concept).
	concepts := g.FindByType(graph.NodeConcept)
	var matched *graph.Node
	for _, c := range concepts {
		if strings.Contains(strings.ToLower(c.Name), "event sourcing") {
			matched = c
		}
	}
	if matched == nil {
		t.Fatal("expected knowledge node for low-score embed (new concept)")
	}
	if matched.Metadata["embed_hint"] != "" {
		t.Error("low-score match should not have embed_hint")
	}
	if matched.Metadata["tier"] != "0" {
		t.Errorf("expected tier=0 for low-score knowledge node, got %q", matched.Metadata["tier"])
	}

	// Should be in unresolved.
	if len(unresolved) == 0 {
		t.Error("expected low-score candidate in unresolved")
	}
}

// TestResolveNLEntities_EmbedHighScore_StaleNode verifies that when the HNSW
// returns a high-score match for a node ID that no longer exists in the graph
// (stale embedding), we gracefully fall through to standard knowledge node
// creation rather than creating a dangling edge.
func TestResolveNLEntities_EmbedHighScore_StaleNode(t *testing.T) {
	g := newTestGraph(t)
	// Note: we do NOT add any node for "stale-node-id" — it's absent from graph.
	addSection(t, g, "docs/stale.md", "Stale", "The `orphan concept` is referenced.", 1)

	er := &mockEmbedResolver{
		embedVec: []float32{1, 0, 0},
		matches: []resolver.EmbedMatch{
			{NodeID: "stale-node-id-that-does-not-exist", Score: 0.91},
		},
	}

	unresolved := resolver.ResolveNLEntities(g, er)

	// Stale node: should fall back to knowledge node creation (no dangling edge).
	concepts := g.FindByType(graph.NodeConcept)
	if len(concepts) == 0 {
		t.Error("expected fallback knowledge node when HNSW points to a non-existent node")
	}

	// Candidate should be in unresolved (not silently dropped).
	if len(unresolved) == 0 {
		t.Error("expected stale-node candidate in unresolved (fallback path)")
	}

	// No edge from any section should point to the non-existent stale node ID.
	for _, sec := range g.FindByType(graph.NodeSection) {
		for _, e := range g.OutEdges(sec.ID) {
			if string(e.To) == "stale-node-id-that-does-not-exist" {
				t.Error("must not create edge to non-existent (stale) node")
			}
		}
	}
}

// TestResolveNLEntities_EmbedError verifies graceful fallback when embedding fails.
func TestResolveNLEntities_EmbedError(t *testing.T) {
	g := newTestGraph(t)
	addSection(t, g, "docs/err.md", "Errors", "The `retry policy` is configurable.", 1)

	er := &mockEmbedResolver{
		embedErr: context.DeadlineExceeded, // simulate timeout
	}

	// Should fall back to name-match path (creates knowledge node, no panic).
	unresolved := resolver.ResolveNLEntities(g, er)

	concepts := g.FindByType(graph.NodeConcept)
	if len(concepts) == 0 {
		t.Error("expected knowledge node even when embedding fails (fallback to name-match)")
	}
	if len(unresolved) == 0 {
		t.Error("expected unresolved candidates when embedding fails")
	}
}

// TestResolveNLEntities_NilEmbedResolver verifies backward-compat: nil er
// behaves identically to the pre-embedding name-match-only path.
func TestResolveNLEntities_NilEmbedResolver(t *testing.T) {
	g := newTestGraph(t)
	addSection(t, g, "docs/nil.md", "Nil", "The `backpressure queue` is bounded.", 1)

	unresolved := resolver.ResolveNLEntities(g, nil)

	if len(unresolved) == 0 {
		t.Error("expected unresolved candidates with nil EmbedResolver")
	}
	concepts := g.FindByType(graph.NodeConcept)
	if len(concepts) == 0 {
		t.Error("expected knowledge node with nil EmbedResolver")
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

	unresolved := resolver.ResolveNLEntities(g, nil)

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
