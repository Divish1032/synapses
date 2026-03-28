package store_test

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// seedEmbedNodes saves two function nodes to the store and returns their IDs as strings.
func seedEmbedNodes(t *testing.T, st interface{ SaveGraph(*graph.Graph) error }) (idA, idB string) {
	t.Helper()
	g := graph.New("embedtest")

	nidA := g.MakeNodeID("pkg.go", "AuthHandler")
	nidB := g.MakeNodeID("pkg.go", "CacheLayer")

	g.AddNode(&graph.Node{ID: nidA, Type: graph.NodeFunction, Name: "AuthHandler",
		Package: "pkg", File: "pkg.go", Line: 1})
	g.AddNode(&graph.Node{ID: nidB, Type: graph.NodeFunction, Name: "CacheLayer",
		Package: "pkg", File: "pkg.go", Line: 10})

	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}
	return string(nidA), string(nidB)
}

func TestUpsertEmbedding_StoresCount(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	idA, _ := seedEmbedNodes(t, st)

	if err := st.UpsertEmbedding(idA, "nomic-embed-text", []float32{0.1, 0.2, 0.3}); err != nil {
		t.Fatalf("UpsertEmbedding: %v", err)
	}
	if count := st.EmbeddingCount(); count != 1 {
		t.Errorf("expected 1 embedding, got %d", count)
	}
}

func TestUpsertEmbedding_Idempotent(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	idA, _ := seedEmbedNodes(t, st)

	_ = st.UpsertEmbedding(idA, "model-a", []float32{1, 0, 0})
	_ = st.UpsertEmbedding(idA, "model-b", []float32{0, 1, 0})

	if count := st.EmbeddingCount(); count != 1 {
		t.Errorf("expected 1 after upsert (not 2), got %d", count)
	}
}

func TestVectorSearch_ReturnsMostSimilar(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	idA, idB := seedEmbedNodes(t, st)

	// AuthHandler → direction [1,0], CacheLayer → direction [0,1]
	_ = st.UpsertEmbedding(idA, "test", []float32{1, 0})
	_ = st.UpsertEmbedding(idB, "test", []float32{0, 1})

	// Query closest to AuthHandler.
	results, err := st.VectorSearch([]float32{0.9, 0.1}, 5)
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results, got none")
	}
	if results[0].Name != "AuthHandler" {
		t.Errorf("expected AuthHandler first, got %s", results[0].Name)
	}
	if results[0].Score <= 0 {
		t.Errorf("expected positive score, got %f", results[0].Score)
	}
}

func TestVectorSearch_EmptyStore_ReturnsNil(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	results, err := st.VectorSearch([]float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatalf("VectorSearch on empty store: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty, got %d results", len(results))
	}
}

func TestGetNodesWithoutEmbeddings_ReturnsOnlyUnembedded(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	idA, idB := seedEmbedNodes(t, st)

	// Embed only idA.
	_ = st.UpsertEmbedding(idA, "test", []float32{1, 0})

	missing, err := st.GetNodesWithoutEmbeddings(100)
	if err != nil {
		t.Fatalf("GetNodesWithoutEmbeddings: %v", err)
	}
	if len(missing) != 1 || missing[0] != idB {
		t.Errorf("expected [%s], got %v", idB, missing)
	}
}

func TestGetNodeTextForEmbedding_ReturnsNameAndDoc(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	g := graph.New("texttest")
	nid := g.MakeNodeID("a.go", "ParseRequest")
	g.AddNode(&graph.Node{
		ID:       nid,
		Type:     graph.NodeFunction,
		Name:     "ParseRequest",
		Package:  "pkg",
		File:     "a.go",
		Line:     1,
		Metadata: map[string]string{"doc": "parses an HTTP request"},
	})
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	text, ok := st.GetNodeTextForEmbedding(string(nid))
	if !ok {
		t.Fatal("expected ok=true, got false")
	}
	if text == "" {
		t.Error("expected non-empty text")
	}
	// NL pipeline converts "ParseRequest" to "parse request" for code nodes.
	if !strings.Contains(text, "parse request") {
		t.Errorf("expected text to contain 'parse request', got: %q", text)
	}
}

func TestUpsertEmbedding_PreNormalizesVector(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	idA, _ := seedEmbedNodes(t, st)

	// Insert a non-unit vector. UpsertEmbedding should normalize it.
	if err := st.UpsertEmbedding(idA, "test", []float32{3, 4}); err != nil {
		t.Fatalf("UpsertEmbedding: %v", err)
	}

	// Search with query in the same direction — should get high score (≈1.0).
	results, err := st.VectorSearch([]float32{3, 4}, 5)
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	// Dot product of two normalized identical-direction vectors ≈ 1.0.
	if results[0].Score < 0.99 {
		t.Errorf("expected score ≈ 1.0, got %f", results[0].Score)
	}
}
