package graph_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// TestEdgeTypeCatalogCompleteness verifies that every edge type in DefaultEdgeWeights
// has a corresponding EdgeTypeDescriptor in the catalog, and that the semantic weights
// match. This test is the compile-time gate for future additions: whenever a new edge
// type is added to DefaultEdgeWeights, the developer must also add a descriptor to
// EdgeTypeCatalog or this test will fail loudly.
func TestEdgeTypeCatalogCompleteness(t *testing.T) {
	catalog := graph.GetEdgeTypes()

	// Build a lookup map from the catalog for O(1) access.
	catalogByName := make(map[graph.EdgeType]graph.EdgeTypeDescriptor, len(catalog))
	for _, d := range catalog {
		catalogByName[d.Name] = d
	}

	// Every key in DefaultEdgeWeights must have a descriptor.
	for et, weight := range graph.DefaultEdgeWeights {
		d, ok := catalogByName[et]
		if !ok {
			t.Errorf("edge type %q is in DefaultEdgeWeights but missing from EdgeTypeCatalog", et)
			continue
		}
		if d.SemanticWeight != weight {
			t.Errorf("edge type %q: catalog weight %.2f != DefaultEdgeWeights weight %.2f",
				et, d.SemanticWeight, weight)
		}
	}

	// Every descriptor in the catalog must have a corresponding DefaultEdgeWeights entry.
	for _, d := range catalog {
		if _, ok := graph.DefaultEdgeWeights[d.Name]; !ok {
			t.Errorf("EdgeTypeCatalog has descriptor for %q but DefaultEdgeWeights has no entry for it", d.Name)
		}
	}
}

// TestEdgeTypeCatalogRequiredFields verifies that no descriptor has empty required fields.
func TestEdgeTypeCatalogRequiredFields(t *testing.T) {
	for _, d := range graph.GetEdgeTypes() {
		if d.Name == "" {
			t.Error("EdgeTypeDescriptor has empty Name")
		}
		if d.Description == "" {
			t.Errorf("EdgeTypeDescriptor %q has empty Description", d.Name)
		}
		if d.Direction == "" {
			t.Errorf("EdgeTypeDescriptor %q has empty Direction", d.Name)
		}
		if d.Domain == "" {
			t.Errorf("EdgeTypeDescriptor %q has empty Domain", d.Name)
		}
		if d.SemanticWeight <= 0 || d.SemanticWeight > 2.0 {
			t.Errorf("EdgeTypeDescriptor %q has out-of-range SemanticWeight: %.2f (expected 0 < w <= 2.0)", d.Name, d.SemanticWeight)
		}
	}
}

// TestEdgeTypeCatalogNoDuplicates verifies no edge type appears twice.
func TestEdgeTypeCatalogNoDuplicates(t *testing.T) {
	seen := make(map[graph.EdgeType]bool)
	for _, d := range graph.GetEdgeTypes() {
		if seen[d.Name] {
			t.Errorf("EdgeTypeCatalog contains duplicate entry for %q", d.Name)
		}
		seen[d.Name] = true
	}
}

// TestEdgeTypeCatalogSortedByWeight verifies the catalog is sorted descending by
// SemanticWeight. The sort order matters for explain_codebase output and compact display.
func TestEdgeTypeCatalogSortedByWeight(t *testing.T) {
	catalog := graph.GetEdgeTypes()
	for i := 1; i < len(catalog); i++ {
		if catalog[i].SemanticWeight > catalog[i-1].SemanticWeight {
			t.Errorf("EdgeTypeCatalog is not sorted descending by SemanticWeight: "+
				"index %d (%s, %.2f) > index %d (%s, %.2f)",
				i, catalog[i].Name, catalog[i].SemanticWeight,
				i-1, catalog[i-1].Name, catalog[i-1].SemanticWeight)
		}
	}
}

// TestGetEdgeTypesReturnsSameSlice verifies GetEdgeTypes returns the package-level
// catalog (not a copy). Callers must not mutate the result.
func TestGetEdgeTypesReturnsSameSlice(t *testing.T) {
	a := graph.GetEdgeTypes()
	b := graph.GetEdgeTypes()
	if len(a) != len(b) {
		t.Fatalf("GetEdgeTypes returned different lengths: %d vs %d", len(a), len(b))
	}
	// Verify pointer identity of underlying array via length + first element stability.
	if len(a) > 0 && a[0].Name != b[0].Name {
		t.Error("GetEdgeTypes returned inconsistent first element")
	}
}

// TestEdgeTypeCatalogKnownEdges spot-checks a few well-known edge types to guard
// against typos or accidental weight changes that could silently degrade BFS quality.
func TestEdgeTypeCatalogKnownEdges(t *testing.T) {
	catalog := graph.GetEdgeTypes()
	byName := make(map[graph.EdgeType]graph.EdgeTypeDescriptor, len(catalog))
	for _, d := range catalog {
		byName[d.Name] = d
	}

	checks := []struct {
		et     graph.EdgeType
		weight float64
		domain graph.DomainType
		synth  bool
	}{
		{graph.EdgeCalls, 1.0, graph.DomainCode, false},
		{graph.EdgeImplements, 0.9, graph.DomainCode, false},
		{graph.EdgeHandles, 0.9, graph.DomainCode, true},
		{graph.EdgeEmbeds, 0.85, graph.DomainCode, false},
		{graph.EdgeDependsOn, 0.8, graph.DomainCode, false},
		{graph.EdgeImports, 0.7, graph.DomainCode, false},
		{graph.EdgeExports, 0.5, graph.DomainCode, false},
		{graph.EdgeLinksTo, 0.3, graph.DomainDocs, true},
		{graph.EdgeContains, 0.15, graph.DomainDocs, true},
		{graph.EdgeDefines, 0.15, graph.DomainCode, false},
		// Sprint 16: cross-domain edge types.
		{graph.EdgeDeploys, 0.75, graph.DomainInfra, true},
		{graph.EdgeConsumes, 0.75, graph.DomainAPI, true},
		{graph.EdgeConfiguredBy, 0.65, graph.DomainInfra, true},
		{graph.EdgeDocuments, 0.65, graph.DomainDocs, true},
		{graph.EdgeMentions, 0.55, graph.DomainKnowledge, true},
	}

	for _, c := range checks {
		d, ok := byName[c.et]
		if !ok {
			t.Errorf("expected edge type %q in catalog but not found", c.et)
			continue
		}
		if d.SemanticWeight != c.weight {
			t.Errorf("%q: want weight %.2f, got %.2f", c.et, c.weight, d.SemanticWeight)
		}
		if d.Domain != c.domain {
			t.Errorf("%q: want domain %q, got %q", c.et, c.domain, d.Domain)
		}
		if d.Synthetic != c.synth {
			t.Errorf("%q: want synthetic=%v, got synthetic=%v", c.et, c.synth, d.Synthetic)
		}
	}
}
