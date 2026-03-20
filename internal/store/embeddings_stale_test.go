package store

// Internal tests for stale-embedding detection (need db access).

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

func TestGetNodesWithoutEmbeddings_DetectsStaleHash(t *testing.T) {
	t.Parallel()
	st, err := Open(t.TempDir() + "/stale_test.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// Insert a function node with no doc/sig.
	g := graph.New("staletest")
	nid := g.MakeNodeID("a.go", "Foo")
	g.AddNode(&graph.Node{
		ID: nid, Type: graph.NodeFunction, Name: "Foo",
		Package: "pkg", File: "a.go", Line: 1,
	})
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	// Embed it — hash stored = sha256("Foo")[:4].
	if err := st.UpsertEmbedding(string(nid), "model", []float32{1, 0}); err != nil {
		t.Fatalf("UpsertEmbedding: %v", err)
	}

	// Node should NOT appear as missing (hash matches).
	missing, err := st.GetNodesWithoutEmbeddings(0)
	if err != nil {
		t.Fatalf("GetNodesWithoutEmbeddings: %v", err)
	}
	for _, id := range missing {
		if id == string(nid) {
			t.Fatal("freshly-embedded node should not appear in missing list")
		}
	}

	// Simulate a code change: update the node's doc column directly.
	if _, err := st.graphDB.Exec(`UPDATE nodes SET doc = 'new doc text' WHERE id = ?`, string(nid)); err != nil {
		t.Fatalf("UPDATE nodes: %v", err)
	}

	// Now the stored hash ("Foo") differs from current hash ("Foo new doc text").
	// Node should appear as stale.
	missing, err = st.GetNodesWithoutEmbeddings(0)
	if err != nil {
		t.Fatalf("GetNodesWithoutEmbeddings after update: %v", err)
	}
	found := false
	for _, id := range missing {
		if id == string(nid) {
			found = true
			break
		}
	}
	if !found {
		t.Error("stale-embedded node should appear in missing list after doc change")
	}
}
